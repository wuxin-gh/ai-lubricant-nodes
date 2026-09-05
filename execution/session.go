package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
	"ai-lubricant-nodes/common/workspaces"
)

// runtimeResultPrefix is the marker agent-compose-runtime prints before the
// one-shot prompt result JSON on its own final stdout line. The node parses it
// to fill NodeSessionResult.result_json. Kept in sync with the runtime's
// constants.ts RESULT_PREFIX.
const runtimeResultPrefix = "__AGENT_RESULT__"

// emitFunc sends an upstream frame on whatever stream is currently live. It
// returns errStreamGone when the connection is down; callers keep the payload
// queued and retry after reconnect.
type emitFunc func(*agentcomposev2.NodeUpstreamFrame) error

// sessionManager owns the sessions running on this node. Each session is a
// working directory plus (at most) one running provider process. The manager is
// the node-local authority: the server only issues create/delete/list and
// receives streamed output/results back.
type sessionManager struct {
	opts           sessionOptions
	logger         *slog.Logger
	emitOutput     emitFunc
	emitResult     emitFunc
	emitStructured emitFunc
	emitStage      emitFunc

	mu          sync.Mutex
	workspaceMu sync.Mutex
	sessions    map[string]*nodeSession
}

func newSessionManager(opts sessionOptions, logger *slog.Logger, emitOutput, emitResult, emitStructured, emitStage emitFunc) *sessionManager {
	return &sessionManager{
		opts:           opts,
		logger:         logger,
		emitOutput:     emitOutput,
		emitResult:     emitResult,
		emitStructured: emitStructured,
		emitStage:      emitStage,
		sessions:       map[string]*nodeSession{},
	}
}

// reportStage tells the server which preparation step a session just completed
// or failed on.
//
// Session bring-up used to be opaque: create() did workspace prep, git clone,
// executor selection and process spawn behind one call that returned a single
// error, so a failure could not be attributed to a step — "exit code 1" was the
// whole story. Emitting one frame per step means the task page can say *where*
// it broke ("拉取代码" vs "启动运行环境") instead of only that it did.
//
// Best-effort by design: a stage frame is diagnostics, so a dead stream is
// logged at debug and never fails the operation being reported on. detail/error
// go through RedactGitURL because a clone failure's text can carry the proxy
// token embedded in the remote URL's userinfo.
func (m *sessionManager) reportStage(
	sessionID string,
	stage agentcomposev2.SessionStage,
	ok bool,
	detail string,
	stageErr error,
) {
	if m.emitStage == nil {
		return
	}
	errText := ""
	if stageErr != nil {
		errText = workspaces.RedactGitURL(stageErr.Error())
	}
	frame := &agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_SessionStage{
			SessionStage: &agentcomposev2.NodeSessionStage{
				SessionId: sessionID,
				Stage:     stage,
				Ok:        ok,
				Detail:    workspaces.RedactGitURL(detail),
				Error:     errText,
				CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
	}
	if err := m.emitStage(frame); err != nil {
		m.logger.Debug("session stage not delivered", "session_id", sessionID, "stage", stage.String(), "error", err)
	}
}

// sessionState is the lifecycle phase of a node session. A session is
// provisioned (workdir/git/config dirs ready, editor NOT started) until a
// StartSessionRuntime (or auto_start) transitions it to running; a restart
// stops the editor process but keeps the session provisioned.
type sessionState string

const (
	sessionProvisioning sessionState = "provisioning"
	sessionProvisioned  sessionState = "provisioned"
	sessionRunning      sessionState = "running"
	sessionStopped      sessionState = "stopped"
)

// nodeSession is one unit of work on the node: a working directory, its applied
// configuration (LLM/MCP/skills/plugins/mode, each with a revision), and at most
// one running editor process. Provisioning, configuration, and runtime start are
// separate steps so the server can apply split config before the editor starts
// and hot-change it afterwards (with an explicit restart to make it effective).
type nodeSession struct {
	id                  string
	taskID              string
	editorID            string
	editorSessionID     string
	projectID           string
	provider            string
	workDir             string
	runtimeDir          string
	workspaceOwned      bool
	persistentWorkspace bool
	// skillsDir/pluginsDir hold synced skills/plugins.
	stateRoot  string
	home       string
	skillsDir  string
	pluginsDir string
	spec       *agentcomposev2.NodeCreateSession

	queue       *outputQueue
	fileService *fileService

	// baseCtx is the session-scoped context (derived from the create-time stream
	// ctx). sessionCancel tears the whole session down (delete/stopAll). Each
	// editor run derives its own runCtx/runCancel from baseCtx so a restart can
	// stop just the editor without killing the session.
	baseCtx       context.Context
	sessionCancel context.CancelFunc

	// mu guards the mutable config + run lifecycle below.
	mu          sync.Mutex
	state       sessionState
	llm         *agentcomposev2.NodeLLMConfig
	mode        string
	interactive bool
	// appliedRevision is the highest config revision persisted to disk;
	// effectiveRevision is the revision the currently running editor loaded. When
	// appliedRevision > effectiveRevision a restart is required to take effect.
	appliedRevision   uint64
	effectiveRevision uint64
	// current run
	executor  executor
	runCancel context.CancelFunc
	runDone   chan struct{}
	// structSeq is the monotonic sequence for structured events parsed from the
	// runtime NDJSON stream, per session.
	structSeq uint64
	// seenMessageIDs holds end-to-end user-message ids already written to the
	// runtime stdin. Same-id replays (gateway timeout retry, control-plane
	// re-delivery) are ACKed but never delivered twice. Guarded by mu.
	seenMessageIDs map[string]bool
	// pendingInputs buffers user messages that arrived before the runtime process
	// was up. When state transitions from provisioning → provisioned/running, the
	// buffer is flushed to the executor. Guarded by mu.
	pendingInputs []*agentcomposev2.NodeSessionInput

	// services maps a reverse-proxy service name (e.g. "files", "jupyter") to the
	// local base URL the node forwards tunneled requests to. Populated at session
	// start; read by the tunnel handler. Guarded by servicesMu.
	servicesMu sync.Mutex
	services   map[string]string
}

// serviceEndpoint returns the local base URL for a named proxy service on this
// session, or "" if the session exposes no such service.
func (s *nodeSession) serviceEndpoint(service string) string {
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()
	return s.services[service]
}

// setServiceEndpoint records the local base URL for a named proxy service.
func (s *nodeSession) setServiceEndpoint(service, baseURL string) {
	s.servicesMu.Lock()
	defer s.servicesMu.Unlock()
	if s.services == nil {
		s.services = map[string]string{}
	}
	s.services[service] = baseURL
}

// create provisions a session: it registers a placeholder in provisioning state
// and returns immediately (acking the dispatch), then runs the heavy preparation
// (git clone, skill/plugin downloads, runtime preflight) in a background goroutine.
// Early-arriving user input is buffered in the placeholder and flushed once the
// runtime is up. It does NOT start the editor unless auto_start is set (or unset,
// for backward compatibility). The async split keeps dispatch ack under 30s even
// for slow networks and large repos with submodules.
func (m *sessionManager) create(ctx context.Context, spec *agentcomposev2.NodeCreateSession) error {
	sessionID := strings.TrimSpace(spec.GetSessionId())
	if sessionID == "" {
		return fmt.Errorf("create session: session_id is required")
	}
	provider := strings.TrimSpace(spec.GetProvider())
	if provider == "" {
		return fmt.Errorf("create session %s: provider is required", sessionID)
	}

	m.mu.Lock()
	if _, exists := m.sessions[sessionID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("create session %s: already exists", sessionID)
	}
	m.mu.Unlock()

	// Fast synchronous prep: resolve paths, create directories, build the session
	// struct with all context/cancel fields so delete() never hits nil pointers.
	workDir := filepath.Join(m.opts.workRoot, sanitizeSessionDir(sessionID))
	workspaceOwned := true
	persistentWorkspace := false
	if taskID := strings.TrimSpace(spec.GetTaskId()); taskID != "" {
		workDir = filepath.Join(m.opts.workRoot, "tasks", sanitizeSessionDir(taskID))
		workspaceOwned = false
		persistentWorkspace = true
	} else if shared := strings.TrimSpace(spec.GetTags()["editor_workdir"]); shared != "" {
		workDir = filepath.Join(m.opts.workRoot, sanitizeSessionDir(shared))
		workspaceOwned = false
	}
	runtimeDir := filepath.Join(workDir, ".agent-compose", "sessions", sanitizeSessionDir(sessionID))
	base := runtimeDir
	stateRoot := filepath.Join(base, "state")
	skillsDir := filepath.Join(base, "skills")
	pluginsDir := filepath.Join(base, "plugins")
	home, err := m.resolveHome(spec, base)
	if err != nil {
		return fmt.Errorf("create session %s: %w", sessionID, err)
	}
	m.reportStage(sessionID, agentcomposev2.SessionStage_SESSION_STAGE_WORKSPACE_PREPARE, true, "准备工作目录", nil)
	for _, dir := range []string{workDir, stateRoot, home, skillsDir, pluginsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			wrapped := fmt.Errorf("create session %s: prepare dir %s: %w", sessionID, dir, err)
			m.reportStage(sessionID, agentcomposev2.SessionStage_SESSION_STAGE_WORKSPACE_PREPARE, false, "", wrapped)
			return wrapped
		}
	}

	// Build the full session struct with contexts so delete() won't panic.
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	session := &nodeSession{
		id:                  sessionID,
		taskID:              strings.TrimSpace(spec.GetTaskId()),
		editorID:            strings.TrimSpace(spec.GetEditorId()),
		editorSessionID:     strings.TrimSpace(spec.GetEditorSessionId()),
		projectID:           strings.TrimSpace(spec.GetProjectId()),
		workDir:             workDir,
		runtimeDir:          runtimeDir,
		spec:                spec,
		queue:               newOutputQueue(sessionID, m.emitOutput, m.logger),
		baseCtx:             sessionCtx,
		sessionCancel:       sessionCancel,
		state:               sessionProvisioning,
		seenMessageIDs:      make(map[string]bool),
		services:            make(map[string]string),
		home:                home,
		stateRoot:           stateRoot,
		workspaceOwned:      workspaceOwned,
		persistentWorkspace: persistentWorkspace,
		provider:            provider,
		skillsDir:           skillsDir,
		pluginsDir:          pluginsDir,
		llm:                 spec.GetLlm(),
		mode:                strings.TrimSpace(spec.GetMode()),
		interactive:         spec.GetInteractive(),
	}

	// Register the complete session BEFORE acking so deliverInput/delete/configure*
	// see real paths and contexts, not nil pointers.
	m.mu.Lock()
	if _, exists := m.sessions[sessionID]; exists {
		m.mu.Unlock()
		sessionCancel()
		return fmt.Errorf("create session %s: already exists", sessionID)
	}
	m.sessions[sessionID] = session
	m.mu.Unlock()

	// Now run the slow parts (clone, downloads, file service, config) in a
	// background goroutine so the ack returns immediately.
	go m.provisionSession(sessionCtx, session, spec)
	return nil
}

// provisionSession performs the heavy preparation work (git clone, skill/plugin
// downloads, file service, config) in the background after create() has built
// the complete session struct and acked. On success it transitions the session to
// provisioned (and optionally starts the runtime); on failure it reports the
// stage error and removes the session from the map.
func (m *sessionManager) provisionSession(ctx context.Context, session *nodeSession, spec *agentcomposev2.NodeCreateSession) {
	sessionID := session.id
	workDir := session.workDir

	// Provision the working tree locally: clone the repo and optionally branch.
	// The provider CLI only reads local disk, so everything must land here.
	if git := spec.GetGit(); git != nil && strings.TrimSpace(git.GetUrl()) != "" {
		branch := strings.TrimSpace(git.GetBranch())
		detail := "拉取代码"
		if branch != "" {
			detail = "拉取代码（分支 " + branch + "）"
		}
		m.reportStage(sessionID, agentcomposev2.SessionStage_SESSION_STAGE_GIT_CLONE, true, detail, nil)
		// Shared editor sessions can arrive concurrently. Serialize workspace
		// initialization so only one clone targets a given editor checkout.
		m.workspaceMu.Lock()
		err := m.provisionGit(ctx, workDir, git)
		m.workspaceMu.Unlock()
		if err != nil {
			wrapped := fmt.Errorf("create session %s: %w", sessionID, err)
			m.reportStage(sessionID, agentcomposev2.SessionStage_SESSION_STAGE_GIT_CLONE, false, "", wrapped)
			m.removeFailedSession(sessionID)
			return
		}
	}

	// Associated projects (project-association feature) clone into workspace
	// subdirectories after the main repo. Best-effort: a dependency that fails
	// must not block the task, so failures are logged, not returned.
	m.workspaceMu.Lock()
	m.provisionAssociatedRepos(ctx, sessionID, workDir, spec)
	m.workspaceMu.Unlock()

	// Start the built-in file service so the server can reverse-proxy file
	// browse/download/upload into this session's working tree (scoped to it).
	// Failure is non-fatal: the session still runs, only file proxying is off.
	if fs, err := startFileService(session.workDir); err != nil {
		m.logger.Warn("file service not started", "session_id", sessionID, "error", err)
	} else {
		session.fileService = fs
		session.setServiceEndpoint("files", fs.endpoint())
	}

	// Apply any config packed into CreateSession up front (LLM/MCP/mode). This
	// keeps single-shot DispatchSession callers working: they pack everything in
	// CreateSession and rely on auto_start. Best-effort — a config write failure
	// is logged, not fatal to provisioning.
	//
	// Skills and plugins are *downloads*, so this step can take real time — it was
	// previously invisible, leaving a silent gap between "拉取代码" and "检查运行
	// 环境" during which the page showed no progress at all. Report it as its own
	// stage, but only when there is actually something to sync: announcing a step
	// that does no work is noise.
	syncsResources := len(spec.GetSkills()) > 0 || len(spec.GetPlugins()) > 0
	if syncsResources {
		detail := "同步技能与插件"
		switch {
		case len(spec.GetPlugins()) == 0:
			detail = fmt.Sprintf("同步技能（%d 个）", len(spec.GetSkills()))
		case len(spec.GetSkills()) == 0:
			detail = fmt.Sprintf("同步插件（%d 个）", len(spec.GetPlugins()))
		default:
			detail = fmt.Sprintf("同步技能（%d 个）与插件（%d 个）", len(spec.GetSkills()), len(spec.GetPlugins()))
		}
		m.reportStage(sessionID, agentcomposev2.SessionStage_SESSION_STAGE_RESOURCE_SYNC, true, detail, nil)
	}
	if err := m.applyInitialConfig(session, spec); err != nil {
		m.logger.Warn("initial session config apply failed", "session_id", sessionID, "error", err)
		// Deliberately non-fatal (unchanged): a session with a missing skill still
		// runs, and failing provisioning here would break sessions that work today.
		// But it must no longer be *invisible* — a skill that failed to download
		// silently produces an agent without the capability the user selected, with
		// nothing on the page to explain it. Report the step as failed while
		// letting bring-up continue.
		if syncsResources {
			m.reportStage(sessionID, agentcomposev2.SessionStage_SESSION_STAGE_RESOURCE_SYNC, false, "", err)
		}
	}

	// Provisioning complete: transition from sessionProvisioning to sessionProvisioned
	// and flush any buffered input that arrived while we were preparing.
	session.mu.Lock()
	session.state = sessionProvisioned
	session.mu.Unlock()

	m.logger.Info("session provisioned", "session_id", sessionID, "provider", session.provider, "work_dir", workDir)

	// Flush buffered input now that the session is fully provisioned (before
	// starting the runtime, so the first message can trigger the turn).
	m.flushPendingInputs(session)

	// Backward compatibility: the classic single-shot DispatchSession caller packs
	// full config into CreateSession and expects immediate execution. defer_start
	// defaults to false, so those callers auto-start unchanged. A caller that wants
	// to apply split config (LLM/MCP/skills/plugins/mode) before the editor starts
	// sets defer_start=true and later sends StartSessionRuntime.
	if !spec.GetDeferStart() {
		if err := m.startRuntime(sessionID); err != nil {
			// Runtime start failed, but the session is already provisioned and
			// registered. Report the failure via stage event (startRuntime does
			// that) and leave the session in provisioned state so the user can
			// see the error on the task page.
			m.logger.Warn("auto-start runtime failed", "session_id", sessionID, "error", err)
		}
	}
}

// removeFailedSession removes a session from the map after provisioning fails.
func (m *sessionManager) removeFailedSession(sessionID string) {
	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()
	m.logger.Warn("session removed after provisioning failure", "session_id", sessionID)
}

// flushPendingInputs delivers all buffered input to the session's executor.
// Called after provisioning completes and before starting the runtime.
func (m *sessionManager) flushPendingInputs(session *nodeSession) {
	session.mu.Lock()
	pending := session.pendingInputs
	session.pendingInputs = nil
	session.mu.Unlock()

	if len(pending) == 0 {
		return
	}
	m.logger.Info("flushing pending input", "session_id", session.id, "count", len(pending))
	for _, input := range pending {
		m.deliverInput(input)
	}
}

// execution is the running provider process, abstracted over where it runs
// (a local process vs. a container exec). Both forms expose the same shape:
// two output readers the queue pumps, and a wait function returning the exit
// code. This keeps git clone / queue / upstream reporting identical regardless
// of isolation form.
type execution struct {
	stdout io.Reader
	stderr io.Reader
	// wait blocks until the process exits and returns its exit code. An error is
	// returned only for infrastructure failures (attach/inspect), not for a
	// non-zero exit.
	wait func() (int, error)
	// cleanup releases any resources the executor allocated for this run (e.g. a
	// docker container). It is called after wait returns, is optional (may be
	// nil), and must be safe to call once.
	cleanup func()
}

// executor starts a session's provider run and returns a handle to its output
// and completion. localExecutor runs the provider CLI as a host process;
// dockerExecutor runs it inside a container. Selection is per-session, driven by
// spec.driver and node capability. Interactive sessions (any provider) use
// streamExecutor; one-shot sessions use localExecutor/dockerExecutor.
type executor interface {
	start(ctx context.Context, session *nodeSession) (*execution, error)
}

// startRuntime starts (or restarts) the editor for a provisioned session. It
// selects an executor from the session's current config, derives a run context
// from the session context so the editor can be stopped independently of the
// session, marks the running config revision as effective, and launches run in
// the background. It is a no-op error if the session is already running.
func (m *sessionManager) startRuntime(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("start session %s: not found", sessionID)
	}

	session.mu.Lock()
	if session.state == sessionRunning {
		session.mu.Unlock()
		return nil
	}
	// selectExecutor is where the runtime pre-flight lives (provider on PATH +
	// a usable agent runtime), so a failure here is the "environment is not
	// ready" case — the one that used to surface as an exit-code-1 mystery.
	// Report it as its own stage so the task page can name it.
	m.reportStage(sessionID, agentcomposev2.SessionStage_SESSION_STAGE_RUNTIME_PREFLIGHT, true, "检查运行环境", nil)
	exec, err := m.selectExecutor(session.spec)
	if err != nil {
		session.mu.Unlock()
		m.reportStage(sessionID, agentcomposev2.SessionStage_SESSION_STAGE_RUNTIME_PREFLIGHT, false, "", err)
		return err
	}
	m.reportStage(sessionID, agentcomposev2.SessionStage_SESSION_STAGE_RUNTIME_START, true, "启动运行环境", nil)
	runCtx, runCancel := context.WithCancel(session.baseCtx)
	runDone := make(chan struct{})
	session.executor = exec
	session.runCancel = runCancel
	session.runDone = runDone
	session.state = sessionRunning
	// The editor loads whatever config is current; per-turn snapshots keep it in
	// sync with later changes, so the on-disk revision is immediately effective.
	session.effectiveRevision = session.appliedRevision
	session.mu.Unlock()

	go m.run(runCtx, session, exec, runDone)
	m.logger.Info("session runtime started", "session_id", sessionID, "provider", session.provider)
	return nil
}

// restartRuntime stops the current editor process and starts it again against
// the latest applied config revision. A fresh restart clears only the runtime's
// conversation state: the Task workspace, provider HOME, synced skills/plugins,
// and the runtime MCP config remain intact.
func (m *sessionManager) restartRuntime(sessionID string, fresh bool) error {
	sessionID = strings.TrimSpace(sessionID)
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("restart session %s: not found", sessionID)
	}
	session.mu.Lock()
	cancel := session.runCancel
	done := session.runDone
	session.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if fresh {
		if err := resetRuntimeState(session.stateRoot); err != nil {
			return fmt.Errorf("restart session %s fresh state: %w", sessionID, err)
		}
	}
	session.mu.Lock()
	session.state = sessionProvisioned
	session.mu.Unlock()
	return m.startRuntime(sessionID)
}

// resetRuntimeState clears provider conversation state while retaining the MCP
// file consumed by agent-compose-runtime. Other persisted Task configuration is
// outside stateRoot (HOME, skills, plugins), and the workspace is never touched.
func resetRuntimeState(stateRoot string) error {
	mcpPath := runtimeMCPConfigPath(stateRoot)
	mcpConfig, err := os.ReadFile(mcpPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read mcp config: %w", err)
	}
	if err := os.RemoveAll(stateRoot); err != nil {
		return fmt.Errorf("clear state root: %w", err)
	}
	if err := os.MkdirAll(stateRoot, 0o755); err != nil {
		return fmt.Errorf("recreate state root: %w", err)
	}
	if len(mcpConfig) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(mcpPath), 0o755); err != nil {
		return fmt.Errorf("recreate mcp config dir: %w", err)
	}
	if err := os.WriteFile(mcpPath, mcpConfig, 0o644); err != nil {
		return fmt.Errorf("restore mcp config: %w", err)
	}
	return nil
}

// run executes the provider for a session via the selected executor, streams its
// output through the local queue, and reports the terminal result. The queue
// survives connection drops; run itself does not block on the network.
func (m *sessionManager) run(ctx context.Context, session *nodeSession, exectr executor, runDone chan struct{}) {
	defer close(runDone)
	defer session.queue.start(ctx)() // start background flusher; returned stop() runs on exit

	exec, err := exectr.start(ctx, session)
	if err != nil {
		// The spawn itself failed (binary missing, bad HOME, permissions). This is
		// the last stage that can fail before the provider owns the outcome, so
		// name it here: without this the only signal was a synthetic exit code 1.
		m.reportStage(session.id, agentcomposev2.SessionStage_SESSION_STAGE_RUNTIME_START, false, "", err)
		m.reportResult(session, 1, false, err.Error(), "")
		m.markStopped(session)
		return
	}
	if exec.cleanup != nil {
		defer exec.cleanup()
	}
	// Process is alive and its pipes are attached: preparation is done and
	// anything after this point is the agent's own behaviour, not setup.
	m.reportStage(session.id, agentcomposev2.SessionStage_SESSION_STAGE_RUNNING, true, "运行中", nil)

	var resultJSON string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Parse stdout for structured events + the terminal result JSON while still
		// appending the raw bytes to the output queue (raw stream is preserved).
		resultJSON = m.pumpParsedStdout(session, exec.stdout)
	}()
	go func() {
		defer wg.Done()
		session.queue.pump(exec.stderr, agentcomposev2.StdioStream_STDIO_STREAM_STDERR)
	}()
	wg.Wait()

	exitCode, waitErr := exec.wait()
	session.queue.drain()

	success := waitErr == nil && exitCode == 0
	errMsg := ""
	if waitErr != nil {
		errMsg = waitErr.Error()
		if exitCode == 0 {
			exitCode = 1
		}
	}
	m.reportResult(session, int32(exitCode), success, errMsg, resultJSON)
	m.markStopped(session)
	m.logger.Info("session finished", "session_id", session.id, "exit_code", exitCode, "success", success)
}

// markStopped transitions a session out of the running state once its editor
// process exits, unless it was already torn down (deleted). A restart sets it
// back to provisioned before this runs, so only flip running→stopped.
func (m *sessionManager) markStopped(session *nodeSession) {
	session.mu.Lock()
	if session.state == sessionRunning {
		session.state = sessionStopped
	}
	session.mu.Unlock()
}

// configResult is what a config-apply command reports back in the NodeCommandAck.
// Config writes are pure disk writes now (the runtime re-prepares the provider
// from the per-turn snapshot on the next human_message), so there is no
// restart-required verdict: appliedRevision always equals effectiveRevision.
type configResult struct {
	appliedRevision   uint64
	effectiveRevision uint64
	restartRequired   bool
}

// lookupSession returns the session or an error suitable for a command ack.
func (m *sessionManager) lookupSession(sessionID string) (*nodeSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	return session, nil
}

// recordRevision bumps the applied revision to the incoming one (config commands
// carry a monotonic revision). Config writes take effect on the next turn (the
// runtime re-prepares from the per-turn snapshot), so the on-disk revision is
// always immediately effective — no restart-required verdict. Call under
// session.mu.
func (s *nodeSession) recordRevisionLocked(revision uint64) configResult {
	if revision > s.appliedRevision {
		s.appliedRevision = revision
	}
	s.effectiveRevision = s.appliedRevision
	return configResult{
		appliedRevision:   s.appliedRevision,
		effectiveRevision: s.appliedRevision,
		restartRequired:   false,
	}
}

// applyInitialConfig writes the config packed into a CreateSession spec (LLM,
// MCP, mode) to the session's editor config files before the editor starts. It
// is best-effort and revision 0 (the baseline the first editor run loads).
func (m *sessionManager) applyInitialConfig(session *nodeSession, spec *agentcomposev2.NodeCreateSession) error {
	// env_mode=system runs against the node operator's real HOME. The node must
	// NOT rewrite editor config into that home: writeMCPConfig / syncSkills /
	// syncPlugins all do exact-set rewrites under ~/.claude, ~/.codex, ~/.agents,
	// which would clobber the operator's own setup and delete skills they
	// installed by hand. The LLM still reaches the editor via buildEnv's env
	// vars, so system mode is usable; it just cannot apply task-selected
	// skills/MCPs/plugins. That is the accepted trade-off for reusing the host
	// toolchain — a system session gets the operator's environment as-is.
	if isSystemEnv(spec) {
		if llm := spec.GetLlm(); llm != nil {
			if err := m.writeLLMConfig(session, llm); err != nil {
				return fmt.Errorf("llm: %w", err)
			}
		}
		return nil
	}
	var errs []string
	if llm := spec.GetLlm(); llm != nil {
		if err := m.writeLLMConfig(session, llm); err != nil {
			errs = append(errs, "llm: "+err.Error())
		}
	}
	// MCP is always written per session, even in the shared tier: it is config,
	// not a file installed in the environment, and it carries this task's own
	// freshly minted token. writeMCPConfig's authoritative output goes to the
	// session's private stateRoot, so tokens never land in a shared HOME where a
	// concurrent task could read or overwrite them.
	if err := m.writeMCPConfig(session, spec.GetMcps()); err != nil {
		errs = append(errs, "mcp: "+err.Error())
	}
	// Skills/plugins are FILES. In the shared tier they belong to the environment
	// and are installed by NodeSyncEnvironment; a session must not rewrite them,
	// or one task dropping a skill would uninstall it for every other task on
	// that environment. The session only activates a subset (active_skills).
	if ownsHomeResources(spec) {
		if len(spec.GetSkills()) > 0 {
			if err := m.syncSkills(session, spec.GetSkills()); err != nil {
				errs = append(errs, "skills: "+err.Error())
			}
		}
		if len(spec.GetPlugins()) > 0 {
			if err := m.syncPlugins(session, spec.GetPlugins()); err != nil {
				errs = append(errs, "plugins: "+err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// configureLLM writes the session's LLM config to point the editor DIRECTLY at
// the given service (no server-side proxy) and records the revision. The change
// takes effect on the next human_message (the runtime re-prepares the provider
// from the per-turn snapshot); there is no live-switch protocol and no restart.
func (m *sessionManager) configureLLM(sessionID string, revision uint64, llm *agentcomposev2.NodeLLMConfig) (configResult, error) {
	session, err := m.lookupSession(sessionID)
	if err != nil {
		return configResult{}, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	session.llm = llm
	if err := m.writeLLMConfig(session, llm); err != nil {
		return configResult{}, err
	}
	return session.recordRevisionLocked(revision), nil
}

// applyMCPs rewrites the session's editor MCP config to exactly the given set.
func (m *sessionManager) applyMCPs(sessionID string, revision uint64, mcps []*agentcomposev2.MCPServerSpec) (configResult, error) {
	session, err := m.lookupSession(sessionID)
	if err != nil {
		return configResult{}, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if isSystemEnv(session.spec) {
		// system mode shares the operator's real HOME; writing MCP config
		// there would clobber ~/.mcp.json. LLM (env vars) is still applied.
		return session.recordRevisionLocked(revision), nil
	}
	if err := m.writeMCPConfig(session, mcps); err != nil {
		return configResult{}, err
	}
	return session.recordRevisionLocked(revision), nil
}

// applySkills syncs the full desired set of skills into the session skills dir.
func (m *sessionManager) applySkills(sessionID string, revision uint64, skills []*agentcomposev2.SkillSpec) (configResult, error) {
	session, err := m.lookupSession(sessionID)
	if err != nil {
		return configResult{}, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if isSystemEnv(session.spec) {
		// system mode shares the operator's real HOME; syncSkills does an
		// exact-set rewrite of ~/.claude/skills (or ~/.agents/skills) and
		// would delete skills the operator installed by hand.
		return session.recordRevisionLocked(revision), nil
	}
	if err := m.syncSkills(session, skills); err != nil {
		return configResult{}, err
	}
	return session.recordRevisionLocked(revision), nil
}

// applyPlugins syncs the full desired set of plugins into the session plugins dir.
func (m *sessionManager) applyPlugins(sessionID string, revision uint64, plugins []*agentcomposev2.NodePluginSpec) (configResult, error) {
	session, err := m.lookupSession(sessionID)
	if err != nil {
		return configResult{}, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if isSystemEnv(session.spec) {
		// system mode shares the operator's real HOME; syncPlugins rewrites
		// ~/.agents/plugins and would clobber the operator's own plugin set.
		return session.recordRevisionLocked(revision), nil
	}
	if err := m.syncPlugins(session, plugins); err != nil {
		return configResult{}, err
	}
	return session.recordRevisionLocked(revision), nil
}

// configureMode records the editor mode for a session and writes it to the
// session mirror. The change takes effect on the next human_message (the runtime
// re-prepares the provider from the per-turn snapshot); there is no live-switch
// protocol and no restart.
func (m *sessionManager) configureMode(sessionID string, revision uint64, mode string) (configResult, error) {
	session, err := m.lookupSession(sessionID)
	if err != nil {
		return configResult{}, err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	session.mode = strings.TrimSpace(mode)
	return session.recordRevisionLocked(revision), nil
}

// markArtifactsCollectable confirms the session exists and its file service (the
// tunnel endpoint the server pulls the packaged working tree through) is up. The
// actual archive is served by the file service over the reverse-proxy tunnel, so
// the node just validates reachability here.
func (m *sessionManager) markArtifactsCollectable(sessionID string) error {
	session, err := m.lookupSession(sessionID)
	if err != nil {
		return err
	}
	if session.fileService == nil {
		return fmt.Errorf("collect artifacts %s: file service is not available", sessionID)
	}
	return nil
}

// promptArgs builds the agent-compose-runtime prompt arguments for the given
// (guest-visible) paths. It is shared by both executors; only the path values
// differ (host paths for local, in-container paths for docker).
// promptArgs builds the agent-compose-runtime prompt arguments for the given
// session paths and config. activeSkills, when non-empty, is passed as repeated
// --skill flags so the one-shot run activates exactly that subset of the
// environment's installed skills (mirrors the stream executor's start frame).
func promptArgs(provider, model, mode, stateRoot, workspace, home string, activeSkills []string) []string {
	args := []string{
		"prompt",
		"--provider", provider,
		"--state-root", stateRoot,
		"--workspace", workspace,
		"--home", home,
	}
	if m := strings.TrimSpace(model); m != "" {
		args = append(args, "--model", m)
	}
	if md := strings.TrimSpace(mode); md != "" {
		args = append(args, "--mode", md)
	}
	for _, skill := range activeSkills {
		if s := strings.TrimSpace(skill); s != "" {
			args = append(args, "--skill", s)
		}
	}
	return args
}

// buildEnv layers the node process env, the session's declared env vars, and the
// pass-through LLM config (endpoint/key/model → provider-recognized vars). It
// also pins HOME/USERPROFILE to the session home so provider CLIs that read
// per-session config files (codex's ~/.codex/config.toml) see the session's
// own config rather than the node operator's.
func (m *sessionManager) buildEnv(session *nodeSession) []string {
	env := os.Environ()
	env = append(env,
		"WORKSPACE="+session.workDir,
		"HOME="+session.home,
		"USERPROFILE="+session.home,
	)
	for _, item := range session.spec.GetEnv() {
		name := strings.TrimSpace(item.GetName())
		if name == "" {
			continue
		}
		env = append(env, name+"="+item.GetValue())
	}
	// Use the session's current LLM config (which ConfigureSessionLLM may have
	// updated after create), not the original spec.
	session.mu.Lock()
	llm := session.llm
	session.mu.Unlock()
	if llm != nil {
		env = append(env, llmEnv(llm)...)
	}
	return env
}

// llmEnv maps the pass-through LLM config onto the environment variables the
// provider CLIs read. The external LLM service owns protocol/key lifecycle; the
// node just plants the values.
func llmEnv(llm *agentcomposev2.NodeLLMConfig) []string {
	var env []string
	endpoint := strings.TrimSpace(llm.GetEndpoint())
	key := strings.TrimSpace(llm.GetApiKey())
	if endpoint != "" {
		env = append(env,
			"LLM_API_ENDPOINT="+endpoint,
			"OPENAI_BASE_URL="+endpoint,
			"ANTHROPIC_BASE_URL="+endpoint,
		)
	}
	if key != "" {
		env = append(env,
			"LLM_API_KEY="+key,
			"OPENAI_API_KEY="+key,
			"ANTHROPIC_API_KEY="+key,
			"ANTHROPIC_AUTH_TOKEN="+key,
		)
	}
	if model := strings.TrimSpace(llm.GetModel()); model != "" {
		env = append(env,
			"LLM_MODEL="+model,
			// Claude Code does not read LLM_MODEL; it reads ANTHROPIC_MODEL.
			// Without this the task's chosen model was dropped and the CLI ran
			// its own default (claude-opus-4-8) regardless of models_snapshot.
			"ANTHROPIC_MODEL="+model,
		)
	}
	for k, v := range llm.GetExtra() {
		if k = strings.TrimSpace(k); k != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (m *sessionManager) provisionGit(ctx context.Context, workDir string, git *agentcomposev2.NodeGitSpec) error {
	cfg := workspaces.GitWorkspaceConfig{
		URL:          strings.TrimSpace(git.GetUrl()),
		Branch:       strings.TrimSpace(git.GetBranch()),
		Commit:       strings.TrimSpace(git.GetCommit()),
		Username:     strings.TrimSpace(git.GetUsername()),
		Password:     strings.TrimSpace(git.GetPassword()),
		CreateBranch: git.GetCreateBranch(),
		NewBranch:    strings.TrimSpace(git.GetNewBranch()),
	}
	if token := strings.TrimSpace(git.GetToken()); token != "" && cfg.Password == "" {
		// A token authenticates as the password half of basic auth.
		cfg.Password = token
	}

	// Keep the credential OUT of the clone URL: `git clone` persists whatever URL
	// it is given as remote.origin.url in .git/config, so a userinfo credential
	// would be readable for the workspace's lifetime by anything running in it
	// (`git remote -v`) — including other users on a shared host. Strip any
	// userinfo the gateway embedded and re-supply it as a per-command header.
	cloneURL := cfg.URL
	authUser, authPass := cfg.Username, cfg.Password
	if stripped, user, pass, ok := workspaces.SplitGitCredentialURL(cloneURL); ok {
		cloneURL = stripped
		if authUser == "" && authPass == "" {
			authUser, authPass = user, pass
		}
	}
	authArgs := workspaces.GitAuthHeaderArgs(authUser, authPass, cloneURL)

	// If the working dir already has a real checkout, skip re-cloning (idempotent
	// reconnect / recreate). A missing dir, or one holding only scratch/partial
	// content from a failed prior provision, is (re)created clean: git clone
	// refuses a non-empty target, and a fresh session has no workDir yet.
	if initialized, err := workspaces.HostWorkspaceInitialized(workDir); err != nil {
		return err
	} else if initialized {
		return nil
	} else if err := workspaces.ResetStaleWorkspace(workDir); err != nil {
		return err
	}

	args := append(authArgs, workspaces.GitCloneArgs(cloneURL, cfg, workDir)...)
	if err := runNodeGit(ctx, "", args...); err != nil {
		return err
	}
	if cfg.Commit != "" {
		fetchArgs := append(authArgs, workspaces.GitCommitFetchArgs(cfg.Commit)...)
		if err := runNodeGit(ctx, workDir, fetchArgs...); err == nil {
			if err := runNodeGit(ctx, workDir, "checkout", "FETCH_HEAD"); err != nil {
				return err
			}
		} else if err := runNodeGit(ctx, workDir, "checkout", cfg.Commit); err != nil {
			return err
		}
	}
	if cfg.CreateBranch {
		branch := strings.TrimSpace(cfg.NewBranch)
		if branch == "" {
			return fmt.Errorf("git create branch requested but new_branch is empty")
		}
		if err := runNodeGit(ctx, workDir, "checkout", "-b", branch); err != nil {
			return err
		}
	}
	return nil
}

// depGitSpec is one associated repository the gateway asked this session to
// clone alongside the main repo. It arrives as JSON in an
// AI_LUBRICANT_DEP_GIT_<i> env var (see the gateway's _build_association_env)
// rather than as a proto field, so the wire contract stays unchanged.
type depGitSpec struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
	Subdir string `json:"subdir"`
	Mode   string `json:"mode"`
}

const (
	depGitEnvPrefix         = "AI_LUBRICANT_DEP_GIT_"
	depPlaceholderEnvPrefix = "AI_LUBRICANT_DEP_PLACEHOLDER_"
)

// safeSubdirSegment reduces a requested subdirectory to a single path segment
// that cannot escape the workspace. The gateway already sanitizes, but the node
// must not trust a value that reaches it over the wire.
func safeSubdirSegment(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}
	// Reject anything with a separator or traversal rather than trying to repair
	// it: a caller sending "a/b" or ".." is not something to silently reinterpret.
	if strings.ContainsAny(trimmed, `/\`) || trimmed == "." || trimmed == ".." || strings.Contains(trimmed, "..") {
		return ""
	}
	if filepath.Base(trimmed) != trimmed {
		return ""
	}
	return trimmed
}

// provisionAssociatedRepos clones each associated repository the session
// declared into its own workspace subdirectory, and materializes a named
// placeholder directory for each associated project the task's token cannot
// read. A failure on one dependency is logged and skipped: the main repo is
// already in place and the task should still run.
func (m *sessionManager) provisionAssociatedRepos(ctx context.Context, sessionID, workDir string, spec *agentcomposev2.NodeCreateSession) {
	for _, item := range spec.GetEnv() {
		name := strings.TrimSpace(item.GetName())
		switch {
		case strings.HasPrefix(name, depGitEnvPrefix):
			var dep depGitSpec
			if err := json.Unmarshal([]byte(item.GetValue()), &dep); err != nil {
				m.logger.Warn("skip malformed dep env", "session_id", sessionID, "name", name, "error", err)
				continue
			}
			subdir := safeSubdirSegment(dep.Subdir)
			if subdir == "" || strings.TrimSpace(dep.URL) == "" {
				m.logger.Warn("skip dep env: unusable subdir or url", "session_id", sessionID, "name", name)
				continue
			}
			target := filepath.Join(workDir, subdir)
			depGit := &agentcomposev2.NodeGitSpec{
				Url:    dep.URL,
				Branch: dep.Branch,
			}
			if err := m.provisionGit(ctx, target, depGit); err != nil {
				// Redact: the dep URL may still carry a proxy token in userinfo.
				m.logger.Warn(
					"clone associated repo failed",
					"session_id", sessionID, "subdir", subdir,
					"error", workspaces.RedactGitURL(err.Error()),
				)
			}
		case strings.HasPrefix(name, depPlaceholderEnvPrefix):
			// "<project name>|<subdir>": the token cannot read this repo, so the
			// agent gets a named directory explaining the gap instead of a clone.
			projectName, subdirRaw, found := strings.Cut(item.GetValue(), "|")
			if !found {
				continue
			}
			subdir := safeSubdirSegment(subdirRaw)
			if subdir == "" {
				continue
			}
			target := filepath.Join(workDir, subdir)
			if err := os.MkdirAll(target, 0o755); err != nil {
				m.logger.Warn("create placeholder failed", "session_id", sessionID, "subdir", subdir, "error", err)
				continue
			}
			note := "# " + strings.TrimSpace(projectName) + "\n\n" +
				"This associated project was not cloned: the task's git credential " +
				"has no read access to its repository.\n"
			notePath := filepath.Join(target, "README.md")
			if err := os.WriteFile(notePath, []byte(note), 0o644); err != nil {
				m.logger.Warn("write placeholder note failed", "session_id", sessionID, "subdir", subdir, "error", err)
			}
		}
	}
}

func runNodeGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if strings.TrimSpace(dir) != "" {
		cmd.Dir = dir
	}
	// Fail fast on any credential prompt instead of hanging the dispatch ack:
	// on Windows the default Git Credential Manager pops a GUI dialog on auth
	// failure, which is the 30s "did not ack in time" symptom. A proxied repo
	// carries its signed token in the URL userinfo, so no interactive helper is
	// ever wanted; a direct (public) repo clone fails with a non-zero exit and
	// no prompt. Empty stdin ends any prompt attempt immediately. No credential
	// helper / git-credentials / global config is written or read.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=0",
		"GIT_ASKPASS=",
	)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		// The args and git stderr may echo the credential twice over: as an
		// http.extraHeader value (session provisioning) or as a clone-URL
		// userinfo (legacy/resource paths). Blank both before surfacing the
		// error so no credential reaches the ack / dispatch_error / logs.
		redactedArgs := workspaces.RedactGitSecrets(strings.Join(args, " "))
		redactedMsg := workspaces.RedactGitSecrets(msg)
		return fmt.Errorf("git %s failed: %s", redactedArgs, redactedMsg)
	}
	return nil
}

func (m *sessionManager) reportResult(session *nodeSession, exitCode int32, success bool, errMsg, resultJSON string) {
	frame := &agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_SessionResult{
			SessionResult: &agentcomposev2.NodeSessionResult{
				SessionId:  session.id,
				ExitCode:   exitCode,
				Success:    success,
				Error:      errMsg,
				ResultJson: resultJSON,
			},
		},
	}
	// Results are terminal and small; if the stream is down we log rather than
	// block — the server reconciles from the absence of the session on reconnect.
	if err := m.emitResult(frame); err != nil {
		m.logger.Warn("session result not delivered (stream down)", "session_id", session.id, "error", err)
	}
}

// deliverInput relays a caller input frame (a new turn / eof / cancel) to an
// interactive session's running stream process. Non-interactive sessions have
// no stream executor, so input is a no-op logged for observability.
//
// For human_message, if the caller did not supply a config snapshot the node
// stamps the session's current model/mode/llm onto the frame so the runtime
// re-prepares the provider with the latest config for this turn.
//
// When the session is still provisioning (git clone / skill downloads in flight),
// input is buffered in pendingInputs and flushed once provisioning completes.
func (m *sessionManager) deliverInput(input *agentcomposev2.NodeSessionInput) {
	sessionID := strings.TrimSpace(input.GetSessionId())
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		m.logger.Warn("session input dropped: unknown session", "session_id", sessionID)
		return
	}

	// If the session is still provisioning, buffer the input for later delivery.
	session.mu.Lock()
	if session.state == sessionProvisioning {
		session.pendingInputs = append(session.pendingInputs, input)
		session.mu.Unlock()
		m.logger.Info("session input buffered during provisioning", "session_id", sessionID, "client_message_id", input.GetClientMessageId())
		// Emit received status immediately so the gateway knows we accepted it,
		// even though the runtime hasn't processed it yet.
		if messageID := strings.TrimSpace(input.GetClientMessageId()); messageID != "" {
			m.emitInputStatus(sessionID, messageID, input.GetDeliveryAttempt(), "received")
		}
		return
	}
	session.mu.Unlock()

	// 端到端消息幂等：同一 (client_message_id, delivery_attempt) 的
	// human_message 只投递一次。网关侧的幂等保证「同 key 只投递一」，节点侧
	// 这层保证「网关超时重试 / 状态行被清理后重投、控制面重放等场景」不会把
	// 同一句话写进 runtime stdin 两遍。重试失败轮会递增 attempt，视为新 key
	// 放行。检查 + 预留在同一临界区完成，并发重复帧没有窗口；投递失败撤销
	// 预留（重试可在 runtime 恢复后再进来），已见过的 key 只回 ACK。
	messageID := strings.TrimSpace(input.GetClientMessageId())
	kind := strings.TrimSpace(input.GetKind())
	dedupe := messageID != "" && (kind == "human_message" || kind == "")
	dedupeKey := messageKey(input)
	if dedupe {
		session.mu.Lock()
		if session.seenMessageIDs == nil {
			session.seenMessageIDs = map[string]bool{}
		}
		if session.seenMessageIDs[dedupeKey] {
			session.mu.Unlock()
			m.logger.Info("session input replay dropped", "session_id", sessionID, "client_message_id", messageID, "delivery_attempt", input.GetDeliveryAttempt())
			m.emitInputStatus(sessionID, messageID, input.GetDeliveryAttempt(), "received")
			return
		}
		session.seenMessageIDs[dedupeKey] = true
		session.mu.Unlock()
	}
	session.mu.Lock()
	exectr := session.executor
	mode := session.mode
	llm := session.llm
	// Prefer the live LLM config's model (updated by ConfigureSessionLLM); fall
	// back to the create-time spec model when no LLM config has been pushed.
	model := ""
	if llm != nil {
		model = strings.TrimSpace(llm.GetModel())
	}
	if model == "" {
		model = strings.TrimSpace(session.spec.GetModel())
	}
	session.mu.Unlock()
	// Stamp the session's current config onto a human_message that arrived
	// without a snapshot, so the runtime re-prepares the provider for this turn.
	// This runs before the stream cast: even a not-yet-started (deferred) session
	// gets the snapshot filled, and a non-interactive session still benefits from
	// the caller seeing the resolved config.
	if kind == "human_message" || kind == "" {
		if strings.TrimSpace(input.GetModel()) == "" && model != "" {
			input.Model = model
		}
		if strings.TrimSpace(input.GetMode()) == "" && mode != "" {
			input.Mode = mode
		}
		if input.GetLlm() == nil && llm != nil {
			input.Llm = llm
		}
	}
	stream, ok := exectr.(*streamExecutor)
	if !ok {
		m.logger.Warn("session input dropped: session is not interactive", "session_id", sessionID)
		return
	}
	if err := stream.deliver(input); err != nil {
		if dedupe {
			// 投递失败撤销预留：runtime 未收到，同 key 重试必须能再进来。
			session.mu.Lock()
			delete(session.seenMessageIDs, messageKey(input))
			session.mu.Unlock()
		}
		m.logger.Warn("session input delivery failed", "session_id", sessionID, "error", err)
		return
	}
	// 写入 stdin 成功才回执：网关据此把消息行从 dispatching 推进到 received。
	// runtime 随后的 input_status / agent_turn_started 等事件同样按 message id
	// 关联（见 streamExecutor.deliver），这里只兜「runtime 不回执」的旧形态。
	if messageID != "" {
		m.emitInputStatus(sessionID, messageID, input.GetDeliveryAttempt(), "received")
	}
}

// messageKey is the node-side dedupe key for one execution of a user message:
// the logical id plus the delivery attempt (a failed turn retried with a bumped
// attempt is a NEW key; a transport replay of the same attempt is not).
func messageKey(input *agentcomposev2.NodeSessionInput) string {
	messageID := strings.TrimSpace(input.GetClientMessageId())
	if messageID == "" {
		return ""
	}
	attempt := input.GetDeliveryAttempt()
	if attempt == 0 {
		attempt = 1
	}
	return fmt.Sprintf("%s:%d", messageID, attempt)
}

// emitInputStatus reports one end-to-end message's delivery status upstream as
// a schema-light structured event (event_type=input_status), reusing the
// runtime event channel — no new proto oneof needed. The gateway's SSE consumer
// advances the mc_task_events row's delivery_status on receipt.
func (m *sessionManager) emitInputStatus(sessionID, messageID string, deliveryAttempt uint32, status string) {
	if m.emitStructured == nil || messageID == "" {
		return
	}
	if deliveryAttempt == 0 {
		deliveryAttempt = 1
	}
	payload, err := json.Marshal(map[string]any{
		"message_id":       messageID,
		"status":           status,
		"delivery_attempt": deliveryAttempt,
		"session_id":       sessionID,
	})
	if err != nil {
		return
	}
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	var seq uint64
	if ok {
		session.mu.Lock()
		seq = session.structSeq
		session.structSeq++
		session.mu.Unlock()
	}
	m.mu.Unlock()
	evt := &agentcomposev2.NodeSessionEventStructured{
		SessionId:   sessionID,
		Seq:         seq,
		EventType:   "input_status",
		PayloadJson: string(payload),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	upstream := &agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_SessionEvent{SessionEvent: evt},
	}
	if err := m.emitStructured(upstream); err != nil {
		// Same policy as runtime structured events: additive, stream-down drops
		// are acceptable; the state row's stale-claim path self-heals.
		m.logger.Debug("input status not delivered (stream down)", "session_id", sessionID, "error", err)
	}
}

func (m *sessionManager) delete(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	m.mu.Lock()
	session, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("delete session %s: not found", sessionID)
	}
	// Tear the whole session down: cancel the session context (which cancels any
	// live editor run derived from it) and wait for the editor goroutine to finish.
	session.mu.Lock()
	runDone := session.runDone
	session.mu.Unlock()
	session.sessionCancel()
	if runDone != nil {
		<-runDone
	}
	if session.fileService != nil {
		session.fileService.stop()
	}
	// Runtime deletion removes only runtime state for Task-owned workspaces. The
	// durable checkout remains until an explicit Task workspace delete operation.
	removeDir := session.runtimeDir
	if session.workspaceOwned && !session.persistentWorkspace {
		removeDir = session.workDir
	}
	if err := os.RemoveAll(removeDir); err != nil {
		return fmt.Errorf("delete session %s: remove runtime dir: %w", sessionID, err)
	}
	m.logger.Info("session deleted", "session_id", sessionID)
	return nil
}

func (m *sessionManager) stopAll() {
	m.mu.Lock()
	sessions := make([]*nodeSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	// Connection dropped: stop the running editor processes but keep the sessions
	// and their working dirs so a reconnect can resume. We cancel only the current
	// run (runCancel), not the whole session, and leave state as provisioned so a
	// later StartSessionRuntime can bring the editor back.
	for _, s := range sessions {
		s.mu.Lock()
		cancel := s.runCancel
		if s.state == sessionRunning {
			s.state = sessionProvisioned
		}
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
}

func (m *sessionManager) activeIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	return ids
}

func (m *sessionManager) summaries() []*agentcomposev2.NodeSessionSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*agentcomposev2.NodeSessionSummary, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, &agentcomposev2.NodeSessionSummary{
			SessionId: s.id,
			ProjectId: s.projectID,
			Provider:  s.provider,
		})
	}
	return out
}

// workspaceDir returns the on-disk workspace directory for a session, or ""
// when the session is unknown. Used by the terminal handler to open a shell in
// the session's own working tree without the server ever learning the path.
func (m *sessionManager) workspaceDir(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[sessionID]; ok {
		return s.workDir
	}
	return ""
}

func (m *sessionManager) hasProvider(provider string) bool {
	provider = strings.TrimSpace(provider)
	for _, p := range m.opts.providers {
		if strings.EqualFold(p, provider) {
			return true
		}
	}
	return false
}

func sanitizeSessionDir(sessionID string) string {
	var b strings.Builder
	for _, r := range sessionID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "session"
	}
	return out
}

var _ = time.Second
