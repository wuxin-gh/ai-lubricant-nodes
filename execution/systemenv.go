package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// System environment (env_mode="system") resource visibility and maintenance.
//
// A system-env session runs the editor against the node operator's REAL HOME, so
// the providers already discover whatever the operator installed there:
// claude reads ~/.claude/skills, the provider-neutral runners read
// ~/.agents/{skills,plugins}, gemini reads ~/.gemini/extensions, and MCP comes
// from ~/.mcp.json. The session code deliberately never writes those paths (see
// applyInitialConfig's isSystemEnv early return) because the shared-environment
// sync is exact-set + prune, which would delete the operator's own setup.
//
// That left the whole set invisible to the server: a system-env task silently got
// capabilities nobody could see or manage. This file closes that gap with three
// node-level operations, all addressed by node (there is exactly one operator
// HOME per node, so no env_id):
//
//   - inspectSystemEnv   read-only enumeration of what the providers would find
//   - syncSystemEnv      INCREMENTAL install / manifest-scoped removal
//   - archiveSystemEnvResource  tar one entry out and POST it to the server
//
// The safety rule that shapes all of it: the HOME is not ours. Anything we did
// not install is read-only — reported, never pruned, never removed. Ownership is
// tracked in a manifest we own (systemEnvManifestPath), and removal refuses any
// name that is not in it.

const (
	systemEnvKindSkill  = "skill"
	systemEnvKindPlugin = "plugin"
	systemEnvKindMCP    = "mcp"
)

// systemEnvHome resolves the operator's HOME, gated on the same opt-in that
// resolveHome's system branch uses. Every operation in this file goes through it,
// so a node that never enabled system mode cannot be made to read or write the
// operator's home by a server frame.
func (m *sessionManager) systemEnvHome() (string, error) {
	if !m.opts.systemEnvAllowed {
		return "", fmt.Errorf("system environment is not enabled on this node (auto: host installs allow it, containers refuse it; override with AGENT_COMPOSE_NODE_ALLOW_SYSTEM_ENV=on|off)")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("user home is empty")
	}
	return home, nil
}

// systemEnvManifestPath is where we record what the PLATFORM installed into the
// operator's HOME. It lives under our own dot-dir, not next to the resources, so
// it never looks like a resource to a provider scan.
func systemEnvManifestPath(home string) string {
	return filepath.Join(home, ".agent-compose", "system-env.json")
}

// systemEnvManifest is the on-disk ownership ledger. Keys are "<kind>/<name>";
// the value carries when we installed it and where, purely for diagnosis.
type systemEnvManifest struct {
	Entries map[string]systemEnvManifestEntry `json:"entries"`
}

type systemEnvManifestEntry struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Provider    string `json:"provider,omitempty"`
	RelPath     string `json:"rel_path,omitempty"`
	InstalledAt string `json:"installed_at,omitempty"`
}

func systemEnvManifestKey(kind, name string) string {
	return strings.ToLower(strings.TrimSpace(kind)) + "/" + strings.TrimSpace(name)
}

// loadSystemEnvManifest reads the ledger. A missing or corrupt file yields an
// empty manifest rather than an error: the worst consequence is that entries we
// did install look operator-owned, which fails CLOSED (we refuse to remove them).
func loadSystemEnvManifest(home string) *systemEnvManifest {
	manifest := &systemEnvManifest{Entries: map[string]systemEnvManifestEntry{}}
	raw, err := os.ReadFile(systemEnvManifestPath(home))
	if err != nil {
		return manifest
	}
	var parsed systemEnvManifest
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return manifest
	}
	if parsed.Entries == nil {
		parsed.Entries = map[string]systemEnvManifestEntry{}
	}
	return &parsed
}

func saveSystemEnvManifest(home string, manifest *systemEnvManifest) error {
	path := systemEnvManifestPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create manifest dir: %w", err)
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// systemEnvScanTarget is one directory a provider scans for resources. readers
// is the set of editors whose runtime would LOAD a resource found here — the
// tab-attribution fact reported to the console (provider only names which
// directory the entry was found under, which is not the same question).
type systemEnvScanTarget struct {
	kind     string
	provider string
	rel      []string
	readers  []string
}

// Editors referenced by readers, in the fixed order the console shows tabs in.
const (
	editorClaude    = "claude"
	editorCodex     = "codex"
	editorGemini    = "gemini"
	editorOpencode  = "opencode"
)

// systemEnvScanTargets enumerates every discovery path a provider actually reads.
// Keep the paths in sync with runtimeSkillsDir / runtimePluginsDir in
// editorconfig.go — if those move, a system-env session's resources move with
// them and this scan must follow, or the console would report an inventory the
// editor doesn't use. Keep readers in sync too: claude loads skills only from
// .claude/skills, every other runner loads them from .agents/skills; plugins
// load from .agents/plugins except gemini, which discovers .gemini/extensions.
func systemEnvScanTargets(provider string) []systemEnvScanTarget {
	all := []systemEnvScanTarget{
		{kind: systemEnvKindSkill, provider: "claude", rel: []string{".claude", "skills"}, readers: []string{editorClaude}},
		{kind: systemEnvKindSkill, provider: "", rel: []string{".agents", "skills"}, readers: []string{editorCodex, editorGemini, editorOpencode}},
		{kind: systemEnvKindPlugin, provider: "", rel: []string{".agents", "plugins"}, readers: []string{editorClaude, editorCodex, editorOpencode}},
		{kind: systemEnvKindPlugin, provider: "gemini", rel: []string{".gemini", "extensions"}, readers: []string{editorGemini}},
	}
	want := normalizeProvider(strings.TrimSpace(provider))
	if want == "" {
		return all
	}
	var out []systemEnvScanTarget
	for _, target := range all {
		// The .agents tree is read by non-claude runners (and claude's plugins),
		// so it stays in scope for any provider filter.
		if target.provider == "" || target.provider == want {
			out = append(out, target)
		}
	}
	return out
}

// systemEnvMCPSource is one MCP server observed in one of the operator's editor
// config files. MCP is config rather than files, so it is reported for visibility
// only — nothing here installs or removes it (writing it would leak per-task
// tokens into the operator's home). readers is the editor set that would load it.
type systemEnvMCPSource struct {
	name     string
	provider string   // editor whose config declared it ("" never happens: every source file has an owner)
	readers  []string // editors that would load this server
	path     string   // HOME-relative config file, so the console can name the source
}

// systemEnvMCPSources sweeps every config file an editor actually reads MCP
// servers from. ~/.mcp.json alone was never the whole story: `claude mcp add`
// writes ~/.claude.json (top-level mcpServers), gemini keeps its own
// ~/.gemini/settings.json, codex uses TOML tables in ~/.codex/config.toml, and
// opencode reads ~/.config/opencode/opencode.json — whose MCP map is under the
// top-level `mcp` key rather than `mcpServers`. ~/.mcp.json is claude's
// project-level config (a system-env session runs with cwd=HOME, so that IS the
// project root claude reads) — hence provider "claude", not a shared bucket.
func systemEnvMCPSources(home string) []systemEnvMCPSource {
	fromJSON := func(path, provider, rel, field string) []systemEnvMCPSource {
		var out []systemEnvMCPSource
		for _, name := range readJSONMapKeys(path, field) {
			out = append(out, systemEnvMCPSource{name: name, provider: provider, readers: []string{provider}, path: rel})
		}
		return out
	}
	var out []systemEnvMCPSource
	out = append(out, fromJSON(filepath.Join(home, ".claude.json"), editorClaude, ".claude.json", "mcpServers")...)
	out = append(out, fromJSON(filepath.Join(home, ".gemini", "settings.json"), editorGemini, ".gemini/settings.json", "mcpServers")...)
	for _, name := range readCodexMCPServers(filepath.Join(home, ".codex", "config.toml")) {
		out = append(out, systemEnvMCPSource{name: name, provider: editorCodex, readers: []string{editorCodex}, path: ".codex/config.toml"})
	}
	out = append(out, fromJSON(filepath.Join(home, ".config", "opencode", "opencode.json"), editorOpencode, ".config/opencode/opencode.json", "mcp")...)
	out = append(out, fromJSON(filepath.Join(home, ".mcp.json"), editorClaude, ".mcp.json", "mcpServers")...)
	return out
}

// readJSONMapKeys returns the sorted keys of a top-level object field, for
// config shapes like {"mcpServers": {"name": {...}}}.
func readJSONMapKeys(path, field string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	var inner map[string]json.RawMessage
	if json.Unmarshal(doc[field], &inner) != nil {
		return nil
	}
	names := make([]string, 0, len(inner))
	for name := range inner {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	sort.Strings(names)
	return names
}

// readCodexMCPServers lists MCP server names from ~/.codex/config.toml by
// scanning `[mcp_servers.<name>]` table headers. The Go stdlib has no TOML
// parser, and a header line is unambiguous — values never contain `]` in codex
// server names — so pulling in a dependency to list names is not justified.
func readCodexMCPServers(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[mcp_servers.") || !strings.HasSuffix(line, "]") {
			continue
		}
		name := strings.TrimSpace(line[len("[mcp_servers.") : len(line)-1])
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// systemEnvClaudePlugins enumerates plugins installed through claude's own
// marketplace system under ~/.claude/plugins. That tree is separate from the
// provider-neutral .agents/plugins directory, so without this sweep the console
// shows no plugins at all for an operator who only ever used `claude plugin`.
// installed_plugins.json maps "<name>@<marketplace>" to per-scope install
// records; the same plugin name from two marketplaces is two installs and is
// reported twice, keyed (and deduped) by the full name@marketplace string.
func systemEnvClaudePlugins(home string) []*agentcomposev2.NodeSystemEnvEntry {
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"))
	if err != nil {
		return nil
	}
	var doc struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
			Version     string `json:"version"`
		} `json:"plugins"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	base := filepath.Join(home, ".claude", "plugins")
	var out []*agentcomposev2.NodeSystemEnvEntry
	seen := map[string]bool{}
	for key, scopes := range doc.Plugins {
		if len(scopes) == 0 || seen[key] {
			continue
		}
		seen[key] = true
		// The entry's Name is the full "<name>@<marketplace>" key. Claude lets
		// the same plugin name be installed from two marketplaces as two real
		// installs, and the server table is unique on (node, kind, name) — the
		// composite key keeps both rows and is exactly what the archive lookup
		// needs to find the install again.
		name := strings.TrimSpace(key)
		if name == "" {
			continue
		}
		record := scopes[0]
		installPath := strings.TrimSpace(record.InstallPath)
		if installPath == "" {
			continue
		}
		if !filepath.IsAbs(installPath) {
			installPath = filepath.Join(base, installPath)
		}
		// The manifest at the install path is the canonical name/version/
		// description; the ledger's own version is the fallback (some manifests
		// omit it and claude records the git sha instead).
		version, description := readResourceMeta(installPath)
		if version == "" {
			version = strings.TrimSpace(record.Version)
		}
		relPath := filepath.ToSlash(strings.TrimPrefix(installPath, home+string(os.PathSeparator)))
		out = append(out, &agentcomposev2.NodeSystemEnvEntry{
			Kind:            systemEnvKindPlugin,
			Name:            name,
			Version:         version,
			Provider:        "claude",
			Path:            relPath,
			PlatformManaged: false,
			Description:     description,
			Readers:         []string{editorClaude},
		})
	}
	return out
}

// claudePluginInstallPath resolves a "<name>@<marketplace>" inventory name back
// to its install directory via the claude plugin ledger. Best-effort: an empty
// return means "not a claude-marketplace plugin (or the ledger moved)" and the
// caller falls back to the discovery-path lookup.
func claudePluginInstallPath(home, key string) string {
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"))
	if err != nil {
		return ""
	}
	var doc struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	scopes, ok := doc.Plugins[strings.TrimSpace(key)]
	if !ok || len(scopes) == 0 {
		return ""
	}
	installPath := strings.TrimSpace(scopes[0].InstallPath)
	if installPath == "" {
		return ""
	}
	if !filepath.IsAbs(installPath) {
		installPath = filepath.Join(home, ".claude", "plugins", installPath)
	}
	if info, statErr := os.Stat(installPath); statErr != nil || !info.IsDir() {
		return ""
	}
	return installPath
}

// inspectSystemEnv enumerates what the providers would discover in the operator's
// HOME. Missing directories are not an error (a fresh machine simply has none),
// so the caller can always render an inventory instead of a failure.
func (m *sessionManager) inspectSystemEnv(frame *agentcomposev2.NodeInspectSystemEnv) ([]*agentcomposev2.NodeSystemEnvEntry, error) {
	home, err := m.systemEnvHome()
	if err != nil {
		return nil, err
	}
	manifest := loadSystemEnvManifest(home)

	var out []*agentcomposev2.NodeSystemEnvEntry
	seen := map[string]int{}
	for _, target := range systemEnvScanTargets(frame.GetProvider()) {
		dir := filepath.Join(append([]string{home}, target.rel...)...)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			// One resource can live under two discovery paths (a platform skill is
			// mirrored into both .claude/skills and .agents/skills). Report it once:
			// the console shows capability, not filesystem layout. The readers of
			// every path it was found under merge, so the merged entry lands in
			// every editor tab whose runtime loads it; provider/path stay the first
			// discovery location so they always agree with each other.
			key := systemEnvManifestKey(target.kind, name)
			if idx, ok := seen[key]; ok {
				existing := out[idx]
				existing.Readers = mergeStrings(existing.GetReaders(), target.readers)
				continue
			}
			seen[key] = len(out)
			_, managed := manifest.Entries[key]
			version, description := readResourceMeta(filepath.Join(dir, name))
			out = append(out, &agentcomposev2.NodeSystemEnvEntry{
				Kind:            target.kind,
				Name:            name,
				Version:         version,
				Provider:        target.provider,
				Path:            filepath.ToSlash(filepath.Join(append(append([]string{}, target.rel...), name)...)),
				PlatformManaged: managed,
				Description:     description,
				Readers:         append([]string{}, target.readers...),
			})
		}
	}

	// claude marketplace plugins live in their own tree, outside every scan
	// target above; sweep them separately so the console finally sees plugins.
	// They dedupe internally by name@marketplace, so the same plugin name from
	// two marketplaces stays two rows.
	out = append(out, systemEnvClaudePlugins(home)...)

	// MCP from every editor config, not just one. A server configured in two of
	// the same editor's files (.claude.json + .mcp.json) collapses to one row;
	// the same name under different editors stays separate — different runtimes
	// would load different definitions.
	seenMCP := map[string]bool{}
	for _, src := range systemEnvMCPSources(home) {
		key := src.provider + "/" + src.name
		if seenMCP[key] {
			continue
		}
		seenMCP[key] = true
		out = append(out, &agentcomposev2.NodeSystemEnvEntry{
			Kind:     systemEnvKindMCP,
			Name:     src.name,
			Provider: src.provider,
			Path:     src.path,
			Readers:  append([]string{}, src.readers...),
			// MCP is never platform-installed here: we deliberately do not write
			// the operator's MCP config, so every entry is theirs.
			PlatformManaged: false,
		})
	}
	return out, nil
}

// mergeStrings unions two string slices preserving first-slice order, then any
// new second-slice values. Readers stay in a stable order for display; an empty
// union returns nil so the proto field stays unset rather than empty.
func mergeStrings(base, extra []string) []string {
	seen := map[string]bool{}
	for _, v := range base {
		seen[v] = true
	}
	out := append([]string{}, base...)
	for _, v := range extra {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// systemEnvInstallDirs is where we PUT a resource we install. Deliberately the
// provider-neutral .agents tree only: writing into a provider's own dir
// (~/.claude, ~/.gemini) puts our files inside config the operator manages
// directly, and every runner reads .agents anyway. Claude also reads
// .claude/skills, so a platform skill is mirrored there too — that mirror is
// tracked in the manifest, so removal cleans both.
func systemEnvInstallDirs(home, kind string) []string {
	switch kind {
	case systemEnvKindSkill:
		return []string{
			filepath.Join(home, ".agents", "skills"),
			filepath.Join(home, ".claude", "skills"),
		}
	case systemEnvKindPlugin:
		return []string{filepath.Join(home, ".agents", "plugins")}
	default:
		return nil
	}
}

// systemEnvKindReaders is the reader set of a freshly written resource: the
// union over every discovery path systemEnvInstallDirs lays it down in (skills
// land in .agents/skills and are mirrored to .claude/skills, so all four
// runtimes load them).
func systemEnvKindReaders(kind string) []string {
	switch kind {
	case systemEnvKindSkill:
		return []string{editorClaude, editorCodex, editorGemini, editorOpencode}
	case systemEnvKindPlugin:
		return []string{editorClaude, editorCodex, editorOpencode}
	default:
		return nil
	}
}

// syncSystemEnv installs platform resources into the operator's HOME and removes
// platform-installed ones. It is INCREMENTAL by construction:
//
//   - nothing is pruned — a resource absent from this call is left alone;
//   - an existing target is skipped unless overwrite is set, because the copy on
//     disk may be the operator's own and silently replacing it would destroy work
//     the platform never created;
//   - removal only accepts names present in our manifest, so a hand-installed
//     resource cannot be deleted through this API even if the server asks.
//
// The returned entries describe what actually happened per resource (installed /
// skipped / removed), so the console can report outcomes without a second call.
func (m *sessionManager) syncSystemEnv(ctx context.Context, frame *agentcomposev2.NodeSyncSystemEnv) ([]*agentcomposev2.NodeSystemEnvEntry, error) {
	home, err := m.systemEnvHome()
	if err != nil {
		return nil, err
	}
	manifest := loadSystemEnvManifest(home)
	session := m.systemEnvSyncSession(ctx, home)
	now := time.Now().UTC().Format(time.RFC3339)
	var touched []*agentcomposev2.NodeSystemEnvEntry

	install := func(kind, name string, src resourceSource) error {
		safe := sanitizeSessionDir(name)
		dirs := systemEnvInstallDirs(home, kind)
		if len(dirs) == 0 {
			return fmt.Errorf("unsupported kind %q", kind)
		}
		primary := filepath.Join(dirs[0], safe)
		// Existence check covers every discovery path, not just ours: a skill the
		// operator dropped in ~/.claude/skills must count as "already there".
		existing := ""
		for _, dir := range dirs {
			if _, statErr := os.Stat(filepath.Join(dir, safe)); statErr == nil {
				existing = filepath.Join(dir, safe)
				break
			}
		}
		_, managed := manifest.Entries[systemEnvManifestKey(kind, name)]
		if existing != "" && !frame.GetOverwrite() {
			touched = append(touched, &agentcomposev2.NodeSystemEnvEntry{
				Kind: kind, Name: name, Path: "skipped",
				Version:         readResourceVersion(existing),
				PlatformManaged: managed,
				Readers:         systemEnvKindReaders(kind),
			})
			return nil
		}
		if err := os.MkdirAll(dirs[0], 0o755); err != nil {
			return fmt.Errorf("prepare %s: %w", dirs[0], err)
		}
		if err := m.fetchResource(session, src, primary); err != nil {
			return fmt.Errorf("%s %s: %w", kind, name, err)
		}
		// Mirror into the remaining discovery paths so the resource reaches every
		// runner that reads them (claude's SDK only scans .claude/skills).
		for _, dir := range dirs[1:] {
			mirror := filepath.Join(dir, safe)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("prepare %s: %w", dir, err)
			}
			if err := replaceTree(primary, mirror); err != nil {
				return fmt.Errorf("mirror %s %s: %w", kind, name, err)
			}
		}
		manifest.Entries[systemEnvManifestKey(kind, name)] = systemEnvManifestEntry{
			Kind:        kind,
			Name:        name,
			RelPath:     filepath.ToSlash(strings.TrimPrefix(primary, home+string(os.PathSeparator))),
			InstalledAt: now,
		}
		touched = append(touched, &agentcomposev2.NodeSystemEnvEntry{
			Kind: kind, Name: name, Path: "installed",
			Version:         readResourceVersion(primary),
			PlatformManaged: true,
			Readers:         systemEnvKindReaders(kind),
		})
		return nil
	}

	for _, skill := range frame.GetSkills() {
		name := strings.TrimSpace(skill.GetName())
		if name == "" {
			continue
		}
		if err := install(systemEnvKindSkill, name, skillSource(skill)); err != nil {
			return touched, err
		}
	}
	for _, plugin := range frame.GetPlugins() {
		name := strings.TrimSpace(plugin.GetName())
		if name == "" {
			continue
		}
		src := resourceSource{url: strings.TrimSpace(plugin.GetUrl())}
		if err := install(systemEnvKindPlugin, name, src); err != nil {
			return touched, err
		}
	}

	for _, target := range frame.GetRemove() {
		kind, name, ok := splitSystemEnvRef(target)
		if !ok {
			return touched, fmt.Errorf("invalid remove entry %q (want kind/name)", target)
		}
		key := systemEnvManifestKey(kind, name)
		if _, managed := manifest.Entries[key]; !managed {
			// The whole point of the manifest: refuse to touch anything we did not
			// install, even on an explicit server request.
			return touched, fmt.Errorf("refusing to remove %s %q: not installed by the platform", kind, name)
		}
		safe := sanitizeSessionDir(name)
		for _, dir := range systemEnvInstallDirs(home, kind) {
			if err := os.RemoveAll(filepath.Join(dir, safe)); err != nil {
				return touched, fmt.Errorf("remove %s %s: %w", kind, name, err)
			}
		}
		delete(manifest.Entries, key)
		touched = append(touched, &agentcomposev2.NodeSystemEnvEntry{
			Kind: kind, Name: name, Path: "removed", PlatformManaged: false,
			Readers: systemEnvKindReaders(kind),
		})
	}

	if err := saveSystemEnvManifest(home, manifest); err != nil {
		return touched, err
	}
	m.logger.Info("system env synced",
		"installed_or_skipped", len(frame.GetSkills())+len(frame.GetPlugins()),
		"removed", len(frame.GetRemove()), "overwrite", frame.GetOverwrite())
	return touched, nil
}

// splitSystemEnvRef parses a "kind/name" remove target.
func splitSystemEnvRef(value string) (string, string, bool) {
	kind, name, ok := strings.Cut(strings.TrimSpace(value), "/")
	kind = strings.ToLower(strings.TrimSpace(kind))
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return "", "", false
	}
	if kind != systemEnvKindSkill && kind != systemEnvKindPlugin {
		return "", "", false
	}
	return kind, name, true
}

// systemEnvSyncSession fabricates the minimal nodeSession fetchResource needs.
// There is no real session behind a system-env install — it is node-level
// maintenance — so this carries only the home and the context, mirroring
// envSyncSession's shape for the shared tier.
func (m *sessionManager) systemEnvSyncSession(ctx context.Context, home string) *nodeSession {
	return &nodeSession{
		id:      "system-env",
		home:    home,
		workDir: home,
		baseCtx: ctx,
	}
}

// archiveSystemEnvResource tars one resource out of the operator's HOME and POSTs
// it to a server-provided endpoint, so a locally installed skill/plugin can enter
// the platform library and be reused on other nodes.
//
// The bytes travel over HTTP rather than the NodeConnect stream: the stream is a
// command channel with a bounded per-connection queue, and pushing a multi-MB
// archive through it would stall session commands and heartbeats behind it.
func (m *sessionManager) archiveSystemEnvResource(ctx context.Context, frame *agentcomposev2.NodeArchiveSystemEnvResource) error {
	home, err := m.systemEnvHome()
	if err != nil {
		return err
	}
	kind := strings.ToLower(strings.TrimSpace(frame.GetKind()))
	name := strings.TrimSpace(frame.GetName())
	if name == "" {
		return fmt.Errorf("archive system env: name is required")
	}
	if kind != systemEnvKindSkill && kind != systemEnvKindPlugin {
		return fmt.Errorf("archive system env: unsupported kind %q (want skill|plugin)", kind)
	}
	uploadURL := strings.TrimSpace(frame.GetUploadUrl())
	if uploadURL == "" {
		return fmt.Errorf("archive system env: upload_url is required")
	}

	// Find the resource in any discovery path — the operator may have installed it
	// under a provider-specific dir we never write to.
	safe := sanitizeSessionDir(name)
	src := ""
	for _, target := range systemEnvScanTargets("") {
		if target.kind != kind {
			continue
		}
		candidate := filepath.Join(append(append([]string{home}, target.rel...), safe)...)
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			src = candidate
			break
		}
	}
	// claude marketplace plugins are named "<name>@<marketplace>" and live outside
	// every scan target; resolve them through the install ledger instead.
	if src == "" && kind == systemEnvKindPlugin && strings.Contains(name, "@") {
		src = claudePluginInstallPath(home, name)
	}
	if src == "" {
		return fmt.Errorf("archive system env: %s %q not found in the operator home", kind, name)
	}

	archive, err := tarGzDir(src)
	if err != nil {
		return fmt.Errorf("archive system env: pack %s %s: %w", kind, name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("archive system env: build upload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/gzip")
	if token := strings.TrimSpace(frame.GetUploadToken()); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("archive system env: upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("archive system env: upload rejected: HTTP %d %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	m.logger.Info("system env resource archived", "kind", kind, "name", name, "bytes", len(archive))
	return nil
}

// tarGzDir packs one directory into an in-memory tar.gz whose members are paths
// relative to that directory (so the archive root is the resource itself, matching
// what fetchArchive expects to extract). Regular files and dirs only: symlinks are
// skipped rather than followed, so an archive can never escape the source tree.
func tarGzDir(root string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		// Skip our own bookkeeping and VCS metadata: the server only needs content.
		if parts := strings.Split(filepath.ToSlash(rel), "/"); len(parts) > 0 && parts[0] == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		if !info.Mode().IsRegular() && !d.IsDir() {
			return nil // symlinks / devices are never archived
		}
		header, hdrErr := tar.FileInfoHeader(info, "")
		if hdrErr != nil {
			return hdrErr
		}
		header.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			header.Name += "/"
		}
		if writeErr := tw.WriteHeader(header); writeErr != nil {
			return writeErr
		}
		if d.IsDir() {
			return nil
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return openErr
		}
		defer func() { _ = file.Close() }()
		_, copyErr := io.Copy(tw, file)
		return copyErr
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}


