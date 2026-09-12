//go:build !darwin

package agent

import "errors"

// freeDiskBytes has no implementation off darwin: the Xcode install job
// rejects non-macOS hosts before the disk pre-check would run, so this stub
// only needs to compile.
func freeDiskBytes(path string) (int64, error) {
	return 0, errors.New("free-disk probe is darwin-only")
}
