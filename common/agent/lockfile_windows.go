//go:build windows

package agent

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// LockFile takes an exclusive, OS-held lock by opening path with no share mode.
// Windows releases the handle when the process dies, so a crashed process never
// leaves a stale lock behind. Returns a release func; a contended lock is
// reported as ErrAlreadyRunning (held=false, nil err).
//
// The host-wide node lock (LockPath) and the iOS-scoped lock both use this.
func LockFile(path string) (release func() error, held bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("create lock dir: %w", err)
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, err
	}
	handle, err := windows.CreateFile(
		p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, // no sharing: a second opener fails with ERROR_SHARING_VIOLATION
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if err == windows.ERROR_SHARING_VIOLATION || err == windows.ERROR_LOCK_VIOLATION || err == windows.ERROR_ACCESS_DENIED {
			return nil, false, nil // another process holds it
		}
		return nil, false, fmt.Errorf("open lock: %w", err)
	}
	return func() error { return windows.CloseHandle(handle) }, true, nil
}
