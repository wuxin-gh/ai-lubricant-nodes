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

// systemEnvScanTarget is one directory a provider scans for resources.
type systemEnvScanTarget struct {
	kind     string
	provider string
	rel      []string
}

// systemEnvScanTargets enumerates every discovery path a provider actually reads.
// Keep in sync with runtimeSkillsDir / runtimePluginsDir in editorconfig.go — if
// those move, a system-env session's resources move with them and this scan must
// follow, or the console would report an inventory the editor doesn't use.
func systemEnvScanTargets(provider string) []systemEnvScanTarget {
	all := []systemEnvScanTarget{
		{kind: systemEnvKindSkill, provider: "claude", rel: []string{".claude", "skills"}},
		{kind: systemEnvKindSkill, provider: "", rel: []string{".agents", "skills"}},
		{kind: systemEnvKindPlugin, provider: "", rel: []string{".agents", "plugins"}},
		{kind: systemEnvKindPlugin, provider: "gemini", rel: []string{".gemini", "extensions"}},
	}
	want := normalizeProvider(strings.TrimSpace(provider))
	if want == "" {
		return all
	}
	var out []systemEnvScanTarget
	for _, target := range all {
		// The provider-neutral .agents tree is read by every non-claude runner, so
		// it stays in scope for any provider filter.
		if target.provider == "" || target.provider == want {
			out = append(out, target)
		}
	}
	return out
}

// systemEnvMCPNames reads the MCP server names out of the operator's claude MCP
// config. MCP is config rather than files, so it is reported for visibility only
// — nothing here installs or removes it (writing it would leak per-task tokens
// into the operator's home).
func systemEnvMCPNames(home string) ([]string, string) {
	path := filepath.Join(home, ".mcp.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, ""
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, ""
	}
	names := make([]string, 0, len(doc.MCPServers))
	for name := range doc.MCPServers {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	sort.Strings(names)
	return names, ".mcp.json"
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
	seen := map[string]bool{}
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
			// One resource can appear under two providers' paths (a skill mirrored
			// into both .claude/skills and .agents/skills). Report it once: the
			// console shows capability, not filesystem layout.
			key := systemEnvManifestKey(target.kind, name)
			if seen[key] {
				continue
			}
			seen[key] = true
			_, managed := manifest.Entries[key]
			out = append(out, &agentcomposev2.NodeSystemEnvEntry{
				Kind:            target.kind,
				Name:            name,
				Version:         readResourceVersion(filepath.Join(dir, name)),
				Provider:        target.provider,
				Path:            filepath.ToSlash(filepath.Join(append(append([]string{}, target.rel...), name)...)),
				PlatformManaged: managed,
			})
		}
	}

	if names, rel := systemEnvMCPNames(home); rel != "" {
		for _, name := range names {
			out = append(out, &agentcomposev2.NodeSystemEnvEntry{
				Kind: systemEnvKindMCP,
				Name: name,
				Path: rel,
				// MCP is never platform-installed here: we deliberately do not write
				// the operator's MCP config, so every entry is theirs.
				PlatformManaged: false,
			})
		}
	}
	return out, nil
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


