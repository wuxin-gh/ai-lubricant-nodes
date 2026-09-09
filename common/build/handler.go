package build

import (
	"context"

	"ai-lubricant-nodes/common/agent"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// HandleBuildFrame routes a NodeBuildRequest or NodeBuildCancel downstream frame
// onto the runner and acks. It is a no-op for any other frame, returning false
// so the caller's own switch keeps going. Mounting this in a role handler is
// one line:
//
//	if h.builds != nil && h.builds.HandleBuildFrame(ctx, c, frame) {
//		return
//	}
//
// nil-safe: a nil receiver returns false so roles that never build (or have no
// runner wired) need no extra guard.
func (r *Runner) HandleBuildFrame(ctx context.Context, c *agent.Client, frame *agentcomposev2.NodeDownstreamFrame) bool {
	if r == nil {
		return false
	}
	frameID := frame.GetServerFrameId()
	switch payload := frame.GetFrame().(type) {
	case *agentcomposev2.NodeDownstreamFrame_NodeBuild:
		// Start is non-blocking: it registers the build and returns. Progress
		// and the terminal result stream back as their own upstream frames, so
		// the ack means "accepted", never "finished".
		err := r.Start(ctx, payload.NodeBuild)
		c.SendAck(frameID, err, nil)
		return true
	case *agentcomposev2.NodeDownstreamFrame_NodeBuildCancel:
		c.SendAck(frameID, r.Cancel(payload.NodeBuildCancel.GetBuildId()), nil)
		return true
	}
	return false
}
