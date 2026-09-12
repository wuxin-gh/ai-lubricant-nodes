// Autostart management: the node-side owner of "start me at login".
//
// The install scripts pre-install an autostart entry on Windows (schtasks
// ONLOGON) and Linux (systemd user unit), but macOS had only a fragile
// crontab @reboot row. This package makes the node itself the authority for
// autostart: it probes what the platform offers, asks the operator once
// (persisted in the state dir, so a restart never re-prompts), installs or
// removes the fixed-name entry, and reports the outcome as capability labels
// so the admin UI can show the node's actual startup mode.
//
// Fixed entry names are shared with the install scripts so a script-installed
// entry and a node-installed entry are the same object, never duplicates:
//
//	darwin:  ~/Library/LaunchAgents/com.agent-compose.node.plist
//	linux:   ~/.config/systemd/user/agent-compose-node.service
//	windows: schtasks task "agent-compose-node" (per-user ONLOGON)
//
// The entries all point at the durable launcher (start-node.sh / start-node.cmd)
// rather than the binary, so a self-upgrade that replaces the binary never
// breaks autostart.
package agent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Autostart method identifiers (reported as the startup_method label value
// suffix and to the operator in the prompt).
const (
	AutostartMethodLaunchd  = "launchd"  // macOS per-user LaunchAgent
	AutostartMethodSystemd  = "systemd" // Linux per-user systemd unit
	AutostartMethodSchtasks = "schtasks" // Windows per-user ONLOGON task
)

// autostartAvailableFunc / autostartInstalledFunc / autostartInstallFunc /
// autostartRemoveFunc are the platform seams (same pattern as the xcode job
// runner): platform files set them via init(), tests override them.
var (
	autostartAvailableFn = autostartAvailablePlatform
	autostartInstalledFn = autostartInstalledPlatform
	autostartInstallFn   = autostartInstallPlatform
	autostartRemoveFn    = autostartRemovePlatform
)

// AutostartStatus is the resolved state of one node's autostart configuration.
type AutostartStatus struct {
	// Method is the platform mechanism that would be / was used
	// (launchd / systemd / schtasks). Empty when unavailable.
	Method string
	// Available: the platform supports per-user autostart from inside this
	// process (launchd present / systemd user session / schtasks exists).
	Available bool
	// Enabled: a fixed-name autostart entry exists right now.
	Enabled bool
	// Prompted: the operator has already answered the one-time ask.
	Prompted bool
}

// AutostartChoicePath is the state file recording the operator's one-time
// answer ("on" / "off") so the prompt never repeats across restarts.
func AutostartChoicePath() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "autostart-choice"), nil
}

// ProbeAutostart reports the current autostart status without side effects.
func ProbeAutostart() AutostartStatus {
	method, ok := autostartAvailableFn()
	return AutostartStatus{
		Method:    method,
		Available: ok,
		Enabled:   ok && autostartInstalledFn(),
	}
}

// startupMethodLabel derives the capability label the node reports: the
// platform autostart mechanism when enabled, "standalone" when the entry is
// absent (manually started), or an explicit env override for odd hosts.
func StartupMethodLabel() string {
	if v := strings.TrimSpace(os.Getenv("AGENT_COMPOSE_STARTUP_METHOD")); v != "" {
		return v
	}
	status := ProbeAutostart()
	if status.Available && status.Enabled {
		return "autostart"
	}
	return "standalone"
}

// saveAutostartChoice records the operator's one-time answer.
func saveAutostartChoice(choice string) error {
	path, err := AutostartChoicePath()
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(choice), 0o644)
}

// readAutostartChoice returns the recorded answer ("" when never asked).
func readAutostartChoice() string {
	path, err := AutostartChoicePath()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// ResolveAutostart applies the --autostart flag (auto|on|off) and returns the
// resulting status. "auto" probes the platform, and — when an entry is not
// already installed AND the operator has never answered — asks once on an
// interactive terminal. Non-TTY (piped install script, service context) or a
// containerized host never prompts: the answer defaults to keeping the
// current state.
//
// Callers should invoke this AFTER the single-instance lock is taken (so the
// "another node is running" exit path never prompts) but BEFORE the client
// dials, so the label reflects the final state at registration. When the
// resolved status is Enabled, the entry is (re)installed idempotently —
// fixing a stale or broken entry costs nothing and matches the install
// scripts' /F semantics.
func ResolveAutostart(value string, opt InstallOptions) (AutostartStatus, error) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "on":
		method, ok := autostartAvailableFn()
		if !ok {
			return AutostartStatus{}, fmt.Errorf("autostart: no per-user autostart mechanism on this host")
		}
		if err := autostartInstallFn(); err != nil {
			return AutostartStatus{}, fmt.Errorf("autostart: install %s entry: %w", method, err)
		}
		return AutostartStatus{Method: method, Available: true, Enabled: true, Prompted: true}, nil
	case "off":
		_ = autostartRemoveFn()
		method, _ := autostartAvailableFn()
		return AutostartStatus{Method: method, Prompted: true}, nil
	case "", "auto":
	default:
		return AutostartStatus{}, fmt.Errorf("autostart: invalid value %q (auto|on|off)", value)
	}

	// ── auto ───────────────────────────────────────────────────────────
	status := ProbeAutostart()
	if !status.Available || RunningInContainer() {
		return status, nil
	}
	choice := readAutostartChoice()
	switch choice {
	case "on":
		// Previously opted in: keep the entry healthy (idempotent reinstall).
		if err := autostartInstallFn(); err != nil {
			return status, fmt.Errorf("autostart: reinstall %s entry: %w", status.Method, err)
		}
		status.Enabled = true
		status.Prompted = true
		return status, nil
	case "off":
		status.Prompted = true
		return status, nil
	}

	// Never asked. An existing entry (script-installed) counts as on.
	if status.Enabled {
		_ = saveAutostartChoice("on")
		status.Prompted = true
		return status, nil
	}

	in, out := opt.In, opt.Out
	if in == nil {
		in = os.Stdin
	}
	if out == nil {
		out = os.Stdout
	}
	if !inputIsTerminal(in) {
		// Unattended (piped installer, cron, service manager): never block on
		// a prompt that cannot be answered. Leave the entry absent.
		return status, nil
	}
	fmt.Fprintf(out, "\n检测到本机可配置开机自启动（%s），是否开启？[Y/n]: ", status.Method)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return status, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		if err := autostartInstallFn(); err != nil {
			return status, fmt.Errorf("autostart: install %s entry: %w", status.Method, err)
		}
		_ = saveAutostartChoice("on")
		status.Enabled = true
		status.Prompted = true
	default:
		_ = saveAutostartChoice("off")
		status.Prompted = true
	}
	return status, nil
}
