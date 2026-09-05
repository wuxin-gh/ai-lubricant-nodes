//go:build windows

package agent

import (
	"golang.org/x/sys/windows"
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

// stopProcess terminates the old node. Windows has no SIGTERM for another
// process, so we open just the rights needed and terminate it.
func stopProcess(pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := windows.TerminateProcess(handle, 0); err != nil {
		return err
	}
	_, _ = windows.WaitForSingleObject(handle, 10_000)
	return nil
}
