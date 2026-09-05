package main

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
	// manager is nil in pure device mode (no NodeConnect identity), in which
	// case the iOS management frames are rejected as unsupported.
	manager *DeviceManager
	jobs    *WdaJobManager
	// builds may be nil (build runner not wired); HandleBuildFrame is nil-safe.
	builds *build.Runner
}

// NewHandler builds the iOS host downstream handler. manager may be nil (pure
// device mode); the iOS management frames then error-ack. builds may be nil.
func NewHandler(c *agent.Client, manager *DeviceManager, jobs *WdaJobManager, builds *build.Runner) *Handler {
	return &Handler{logger: c.Logger(), manager: manager, jobs: jobs, builds: builds}
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

// errNoManager is the ack error for an iOS management frame that arrived at a
// host running in pure device mode (no NodeConnect-managed device inventory).
// The server gates these frames on the ios_mgmt capability label, so this is a
// defensive reply rather than an expected path.
var errNoManager = errors.New("iOS device management is not enabled on this host")

// HandleFrame implements agent.DownstreamHandler: accept the two upgrade frames
// (copied verbatim in shape from management/handler.go), explicitly reject the
// session/launch/terminal/host-exec frames so the server does not wait on an ack
// that never comes, and log-and-drop anything unknown.
func (h *Handler) HandleFrame(ctx context.Context, c *agent.Client, frame *agentcomposev2.NodeDownstreamFrame) {
	frameID := frame.GetServerFrameId()
	if h.builds.HandleBuildFrame(ctx, c, frame) {
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

	// ─── iOS device management ───────────────────────────────────────────────
	case *agentcomposev2.NodeDownstreamFrame_IosDiscover:
		if h.manager == nil {
			c.SendAck(frameID, errNoManager, nil)
			return
		}
		// Ack immediately, then rescan: enumeration is a USB/network round trip
		// per device and must not block the dispatch loop (heartbeats and other
		// frames keep flowing). The inventory arrives as its own upstream frame.
		c.SendAck(frameID, nil, nil)
		go func(requestID string) {
			h.manager.Rescan(ctx)
			if err := c.EmitUpstream(&agentcomposev2.NodeUpstreamFrame{
				Frame: &agentcomposev2.NodeUpstreamFrame_IosDevicesReport{
					IosDevicesReport: h.manager.Snapshot(requestID),
				},
			}); err != nil {
				c.Logger().Debug("ios discover: report not sent", "error", err)
			}
		}(payload.IosDiscover.GetRequestId())

	case *agentcomposev2.NodeDownstreamFrame_IosClaimDevice:
		if h.manager == nil {
			c.SendAck(frameID, errNoManager, nil)
			return
		}
		// Claim redeems a pairing code over HTTP; off the dispatch loop.
		go func(req *agentcomposev2.NodeIosClaimDevice) {
			deviceID, err := h.manager.Claim(ctx, req)
			if err != nil {
				c.Logger().Warn("ios claim failed", "udid", req.GetUdid(), "error", err)
			} else {
				c.Logger().Info("ios device claimed", "udid", req.GetUdid(), "device_id", deviceID)
			}
			c.SendAck(frameID, err, nil)
		}(payload.IosClaimDevice)

	case *agentcomposev2.NodeDownstreamFrame_IosReleaseDevice:
		if h.manager == nil {
			c.SendAck(frameID, errNoManager, nil)
			return
		}
		go func(req *agentcomposev2.NodeIosReleaseDevice) {
			err := h.manager.Release(req)
			if err != nil {
				c.Logger().Warn("ios release failed", "udid", req.GetUdid(), "error", err)
			}
			c.SendAck(frameID, err, nil)
		}(payload.IosReleaseDevice)

	case *agentcomposev2.NodeDownstreamFrame_IosConfigureDevice:
		if h.manager == nil {
			c.SendAck(frameID, errNoManager, nil)
			return
		}
		// Applying config can restart a device's connection loop, so run it off
		// the dispatch loop and reply with the revision actually in effect.
		go func(req *agentcomposev2.NodeIosConfigureDevice) {
			applied, err := h.manager.ConfigureDevice(ctx, req)
			c.SendConfigAck(frameID, err, uint64(applied), uint64(applied), false)
		}(payload.IosConfigureDevice)

	case *agentcomposev2.NodeDownstreamFrame_IosWdaJob:
		if h.manager == nil || h.jobs == nil {
			c.SendAck(frameID, errNoManager, nil)
			return
		}
		// Start is non-blocking: it registers the job and returns. Progress and
		// the terminal result stream back as their own upstream frames, so the
		// ack means "accepted", never "finished".
		err := h.jobs.Start(ctx, payload.IosWdaJob)
		c.SendAck(frameID, err, nil)

	case *agentcomposev2.NodeDownstreamFrame_IosJobCancel:
		if h.jobs == nil {
			c.SendAck(frameID, errNoManager, nil)
			return
		}
		c.SendAck(frameID, h.jobs.Cancel(payload.IosJobCancel.GetJobId()), nil)

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
