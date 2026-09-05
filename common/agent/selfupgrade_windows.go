//go:build windows

package agent

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// swapBinary replaces the current executable on Windows. A running .exe cannot
// be overwritten or renamed-onto, but the running image CAN be renamed away.
// So we move the current exe aside to <exe>.old, then move the new binary into
// the original path. The .old file is deleted best-effort on next start (it is
// still locked while this process runs).
func swapBinary(exe, newPath string) error {
	old := exe + ".old"
	_ = os.Remove(old) // clear a stale one from a previous upgrade
	if err := os.Rename(exe, old); err != nil {
		return fmt.Errorf("self-upgrade: move current binary aside: %w", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		// Best-effort rollback so the node is not left with no binary in place.
		_ = os.Rename(old, exe)
		return fmt.Errorf("self-upgrade: move new binary into place: %w", err)
	}
	return nil
}

// restartSelf starts the new binary as a detached process and returns nil so the
// caller can exit. Windows has no exec-replace; the fresh process re-dials the
// server and the old one exits, which the server sees as a normal reconnect.
func restartSelf(exe string) error {
	argv := append([]string{exe}, os.Args[1:]...)
	attr := &os.ProcAttr{
		Env:   os.Environ(),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Sys:   &windows.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP},
	}
	proc, err := os.StartProcess(exe, argv, attr)
	if err != nil {
		return fmt.Errorf("self-upgrade: start new process: %w", err)
	}
	_ = proc.Release()
	// Signal the caller to exit the current process so only the new one remains.
	return errRestartExit
}
