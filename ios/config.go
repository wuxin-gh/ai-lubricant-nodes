package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ai-lubricant-nodes/common/agent"
)

// DeviceConfig is one iPhone this host drives. WDA params come from the operator's
// Mac-built WebDriverAgent (see device-control/ios/README.md); the paired
// credential (server URL + device_id + token) lives in its own 0600 file at
// CredentialPath, written by devicecontrol.Pair.
type DeviceConfig struct {
	// Name is a human label for logs; also the upsert key when UDID is empty.
	Name string `json:"name"`
	// UDID selects the device via go-ios; empty means "first available".
	UDID string `json:"udid"`
	// Transport is "usb" (default, stable) or "network" (LAN WiFi).
	Transport string `json:"transport,omitempty"`
	// WDABundle is the WebDriverAgentRunner bundle id (user-built, signed).
	WDABundle string `json:"wda_bundle,omitempty"`
	// XCTest is the .xctest config name inside the runner.
	XCTest string `json:"xctest,omitempty"`
	// WDAPort is the local port WDA's HTTP server is forwarded to.
	WDAPort int `json:"wda_port,omitempty"`
	// CredentialPath is the 0600 file written by Pair holding {server, device_id,
	// token}. One per device.
	CredentialPath string `json:"credential_path"`
}

// DevicesConfig is the on-disk device list this host manages.
type DevicesConfig struct {
	// Node, when present, is the NodeConnect identity this host registers with
	// so it appears on the node page and can self-upgrade. Absent = pure device
	// mode (device-control WS only, no node identity), the original behavior.
	Node    *NodeIdentity  `json:"node,omitempty"`
	// HostNodeID associates devices with an existing agent-compose node (e.g. the
	// execution node on the same host) when running in pure device mode — no
	// second NodeConnect identity is created; the id is carried in each device's
	// register frame so the server joins device ↔ node for display only.
	HostNodeID string         `json:"host_node_id,omitempty"`
	Devices    []DeviceConfig `json:"devices"`
}

// NodeIdentity is the agent-compose node credential minted at onboard, persisted
// alongside the device list so `run` can dial the NodeConnect control plane in
// addition to the per-device device-control WebSockets. It intentionally lives
// here (not in agent.LocalConfig) so the iOS host does not contend on the
// host-wide node lock with an execution/management node on the same machine.
type NodeIdentity struct {
	Server      string `json:"server"`
	NodeID      string `json:"node_id"`
	Secret      string `json:"secret"`
	NodeName    string `json:"node_name,omitempty"`
	TLSInsecure bool   `json:"tls_insecure,omitempty"`
}

// configEnvOverride relocates the iOS config dir (tests, packaging). Single env
// var for both the flag default and the dir override — there is no separate
// "_DIR" form to confuse an operator into setting the wrong one.
const configEnvOverride = "AGENT_COMPOSE_IOS_CONFIG_DIR"

// configDir returns <user-config-dir>/agent-compose/ios, honoring the env
// override. Delegates to the shared agent.ResolveStateDir so the resolution
// algorithm cannot drift from the other node binaries' config dir.
func configDir() (string, error) {
	return agent.ResolveStateDir(configEnvOverride, "ios")
}

// resolveConfigPath returns the config path to use: the explicit flag when set,
// otherwise <configDir>/devices.json.
func resolveConfigPath(explicit string) (string, error) {
	if p := strings.TrimSpace(explicit); p != "" {
		return p, nil
	}
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "devices.json"), nil
}

// defaultCredentialPath returns the credential file path for a device, beside the
// config as credentials/<safe-name>.json, so a device's long-lived token lives
// next to the config that references it.
func defaultCredentialPath(configPath, deviceName string) string {
	return filepath.Join(filepath.Dir(configPath), "credentials", sanitizeFileName(deviceName)+".json")
}

// lockPath returns the iOS-scoped single-instance lock file. It sits in the iOS
// config dir, separate from the agent-compose node's host-wide lock, so a node
// and an iOS host can coexist on one machine and so two `node-ios run` processes
// cannot double-dial every device.
func lockPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "ios.lock"), nil
}

// acquireLock takes the iOS-scoped single-instance lock. Reuses the shared
// platform LockFile (kernel-held, auto-released on process death). Returns
// ErrAlreadyRunning if another node-ios holds it — there is no rebind/replacement
// flow here because iOS has no TOTP node identity to verify the old process
// against; the operator stops the old one manually.
func acquireLock() (func() error, error) {
	path, err := lockPath()
	if err != nil {
		return nil, err
	}
	release, held, err := agent.LockFile(path)
	if err != nil {
		return nil, err
	}
	if !held {
		return nil, agent.ErrAlreadyRunning
	}
	return release, nil
}

// sanitizeFileName keeps a device name usable as a file stem: letters, digits,
// dash, underscore, dot; everything else becomes '_'. An empty result falls back
// to "device" so a nameless device still gets a stable path.
func sanitizeFileName(name string) string {
	var sb strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	if sb.Len() == 0 {
		return "device"
	}
	return sb.String()
}

// LoadDevicesConfig reads the device list, returning an empty config (not an
// error) when the file does not exist — a fresh host that has paired nothing yet.
// It returns the resolved path so callers can log where they looked.
func LoadDevicesConfig(explicit string) (*DevicesConfig, string, error) {
	path, err := resolveConfigPath(explicit)
	if err != nil {
		return nil, "", err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &DevicesConfig{}, path, nil
	}
	if err != nil {
		return nil, path, fmt.Errorf("read devices config: %w", err)
	}
	var cfg DevicesConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, path, fmt.Errorf("parse devices config %s: %w", path, err)
	}
	return &cfg, path, nil
}

// SaveDevicesConfig writes the device list atomically with owner-only
// permissions, delegating to agent.AtomicWriteJSON so the Windows replace + ACL
// path is shared with the node config (a re-pair overwrites devices.json in
// place, which plain os.Rename cannot do on Windows).
func SaveDevicesConfig(path string, cfg *DevicesConfig) error {
	return agent.AtomicWriteJSON(path, cfg)
}

// Upsert inserts or updates a device, matched by a single key: UDID when present
// (the stable device identity), else Name. Re-pairing the same phone under a new
// label with the same UDID updates in place rather than spawning a second
// connection goroutine for one device.
func (c *DevicesConfig) Upsert(dev DeviceConfig) DeviceConfig {
	key := dev.UDID
	matchByName := key == "" && dev.Name != ""
	for i := range c.Devices {
		existing := &c.Devices[i]
		if (key != "" && existing.UDID == key) || (matchByName && existing.Name == dev.Name) {
			*existing = dev
			return *existing
		}
	}
	c.Devices = append(c.Devices, dev)
	return dev
}
