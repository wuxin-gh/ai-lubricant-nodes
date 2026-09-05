package agent

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// normPath resolves a path through EvalSymlinks so the two sides of a comparison
// agree on Windows, where os.Executable() may hand back the short (8.3) form
// ("ADMINI~1") while t.TempDir() returns the long one.
func normPath(t *testing.T, path string) string {
	t.Helper()
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// Logs must land beside the node executable — the install directory the operator
// is already in when they go looking for them.
func TestLogDirPrefersInstallDir(t *testing.T) {
	t.Setenv(logDirEnvOverride, "")
	exe, err := os.Executable()
	if err != nil {
		t.Skip("os.Executable unavailable on this platform")
	}
	got, err := LogDir()
	if err != nil {
		t.Fatalf("LogDir: %v", err)
	}
	want := filepath.Join(filepath.Dir(exe), LogDirName)
	if normPath(t, got) != normPath(t, want) {
		t.Errorf("LogDir() = %q, want install-dir path %q", got, want)
	}
}

// The env override wins over both the install dir and the state dir, so
// packaging / containers can relocate logs without touching code.
func TestLogDirEnvOverrideWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(logDirEnvOverride, dir)
	got, err := LogDir()
	if err != nil {
		t.Fatalf("LogDir: %v", err)
	}
	if got != dir {
		t.Errorf("LogDir() = %q, want override %q", got, dir)
	}
}

// A write must produce a dated file named after the running binary, so the
// execution and management roles never interleave into one file.
func TestDailyLogWriterWritesDatedFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(logDirEnvOverride, dir)

	w, path, err := newDailyLogWriter()
	if err != nil {
		t.Fatalf("newDailyLogWriter: %v", err)
	}
	// Release the handle before TempDir cleanup: on Windows an open file cannot
	// be unlinked, which would fail the test in cleanup rather than in the body.
	if closer, ok := w.(io.Closer); ok {
		t.Cleanup(func() { _ = closer.Close() })
	} else {
		t.Fatal("dailyLogWriter must implement io.Closer so callers can release the file")
	}

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	today := time.Now().Format("2006-01-02")
	if !strings.HasSuffix(path, today+".log") {
		t.Errorf("log path %q does not carry today's date %q", path, today)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("log path %q is not inside %q", path, dir)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(body), "hello") {
		t.Errorf("log file does not contain the written line: %q", body)
	}
}

// Pruning removes only this prefix's files past the window, and never touches
// another binary's logs or unrelated files in the same directory.
func TestPruneOldLogsRespectsWindowAndOwnership(t *testing.T) {
	dir := t.TempDir()
	stale := time.Now().AddDate(0, 0, -(LogRetentionDays + 3)).Format("2006-01-02")
	fresh := time.Now().Format("2006-01-02")

	files := map[string]bool{ // name -> should survive
		"node-execution-" + stale + ".log":  false,
		"node-execution-" + fresh + ".log":  true,
		"agent-compose-node-management-" + stale + ".log": true, // other binary
		"notes.txt": true, // not ours
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	pruneOldLogs(dir, "node-execution", LogRetentionDays)

	for name, shouldSurvive := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		survived := err == nil
		if survived != shouldSurvive {
			t.Errorf("%s: survived=%v, want %v", name, survived, shouldSurvive)
		}
	}
}
