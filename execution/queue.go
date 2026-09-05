package main

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// outputQueue is the node-local, in-order buffer between a running session's
// stdout/stderr and the upstream stream. Writes never block on the network: the
// producer (pump) appends chunks; a background flusher drains them to the server
// and, on send failure (stream down), keeps the chunk at the head and retries
// after a short delay. This is the anti-jitter mechanism — a connection drop
// only slows delivery, it never loses or reorders output, and the session
// process keeps running regardless.
//
// The queue is bounded in practice by a single session's output volume, which is
// finite, so unbounded growth is not a concern in the phase-1 local-agent form.
type outputQueue struct {
	sessionID string
	emit      emitFunc
	logger    *slog.Logger

	mu     sync.Mutex
	cond   *sync.Cond
	items  []queuedChunk
	offset uint64
	closed bool
}

type queuedChunk struct {
	data   []byte
	stream agentcomposev2.StdioStream
	offset uint64
	at     time.Time
}

func newOutputQueue(sessionID string, emit emitFunc, logger *slog.Logger) *outputQueue {
	q := &outputQueue{sessionID: sessionID, emit: emit, logger: logger}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// start launches the background flusher and returns a stop function to be
// deferred; stop signals the flusher to exit once the queue is drained.
func (q *outputQueue) start(ctx context.Context) func() {
	go q.flush(ctx)
	return func() {
		q.mu.Lock()
		q.closed = true
		q.mu.Unlock()
		q.cond.Broadcast()
	}
}

// pump reads a stream to EOF, appending every chunk to the queue. It is the
// producer side; it does not touch the network.
func (q *outputQueue) pump(r io.Reader, stream agentcomposev2.StdioStream) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			q.append(chunk, stream)
		}
		if err != nil {
			return
		}
	}
}

func (q *outputQueue) append(data []byte, stream agentcomposev2.StdioStream) {
	q.mu.Lock()
	q.items = append(q.items, queuedChunk{
		data:   data,
		stream: stream,
		offset: q.offset,
		at:     time.Now().UTC(),
	})
	q.offset += uint64(len(data))
	q.mu.Unlock()
	q.cond.Signal()
}

// drain blocks until every queued chunk has been delivered (or the queue is
// closed). Called after the process exits so the terminal result is only
// reported once all output is upstream.
func (q *outputQueue) drain() {
	q.mu.Lock()
	for len(q.items) > 0 && !q.closed {
		q.cond.Wait()
	}
	q.mu.Unlock()
}

// flush is the consumer: it delivers chunks in order, retrying the head chunk on
// send failure so a dropped connection resumes exactly where it left off.
func (q *outputQueue) flush(ctx context.Context) {
	for {
		q.mu.Lock()
		for len(q.items) == 0 && !q.closed {
			q.cond.Wait()
		}
		if len(q.items) == 0 && q.closed {
			q.mu.Unlock()
			return
		}
		chunk := q.items[0]
		q.mu.Unlock()

		frame := &agentcomposev2.NodeUpstreamFrame{
			Frame: &agentcomposev2.NodeUpstreamFrame_SessionOutput{
				SessionOutput: &agentcomposev2.NodeSessionOutput{
					SessionId: q.sessionID,
					Data:      chunk.data,
					Stream:    chunk.stream,
					Offset:    chunk.offset,
					CreatedAt: chunk.at.Format(time.RFC3339Nano),
				},
			},
		}
		if err := q.emit(frame); err != nil {
			// Stream down: keep the chunk at the head, back off briefly, retry.
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			continue
		}
		// Delivered: pop the head and wake any drain() waiter.
		q.mu.Lock()
		q.items = q.items[1:]
		q.mu.Unlock()
		q.cond.Broadcast()
	}
}
