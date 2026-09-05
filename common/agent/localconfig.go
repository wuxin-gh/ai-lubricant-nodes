package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LocalConfig is the node identity + connection parameters persisted on the host
// so a node can be restarted with no arguments. It is written once at install /
// rebind time and read on every subsequent launch.
//
// Only the two "manual" host binaries (execution / management installed directly
// on a machine) use this. Management-launched execution children are transient
// and keep passing credentials on the command line.
type LocalConfig struct {
	Server   string `json:"server"`
	NodeID   string `json:"node_id"`
	Secret   string `json:"secret"`
	Role     string `json:"role"`
	NodeName string `json:"node_name,omitempty"`
	// Role-specific extras kept so a no-arg restart reproduces the install exactly.
	WorkRoot     string `json:"work_root,omitempty"`
	Providers    string `json:"providers,omitempty"`
	AgentImage   string `json:"agent_image,omitempty"`
	ExecutionBin string `json:"execution_bin,omitempty"`
	Labels       string `json:"labels,omitempty"`
	TLSInsecure  bool   `json:"tls_insecure,omitempty"`
}

// configEnvOverride lets an operator relocate the state dir (tests, packaging).
const configEnvOverride = "AGENT_COMPOSE_NODE_STATE_DIR"

// stateDir returns the per-user directory holding the node config + lock. It is a
// single host-wide location shared by both roles so the whole-machine single
// instance rule can be enforced across the two binaries.
func stateDir() (string, error) {
	return ResolveStateDir(configEnvOverride, "node")
}

// ConfigPath is the absolute path of the persisted node config.
func ConfigPath() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// LockPath is the absolute path of the host-wide single-instance lock. It sits
// beside the config so both roles contend on the same file.
func LockPath() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "node.lock"), nil
}

// LoadLocalConfig reads the persisted config. It returns (nil, nil) when no
// config has been written yet — a fresh machine, not an error.
func LoadLocalConfig() (*LocalConfig, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read node config: %w", err)
	}
	var cfg LocalConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse node config %s: %w", path, err)
	}
	return &cfg, nil
}

// SaveLocalConfig writes the config atomically with owner-only permissions so the
// secret is not world-readable. Delegates to AtomicWriteJSON for the shared
// cross-platform write path (Windows MoveFileEx replace + ACL tightening).
func SaveLocalConfig(cfg *LocalConfig) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	return AtomicWriteJSON(path, cfg)
}

// SameCredentials reports whether the persisted config already targets this exact
// (server, node_id, secret, role). A repeated install with identical credentials
// is idempotent and must not prompt or recreate anything.
func (c *LocalConfig) SameCredentials(server, nodeID, secret, role string) bool {
	if c == nil {
		return false
	}
	return c.Server == strings.TrimRight(strings.TrimSpace(server), "/") &&
		c.NodeID == strings.TrimSpace(nodeID) &&
		c.Secret == strings.TrimSpace(secret) &&
		c.Role == strings.TrimSpace(role)
}

// Describe returns a short, secret-free summary for confirmation prompts/logs.
func (c *LocalConfig) Describe() string {
	if c == nil {
		return "(none)"
	}
	name := c.NodeName
	if name == "" {
		name = "-"
	}
	return fmt.Sprintf("node_id=%s role=%s name=%s server=%s", c.NodeID, c.Role, name, c.Server)
}
