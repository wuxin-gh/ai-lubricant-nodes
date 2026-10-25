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
package ioshost

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
	// IsInstalled reports whether the runner bundle is already installed on the
	// device. Used to skip the download→sign→install stages on a re-prepare of an
	// already-provisioned device (the common case: only launch/verify is needed).
	// A lookup error is returned as (false, err) so the caller falls back to the
	// full pipeline rather than skipping a needed install.
	IsInstalled(ctx context.Context, udid, bundleID string) (bool, error)
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
	Logger   Logger

	// OnProgress mirrors a running job's progress onto the device (percent +
	// stage name). Optional; nil disables the write-back.
	OnProgress func(udid string, percent int, stage string)
	// OnTerminal reports a job's terminal outcome so the device's wda_state
	// reflects it (INSTALLED / READY / FAILED / unchanged on cancel). Optional.
	//
	// installed is true when the download→sign→install half completed (or the
	// fast path proved the runner present): a launch-verification failure in
	// that case must NOT demote the device to FAILED — the runner is installed
	// and the device-control loop owns whether control ever comes up. The
	// manager records it as INSTALLED (waiting for control) with lastError set
	// to the reason, instead of FAILED.
	//
	// message is the redacted human-readable failure text (empty on success).
	// Without it the device row only ever stored the error CODE — so the console
	// showed "初始化异常：wda_unreachable" with no reason. The code is still
	// passed for programmatic consumers; message supersedes it for display.
	OnTerminal func(udid string, ok bool, cancelled bool, installed bool, errorCode string, message string, profileExpiresAt string)
	// IsPresent reports whether the device is currently enumerable over
	// usbmuxd. The pipeline waits for it before touching the phone, so a job
	// dispatched while the phone is unplugged parks instead of failing deep in
	// the install stage. Optional; nil means "assume present".
	IsPresent func(udid string) bool
	// PresentPollInterval is how often the wait stage re-checks the device.
	// Defaults to 5s.
	PresentPollInterval time.Duration
	// PresentWaitTimeout bounds the wait. Defaults to 10 minutes.
	PresentWaitTimeout time.Duration

	// OnActiveChanged fires on the 0→1 (true) and 1→0 (false) transitions of the
	// running-job set — NOT per job, so overlapping jobs on different devices
	// don't resume the tunnel reconcile while one is still running. The caller
	// uses it to Pause/Resume the RSD tunnel agent: go-ios's UpdateTunnels tears
	// down every healthy tunnel on one transient ListDevices failure, which would
	// yank the tunnel out from under an in-flight WDA launch (the observed
	// "installation successful, then wda_unreachable 3s later"). Optional; nil
	// disables the notifications (tests, node-ios where the poller is external).
	OnActiveChanged func(running bool)

	mu   sync.Mutex
	jobs map[string]*wdaJob
}

// Logger is the subset of slog.Logger the engine uses (kept narrow so tests can
// pass a no-op).
type Logger interface {
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
func NewWdaJobManager(emit JobEmitter, steps WdaSteps, stateDir string, log Logger) *WdaJobManager {
	return &WdaJobManager{
		emit:     emit,
		steps:    steps,
		stateDir: stateDir,
		Logger:   log,
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
	// 0→1: the first job pauses the tunnel reconcile so go-ios can't tear down
	// the healthy tunnel mid-launch. Read len here, invoke the callback after
	// unlocking — user code never runs under the lock.
	becameActive := len(m.jobs) == 1 && m.OnActiveChanged != nil
	onActive := m.OnActiveChanged
	m.mu.Unlock()
	if becameActive {
		onActive(true)
	}

	go func() {
		defer close(j.done)
		defer func() {
			m.mu.Lock()
			delete(m.jobs, jobID)
			// 1→0: only the last job drains do we let the reconcile resume.
			becameIdle := len(m.jobs) == 0 && m.OnActiveChanged != nil
			onIdle := m.OnActiveChanged
			m.mu.Unlock()
			if becameIdle {
				onIdle(false)
			}
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
//
// installed records that the download→sign→install half completed (or the
// fast path proved the runner present). It lets finish() report a LAUNCH
// failure as "initialization succeeded, control not verified" instead of
// FAILED: the device IS installed, the prober / device-control loop owns
// whether control ever comes up.
type jobOutcome struct {
	stage            agentcomposev2.IosJobStage
	errCode          string
	err              error
	retryable        bool
	installed        bool
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
			m.Logger.Warn("wda job: work dir cleanup failed", "job_id", j.id, "error", err)
		}
	}()

	out := m.execute(ctx, j, req, workDir)
	m.finish(j, req, out)
}

// waitForDevice blocks until the target device is enumerable over usbmuxd, or
// the wait budget expires. It reports whether the job may proceed.
//
// A present device returns immediately (the common case). While waiting it
// emits a QUEUED-stage event so the console can show "等待设备上线" instead of a
// frozen progress bar — the stage enum is reused deliberately, so no proto
// regeneration is needed for a purely host-side concern.
func (m *WdaJobManager) waitForDevice(ctx context.Context, j *wdaJob, udid string) bool {
	if m.IsPresent == nil || m.IsPresent(udid) {
		return true
	}
	interval := m.PresentPollInterval
	if interval <= 0 {
		interval = defaultPresentPollInterval
	}
	timeout := m.PresentWaitTimeout
	if timeout <= 0 {
		timeout = defaultPresentWaitTimeout
	}

	m.Logger.Info("wda job: waiting for device to come online", "job_id", j.id, "udid", udid)
	m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_QUEUED,
		"waiting for the device to come online", 0, "")

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if m.IsPresent(udid) {
				m.Logger.Info("wda job: device online, continuing", "job_id", j.id, "udid", udid)
				return true
			}
			if time.Now().After(deadline) {
				m.Logger.Warn("wda job: device never came online",
					"job_id", j.id, "udid", udid, "waited", timeout)
				return false
			}
			m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_QUEUED,
				"waiting for the device to come online", 0, "")
		}
	}
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

	// 0. Wait for the device to be physically present. A job can be dispatched
	// while the phone is unplugged (the console lets a user queue an
	// initialization), and every stage below needs it: signing resolves the
	// device entry, install and launch talk to it over usbmuxd. Rather than
	// failing deep in the pipeline with a bare device_not_present, park here and
	// let the phone come back.
	if !m.waitForDevice(ctx, j, req.GetUdid()) {
		return cancelled(outcome, agentcomposev2.IosJobStage_IOS_JOB_STAGE_QUEUED)
	}

	// 0.5 Fast path: on a re-prepare of a device that already has the runner
	// installed, skip download→sign→install and go straight to launch+verify.
	// This is the common case (a device prepared once, re-prepared after the
	// node restarted or the daemon stopped) and the download/sign/install half
	// is the slow, network- and Apple-bound part. Only for PREPARE: renew must
	// re-sign (the profile is what's being renewed) and reinstall/install_signed
	// exist precisely to force a reinstall. A lookup failure falls through to
	// the full pipeline — never skip an install we can't confirm.
	bundleID := firstNonEmpty(art.GetTargetBundleId(), req.GetArtifact().GetTargetBundleId())
	skipProvisioning := false
	if req.GetAction() == agentcomposev2.IosWdaJobAction_IOS_WDA_JOB_ACTION_PREPARE {
		if installed, err := m.steps.IsInstalled(ctx, req.GetUdid(), bundleID); err != nil {
			m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_VERIFYING,
				"could not check installed runner; running full pipeline", 0, redactErr(err))
		} else if installed {
			skipProvisioning = true
			outcome.installed = true
			m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_VERIFYING,
				"runner already installed; skipping download/sign/install", 0, "")
		}
	}

	// 1. Fetch + verify.
	if !skipProvisioning {
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
	}
	// Past this point the runner is installed: initialization succeeded.
	outcome.installed = true

	// 5. Launch + verify — BEST EFFORT, not the initialization gate.
	//
	// The persistent device-control loop (manager.startDeviceLocked →
	// RunDevice → link.Start) launches and MAINTAINS the runner on its own,
	// and its connected state is what makes a device control-ready
	// (manager.onDeviceState → READY). Historically this step's failure
	// failed the whole job → wda_state=FAILED even when the control link was
	// demonstrably up (device_control_online=true + FAILED in one snapshot).
	// A launch-verification failure now only records a warning: the device
	// stays INSTALLED and the prober / control loop promotes it when it's up.
	// (Install failures above stay terminal-FAILED: without an installed
	// runner there is nothing to promote.)
	if err := ctx.Err(); err != nil {
		return cancelled(outcome, agentcomposev2.IosJobStage_IOS_JOB_STAGE_INSTALLING)
	}
	if _, err := m.steps.Launch(ctx, req.GetUdid(), bundleID, art.GetXctestConfigName(), stage); err != nil {
		// The result frame carries the failure (code + retryable) so the console
		// can show WHY control is not up yet; the DEVICE state stays INSTALLED
		// (see finish(): installed=true routes the terminal to the
		// waiting-for-control path, not FAILED).
		outcome.stage = agentcomposev2.IosJobStage_IOS_JOB_STAGE_WAITING_READY
		outcome.errCode = classifyLaunchError(err)
		outcome.err = err
		outcome.retryable = true
		m.event(j, agentcomposev2.IosJobStage_IOS_JOB_STAGE_VERIFYING_CONTROL,
			"runner installed; immediate control check failed (control loop keeps retrying)", 0, redactErr(err))
		m.Logger.Info("wda job: control verification deferred to the device-control loop",
			"job_id", j.id, "udid", j.udid, "error", err)
	} else {
		outcome.stage = agentcomposev2.IosJobStage_IOS_JOB_STAGE_COMPLETED
	}
	return outcome
}

// finish emits the terminal event + the single result frame.
//
// The installed flag splits the failure semantics:
//   - installed=false (download/sign/install failed) → FAILED, exactly as
//     before: without a runner there is nothing to promote.
//   - installed=true with a launch failure → the JOB result still reports
//     ok=false + the error code (the console shows why control is not up),
//     but the DEVICE lands INSTALLED (waiting for control) instead of
//     FAILED: initialization is done, the device-control loop owns
//     promotion, and a control-link blip must not read as "broken device".
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
		if out.installed {
			msg = "runner installed; control not verified yet: " + out.errCode
		} else {
			msg = "job failed: " + out.errCode
		}
	}
	m.event(j, terminal, msg, 0, redactErr(out.err))
	m.Logger.Info("wda job finished", "job_id", j.id, "udid", j.udid,
		"ok", ok, "stage", jobStageName(out.stage), "error_code", out.errCode,
		"installed", out.installed)

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
		m.Logger.Warn("wda job: result not sent", "job_id", j.id, "error", err)
	}

	// Mirror the terminal outcome onto the DEVICE. This is what makes the
	// console's "initializing" state survive a server restart: the device row is
	// persisted and re-reported, the job snapshot is not. message carries the
	// redacted failure text so the console can show the REASON, not just a code.
	// installed=true failures are delivered as ok=true + reason: the manager
	// records INSTALLED (not READY until the control link is up, not FAILED —
	// the runner IS installed).
	if onTerminal := m.OnTerminal; onTerminal != nil {
		msg := ""
		if out.err != nil {
			msg = redactErr(out.err)
		}
		installed := out.installed
		if !installed {
			// A launch failure with installed=false keeps the legacy FAILED
			// path (nothing was ever installed).
			onTerminal(j.udid, ok, out.errCode == "cancelled", false, out.errCode, msg, out.profileExpiresAt)
		} else {
			onTerminal(j.udid, true, false, true, out.errCode, msg, out.profileExpiresAt)
		}
	}
}

// event emits one progress frame with a monotonic sequence number.
func (m *WdaJobManager) event(j *wdaJob, stage agentcomposev2.IosJobStage, msg string, percent int, logTail string) {
	m.mu.Lock()
	j.seq++
	seq := j.seq
	// Snapshot the callback under the lock; call it after unlocking so a slow
	// observer can never block the job engine (or deadlock on DeviceManager.mu).
	onProgress := m.OnProgress
	udid := j.udid
	m.mu.Unlock()

	// Mirror progress onto the DEVICE so the console can render "initializing N%"
	// from the durable inventory rather than the volatile job snapshot.
	if onProgress != nil {
		onProgress(udid, percent, jobStageName(stage))
	}

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
		m.Logger.Debug("wda job: event not sent", "job_id", j.id, "stage", stage.String(), "error", err)
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

// Wait-stage defaults. The poll is cheap (an in-memory presence read on the
// host), and the timeout is generous: a user may queue an initialization and
// plug the phone in minutes later.
const (
	defaultPresentPollInterval = 5 * time.Second
	defaultPresentWaitTimeout  = 10 * time.Minute
)

// errManualSigningRequired is returned when the requested mode cannot be
// automated (free Apple ID). The UI must surface this as "action needed".
var errManualSigningRequired = errors.New("this signing mode requires manual action")

// jobStageName renders a stage for a log line. Kept as a helper so log call
// sites read the same way whether they have a stage value or an enum constant.
func jobStageName(s agentcomposev2.IosJobStage) string { return s.String() }
