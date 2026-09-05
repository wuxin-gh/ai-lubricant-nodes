package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func prepareGitWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "tracked.txt")
	runGit(t, root, "commit", "-m", "initial")
	return root
}

func TestFileServiceGitChangesPreservesHistoricalContract(t *testing.T) {
	root := prepareGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := &fileService{root: root}
	req := httptest.NewRequest(http.MethodGet, gitChangesPath, nil)
	rec := httptest.NewRecorder()
	fs.handle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got repoFileChangesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Success || got.CommitHash == "" || got.Branch == "" {
		t.Fatalf("unexpected metadata: %+v", got)
	}
	byPath := make(map[string]repoFileChange)
	for _, change := range got.Changes {
		byPath[change.Path] = change
	}
	tracked, ok := byPath["tracked.txt"]
	if !ok || tracked.Status != "M" || tracked.Additions != 1 {
		t.Fatalf("tracked change mismatch: %+v", tracked)
	}
	untracked, ok := byPath["untracked.txt"]
	if !ok || untracked.Status != "??" {
		t.Fatalf("untracked change mismatch: %+v", untracked)
	}
}

func TestFileServiceGitDiffAndTraversalGuard(t *testing.T) {
	root := prepareGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := &fileService{root: root}
	req := httptest.NewRequest(http.MethodGet, gitDiffPath+"?path="+url.QueryEscape("tracked.txt"), nil)
	rec := httptest.NewRecorder()
	fs.handle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got repoFileDiffResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Success || got.Path != "tracked.txt" || !strings.Contains(got.Diff, "+two") {
		t.Fatalf("diff mismatch: %+v", got)
	}

	bad := httptest.NewRequest(http.MethodGet, gitDiffPath+"?path="+url.QueryEscape("../outside.txt"), nil)
	badRec := httptest.NewRecorder()
	fs.handle(badRec, bad)
	if badRec.Code != http.StatusForbidden {
		t.Fatalf("traversal status=%d body=%s", badRec.Code, badRec.Body.String())
	}
}
