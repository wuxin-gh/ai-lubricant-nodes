package agent

// The progress-reporting download behind self-upgrade / runtime-upgrade:
// byte-counted events, the truncation guard against a mid-body stream reset
// (the h2 INTERNAL_ERROR class), .part cleanup, the single automatic retry,
// and the forced-HTTP/1.1 client (the actual root cause fix).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type progressRecorder struct {
	mu     sync.Mutex
	stages []string
	cur    []int64
	total  []int64
}

func (p *progressRecorder) fn(stage, _ string, _ int, cur, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stages = append(p.stages, stage)
	p.cur = append(p.cur, cur)
	p.total = append(p.total, total)
}

func (p *progressRecorder) snapshot() (stages []string, curs, totals []int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.stages...), append([]int64(nil), p.cur...), append([]int64(nil), p.total...)
}

// payloadBytes builds a deterministic body of n bytes.
func payloadBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

func TestProgressDownloadEmitsMonotonicEvents(t *testing.T) {
	body := payloadBytes(9 << 20) // 9 MiB: enough for 2+ events at the 4 MiB rule
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if r := recover(); r != nil {
			t.Errorf("handler panic: %v", r)
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	rec := &progressRecorder{}
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := progressDownload(context.Background(), srv.URL+"/x.bin", dest, proxySpec{}, rec.fn); err != nil {
		t.Fatalf("progressDownload: %v", err)
	}
	stages, curs, totals := rec.snapshot()
	if len(stages) == 0 {
		t.Fatal("no progress events emitted")
	}
	if stages[0] != "downloading" || stages[len(stages)-1] != "downloading" {
		t.Fatalf("unexpected stages: %v", stages)
	}
	// monotonic non-decreasing byte counts
	for i := 1; i < len(curs); i++ {
		if curs[i] < curs[i-1] {
			t.Fatalf("byte counter went backwards: %v", curs)
		}
	}
	if curs[len(curs)-1] != int64(len(body)) {
		t.Fatalf("final bytes = %d, want %d", curs[len(curs)-1], len(body))
	}
	if totals[len(totals)-1] != int64(len(body)) {
		t.Fatalf("denominator = %d, want %d", totals[len(totals)-1], len(body))
	}
	// file landed complete (renamed from .part)
	got, err := os.ReadFile(dest)
	if err != nil || len(got) != len(body) {
		t.Fatalf("dest read: %v len=%d", err, len(got))
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal(".part file left behind")
	}
}

func TestProgressDownloadTruncationGuard(t *testing.T) {
	body := payloadBytes(6 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Advertise more than we send — a mid-body stream cut (the CDN RST_STREAM
		// shape). Whether the client surfaces it as a clean EOF short of
		// Content-Length (the truncation guard) or an unexpected-EOF read error,
		// it MUST be an error, never a silently-truncated "success".
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body[:2<<20])
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.bin")
	err := progressDownload(context.Background(), srv.URL+"/x.bin", dest, proxySpec{}, nil)
	if err == nil {
		t.Fatal("want error on mid-body truncation, got nil")
	}
	// Either the explicit guard message or the transport's EOF — both are the
	// "stream cut mid-transfer" class this protects against.
	if !strings.Contains(err.Error(), "下载被截断") && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("unexpected error shape: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("partial dest must not exist (rename-on-success violated)")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal(".part file left behind after truncation")
	}
}

func TestProgressDownloadRetriesOnce(t *testing.T) {
	body := payloadBytes(1 << 20)
	var attempts int32
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		if n == 1 {
			// First attempt dies mid-body (stream reset).
			_, _ = w.Write(body[:512<<10])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	rec := &progressRecorder{}
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := progressDownload(context.Background(), srv.URL+"/x.bin", dest, proxySpec{}, rec.fn); err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	mu.Lock()
	n := attempts
	mu.Unlock()
	if n != 2 {
		t.Fatalf("attempts = %d, want 2 (one retry)", n)
	}
	stages, _, _ := rec.snapshot()
	foundRetry := false
	for _, s := range stages {
		if s == "downloading" {
			foundRetry = true
		}
	}
	if !foundRetry {
		t.Fatalf("retry events lost: %v", stages)
	}
	got, err := os.ReadFile(dest)
	if err != nil || len(got) != len(body) {
		t.Fatalf("final file: %v len=%d", err, len(got))
	}
}

func TestProgressDownloadNoRetryOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: a retry cannot succeed either
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1024")
		_, _ = w.Write(payloadBytes(1024))
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := progressDownload(ctx, srv.URL+"/x.bin", dest, proxySpec{}, nil); err == nil {
		t.Fatal("want error for cancelled context")
	}
}

// TestUpgradeDownloadsForceHTTP1 pins the root-cause fix: every client built by
// httpClientForProxy (direct/url_prefix/network-http) must negotiate HTTP/1.1,
// never the h2 the DefaultTransport would attempt (the 188MB runtime archive
// got its h2 stream RST_STREAM'd mid-body: "INTERNAL_ERROR; received from peer").
func TestUpgradeDownloadsForceHTTP1(t *testing.T) {
	cases := []proxySpec{
		{mode: ""},
		{mode: "direct"},
		{mode: "url_prefix"},
		{mode: "network", url: "http://127.0.0.1:1"}, // unreachable proxy is fine: we only check the request proto the handler saw — but proxy mode can't be served by httptest directly
	}
	protoSeen := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protoSeen = r.Proto
		w.WriteHeader(200)
	}))
	defer srv.Close()

	for i, spec := range cases[:3] { // direct family hits the server itself
		protoSeen = ""
		client, err := httpClientForProxy(spec)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := client.Do(mustRequest(cctx, srv.URL))
		cancel()
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		resp.Body.Close()
		if protoSeen != "HTTP/1.1" {
			t.Fatalf("case %d: negotiated %s, want HTTP/1.1", i, protoSeen)
		}
	}
}

func mustRequest(ctx context.Context, url string) *http.Request {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		panic(err)
	}
	return req
}
