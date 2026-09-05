package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// This file writes each editor's on-disk configuration so the provider CLI on
// this node talks DIRECTLY to the caller's LLM service (no server-side proxy)
// and loads the requested MCP servers. The node is self-contained: it does not
// import pkg/llms (which pulls in daemon-only, non-cross-compilable deps); the
// config formats here mirror what the daemon writes for its local sessions.
//
// Config lives under the session home dir (<workDir>/.agent-compose/home), which
// the runtime is told to use as the editor's HOME via --home. Skills and plugins
// sync into their own per-session dirs.

const (
	// codexManagedMCPStart/End bracket the block this node owns in config.toml so
	// a rewrite replaces exactly the managed servers and leaves anything else the
	// user placed in the file intact.
	codexManagedMCPStart = "# agent-compose managed mcp start"
	codexManagedMCPEnd   = "# agent-compose managed mcp end"

	// llmKeyEnvVar is the env var codex's config.toml references for the API key,
	// so the key stays out of the on-disk config file.
	llmKeyEnvVar = "AGENT_COMPOSE_LLM_KEY"
)

// writeLLMConfig points the session's editor at the LLM service in llm, writing
// whatever on-disk config that provider needs. For env-only providers (claude,
// gemini) the LLM config is delivered through the process environment (see
// llmEnv) and this only needs to handle providers that read a config file.
func (m *sessionManager) writeLLMConfig(session *nodeSession, llm *agentcomposev2.NodeLLMConfig) error {
	if llm == nil {
		return nil
	}
	switch normalizeProvider(session.provider) {
	case "codex":
		return writeCodexRuntimeConfig(session.home, llm)
	case "opencode":
		return writeOpenCodeRuntimeConfig(session.home, llm)
	default:
		// claude/gemini: env-based, handled by llmEnv at process start.
		return nil
	}
}

// writeMCPConfig rewrites the session editor's MCP configuration to exactly the
// given set (empty clears the managed block).
//
// Two artifacts are written for every provider:
//
//   - the runtime's own input at <stateRoot>/agents/mcp/config.json, which
//     agent-compose-runtime reads (readMCPConfig) and hands to the provider SDK.
//     This is the authoritative channel: it is provider-agnostic and does not
//     depend on the CLI honouring HOME.
//   - the provider-native config under the session home, for CLIs that read
//     their own config file directly.
//
// Writing only the provider-native file used to leave the runtime with an empty
// MCP set, so session-scoped servers (e.g. issue-workflow) never reached the
// agent.
func (m *sessionManager) writeMCPConfig(session *nodeSession, mcps []*agentcomposev2.MCPServerSpec) error {
	if err := writeRuntimeMCPConfig(session.stateRoot, mcps); err != nil {
		return err
	}
	// The provider-native copy lives in HOME. When HOME is SHARED across tasks,
	// writing it would put this task's freshly minted MCP tokens in a directory
	// concurrent tasks read — and each task's write would clobber the previous
	// one's. The runtime reads the stateRoot copy above (per session, private),
	// so skipping the native copy costs nothing but the credential leak.
	if session.spec != nil && isSharedEnv(session.spec) {
		return nil
	}
	switch normalizeProvider(session.provider) {
	case "codex":
		return writeCodexMCPConfig(session.home, mcps)
	case "opencode":
		return writeOpenCodeMCPConfig(session.home, mcps)
	case "claude":
		return writeClaudeMCPConfig(session.home, mcps)
	default:
		return nil
	}
}

// runtimeMCPConfigPath is the file agent-compose-runtime reads for MCP servers.
// Keep in sync with the runtime's mcp-config.ts (agentMCPConfigPath).
func runtimeMCPConfigPath(stateRoot string) string {
	return filepath.Join(stateRoot, "agents", "mcp", "config.json")
}

// writeRuntimeMCPConfig writes the exact desired MCP set in the runtime's own
// schema. An empty set removes the file so the runtime sees no servers.
func writeRuntimeMCPConfig(stateRoot string, mcps []*agentcomposev2.MCPServerSpec) error {
	path := runtimeMCPConfigPath(stateRoot)
	servers := map[string]any{}
	for _, mcp := range mcps {
		name := strings.TrimSpace(mcp.GetName())
		if name == "" {
			continue
		}
		entry := map[string]any{}
		if isRemoteMCP(mcp) {
			entry["type"] = "remote"
			entry["transport"] = normalizeRuntimeTransport(mcp.GetTransport(), mcp.GetUrl())
			entry["url"] = strings.TrimSpace(mcp.GetUrl())
			if headers := runtimeEnvMap(mcp.GetHeaders()); headers != nil {
				entry["headers"] = headers
			}
		} else {
			entry["type"] = "local"
			entry["command"] = strings.TrimSpace(mcp.GetCommand())
			if args := mcp.GetArgs(); len(args) > 0 {
				entry["args"] = args
			}
			if env := runtimeEnvMap(mcp.GetEnv()); env != nil {
				entry["env"] = env
			}
		}
		servers[name] = entry
	}
	if len(servers) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove runtime mcp config: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create runtime mcp config dir: %w", err)
	}
	data, err := json.MarshalIndent(map[string]any{"mcps": servers}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write runtime mcp config: %w", err)
	}
	return nil
}

// runtimeEnvMap converts env/header specs into the runtime's {name: {value}}
// shape. It returns nil when there is nothing to write.
func runtimeEnvMap(items []*agentcomposev2.EnvVarSpec) map[string]any {
	out := map[string]any{}
	for _, item := range items {
		name := strings.TrimSpace(item.GetName())
		if name == "" {
			continue
		}
		out[name] = map[string]any{"value": item.GetValue()}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeRuntimeTransport picks the remote transport the runtime should use.
// An explicit spec value wins; otherwise an ".../sse" URL implies SSE and
// everything else defaults to streamable HTTP.
func normalizeRuntimeTransport(transport, url string) string {
	switch strings.ToLower(strings.TrimSpace(transport)) {
	case "sse":
		return "sse"
	case "http", "streamable_http", "streamable-http":
		return "http"
	}
	trimmed := strings.ToLower(strings.TrimSpace(url))
	if idx := strings.IndexAny(trimmed, "?#"); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	if strings.HasSuffix(strings.TrimRight(trimmed, "/"), "/sse") {
		return "sse"
	}
	return "http"
}

// normalizeProvider maps provider aliases to a canonical name.
func normalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude", "claude-code", "claude_code":
		return "claude"
	case "codex":
		return "codex"
	case "gemini", "gemini-cli", "gemini_cli":
		return "gemini"
	case "opencode", "open-code", "open_code":
		return "opencode"
	case "cursor", "cursor-agent", "cursor_agent":
		return "cursor"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

// ── codex (TOML) ────────────────────────────────────────────────────────────

func codexConfigPath(home string) string {
	return filepath.Join(home, ".codex", "config.toml")
}

func writeCodexRuntimeConfig(home string, llm *agentcomposev2.NodeLLMConfig) error {
	baseURL := strings.TrimRight(strings.TrimSpace(llm.GetEndpoint()), "/")
	model := strings.TrimSpace(llm.GetModel())
	if baseURL == "" {
		return nil
	}
	path := codexConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create codex config dir: %w", err)
	}
	wireAPI := normalizeWireAPI(llm.GetProtocol())
	reasoningEffort := strings.TrimSpace(llm.GetExtra()["REASONING_EFFORT"])
	// Preserve any managed MCP block already on disk when rewriting the head.
	existing, _ := os.ReadFile(path)
	managed := extractManagedTextBlock(string(existing), codexManagedMCPStart, codexManagedMCPEnd)
	head := fmt.Sprintf(`model_provider = "agent_compose"
model = %q
`, model)
	if reasoningEffort != "" {
		head += fmt.Sprintf("model_reasoning_effort = %q\n", reasoningEffort)
	}
	head += fmt.Sprintf(`
[model_providers.agent_compose]
name = "agent-compose"
base_url = %q
env_key = %q
wire_api = %q
request_max_retries = 30
stream_max_retries = 50
stream_idle_timeout_ms = 120000

[shell_environment_policy]
inherit = "all"

[history]
persistence = "save-all"
`, baseURL, llmKeyEnvVar, wireAPI)
	out := head
	if managed != "" {
		out += "\n" + managed + "\n"
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return fmt.Errorf("write codex config: %w", err)
	}
	return nil
}

func writeCodexMCPConfig(home string, mcps []*agentcomposev2.MCPServerSpec) error {
	path := codexConfigPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create codex config dir: %w", err)
	}
	existing, _ := os.ReadFile(path)
	managed := buildCodexManagedMCPBlock(mcps)
	merged := replaceManagedTextBlock(string(existing), codexManagedMCPStart, codexManagedMCPEnd, managed)
	if strings.TrimSpace(merged) == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove codex config: %w", err)
		}
		return nil
	}
	if err := os.WriteFile(path, []byte(merged), 0o644); err != nil {
		return fmt.Errorf("write codex mcp config: %w", err)
	}
	return nil
}

func buildCodexManagedMCPBlock(mcps []*agentcomposev2.MCPServerSpec) string {
	if len(mcps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(codexManagedMCPStart)
	b.WriteString("\n")
	sorted := append([]*agentcomposev2.MCPServerSpec(nil), mcps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetName() < sorted[j].GetName() })
	for _, mcp := range sorted {
		name := strings.TrimSpace(mcp.GetName())
		if name == "" {
			continue
		}
		fmt.Fprintf(&b, "[mcp_servers.%s]\n", name)
		if isRemoteMCP(mcp) {
			fmt.Fprintf(&b, "url = %q\n", strings.TrimSpace(mcp.GetUrl()))
			if hdrs := mcp.GetHeaders(); len(hdrs) > 0 {
				fmt.Fprintf(&b, "[mcp_servers.%s.http_headers]\n", name)
				for _, h := range hdrs {
					fmt.Fprintf(&b, "%s = %q\n", h.GetName(), h.GetValue())
				}
			}
		} else {
			fmt.Fprintf(&b, "command = %q\n", strings.TrimSpace(mcp.GetCommand()))
			if args := mcp.GetArgs(); len(args) > 0 {
				encoded, _ := json.Marshal(args)
				fmt.Fprintf(&b, "args = %s\n", string(encoded))
			}
			if env := mcp.GetEnv(); len(env) > 0 {
				fmt.Fprintf(&b, "[mcp_servers.%s.env]\n", name)
				for _, e := range env {
					fmt.Fprintf(&b, "%s = %q\n", e.GetName(), e.GetValue())
				}
			}
		}
	}
	b.WriteString(codexManagedMCPEnd)
	return b.String()
}

// ── opencode (JSON) ───────────────────────────────────────────────────────────

func openCodeConfigPath(home string) string {
	return filepath.Join(home, ".config", "opencode", "opencode.json")
}

func loadOpenCodeConfig(path string) (map[string]any, error) {
	payload := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return payload, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return payload, nil
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		// Corrupt/foreign config: start fresh rather than fail the session.
		return map[string]any{}, nil
	}
	return payload, nil
}

func saveOpenCodeConfig(path string, payload map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create opencode config dir: %w", err)
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write opencode config: %w", err)
	}
	return nil
}

func writeOpenCodeRuntimeConfig(home string, llm *agentcomposev2.NodeLLMConfig) error {
	baseURL := strings.TrimRight(strings.TrimSpace(llm.GetEndpoint()), "/")
	if baseURL == "" {
		return nil
	}
	path := openCodeConfigPath(home)
	payload, err := loadOpenCodeConfig(path)
	if err != nil {
		return err
	}
	model := strings.TrimSpace(llm.GetModel())
	provider, _ := payload["provider"].(map[string]any)
	if provider == nil {
		provider = map[string]any{}
	}
	provider["agent-compose"] = map[string]any{
		"options": map[string]any{
			"baseURL": baseURL,
			"apiKey":  strings.TrimSpace(llm.GetApiKey()),
		},
	}
	payload["provider"] = provider
	if model != "" {
		payload["model"] = "agent-compose/" + model
	}
	return saveOpenCodeConfig(path, payload)
}

func writeOpenCodeMCPConfig(home string, mcps []*agentcomposev2.MCPServerSpec) error {
	path := openCodeConfigPath(home)
	payload, err := loadOpenCodeConfig(path)
	if err != nil {
		return err
	}
	mcpMap := map[string]any{}
	for _, mcp := range mcps {
		name := strings.TrimSpace(mcp.GetName())
		if name == "" {
			continue
		}
		if isRemoteMCP(mcp) {
			entry := map[string]any{"type": "remote", "url": strings.TrimSpace(mcp.GetUrl())}
			if hdrs := mcp.GetHeaders(); len(hdrs) > 0 {
				h := map[string]any{}
				for _, item := range hdrs {
					h[item.GetName()] = item.GetValue()
				}
				entry["headers"] = h
			}
			mcpMap[name] = entry
		} else {
			cmd := append([]string{strings.TrimSpace(mcp.GetCommand())}, mcp.GetArgs()...)
			entry := map[string]any{"type": "local", "command": cmd}
			if env := mcp.GetEnv(); len(env) > 0 {
				e := map[string]any{}
				for _, item := range env {
					e[item.GetName()] = item.GetValue()
				}
				entry["environment"] = e
			}
			mcpMap[name] = entry
		}
	}
	if len(mcpMap) == 0 {
		delete(payload, "mcp")
	} else {
		payload["mcp"] = mcpMap
	}
	return saveOpenCodeConfig(path, payload)
}

// ── claude (.mcp.json) ────────────────────────────────────────────────────────

// writeClaudeMCPConfig writes claude's MCP config to <home>/.mcp.json in the
// mcpServers shape claude-code reads. The LLM side for claude is env-based
// (llmEnv), so there is no separate claude LLM config file.
func writeClaudeMCPConfig(home string, mcps []*agentcomposev2.MCPServerSpec) error {
	path := filepath.Join(home, ".mcp.json")
	if len(mcps) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove claude mcp config: %w", err)
		}
		return nil
	}
	servers := map[string]any{}
	for _, mcp := range mcps {
		name := strings.TrimSpace(mcp.GetName())
		if name == "" {
			continue
		}
		if isRemoteMCP(mcp) {
			entry := map[string]any{"type": "http", "url": strings.TrimSpace(mcp.GetUrl())}
			if hdrs := mcp.GetHeaders(); len(hdrs) > 0 {
				h := map[string]any{}
				for _, item := range hdrs {
					h[item.GetName()] = item.GetValue()
				}
				entry["headers"] = h
			}
			servers[name] = entry
		} else {
			entry := map[string]any{"command": strings.TrimSpace(mcp.GetCommand())}
			if args := mcp.GetArgs(); len(args) > 0 {
				entry["args"] = args
			}
			if env := mcp.GetEnv(); len(env) > 0 {
				e := map[string]any{}
				for _, item := range env {
					e[item.GetName()] = item.GetValue()
				}
				entry["env"] = e
			}
			servers[name] = entry
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create claude config dir: %w", err)
	}
	data, err := json.MarshalIndent(map[string]any{"mcpServers": servers}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write claude mcp config: %w", err)
	}
	return nil
}

// ── skills / plugins sync ─────────────────────────────────────────────────────

// runtimeSkillsDir is the directory the provider runner looks in for skill
// metadata. Claude Code's SDK discovers skills at ~/.claude/skills/<name>/
// SKILL.md (loaded via the 'user' setting source), so claude sessions mirror
// there; the other providers' runners still read home/.agents/skills. Keep in
// sync with the runners in runtime/javascript/src.
func runtimeSkillsDir(home, provider string) string {
	if normalizeProvider(provider) == "claude" {
		return filepath.Join(home, ".claude", "skills")
	}
	return filepath.Join(home, ".agents", "skills")
}

// runtimePluginsDir is the scan point for local plugin packages the runtime
// hands to the provider. Claude's SDK takes them as {type:'local', path} from
// home/.agents/plugins; Gemini discovers extensions at ~/.gemini/extensions.
// A package laid down with the matching manifest is loaded wholesale by the
// provider, which is how multi-skill repos (e.g. obra/superpowers) reach the
// session without per-skill plumbing. Keep in sync with the runners in
// runtime/javascript/src.
func runtimePluginsDir(home, provider string) string {
	if normalizeProvider(provider) == "gemini" {
		return filepath.Join(home, ".gemini", "extensions")
	}
	return filepath.Join(home, ".agents", "plugins")
}

// ensureCodexMarketplaceManifest makes a package discoverable by
// `codex plugin marketplace add`. Codex requires `.agents/plugins/marketplace.json`
// at the package root; a self-marketed package (obra/superpowers) already has it,
// so this is a no-op. A bare `.codex-plugin/plugin.json` package gets a wrapper
// generated that points source.url at "./" so the CLI loads the whole root.
func ensureCodexMarketplaceManifest(pkgDir, name string) error {
	manifest := filepath.Join(pkgDir, ".agents", "plugins", "marketplace.json")
	if _, err := os.Stat(manifest); err == nil {
		return nil // package ships its own marketplace manifest
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat marketplace manifest: %w", err)
	}
	// Only wrap packages that declare themselves as a Codex plugin.
	pluginJSON := filepath.Join(pkgDir, ".codex-plugin", "plugin.json")
	if _, err := os.Stat(pluginJSON); err != nil {
		return nil // not a codex plugin package; skill/repo path handles it
	}
	wrapper := map[string]any{
		"name": name,
		"plugins": []map[string]any{
			{
				"name":   name,
				"source": map[string]any{"source": "url", "url": "./"},
				"policy": map[string]any{
					"installation":   "AVAILABLE",
					"authentication": "ON_INSTALL",
				},
			},
		},
	}
	data, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal marketplace wrapper: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		return fmt.Errorf("create marketplace dir: %w", err)
	}
	if err := os.WriteFile(manifest, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write marketplace wrapper: %w", err)
	}
	return nil
}

// syncSkills makes the session's skill set match the desired list exactly:
// each desired skill is materialized under both the node-managed skills dir and
// the runtime-read home/.agents/skills path, and any skill already on disk that
// is no longer desired is removed. An empty desired list clears every synced
// skill (it never touches files the node did not place there, because the dir is
// per-session and owned by the node).
func (m *sessionManager) syncSkills(session *nodeSession, skills []*agentcomposev2.SkillSpec) error {
	desired := map[string]bool{}
	for _, skill := range skills {
		name := strings.TrimSpace(skill.GetName())
		if name == "" {
			continue
		}
		desired[name] = true
		safe := sanitizeSessionDir(name)
		nodeDest := filepath.Join(session.skillsDir, safe)
		if err := m.fetchResource(session, skillSource(skill), nodeDest); err != nil {
			return fmt.Errorf("skill %s: %w", name, err)
		}
		// Mirror the fetched skill into the provider runner's discovery dir so a
		// skill the node pulled down actually reaches the CLI/SDK. Claude Code's
		// SDK discovers ~/.claude/skills/<name>/SKILL.md via the 'user' setting
		// source; the other providers' runners read home/.agents/skills.
		runtimeDest := filepath.Join(runtimeSkillsDir(session.home, session.provider), safe)
		if err := os.MkdirAll(filepath.Dir(runtimeDest), 0o755); err != nil {
			return fmt.Errorf("create runtime skills dir: %w", err)
		}
		if err := replaceTree(nodeDest, runtimeDest); err != nil {
			return fmt.Errorf("mirror skill %s to runtime dir: %w", name, err)
		}
	}
	// Garbage-collect skills that are no longer desired, in both locations.
	if err := pruneResourceDir(session.skillsDir, desired); err != nil {
		return fmt.Errorf("prune node skills dir: %w", err)
	}
	if err := pruneResourceDir(runtimeSkillsDir(session.home, session.provider), desired); err != nil {
		return fmt.Errorf("prune runtime skills dir: %w", err)
	}
	return nil
}

// syncPlugins makes the session's plugin set match the desired list exactly,
// mirroring syncSkills' semantics for the node-managed plugins dir, and mirrors
// each package into home/.agents/plugins so the runtime can hand it to the
// Claude Agent SDK as {type:'local', path}. The SDK loads a package wholesale
// (commands, agents, skills, hooks) from its .claude-plugin/plugin.json, which
// is what makes a multi-skill repo usable without per-skill plumbing.
func (m *sessionManager) syncPlugins(session *nodeSession, plugins []*agentcomposev2.NodePluginSpec) error {
	desired := map[string]bool{}
	for _, plugin := range plugins {
		name := strings.TrimSpace(plugin.GetName())
		if name == "" {
			continue
		}
		desired[name] = true
		safe := sanitizeSessionDir(name)
		dest := filepath.Join(session.pluginsDir, safe)
		if err := m.fetchResource(session, resourceSource{url: strings.TrimSpace(plugin.GetUrl())}, dest); err != nil {
			return fmt.Errorf("plugin %s: %w", name, err)
		}
		runtimeDest := filepath.Join(runtimePluginsDir(session.home, session.provider), safe)
		if err := os.MkdirAll(filepath.Dir(runtimeDest), 0o755); err != nil {
			return fmt.Errorf("create runtime plugins dir: %w", err)
		}
		if err := replaceTree(dest, runtimeDest); err != nil {
			return fmt.Errorf("mirror plugin %s to runtime dir: %w", name, err)
		}
		// Codex loads plugins through `codex plugin marketplace add <root>`, which
		// requires a `.agents/plugins/marketplace.json` at the root. A package
		// that ships only `.codex-plugin/plugin.json` (no marketplace manifest)
		// gets a wrapper generated here so the CLI can still discover it.
		if normalizeProvider(session.provider) == "codex" {
			if err := ensureCodexMarketplaceManifest(runtimeDest, name); err != nil {
				// Non-fatal: the plugin just won't register; don't block the session.
				fmt.Fprintf(os.Stderr, "[syncPlugins] codex marketplace manifest for %s: %v\n", name, err)
			}
		}
	}
	if err := pruneResourceDir(session.pluginsDir, desired); err != nil {
		return fmt.Errorf("prune node plugins dir: %w", err)
	}
	if err := pruneResourceDir(runtimePluginsDir(session.home, session.provider), desired); err != nil {
		return fmt.Errorf("prune runtime plugins dir: %w", err)
	}
	return nil
}

// pruneResourceDir removes every immediate child directory whose name (after
// sanitization) is not in desired. It only touches directories; stray files are
// left alone so a caller that writes a marker file is not surprised.
func pruneResourceDir(dir string, desired map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if desired[name] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove stale %s: %w", name, err)
		}
	}
	return nil
}

// replaceTree makes dest hold an exact copy of src: it removes dest if present,
// then copies src over. This is the full-replace counterpart to copyTree.
func replaceTree(src, dest string) error {
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return copyTree(src, dest)
}

// resourceSource is a normalized fetch spec for a skill or plugin.
type resourceSource struct {
	url      string
	gitRef   string
	path     string
	username string
	password string
	token    string
	// source 标识来源形态：github/git -> git clone；archive -> 从服务端拉 tar.gz。
	// 空串时按 url 兜底判断（以 .git 结尾或带 ref 视为 git，否则若 url 指向服务端
	// /api/v1/resources/.../fetch 也按 archive 处理）。
	source string
	// digest 是服务端在响应头 X-Resource-Digest 返回的 sha256，用作节点侧缓存键：
	// 同 url 内容变了 digest 变，缓存自动失效换新。git 形态不填。
	digest string
}

// isArchiveSource 判断是否走 archive 下载分支。优先看显式 source 标记，
// 否则按 url 是否指向我们的资源 fetch 端点兜底。
func (s resourceSource) isArchiveSource() bool {
	if s.source == "archive" {
		return true
	}
	if s.source == "github" || s.source == "git" {
		return false
	}
	// 兜底：url 含 /api/v1/resources/ 且 /fetch/ 的，按 archive 处理。
	return strings.Contains(s.url, "/api/v1/resources/") && strings.Contains(s.url, "/fetch/")
}

func skillSource(skill *agentcomposev2.SkillSpec) resourceSource {
	return resourceSource{
		url:      strings.TrimSpace(skill.GetUrl()),
		gitRef:   strings.TrimSpace(skill.GetRef()),
		path:     strings.TrimSpace(skill.GetPath()),
		username: strings.TrimSpace(skill.GetUsername()),
		password: strings.TrimSpace(skill.GetPassword()),
		token:    strings.TrimSpace(skill.GetToken()),
		source:   strings.TrimSpace(skill.GetSource()),
	}
}

// resourceCacheRoot returns the node-wide resource cache dir under workRoot.
// Cached archives are keyed by digest so content changes invalidate cleanly; the
// same skill reused across sessions on one node is downloaded only once.
func (m *sessionManager) resourceCacheRoot() string {
	return filepath.Join(m.opts.workRoot, ".resource-cache")
}

// fetchResource materializes a resource into dest. A local path is copied; an
// archive source (source=archive, or a URL hitting our /api/v1/resources/.../fetch
// endpoint) is pulled over HTTP and untarred; a git URL is cloned.
//
// Archive fetches consult the node-wide digest-keyed cache first: a hit means
// copyTree from cache to dest with no network. The cache is populated on miss.
//
// The destination is replaced on every sync so removed/renamed files inside an
// existing skill/plugin cannot survive a re-apply.
func (m *sessionManager) fetchResource(session *nodeSession, src resourceSource, dest string) error {
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if src.path != "" {
		return copyTree(src.path, dest)
	}
	if src.url == "" {
		return nil
	}
	if src.isArchiveSource() {
		return m.fetchArchive(session, src, dest)
	}
	// Clone via git into dest. `--recurse-submodules` so a resource repo that
	// keeps parts of itself as submodules lands complete instead of with empty
	// stub directories; `--shallow-submodules` keeps them at depth 1 like the
	// parent clone.
	args := []string{"clone", "--depth", "1", "--recurse-submodules", "--shallow-submodules"}
	if src.gitRef != "" {
		args = append(args, "--branch", src.gitRef)
	}
	args = append(args, applyResourceCredentials(src), dest)
	return runNodeGit(session.baseCtx, "", args...)
}

// fetchArchive pulls a server-hosted tar.gz into the digest-keyed cache, then
// copies it to the session dest. On a cache hit (same digest) no network is used.
//
// 缓存布局：cacheRoot/<key>/content/  放真正要 copy 到会话目录的解包结果；
//
//	cacheRoot/<key>/.ready    是标记文件，与 content 同级而不在内部，
//
// 因此 copyTree(content -> dest) 不会把 .ready 带进会话的 skill 目录。
// <key> 优先用 src.digest；未知时用 url 兜底键，下载后读取响应头 X-Resource-Digest
// 写回（见 downloadAndExtract 的返回），下次同 url 仍命中。
func (m *sessionManager) fetchArchive(session *nodeSession, src resourceSource, dest string) error {
	cacheKey := src.digest
	if cacheKey == "" {
		sum := sha256.Sum256([]byte(src.url))
		cacheKey = "url-" + hex.EncodeToString(sum[:])[:16]
	}
	cacheDir := filepath.Join(m.resourceCacheRoot(), cacheKey)
	contentDir := filepath.Join(cacheDir, "content")
	readyMarker := filepath.Join(cacheDir, ".ready")
	// 命中缓存：直接从 content 拷到 dest，零网络。
	if _, err := os.Stat(readyMarker); err == nil {
		return copyTree(contentDir, dest)
	}
	// 未命中：下载并解包到 contentDir。
	if err := os.MkdirAll(contentDir, 0o755); err != nil {
		return fmt.Errorf("create cache content dir: %w", err)
	}
	serverDigest, err := m.downloadAndExtract(session, src, contentDir)
	if err != nil {
		_ = os.RemoveAll(cacheDir) // 失败不留半成品，避免下次误命中
		return err
	}
	// 标记缓存就绪。服务端若回了真实 digest，记进 .ready 便于诊断（不参与判断）。
	marker := []byte("ok")
	if serverDigest != "" {
		marker = []byte(serverDigest)
	}
	if err := os.WriteFile(readyMarker, marker, 0o644); err != nil {
		return fmt.Errorf("mark cache ready: %w", err)
	}
	return copyTree(contentDir, dest)
}

// downloadAndExtract GETs the tar.gz from src.url (with Bearer token auth) and
// extracts it into dest. Returns the server-reported digest (X-Resource-Digest,
// sha256 hex) if present, else "". When both server digest and src.digest are
// known and they differ, that's a content/addr mismatch -> refuse and drop cache.
func (m *sessionManager) downloadAndExtract(session *nodeSession, src resourceSource, dest string) (string, error) {
	req, err := http.NewRequestWithContext(session.baseCtx, http.MethodGet, src.url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	// token 作为 Bearer；服务端 verify_fetch_token 同时接受 query ?token=，这里走头更干净。
	if src.token != "" {
		req.Header.Set("Authorization", "Bearer "+src.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", src.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", src.url, resp.StatusCode)
	}
	serverDigest := strings.TrimSpace(resp.Header.Get("X-Resource-Digest"))
	if serverDigest != "" && src.digest != "" && !strings.EqualFold(serverDigest, src.digest) {
		return serverDigest, fmt.Errorf("digest mismatch: server %s != expected %s", serverDigest, src.digest)
	}
	return serverDigest, extractTarGz(resp.Body, dest)
}

// extractTarGz extracts a .tar.gz stream into dest. Rejects absolute paths and
// traversal entries (../../etc/passwd) so an untrusted archive can't escape dest.
func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		// 安全：显式拒绝绝对路径。filepath.Join 会把 "/abs/x" 规范进 dest，
		// 静默改写归档声明的路径；宁可失败也不接受来源不合规的存档。
		name := filepath.ToSlash(hdr.Name)
		if strings.HasPrefix(name, "/") || filepath.IsAbs(hdr.Name) || strings.Contains(name, ":") {
			return fmt.Errorf("archive entry has absolute path (refused): %s", hdr.Name)
		}
		// 安全：清洗路径穿越。target 必须在 dest 之内。
		target := filepath.Join(dest, hdr.Name)
		if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("archive entry escapes dest: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			// 拒绝符号链接：跨会话的 skill 不需要，且符号链接是路径穿越的常见载体。
			return fmt.Errorf("archive contains symlink (refused): %s -> %s", hdr.Name, hdr.Linkname)
		}
	}
	return nil
}

func applyResourceCredentials(src resourceSource) string {
	url := src.url
	pass := src.password
	if pass == "" {
		pass = src.token
	}
	if src.username == "" && pass == "" {
		return url
	}
	// Best-effort basic-auth injection for http(s) URLs.
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(url, scheme) {
			rest := strings.TrimPrefix(url, scheme)
			cred := src.username
			if pass != "" {
				cred += ":" + pass
			}
			return scheme + cred + "@" + rest
		}
	}
	return url
}

// copyTree recursively copies src into dest.
func copyTree(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dest, info)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		s := filepath.Join(src, entry.Name())
		d := filepath.Join(dest, entry.Name())
		if entry.IsDir() {
			if err := copyTree(s, d); err != nil {
				return err
			}
		} else {
			fi, err := entry.Info()
			if err != nil {
				return err
			}
			if err := copyFile(s, d, fi); err != nil {
				return err
			}
		}
	}
	return nil
}

func copyFile(src, dest string, info os.FileInfo) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dest, data, info.Mode().Perm())
}

// ── shared helpers ────────────────────────────────────────────────────────────

func isRemoteMCP(mcp *agentcomposev2.MCPServerSpec) bool {
	if strings.EqualFold(strings.TrimSpace(mcp.GetType()), "local") {
		return false
	}
	if strings.TrimSpace(mcp.GetCommand()) != "" {
		return false
	}
	return strings.TrimSpace(mcp.GetUrl()) != ""
}

func normalizeWireAPI(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "chat", "chat_completions", "chat-completions":
		return "chat"
	case "responses", "":
		return "responses"
	default:
		return strings.ToLower(strings.TrimSpace(protocol))
	}
}

// replaceManagedTextBlock replaces the text between start and end markers
// (inclusive) with block. When the markers are absent, block is appended. When
// block is empty, the managed section is removed.
func replaceManagedTextBlock(existing, start, end, block string) string {
	s := strings.Index(existing, start)
	e := strings.Index(existing, end)
	if s >= 0 && e > s {
		before := existing[:s]
		after := existing[e+len(end):]
		if block == "" {
			return strings.TrimRight(before, "\n") + strings.TrimLeft(after, "\n")
		}
		return before + block + after
	}
	if block == "" {
		return existing
	}
	if strings.TrimSpace(existing) == "" {
		return block
	}
	return strings.TrimRight(existing, "\n") + "\n\n" + block + "\n"
}

// extractManagedTextBlock returns the managed block (inclusive of markers) from
// existing, or "" if absent.
func extractManagedTextBlock(existing, start, end string) string {
	s := strings.Index(existing, start)
	e := strings.Index(existing, end)
	if s >= 0 && e > s {
		return existing[s : e+len(end)]
	}
	return ""
}
