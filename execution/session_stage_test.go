package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// stageRecorder captures the NodeSessionStage frames a session manager emits so
// a test can assert on the sequence of steps, not just the final error.
type stageRecorder struct {
	mu     sync.Mutex
	frames []*agentcomposev2.NodeSessionStage
}

func (r *stageRecorder) emit(frame *agentcomposev2.NodeUpstreamFrame) error {
	stage := frame.GetSessionStage()
	if stage == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, stage)
	return nil
}

// names returns "STAGE:ok" / "STAGE:fail" in emission order, which is what the
// assertions care about (the sequence and which entry flipped to failed).
func (r *stageRecorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.frames))
	for _, f := range r.frames {
		verdict := "ok"
		if !f.GetOk() {
			verdict = "fail"
		}
		out = append(out, f.GetStage().String()+":"+verdict)
	}
	return out
}

func (r *stageRecorder) find(stage agentcomposev2.SessionStage, ok bool) *agentcomposev2.NodeSessionStage {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range r.frames {
		if f.GetStage() == stage && f.GetOk() == ok {
			return f
		}
	}
	return nil
}

// waitForStage polls until the recorder has at least one stage frame, or timeout.
func (r *stageRecorder) waitForStage(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		count := len(r.frames)
		r.mu.Unlock()
		if count > 0 {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// waitForFail polls until the recorder has a failed frame for the given stage.
func (r *stageRecorder) waitForFail(stage agentcomposev2.SessionStage, timeout time.Duration) *agentcomposev2.NodeSessionStage {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f := r.find(stage, false); f != nil {
			return f
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// waitProvisioningDone polls until a session finishes provisioning (success or failure).
// For success: looks for a non-provisioning state. For failure: looks for a failed stage.
func waitProvisioningDone(t *testing.T, m *sessionManager, sessionID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		sess, ok := m.sessions[sessionID]
		m.mu.Unlock()
		if !ok {
			// Session was removed after provisioning failure
			return
		}
		sess.mu.Lock()
		state := sess.state
		sess.mu.Unlock()
		if state != sessionProvisioning {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func newStageManager(t *testing.T) (*sessionManager, *stageRecorder) {
	t.Helper()
	rec := &stageRecorder{}
	m := newSessionManager(
		sessionOptions{workRoot: t.TempDir(), providers: []string{"claude"}},
		testLogger(),
		noopEmit, noopEmit, noopEmit, rec.emit,
	)
	return m, rec
}

// A session with no repo still reports that it prepared its workspace: the
// stage stream must describe every run, so the UI has a step to show from the
// first moment rather than only when something breaks.
func TestCreateReportsWorkspaceStage(t *testing.T) {
	m, rec := newStageManager(t)
	spec := &agentcomposev2.NodeCreateSession{
		SessionId:  "sess-stage-1",
		Provider:   "claude",
		DeferStart: true, // no runtime needed: this test is about create()'s stages
	}
	if err := m.create(context.Background(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	// create() now returns immediately and runs provisioning in background.
	// Wait for at least one stage frame to appear.
	if !rec.waitForStage(5 * time.Second) {
		t.Fatal("no stage frames emitted within 5s")
	}

	// One frame per stage *entered*; a stage is implicitly done once the next one
	// starts (and RUNNING marks the whole bring-up complete). Only failures emit a
	// second frame for the same stage, so the healthy path stays cheap on the wire.
	got := rec.names()
	want := []string{"SESSION_STAGE_WORKSPACE_PREPARE:ok"}
	if len(got) != len(want) {
		t.Fatalf("stages = %v, want exactly %v", got, want)
	}
	if got[0] != want[0] {
		t.Fatalf("stages = %v, want %v", got, want)
	}
	if rec.find(agentcomposev2.SessionStage_SESSION_STAGE_GIT_CLONE, true) != nil {
		t.Error("a session with no repo must not report a git clone stage")
	}
}

// A clone failure must be attributed to the git stage, and the proxy token
// embedded in the clone URL's userinfo must never appear in the reported text.
func TestCreateReportsGitCloneFailureRedacted(t *testing.T) {
	m, rec := newStageManager(t)
	const token = "SUPERSECRETPROXYTOKEN"
	spec := &agentcomposev2.NodeCreateSession{
		SessionId:  "sess-stage-2",
		Provider:   "claude",
		DeferStart: true,
		Git: &agentcomposev2.NodeGitSpec{
			// Unroutable host: git fails fast rather than hanging, and
			// GIT_TERMINAL_PROMPT=0 keeps it from waiting on credentials.
			Url: "http://" + token + "@127.0.0.1:1/nope/repo.git",
		},
	}
	// create() now returns immediately; the clone failure happens in background.
	if err := m.create(context.Background(), spec); err != nil {
		t.Fatalf("create returned error (should be async now): %v", err)
	}
	// Wait for the git clone stage to fail (polling stage recorder, not session state).
	failed := rec.waitForFail(agentcomposev2.SessionStage_SESSION_STAGE_GIT_CLONE, 10*time.Second)
	if failed == nil {
		t.Fatalf("expected a failed git clone stage within 10s, got %v", rec.names())
	}
	if failed.GetError() == "" {
		t.Error("a failed stage must carry the reason it failed")
	}
	// The token travels in the clone URL, so it can reach the stage text via the
	// git command line and git's own stderr. Both must be redacted.
	for label, text := range map[string]string{
		"error":  failed.GetError(),
		"detail": failed.GetDetail(),
	} {
		if strings.Contains(text, token) {
			t.Errorf("stage %s leaked the proxy token: %q", label, text)
		}
	}
	// Provisioning strips the URL userinfo and re-supplies the credential as an
	// http.extraHeader, so that header value — not a URL userinfo — is what the
	// redactor must blank in the echoed argv.
	if !strings.Contains(failed.GetError(), "Basic ***") {
		t.Errorf("expected redacted auth header in error, got %q", failed.GetError())
	}
	// The clean clone URL must still be visible: an operator needs to see which
	// remote failed, just without the credential.
	if !strings.Contains(failed.GetError(), "http://127.0.0.1:1/nope/repo.git") {
		t.Errorf("expected the credential-free clone URL in error, got %q", failed.GetError())
	}
	// The workspace step ran before the clone, so it must be reported as done —
	// that ordering is what lets the UI point at the step that broke.
	if rec.find(agentcomposev2.SessionStage_SESSION_STAGE_WORKSPACE_PREPARE, true) == nil {
		t.Errorf("workspace stage should have completed before the clone, got %v", rec.names())
	}
}

// The pre-flight stage is where "the node cannot run the agent" surfaces. It
// must be reported as its own failed step rather than collapsing into a generic
// start error, since that is the case operators hit most often.
func TestStartRuntimeReportsPreflightFailure(t *testing.T) {
	m, rec := newStageManager(t)
	spec := &agentcomposev2.NodeCreateSession{
		SessionId:  "sess-stage-3",
		Provider:   "not-installed-provider", // fails selectExecutor's provider check
		DeferStart: true,
	}
	if err := m.create(context.Background(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	// create() now returns immediately and provisions in the background; wait for
	// the placeholder to become a real session before starting the runtime.
	waitProvisioningDone(t, m, "sess-stage-3", 5*time.Second)
	if err := m.startRuntime("sess-stage-3"); err == nil {
		t.Fatal("expected startRuntime to fail for an unavailable provider")
	}

	failed := rec.find(agentcomposev2.SessionStage_SESSION_STAGE_RUNTIME_PREFLIGHT, false)
	if failed == nil {
		t.Fatalf("expected a failed runtime preflight stage, got %v", rec.names())
	}
	if !strings.Contains(failed.GetError(), "not-installed-provider") {
		t.Errorf("preflight error should name the unsatisfied requirement, got %q", failed.GetError())
	}
	// Nothing was spawned, so the start stage must not claim to have run.
	if rec.find(agentcomposev2.SessionStage_SESSION_STAGE_RUNTIME_START, true) != nil {
		t.Error("runtime start must not be reported when pre-flight failed")
	}
}

// Every stage frame must identify its session and carry a timestamp: the server
// correlates frames to a task by session id, and orders them by created_at.
func TestStageFramesCarrySessionIDAndTimestamp(t *testing.T) {
	m, rec := newStageManager(t)
	spec := &agentcomposev2.NodeCreateSession{
		SessionId:  "sess-stage-4",
		Provider:   "claude",
		DeferStart: true,
	}
	if err := m.create(context.Background(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	// create() now returns immediately and provisions in the background; wait for
	// at least one stage frame to arrive before asserting on the sequence.
	if !rec.waitForStage(5 * time.Second) {
		t.Fatal("expected at least one stage frame")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.frames) == 0 {
		t.Fatal("expected at least one stage frame")
	}
	for i, f := range rec.frames {
		if f.GetSessionId() != "sess-stage-4" {
			t.Errorf("frame %d session id = %q, want sess-stage-4", i, f.GetSessionId())
		}
		if f.GetCreatedAt() == "" {
			t.Errorf("frame %d has no created_at", i)
		}
	}
}
