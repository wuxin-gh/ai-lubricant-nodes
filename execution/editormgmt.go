package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"ai-lubricant-nodes/common/agent"
	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// Editor install/upgrade on this node.
//
// Each supported editor is managed with ITS OWN official command where one
// exists (claude update / opencode upgrade), falling back to the editor's
// published npm package otherwise (codex / gemini). This is deliberately
// per-editor rather than a blanket "npm -g everything": the CLIs that ship a
// self-updater handle their own platform-specific binaries (Claude Code and
// opencode both download a platform package), and calling their updater is what
// their vendors support.
//
// Cross-platform: npm is the same invocation on linux/darwin/windows, so the
// only platform branch needed is how we spawn a shell-less command — we always
// exec the binary directly (no shell), and on Windows npm is `npm.cmd`.

// editorInstallSpec describes how to install and upgrade one editor.
type editorInstallSpec struct {
	// npmPackage is the published package installed with `npm i -g <pkg>@latest`.
	npmPackage string
	// selfUpgrade, when non-empty, is the editor's own upgrade subcommand
	// (argv after the editor binary). Used only when the editor is present.
	selfUpgrade []string
	// manualInstall, when non-empty, is the actionable hint returned when an
	// install is requested for an editor with no npm package (e.g. Cursor's
	// official curl/PowerShell installer).
	manualInstall string
}

var editorInstallSpecs = map[string]editorInstallSpec{
	// Claude Code ships `claude update`, which pulls the right platform package.
	"claude": {npmPackage: "@anthropic-ai/claude-code", selfUpgrade: []string{"update"}},
	// opencode ships `opencode upgrade`.
	"opencode": {npmPackage: "opencode-ai", selfUpgrade: []string{"upgrade"}},
	// codex / gemini have no self-updater: reinstall the latest npm package.
	"codex":  {npmPackage: "@openai/codex"},
	"gemini": {npmPackage: "@google/gemini-cli"},
	// Cursor CLI installs via its official script and self-upgrades with
	// `agent update` (its binary is named ``agent``, see agent.EditorCommandName).
	// It is not published as an npm package, so installs point at the official
	// installer instead of inventing one.
	"cursor": {
		selfUpgrade:   []string{"update"},
		manualInstall: `run the official installer: "curl https://cursor.com/install -fsS | bash" (Windows: irm 'https://cursor.com/install?win32=true' | iex)`,
	},
	// OpenCodeReview is not an editor (it has no provider/modes), but it ships
	// as an npm global and installs the same way. Exposed through the same
	// ManageEditor command so the Review config page can install it via npm
	// without a second protocol path. ``normalizeEditor`` admits it even
	// though it is not in SupportedEditors.
	"ocr": {npmPackage: "@alibaba-group/open-code-review"},
}

// editorManageTimeout bounds one install/upgrade. A global npm install of these
// CLIs routinely takes tens of seconds on a cold cache.
const editorManageTimeout = 10 * time.Minute

// npmBinary is the npm executable name for this platform.
func npmBinary() string {
	if runtime.GOOS == "windows" {
		return "npm.cmd"
	}
	return "npm"
}

// normalizeEditor maps an incoming editor name onto a supported key, reusing the
// provider alias table so "claude-code"/"gemini-cli" also resolve. “ocr“ is
// admitted as an installable npm tool even though it is not an editor.
func normalizeEditor(name string) (string, bool) {
	key := normalizeProvider(name)
	if _, ok := editorInstallSpecs[key]; ok {
		return key, true
	}
	if key == "ocr" {
		return "ocr", true
	}
	return "", false
}

// manageEditor installs or upgrades one editor CLI and returns the version
// probed afterwards. An unknown editor, a missing npm, or a non-zero command
// exit is returned as an error with the combined output tail for diagnosis.
func manageEditor(ctx context.Context, frame *agentcomposev2.NodeManageEditor) (string, error) {
	editor, ok := normalizeEditor(frame.GetEditor())
	if !ok {
		return "", fmt.Errorf("unsupported editor %q (supported: %s)",
			frame.GetEditor(), strings.Join(agent.SupportedEditors, ", "))
	}
	spec := editorInstallSpecs[editor]

	upgrade := frame.GetAction() == agentcomposev2.EditorAction_EDITOR_ACTION_UPGRADE
	// Probe the provider's real binary name (cursor installs as ``agent``).
	editorBin := agent.EditorCommandName(editor)
	_, lookErr := agent.LookPath(editorBin)
	installed := lookErr == nil

	if upgrade && !installed {
		return "", fmt.Errorf("editor %s is not installed; install it first", editor)
	}
	if !upgrade && installed {
		// Install on an already-present editor is treated as a no-op success so
		// the console stays idempotent; report the current version.
		return probeVersion(ctx, editor), nil
	}

	bin, args, err := editorCommand(editor, spec, upgrade, installed)
	if err != nil {
		return "", err
	}

	cctx, cancel := context.WithTimeout(ctx, editorManageTimeout)
	defer cancel()
	out, runErr := exec.CommandContext(cctx, bin, args...).CombinedOutput()
	if runErr != nil {
		return "", fmt.Errorf("%s %s failed: %v: %s",
			bin, strings.Join(args, " "), runErr, tailOutput(string(out)))
	}
	return probeVersion(ctx, editor), nil
}

// editorCommand picks the command to run: the editor's own upgrade subcommand
// when upgrading an installed editor that has one, otherwise npm global install
// of the editor's package pinned to @latest.
func editorCommand(editor string, spec editorInstallSpec, upgrade, installed bool) (string, []string, error) {
	if upgrade && installed && len(spec.selfUpgrade) > 0 {
		resolved, err := agent.LookPath(agent.EditorCommandName(editor))
		if err != nil {
			return "", nil, fmt.Errorf("locate %s: %w", editor, err)
		}
		return resolved, spec.selfUpgrade, nil
	}
	if spec.npmPackage == "" {
		if spec.manualInstall != "" {
			return "", nil, fmt.Errorf("editor %s has no automatic install: %s", editor, spec.manualInstall)
		}
		return "", nil, fmt.Errorf("editor %s has no install method configured", editor)
	}
	npm, err := agent.LookPath(npmBinary())
	if err != nil {
		return "", nil, fmt.Errorf("npm is required to install %s but was not found on PATH: %w", editor, err)
	}
	return npm, []string{"install", "-g", spec.npmPackage + "@latest"}, nil
}

// probeVersion re-reads the editor/tool's version after a successful command. An
// empty result is not an error: the command succeeded, only the probe is unsure.
func probeVersion(ctx context.Context, editor string) string {
	if editor == "ocr" {
		return agent.HostToolLabels(ctx)["ocr_version"]
	}
	return agent.EditorLabels(ctx)["editor_version_"+editor]
}

// tailOutput trims command output to the last few hundred bytes for error text.
func tailOutput(s string) string {
	s = strings.TrimSpace(s)
	const max = 400
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}
