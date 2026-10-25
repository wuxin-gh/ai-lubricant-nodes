package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// errRestartExit is returned by restartSelf on platforms (Windows) that start a
// fresh process instead of replacing the image: the caller must exit the current
// process so only the new one remains. IsRestartExit distinguishes it from a
// real failure.
var errRestartExit = errors.New("self-upgrade: restart requested, current process should exit")

// IsRestartExit reports whether err signals "upgrade started a new process,
// exit now" rather than a genuine upgrade failure.
func IsRestartExit(err error) bool { return errors.Is(err, errRestartExit) }

// SelfUpgrade downloads a replacement binary for THIS node and re-execs it.
//
// The server pushes a NodeSelfUpgrade frame with the platform-specific download
// URL (a public mirror of the freshly built binary) and an optional sha256. The
// node fetches it next to its current executable, verifies the checksum, swaps
// it into place atomically, and restarts its own process against the original
// argv — the reconnect then re-registers with the new client_version.
//
// Docker deployments cannot self-swap their image this way; those are detected
// and refused with a clear error so the console can tell the operator to re-pull.
//
// This function does not return on success under Unix (syscall.Exec replaces the
// process image); on Windows it starts the replacement process and returns nil,
// after which the caller should exit. Any error means the running binary is
// untouched (the download/verify failed before the swap) OR the swap partially
// happened and is reported for diagnosis.
// SelfUpgrade downloads and atomically installs a new node binary, then
// restarts into it. The per-frame proxy (admin override for this one upgrade)
// takes precedence; when unset it falls back to the node's persisted egress-proxy
// snapshot pushed by the server. Callers without a Client pass a zero fallback.
// onProgress (may be nil) streams NodeUpgradeEvent stages upstream so the
// console shows live download bytes and the step that failed; every early
// error return emits "failed" before returning.
func SelfUpgrade(ctx context.Context, spec *agentcomposev2.NodeSelfUpgrade, log *slog.Logger, fallback proxySpec, onProgress UpgradeProgressFunc) error {
	if log == nil {
		log = slog.Default()
	}
	emit := func(stage, msg string, pct int, cur, total int64) {
		if onProgress != nil {
			onProgress(stage, msg, pct, cur, total)
		}
	}
	fail := func(stage, msg string) {
		emit("failed", msg, 0, 0, 0)
	}
	if spec == nil {
		log.Error("self-upgrade: nil spec")
		fail("downloading", "self-upgrade: nil spec")
		return fmt.Errorf("self-upgrade: nil spec")
	}
	url := strings.TrimSpace(spec.GetDownloadUrl())
	targetVersion := strings.TrimSpace(spec.GetTargetVersion())
	pspec := proxySpec{
		mode:      strings.TrimSpace(spec.GetProxyMode()),
		url:       strings.TrimSpace(spec.GetProxyUrl()),
		urlPrefix: strings.TrimSpace(spec.GetProxyUrlPrefix()),
	}
	// Per-frame proxy wins; when the frame didn't carry one, fall back to the
	// persisted node egress-proxy snapshot so an upgrade still honors the
	// admin's configured node proxy without a per-upgrade override.
	if pspec.mode == "" && pspec.url == "" && pspec.urlPrefix == "" {
		pspec = fallback
	}
	log = log.With("target_version", targetVersion, "download_url", url, "proxy_mode", pspec.mode)
	log.Info("self-upgrade: received upgrade command, starting")
	if url == "" {
		log.Error("self-upgrade: download_url is required")
		fail("downloading", "self-upgrade: download_url is required")
		return fmt.Errorf("self-upgrade: download_url is required")
	}
	if RunningInContainer() {
		log.Warn("self-upgrade: rejected — running inside docker; re-pull the image instead")
		fail("downloading", "self-upgrade unsupported for docker deployments; re-pull the node image to update")
		return fmt.Errorf("self-upgrade unsupported for docker deployments; re-pull the node image to update")
	}

	exe, err := os.Executable()
	if err != nil {
		log.Error("self-upgrade: locate current executable failed", "error", err)
		fail("downloading", fmt.Sprintf("locate current executable: %v", err))
		return fmt.Errorf("self-upgrade: locate current executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		log.Error("self-upgrade: resolve executable path failed", "error", err)
		fail("downloading", fmt.Sprintf("resolve executable path: %v", err))
		return fmt.Errorf("self-upgrade: resolve executable path: %w", err)
	}
	log.Info("self-upgrade: current binary resolved", "path", exe)

	newPath := exe + ".new"
	log.Info("self-upgrade: downloading new binary", "dest", newPath)
	emit("downloading", "开始下载节点程序", 0, 0, 0)
	if err := progressDownload(ctx, url, newPath, pspec, onProgress); err != nil {
		log.Error("self-upgrade: download failed", "error", err)
		fail("downloading", fmt.Sprintf("下载节点程序失败：%v", err))
		_ = os.Remove(newPath)
		return err
	}
	if want := strings.TrimSpace(spec.GetSha256()); want != "" {
		log.Info("self-upgrade: verifying sha256", "expected", want)
		emit("verifying", "校验 sha256", 92, 0, 0)
		if err := verifySHA256(newPath, want); err != nil {
			log.Error("self-upgrade: checksum verification failed", "error", err)
			fail("verifying", fmt.Sprintf("sha256 校验失败：%v", err))
			_ = os.Remove(newPath)
			return err
		}
		log.Info("self-upgrade: checksum verified")
	} else {
		log.Info("self-upgrade: no sha256 provided, skipping verification")
	}
	if err := os.Chmod(newPath, 0o755); err != nil {
		log.Error("self-upgrade: chmod new binary failed", "error", err)
		fail("swapping", fmt.Sprintf("chmod new binary: %v", err))
		_ = os.Remove(newPath)
		return fmt.Errorf("self-upgrade: chmod new binary: %w", err)
	}

	log.Info("self-upgrade: swapping binary into place")
	emit("swapping", "替换节点程序二进制", 96, 0, 0)
	if err := swapBinary(exe, newPath); err != nil {
		log.Error("self-upgrade: swap failed", "error", err)
		fail("swapping", fmt.Sprintf("替换二进制失败：%v", err))
		return err
	}
	log.Info("self-upgrade: binary swapped, restarting", "path", exe)
	// Last event the process can send: on Unix restartSelf replaces the image
	// (never returns), on Windows it spawns the replacement. Give the stream a
	// short window to flush so the console sees "restarting" before the link
	// drops; the terminal "completed" comes from the reconnect reporting the
	// new client_version, not from here.
	emit("restarting", "正在重启节点程序", 98, 0, 0)
	time.Sleep(300 * time.Millisecond)
	// swapBinary left the new binary at exe; restart against the original argv.
	return restartSelf(exe)
}

// downloadTo fetches url into dest (truncating any existing file). A generous
// timeout covers slow links; the binaries are ~15MB. When spec selects a
// proxy, the client honors it (http/https via Transport.Proxy, socks5 via a
// dialer, url_prefix by rewriting the URL onto a default client). Thin wrapper
// over progressDownload without events — the host-tool installer and tests
// only need the bytes on disk.
func downloadTo(ctx context.Context, url, dest string, spec proxySpec) error {
	return progressDownload(ctx, url, dest, spec, nil)
}

// verifySHA256 checks that the file at path hashes to want (hex, case-insensitive).
func verifySHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("self-upgrade: open for checksum: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("self-upgrade: read for checksum: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, strings.TrimSpace(want)) {
		return fmt.Errorf("self-upgrade: checksum mismatch: got %s want %s", got, want)
	}
	return nil
}

// RunningInContainer reports whether this process is inside a container. Two
// callers: self-upgrade refuses a binary swap here (the image is immutable),
// and ResolveSystemEnvCapability's "auto" default keeps env_mode=system off
// here (a container's HOME is the image's, not the operator's). Best-effort
// probe; on Windows both paths simply don't exist, so it reports false — the
// expected answer for a host install.
func RunningInContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(data)
		if strings.Contains(s, "docker") || strings.Contains(s, "containerd") || strings.Contains(s, "kubepods") {
			return true
		}
	}
	return false
}

// osArchSuffix is the "<os>-<arch>" fragment used in node binary names, matching
// nodes/build.sh. Exposed so the server can name the download; kept here so the
// node and server agree on the convention.
func OSArchSuffix() string {
	return runtime.GOOS + "-" + runtime.GOARCH
}
