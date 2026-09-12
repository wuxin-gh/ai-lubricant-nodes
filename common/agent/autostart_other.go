//go:build !darwin && !linux && !windows

package agent

// Platforms without a supported per-user autostart mechanism (BSDs, etc.)
// report unavailable; no entry is ever installed.
func autostartAvailablePlatform() (string, bool) { return "", false }
func autostartInstalledPlatform() bool           { return false }
func autostartInstallPlatform() error             { return nil }
func autostartRemovePlatform() error              { return nil }
