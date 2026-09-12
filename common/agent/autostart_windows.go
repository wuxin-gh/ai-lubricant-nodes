//go:build windows

package agent

import (
	"fmt"
	"os/exec"
	"strings"
)

// schtasksTaskName is the fixed per-user logon task (same name the install
// script writes, so the two never duplicate). A per-user ONLOGON task runs
// the launcher at the operator's login without elevation.
const schtasksTaskName = "agent-compose-node"

func autostartAvailablePlatform() (string, bool) {
	if _, err := exec.LookPath("schtasks.exe"); err != nil {
		return "", false
	}
	return AutostartMethodSchtasks, true
}

func autostartInstalledPlatform() bool {
	// schtasks /Query exits 0 when the task exists.
	return exec.Command("schtasks.exe", "/Query", "/TN", schtasksTaskName).Run() == nil
}

func autostartInstallPlatform() error {
	launcher, err := autostartLauncherPath()
	if err != nil {
		return err
	}
	// /F overwrites any existing task of the same name (idempotent reinstall).
	// The task runs the durable launcher (start-node.cmd), not the binary, so a
	// self-upgrade never breaks login autostart.
	cmd := exec.Command("schtasks.exe", "/Create", "/F", "/SC", "ONLOGON", "/RL", "LIMITED",
		"/TN", schtasksTaskName, "/TR", launcher)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks /Create: %s: %w", string(out), err)
	}
	return nil
}

func autostartRemovePlatform() error {
	if out, err := exec.Command("schtasks.exe", "/Delete", "/F", "/TN", schtasksTaskName).CombinedOutput(); err != nil {
		// "The task does not exist" is not an error here.
		if !strings.Contains(string(out), "cannot find") {
			return fmt.Errorf("schtasks /Delete: %s: %w", string(out), err)
		}
	}
	return nil
}
