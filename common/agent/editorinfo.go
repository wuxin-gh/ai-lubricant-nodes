package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// SupportedEditors is the set of provider CLIs this system can install,
// upgrade, and report versions for.
var SupportedEditors = []string{"claude", "codex", "gemini", "opencode", "cursor"}

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
			continue
		}
	}
	return labels
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
