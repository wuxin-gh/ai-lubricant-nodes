// Host-shell terminal support for nodes.
//
// A node can open an interactive PTY shell on its OWN host (not a session
// sandbox) so the management console can drive it like an SSH session. The
// server pushes NodeTerminalOpen/Input/Resize/Close down the NodeConnect
// stream, keyed by terminal_id; the node streams PTY output back up as
// NodeTerminalOutput and reports the shell exit as NodeTerminalExit.
//
// This file is platform-agnostic: it owns the terminal registry, the
// output-pump goroutine, and the upstream framing. The actual PTY is created by
// startPlatformPTY, implemented per-OS (creack/pty on unix, ConPTY on windows).
//
// Terminals survive a dropped NodeConnect stream: the pump keeps reading and
// buffering output even when the stream is down, and a NodeTerminalAttach
// (sent on reconnect) replays the buffered tail before resuming live streaming.
// A terminal with no attached stream is reaped after a configurable timeout so
// it does not leak indefinitely.
package agent

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// resolveTerminalCwd returns the working directory a shell should start in.
// An explicit cwd wins; empty means the node user's home directory
// (os.UserHomeDir()), matching the product decision that a terminal defaults to
// the user directory. If the home directory cannot be resolved, "" is returned
// so the OS picks its own default (never fatal).
func resolveTerminalCwd(cwd string) (string, error) {
	if c := strings.TrimSpace(cwd); c != "" {
		return c, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", nil
	}
	return home, nil
}

// Default PTY window used when the open frame carries no (or a zero) size. A
// real terminal is unusable at 0x0, so we clamp to the classic 80x24.
const (
	defaultTermCols uint16 = 80
	defaultTermRows uint16 = 24
	// termReadChunk bounds one PTY read; output is streamed, so a modest buffer
	// keeps latency low without oversized frames.
	termReadChunk = 32 * 1024
	// termReplayLimit is the maximum size of the per-terminal replay buffer.
	// When the stream is down for a long time the buffer is bounded so a
	// terminal cannot grow unbounded.
	termReplayLimit = 256 * 1024
	// termDetachTimeout is how long a terminal may sit with no attached stream
	// (no browser watching) before it is reaped. A terminal that is truly
	// abandoned would otherwise leak a PTY and its foreground process.
	termDetachTimeout = 30 * time.Minute
)

// ptyHandle abstracts a live pseudo-terminal across platforms. Read yields the
// shell's combined output; Write feeds its stdin; Resize changes the window;
// Close kills the shell and releases the PTY; Wait blocks for the exit code.
type ptyHandle interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	Wait() (exitCode int, err error)
}

// terminalSession is one open host shell.
type terminalSession struct {
	id        string
	pty       ptyHandle
	closeOnce sync.Once
	// attached says whether the control stream has announced this terminal as
	// attached. StopAll clears it; NodeTerminalAttach sets it again. The PTY
	// reader itself continues across both states.
	attached atomic.Bool
	// detachedAt is set when the stream drops. The reaper uses it as a strict
	// safety backstop for abandoned PTYs; output activity alone never keeps a
	// detached terminal alive forever.
	detachedAt atomic.Int64
	seq        atomic.Uint64

	// Last submitted command line, parsed best-effort from PTY input. The
	// management console renders this so an operator can see what each terminal
	// is doing before deciding whether to interrupt. Parsing is intentionally
	// simple: accumulate printable bytes; on CR/LF commit the line. Control
	// sequences (arrows, bracketed paste wrappers) are ignored. The SERVER
	// overlays the accurate agent-command state when one is running, so this is
	// only the fallback for operator-typed commands.
	cmdMu        sync.Mutex
	lineBuf      []byte
	lastCommand  string
	lastCommandT time.Time
	createdT     time.Time
}

func (s *terminalSession) markAttached() {
	s.attached.Store(true)
	s.detachedAt.Store(0)
}

func (s *terminalSession) markDetached() {
	if s.attached.Swap(false) {
		s.detachedAt.Store(time.Now().UnixNano())
	}
}

// terminalBuffer is a bounded FIFO ring buffer of terminal output, keyed by
// sequence number. It is used to replay output after a NodeConnect reconnect
// (NodeTerminalAttach). The buffer is bounded so a terminal that is detached
// for a long time does not grow unbounded.
type terminalBuffer struct {
	mu      sync.Mutex
	entries []terminalEntry
	// limit is the maximum total bytes retained; size tracks the current total.
	limit int
	size  int
}

type terminalEntry struct {
	seq  uint64
	data []byte
}

func newTerminalBuffer(limit int) *terminalBuffer {
	return &terminalBuffer{limit: limit}
}

// append records one output chunk under the sequence the caller already assigned
// (the manager owns the counter so a frame and its buffered copy always agree).
// Oldest entries are evicted once the retained bytes exceed the limit.
func (b *terminalBuffer) append(seq uint64, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = append(b.entries, terminalEntry{seq: seq, data: data})
	b.size += len(data)
	for b.size > b.limit && len(b.entries) > 0 {
		b.size -= len(b.entries[0].data)
		b.entries = b.entries[1:]
	}
}

// replay returns the entries whose sequence is strictly after afterSeq.
// truncated is true when afterSeq predates the oldest retained entry: output
// between the two was evicted and is genuinely lost, so the caller must say so
// rather than presenting the gap as a continuous stream.
func (b *terminalBuffer) replay(afterSeq uint64) (entries []terminalEntry, truncated bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == 0 {
		return nil, false
	}
	// entries[0].seq is the oldest byte we still hold. Anything the server had
	// not yet seen below that is gone.
	truncated = afterSeq+1 < b.entries[0].seq
	for _, entry := range b.entries {
		if entry.seq > afterSeq {
			entries = append(entries, entry)
		}
	}
	return entries, truncated
}

// TerminalManager owns all host-shell terminals for one node connection,
// keyed by terminal_id, and pumps their output up the shared stream. It is
// safe for concurrent use: dispatch delivers open/input/resize/close frames
// from the single dispatch loop, while each terminal's read pump runs in its
// own goroutine.
type TerminalManager struct {
	emit   func(*agentcomposev2.NodeUpstreamFrame) error
	logger *slog.Logger

	mu    sync.Mutex
	terms map[string]*terminalSession
	// Per-terminal replay buffers, keyed by terminal_id.
	buffers map[string]*terminalBuffer

	// Detached reaper: a terminal with no attached stream is reaped after a
	// configurable timeout so it does not leak indefinitely.
	reaperInterval time.Duration
	reaperDone     chan struct{}
}

// NewTerminalManager builds a manager that emits upstream frames through emit
// (the client's EmitUpstream). logger may be nil.
func NewTerminalManager(emit func(*agentcomposev2.NodeUpstreamFrame) error, logger *slog.Logger) *TerminalManager {
	return &TerminalManager{
		emit:           emit,
		logger:         logger,
		terms:          make(map[string]*terminalSession),
		buffers:        make(map[string]*terminalBuffer),
		reaperInterval: 30 * time.Second,
		reaperDone:     make(chan struct{}),
	}
}

// StartReaper begins the background goroutine that reaps terminals whose
// upstream stream has been down (no browser relaying) for longer than the
// configured timeout. Call once at startup.
func (m *TerminalManager) StartReaper() {
	go m.reapLoop()
}

// StopReaper stops the reaper goroutine.
func (m *TerminalManager) StopReaper() {
	select {
	case <-m.reaperDone:
	default:
		close(m.reaperDone)
	}
}

// ActiveTerminalIDs returns the IDs of terminals that are still alive on this
// node. The server uses this (in the heartbeat) to learn which terminals it can
// still reattach to after a NodeConnect reconnect or a server restart.
func (m *TerminalManager) ActiveTerminalIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.terms))
	for id := range m.terms {
		ids = append(ids, id)
	}
	return ids
}

// Open starts a new host shell for terminalID. shell/cwd empty pick the
// platform defaults (see startPlatformPTY). A duplicate terminalID is a no-op
// (the existing terminal stays). On failure it emits a NodeTerminalExit with
// the error so the server can close the websocket.
//
// extraEnv are appended to the shell's environment after the platform base.
// A maintenance terminal opened inside a shared environment passes HOME (and
// USERPROFILE) here so logins/installs land in that environment, not the
// operator's real home.
func (m *TerminalManager) Open(spec *agentcomposev2.NodeTerminalOpen, extraEnv ...string) {
	id := spec.GetTerminalId()
	if id == "" {
		return
	}
	m.mu.Lock()
	if _, exists := m.terms[id]; exists {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	cols, rows := defaultTermCols, defaultTermRows
	if size := spec.GetTerminalSize(); size != nil {
		if c := uint16(size.GetCols()); c > 0 {
			cols = c
		}
		if r := uint16(size.GetRows()); r > 0 {
			rows = r
		}
	}

	pty, err := startPlatformPTY(spec.GetShell(), spec.GetCwd(), cols, rows, extraEnv)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("terminal open failed", "terminal_id", id, "error", err)
		}
		m.emitExit(id, -1, err.Error())
		return
	}

	sess := &terminalSession{id: id, pty: pty, createdT: time.Now()}
	// Start detached: "attached" means the server is actually relaying this
	// terminal's output to a browser, which is proven by the first successful
	// emit in pump() (markAttached). Stamping detachedAt now means a terminal
	// that is opened but never watched still ages out via the reaper instead of
	// leaking a PTY forever.
	sess.detachedAt.Store(time.Now().UnixNano())
	m.mu.Lock()
	// Re-check: a racing Open for the same id could have won; if so, close ours.
	if _, exists := m.terms[id]; exists {
		m.mu.Unlock()
		_ = pty.Close()
		return
	}
	m.terms[id] = sess
	m.buffers[id] = newTerminalBuffer(termReplayLimit)
	m.mu.Unlock()

	go m.pump(sess)
}

// Attach resumes streaming an EXISTING terminal after a NodeConnect reconnect.
// The node replays whatever it still holds in its bounded buffer beyond
// afterSequence, then resumes live streaming. If the requested point has already
// been evicted from the buffer, the first frame carries replayTruncated=true so
// the UI can say output was dropped.
func (m *TerminalManager) Attach(spec *agentcomposev2.NodeTerminalAttach) {
	id := spec.GetTerminalId()
	if id == "" {
		return
	}
	m.mu.Lock()
	sess, ok := m.terms[id]
	if !ok {
		m.mu.Unlock()
		// The server asked to reattach a terminal that is really gone (node
		// restarted, or the shell already exited). Report it as exited so the
		// server closes the websocket.
		m.emitExit(id, -1, "terminal_not_found")
		return
	}
	m.mu.Unlock()

	// Replay buffered output from the requested sequence point.
	buf := m.buffers[id]
	if buf != nil {
		entries, truncated := buf.replay(spec.GetAfterSequence())
		for _, entry := range entries {
			m.emitOutput(id, entry.data, entry.seq, truncated)
			truncated = false // only the first frame of a reattach is truncated
		}
	}

	// Mark the terminal as attached again so the reaper does not close it. The
	// pump goroutine is already running and keeps reading the PTY; we do NOT
	// start a second one (that would race on the PTY).
	sess.markAttached()
}

// pump streams one terminal's output up until the shell exits or the PTY
// closes, then reports the exit and removes the terminal.
//
// Unlike the original version, this pump does NOT stop when the stream is down.
// It keeps reading the PTY and buffering output so a NodeTerminalAttach on
// reconnect can replay the tail. The pump only stops when the PTY itself closes
// (the shell exited) or the terminal is explicitly closed.
func (m *TerminalManager) pump(sess *terminalSession) {
	buf := make([]byte, termReadChunk)
	for {
		n, err := sess.pty.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			seq := sess.seq.Add(1)
			// Buffer the output so a reattach can replay it.
			if b := m.buffers[sess.id]; b != nil {
				b.append(seq, data)
			}
			if emitErr := m.emit(&agentcomposev2.NodeUpstreamFrame{
				Frame: &agentcomposev2.NodeUpstreamFrame_TerminalOutput{
					TerminalOutput: &agentcomposev2.NodeTerminalOutput{
						TerminalId: sess.id,
						Data:       data,
						Sequence:   seq,
					},
				},
			}); emitErr != nil {
				// Stream gone (connection dropped): keep reading and buffering.
				// The pump does NOT stop here — a NodeTerminalAttach on reconnect
				// replays the buffered tail.
				if m.logger != nil {
					m.logger.Debug("terminal output emit failed", "terminal_id", sess.id, "error", emitErr)
				}
			} else {
				// A successful emit means the server is relaying to a browser.
				sess.markAttached()
			}
		}
		if err != nil {
			break
		}
	}

	// The read loop ended: the shell closed its side. Reap the exit code and
	// report it, then drop the terminal from the registry.
	exitCode, waitErr := sess.pty.Wait()
	errMsg := ""
	if waitErr != nil {
		errMsg = waitErr.Error()
	}
	m.remove(sess.id)
	m.emitExit(sess.id, exitCode, errMsg)
}

// Input feeds stdin bytes to an open terminal (no-op if unknown/closed).
func (m *TerminalManager) Input(spec *agentcomposev2.NodeTerminalInput) {
	sess := m.get(spec.GetTerminalId())
	if sess == nil {
		return
	}
	data := spec.GetData()
	if _, err := sess.pty.Write(data); err != nil {
		if m.logger != nil {
			m.logger.Debug("terminal input write failed", "terminal_id", sess.id, "error", err)
		}
	}
	// Best-effort: record the submitted command so the management console can
	// show what this terminal is doing. Never let a parse error disturb the PTY.
	sess.recordInput(data)
}

// recordInput accumulates printable bytes into a line buffer and commits the
// line on CR/LF. Control sequences are dropped so the recorded command stays
// readable. This is only a fallback for operator-typed input; the server
// overlays accurate state when an agent command is running on this terminal.
func (s *terminalSession) recordInput(data []byte) {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	for _, b := range data {
		switch {
		case b == '\n' || b == '\r':
			line := strings.TrimSpace(string(s.lineBuf))
			s.lineBuf = s.lineBuf[:0]
			if line != "" {
				s.lastCommand = line
				s.lastCommandT = time.Now()
			}
		case b == 0x7f || b == 0x08: // backspace / ctrl-H
			if len(s.lineBuf) > 0 {
				s.lineBuf = s.lineBuf[:len(s.lineBuf)-1]
			}
		case b == 0x03: // Ctrl-C: interrupt clears the pending line
			s.lineBuf = s.lineBuf[:0]
		case b == 0x1b:
			// ESC begins a control sequence (arrows, bracketed paste, etc.).
			// A full VT parser is overkill here; just skip the ESC byte itself
			// and let following bytes fall through — they will mostly be dropped
			// by the printable filter, keeping the recorded line readable enough.
			continue
		default:
			if b >= 0x20 && b < 0x7f {
				s.lineBuf = append(s.lineBuf, b)
			}
		}
	}
}

// status snapshots this terminal for the management console.
func (s *terminalSession) status() *agentcomposev2.NodeTerminalStatus {
	s.cmdMu.Lock()
	cmd := s.lastCommand
	startedAt := ""
	if !s.lastCommandT.IsZero() {
		startedAt = s.lastCommandT.UTC().Format(time.RFC3339Nano)
	}
	s.cmdMu.Unlock()
	created := ""
	if !s.createdT.IsZero() {
		created = s.createdT.UTC().Format(time.RFC3339Nano)
	}
	return &agentcomposev2.NodeTerminalStatus{
		TerminalId:     s.id,
		CurrentCommand: cmd,
		StartedAt:      startedAt,
		CreatedAt:      created,
		Attached:       s.attached.Load(),
	}
}

// Resize changes an open terminal's PTY window (no-op if unknown/closed).
func (m *TerminalManager) Resize(spec *agentcomposev2.NodeTerminalResize) {
	sess := m.get(spec.GetTerminalId())
	if sess == nil {
		return
	}
	size := spec.GetTerminalSize()
	if size == nil {
		return
	}
	cols, rows := uint16(size.GetCols()), uint16(size.GetRows())
	if cols == 0 || rows == 0 {
		return
	}
	if err := sess.pty.Resize(cols, rows); err != nil {
		if m.logger != nil {
			m.logger.Debug("terminal resize failed", "terminal_id", sess.id, "error", err)
		}
	}
}

// Close kills an open terminal's shell and releases its PTY. The pump goroutine
// then observes the closed PTY, reaps the exit, and emits NodeTerminalExit.
func (m *TerminalManager) Close(spec *agentcomposev2.NodeTerminalClose) {
	sess := m.get(spec.GetTerminalId())
	if sess == nil {
		return
	}
	sess.close()
}

// List replies with every open host terminal and the command it most recently
// submitted. The management console renders this so an operator can see what each
// terminal is doing before interrupting or closing it. Correlated by request_id.
func (m *TerminalManager) List(spec *agentcomposev2.NodeTerminalListRequest) {
	m.mu.Lock()
	sessions := make([]*terminalSession, 0, len(m.terms))
	for _, sess := range m.terms {
		sessions = append(sessions, sess)
	}
	m.mu.Unlock()

	statuses := make([]*agentcomposev2.NodeTerminalStatus, 0, len(sessions))
	for _, sess := range sessions {
		statuses = append(statuses, sess.status())
	}
	_ = m.emit(&agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_TerminalListResult{
			TerminalListResult: &agentcomposev2.NodeTerminalListResult{
				RequestId:  spec.GetRequestId(),
				Terminals:  statuses,
			},
		},
	})
}

// Interrupt writes the interrupt character (Ctrl-C) into an open terminal,
// ending whatever command it is currently running and handing the prompt back.
// The terminal itself stays open — this is "stop what it is doing", not close.
func (m *TerminalManager) Interrupt(spec *agentcomposev2.NodeTerminalInterrupt) {
	sess := m.get(spec.GetTerminalId())
	if sess == nil {
		return
	}
	if _, err := sess.pty.Write([]byte{0x03}); err != nil {
		if m.logger != nil {
			m.logger.Debug("terminal interrupt write failed", "terminal_id", sess.id, "error", err)
		}
		return
	}
	// Ctrl-C abandons the in-progress line, so clear the buffer the same way the
	// recordInput path does — otherwise the next submission would prepend stale
	// bytes the operator already discarded.
	sess.cmdMu.Lock()
	sess.lineBuf = sess.lineBuf[:0]
	sess.cmdMu.Unlock()
}

// CloseAll kills every open terminal. Kept for callers that still need it, but
// StopAll no longer calls it so host terminals survive a dropped NodeConnect
// stream.
func (m *TerminalManager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*terminalSession, 0, len(m.terms))
	for _, sess := range m.terms {
		sessions = append(sessions, sess)
	}
	m.mu.Unlock()
	for _, sess := range sessions {
		sess.close()
	}
}

// DetachAll marks every terminal as detached (no browser watching) without
// killing the PTYs. Called on a connection drop so the reaper can age out
// abandoned terminals while the PTYs and their foreground processes survive.
func (m *TerminalManager) DetachAll() {
	m.mu.Lock()
	sessions := make([]*terminalSession, 0, len(m.terms))
	for _, sess := range m.terms {
		sessions = append(sessions, sess)
	}
	m.mu.Unlock()
	for _, sess := range sessions {
		sess.markDetached()
	}
}

func (m *TerminalManager) get(id string) *terminalSession {
	if id == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.terms[id]
}

func (m *TerminalManager) remove(id string) {
	m.mu.Lock()
	delete(m.terms, id)
	delete(m.buffers, id)
	m.mu.Unlock()
}

func (m *TerminalManager) emitOutput(id string, data []byte, seq uint64, truncated bool) {
	_ = m.emit(&agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_TerminalOutput{
			TerminalOutput: &agentcomposev2.NodeTerminalOutput{
				TerminalId:      id,
				Data:            data,
				Sequence:        seq,
				ReplayTruncated: truncated,
			},
		},
	})
}

func (m *TerminalManager) emitExit(id string, exitCode int, errMsg string) {
	_ = m.emit(&agentcomposev2.NodeUpstreamFrame{
		Frame: &agentcomposev2.NodeUpstreamFrame_TerminalExit{
			TerminalExit: &agentcomposev2.NodeTerminalExit{
				TerminalId: id,
				ExitCode:   int32(exitCode),
				Error:      errMsg,
			},
		},
	})
}

func (s *terminalSession) close() {
	s.closeOnce.Do(func() { _ = s.pty.Close() })
}

// reapLoop periodically checks for terminals whose stream has been down for
// longer than the configured timeout and closes them. A terminal that is truly
// abandoned would otherwise leak a PTY and its foreground process.
func (m *TerminalManager) reapLoop() {
	ticker := time.NewTicker(m.reaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.reaperDone:
			return
		case <-ticker.C:
			m.reapDetached()
		}
	}
}

func (m *TerminalManager) reapDetached() {
	m.mu.Lock()
	sessions := make([]*terminalSession, 0, len(m.terms))
	for _, sess := range m.terms {
		if sess.detachedAt.Load() == 0 {
			continue
		}
		if time.Since(time.Unix(0, sess.detachedAt.Load())) < termDetachTimeout {
			continue
		}
		sessions = append(sessions, sess)
	}
	m.mu.Unlock()
	for _, sess := range sessions {
		if m.logger != nil {
			m.logger.Info("reaping detached terminal", "terminal_id", sess.id)
		}
		sess.close()
	}
}