//go:build !windows

package agent

import (
	"os"
	"syscall"
)

// tryPlatformLock takes the host-wide node lock. Delegates to LockFile; the
// owner-file write/read and PID verification live in lock_owner.go.
func tryPlatformLock() (func() error, bool, error) {
	path, err := LockPath()
	if err != nil {
		return nil, false, err
	}
	return LockFile(path)
}

// stopProcess asks the old node to exit gracefully so it can quiesce sessions.
func stopProcess(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}
