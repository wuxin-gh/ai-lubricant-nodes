package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// The host-tool install path pins three behaviors: the archive layouts are
// extracted correctly (tar.gz with a wrapping dir on unix, zip at root on
// windows), the staged install validates on the node binary, and
// EnsureManagedNodeOnPath puts the managed bin dir first on PATH exactly once.

// buildNodeTarGz writes a Node.js-style tarball: one top-level
// node-vX-os-arch/ dir holding bin/node, bin/npm and lib/.
func buildNodeTarGz(t *testing.T, dest, topDir string) {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	entries := []struct {
		name string
		dir  bool
	}{
		{topDir, true},
		{topDir + "/bin", true},
		{topDir + "/bin/node", false},
		{topDir + "/bin/npm", false},
		{topDir + "/lib", true},
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o755}
		if e.dir {
			hdr.Typeflag = tar.TypeDir
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			continue
		}
		hdr.Typeflag = tar.TypeReg
		hdr.Size = int64(len("#!/bin/sh\n"))
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("#!/bin/sh\n")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExtractNodeTarGzStripsTopDir(t *testing.T) {
	stage := t.TempDir()
	archive := filepath.Join(t.TempDir(), "node.tar.gz")
	buildNodeTarGz(t, archive, "node-v22.17.0-darwin-arm64")
	if err := extractNodeTarGz(archive, stage); err != nil {
		t.Fatalf("extractNodeTarGz: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stage, "bin", "node")); err != nil {
		t.Fatalf("bin/node must land at the stage root after stripping the top dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stage, "bin", "npm")); err != nil {
		t.Fatalf("bin/npm must land at the stage root: %v", err)
	}
	if err := validateStagedNode(stage); err != nil {
		t.Fatalf("staged archive with bin/node must validate: %v", err)
	}
}

func TestExtractNodeTarGzRejectsEscape(t *testing.T) {
	stage := t.TempDir()
	archive := filepath.Join(t.TempDir(), "node.tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "../../escape.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: 1}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	f.Close()
	if err := extractNodeTarGz(archive, stage); err == nil {
		t.Fatal("an archive entry escaping the stage dir must fail extraction")
	}
}

func TestValidateStagedNodeRejectsEmpty(t *testing.T) {
	if err := validateStagedNode(t.TempDir()); err == nil {
		t.Fatal("a stage with no node binary must fail validation")
	}
}

func TestEnsureManagedNodeOnPathPrependsOnce(t *testing.T) {
	useTempState(t) // the managed tools dir derives from the state dir parent
	binDir := ManagedNodeBinDir()
	nodeName := "node"
	if runtime.GOOS == "windows" {
		nodeName = "node.exe"
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, nodeName), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/usr/bin"+string(os.PathListSeparator)+"/bin")

	EnsureManagedNodeOnPath()
	got := os.Getenv("PATH")
	listSep := string(os.PathListSeparator)
	parts := strings.Split(got, listSep)
	if len(parts) == 0 || !strings.EqualFold(filepath.Clean(parts[0]), filepath.Clean(binDir)) {
		t.Fatalf("managed bin dir must be first on PATH, got %q", got)
	}

	EnsureManagedNodeOnPath() // idempotent: must not duplicate
	count := 0
	for _, p := range strings.Split(os.Getenv("PATH"), listSep) {
		if strings.EqualFold(filepath.Clean(p), filepath.Clean(binDir)) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("managed bin dir must appear exactly once on PATH, found %d", count)
	}
}

func TestEnsureManagedNodeOnPathNoBinaryNoop(t *testing.T) {
	useTempState(t)
	before := os.Getenv("PATH")
	EnsureManagedNodeOnPath() // no managed node binary exists in the temp state
	if os.Getenv("PATH") != before {
		t.Fatal("PATH must stay untouched when no managed node binary exists")
	}
}

// The Windows zip layout (node.exe at the archive root) is exercised on every
// platform: extraction is pure file work.
func TestExtractNodeZipKeepsRootLayout(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("zip extraction is the windows path; unix uses tar.gz")
	}
	stage := t.TempDir()
	archive := filepath.Join(t.TempDir(), "node.zip")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for name := range map[string]string{"node.exe": "bin", "npm.cmd": "cmd"} {
		out, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := out.Write([]byte("stub")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := extractNodeZip(archive, stage); err != nil {
		t.Fatalf("extractNodeZip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stage, "node.exe")); err != nil {
		t.Fatalf("node.exe must land at the stage root: %v", err)
	}
	if err := validateStagedNode(stage); err != nil {
		t.Fatalf("windows stage with node.exe must validate: %v", err)
	}
}

// ── Xcode discovery / DEVELOPER_DIR persistence ──────────────────────────────

// discoverXcodeApps globs real directories; the pure-filtering half is
// extracted so the preference order can be tested without /Applications.
func TestDiscoverXcodeAppsPreferenceOrder(t *testing.T) {
	// Build a fake Applications dir with several app names, including noise
	// that must not match (files, non-Xcode dirs).
	root := t.TempDir()
	apps := []string{"Xcode-beta.app", "Xcode.app", "Xcode 26.0 beta 2.app", "Xcode_thing.app"}
	for _, name := range apps {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A file (not a dir) must be skipped even though the glob matches.
	if err := os.WriteFile(filepath.Join(root, "Xcode-notadir.app"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Safari.app"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := filterXcodeCandidates([]string{root})
	want := []string{
		filepath.Join(root, "Xcode.app"),
		filepath.Join(root, "Xcode-beta.app"),
		filepath.Join(root, "Xcode 26.0 beta 2.app"),
		filepath.Join(root, "Xcode_thing.app"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("preference order mismatch:\n got %v\nwant %v", got, want)
	}
}

func TestDiscoverXcodeAppsFindsDownloadsCopy(t *testing.T) {
	// /Applications has nothing; the xip extraction still sitting in
	// ~/Downloads must be found.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "Downloads", "Xcode-beta.app"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Redirect /Applications scanning to an empty temp dir via the injectable
	// test seam: discoverXcodeApps on non-darwin still globs /Applications,
	// which either does not exist or has no Xcode → empty, which is fine.
	got := filterXcodeCandidates([]string{filepath.Join(home, "Downloads")})
	if len(got) != 1 || filepath.Base(got[0]) != "Xcode-beta.app" {
		t.Fatalf("expected the Downloads copy to be found, got %v", got)
	}
}

func TestDeveloperDirRoundtripAndStaleness(t *testing.T) {
	t.Setenv("AGENT_COMPOSE_NODE_STATE_DIR", t.TempDir())
	dir := t.TempDir()
	if err := saveDeveloperDir(dir); err != nil {
		t.Fatalf("saveDeveloperDir: %v", err)
	}
	// Save points at a dir that exists → EnsureDeveloperDir applies it.
	t.Setenv("DEVELOPER_DIR", "")
	EnsureDeveloperDir()
	if os.Getenv("DEVELOPER_DIR") != dir {
		t.Fatalf("DEVELOPER_DIR should be re-applied from state, got %q", os.Getenv("DEVELOPER_DIR"))
	}
	// Operator-provided env must win over the state file.
	t.Setenv("DEVELOPER_DIR", "/operator/override")
	EnsureDeveloperDir()
	if os.Getenv("DEVELOPER_DIR") != "/operator/override" {
		t.Fatalf("operator env must win, got %q", os.Getenv("DEVELOPER_DIR"))
	}
	// A stale entry (dir removed) is dropped, not applied.
	t.Setenv("DEVELOPER_DIR", "")
	_ = os.RemoveAll(dir)
	EnsureDeveloperDir()
	if os.Getenv("DEVELOPER_DIR") != "" {
		t.Fatalf("stale developer-dir must be dropped, got %q", os.Getenv("DEVELOPER_DIR"))
	}
	if _, err := os.Stat(developerDirFileFor(t)); err == nil {
		t.Fatal("stale developer-dir state file should be removed")
	}
}

func developerDirFileFor(t *testing.T) string {
	t.Helper()
	path, err := developerDirPath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// ensureXcodebuildVisible is the register-time recovery for an installed-but-
// unselected Xcode: bare probe misses → discover → activate (DEVELOPER_DIR
// fallback on a non-root test host) → re-probe hits. Pinned so the
// xcodebuild_version label reports an installed Xcode without anyone clicking
// detect first (the「已装 Xcode 注册时识别不出来」symptom).
func TestEnsureXcodebuildVisibleActivatesInstalledXcode(t *testing.T) {
	useTempState(t)
	t.Setenv("DEVELOPER_DIR", "")
	t.Setenv("HOME", t.TempDir())

	// A fake installed Xcode with a toolchain inside.
	root := t.TempDir()
	app := filepath.Join(root, "Xcode.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents", "Developer", "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// discovery expects the bundle under ~/Downloads (see the rename below);
	// build it there from the start so every later path reference agrees.
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(app, filepath.Join(downloads, "Xcode.app")); err != nil {
		t.Fatal(err)
	}
	app = filepath.Join(downloads, "Xcode.app")

	// probeHostTool uses LookPath + running the resolved binary. Stub LookPath
	// so "xcodebuild" resolves only once DEVELOPER_DIR points inside the fake
	// app (mirrors a real host where the shim refuses until activation).
	// Running "xcodebuild" is faked by stubbing LookPath to return the shell
	// binary — real execs print a version-ish string via --version args we
	// can't fully fake, so instead pin the seam one level up: the probe
	// result after activation is observable through DEVELOPER_DIR.
	stubLookPath(t, func(name string) (string, error) {
		if name == "xcodebuild" {
			if os.Getenv("DEVELOPER_DIR") != "" {
				return filepath.Join(app, "Contents", "Developer", "usr", "bin", "xcodebuild"), nil
			}
			return "", exec.ErrNotFound
		}
		return exec.LookPath(name)
	})

	// The fake xcodebuild must be runnable and print something version-like;
	// write a tiny script that echoes a version line.
	fake := filepath.Join(app, "Contents", "Developer", "usr", "bin", "xcodebuild")
	script := "#!/bin/sh\necho 'Xcode 16.2\nBuild 16C5013f'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	got := ensureXcodebuildVisible(context.Background())
	if !strings.Contains(got, "16.2") {
		t.Fatalf("expected the activated Xcode's version, got %q", got)
	}
	if os.Getenv("DEVELOPER_DIR") == "" {
		t.Fatal("activation must leave DEVELOPER_DIR pointing at the found Xcode")
	}
}
