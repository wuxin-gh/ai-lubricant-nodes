package build

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// tarGzBytes builds a .tar.gz whose entries are the given name→content pairs
// (dirs are created implicitly by callers using trailing slashes in names is
// not needed — the extractor mkdirs parents).
func tarGzBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(name, "/") {
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// archiveReq is happyReq with the archive source kind selected.
func archiveReq(buildID, url string) *agentcomposev2.NodeBuildRequest {
	req := happyReq(buildID)
	req.SourceKind = "archive"
	req.SourceRef = "" // archives are not ref-pinned
	req.SourceUrl = url
	return req
}

func TestRunnerArchiveSourceHappyPath(t *testing.T) {
	body := tarGzBytes(t, map[string]string{
		"repo-abc123/App.xcodeproj/project.pbxproj": "// pbxproj",
		"repo-abc123/README.md":                     "hi",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	em := &captureEmitter{}
	r := NewRunner(em.emit, fakeRunnerLogger{}, nil)
	se := &scriptExec{script: map[string]scriptEntry{}}
	se.script["sh -c xcodebuild -project App.xcodeproj"] = scriptEntry{out: []byte("built\n")}
	r.execFn = func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
		// The extracted project root must be exactly <workDir>/src, so the
		// wrapper dir "repo-abc123" must have been stripped.
		if name == "sh" {
			if _, err := os.Stat(filepath.Join(dir, "App.xcodeproj", "project.pbxproj")); err != nil {
				t.Errorf("extracted tree missing project file: %v", err)
			}
			rel := filepath.FromSlash("build/out/unsigned.ipa")
			_ = os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o755)
			_ = os.WriteFile(filepath.Join(dir, rel), []byte("ipa"), 0o644)
		}
		return se.call(ctx, dir, name, args)
	}
	r.uploadFn = func(ctx context.Context, url, token, path, filename string) error { return nil }

	req := archiveReq("b-arch", srv.URL+"/archive.tar.gz")
	req.Steps = []string{"xcodebuild -project App.xcodeproj"}
	runAndWait(t, r, req, em)

	res := em.buildResult()
	if res == nil || !res.GetOk() {
		t.Fatalf("archive build failed: %v %s", res.GetErrorCode(), res.GetErrorMessage())
	}
	// No git clone should have run.
	for _, c := range se.calls {
		if strings.HasPrefix(c, "git ") {
			t.Errorf("archive kind ran git: %s", c)
		}
	}
}

func TestRunnerArchiveSourceZipAndSubdir(t *testing.T) {
	body := zipBytes(t, map[string]string{
		"monorepo-main/apps/ios/App.xcodeproj/project.pbxproj": "// pbxproj",
		"monorepo-main/apps/android/build.gradle":              "// gradle",
		"monorepo-main/README.md":                              "hi",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	em := &captureEmitter{}
	r := NewRunner(em.emit, fakeRunnerLogger{}, nil)
	se := &scriptExec{script: map[string]scriptEntry{}}
	se.script["sh -c build"] = scriptEntry{}
	r.execFn = func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
		if name == "sh" {
			if _, err := os.Stat(filepath.Join(dir, "App.xcodeproj", "project.pbxproj")); err != nil {
				t.Errorf("subdir selection wrong; project file not at root: %v", err)
			}
			rel := filepath.FromSlash("out.ipa")
			_ = os.WriteFile(filepath.Join(dir, rel), []byte("ipa"), 0o644)
		}
		return se.call(ctx, dir, name, args)
	}
	r.uploadFn = func(ctx context.Context, url, token, path, filename string) error { return nil }

	req := archiveReq("b-arch-sub", srv.URL+"/mono.zip")
	req.SourceSubdir = "apps/ios"
	req.Steps = []string{"build"}
	req.ArtifactGlob = "out.ipa"
	runAndWait(t, r, req, em)

	if res := em.buildResult(); res == nil || !res.GetOk() {
		t.Fatalf("zip+subdir build failed: %v %s", res.GetErrorCode(), res.GetErrorMessage())
	}
}

func TestRunnerArchiveSourceHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	em := &captureEmitter{}
	r := NewRunner(em.emit, fakeRunnerLogger{}, nil)
	r.execFn = func(ctx context.Context, dir, name string, args []string) ([]byte, error) {
		t.Fatalf("no command should run when the download fails (got %s)", name)
		return nil, nil
	}
	r.uploadFn = func(ctx context.Context, url, token, path, filename string) error { return nil }

	runAndWait(t, r, archiveReq("b-arch-404", srv.URL+"/x.tar.gz"), em)

	res := em.buildResult()
	if res.GetOk() {
		t.Fatal("expected failure")
	}
	if res.GetErrorCode() != "source_failed" {
		t.Errorf("errCode = %s, want source_failed", res.GetErrorCode())
	}
	if !res.GetRetryable() {
		t.Error("download failure should be retryable")
	}
}

func TestRunnerArchiveRejectsBadMagic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not an archive</html>"))
	}))
	defer srv.Close()

	em := &captureEmitter{}
	r := NewRunner(em.emit, fakeRunnerLogger{}, nil)
	r.execFn = func(ctx context.Context, dir, name string, args []string) ([]byte, error) { return nil, nil }
	r.uploadFn = func(ctx context.Context, url, token, path, filename string) error { return nil }

	runAndWait(t, r, archiveReq("b-arch-magic", srv.URL+"/x"), em)
	if res := em.buildResult(); res.GetOk() {
		t.Fatal("expected failure on non-archive body")
	}
}

func TestRunnerUnknownSourceKindRejected(t *testing.T) {
	r := NewRunner(func(*agentcomposev2.NodeUpstreamFrame) error { return nil }, fakeRunnerLogger{}, nil)
	req := happyReq("b-badkind")
	req.SourceKind = "ftp"
	if err := r.Start(context.Background(), req); err == nil {
		t.Fatal("unknown source_kind must be rejected")
	}
}

func TestExtractArchiveRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("bad"))
	_ = tw.Close()
	_ = gz.Close()
	_ = f.Close()

	if err := extractArchive(archive, filepath.Join(dir, "out")); err == nil {
		t.Fatal("tar-slip entry must be rejected")
	}
}

func TestSingleSubdir(t *testing.T) {
	dir := t.TempDir()
	// empty: no wrapper
	if _, ok := singleSubdir(dir); ok {
		t.Error("empty dir has no single subdir")
	}
	// one dir: wrapper
	if err := os.Mkdir(filepath.Join(dir, "repo-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, ok := singleSubdir(dir); !ok || filepath.Base(got) != "repo-x" {
		t.Errorf("single dir not detected: %q %v", got, ok)
	}
	// two entries: no wrapper
	if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := singleSubdir(dir); ok {
		t.Error("two entries must not be treated as a wrapper")
	}
}
