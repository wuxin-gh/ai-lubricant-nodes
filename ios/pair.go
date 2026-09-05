package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"device-control/ios/devicecontrol"

	"ai-lubricant-nodes/common/agent"
)

// newFlagSet builds a flag set that reports parse errors without os.Exit, so the
// caller controls the exit path (and tests can drive it).
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// configFlag registers the shared --config flag (empty = default location).
func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "",
		"path to the devices config JSON (env "+configEnvOverride+"); default is <user-config-dir>/agent-compose/ios/devices.json")
}

// cmdPair redeems a pairing code, stores the credential, and records the device
// in the config for `run` to pick up. Pairing is pure HTTP: it needs no attached
// iPhone, so it is fully verifiable on its own.
func cmdPair(args []string) {
	fs := newFlagSet("pair")
	configPath := configFlag(fs)
	server := fs.String("server", "", "device-control server base URL, e.g. http://host:8001 (bare host is fine; the /mcp/device-control prefix is auto-probed)")
	code := fs.String("code", "", "pairing code minted by the server")
	udid := fs.String("udid", "", "device UDID (from `go-ios list` / `idevice_id -l`; empty means first available at run time)")
	name := fs.String("name", "", "human label for this device (default: the UDID, or 'device')")
	transport := fs.String("transport", "usb", "usb|network")
	wdaBundle := fs.String("wda-bundle", "", "WebDriverAgent Runner bundle id (e.g. com.you.WebDriverAgentRunner.xctrunner)")
	xctest := fs.String("xctest", "WebDriverAgentRunner.xctest", "xctest config name inside the runner")
	wdaPort := fs.Int("wda-port", 0, "forwarded WDA HTTP port (the port WDA prints on start)")
	// host-node-id associates this sidecar with an existing agent-compose node
	// (typically the execution node on the same host) WITHOUT registering a
	// second NodeConnect identity: the id is carried in each device's register
	// frame so the server joins device ↔ node for version/online display.
	hostNodeID := fs.String("host-node-id", "", "associate devices with this agent-compose node id (no second node identity is created)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	logger := agent.SetupLogger("info")
	slog.SetDefault(logger)

	// Trim once into locals; every downstream use (check, Pair, Upsert) reads
	// these. The reference CLI trims at each use site; doing it once avoids the
	// double-TrimSpace and keeps a single source of truth for the parsed value.
	serverVal := strings.TrimSpace(*server)
	codeVal := strings.TrimSpace(*code)
	udidVal := strings.TrimSpace(*udid)
	deviceName := strings.TrimSpace(*name)
	if deviceName == "" {
		if udidVal != "" {
			deviceName = udidVal
		} else {
			deviceName = "device"
		}
	}
	if serverVal == "" || codeVal == "" {
		fmt.Fprintln(os.Stderr, "pair: --server and --code are required")
		os.Exit(2)
	}

	cfg, path, err := LoadDevicesConfig(*configPath)
	if err != nil {
		logger.Error("load devices config", "error", err)
		os.Exit(1)
	}
	credPath := defaultCredentialPath(path, deviceName)

	// Pair redeems the code (POST /pair, probing the /mcp/device-control prefix)
	// and writes the 0600 credential holding the probe-effective server URL. run
	// reads the live server URL from that credential, so the device config stores
	// no server field of its own (a stored copy would only drift from it).
	cred, err := devicecontrol.Pair(context.Background(), serverVal, codeVal, credPath)
	if err != nil {
		logger.Error("pair failed", "error", err)
		os.Exit(1)
	}

	if hostNodeIDVal := strings.TrimSpace(*hostNodeID); hostNodeIDVal != "" {
		cfg.HostNodeID = hostNodeIDVal
	}

	dev := cfg.Upsert(DeviceConfig{
		Name:           deviceName,
		UDID:           udidVal,
		Transport:      strings.TrimSpace(*transport),
		WDABundle:      strings.TrimSpace(*wdaBundle),
		XCTest:         strings.TrimSpace(*xctest),
		WDAPort:        *wdaPort,
		CredentialPath: credPath,
	})
	if err := SaveDevicesConfig(path, cfg); err != nil {
		logger.Error("save devices config", "error", err)
		os.Exit(1)
	}

	logger.Info("device paired",
		"device", dev.Name,
		"device_id", cred.DeviceID,
		"server", cred.ServerURL,
		"credential", credPath,
		"config", path)
	fmt.Printf("paired: device=%s device_id=%s server=%s\n", dev.Name, cred.DeviceID, cred.ServerURL)
	fmt.Println("now run: node-ios run")
}
