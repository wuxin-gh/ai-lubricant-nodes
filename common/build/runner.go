// Build runner: the node-side executor for server-rendered build jobs
// (project-page「构建」tab). The server decides WHAT to build — source URL,
// pinned ref, shell steps, artifact glob — and this runner only executes.
//
// A build is the unit the server schedules and the UI renders:
//
//	temp workspace → git clone (pinned ref) → exec steps in order →
//	glob artifact → sha256 → POST upload (one-time token) → result frame
//
// Every step emits a NodeBuildEvent (monotonic seq, bounded log tail) and the
// build ends with exactly one NodeBuildResult, so a server-side future never
// hangs on the ack alone. Steps run with the node's own environment; there is
// no sandbox — a build node is a trusted macOS host by definition, same trust
// level as host_exec.
//
// SECURITY: upload_token arrives per job over the authenticated NodeConnect
// stream and is only ever sent to upload_url the server chose. Step output is
// redacted (redactSecrets) and capped (logTailBytes) before it reaches an
// event, so a chatty build log cannot flood the control plane or leak key
// material echoed by a tool.
package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// UpstreamEmitter sends one upstream frame. Mirrors wdajob.JobEmitter so tests
// can capture the event stream without a client.
type UpstreamEmitter func(*agentcomposev2.NodeUpstreamFrame) error

// Runner tracks and executes build jobs for this host.
type Runner struct {
	emit   UpstreamEmitter
	logger Logger

	// execFn / uploadFn are production seams (wdajob.go's WdaSteps pattern):
	// tests inject fakes instead of requiring git/xcodebuild on the test host.
	execFn   func(ctx context.Context, dir, name string, args []string) ([]byte, error)
	uploadFn func(ctx context.Context, url, token, path, filename string) error

	mu   sync.Mutex
	jobs map[string]*buildJob
}

// Logger is the subset of slog.Logger the runner uses.
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
	Error(msg string, args ...any)
}

type buildJob struct {
	id     string
	cancel context.CancelFunc
	done   chan struct{}
	seq    int64
}

// logTailBytes caps the per-step output tail carried in an event frame. Build
// logs can be megabytes; only the last tail matters for a failed step, and the
// control plane must not carry unbounded payloads.
const logTailBytes = 4 << 10 // 4 KiB

// NewRunner builds the runner.
func NewRunner(emit UpstreamEmitter, log Logger) *Runner {
	return &Runner{emit: emit, logger: log, jobs: map[string]*buildJob{}}
}

// ActiveBuilds returns the ids of builds currently running (reconciliation on
// reconnect uses this indirectly via results; kept for symmetry with WDA jobs).
func (r *Runner) ActiveBuilds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.jobs))
	for id := range r.jobs {
		out = append(out, id)
	}
	return out
}

// Start registers and launches one build. Non-blocking: it returns as soon as
// the job is registered — never after the pipeline finishes. A retried dispatch
// for an already-running build is accepted idempotently.
func (r *Runner) Start(ctx context.Context, req *agentcomposev2.NodeBuildRequest) error {
	buildID := req.GetBuildId()
	if buildID == "" {
		return errors.New("build: build_id is required")
	}
	if req.GetSourceUrl() == "" {
		return errors.New("build: source_url is required")
	}
	if len(req.GetSteps()) == 0 {
		return errors.New("build: steps is required")
	}
	if req.GetUploadUrl() == "" || req.GetUploadToken() == "" {
		return errors.New("build: upload_url and upload_token are required")
	}

	r.mu.Lock()
	if _, running := r.jobs[buildID]; running {
		r.mu.Unlock()
		return nil
	}
	buildCtx, cancel := context.WithCancel(ctx)
	j := &buildJob{id: buildID, cancel: cancel, done: make(chan struct{})}
	r.jobs[buildID] = j
	r.mu.Unlock()

	timeout := time.Duration(req.GetTimeoutSeconds()) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	runCtx, cancelTimeout := context.WithTimeout(buildCtx, timeout)

	go func() {
		defer close(j.done)
		defer func() {
			r.mu.Lock()
			delete(r.jobs, buildID)
			r.mu.Unlock()
			cancelTimeout()
			cancel()
		}()
		out := r.runBuild(runCtx, j, req)
		r.finish(j, req, out)
	}()
	return nil
}

// Cancel asks a running build to stop at the next step boundary. Cancelling an
// unknown build is not an error: it may have just finished, and the server
// should not have to distinguish that from a bad id.
func (r *Runner) Cancel(buildID string) error {
	r.mu.Lock()
	j, ok := r.jobs[buildID]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	j.cancel()
	return nil
}

// StopAll cancels every running build (host shutdown).
func (r *Runner) StopAll() {
	r.mu.Lock()
	jobs := make([]*buildJob, 0, len(r.jobs))
	for _, j := range r.jobs {
		jobs = append(jobs, j)
	}
	r.mu.Unlock()
	for _, j := range jobs {
		j.cancel()
	}
}

// buildOutcome carries what the terminal result frame needs.
type buildOutcome struct {
	stage        string // free-form stage reached ("cloning"/"building"/"packaging"/"uploading")
	errCode      string
	err          error
	retryable    bool
	artifactSHA  string
	artifactSize int64
	logTail      string // bounded, redacted output of the failed step
}

// runBuild drives the pipeline and returns the outcome. The work directory is
// a temp dir removed when the build ends — build trees are large and have no
// reuse across runs (the server pins the ref per build).
func (r *Runner) runBuild(ctx context.Context, j *buildJob, req *agentcomposev2.NodeBuildRequest) buildOutcome {
	workDir, err := os.MkdirTemp("", "nodebuild-"+sanitizeFileName(j.id)+"-")
	if err != nil {
		return buildOutcome{stage: "cloning", errCode: "workdir_failed", err: err}
	}
	defer func() {
		if err := os.RemoveAll(workDir); err != nil {
			r.logger.Warn("build: work dir cleanup failed", "build_id", j.id, "error", err)
		}
	}()

	r.event(j, "queued", "build accepted", 0, "")

	// ── clone ────────────────────────────────────────────────────────────
	r.event(j, "cloning", "git clone "+req.GetSourceUrl()+" @ "+refLabel(req.GetSourceRef()), 5, "")
	cloneOut, err := r.exec(ctx, workDir, "git", []string{"clone", "--depth", "1", req.GetSourceUrl(), "src"})
	if err != nil {
		if ctx.Err() != nil {
			return buildOutcome{stage: "cloning", errCode: "cancelled", err: ctx.Err()}
		}
		return buildOutcome{stage: "cloning", errCode: "clone_failed", err: err, retryable: true, logTail: logTail(cloneOut)}
	}
	srcDir := filepath.Join(workDir, "src")
	ref := strings.TrimSpace(req.GetSourceRef())
	if ref != "" {
		// Pin after a shallow clone. A depth-1 clone of a non-branch ref
		// (commit sha) can miss the object; fall back to a full fetch then.
		checkoutOut, err := r.exec(ctx, srcDir, "git", []string{"checkout", ref})
		if err != nil {
			fetchOut, ferr := r.exec(ctx, srcDir, "git", []string{"fetch", "--unshallow", "origin"})
			if ferr != nil {
				return buildOutcome{stage: "cloning", errCode: "clone_failed", err: ferr, retryable: true, logTail: logTail(fetchOut)}
			}
			checkoutOut, err = r.exec(ctx, srcDir, "git", []string{"checkout", ref})
			if err != nil {
				return buildOutcome{stage: "cloning", errCode: "checkout_failed", err: err, retryable: true, logTail: logTail(checkoutOut)}
			}
		}
	}

	// ── steps ────────────────────────────────────────────────────────────
	total := len(req.GetSteps())
	for i, step := range req.GetSteps() {
		if err := ctx.Err(); err != nil {
			return buildOutcome{stage: "building", errCode: "cancelled", err: err}
		}
		percent := 10 + (90*i)/max(total, 1)
		r.event(j, "building", fmt.Sprintf("step %d/%d: %s", i+1, total, firstLine(step)), percent, "")
		out, err := r.exec(ctx, srcDir, "sh", []string{"-c", step})
		if err != nil {
			if ctx.Err() != nil {
				return buildOutcome{stage: "building", errCode: "cancelled", err: ctx.Err()}
			}
			return buildOutcome{stage: "building", errCode: "step_failed", err: err, logTail: logTail(out)}
		}
	}

	// ── package ──────────────────────────────────────────────────────────
	artifactPath, err := globOne(srcDir, req.GetArtifactGlob())
	if err != nil {
		return buildOutcome{stage: "packaging", errCode: "artifact_not_found", err: err}
	}
	sum, size, err := sha256File(artifactPath)
	if err != nil {
		return buildOutcome{stage: "packaging", errCode: "artifact_read_failed", err: err}
	}

	// ── upload ───────────────────────────────────────────────────────────
	r.event(j, "uploading", fmt.Sprintf("upload %s (%d bytes)", filepath.Base(artifactPath), size), 95, "")
	upload := uploadArtifact
	if r.uploadFn != nil {
		upload = r.uploadFn
	}
	if err := upload(ctx, req.GetUploadUrl(), req.GetUploadToken(), artifactPath, filepath.Base(artifactPath)); err != nil {
		if ctx.Err() != nil {
			return buildOutcome{stage: "uploading", errCode: "cancelled", err: ctx.Err()}
		}
		return buildOutcome{stage: "uploading", errCode: "upload_failed", err: err, retryable: true, artifactSHA: sum, artifactSize: size}
	}

	return buildOutcome{stage: "completed", artifactSHA: sum, artifactSize: size}
}

// finish emits the terminal event + the single result frame.
func (r *Runner) finish(j *buildJob, req *agentcomposev2.NodeBuildRequest, out buildOutcome) {
	ok := out.err == nil && out.errCode == ""
	msg := "build complete: " + filepath.Base(req.GetArtifactName())
	if !ok {
		msg = "build failed: " + out.errCode
	}
	r.event(j, out.stage, msg, ternary(ok, 100, 0), out.logTail)
	r.logger.Info("build finished", "build_id", j.id, "ok", ok, "stage", out.stage, "error_code", out.errCode)

	res := &agentcomposev2.NodeBuildResult{
		BuildId:           j.id,
		Ok:                ok,
		ErrorCode:         out.errCode,
		Retryable:         out.retryable,
		StageReached:      out.stage,
		ArtifactSha256:    out.artifactSHA,
		ArtifactSizeBytes: out.artifactSize,
	}
	if out.err != nil {
		res.ErrorMessage = redactErr(out.err)
	}
	if err := r.emit(&agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_NodeBuildResult{NodeBuildResult: res},
	}); err != nil {
		r.logger.Warn("build: result not sent", "build_id", j.id, "error", err)
	}
}

// event emits one progress frame with a monotonic sequence number.
func (r *Runner) event(j *buildJob, stage, msg string, percent int, logTail string) {
	r.mu.Lock()
	j.seq++
	seq := j.seq
	r.mu.Unlock()

	ev := &agentcomposev2.NodeBuildEvent{
		BuildId:    j.id,
		Seq:        seq,
		Stage:      stage,
		Message:    msg,
		Percent:    int32(percent),
		ReportedAt: time.Now().UTC().Format(time.RFC3339),
		LogTail:    logTail,
	}
	if err := r.emit(&agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_NodeBuildEvent{NodeBuildEvent: ev},
	}); err != nil {
		r.logger.Debug("build: event not sent", "build_id", j.id, "stage", stage, "error", err)
	}
}

// exec runs one command in dir, capturing combined output for the tail. The
// command inherits the node's environment (xcodebuild needs the developer dir;
// no sandbox — same trust level as host_exec).
func (r *Runner) exec(ctx context.Context, dir, name string, args []string) ([]byte, error) {
	if r.execFn != nil {
		return r.execFn(ctx, dir, name, args)
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// logTail trims command output to the bounded tail and redacts it.
func logTail(out []byte) string {
	return redactSecrets(tail(string(out), logTailBytes))
}

// redactErr renders an error for a user-visible field with secrets stripped.
func redactErr(err error) string {
	if err == nil {
		return ""
	}
	return redactSecrets(err.Error())
}

func redactSecrets(s string) string {
	const marker = "-----BEGIN"
	if i := strings.Index(s, marker); i >= 0 {
		s = s[:i] + "[redacted key material]"
	}
	return s
}

// tail returns at most max bytes from the END of s, keeping whole lines when
// the cut would split one.
func tail(s string, max int) string {
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

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func refLabel(ref string) string {
	if ref == "" {
		return "default branch"
	}
	return ref
}

// globOne resolves a glob relative to dir and requires exactly one match —
// ambiguity means the server rendered a bad artifact_glob, which must fail the
// build rather than silently upload the wrong file.
func globOne(dir, pattern string) (string, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", errors.New("artifact_glob is empty")
	}
	matches, err := filepath.Glob(filepath.Join(dir, filepath.FromSlash(pattern)))
	if err != nil {
		return "", fmt.Errorf("bad artifact_glob %q: %w", pattern, err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("artifact_glob %q matched nothing", pattern)
	}
	if len(matches) > 1 {
		names := make([]string, 0, 3)
		for i, m := range matches {
			if i == 3 {
				names = append(names, "…")
				break
			}
			names = append(names, filepath.Base(m))
		}
		return "", fmt.Errorf("artifact_glob %q matched %d files (%s)", pattern, len(matches), strings.Join(names, ", "))
	}
	info, err := os.Stat(matches[0])
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("artifact_glob %q matched a directory", pattern)
	}
	return matches[0], nil
}

func sha256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// sanitizeFileName keeps a build id usable as a temp-dir suffix.
func sanitizeFileName(s string) string {
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

func ternary(cond bool, a, b int) int {
	if cond {
		return a
	}
	return b
}
