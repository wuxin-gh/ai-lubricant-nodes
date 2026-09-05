//go:build !windows

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFile takes an exclusive advisory flock on path. The lock is kernel-held,
// so it is released automatically if the process dies — no stale-PID cleanup
// needed. Returns a release func and a non-nil error only on I/O failure; a
// contended lock is reported as ErrAlreadyRunning.
//
// The host-wide node lock (LockPath) and the iOS-scoped lock both use this.
func LockFile(path string) (release func() error, held bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, false, nil // another process holds it
		}
		return nil, false, fmt.Errorf("lock: %w", err)
	}
	return func() error {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return f.Close()
	}, true, nil
}
