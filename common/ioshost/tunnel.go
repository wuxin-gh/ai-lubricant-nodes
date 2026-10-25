// Tunnel agent: iOS 17+ devices only expose testmanagerd (the service that
// launches the WDA XCTest runner) through a CoreDevice RSD tunnel. go-ios's
// RunTestWithConfig refuses to launch on 17+ unless the DeviceEntry carries an
// RSD provider, which only a running tunnel agent can supply. This file embeds
// go-ios's TunnelManager into node-ios so the host needs no separate
// `ios tunnel start` process and no operator setup.
//
// REAL-DEVICE GATE: wired against the go-ios v1.3.2 API; the tunnel lifecycle
// (pair-record creation, quic tunnel handshake, RSD query) has not been
// exercised against a physical iOS 17+ iPhone yet. iOS <17 devices are
// untouched by all of this (usbmuxd path, no tunnel).
package ioshost

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	ios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/tunnel"
)

// StartTunnelAgent runs go-ios's tunnel manager in-process (userspace mode —
// gVisor netstack, no root/sudo needed). It reconciles every second: tunnels
// come up for connected USB devices, go down on unplug, and failed starts
// back off. devicelink picks the tunnels up through the tunnel-info HTTP
// endpoint (same 127.0.0.1:60106 the go-ios CLI agent uses by default, both
// sides honoring GO_IOS_AGENT_HOST/GO_IOS_AGENT_PORT).
//
// Pair records live under the node's state dir, NOT go-ios's /var/db/lockdown
// default — that path needs sudo on a stock Mac and macOS 26's TCC blocks
// third-party binaries from it entirely. A stable per-host record dir also
// keeps the tunnel pairing identity across restarts (re-pairing on every
// start would trip the device's trust prompt).
//
// Failure to start is degraded, not fatal: hosts driving only iOS ≤16 phones
// never touch the tunnel, so a bind conflict (an external `ios tunnel start`
// agent already on 60106) or a state-dir problem must not take node-ios down.
func StartTunnelAgent(ctx context.Context, logger *slog.Logger, stateDir string) *TunnelAgent {
	if tunnel.IsAgentRunning() {
		// An operator-run `ios tunnel start` already serves tunnel info on the
		// endpoint; reuse it instead of fighting over the port. Its tunnels are
		// equivalent for devicelink's RSD lookup.
		logger.Info("ios tunnel: reusing existing go-ios agent",
			"endpoint", ios.HttpApiHost(), "port", ios.HttpApiPort())
		return &TunnelAgent{}
	}

	// go-ios creates selfIdentity.plist and peers/*.plist inside the record
	// dir but never the dir itself.
	records := filepath.Join(stateDir, "tunnel-pair-records")
	if err := os.MkdirAll(records, 0o700); err != nil {
		logger.Warn("ios tunnel: cannot create pair-record dir; tunnels disabled",
			"dir", records, "error", err)
		return &TunnelAgent{}
	}
	pm, err := tunnel.NewPairRecordManager(records)
	if err != nil {
		logger.Warn("ios tunnel: cannot init pair records; tunnels disabled",
			"error", err)
		return &TunnelAgent{}
	}

	tm := tunnel.NewTunnelManager(pm, true)
	// paused is shared between the reconcile goroutine (reads it every tick) and
	// the returned TunnelAgent (Pause/Resume write it). A pointer, not a field on
	// the struct, because the goroutine starts before the struct exists.
	paused := new(atomic.Bool)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		// Reconcile ONLY when something actually changed.
		//
		// go-ios's UpdateTunnels tears down every healthy tunnel the moment its
		// internal ListDevices() fails once. On Windows the Apple Mobile Device
		// Service drops ~1-in-8 dials under concurrent load (measured: 8-probe
		// runs fail on the 8th; the standalone upstream `ios tunnel start`
		// thrashes identically — this is upstream behaviour, not our code). At a
		// 1s tick that tore the RSD tunnel down 1418 times in one day while the
		// phone stayed attached, and every consumer (WDA jobs, the always-on
		// runner loop, syslog) kept hitting "no RSD tunnel" gaps.
		//
		// go-ios is a read-only module dependency, so the destructive path is
		// avoided by never calling UpdateTunnels unless reconciliation is
		// actually needed:
		//   1. probe usbmuxd ourselves first;
		//   2. probe FAILED  → state unknown → keep tunnels, skip (a real unplug
		//      is detected on the next successful probe anyway). After 10
		//      consecutive failures force one reconcile so a departed device
		//      can't keep a stale tunnel forever while the service is wedged;
		//   3. probe OK and the device set is UNCHANGED and every present device
		//      already has a tunnel → nothing to reconcile, skip (this is the
		//      steady state — no destructive call ever runs);
		//   4. otherwise (device plugged/unplugged, tunnel missing) → reconcile.
		// A genuine unplug still tears down its tunnel: the probe succeeds
		// without the device, the set differs, and UpdateTunnels cleans up.
		lastReconciledSet := ""
		probeFailStreak := 0
		var lastReconcileAttempt time.Time
		const forceAfterFailures = 10 // ~10s of continuous probe failure
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Paused while a WDA job is in flight: never reconcile mid-job,
				// even if a device change landed (see Pause).
				if paused.Load() && tm.FirstUpdateCompleted() {
					continue
				}
				list, err := ios.ListDevices()
				if err != nil {
					probeFailStreak++
					if probeFailStreak < forceAfterFailures {
						logger.Debug("ios tunnel: usbmuxd probe failed; keeping tunnels",
							"error", err, "streak", probeFailStreak)
						continue
					}
					probeFailStreak = 0 // safety valve: force one reconcile
				} else {
					probeFailStreak = 0
					set := deviceSetKey(list.DeviceList)
					if set == lastReconciledSet && allDevicesTunneled(tm, list.DeviceList) {
						continue // steady state: membership unchanged, tunnels up
					}
					// Membership changed (or a tunnel is missing) → reconcile.
					// Throttle to one attempt every 5s while a tunnel stays
					// missing (a device that refuses to tunnel — untrusted,
					// pairing revoked — must not turn us back into a 1s storm).
					if set == lastReconciledSet && time.Since(lastReconcileAttempt) < 5*time.Second {
						continue
					}
					lastReconcileAttempt = time.Now()
					if uerr := tm.UpdateTunnels(ctx); uerr == nil {
						lastReconciledSet = set
					} else {
						logger.Debug("ios tunnel: update failed", "error", uerr)
					}
					continue
				}
				// Safety-valve path: probe kept failing, force reconcile.
				if uerr := tm.UpdateTunnels(ctx); uerr != nil {
					logger.Debug("ios tunnel: update failed", "error", uerr)
				}
			}
		}
	}()
	go func() {
		if err := tunnel.ServeTunnelInfo(tm, ios.HttpApiHost(), ios.HttpApiPort()); err != nil {
			logger.Warn("ios tunnel: info server exited", "error", err)
		}
	}()
	logger.Info("ios tunnel: manager started (userspace, no root)",
		"endpoint", ios.HttpApiHost(), "port", ios.HttpApiPort(),
		"pair_records", records)
	return &TunnelAgent{tm: tm, pm: pm, paused: paused}
}

// deviceSetKey renders a device list into a comparable membership key (sorted
// UDIDs). Two ticks with the same key mean nothing plugged or unplugged —
// reconciliation can then be skipped entirely (see the ticker loop).
func deviceSetKey(list []ios.DeviceEntry) string {
	udids := make([]string, 0, len(list))
	for _, d := range list {
		udids = append(udids, d.Properties.SerialNumber)
	}
	sort.Strings(udids)
	return strings.Join(udids, ",")
}

// allDevicesTunneled reports whether every attached device already has a
// tunnel. Together with an unchanged membership key this is the steady state
// in which UpdateTunnels — and with it the destructive teardown path — is
// never called.
func allDevicesTunneled(tm *tunnel.TunnelManager, list []ios.DeviceEntry) bool {
	tunnels, err := tm.ListTunnels()
	if err != nil {
		return false // can't confirm coverage → reconcile
	}
	byUdid := make(map[string]bool, len(tunnels))
	for _, t := range tunnels {
		byUdid[t.Udid] = true
	}
	for _, d := range list {
		if !byUdid[d.Properties.SerialNumber] {
			return false
		}
	}
	return true
}

// TunnelAgent owns the embedded tunnel manager's lifecycle. A nil tm means the
// agent reused an external one or degraded at startup — Close is then a no-op
// (and the zero value keeps the cmdRun wiring uniform).
//
// Pause/Resume let a caller (the WDA job engine) stop the reconcile loop for the
// duration of a job. The reason is go-ios's UpdateTunnels design: ONE transient
// ListDevices() failure (Apple Mobile Device Service on Windows drops ~1 in 8
// concurrent dials — reproducible with a bare probe) tears down every healthy
// tunnel, which yanks the RSD connection out from under an in-flight WDA launch
// (install succeeds, then "wda_unreachable" 3 seconds later). While a WDA job is
// running the poller must not run at all: no ListDevices, no teardown, and the
// already-negotiated tunnel stays put. Resume restores normal reconcile after
// the last job drains (unplug/reconnect sensing).
type TunnelAgent struct {
	tm     *tunnel.TunnelManager
	pm     tunnel.PairRecordManager
	paused *atomic.Bool
	lan    *LanAgent
}

// SetLanHooks wires the LAN tunnel agent into this TunnelAgent. Must be called
// after the DeviceManager exists (it supplies claimed UDIDs and the
// reachability/pairing hooks). Safe to call on a degraded agent (no tm).
func (a *TunnelAgent) SetLanHooks(ctx context.Context, logger *slog.Logger, targets func() []string, hook LanHostHooks) {
	if a == nil || a.tm == nil {
		// Degraded start: tunnels are served by an operator's external
		// `ios tunnel start` agent, whose manager we cannot register on. LAN
		// control needs the manager we own, so it stays off — say so instead of
		// silently ignoring the flag.
		if logger != nil && lanEnabled() {
			logger.Warn("ios lan: disabled — no tunnel manager of our own (external go-ios agent is serving tunnel info)")
		}
		return
	}
	a.lan = newLanAgent(logger, goiosLanOpener{pm: a.pm, log: logger}, managerRegistry{tm: a.tm}, targets, hook, a.paused)
	a.lan.Start(ctx)
}

// Pause stops the reconcile loop. Idempotent; nested jobs (overlapping prepare
// and renew on different devices) are fine — the engine only resumes when the
// last job drains.
func (a *TunnelAgent) Pause() {
	if a == nil || a.paused == nil {
		return
	}
	a.paused.Store(true)
}

// Resume restarts the reconcile loop.
func (a *TunnelAgent) Resume() {
	if a == nil || a.paused == nil {
		return
	}
	a.paused.Store(false)
}

// Close tears down every tunnel this manager created — USB ones via the
// manager's Close, LAN ones via the agent's Stop (which removes the external
// tunnels registered on the same manager). Safe on the zero value and
// idempotent (TunnelManager.Close is closeOnce-guarded).
func (a *TunnelAgent) Close() error {
	if a == nil {
		return nil
	}
	if a.lan != nil {
		a.lan.Stop()
	}
	if a.tm == nil {
		return nil
	}
	return a.tm.Close()
}
