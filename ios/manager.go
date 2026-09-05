// DeviceManager owns the live state of an iOS host: which iPhones it sees,
// which it has claimed, and which device-control connection loops are running.
//
// One process, one manager. cmdRun constructs it when the host has a
// NodeConnect identity and threads it into the handler; the handler is the only
// caller of the mutating methods (Claim / Release / ConfigureDevice), each of
// which is driven by a server frame.
//
// Discovery is the reason this type exists: the old `run` read a static
// devices.json and started one goroutine per configured entry, so a user had to
// type a UDID into the web UI before the host would touch a phone. Here the host
// enumerates its own hardware, reports it, and adopts a device only when the
// server says to.
//
// REAL-DEVICE GATE: the go-ios enumeration and attach/detach stream have not
// run against a physical iPhone yet. The Enumerator and PairClient seams are
// indirected so the reconcile/claim/release logic is unit-tested without
// usbmuxd.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"device-control/ios/devicecontrol"

	"ai-lubricant-nodes/common/agent"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// Enumerator abstracts go-ios device discovery so tests can drive the manager
// without usbmuxd or a phone.
type Enumerator interface {
	// List returns the currently attached/reachable devices.
	List(ctx context.Context) ([]EnumeratedDevice, error)
	// Subscribe streams attach/detach events until ctx is cancelled or stop is
	// called. Implementations must close the event channel when they finish.
	Subscribe(ctx context.Context) (events <-chan DeviceEvent, errs <-chan error, stop func(), err error)
}

// EnumeratedDevice is one device as the host's USB/network stack sees it,
// before any lockdown queries. UDID is the stable identity everything else
// keys on.
type EnumeratedDevice struct {
	UDID           string
	ConnectionType string // go-ios ConnectionType: "USB" or "Network"
	DeviceID       int    // go-ios's per-connection handle, needed for lockdown
}

// DeviceInfo is the lockdown-sourced detail for one device.
type DeviceInfo struct {
	Name           string
	Model          string
	ProductVersion string
}

// DeviceEvent is one attach/detach notification.
type DeviceEvent struct {
	UDID     string
	Attached bool
}

// PairClient redeems a device-control pairing code. Indirected so a test can
// stamp a deterministic device_id without an HTTP server.
type PairClient interface {
	Pair(ctx context.Context, serverURL, code, credentialPath string) (devicecontrol.Credential, error)
}

// DeviceRunner runs one device's device-control connection loop until ctx is
// cancelled. Indirected for the same reason as PairClient.
type DeviceRunner interface {
	Run(ctx context.Context, cfg devicecontrol.RunConfig) error
}

type realPairClient struct{}

func (realPairClient) Pair(ctx context.Context, serverURL, code, credentialPath string) (devicecontrol.Credential, error) {
	return devicecontrol.Pair(ctx, serverURL, code, credentialPath)
}

type realDeviceRunner struct{}

func (realDeviceRunner) Run(ctx context.Context, cfg devicecontrol.RunConfig) error {
	return devicecontrol.RunDeviceWithRetry(ctx, cfg)
}

// ManagerConfig is what cmdRun wires into the manager.
type ManagerConfig struct {
	Logger *slog.Logger
	// ConfigPath is the resolved devices.json path, so persistence uses the
	// same file cmdRun loaded (including an explicit --config).
	ConfigPath string
	// DevicesConfig is the loaded config; the manager persists claims into it.
	DevicesConfig *DevicesConfig
	// NodeID is carried in each device's device-control register frame so the
	// server can join device ↔ host node.
	NodeID string
	// OnReport pushes an inventory snapshot upstream. nil disables reporting.
	OnReport func(*agentcomposev2.NodeIosDevicesReport) error

	// Seams (nil = production implementation).
	Enumerator Enumerator
	Pair       PairClient
	Runner     DeviceRunner
	LookupInfo func(EnumeratedDevice) (DeviceInfo, error)
	// ReportInterval bounds how often the periodic re-snapshot runs.
	ReportInterval time.Duration
}

// DeviceManager is the iOS host's device state center.
type DeviceManager struct {
	cfg    ManagerConfig
	enum   Enumerator
	pair   PairClient
	runner DeviceRunner
	lookup func(EnumeratedDevice) (DeviceInfo, error)

	mu             sync.Mutex
	devices        map[string]*managedDevice // keyed by UDID
	snapshotRev    int64
	enumerateError string

	stopOnce sync.Once
	stopCh   chan struct{}
	stopped  atomic.Bool
}

// managedDevice is per-UDID runtime state: what we discovered, what the server
// told us, and the handle to the running connection loop.
type managedDevice struct {
	udid           string
	name           string
	model          string
	productVersion string
	connectionType agentcomposev2.IosConnectionType
	present        bool

	claimed          bool
	claiming         bool
	deviceID         string
	credentialPath   string
	configRevision   int64
	transport        string
	wdaBundle        string
	xctest           string
	hostWDAPort      int
	deviceControlOn  bool
	wdaState         agentcomposev2.IosWdaState
	profileExpiresAt string
	// renewBeforeDays 来自 NodeIosConfigureDevice（默认 14）。watchLoop 的周期
	// Rescan 据此把 READY 设备提前 RENEWAL_DUE，让自动续签徽章与节点 inventory
	// 一致。autoPrepare 仍不启用节点自主派发——续签 job 一律由服务端扫描器派发。
	renewBeforeDays  int
	lastError        string

	cancel context.CancelFunc
	done   chan struct{}
	log    *slog.Logger
}

// NewDeviceManager builds a manager, filling in production seams.
func NewDeviceManager(cfg ManagerConfig) *DeviceManager {
	m := &DeviceManager{
		cfg:     cfg,
		enum:    cfg.Enumerator,
		pair:    cfg.Pair,
		runner:  cfg.Runner,
		lookup:  cfg.LookupInfo,
		devices: map[string]*managedDevice{},
		stopCh:  make(chan struct{}),
	}
	if m.enum == nil {
		m.enum = goiosEnumerator{}
	}
	if m.pair == nil {
		m.pair = realPairClient{}
	}
	if m.runner == nil {
		m.runner = realDeviceRunner{}
	}
	if m.lookup == nil {
		m.lookup = lookupDeviceInfo
	}
	if m.cfg.ReportInterval <= 0 {
		m.cfg.ReportInterval = 30 * time.Second
	}
	m.adoptConfiguredDevices()
	return m
}

// adoptConfiguredDevices seeds the map from devices.json so a restart keeps
// serving devices claimed in an earlier run. present stays false until
// discovery confirms the hardware is actually there.
func (m *DeviceManager) adoptConfiguredDevices() {
	if m.cfg.DevicesConfig == nil {
		return
	}
	for _, d := range m.cfg.DevicesConfig.Devices {
		if strings.TrimSpace(d.UDID) == "" {
			// A legacy entry with no UDID cannot be reconciled against
			// discovery; cmdRun still runs those through the static path.
			continue
		}
		m.devices[d.UDID] = &managedDevice{
			udid:           d.UDID,
			name:           d.Name,
			transport:      d.Transport,
			wdaBundle:      d.WDABundle,
			xctest:         d.XCTest,
			hostWDAPort:    d.WDAPort,
			credentialPath: d.CredentialPath,
			claimed:        d.CredentialPath != "",
			wdaState:       agentcomposev2.IosWdaState_IOS_WDA_STATE_UNSPECIFIED,
		}
	}
}

// Start begins discovery and reporting on a background goroutine.
func (m *DeviceManager) Start(ctx context.Context) {
	go m.run(ctx)
}

// Stop winds the manager down, cancelling every device loop. Idempotent.
func (m *DeviceManager) Stop() {
	m.stopped.Store(true)
	m.stopOnce.Do(func() { close(m.stopCh) })
	m.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(m.devices))
	for _, d := range m.devices {
		if d.cancel != nil {
			cancels = append(cancels, d.cancel)
		}
	}
	m.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// StartClaimedDevices launches connection loops for devices already claimed in
// devices.json. Called once by cmdRun after Start so a restart reconnects
// without waiting for a server frame.
func (m *DeviceManager) StartClaimedDevices(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.devices {
		if d.claimed && d.credentialPath != "" {
			m.startDeviceLocked(ctx, d)
		}
	}
}

// Claim adopts one discovered iPhone: redeem the one-time pairing code, write
// the 0600 credential, persist the mapping, and start the device's connection
// loop. Returns the server-assigned device_id.
//
// Redeeming happens outside the lock (it is a network call) but the UDID is
// reserved first, so two concurrent claims for one phone cannot both mint a
// credential.
func (m *DeviceManager) Claim(ctx context.Context, req *agentcomposev2.NodeIosClaimDevice) (string, error) {
	udid := strings.TrimSpace(req.GetUdid())
	if udid == "" {
		return "", errors.New("claim: udid is required")
	}
	code := strings.TrimSpace(req.GetPairingCode())
	server := strings.TrimSpace(req.GetServerUrl())
	if code == "" || server == "" {
		return "", errors.New("claim: pairing_code and server_url are required")
	}

	m.mu.Lock()
	dev, known := m.devices[udid]
	if known && dev.claimed {
		id := dev.deviceID
		m.mu.Unlock()
		return id, fmt.Errorf("claim: udid %s is already claimed", udid)
	}
	if !known {
		// The server asked for a UDID we have never seen. Refuse rather than
		// pairing blind: the credential would be minted for hardware that may
		// belong to a different host.
		m.mu.Unlock()
		return "", fmt.Errorf("claim: udid %s is not in this host's inventory", udid)
	}
	if dev.claiming {
		m.mu.Unlock()
		return "", fmt.Errorf("claim: udid %s is already being claimed", udid)
	}
	dev.claiming = true
	name := strings.TrimSpace(req.GetDeviceLabel())
	if name == "" {
		name = firstNonEmpty(dev.name, udid)
	}
	m.mu.Unlock()

	credPath := defaultCredentialPath(m.configPath(), name)
	cred, err := m.pair.Pair(ctx, server, code, credPath)

	m.mu.Lock()
	dev.claiming = false
	if err != nil {
		dev.lastError = "pairing failed: " + err.Error()
		m.markDirtyLocked()
		m.reportLocked()
		m.mu.Unlock()
		return "", fmt.Errorf("claim: redeem pairing code: %w", err)
	}
	dev.claimed = true
	dev.deviceID = cred.DeviceID
	dev.credentialPath = credPath
	dev.name = name
	dev.lastError = ""
	if t := strings.TrimSpace(req.GetTransport()); t != "" {
		dev.transport = t
	}
	if b := strings.TrimSpace(req.GetWdaBundleId()); b != "" {
		dev.wdaBundle = b
	}
	if x := strings.TrimSpace(req.GetXctestConfigName()); x != "" {
		dev.xctest = x
	}
	dev.configRevision = req.GetConfigRevision()
	m.persistLocked(dev)
	m.startDeviceLocked(ctx, dev)
	m.markDirtyLocked()
	m.reportLocked()
	m.mu.Unlock()
	return cred.DeviceID, nil
}

// Release stops driving a device and optionally deletes its local credential.
// The discovered record stays (the hardware is still plugged in) so the device
// can be claimed again without a rescan.
func (m *DeviceManager) Release(req *agentcomposev2.NodeIosReleaseDevice) error {
	m.mu.Lock()
	dev := m.findLocked(req.GetDeviceId(), req.GetUdid())
	if dev == nil {
		m.mu.Unlock()
		return fmt.Errorf("release: device %s not found on this host", describeTarget(req.GetDeviceId(), req.GetUdid()))
	}
	cancel := dev.cancel
	credPath := dev.credentialPath
	dev.cancel = nil
	dev.claimed = false
	dev.deviceID = ""
	dev.deviceControlOn = false
	dev.wdaState = agentcomposev2.IosWdaState_IOS_WDA_STATE_UNSPECIFIED
	if req.GetDeleteCredential() {
		dev.credentialPath = ""
	}
	m.removeFromConfigLocked(dev.udid)
	m.markDirtyLocked()
	m.reportLocked()
	m.mu.Unlock()

	// Cancel outside the lock: the device goroutine calls back into OnState,
	// which takes m.mu, so cancelling while holding it can deadlock.
	if cancel != nil {
		cancel()
	}
	if req.GetDeleteCredential() && credPath != "" {
		if err := os.Remove(credPath); err != nil && !os.IsNotExist(err) {
			m.cfg.Logger.Warn("release: delete credential", "path", credPath, "error", err)
		}
	}
	return nil
}

// ConfigureDevice applies desired per-device configuration. Revision-gated: a
// frame carrying an older revision than what we already applied is a no-op, so
// a retried or reordered push cannot roll the host back. Returns the revision
// now in effect.
func (m *DeviceManager) ConfigureDevice(ctx context.Context, req *agentcomposev2.NodeIosConfigureDevice) (int64, error) {
	m.mu.Lock()
	dev := m.findLocked(req.GetDeviceId(), req.GetUdid())
	if dev == nil {
		m.mu.Unlock()
		return 0, fmt.Errorf("configure: device %s not found on this host", describeTarget(req.GetDeviceId(), req.GetUdid()))
	}
	if rev := req.GetConfigRevision(); rev != 0 && rev <= dev.configRevision {
		applied := dev.configRevision
		m.mu.Unlock()
		return applied, nil
	}

	restartNeeded := false
	if t := strings.TrimSpace(req.GetTransport()); t != "" && t != dev.transport {
		dev.transport = t
		restartNeeded = true
	}
	if b := strings.TrimSpace(req.GetWdaBundleId()); b != "" && b != dev.wdaBundle {
		dev.wdaBundle = b
		restartNeeded = true
	}
	if x := strings.TrimSpace(req.GetXctestConfigName()); x != "" && x != dev.xctest {
		dev.xctest = x
		restartNeeded = true
	}
	if p := int(req.GetHostWdaPort()); p != dev.hostWDAPort {
		dev.hostWDAPort = p
		restartNeeded = true
	}
	// renew_before_days：节点据此在周期 Rescan 里把 READY 流转到 RENEWAL_DUE。
	// 0/缺失按 14 天兜底。只存内存（devices.json 的 DeviceConfig 不含它），
	// 重启后等下一次 ConfigureDevice 重新下发；未下发前的兜底同样是 14 天。
	if r := int(req.GetRenewBeforeDays()); r > 0 {
		dev.renewBeforeDays = r
	} else if dev.renewBeforeDays <= 0 {
		dev.renewBeforeDays = 14
	}
	dev.configRevision = req.GetConfigRevision()
	m.persistLocked(dev)
	cancel := dev.cancel
	done := dev.done
	if restartNeeded && cancel != nil {
		// Drop the handle now; the restart below re-creates it. Waiting for the
		// old goroutine happens outside the lock.
		dev.cancel = nil
		dev.done = nil
	} else {
		cancel = nil
	}
	applied := dev.configRevision
	m.markDirtyLocked()
	m.mu.Unlock()

	if cancel != nil {
		cancel()
		if done != nil {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				m.cfg.Logger.Warn("configure: device loop did not stop in time", "udid", dev.udid)
			}
		}
		m.mu.Lock()
		if dev.claimed && dev.credentialPath != "" {
			m.startDeviceLocked(ctx, dev)
		}
		m.reportLocked()
		m.mu.Unlock()
	} else {
		m.mu.Lock()
		m.reportLocked()
		m.mu.Unlock()
	}
	return applied, nil
}

// Snapshot returns the full inventory. Always a complete snapshot so a
// reconnecting server converges in one frame.
func (m *DeviceManager) Snapshot(requestID string) *agentcomposev2.NodeIosDevicesReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	rep := m.snapshotLocked()
	rep.RequestId = requestID
	return rep
}

// Rescan re-enumerates immediately (the user pressed "rescan") and reports.
// An enumeration failure is reported too: "cannot see devices" (usbmuxd down,
// Apple Devices service missing on Windows) is a different story for the user
// than "no devices attached", and only a report carries that distinction.
func (m *DeviceManager) Rescan(ctx context.Context) {
	list, err := m.enum.List(ctx)
	if err != nil {
		m.mu.Lock()
		if m.enumerateError != err.Error() {
			m.enumerateError = err.Error()
			m.markDirtyLocked()
		}
		m.reportLocked()
		m.mu.Unlock()
		return
	}
	m.applySnapshot(list)
}

// ── internals ────────────────────────────────────────────────────────────

func (m *DeviceManager) run(ctx context.Context) {
	m.Rescan(ctx)
	m.reportNow()

	events, errs, stop, err := m.enum.Subscribe(ctx)
	if err != nil {
		m.mu.Lock()
		m.enumerateError = "device event stream unavailable: " + err.Error()
		m.markDirtyLocked()
		m.mu.Unlock()
		m.cfg.Logger.Warn("ios: attach/detach stream unavailable; falling back to polling", "error", err)
		m.reportNow()
		m.pollLoop(ctx)
		return
	}
	defer stop()

	ticker := time.NewTicker(m.cfg.ReportInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case err := <-errs:
			if err != nil {
				m.cfg.Logger.Warn("ios: device event stream error", "error", err)
			}
		case ev, ok := <-events:
			if !ok {
				// Stream ended (usbmuxd restart). Fall back to polling rather
				// than going blind until the process restarts.
				m.cfg.Logger.Warn("ios: device event stream closed; falling back to polling")
				m.pollLoop(ctx)
				return
			}
			m.applyEvent(ctx, ev)
		case <-ticker.C:
			m.Rescan(ctx)
		}
	}
}

// pollLoop is the degraded discovery mode used when the event stream is
// unavailable: periodic re-enumeration only.
func (m *DeviceManager) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.ReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.Rescan(ctx)
		}
	}
}

// applySnapshot reconciles an observed device list against the host's view. A
// device that disappears loses `present` but keeps its claim: unplugging a
// phone must not silently release it.
func (m *DeviceManager) applySnapshot(list []EnumeratedDevice) {
	// Lockdown lookups are network/USB round trips; do them before taking the
	// lock so discovery never blocks a claim or a report.
	infos := make(map[string]DeviceInfo, len(list))
	for _, d := range list {
		if info, err := m.lookup(d); err == nil {
			infos[d.UDID] = info
		} else {
			m.cfg.Logger.Debug("ios: lockdown lookup failed", "udid", d.UDID, "error", err)
		}
	}

	m.mu.Lock()
	seen := make(map[string]bool, len(list))
	changed := false
	for _, d := range list {
		seen[d.UDID] = true
		if m.upsertLocked(d, infos[d.UDID]) {
			changed = true
		}
	}
	for udid, dev := range m.devices {
		if !seen[udid] && dev.present {
			dev.present = false
			changed = true
		}
	}
	if m.enumerateError != "" {
		m.enumerateError = ""
		changed = true
	}
	if m.refreshWdaExpiryLocked(time.Now()) {
		changed = true
	}
	if changed {
		m.markDirtyLocked()
		m.reportLocked()
	}
	m.mu.Unlock()
}

// refreshWdaExpiryLocked walks every device and advances wdaState purely by
// time: READY → RENEWAL_DUE inside the renewal window (renewBeforeDays before
// profile expiry), READY → EXPIRED past expiry. No IO — profileExpiresAt was
// recorded when the last WDA job verified the profile, so this is a clock
// comparison on every periodic Rescan (30s tick). Returns whether anything
// changed so the caller reports a new snapshot. RENEWAL_DUE/EXPIRED are not
// demoted here: only a WDA job completion (OnState) promotes back to READY.
// Caller holds m.mu.
func (m *DeviceManager) refreshWdaExpiryLocked(now time.Time) bool {
	changed := false
	for _, d := range m.devices {
		if d.wdaState != agentcomposev2.IosWdaState_IOS_WDA_STATE_READY {
			continue
		}
		exp, err := time.Parse(time.RFC3339, strings.TrimSpace(d.profileExpiresAt))
		if err != nil {
			continue
		}
		window := 14
		if d.renewBeforeDays > 0 {
			window = d.renewBeforeDays
		}
		var next agentcomposev2.IosWdaState
		if now.After(exp) {
			next = agentcomposev2.IosWdaState_IOS_WDA_STATE_EXPIRED
		} else if now.AddDate(0, 0, window).After(exp) {
			next = agentcomposev2.IosWdaState_IOS_WDA_STATE_RENEWAL_DUE
		} else {
			continue
		}
		d.wdaState = next
		changed = true
	}
	return changed
}

func (m *DeviceManager) applyEvent(ctx context.Context, ev DeviceEvent) {
	if !ev.Attached {
		m.mu.Lock()
		if dev, ok := m.devices[ev.UDID]; ok && dev.present {
			dev.present = false
			dev.deviceControlOn = false
			m.markDirtyLocked()
			m.reportLocked()
		}
		m.mu.Unlock()
		return
	}
	// An attach event carries only the basics; re-enumerate so the new device
	// arrives with its connection type and lockdown detail filled in.
	m.Rescan(ctx)
}

// upsertLocked adds or refreshes a discovered device, returning whether
// anything the server can see changed. Caller holds m.mu.
func (m *DeviceManager) upsertLocked(d EnumeratedDevice, info DeviceInfo) bool {
	dev, ok := m.devices[d.UDID]
	if !ok {
		dev = &managedDevice{udid: d.UDID}
		m.devices[d.UDID] = dev
	}
	changed := !ok || !dev.present
	dev.present = true
	if ct := toConnectionType(d.ConnectionType); ct != dev.connectionType {
		dev.connectionType = ct
		changed = true
	}
	if info.Name != "" && info.Name != dev.name && dev.name == "" {
		// Only fill an empty name: a server-supplied label must win over the
		// device's own name once the device is claimed.
		dev.name = info.Name
		changed = true
	}
	if info.Model != "" && info.Model != dev.model {
		dev.model = info.Model
		changed = true
	}
	if info.ProductVersion != "" && info.ProductVersion != dev.productVersion {
		dev.productVersion = info.ProductVersion
		changed = true
	}
	return changed
}

func (m *DeviceManager) markDirtyLocked() { m.snapshotRev++ }

// snapshotLocked builds a report from current state. Caller holds m.mu.
func (m *DeviceManager) snapshotLocked() *agentcomposev2.NodeIosDevicesReport {
	rep := &agentcomposev2.NodeIosDevicesReport{
		SnapshotRevision: m.snapshotRev,
		ReportedAt:       time.Now().UTC().Format(time.RFC3339),
		EnumerateError:   m.enumerateError,
	}
	for _, d := range sortedDevices(m.devices) {
		rep.Devices = append(rep.Devices, d.toProto())
	}
	return rep
}

// reportLocked pushes a snapshot upstream. Caller holds m.mu. Send failures are
// debug-level: a dropped stream is normal and the next connect re-reports.
func (m *DeviceManager) reportLocked() {
	if m.cfg.OnReport == nil {
		return
	}
	rep := m.snapshotLocked()
	if err := m.cfg.OnReport(rep); err != nil {
		m.cfg.Logger.Debug("ios: inventory report not sent", "error", err)
	}
}

func (m *DeviceManager) reportNow() {
	m.mu.Lock()
	m.reportLocked()
	m.mu.Unlock()
}

// startDeviceLocked spawns the connection loop for a claimed device. Caller
// holds m.mu. A device already running is left alone.
func (m *DeviceManager) startDeviceLocked(ctx context.Context, d *managedDevice) {
	if d.cancel != nil || m.stopped.Load() {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	d.cancel = cancel
	d.done = done
	if d.log == nil || d.name != "" {
		d.log = m.cfg.Logger.With("device", firstNonEmpty(d.name, d.udid), "udid", d.udid)
	}
	cfg := devicecontrol.RunConfig{
		CredentialPath: d.credentialPath,
		Device: devicecontrol.Options{
			UDID:         d.udid,
			Transport:    d.transport,
			WDABundleID:  d.wdaBundle,
			XCTestConfig: d.xctest,
			WDAPort:      d.hostWDAPort,
		},
		OnState: m.onDeviceState(d),
		OnWipe:  m.onDeviceWipe(d),
	}
	if m.cfg.NodeID != "" {
		cfg.DeviceInfoExtra = map[string]any{"node_id": m.cfg.NodeID}
	}
	log := d.log
	go func() {
		defer close(done)
		log.Info("device connecting")
		err := m.runner.Run(runCtx, cfg)
		m.mu.Lock()
		d.deviceControlOn = false
		if err != nil && runCtx.Err() == nil {
			d.lastError = err.Error()
		}
		m.markDirtyLocked()
		m.reportLocked()
		m.mu.Unlock()
		if err != nil && runCtx.Err() == nil {
			log.Error("device ended", "error", err)
		} else {
			log.Info("device stopped")
		}
	}()
}

// onDeviceState returns the OnState callback for one device. Split out so the
// closure does not capture the loop variable of a caller.
func (m *DeviceManager) onDeviceState(d *managedDevice) func(devicecontrol.State) {
	return func(s devicecontrol.State) {
		m.mu.Lock()
		online := s == devicecontrol.StateConnected
		if online != d.deviceControlOn {
			d.deviceControlOn = online
			m.markDirtyLocked()
			m.reportLocked()
		}
		m.mu.Unlock()
		d.log.Info("device state", "state", s)
	}
}

// onDeviceWipe returns the OnWipe callback: the server revoked this device's
// token, so drop the claim and surface it for re-adoption.
func (m *DeviceManager) onDeviceWipe(d *managedDevice) func() {
	return func() {
		m.mu.Lock()
		d.claimed = false
		d.deviceID = ""
		d.deviceControlOn = false
		d.lastError = "credential wiped by server; re-claim required"
		m.markDirtyLocked()
		m.reportLocked()
		m.mu.Unlock()
		d.log.Warn("device credential wiped (auth failed); re-claim required")
	}
}

// findLocked resolves a device by server device_id or UDID. Caller holds m.mu.
func (m *DeviceManager) findLocked(deviceID, udid string) *managedDevice {
	if id := strings.TrimSpace(deviceID); id != "" {
		for _, d := range m.devices {
			if d.deviceID == id {
				return d
			}
		}
	}
	if u := strings.TrimSpace(udid); u != "" {
		return m.devices[u]
	}
	return nil
}

func (m *DeviceManager) configPath() string {
	if p := strings.TrimSpace(m.cfg.ConfigPath); p != "" {
		return p
	}
	dir, err := configDir()
	if err != nil {
		return "devices.json"
	}
	return dir + string(os.PathSeparator) + "devices.json"
}

// persistLocked writes the device's mapping into devices.json so a restart
// resumes it. Caller holds m.mu.
func (m *DeviceManager) persistLocked(d *managedDevice) {
	cfg := m.cfg.DevicesConfig
	if cfg == nil {
		return
	}
	cfg.Upsert(DeviceConfig{
		Name:           firstNonEmpty(d.name, d.udid),
		UDID:           d.udid,
		Transport:      d.transport,
		WDABundle:      d.wdaBundle,
		XCTest:         d.xctest,
		WDAPort:        d.hostWDAPort,
		CredentialPath: d.credentialPath,
	})
	if err := SaveDevicesConfig(m.configPath(), cfg); err != nil {
		m.cfg.Logger.Warn("ios: save devices config", "udid", d.udid, "error", err)
	}
}

// removeFromConfigLocked drops a device from devices.json. Caller holds m.mu.
func (m *DeviceManager) removeFromConfigLocked(udid string) {
	cfg := m.cfg.DevicesConfig
	if cfg == nil {
		return
	}
	out := make([]DeviceConfig, 0, len(cfg.Devices))
	for _, d := range cfg.Devices {
		if d.UDID != udid {
			out = append(out, d)
		}
	}
	cfg.Devices = out
	if err := SaveDevicesConfig(m.configPath(), cfg); err != nil {
		m.cfg.Logger.Warn("ios: save devices config after release", "udid", udid, "error", err)
	}
}

// ── conversions ──────────────────────────────────────────────────────────

func (d *managedDevice) toProto() *agentcomposev2.NodeIosDevice {
	return &agentcomposev2.NodeIosDevice{
		Udid:                  d.udid,
		Name:                  d.name,
		Model:                 d.model,
		ProductVersion:        d.productVersion,
		ConnectionType:        d.connectionType,
		Present:               d.present,
		Claimed:               d.claimed,
		DeviceId:              d.deviceID,
		DeviceControlOnline:   d.deviceControlOn,
		WdaState:              d.wdaState,
		WdaBundleId:           d.wdaBundle,
		ProfileExpiresAt:      d.profileExpiresAt,
		LastError:             d.lastError,
		ConfigRevisionApplied: d.configRevision,
	}
}

func sortedDevices(devices map[string]*managedDevice) []*managedDevice {
	out := make([]*managedDevice, 0, len(devices))
	for _, d := range devices {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].udid < out[j].udid })
	return out
}

func toConnectionType(c string) agentcomposev2.IosConnectionType {
	switch strings.ToUpper(strings.TrimSpace(c)) {
	case "USB":
		return agentcomposev2.IosConnectionType_IOS_CONNECTION_TYPE_USB
	case "NETWORK":
		return agentcomposev2.IosConnectionType_IOS_CONNECTION_TYPE_NETWORK
	default:
		return agentcomposev2.IosConnectionType_IOS_CONNECTION_TYPE_UNSPECIFIED
	}
}

func describeTarget(deviceID, udid string) string {
	if strings.TrimSpace(deviceID) != "" {
		return "device_id=" + deviceID
	}
	return "udid=" + udid
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// unused keeps the agent import meaningful for future manager↔client wiring
// (the handler owns the client today).
var _ = (*agent.Client)(nil)
