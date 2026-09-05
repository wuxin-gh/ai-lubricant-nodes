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
package main

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
	"github.com/danielpaulus/go-ios/ios/signing"
	"github.com/danielpaulus/go-ios/ios/zipconduit"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
	"device-control/ios/devicecontrol"
)

// downloadResolver supplies the node's egress-proxy-aware download route.
// *agent.Client implements it; nil means download directly with no URL rewrite.
type downloadResolver interface {
	ResolveDownloadURL(string) string
	DownloadHTTPClient(time.Duration) (*http.Client, error)
}

// goiosWdaSteps is the production WdaSteps.
type goiosWdaSteps struct {
	logger logger
	// httpClient downloads artifacts when no resolver is wired (direct egress).
	// Bounded timeout: a WDA .ipa is tens of MB.
	httpClient *http.Client
	// dl, when set, routes Fetch through the node's egress proxy (URL rewrite +
	// proxy transport) — the same download path self-upgrade takes for release
	// assets. Read per fetch, so a runtime proxy-config update applies.
	dl downloadResolver
}

// newGoiosWdaSteps builds the production pipeline. dl may be nil (direct).
func newGoiosWdaSteps(log logger, dl downloadResolver) *goiosWdaSteps {
	return &goiosWdaSteps{
		logger:     log,
		httpClient: &http.Client{Timeout: 15 * time.Minute},
		dl:         dl,
	}
}

// artifactSizeLimit bounds a downloaded artifact. A WDA runner is ~10-40 MB;
// 512 MB is generous while still refusing a mis-pointed URL that streams
// forever.
const artifactSizeLimit = 512 << 20

// Fetch downloads the artifact and verifies its sha256 before it ever reaches
// the device. An unpinned or mismatched artifact is refused: this is the one
// gate between "the server named a URL" and "we install code on a phone".
func (s *goiosWdaSteps) Fetch(ctx context.Context, art *agentcomposev2.NodeIosWdaArtifact, workDir string, progress func(int)) (string, error) {
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
func (s *goiosWdaSteps) EnableDeveloperMode(ctx context.Context, udid string) error {
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
func (s *goiosWdaSteps) PrepareSigning(ctx context.Context, req *agentcomposev2.NodeIosWdaJobRequest, workDir string, stage func(agentcomposev2.IosJobStage)) (SigningAssets, error) {
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
		bundleID := firstNonEmpty(req.GetArtifact().GetTargetBundleId(), "com.devicecontrol.WebDriverAgentRunner")
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
func (s *goiosWdaSteps) Sign(ctx context.Context, artifactPath string, assets SigningAssets, targetBundleID, workDir string) (string, error) {
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
func (s *goiosWdaSteps) Install(ctx context.Context, udid, signedPath string) error {
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

// Launch starts the runner through the device-control link (which owns the
// testmanagerd + port-forward lifecycle), waits for WDA to answer, and runs a
// control smoke test. Only if all of that passes does the caller mark the
// device ready — an installed-but-unreachable WDA must never look "ready".
func (s *goiosWdaSteps) Launch(ctx context.Context, udid, bundleID, xctestConfig string, stage func(agentcomposev2.IosJobStage)) (int, error) {
	if bundleID == "" {
		return 0, errors.New("launch: WDA bundle id is empty")
	}
	if xctestConfig == "" {
		xctestConfig = "WebDriverAgentRunner.xctest"
	}
	stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_STARTING_RUNNER)

	link := devicecontrol.NewLink(devicecontrol.Options{
		UDID:         udid,
		WDABundleID:  bundleID,
		XCTestConfig: xctestConfig,
	})
	stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_FORWARDING_PORT)
	stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_WAITING_READY)
	if err := link.Start(ctx); err != nil {
		if isDeveloperModeError(err) {
			return 0, fmt.Errorf("%w: %v", errDeveloperModeRequired, err)
		}
		return 0, err
	}
	defer link.Close()

	// Smoke test: a real screen read proves the whole chain (runner → forward →
	// HTTP → session → accessibility tree), not just that a port is open.
	stage(agentcomposev2.IosJobStage_IOS_JOB_STAGE_VERIFYING_CONTROL)
	if _, err := link.Source(ctx); err != nil {
		return 0, fmt.Errorf("wda control check: %w", err)
	}
	return 0, nil
}

// ── helpers ──────────────────────────────────────────────────────────────

// deviceByUDID resolves a UDID to a go-ios device entry, distinguishing "not
// attached" so the job engine can mark the failure retryable.
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
var _ WdaSteps = (*goiosWdaSteps)(nil)
