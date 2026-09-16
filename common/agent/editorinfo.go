package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// SupportedEditors is the set of provider CLIs this system can install,
// upgrade, and report versions for.
var SupportedEditors = []string{"claude", "codex", "gemini", "opencode", "cursor", "dsh"}

// HostTools are non-editor CLIs the node probes and reports as capability
// labels (git / node / npm / agent-compose-runtime / ocr). Each entry maps the
// capability suffix to the probe command. A missing binary is reported as an
// absent label (not an error) so a node without OCR still registers and can
// run ordinary tasks — Review capability gating happens server-side.
var hostTools = []struct {
	suffix string
	cmd    string
	args   []string
}{
	{"git", "git", []string{"--version"}},
	{"node", "node", []string{"--version"}},
	{"npm", "npm", []string{"--version"}},
	{"runtime", "agent-compose-runtime", []string{"--version"}},
	{"ocr", "ocr", []string{"--version"}},
	// xcodebuild（macOS 专属，随 Xcode 安装）：项目页「构建」tab 按
	// xcodebuild_version 标签筛选构建节点。非 macOS LookPath 落空 → 自然缺席。
	{"xcodebuild", "xcodebuild", []string{"-version"}},
}

// HostToolLabels probes each host tool and returns labels like
// “git_version“/“node_version“/“ocr_version“. Best-effort: a tool that
// is not on PATH or fails to report a version is simply omitted.
//
// On Windows the node is launched by a per-user ONLOGON scheduled task, which
// inherits a PATH that often omits Node.js/npm install locations (Program Files
// Node.js, %APPDATA%\npm, nvm shims). “LookPath“ would miss them, so the node
// would register reporting no node_version/npm_version and the approve gate
// would deadlock even though Node.js is installed. We therefore probe a small
// set of well-known locations before giving up; the first hit that prints a
// version wins.
func HostToolLabels(ctx context.Context) map[string]string {
	labels := map[string]string{}
	for _, tool := range hostTools {
		if tool.suffix == "runtime" {
			if version := ManagedRuntimeVersion(ctx); version != "" {
				labels["runtime_version"] = version
				continue
			}
		}
		if version := probeHostTool(ctx, tool.cmd, tool.args...); version != "" {
			labels[tool.suffix+"_version"] = version
			// xcodebuild itself being present must not skip the iOS platform
			// probe below: Xcode can be installed while iphoneos SDK support is
			// absent. Other host tools are done after their successful probe.
			if tool.suffix != "xcodebuild" {
				continue
			}
		}
		// xcodebuild only: a PATH miss usually means xcode-select points at
		// CLT (or nothing) while a full Xcode sits unselected in /Applications.
		// Same dance as the sync detect command — find it, activate its dev
		// dir, re-probe — so the register-time label reports an installed
		// Xcode without anyone clicking detect first. Activation persists
		// (EnsureDeveloperDir re-applies it across restarts), so this dance
		// runs at most once in a node's lifetime on a healthy host.
		if tool.suffix == "xcodebuild" {
			if version := ensureXcodebuildVisible(ctx); version != "" {
				labels["xcodebuild_version"] = version
			}
			// iOS build environment: xcodebuild alone is not enough to build
			// for a device — the iOS platform support (SDK + device support
			// files) is a separate component that ships missing on fresh
			// installs (Xcode 15+: "iOS 17.2 is not installed. To use with
			// Xcode, first download and install the platform"). Probe it as
			// its own label so the build tab can gate on "has Xcode AND has
			// the iOS platform" and tell the operator exactly which piece a
			// node is missing (fix: xcodebuild -downloadPlatform iOS).
			// probeIOSSDK tries several detection strategies in order — a
			// command failing or having a different output shape must never
			// read as "platform not installed".
			if sdk := probeIOSSDK(ctx); sdk != "" {
				labels["xcode_ios_sdk"] = sdk
			}
		}
	}
	return labels
}

// probeIOSSDK reports the installed iOS device SDK version, e.g. "17.2".
// Empty means every detection strategy ran and none found a device SDK.
//
// Strategies, most reliable first — each wraps the previous, so one tool
// misbehaving (missing PATH entry, changed output shape, timeout) falls
// through to the next instead of flipping the label to "missing":
//
//  1. `xcodebuild -showsdks` and parse the "-sdk iphoneos<ver>" token.
//  2. `xcrun --sdk iphoneos --show-sdk-version` — asks the toolchain directly;
//     xcrun resolves the developer dir itself, so it works even when the
//     xcodebuild shim is awkward.
//  3. Filesystem: glob "<devdir>/Platforms/iPhoneOS.platform/Developer/SDKs/
//     iPhoneOS<ver>.sdk" — the SDK is a physical directory, so no command at
//     all is needed; the developer dir comes from DEVELOPER_DIR, then the
//     discovered Xcode apps, then the /usr/bin shim's default location.
func probeIOSSDK(ctx context.Context) string {
	if sdk := parseIOSSDKFromShowsdks(probeCommandOutput(ctx, "xcodebuild", "-showsdks")); sdk != "" {
		return sdk
	}
	if out := probeCommandOutput(ctx, "xcrun", "--sdk", "iphoneos", "--show-sdk-version"); out != "" {
		if match := iosSDKVersionRe.FindString(out); match != "" {
			return match
		}
	}
	return iosSDKFromDisk()
}

// iosSDKFromDisk finds the iPhoneOS device SDK by looking at the filesystem
// (no subprocess): <devdir>/Platforms/iPhoneOS.platform/Developer/SDKs holds
// one iPhoneOS<ver>.sdk directory per installed platform.
func iosSDKFromDisk() string {
	for _, devDir := range candidateDeveloperDirs() {
		sdkDir := filepath.Join(devDir, "Platforms", "iPhoneOS.platform", "Developer", "SDKs")
		matches, err := filepath.Glob(filepath.Join(sdkDir, "iPhoneOS*.sdk"))
		if err != nil || len(matches) == 0 {
			continue
		}
		// Multiple SDKs: the platform keeps the latest as a real directory and
		// older ones as .simlink stubs; a symlinked duplicate is harmless — pick
		// the highest version seen.
		best := ""
		for _, m := range matches {
			match := iosSDKVersionRe.FindString(filepath.Base(m))
			if match != "" && (best == "" || sdkVersionAfter(match, best)) {
				best = match
			}
		}
		if best != "" {
			return best
		}
	}
	return ""
}

// candidateDeveloperDirs lists places the active developer directory can be,
// in priority order: $DEVELOPER_DIR, each discovered Xcode.app, then the
// standard default install.
func candidateDeveloperDirs() []string {
	var dirs []string
	if v := strings.TrimSpace(os.Getenv("DEVELOPER_DIR")); v != "" {
		dirs = append(dirs, v)
	}
	for _, app := range discoverXcodeApps() {
		dirs = append(dirs, filepath.Join(app, "Contents", "Developer"))
	}
	dirs = append(dirs, "/Applications/Xcode.app/Contents/Developer")
	return dedupeStrings(dirs)
}

// sdkVersionAfter reports whether version a sorts after b ("17.4" > "17.2").
func sdkVersionAfter(a, b string) bool {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var na, nb int
		if i < len(pa) {
			na = atoiSafe(pa[i])
		}
		if i < len(pb) {
			nb = atoiSafe(pb[i])
		}
		if na != nb {
			return na > nb
		}
	}
	return false
}

func atoiSafe(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// parseIOSSDKFromShowsdks extracts the iOS *device* SDK version from
// `xcodebuild -showsdks` output. Only the device SDK counts — the simulator SDK
// (iphonesimulator) installs with the base Xcode and is useless for a
// real-device build; a host with only the simulator SDK reports "".
// Known renderings (kept permissive — a formatting change must not flip the
// label to "missing"):
//
//	Xcode 15:  "        iOS 17.2                -sdk iphoneos17.2"
//	Xcode 16+: "        iOS 17.2                        -sdk iphoneos17.2"
//	older:     "        iphoneos17.2                - iOS 17.2 (iphoneos17.2)"
//
// The simulator line ("…-sdk iphonesimulator17.2") must never match.
func parseIOSSDKFromShowsdks(out string) string {
	if out == "" {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		// A line that mentions the simulator form is never the device entry.
		if strings.Contains(line, "iphonesimulator") {
			continue
		}
		for _, token := range strings.Fields(line) {
			// Canonical: "-sdk iphoneos<ver>".
			if strings.HasPrefix(token, "iphoneos") && len(token) > len("iphoneos") {
				if match := iosSDKVersionRe.FindString(token); match != "" {
					return match
				}
			}
			// Older rendering: "(iphoneos17.2)" — token carries parentheses.
			trimmed := strings.Trim(token, "()")
			if strings.HasPrefix(trimmed, "iphoneos") && len(trimmed) > len("iphoneos") {
				if match := iosSDKVersionRe.FindString(trimmed); match != "" {
					return match
				}
			}
		}
	}
	return ""
}

// iosSDKVersionRe matches the two-segment SDK version in an iphoneos<ver> token.
var iosSDKVersionRe = regexp.MustCompile(`\d+\.\d+`)

// probeCommandOutput runs cmd and returns its combined output ("" on error or
// empty), without the version-number extraction probeHostTool applies. A
// failure is just "this strategy found nothing" — the caller falls through to
// the next strategy (probeIOSSDK), never to a user-facing error.
func probeCommandOutput(ctx context.Context, cmd string, args ...string) string {
	path, err := LookPath(cmd)
	if err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ensureXcodebuildVisible runs the installed-but-unselected Xcode recovery:
// discoverXcodeApps → activateDeveloperDir → re-probe. Returns the probed
// version ("" when no Xcode could be made to run). Split from
// installXcodeDetection so the register-time label path can use it without
// the license-acceptance side effect.
func ensureXcodebuildVisible(ctx context.Context) string {
	if v := probeHostTool(ctx, "xcodebuild", "-version"); v != "" {
		return v
	}
	for _, app := range discoverXcodeApps() {
		devDir := filepath.Join(app, "Contents", "Developer")
		// A real Xcode carries its own xcodebuild inside the app bundle; the
		// /usr/bin shim alone proves nothing (it exists on CLT-only boxes too).
		if _, err := os.Stat(filepath.Join(devDir, "usr", "bin", "xcodebuild")); err != nil {
			continue
		}
		activateDeveloperDir(devDir, slog.Default())
		if v := probeHostTool(ctx, "xcodebuild", "-version"); v != "" {
			return v
		}
	}
	return ""
}

// probeHostTool finds the tool on PATH (with a Windows well-known-locations
// fallback for node/npm) and returns its version string. Empty on miss/failure.
func probeHostTool(ctx context.Context, cmd string, args ...string) string {
	if path, err := LookPath(cmd); err == nil {
		if v := runVersionCmd(ctx, path, args...); v != "" {
			return v
		}
	}
	for _, candidate := range wellKnownBinaryPaths(cmd) {
		if v := runVersionCmd(ctx, candidate, args...); v != "" {
			return v
		}
	}
	return ""
}

func runVersionCmd(ctx context.Context, exe string, args ...string) string {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, exe, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return ""
	}
	if match := versionNumberRe.FindString(raw); match != "" {
		return match
	}
	if idx := strings.IndexAny(raw, "\r\n"); idx >= 0 {
		raw = raw[:idx]
	}
	return strings.TrimSpace(raw)
}

// wellKnownBinaryPaths returns install-location candidates for a tool that
// LookPath may miss on Windows scheduled-task launches (node/npm). Empty on
// non-Windows or for tools without known locations.
func wellKnownBinaryPaths(cmd string) []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	localApp := os.Getenv("LOCALAPPDATA")
	appData := os.Getenv("APPDATA")
	programFiles := os.Getenv("ProgramFiles")
	programFilesX86 := os.Getenv("ProgramFiles(x86)")
	switch strings.ToLower(cmd) {
	case "node", "node.exe":
		var out []string
		for _, base := range []string{programFiles, programFilesX86} {
			if base == "" {
				continue
			}
			out = append(out, filepath.Join(base, "nodejs", "node.exe"))
		}
		if nvmHome := os.Getenv("NVM_HOME"); nvmHome != "" {
			out = append(out, filepath.Join(nvmHome, "node.exe"))
		}
		return out
	case "npm", "npm.cmd":
		var out []string
		if appData != "" {
			out = append(out, filepath.Join(appData, "npm", "npm.cmd"))
		}
		if localApp != "" {
			out = append(out, filepath.Join(localApp, "npm", "npm.cmd"))
		}
		if programFiles != "" {
			out = append(out, filepath.Join(programFiles, "nodejs", "npm.cmd"))
		}
		if programFilesX86 != "" {
			out = append(out, filepath.Join(programFilesX86, "nodejs", "npm.cmd"))
		}
		return out
	}
	return nil
}

var editorVersionArgs = map[string][]string{
	"claude": {"--version"}, "codex": {"--version"},
	"gemini": {"--version"}, "opencode": {"--version"},
	"cursor": {"--version"},
	"dsh":    {"--version"},
}

var versionNumberRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-.][0-9A-Za-z.]+)?`)

// editorBinary maps a provider onto the CLI binary name when they differ.
// Cursor's CLI is installed as “agent“ (per docs.cursor.com CLI installation).
var editorBinary = map[string]string{
	"cursor": "agent",
}

// EditorCommandName returns the executable name a provider's CLI is installed
// as (cursor's binary is “agent“; every other provider matches its name).
func EditorCommandName(editor string) string {
	if name, ok := editorBinary[editor]; ok {
		return name
	}
	return editor
}

func probeEditorVersion(ctx context.Context, editor string) string {
	args, ok := editorVersionArgs[editor]
	if !ok {
		return ""
	}
	command := EditorCommandName(editor)
	if _, err := LookPath(command); err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, command, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return ""
	}
	if match := versionNumberRe.FindString(raw); match != "" {
		return match
	}
	if idx := strings.IndexAny(raw, "\r\n"); idx >= 0 {
		raw = raw[:idx]
	}
	return strings.TrimSpace(raw)
}

func EditorLabels(ctx context.Context) map[string]string {
	labels := map[string]string{}
	for _, editor := range SupportedEditors {
		if version := probeEditorVersion(ctx, editor); version != "" {
			labels["editor_version_"+editor] = version
		}
	}
	return labels
}

// EditorCapabilities reports only modes confirmed by the installed CLI's own
// help/discovery output. Missing CLIs are omitted; failed probes advertise no
// modes, so scheduling never silently falls back to a broader permission set.
func EditorCapabilities(ctx context.Context) []*agentcomposev2.EditorCapability {
	result := make([]*agentcomposev2.EditorCapability, 0, len(SupportedEditors))
	for _, provider := range SupportedEditors {
		version := probeEditorVersion(ctx, provider)
		if version == "" {
			continue
		}
		capability := &agentcomposev2.EditorCapability{
			Provider: provider, Version: version, ProbeStatus: "ok",
			ProbedAt:            time.Now().UTC().Format(time.RFC3339),
			SupportsInteractive: provider == "codex",
			SupportsModelSwitch: true,
		}
		switch provider {
		case "opencode":
			capability.Modes = probeOpenCodeModes(ctx)
		case "claude":
			capability.Modes = probeNamedModes(ctx, provider, []string{"default", "plan", "acceptEdits", "bypassPermissions", "auto"})
		case "codex":
			capability.Modes = probeNamedModes(ctx, provider, []string{"read-only", "workspace-write", "auto", "danger-full-access"})
		case "gemini":
			capability.Modes = probeNamedModes(ctx, provider, []string{"default", "auto_edit", "yolo"})
		case "cursor":
			capability.Modes = probeCursorModes(ctx)
		case "dsh":
			capability.Modes = probeNamedModes(ctx, provider, []string{"default", "plan", "full"})
		}
		if len(capability.Modes) == 0 {
			capability.ProbeStatus = "partial"
			capability.ProbeError = "no confirmed permission modes"
		}
		result = append(result, capability)
	}
	return result
}

func probeNamedModes(ctx context.Context, provider string, candidates []string) []*agentcomposev2.EditorModeSpec {
	args := []string{"--help"}
	if provider == "codex" || provider == "gemini" {
		args = []string{"exec", "--help"}
	}
	output := probeCommand(ctx, provider, args...)
	if output == "" {
		return nil
	}
	modes := make([]*agentcomposev2.EditorModeSpec, 0, len(candidates))
	for _, id := range candidates {
		if !strings.Contains(output, id) {
			continue
		}
		modes = append(modes, modeSpec(provider, id, id))
	}
	return modes
}

func probeOpenCodeModes(ctx context.Context) []*agentcomposev2.EditorModeSpec {
	output := probeCommand(ctx, "opencode", "agent", "list")
	if output == "" {
		return nil
	}
	// ``agent list`` is human-readable JSON blocks. Use the first line of each
	// top-level agent entry and preserve the editor's own name verbatim.
	seen := map[string]bool{}
	var modes []*agentcomposev2.EditorModeSpec
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "{") {
			continue
		}
		name := strings.TrimSpace(strings.TrimSuffix(line, "(primary)"))
		name = strings.TrimSpace(strings.TrimSuffix(name, "(subagent)"))
		if name == "" || strings.ContainsAny(name, "{}[]\"") || strings.HasPrefix(name, "\"") {
			continue
		}
		if seen[name] || strings.Contains(name, ":") {
			continue
		}
		seen[name] = true
		modes = append(modes, modeSpec("opencode", name, name))
	}
	return modes
}

// probeCursorModes reports Cursor CLI's own modes. The Cursor agent binary is
// named “agent“; its “--help“ output advertises the mode vocabulary
// (agent/plan/ask). Missing CLI yields no modes, so scheduling never falls
// back to a broader permission set.
func probeCursorModes(ctx context.Context) []*agentcomposev2.EditorModeSpec {
	if _, err := LookPath("agent"); err != nil {
		return nil
	}
	output := probeCommand(ctx, "agent", "--help")
	if output == "" {
		return nil
	}
	modes := make([]*agentcomposev2.EditorModeSpec, 0, 3)
	for _, id := range []string{"agent", "plan", "ask"} {
		if !strings.Contains(output, id) {
			continue
		}
		modes = append(modes, modeSpec("cursor", id, id))
	}
	return modes
}

func modeSpec(provider, id, label string) *agentcomposev2.EditorModeSpec {
	mode := &agentcomposev2.EditorModeSpec{
		Id: id, Label: label, Source: "probe",
		Native: map[string]string{"mode": id},
		Semantics: &agentcomposev2.EditorModeSemantics{
			CanRead: true, CanEditWorkspace: true, CanRunCommands: true,
			RequiresApproval: true, NetworkAccess: true,
		},
	}
	if id == "plan" || id == "read-only" || id == "default" && provider == "gemini" || id == "ask" {
		mode.Semantics.CanEditWorkspace = false
	}
	if id == "auto" || id == "yolo" || id == "danger-full-access" || id == "build-auto" || id == "agent" && provider == "cursor" {
		mode.Semantics.RequiresApproval = false
	}
	return mode
}

func probeCommand(ctx context.Context, command string, args ...string) string {
	if _, err := LookPath(command); err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, command, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// capabilityJSON is useful to tests and logging without exposing protobuf
// implementation details. It is deliberately not used for wire transport.
func capabilityJSON(cap *agentcomposev2.EditorCapability) string {
	if cap == nil {
		return ""
	}
	data, err := json.Marshal(cap)
	if err != nil {
		return ""
	}
	return string(data)
}
