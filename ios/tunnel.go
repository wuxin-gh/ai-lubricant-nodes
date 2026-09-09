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
package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	ios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/tunnel"
)

// startTunnelAgent runs go-ios's tunnel manager in-process (userspace mode —
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
func startTunnelAgent(ctx context.Context, logger *slog.Logger, stateDir string) *tunnelAgent {
	if tunnel.IsAgentRunning() {
		// An operator-run `ios tunnel start` already serves tunnel info on the
		// endpoint; reuse it instead of fighting over the port. Its tunnels are
		// equivalent for devicelink's RSD lookup.
		logger.Info("ios tunnel: reusing existing go-ios agent",
			"endpoint", ios.HttpApiHost(), "port", ios.HttpApiPort())
		return &tunnelAgent{}
	}

	// go-ios creates selfIdentity.plist and peers/*.plist inside the record
	// dir but never the dir itself.
	records := filepath.Join(stateDir, "tunnel-pair-records")
	if err := os.MkdirAll(records, 0o700); err != nil {
		logger.Warn("ios tunnel: cannot create pair-record dir; tunnels disabled",
			"dir", records, "error", err)
		return &tunnelAgent{}
	}
	pm, err := tunnel.NewPairRecordManager(records)
	if err != nil {
		logger.Warn("ios tunnel: cannot init pair records; tunnels disabled",
			"error", err)
		return &tunnelAgent{}
	}

	tm := tunnel.NewTunnelManager(pm, true)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := tm.UpdateTunnels(ctx); err != nil {
					logger.Debug("ios tunnel: update failed", "error", err)
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
	return &tunnelAgent{tm: tm}
}

// tunnelAgent owns the embedded tunnel manager's lifecycle. A nil tm means the
// agent reused an external one or degraded at startup — Close is then a no-op
// (and the zero value keeps the cmdRun wiring uniform).
type tunnelAgent struct {
	tm *tunnel.TunnelManager
}

// Close tears down every tunnel this manager created. Safe on the zero value
// and idempotent (TunnelManager.Close is closeOnce-guarded).
func (a *tunnelAgent) Close() error {
	if a == nil || a.tm == nil {
		return nil
	}
	return a.tm.Close()
}
