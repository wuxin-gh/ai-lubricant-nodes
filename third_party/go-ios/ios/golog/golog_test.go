package golog_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/danielpaulus/go-ios/ios/golog"
)

// TestSourceReportsCallerNotWrapper is the regression test for the seam: with
// AddSource enabled, a handler must report the file that called golog (this
// test file), not golog.go. It also confirms attrs pass through.
func TestSourceReportsCallerNotWrapper(t *testing.T) {
	var buf bytes.Buffer
	golog.SetLogger(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		AddSource: true,
		Level:     golog.LevelTrace,
	})))
	defer golog.SetLogger(nil)

	golog.Info("hello", "k", "v") // <- this callsite is what source must resolve to

	var rec struct {
		Msg    string `json:"msg"`
		K      string `json:"k"`
		Source struct {
			File string `json:"file"`
		} `json:"source"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("parse log line: %v\noutput: %s", err, buf.String())
	}
	if rec.Msg != "hello" || rec.K != "v" {
		t.Fatalf("record lost message/attrs: %+v", rec)
	}
	if strings.Contains(rec.Source.File, "golog.go") {
		t.Fatalf("source points at the wrapper golog.go, want the caller: %q", rec.Source.File)
	}
	if !strings.HasSuffix(rec.Source.File, "golog_test.go") {
		t.Fatalf("source file = %q, want the caller (golog_test.go)", rec.Source.File)
	}
}

// TestTraceLevel checks the custom LevelTrace is emitted when the handler level
// allows it and filtered out otherwise.
func TestTraceLevel(t *testing.T) {
	var buf bytes.Buffer
	golog.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: golog.LevelTrace})))
	defer golog.SetLogger(nil)

	golog.Trace("tracemsg")
	if !strings.Contains(buf.String(), "tracemsg") {
		t.Fatalf("trace not emitted at LevelTrace: %q", buf.String())
	}

	buf.Reset()
	golog.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	golog.Trace("hidden")
	if strings.Contains(buf.String(), "hidden") {
		t.Fatalf("trace should be filtered at Info level: %q", buf.String())
	}
}
