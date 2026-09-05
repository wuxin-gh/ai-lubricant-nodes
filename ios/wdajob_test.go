package main

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
		close(h.done)
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
	if got, want := steps.sequence(), []string{"fetch", "prepare", "sign", "install", "launch"}; !equalStrings(got, want) {
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
	if got := steps.sequence(); len(got) != 1 || got[0] != "fetch" {
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
