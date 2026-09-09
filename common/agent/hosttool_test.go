package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
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
