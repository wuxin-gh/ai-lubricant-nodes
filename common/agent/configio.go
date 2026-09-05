package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ResolveStateDir returns the per-user agent-compose state directory, optionally
// scoped under a role/capability subdir. It honors envOverride when set (tests,
// packaging), then os.UserConfigDir() with a ~/.config fallback for a bare
// service account, then joins agent-compose/<subdir>.
//
// This is the single resolution algorithm shared by the node config
// (LocalConfig → subdir "node") and the iOS devices config (subdir "ios").
// Keeping it in one place prevents the two from drifting on the
// UserConfigDir-fallback or the agent-compose parent.
func ResolveStateDir(envOverride, subdir string) (string, error) {
	if override := strings.TrimSpace(os.Getenv(envOverride)); override != "" {
		return override, nil
	}
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		home, herr := os.UserHomeDir()
		if herr != nil || strings.TrimSpace(home) == "" {
			return "", fmt.Errorf("resolve state dir: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	parts := []string{base, "agent-compose"}
	if subdir = strings.TrimSpace(subdir); subdir != "" {
		parts = append(parts, subdir)
	}
	return filepath.Join(parts...), nil
}

// AtomicWriteJSON writes body to path atomically (temp file + cross-platform
// replace) with owner-only permissions. It is the shared write path for the
// node config and the iOS devices config: the temp file is created 0600, then
// committed via commitConfig — which on Windows uses MoveFileEx(REPLACE_EXISTING)
// because os.Rename cannot replace an existing file there — and finally has its
// ACL restricted via restrictConfigACL so the file is owner-only on Windows too
// (where 0600 mode bits are ignored).
//
// The directory is created 0700 if missing. A write failure never leaves a
// partial file at path: the temp file is removed if the rename fails.
func AtomicWriteJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := commitConfig(tmpName, path); err != nil {
		return fmt.Errorf("commit config: %w", err)
	}
	restrictConfigACL(path) // best-effort Windows ACL tightening
	return nil
}
