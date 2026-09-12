package agent

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// The autostart resolver: the one-time prompt, choice-file idempotency, and
// the explicit on/off paths. Platform install/remove is faked via the package
// seams so the test never touches launchd/systemd/schtasks.

func withFakeAutostart(t *testing.T, available bool) *fakeAutostart {
	t.Helper()
	useTempState(t)
	fake := &fakeAutostart{available: available, method: "launchd"}
	origAvail, origInst, origInstall, origRemove := autostartAvailableFn, autostartInstalledFn, autostartInstallFn, autostartRemoveFn
	autostartAvailableFn = func() (string, bool) { return fake.method, fake.available }
	autostartInstalledFn = func() bool { return fake.installed }
	autostartInstallFn = func() error { fake.installed = true; fake.installs++; return nil }
	autostartRemoveFn = func() error { fake.installed = false; fake.removes++; return nil }
	t.Cleanup(func() {
		autostartAvailableFn, autostartInstalledFn, autostartInstallFn, autostartRemoveFn = origAvail, origInst, origInstall, origRemove
	})
	return fake
}

type fakeAutostart struct {
	available bool
	method    string
	installed bool
	installs  int
	removes   int
}

func TestResolveAutostartExplicitOn(t *testing.T) {
	fake := withFakeAutostart(t, true)
	status, err := ResolveAutostart("on", InstallOptions{})
	if err != nil {
		t.Fatalf("on: %v", err)
	}
	if !status.Enabled || fake.installs != 1 {
		t.Fatalf("explicit on must install once, got status=%+v installs=%d", status, fake.installs)
	}
}

func TestResolveAutostartExplicitOff(t *testing.T) {
	fake := withFakeAutostart(t, true)
	fake.installed = true
	status, err := ResolveAutostart("off", InstallOptions{})
	if err != nil {
		t.Fatalf("off: %v", err)
	}
	if status.Enabled || fake.removes != 1 {
		t.Fatalf("explicit off must remove, got status=%+v removes=%d", status, fake.removes)
	}
}

func TestResolveAutostartOnUnavailableErrors(t *testing.T) {
	withFakeAutostart(t, false)
	if _, err := ResolveAutostart("on", InstallOptions{}); err == nil {
		t.Fatal("explicit on with no mechanism must error")
	}
}

func TestResolveAutostartAutoPromptYesInstallsAndRecords(t *testing.T) {
	fake := withFakeAutostart(t, true)
	in := strings.NewReader("y\n")
	var out bytes.Buffer
	status, err := ResolveAutostart("auto", InstallOptions{In: in, Out: &out})
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	if !status.Enabled || fake.installs != 1 {
		t.Fatalf("auto+yes must install, got status=%+v installs=%d", status, fake.installs)
	}
	if readAutostartChoice() != "on" {
		t.Fatalf("choice must be recorded on, got %q", readAutostartChoice())
	}
	if !strings.Contains(out.String(), "开机自启") {
		t.Fatalf("prompt must mention autostart, got %q", out.String())
	}
}

func TestResolveAutostartAutoPromptNoRecordsOff(t *testing.T) {
	fake := withFakeAutostart(t, true)
	status, err := ResolveAutostart("auto", InstallOptions{In: strings.NewReader("n\n"), Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	if status.Enabled || fake.installs != 0 {
		t.Fatalf("auto+no must not install, got %+v", status)
	}
	if readAutostartChoice() != "off" {
		t.Fatalf("choice must be recorded off, got %q", readAutostartChoice())
	}
}

func TestResolveAutostartAutoNeverRepromptsAfterChoice(t *testing.T) {
	fake := withFakeAutostart(t, true)
	if err := saveAutostartChoice("off"); err != nil {
		t.Fatal(err)
	}
	// A reader that would panic if read proves the prompt is skipped.
	status, err := ResolveAutostart("auto", InstallOptions{In: &panicReader{t}, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	if status.Enabled || fake.installs != 0 {
		t.Fatalf("recorded off must not prompt or install, got %+v", status)
	}
}

func TestResolveAutostartAutoRecordedOnReinstalls(t *testing.T) {
	fake := withFakeAutostart(t, true)
	if err := saveAutostartChoice("on"); err != nil {
		t.Fatal(err)
	}
	status, err := ResolveAutostart("auto", InstallOptions{In: &panicReader{t}, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	if !status.Enabled || fake.installs != 1 {
		t.Fatalf("recorded on must reinstall idempotently, got status=%+v installs=%d", status, fake.installs)
	}
}

func TestResolveAutostartExistingEntryCountsAsOn(t *testing.T) {
	fake := withFakeAutostart(t, true)
	fake.installed = true // a script-installed entry, no choice recorded yet
	status, err := ResolveAutostart("auto", InstallOptions{In: &panicReader{t}, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	if !status.Enabled || readAutostartChoice() != "on" {
		t.Fatalf("existing entry must record on without prompting, got status=%+v choice=%q", status, readAutostartChoice())
	}
}

func TestResolveAutostartUnavailableIsNoop(t *testing.T) {
	withFakeAutostart(t, false)
	status, err := ResolveAutostart("auto", InstallOptions{In: &panicReader{t}, Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("auto: %v", err)
	}
	if status.Available || status.Enabled {
		t.Fatalf("no mechanism must be a silent no-op, got %+v", status)
	}
}

func TestStartupMethodLabelEnvOverride(t *testing.T) {
	withFakeAutostart(t, true)
	t.Setenv("AGENT_COMPOSE_STARTUP_METHOD", "docker")
	if got := StartupMethodLabel(); got != "docker" {
		t.Fatalf("env override wins, got %q", got)
	}
}

func TestStartupMethodLabelReflectsEntry(t *testing.T) {
	fake := withFakeAutostart(t, true)
	t.Setenv("AGENT_COMPOSE_STARTUP_METHOD", "")
	if got := StartupMethodLabel(); got != "standalone" {
		t.Fatalf("no entry = standalone, got %q", got)
	}
	fake.installed = true
	if got := StartupMethodLabel(); got != "autostart" {
		t.Fatalf("entry present = autostart, got %q", got)
	}
}

// panicReader fails the test if the prompt tries to read from it — proving the
// prompt was skipped.
type panicReader struct{ t *testing.T }

func (p *panicReader) Read([]byte) (int, error) {
	p.t.Fatal("prompt should not read stdin here")
	return 0, os.ErrClosed
}
