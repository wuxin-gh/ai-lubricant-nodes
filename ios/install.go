package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"ai-lubricant-nodes/common/agent"
)

// cmdInstall saves the NodeConnect identity (server/node-id/secret) into the
// devices config so `run` registers as an ios_host node. It mirrors the
// execution/management `--install` shape but persists into the iOS config
// (agent-compose/ios/devices.json), not the host-wide LocalConfig, so the two
// node roles do not contend on the host-wide single-instance lock.
func cmdInstall(args []string) {
	fs := newFlagSet("install")
	configPath := configFlag(fs)
	server := fs.String("server", "", "agent-compose server base URL, e.g. https://host:7410")
	nodeID := fs.String("node-id", "", "node id minted at onboard (node-<uuid>)")
	secret := fs.String("secret", "", "node TOTP secret (base32) minted at onboard")
	name := fs.String("name", "", "human-readable node name (default: the host name)")
	tlsInsecure := fs.Bool("tls-insecure", false, "skip TLS certificate verification (dev only)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	logger := agent.SetupLogger("info")
	slog.SetDefault(logger)

	serverVal := strings.TrimSpace(*server)
	nodeIDVal := strings.TrimSpace(*nodeID)
	secretVal := strings.TrimSpace(*secret)
	if serverVal == "" || nodeIDVal == "" || secretVal == "" {
		fmt.Fprintln(os.Stderr, "install: --server, --node-id and --secret are required")
		os.Exit(2)
	}

	cfg, path, err := LoadDevicesConfig(*configPath)
	if err != nil {
		logger.Error("load devices config", "error", err)
		os.Exit(1)
	}

	nodeName := strings.TrimSpace(*name)
	if nodeName == "" {
		nodeName = agent.DefaultNodeName()
	}
	cfg.Node = &NodeIdentity{
		Server:      strings.TrimRight(serverVal, "/"),
		NodeID:      nodeIDVal,
		Secret:      secretVal,
		NodeName:    nodeName,
		TLSInsecure: *tlsInsecure,
	}
	if err := SaveDevicesConfig(path, cfg); err != nil {
		logger.Error("save devices config", "error", err)
		os.Exit(1)
	}

	logger.Info("ios host identity installed",
		"node_id", nodeIDVal,
		"server", cfg.Node.Server,
		"name", nodeName,
		"config", path)
	fmt.Println("installed; run `node-ios run` to connect as an ios_host node")
}
