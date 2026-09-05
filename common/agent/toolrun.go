package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	log "log/slog"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// ToolRunManager owns long-running external CLI processes started on behalf of
// the tunnel manager (frpc / cloudflared / npc / …). Each run is keyed by
// run_id; its stdout/stderr are streamed back as NodeToolRunEvent frames, and a
// final EXITED event carries the exit code. Stop kills the process by run_id.
//
// Unlike host_exec this is for daemons that stay alive: there is no overall
// timeout and no output cap across the whole run (only per-chunk). The
// manager survives a NodeConnect drop — running processes keep going; a later
// reconnect re-streams future output (re-attach-by-run_id is a future hook; for
// now the process simply keeps running and the server learns its fate when it
// reconnects and the process eventually exits).
type ToolRunManager struct {
	emit   func(*agentcomposev2.NodeUpstreamFrame) error
	logger *log.Logger

	mu     sync.Mutex
	runs   map[string]*toolRun
	cancel map[string]context.CancelFunc
}

// defaultToolRunChunk is the per-event byte cap (stdout/stderr chunks).
const defaultToolRunChunk = 16 * 1024

// defaultStopGrace is the SIGTERM→SIGKILL window for ToolRunStop.
const defaultStopGrace = 5 * time.Second

// NewToolRunManager builds a manager that emits upstream frames through emit
// (the client's EmitUpstream). logger may be nil.
func NewToolRunManager(emit func(*agentcomposev2.NodeUpstreamFrame) error, logger *log.Logger) *ToolRunManager {
	return &ToolRunManager{
		emit:   emit,
		logger: logger,
		runs:   make(map[string]*toolRun),
		cancel: make(map[string]context.CancelFunc),
	}
}

// toolRun is one live process. The cancel func stops both the process (via the
// command's context) and the pump goroutines.
type toolRun struct {
	cmd      *exec.Cmd
	runCtx   context.CancelFunc
	revision uint64
}

// ActiveRuns returns a stable snapshot for heartbeat reconciliation.
func (m *ToolRunManager) ActiveRuns() []*agentcomposev2.NodeActiveToolRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*agentcomposev2.NodeActiveToolRun, 0, len(m.runs))
	for runID, run := range m.runs {
		pid := int64(0)
		if run.cmd != nil && run.cmd.Process != nil {
			pid = int64(run.cmd.Process.Pid)
		}
		out = append(out, &agentcomposev2.NodeActiveToolRun{
			RunId: runID, Revision: run.revision, Pid: pid,
		})
	}
	return out
}

// Start spawns the binary described by req and begins streaming output. It
// returns immediately; all output and the exit event are delivered as
// NodeToolRunEvent frames. A non-empty error means the process could not be
// spawned at all (the manager emits a single EXITED event with the error and
// does not register the run).
func (m *ToolRunManager) Start(req *agentcomposev2.NodeToolRunRequest) {
	runID := strings.TrimSpace(req.GetRunId())
	if runID == "" {
		return
	}
	binary := strings.TrimSpace(req.GetBinaryPath())
	if binary == "" {
		m.emitExit(runID, -1, "binary_path is required")
		return
	}

	chunk := int(req.GetMaxEventBytes())
	if chunk <= 0 {
		chunk = defaultToolRunChunk
	}

	runCtx, cancel := context.WithCancel(context.Background())
	// The process is killed when runCtx is canceled (exec.CommandContext
	// sends os.Kill by default on cancellation). We do not attempt to signal
	// the whole process group — that needs platform-specific syscalls and the
	// tunnel clients (frpc/cloudflared/npc) do not fork long-lived children
	// that would outlive their parent. SIGTERM-then-KILL grace is implemented
	// in Stop via Process.Signal + a timer.
	cmd := exec.CommandContext(runCtx, binary, req.GetArgs()...)
	if cwd := strings.TrimSpace(req.GetCwd()); cwd != "" {
		cmd.Dir = cwd
	} else if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	cmd.Env = mergeEnv(req.GetEnv())

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		m.emitExit(runID, -1, "stdout pipe: "+err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		m.emitExit(runID, -1, "stderr pipe: "+err.Error())
		return
	}

	if err := cmd.Start(); err != nil {
		cancel()
		m.emitExit(runID, -1, "spawn: "+err.Error())
		return
	}

	m.mu.Lock()
	if existing := m.cancel[runID]; existing != nil {
		// Duplicate run_id: cancel the previous one before registering the new.
		m.mu.Unlock()
		existing()
		m.mu.Lock()
	}
	m.runs[runID] = &toolRun{cmd: cmd, runCtx: cancel, revision: req.GetRevision()}
	m.cancel[runID] = cancel
	m.mu.Unlock()

	// STARTED is the spawn acknowledgement. It means the OS process exists (not
	// yet that frpc/cloudflared connected); the server promotes to READY by
	// parsing subsequent stdout/stderr for kind-specific success markers.
	_ = m.emit(&agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_ToolRunEvent{
			ToolRunEvent: &agentcomposev2.NodeToolRunEvent{
				RunId: runID, Kind: agentcomposev2.NodeToolRunKind_NODE_TOOL_RUN_KIND_STARTED,
				Revision: req.GetRevision(), Pid: int64(cmd.Process.Pid),
			},
		},
	})

	if m.logger != nil {
		m.logger.Info("tool run started", "run_id", runID, "binary", binary, "pid", cmd.Process.Pid)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go m.pump(runID, stdout, agentcomposev2.NodeToolRunKind_NODE_TOOL_RUN_KIND_STDOUT, chunk, &wg)
	go m.pump(runID, stderr, agentcomposev2.NodeToolRunKind_NODE_TOOL_RUN_KIND_STDERR, chunk, &wg)

	go func(revision uint64) {
		wg.Wait()
		waitErr := cmd.Wait()
		cancel()
		m.mu.Lock()
		delete(m.runs, runID)
		delete(m.cancel, runID)
		m.mu.Unlock()

		exitCode := 0
		errMsg := ""
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else if errors.Is(waitErr, context.Canceled) {
				exitCode = -1
				errMsg = "stopped"
			} else {
				exitCode = -1
				errMsg = waitErr.Error()
			}
		}
		m.emitExitRevision(runID, int32(exitCode), errMsg, revision)
		if m.logger != nil {
			m.logger.Info("tool run exited", "run_id", runID, "revision", revision, "exit_code", exitCode, "err", errMsg)
		}
	}(req.GetRevision())
}

// Stop signals a running tool by run_id: SIGTERM the process group, then
// SIGKILL after the grace window. A no-op if the run_id is unknown (already
// exited). The eventual EXITED event is emitted by the wait goroutine.
func (m *ToolRunManager) Stop(req *agentcomposev2.NodeToolRunStop) {
	runID := strings.TrimSpace(req.GetRunId())
	if runID == "" {
		return
	}
	m.mu.Lock()
	run, ok := m.runs[runID]
	m.mu.Unlock()
	if !ok {
		return
	}
	grace := time.Duration(req.GetGraceMs()) * time.Millisecond
	if grace <= 0 {
		grace = defaultStopGrace
	}
	proc := run.cmd.Process
	// Ask it to terminate politely (SIGTERM on unix, a CTRL_BREAK-ish on
	// windows via os.Interrupt — best effort), then force-kill after grace.
	if proc != nil {
		_ = proc.Signal(os.Interrupt)
	}
	timer := time.AfterFunc(grace, func() {
		if proc != nil {
			_ = proc.Kill()
		}
		// Cancel as a last-resort: ensures CommandContext reaps the process
		// even if Kill did not reach it.
		run.runCtx()
	})
	_ = timer
}

// StopAll kills every running tool. Called on connection drop so daemons do not
// leak. (Processes that should survive a reconnect are owned by the tunnel
// manager's persistence, not here — a future reattach hook can preserve them.)
func (m *ToolRunManager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.runs))
	for id := range m.runs {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Stop(&agentcomposev2.NodeToolRunStop{RunId: id})
	}
}

// pump reads one stream and emits NodeToolRunEvent chunks until EOF.
func (m *ToolRunManager) pump(runID string, r io.Reader, kind agentcomposev2.NodeToolRunKind, chunk int, wg *sync.WaitGroup) {
	defer wg.Done()
	br := bufio.NewReader(r)
	buf := make([]byte, chunk)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			_ = m.emit(&agentcomposev2.NodeUpstreamFrame{
				Frame: &agentcomposev2.NodeUpstreamFrame_ToolRunEvent{
					ToolRunEvent: &agentcomposev2.NodeToolRunEvent{
						RunId: runID,
						Kind:  kind,
						Data:  data,
					},
				},
			})
		}
		if err != nil {
			return
		}
	}
}

// emitExit sends the terminal EXITED event for a run.
func (m *ToolRunManager) emitExit(runID string, exitCode int32, errMsg string) {
	m.emitExitRevision(runID, exitCode, errMsg, 0)
}

func (m *ToolRunManager) emitExitRevision(runID string, exitCode int32, errMsg string, revision uint64) {
	_ = m.emit(&agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_ToolRunEvent{
			ToolRunEvent: &agentcomposev2.NodeToolRunEvent{
				RunId:    runID,
				Kind:     agentcomposev2.NodeToolRunKind_NODE_TOOL_RUN_KIND_EXITED,
				ExitCode: exitCode,
				Error:    errMsg,
				Revision: revision,
			},
		},
	})
}

// mergeEnv layers the request env on top of the node's environment.
func mergeEnv(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		env = append(env, k+"="+v)
	}
	return env
}
