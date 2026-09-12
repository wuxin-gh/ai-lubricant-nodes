package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestProbeIOSSDKParsesShowsdks pins the -showsdks parser: the device SDK line
// shape is " iphoneos17.2 - iOS 17.2 (iphoneos17.2)" (leading space, no "-SDK"
// prefix) while simulators are "iphonesimulator17.2 …" — the probe must pick
// the device line only, and tolerate the platform-missing case (no iphoneos
// line at all → ""), which is the "缺 iOS 平台组件" signal the build tab gates on.
func TestProbeIOSSDKParsesShowsdks(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "device sdk present",
			out: `iOS SDKs:
	iOS 17.2                        -sdk iphoneos17.2
	iOS Simulator SDKs:
	Simulator - iOS 17.2            -sdk iphonesimulator17.2`,
			// The -showsdks "iPhoneOS" section shape (Xcode 15):
			want: "17.2",
		},
		{
			name: "bare iphoneos name line",
			out: `iOS SDKs:
	iphoneos17.4                    - iOS 17.4 (iphoneos17.4)`,
			want: "17.4",
		},
		{
			name: "platform missing (only simulator)",
			out: `iOS Simulator SDKs:
	Simulator - iOS 17.2            -sdk iphonesimulator17.2`,
			want: "",
		},
		{name: "empty output", out: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// probeIOSSDK calls probeCommandOutput which runs the real binary;
			// extract the parsing core via a test seam: feed output directly.
			got := parseIOSSDKFromShowsdks(tc.out)
			if got != tc.want {
				t.Errorf("parseIOSSDKFromShowsdks(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

// TestProbeIOSSDKAbsentBinary ensures a host without xcodebuild reports "" and
// no iOS-SDK label. The probe must fall through every strategy and return ""
// rather than panic; here every command fails and no Xcode.app exists, so the
// filesystem strategy also finds nothing.
func TestProbeIOSSDKAbsentBinary(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("DEVELOPER_DIR", "")
	if v := probeIOSSDK(context.Background()); v != "" {
		t.Errorf("expected empty SDK on a PATH-less host, got %q", v)
	}
	_ = os.Environ() // keep the import if the compiler trims it otherwise
}

// TestProbeIOSSDKFromDisk pins the filesystem fallback: a fake Xcode.app bundle
// with an iPhoneOS17.2.sdk directory must report "17.2" even when xcodebuild /
// xcrun are unreachable (no PATH). This is the strategy that makes the label
// correct regardless of the node process's toolchain wiring.
func TestProbeIOSSDKFromDisk(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("DEVELOPER_DIR", "")
	// Fake an Xcode.app tree under HOME so discoverXcodeApps finds it via the
	// ~/Downloads root (see the home-scanning roots there).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME")) // Windows os.UserHomeDir reads USERPROFILE
	downloads := filepath.Join(os.Getenv("HOME"), "Downloads")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatal(err)
	}
	sdkDir := filepath.Join(downloads, "Xcode.app", "Contents", "Developer",
		"Platforms", "iPhoneOS.platform", "Developer", "SDKs", "iPhoneOS17.2.sdk")
	if err := os.MkdirAll(sdkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// discoverXcodeApps uses os.UserHomeDir; HomeDir honors $HOME on unix.
	if got := probeIOSSDK(context.Background()); got != "17.2" {
		t.Errorf("filesystem fallback = %q, want 17.2", got)
	}
}
