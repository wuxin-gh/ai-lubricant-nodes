package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarGz 造一个内存里的 tar.gz 供 extractTarGz 测试。
func tarGz(t *testing.T, files map[string]string) ioReaderSeeker {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gz close: %v", err)
	}
	return bytes.NewReader(buf.Bytes())
}

type ioReaderSeeker interface {
	Read(p []byte) (int, error)
}

func TestExtractTarGzWritesFiles(t *testing.T) {
	dest := t.TempDir()
	src := tarGz(t, map[string]string{
		"SKILL.md":            "# hello",
		"scripts/run.sh":      "#!/bin/sh",
		"refs/deep/nested.md": "n",
	})
	if err := extractTarGz(src, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, name := range []string{"SKILL.md", "scripts/run.sh", "refs/deep/nested.md"} {
		p := filepath.Join(dest, filepath.FromSlash(name))
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dest, "SKILL.md"))
	if string(got) != "# hello" {
		t.Fatalf("content mismatch: %q", got)
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	dest := t.TempDir()
	src := tarGz(t, map[string]string{"../../evil.sh": "bad"})
	err := extractTarGz(src, dest)
	if err == nil {
		t.Fatal("expected traversal rejection")
	}
	if !strings.Contains(err.Error(), "escapes dest") {
		t.Fatalf("unexpected error: %v", err)
	}
	// 越权文件必须没被写出来
	if _, err := os.Stat(filepath.Join(dest, "..", "..", "evil.sh")); !os.IsNotExist(err) {
		t.Fatal("evil.sh leaked out of dest")
	}
}

func TestExtractTarGzRejectsAbsolute(t *testing.T) {
	dest := t.TempDir()
	// 直接构造带绝对路径的 header。
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: "/abs/path.dll", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("x"))
	tw.Close()
	gw.Close()
	err := extractTarGz(&buf, dest)
	if err == nil {
		t.Fatal("expected absolute path rejection")
	}
}

func TestExtractTarGzRejectsSymlink(t *testing.T) {
	dest := t.TempDir()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{
		Name: "escape", Linkname: "/etc/passwd", Mode: 0o777, Typeflag: tar.TypeSymlink,
	})
	tw.Close()
	gw.Close()
	err := extractTarGz(&buf, dest)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

// archiveServer 提供一个假的服务端 fetch 端点：返回 tar.gz + X-Resource-Digest，
// 并记录收到的请求次数与 Authorization 头，用于验证缓存命中与 Bearer 鉴权。
type archiveServer struct {
	hits   int
	auth   string
	body   []byte
	digest string
}

func newArchiveServer(t *testing.T, files map[string]string) (*archiveServer, *httptest.Server) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	tw.Close()
	gw.Close()
	sum := sha256.Sum256(buf.Bytes())
	as := &archiveServer{body: buf.Bytes(), digest: hex.EncodeToString(sum[:])}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as.hits++
		as.auth = r.Header.Get("Authorization")
		w.Header().Set("X-Resource-Digest", as.digest)
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(as.body)
	}))
	t.Cleanup(srv.Close)
	return as, srv
}

// testSession 造一个只用于资源下载的最小会话（fetchResource 只用到 baseCtx）。
func testSession() *nodeSession {
	return &nodeSession{id: "sess-archive", baseCtx: context.Background()}
}

func TestFetchResourceRoutesArchiveAndCaches(t *testing.T) {
	m, workRoot := newTestManager(t)
	as, srv := newArchiveServer(t, map[string]string{"SKILL.md": "# mirrored"})
	url := srv.URL + "/api/v1/resources/skills/fetch/demo"
	src := resourceSource{url: url, source: "archive", token: "tok-123"}

	destA := filepath.Join(workRoot, "sessA", "skills", "demo")
	if err := m.fetchResource(testSession(), src, destA); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(destA, "SKILL.md"))
	if err != nil || string(got) != "# mirrored" {
		t.Fatalf("content mismatch: %q err=%v", got, err)
	}
	if as.hits != 1 {
		t.Fatalf("expected 1 download, got %d", as.hits)
	}
	if as.auth != "Bearer tok-123" {
		t.Fatalf("expected Bearer token, got %q", as.auth)
	}
	// 缓存元数据不得泄漏进会话目录。
	if _, err := os.Stat(filepath.Join(destA, ".ready")); !os.IsNotExist(err) {
		t.Fatal(".ready leaked into session skill dir")
	}

	// 第二个会话同 url：命中缓存，零网络。
	destB := filepath.Join(workRoot, "sessB", "skills", "demo")
	if err := m.fetchResource(testSession(), src, destB); err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if as.hits != 1 {
		t.Fatalf("expected cache hit (still 1 download), got %d", as.hits)
	}
	if got, _ := os.ReadFile(filepath.Join(destB, "SKILL.md")); string(got) != "# mirrored" {
		t.Fatalf("cached copy mismatch: %q", got)
	}
}

func TestFetchArchiveRejectsDigestMismatch(t *testing.T) {
	m, workRoot := newTestManager(t)
	_, srv := newArchiveServer(t, map[string]string{"SKILL.md": "x"})
	src := resourceSource{
		url:    srv.URL + "/api/v1/resources/skills/fetch/demo",
		source: "archive",
		digest: strings.Repeat("a", 64), // 与服务端实际 digest 不符
	}
	dest := filepath.Join(workRoot, "sess", "skills", "demo")
	err := m.fetchResource(testSession(), src, dest)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected digest mismatch, got %v", err)
	}
	// 失败不得留下可被后续误命中的缓存。
	entries, _ := os.ReadDir(m.resourceCacheRoot())
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(m.resourceCacheRoot(), e.Name(), ".ready")); err == nil {
			t.Fatal("failed fetch left a ready cache entry")
		}
	}
}

func TestFetchArchiveSurfacesHTTPError(t *testing.T) {
	m, workRoot := newTestManager(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	src := resourceSource{url: srv.URL + "/api/v1/resources/skills/fetch/demo", source: "archive"}
	err := m.fetchResource(testSession(), src, filepath.Join(workRoot, "sess", "skills", "demo"))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", http.StatusForbidden)) {
		t.Fatalf("expected HTTP 403 surfaced, got %v", err)
	}
}

func TestIsArchiveSourceClassification(t *testing.T) {
	cases := []struct {
		name string
		src  resourceSource
		want bool
	}{
		{"explicit archive", resourceSource{source: "archive"}, true},
		{"explicit github wins over url shape", resourceSource{
			source: "github", url: "https://x/api/v1/resources/skills/fetch/a",
		}, false},
		{"fetch endpoint fallback", resourceSource{
			url: "https://x/api/v1/resources/skills/fetch/a",
		}, true},
		{"plain git url", resourceSource{url: "https://github.com/o/r.git"}, false},
	}
	for _, tc := range cases {
		if got := tc.src.isArchiveSource(); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
