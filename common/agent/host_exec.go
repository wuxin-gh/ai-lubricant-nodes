package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

const (
	defaultHostExecTimeout = 2 * time.Minute
	maxHostExecTimeout     = 10 * time.Minute
	defaultHostExecOutput  = 1024 * 1024
	maxHostExecOutput      = 16 * 1024 * 1024
)

type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.max <= 0 {
		return len(p), nil
	}
	remaining := b.max - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buf.Write(p[:remaining])
		} else {
			_, _ = b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) truncated() bool { return b.max > 0 && b.buf.Len() >= b.max }

// RunHostExec executes one non-interactive command on the node host. It uses a
// shell deliberately: the caller is an administrator-side tool that accepts a
// shell command, while output and exit status remain structured for automation.
func RunHostExec(ctx context.Context, req *agentcomposev2.NodeHostExecRequest) *agentcomposev2.NodeHostExecResult {
	result := &agentcomposev2.NodeHostExecResult{RequestId: req.GetRequestId(), ExitCode: -1}
	command := strings.TrimSpace(req.GetCommand())
	if command == "" {
		result.Error = "command is required"
		return result
	}

	timeout := time.Duration(req.GetTimeoutMs()) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultHostExecTimeout
	}
	if timeout > maxHostExecTimeout {
		timeout = maxHostExecTimeout
	}
	maxOutput := int(req.GetMaxOutputBytes())
	if maxOutput <= 0 {
		maxOutput = defaultHostExecOutput
	}
	if maxOutput > maxHostExecOutput {
		maxOutput = maxHostExecOutput
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(runCtx, "cmd.exe", "/D", "/S", "/C", command)
	} else {
		cmd = exec.CommandContext(runCtx, "/bin/sh", "-c", command)
	}
	cwd := strings.TrimSpace(req.GetCwd())
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	var stdout, stderr cappedBuffer
	stdout.max, stderr.max = maxOutput, maxOutput
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		result.Stdout = stdout.buf.String()
		result.Stderr = stderr.buf.String()
		result.StdoutTruncated = stdout.truncated()
		result.StderrTruncated = stderr.truncated()
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			result.Error = "timeout"
		} else if errors.Is(runCtx.Err(), context.Canceled) {
			result.Error = "canceled"
		} else {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				result.ExitCode = int32(exitErr.ExitCode())
			} else {
				result.Error = err.Error()
			}
		}
		return result
	}
	result.ExitCode = 0
	result.Success = true
	result.Stdout = stdout.buf.String()
	result.Stderr = stderr.buf.String()
	result.StdoutTruncated = stdout.truncated()
	result.StderrTruncated = stderr.truncated()
	return result
}

// Keep io imported on platforms where exec's command implementation does not
// reference it in generated build constraints.
var _ io.Writer = (*cappedBuffer)(nil)
