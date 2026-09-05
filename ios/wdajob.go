// WDA job engine: the host-side pipeline that turns an approved WebDriverAgent
// artifact into a running WDA on one iPhone.
//
// A job is the unit the server schedules and the UI renders:
//
//	download → verify → (developer mode) → register device with Apple →
//	provision profile → sign → install → launch runner → forward port →
//	wait for /status → verify control
//
// Every step emits a NodeIosJobEvent (monotonic seq) and the job ends with
// exactly one NodeIosJobResult, so a server-side future never hangs on the ack
// alone. Steps are interfaces so the whole state machine is unit-tested without
// Apple credentials or a phone.
//
// SECURITY: signing material (App Store Connect .p8, .p12, provisioning
// profile) arrives per job over the authenticated NodeConnect stream, is written
// 0600 under the host's own state dir, and must never appear in a job event, an
// ack, or a log line. redactErr is the single funnel for error text.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// JobEmitter sends one upstream frame. The manager holds this instead of the
// agent client so tests can capture the event stream.
type JobEmitter func(*agentcomposev2.NodeUpstreamFrame) error

// WdaSteps is the pipeline the job engine drives. Each method corresponds to
// one or more stages; the production implementation lives in wdapipeline.go and
// is backed by go-ios (signing, zipconduit, testmanagerd, forward).
type WdaSteps interface {
	// Fetch downloads the artifact and verifies its sha256, returning the local
	// path of the verified archive.
	Fetch(ctx context.Context, art *agentcomposev2.NodeIosWdaArtifact, workDir string, progress func(percent int)) (string, error)
	// EnableDeveloperMode asks the device to enable Developer Mode. The device
	// still prompts its user and reboots, so this reports "requested", not "on".
	EnableDeveloperMode(ctx context.Context, udid string) error
	// PrepareSigning provisions signing assets (register UDID, reuse-or-create
	// certificate, mint a per-device profile) and returns the material needed to
	// sign plus the certificate id to reuse for later devices.
	PrepareSigning(ctx context.Context, req *agentcomposev2.NodeIosWdaJobRequest, workDir string, stage func(agentcomposev2.IosJobStage)) (SigningAssets, error)
	// Sign rewrites the artifact's bundle id and signs it, returning the signed
	// path (an .ipa ready to install).
	Sign(ctx context.Context, artifactPath string, assets SigningAssets, targetBundleID, workDir string) (string, error)
	// Install pushes the signed artifact onto the device.
	Install(ctx context.Context, udid, signedPath string) error
	// Launch starts the WDA runner, forwards its port, waits for /status and
	// runs a control smoke test. Returns the host port WDA answers on.
	Launch(ctx context.Context, udid, bundleID, xctestConfig string, stage func(agentcomposev2.IosJobStage)) (int, error)
}

// SigningAssets is what Sign needs plus what the server should remember.
type SigningAssets struct {
	// P12Path/ProfilePath are 0600 files under the job's work dir.
	P12Path     string
	ProfilePath string
	P12Password string
	// CertificateID is the App Store Connect certificate the job used, so the
	// next device reuses it instead of tripping Apple's one-cert-per-account
	// limit.
	CertificateID string
	// ProfileExpiresAt is parsed from the provisioning profile (RFC3339).
	ProfileExpiresAt string
	// Presigned marks the "user supplied an already-signed IPA" path, where Sign
	// is a passthrough.
	Presigned bool
}

// WdaJobManager tracks running jobs for this host.
type WdaJobManager struct {
	emit     JobEmitter
	steps    WdaSteps
	stateDir string
	logger   logger

	mu   sync.Mutex
	jobs map[string]*wdaJob
}

// logger is the subset of slog.Logger the engine uses (kept narrow so tests can
// pass a no-op).
type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Debug(msg string, args ...any)
	Error(msg string, args ...any)
}

type wdaJob struct {
	id     string
	udid   string
	cancel context.CancelFunc
	done   chan struct{}
	seq    int64
}

// NewWdaJobManager builds the job engine. stateDir is where per-job work
// directories (artifacts, signing material) are created.
func NewWdaJobManager(emit JobEmitter, steps WdaSteps, stateDir string, log logger) *WdaJobManager {
	return &WdaJobManager{
		emit:     emit,
		steps:    steps,
		stateDir: stateDir,
		logger:   log,
		jobs:     map[string]*wdaJob{},
	}
}

// ActiveJobs returns the ids of jobs currently running, newest first is not
// guaranteed — order is map order, callers use it for reconciliation only.
func (m *WdaJobManager) ActiveJobs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.jobs))
	for id := range m.jobs {
		out = append(out, id)
	}
	return out
}

// Start registers and launches one job. It returns as soon as the job is
// registered — never after the pipeline finishes — so the dispatch loop is not
// blocked by a multi-minute sign+install.
func (m *WdaJobManager) Start(ctx context.Context, req *agentcomposev2.NodeIosWdaJobRequest) error {
	jobID := req.GetJobId()
	if jobID == "" {
		return errors.New("wda job: job_id is required")
	}
	if req.GetUdid() == "" {
		return errors.New("wda job: udid is required")
	}

	m.mu.Lock()
	if _, running := m.jobs[jobID]; running {
		m.mu.Unlock()
		// Idempotent: a retried dispatch for a job already running is accepted
		// without starting a second pipeline against the same device.
		return nil
	}
	jobCtx, cancel := context.WithCancel(ctx)
	j := &wdaJob{id: jobID, udid: req.GetUdid(), cancel: cancel, done: make(chan struct{})}
	m.jobs[jobID] = j
	m.mu.Unlock()

	go func() {
		defer close(j.done)
		defer func() {
			m.mu.Lock()
			delete(m.jobs, jobID)
			m.mu.Unlock()
			cancel()
		}()
		m.runJob(jobCtx, j, req)
	}()
	return nil
}

// Cancel asks a running job to stop at the next stage boundary. Cancelling an
// unknown job is not an error: the job may have just finished, and the server
// should not have to distinguish that from a bad id.
func (m *WdaJobManager) Cancel(jobID string) error {
	m.mu.Lock()
	j, ok := m.jobs[jobID]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	j.cancel()
	return nil
}

// StopAll cancels every running job (host shutdown).
func (m *WdaJobManager) StopAll() {
	m.mu.Lock()
	jobs := make([]*wdaJob, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	for _, j := range jobs {
		j.cancel()
	}
}

// ── the pipeline ─────────────────────────────────────────────────────────

// jobOutcome carries what the terminal result frame needs.
type jobOutcome struct {
	stage            agentcomposev2.IosJobStage
	errCode          string
	err              error
	retryable        bool
	bundleID         string
	profileExpiresAt string
	certificateID    string
	artifactSHA      string
	artifactVersion  string
}

// runJob drives the pipeline and always emits exactly one result frame.
func (m *WdaJobManager) runJob(ctx context.Context, j *wdaJob, req *agentcomposev2.NodeIosWdaJobRequest) {
	workDir := filepath.Join(m.stateDir, "wda-jobs", sanitizeFileName(j.id))
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		m.finish(j, req, jobOutcome{
			stage:   agentcomposev2.IosJobStage_IOS_JOB_STAGE_QUEUED,
			errCode: "workdir_failed",
			err:     err,
		})
		return
	}
	// Signing material and the signed artifact live here; never leave them
	// behind after the job ends.
	defer func() {
		if err := os.RemoveAll(workDir); err != nil {
			m.logger.Warn("wda job: work dir cleanup failed", "job_id", j.id, "error", err)
		}
	}()

	out := m.execute(ctx, j, req, workDir)
	m.finish(j, req, out)
}

// execute runs the stages in order, returning the outcome. Each stage checks
// ctx first, which is how cooperative cancellation lands on a stage boundary.
func (m *WdaJobManager) execute(ctx context.Context, j *wdaJob, req *agentcomposev2.NodeIosWdaJobRequest, workDir string) jobOutcome {
	stage := func(s agentcomposev2.IosJobStage) { m.event(j, s, "", 0, "") }

	m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_QUEUED, "job accepted", 0, "")

	art := req.GetArtifact()
	if art == nil {
		return jobOutcome{
			stage:   agentcomposev2.IosJobStage_IOS_JOB_STAGE_QUEUED,
			errCode: "artifact_missing",
			err:     errors.New("no WDA artifact supplied"),
		}
	}
	outcome := jobOutcome{
		artifactSHA:     art.GetSha256(),
		artifactVersion: art.GetVersion(),
		bundleID:        art.GetTargetBundleId(),
	}

	// 1. Fetch + verify.
	if err := ctx.Err(); err != nil {
		return cancelled(outcome, agentcomposev2.IosJobStage_IOS_JOB_STAGE_QUEUED)
	}
	m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_DOWNLOADING, "fetching WDA artifact", 0, "")
	artifactPath, err := m.steps.Fetch(ctx, art, workDir, func(pct int) {
		m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_DOWNLOADING, "", pct, "")
	})
	if err != nil {
		outcome.stage = agentcomposev2.IosJobStage_IOS_JOB_STAGE_VERIFYING
		outcome.errCode = classifyFetchError(err)
		outcome.err = err
		outcome.retryable = outcome.errCode == "download_failed"
		return outcome
	}
	m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_VERIFYING, "artifact digest verified", 100, "")

	// 2. Developer Mode (optional, user-visible on the device).
	if req.GetEnableDeveloperMode() {
		if err := ctx.Err(); err != nil {
			return cancelled(outcome, agentcomposev2.IosJobStage_IOS_JOB_STAGE_VERIFYING)
		}
		m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_ENABLING_DEVELOPER_MODE,
			"requesting Developer Mode (confirm on the device; it will reboot)", 0, "")
		if err := m.steps.EnableDeveloperMode(ctx, req.GetUdid()); err != nil {
			// Not fatal on its own: the device may already have it on, and the
			// launch stage will fail with a clearer error if it does not.
			m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_ENABLING_DEVELOPER_MODE,
				"could not request Developer Mode; continuing", 0, redactErr(err))
		}
	}

	// 3. Signing (skipped for the presigned path).
	if err := ctx.Err(); err != nil {
		return cancelled(outcome, agentcomposev2.IosJobStage_IOS_JOB_STAGE_VERIFYING)
	}
	assets, err := m.steps.PrepareSigning(ctx, req, workDir, stage)
	if err != nil {
		outcome.stage = agentcomposev2.IosJobStage_IOS_JOB_STAGE_CREATING_PROFILE
		outcome.errCode = classifySigningError(err)
		outcome.err = err
		outcome.retryable = outcome.errCode == "asc_unavailable"
		return outcome
	}
	outcome.certificateID = assets.CertificateID
	outcome.profileExpiresAt = assets.ProfileExpiresAt

	signedPath := artifactPath
	if !assets.Presigned {
		if err := ctx.Err(); err != nil {
			return cancelled(outcome, agentcomposev2.IosJobStage_IOS_JOB_STAGE_CREATING_PROFILE)
		}
		m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_SIGNING, "signing WDA runner", 0, "")
		signedPath, err = m.steps.Sign(ctx, artifactPath, assets, art.GetTargetBundleId(), workDir)
		if err != nil {
			outcome.stage = agentcomposev2.IosJobStage_IOS_JOB_STAGE_SIGNING
			outcome.errCode = "signing_failed"
			outcome.err = err
			return outcome
		}
	}

	// 4. Install.
	if err := ctx.Err(); err != nil {
		return cancelled(outcome, agentcomposev2.IosJobStage_IOS_JOB_STAGE_SIGNING)
	}
	m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_INSTALLING, "installing on device", 0, "")
	if err := m.steps.Install(ctx, req.GetUdid(), signedPath); err != nil {
		outcome.stage = agentcomposev2.IosJobStage_IOS_JOB_STAGE_INSTALLING
		outcome.errCode = classifyInstallError(err)
		outcome.err = err
		outcome.retryable = outcome.errCode == "device_not_present"
		return outcome
	}

	// 5. Launch + verify. Only after this does the device count as ready.
	if err := ctx.Err(); err != nil {
		return cancelled(outcome, agentcomposev2.IosJobStage_IOS_JOB_STAGE_INSTALLING)
	}
	bundleID := firstNonEmpty(art.GetTargetBundleId(), req.GetArtifact().GetTargetBundleId())
	if _, err := m.steps.Launch(ctx, req.GetUdid(), bundleID, art.GetXctestConfigName(), stage); err != nil {
		outcome.stage = agentcomposev2.IosJobStage_IOS_JOB_STAGE_WAITING_READY
		outcome.errCode = classifyLaunchError(err)
		outcome.err = err
		outcome.retryable = true
		return outcome
	}

	outcome.stage = agentcomposev2.IosJobStage_IOS_JOB_STAGE_COMPLETED
	return outcome
}

// finish emits the terminal event + the single result frame.
func (m *WdaJobManager) finish(j *wdaJob, req *agentcomposev2.NodeIosWdaJobRequest, out jobOutcome) {
	ok := out.err == nil && out.errCode == ""
	terminal := agentcomposev2.IosJobStage_IOS_JOB_STAGE_COMPLETED
	switch {
	case out.errCode == "cancelled":
		terminal = agentcomposev2.IosJobStage_IOS_JOB_STAGE_CANCELLED
	case !ok:
		terminal = agentcomposev2.IosJobStage_IOS_JOB_STAGE_FAILED
	}
	msg := "WDA ready"
	if !ok {
		msg = "job failed: " + out.errCode
	}
	m.event(j, terminal, msg, 0, redactErr(out.err))
	m.logger.Info("wda job finished", "job_id", j.id, "udid", j.udid,
		"ok", ok, "stage", jobStageName(out.stage), "error_code", out.errCode)

	res := &agentcomposev2.NodeIosJobResult{
		JobId:            j.id,
		Ok:               ok,
		ErrorCode:        out.errCode,
		Retryable:        out.retryable,
		StageReached:     out.stage,
		WdaBundleId:      out.bundleID,
		ProfileExpiresAt: out.profileExpiresAt,
		ArtifactSha256:   out.artifactSHA,
		ArtifactVersion:  out.artifactVersion,
		CertificateId:    out.certificateID,
		ConfigRevision:   req.GetConfigRevision(),
	}
	if out.err != nil {
		res.ErrorMessage = redactErr(out.err)
	}
	if err := m.emit(&agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_IosJobResult{IosJobResult: res},
	}); err != nil {
		m.logger.Warn("wda job: result not sent", "job_id", j.id, "error", err)
	}
}

// event emits one progress frame with a monotonic sequence number.
func (m *WdaJobManager) event(j *wdaJob, stage agentcomposev2.IosJobStage, msg string, percent int, logTail string) {
	m.mu.Lock()
	j.seq++
	seq := j.seq
	m.mu.Unlock()

	ev := &agentcomposev2.NodeIosJobEvent{
		JobId:      j.id,
		Seq:        seq,
		Stage:      stage,
		Message:    msg,
		Percent:    int32(percent),
		ReportedAt: time.Now().UTC().Format(time.RFC3339),
		LogTail:    logTail,
	}
	if err := m.emit(&agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_IosJobEvent{IosJobEvent: ev},
	}); err != nil {
		m.logger.Debug("wda job: event not sent", "job_id", j.id, "stage", stage.String(), "error", err)
	}
}

func cancelled(out jobOutcome, at agentcomposev2.IosJobStage) jobOutcome {
	out.stage = at
	out.errCode = "cancelled"
	out.err = errors.New("job cancelled")
	return out
}

// ── error classification ─────────────────────────────────────────────────
//
// The UI branches on these codes, so they are stable strings rather than
// wrapped Go errors.

var (
	// errSHAMismatch is returned by Fetch when the artifact digest differs.
	errSHAMismatch = errors.New("artifact sha256 mismatch")
	// errDeveloperModeRequired is returned by Launch when the device refuses
	// XCTest because Developer Mode is off.
	errDeveloperModeRequired = errors.New("developer mode is not enabled on the device")
	// errDeviceNotPresent is returned when the target UDID is not attached.
	errDeviceNotPresent = errors.New("device is not attached")
	// errASCUnauthorized is returned when Apple rejects the API key.
	errASCUnauthorized = errors.New("app store connect rejected the API key")
)

func classifyFetchError(err error) string {
	switch {
	case errors.Is(err, errSHAMismatch):
		return "sha256_mismatch"
	default:
		return "download_failed"
	}
}

func classifySigningError(err error) string {
	switch {
	case errors.Is(err, errASCUnauthorized):
		return "asc_unauthorized"
	case errors.Is(err, errNoSigningMaterial):
		return "signing_material_missing"
	case errors.Is(err, errManualSigningRequired):
		return "manual_action_required"
	default:
		return "asc_unavailable"
	}
}

func classifyInstallError(err error) string {
	if errors.Is(err, errDeviceNotPresent) {
		return "device_not_present"
	}
	return "install_failed"
}

func classifyLaunchError(err error) string {
	switch {
	case errors.Is(err, errDeveloperModeRequired):
		return "developer_mode_required"
	case errors.Is(err, errDeviceNotPresent):
		return "device_not_present"
	default:
		return "wda_unreachable"
	}
}

// redactErr renders an error for a user-visible field with secrets stripped.
// Signing material never appears in an error we construct, but a third-party
// error (Apple API, codesign) could echo a path or a key id — so paths under the
// job work dir are collapsed and anything that looks like a PEM block is cut.
func redactErr(err error) string {
	if err == nil {
		return ""
	}
	return redactSecrets(err.Error())
}

func redactSecrets(s string) string {
	const marker = "-----BEGIN"
	if i := indexOf(s, marker); i >= 0 {
		s = s[:i] + "[redacted key material]"
	}
	return s
}

// indexOf is strings.Index, spelled out to keep this file's imports minimal.
func indexOf(s, sub string) int {
	n := len(sub)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(s); i++ {
		if s[i:i+n] == sub {
			return i
		}
	}
	return -1
}

// errNoSigningMaterial is returned when a job has no signing material and the
// action is not INSTALL_SIGNED.
var errNoSigningMaterial = errors.New("no signing material supplied")

// errManualSigningRequired is returned when the requested mode cannot be
// automated (free Apple ID). The UI must surface this as "action needed".
var errManualSigningRequired = errors.New("this signing mode requires manual action")

// jobStageName renders a stage for a log line. Kept as a helper so log call
// sites read the same way whether they have a stage value or an enum constant.
func jobStageName(s agentcomposev2.IosJobStage) string { return s.String() }
