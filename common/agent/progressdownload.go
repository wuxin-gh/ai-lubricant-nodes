package agent

// Progress-reporting download shared by the self-upgrade and runtime-upgrade
// paths. The plain downloadTo loop (one io.Copy, no events) left an operator
// staring at a silent POST for minutes with no way to tell a slow link from a
// dead one — the "stream error: INTERNAL_ERROR" mid-download of the ~188MB
// runtime archive surfaced as an opaque failure with no stage attached. This
// mirrors common/agent/xcodejob.go's downloadXip: a manual read loop with
// byte-counted progress, a truncation guard, a .part file renamed on success,
// and one automatic full retry.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// UpgradeProgressFunc receives one progress step of an upgrade. stage uses the
// NodeUpgradeEvent vocabulary (downloading/verifying/extracting/activating/
// swapping/restarting/done/failed); current/totalBytes are set during the
// downloading stage (0/0 otherwise). Nil-safe call sites: handlers pass nil
// when no upstream stream is worth reporting on.
type UpgradeProgressFunc func(stage, message string, percent int, currentBytes, totalBytes int64)

// progressEmitBytes / progressEmitInterval bound the event rate during the
// download loop: an event every 4 MiB or 5 s, whichever comes first — fast
// links hit the size rule, slow links keep the UI alive via the time rule.
const (
	progressEmitBytes    = 4 << 20
	progressEmitInterval = 5 * time.Second
)

// progressDownload fetches url into dest with byte-counted progress via
// onBytes (nil means download without events). The file lands via a .part
// temp renamed on success, so a partial download never looks complete; the
// truncation guard rejects a clean EOF before ContentLength bytes (the
// proxy/CDN mid-body RST_STREAM class that used to pass as a "successful"
// short file). One automatic full retry covers a transient mid-body error;
// sha256 verification downstream guards correctness, so no Range resume.
func progressDownload(ctx context.Context, url, dest string, spec proxySpec, onBytes UpgradeProgressFunc) error {
	err := progressDownloadOnce(ctx, url, dest, spec, onBytes)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err // cancelled: a retry cannot succeed either
	}
	if onBytes != nil {
		onBytes("downloading", fmt.Sprintf("下载中断，自动重试：%v", err), 0, 0, 0)
	}
	return progressDownloadOnce(ctx, url, dest, spec, onBytes)
}

func progressDownloadOnce(ctx context.Context, url, dest string, spec proxySpec, onBytes UpgradeProgressFunc) error {
	finalURL := resolveDownloadURL(spec, url)
	client, err := httpClientForProxy(spec)
	if err != nil {
		return fmt.Errorf("build http client: %w", err)
	}
	if client == http.DefaultClient {
		client = &http.Client{} // never mutate the shared default's Timeout
	}
	// One line per attempt: the resolved (post-rewrite) URL next to the raw one,
	// so a url_prefix rewrite is visible. The egress route itself is logged by
	// httpClientForProxy below (the universal choke point), so it is not repeated
	// here.
	slog.Default().Info("download: start",
		"url", finalURL,
		"raw_url", url,
		"dest", dest,
	)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, finalURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", finalURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", finalURL, resp.StatusCode)
	}
	total := resp.ContentLength

	part := dest + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", part, err)
	}
	var n, lastSizeEmit int64
	lastTimeEmit := time.Now()
	buf := make([]byte, 1<<20)
	for {
		if ctx.Err() != nil {
			f.Close()
			_ = os.Remove(part)
			return ctx.Err()
		}
		nr, er := resp.Body.Read(buf)
		if nr > 0 {
			nw, ew := f.Write(buf[:nr])
			if ew != nil {
				f.Close()
				_ = os.Remove(part)
				return fmt.Errorf("write %s: %w", part, ew)
			}
			n += int64(nw)
			if onBytes != nil && (n-lastSizeEmit >= progressEmitBytes || time.Since(lastTimeEmit) >= progressEmitInterval) {
				lastSizeEmit = n
				lastTimeEmit = time.Now()
				onBytes("downloading", fmt.Sprintf("已下载 %s", formatBytes(n)), downloadPercent(n, total), n, total)
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			f.Close()
			_ = os.Remove(part)
			return fmt.Errorf("download %s: %w", finalURL, er)
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("close %s: %w", part, err)
	}
	// Truncation guard: a clean EOF before ContentLength bytes means the stream
	// was cut mid-transfer (proxy drop / CDN RST_STREAM) — the plain io.Copy
	// loop treated that as success and the short archive only surfaced as a
	// damaged-gzip error afterwards.
	if total > 0 && n != total {
		_ = os.Remove(part)
		return fmt.Errorf("下载被截断：得到 %s / 期望 %s", formatBytes(n), formatBytes(total))
	}
	if err := os.Rename(part, dest); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("finalize %s: %w", dest, err)
	}
	if onBytes != nil {
		onBytes("downloading", fmt.Sprintf("下载完成 %s", formatBytes(n)), downloadPercent(n, total), n, total)
	}
	return nil
}

// downloadPercent maps the download loop's byte count onto 5-90 of the overall
// upgrade progress: 0-4 is the dispatch/discovery head, 91+ is verify/extract/
// activate. total<=0 (no Content-Length) yields 0 — the UI shows bytes only.
func downloadPercent(cur, total int64) int {
	if total <= 0 || cur <= 0 {
		return 0
	}
	pct := 5 + int(90*cur/total)
	if pct > 90 {
		pct = 90
	}
	return pct
}
