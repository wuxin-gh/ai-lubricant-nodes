package ioshost

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielpaulus/go-ios/ios/tunnel"
)

type fakeLanTunnel struct {
	serial     string
	aliveVal   bool
	closeCalls *int
}

func (f *fakeLanTunnel) udid() string               { return f.serial }
func (f *fakeLanTunnel) alive(context.Context) bool { return f.aliveVal }
func (f *fakeLanTunnel) Close() error               { *f.closeCalls++; return nil }

type fakeLanRegistry struct {
	mu      sync.Mutex
	added   []string
	dropped []string
}

func (r *fakeLanRegistry) add(t tunnel.Tunnel) {
	r.mu.Lock()
	r.added = append(r.added, t.Udid)
	r.mu.Unlock()
}
func (r *fakeLanRegistry) remove(udid string) error {
	r.mu.Lock()
	r.dropped = append(r.dropped, udid)
	r.mu.Unlock()
	return nil
}

type fakeLanOpener struct {
	mu        sync.Mutex
	openCalls []string
	err       error
	tun       tunnel.Tunnel
	handle    lanTunnel
}

func (o *fakeLanOpener) open(_ context.Context, udid string, _ int) (tunnel.Tunnel, lanTunnel, error) {
	o.mu.Lock()
	o.openCalls = append(o.openCalls, udid)
	o.mu.Unlock()
	if o.err != nil {
		return tunnel.Tunnel{}, nil, o.err
	}
	return o.tun, o.handle, nil
}

type fakeLanHooks struct {
	reach []string
	pair  []string
}

func (h *fakeLanHooks) reachable(udid string, on bool) {
	if on {
		h.reach = append(h.reach, udid)
	}
}
func (h *fakeLanHooks) pairingPending(udid string) { h.pair = append(h.pair, udid) }

func newTestLanAgent(t *testing.T, targets []string, opener *fakeLanOpener, reg *fakeLanRegistry, hooks *fakeLanHooks, paused *atomic.Bool, usb map[string]bool, probeErr error) *LanAgent {
	t.Helper()
	a := newLanAgent(nil, opener, reg, func() []string { return targets }, LanHostHooks{
		Reachable:      hooks.reachable,
		PairingPending: hooks.pairingPending,
	}, paused)
	a.probe = func(context.Context) (map[string]bool, error) {
		return usb, probeErr
	}
	a.tick = time.Hour // reconcile is driven manually in tests
	return a
}

func TestLanOpensTunnelForClaimedDevice(t *testing.T) {
	reg := &fakeLanRegistry{}
	h := &fakeLanHooks{}
	o := &fakeLanOpener{tun: tunnel.Tunnel{Udid: "U"}, handle: &fakeLanTunnel{serial: "U", aliveVal: true}}
	a := newTestLanAgent(t, []string{"U"}, o, reg, h, new(atomic.Bool), nil, nil)

	a.reconcile(context.Background())

	if len(o.openCalls) != 1 || o.openCalls[0] != "U" {
		t.Fatalf("openCalls = %v, want [U]", o.openCalls)
	}
	if len(reg.added) != 1 || reg.added[0] != "U" {
		t.Fatalf("registered = %v, want [U]", reg.added)
	}
	if len(h.reach) != 1 || h.reach[0] != "U" {
		t.Fatalf("reachable = %v, want [U]", h.reach)
	}
}

func TestLanSkipsUsbPresentDevice(t *testing.T) {
	o := &fakeLanOpener{}
	a := newTestLanAgent(t, []string{"U"}, o, &fakeLanRegistry{}, &fakeLanHooks{}, new(atomic.Bool), map[string]bool{"U": true}, nil)
	a.reconcile(context.Background())
	if len(o.openCalls) != 0 {
		t.Fatalf("must not open for a USB-present device, got %v", o.openCalls)
	}
}

func TestLanBacksOffAfterFailure(t *testing.T) {
	o := &fakeLanOpener{err: errors.New("failed to pair device: user did not trust")}
	h := &fakeLanHooks{}
	a := newTestLanAgent(t, []string{"U"}, o, &fakeLanRegistry{}, h, new(atomic.Bool), nil, nil)

	a.reconcile(context.Background())
	a.reconcile(context.Background()) // immediately after: within backoff, must not retry
	a.reconcile(context.Background())

	if len(o.openCalls) != 1 {
		t.Fatalf("a failing device must back off, got %d attempts", len(o.openCalls))
	}
	if len(h.pair) != 1 {
		t.Fatalf("pairing-pending hook must fire once for a Trust-prompt failure, got %d", len(h.pair))
	}
}

func TestLanDropsTunnelWhenUsbReturns(t *testing.T) {
	var closes int
	o := &fakeLanOpener{tun: tunnel.Tunnel{Udid: "U"}, handle: &fakeLanTunnel{serial: "U", aliveVal: true, closeCalls: &closes}}
	reg := &fakeLanRegistry{}
	h := &fakeLanHooks{}
	a := newTestLanAgent(t, []string{"U"}, o, reg, h, new(atomic.Bool), nil, nil)

	// Bring the LAN tunnel up, then have USB appear.
	usbNone := map[string]bool{}
	usbBoth := map[string]bool{"U": true}
	a.probe = func(context.Context) (map[string]bool, error) { return usbNone, nil }
	a.reconcile(context.Background())
	if len(reg.added) != 1 {
		t.Fatalf("tunnel never came up: registered=%v", reg.added)
	}
	a.probe = func(context.Context) (map[string]bool, error) { return usbBoth, nil }
	a.reconcile(context.Background())

	if len(reg.dropped) != 1 || reg.dropped[0] != "U" {
		t.Fatalf("USB return must drop the LAN tunnel, dropped=%v", reg.dropped)
	}
	if len(h.reach) != 1 {
		t.Fatalf("reachable(true) must have fired exactly once on bring-up, got %v", h.reach)
	}
}

func TestLanProbeFailsThenReopens(t *testing.T) {
	o := &fakeLanOpener{tun: tunnel.Tunnel{Udid: "U"}, handle: &fakeLanTunnel{serial: "U", aliveVal: false}}
	reg := &fakeLanRegistry{}
	a := newTestLanAgent(t, []string{"U"}, o, reg, &fakeLanHooks{}, new(atomic.Bool), nil, nil)

	a.reconcile(context.Background()) // open
	if len(reg.added) != 1 {
		t.Fatal("initial open must register")
	}
	time.Sleep(5 * time.Millisecond) // lastProbe was just stamped; let the probe window elapse
	a.probeEvery = time.Nanosecond
	a.reconcile(context.Background()) // alive=false → drop (next pass will re-open)

	if len(reg.dropped) != 1 {
		t.Fatalf("dead tunnel must be dropped, dropped=%v", reg.dropped)
	}
	// The drop re-arms the device with nextTry=now, so creation happens on the
	// FOLLOWING pass — one reconcile only ever does one of drop / open.
	a.reconcile(context.Background())
	if len(o.openCalls) < 2 {
		t.Fatalf("a dropped tunnel must be re-opened, openCalls=%v", o.openCalls)
	}
}

func TestLanPausedStillCreatesForMissingTunnel(t *testing.T) {
	// Deadlock regression: a job dispatched to a LAN-only device whose tunnel
	// is down would park in waitForDevice forever if the pause also blocked
	// tunnel creation.
	o := &fakeLanOpener{handle: &fakeLanTunnel{serial: "U", aliveVal: true}}
	reg := &fakeLanRegistry{}
	paused := new(atomic.Bool)
	paused.Store(true)
	a := newTestLanAgent(t, []string{"U"}, o, reg, &fakeLanHooks{}, paused, nil, nil)

	a.reconcile(context.Background())

	if len(reg.added) != 1 {
		t.Fatalf("paused agent must still create a missing tunnel, added=%v", reg.added)
	}
}
