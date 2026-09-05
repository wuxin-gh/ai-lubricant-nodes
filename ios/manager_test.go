package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"device-control/ios/devicecontrol"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// ── test doubles ─────────────────────────────────────────────────────────

// fakeEnumerator serves a scripted device list and a manual event channel, so a
// test drives discovery without usbmuxd.
type fakeEnumerator struct {
	mu       sync.Mutex
	list     []EnumeratedDevice
	listErr  error
	events   chan DeviceEvent
	errs     chan error
	subErr   error
	stops    int
	listCall int
}

func newFakeEnumerator(devices ...EnumeratedDevice) *fakeEnumerator {
	return &fakeEnumerator{
		list:   devices,
		events: make(chan DeviceEvent, 8),
		errs:   make(chan error, 1),
	}
}

func (f *fakeEnumerator) List(context.Context) ([]EnumeratedDevice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCall++
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]EnumeratedDevice, len(f.list))
	copy(out, f.list)
	return out, nil
}

func (f *fakeEnumerator) Subscribe(context.Context) (<-chan DeviceEvent, <-chan error, func(), error) {
	if f.subErr != nil {
		return nil, nil, nil, f.subErr
	}
	return f.events, f.errs, func() {
		f.mu.Lock()
		f.stops++
		f.mu.Unlock()
	}, nil
}

func (f *fakeEnumerator) setList(devices ...EnumeratedDevice) {
	f.mu.Lock()
	f.list = devices
	f.mu.Unlock()
}

// fakePair mints deterministic credentials and records what it was asked to do.
type fakePair struct {
	mu       sync.Mutex
	calls    []string // "server|code|path"
	deviceID string
	err      error
}

func (f *fakePair) Pair(_ context.Context, serverURL, code, credentialPath string) (devicecontrol.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, serverURL+"|"+code+"|"+filepath.Base(credentialPath))
	if f.err != nil {
		return devicecontrol.Credential{}, f.err
	}
	id := f.deviceID
	if id == "" {
		id = "dev-" + fmt.Sprint(len(f.calls))
	}
	// Write the file so a Release(delete_credential=true) has something to remove.
	_ = os.MkdirAll(filepath.Dir(credentialPath), 0o700)
	_ = os.WriteFile(credentialPath, []byte(`{"device_id":"`+id+`"}`), 0o600)
	return devicecontrol.Credential{ServerURL: serverURL, DeviceID: id, Token: "tok"}, nil
}

func (f *fakePair) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeRunner blocks until its context is cancelled, standing in for a live
// device-control connection loop.
type fakeRunner struct {
	mu      sync.Mutex
	started []string // UDIDs, in start order
	running map[string]int
	err     error
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{running: map[string]int{}}
}

func (f *fakeRunner) Run(ctx context.Context, cfg devicecontrol.RunConfig) error {
	f.mu.Lock()
	f.started = append(f.started, cfg.Device.UDID)
	f.running[cfg.Device.UDID]++
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return err
	}
	// Report connected so the manager's online bookkeeping is exercised.
	if cfg.OnState != nil {
		cfg.OnState(devicecontrol.StateConnected)
	}
	<-ctx.Done()
	f.mu.Lock()
	f.running[cfg.Device.UDID]--
	f.mu.Unlock()
	return nil
}

func (f *fakeRunner) startCount(udid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, u := range f.started {
		if u == udid {
			n++
		}
	}
	return n
}

func (f *fakeRunner) liveCount(udid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[udid]
}

// testManager builds a manager with all seams faked and reporting captured.
func testManager(t *testing.T, devices ...EnumeratedDevice) (*DeviceManager, *fakeEnumerator, *fakePair, *fakeRunner, func() []*agentcomposev2.NodeIosDevicesReport) {
	t.Helper()
	dir := t.TempDir()
	enum := newFakeEnumerator(devices...)
	pair := &fakePair{}
	runner := newFakeRunner()

	var mu sync.Mutex
	var reports []*agentcomposev2.NodeIosDevicesReport

	m := NewDeviceManager(ManagerConfig{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ConfigPath:    filepath.Join(dir, "devices.json"),
		DevicesConfig: &DevicesConfig{},
		NodeID:        "node-test",
		Enumerator:    enum,
		Pair:          pair,
		Runner:        runner,
		LookupInfo: func(d EnumeratedDevice) (DeviceInfo, error) {
			return DeviceInfo{Name: "iPhone " + d.UDID[len(d.UDID)-1:], Model: "iPhone15,2", ProductVersion: "17.4"}, nil
		},
		ReportInterval: time.Hour, // no periodic rescan during tests
		OnReport: func(rep *agentcomposev2.NodeIosDevicesReport) error {
			mu.Lock()
			reports = append(reports, rep)
			mu.Unlock()
			return nil
		},
	})
	return m, enum, pair, runner, func() []*agentcomposev2.NodeIosDevicesReport {
		mu.Lock()
		defer mu.Unlock()
		out := make([]*agentcomposev2.NodeIosDevicesReport, len(reports))
		copy(out, reports)
		return out
	}
}

func claimReq(udid string) *agentcomposev2.NodeIosClaimDevice {
	return &agentcomposev2.NodeIosClaimDevice{
		RequestId:   "req-1",
		Udid:        udid,
		DeviceLabel: "My iPhone",
		PairingCode: "ABCD-EFGH",
		ServerUrl:   "https://example.test",
		Transport:   "usb",
	}
}

// ── discovery ────────────────────────────────────────────────────────────

func TestRescanReportsDiscoveredDevices(t *testing.T) {
	m, _, _, _, reports := testManager(t,
		EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1},
		EnumeratedDevice{UDID: "udid-b", ConnectionType: "Network", DeviceID: 2},
	)
	m.Rescan(context.Background())

	got := reports()
	if len(got) == 0 {
		t.Fatal("expected an inventory report after rescan")
	}
	last := got[len(got)-1]
	if len(last.Devices) != 2 {
		t.Fatalf("devices = %d, want 2", len(last.Devices))
	}
	// Sorted by UDID, so a is first.
	a, b := last.Devices[0], last.Devices[1]
	if a.GetUdid() != "udid-a" || !a.GetPresent() || a.GetClaimed() {
		t.Fatalf("device a = %+v, want present + unclaimed", a)
	}
	if a.GetConnectionType() != agentcomposev2.IosConnectionType_IOS_CONNECTION_TYPE_USB {
		t.Fatalf("device a connection = %v, want USB", a.GetConnectionType())
	}
	if b.GetConnectionType() != agentcomposev2.IosConnectionType_IOS_CONNECTION_TYPE_NETWORK {
		t.Fatalf("device b connection = %v, want NETWORK", b.GetConnectionType())
	}
	if a.GetModel() != "iPhone15,2" || a.GetProductVersion() != "17.4" {
		t.Fatalf("lockdown detail missing: %+v", a)
	}
}

func TestDetachKeepsClaimButClearsPresent(t *testing.T) {
	m, enum, _, _, reports := testManager(t, EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1})
	ctx := context.Background()
	m.Rescan(ctx)
	if _, err := m.Claim(ctx, claimReq("udid-a")); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Unplug: the device leaves the list.
	enum.setList()
	m.Rescan(ctx)

	last := reports()[len(reports())-1]
	if len(last.Devices) != 1 {
		t.Fatalf("devices = %d, want the claim to survive a detach", len(last.Devices))
	}
	d := last.Devices[0]
	if d.GetPresent() {
		t.Error("present should be false after detach")
	}
	if !d.GetClaimed() {
		t.Error("claim must survive an unplug — unplugging is not releasing")
	}
}

func TestEnumerateErrorSurfacesInReport(t *testing.T) {
	m, enum, _, _, reports := testManager(t)
	enum.listErr = errors.New("usbmuxd not running")
	m.Rescan(context.Background())

	got := reports()
	if len(got) == 0 {
		t.Fatal("expected a report carrying the enumerate error")
	}
	last := got[len(got)-1]
	if last.GetEnumerateError() == "" {
		t.Fatal("enumerate_error must be set so the UI says 'cannot see devices', not 'no devices'")
	}
	if len(last.Devices) != 0 {
		t.Fatalf("devices = %d, want 0 on enumerate failure", len(last.Devices))
	}
}

func TestSnapshotRevisionIsMonotonic(t *testing.T) {
	m, enum, _, _, _ := testManager(t, EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1})
	ctx := context.Background()
	m.Rescan(ctx)
	first := m.Snapshot("").GetSnapshotRevision()

	enum.setList(
		EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1},
		EnumeratedDevice{UDID: "udid-b", ConnectionType: "USB", DeviceID: 2},
	)
	m.Rescan(ctx)
	second := m.Snapshot("").GetSnapshotRevision()
	if second <= first {
		t.Fatalf("snapshot revision did not advance: %d → %d", first, second)
	}
}

// ── claim ────────────────────────────────────────────────────────────────

func TestClaimRedeemsCodeAndStartsDevice(t *testing.T) {
	m, _, pair, runner, _ := testManager(t, EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Rescan(ctx)

	deviceID, err := m.Claim(ctx, claimReq("udid-a"))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if deviceID == "" {
		t.Fatal("Claim returned an empty device id")
	}
	if pair.callCount() != 1 {
		t.Fatalf("pair calls = %d, want 1", pair.callCount())
	}
	waitFor(t, func() bool { return runner.startCount("udid-a") == 1 }, "device loop to start")

	snap := m.Snapshot("")
	d := snap.Devices[0]
	if !d.GetClaimed() || d.GetDeviceId() != deviceID {
		t.Fatalf("device not marked claimed: %+v", d)
	}
	if d.GetName() != "My iPhone" {
		t.Errorf("server label should win over the device's own name, got %q", d.GetName())
	}
}

func TestClaimRejectsUnknownUDID(t *testing.T) {
	m, _, pair, _, _ := testManager(t, EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1})
	ctx := context.Background()
	m.Rescan(ctx)

	if _, err := m.Claim(ctx, claimReq("udid-ghost")); err == nil {
		t.Fatal("claiming a UDID this host never saw must fail")
	}
	if pair.callCount() != 0 {
		t.Fatal("must not redeem a pairing code for hardware we cannot see")
	}
}

func TestClaimTwiceIsRejected(t *testing.T) {
	m, _, pair, _, _ := testManager(t, EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Rescan(ctx)

	if _, err := m.Claim(ctx, claimReq("udid-a")); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if _, err := m.Claim(ctx, claimReq("udid-a")); err == nil {
		t.Fatal("second claim of the same UDID must fail")
	}
	if pair.callCount() != 1 {
		t.Fatalf("pair calls = %d, want 1 — a second claim must not burn another code", pair.callCount())
	}
}

func TestClaimFailureLeavesDeviceUnclaimed(t *testing.T) {
	m, _, pair, runner, _ := testManager(t, EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1})
	pair.err = errors.New("pairing code expired")
	ctx := context.Background()
	m.Rescan(ctx)

	if _, err := m.Claim(ctx, claimReq("udid-a")); err == nil {
		t.Fatal("Claim should fail when redeem fails")
	}
	snap := m.Snapshot("")
	d := snap.Devices[0]
	if d.GetClaimed() {
		t.Fatal("a failed claim must not mark the device claimed")
	}
	if d.GetLastError() == "" {
		t.Fatal("a failed claim must surface last_error")
	}
	if runner.startCount("udid-a") != 0 {
		t.Fatal("a failed claim must not start a device loop")
	}
}

func TestClaimPersistsToDevicesConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "devices.json")
	cfg := &DevicesConfig{}
	enum := newFakeEnumerator(EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1})
	m := NewDeviceManager(ManagerConfig{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ConfigPath:     cfgPath,
		DevicesConfig:  cfg,
		Enumerator:     enum,
		Pair:           &fakePair{},
		Runner:         newFakeRunner(),
		LookupInfo:     func(EnumeratedDevice) (DeviceInfo, error) { return DeviceInfo{}, nil },
		ReportInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Rescan(ctx)
	if _, err := m.Claim(ctx, claimReq("udid-a")); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	reloaded, _, err := LoadDevicesConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Devices) != 1 || reloaded.Devices[0].UDID != "udid-a" {
		t.Fatalf("devices.json not persisted: %+v", reloaded.Devices)
	}
	if reloaded.Devices[0].CredentialPath == "" {
		t.Fatal("persisted device must record its credential path so a restart resumes it")
	}
}

// ── release ──────────────────────────────────────────────────────────────

func TestReleaseStopsDeviceAndDropsClaim(t *testing.T) {
	m, _, _, runner, _ := testManager(t, EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Rescan(ctx)
	deviceID, err := m.Claim(ctx, claimReq("udid-a"))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	waitFor(t, func() bool { return runner.liveCount("udid-a") == 1 }, "device loop to be live")

	if err := m.Release(&agentcomposev2.NodeIosReleaseDevice{DeviceId: deviceID, DeleteCredential: true}); err != nil {
		t.Fatalf("Release: %v", err)
	}
	waitFor(t, func() bool { return runner.liveCount("udid-a") == 0 }, "device loop to stop")

	snap := m.Snapshot("")
	if len(snap.Devices) != 1 {
		t.Fatalf("released device should remain discoverable, got %d devices", len(snap.Devices))
	}
	if snap.Devices[0].GetClaimed() {
		t.Fatal("released device must not be claimed")
	}
}

func TestReleaseUnknownDeviceErrors(t *testing.T) {
	m, _, _, _, _ := testManager(t)
	if err := m.Release(&agentcomposev2.NodeIosReleaseDevice{DeviceId: "nope"}); err == nil {
		t.Fatal("releasing an unknown device must error so the server does not think it succeeded")
	}
}

// ── configure ────────────────────────────────────────────────────────────

func TestConfigureAppliesAndRestartsDevice(t *testing.T) {
	m, _, _, runner, _ := testManager(t, EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Rescan(ctx)
	deviceID, err := m.Claim(ctx, claimReq("udid-a"))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	waitFor(t, func() bool { return runner.startCount("udid-a") == 1 }, "initial start")

	applied, err := m.ConfigureDevice(ctx, &agentcomposev2.NodeIosConfigureDevice{
		DeviceId:         deviceID,
		ConfigRevision:   5,
		Transport:        "network",
		WdaBundleId:      "com.example.WebDriverAgentRunner.xctrunner",
		XctestConfigName: "WebDriverAgentRunner.xctest",
	})
	if err != nil {
		t.Fatalf("ConfigureDevice: %v", err)
	}
	if applied != 5 {
		t.Fatalf("applied revision = %d, want 5", applied)
	}
	waitFor(t, func() bool { return runner.startCount("udid-a") == 2 }, "restart after config change")
}

func TestConfigureIgnoresStaleRevision(t *testing.T) {
	m, _, _, runner, _ := testManager(t, EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Rescan(ctx)
	deviceID, _ := m.Claim(ctx, claimReq("udid-a"))
	waitFor(t, func() bool { return runner.startCount("udid-a") == 1 }, "initial start")

	if _, err := m.ConfigureDevice(ctx, &agentcomposev2.NodeIosConfigureDevice{
		DeviceId: deviceID, ConfigRevision: 7, Transport: "network",
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return runner.startCount("udid-a") == 2 }, "restart for rev 7")

	// A reordered/retried older revision must not roll the host back.
	applied, err := m.ConfigureDevice(ctx, &agentcomposev2.NodeIosConfigureDevice{
		DeviceId: deviceID, ConfigRevision: 3, Transport: "usb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied != 7 {
		t.Fatalf("applied = %d, want the newer revision 7 to stand", applied)
	}
	if runner.startCount("udid-a") != 2 {
		t.Fatal("a stale revision must not restart the device")
	}
}

func TestConfigureUnknownDeviceErrors(t *testing.T) {
	m, _, _, _, _ := testManager(t)
	if _, err := m.ConfigureDevice(context.Background(), &agentcomposev2.NodeIosConfigureDevice{DeviceId: "nope"}); err == nil {
		t.Fatal("configuring an unknown device must error")
	}
}

// ── restart resumption ───────────────────────────────────────────────────

func TestStartClaimedDevicesResumesFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "devices.json")
	cfg := &DevicesConfig{Devices: []DeviceConfig{{
		Name:           "iPhone",
		UDID:           "udid-a",
		Transport:      "usb",
		CredentialPath: filepath.Join(dir, "credentials", "iPhone.json"),
	}}}
	runner := newFakeRunner()
	m := NewDeviceManager(ManagerConfig{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ConfigPath:     cfgPath,
		DevicesConfig:  cfg,
		Enumerator:     newFakeEnumerator(),
		Pair:           &fakePair{},
		Runner:         runner,
		LookupInfo:     func(EnumeratedDevice) (DeviceInfo, error) { return DeviceInfo{}, nil },
		ReportInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.StartClaimedDevices(ctx)
	waitFor(t, func() bool { return runner.startCount("udid-a") == 1 }, "claimed device to resume after restart")

	// The device is not physically present yet, so the report must say so while
	// still showing the claim.
	snap := m.Snapshot("")
	if len(snap.Devices) != 1 {
		t.Fatalf("devices = %d, want the remembered claim", len(snap.Devices))
	}
	if snap.Devices[0].GetPresent() {
		t.Error("a remembered claim must not report present until discovery confirms it")
	}
	if !snap.Devices[0].GetClaimed() {
		t.Error("a device with a credential on disk is claimed")
	}
}

func TestStopCancelsAllDevices(t *testing.T) {
	m, _, _, runner, _ := testManager(t,
		EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1},
		EnumeratedDevice{UDID: "udid-b", ConnectionType: "USB", DeviceID: 2},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Rescan(ctx)
	if _, err := m.Claim(ctx, claimReq("udid-a")); err != nil {
		t.Fatal(err)
	}
	req := claimReq("udid-b")
	req.PairingCode = "IJKL-MNOP"
	if _, err := m.Claim(ctx, req); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return runner.liveCount("udid-a") == 1 && runner.liveCount("udid-b") == 1 }, "both loops live")

	m.Stop()
	waitFor(t, func() bool { return runner.liveCount("udid-a") == 0 && runner.liveCount("udid-b") == 0 }, "all loops stopped")
}

// ── helpers ──────────────────────────────────────────────────────────────

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ── WDA profile expiry transitions (auto-renewal badge source) ────────────

// expiryManager builds a manager whose discovered device carries a wdaState
// and profileExpiresAt directly, without running a WDA job: the test drives
// refreshWdaExpiryLocked through Rescan the way the periodic ticker does.
func expiryManager(t *testing.T, state agentcomposev2.IosWdaState, expiresAt string) (*DeviceManager, *fakeEnumerator) {
	t.Helper()
	m, enum, _, _, _ := testManager(t, EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1})
	m.Rescan(context.Background()) // seeds m.devices["udid-a"]
	m.mu.Lock()
	dev := m.devices["udid-a"]
	dev.present = true
	dev.wdaState = state
	dev.profileExpiresAt = expiresAt
	m.mu.Unlock()
	return m, enum
}

func TestExpiryTransitionToRenewalDue(t *testing.T) {
	// Profile expires in 7 days; the default 14-day window must flip READY →
	// RENEWAL_DUE on the next Rescan.
	m, _ := expiryManager(t, agentcomposev2.IosWdaState_IOS_WDA_STATE_READY,
		time.Now().Add(7*24*time.Hour).UTC().Format(time.RFC3339))
	m.Rescan(context.Background())

	snap := m.Snapshot("")
	if got := snap.Devices[0].GetWdaState(); got != agentcomposev2.IosWdaState_IOS_WDA_STATE_RENEWAL_DUE {
		t.Fatalf("wdaState = %v, want RENEWAL_DUE", got)
	}
}

func TestExpiryRespectsRenewBeforeDays(t *testing.T) {
	// Same 7-day horizon, but the server narrowed the window to 3 days: the
	// device must stay READY.
	m, _ := expiryManager(t, agentcomposev2.IosWdaState_IOS_WDA_STATE_READY,
		time.Now().Add(7*24*time.Hour).UTC().Format(time.RFC3339))
	if _, err := m.ConfigureDevice(context.Background(), &agentcomposev2.NodeIosConfigureDevice{
		DeviceId:       "nonexistent", // does not matter: unknown device errors
		ConfigRevision: 0,
	}); err == nil {
		t.Fatal("expected error for unknown device")
	}
	// Set the window directly the way ConfigureDevice would after a successful
	// configure of udid-a.
	m.mu.Lock()
	m.devices["udid-a"].renewBeforeDays = 3
	m.mu.Unlock()
	m.Rescan(context.Background())

	snap := m.Snapshot("")
	if got := snap.Devices[0].GetWdaState(); got != agentcomposev2.IosWdaState_IOS_WDA_STATE_READY {
		t.Fatalf("wdaState = %v, want READY (window narrowed to 3 days)", got)
	}
}

func TestExpiryDefaultWindowIs14Days(t *testing.T) {
	// Expires in 20 days — outside the default 14-day window: stays READY.
	m, _ := expiryManager(t, agentcomposev2.IosWdaState_IOS_WDA_STATE_READY,
		time.Now().Add(20*24*time.Hour).UTC().Format(time.RFC3339))
	m.Rescan(context.Background())

	snap := m.Snapshot("")
	if got := snap.Devices[0].GetWdaState(); got != agentcomposev2.IosWdaState_IOS_WDA_STATE_READY {
		t.Fatalf("wdaState = %v, want READY", got)
	}
}

func TestExpiryTransitionToExpired(t *testing.T) {
	// Expiry timestamp in the past: READY → EXPIRED.
	m, _ := expiryManager(t, agentcomposev2.IosWdaState_IOS_WDA_STATE_READY,
		time.Now().Add(-1*time.Hour).UTC().Format(time.RFC3339))
	m.Rescan(context.Background())

	snap := m.Snapshot("")
	if got := snap.Devices[0].GetWdaState(); got != agentcomposev2.IosWdaState_IOS_WDA_STATE_EXPIRED {
		t.Fatalf("wdaState = %v, want EXPIRED", got)
	}
}

func TestExpiryOnlyReadyTransitions(t *testing.T) {
	// A PREPARING device must not be flipped by the time check — only READY
	// devices advance, and only a WDA job completion promotes back.
	for _, state := range []agentcomposev2.IosWdaState{
		agentcomposev2.IosWdaState_IOS_WDA_STATE_UNSPECIFIED,
		agentcomposev2.IosWdaState_IOS_WDA_STATE_MISSING,
		agentcomposev2.IosWdaState_IOS_WDA_STATE_PREPARING,
		agentcomposev2.IosWdaState_IOS_WDA_STATE_FAILED,
		agentcomposev2.IosWdaState_IOS_WDA_STATE_RENEWAL_DUE, // not demoted
		agentcomposev2.IosWdaState_IOS_WDA_STATE_EXPIRED,     // not demoted
	} {
		m, _ := expiryManager(t, state,
			time.Now().Add(-1*time.Hour).UTC().Format(time.RFC3339))
		m.Rescan(context.Background())
		snap := m.Snapshot("")
		if got := snap.Devices[0].GetWdaState(); got != state {
			t.Fatalf("state %v must be left alone by the expiry check, got %v", state, got)
		}
	}
}

func TestExpiryBadTimestampIsIgnored(t *testing.T) {
	m, _ := expiryManager(t, agentcomposev2.IosWdaState_IOS_WDA_STATE_READY, "not-a-timestamp")
	m.Rescan(context.Background())
	snap := m.Snapshot("")
	if got := snap.Devices[0].GetWdaState(); got != agentcomposev2.IosWdaState_IOS_WDA_STATE_READY {
		t.Fatalf("wdaState = %v, want READY (unparseable expiry ignored)", got)
	}
}

func TestExpiryEmptyTimestampIsIgnored(t *testing.T) {
	m, _ := expiryManager(t, agentcomposev2.IosWdaState_IOS_WDA_STATE_READY, "")
	m.Rescan(context.Background())
	snap := m.Snapshot("")
	if got := snap.Devices[0].GetWdaState(); got != agentcomposev2.IosWdaState_IOS_WDA_STATE_READY {
		t.Fatalf("wdaState = %v, want READY (empty expiry ignored)", got)
	}
}

func TestConfigureStoresRenewBeforeDays(t *testing.T) {
	m, _, _, _, _ := testManager(t, EnumeratedDevice{UDID: "udid-a", ConnectionType: "USB", DeviceID: 1})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Rescan(ctx)
	if _, err := m.Claim(ctx, claimReq("udid-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConfigureDevice(ctx, &agentcomposev2.NodeIosConfigureDevice{
		DeviceId:        m.devices["udid-a"].deviceID,
		ConfigRevision:  9,
		RenewBeforeDays: 7,
	}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	got := m.devices["udid-a"].renewBeforeDays
	m.mu.Unlock()
	if got != 7 {
		t.Fatalf("renewBeforeDays = %d, want 7", got)
	}
}
