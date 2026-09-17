package ioshost

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"ai-lubricant-nodes/common/agent"
	"ai-lubricant-nodes/common/build"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// Handler is the iOS host's downstream command handler. An iOS host is a real
// NodeConnect node but runs no coding sessions and launches no nodes — it dials
// in for identity/heartbeat/version-report/self-upgrade, serves the iOS device
// management frames (discover/claim/release/configure + WDA jobs), and drives
// its claimed iPhones over separate device-control WebSockets. It also serves
// build frames (NodeBuildRequest) so a macOS iOS host can double as the
// project-page build node — the xcodebuild capability that iOS builds need
// lives on exactly these hosts. Frames outside those families are rejected
// with an error ack rather than silently dropped, so the server's ack future
// never waits out its timeout.
type Handler struct {
	logger *slog.Logger
	// ios serves the device management frames. Its manager is nil in pure device
	// mode (no NodeConnect identity), in which case those frames error-ack.
	ios *FrameHandler
	// builds may be nil (build runner not wired); HandleBuildFrame is nil-safe.
	builds *build.Runner
	// xcode may be nil; HandleHostToolJobFrame is nil-safe.
	xcode *agent.XcodeJobRunner
}

// NewHandler builds the iOS host downstream handler. manager may be nil (pure
// device mode); the iOS management frames then error-ack. builds and xcode may
// be nil.
func NewHandler(c *agent.Client, manager *DeviceManager, jobs *WdaJobManager, builds *build.Runner, xcode *agent.XcodeJobRunner) *Handler {
	return &Handler{
		logger: c.Logger(),
		ios:    NewFrameHandler(manager, jobs),
		builds: builds,
		xcode:  xcode,
	}
}

// ActiveSessionIDs implements agent.DownstreamHandler: an iOS host runs no
// sessions.
func (h *Handler) ActiveSessionIDs() []string { return nil }

// ActiveToolRuns implements agent.DownstreamHandler: an iOS host hosts no tunnel
// clients.
func (h *Handler) ActiveToolRuns() []*agentcomposev2.NodeActiveToolRun { return nil }

// StopAll implements agent.DownstreamHandler: nothing to quiesce — the device-
// control sidecars have their own lifecycle and reconnect independently.
func (h *Handler) StopAll() {}

// errNotSupported is the ack error returned for any frame an iOS host does not
// serve. It is a typed error so the server can distinguish "unsupported on this
// role" from a transient handler failure if it ever inspects ack errors.
var errNotSupported = errors.New("iOS host does not run sessions or launch nodes")

// HandleFrame implements agent.DownstreamHandler: accept the two upgrade frames
// (copied verbatim in shape from management/handler.go), serve the iOS device
// management frames via the shared FrameHandler, explicitly reject the
// session/launch/terminal/host-exec frames so the server does not wait on an ack
// that never comes, and log-and-drop anything unknown.
func (h *Handler) HandleFrame(ctx context.Context, c *agent.Client, frame *agentcomposev2.NodeDownstreamFrame) {
	frameID := frame.GetServerFrameId()
	if h.builds.HandleBuildFrame(ctx, c, frame) {
		return
	}
	if h.xcode.HandleHostToolJobFrame(ctx, c, frame) {
		return
	}
	if h.ios.HandleIosFrame(ctx, c, frame) {
		return
	}
	switch payload := frame.GetFrame().(type) {
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
			if err := agent.SelfUpgrade(agent.DetachStreamContext(ctx), spec, c.Logger(), c.DownloadProxy()); err != nil {
				if agent.IsRestartExit(err) {
					c.Logger().Info("self-upgrade: restarting into new binary")
					os.Exit(0)
				}
				c.Logger().Error("self-upgrade failed", "error", err)
			}
		}(payload.SelfUpgrade)
	case *agentcomposev2.NodeDownstreamFrame_RuntimeUpgrade:
		// Detached from the stream ctx: the download can take minutes and a
		// reconnect must not abort it (see agent.DetachStreamContext).
		go func(spec *agentcomposev2.NodeRuntimeUpgrade) {
			err := agent.RuntimeUpgrade(agent.DetachStreamContext(ctx), spec, c.Logger(), c.DownloadProxy())
			if err != nil {
				c.Logger().Error("runtime-upgrade failed", "error", err)
			}
			c.SendAck(frameID, err, nil)
		}(payload.RuntimeUpgrade)
	case *agentcomposev2.NodeDownstreamFrame_InstallHostTool:
		// A macOS iOS host is the project-page build node, so it serves the same
		// host-tool surface as a management node — notably the xcodebuild
		// detection the build tab's install guidance drives. Same shape as
		// management/handler.go; installs share the managed-tools dir. Detached
		// from the stream ctx: the ack waits on the download.
		go func(spec *agentcomposev2.NodeInstallHostTool) {
			nodeV, npmV, xcodeV, err := agent.InstallHostTool(agent.DetachStreamContext(ctx), spec, c.Logger(), c.DownloadProxy())
			c.SendHostToolAck(frameID, err, nodeV, npmV, xcodeV)
		}(payload.InstallHostTool)

	// ─── iOS device management ───────────────────────────────────────────────
	// The iOS device management frames (discover/claim/release/configure/WDA
	// job/runner control) are served by the shared FrameHandler above — this is
	// the standalone iOS host's copy; an execution node delegates to the same
	// FrameHandler from its own HandleFrame. The frame semantics live once, in
	// frames.go.
	case *agentcomposev2.NodeDownstreamFrame_CreateSession,
		*agentcomposev2.NodeDownstreamFrame_DeleteSession,
		*agentcomposev2.NodeDownstreamFrame_ListSessions,
		*agentcomposev2.NodeDownstreamFrame_CreateExecutionNode,
		*agentcomposev2.NodeDownstreamFrame_DeleteExecutionNode,
		*agentcomposev2.NodeDownstreamFrame_TerminalOpen,
		*agentcomposev2.NodeDownstreamFrame_HostExec,
		*agentcomposev2.NodeDownstreamFrame_FileUpload,
		*agentcomposev2.NodeDownstreamFrame_ToolRunRequest:
		// These frames expect an ack. An iOS host serves none of them, so reply
		// with an error ack instead of silently dropping (which would leave the
		// server's await_ack future hanging until its timeout).
		c.SendAck(frameID, errNotSupported, nil)
	default:
		c.Logger().Warn("unexpected downstream frame for ios host", "frame_id", frameID)
	}
}
