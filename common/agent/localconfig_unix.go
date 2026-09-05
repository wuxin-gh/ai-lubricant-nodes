//go:build !windows

package agent

import "os"

// commitConfig atomically replaces the current config on Unix.
func commitConfig(tmp, final string) error { return os.Rename(tmp, final) }

// restrictConfigACL is a no-op on Unix: the 0600 mode set at write time already
// limits the config to its owner.
func restrictConfigACL(string) {}
