//go:build darwin

package agent

import "syscall"

// freeDiskBytes reports the bytes available to unprivileged users on the
// volume holding path (Statfs Bavail honors the filesystem reserve). Used by
// the Xcode install job's disk pre-check; darwin-only because Xcode installs
// are (the !darwin stub below never runs — the job rejects non-macOS hosts
// before the check).
func freeDiskBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
