//go:build !windows

package agent

import (
	"os"
	"strconv"
	"syscall"
)

// processAlive reports whether pid names a live process this user can signal.
// Signal 0 performs the permission/existence check without delivering anything.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// ownerProcessPath returns the executable backing pid. The bool is false when the
// platform cannot report it (no procfs, e.g. macOS), in which case the caller
// falls back to a liveness + recorded-name check instead of a path comparison.
func ownerProcessPath(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	path, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil || path == "" {
		return "", false
	}
	return path, true
}
