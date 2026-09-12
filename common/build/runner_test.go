package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ai-lubricant-nodes/common/agent"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// fakeRunnerLogger is a no-op logger.
type fakeRunnerLogger struct{}

func (fakeRunnerLogger) Info(string, ...any)  {}
func (fakeRunnerLogger) Warn(string, ...any)  {}
func (fakeRunnerLogger) Debug(string, ...any) {}
func (fakeRunnerLogger) Error(string, ...any) {}

// captureEmitter collects upstream frames so tests assert on events/results.
type captureEmitter struct {
	mu     sync.Mutex
	frames []*agentcomposev2.NodeUpstreamFrame
}

func (e *captureEmitter) emit(f *agentcomposev2.NodeUpstreamFrame) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.frames = append(e.frames, f)
	return nil
}

func (e *captureEmitter) buildEvents() []*agentcomposev2.NodeBuildEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []*agentcomposev2.NodeBuildEvent{}
	for _, f := range e.frames {
		if ev, ok := f.Frame.(*agentcomposev2.NodeUpstreamFrame_NodeBuildEvent); ok {
			out = append(out, ev.NodeBuildEvent)
		}
	}
	return out
}

func (e *captureEmitter) buildResult() *agentcomposev2.NodeBuildResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range e.frames {
		if res, ok := f.Frame.(*agentcomposev2.NodeUpstreamFrame_NodeBuildResult); ok {
			return res.NodeBuildResult
		}
	}
	return nil
}

// scriptExec replays a canned outcome per command: the args joined with spaces
// select the entry. Anything unknown fails — tests must be explicit about the
// full command sequence they expect.
type scriptExec struct {
	mu    sync.Mutex
	calls []string // "git clone --depth 1 …" per invocation, in order
	// script maps the joined args to (output, error).
	script map[string]scriptEntry
}

type scriptEntry struct {
	out []byte
	err error
}

func (s *scriptExec) call(ctx context.Context, dir, name string, args []string) ([]byte, error) {
	joined := name + " " + strings.Join(args, " ")
	s.mu.Lock()
	s.calls = append(s.calls, joined)
	entry, ok := s.script[joined]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("scriptExec: unexpected command %q", joined)
	}
	return entry.out, entry.err
}

func (s *scriptExec) invoked(joined string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c == joined {
			return true
		}
	}
	return false
}

// happyReq is a minimal valid build request. artifact_glob points at a file the
// test plants via the step side effect: steps run through execFn, which cannot
// create files — so the fake exec plants the artifact via globTarget instead
// (runBuild globs inside srcDir, and globOne needs a real file on disk).
func happyReq(buildID string) *agentcomposev2.NodeBuildRequest {
	return &agentcomposev2.NodeBuildRequest{
		BuildId:        buildID,
		RecipeKind:     "xcode_wda",
		SourceUrl:      "https://example.invalid/wda.git",
		SourceRef:      "v1.0.0",
		Steps:          []string{"xcodebuild build-for-testing -project WDA.xcodeproj"},
		ArtifactGlob:   "build/out/unsigned.ipa",
		UploadUrl:      "https://example.invalid/upload",
		UploadToken:    "tok-1",
		TimeoutSeconds: 60,
		ArtifactName:   "wda-unsigned.ipa",
	}
}

// runAndWait drives Start and blocks until the result frame lands.
func runAndWait(t *testing.T, r *Runner, req *agentcomposev2.NodeBuildRequest, em *captureEmitter) {
	t.Helper()
	if err := r.Start(context.Background(), req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if em.buildResult() != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("build did not produce a result frame in time")
}

func TestRunnerHappyPath(t *testing.T) {
	em := &captureEmitter{}
	r := NewRunner(em.emit, fakeRunnerLogger{}, nil)

	// The exec script: clone succeeds, checkout succeeds, one build step
	// succeeds. The step must also plant the artifact on disk because globOne
	// stats a real file (workDir is a real temp dir, srcDir is real too).
	se := &scriptExec{script: map[string]scriptEntry{}}
	se.script["git clone --depth 1 --branch v1.0.0 https://example.invalid/wda.git src"] = scriptEntry{}
	se.script["sh -c xcodebuild build-for-testing -project WDA.xcodeproj"] = scriptEntry{out: []byte("xcodebuild: build succeeded\n")}
	// Plant the artifact: the fake sh step cannot write files, so intercept at
	// the glob — plant it from a pre-step by making clone the writer is not
	// possible. Instead the test plants the file AFTER Start begins, from the
	// execFn hook when the sh step is called.
	se.mu.Lock()
	r.execFn = func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
		if name == "sh" {
			// The real srcDir is workDir/src, but workDir is randomized inside
			// runBuild. execFn sees dir == srcDir here, so plant the artifact
			// relative to it.
			rel := filepath.FromSlash("build/out/unsigned.ipa")
			full := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(full, []byte("fake ipa bytes"), 0o644); err != nil {
				return nil, err
			}
		}
		return se.call(ctx, dir, name, args)
	}
	se.mu.Unlock()
	r.uploadFn = func(ctx context.Context, url, token, path, filename string) error {
		if url != "https://example.invalid/upload" || token != "tok-1" {
			return fmt.Errorf("wrong upload target: %s %s", url, token)
		}
		if filename != "unsigned.ipa" {
			return fmt.Errorf("wrong filename: %s", filename)
		}
		return nil
	}

	req := happyReq("b-happy")
	runAndWait(t, r, req, em)

	res := em.buildResult()
	if res == nil {
		t.Fatal("no result frame")
	}
	if !res.GetOk() {
		t.Fatalf("expected ok build, got errCode=%s err=%s logTail=%q", res.GetErrorCode(), res.GetErrorMessage(), "")
	}
	if res.GetArtifactSha256() == "" {
		t.Error("expected artifact sha256 in result")
	}
	if res.GetArtifactSizeBytes() != int64(len("fake ipa bytes")) {
		t.Errorf("artifact size = %d, want %d", res.GetArtifactSizeBytes(), len("fake ipa bytes"))
	}
	evs := em.buildEvents()
	if len(evs) == 0 {
		t.Fatal("expected build events")
	}
	// seq must be monotonic
	for i := 1; i < len(evs); i++ {
		if evs[i].GetSeq() <= evs[i-1].GetSeq() {
			t.Errorf("event seq not monotonic: %d after %d", evs[i].GetSeq(), evs[i-1].GetSeq())
		}
	}
	// terminal event must be 100% and carry stage "completed"
	last := evs[len(evs)-1]
	if last.GetStage() != "completed" || last.GetPercent() != 100 {
		t.Errorf("terminal event stage=%s percent=%d, want completed/100", last.GetStage(), last.GetPercent())
	}
}

func TestRunnerCloneFails(t *testing.T) {
	em := &captureEmitter{}
	r := NewRunner(em.emit, fakeRunnerLogger{}, nil)
	se := &scriptExec{script: map[string]scriptEntry{}}
	se.script["git clone --depth 1 --branch v1.0.0 https://example.invalid/wda.git src"] = scriptEntry{
		out: []byte("fatal: repository not found"),
		err: errors.New("exit status 128"),
	}
	r.execFn = se.call

	runAndWait(t, r, happyReq("b-clonefail"), em)

	res := em.buildResult()
	if res.GetOk() {
		t.Fatal("expected failure")
	}
	if res.GetErrorCode() != "clone_failed" {
		t.Errorf("errCode = %s, want clone_failed", res.GetErrorCode())
	}
	if !res.GetRetryable() {
		t.Error("clone failure should be retryable")
	}
	// log tail carries the git output, bounded
	last := em.buildEvents()[len(em.buildEvents())-1]
	if !strings.Contains(last.GetLogTail(), "fatal: repository not found") {
		t.Errorf("log tail missing git output: %q", last.GetLogTail())
	}
}

func TestRunnerStepFailsRedactsSecrets(t *testing.T) {
	em := &captureEmitter{}
	r := NewRunner(em.emit, fakeRunnerLogger{}, nil)
	se := &scriptExec{script: map[string]scriptEntry{}}
	se.script["git clone --depth 1 --branch v1.0.0 https://example.invalid/wda.git src"] = scriptEntry{}
	se.script["sh -c xcodebuild build-for-testing -project WDA.xcodeproj"] = scriptEntry{
		out: []byte("error: -----BEGIN RSA PRIVATE KEY-----\nMIIEpA never mind\nbuild failed"),
		err: errors.New("exit status 65"),
	}
	r.execFn = se.call

	runAndWait(t, r, happyReq("b-stepfail"), em)

	res := em.buildResult()
	if res.GetOk() {
		t.Fatal("expected failure")
	}
	if res.GetErrorCode() != "step_failed" {
		t.Errorf("errCode = %s, want step_failed", res.GetErrorCode())
	}
	// The terminal event's log tail must carry the redacted output; the raw
	// error string itself must not appear anywhere in the emitted frames.
	for _, ev := range em.buildEvents() {
		if strings.Contains(ev.GetLogTail(), "MIIEpA") {
			t.Error("PEM body leaked into event log tail")
		}
	}
}

func TestRunnerArtifactGlobNoMatch(t *testing.T) {
	em := &captureEmitter{}
	r := NewRunner(em.emit, fakeRunnerLogger{}, nil)
	se := &scriptExec{script: map[string]scriptEntry{}}
	se.script["git clone --depth 1 --branch v1.0.0 https://example.invalid/wda.git src"] = scriptEntry{}
	se.script["sh -c xcodebuild build-for-testing -project WDA.xcodeproj"] = scriptEntry{}
	r.execFn = se.call

	runAndWait(t, r, happyReq("b-nomatch"), em)

	res := em.buildResult()
	if res.GetOk() {
		t.Fatal("expected failure")
	}
	if res.GetErrorCode() != "artifact_not_found" {
		t.Errorf("errCode = %s, want artifact_not_found", res.GetErrorCode())
	}
}

func TestRunnerUploadFailsRetryable(t *testing.T) {
	em := &captureEmitter{}
	r := NewRunner(em.emit, fakeRunnerLogger{}, nil)
	se := &scriptExec{script: map[string]scriptEntry{}}
	se.script["git clone --depth 1 --branch v1.0.0 https://example.invalid/wda.git src"] = scriptEntry{}
	se.script["sh -c xcodebuild build-for-testing -project WDA.xcodeproj"] = scriptEntry{}
	r.execFn = func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
		if name == "sh" {
			rel := filepath.FromSlash("build/out/unsigned.ipa")
			if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, rel), []byte("ipa"), 0o644); err != nil {
				return nil, err
			}
		}
		return se.call(ctx, dir, name, args)
	}
	r.uploadFn = func(ctx context.Context, url, token, path, filename string) error {
		return errors.New("connection reset")
	}

	runAndWait(t, r, happyReq("b-uploadfail"), em)

	res := em.buildResult()
	if res.GetOk() {
		t.Fatal("expected failure")
	}
	if res.GetErrorCode() != "upload_failed" {
		t.Errorf("errCode = %s, want upload_failed", res.GetErrorCode())
	}
	if !res.GetRetryable() {
		t.Error("upload failure should be retryable")
	}
	if res.GetArtifactSha256() == "" {
		t.Error("sha256 should still be reported on upload failure")
	}
}

func TestRunnerCancelUnknownBuildIsNoop(t *testing.T) {
	r := NewRunner(func(*agentcomposev2.NodeUpstreamFrame) error { return nil }, fakeRunnerLogger{}, nil)
	if err := r.Cancel("nope"); err != nil {
		t.Fatalf("cancel of unknown build: %v", err)
	}
	r.StopAll() // no builds: must not panic
}

func TestRunnerStartValidation(t *testing.T) {
	r := NewRunner(func(*agentcomposev2.NodeUpstreamFrame) error { return nil }, fakeRunnerLogger{}, nil)
	cases := []*agentcomposev2.NodeBuildRequest{
		{},                             // no build id
		{BuildId: "x"},                 // no source
		{BuildId: "x", SourceUrl: "u"}, // no steps
		{BuildId: "x", SourceUrl: "u", Steps: []string{"s"}}, // no upload
	}
	for i, req := range cases {
		if err := r.Start(context.Background(), req); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

func TestRunnerStartIdempotent(t *testing.T) {
	em := &captureEmitter{}
	r := NewRunner(em.emit, fakeRunnerLogger{}, nil)
	// Block the first build forever at clone; a second Start with the same id
	// must return nil without registering a second pipeline.
	block := make(chan struct{})
	se := &scriptExec{script: map[string]scriptEntry{}}
	se.script["git clone --depth 1 --branch v1.0.0 https://example.invalid/wda.git src"] = scriptEntry{}
	r.execFn = func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
		<-block
		return se.call(ctx, dir, name, args)
	}
	req := happyReq("b-dup")
	if err := r.Start(context.Background(), req); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := r.Start(context.Background(), req); err != nil {
		t.Fatalf("second start must be idempotent, got %v", err)
	}
	if len(r.ActiveBuilds()) != 1 {
		t.Errorf("expected exactly 1 active build, got %d", len(r.ActiveBuilds()))
	}
	close(block)
}

func TestTailBounded(t *testing.T) {
	big := strings.Repeat("a\n", 10_000)
	got := tail(big, logTailBytes)
	if len(got) > logTailBytes+10 { // +10 for the ellipsis and line realignment
		t.Errorf("tail length %d exceeds bound %d", len(got), logTailBytes)
	}
	if !strings.HasPrefix(got, "…") {
		t.Error("truncated tail should start with ellipsis")
	}
	// under the bound: returned as-is
	small := "short"
	if got := tail(small, 4096); got != small {
		t.Errorf("small input mutated: %q", got)
	}
}

func TestRedactSecrets(t *testing.T) {
	in := "prefix -----BEGIN PRIVATE KEY-----\nabc\nsuffix"
	got := redactPEM(in)
	if strings.Contains(got, "abc") {
		t.Errorf("key body leaked: %q", got)
	}
	if !strings.Contains(got, "[redacted key material]") {
		t.Errorf("redaction marker missing: %q", got)
	}
	// git-secret shapes: a clone-URL userinfo and an http.extraHeader value.
	if got := logTail([]byte("fatal: unable to access 'https://tok@host/repo.git/'")); strings.Contains(got, "tok@") {
		t.Errorf("clone URL userinfo leaked: %q", got)
	}
	if got := logTail([]byte("git -c http.extraHeader=Authorization: Basic dG9rOg== clone")); strings.Contains(got, "dG9rOg") {
		t.Errorf("extraHeader credential leaked: %q", got)
	}
}

func TestGlobOne(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build", "a.ipa"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build", "b.ipa"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := globOne(dir, "build/a.ipa"); err != nil {
		t.Errorf("single match should succeed: %v", err)
	}
	if _, err := globOne(dir, "build/*.ipa"); err == nil {
		t.Error("two matches must fail (ambiguity)")
	} else if !strings.Contains(err.Error(), "matched 2") {
		t.Errorf("unexpected ambiguity error: %v", err)
	}
	if _, err := globOne(dir, "nope/*.ipa"); err == nil {
		t.Error("no match must fail")
	}
	if _, err := globOne(dir, ""); err == nil {
		t.Error("empty pattern must fail")
	}
}

func TestSanitizeFileName(t *testing.T) {
	if got := sanitizeFileName("build/../../wda"); strings.ContainsAny(got, "/.") {
		t.Errorf("path components survived: %q", got)
	}
	long := strings.Repeat("z", 100)
	if got := sanitizeFileName(long); len(got) > 48 {
		t.Errorf("long id not clamped: %d", len(got))
	}
}

// fakeProxyReader is a ProxySpecReader stub: tests inject the egress-proxy
// snapshot the runner should see, without a real agent.Client.
type fakeProxyReader struct{ spec agent.ProxySpec }

func (f fakeProxyReader) DownloadProxySpec() agent.ProxySpec { return f.spec }

// proxyReq is happyReq with the egress paths in scope: the runner's clone
// command is what each proxy test asserts on.
func newProxyRunner(t *testing.T, spec agent.ProxySpec) (*Runner, *scriptExec, *captureEmitter) {
	t.Helper()
	em := &captureEmitter{}
	r := NewRunner(em.emit, fakeRunnerLogger{}, fakeProxyReader{spec: spec})
	se := &scriptExec{script: map[string]scriptEntry{}}
	// Clone succeeds (the artifact/upload path is irrelevant — these tests
	// only assert the clone argv). Plant no artifact; the build will fail at
	// artifact_not_found, which is fine for asserting clone args.
	r.execFn = se.call
	return r, se, em
}

func TestRunnerCloneURLPrefixRewritesURL(t *testing.T) {
	spec := agent.ProxySpec{Mode: "url_prefix", URLPrefix: "https://ghmirror.example"}
	r, se, em := newProxyRunner(t, spec)
	se.script["git clone --depth 1 --branch v1.0.0 https://ghmirror.example/https://example.invalid/wda.git src"] = scriptEntry{}

	runAndWait(t, r, happyReq("b-prefix"), em)

	joined := "git clone --depth 1 --branch v1.0.0 https://ghmirror.example/https://example.invalid/wda.git src"
	if !se.invoked(joined) {
		t.Errorf("url_prefix mode did not rewrite the clone URL\ninvoked: %v", se.calls)
	}
}

func TestRunnerCloneNetworkModeInjectsProxyArgs(t *testing.T) {
	spec := agent.ProxySpec{Mode: "network", URL: "http://proxy.local:8080"}
	r, se, em := newProxyRunner(t, spec)
	joined := "git -c https.proxy=http://proxy.local:8080 -c http.proxy=http://proxy.local:8080 clone --depth 1 --branch v1.0.0 https://example.invalid/wda.git src"
	se.script[joined] = scriptEntry{}

	runAndWait(t, r, happyReq("b-network"), em)

	if !se.invoked(joined) {
		t.Errorf("network mode did not inject -c proxy args\ninvoked: %v", se.calls)
	}
}
