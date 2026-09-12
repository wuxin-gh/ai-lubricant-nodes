//go:build linux

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// autostartAvailablePlatform: Linux per-user systemd (the same mechanism the
// install script uses when a systemd user session exists).
func autostartAvailablePlatform() (string, bool) {
	bin, err := exec.LookPath("systemctl")
	if err != nil {
		return "", false
	}
	// A usable *user* manager is the real requirement (root login sessions
	// without a user bus have no systemd --user). systemctl --user show is
	// the same probe the install script uses.
	if err := exec.Command(bin, "--user", "show").Run(); err != nil {
		return "", false
	}
	return AutostartMethodSystemd, true
}

// systemdUnitPath is the fixed user-unit path (same name the install script
// writes, so the two never duplicate).
func systemdUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", "agent-compose-node.service"), nil
}

func autostartInstalledPlatform() bool {
	path, err := systemdUnitPath()
	if err != nil {
		return false
	}
	_, statErr := os.Stat(path)
	return statErr == nil
}

func autostartInstallPlatform() error {
	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	launcher, err := autostartLauncherPath()
	if err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=Agent Compose node
After=network-online.target

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, launcher)
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "--now", "agent-compose-node.service"},
	} {
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %v: %s: %w", args, string(out), err)
		}
	}
	return nil
}

func autostartRemovePlatform() error {
	path, err := systemdUnitPath()
	if err != nil {
		return err
	}
	_ = exec.Command("systemctl", "--user", "disable", "--now", "agent-compose-node.service").Run()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
