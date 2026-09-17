package ioshost

import (
	"context"
	"errors"

	"ai-lubricant-nodes/common/agent"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// errNoManager is the ack error for an iOS management frame that arrived where
// no DeviceManager is wired. The server gates these frames on the ios_mgmt
// capability label, so this is a defensive reply rather than an expected path.
var errNoManager = errors.New("iOS device management is not enabled on this host")

// FrameHandler serves the iOS device management frames: discover / claim /
// release / configure / WDA job / job cancel / runner control.
//
// It is deliberately a standalone type rather than part of Handler, because two
// different binaries serve these frames:
//
//   - the dedicated node-ios host (role=ios_host), whose Handler owns nothing
//     but device management plus builds;
//   - an ordinary execution node on a machine with an iPhone attached, whose
//     Handler already owns sessions/terminals and now delegates here as well.
//
// Both wire a DeviceManager + WdaJobManager and call HandleIosFrame from their
// own HandleFrame; the frame semantics live here exactly once.
type FrameHandler struct {
	manager *DeviceManager
	jobs    *WdaJobManager
}

// NewFrameHandler builds the iOS frame handler. manager is required for the
// device frames (discover/claim/release/configure/runner control); jobs is
// required for the WDA job frames. A nil manager error-acks every device frame.
func NewFrameHandler(manager *DeviceManager, jobs *WdaJobManager) *FrameHandler {
	return &FrameHandler{manager: manager, jobs: jobs}
}

// HandleIosFrame serves one downstream frame when it belongs to the iOS device
// management family, and reports whether it did.
//
// Returning false means "not an iOS frame" — the caller keeps dispatching. This
// mirrors the build/xcode delegation shape (build.Runner.HandleBuildFrame,
// agent.XcodeJobRunner.HandleHostToolJobFrame), so a handler can chain the three
// with identical control flow.
func (h *FrameHandler) HandleIosFrame(ctx context.Context, c *agent.Client, frame *agentcomposev2.NodeDownstreamFrame) bool {
	frameID := frame.GetServerFrameId()
	switch payload := frame.GetFrame().(type) {
	case *agentcomposev2.NodeDownstreamFrame_IosDiscover:
		if h.manager == nil {
			c.SendAck(frameID, errNoManager, nil)
			return true
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
			return true
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
			return true
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
			return true
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
			return true
		}
		// Mark the device PREPARING before the job registers, so the console sees
		// "initializing" from the device inventory the moment work starts — not
		// only from the job snapshot (which a server restart would drop).
		h.manager.NoteWdaJobStarted(payload.IosWdaJob.GetUdid())
		// Start is non-blocking: it registers the job and returns. Progress and
		// the terminal result stream back as their own upstream frames, so the
		// ack means "accepted", never "finished".
		err := h.jobs.Start(ctx, payload.IosWdaJob)
		c.SendAck(frameID, err, nil)

	case *agentcomposev2.NodeDownstreamFrame_IosJobCancel:
		if h.jobs == nil {
			c.SendAck(frameID, errNoManager, nil)
			return true
		}
		c.SendAck(frameID, h.jobs.Cancel(payload.IosJobCancel.GetJobId()), nil)

	case *agentcomposev2.NodeDownstreamFrame_IosRunnerControl:
		if h.manager == nil {
			c.SendAck(frameID, errNoManager, nil)
			return true
		}
		// Start/stop/restart drives the persistent device loop: it can cancel
		// and relaunch a runner, which is off-dispatch-loop work. Ack carries
		// the transition result (a no-op START on an already-running device is
		// still ok).
		go func(req *agentcomposev2.NodeIosRunnerControl) {
			err := h.manager.ControlRunner(ctx, req)
			if err != nil {
				c.Logger().Warn("ios runner control failed",
					"udid", req.GetUdid(), "action", req.GetAction(), "error", err)
			} else {
				c.Logger().Info("ios runner control",
					"udid", req.GetUdid(), "action", req.GetAction())
			}
			c.SendAck(frameID, err, nil)
		}(payload.IosRunnerControl)

	default:
		return false
	}
	return true
}
