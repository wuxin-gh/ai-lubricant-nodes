package agent

import "context"

// DetachStreamContext returns a context carrying ctx's values but not its
// cancellation, for node commands that must outlive a control-stream drop.
//
// The context handed to DownstreamHandler.HandleFrame is the per-*stream* one
// (client.serve creates it and cancels it the moment the NodeConnect stream
// breaks). A stream breaks on every reconnect — a server restart, a network
// blip, a control-plane redeploy — and commands that install something (an
// editor CLI, a host tool, a runtime archive) run for minutes and ack only when
// they finish. Inheriting the stream context meant a reconnect killed the child
// process halfway through: `npm install -g @anthropic-ai/claude-code` died with
// "exit status 0xffffffff" and produced no output at all, and the server —
// whose ack waiter had already been failed by the dropped connection — reported
// an install failure with no explanation of what actually went wrong.
//
// This mirrors reasoning already applied elsewhere in this package: the iOS
// device loops (execution/handler.go newIosFrameHandler) and the session
// manager both run on the process-lifetime context for exactly this reason.
// Callers keep their own timeout; losing the stream is never a reason to abort
// the work, because StopAll() is what deliberately quiesces node-side state on
// a drop.
func DetachStreamContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}
