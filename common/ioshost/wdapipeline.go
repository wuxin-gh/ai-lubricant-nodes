// Production implementation of the WDA pipeline steps, backed by go-ios.
//
// This is the file that actually touches Apple: it downloads the approved
// artifact, provisions signing assets through the App Store Connect API,
// re-signs the runner with go-codesign (pure Go — no Xcode, no macOS), installs
// via zipconduit, then launches the XCTest runner and waits for WebDriverAgent
// to answer.
//
// REAL-DEVICE GATE: every go-ios call here is implemented against the v1.3.2
// API but has not run against a physical iPhone or a live App Store Connect
// account in this project. The WdaSteps seam exists so the job state machine is
// fully unit-tested without them; what a device/account proves is the behaviour
// of these calls, not the pipeline's control flow.
//
// SECURITY: signing material is written 0600 into the job work dir and deleted
// with it. Nothing here logs a key, a password, or a profile body.
package ioshost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aluedeke/go-codesign/pkg/codesign"
	ios "github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/amfi"
	"github.com/danielpaulus/go-ios/ios/installationproxy"
	"github.com/danielpaulus/go-ios/ios/signing"
	"github.com/danielpaulus/go-ios/ios/zipconduit"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
	"device-control/ios/devicecontrol"
)

// Default automation runner. go-ios 1.3.2 ships DeviceKit as the default UI
// backend; the legacy WebDriverAgent bundle is kept as a documented fallback
// (override the artifact's target_bundle_id / xctest_config_name from the
// server to ship a WDA runner ipa instead).
const (
	defaultDeviceKitBundleID = "com.deviceboxhq.goios.devicekit.runner"
	defaultDeviceKitXctest   = "devicekit-iosUITests.xctest"
)

// DownloadResolver supplies the node's egress-proxy-aware download route.
// *agent.Client implements it; nil means download directly with no URL rewrite.
type DownloadResolver interface {
	ResolveDownloadURL(string) string
	DownloadHTTPClient(time.Duration) (*http.Client, error)
	// DirectHTTPClient returns a plain direct-egress client, used as the
	// fallback when the proxy mangles the download. Optional in practice:
	// callers that only need the proxy path may omit it (duck-typed).
	DirectHTTPClient(time.Duration) (*http.Client, error)
}

// GoiosWdaSteps is the production WdaSteps.
type GoiosWdaSteps struct {
	Logger Logger
	// httpClient downloads artifacts when no resolver is wired (direct egress).
	// Bounded timeout: a WDA .ipa is tens of MB.
	httpClient *http.Client
	// dl, when set, routes Fetch through the node's egress proxy (URL rewrite +
	// proxy transport) — the same download path self-upgrade takes for release
	// assets. Read per fetch, so a runtime proxy-config update applies.
	dl DownloadResolver
	// cacheDir, when set, persists downloaded+verified artifacts keyed by
	// sha256, so repeated prepare/renew/reinstall on one host never re-download
	// (and never re-expose themselves to a flaky egress proxy). Owned by the
	// host: the node passes its ios state dir.
	cacheDir string
}

// NewGoiosWdaSteps builds the production pipeline. dl may be nil (direct).
func NewGoiosWdaSteps(log Logger, dl DownloadResolver) *GoiosWdaSteps {
	return &GoiosWdaSteps{
		Logger:     log,
		httpClient: &http.Client{Timeout: 15 * time.Minute},
		dl:         dl,
	}
}

// WithArtifactCache enables the verified-artifact cache under dir. Chainable.
func (s *GoiosWdaSteps) WithArtifactCache(dir string) *GoiosWdaSteps {
	s.cacheDir = dir
	return s
}

// directHTTPClient is the no-proxy fallback client used when the egress route
// mangles the download (observed: a local MITM proxy answering HTTP/2 frames
// Go's HTTP/1 transport rejects). Direct egress is the safety net.
func directHTTPClient(timeout time.Duration) (*http.Client, error) {
	return &http.Client{Timeout: timeout}, nil
}

// cachedArtifact returns the cached copy for the digest, if one exists AND its
// on-disk sha256 still matches (a truncated/interrupted write must never be
// served). The copy is linked into workDir under its real name, because the
// signing stage reads the artifact from there.
func (s *GoiosWdaSteps) cachedArtifact(sha string) (string, bool) {
	if s.cacheDir == "" {
		return "", false
	}
	cached := filepath.Join(s.cacheDir, "artifacts", sha+".ipa")
	f, err := os.Open(cached)
	if err != nil {
		return "", false
	}
	defer f.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, f); err != nil {
		return "", false
	}
	if hex.EncodeToString(hasher.Sum(nil)) != sha {
		// Corrupt cache entry: drop it so the next attempt re-downloads.
		_ = os.Remove(cached)
		return "", false
	}
	return cached, true
}

// storeCachedArtifact persists a verified download into the cache dir. The
// file is already sha256-verified by the fetcher; a best-effort rename keeps
// partial writes out.
func (s *GoiosWdaSteps) storeCachedArtifact(sha, src string) {
	if s.cacheDir == "" || src == "" {
		return
	}
	dir := filepath.Join(s.cacheDir, "artifacts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	dst := filepath.Join(dir, sha+".ipa")
	if src == dst {
		return
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return
	}
}

// artifactSizeLimit bounds a downloaded artifact. A WDA runner is ~10-40 MB;
// 512 MB is generous while still refusing a mis-pointed URL that streams
// forever.
const artifactSizeLimit = 512 << 20

// Fetch downloads the artifact and verifies its sha256 before it ever reaches
// the device. An unpinned or mismatched artifact is refused: this is the one
// gate between "the server named a URL" and "we install code on a phone".
func (s *GoiosWdaSteps) Fetch(ctx context.Context, art *agentcomposev2.NodeIosWdaArtifact, workDir string, progress func(int)) (string, error) {
	fetchURL := strings.TrimSpace(art.GetUrl())
	if fetchURL == "" {
		return "", errors.New("artifact url is empty")
	}
	want := strings.ToLower(strings.TrimSpace(art.GetSha256()))
	if want == "" {
		// Refuse rather than trust: without a digest we cannot tell the approved
		// artifact from a substituted one.
		return "", errors.New("artifact sha256 is required")
	}

	// Cache hit: the same artifact (identical sha256) downloaded once before is
	// returned without touching the network. The runner IPA is identical for
	// every device, so repeated prepares/renews/reinstalls on one host never
	// re-download it — and never re-expose themselves to a flaky egress proxy.
	if cached, ok := s.cachedArtifact(want); ok {
		if progress != nil {
			progress(100)
		}
		return cached, nil
	}

	dest, err := s.fetchThroughProxy(ctx, art, fetchURL, want, workDir, progress)
	if err != nil {
		// The egress proxy can garble the stream (observed: a local Clash
		// answering with raw HTTP/2 frames that Go's HTTP/1 transport rejects —
		// "malformed HTTP response \x00\x00\x12\x04..."). The artifact is also
		// fetchable directly, so fall back once: the proxy stays first-choice
		// (some hosts have no direct egress), the direct path is the safety net.
		if direct, derr := directHTTPClient(15 * time.Minute); derr == nil {
			if dest2, ferr := s.fetchWithClient(ctx, direct, strings.TrimSpace(art.GetUrl()), want, workDir, progress); ferr == nil {
				dest, err = dest2, nil
			}
		}
	}
	if err != nil {
		return "", err
	}
	s.storeCachedArtifact(want, dest)
	return dest, nil
}

// fetchThroughProxy downloads via the egress-proxy client (or directly when no
// resolver is wired), verifying the sha256.
func (s *GoiosWdaSteps) fetchThroughProxy(ctx context.Context, art *agentcomposev2.NodeIosWdaArtifact, fetchURL, want, workDir string, progress func(int)) (string, error) {
	client := s.httpClient
	if s.dl != nil {
		// Route through the node's egress proxy (URL rewrite + transport), the
		// same download path self-upgrade uses for release assets.
		fetchURL = s.dl.ResolveDownloadURL(fetchURL)
		proxyClient, err := s.dl.DownloadHTTPClient(15 * time.Minute)
		if err != nil {
			return "", err
		}
		client = proxyClient
	}
	return s.fetchWithClient(ctx, client, fetchURL, want, workDir, progress)
}

// fetchWithClient streams the artifact through the given client, verifying the
// digest before the file counts as downloaded.
func (s *GoiosWdaSteps) fetchWithClient(ctx context.Context, client *http.Client, fetchURL, want, workDir string, progress func(int)) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download artifact: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("download artifact: HTTP %d", resp.StatusCode)
	}

	dest := filepath.Join(workDir, "wda-artifact"+artifactExt(fetchURL))
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hasher := sha256.New()
	total := resp.ContentLength
	var written int64
	buf := make([]byte, 256<<10)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > artifactSizeLimit {
				return "", fmt.Errorf("artifact exceeds %d bytes", int64(artifactSizeLimit))
			}
			if _, err := f.Write(buf[:n]); err != nil {
				return "", err
			}
			hasher.Write(buf[:n])
			if progress != nil && total > 0 {
				progress(int(written * 100 / total))
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("download artifact: %w", readErr)
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != want {
		// errSHAMismatch is what the job engine classifies as a terminal,
		// non-retryable supply-chain failure.
		return "", fmt.Errorf("%w: got %s", errSHAMismatch, got)
	}
	return dest, nil
}

func artifactExt(url string) string {
	lower := strings.ToLower(url)
	switch {
	case strings.HasSuffix(lower, ".ipa"):
		return ".ipa"
	case strings.HasSuffix(lower, ".zip"):
		return ".zip"
	default:
		return ".ipa"
	}
}

// EnableDeveloperMode asks the device to turn Developer Mode on (iOS 16+). The
// device shows its own confirmation and reboots, so success here means
// "requested", not "enabled" — the launch stage is what proves it took.
func (s *GoiosWdaSteps) EnableDeveloperMode(ctx context.Context, udid string) error {
	entry, err := deviceByUDID(udid)
	if err != nil {
		return err
	}
	// enablePostRestart=true so go-ios also runs the post-reboot confirmation
	// step when the device comes back.
	return amfi.EnableDeveloperMode(entry, true)
}

// PrepareSigning provisions the signing assets for one device.
//
// Paid App Store Connect path (the only automatable one): register the UDID,
// reuse the account's existing certificate when the server passed one, mint a
// per-device development profile, and hand back the p12 + profile.
//
// The presigned path short-circuits: the operator already signed the artifact
// (free Apple ID), so there is nothing to provision.
func (s *GoiosWdaSteps) PrepareSigning(ctx context.Context, req *agentcomposev2.NodeIosWdaJobRequest, workDir string, stage func(agentcomposev2.IosJobStage)) (SigningAssets, error) {
	mat := req.GetSigning()
	mode := mat.GetMode()
	if req.GetAction() == agentcomposev2.IosWdaJobAction_IOS_WDA_JOB_ACTION_INSTALL_SIGNED ||
		mode == agentcomposev2.IosSigningMode_IOS_SIGNING_MODE_PRESIGNED {
		return SigningAssets{Presigned: true}, nil
	}
	if mat == nil {
		return SigningAssets{}, errNoSigningMaterial
	}

	switch mode {
	case agentcomposev2.IosSigningMode_IOS_SIGNING_MODE_MANUAL_P12:
		// Operator-supplied identity: write both files 0600 and sign with them.
		if len(mat.GetCertificateP12()) == 0 || len(mat.GetProvisioningProfile()) == 0 {
			return SigningAssets{}, errNoSigningMaterial
		}
		p12Path := filepath.Join(workDir, "identity.p12")
		profilePath := filepath.Join(workDir, "profile.mobileprovision")
		if err := os.WriteFile(p12Path, mat.GetCertificateP12(), 0o600); err != nil {
			return SigningAssets{}, err
		}
		if err := os.WriteFile(profilePath, mat.GetProvisioningProfile(), 0o600); err != nil {
			return SigningAssets{}, err
		}
		return SigningAssets{
			P12Path:          p12Path,
			ProfilePath:      profilePath,
			P12Password:      mat.GetP12Password(),
			CertificateID:    mat.GetCertificateId(),
			ProfileExpiresAt: profileExpiry(mat.GetProvisioningProfile()),
		}, nil

	case agentcomposev2.IosSigningMode_IOS_SIGNING_MODE_APP_STORE_CONNECT:
		if mat.GetAscKeyId() == "" || mat.GetAscIssuerId() == "" || len(mat.GetAscPrivateKey()) == 0 {
			return SigningAssets{}, errNoSigningMaterial
		}
		entry, err := deviceByUDID(req.GetUdid())
		if err != nil {
			return SigningAssets{}, err
		}
		creds := signing.AppStoreConnectCredentials{
			KeyID:      mat.GetAscKeyId(),
			IssuerID:   mat.GetAscIssuerId(),
			PrivateKey: mat.GetAscPrivateKey(),
		}
		bundleID := firstNonEmpty(req.GetArtifact().GetTargetBundleId(), defaultDeviceKitBundleID)
		p12Path := filepath.Join(workDir, "identity.p12")
		profilePath := filepath.Join(workDir, "profile.mobileprovision")
		// Password protects the p12 at rest inside the job work dir only; the
		// file is deleted with the dir when the job ends.
		p12Password := mat.GetP12Password()
		if p12Password == "" {
			p12Password = "wda-job"
		}

		stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_ENSURING_DEVICE)
		opts := signing.PrepareAssetsOptions{
			BundleID:    bundleID,
			ProfileName: "device-control WDA " + shortUDID(req.GetUdid()),
			DeviceName:  "device-control " + shortUDID(req.GetUdid()),
			P12Password: p12Password,
			P12Output:   p12Path,
			ProfileOut:  profilePath,
			Credentials: creds,
			Device:      entry,
			// Reuse the account certificate when the server has one: Apple
			// allows a single current iOS Development certificate, so a fleet
			// must share it — only the per-device profile is new.
			CertificateID: mat.GetCertificateId(),
		}
		stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_ENSURING_BUNDLE_ID)
		if mat.GetCertificateId() == "" {
			stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_PREPARING_CERTIFICATE)
		}
		stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_CREATING_PROFILE)
		res, err := signing.PrepareSigningAssets(ctx, opts)
		if err != nil {
			if isUnauthorized(err) {
				return SigningAssets{}, fmt.Errorf("%w: %v", errASCUnauthorized, err)
			}
			return SigningAssets{}, err
		}
		profileBytes, _ := os.ReadFile(res.ProfilePath)
		return SigningAssets{
			P12Path:          res.P12Path,
			ProfilePath:      res.ProfilePath,
			P12Password:      p12Password,
			CertificateID:    res.CertificateID,
			ProfileExpiresAt: profileExpiry(profileBytes),
		}, nil

	default:
		// An unset mode with material present is a server bug; an unset mode
		// with no material means the operator has to sign by hand.
		return SigningAssets{}, errManualSigningRequired
	}
}

// Sign re-signs the artifact with the prepared identity, rewriting the bundle
// id so a free/dev profile can host it.
func (s *GoiosWdaSteps) Sign(ctx context.Context, artifactPath string, assets SigningAssets, targetBundleID, workDir string) (string, error) {
	if assets.Presigned {
		return artifactPath, nil
	}
	out := filepath.Join(workDir, "wda-signed.ipa")
	res, err := signing.SignWithFiles(signing.SignWithFilesOptions{
		AppPath:     artifactPath,
		OutputPath:  out,
		BundleID:    targetBundleID,
		P12Path:     assets.P12Path,
		P12Password: assets.P12Password,
		ProfilePath: assets.ProfilePath,
	})
	if err != nil {
		return "", err
	}
	return res.OutputPath, nil
}

// Install pushes the signed artifact onto the device over zipconduit (the same
// service Xcode uses; no Apple Configurator or macOS needed).
func (s *GoiosWdaSteps) Install(ctx context.Context, udid, signedPath string) error {
	entry, err := deviceByUDID(udid)
	if err != nil {
		return err
	}
	conn, err := zipconduit.New(entry)
	if err != nil {
		return fmt.Errorf("zipconduit connect: %w", err)
	}
	defer conn.Close()
	if err := conn.SendFile(signedPath); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	return nil
}

// IsInstalled reports whether the runner bundle is already installed on the
// device, by browsing user apps over the installation proxy. Used to skip the
// download→sign→install half on a re-prepare of an already-provisioned device.
//
// A browse failure is returned as an error (not a bare false): the caller must
// fall back to the full pipeline rather than skip an install it cannot confirm.
func (s *GoiosWdaSteps) IsInstalled(ctx context.Context, udid, bundleID string) (bool, error) {
	if bundleID == "" {
		return false, errors.New("is-installed: bundle id is empty")
	}
	entry, err := deviceByUDID(udid)
	if err != nil {
		return false, err
	}
	conn, err := installationproxy.New(entry)
	if err != nil {
		return false, fmt.Errorf("is-installed: connect installation proxy: %w", err)
	}
	defer conn.Close()
	apps, err := conn.BrowseUserApps()
	if err != nil {
		return false, fmt.Errorf("is-installed: browse user apps: %w", err)
	}
	for _, app := range apps {
		if app.CFBundleIdentifier() == bundleID {
			return true, nil
		}
	}
	return false, nil
}

// Launch starts the runner through the device-control link (which owns the
// testmanagerd + port-forward lifecycle), waits for WDA to answer, and runs a
// control smoke test. Only if all of that passes does the caller mark the
// device ready — an installed-but-unreachable WDA must never look "ready".
func (s *GoiosWdaSteps) Launch(ctx context.Context, udid, bundleID, xctestConfig string, stage func(agentcomposev2.IosJobStage)) (int, error) {
	if bundleID == "" {
		return 0, errors.New("launch: runner bundle id is empty")
	}
	if xctestConfig == "" {
		xctestConfig = defaultDeviceKitXctest
	}
	stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_STARTING_RUNNER)

	stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_FORWARDING_PORT)
	stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_WAITING_READY)
	// Retry on transient usbmuxd/AMDS failures: on Windows the Apple Mobile
	// Device Service intermittently drops a connection under concurrent load
	// (reproduced ~1-in-8 even from a bare probe). One such blip during launch
	// was the whole reason a freshly-installed runner reported "wda_unreachable".
	// A FRESH Link per attempt: Start tears down everything it opened before
	// returning an error, but is only guaranteed retryable on a new Link. The
	// successful link is kept alive for the smoke test and closed on return.
	// Developer-mode / non-transient errors stay terminal.
	var launchErr error
	success := false
	var liveLink *devicecontrol.Link
	for attempt := 1; attempt <= 3 && !success; attempt++ {
		link := devicecontrol.NewLink(devicecontrol.Options{
			UDID:         udid,
			WDABundleID:  bundleID,
			XCTestConfig: xctestConfig,
		})
		err := link.Start(ctx)
		if err == nil {
			success = true
			liveLink = link
			// Keep the successful link alive for the smoke test; Close runs after
			// Launch returns (captured so the loop variable doesn't alias).
			defer func(l *devicecontrol.Link) {
				_ = l.Close()
			}(link)
			break
		}
		_ = link.Close() // no-op when Start already tore down, safe to repeat
		if isDeveloperModeError(err) {
			return 0, fmt.Errorf("%w: %v", errDeveloperModeRequired, err)
		}
		if !isTransientUsbmuxdError(err) {
			return 0, err
		}
		launchErr = err
		// Back off 2s, then 4s — a retry must not add to the AMDS contention
		// that caused the failure in the first place.
		backoff := time.Duration(1<<attempt) * time.Second // 2s, 4s
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(backoff):
		}
	}
	if !success {
		return 0, launchErr
	}

	// Smoke test: a real screen read proves the whole chain (runner → forward →
	// control RPC → accessibility tree), not just that a port is open.
	stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_VERIFYING_CONTROL)
	if _, err := liveLink.Source(ctx); err != nil {
		return 0, fmt.Errorf("runner control check: %w", err)
	}
	return 0, nil
}

// isTransientUsbmuxdError reports whether a launch error looks like a transient
// usbmuxd/AMDS failure worth retrying rather than a terminal setup problem.
// Kept conservative: any connectex timeout, "did not properly respond", i/o
// timeout, EOF on a dial, or a closed/reset connection. A real app crash or
// signature rejection surfaces a different error shape and is NOT retried.
func isTransientUsbmuxdError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connectex") ||
		strings.Contains(msg, "did not properly respond") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "no connection could be made") ||
		strings.Contains(msg, "eof") {
		return true
	}
	return false
}

// ── helpers ──────────────────────────────────────────────────────────────

// deviceByUDID resolves a UDID to a go-ios device entry, distinguishing "not
// attached" so the job engine can mark the failure retryable.
//
// USB first (zero behaviour change for a plugged phone). When usbmuxd does not
// know the device, fall back to the LAN entry: an unplugged phone the node's
// LAN tunnel agent has an RSD tunnel for is reachable through the tunnel —
// zipconduit (install) and installationproxy (list) both have shim paths for a
// DeviceID==0 + RSD entry. No tunnel → the original not-present error, so the
// job still parks in waitForDevice rather than failing deep in install.
func deviceByUDID(udid string) (ios.DeviceEntry, error) {
	list, err := ios.ListDevices()
	if err != nil {
		return ios.DeviceEntry{}, fmt.Errorf("list devices: %w", err)
	}
	for _, d := range list.DeviceList {
		if d.Properties.SerialNumber == udid {
			return d, nil
		}
	}
	// LAN fallback: an unplugged phone with a live tunnel is reachable here.
	// No tunnel → the original not-present error, so the job parks in
	// waitForDevice instead of failing deep in install.
	if entry, err := devicecontrol.LanDeviceEntry(udid); err == nil {
		return entry, nil
	}
	return ios.DeviceEntry{}, fmt.Errorf("%w: udid %s", errDeviceNotPresent, udid)
}

// profileExpiry parses a provisioning profile's ExpirationDate. Returns "" when
// the profile cannot be parsed — the caller then reports an unknown expiry
// rather than claiming a wrong one.
func profileExpiry(profile []byte) string {
	if len(profile) == 0 {
		return ""
	}
	p, err := codesign.ParseProvisioningProfile(profile)
	if err != nil {
		return ""
	}
	if p.ExpirationDate.IsZero() {
		return ""
	}
	return p.ExpirationDate.UTC().Format(time.RFC3339)
}

func shortUDID(udid string) string {
	if len(udid) <= 8 {
		return udid
	}
	return udid[len(udid)-8:]
}

func isUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "401") || strings.Contains(msg, "403") ||
		strings.Contains(msg, "unauthorized") || strings.Contains(msg, "forbidden")
}

func isDeveloperModeError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "developer mode") || strings.Contains(msg, "developermode")
}

// compile-time check.
var _ WdaSteps = (*GoiosWdaSteps)(nil)
