package ioshost

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// fakeSteps records the pipeline calls and can fail at a chosen stage, so the
// job engine's ordering, event stream and error classification are testable
// without Apple credentials or a phone.
type fakeSteps struct {
	mu    sync.Mutex
	calls []string

	fetchErr    error
	devModeErr  error
	signPrepErr error
	signErr     error
	installErr  error
	launchErr   error

	// installed / isInstalledErr drive the fast-path check: installed=true makes
	// a PREPARE job skip fetch/sign/install.
	installed      bool
	isInstalledErr error

	assets SigningAssets
	// blockLaunch, when non-nil, holds Launch until closed — used to test cancel.
	blockLaunch chan struct{}
}

func (f *fakeSteps) record(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
}

func (f *fakeSteps) sequence() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeSteps) Fetch(_ context.Context, _ *agentcomposev2.NodeIosWdaArtifact, _ string, progress func(int)) (string, error) {
	f.record("fetch")
	if f.fetchErr != nil {
		return "", f.fetchErr
	}
	if progress != nil {
		progress(50)
		progress(100)
	}
	return "/tmp/wda.ipa", nil
}

func (f *fakeSteps) EnableDeveloperMode(context.Context, string) error {
	f.record("devmode")
	return f.devModeErr
}

func (f *fakeSteps) PrepareSigning(_ context.Context, _ *agentcomposev2.NodeIosWdaJobRequest, _ string, stage func(agentcomposev2.IosJobStage)) (SigningAssets, error) {
	f.record("prepare")
	if f.signPrepErr != nil {
		return SigningAssets{}, f.signPrepErr
	}
	stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_CREATING_PROFILE)
	return f.assets, nil
}

func (f *fakeSteps) Sign(_ context.Context, artifactPath string, _ SigningAssets, _, _ string) (string, error) {
	f.record("sign")
	if f.signErr != nil {
		return "", f.signErr
	}
	return artifactPath + ".signed", nil
}

func (f *fakeSteps) Install(context.Context, string, string) error {
	f.record("install")
	return f.installErr
}

func (f *fakeSteps) IsInstalled(context.Context, string, string) (bool, error) {
	f.record("isinstalled")
	return f.installed, f.isInstalledErr
}

func (f *fakeSteps) Launch(ctx context.Context, _, _, _ string, stage func(agentcomposev2.IosJobStage)) (int, error) {
	f.record("launch")
	if f.blockLaunch != nil {
		select {
		case <-f.blockLaunch:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if f.launchErr != nil {
		return 0, f.launchErr
	}
	stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_VERIFYING_CONTROL)
	return 8100, nil
}

// jobHarness captures the upstream frames a job emits.
type jobHarness struct {
	mu      sync.Mutex
	events  []*agentcomposev2.NodeIosJobEvent
	results []*agentcomposev2.NodeIosJobResult
	done    chan struct{}
	// doneClosed: close(h.done) must be idempotent — two overlapping jobs both
	// emit a result frame, and a second close panics.
	doneClosed bool
}

func newJobHarness() *jobHarness {
	return &jobHarness{done: make(chan struct{})}
}

func (h *jobHarness) emit(frame *agentcomposev2.NodeUpstreamFrame) error {
	h.mu.Lock()
	switch f := frame.GetFrame().(type) {
	case *agentcomposev2.NodeUpstreamFrame_IosJobEvent:
		h.events = append(h.events, f.IosJobEvent)
	case *agentcomposev2.NodeUpstreamFrame_IosJobResult:
		h.results = append(h.results, f.IosJobResult)
		if !h.doneClosed {
			h.doneClosed = true
			close(h.done)
		}
	}
	h.mu.Unlock()
	return nil
}

func (h *jobHarness) waitResult(t *testing.T) *agentcomposev2.NodeIosJobResult {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the job result frame")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.results[0]
}

func (h *jobHarness) stages() []agentcomposev2.IosJobStage {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]agentcomposev2.IosJobStage, 0, len(h.events))
	for _, e := range h.events {
		out = append(out, e.GetStage())
	}
	return out
}

func (h *jobHarness) seqs() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]int64, 0, len(h.events))
	for _, e := range h.events {
		out = append(out, e.GetSeq())
	}
	return out
}

func testJobManager(t *testing.T, steps WdaSteps) (*WdaJobManager, *jobHarness) {
	t.Helper()
	h := newJobHarness()
	m := NewWdaJobManager(h.emit, steps, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return m, h
}

func jobRequest() *agentcomposev2.NodeIosWdaJobRequest {
	return &agentcomposev2.NodeIosWdaJobRequest{
		JobId:  "job-1",
		Udid:   "udid-a",
		Action: agentcomposev2.IosWdaJobAction_IOS_WDA_JOB_ACTION_PREPARE,
		Artifact: &agentcomposev2.NodeIosWdaArtifact{
			ArtifactId:       "art-1",
			Url:              "https://example.test/wda.ipa",
			Sha256:           "abc123",
			Version:          "9.0.0",
			TargetBundleId:   "com.example.WebDriverAgentRunner.xctrunner",
			XctestConfigName: "WebDriverAgentRunner.xctest",
		},
		Signing: &agentcomposev2.NodeIosSigningMaterial{
			Mode:          agentcomposev2.IosSigningMode_IOS_SIGNING_MODE_APP_STORE_CONNECT,
			AscKeyId:      "KEYID",
			AscIssuerId:   "ISSUER",
			AscPrivateKey: []byte("-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----"),
		},
		ConfigRevision: 3,
	}
}

func TestJobHappyPathRunsStagesInOrder(t *testing.T) {
	steps := &fakeSteps{assets: SigningAssets{CertificateID: "cert-1", ProfileExpiresAt: "2027-01-01T00:00:00Z"}}
	m, h := testJobManager(t, steps)

	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := h.waitResult(t)

	if !res.GetOk() {
		t.Fatalf("job failed: %s / %s", res.GetErrorCode(), res.GetErrorMessage())
	}
	if got, want := steps.sequence(), []string{"isinstalled", "fetch", "prepare", "sign", "install", "launch"}; !equalStrings(got, want) {
		t.Fatalf("pipeline order = %v, want %v", got, want)
	}
	if res.GetCertificateId() != "cert-1" {
		t.Errorf("certificate id must round-trip so later devices reuse it, got %q", res.GetCertificateId())
	}
	if res.GetProfileExpiresAt() != "2027-01-01T00:00:00Z" {
		t.Errorf("profile expiry = %q", res.GetProfileExpiresAt())
	}
	if res.GetConfigRevision() != 3 {
		t.Errorf("config revision = %d, want 3", res.GetConfigRevision())
	}
	if res.GetStageReached() != agentcomposev2.IosJobStage_IOS_JOB_STAGE_COMPLETED {
		t.Errorf("stage reached = %v", res.GetStageReached())
	}
}

func TestJobPrepareSkipsProvisioningWhenInstalled(t *testing.T) {
	// A re-prepare of a device that already has the runner: skip fetch/sign/
	// install and go straight to launch.
	steps := &fakeSteps{installed: true}
	m, h := testJobManager(t, steps)
	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	res := h.waitResult(t)
	if !res.GetOk() {
		t.Fatalf("fast-path prepare failed: %s / %s", res.GetErrorCode(), res.GetErrorMessage())
	}
	if got, want := steps.sequence(), []string{"isinstalled", "launch"}; !equalStrings(got, want) {
		t.Fatalf("installed prepare must skip download/sign/install, got %v", got)
	}
}

func TestJobRenewAlwaysReinstalls(t *testing.T) {
	// renew must NOT take the fast path even when installed: the profile is what
	// is being renewed, so it must re-sign.
	steps := &fakeSteps{installed: true, assets: SigningAssets{CertificateID: "c"}}
	m, h := testJobManager(t, steps)
	req := jobRequest()
	req.Action = agentcomposev2.IosWdaJobAction_IOS_WDA_JOB_ACTION_RENEW
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	h.waitResult(t)
	if got, want := steps.sequence(), []string{"fetch", "prepare", "sign", "install", "launch"}; !equalStrings(got, want) {
		t.Fatalf("renew must run the full pipeline, got %v", got)
	}
}

func TestJobInstalledCheckErrorFallsBackToFullPipeline(t *testing.T) {
	// A browse failure must not skip the install — fall back to the full pipeline.
	steps := &fakeSteps{isInstalledErr: errors.New("usbmuxd blip"), assets: SigningAssets{CertificateID: "c"}}
	m, h := testJobManager(t, steps)
	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	res := h.waitResult(t)
	if !res.GetOk() {
		t.Fatalf("job should still succeed via full pipeline: %s", res.GetErrorCode())
	}
	if got, want := steps.sequence(), []string{"isinstalled", "fetch", "prepare", "sign", "install", "launch"}; !equalStrings(got, want) {
		t.Fatalf("a failed install check must not skip stages, got %v", got)
	}
}

func TestJobEventSeqIsMonotonic(t *testing.T) {
	m, h := testJobManager(t, &fakeSteps{})
	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	h.waitResult(t)

	seqs := h.seqs()
	if len(seqs) < 3 {
		t.Fatalf("expected several progress events, got %d", len(seqs))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("seq not monotonic at %d: %v", i, seqs)
		}
	}
	if seqs[0] != 1 {
		t.Errorf("first seq = %d, want 1", seqs[0])
	}
}

func TestJobStopsAtVerifyOnDigestMismatch(t *testing.T) {
	steps := &fakeSteps{fetchErr: errSHAMismatch}
	m, h := testJobManager(t, steps)
	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	res := h.waitResult(t)

	if res.GetOk() {
		t.Fatal("a digest mismatch must fail the job")
	}
	if res.GetErrorCode() != "sha256_mismatch" {
		t.Fatalf("error code = %q, want sha256_mismatch", res.GetErrorCode())
	}
	if res.GetRetryable() {
		t.Error("a digest mismatch is a supply-chain failure, not retryable")
	}
	if got := steps.sequence(); len(got) != 2 || got[0] != "isinstalled" || got[1] != "fetch" {
		t.Fatalf("nothing after fetch may run: %v", got)
	}
}

func TestJobSurfacesUnauthorizedSigning(t *testing.T) {
	steps := &fakeSteps{signPrepErr: errASCUnauthorized}
	m, h := testJobManager(t, steps)
	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	res := h.waitResult(t)

	if res.GetErrorCode() != "asc_unauthorized" {
		t.Fatalf("error code = %q, want asc_unauthorized", res.GetErrorCode())
	}
	if res.GetRetryable() {
		t.Error("bad credentials need operator action, not a retry")
	}
}

func TestJobMarksDeveloperModeRequired(t *testing.T) {
	steps := &fakeSteps{launchErr: errDeveloperModeRequired}
	m, h := testJobManager(t, steps)
	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	res := h.waitResult(t)

	if res.GetErrorCode() != "developer_mode_required" {
		t.Fatalf("error code = %q, want developer_mode_required", res.GetErrorCode())
	}
	if res.GetStageReached() != agentcomposev2.IosJobStage_IOS_JOB_STAGE_WAITING_READY {
		t.Errorf("stage reached = %v, want WAITING_READY", res.GetStageReached())
	}
}

func TestJobPresignedSkipsSigning(t *testing.T) {
	steps := &fakeSteps{assets: SigningAssets{Presigned: true}}
	m, h := testJobManager(t, steps)
	req := jobRequest()
	req.Action = agentcomposev2.IosWdaJobAction_IOS_WDA_JOB_ACTION_INSTALL_SIGNED
	req.Signing = &agentcomposev2.NodeIosSigningMaterial{Mode: agentcomposev2.IosSigningMode_IOS_SIGNING_MODE_PRESIGNED}

	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	res := h.waitResult(t)
	if !res.GetOk() {
		t.Fatalf("presigned install failed: %s", res.GetErrorCode())
	}
	for _, c := range steps.sequence() {
		if c == "sign" {
			t.Fatal("the presigned path must not re-sign the artifact")
		}
	}
}

func TestJobDeveloperModeStageOnlyWhenRequested(t *testing.T) {
	steps := &fakeSteps{}
	m, h := testJobManager(t, steps)
	req := jobRequest()
	req.EnableDeveloperMode = true
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	h.waitResult(t)

	sawDevMode := false
	for _, c := range steps.sequence() {
		if c == "devmode" {
			sawDevMode = true
		}
	}
	if !sawDevMode {
		t.Fatal("enable_developer_mode=true must call the step")
	}
	if !containsStage(h.stages(), agentcomposev2.IosJobStage_IOS_JOB_STAGE_ENABLING_DEVELOPER_MODE) {
		t.Fatal("the developer-mode stage must appear in the event stream")
	}
}

func TestJobDeveloperModeFailureIsNotFatal(t *testing.T) {
	steps := &fakeSteps{devModeErr: errors.New("device refused")}
	m, h := testJobManager(t, steps)
	req := jobRequest()
	req.EnableDeveloperMode = true
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	res := h.waitResult(t)
	if !res.GetOk() {
		t.Fatalf("a developer-mode request failure must not fail the job (the launch stage decides): %s", res.GetErrorCode())
	}
}

func TestJobCancelStopsAtStageBoundary(t *testing.T) {
	block := make(chan struct{})
	steps := &fakeSteps{blockLaunch: block}
	m, h := testJobManager(t, steps)
	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	// Wait until the job reaches Launch, then cancel it.
	waitFor(t, func() bool {
		for _, c := range steps.sequence() {
			if c == "launch" {
				return true
			}
		}
		return false
	}, "job to reach launch")

	if err := m.Cancel("job-1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	res := h.waitResult(t)
	close(block)

	if res.GetOk() {
		t.Fatal("a cancelled job must not report ok")
	}
	// Launch returns ctx.Err(), which the engine classifies as a launch failure;
	// what matters is that the job terminates promptly with a result frame.
	if res.GetErrorCode() == "" {
		t.Fatal("a cancelled job must carry an error code")
	}
}

func TestJobCancelUnknownIsNoError(t *testing.T) {
	m, _ := testJobManager(t, &fakeSteps{})
	if err := m.Cancel("no-such-job"); err != nil {
		t.Fatalf("cancelling an already-finished job must not error: %v", err)
	}
}

func TestJobStartIsIdempotent(t *testing.T) {
	block := make(chan struct{})
	steps := &fakeSteps{blockLaunch: block}
	m, h := testJobManager(t, steps)
	req := jobRequest()
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(m.ActiveJobs()) == 1 }, "job to register")
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatalf("a retried dispatch must be accepted: %v", err)
	}
	if n := len(m.ActiveJobs()); n != 1 {
		t.Fatalf("active jobs = %d, want 1 — a retry must not start a second pipeline", n)
	}
	close(block)
	h.waitResult(t)
}

func TestJobRequiresIDAndUDID(t *testing.T) {
	m, _ := testJobManager(t, &fakeSteps{})
	if err := m.Start(context.Background(), &agentcomposev2.NodeIosWdaJobRequest{Udid: "udid-a"}); err == nil {
		t.Error("a job without job_id must be rejected")
	}
	if err := m.Start(context.Background(), &agentcomposev2.NodeIosWdaJobRequest{JobId: "j"}); err == nil {
		t.Error("a job without udid must be rejected")
	}
}

func TestJobMissingArtifactFails(t *testing.T) {
	m, h := testJobManager(t, &fakeSteps{})
	req := jobRequest()
	req.Artifact = nil
	if err := m.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	res := h.waitResult(t)
	if res.GetErrorCode() != "artifact_missing" {
		t.Fatalf("error code = %q, want artifact_missing", res.GetErrorCode())
	}
}

func TestRedactStripsKeyMaterial(t *testing.T) {
	err := errors.New("apple rejected key -----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----")
	got := redactErr(err)
	if strings.Contains(got, "secret") || strings.Contains(got, "BEGIN PRIVATE KEY") {
		t.Fatalf("key material leaked into a user-visible error: %q", got)
	}
	if !strings.Contains(got, "redacted") {
		t.Fatalf("redaction marker missing: %q", got)
	}
}

func TestJobResultEmittedExactlyOnce(t *testing.T) {
	m, h := testJobManager(t, &fakeSteps{})
	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	h.waitResult(t)
	waitFor(t, func() bool { return len(m.ActiveJobs()) == 0 }, "job to deregister")

	h.mu.Lock()
	n := len(h.results)
	h.mu.Unlock()
	if n != 1 {
		t.Fatalf("result frames = %d, want exactly 1 (the server's future depends on it)", n)
	}
}

// ── wait-for-device stage ────────────────────────────────────────────────
//
// A job for an unplugged phone must park, not fail deep in the pipeline with a
// bare device_not_present. Presence is injected so the state machine is tested
// without usbmuxd.

func TestJobWaitsForAbsentDeviceThenProceeds(t *testing.T) {
	steps := &fakeSteps{assets: SigningAssets{}}
	m, h := testJobManager(t, steps)
	m.PresentPollInterval = 10 * time.Millisecond

	present := false
	m.IsPresent = func(string) bool { return present }

	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	// The phone comes back mid-wait.
	time.AfterFunc(50*time.Millisecond, func() { present = true })

	res := h.waitResult(t)
	if !res.GetOk() {
		t.Fatalf("job should complete after the device comes back, got error_code=%s", res.GetErrorCode())
	}
}

func TestJobWaitTimesOutWhenDeviceNeverReturns(t *testing.T) {
	m, h := testJobManager(t, &fakeSteps{})
	m.PresentPollInterval = 10 * time.Millisecond
	m.PresentWaitTimeout = 50 * time.Millisecond

	m.IsPresent = func(string) bool { return false }

	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	res := h.waitResult(t)

	if res.GetOk() {
		t.Fatal("job should fail when the device never comes back")
	}
	if res.GetErrorCode() != "cancelled" {
		t.Fatalf("error_code = %s, want cancelled (the user can re-queue freely)", res.GetErrorCode())
	}
	if stage := res.GetStageReached(); stage != agentcomposev2.IosJobStage_IOS_JOB_STAGE_QUEUED {
		t.Fatalf("stage_reached = %s, want QUEUED (nothing was touched)", stage)
	}
}

func TestJobCancelEndsWait(t *testing.T) {
	m, h := testJobManager(t, &fakeSteps{})
	m.PresentPollInterval = 10 * time.Millisecond
	m.PresentWaitTimeout = time.Minute // would outlive the test

	m.IsPresent = func(string) bool { return false }

	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel("job-1"); err != nil {
		t.Fatal(err)
	}
	res := h.waitResult(t)

	if res.GetErrorCode() != "cancelled" {
		t.Fatalf("error_code = %s, want cancelled", res.GetErrorCode())
	}
}

func TestJobPresentDeviceSkipsWait(t *testing.T) {
	// The common case: no wait events between QUEUED and DOWNLOADING.
	m, h := testJobManager(t, &fakeSteps{})
	m.IsPresent = func(string) bool { return true }

	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	h.waitResult(t)

	for _, e := range h.events {
		if e.GetMessage() == "waiting for the device to come online" {
			t.Fatal("present device must not emit wait events")
		}
	}
}

// ── OnActiveChanged（job 期间暂停隧道对账） ────────────────────────────────
//
// go-ios 的 UpdateTunnels 在一次 ListDevices 失败时会把健康隧道整个拆掉，砸烂
// 进行中的 WDA 启动（日志实锤：install 100% → 3s 后 wda_unreachable）。所以
// job 引擎在 0→1 / 1→0 时通知调用方暂停/恢复隧道轮询。

func TestOnActiveChangedFiresTrueThenFalse(t *testing.T) {
	var mu sync.Mutex
	var transitions []bool
	m, h := testJobManager(t, &fakeSteps{})
	m.OnActiveChanged = func(running bool) {
		mu.Lock()
		transitions = append(transitions, running)
		mu.Unlock()
	}

	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	h.waitResult(t)
	// The 1→0 fires from the job goroutine's defer; give it a moment.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(transitions)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(transitions) != 2 || transitions[0] != true || transitions[1] != false {
		t.Fatalf("transitions = %v, want [true false]", transitions)
	}
}

func TestOnActiveChangedNotPerJobWhileOverlapping(t *testing.T) {
	// Two overlapping jobs: the second Start must NOT fire another true (the
	// tunnel reconcile must only resume when the LAST job drains).
	var mu sync.Mutex
	var transitions []bool
	m, h := testJobManager(t, &fakeSteps{})
	m.OnActiveChanged = func(running bool) {
		mu.Lock()
		transitions = append(transitions, running)
		mu.Unlock()
	}

	req2 := jobRequest()
	req2.JobId = "job-2"
	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), req2); err != nil {
		t.Fatal(err)
	}
	// Both still running: only ONE true so far.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(transitions)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	if len(transitions) != 1 || transitions[0] != true {
		t.Fatalf("transitions while both running = %v, want exactly one [true]", transitions)
	}
	mu.Unlock()

	m.Cancel("job-1")
	m.Cancel("job-2")
	h.waitResult(t)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(transitions)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(transitions) != 2 || transitions[1] != false {
		t.Fatalf("transitions = %v, want [true false] — one resume only after BOTH drained", transitions)
	}
}

func TestOnTerminalCarriesRedactedMessage(t *testing.T) {
	// 失败原因（redactErr 后的文本）必须进 OnTerminal——设备行只存 code 时
	// 控制台只能显示「初始化异常：wda_unreachable」，用户看不到真因。
	// 用 fakeSteps 注入一个 launch 错误，走真实 finish() 链路。
	//
	// 语义（2026-10 状态机拆分）：launch 验证失败但 runner 已装时，OnTerminal
	// 报 ok=true + installed=true + 原因——设备落 INSTALLED（待控制）而非
	// FAILED；job 结果帧仍报 ok=false + errCode 供控制台显示。
	boom := errors.New("runner control check: dial tcp 127.0.0.1:27015: connectex: timeout")
	m, h := testJobManager(t, &fakeSteps{launchErr: boom})
	var mu sync.Mutex
	var gotMsg, gotCode string
	var gotOK, gotInstalled bool
	// waitResult 只等结果帧；OnTerminal 在 finish() 里于帧之后调用——用 channel
	// 同步，否则本测试会在回调落盘前读 gotOK（竞态：单独跑快而过、全量跑慢而挂）。
	terminalDone := make(chan struct{})
	m.OnTerminal = func(_ string, ok bool, _ bool, installed bool, errCode string, message string, _ string) {
		mu.Lock()
		gotOK, gotInstalled, gotCode, gotMsg = ok, installed, errCode, message
		mu.Unlock()
		close(terminalDone)
	}

	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	h.waitResult(t)
	<-terminalDone

	mu.Lock()
	defer mu.Unlock()
	if !gotOK {
		t.Fatal("terminal must report initialization success (runner installed)")
	}
	if !gotInstalled {
		t.Fatal("terminal must carry installed=true so the device lands INSTALLED, not FAILED")
	}
	if gotCode != "wda_unreachable" {
		t.Fatalf("error_code = %q, want wda_unreachable", gotCode)
	}
	if gotMsg != boom.Error() {
		t.Fatalf("OnTerminal message = %q, want the redacted reason", gotMsg)
	}
}

func TestOnTerminalInstallFailureIsFailed(t *testing.T) {
	// 安装失败（没装上 runner）：OnTerminal 必须报 installed=false + FAILED，
	// 没有可提升的 runner，设备落 FAILED（老语义）。
	m, h := testJobManager(t, &fakeSteps{installErr: errors.New("zipconduit broke")})
	var mu sync.Mutex
	var gotOK, gotInstalled bool
	terminalDone := make(chan struct{})
	m.OnTerminal = func(_ string, ok bool, _ bool, installed bool, _ string, _ string, _ string) {
		mu.Lock()
		gotOK, gotInstalled = ok, installed
		mu.Unlock()
		close(terminalDone)
	}

	if err := m.Start(context.Background(), jobRequest()); err != nil {
		t.Fatal(err)
	}
	h.waitResult(t)
	<-terminalDone

	mu.Lock()
	defer mu.Unlock()
	if gotOK {
		t.Fatal("install failure must report ok=false")
	}
	if gotInstalled {
		t.Fatal("install failure must report installed=false")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsStage(stages []agentcomposev2.IosJobStage, want agentcomposev2.IosJobStage) bool {
	for _, s := range stages {
		if s == want {
			return true
		}
	}
	return false
}
