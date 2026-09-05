package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	containerapi "github.com/docker/docker/api/types/container"
	imageapi "github.com/docker/docker/api/types/image"
	mountapi "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Guest-visible paths inside the container. The editor workspace is bind mounted
// at guestWorkspace; each session's state/home is isolated beneath it.
const (
	guestWorkspace = "/workspace"
)

// dockerExecutor runs agent-compose-runtime inside a container. It reuses the
// node's local working tree (already git-cloned) by bind mounting it into the
// container, so the provider CLI sees the same files as the local executor —
// only the isolation boundary differs. It talks to the local Docker daemon via
// the SDK directly rather than importing pkg/driver, which keeps the node agent
// cross-compilable (pkg/driver pulls in Unix-only and cgo deps).
//
// The container is created per-run and removed on cleanup; the node does not
// keep long-lived containers in the phase-2 form.
type dockerExecutor struct {
	mgr   *sessionManager
	image string
}

func (e *dockerExecutor) start(ctx context.Context, session *nodeSession) (*execution, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("connect docker daemon: %w", err)
	}

	if err := e.ensureImage(ctx, cli); err != nil {
		_ = cli.Close()
		return nil, err
	}

	// Bind mount the host working tree into the container. The host path is the
	// editor workspace; runtime state is kept under a session-specific subtree
	// inside that workspace so multiple sessions can share the checkout.
	hostWorkDir, err := filepath.Abs(session.workDir)
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("resolve work dir: %w", err)
	}

	containerName := "agent-compose-node-" + sanitizeSessionDir(session.id)
	createResp, err := cli.ContainerCreate(ctx,
		&containerapi.Config{
			Image:      e.image,
			WorkingDir: guestWorkspace,
			Entrypoint: []string{"sh", "-lc"},
			Cmd:        []string{"tail -f /dev/null"},
			Env:        e.mgr.buildGuestEnv(session),
			Labels: map[string]string{
				"agent-compose.node.session_id": session.id,
			},
		},
		&containerapi.HostConfig{
			Mounts: []mountapi.Mount{{
				Type:   mountapi.TypeBind,
				Source: hostWorkDir,
				Target: guestWorkspace,
			}},
			AutoRemove: false,
		},
		nil, nil, containerName)
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("create container: %w", err)
	}
	containerID := createResp.ID

	// cleanup force-removes the container and closes the client. Safe to call once
	// after wait; also used on any error path below.
	cleanup := func() {
		removeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = cli.ContainerRemove(removeCtx, containerID, containerapi.RemoveOptions{Force: true})
		_ = cli.Close()
	}

	if err := cli.ContainerStart(ctx, containerID, containerapi.StartOptions{}); err != nil {
		cleanup()
		return nil, fmt.Errorf("start container: %w", err)
	}

	// Exec the provider run inside the running container. Paths are the
	// guest-visible ones; the bind mount makes them the same bytes as the host
	// work dir.
	session.mu.Lock()
	mode := session.mode
	session.mu.Unlock()
	guestStateRoot := filepath.Join(guestWorkspace, ".agent-compose", "sessions", sanitizeSessionDir(session.id), "state")
	guestHome := filepath.Join(guestWorkspace, ".agent-compose", "sessions", sanitizeSessionDir(session.id), "home")
	args := promptArgs(session.provider, session.spec.GetModel(), mode, guestStateRoot, guestWorkspace, guestHome, e.mgr.activeSkillNames(session.spec))
	mkdirs := "mkdir -p " + guestStateRoot + " " + guestHome
	shell := mkdirs + " && agent-compose-runtime " + shellJoin(args)
	execResp, err := cli.ContainerExecCreate(ctx, containerID, containerapi.ExecOptions{
		Cmd:          []string{"sh", "-lc", shell},
		Env:          e.mgr.buildGuestEnv(session),
		WorkingDir:   guestWorkspace,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("create exec: %w", err)
	}
	attach, err := cli.ContainerExecAttach(ctx, execResp.ID, containerapi.ExecAttachOptions{})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("attach exec: %w", err)
	}

	// Docker multiplexes stdout+stderr on one stream; stdcopy demultiplexes it.
	// We pipe each demuxed stream so the queue can pump them separately, matching
	// the local executor's two-reader shape.
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	go func() {
		_, copyErr := stdcopy.StdCopy(stdoutW, stderrW, attach.Reader)
		_ = stdoutW.CloseWithError(copyErr)
		_ = stderrW.CloseWithError(copyErr)
	}()

	wait := func() (int, error) {
		defer attach.Close()
		// Poll the exec until it stops running, then read its exit code.
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			inspect, err := cli.ContainerExecInspect(ctx, execResp.ID)
			if err != nil {
				return 1, fmt.Errorf("inspect exec: %w", err)
			}
			if !inspect.Running {
				return inspect.ExitCode, nil
			}
			select {
			case <-ctx.Done():
				return 1, ctx.Err()
			case <-ticker.C:
			}
		}
	}

	return &execution{
		stdout:  stdoutR,
		stderr:  stderrR,
		wait:    wait,
		cleanup: cleanup,
	}, nil
}

// ensureImage pulls the guest image if it is not already present locally.
func (e *dockerExecutor) ensureImage(ctx context.Context, cli *client.Client) error {
	if _, err := cli.ImageInspect(ctx, e.image); err == nil {
		return nil
	}
	reader, err := cli.ImagePull(ctx, e.image, imageapi.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", e.image, err)
	}
	defer func() { _ = reader.Close() }()
	// Drain the pull progress stream to completion; discarding the body.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("pull image %s: %w", e.image, err)
	}
	return nil
}

// buildGuestEnv layers the session's declared env vars and the pass-through LLM
// config for the in-container process. Unlike the local executor it does not
// inherit the node's os.Environ — the container has its own base environment.
func (m *sessionManager) buildGuestEnv(session *nodeSession) []string {
	env := []string{
		"WORKSPACE=" + guestWorkspace,
	}
	for _, item := range session.spec.GetEnv() {
		name := strings.TrimSpace(item.GetName())
		if name == "" {
			continue
		}
		env = append(env, name+"="+item.GetValue())
	}
	if llm := session.spec.GetLlm(); llm != nil {
		env = append(env, llmEnv(llm)...)
	}
	return env
}

// shellJoin quotes prompt args for a `sh -lc` command line so paths/values with
// spaces survive. Values here are provider/model/paths, none of which normally
// contain quotes, but we single-quote defensively.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'"'"'`) + "'"
	}
	return strings.Join(quoted, " ")
}
