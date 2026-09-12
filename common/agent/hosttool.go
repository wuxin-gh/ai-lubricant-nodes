package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
) // Host-level tool install (currently Node.js). The node downloads the official
// platform archive the server picked, extracts it under a node-managed tools
// directory beside the state dir and puts that bin dir on the process PATH, so
// every later LookPath-based probe (runtime launcher, editor install, version
// labels) resolves the managed install without any root privileges or system
// PATH edits. A tarball Node.js also makes `npm i -g` work unprivileged: npm's
// default prefix is the directory containing the node binary, so global
// shims land in the same user-writable bin dir.

// hostToolDirName is the single fixed install dir under the state dir's tools/
// parent. Fixed name (no version in the path) so no symlink is needed — an
// upgrade swaps the directory atomically via rename, same as RuntimeUpgrade.
const hostToolNodeDirName = "node"

// ManagedToolsParent returns <stateDir>/tools — the parent of all node-managed
// host tool installs. It sits beside the persisted config so a relocated state
// dir relocates the tools too.
func ManagedToolsParent() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), "tools"), nil
}

// ManagedNodeDir returns the node-managed Node.js install dir (empty string
// when the state dir cannot be resolved).
func ManagedNodeDir() string {
	parent, err := ManagedToolsParent()
	if err != nil {
		return ""
	}
	return filepath.Join(parent, hostToolNodeDirName)
}

// ManagedNodeBinDir returns the bin/ dir inside the managed Node.js install
// (empty when there is no managed layout for this platform).
func ManagedNodeBinDir() string {
	dir := ManagedNodeDir()
	if dir == "" {
		return ""
	}
	if runtime.GOOS == "windows" {
		// The Windows zip keeps node.exe/npm.cmd directly at the archive root.
		return dir
	}
	return filepath.Join(dir, "bin")
}

// EnsureManagedNodeOnPath prepends the managed Node.js bin dir to this
// process's PATH when a usable node binary lives there. Idempotent. Called at
// client start (so a restarted node finds its tools again) and right after a
// successful host-tool install (so the current process sees it immediately).
// Children spawned later (runtime launcher, npm, editor CLIs, host shells)
// inherit the adjusted PATH, and every exec.LookPath in this process starts
// resolving the managed binaries — no per-call-site changes needed.
func EnsureManagedNodeOnPath() {
	binDir := ManagedNodeBinDir()
	if binDir == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(binDir, nodeBinary())); err != nil {
		return
	}
	current := os.Getenv("PATH")
	if current == "" {
		os.Setenv("PATH", binDir)
		return
	}
	listSep := string(os.PathListSeparator)
	for _, part := range strings.Split(current, listSep) {
		if strings.EqualFold(filepath.Clean(part), filepath.Clean(binDir)) {
			return // already on PATH
		}
	}
	os.Setenv("PATH", binDir+listSep+current)
}

// HostToolInstaller is the per-tool install implementation. A tool id from the
// whitelist maps to one of these; unknown ids fail the ack.
type HostToolInstaller func(ctx context.Context, spec *agentcomposev2.NodeInstallHostTool, log *slog.Logger, fallback proxySpec) error

// hostToolInstallers enumerates the installable host tools. Node.js installs
// node + npm together — one archive carries both. "xcode" is detection-only:
// there is no automated install (App Store-only distribution), the entry
// exists so the operator's install button returns either "present" or the
// exact reason it cannot be auto-installed.
var hostToolInstallers = map[string]HostToolInstaller{
	"nodejs": installHostNodeJS,
	"xcode":  installXcodeDetection,
}

// InstallHostTool installs one whitelisted host tool and returns the freshly
// probed node/npm/xcodebuild versions for the ack. Errors surface the failing
// stage with the same shape RuntimeUpgrade uses (stage prefix + %w).
func InstallHostTool(
	ctx context.Context,
	spec *agentcomposev2.NodeInstallHostTool,
	log *slog.Logger,
	fallback proxySpec,
) (nodeVersion, npmVersion, xcodebuildVersion string, err error) {
	if log == nil {
		log = slog.Default()
	}
	if spec == nil {
		return "", "", "", fmt.Errorf("host-tool: nil spec")
	}
	tool := strings.TrimSpace(spec.GetTool())
	installer, ok := hostToolInstallers[tool]
	if !ok {
		return "", "", "", fmt.Errorf("host-tool: unsupported tool %q (supported: nodejs)", tool)
	}
	if err := installer(ctx, spec, log, fallback); err != nil {
		return "", "", "", err
	}
	// The PATH prepend above makes the plain LookPath probes below see the
	// managed install (and fall back to any system-wide Node.js that was
	// already there).
	EnsureManagedNodeOnPath()
	nodeVersion = probeHostTool(ctx, "node", "--version")
	npmVersion = probeHostTool(ctx, npmBinary(), "--version")
	// xcode detection has no install step; the probed version is the ack's whole
	// payload. Same probe that fills the xcodebuild_version label at register,
	// so "detected now" and the advertised label cannot disagree.
	xcodebuildVersion = probeHostTool(ctx, "xcodebuild", "-version")
	return nodeVersion, npmVersion, xcodebuildVersion, nil
}

// installXcodeDetection is the Xcode "installer". Full Xcode ships only via
// the App Store (~7GB, Apple ID sign-in, interactive license acceptance), so
// unlike Node.js there is no archive to download — the node cannot install it.
// The entry exists to answer the operator's install click definitively: ack ok
// when a working xcodebuild is present, otherwise an ack error carrying the
// reason auto-install is impossible plus the manual path. The probe reuses
// probeHostTool (same one that fills the xcodebuild_version label at register),
// so "detected here" and "label reported to the server" cannot disagree.
func installXcodeDetection(ctx context.Context, spec *agentcomposev2.NodeInstallHostTool, log *slog.Logger, _ proxySpec) error {
	if v := probeHostTool(ctx, "xcodebuild", "-version"); v != "" {
		log.Info("host-tool: xcode detected", "version", v)
		acceptXcodeLicenseBestEffort(ctx, log)
		return nil
	}

	candidates := discoverXcodeApps()
	for _, app := range candidates {
		devDir := filepath.Join(app, "Contents", "Developer")
		// A real Xcode carries its own xcodebuild inside the app bundle; the
		// /usr/bin shim alone proves nothing (it exists on CLT-only boxes too).
		if _, err := os.Stat(filepath.Join(devDir, "usr", "bin", "xcodebuild")); err != nil {
			log.Debug("host-tool: skipping xcode candidate without toolchain", "app", app)
			continue
		}
		mode := activateDeveloperDir(devDir, log)
		if v := probeHostTool(ctx, "xcodebuild", "-version"); v != "" {
			log.Info("host-tool: xcode detected after activation", "version", v, "app", app, "activation", mode)
			acceptXcodeLicenseBestEffort(ctx, log)
			return nil
		}
	}

	return xcodeDetectionFailure(candidates)
}

// xcodeDetectionFailure builds the ack error when no Xcode on the host could
// be made to run. It names the active developer directory (xcode-select -p):
// the /usr/bin/xcodebuild shim exists on a CLT-only machine too but refuses to
// run — naming it turns "detection failed" into the host's actual state.
func xcodeDetectionFailure(candidates []string) error {
	detail := ""
	if out, err := exec.Command("xcode-select", "-p").Output(); err == nil {
		if active := strings.TrimSpace(string(out)); active != "" {
			detail = fmt.Sprintf("当前 xcode-select 指向 %s。", active)
		}
	}
	if len(candidates) > 0 {
		return fmt.Errorf("本机发现 Xcode（%s）但激活后 xcodebuild 仍无法运行，%s"+
			"常见原因是首次启动未完成组件安装：打开一次 Xcode 让它装完组件，"+
			"或在本机终端执行 sudo xcode-select -s <上述 Xcode>/Contents/Developer 与 "+
			"sudo xcodebuild -license accept 后重试检测",
			strings.Join(candidates, "、"), detail)
	}
	return fmt.Errorf("未在 /Applications、~/Applications、~/Downloads 找到 Xcode.app 或 Xcode-beta.app，%s"+
		"Xcode 只能从 App Store 或 developer.apple.com 手动安装（约 7GB、需 Apple ID 登录并接受许可协议），"+
		"节点无法自动下载安装。装完后回到环境面板再点一次「检测 Xcode」，节点会自动查找并激活它",
		detail)
}

// installHostNodeJS downloads the official Node.js archive for this platform,
// verifies it, extracts it into a staged dir and atomically swaps the managed
// node dir. Layout: the linux/darwin tarballs wrap everything in a single
// top-level node-vX-os-arch/ directory which is stripped; the Windows zip has
// node.exe/npm.cmd at the archive root already.
func installHostNodeJS(ctx context.Context, spec *agentcomposev2.NodeInstallHostTool, log *slog.Logger, fallback proxySpec) error {
	url := strings.TrimSpace(spec.GetDownloadUrl())
	if url == "" {
		return fmt.Errorf("host-tool: download_url is required")
	}
	parent, err := ManagedToolsParent()
	if err != nil {
		return fmt.Errorf("host-tool: resolve tools dir: %w", err)
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("host-tool: prepare tools dir: %w", err)
	}
	target := filepath.Join(parent, hostToolNodeDirName)
	archivePath := target + ".archive"
	stage := target + ".new"
	backup := target + ".old"
	_ = os.Remove(archivePath)
	_ = os.RemoveAll(stage)
	defer os.Remove(archivePath)

	log.Info("host-tool: downloading node.js", "target_version", spec.GetTargetVersion(), "download_url", url)
	pspec := proxySpec{
		mode:      strings.TrimSpace(spec.GetProxyMode()),
		url:       strings.TrimSpace(spec.GetProxyUrl()),
		urlPrefix: strings.TrimSpace(spec.GetProxyUrlPrefix()),
	}
	// Per-frame proxy wins; an unset frame falls back to the persisted node
	// egress-proxy snapshot (same rule as runtime-upgrade).
	if pspec.mode == "" && pspec.url == "" && pspec.urlPrefix == "" {
		pspec = fallback
	}
	if err := downloadTo(ctx, url, archivePath, pspec); err != nil {
		return err
	}

	// Verify: explicit sha256 first; otherwise best-effort the dist's
	// SHASUMS256.txt (one directory up from the archive URL). A missing
	// checksum source only downgrades to a warning — the download itself
	// already succeeded over TLS.
	if want := strings.TrimSpace(spec.GetSha256()); want != "" {
		if err := verifySHA256(archivePath, want); err != nil {
			return fmt.Errorf("host-tool: %w", err)
		}
	} else if sha, err := fetchDistSHA256(ctx, url, pspec); err != nil {
		log.Warn("host-tool: could not load SHASUMS256.txt; skipping checksum", "error", err)
	} else if sha == "" {
		log.Warn("host-tool: archive not listed in SHASUMS256.txt; skipping checksum")
	} else if err := verifySHA256(archivePath, sha); err != nil {
		return fmt.Errorf("host-tool: %w", err)
	}

	if err := extractNodeArchive(archivePath, stage); err != nil {
		_ = os.RemoveAll(stage)
		return err
	}
	if err := validateStagedNode(stage); err != nil {
		_ = os.RemoveAll(stage)
		return err
	}

	// Atomic swap, same choreography as RuntimeUpgrade: old → .old, staged →
	// target, drop the backup on success and roll back on failure.
	_ = os.RemoveAll(backup)
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			_ = os.RemoveAll(stage)
			return fmt.Errorf("host-tool: preserve current node install: %w", err)
		}
	}
	if err := os.Rename(stage, target); err != nil {
		if _, backupErr := os.Stat(backup); backupErr == nil {
			_ = os.Rename(backup, target)
		}
		return fmt.Errorf("host-tool: activate staged node install: %w", err)
	}
	_ = os.RemoveAll(backup)
	log.Info("host-tool: node.js installed", "path", target)
	return nil
}

// fetchDistSHA256 fetches the archive directory's SHASUMS256.txt and returns the
// hex digest listed for the archive file name. Empty string when the file does
// not mention it (mirrors may trim the list). The nodejs.org layout serves
// SHASUMS256.txt beside the release archives, so it is the directory of the
// download URL.
func fetchDistSHA256(ctx context.Context, archiveURL string, spec proxySpec) (string, error) {
	dir := archiveURL[:strings.LastIndex(archiveURL, "/")+1]
	sumsURL := dir + "SHASUMS256.txt"
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	finalURL := resolveDownloadURL(spec, sumsURL)
	client, err := httpClientForProxy(spec)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, finalURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("SHASUMS256.txt: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	name := archiveURL[strings.LastIndex(archiveURL, "/")+1:]
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && strings.EqualFold(fields[1], name) {
			return fields[0], nil
		}
	}
	return "", nil
}

// extractNodeArchive unpacks the downloaded archive into stage. tar.gz on
// linux/darwin (strip the single top-level dir), zip on windows.
func extractNodeArchive(archivePath, stage string) error {
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return fmt.Errorf("host-tool: prepare stage dir: %w", err)
	}
	if runtime.GOOS == "windows" || strings.HasSuffix(strings.ToLower(archivePath), ".zip") {
		return extractNodeZip(archivePath, stage)
	}
	return extractNodeTarGz(archivePath, stage)
}

// extractNodeTarGz handles the linux/darwin layout: one top-level
// node-vX-os-arch/ directory holding bin/node, bin/npm, …. The top-level name
// is not fixed (version/platform vary), so the first regular/symlink entry's
// dir count decides what to strip; entries are written relative to it.
func extractNodeTarGz(archivePath, stage string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("host-tool: open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("host-tool: read gzip: %w", err)
	}
	defer gz.Close()
	root := filepath.Clean(stage)
	tr := tar.NewReader(gz)
	var strip string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("host-tool: read tar: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." || strings.HasPrefix(name, "..") {
			return fmt.Errorf("host-tool: archive entry escapes stage dir: %s", hdr.Name)
		}
		if strip == "" {
			// First entry decides the single top-level directory to strip.
			if hdr.Typeflag == tar.TypeDir {
				strip = name
				continue
			}
			// No wrapping dir: archive root IS the install root.
			strip = "."
		}
		rel := strings.TrimPrefix(name, strip)
		rel = strings.TrimPrefix(rel, string(filepath.Separator))
		if rel == "" {
			continue
		}
		dest := filepath.Join(root, rel)
		if dest != root && !strings.HasPrefix(dest, root+string(filepath.Separator)) {
			return fmt.Errorf("host-tool: archive entry escapes stage dir: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, os.FileMode(hdr.Mode)&0o777); err != nil {
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
		case tar.TypeSymlink:
			// The official archives ship bin/npm → ../lib/node_modules/npm/… as
			// a relative symlink; keep symlinks, but only relative ones.
			link := hdr.Linkname
			if link == "" || filepath.IsAbs(link) {
				return fmt.Errorf("host-tool: unexpected symlink %q -> %q", hdr.Name, link)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return err
			}
			_ = os.Remove(dest)
			if err := os.Symlink(link, dest); err != nil {
				// Some filesystems (FAT) refuse symlinks; degrade by skipping —
				// validateStagedNode catches a truly unusable archive.
				continue
			}
		default:
			return fmt.Errorf("host-tool: unsupported archive entry type %v: %s", hdr.Typeflag, hdr.Name)
		}
	}
	return nil
}

// extractNodeZip handles the Windows layout: node.exe/npm.cmd at the archive
// root (the official zips have no wrapping directory).
func extractNodeZip(archivePath, stage string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("host-tool: open zip: %w", err)
	}
	defer r.Close()
	root := filepath.Clean(stage)
	for _, f := range r.File {
		name := filepath.Clean(filepath.FromSlash(f.Name))
		if name == "." || strings.HasPrefix(name, "..") {
			return fmt.Errorf("host-tool: zip entry escapes stage dir: %s", f.Name)
		}
		dest := filepath.Join(root, name)
		if dest != root && !strings.HasPrefix(dest, root+string(filepath.Separator)) {
			return fmt.Errorf("host-tool: zip entry escapes stage dir: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		in, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode())
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		in.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// validateStagedNode fails fast when the extracted archive cannot run: the node
// binary must exist at the expected platform path. A missing npm is not fatal
// here (probeHostTool reports it); the node binary alone is the hard floor.
func validateStagedNode(stage string) error {
	candidates := []string{
		filepath.Join(stage, "bin", "node"),
		filepath.Join(stage, "node.exe"),
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return nil
		}
	}
	return fmt.Errorf("host-tool: staged archive has no node binary (looked for bin/node, node.exe)")
}

// npmBinary returns the npm executable name for this platform (mirrors the
// execution-node editor installer's helper).
func npmBinary() string {
	if runtime.GOOS == "windows" {
		return "npm.cmd"
	}
	return "npm"
}

// ── Xcode discovery / activation ──────────────────────────────────────────────

// discoverXcodeApps returns installed Xcode app bundles by preference: the
// stable "Xcode.app" first, then "Xcode-beta.app", then any other Xcode*.app
// (Apple also ships "Xcode 26.0 beta 2.app" style names). Scans /Applications,
// ~/Applications and ~/Downloads — the latter because a freshly extracted xip
// often never made it out of there.
func discoverXcodeApps() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{"/Applications"}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, "Applications"), filepath.Join(home, "Downloads"))
	}
	return filterXcodeCandidates(dirs)
}

// filterXcodeCandidates is the testable core of discoverXcodeApps: glob each
// dir for Xcode*.app, keep real directories, stable name first, then beta,
// then anything else sorted.
func filterXcodeCandidates(dirs []string) []string {
	var exact, rest []string
	for _, dir := range dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "Xcode*.app"))
		for _, m := range matches {
			if info, err := os.Stat(m); err != nil || !info.IsDir() {
				continue
			}
			switch filepath.Base(m) {
			case "Xcode.app", "Xcode-beta.app":
				exact = append(exact, m)
			default:
				rest = append(rest, m)
			}
		}
	}
	sort.Slice(exact, func(i, j int) bool {
		// "Xcode.app" (stable) beats "Xcode-beta.app" — lexicographic order
		// would put the beta first.
		return filepath.Base(exact[i]) == "Xcode.app"
	})
	sort.Strings(rest)
	return append(exact, rest...)
}

// activateDeveloperDir points the toolchain at devDir and returns how:
// "xcode-select" (system-wide, needs root — the node often runs as root on
// dedicated build hosts) or "DEVELOPER_DIR" (rootless, scoped to this process
// and its children — the build runner included). The env fallback is
// persisted via saveDeveloperDir and re-applied by EnsureDeveloperDir at
// startup, so a restart keeps working without anyone re-clicking detect.
func activateDeveloperDir(devDir string, log *slog.Logger) string {
	if out, err := exec.Command("xcode-select", "-s", devDir).CombinedOutput(); err == nil {
		return "xcode-select"
	} else {
		log.Debug("host-tool: xcode-select -s failed (root needed?), falling back to DEVELOPER_DIR",
			"error", err, "output", strings.TrimSpace(string(out)))
	}
	os.Setenv("DEVELOPER_DIR", devDir)
	if err := saveDeveloperDir(devDir); err != nil {
		log.Warn("host-tool: persist DEVELOPER_DIR for restarts failed", "error", err)
	}
	return "DEVELOPER_DIR"
}

// developerDirPath is the state file recording the DEVELOPER_DIR fallback so
// a node restart re-applies it before the register-time host probes.
func developerDirPath() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "developer-dir"), nil
}

func saveDeveloperDir(devDir string) error {
	path, err := developerDirPath()
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(devDir), 0o644)
}

// EnsureDeveloperDir re-applies the persisted DEVELOPER_DIR fallback at
// startup (before the register-time HostToolLabels probe), mirroring
// EnsureManagedNodeOnPath. Idempotent; a stale entry (Xcode uninstalled) is
// dropped so the state file never pins a dead path.
func EnsureDeveloperDir() {
	if os.Getenv("DEVELOPER_DIR") != "" {
		return // operator-provided env wins; never override it
	}
	path, err := developerDirPath()
	if err != nil {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	devDir := strings.TrimSpace(string(raw))
	if devDir == "" {
		return
	}
	if _, err := os.Stat(devDir); err != nil {
		_ = os.Remove(path) // the Xcode it pointed at is gone; reset cleanly
		return
	}
	os.Setenv("DEVELOPER_DIR", devDir)
}

// acceptXcodeLicenseBestEffort runs `xcodebuild -license accept` once the
// toolchain runs. License acceptance needs root when the app bundle is
// root-owned (the usual case); on a non-root node it fails fast and we only
// log — detection (-version) succeeds unlicensed, the build would not, and
// the operator can accept manually with sudo.
func acceptXcodeLicenseBestEffort(ctx context.Context, log *slog.Logger) {
	licenseCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(licenseCtx, "xcodebuild", "-license", "accept").CombinedOutput(); err != nil {
		log.Debug("host-tool: xcodebuild -license accept not completed (non-root?)",
			"error", err, "output", strings.TrimSpace(string(out)),
			"hint", "build will prompt; run sudo xcodebuild -license accept on the host")
	}
}
