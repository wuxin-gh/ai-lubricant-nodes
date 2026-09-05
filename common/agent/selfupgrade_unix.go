//go:build !windows

package agent

import (
	"fmt"
	"os"
	"syscall"
)

// swapBinary replaces the current executable with the freshly downloaded one.
// On Unix a running binary's file can be replaced in place, so a same-directory
// rename is atomic and immediate.
func swapBinary(exe, newPath string) error {
	if err := os.Rename(newPath, exe); err != nil {
		return fmt.Errorf("self-upgrade: replace binary: %w", err)
	}
	return nil
}

// restartSelf replaces the current process image with the new binary using the
// original argv/env. syscall.Exec does not return on success.
func restartSelf(exe string) error {
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("self-upgrade: exec new binary: %w", err)
	}
	return nil
}
