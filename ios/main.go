// Command node-ios is the iOS device host for device-control. It drives one or
// more iPhones over USB/LAN (go-ios + WebDriverAgent) and dials a device-control
// server exactly like the Android app, so the server cannot tell it apart on the
// wire. It is a separate, small binary — NOT the agent-compose execution or
// management node: device-control uses its own pairing-code → long-lived-token
// auth and its own WebSocket, none of which reuses the node NodeConnect/TOTP
// stack, so folding it into execution would only drag go-ios into every node.
//
// Two subcommands, mirroring the reference device-control-ios CLI but scaled to
// "one host, many phones":
//
//	node-ios pair --server URL --code CODE --udid X [--name N]
//	              [--wda-bundle ID --xctest NAME --wda-port N] [--config PATH]
//	node-ios run  [--config PATH]
//
// pair redeems a pairing code (pure HTTP, no device needed), writes the 0600
// credential, and records the device in the devices config. run loads the config
// and runs one connection loop per device on its own goroutine.
//
// The real-device link (go-ios discovery + WDA launch/port-forward) lives in the
// device-control/ios driver and is still a work in progress there; without an
// attached iPhone, run logs a per-device "no iOS device found" and keeps the
// process alive. Pairing is fully exercisable without a device.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"device-control/ios/devicecontrol"

	"ai-lubricant-nodes/common/agent"
	"ai-lubricant-nodes/common/build"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// buildVersion is overridden at link time, mirroring the other node binaries.
var buildVersion = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "install":
		cmdInstall(os.Args[2:])
	case "pair":
		cmdPair(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `node-ios — iOS device host for device-control

  node-ios install --server URL --node-id ID --secret S [--name N] [--tls-insecure]
                   [--config PATH]
  node-ios pair   --server URL --code CODE --udid X [--name N]
                   [--wda-bundle ID --xctest NAME --wda-port N] [--config PATH]
  node-ios run    [--config PATH]

  install saves the NodeConnect identity (server/node-id/secret) so `+"`run`"+`
       registers as an ios_host node: node page presence, version report,
       self-upgrade. Without it, run is pure device-control mode (no node page).
  pair redeems a device-control pairing code, stores the 0600 credential, and
       records the device (UDID + WDA params) in the devices config for run.
       --host-node-id lets devices register as hosted by an existing node
       (no second node identity is created).
  run connects one goroutine per configured device and serves inbound calls;
       with a node identity it also dials the NodeConnect control plane.

  --config defaults to <user-config-dir>/agent-compose/ios/devices.json.
  The WDA params (bundle id, xctest name, forwarded port) come from your
  Mac-built WebDriverAgent — see device-control/ios/README.md.`)
}

func cmdRun(args []string) {
	fs := newFlagSet("run")
	configPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	logger := agent.SetupLogger("info")
	slog.SetDefault(logger)

	// iOS-scoped single-instance lock: two `node-ios run` on one host would each
	// spawn a WS connection per device against the same device_id, which the
	// spec carries one device_id each. No rebind flow — iOS has no TOTP node
	// identity to verify the old process against; the operator stops it manually.
	release, err := acquireLock()
	if err != nil {
		logger.Error("acquire instance lock", "error", err)
		os.Exit(1)
	}
	defer release()

	cfg, path, err := LoadDevicesConfig(*configPath)
	if err != nil {
		logger.Error("load devices config", "error", err)
		os.Exit(1)
	}
	if len(cfg.Devices) == 0 && cfg.Node == nil {
		logger.Error("nothing configured; run 'node-ios pair' (device) or 'node-ios install' (node) first", "config", path)
		os.Exit(1)
	}

	logger.Info("node-ios starting", "version", buildVersion, "config", path,
		"devices", len(cfg.Devices), "node_mode", cfg.Node != nil)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	// NodeConnect control plane (identity / heartbeat / version / self-upgrade /
	// iOS device management). Absent identity = pure device mode: the host serves
	// only the devices already in devices.json and cannot be driven from the
	// console. HostNodeID lets such a sidecar still associate its iPhones with an
	// existing execution node without registering a second NodeConnect identity.
	nodeID := cfg.HostNodeID
	if cfg.Node != nil {
		nodeID = cfg.Node.NodeID
		client := agent.NewClient(nodeClientOptions(cfg.Node), logger)

		// The manager owns discovery and the per-device connection loops; the job
		// engine owns WDA preparation. Both report upstream through the client's
		// stable EmitUpstream, which is why they are built after the client.
		manager := NewDeviceManager(ManagerConfig{
			Logger:        logger,
			ConfigPath:    path,
			DevicesConfig: cfg,
			NodeID:        nodeID,
			OnReport: func(rep *agentcomposev2.NodeIosDevicesReport) error {
				return client.EmitUpstream(&agentcomposev2.NodeUpstreamFrame{
					Frame: &agentcomposev2.NodeUpstreamFrame_IosDevicesReport{IosDevicesReport: rep},
				})
			},
		})
		jobs := NewWdaJobManager(client.EmitUpstream, newGoiosWdaSteps(logger, client), stateDir(path), logger)
		// A macOS iOS host is also the project-page build node (xcodebuild
		// lives here). The runner is always wired; the server picks hosts by
		// the xcodebuild_version capability label, so non-macOS hosts simply
		// never receive a build frame.
		builds := build.NewRunner(client.EmitUpstream, logger)

		client.SetHandler(NewHandler(client, manager, jobs, builds))
		manager.Start(ctx)
		manager.StartClaimedDevices(ctx)
		defer manager.Stop()
		defer jobs.StopAll()
		defer builds.StopAll()

		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := client.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("node control plane exited with error", "error", err)
			}
		}()
		wg.Wait()
		logger.Info("node-ios stopped")
		return
	}

	logger.Info("no node identity; running in pure device mode (run 'node-ios install' to appear on the node page)")
	for _, dev := range cfg.Devices {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runDevice(ctx, logger, dev, nodeID)
		}()
	}
	wg.Wait()
	logger.Info("node-ios stopped")
}

// stateDir returns the directory WDA jobs use for scratch space (downloaded
// artifacts, signing material). It sits beside devices.json so everything the
// host writes lives under one owner-only tree.
func stateDir(configPath string) string {
	return filepath.Dir(configPath)
}

// nodeClientOptions builds the shared agent connection options from the iOS
// host's persisted node identity. Role is fixed to ios_host; it advertises no
// providers (runs no coding sessions).
//
// The ios_mgmt label is the capability gate for the iOS management frames
// (discover/claim/release/configure + WDA jobs). The server only sends those to
// a node that advertises it, so an older node-ios — which would log-and-drop
// them — never gets one, and the console can tell the user to upgrade instead of
// leaving a button that times out.
func nodeClientOptions(id *NodeIdentity) agent.Options {
	name := id.NodeName
	if name == "" {
		name = agent.DefaultNodeName()
	}
	return agent.Options{
		Server:      id.Server,
		NodeID:      id.NodeID,
		Secret:      id.Secret,
		NodeName:    name,
		Role:        agent.RoleIosHost,
		Version:     buildVersion,
		TLSInsecure: id.TLSInsecure,
		Labels:      map[string]string{"ios_mgmt": "true"},
		MinBackoff:  time.Second,
		MaxBackoff:  30 * time.Second,
		Heartbeat:   15 * time.Second,
	}
}

// runDevice runs the connection loop for one device until ctx is cancelled or a
// terminal error (ErrNotPaired, 4003/4004 fatal) lands. RunDeviceWithRetry
// handles both WS-drop reconnects (inside ConnectLoop) and start-failure retries
// (device not attached, WDA down) with full-jitter backoff, so a USB replug or a
// WDA 7-day re-sign self-heals without a process restart — one flaky phone never
// forces a restart that drops the other N.
//
// nodeID, when non-empty, is carried in the device's register frame so the
// server can join this device to its hosting node (version / online / upgrade).
func runDevice(ctx context.Context, logger *slog.Logger, dev DeviceConfig, nodeID string) {
	log := logger.With("device", dev.Name, "udid", dev.UDID)
	log.Info("device connecting")

	var extra map[string]any
	if nodeID != "" {
		extra = map[string]any{"node_id": nodeID}
	}

	err := devicecontrol.RunDeviceWithRetry(ctx, devicecontrol.RunConfig{
		CredentialPath:  dev.CredentialPath,
		DeviceInfoExtra: extra,
		Device: devicecontrol.Options{
			UDID:         dev.UDID,
			Transport:    dev.Transport,
			WDABundleID:  dev.WDABundle,
			XCTestConfig: dev.XCTest,
			WDAPort:      dev.WDAPort,
		},
		OnState: func(s devicecontrol.State) {
			log.Info("device state", "state", s) // State has a String() method
		},
		OnWipe: func() {
			log.Warn("device credential wiped (auth failed); re-pair required")
		},
	})

	// RunDeviceWithRetry returns nil on ctx cancel, a non-nil error only on a
	// terminal result (ErrNotPaired, fatal close). Start failures never escape —
	// they're retried inside — so a returned error is always worth surfacing.
	if err != nil {
		log.Error("device ended", "error", err)
	} else {
		log.Info("device stopped")
	}
}
