package workspaces

import (
	"os"
	"path/filepath"
	"testing"
)

// HostWorkspaceInitialized must treat a missing dir as "not initialized" (no
// error): a fresh session has no workDir yet, and provisionGit relies on this so
// the first dispatch of a new session isn't rejected with a "read workspace root"
// error before git clone ever runs.
func TestHostWorkspaceInitializedMissingDir(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	got, err := HostWorkspaceInitialized(missing)
	if err != nil {
		t.Fatalf("missing dir must not be an error; got %v", err)
	}
	if got {
		t.Fatalf("missing dir must report not-initialized (false); got true")
	}
}

// A dir holding only the node's own scratch dirs (.agent-compose / the clone
// temp dir) is NOT a real checkout — a reconnect must re-clone, not skip. This is
// the leftover from a failed prior provision: git clone then refuses the non-empty
// target unless ResetStaleWorkspace clears it first.
func TestHostWorkspaceInitializedScratchOnly(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".agent-compose", GitWorkspaceTempDirName} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	got, err := HostWorkspaceInitialized(dir)
	if err != nil {
		t.Fatalf("scratch-only dir must not error; got %v", err)
	}
	if got {
		t.Fatalf("scratch-only dir must report not-initialized (false); got true")
	}
}

// A dir with any entry beyond the node's scratch dirs is a real checkout; a
// reconnect must skip re-cloning (idempotent recreate).
func TestHostWorkspaceInitializedRealCheckout(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	got, err := HostWorkspaceInitialized(dir)
	if err != nil {
		t.Fatalf("real checkout must not error; got %v", err)
	}
	if !got {
		t.Fatalf("real checkout must report initialized (true); got false")
	}
}

// ResetStaleWorkspace must clear a scratch-only dir so git clone finds a clean
// target, and be a no-op on a missing dir (fresh session path).
func TestResetStaleWorkspaceClearsScratchAndIgnoresMissing(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".agent-compose", GitWorkspaceTempDirName, ".git"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	if err := ResetStaleWorkspace(dir); err != nil {
		t.Fatalf("reset scratch-only dir: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatalf("scratch-only dir must be removed so clone recreates it")
	}
	// Missing dir: must not error (fresh session has no workDir yet).
	missing := filepath.Join(t.TempDir(), "never-existed")
	if err := ResetStaleWorkspace(missing); err != nil {
		t.Fatalf("reset missing dir must be a no-op; got %v", err)
	}
}
