// Package management is the management node binary's core: it launches and tears
// down execution nodes on its host on behalf of the server. It plugs into the
// shared connection layer (ai-lubricant-nodes/common/agent) by implementing
// agent.DownstreamHandler — the shared client owns the connection, registration,
// heartbeat, and framing; this handler owns the launch/delete commands.
//
// A management node does not run provider sessions itself; the execution nodes
// it launches dial the server directly and are not in this node's data path.
package main

import (
	"context"
	"log/slog"
	"os"

	"ai-lubricant-nodes/common/agent"
	"ai-lubricant-nodes/common/build"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// launchOptions are the management-node knobs: the server URL launched nodes
// dial back (falls back to the spec's server_url), the container image used for
// docker launches, and the execution binary used for standalone launches.
type launchOptions struct {
	server       string
	agentImage   string
	executionBin string
}

// Handler is the management node's downstream command handler. It owns the
// launch registry and routes create/delete execution-node commands onto it. It
// also serves build frames (a macOS management host can double as a build
// node — the server picks hosts by the xcodebuild_version label).
type Handler struct {
	opts      launchOptions
	logger    *slog.Logger
	launches  *launchRegistry
	terminals *agent.TerminalManager
	builds    *build.Runner
}

// NewHandler builds a management handler. server is the address launched
// execution nodes dial back (empty falls through to each spec's server_url);
// agentImage is the container image docker launches use; executionBin is the
// execution-node binary path standalone launches exec.
func NewHandler(c *agent.Client, server, agentImage, executionBin string) *Handler {
	return &Handler{
		opts:     launchOptions{server: server, agentImage: agentImage, executionBin: executionBin},
		logger:   c.Logger(),
		launches: newLaunchRegistry(),
		terminals: func() *agent.TerminalManager {
			t := agent.NewTerminalManager(c.EmitUpstream, c.Logger())
			t.StartReaper()
			return t
		}(),
		builds: build.NewRunner(c.EmitUpstream, c.Logger()),
	}
}

// ActiveSessionIDs implements agent.DownstreamHandler: a management node runs no
// sessions, so it always reports none.
func (h *Handler) ActiveSessionIDs() []string { return nil }

// ActiveToolRuns implements agent.DownstreamHandler: management nodes do not
// host tunnel clients; their execution children report their own inventories.
func (h *Handler) ActiveToolRuns() []*agentcomposev2.NodeActiveToolRun { return nil }

// StopAll implements agent.DownstreamHandler: launched execution nodes have their
// own lifecycle and reconnect independently, so a management-node connection drop
// does not tear them down. Host terminals are detached from the old stream but
// keep their PTYs and foreground processes until an explicit close or the node
// terminal reaper expires them.
func (h *Handler) StopAll() { h.terminals.DetachAll() }

// HandleFrame implements agent.DownstreamHandler: it routes one downstream
// launch command onto the launch registry and acks through the client.
func (h *Handler) HandleFrame(ctx context.Context, c *agent.Client, frame *agentcomposev2.NodeDownstreamFrame) {
	if h.builds.HandleBuildFrame(ctx, c, frame) {
		return
	}
	frameID := frame.GetServerFrameId()
	switch payload := frame.GetFrame().(type) {
	case *agentcomposev2.NodeDownstreamFrame_CreateExecutionNode:
		// Launch an execution node on this host. Runs in its own goroutine so a
		// slow launch (image pull) does not stall the dispatch loop.
		go h.handleCreateExecutionNode(ctx, c, frameID, payload.CreateExecutionNode)
	case *agentcomposev2.NodeDownstreamFrame_DeleteExecutionNode:
		go h.handleDeleteExecutionNode(c, frameID, payload.DeleteExecutionNode)
	case *agentcomposev2.NodeDownstreamFrame_SelfUpgrade:
		// Download a replacement binary and restart. Ack "accepted" first (the
		// connection drops when we restart, so a post-restart ack is impossible);
		// the server confirms success by the reconnect reporting the new version.
		c.Logger().Info("self-upgrade: received command from server",
			"frame_id", frameID,
			"target_version", payload.SelfUpgrade.GetTargetVersion(),
			"download_url", payload.SelfUpgrade.GetDownloadUrl())
		c.SendAck(frameID, nil, nil)
		go func(spec *agentcomposev2.NodeSelfUpgrade) {
			if err := agent.SelfUpgrade(ctx, spec, c.Logger(), c.DownloadProxy()); err != nil {
				if agent.IsRestartExit(err) {
					c.Logger().Info("self-upgrade: restarting into new binary")
					os.Exit(0)
				}
				c.Logger().Error("self-upgrade failed", "error", err)
			}
		}(payload.SelfUpgrade)
	case *agentcomposev2.NodeDownstreamFrame_RuntimeUpgrade:
		go func(spec *agentcomposev2.NodeRuntimeUpgrade) {
			err := agent.RuntimeUpgrade(ctx, spec, c.Logger(), c.DownloadProxy())
			c.SendAck(frameID, err, nil)
		}(payload.RuntimeUpgrade)
	case *agentcomposev2.NodeDownstreamFrame_TerminalOpen:
		// A management node with a client can also host a console shell (the
		// admin operates its host machine directly). env_id (environment
		// maintenance) is not resolved here: a management node runs no sessions
		// and owns no work root, so it has no shared environments to maintain.
		// The frame's cwd/session_id still drive the shell location.
		h.terminals.Open(payload.TerminalOpen)
	case *agentcomposev2.NodeDownstreamFrame_TerminalInput:
		h.terminals.Input(payload.TerminalInput)
	case *agentcomposev2.NodeDownstreamFrame_TerminalResize:
		h.terminals.Resize(payload.TerminalResize)
	case *agentcomposev2.NodeDownstreamFrame_TerminalClose:
		h.terminals.Close(payload.TerminalClose)
	case *agentcomposev2.NodeDownstreamFrame_TerminalAttach:
		h.terminals.Attach(payload.TerminalAttach)
	case *agentcomposev2.NodeDownstreamFrame_TerminalList:
		h.terminals.List(payload.TerminalList)
	case *agentcomposev2.NodeDownstreamFrame_TerminalInterrupt:
		h.terminals.Interrupt(payload.TerminalInterrupt)
	case *agentcomposev2.NodeDownstreamFrame_HostExec:
		go func(req *agentcomposev2.NodeHostExecRequest) {
			result := agent.RunHostExec(ctx, req)
			_ = c.EmitUpstream(&agentcomposev2.NodeUpstreamFrame{
				Frame: &agentcomposev2.NodeUpstreamFrame_HostExecResult{HostExecResult: result},
			})
		}(payload.HostExec)
	case *agentcomposev2.NodeDownstreamFrame_FileUpload:
		go func(req *agentcomposev2.NodeFileUploadRequest) {
			result := agent.RunFileUpload(ctx, req)
			_ = c.EmitUpstream(&agentcomposev2.NodeUpstreamFrame{
				Frame: &agentcomposev2.NodeUpstreamFrame_FileUploadResult{FileUploadResult: result},
			})
		}(payload.FileUpload)
	default:
		c.Logger().Warn("unexpected downstream frame for management node", "frame_id", frameID)
	}
}
