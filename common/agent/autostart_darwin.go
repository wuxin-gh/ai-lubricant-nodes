//go:build darwin

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// autostartAvailablePlatform: macOS per-user LaunchAgents. launchctl exists on
// every macOS; ~/Library/LaunchAgents is where per-user agents live.
func autostartAvailablePlatform() (string, bool) {
	if _, err := exec.LookPath("launchctl"); err != nil {
		return "", false
	}
	return AutostartMethodLaunchd, true
}

// launchdPlistPath is the fixed agent plist path (same name the install script
// would use, so the two never create duplicates).
func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", "com.agent-compose.node.plist"), nil
}

// autostartInstalledPlatform: the plist exists (whether we or the operator
// wrote it — same fixed name).
func autostartInstalledPlatform() bool {
	path, err := launchdPlistPath()
	if err != nil {
		return false
	}
	_, statErr := os.Stat(path)
	return statErr == nil
}

// autostartInstallPlatform writes the fixed-name LaunchAgent pointing at the
// durable launcher and loads it now. Idempotent: boot-out first (ignoring
// "not loaded"), rewrite, bootstrap again.
func autostartInstallPlatform() error {
	path, err := launchdPlistPath()
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
	// RunAtLoad starts the agent at login; KeepAlive restarts it if the
	// process exits. The state-dir env the launcher needs is baked into
	// start-node.sh itself.
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.agent-compose.node</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>%s</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launcher, autostartLogPath("launchd.out"), autostartLogPath("launchd.err"))
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}
	// Unload any previous instance first so the rewrite takes effect now,
	// not at next login. Both failures are acceptable here (not loaded yet).
	_ = exec.Command("launchctl", "unload", path).Run()
	if err := exec.Command("launchctl", "load", path).Run(); err != nil {
		return fmt.Errorf("launchctl load: %w", err)
	}
	return nil
}

// autostartRemovePlatform unloads and deletes the agent.
func autostartRemovePlatform() error {
	path, err := launchdPlistPath()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "unload", path).Run()
	return os.Remove(path)
}
