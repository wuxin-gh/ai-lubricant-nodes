// Archive source support for the build runner.
//
// The historic build source is a git clone (source_kind "" / "git"). This file
// adds the second source kind: download a plain archive (tar.gz / zip) over
// http(s) and extract it. That lets a build point at something with no git
// remote at all — a GitHub codeload archive, a release asset, a mirror tarball —
// which is what the iOS device-proxy verification app uses (its source ships as
// a repository archive rather than a git URL).
//
// The download routes through the node's persisted egress-proxy snapshot exactly
// like every other node download (self-upgrade, WDA artifact, Xcode .xip): a
// GFW-restricted host reaches the archive through the same route. Extraction is
// path-traversal-safe (tar-slip / zip-slip guards, mirroring the host-tool
// extractors) and strips the single wrapping directory that both tar.gz and zip
// repository archives carry, so the workspace root is the project root.
package build

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxArchiveBytes bounds a source archive download. Source trees are small (a
// few MiB); this only stops a runaway/HTML error page from filling the disk.
const maxArchiveBytes = 2 << 30 // 2 GiB

// fetchArchive downloads sourceURL as an archive and extracts it under destDir,
// returning the directory that should be used as the workspace root. subdir, if
// non-empty, is appended to the extracted project root (for monorepo archives).
//
// The caller supplies the http.Client (proxy-aware) and the URL already resolved
// for url_prefix mode, so this function has no dependency on the agent package.
func fetchArchive(ctx context.Context, client *http.Client, sourceURL, destDir, subdir string, onStage func(msg string)) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	// Scratch lives entirely under destDir so the whole thing is one workspace
	// subtree the caller removes with the build's temp dir.
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("archive: prepare dir: %w", err)
	}
	archivePath := filepath.Join(destDir, ".source.archive")

	if onStage != nil {
		onStage("downloading " + sourceURL)
	}
	if err := downloadArchive(ctx, client, sourceURL, archivePath); err != nil {
		return "", err
	}

	rawRoot := filepath.Join(destDir, ".extract")
	if err := os.MkdirAll(rawRoot, 0o755); err != nil {
		return "", err
	}
	if onStage != nil {
		onStage("extracting archive")
	}
	if err := extractArchive(archivePath, rawRoot); err != nil {
		return "", err
	}

	// Strip the single wrapping directory (repo-<sha>/ for both codeload tar.gz
	// and GitHub zip archives). If the archive has no wrapper (multiple entries
	// or a lone file), the extracted root IS the project root.
	projectRoot := rawRoot
	if inner, ok := singleSubdir(rawRoot); ok {
		projectRoot = inner
	}
	if sub := strings.Trim(strings.TrimSpace(subdir), "/"); sub != "" {
		projectRoot = filepath.Join(projectRoot, filepath.FromSlash(sub))
		// Guard against a subdir that walks out of the tree.
		if rel, err := filepath.Rel(rawRoot, projectRoot); err != nil || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("archive: source_subdir %q escapes the extracted tree", subdir)
		}
	}
	info, err := os.Stat(projectRoot)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("archive: project root not found (subdir=%q)", subdir)
	}
	return projectRoot, nil
}

// downloadArchive streams sourceURL to dest with a bounded size. Non-2xx is an
// error (a GitHub HTML 404 page must not be extracted as a bogus tree).
func downloadArchive(ctx context.Context, client *http.Client, sourceURL, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return fmt.Errorf("archive: bad url %q: %w", sourceURL, err)
	}
	req.Header.Set("User-Agent", "ai-lubricant-node-build")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("archive: download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("archive: download rejected: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("archive: create archive file: %w", err)
	}
	defer f.Close()
	limited := io.LimitReader(resp.Body, maxArchiveBytes+1)
	n, err := io.Copy(f, limited)
	if err != nil {
		return fmt.Errorf("archive: download interrupted: %w", err)
	}
	if n > maxArchiveBytes {
		return fmt.Errorf("archive: download exceeds %d bytes", maxArchiveBytes)
	}
	return nil
}

// extractArchive unpacks a .tar.gz or .zip archive into dest, dispatching on
// the file's magic bytes (a codeload URL has no reliable extension). Entry
// names are path-traversal-guarded.
func extractArchive(archivePath, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("archive: open: %w", err)
	}
	head := make([]byte, 4)
	n, _ := io.ReadFull(f, head)
	_ = f.Close()
	magic := head[:n]
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		return extractTarGz(archivePath, dest)
	case len(magic) >= 4 && magic[0] == 'P' && magic[1] == 'K':
		return extractZip(archivePath, dest)
	default:
		return errors.New("archive: unrecognized format (expected gzip or zip)")
	}
}

func extractTarGz(archivePath, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("archive: gzip: %w", err)
	}
	defer gz.Close()
	root := filepath.Clean(dest)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("archive: tar: %w", err)
		}
		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777|0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Only relative symlinks; a source tree legitimately carries them, but
			// an absolute one could point outside the workspace.
			link := hdr.Linkname
			if link == "" || filepath.IsAbs(link) {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(link, target); err != nil {
				continue // filesystems without symlink support degrade by skipping
			}
		default:
			// Skip devices/fifos/other special entries — a source archive needs none.
			continue
		}
	}
	return nil
}

func extractZip(archivePath, dest string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("archive: zip: %w", err)
	}
	defer r.Close()
	root := filepath.Clean(dest)
	for _, zf := range r.File {
		target, err := safeJoin(root, zf.Name)
		if err != nil {
			return err
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		// Preserve the executable bit (build scripts need it); zip stores it in
		// the high mode bits.
		mode := zf.Mode()
		if mode&0o111 != 0 {
			mode = 0o755
		} else {
			mode = 0o644
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
		if err != nil {
			_ = rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			_ = out.Close()
			_ = rc.Close()
			return err
		}
		_ = rc.Close()
		if err := out.Close(); err != nil {
			return err
		}
	}
	return nil
}

// safeJoin joins name onto root, rejecting any entry that escapes root
// (tar-slip / zip-slip). Absolute names and ".." components are refused.
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." {
		return root, nil
	}
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("archive: entry escapes destination: %s", name)
	}
	target := filepath.Join(root, clean)
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", fmt.Errorf("archive: entry escapes destination: %s", name)
	}
	return target, nil
}

// singleSubdir returns the one subdirectory inside root when root holds exactly
// one entry and it is a directory (the wrapper directory a repository archive
// carries). Otherwise ok is false and the caller keeps root.
func singleSubdir(root string) (string, bool) {
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		return "", false
	}
	return filepath.Join(root, entries[0].Name()), true
}

// archiveDownloadTimeout bounds a source-archive download (large enough for a
// big monorepo tarball on a slow link, small enough to fail fast on a hang).
const archiveDownloadTimeout = 10 * time.Minute
