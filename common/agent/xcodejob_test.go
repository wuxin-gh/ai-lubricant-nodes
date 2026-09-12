package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// The Xcode install job runner: full pipeline with faked exec/download, the
// already-satisfied short-circuit, failure and cancel paths, the single-job
// rule, and monotonic seqs with exactly one terminal result. CI hosts are not
// macOS, so the runner's goos/free seams force the darwin path.

type frameCollector struct {
	mu     sync.Mutex
	frames []*agentcomposev2.NodeUpstreamFrame
	notify chan struct{}
}

func newFrameCollector() *frameCollector {
	return &frameCollector{notify: make(chan struct{}, 1)}
}

func (c *frameCollector) emit(f *agentcomposev2.NodeUpstreamFrame) error {
	c.mu.Lock()
	c.frames = append(c.frames, f)
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
	return nil
}

// events returns every job event emitted so far, in order.
func (c *frameCollector) events() []*agentcomposev2.NodeHostToolJobEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*agentcomposev2.NodeHostToolJobEvent
	for _, f := range c.frames {
		if ev := f.GetHostToolJobEvent(); ev != nil {
			out = append(out, ev)
		}
	}
	return out
}

// resultCount reports how many terminal result frames have arrived — exactly
// one is the runner's contract.
func (c *frameCollector) resultCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, f := range c.frames {
		if f.GetHostToolJobResult() != nil {
			n++
		}
	}
	return n
}

// stageNames lists the stage of every event in order (test diagnostics).
func (c *frameCollector) stageNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, f := range c.frames {
		if ev := f.GetHostToolJobEvent(); ev != nil {
			out = append(out, ev.GetStage())
		}
	}
	return out
}

func (c *frameCollector) waitForResult(t *testing.T, timeout time.Duration) *agentcomposev2.NodeHostToolJobResult {
	t.Helper()
	deadline := time.After(timeout)
	for {
		c.mu.Lock()
		for _, f := range c.frames {
			if res := f.GetHostToolJobResult(); res != nil {
				c.mu.Unlock()
				return res
			}
		}
		c.mu.Unlock()
		select {
		case <-c.notify:
		case <-deadline:
			t.Fatal("timed out waiting for the result frame")
		}
	}
}

// fakeExec mirrors a real install: xcodebuild -version fails until the fake
// xip has run, xip produces an app bundle with a toolchain, sudo -n fails (the
// rootless DEVELOPER_DIR fallback then activates), -runFirstLaunch fails
// (best-effort) — the pipeline must still complete.
func fakeExec(t *testing.T) func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
	installed := false
	return func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
		switch name {
		case "xcodebuild":
			if len(args) > 0 && args[0] == "-version" {
				if installed {
					return []byte("Xcode 15.2\nBuild version 15C500b\n"), nil
				}
				return nil, errors.New("xcode-select: error: tool requires a full Xcode")
			}
			return nil, errors.New("xcodebuild: error: must run as root for first launch")
		case "xip":
			installed = true
			toolchain := filepath.Join(dir, "Xcode.app", "Contents", "Developer", "usr", "bin")
			if err := os.MkdirAll(toolchain, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(toolchain, "xcodebuild"), []byte("stub"), 0o755); err != nil {
				t.Fatal(err)
			}
			return []byte("xip: unpacked"), nil
		case "sudo":
			return nil, errors.New("sudo: a password is required")
		}
		return nil, fmt.Errorf("unexpected exec %q %v", name, args)
	}
}

func newXcodeTestRunner(t *testing.T, fc *frameCollector) *XcodeJobRunner {
	t.Helper()
	useTempState(t)
	t.Setenv("HOME", t.TempDir())       // darwin user-home resolution
	t.Setenv("USERPROFILE", os.Getenv("HOME")) // windows os.UserHomeDir
	t.Setenv("DEVELOPER_DIR", "")
	return NewXcodeJobRunner(fc.emit, nil, nil)
}

func xcodeJobReq(jobID string) *agentcomposev2.NodeHostToolJob {
	return &agentcomposev2.NodeHostToolJob{
		JobId:              jobID,
		Tool:               "xcode",
		TargetVersion:      "15.2",
		DownloadUrl:        "https://example.com/Xcode_15.2.xip",
		DownloadSizeBytes:  12, // == len(fakeXipContent): the verify stage checks the downloaded size against this
		TimeoutSeconds:     60,
	}
}

func TestXcodeJobHappyPath(t *testing.T) {
	fc := newFrameCollector()
	r := newXcodeTestRunner(t, fc)
	r.goos = "darwin"
	r.execFn = fakeExec(t)
	r.downloadFn = func(ctx context.Context, j *xcodeJob, url, dest string, spec proxySpec, total int64) error {
		// Report byte progress like the real downloader would before finishing.
		r.downloadProgress(j, 2, total)
		return os.WriteFile(dest, fakeXipContent, 0o644)
	}
	// The move must land at the STANDARD name — /Applications/Xcode.app, not a
	// versioned Xcode-15.2.app (the shim, Spotlight and every Apple tool
	// assume the standard name). Tests must not touch the real /Applications:
	// the fake asserts the requested destination is the standard name, then
	// physically renames the extracted bundle into a temp dir under the SAME
	// basename — the post-move toolchain check Stats <dest>/Contents/…, and the
	// fake xip created the bundle under appDir, so it must actually move.
	var movedTo string
	moveRoot := t.TempDir()
	r.moveAppFn = func(appDir, dest string) (string, error) {
		movedTo = dest
		final := filepath.Join(moveRoot, filepath.Base(dest))
		if err := os.Rename(appDir, final); err != nil {
			return "", err
		}
		return final, nil
	}

	if err := r.Start(context.Background(), xcodeJobReq("htj-happy")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := fc.waitForResult(t, 10*time.Second)
	if !res.GetOk() {
		t.Fatalf("expected ok result, got %+v", res)
	}
	if res.GetStageReached() != "completed" {
		t.Fatalf("stage_reached = %q, want completed", res.GetStageReached())
	}
	if !strings.Contains(res.GetXcodebuildVersion(), "Xcode 15.2") {
		t.Fatalf("xcodebuild_version = %q, want the probed Xcode 15.2 output", res.GetXcodebuildVersion())
	}
	if movedTo != "/Applications/Xcode.app" {
		t.Fatalf("move destination = %q, want the standard /Applications/Xcode.app", movedTo)
	}
	// The fake redirects the physical rename into a temp root (tests must not
	// touch the real /Applications); the reported app_path is the fake's return,
	// so assert the basename + that a toolchain actually landed there (the
	// post-move check would have failed the job otherwise).
	if filepath.Base(res.GetAppPath()) != "Xcode.app" || filepath.Dir(res.GetAppPath()) != moveRoot {
		t.Fatalf("app_path = %q, want Xcode.app under %q", res.GetAppPath(), moveRoot)
	}
	if _, err := os.Stat(filepath.Join(res.GetAppPath(), "Contents", "Developer", "usr", "bin", "xcodebuild")); err != nil {
		t.Fatalf("moved bundle missing toolchain: %v", err)
	}

	// Stages arrive in pipeline order (multiple events per stage are fine) with
	// strictly increasing seqs.
	wantOrder := []string{"checking", "downloading", "verifying", "extracting", "moving", "activating", "probing", "completed"}
	wantIdx := 0
	last := int64(0)
	for _, ev := range fc.events() {
		if ev.GetSeq() <= last {
			t.Fatalf("seq not monotonic: %d after %d", ev.GetSeq(), last)
		}
		last = ev.GetSeq()
		if wantIdx < len(wantOrder) && ev.GetStage() == wantOrder[wantIdx] {
			wantIdx++
		}
	}
	if wantIdx != len(wantOrder) {
		t.Fatalf("stage order = %v, missing stages after %q", fc.stageNames(), wantOrder[wantIdx])
	}
	// Download progress carries byte counters.
	var sawBytes bool
	for _, ev := range fc.events() {
		if ev.GetStage() == "downloading" && ev.GetTotalBytes() > 0 {
			sawBytes = true
		}
	}
	if !sawBytes {
		t.Fatal("download events must carry current/total bytes")
	}
}

func TestXcodeJobShortCircuitsWhenSatisfied(t *testing.T) {
	fc := newFrameCollector()
	r := newXcodeTestRunner(t, fc)
	r.goos = "darwin"
	r.execFn = func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
		if name == "xcodebuild" {
			return []byte("Xcode 16.2\nBuild version 16C5013f\n"), nil
		}
		t.Fatalf("unexpected exec %q during short-circuit", name)
		return nil, nil
	}
	r.downloadFn = func(ctx context.Context, j *xcodeJob, url, dest string, spec proxySpec, total int64) error {
		t.Fatal("a satisfied target must never download")
		return nil
	}

	req := xcodeJobReq("htj-short")
	if err := r.Start(context.Background(), req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := fc.waitForResult(t, 10*time.Second)
	if !res.GetOk() || res.GetStageReached() != "checking" {
		t.Fatalf("expected an ok short-circuit at checking, got %+v", res)
	}
	if !strings.Contains(res.GetXcodebuildVersion(), "16.2") {
		t.Fatalf("xcodebuild_version = %q", res.GetXcodebuildVersion())
	}
}

// fakeXipContent is a minimal plausible .xip body: XAR magic + padding. The
// verifying stage checks the magic, so every fake download must write this.
var fakeXipContent = append([]byte("xar!"), bytes.Repeat([]byte{0}, 8)...)

// A .xip corrupted in transit (Apple signature check fails inside xip) must
// be retried automatically within the same job and succeed on a later
// attempt — the「damaged and can't be expanded」symptom, without the operator
// having to click retry after a wasted 30-minute extract.
func TestXcodeJobRetriesDamagedXipAndSucceeds(t *testing.T) {
	fc := newFrameCollector()
	r := newXcodeTestRunner(t, fc)
	r.goos = "darwin"
	xipCalls := 0
	r.execFn = func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
		switch name {
		case "xcodebuild":
			if xipCalls == 0 {
				return nil, errors.New("xcode-select: error: tool requires a full Xcode")
			}
			return []byte("Xcode 15.2\nBuild version 15C500b\n"), nil
		case "xip":
			xipCalls++
			if xipCalls <= 2 {
				// Corrupted copy: fails with the classic damaged-archive error.
				return []byte("xip: error: The archive \"Xcode.xip\" is damaged and can't be expanded."),
					errors.New("exit status 1")
			}
			// Third attempt: a good copy extracts an app with a toolchain.
			toolchain := filepath.Join(dir, "Xcode.app", "Contents", "Developer", "usr", "bin")
			if err := os.MkdirAll(toolchain, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(toolchain, "xcodebuild"), []byte("stub"), 0o755); err != nil {
				t.Fatal(err)
			}
			return []byte("xip: unpacked"), nil
		case "sudo":
			return nil, errors.New("sudo: a password is required")
		}
		return nil, fmt.Errorf("unexpected exec %q %v", name, args)
	}
	r.downloadFn = func(ctx context.Context, j *xcodeJob, url, dest string, spec proxySpec, total int64) error {
		return os.WriteFile(dest, fakeXipContent, 0o644)
	}
	// Same fake move as the happy path: the real moveXcodeApp would try to
	// rename into the real /Applications (impossible on a non-mac CI host);
	// redirect into a temp root under the standard basename.
	r.moveAppFn = func(appDir, dest string) (string, error) {
		final := filepath.Join(t.TempDir(), filepath.Base(dest))
		if err := os.Rename(appDir, final); err != nil {
			return "", err
		}
		return final, nil
	}

	if err := r.Start(context.Background(), xcodeJobReq("htj-damaged")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := fc.waitForResult(t, 10*time.Second)
	if !res.GetOk() || res.GetStageReached() != "completed" {
		t.Fatalf("expected ok completed after retries, got %+v", res)
	}
	if xipCalls != 3 {
		t.Fatalf("xip calls = %d, want 3 (two damaged + one good)", xipCalls)
	}
	// The operator must have seen the retry events.
	var sawRetry bool
	for _, ev := range fc.events() {
		if ev.GetStage() == "downloading" && strings.Contains(ev.GetMessage(), "自动重试") {
			sawRetry = true
		}
	}
	if !sawRetry {
		t.Fatal("retry events must be emitted for each re-download")
	}
	if fc.resultCount() != 1 {
		t.Fatalf("result frames = %d, want exactly 1", fc.resultCount())
	}
}

// Three damaged copies in a row exhaust the retries: the job fails with
// extract_failed, the xip stderr in the tail, and guidance that names the
// usual culprit (egress proxy corrupting large transfers).
func TestXcodeJobDamagedXipExhaustsRetries(t *testing.T) {
	fc := newFrameCollector()
	r := newXcodeTestRunner(t, fc)
	r.goos = "darwin"
	r.execFn = func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
		switch name {
		case "xcodebuild":
			return nil, errors.New("xcode-select: error: tool requires a full Xcode")
		case "xip":
			return []byte("xip: error: The archive \"Xcode.xip\" is damaged and can't be expanded."),
				errors.New("exit status 1")
		}
		return nil, fmt.Errorf("unexpected exec %q %v", name, args)
	}
	r.downloadFn = func(ctx context.Context, j *xcodeJob, url, dest string, spec proxySpec, total int64) error {
		return os.WriteFile(dest, fakeXipContent, 0o644)
	}

	if err := r.Start(context.Background(), xcodeJobReq("htj-damaged3")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := fc.waitForResult(t, 10*time.Second)
	if res.GetOk() || res.GetErrorCode() != "extract_failed" {
		t.Fatalf("expected extract_failed after 3 attempts, got %+v", res)
	}
	if !strings.Contains(res.GetErrorMessage(), "出口代理") {
		t.Fatalf("error message should name the proxy guidance: %q", res.GetErrorMessage())
	}
	if !strings.Contains(res.GetErrorMessage(), "damaged") {
		t.Fatalf("error message should carry the xip stderr tail: %q", res.GetErrorMessage())
	}
}

// The move is standard-destination-or-fail: an unwritable /Applications is a
// terminal move_failed with the manual-mv hint — never a silent detour into
// ~/Applications (a non-standard home the shim and Spotlight ignore, which is
// exactly how「已装 Xcode 识别不出来」happened).
func TestXcodeJobMoveFailureIsTerminalWithHint(t *testing.T) {
	fc := newFrameCollector()
	r := newXcodeTestRunner(t, fc)
	r.goos = "darwin"
	r.execFn = fakeExec(t)
	r.downloadFn = func(ctx context.Context, j *xcodeJob, url, dest string, spec proxySpec, total int64) error {
		return os.WriteFile(dest, fakeXipContent, 0o644)
	}
	r.moveAppFn = func(appDir, dest string) (string, error) {
		// Real moveXcodeApp on a non-root CI host: the rename into the real
		// /Applications fails with a permission error. Mirror the production
		// message verbatim (hint included) — the test asserts the operator-
		// facing text below.
		return "", fmt.Errorf(
			"移动 Xcode.app 到 %s 失败: permission denied（常见原因：节点非 root 且 /Applications 不可写。"+
				"可手动执行 sudo mv %q %q，或在节点详情页用 sudo 启动节点后重试）",
			dest, appDir, dest)
	}

	if err := r.Start(context.Background(), xcodeJobReq("htj-movefail")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := fc.waitForResult(t, 10*time.Second)
	if res.GetOk() || res.GetErrorCode() != "move_failed" {
		t.Fatalf("expected move_failed, got %+v", res)
	}
	if !strings.Contains(res.GetErrorMessage(), "/Applications/Xcode.app") {
		t.Fatalf("error message should name the standard destination, got %q", res.GetErrorMessage())
	}
	if !strings.Contains(res.GetErrorMessage(), "sudo mv") {
		t.Fatalf("error message should carry the manual-mv hint, got %q", res.GetErrorMessage())
	}
}

func TestXcodeJobDownloadFailureIsRetryable(t *testing.T) {
	fc := newFrameCollector()
	r := newXcodeTestRunner(t, fc)
	r.goos = "darwin"
	r.execFn = fakeExec(t)
	r.downloadFn = func(ctx context.Context, j *xcodeJob, url, dest string, spec proxySpec, total int64) error {
		return errors.New("connection reset mid-transfer")
	}

	if err := r.Start(context.Background(), xcodeJobReq("htj-dlfail")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := fc.waitForResult(t, 10*time.Second)
	if res.GetOk() || res.GetErrorCode() != "download_failed" || !res.GetRetryable() {
		t.Fatalf("expected retryable download_failed, got %+v", res)
	}
}

func TestXcodeJobInsufficientSpace(t *testing.T) {
	fc := newFrameCollector()
	r := newXcodeTestRunner(t, fc)
	r.goos = "darwin"
	r.freeFn = func(path string) (int64, error) { return 1 << 20, nil }

	if err := r.Start(context.Background(), xcodeJobReq("htj-disk")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := fc.waitForResult(t, 10*time.Second)
	if res.GetOk() || res.GetErrorCode() != "insufficient_space" {
		t.Fatalf("expected insufficient_space, got %+v", res)
	}
}

// An installed-but-unselected Xcode must short-circuit at checking: the bare
// probe fails (xcode-select points elsewhere / CLT-only shim), discovery finds
// the app, activation points the dev dir at it, the re-probe succeeds — no
// download, no extract. Regression for the「已装 Xcode 仍触发下载解压并失败」
// symptom.
func TestXcodeJobShortCircuitsAfterActivatingInstalledXcode(t *testing.T) {
	fc := newFrameCollector()
	r := newXcodeTestRunner(t, fc)
	r.goos = "darwin"
	r.execFn = func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
		if name == "xcodebuild" {
			// Before activation: the /usr/bin shim refuses (CLT-only shape).
			// After: the activated app's real xcodebuild answers.
			if os.Getenv("DEVELOPER_DIR") == "" {
				return nil, errors.New("xcode-select: error: tool requires a full Xcode")
			}
			return []byte("Xcode 16.2\nBuild version 16C5013f\n"), nil
		}
		t.Fatalf("unexpected exec %q %v", name, args)
		return nil, nil
	}
	appDir := t.TempDir()
	toolchain := filepath.Join(appDir, "Xcode.app", "Contents", "Developer", "usr", "bin")
	if err := os.MkdirAll(toolchain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolchain, "xcodebuild"), []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.discoverAppsFn = func() []string { return []string{appDir + string(os.PathSeparator) + "Xcode.app"} }
	activated := ""
	r.activateDevDirFn = func(devDir string) string {
		activated = devDir
		os.Setenv("DEVELOPER_DIR", devDir)
		return "DEVELOPER_DIR"
	}
	r.downloadFn = func(ctx context.Context, j *xcodeJob, url, dest string, spec proxySpec, total int64) error {
		t.Fatal("an activated installed Xcode must never download")
		return nil
	}

	req := xcodeJobReq("htj-activate")
	req.TargetVersion = "16.2"
	if err := r.Start(context.Background(), req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := fc.waitForResult(t, 10*time.Second)
	if !res.GetOk() || res.GetStageReached() != "checking" {
		t.Fatalf("expected an ok short-circuit at checking, got %+v", res)
	}
	if !strings.Contains(res.GetXcodebuildVersion(), "16.2") {
		t.Fatalf("xcodebuild_version = %q", res.GetXcodebuildVersion())
	}
	if activated == "" {
		t.Fatal("the installed Xcode must have been activated before the re-probe")
	}
}

func TestXcodeJobCancelDuringDownload(t *testing.T) {
	fc := newFrameCollector()
	r := newXcodeTestRunner(t, fc)
	r.goos = "darwin"
	r.execFn = fakeExec(t)
	r.downloadFn = func(ctx context.Context, j *xcodeJob, url, dest string, spec proxySpec, total int64) error {
		<-ctx.Done()
		return ctx.Err()
	}

	if err := r.Start(context.Background(), xcodeJobReq("htj-cancel")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := r.Cancel("htj-cancel"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	res := fc.waitForResult(t, 10*time.Second)
	if res.GetOk() || res.GetErrorCode() != "cancelled" {
		t.Fatalf("expected cancelled, got %+v", res)
	}
}

func TestXcodeJobSecondJobIsBusy(t *testing.T) {
	fc := newFrameCollector()
	r := newXcodeTestRunner(t, fc)
	r.goos = "darwin"
	r.execFn = fakeExec(t)
	release := make(chan struct{})
	r.downloadFn = func(ctx context.Context, j *xcodeJob, url, dest string, spec proxySpec, total int64) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return os.WriteFile(dest, fakeXipContent, 0o644)
		}
	}
	// Same fake move as the happy path (CI hosts cannot rename into the real
	// /Applications); the first job must run to completion for the busy check.
	r.moveAppFn = func(appDir, dest string) (string, error) {
		final := filepath.Join(t.TempDir(), filepath.Base(dest))
		if err := os.Rename(appDir, final); err != nil {
			return "", err
		}
		return final, nil
	}

	if err := r.Start(context.Background(), xcodeJobReq("htj-first")); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// A retried dispatch for the same id is accepted idempotently…
	if err := r.Start(context.Background(), xcodeJobReq("htj-first")); err != nil {
		t.Fatalf("same-id re-dispatch must be idempotent, got %v", err)
	}
	// …but a second concurrent install is rejected.
	if err := r.Start(context.Background(), xcodeJobReq("htj-second")); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second job must be rejected as busy, got %v", err)
	}
	close(release)
	res := fc.waitForResult(t, 10*time.Second)
	if !res.GetOk() || res.GetJobId() != "htj-first" {
		t.Fatalf("expected the first job to complete, got %+v", res)
	}
}

func TestXcodeJobRejectsBadRequests(t *testing.T) {
	fc := newFrameCollector()
	r := newXcodeTestRunner(t, fc)

	// Non-darwin host: rejected before anything else runs.
	r.goos = "linux"
	calls := 0
	r.execFn = func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
		calls++
		return nil, nil
	}
	if err := r.Start(context.Background(), xcodeJobReq("htj-linux")); err != nil {
		t.Fatalf("Start (accepted then fails as a result): %v", err)
	}
	res := fc.waitForResult(t, 10*time.Second)
	if res.GetOk() || res.GetErrorCode() != "not_darwin" || calls != 0 {
		t.Fatalf("expected not_darwin with no exec, got %+v (calls=%d)", res, calls)
	}

	// Bad tool and non-https/non-xip URL: rejected at Start (ack error).
	r.goos = "darwin"
	req := xcodeJobReq("htj-badtool")
	req.Tool = "clang"
	if err := r.Start(context.Background(), req); err == nil {
		t.Fatal("unknown tool must be rejected at Start")
	}
	req = xcodeJobReq("htj-badurl")
	req.DownloadUrl = "http://example.com/Xcode.xip"
	if err := r.Start(context.Background(), req); err == nil {
		t.Fatal("a non-https download_url must be rejected at Start")
	}
}

func TestXcodeTargetComparison(t *testing.T) {
	cases := []struct {
		current, target string
		want            bool
	}{
		{"Xcode 15.2\nBuild version 15C500b", "15.2", true},
		{"Xcode 16.2\nBuild version 16C5013f", "15.2", true},
		{"Xcode 15.2", "16.0", false},
		{"Xcode 15.1", "15.2", false},
		{"", "15.2", false},
		{"garbage", "15.2", false},
		{"Xcode 16.0\nBuild version 16A1", "16.0 beta 2", true},
	}
	for _, tc := range cases {
		if got := satisfiesXcodeTarget(tc.current, tc.target); got != tc.want {
			t.Errorf("satisfiesXcodeTarget(%q, %q) = %v, want %v", tc.current, tc.target, got, tc.want)
		}
	}
}

func TestXcodeVersionSlug(t *testing.T) {
	cases := map[string]string{
		"15.2":        "15.2",
		"16":          "16",
		"16.0 beta 2": "16.0",
		"Xcode 26.0":  "26.0",
		"":            "Xcode",
	}
	for in, want := range cases {
		if got := versionSlug(in); got != want {
			t.Errorf("versionSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
