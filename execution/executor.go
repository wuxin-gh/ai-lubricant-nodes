package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"ai-lubricant-nodes/common/agent"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// selectExecutor chooses how a session runs based on the requested driver and
// this node's capabilities. An empty/"local"/"process" driver runs the provider
// CLI as a host process; "docker" runs it inside a container. Selection fails
// fast with a clear error when the node can't satisfy the requested driver, so
// the create ack tells the server exactly why.
func (m *sessionManager) selectExecutor(spec *agentcomposev2.NodeCreateSession) (executor, error) {
	driver := strings.ToLower(strings.TrimSpace(spec.GetDriver()))
	switch driver {
	case "", "local", "process":
		provider := strings.TrimSpace(spec.GetProvider())
		if !m.hasProvider(provider) {
			return nil, fmt.Errorf("provider %q is not available on this node (local driver)", provider)
		}
		// Pre-flight the agent runtime here rather than discovering it missing in
		// the spawned process: RuntimeCommand failing inside run() only surfaces
		// as a NodeSessionResult{exit_code:1} *after* the create ack already said
		// accepted=true, which reads as a mysterious instant-exit session. Failing
		// here makes the ack carry the real reason (dispatch_error), so the task
		// shows "runtime not installed" instead of an unexplained exit 1.
		if err := agent.RuntimeInstalled(); err != nil {
			return nil, fmt.Errorf("node agent runtime is not usable: %w", err)
		}
		// Interactive sessions run the long-lived stream mode so turns can be fed
		// over time; one-shot sessions run prompt-and-exit. Every provider
		// (claude/codex/opencode/gemini) supports the stream mode.
		if spec.GetInteractive() {
			return &streamExecutor{mgr: m}, nil
		}
		return &localExecutor{mgr: m}, nil
	case "docker":
		if !m.opts.docker {
			return nil, fmt.Errorf("docker driver requested but this node does not offer docker")
		}
		image := strings.TrimSpace(spec.GetGuestImage())
		if image == "" {
			return nil, fmt.Errorf("docker driver requires guest_image")
		}
		return &dockerExecutor{mgr: m, image: image}, nil
	default:
		return nil, fmt.Errorf("unsupported driver %q", driver)
	}
}

// localExecutor runs agent-compose-runtime as a host process against the
// session's local working tree. This is the lightweight ("本机 agent") form:
// process/project-level isolation only.
type localExecutor struct {
	mgr *sessionManager
}

func (e *localExecutor) start(ctx context.Context, session *nodeSession) (*execution, error) {
	stateRoot := session.stateRoot
	home := session.home
	for _, dir := range []string{stateRoot, home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("prepare %s: %w", dir, err)
		}
	}

	session.mu.Lock()
	mode := session.mode
	session.mu.Unlock()
	args := promptArgs(session.provider, session.spec.GetModel(), mode, stateRoot, session.workDir, home, e.mgr.activeSkillNames(session.spec))
	cmd, err := agent.RuntimeCommand(ctx, args...)
	if err != nil {
		return nil, err
	}
	cmd.Dir = session.workDir
	cmd.Env = e.mgr.buildEnv(session)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start provider: %w", err)
	}
	return &execution{
		stdout: stdout,
		stderr: stderr,
		wait: func() (int, error) {
			waitErr := cmd.Wait()
			if waitErr == nil {
				return 0, nil
			}
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				return exitErr.ExitCode(), waitErr
			}
			return 1, waitErr
		},
	}, nil
}
