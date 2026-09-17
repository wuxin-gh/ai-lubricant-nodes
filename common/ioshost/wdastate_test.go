package ioshost

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// fakeOnReport captures inventory snapshots so a test can assert wda_state /
// wda_progress were carried on the device report.
type fakeOnReport struct {
	mu        sync.Mutex
	last      *agentcomposev2.NodeIosDevicesReport
	reportCnt int
}

func (f *fakeOnReport) emit(rep *agentcomposev2.NodeIosDevicesReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = rep
	f.reportCnt++
	return nil
}

func (f *fakeOnReport) snapshot() *agentcomposev2.NodeIosDevice {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil || len(f.last.Devices) == 0 {
		return nil
	}
	return f.last.Devices[0]
}

func (f *fakeOnReport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reportCnt
}

// newTestManager builds a DeviceManager with one claimed device and a fake
// report sink, so the Note* write-back methods have something to act on.
func newTestManager(t *testing.T, udid string) (*DeviceManager, *fakeOnReport) {
	t.Helper()
	rep := &fakeOnReport{}
	m := NewDeviceManager(ManagerConfig{
		Logger:     testLogger(),
		ConfigPath: "", // devices.json absent → empty config is fine
		NodeID:     "node-test",
		OnReport:   rep.emit,
	})
	// Seed a claimed device directly so Note* can find it by UDID. adoptConfiguredDevices
	// reads devices.json; we skip that and inject the device the way Claim would
	// leave it.
	m.mu.Lock()
	m.devices[udid] = &managedDevice{
		udid:     udid,
		name:     "test-phone",
		claimed:  true,
		wdaState: agentcomposev2.IosWdaState_IOS_WDA_STATE_UNSPECIFIED,
	}
	m.mu.Unlock()
	return m, rep
}

// ── NoteWdaJobStarted ──────────────────────────────────────────────────────

func TestNoteWdaJobStartedSetsPreparing(t *testing.T) {
	m, rep := newTestManager(t, "UDID-1")
	m.NoteWdaJobStarted("UDID-1")

	d := rep.snapshot()
	if d == nil {
		t.Fatal("expected a report after NoteWdaJobStarted")
	}
	if d.WdaState != agentcomposev2.IosWdaState_IOS_WDA_STATE_PREPARING {
		t.Fatalf("wda_state = %v, want PREPARING", d.WdaState)
	}
	if d.WdaProgress != 0 {
		t.Fatalf("wda_progress = %d, want 0 at start", d.WdaProgress)
	}
}

func TestNoteWdaJobStartedUnknownDeviceIsNoop(t *testing.T) {
	m, rep := newTestManager(t, "UDID-1")
	before := rep.count()
	m.NoteWdaJobStarted("not-a-real-udid")
	if rep.count() != before {
		t.Fatal("report fired for an unknown device; should be a silent no-op")
	}
}

// ── NoteWdaJobProgress ─────────────────────────────────────────────────────

func TestNoteWdaJobProgressUpdatesPercentAndStage(t *testing.T) {
	m, rep := newTestManager(t, "UDID-1")
	m.NoteWdaJobStarted("UDID-1")
	rep.count() // drain the start report

	m.NoteWdaJobProgress("UDID-1", 42, "downloading")
	d := rep.snapshot()
	if d.WdaProgress != 42 || d.WdaStage != "downloading" {
		t.Fatalf("got progress=%d stage=%q, want 42/downloading", d.WdaProgress, d.WdaStage)
	}
	if d.WdaState != agentcomposev2.IosWdaState_IOS_WDA_STATE_PREPARING {
		t.Fatalf("progress must not change state, got %v", d.WdaState)
	}
}

func TestNoteWdaJobProgressClampsTo100(t *testing.T) {
	m, rep := newTestManager(t, "UDID-1")
	m.NoteWdaJobStarted("UDID-1")
	rep.count()

	m.NoteWdaJobProgress("UDID-1", 250, "verifying")
	if d := rep.snapshot(); d.WdaProgress != 100 {
		t.Fatalf("progress = %d, want clamped to 100", d.WdaProgress)
	}
}

func TestNoteWdaJobProgressIgnoredAfterTerminal(t *testing.T) {
	// A late progress event must not resurrect a finished job's PREPARING state.
	m, rep := newTestManager(t, "UDID-1")
	m.NoteWdaJobStarted("UDID-1")
	m.NoteWdaJobResult("UDID-1", true, false, "", "2099-01-01T00:00:00Z")
	rep.count()

	m.NoteWdaJobProgress("UDID-1", 50, "downloading")
	d := rep.snapshot()
	if d.WdaState != agentcomposev2.IosWdaState_IOS_WDA_STATE_READY {
		t.Fatalf("late progress changed READY back; got %v", d.WdaState)
	}
	if d.WdaProgress != 0 {
		t.Fatalf("late progress wrote percent onto a READY device; got %d", d.WdaProgress)
	}
}

// ── NoteWdaJobResult ───────────────────────────────────────────────────────

func TestNoteWdaJobResultSuccessSetsReady(t *testing.T) {
	m, rep := newTestManager(t, "UDID-1")
	m.NoteWdaJobStarted("UDID-1")
	rep.count()

	m.NoteWdaJobResult("UDID-1", true, false, "", "2099-01-01T00:00:00Z")
	d := rep.snapshot()
	if d.WdaState != agentcomposev2.IosWdaState_IOS_WDA_STATE_READY {
		t.Fatalf("wda_state = %v, want READY", d.WdaState)
	}
	if d.ProfileExpiresAt != "2099-01-01T00:00:00Z" {
		t.Fatalf("profile_expires_at = %q, want recorded", d.ProfileExpiresAt)
	}
	if d.WdaProgress != 0 || d.WdaStage != "" {
		t.Fatalf("progress/stage not cleared on terminal: %d/%q", d.WdaProgress, d.WdaStage)
	}
}

func TestNoteWdaJobResultFailureSetsFailed(t *testing.T) {
	m, rep := newTestManager(t, "UDID-1")
	m.NoteWdaJobStarted("UDID-1")
	rep.count()

	m.NoteWdaJobResult("UDID-1", false, false, "signing_failed", "")
	d := rep.snapshot()
	if d.WdaState != agentcomposev2.IosWdaState_IOS_WDA_STATE_FAILED {
		t.Fatalf("wda_state = %v, want FAILED", d.WdaState)
	}
	if d.LastError != "signing_failed" {
		t.Fatalf("last_error = %q, want error_code recorded", d.LastError)
	}
}

func TestNoteWdaJobResultCancelledDoesNotFail(t *testing.T) {
	// A user-initiated cancel while PREPARING must revert to MISSING (待初始化),
	// not FAILED. Otherwise "cancel" looks like a fault and blocks retry.
	m, rep := newTestManager(t, "UDID-1")
	m.NoteWdaJobStarted("UDID-1")
	rep.count()

	m.NoteWdaJobResult("UDID-1", false, true, "cancelled", "")
	d := rep.snapshot()
	if d.WdaState == agentcomposev2.IosWdaState_IOS_WDA_STATE_FAILED {
		t.Fatal("cancel must not set FAILED")
	}
	if d.WdaState != agentcomposev2.IosWdaState_IOS_WDA_STATE_MISSING {
		t.Fatalf("wda_state = %v, want MISSING after cancel of a PREPARING job", d.WdaState)
	}
}

func TestNoteWdaJobResultCancelledPreservesReady(t *testing.T) {
	// Cancelling a renew on an already-READY device must NOT discard the working
	// WDA. State stays READY. noteWdaState is a no-op (READY→READY), so read the
	// manager's in-memory state directly rather than the report.
	m, _ := newTestManager(t, "UDID-1")
	m.mu.Lock()
	m.devices["UDID-1"].wdaState = agentcomposev2.IosWdaState_IOS_WDA_STATE_READY
	m.mu.Unlock()

	m.NoteWdaJobResult("UDID-1", false, true, "cancelled", "")
	m.mu.Lock()
	got := m.devices["UDID-1"].wdaState
	m.mu.Unlock()
	if got != agentcomposev2.IosWdaState_IOS_WDA_STATE_READY {
		t.Fatalf("cancel of a READY-renew dropped state; got %v", got)
	}
}

// ── report-on-change ───────────────────────────────────────────────────────

func TestNoReportWhenDeviceUnchanged(t *testing.T) {
	// A progress event whose percent+stage are identical to the current value
	// must not fire a report (avoids a storm on no-op progress ticks).
	m, rep := newTestManager(t, "UDID-1")
	m.NoteWdaJobStarted("UDID-1")
	m.NoteWdaJobProgress("UDID-1", 10, "downloading")
	count := rep.count()

	m.NoteWdaJobProgress("UDID-1", 10, "downloading") // identical
	if rep.count() != count {
		t.Fatalf("no-op progress fired a report: %d → %d", count, rep.count())
	}
}

// ── toProto carries the new fields ─────────────────────────────────────────

func TestToProtoCarriesProgressFields(t *testing.T) {
	d := &managedDevice{
		udid:        "X",
		wdaState:    agentcomposev2.IosWdaState_IOS_WDA_STATE_PREPARING,
		wdaProgress: 73,
		wdaStage:    "installing",
	}
	p := d.toProto()
	if p.WdaProgress != 73 || p.WdaStage != "installing" {
		t.Fatalf("toProto dropped progress: %d/%q", p.WdaProgress, p.WdaStage)
	}
	if p.WdaState != agentcomposev2.IosWdaState_IOS_WDA_STATE_PREPARING {
		t.Fatalf("wda_state not carried: %v", p.WdaState)
	}
}

// testLogger returns a no-op slog.Logger for tests (the manager logs but tests
// don't assert on it).
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// satisfy unused imports if the file is the only one importing them.
var _ = context.Background