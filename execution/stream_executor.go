package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"ai-lubricant-nodes/common/agent"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// streamExecutor runs agent-compose-runtime in its long-lived "stream" mode: the
// runtime process stays alive across turns, reading NDJSON input frames on
// stdin and emitting NDJSON output on stdout. This backs single-session
// multi-turn conversations for every provider (claude/codex/opencode/gemini).
//
// Unlike the one-shot executors, the stream process does not exit when a turn
// finishes — it waits for the next human_message (or eof). Input frames arrive
// out-of-band via NodeSessionInput and are written to stdin through the pipe
// held here.
//
// Hot config updates are NOT driven by a control protocol here. Each
// human_message frame carries the session's current config snapshot
// (model/mode/llm); the runtime re-prepares the provider with that snapshot on
// every turn. Config changes (model/mode/MCP/skill/plugin) therefore take
// effect on the next turn with no configure-ack and no restart.
type streamExecutor struct {
	mgr *sessionManager

	mu     sync.Mutex
	stdin  io.WriteCloser
	seq    int
	closed bool
}

func (e *streamExecutor) start(ctx context.Context, session *nodeSession) (*execution, error) {
	cmd, err := agent.RuntimeCommand(ctx, "stream")
	if err != nil {
		return nil, err
	}
	stateRoot := session.stateRoot
	home := session.home
	for _, dir := range []string{stateRoot, home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("prepare %s: %w", dir, err)
		}
	}

	cmd.Dir = session.workDir
	cmd.Env = e.mgr.buildEnv(session)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start provider stream: %w", err)
	}

	e.mu.Lock()
	e.stdin = stdin
	e.mu.Unlock()

	// The start frame carries the session's stable identity + paths. Per-turn
	// config (model/mode/llm) is NOT sent here; it rides each human_message so a
	// change between turns is honoured without restarting the runtime.
	startFrame := map[string]any{
		"v":               1,
		"seq":             e.nextSeq(),
		"type":            "start",
		"provider":        session.provider,
		"stateRoot":       stateRoot,
		"workspace":       session.workDir,
		"home":            home,
		"sessionId":       session.id,
		"taskId":          session.taskID,
		"editorId":        session.editorID,
		"editorSessionId": session.editorSessionID,
	}
	// active_skills narrows which of the environment's installed skills this
	// session turns on (shared tier). The runtime activates by name; the files
	// themselves live in the shared HOME and are never touched per session.
	if skills := e.mgr.activeSkillNames(session.spec); len(skills) > 0 {
		startFrame["skills"] = skills
	}
	if err := e.writeFrame(startFrame); err != nil {
		return nil, fmt.Errorf("send start frame: %w", err)
	}

	return &execution{
		stdout: stdout,
		stderr: stderr,
		wait: func() (int, error) {
			waitErr := cmd.Wait()
			e.mu.Lock()
			e.closed = true
			e.mu.Unlock()
			if waitErr == nil {
				return 0, nil
			}
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				return exitErr.ExitCode(), waitErr
			}
			return 1, waitErr
		},
		cleanup: func() {
			e.mu.Lock()
			if e.stdin != nil {
				_ = e.stdin.Close()
			}
			e.mu.Unlock()
		},
	}, nil
}

// deliver relays a caller input frame into the running stream process. It maps
// the NodeSessionInput kind onto the runtime's NDJSON input protocol. For
// human_message it stamps the frame with the session's current config snapshot
// (model/mode/llm) so the runtime re-prepares the provider for this turn.
func (e *streamExecutor) deliver(input *agentcomposev2.NodeSessionInput) error {
	kind := strings.TrimSpace(input.GetKind())
	switch kind {
	case "human_message", "":
		frame := map[string]any{
			"v":       1,
			"seq":     e.nextSeq(),
			"type":    "human_message",
			"message": input.GetText(),
		}
		if messageID := strings.TrimSpace(input.GetClientMessageId()); messageID != "" {
			frame["messageId"] = messageID
			attempt := input.GetDeliveryAttempt()
			if attempt == 0 {
				attempt = 1
			}
			frame["deliveryAttempt"] = attempt
		}
		if model := strings.TrimSpace(input.GetModel()); model != "" {
			frame["model"] = model
		}
		if mode := strings.TrimSpace(input.GetMode()); mode != "" {
			frame["mode"] = mode
		}
		if llm := input.GetLlm(); llm != nil {
			frame["llm"] = llmToFrame(llm)
		}
		return e.writeFrame(frame)
	case "eof":
		return e.writeFrame(map[string]any{"v": 1, "seq": e.nextSeq(), "type": "eof"})
	case "cancel":
		return e.writeFrame(map[string]any{"v": 1, "seq": e.nextSeq(), "type": "cancel"})
	default:
		return fmt.Errorf("unknown session input kind %q", kind)
	}
}

// llmToFrame maps the proto LLM config onto the runtime's NDJSON shape. The
// runtime reads this per turn to point the provider at the current endpoint/key
// without restarting the stream process.
func llmToFrame(llm *agentcomposev2.NodeLLMConfig) map[string]any {
	if llm == nil {
		return nil
	}
	out := map[string]any{
		"endpoint": llm.GetEndpoint(),
		"apiKey":   llm.GetApiKey(),
		"model":    llm.GetModel(),
		"protocol": llm.GetProtocol(),
	}
	if headers := llm.GetHeaders(); len(headers) > 0 {
		out["headers"] = headers
	}
	if extra := llm.GetExtra(); len(extra) > 0 {
		out["extra"] = extra
	}
	return out
}

func (e *streamExecutor) writeFrame(frame map[string]any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.stdin == nil {
		return fmt.Errorf("stream process is not accepting input")
	}
	_, err = e.stdin.Write(data)
	return err
}

func (e *streamExecutor) nextSeq() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	seq := e.seq
	e.seq++
	return seq
}
