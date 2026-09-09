package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// Unit tests for the system-env scan helpers that read operator config: MCP
// multi-source discovery, claude marketplace plugins, and SKILL.md description
// parsing. These run against fixture directories, never the real HOME.

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFrontmatterDescription(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"basic", "---\nname: x\ndescription: Does things\n---\n\nBody\n", "Does things"},
		{"quoted", "---\ndescription: \"Does things\"\n---\nbody", "Does things"},
		{"crlf", "---\r\ndescription: Does things\r\n---\r\nbody", "Does things"},
		{"no frontmatter", "# just a doc\n", ""},
		{"unclosed fence", "---\ndescription: x\n", ""},
		{"folded scalar skipped", "---\ndescription: >-\n  folded text\n---\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := frontmatterDescription(tc.text); got != tc.want {
				t.Fatalf("frontmatterDescription = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadCodexMCPServers(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"config.toml": `model = "gpt-5"

[mcp_servers]
command = "unused"

[mcp_servers.codegraph]
command = "cg"

[mcp_servers.git]
command = "g"

[mcp_servers.codegraph]
command = "dup"
`})
	got := readCodexMCPServers(filepath.Join(dir, "config.toml"))
	// The bare [mcp_servers] header must not surface as a server, and the
	// duplicate codegraph table collapses to one entry (last-wins is fine —
	// we only enumerate names, not which block wins).
	want := []string{"codegraph", "git"}
	if len(got) != len(want) {
		t.Fatalf("readCodexMCPServers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("readCodexMCPServers[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestReadJSONMapKeys(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"plain.json":     `{"mcpServers": {"codegraph": {}, "playwright": {}}}`,
		"nomcp.json":     `{"other": {}}`,
		"corrupt.json":   `{not json`,
		"nested.json":    `{"mcpServers": {"inner": {}}}`,
	})
	got := readJSONMapKeys(filepath.Join(dir, "plain.json"), "mcpServers")
	if len(got) != 2 || got[0] != "codegraph" || got[1] != "playwright" {
		t.Fatalf("plain = %v, want [codegraph playwright]", got)
	}
	if names := readJSONMapKeys(filepath.Join(dir, "nomcp.json"), "mcpServers"); len(names) != 0 {
		t.Fatalf("nomcp = %v, want []", names)
	}
	if names := readJSONMapKeys(filepath.Join(dir, "corrupt.json"), "mcpServers"); len(names) != 0 {
		t.Fatalf("corrupt = %v, want []", names)
	}
	if names := readJSONMapKeys(filepath.Join(dir, "nested.json"), "mcpServers"); len(names) != 1 || names[0] != "inner" {
		t.Fatalf("nested = %v, want [inner]", names)
	}
	if names := readJSONMapKeys(filepath.Join(dir, "missing.json"), "mcpServers"); len(names) != 0 {
		t.Fatalf("missing file = %v, want []", names)
	}
}

func TestSystemEnvMCPSources(t *testing.T) {
	home := t.TempDir()
	writeTree(t, home, map[string]string{
		// claude config: top-level mcpServers with two real servers.
		".claude.json": `{"mcpServers": {"codegraph": {}, "playwright": {}}}`,
		// gemini: one server.
		".gemini/settings.json": `{"mcpServers": {"search": {}}}`,
		// codex: two [mcp_servers.x] tables (real newlines — TOML is line-based).
		".codex/config.toml": "[mcp_servers.cg]\ncommand=\"cg\"\n\n[mcp_servers.git]\ncommand=\"g\"\n",
		// opencode: MCP lives under the top-level `mcp` key (not mcpServers).
		".config/opencode/opencode.json": `{"mcp": {"codegraph": {}}, "provider": {}}`,
		// claude's project-level MCP config (a system-env session's cwd is HOME,
		// so this IS the project root claude reads): provider claude, not a bucket.
		".mcp.json": `{"mcpServers": {"shared": {}}}`,
	})
	srcs := systemEnvMCPSources(home)
	// opencode's codegraph shares a name with claude's; both stay (different
	// providers), which is the whole point of the per-editor view.
	wantProviders := map[string]map[string]bool{
		"codegraph":  {"claude": true, "opencode": true},
		"playwright": {"claude": true},
		"search":     {"gemini": true},
		"cg":         {"codex": true},
		"git":        {"codex": true},
		"shared":     {"claude": true},
	}
	got := map[string]map[string]bool{}
	for _, s := range srcs {
		if got[s.name] == nil {
			got[s.name] = map[string]bool{}
		}
		got[s.name][s.provider] = true
	}
	if len(got) != len(wantProviders) {
		t.Fatalf("got %d distinct names %v, want %d", len(got), got, len(wantProviders))
	}
	for name, providers := range wantProviders {
		gotProviders, ok := got[name]
		if !ok {
			t.Errorf("missing source %q", name)
			continue
		}
		for prov := range providers {
			if !gotProviders[prov] {
				t.Errorf("source %q missing provider %q (got %v)", name, prov, gotProviders)
			}
		}
	}
}

// The reader set is the tab-attribution contract with the console: it must
// mirror what the runtime actually loads (editorconfig.go runtimeSkillsDir /
// runtimePluginsDir), or resources show up under the wrong editor's tab.
func TestSystemEnvScanTargetReaders(t *testing.T) {
	for _, target := range systemEnvScanTargets("") {
		rel := filepath.Join(target.rel...)
		var want []string
		switch rel {
		case filepath.Join(".claude", "skills"):
			want = []string{"claude"}
		case filepath.Join(".agents", "skills"):
			want = []string{"codex", "gemini", "opencode"}
		case filepath.Join(".agents", "plugins"):
			want = []string{"claude", "codex", "opencode"}
		case filepath.Join(".gemini", "extensions"):
			want = []string{"gemini"}
		default:
			t.Fatalf("unmapped scan target %s", rel)
		}
		if strings.Join(target.readers, ",") != strings.Join(want, ",") {
			t.Errorf("%s readers = %v, want %v", rel, target.readers, want)
		}
	}
}

// The same resource found under two discovery paths (a platform skill mirrored
// into .claude/skills and .agents/skills) must union its readers so it shows up
// in every editor tab that loads it.
func TestMergeStrings(t *testing.T) {
	got := mergeStrings([]string{"claude"}, []string{"codex", "claude", "gemini"})
	want := []string{"claude", "codex", "gemini"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mergeStrings = %v, want %v", got, want)
	}
	if mergeStrings(nil, nil) != nil {
		t.Fatalf("mergeStrings(nil, nil) should stay nil")
	}
}

func TestSystemEnvClaudePlugins(t *testing.T) {
	home := t.TempDir()
	// A plugin installed through a marketplace: the ledger maps
	// "<name>@<marketplace>" to per-scope records; description and version
	// come from .claude-plugin/plugin.json at the install path. The entry's
	// Name is the full key (not the bare name) so two marketplaces shipping
	// the same plugin name stay distinct rows under the server's
	// (node, kind, name) unique constraint.
	manifest := `{"name": "frontend-design", "version": "1.2.0", "description": "UI skill"}`
	ledger := `{"version": 1, "plugins": {
		"frontend-design@claude-code-plugins": [
			{"scope": "user", "installPath": "cache/claude-code-plugins/frontend-design/1.2.0", "version": "1.2.0"}
		]
	}}`
	writeTree(t, home, map[string]string{
		".claude/plugins/installed_plugins.json":                       ledger,
		".claude/plugins/cache/claude-code-plugins/frontend-design/1.2.0/.claude-plugin/plugin.json": manifest,
	})
	got := systemEnvClaudePlugins(home)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got), got)
	}
	e := got[0]
	if e.GetKind() != systemEnvKindPlugin {
		t.Errorf("kind = %q, want plugin", e.GetKind())
	}
	if e.GetName() != "frontend-design@claude-code-plugins" {
		t.Errorf("name = %q, want full key", e.GetName())
	}
	if e.GetProvider() != "claude" {
		t.Errorf("provider = %q, want claude", e.GetProvider())
	}
	if strings.Join(e.GetReaders(), ",") != "claude" {
		t.Errorf("readers = %v, want [claude]", e.GetReaders())
	}
	if e.GetVersion() != "1.2.0" {
		t.Errorf("version = %q, want 1.2.0", e.GetVersion())
	}
	if e.GetDescription() != "UI skill" {
		t.Errorf("description = %q, want UI skill", e.GetDescription())
	}
	if e.GetPath() != ".claude/plugins/cache/claude-code-plugins/frontend-design/1.2.0" {
		t.Errorf("path = %q, want relative", e.GetPath())
	}
	if e.GetPlatformManaged() {
		t.Errorf("platform_managed = true, want false")
	}
	// The archive lookup must resolve the full key back to the install dir.
	resolved := claudePluginInstallPath(home, e.GetName())
	if resolved != filepath.Join(home, ".claude", "plugins", "cache", "claude-code-plugins", "frontend-design", "1.2.0") {
		t.Errorf("claudePluginInstallPath = %q, want the install dir", resolved)
	}
}

func TestSystemEnvClaudePluginsManifestFallback(t *testing.T) {
	// Some plugin manifests omit version; the ledger's own version (a git sha)
	// must become the reported version when the manifest has none.
	home := t.TempDir()
	manifest := `{"name": "broken", "description": "no version here"}`
	ledger := `{"plugins": {"broken@market": [{"installPath": "cache/market/broken/sha", "version": "sha123"}]}}`
	writeTree(t, home, map[string]string{
		".claude/plugins/installed_plugins.json":            ledger,
		".claude/plugins/cache/market/broken/sha/.claude-plugin/plugin.json": manifest,
	})
	got := systemEnvClaudePlugins(home)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].GetVersion() != "sha123" {
		t.Errorf("version = %q, want sha123 fallback", got[0].GetVersion())
	}
	if got[0].GetDescription() != "no version here" {
		t.Errorf("description = %q, want fallback desc", got[0].GetDescription())
	}
}

// Compile-time assertion that the proto Entry exposes the new description field,
// so renaming or dropping it is caught at build time.
var _ = agentcomposev2.NodeSystemEnvEntry{}
