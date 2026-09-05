//go:build windows

package agent

import (
	"os/exec"
	"strings"

	"golang.org/x/sys/windows"
)

// commitConfig replaces an existing config on Windows. os.Rename cannot replace
// an existing destination there, so use MoveFileEx with REPLACE_EXISTING while
// keeping the same-directory temp-file atomicity.
func commitConfig(tmp, final string) error {
	from, err := windows.UTF16PtrFromString(tmp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(final)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// restrictConfigACL tightens the config file's ACL to the current user only.
// Windows ignores the 0600 mode bits Go passes to CreateFile, so the file would
// otherwise inherit the directory's ACL. Best-effort: the config lives under the
// per-user profile already, so a failure here is not fatal.
func restrictConfigACL(path string) {
	user := strings.TrimSpace(EnvOr("USERNAME", ""))
	if user == "" {
		return
	}
	// /inheritance:r drops inherited ACEs, then grant only this user full control.
	cmd := exec.Command("icacls.exe", path, "/inheritance:r", "/grant:r", user+":F")
	_ = cmd.Run()
}
