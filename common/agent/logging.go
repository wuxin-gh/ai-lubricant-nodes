// Package-level logger setup for the node binaries.
//
// Logs go to stderr AND to a rotating file under the node's state dir. stderr
// alone was not debuggable in practice: on Windows the node usually runs as a
// service (or in a console window the operator eventually closes), so by the
// time someone asks "why did this session fail?" the only copy of the answer is
// gone. The file is the thing you open when a task misbehaves in production —
// it holds the same lines you see live, including the provider's own stderr
// (e.g. a Node.js ERR_MODULE_NOT_FOUND stack) that explains an exit code 1.
//
// Location: ``<install dir>/logs/<binary>-YYYY-MM-DD.log`` — the folder holding
// the node executable, because that is where an operator already is when they
// go looking. Falls back to ``<stateDir>/logs`` when the install root is not
// writable, and AGENT_COMPOSE_NODE_LOG_DIR overrides both. The file name carries
// the binary name so the execution and management roles never share a file.
//
// Retention is time-based (LogRetentionDays) and pruned on startup + on each
// date rollover. No external dependency: a size-based rotator would need one,
// and one file per day is what an operator actually reasons about.
package agent

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LogDirName is the subdirectory of the node state dir holding log files.
const LogDirName = "logs"

// LogRetentionDays is how long a daily log file is kept before startup/rollover
// pruning removes it.
const LogRetentionDays = 14

// logDirEnvOverride relocates the log directory (packaging, containers, or an
// install root the operator deliberately keeps read-only).
const logDirEnvOverride = "AGENT_COMPOSE_NODE_LOG_DIR"

// SetupLogger builds the node logger: stderr plus a rotating daily file.
//
// A file-logging failure is never fatal — the node must still start and log to
// stderr if its state dir is read-only or full; the failure itself is reported
// on the returned logger so it is visible rather than silent.
func SetupLogger(level string) *slog.Logger {
	writers := []io.Writer{os.Stderr}
	var setupErr error
	path := ""
	if w, p, err := newDailyLogWriter(); err != nil {
		setupErr = err
	} else {
		writers = append(writers, w)
		path = p
	}

	handler := slog.NewTextHandler(io.MultiWriter(writers...), &slog.HandlerOptions{
		Level: parseLevel(level),
	})
	logger := slog.New(handler)
	if setupErr != nil {
		logger.Warn("file logging disabled; logs are stderr-only", "error", setupErr)
	} else {
		// Print the path once at startup: the first question when debugging is
		// "where are the logs", and the answer should not require reading code.
		logger.Info("node log file", "path", path, "retention_days", LogRetentionDays)
	}
	return logger
}

// LogDir returns the directory holding the node's log files: ``logs/`` beside
// the running executable, i.e. the install directory. That is where an operator
// looks first — the same folder they installed/upgraded the binary in — rather
// than a per-user config path they have to be told about.
//
// If that directory cannot be used (read-only install root, e.g. under Program
// Files without elevation, or a service account that cannot write there), it
// falls back to the state dir so logging still happens somewhere predictable.
// AGENT_COMPOSE_NODE_LOG_DIR overrides both for packaging/containers.
func LogDir() (string, error) {
	if override := strings.TrimSpace(os.Getenv(logDirEnvOverride)); override != "" {
		return override, nil
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, linkErr := filepath.EvalSymlinks(exe); linkErr == nil {
			exe = resolved
		}
		candidate := filepath.Join(filepath.Dir(exe), LogDirName)
		if writableDir(candidate) {
			return candidate, nil
		}
	}
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, LogDirName), nil
}

// writableDir reports whether dir can be created and written to. Probing beats
// checking permission bits: on Windows the effective answer depends on ACLs and
// UAC, not on mode, so the only reliable test is to try.
func writableDir(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	probe, err := os.CreateTemp(dir, ".write-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return true
}

// dailyLogWriter appends to one file per calendar day, reopening on rollover.
// Writes are serialized: slog handlers may be called from many goroutines
// (session pumps, heartbeat, dispatch) and a torn line is worse than a lock.
type dailyLogWriter struct {
	dir    string
	prefix string

	mu   sync.Mutex
	file *os.File
	day  string // YYYY-MM-DD of the currently open file
}

// newDailyLogWriter opens today's log file, returning the writer and its path.
func newDailyLogWriter() (io.Writer, string, error) {
	dir, err := LogDir()
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("create log dir %s: %w", dir, err)
	}
	w := &dailyLogWriter{dir: dir, prefix: logFilePrefix()}
	if err := w.rotate(time.Now()); err != nil {
		return nil, "", err
	}
	return w, w.currentPath(), nil
}

// logFilePrefix derives the file-name prefix from the running binary so the
// execution and management roles do not interleave into one file. Falls back to
// a generic name if the executable path is unavailable.
func logFilePrefix() string {
	exe, err := os.Executable()
	if err != nil {
		return "node"
	}
	name := filepath.Base(exe)
	name = strings.TrimSuffix(name, filepath.Ext(name)) // drop .exe
	name = strings.TrimSpace(name)
	if name == "" {
		return "node"
	}
	return name
}

func (w *dailyLogWriter) currentPath() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return ""
	}
	return w.file.Name()
}

func (w *dailyLogWriter) Write(p []byte) (int, error) {
	day := time.Now().Format("2006-01-02")
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil || day != w.day {
		if err := w.rotateLocked(day); err != nil {
			// Rotation failed (disk full, permissions revoked mid-run). Report the
			// write as succeeded so a logging fault never breaks the caller; stderr
			// still carries the line via the MultiWriter.
			return len(p), nil
		}
	}
	return w.file.Write(p)
}

func (w *dailyLogWriter) rotate(now time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rotateLocked(now.Format("2006-01-02"))
}

// Close releases the open log file. The node process holds its logger for its
// whole lifetime and the OS reclaims the handle on exit, so this exists for
// callers with a bounded lifetime (tests, future embedded use) rather than for
// the binaries' own shutdown path.
func (w *dailyLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// rotateLocked closes the current file and opens the one for day. Call under mu.
func (w *dailyLogWriter) rotateLocked(day string) error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	path := filepath.Join(w.dir, fmt.Sprintf("%s-%s.log", w.prefix, day))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", path, err)
	}
	w.file = file
	w.day = day
	// Prune on every rollover (and thus on startup) so a long-lived node does not
	// accumulate files forever. Best-effort: pruning must never block logging.
	pruneOldLogs(w.dir, w.prefix, LogRetentionDays)
	return nil
}

// pruneOldLogs deletes this prefix's log files older than keepDays.
func pruneOldLogs(dir, prefix string, keepDays int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix+"-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, prefix+"-"), ".log")
		day, err := time.Parse("2006-01-02", stamp)
		if err != nil {
			continue // not one of ours; leave it alone
		}
		if day.Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

func parseLevel(level string) *slog.Level {
	parsed := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		parsed = slog.LevelDebug
	case "warn", "warning":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	}
	return &parsed
}
