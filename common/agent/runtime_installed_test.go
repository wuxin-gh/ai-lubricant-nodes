package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// RuntimeInstalled is the spawn-free pre-flight selectExecutor uses to reject a
// create ack when the node has no usable agent runtime — instead of spawning a
// process that immediately exits 1 (the "session finished exit_code=1" symptom).
// These pin its three states: no runtime at all, a managed runtime present, and
// a managed runtime whose Node.js launcher is missing.

// stubLookPath swaps the package LookPath var for one test and restores it.
func stubLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	prev := LookPath
	LookPath = fn
	t.Cleanup(func() { LookPath = prev })
}

func TestRuntimeInstalledMissingEverywhere(t *testing.T) {
	useTempState(t) // empty runtime dir
	stubLookPath(t, func(string) (string, error) {
		return "", exec.ErrNotFound // neither node nor legacy binary on PATH
	})
	if err := RuntimeInstalled(); err == nil {
		t.Fatal("expected error when no managed runtime and no legacy binary exist")
	}
}

func TestRuntimeInstalledLegacyOnPath(t *testing.T) {
	useTempState(t)
	stubLookPath(t, func(name string) (string, error) {
		if name == "agent-compose-runtime" {
			return "/usr/local/bin/agent-compose-runtime", nil
		}
		return "", exec.ErrNotFound
	})
	if err := RuntimeInstalled(); err != nil {
		t.Fatalf("legacy runtime on PATH should satisfy the pre-flight: %v", err)
	}
}

func TestRuntimeInstalledManagedRuntimeWithNode(t *testing.T) {
	dir := useTempState(t)
	cli := filepath.Join(dir, "runtime", "dist", "cli.js")
	if err := os.MkdirAll(filepath.Dir(cli), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli, []byte("// runtime"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubLookPath(t, func(name string) (string, error) {
		// node present; legacy binary absent — the managed path must win.
		if name == nodeBinary() {
			return "/usr/bin/" + nodeBinary(), nil
		}
		return "", exec.ErrNotFound
	})
	if err := RuntimeInstalled(); err != nil {
		t.Fatalf("managed runtime + node should satisfy the pre-flight: %v", err)
	}
}

func TestRuntimeInstalledManagedRuntimeMissingNode(t *testing.T) {
	dir := useTempState(t)
	cli := filepath.Join(dir, "runtime", "dist", "cli.js")
	if err := os.MkdirAll(filepath.Dir(cli), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli, []byte("// runtime"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubLookPath(t, func(string) (string, error) {
		return "", exec.ErrNotFound // node missing, no legacy fallback
	})
	if err := RuntimeInstalled(); err == nil {
		t.Fatal("a managed runtime with no Node.js must fail the pre-flight")
	}
}
