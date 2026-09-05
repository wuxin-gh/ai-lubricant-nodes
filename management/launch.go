package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"ai-lubricant-nodes/common/agent"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// launchRegistry tracks execution nodes this management node has launched, keyed
// by the server-assigned launch_id, so DeleteExecutionNode can tear them down.
type launchRegistry struct {
	mu      sync.Mutex
	handles map[string]launchHandle
}

// launchHandle records how a launched node was started so it can be stopped.
type launchHandle struct {
	method        string
	containerName string // docker
	composeDir    string // docker-compose
	unitName      string // systemd
}

func newLaunchRegistry() *launchRegistry {
	return &launchRegistry{handles: map[string]launchHandle{}}
}

func (r *launchRegistry) put(launchID string, h launchHandle) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handles[launchID] = h
}

func (r *launchRegistry) take(launchID string) (launchHandle, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.handles[launchID]
	if ok {
		delete(r.handles, launchID)
	}
	return h, ok
}

// handleCreateExecutionNode launches an execution node on this host per the
// server's spec, then acks. The launched node dials the server itself; this
// management node is not in its data path.
func (h *Handler) handleCreateExecutionNode(ctx context.Context, c *agent.Client, frameID string, spec *agentcomposev2.NodeCreateExecutionNode) {
	handle, err := h.launchExecutionNode(ctx, spec)
	if err == nil {
		h.launches.put(strings.TrimSpace(spec.GetLaunchId()), handle)
	}
	c.SendAck(frameID, err, nil)
}

func (h *Handler) handleDeleteExecutionNode(c *agent.Client, frameID string, spec *agentcomposev2.NodeDeleteExecutionNode) {
	handle, ok := h.launches.take(strings.TrimSpace(spec.GetLaunchId()))
	if !ok {
		// Nothing tracked; treat as already gone (idempotent).
		c.SendAck(frameID, nil, nil)
		return
	}
	c.SendAck(frameID, h.stopExecutionNode(handle), nil)
}

// launchExecutionNode starts an execution node using the requested method. The
// server address, node id, and TOTP secret are passed via the environment so
// they do not leak into argv/history.
func (h *Handler) launchExecutionNode(ctx context.Context, spec *agentcomposev2.NodeCreateExecutionNode) (launchHandle, error) {
	serverURL := strings.TrimSpace(spec.GetServerUrl())
	if serverURL == "" {
		serverURL = h.opts.server
	}
	// New specs carry the durable (node_id, secret) pair the launched node
	// authenticates with (TOTP). Both are required.
	nodeID := strings.TrimSpace(spec.GetNodeId())
	secret := strings.TrimSpace(spec.GetSecret())
	if nodeID == "" || secret == "" {
		return launchHandle{}, fmt.Errorf("create execution node: node credential (node_id+secret) is required")
	}
	method := nodeStartupMethodString(spec.GetStartupMethod())
	name := strings.TrimSpace(spec.GetNodeName())

	switch method {
	case "docker", "docker-compose":
		return h.launchExecutionNodeDocker(ctx, spec, serverURL, nodeID, secret, name)
	case "systemd":
		return launchHandle{}, fmt.Errorf("create execution node: systemd launch is not supported from a containerized management node; use the docker method")
	case "standalone", "":
		return h.launchExecutionNodeStandalone(ctx, serverURL, nodeID, secret, name)
	default:
		return launchHandle{}, fmt.Errorf("create execution node: unsupported startup method %q", method)
	}
}

// launchExecutionNodeDocker runs the execution node as a detached container.
func (h *Handler) launchExecutionNodeDocker(ctx context.Context, spec *agentcomposev2.NodeCreateExecutionNode, serverURL, nodeID, secret, name string) (launchHandle, error) {
	if _, err := agent.LookPath("docker"); err != nil {
		return launchHandle{}, fmt.Errorf("create execution node: docker not found on management node: %w", err)
	}
	image := strings.TrimSpace(spec.GetGuestImage())
	if image == "" {
		image = h.opts.agentImage
	}
	if image == "" {
		return launchHandle{}, fmt.Errorf("create execution node: no agent image configured (set --agent-image or spec guest_image)")
	}
	container := "agent-compose-exec-" + sanitizeLaunchName(strings.TrimSpace(spec.GetLaunchId()))

	args := []string{
		"run", "-d",
		"--name", container,
		"--restart", "always",
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		"-e", "AGENT_COMPOSE_SERVER=" + serverURL,
		"-e", "AGENT_COMPOSE_NODE_ID=" + nodeID,
		"-e", "AGENT_COMPOSE_NODE_SECRET=" + secret,
		"-e", "AGENT_COMPOSE_NODE_MANAGED=1",
		// The shared node image's entrypoint dispatches on AGENT_COMPOSE_NODE_ROLE.
		// Pin execution explicitly rather than relying on the default, so a future
		// change to the default cannot silently turn launched children into managers.
		"-e", "AGENT_COMPOSE_NODE_ROLE=execution",
		// Forward the manager's image tag so a manually-built local image (e.g.
		// ai-lubricant-node:local) is reused for children instead of pulling from a
		// registry. h.opts.agentImage already defaults to this; mirror it for the
		// case where the spec overrides guest_image but should still propagate.
		"-e", "AGENT_COMPOSE_AGENT_IMAGE=" + image,
	}
	if name != "" {
		args = append(args, "-e", "AGENT_COMPOSE_NODE_NAME="+name)
	}
	for _, item := range spec.GetEnv() {
		if k := strings.TrimSpace(item.GetName()); k != "" {
			args = append(args, "-e", k+"="+item.GetValue())
		}
	}
	args = append(args, image)

	cmd := exec.CommandContext(ctx, "docker", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return launchHandle{}, fmt.Errorf("create execution node: docker run failed: %s", strings.TrimSpace(string(out)))
	}
	h.logger.Info("launched execution node container", "container", container, "image", image)
	return launchHandle{method: "docker", containerName: container}, nil
}

// launchExecutionNodeStandalone runs the execution node as a child process. It
// launches the separate execution-node binary (executionBin) rather than
// re-exec'ing itself: the two roles are now distinct binaries, so a management
// node must know where the execution binary lives (falls back to
// "node-execution" on PATH; the legacy name remains a fallback).
func (h *Handler) launchExecutionNodeStandalone(ctx context.Context, serverURL, nodeID, secret, name string) (launchHandle, error) {
	bin := strings.TrimSpace(h.opts.executionBin)
	if bin == "" {
		var err error
		for _, candidate := range []string{"node-execution", "agent-compose-node-execution"} {
			bin, err = agent.LookPath(candidate)
			if err == nil {
				break
			}
		}
		if err != nil {
			return launchHandle{}, fmt.Errorf("create execution node: execution binary not configured (set --execution-bin) and not found on PATH: %w", err)
		}
	}
	args := []string{"--managed", "--server", serverURL, "--node-id", nodeID, "--secret", secret}
	if name != "" {
		args = append(args, "--name", name)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = os.Environ()
	// Detach: the launched node has its own lifecycle and reconnects on its own.
	if err := cmd.Start(); err != nil {
		return launchHandle{}, fmt.Errorf("create execution node: start process: %w", err)
	}
	h.logger.Info("launched execution node process", "pid", cmd.Process.Pid, "bin", bin)
	// We don't track the PID for teardown in standalone mode; stopping is a
	// host-level concern. Record the method so delete is a no-op ack.
	return launchHandle{method: "standalone"}, nil
}

// stopExecutionNode tears down a previously launched node.
func (h *Handler) stopExecutionNode(handle launchHandle) error {
	switch handle.method {
	case "docker":
		cmd := exec.Command("docker", "rm", "-f", handle.containerName)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("delete execution node: docker rm failed: %s", strings.TrimSpace(string(out)))
		}
		return nil
	case "docker-compose":
		if handle.composeDir == "" {
			return nil
		}
		cmd := exec.Command("docker", "compose", "down")
		cmd.Dir = handle.composeDir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("delete execution node: docker compose down failed: %s", strings.TrimSpace(string(out)))
		}
		_ = os.RemoveAll(handle.composeDir)
		return nil
	default:
		// standalone/systemd: not tracked for teardown here.
		return nil
	}
}

func nodeStartupMethodString(m agentcomposev2.NodeStartupMethod) string {
	switch m {
	case agentcomposev2.NodeStartupMethod_NODE_STARTUP_METHOD_STANDALONE:
		return "standalone"
	case agentcomposev2.NodeStartupMethod_NODE_STARTUP_METHOD_SYSTEMD:
		return "systemd"
	case agentcomposev2.NodeStartupMethod_NODE_STARTUP_METHOD_DOCKER:
		return "docker"
	case agentcomposev2.NodeStartupMethod_NODE_STARTUP_METHOD_DOCKER_COMPOSE:
		return "docker-compose"
	default:
		return ""
	}
}

func sanitizeLaunchName(id string) string {
	if id == "" {
		return "node"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "node"
	}
	return out
}
