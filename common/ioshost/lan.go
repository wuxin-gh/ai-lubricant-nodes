// LAN (no-USB) tunnel agent: keeps an iPhone reachable after the cable is gone.
//
// iOS 17+ only exposes testmanagerd / DeviceKit through an RSD tunnel, and
// go-ios's TunnelManager builds that tunnel over **usbmuxd** — i.e. only for a
// physically plugged device. go-ios *does* implement the wireless path
// (mDNS discovery of `_remoted._tcp` + RemotePairing + a TLS-PSK TCP tunnel,
// iOS 18.2+), but its automatic manager deliberately skips Network devices and
// has no way to register a tunnel it did not create itself. Our fork adds
// exactly those two pieces (ManualPairAndConnectToNetworkTunnel + the
// "external" tunnel markers); this file drives them.
//
// Every ~10s the agent looks at the devices this host has CLAIMED (devices.json
// — so it works while the phone is unplugged) and, for each one that usbmuxd
// cannot see, tries to bring up a LAN tunnel and registers it on the shared
// TunnelManager. Everything downstream (WDA jobs, the always-on device-control
// loop, port forwarding) then works unchanged: consumers read tunnel info from
// the same local HTTP endpoint they already use.
//
// Design rules this file follows:
//   - USB wins. The moment usbmuxd sees the device again, the LAN tunnel is
//     dropped and the stock manager takes over — zero behavioural change for a
//     plugged phone.
//   - Never storm. A device that will not tunnel (not on this LAN, not
//     trusted, Wi-Fi off) backs off per-UDID (30s → 5min), and the whole agent
//     pauses mid-WDA-job via the same flag the USB reconciler uses, so it can
//     never yank a tunnel out from under an in-flight launch.
//   - Absent ≠ dead. A failed usbmuxd probe (Windows' Apple Mobile Device
//     Service drops ~1 in 8 dials) must not be read as "unplugged": the tick is
//     skipped, exactly like tunnel.go's reconcile guard.
package ioshost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/tunnel"
)

// LanEnvVar gates the feature: "on" (default) / "off". Anything else (including
// unset) behaves as "on" — the agent is inert on a host with no claimed
// devices, so an unwanted switch would only add startup noise.
const LanEnvVar = "AGENT_COMPOSE_IOS_LAN"

const (
	lanTickEvery   = 10 * time.Second // how often the reconcile pass runs
	lanProbeEvery  = 30 * time.Second // how often a live tunnel is re-verified
	lanOpenTimeout = 90 * time.Second // first Trust prompt needs time for a tap
	lanPortStride  = 900              // keeps our userspace listeners out of the
	// manager's basePort+portOffset range
)

// lanTunnel is one live LAN tunnel handle, narrowed to what the loop needs so
// tests can drive every branch without a device.
type lanTunnel interface {
	udid() string
	// alive re-verifies the data plane (an RSD handshake through the tunnel).
	alive(ctx context.Context) bool
}

// lanOpener performs discovery + pairing and registers the result; it returns
// the handle the loop keeps, plus the tunnel value for the registry.
type lanOpener interface {
	open(ctx context.Context, udid string, ifacePort int) (tunnel.Tunnel, lanTunnel, error)
}

// lanRegistry is the seam onto TunnelManager (add/remove external tunnels).
type lanRegistry interface {
	add(t tunnel.Tunnel)
	remove(udid string) error
}

// LanAgent owns the per-UDID LAN tunnel state machine. Zero value is unusable;
// build it with newLanAgent.
type LanAgent struct {
	log     *slog.Logger
	opener  lanOpener
	reg     lanRegistry
	targets func() []string                                    // claimed UDIDs
	hook    LanHostHooks                                       // reports back into the DeviceManager
	paused  *atomic.Bool                                       // shared with the USB reconcile guard
	probe   func(ctx context.Context) (map[string]bool, error) // usbmuxd presence

	tick       time.Duration
	probeEvery time.Duration
	portBase   int

	mu     sync.Mutex
	states map[string]*lanState
	stopCh chan struct{}
	once   sync.Once
	// stopped: set by Stop so an in-flight open (blocking on a Trust prompt)
	// does not resurrect a tunnel after the agent has shut down.
	stopped atomic.Bool
}

// LanHostHooks carries agent observations back to the device manager. All
// fields are optional.
type LanHostHooks struct {
	// Reachable flips the LAN half of the online axis (present = usbmuxd || lan).
	Reachable func(udid string, on bool)
	// PairingPending surfaces "tap Trust on the phone" — the one step only a
	// human can do, so it must not look like an unexplained failure.
	PairingPending func(udid string)
}

type lanState struct {
	tun       lanTunnel
	lastProbe time.Time
	nextTry   time.Time
	backoff   time.Duration
	failures  int
	// opening: an open is in flight for this UDID. reconcile runs on a single
	// goroutine today, so this cannot fire — it is a guard against future
	// concurrency (an async open, or reconcile driven from two places), because
	// a duplicate would mean two Trust prompts and one leaked tunnel.
	opening bool
}

func newLanAgent(log *slog.Logger, opener lanOpener, reg lanRegistry, targets func() []string, hook LanHostHooks, paused *atomic.Bool) *LanAgent {
	if log == nil {
		log = slog.Default()
	}
	if targets == nil {
		targets = func() []string { return nil }
	}
	base := ios.HttpApiPort() + lanPortStride
	return &LanAgent{
		log:        log,
		opener:     opener,
		reg:        reg,
		targets:    targets,
		hook:       hook,
		paused:     paused,
		probe:      usbmuxdPresence,
		tick:       lanTickEvery,
		probeEvery: lanProbeEvery,
		portBase:   base,
		states:     map[string]*lanState{},
		stopCh:     make(chan struct{}),
	}
}

// Start runs the reconcile loop until ctx is cancelled or Stop is called.
// A no-op when the feature gate is off — the caller wires unconditionally, the
// switch decides whether anything runs.
func (a *LanAgent) Start(ctx context.Context) {
	if a == nil || a.opener == nil || a.reg == nil {
		return
	}
	if lanDisabled() {
		a.log.Info("ios lan: disabled by env", "env", LanEnvVar)
		return
	}
	go func() {
		ticker := time.NewTicker(a.tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-a.stopCh:
				return
			case <-ticker.C:
				a.reconcile(ctx)
			}
		}
	}()
}

// Stop tears down every LAN tunnel this agent created. Idempotent.
func (a *LanAgent) Stop() {
	if a == nil {
		return
	}
	a.stopped.Store(true)
	a.once.Do(func() { close(a.stopCh) })
	a.mu.Lock()
	udids := make([]string, 0, len(a.states))
	for udid, st := range a.states {
		if st.tun != nil {
			udids = append(udids, udid)
		}
		st.tun = nil
	}
	a.mu.Unlock()
	for _, udid := range udids {
		a.drop(udid)
		a.setReachable(udid, false)
	}
}

// reconcile is one pass. Split out so tests can drive a single tick without a
// goroutine. The feature gate is applied in Start — a disabled agent is never
// started, so no runtime check lives here.
func (a *LanAgent) reconcile(ctx context.Context) {
	// A failed usbmuxd probe tells us nothing about who is plugged in — skipping
	// the tick can only delay a tunnel, never tear one down wrongly.
	usb, err := a.probe(ctx)
	if err != nil {
		a.log.Debug("ios lan: usbmuxd probe failed; skipping tick", "error", err)
		return
	}

	// Prune state for devices that are no longer claimed here.
	claimed := map[string]bool{}
	for _, u := range a.targets() {
		claimed[u] = true
	}

	a.mu.Lock()
	now := time.Now()
	type action struct {
		kind string // "open" | "drop" | "probe"
		udid string
		port int
		st   *lanState
	}
	var actions []action
	for udid, st := range a.states {
		if !claimed[udid] {
			if st.tun != nil {
				st.tun = nil
				actions = append(actions, action{kind: "drop", udid: udid, st: st})
			}
			delete(a.states, udid)
			continue
		}
	}
	for _, udid := range a.targets() {
		st := a.states[udid]
		if st == nil {
			st = &lanState{}
			a.states[udid] = st
		}
		if st.tun != nil {
			if usb[udid] && !a.isPaused() {
				// Cable is back: the stock USB tunnel wins. Never while a job is
				// in flight — that job is using this very tunnel.
				st.tun = nil
				actions = append(actions, action{kind: "drop", udid: udid, st: st})
				continue
			}
			if a.isPaused() {
				continue // mid-job: keep the tunnel, do not probe or tear it down
			}
			if now.Sub(st.lastProbe) >= a.probeEvery {
				st.lastProbe = now
				actions = append(actions, action{kind: "probe", udid: udid, st: st})
			}
			continue
		}
		// Creating a tunnel is non-destructive, so it is allowed even while a
		// job is in flight: otherwise a job dispatched to a LAN-only device
		// whose tunnel dropped would park in waitForDevice forever (the pause
		// would prevent the very tunnel it waits for).
		if usb[udid] || st.opening || now.Before(st.nextTry) {
			continue
		}
		st.opening = true
		port := a.freePortLocked(len(actions))
		actions = append(actions, action{kind: "open", udid: udid, st: st, port: port})
	}
	a.mu.Unlock()

	for _, act := range actions {
		switch act.kind {
		case "drop":
			a.drop(act.udid)
			a.setReachable(act.udid, false)
		case "probe":
			if !act.st.tun.alive(ctx) {
				a.log.Info("ios lan: tunnel no longer answering; re-pairing", "udid", act.udid)
				a.mu.Lock()
				act.st.tun = nil
				act.st.nextTry = time.Now()
				a.mu.Unlock()
				a.drop(act.udid)
				a.setReachable(act.udid, false)
			}
		case "open":
			a.open(ctx, act.udid, act.st, act.port)
		}
	}
}

// open brings one tunnel up outside the lock (pairing blocks on a human tap).
func (a *LanAgent) open(ctx context.Context, udid string, st *lanState, ifacePort int) {
	oc, cancel := context.WithTimeout(ctx, lanOpenTimeout)
	defer cancel()
	tun, handle, err := a.opener.open(oc, udid, ifacePort)

	a.mu.Lock()
	st.opening = false
	st.failures++
	backoff := lanBackoff(st.failures)
	st.backoff = backoff
	st.nextTry = time.Now().Add(backoff)
	if err == nil && !a.stopped.Load() {
		st.tun = handle
		st.lastProbe = time.Now()
		st.failures = 0
		st.nextTry = time.Time{}
	}
	stopped := a.stopped.Load()
	a.mu.Unlock()

	if err != nil {
		switch {
		case isPairingPending(err):
			// The device is showing (or waiting for) its Trust prompt. Say so —
			// "no tunnel" with no explanation reads as a broken feature.
			a.log.Info("ios lan: waiting for the Trust prompt on the device",
				"udid", udid, "error", err)
			if a.hook.PairingPending != nil {
				a.hook.PairingPending(udid)
			}
		case isLanUnreachable(err):
			// Never found the phone on the network: Wi-Fi off, different subnet,
			// or AP isolation blocking mDNS. Logged at debug so a normal
			// "cable is unplugged, phone is on cellular" state stays quiet.
			a.log.Debug("ios lan: device not found on the LAN; retrying",
				"udid", udid, "error", err)
		default:
			a.log.Info("ios lan: tunnel attempt failed; backing off",
				"udid", udid, "error", err, "retry_in", backoff.String())
		}
		return
	}
	if stopped {
		// Stop() landed while we were pairing (agent shutting down). Do not
		// resurrect a tunnel nobody will maintain — close what we made.
		if cerr := tun.Close(); cerr != nil {
			a.log.Debug("ios lan: close after stop", "udid", udid, "error", cerr)
		}
		return
	}
	a.reg.add(tun)
	a.log.Info("ios lan: tunnel up", "udid", udid, "address", tun.Address,
		"rsd_port", tun.RsdPort, "userspace_port", tun.UserspaceTUNPort)
	a.setReachable(udid, true)
}

// drop removes the registered tunnel (closing it).
func (a *LanAgent) drop(udid string) {
	if err := a.reg.remove(udid); err != nil && !errors.Is(err, tunnel.ErrTunnelNotFound) {
		a.log.Debug("ios lan: remove tunnel", "udid", udid, "error", err)
	}
}

func (a *LanAgent) setReachable(udid string, on bool) {
	if a.hook.Reachable != nil {
		a.hook.Reachable(udid, on)
	}
}

func (a *LanAgent) isPaused() bool {
	return a.paused != nil && a.paused.Load()
}

// freePortLocked picks a userspace listener port nobody is holding. Callers
// hold a.mu; the bind probe is racy by nature, and a collision just fails the
// attempt (retried after backoff).
func (a *LanAgent) freePortLocked(salt int) int {
	for i := 0; i < 8; i++ {
		port := a.portBase + ((salt+i)%8)*10 + 1
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port
	}
	return a.portBase // last resort; a collision shows up as a failed attempt
}

// lanBackoff: 30s, 60s, 120s, 240s, capped at 5min — mirrors go-ios's own
// failed-device schedule so a device that never tunnels cannot drive a storm.
func lanBackoff(failures int) time.Duration {
	shift := failures - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 4 {
		shift = 4
	}
	seconds := 30 * (1 << shift)
	if seconds > 300 {
		seconds = 300
	}
	return time.Duration(seconds) * time.Second
}

func lanDisabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(LanEnvVar)), "off")
}

// lanEnabled is the default-on reading of the gate: only an explicit "off"
// disables. The agent is inert on a host with no claimed devices, so an
// opt-out switch would only add startup noise.
func lanEnabled() bool { return !lanDisabled() }

// isPairingPending recognises "the device is showing (or waiting for) its Trust
// prompt" — the manual-pairing step, which needs a human tap. Distinguish it
// from a plain discovery timeout: mDNS not finding the phone means "not on this
// LAN / Wi-Fi off / AP isolation", a completely different operator action, and
// reporting it as "tap Trust" sends them looking in the wrong place.
func isPairingPending(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// A timeout only means pairing if we actually reached the pairing stage.
		// FindDeviceInterfaceAddress timing out is discovery, not pairing.
		return strings.Contains(err.Error(), "pair")
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "pair") || strings.Contains(msg, "trust")
}

// isLanUnreachable recognises "we never found the device on the LAN". Reported
// separately from a pairing failure so the operator knows it is a network
// problem, not a phone action.
func isLanUnreachable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "failed to find device interface address") ||
		strings.Contains(msg, "could not find port for")
}

// ── production wiring ─────────────────────────────────────────────────────

// goiosLanOpener is the real opener: mDNS discovery + RemotePairing + the
// userspace data plane (fork-only entry point), then registration.
type goiosLanOpener struct {
	pm  tunnel.PairRecordManager
	log *slog.Logger
}

func (o goiosLanOpener) open(ctx context.Context, udid string, ifacePort int) (tunnel.Tunnel, lanTunnel, error) {
	// A LAN-only device has no usbmuxd entry, so the entry is fabricated from
	// the UDID alone: FindDeviceInterfaceAddress matches by UDID over the RSD
	// handshake and needs nothing else.
	entry := ios.DeviceEntry{
		Properties: ios.DeviceProperties{SerialNumber: udid, ConnectionType: "Network"},
	}
	tun, err := tunnel.ManualPairAndConnectToNetworkTunnel(ctx, entry, o.pm, ifacePort, true)
	if err != nil {
		return tunnel.Tunnel{}, nil, err
	}
	return tun, &goiosLanTunnel{tun: tun, serial: udid}, nil
}

type goiosLanTunnel struct {
	tun    tunnel.Tunnel
	serial string
}

func (g *goiosLanTunnel) udid() string { return g.serial }

// alive re-does the RSD handshake through the tunnel's userspace listener. A
// dead data plane fails fast here (15s bound upstream), which is what we want:
// the alternative is trusting a tunnel that stopped carrying traffic.
func (g *goiosLanTunnel) alive(ctx context.Context) bool {
	host, port := ios.HttpApiHost(), ios.HttpApiPort()
	info, err := tunnel.TunnelInfoForDevice(g.serial, host, port)
	if err != nil {
		return false
	}
	d := ios.DeviceEntry{
		Properties:       ios.DeviceProperties{SerialNumber: g.serial, ConnectionType: "Network"},
		UserspaceTUN:     info.UserspaceTUN,
		UserspaceTUNHost: host,
		UserspaceTUNPort: info.UserspaceTUNPort,
	}
	svc, err := ios.NewWithAddrPortDevice(info.Address, info.RsdPort, d)
	if err != nil {
		return false
	}
	defer svc.Close()
	hsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, herr := svc.Handshake()
		done <- herr
	}()
	select {
	case <-hsCtx.Done():
		return false
	case herr := <-done:
		return herr == nil
	}
}

// managerRegistry adapts TunnelManager to lanRegistry.
type managerRegistry struct{ tm *tunnel.TunnelManager }

func (r managerRegistry) add(t tunnel.Tunnel) { r.tm.AddExternalTunnel(t) }
func (r managerRegistry) remove(udid string) error {
	return r.tm.RemoveExternalTunnel(udid)
}

// usbmuxdPresence returns the set of UDIDs usbmuxd currently sees.
func usbmuxdPresence(_ context.Context) (map[string]bool, error) {
	list, err := ios.ListDevices()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list.DeviceList))
	for _, d := range list.DeviceList {
		out[d.Properties.SerialNumber] = true
	}
	return out, nil
}
