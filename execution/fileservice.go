package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reserved file-service routes. These preserve the historical Task control
// contract while using the canonical Task -> node-session tunnel.
const (
	archivePath    = "/__archive__"
	gitChangesPath = "/__git_changes__"
	gitDiffPath    = "/__git_diff__"
)

// fileService is a per-session HTTP endpoint that browses, downloads, and
// uploads files under the session's working directory. It is the node-local
// "files" service the server reaches through the reverse-proxy tunnel; the
// server never touches the node's disk directly.
//
// All paths are confined to the session work dir: a request path is cleaned and
// rejected if it escapes the root. The service binds to loopback on an ephemeral
// port — it is only ever reached via the tunnel, never exposed directly.
type fileService struct {
	root   string
	server *http.Server
	addr   string
}

// startFileService binds a loopback file server rooted at workDir and returns
// it. The caller records addr as the session's "files" service endpoint and
// closes the service when the session ends.
func startFileService(workDir string) (*fileService, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind file service: %w", err)
	}
	fs := &fileService{
		root: workDir,
		addr: "http://" + listener.Addr().String(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", fs.handle)
	fs.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = fs.server.Serve(listener) }()
	return fs, nil
}

// endpoint is the loopback base URL the tunnel handler forwards "files"
// requests to.
func (fs *fileService) endpoint() string {
	if fs == nil {
		return ""
	}
	return fs.addr
}

// stop shuts the file server down. Safe to call once when the session ends.
func (fs *fileService) stop() {
	if fs == nil || fs.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = fs.server.Shutdown(ctx)
}

// resolve maps a request path to an absolute path under root, rejecting any
// path that escapes the root (path traversal guard).
func (fs *fileService) resolve(reqPath string) (string, error) {
	// Reject traversal syntax before cleaning. filepath.Clean("/../x") becomes
	// "/x", which would otherwise hide the escape attempt and resolve it inside
	// the workspace instead of rejecting the caller-controlled path.
	normalized := strings.ReplaceAll(reqPath, "\\", "/")
	for _, segment := range strings.Split(normalized, "/") {
		if segment == ".." {
			return "", fmt.Errorf("path %q escapes the session workspace", reqPath)
		}
	}
	clean := filepath.Clean("/" + strings.TrimPrefix(normalized, "/"))
	abs := filepath.Join(fs.root, filepath.FromSlash(clean))
	rel, err := filepath.Rel(fs.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the session workspace", reqPath)
	}
	return abs, nil
}

func (fs *fileService) handle(w http.ResponseWriter, r *http.Request) {
	// Reserved workspace operations. They never accept a caller-controlled cwd;
	// every git command is pinned to fs.root and file paths pass resolve().
	switch r.URL.Path {
	case archivePath:
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fs.handleArchive(w, r)
		return
	case gitChangesPath:
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fs.handleGitChanges(w, r)
		return
	case gitDiffPath:
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		fs.handleGitDiff(w, r)
		return
	}
	abs, err := fs.resolve(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		fs.handleGet(w, abs)
	case http.MethodPut, http.MethodPost:
		fs.handlePut(w, r, abs)
	case http.MethodDelete:
		fs.handleDelete(w, abs)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleGet serves a file's bytes, or a JSON directory listing when the target
// is a directory.
func (fs *fileService) handleGet(w http.ResponseWriter, abs string) {
	info, err := os.Stat(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if info.IsDir() {
		fs.listDir(w, abs)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, &http.Request{Header: http.Header{}}, filepath.Base(abs), info.ModTime(), f)
}

type fileEntry struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

func (fs *fileService) listDir(w http.ResponseWriter, abs string) {
	entries, err := os.ReadDir(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, fileEntry{Name: e.Name(), Size: info.Size(), IsDir: e.IsDir()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handlePut writes the request body to the target path, creating parent dirs.
func (fs *fileService) handlePut(w http.ResponseWriter, r *http.Request, abs string) {
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Create(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (fs *fileService) handleDelete(w http.ResponseWriter, abs string) {
	if abs == fs.root {
		http.Error(w, "refusing to delete the workspace root", http.StatusForbidden)
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type repoFileChange struct {
	Path      string `json:"path"`
	Status    string `json:"status,omitempty"`
	Additions int    `json:"additions,omitempty"`
	Deletions int    `json:"deletions,omitempty"`
	OldPath   string `json:"old_path,omitempty"`
}

type repoFileChangesResponse struct {
	Changes    []repoFileChange `json:"changes"`
	Branch     string           `json:"branch,omitempty"`
	CommitHash string           `json:"commit_hash,omitempty"`
	Success    bool             `json:"success"`
	Error      string           `json:"error,omitempty"`
}

type repoFileDiffResponse struct {
	Path    string `json:"path,omitempty"`
	Diff    string `json:"diff,omitempty"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

func (fs *fileService) git(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = fs.root
	return cmd.CombinedOutput()
}

func (fs *fileService) handleGitChanges(w http.ResponseWriter, _ *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	statusOut, err := fs.git(ctx, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		writeJSON(w, http.StatusConflict, repoFileChangesResponse{Success: false, Error: strings.TrimSpace(string(statusOut))})
		return
	}
	stats := fs.gitNumstat(ctx)
	changes := make([]repoFileChange, 0)
	for _, raw := range strings.Split(strings.TrimRight(string(statusOut), "\r\n"), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if len(line) < 3 {
			continue
		}
		status := strings.TrimSpace(line[:2])
		if status == "" {
			continue
		}
		pathPart := strings.TrimSpace(line[3:])
		path := pathPart
		oldPath := ""
		if arrow := strings.Index(pathPart, " -> "); arrow >= 0 {
			oldPath = strings.Trim(pathPart[:arrow], `"`)
			path = strings.Trim(pathPart[arrow+4:], `"`)
		}
		path = strings.Trim(path, `"`)
		additions, deletions := 0, 0
		if pair, ok := stats[path]; ok {
			additions, deletions = pair[0], pair[1]
		}
		changes = append(changes, repoFileChange{
			Path: filepath.ToSlash(path), Status: status,
			Additions: additions, Deletions: deletions,
			OldPath: filepath.ToSlash(oldPath),
		})
	}
	branch := strings.TrimSpace(string(mustGit(fs, ctx, "rev-parse", "--abbrev-ref", "HEAD")))
	commit := strings.TrimSpace(string(mustGit(fs, ctx, "rev-parse", "--verify", "HEAD")))
	writeJSON(w, http.StatusOK, repoFileChangesResponse{
		Changes: changes, Branch: branch, CommitHash: commit, Success: true,
	})
}

func (fs *fileService) gitNumstat(ctx context.Context) map[string][2]int {
	out, err := fs.git(ctx, "diff", "--numstat", "HEAD", "--")
	if err != nil {
		return map[string][2]int{}
	}
	stats := make(map[string][2]int)
	for _, raw := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(strings.TrimSuffix(raw, "\r"), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		additions, _ := strconv.Atoi(parts[0]) // binary '-' intentionally becomes 0
		deletions, _ := strconv.Atoi(parts[1])
		stats[filepath.ToSlash(parts[2])] = [2]int{additions, deletions}
	}
	return stats
}

func mustGit(fs *fileService, ctx context.Context, args ...string) []byte {
	out, _ := fs.git(ctx, args...)
	return out
}

func (fs *fileService) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	reqPath := strings.TrimSpace(r.URL.Query().Get("path"))
	if reqPath == "" {
		writeJSON(w, http.StatusBadRequest, repoFileDiffResponse{Success: false, Error: "path is required"})
		return
	}
	abs, err := fs.resolve(reqPath)
	if err != nil {
		writeJSON(w, http.StatusForbidden, repoFileDiffResponse{Success: false, Error: err.Error()})
		return
	}
	rel, err := filepath.Rel(fs.root, abs)
	if err != nil {
		writeJSON(w, http.StatusForbidden, repoFileDiffResponse{Success: false, Error: err.Error()})
		return
	}
	rel = filepath.ToSlash(rel)
	contextLines := 20
	if raw := strings.TrimSpace(r.URL.Query().Get("context_lines")); raw != "" {
		if parsed, parseErr := strconv.Atoi(raw); parseErr == nil {
			if parsed < 0 {
				parsed = 0
			}
			if parsed > 100 {
				parsed = 100
			}
			contextLines = parsed
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := fs.git(ctx, "diff", "--no-ext-diff", fmt.Sprintf("--unified=%d", contextLines), "HEAD", "--", rel)
	if err != nil {
		writeJSON(w, http.StatusConflict, repoFileDiffResponse{Path: rel, Success: false, Error: strings.TrimSpace(string(out))})
		return
	}
	if len(out) == 0 {
		// git diff omits untracked files. Preserve the historical viewer contract
		// by generating a normal no-index unified diff for an untracked path.
		if _, statErr := os.Stat(abs); statErr == nil {
			if tracked, _ := fs.git(ctx, "ls-files", "--error-unmatch", "--", rel); len(tracked) == 0 {
				out, _ = fs.git(ctx, "diff", "--no-index", "--no-ext-diff", fmt.Sprintf("--unified=%d", contextLines), "--", os.DevNull, rel)
			}
		}
	}
	writeJSON(w, http.StatusOK, repoFileDiffResponse{Path: rel, Diff: string(out), Success: true})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// handleArchive streams the workspace (or the subdir named by ?path=) as a
// gzipped tar. The server pulls it back through the reverse-proxy tunnel to
// collect a session's produced artifacts — no separate upload frame protocol.
// The subdir is resolved through the same traversal guard as file access; the
// per-session .agent-compose runtime dir is skipped (it holds editor state, not
// user artifacts).
func (fs *fileService) handleArchive(w http.ResponseWriter, r *http.Request) {
	root := fs.root
	if sub := strings.TrimSpace(r.URL.Query().Get("path")); sub != "" {
		abs, err := fs.resolve(sub)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		root = abs
	}
	info, err := os.Stat(root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !info.IsDir() {
		http.Error(w, "archive path must be a directory", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="workspace.tar.gz"`)

	gz := gzip.NewWriter(w)
	defer func() { _ = gz.Close() }()
	tw := tar.NewWriter(gz)
	defer func() { _ = tw.Close() }()

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries rather than aborting the whole archive
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return nil
		}
		// Skip the node's per-session runtime dir; it is editor state, not artifacts.
		if rel == ".agent-compose" || strings.HasPrefix(rel, ".agent-compose"+string(filepath.Separator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		// Symlinks are archived as their link target string; regular files/dirs only.
		linkTarget := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			linkTarget, _ = os.Readlink(path)
		}
		hdr, err := tar.FileInfoHeader(fi, linkTarget)
		if err != nil {
			return nil
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()
		_, _ = io.Copy(tw, f)
		return nil
	})
}
