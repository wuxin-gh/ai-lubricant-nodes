// Xcode install job: the node-side executor for the async host-tool pipeline.
// Unlike Node.js (a ~30MB tarball that fits the synchronous InstallHostTool
// ack window), a full Xcode ships as a multi-gigabyte .xip whose download +
// extraction + first launch take tens of minutes to hours — so it runs as a
// background job with progress streaming back, mirroring common/build's
// Runner lifecycle:
//
//	start (short ack = accepted) → background stages → progress events →
//	exactly one terminal result
//
// checking → downloading → verifying → extracting → moving → activating → probing
//
// The server resolves the .xip URL (xcodereleases.com) and gates macOS
// compatibility before dispatch (it has both the release's minimum-macOS and
// the node's version in its capability tables); the node re-enforces the
// security floor — HTTPS-only, .xip-suffixed URL, sanitized job id — because a
// frame, not the server's database, is the trust boundary here.
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// XcodeJobRunner tracks and executes Xcode install jobs for this host.
type XcodeJobRunner struct {
	emit   func(*agentcomposev2.NodeUpstreamFrame) error
	logger *slog.Logger
	// proxy returns the node's persisted egress-proxy snapshot; used when the
	// job frame carries no proxy fields (same per-frame-wins rule as
	// installHostNodeJS and runtime upgrades).
	proxy func() proxySpec

	// execFn / downloadFn are production seams (the build runner's pattern):
	// tests inject fakes instead of requiring xcodebuild/xip on the host.
	execFn     func(ctx context.Context, dir, name string, args []string) ([]byte, error)
	downloadFn func(ctx context.Context, j *xcodeJob, url, dest string, spec proxySpec, total int64) error
	// goos overrides runtime.GOOS in tests (the pipeline is darwin-only and
	// CI hosts are not).
	goos string
	// freeFn overrides the free-disk probe in tests (the real one is a
	// darwin syscall).
	freeFn func(path string) (int64, error)
	// discoverAppsFn / activateDevDirFn override the installed-Xcode discovery
	// + activation in tests (the real ones touch /Applications and run
	// xcode-select, which must not happen on a CI host). Defaults resolve to
	// discoverXcodeApps / activateDeveloperDir at call time.
	discoverAppsFn   func() []string
	activateDevDirFn func(devDir string) string
	// moveAppFn overrides moveXcodeApp in tests (the real rename targets the
	// standard /Applications/Xcode.app, which a CI host must not touch).
	// Returns the final app path.
	moveAppFn func(appDir, dest string) (string, error)

	mu   sync.Mutex
	jobs map[string]*xcodeJob
}

type xcodeJob struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
	seq    int64
}

// xcodeLogTailBytes caps the output tail carried in an event frame (same bound
// as the build runner: the control plane must not carry unbounded payloads).
const xcodeLogTailBytes = 4 << 10 // 4 KiB

// NewXcodeJobRunner builds the runner. proxy may be nil (direct downloads).
func NewXcodeJobRunner(emit func(*agentcomposev2.NodeUpstreamFrame) error, log *slog.Logger, proxy func() proxySpec) *XcodeJobRunner {
	if log == nil {
		log = slog.Default()
	}
	return &XcodeJobRunner{emit: emit, logger: log, proxy: proxy, jobs: map[string]*xcodeJob{}}
}

// Start registers and launches one Xcode install. Non-blocking: it returns as
// soon as the job is registered — never after the pipeline finishes. A retried
// dispatch for an already-running job is accepted idempotently; a different job
// while one runs is rejected (Xcode installs contend for /Applications and
// disk, so they must not overlap).
func (r *XcodeJobRunner) Start(ctx context.Context, req *agentcomposev2.NodeHostToolJob) error {
	jobID := strings.TrimSpace(req.GetJobId())
	if jobID == "" {
		return errors.New("xcode-job: job_id is required")
	}
	if strings.TrimSpace(req.GetTool()) != "xcode" {
		return fmt.Errorf("xcode-job: unsupported tool %q (only xcode)", req.GetTool())
	}
	url := strings.TrimSpace(req.GetDownloadUrl())
	if !strings.HasPrefix(url, "https://") || !strings.HasSuffix(strings.ToLower(url), ".xip") {
		return fmt.Errorf("xcode-job: download_url must be an https .xip link, got %q", url)
	}

	r.mu.Lock()
	if _, running := r.jobs[jobID]; running {
		r.mu.Unlock()
		return nil
	}
	if len(r.jobs) > 0 {
		r.mu.Unlock()
		return errors.New("xcode-job: another Xcode install is already running on this host")
	}
	jobCtx, cancel := context.WithCancel(ctx)
	j := &xcodeJob{id: jobID, cancel: cancel, done: make(chan struct{})}
	r.jobs[jobID] = j
	r.mu.Unlock()

	timeout := time.Duration(req.GetTimeoutSeconds()) * time.Second
	if timeout <= 0 {
		timeout = 4 * time.Hour // a 12GB download on a slow link plus extraction
	}
	runCtx, cancelTimeout := context.WithTimeout(jobCtx, timeout)

	go func() {
		defer close(j.done)
		defer func() {
			r.mu.Lock()
			delete(r.jobs, jobID)
			r.mu.Unlock()
			cancelTimeout()
			cancel()
		}()
		out := r.runJob(runCtx, j, req)
		r.finish(j, out)
	}()
	return nil
}

// Cancel asks a running install to stop at the next stage boundary. Cancelling
// an unknown job is not an error: it may have just finished.
func (r *XcodeJobRunner) Cancel(jobID string) error {
	r.mu.Lock()
	j, ok := r.jobs[jobID]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	j.cancel()
	return nil
}

// StopAll cancels every running install (host shutdown).
func (r *XcodeJobRunner) StopAll() {
	r.mu.Lock()
	jobs := make([]*xcodeJob, 0, len(r.jobs))
	for _, j := range r.jobs {
		jobs = append(jobs, j)
	}
	r.mu.Unlock()
	for _, j := range jobs {
		j.cancel()
	}
}

// HandleHostToolJobFrame routes a NodeHostToolJob or NodeHostToolJobCancel
// downstream frame onto the runner and acks. No-op for any other frame,
// returning false so the caller's own switch keeps going. Mounting it is one
// line (nil-safe, same shape as build.Runner.HandleBuildFrame):
//
//	if h.xcode.HandleHostToolJobFrame(ctx, c, frame) {
//		return
//	}
func (r *XcodeJobRunner) HandleHostToolJobFrame(ctx context.Context, c *Client, frame *agentcomposev2.NodeDownstreamFrame) bool {
	if r == nil {
		return false
	}
	frameID := frame.GetServerFrameId()
	switch payload := frame.GetFrame().(type) {
	case *agentcomposev2.NodeDownstreamFrame_HostToolJob:
		// Start is non-blocking: it registers the job and returns. Progress and
		// the terminal result stream back as their own upstream frames, so the
		// ack means "accepted", never "installed".
		err := r.Start(ctx, payload.HostToolJob)
		c.SendAck(frameID, err, nil)
		return true
	case *agentcomposev2.NodeDownstreamFrame_HostToolJobCancel:
		c.SendAck(frameID, r.Cancel(payload.HostToolJobCancel.GetJobId()), nil)
		return true
	}
	return false
}

// xcodeOutcome carries what the terminal result frame needs.
type xcodeOutcome struct {
	stage     string // stage reached ("checking"/"downloading"/…/"completed")
	errCode   string
	err       error
	retryable bool
	version   string // full `xcodebuild -version` output on success
	appPath   string // where the .app landed
	logTail   string // bounded, redacted output of the failed stage
}

// runJob drives the pipeline and returns the outcome. The work directory is a
// temp dir removed when the job ends — the .xip and the extracted app are huge
// and have no reuse across runs.
func (r *XcodeJobRunner) runJob(ctx context.Context, j *xcodeJob, req *agentcomposev2.NodeHostToolJob) xcodeOutcome {
	stage := func(s, msg string, pct int) { r.event(j, s, msg, pct, "") }

	stage("checking", "检查主机条件", 2)
	goos := r.goos
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos != "darwin" {
		return xcodeOutcome{stage: "checking", errCode: "not_darwin", err: errors.New("Xcode 只能安装在 macOS 主机上")}
	}

	// Already satisfied? Same contract as the sync detect command
	// (installXcodeDetection): a bare probe first (the fast path when the dev
	// dir is already selected), then — because a box can have Xcode installed
	// without xcode-select pointing at it — discover + activate + re-probe.
	// Without the second step an installed-but-unselected Xcode fell through
	// to the multi-GB download + extract for no reason (the「已装 Xcode 还是
	// 触发下载解压」symptom). Comparing major.minor: an installed version at
	// or above the target means nothing to download.
	cur := r.detectInstalledXcode(ctx)
	if cur != "" && satisfiesXcodeTarget(cur, req.GetTargetVersion()) {
		r.logger.Info("xcode-job: target already satisfied", "job_id", j.id, "current", firstXcodeLine(cur), "target", req.GetTargetVersion())
		return xcodeOutcome{stage: "checking", version: cur}
	}

	workDir, err := os.MkdirTemp("", "xcode-install-"+sanitizeXcodeJobID(j.id)+"-")
	if err != nil {
		return xcodeOutcome{stage: "checking", errCode: "workdir_failed", err: err}
	}
	defer func() {
		if err := os.RemoveAll(workDir); err != nil {
			r.logger.Warn("xcode-job: work dir cleanup failed", "job_id", j.id, "error", err)
		}
	}()

	// Disk pre-check: xip + extracted copy + rename headroom ≈ 3× the archive
	// plus slack. Best-effort — a statfs failure just skips the check.
	probeFree := r.freeFn
	if probeFree == nil {
		probeFree = freeDiskBytes
	}
	if want := req.GetDownloadSizeBytes(); want > 0 {
		if free, ferr := probeFree(workDir); ferr == nil && free < want*3+(10<<30) {
			need := want*3 + (10 << 30)
			return xcodeOutcome{stage: "checking", errCode: "insufficient_space",
				err: fmt.Errorf("磁盘空间不足：需要约 %s（下载+解压+移动），可用 %s", formatBytes(need), formatBytes(free))}
		}
	}

	// ── download → verify → extract ──────────────────────────────────────
	// One attempt = a fresh subdirectory (so a half-extracted .app from a
	// failed attempt can never be picked up by the next one's
	// extractedXcodeApp scan) holding the .xip and the expansion.
	// A corrupted .xip is only detectable at extraction — Apple's signature
	// check lives inside `xip -x` — and 12GB downloads through egress
	// proxies get truncated or rewritten disturbingly often, so a transient
	// corruption is retried automatically instead of failing a
	// 30+ minute job on the first bad copy.
	const maxAttempts = 3
	var appDir string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return xcodeOutcome{stage: "downloading", errCode: "cancelled", err: err}
		}
		attemptDir := filepath.Join(workDir, fmt.Sprintf("try%d", attempt))
		if err := os.MkdirAll(attemptDir, 0o755); err != nil {
			return xcodeOutcome{stage: "checking", errCode: "workdir_failed", err: err}
		}
		xipPath := filepath.Join(attemptDir, "Xcode.xip")

		if attempt > 1 {
			// The previous attempt's dir is dropped before the next download
			// so disk usage stays at ~one attempt's peak, not a growing pile.
			_ = os.RemoveAll(filepath.Join(workDir, fmt.Sprintf("try%d", attempt-1)))
			r.event(j, "downloading",
				fmt.Sprintf("上次下载的 .xip 未通过校验/解压，自动重试（第 %d/%d 次）", attempt, maxAttempts), 5, "")
		}

		r.event(j, "downloading", fmt.Sprintf("开始下载 Xcode %s（%s）", req.GetTargetVersion(), formatBytes(req.GetDownloadSizeBytes())), 5, "")
		spec := proxySpec{
			mode:      strings.TrimSpace(req.GetProxyMode()),
			url:       strings.TrimSpace(req.GetProxyUrl()),
			urlPrefix: strings.TrimSpace(req.GetProxyUrlPrefix()),
		}
		// Per-frame proxy wins; an unset frame falls back to the persisted node
		// egress-proxy snapshot (same rule as installHostNodeJS).
		if spec.mode == "" && spec.url == "" && spec.urlPrefix == "" && r.proxy != nil {
			spec = r.proxy()
		}
		dl := r.downloadFn
		if dl == nil {
			dl = r.downloadXip
		}
		if err := dl(ctx, j, req.GetDownloadUrl(), xipPath, spec, req.GetDownloadSizeBytes()); err != nil {
			if ctx.Err() != nil {
				return xcodeOutcome{stage: "downloading", errCode: "cancelled", err: ctx.Err()}
			}
			return xcodeOutcome{stage: "downloading", errCode: "download_failed", err: err, retryable: true}
		}

		// ── verify ───────────────────────────────────────────────────────
		// No explicit checksum is normal for xcodereleases: the .xip carries
		// Apple's own signature, which `xip -x` validates during extraction.
		// What we CAN check up front: exact size (a truncated stream is the
		// #1 corruption) and the XAR magic (an error page from a proxy that
		// still answers 200 would otherwise waste a 30-minute extract).
		stage("verifying", "校验下载产物", 72)
		if want := strings.TrimSpace(req.GetSha256()); want != "" {
			if err := verifySHA256(xipPath, want); err != nil {
				if attempt < maxAttempts && ctx.Err() == nil {
					continue
				}
				return xcodeOutcome{stage: "verifying", errCode: "download_failed", err: err, retryable: true}
			}
		} else if want := req.GetDownloadSizeBytes(); want > 0 {
			if st, serr := os.Stat(xipPath); serr == nil && st.Size() != want {
				if attempt < maxAttempts && ctx.Err() == nil {
					continue
				}
				return xcodeOutcome{stage: "verifying", errCode: "download_failed", retryable: true,
					err: fmt.Errorf("下载文件大小不符：得到 %s，期望 %s", formatBytes(st.Size()), formatBytes(want))}
			}
		}
		if err := checkXipMagic(xipPath); err != nil {
			if attempt < maxAttempts && ctx.Err() == nil {
				continue
			}
			return xcodeOutcome{stage: "verifying", errCode: "download_failed", err: err, retryable: true}
		}

		// ── extract ──────────────────────────────────────────────────────
		stage("extracting", "解压 .xip（含 Apple 签名校验，通常需要 10-40 分钟）", 75)
		out, err := r.execHeartbeat(ctx, j, attemptDir, "xip", []string{"-x", xipPath},
			60*time.Second, "extracting", "仍在解压", 78)
		if err == nil {
			appDir, err = extractedXcodeApp(attemptDir)
			if err == nil {
				break // success — keep this attempt dir; the move stage consumes it
			}
		}
		if ctx.Err() != nil {
			return xcodeOutcome{stage: "extracting", errCode: "cancelled", err: ctx.Err()}
		}
		if attempt < maxAttempts {
			continue
		}
		// Final attempt failed. The xip's stderr (the "damaged archive" line)
		// is the operator's real diagnosis; fold guidance in too — a proxy
		// that corrupts once usually corrupts again, so the actionable next
		// step is changing the download route, not just clicking retry.
		errMsg := "解压失败（.xip 未通过 Apple 签名校验或损坏）。已自动重试 3 次仍失败：出口代理/镜像在传输大文件时截断或改写是最常见原因，请为该节点更换或关闭下载代理后重试；若本机可手动安装 Xcode（App Store 或手动解压 .xip 到 /Applications），完成后点「检测」即可识别。"
		logTail := xcodeLogTail(out)
		var extractErr error
		if err != nil {
			extractErr = fmt.Errorf("%w", err)
		} else {
			extractErr = errors.New(errMsg)
		}
		return xcodeOutcome{
			stage: "extracting", errCode: "extract_failed", err: fmt.Errorf("%s\n%s", errMsg, extractErr), logTail: logTail,
		}
	}

	// ── move ─────────────────────────────────────────────────────────────
	// Standard location + standard name (/Applications/Xcode.app): everything
	// downstream — xcode-select, the register-time probe, other Apple tools —
	// assumes exactly this, so a freshly installed Xcode is recognized without
	// any discovery fallback. A versioned name (Xcode-16.2.app) is NOT used:
	// xcode-select would work, but the /usr/bin/xcodebuild shim and mdfind
	// would not point at it, and side-by-side installs are the operator's
	// choice, not the auto-installer's.
	dest := "/Applications/Xcode.app"
	stage("moving", "移动到 "+dest, 92)
	moveApp := r.moveAppFn
	if moveApp == nil {
		moveApp = moveXcodeApp
	}
	dest, err = moveApp(appDir, dest)
	if err != nil {
		if ctx.Err() != nil {
			return xcodeOutcome{stage: "moving", errCode: "cancelled", err: ctx.Err()}
		}
		return xcodeOutcome{stage: "moving", errCode: "move_failed", err: err}
	}

	// ── activate ─────────────────────────────────────────────────────────
	devDir := filepath.Join(dest, "Contents", "Developer")
	if _, err := os.Stat(filepath.Join(devDir, "usr", "bin", "xcodebuild")); err != nil {
		return xcodeOutcome{stage: "activating", errCode: "extract_failed",
			err: fmt.Errorf("解压出的 %s 不含工具链（xcodebuild 缺失）", dest)}
	}
	stage("activating", "激活 Xcode（xcode-select / DEVELOPER_DIR）", 95)
	mode := "xcode-select"
	// Non-interactive sudo first: succeeds unattended when the node runs as
	// root or has passwordless sudo; fails fast otherwise (never hangs on a
	// password prompt) and the rootless fallback below takes over.
	if _, serr := r.exec(ctx, "", "sudo", []string{"-n", "xcode-select", "-s", devDir}); serr != nil {
		mode = activateDeveloperDir(devDir, r.logger)
	}
	r.event(j, "activating", "已通过 "+mode+" 激活 "+devDir, 96, "")

	if out, lerr := r.exec(ctx, "", "xcodebuild", []string{"-runFirstLaunch"}); lerr != nil {
		// Best-effort: component installation may need an interactive sudo; the
		// probe below still decides success and the message names the manual step.
		r.event(j, "activating", "首次启动组件安装未完成（可在主机手动执行 sudo xcodebuild -runFirstLaunch），继续探测", 96, xcodeLogTail(out))
	} else {
		r.event(j, "activating", "首次启动完成", 96, "")
	}
	acceptXcodeLicenseBestEffort(ctx, r.logger)

	// ── probe ────────────────────────────────────────────────────────────
	stage("probing", "探测 xcodebuild 版本", 98)
	ver := r.probeXcodeVersion(ctx)
	if ver == "" {
		return xcodeOutcome{stage: "probing", errCode: "probe_failed", appPath: dest,
			err: errors.New("安装后 xcodebuild 仍无法运行：可能需要 sudo xcodebuild -license accept 或完成首次启动")}
	}
	return xcodeOutcome{stage: "completed", version: ver, appPath: dest}
}

// finish emits the terminal event + the single result frame.
func (r *XcodeJobRunner) finish(j *xcodeJob, out xcodeOutcome) {
	ok := out.err == nil && out.errCode == ""
	msg := "Xcode 安装完成：" + firstXcodeLine(out.version)
	if !ok {
		msg = "Xcode 安装失败：" + out.errCode
	}
	r.event(j, out.stage, msg, ternaryInt(ok, 100, 0), out.logTail)
	r.logger.Info("xcode-job finished", "job_id", j.id, "ok", ok, "stage", out.stage, "error_code", out.errCode)

	res := &agentcomposev2.NodeHostToolJobResult{
		JobId:            j.id,
		Ok:               ok,
		ErrorCode:        out.errCode,
		Retryable:        out.retryable,
		StageReached:     out.stage,
		XcodebuildVersion: out.version,
		AppPath:          out.appPath,
	}
	if out.err != nil {
		// Fold the failed stage's stderr into the error message: the exec
		// error alone ("exit status 1") tells the operator nothing, while the
		// logTail carries xip's actual complaint (disk full, signature
		// mismatch, unsupported macOS…). The tail is what the UI surfaces.
		msg := redactXcodeErr(out.err)
		if tail := strings.TrimSpace(out.logTail); tail != "" {
			msg = strings.TrimSpace(msg + "\n" + tail)
		}
		res.ErrorMessage = msg
	}
	if err := r.emit(&agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_HostToolJobResult{HostToolJobResult: res},
	}); err != nil {
		r.logger.Warn("xcode-job: result not sent", "job_id", j.id, "error", err)
	}
}

// event emits one progress frame with a monotonic sequence number.
func (r *XcodeJobRunner) event(j *xcodeJob, stage, msg string, percent int, logTail string) {
	r.eventBytes(j, stage, msg, percent, logTail, 0, 0)
}

// eventBytes is event with download byte counters (the big-file progress
// denominator the frontend renders as "1.2 GB / 12 GB").
func (r *XcodeJobRunner) eventBytes(j *xcodeJob, stage, msg string, percent int, logTail string, cur, total int64) {
	r.mu.Lock()
	j.seq++
	seq := j.seq
	r.mu.Unlock()

	ev := &agentcomposev2.NodeHostToolJobEvent{
		JobId:        j.id,
		Seq:          seq,
		Stage:        stage,
		Message:      msg,
		Percent:      int32(percent),
		ReportedAt:   time.Now().UTC().Format(time.RFC3339),
		LogTail:      logTail,
		CurrentBytes: cur,
		TotalBytes:   total,
	}
	if err := r.emit(&agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_HostToolJobEvent{HostToolJobEvent: ev},
	}); err != nil {
		r.logger.Debug("xcode-job: event not sent", "job_id", j.id, "stage", stage, "error", err)
	}
}

// xarMagic is the 4-byte header every XAR archive (and thus every .xip) must
// start with. A "200 OK" error page from a broken egress proxy is the
// classic false-positive download that would otherwise waste a full
// extraction cycle before failing Apple's signature check.
var xarMagic = []byte("xar!")

// checkXipMagic reads the first bytes of the downloaded file and rejects
// anything that is not a XAR archive. Cheap (4 bytes) and catches the
// proxy-served-HTML-error-page corruption class instantly.
func checkXipMagic(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("校验 .xip 失败: %w", err)
	}
	defer f.Close()
	head := make([]byte, len(xarMagic))
	if _, err := io.ReadFull(f, head); err != nil {
		return fmt.Errorf("下载产物过小，不是完整的 .xip: %w", err)
	}
	if !bytes.Equal(head, xarMagic) {
		return fmt.Errorf("下载产物不是 .xip 归档（开头魔数不符——出口代理可能返回了错误页），开头 %q", head)
	}
	return nil
}

// downloadXip fetches the .xip into dest (via a .part file renamed on success,
// so a partial download never looks complete) with byte-counted progress:
// an event every 32 MiB or every 5s, whichever comes first — fast links hit
// the size rule, slow links keep the UI alive via the time rule.
func (r *XcodeJobRunner) downloadXip(ctx context.Context, j *xcodeJob, url, dest string, spec proxySpec, total int64) error {
	finalURL := resolveDownloadURL(spec, url)
	client, err := httpClientForProxy(spec)
	if err != nil {
		return fmt.Errorf("build http client: %w", err)
	}
	if client == http.DefaultClient {
		client = &http.Client{} // never mutate the shared default's Timeout
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finalURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", finalURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", finalURL, resp.StatusCode)
	}
	if resp.ContentLength > 0 && total <= 0 {
		total = resp.ContentLength
	}

	part := dest + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", part, err)
	}
	var n, lastSizeEmit int64
	lastTimeEmit := time.Now()
	buf := make([]byte, 1<<20)
	for {
		if ctx.Err() != nil {
			f.Close()
			_ = os.Remove(part)
			return ctx.Err()
		}
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			nw, ew := f.Write(buf[:nr])
			if ew != nil {
				f.Close()
				_ = os.Remove(part)
				return fmt.Errorf("write %s: %w", part, ew)
			}
			n += int64(nw)
			if n-lastSizeEmit >= 32<<20 || time.Since(lastTimeEmit) >= 5*time.Second {
				lastSizeEmit = n
				lastTimeEmit = time.Now()
				r.downloadProgress(j, n, total)
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			f.Close()
			_ = os.Remove(part)
			return fmt.Errorf("download %s: %w", finalURL, er)
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("close %s: %w", part, err)
	}
	// Truncation guard: when the expected size is known (catalog size or the
	// response's own Content-Length), a clean EOF before that many bytes means
	// the stream was cut mid-transfer (proxy drop / worker timeout) — the old
	// loop treated EOF as success and the short file only surfaced as a
	// "damaged archive" 30 minutes later inside xip.
	if total > 0 && n != total {
		_ = os.Remove(part)
		return fmt.Errorf("下载被截断：得到 %s / 期望 %s", formatBytes(n), formatBytes(total))
	}
	if err := os.Rename(part, dest); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("finalize %s: %w", dest, err)
	}
	r.downloadProgress(j, n, total)
	return nil
}

func (r *XcodeJobRunner) downloadProgress(j *xcodeJob, cur, total int64) {
	pct := 5
	msg := fmt.Sprintf("已下载 %s", formatBytes(cur))
	if total > 0 {
		frac := 65 * cur / total
		if frac < 0 {
			frac = 0
		}
		if frac > 65 {
			frac = 65
		}
		pct = 5 + int(frac)
		msg = fmt.Sprintf("已下载 %s / %s（%d%%）", formatBytes(cur), formatBytes(total), pct-5)
	}
	r.eventBytes(j, "downloading", msg, pct, "", cur, total)
}

// execHeartbeat runs one command while emitting a heartbeat event every
// `every` — xip -x prints nothing for tens of minutes, and a silent stage
// looks hung from the UI. The command itself is ctx-aware (killed on cancel).
func (r *XcodeJobRunner) execHeartbeat(ctx context.Context, j *xcodeJob, dir, name string, args []string, every time.Duration, stage, msg string, pct int) ([]byte, error) {
	type result struct {
		out []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := r.exec(ctx, dir, name, args)
		ch <- result{out, err}
	}()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case res := <-ch:
			return res.out, res.err
		case <-ticker.C:
			minutes := int(time.Since(start).Minutes()) + 1
			r.event(j, stage, fmt.Sprintf("%s，已耗时 %d 分钟", msg, minutes), pct, "")
		}
	}
}

// exec runs one command in dir, capturing combined output for the tail. The
// command inherits the node's environment (xcodebuild needs the developer
// dir); no sandbox — a node host is trusted, same trust level as host_exec.
func (r *XcodeJobRunner) exec(ctx context.Context, dir, name string, args []string) ([]byte, error) {
	if r.execFn != nil {
		return r.execFn(ctx, dir, name, args)
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// probeXcodeVersion is the register-time probe (probeHostTool) routed through
// the exec seam so tests can fake it.
func (r *XcodeJobRunner) probeXcodeVersion(ctx context.Context) string {
	out, err := r.exec(ctx, "", "xcodebuild", []string{"-version"})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// detectInstalledXcode mirrors the sync detect command (installXcodeDetection):
// a bare xcodebuild probe first (fast path when the dev dir is already
// selected), then discover + activate + re-probe for an installed-but-
// unselected Xcode. Returns the full `xcodebuild -version` output, or "" when
// no Xcode on the host could be made to run. Activation is a deliberate side
// effect — it leaves DEVELOPER_DIR (or xcode-select) pointing at a working
// Xcode so both this job's later probes and every future session work without
// anyone re-clicking detect.
func (r *XcodeJobRunner) detectInstalledXcode(ctx context.Context) string {
	if v := r.probeXcodeVersion(ctx); v != "" {
		return v
	}
	discover := r.discoverAppsFn
	if discover == nil {
		discover = discoverXcodeApps
	}
	activate := r.activateDevDirFn
	if activate == nil {
		activate = func(devDir string) string {
			return activateDeveloperDir(devDir, r.logger)
		}
	}
	for _, app := range discover() {
		devDir := filepath.Join(app, "Contents", "Developer")
		// A real Xcode carries its own xcodebuild inside the app bundle; the
		// /usr/bin shim alone proves nothing (it exists on CLT-only boxes too).
		if _, err := os.Stat(filepath.Join(devDir, "usr", "bin", "xcodebuild")); err != nil {
			r.logger.Debug("xcode-job: skipping xcode candidate without toolchain", "app", app)
			continue
		}
		mode := activate(devDir)
		if v := r.probeXcodeVersion(ctx); v != "" {
			r.logger.Info("xcode-job: installed Xcode activated", "app", app, "activation", mode, "version", firstXcodeLine(v))
			return v
		}
	}
	return ""
}

// extractedXcodeApp finds the .app the xip produced in workDir — exactly one
// is expected; the preference filter breaks ties the same way detection does.
func extractedXcodeApp(workDir string) (string, error) {
	candidates := filterXcodeCandidates([]string{workDir})
	if len(candidates) == 0 {
		return "", errors.New(".xip 解压完成但未在产物中找到 Xcode.app")
	}
	return candidates[0], nil
}

// moveXcodeApp renames the extracted app onto dest (the standard
// /Applications/Xcode.app). No user-level fallback: ~/Applications is a
// non-standard home the /usr/bin/xcodebuild shim and Spotlight do not
// recognize — silently landing there is exactly how「已装 Xcode 识别不出来」
// happens. A rename failure surfaces as an error with the actionable hint
// instead (the operator moves it by hand or grants write access, then
// re-runs; a pre-existing install is never overwritten — remove it first).
func moveXcodeApp(appDir, dest string) (string, error) {
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("已存在同名 Xcode：%s（本次不会覆盖，请先移除旧版后重试）", dest)
	}
	if err := os.Rename(appDir, dest); err != nil {
		return "", fmt.Errorf(
			"移动 Xcode.app 到 %s 失败: %w（常见原因：节点非 root 且 /Applications 不可写。"+
				"可手动执行 sudo mv %q %q，或在节点详情页用 sudo 启动节点后重试）",
			dest, err, appDir, dest)
	}
	return dest, nil
}

// satisfiesXcodeTarget reports whether the installed version (full
// `xcodebuild -version` output) already meets the requested one
// (major.minor compare; "16.0 beta 2"-style targets compare as 16.0). When
// either side fails to parse it returns false — unparseable means install.
func satisfiesXcodeTarget(current, target string) bool {
	cmaj, cmin, cok := xcodeVersionParts(firstXcodeLine(current))
	tmaj, tmin, tok := xcodeVersionParts(target)
	if !cok || !tok {
		return false
	}
	return cmaj > tmaj || (cmaj == tmaj && cmin >= tmin)
}

// xcodeVersionParts parses the leading major.minor out of s ("Xcode 15.2",
// "15.2", "16.0 beta 2" → 15/2, 15/2, 16/0).
func xcodeVersionParts(s string) (int, int, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "Xcode"))
	var b strings.Builder
	dot := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '.' && !dot && b.Len() > 0:
			dot = true
			b.WriteByte(c)
		default:
			i = len(s) // stop scanning at the first non-version byte
		}
	}
	parts := strings.Split(strings.Trim(b.String(), "."), ".")
	if len(parts) == 0 || parts[0] == "" {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	return major, minor, true
}

// versionSlug reduces a target version to the filename-safe numeric run:
// "15.2" → "15.2", "16" → "16", "16.0 beta 2" → "16.0".
func versionSlug(target string) string {
	target = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(target), "Xcode"))
	var b strings.Builder
	dot := false
	for i := 0; i < len(target); i++ {
		c := target[i]
		switch {
		case c >= '0' && c <= '9':
			b.WriteByte(c)
		case c == '.' && !dot && b.Len() > 0:
			dot = true
			b.WriteByte(c)
		default:
			i = len(target) // stop scanning at the first non-version byte
		}
	}
	if slug := strings.Trim(b.String(), "."); slug != "" {
		return slug
	}
	return "Xcode"
}

func firstXcodeLine(out string) string {
	out = strings.TrimSpace(out)
	if i := strings.IndexAny(out, "\r\n"); i >= 0 {
		out = out[:i]
	}
	return out
}

func xcodeLogTail(out []byte) string {
	return redactXcodeSecrets(xcodeTail(string(out), xcodeLogTailBytes))
}

func redactXcodeErr(err error) string {
	if err == nil {
		return ""
	}
	return redactXcodeSecrets(err.Error())
}

func redactXcodeSecrets(s string) string {
	const marker = "-----BEGIN"
	if i := strings.Index(s, marker); i >= 0 {
		s = s[:i] + "[redacted key material]"
	}
	return s
}

// xcodeTail returns at most max bytes from the END of s, keeping whole lines
// when the cut would split one (same shape as the build runner's tail).
func xcodeTail(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := s[len(s)-max:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[i+1:]
	}
	return "…" + cut
}

// sanitizeXcodeJobID keeps a job id usable as a temp-dir suffix.
func sanitizeXcodeJobID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 48 {
		out = out[len(out)-48:]
	}
	return out
}

// formatBytes renders a byte count for a user-visible message ("12.3 GB").
func formatBytes(n int64) string {
	const unit = 1 << 30
	switch {
	case n >= unit:
		return fmt.Sprintf("%.1f GB", float64(n)/unit)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func ternaryInt(cond bool, a, b int) int {
	if cond {
		return a
	}
	return b
}
