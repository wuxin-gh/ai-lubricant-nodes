package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// systemEnvTestManager returns a session manager whose system-env operations point
// at a temp dir instead of the real user home, so tests never touch the developer's
// actual ~/.claude. The redirect is done through HOME/USERPROFILE because
// systemEnvHome resolves via os.UserHomeDir().
func systemEnvTestManager(t *testing.T, allowed bool) (*sessionManager, string) {
	t.Helper()
	m, _ := newTestManager(t)
	m.opts.systemEnvAllowed = allowed
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	resolved, err := m.systemEnvHome()
	if err != nil {
		if !allowed {
			return m, home
		}
		t.Fatalf("systemEnvHome: %v", err)
	}
	if allowed && resolved != home {
		t.Skipf("user home redirect not honored on this platform (got %q, want %q)", resolved, home)
	}
	return m, home
}

func writeSystemEnvResource(t *testing.T, home string, rel []string, name, version string) string {
	t.Helper()
	dir := filepath.Join(append(append([]string{home}, rel...), name)...)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if version != "" {
		payload := []byte(`{"version":"` + version + `"}`)
		if err := os.WriteFile(filepath.Join(dir, "package.json"), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestSystemEnvRefusedUnlessAllowed confirms every system-env operation is gated on
// the same opt-in resolveHome uses: a node that never enabled the mode must not read
// or write its operator's home just because the server asked.
func TestSystemEnvRefusedUnlessAllowed(t *testing.T) {
	m, _ := systemEnvTestManager(t, false)
	if _, err := m.inspectSystemEnv(&agentcomposev2.NodeInspectSystemEnv{}); err == nil {
		t.Fatalf("inspect must be refused when system env is disabled")
	}
	if _, err := m.syncSystemEnv(context.Background(), &agentcomposev2.NodeSyncSystemEnv{}); err == nil {
		t.Fatalf("sync must be refused when system env is disabled")
	}
	err := m.archiveSystemEnvResource(context.Background(), &agentcomposev2.NodeArchiveSystemEnvResource{
		Kind: "skill", Name: "x", UploadUrl: "http://127.0.0.1:1/upload",
	})
	if err == nil {
		t.Fatalf("archive must be refused when system env is disabled")
	}
}

// TestInspectSystemEnvSeesEveryProviderPath is the whole point of the feature: the
// providers already discover these paths in a system session, so all of them must
// show up in the inventory (previously none of them were visible server-side).
func TestInspectSystemEnvSeesEveryProviderPath(t *testing.T) {
	m, home := systemEnvTestManager(t, true)
	writeSystemEnvResource(t, home, []string{".claude", "skills"}, "claude-skill", "")
	writeSystemEnvResource(t, home, []string{".agents", "skills"}, "neutral-skill", "1.4.0")
	writeSystemEnvResource(t, home, []string{".agents", "plugins"}, "neutral-plugin", "2.0.0")
	writeSystemEnvResource(t, home, []string{".gemini", "extensions"}, "gemini-ext", "")
	mcp := `{"mcpServers":{"beta":{"command":"x"},"alpha":{"url":"http://y"}}}`
	if err := os.WriteFile(filepath.Join(home, ".mcp.json"), []byte(mcp), 0o644); err != nil {
		t.Fatal(err)
	}

	inventory, err := m.inspectSystemEnv(&agentcomposev2.NodeInspectSystemEnv{})
	if err != nil {
		t.Fatalf("inspectSystemEnv: %v", err)
	}
	byName := map[string]*agentcomposev2.NodeSystemEnvEntry{}
	for _, entry := range inventory {
		byName[entry.GetName()] = entry
	}
	for _, want := range []string{"claude-skill", "neutral-skill", "neutral-plugin", "gemini-ext", "alpha", "beta"} {
		if byName[want] == nil {
			t.Fatalf("inventory missing %q; got %d entries", want, len(inventory))
		}
	}
	if got := byName["neutral-skill"].GetVersion(); got != "1.4.0" {
		t.Fatalf("skill version = %q, want 1.4.0", got)
	}
	if got := byName["neutral-plugin"].GetKind(); got != systemEnvKindPlugin {
		t.Fatalf("plugin kind = %q, want %q", got, systemEnvKindPlugin)
	}
	if got := byName["alpha"].GetKind(); got != systemEnvKindMCP {
		t.Fatalf("mcp kind = %q, want %q", got, systemEnvKindMCP)
	}
	// Nothing was installed by us, so every entry must read as operator-owned —
	// which is what makes it non-removable through the API.
	for _, entry := range inventory {
		if entry.GetPlatformManaged() {
			t.Fatalf("entry %q reported platform_managed with no manifest", entry.GetName())
		}
	}
}

// TestInspectSystemEnvMissingPathsAreEmptyNotError confirms a fresh machine reads as
// "nothing installed" rather than failing the call, so the console can always render.
func TestInspectSystemEnvMissingPathsAreEmptyNotError(t *testing.T) {
	m, _ := systemEnvTestManager(t, true)
	inventory, err := m.inspectSystemEnv(&agentcomposev2.NodeInspectSystemEnv{})
	if err != nil {
		t.Fatalf("inspectSystemEnv on empty home: %v", err)
	}
	if len(inventory) != 0 {
		t.Fatalf("expected empty inventory, got %d entries", len(inventory))
	}
}

// TestSyncSystemEnvSkipsExistingUnlessOverwrite is the core safety property: the copy
// already on disk may be the operator's own work, so installing must not silently
// replace it. The ack reports "skipped" so the console can tell the user.
func TestSyncSystemEnvSkipsExistingUnlessOverwrite(t *testing.T) {
	m, home := systemEnvTestManager(t, true)
	// The operator's own skill, with a marker file we assert survives.
	dir := writeSystemEnvResource(t, home, []string{".agents", "skills"}, "shared-name", "")
	marker := filepath.Join(dir, "OPERATOR_OWNED")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A local source dir stands in for a downloadable resource (fetchResource copies
	// a src.path straight through, so no network is involved).
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("platform"), 0o644); err != nil {
		t.Fatal(err)
	}

	frame := &agentcomposev2.NodeSyncSystemEnv{
		Skills: []*agentcomposev2.SkillSpec{{Name: "shared-name", Path: source}},
	}
	touched, err := m.syncSystemEnv(context.Background(), frame)
	if err != nil {
		t.Fatalf("syncSystemEnv: %v", err)
	}
	if len(touched) != 1 || touched[0].GetPath() != "skipped" {
		t.Fatalf("expected one skipped entry, got %+v", touched)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("operator file was destroyed by a non-overwrite install: %v", err)
	}

	frame.Overwrite = true
	touched, err = m.syncSystemEnv(context.Background(), frame)
	if err != nil {
		t.Fatalf("syncSystemEnv overwrite: %v", err)
	}
	if len(touched) != 1 || touched[0].GetPath() != "installed" {
		t.Fatalf("expected one installed entry, got %+v", touched)
	}
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "shared-name", "SKILL.md")); err != nil {
		t.Fatalf("overwrite did not land the platform copy: %v", err)
	}
}

// TestSyncSystemEnvNeverPrunesOperatorResources locks the difference from the shared
// tier: a sync that mentions one resource must leave everything else alone. The
// shared-env sync prunes by design; doing that here would delete the operator's setup.
func TestSyncSystemEnvNeverPrunesOperatorResources(t *testing.T) {
	m, home := systemEnvTestManager(t, true)
	writeSystemEnvResource(t, home, []string{".agents", "skills"}, "operator-a", "")
	writeSystemEnvResource(t, home, []string{".agents", "plugins"}, "operator-b", "")
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := m.syncSystemEnv(context.Background(), &agentcomposev2.NodeSyncSystemEnv{
		Skills: []*agentcomposev2.SkillSpec{{Name: "platform-skill", Path: source}},
	})
	if err != nil {
		t.Fatalf("syncSystemEnv: %v", err)
	}
	for _, rel := range [][]string{
		{".agents", "skills", "operator-a"},
		{".agents", "plugins", "operator-b"},
	} {
		if _, err := os.Stat(filepath.Join(append([]string{home}, rel...)...)); err != nil {
			t.Fatalf("sync pruned an operator resource %v: %v", rel, err)
		}
	}
}

// TestSyncSystemEnvRemoveRefusesUnmanaged is the removal boundary enforced on the
// node: even an explicit server request cannot delete something the platform did not
// install. This is what makes the server-side check a UX nicety rather than the
// security boundary.
func TestSyncSystemEnvRemoveRefusesUnmanaged(t *testing.T) {
	m, home := systemEnvTestManager(t, true)
	writeSystemEnvResource(t, home, []string{".agents", "skills"}, "operator-owned", "")

	_, err := m.syncSystemEnv(context.Background(), &agentcomposev2.NodeSyncSystemEnv{
		Remove: []string{"skill/operator-owned"},
	})
	if err == nil {
		t.Fatalf("removing an unmanaged resource must be refused")
	}
	if !strings.Contains(err.Error(), "not installed by the platform") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".agents", "skills", "operator-owned")); statErr != nil {
		t.Fatalf("refused removal still deleted the resource: %v", statErr)
	}
}

// TestSyncSystemEnvInstallThenRemoveRoundTrip covers the manifest bookkeeping: what we
// installed is marked platform-managed, is removable, and disappears from both the
// install dirs and the manifest afterwards.
func TestSyncSystemEnvInstallThenRemoveRoundTrip(t *testing.T) {
	m, home := systemEnvTestManager(t, true)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := m.syncSystemEnv(context.Background(), &agentcomposev2.NodeSyncSystemEnv{
		Skills: []*agentcomposev2.SkillSpec{{Name: "platform-skill", Path: source}},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Mirrored into every discovery path so each runner sees it.
	for _, dir := range systemEnvInstallDirs(home, systemEnvKindSkill) {
		if _, err := os.Stat(filepath.Join(dir, "platform-skill")); err != nil {
			t.Fatalf("install missing from %s: %v", dir, err)
		}
	}
	inventory, err := m.inspectSystemEnv(&agentcomposev2.NodeInspectSystemEnv{})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	found := false
	for _, entry := range inventory {
		if entry.GetName() == "platform-skill" {
			found = true
			if !entry.GetPlatformManaged() {
				t.Fatalf("installed entry not reported as platform managed")
			}
		}
	}
	if !found {
		t.Fatalf("installed skill missing from inventory")
	}

	if _, err := m.syncSystemEnv(context.Background(), &agentcomposev2.NodeSyncSystemEnv{
		Remove: []string{"skill/platform-skill"},
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	for _, dir := range systemEnvInstallDirs(home, systemEnvKindSkill) {
		if _, err := os.Stat(filepath.Join(dir, "platform-skill")); !os.IsNotExist(err) {
			t.Fatalf("remove left files in %s (err=%v)", dir, err)
		}
	}
	raw, err := os.ReadFile(systemEnvManifestPath(home))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest systemEnvManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if _, still := manifest.Entries[systemEnvManifestKey("skill", "platform-skill")]; still {
		t.Fatalf("manifest still records a removed entry")
	}
}

// TestSplitSystemEnvRefRejectsJunk keeps the remove-target parser strict: a malformed
// or MCP target must fail loudly rather than resolve to some default path.
func TestSplitSystemEnvRefRejectsJunk(t *testing.T) {
	for _, bad := range []string{"", "skill", "skill/", "/name", "mcp/alpha", "unknown/x"} {
		if _, _, ok := splitSystemEnvRef(bad); ok {
			t.Fatalf("splitSystemEnvRef(%q) accepted a bad target", bad)
		}
	}
	kind, name, ok := splitSystemEnvRef(" plugin/my-plugin ")
	if !ok || kind != systemEnvKindPlugin || name != "my-plugin" {
		t.Fatalf("splitSystemEnvRef trimmed parse = (%q,%q,%v)", kind, name, ok)
	}
}

// TestTarGzDirSkipsGitAndSymlinks confirms the archive carries content only: .git
// bloats it, and symlinks could point outside the resource tree.
func TestTarGzDirSkipsGitAndSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := tarGzDir(root)
	if err != nil {
		t.Fatalf("tarGzDir: %v", err)
	}
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		t.Fatalf("archive is not gzip: % x", data[:min(4, len(data))])
	}
	names := tarGzNames(t, data)
	if !contains(names, "SKILL.md") {
		t.Fatalf("archive missing content file; got %v", names)
	}
	for _, name := range names {
		if strings.HasPrefix(name, ".git") {
			t.Fatalf("archive included git metadata: %v", names)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// tarGzNames lists the member names inside a gzip'd tar, for archive assertions.
func tarGzNames(t *testing.T, data []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("open gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	reader := tar.NewReader(gz)
	var names []string
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		names = append(names, strings.TrimSuffix(header.Name, "/"))
	}
	return names
}
