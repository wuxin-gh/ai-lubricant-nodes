package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"ai-lubricant-nodes/common/agent"
)

func TestUpsertByUDIDUpdatesInPlace(t *testing.T) {
	cfg := &DevicesConfig{}
	cfg.Upsert(DeviceConfig{Name: "phone", UDID: "UDID-1", WDAPort: 9100})
	cfg.Upsert(DeviceConfig{Name: "phone-renamed", UDID: "UDID-1", WDAPort: 9200})

	if len(cfg.Devices) != 1 {
		t.Fatalf("expected 1 device after re-pairing same UDID, got %d", len(cfg.Devices))
	}
	got := cfg.Devices[0]
	if got.Name != "phone-renamed" || got.WDAPort != 9200 {
		t.Fatalf("re-pair did not update in place: %+v", got)
	}
}

func TestUpsertByNameWhenNoUDID(t *testing.T) {
	cfg := &DevicesConfig{}
	cfg.Upsert(DeviceConfig{Name: "device", UDID: "", WDAPort: 9100})
	cfg.Upsert(DeviceConfig{Name: "device", UDID: "", WDAPort: 9200})

	if len(cfg.Devices) != 1 {
		t.Fatalf("expected 1 device after re-pairing same name, got %d", len(cfg.Devices))
	}
	if cfg.Devices[0].WDAPort != 9200 {
		t.Fatalf("re-pair by name did not update in place: %+v", cfg.Devices[0])
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.json")
	cfg := &DevicesConfig{Devices: []DeviceConfig{
		{Name: "phone", UDID: "UDID-1", Transport: "usb",
			WDABundle: "com.x.WDA.xctrunner", XCTest: "WebDriverAgentRunner.xctest",
			WDAPort: 9100, CredentialPath: filepath.Join(dir, "credentials", "phone.json")},
	}}
	if err := SaveDevicesConfig(path, cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// On Windows, Chmod 0600 is a no-op (see agent.AtomicWriteJSON), so only
	// assert the mode on POSIX. The chmod itself is exercised on Linux/macOS CI.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600, got %v", info.Mode().Perm())
	}

	loaded, _, err := LoadDevicesConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(loaded.Devices))
	}
	if loaded.Devices[0].Name != "phone" || loaded.Devices[0].UDID != "UDID-1" {
		t.Fatalf("round-trip mismatch: %+v", loaded.Devices[0])
	}
}

// TestSaveDevicesConfigReplacesExisting exercises the Windows replace path: a
// re-pair overwrites devices.json in place. agent.AtomicWriteJSON uses
// MoveFileEx(REPLACE_EXISTING) on Windows; plain os.Rename would fail with
// "Access is denied" here.
func TestSaveDevicesConfigReplacesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	cfg := &DevicesConfig{Devices: []DeviceConfig{{Name: "a", UDID: "U1"}}}
	if err := SaveDevicesConfig(path, cfg); err != nil {
		t.Fatalf("first save: %v", err)
	}
	cfg2 := &DevicesConfig{Devices: []DeviceConfig{{Name: "b", UDID: "U2"}}}
	if err := SaveDevicesConfig(path, cfg2); err != nil {
		t.Fatalf("second save (replace): %v", err)
	}
	loaded, _, err := LoadDevicesConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Devices) != 1 || loaded.Devices[0].UDID != "U2" {
		t.Fatalf("replace did not take: %+v", loaded.Devices)
	}
}

func TestLoadMissingConfigIsNotAnError(t *testing.T) {
	loaded, path, err := LoadDevicesConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("load of missing config should be a non-error, got: %v", err)
	}
	if loaded == nil || len(loaded.Devices) != 0 {
		t.Fatalf("expected empty config for missing file")
	}
	if path == "" {
		t.Fatalf("expected resolved path")
	}
}

func TestSanitizeFileName(t *testing.T) {
	cases := map[string]string{
		"phone-1":   "phone-1",
		"my device": "my_device",
		"":          "device",
		"../escape": ".._escape",
		"a.b.c":     "a.b.c",
	}
	for in, want := range cases {
		if got := sanitizeFileName(in); got != want {
			t.Errorf("sanitizeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultCredentialPath(t *testing.T) {
	got := defaultCredentialPath(filepath.Join("dir", "devices.json"), "phone-1")
	want := filepath.Join("dir", "credentials", "phone-1.json")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAcquireLockIsExclusive confirms a second acquirer hits ErrAlreadyRunning,
// which is the whole point of the iOS-scoped instance lock.
func TestAcquireLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(configEnvOverride, dir)

	release, err := acquireLock()
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()

	if _, err := acquireLock(); err != agent.ErrAlreadyRunning {
		t.Fatalf("second acquire: want ErrAlreadyRunning, got %v", err)
	}
}
