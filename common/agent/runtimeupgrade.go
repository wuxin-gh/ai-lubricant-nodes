package agent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

const runtimeDirName = "runtime"

// runtimeReleaseMarker records the marketplace *release* tag (e.g. 20260816-0846)
// the currently-installed runtime came from. The runtime's own package.json
// carries a semver (e.g. 0.6.0) that lives in a different version space than the
// release tag the server compares against; reporting that semver made the server
// see a permanent "runtime needs upgrade". So RuntimeUpgrade stamps the release
// tag here, and ManagedRuntimeVersion prefers it — a node upgraded through the
// server reports the release tag and compares cleanly.
const runtimeReleaseMarker = ".release-version"

// RuntimeDir is the node-managed JavaScript runtime directory. It lives beside
// the persisted node config (not beside the executable) so service accounts and
// Program Files installs remain writable on Windows.
func RuntimeDir() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, runtimeDirName), nil
}

// resolveRuntimeLauncher returns the executable and its leading args for the
// node's agent runtime: the managed JS runtime (``node dist/cli.js``) when
// installed, else the legacy globally-installed ``agent-compose-runtime``. It is
// the single resolution point shared by RuntimeCommand (which builds the
// exec.Cmd) and RuntimeInstalled (a spawn-free pre-flight). A non-nil error
// means no usable launcher exists — the message names the concrete gap (missing
// Node.js beside a managed runtime, or no runtime at all).
func resolveRuntimeLauncher() (string, []string, error) {
	managed, err := RuntimeDir()
	if err == nil {
		cli := filepath.Join(managed, "dist", "cli.js")
		if info, statErr := os.Stat(cli); statErr == nil && !info.IsDir() {
			node, lookErr := LookPath(nodeBinary())
			if lookErr != nil {
				return "", nil, fmt.Errorf("node runtime is installed but Node.js is missing: %w", lookErr)
			}
			return node, []string{cli}, nil
		}
	}
	legacy, err := LookPath("agent-compose-runtime")
	if err != nil {
		return "", nil, fmt.Errorf("agent-compose-runtime not found on node: %w", err)
	}
	return legacy, nil, nil
}

// RuntimeCommand resolves the managed runtime first, then falls back to a
// legacy globally-installed agent-compose-runtime command. args are appended to
// whichever launcher is selected.
func RuntimeCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	name, lead, err := resolveRuntimeLauncher()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, name, append(lead, args...)...), nil
}

// RuntimeInstalled reports whether a usable agent-runtime launcher exists on
// this node, without spawning it. selectExecutor calls this before a local run
// so a node that passed approval but lost its runtime rejects the create ack
// with a clear reason, instead of spawning a process that immediately exits 1
// (the "session finished exit_code=1" symptom that looks like a dispatch
// failure but is really a missing runtime).
func RuntimeInstalled() error {
	_, _, err := resolveRuntimeLauncher()
	return err
}

func nodeBinary() string {
	if runtime.GOOS == "windows" {
		return "node.exe"
	}
	return "node"
}

// ManagedRuntimeVersion probes the node-managed runtime without depending on a
// global npm shim. Empty means no usable managed runtime is installed.
//
// It prefers the release-tag marker (written by a server-driven RuntimeUpgrade)
// over the runtime's own semver: the marker is in the same version space as the
// marketplace release the server compares against, so a node that upgraded
// through the server reports a version the server can match. Only a fresh
// install (no marker yet) falls back to running ``node cli.js --version`` or, if
// Node.js isn't on PATH, reading ``runtime/package.json``'s version field — that
// value clears the approve gate until the first upgrade writes the marker.
func ManagedRuntimeVersion(ctx context.Context) string {
	dir, err := RuntimeDir()
	if err != nil {
		return ""
	}
	if v := strings.TrimSpace(runtimeReleaseMarkerValue(dir)); v != "" {
		return v
	}
	cli := filepath.Join(dir, "dist", "cli.js")
	if _, err := os.Stat(cli); err != nil {
		return ""
	}
	if node, lookErr := LookPath(nodeBinary()); lookErr == nil {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, runErr := exec.CommandContext(cctx, node, cli, "--version").CombinedOutput()
		if runErr == nil {
			if v := strings.TrimSpace(string(out)); v != "" {
				return v
			}
		}
	}
	return runtimePackageVersion(dir)
}

// runtimeReleaseMarkerValue reads the release-tag marker file the last
// RuntimeUpgrade wrote into the runtime directory. Empty means none is present
// (fresh install / never upgraded through the server).
func runtimeReleaseMarkerValue(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, runtimeReleaseMarker))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// runtimePackageVersion reads the “version“ field of the managed runtime's
// package.json. It is the fallback path used when Node.js is not on PATH: the
// install bootstrap has already extracted the archive, so the version is
// discoverable without executing the runtime.
func runtimePackageVersion(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return ""
	}
	return strings.TrimSpace(pkg.Version)
}

// RuntimeUpgrade installs a platform runtime archive into the node-managed
// directory. Extraction happens in a sibling staging directory; the old runtime
// is retained until the staged package has passed structural validation. The
// per-frame proxy takes precedence; when unset it falls back to the node's
// persisted egress-proxy snapshot. Callers without a Client pass a zero fallback.
func RuntimeUpgrade(ctx context.Context, spec *agentcomposev2.NodeRuntimeUpgrade, log *slog.Logger, fallback proxySpec) error {
	if log == nil {
		log = slog.Default()
	}
	if spec == nil {
		return fmt.Errorf("runtime-upgrade: nil spec")
	}
	url := strings.TrimSpace(spec.GetDownloadUrl())
	if url == "" {
		return fmt.Errorf("runtime-upgrade: download_url is required")
	}
	target, err := RuntimeDir()
	if err != nil {
		return fmt.Errorf("runtime-upgrade: resolve install dir: %w", err)
	}
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("runtime-upgrade: prepare install dir: %w", err)
	}
	archivePath := target + ".tar.gz.new"
	stage := target + ".new"
	backup := target + ".old"
	_ = os.Remove(archivePath)
	_ = os.RemoveAll(stage)
	defer os.Remove(archivePath)

	log.Info("runtime-upgrade: downloading", "target_version", spec.GetTargetVersion(), "download_url", url, "proxy_mode", spec.GetProxyMode())
	pspec := proxySpec{
		mode:      strings.TrimSpace(spec.GetProxyMode()),
		url:       strings.TrimSpace(spec.GetProxyUrl()),
		urlPrefix: strings.TrimSpace(spec.GetProxyUrlPrefix()),
	}
	// Per-frame proxy wins; when the frame didn't carry one, fall back to the
	// persisted node egress-proxy snapshot (same rule as self-upgrade).
	if pspec.mode == "" && pspec.url == "" && pspec.urlPrefix == "" {
		pspec = fallback
	}
	if err := downloadTo(ctx, url, archivePath, pspec); err != nil {
		return err
	}
	if want := strings.TrimSpace(spec.GetSha256()); want != "" {
		if err := verifySHA256(archivePath, want); err != nil {
			return err
		}
	}
	if err := extractRuntimeArchive(archivePath, stage); err != nil {
		_ = os.RemoveAll(stage)
		return err
	}
	if _, err := os.Stat(filepath.Join(stage, "dist", "cli.js")); err != nil {
		_ = os.RemoveAll(stage)
		return fmt.Errorf("runtime-upgrade: archive is missing runtime/dist/cli.js")
	}

	_ = os.RemoveAll(backup)
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			_ = os.RemoveAll(stage)
			return fmt.Errorf("runtime-upgrade: preserve current runtime: %w", err)
		}
	}
	if err := os.Rename(stage, target); err != nil {
		if _, backupErr := os.Stat(backup); backupErr == nil {
			_ = os.Rename(backup, target)
		}
		return fmt.Errorf("runtime-upgrade: activate staged runtime: %w", err)
	}
	_ = os.RemoveAll(backup)
	// Stamp the marketplace release tag so ManagedRuntimeVersion reports a value
	// in the same version space the server compares against (best-effort: a write
	// failure only means the node falls back to package.json semver next probe).
	if tag := strings.TrimSpace(spec.GetTargetVersion()); tag != "" {
		if err := os.WriteFile(filepath.Join(target, runtimeReleaseMarker), []byte(tag), 0o644); err != nil {
			log.Warn("runtime-upgrade: write release marker failed", "err", err)
		}
	}
	log.Info("runtime-upgrade: installed", "path", target, "target_version", spec.GetTargetVersion())
	return nil
}

func extractRuntimeArchive(archivePath, stage string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("runtime-upgrade: open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("runtime-upgrade: read gzip: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	root := filepath.Clean(stage)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("runtime-upgrade: read tar: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		parts := strings.Split(name, string(filepath.Separator))
		if len(parts) == 0 || parts[0] != runtimeDirName {
			return fmt.Errorf("runtime-upgrade: archive entry must be under runtime/: %s", hdr.Name)
		}
		rel := filepath.Join(parts[1:]...)
		dest := filepath.Join(root, rel)
		if dest != root && !strings.HasPrefix(dest, root+string(filepath.Separator)) {
			return fmt.Errorf("runtime-upgrade: archive entry escapes install dir: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("runtime-upgrade: links are not allowed in runtime archives: %s", hdr.Name)
		}
	}
	return nil
}
