package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// useTempState points the state dir at a fresh temp dir for one test so the
// suite never touches the developer's real ~/.config.
func useTempState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(configEnvOverride, dir)
	return dir
}

func execCandidate(server, id, secret string) LocalConfig {
	return LocalConfig{Server: server, NodeID: id, Secret: secret, Role: RoleExecution}
}

func TestResolveNoArgsRequiresInstall(t *testing.T) {
	useTempState(t)
	_, err := ResolveLocalConfig(LocalConfig{Role: RoleExecution}, InstallOptions{})
	if err == nil {
		t.Fatal("expected an error when no config is installed and no credentials given")
	}
}

func TestResolveNoArgsLoadsPersisted(t *testing.T) {
	useTempState(t)
	saved := execCandidate("https://srv", "node-1", "SECRET")
	if err := SaveLocalConfig(&saved); err != nil {
		t.Fatalf("SaveLocalConfig: %v", err)
	}
	res, err := ResolveLocalConfig(LocalConfig{Role: RoleExecution}, InstallOptions{})
	if err != nil {
		t.Fatalf("ResolveLocalConfig: %v", err)
	}
	if res.Config.NodeID != "node-1" || res.Persist {
		t.Fatalf("expected persisted node-1 without re-persist, got %+v", res)
	}
}

func TestResolveNoArgsRoleMismatch(t *testing.T) {
	useTempState(t)
	saved := LocalConfig{Server: "https://srv", NodeID: "node-1", Secret: "S", Role: RoleManagement}
	if err := SaveLocalConfig(&saved); err != nil {
		t.Fatalf("SaveLocalConfig: %v", err)
	}
	if _, err := ResolveLocalConfig(LocalConfig{Role: RoleExecution}, InstallOptions{}); err == nil {
		t.Fatal("expected role-mismatch error starting execution binary on a management host")
	}
}

func TestResolveIdempotentSameCredentials(t *testing.T) {
	useTempState(t)
	saved := execCandidate("https://srv", "node-1", "SECRET")
	saved.WorkRoot = "/data/work"
	if err := SaveLocalConfig(&saved); err != nil {
		t.Fatalf("SaveLocalConfig: %v", err)
	}
	// Same credentials, no --install: reuse stored extras, do not prompt.
	res, err := ResolveLocalConfig(execCandidate("https://srv", "node-1", "SECRET"), InstallOptions{})
	if err != nil {
		t.Fatalf("ResolveLocalConfig: %v", err)
	}
	if res.Rebind {
		t.Fatal("same credentials must not be treated as a rebind")
	}
	if res.Config.WorkRoot != "/data/work" {
		t.Fatalf("expected stored extras merged, got work_root=%q", res.Config.WorkRoot)
	}
}

func TestResolveFreshInstallPersists(t *testing.T) {
	useTempState(t)
	res, err := ResolveLocalConfig(execCandidate("https://srv/", "node-1", "SECRET"), InstallOptions{Install: true})
	if err != nil {
		t.Fatalf("ResolveLocalConfig: %v", err)
	}
	if !res.Persist || res.Rebind {
		t.Fatalf("fresh install should persist and not rebind, got %+v", res)
	}
	if res.Config.Server != "https://srv" {
		t.Fatalf("server should be trimmed of trailing slash, got %q", res.Config.Server)
	}
}

func TestResolveDifferentCredentialsWithoutInstallRefuses(t *testing.T) {
	useTempState(t)
	saved := execCandidate("https://srv", "node-1", "SECRET")
	if err := SaveLocalConfig(&saved); err != nil {
		t.Fatalf("SaveLocalConfig: %v", err)
	}
	_, err := ResolveLocalConfig(execCandidate("https://srv", "node-2", "OTHER"), InstallOptions{})
	if err == nil || !strings.Contains(err.Error(), "--install") {
		t.Fatalf("expected a refusal pointing at --install, got %v", err)
	}
}

func TestResolveRebindConfirmYes(t *testing.T) {
	useTempState(t)
	saved := execCandidate("https://srv", "node-1", "SECRET")
	if err := SaveLocalConfig(&saved); err != nil {
		t.Fatalf("SaveLocalConfig: %v", err)
	}
	res, err := ResolveLocalConfig(
		execCandidate("https://srv", "node-2", "OTHER"),
		InstallOptions{Install: true, In: strings.NewReader("y\n")},
	)
	if err != nil {
		t.Fatalf("ResolveLocalConfig: %v", err)
	}
	if !res.Rebind || !res.Persist || res.Config.NodeID != "node-2" {
		t.Fatalf("expected an approved rebind to node-2, got %+v", res)
	}
}

func TestResolveRebindConfirmNoCancels(t *testing.T) {
	useTempState(t)
	saved := execCandidate("https://srv", "node-1", "SECRET")
	if err := SaveLocalConfig(&saved); err != nil {
		t.Fatalf("SaveLocalConfig: %v", err)
	}
	_, err := ResolveLocalConfig(
		execCandidate("https://srv", "node-2", "OTHER"),
		InstallOptions{Install: true, In: strings.NewReader("n\n")},
	)
	if err != ErrInstallCancelled {
		t.Fatalf("expected ErrInstallCancelled, got %v", err)
	}
	// The stored config must be untouched after a cancelled rebind.
	cur, _ := LoadLocalConfig()
	if cur == nil || cur.NodeID != "node-1" {
		t.Fatalf("cancelled rebind must not change stored config, got %+v", cur)
	}
}

func TestResolveRebindNonInteractiveRefuses(t *testing.T) {
	useTempState(t)
	saved := execCandidate("https://srv", "node-1", "SECRET")
	if err := SaveLocalConfig(&saved); err != nil {
		t.Fatalf("SaveLocalConfig: %v", err)
	}
	// A closed stdin (empty reader that is not a terminal) with --install but no
	// --yes must refuse rather than silently overwrite.
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer f.Close()
	_, err = ResolveLocalConfig(
		execCandidate("https://srv", "node-2", "OTHER"),
		InstallOptions{Install: true, In: f},
	)
	if err == nil || err == ErrInstallCancelled {
		t.Fatalf("expected a hard refusal on a non-interactive rebind, got %v", err)
	}
}

func TestResolveRebindAssumeYes(t *testing.T) {
	useTempState(t)
	saved := execCandidate("https://srv", "node-1", "SECRET")
	if err := SaveLocalConfig(&saved); err != nil {
		t.Fatalf("SaveLocalConfig: %v", err)
	}
	res, err := ResolveLocalConfig(
		execCandidate("https://srv", "node-2", "OTHER"),
		InstallOptions{Install: true, AssumeYes: true},
	)
	if err != nil {
		t.Fatalf("ResolveLocalConfig: %v", err)
	}
	if !res.Rebind || res.Config.NodeID != "node-2" {
		t.Fatalf("expected an approved rebind with --yes, got %+v", res)
	}
}

func TestSaveLocalConfigPermissions(t *testing.T) {
	useTempState(t)
	cfg := execCandidate("https://srv", "node-1", "SECRET")
	if err := SaveLocalConfig(&cfg); err != nil {
		t.Fatalf("SaveLocalConfig: %v", err)
	}
	path, _ := ConfigPath()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("config perm = %o, want 0600", info.Mode().Perm())
	}
	// The persisted file must not be left behind as a temp file.
	if filepath.Ext(path) == ".tmp" {
		t.Fatalf("config path should not be a temp file: %s", path)
	}
}

func TestAcquireInstanceSingleton(t *testing.T) {
	useTempState(t)
	cfg := execCandidate("https://srv", "node-1", "SECRET")
	inst, err := AcquireInstance(cfg, false)
	if err != nil {
		t.Fatalf("first AcquireInstance: %v", err)
	}
	defer inst.Close()
	// A second acquire without replace must report the machine is already running.
	if _, err := AcquireInstance(cfg, false); err != ErrAlreadyRunning {
		t.Fatalf("second AcquireInstance = %v, want ErrAlreadyRunning", err)
	}
	// After releasing, the lock is free again.
	if err := inst.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	inst2, err := AcquireInstance(cfg, false)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	inst2.Close()
}
