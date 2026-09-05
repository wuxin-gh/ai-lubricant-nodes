package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// envMode values the server sends on NodeCreateSession to pick which HOME the
// editor runs against. Empty means isolated (the historic default), so a caller
// that never sets env_mode lands in a throwaway per-session home unchanged.
const (
	envModeIsolated = "isolated"
	envModeSystem   = "system"
	envModeShared   = "shared"
)

// envDir is the on-node path a named shared environment lives at. The server
// stores only the env id; the node resolves it to a stable directory under its
// work root (sibling of the per-task and per-session trees). Multiple sessions
// that pick the same env_id share this one HOME — that is the point of the
// mode, not a defect to lock against.
func (m *sessionManager) envDir(envID string) string {
	return filepath.Join(m.opts.workRoot, "envs", sanitizeSessionDir(envID))
}

// resolveHome picks the HOME the editor runs against for a session, per its
// env_mode:
//   - isolated (default): a throwaway per-session dir under the runtime tree.
//   - shared: a named persistent dir reused across sessions that pick the same
//     env_id; created on first use so a retried dispatch never wipes a live env.
//   - system: the node operator's real home, so the editor reuses installed
//     tooling and CLI login state. Refused unless the node opted in via flag —
//     it hands the whole home (ssh keys, CLI creds) to the agent.
//
// base is the per-session runtime tree (runtimeDir); only the isolated mode
// nests home under it.
func (m *sessionManager) resolveHome(spec *agentcomposev2.NodeCreateSession, base string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(spec.GetEnvMode()))
	switch mode {
	case "", envModeIsolated:
		return filepath.Join(base, "home"), nil
	case envModeSystem:
		if !m.opts.systemEnvAllowed {
			return "", fmt.Errorf("env_mode=%q is not enabled on this node (auto: host installs allow it, containers refuse it; override with AGENT_COMPOSE_NODE_ALLOW_SYSTEM_ENV=on|off)", mode)
		}
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("env_mode=%q requires a resolvable user home: %w", mode, err)
		}
		if strings.TrimSpace(h) == "" {
			return "", fmt.Errorf("env_mode=%q requires a non-empty user home", mode)
		}
		return h, nil
	case envModeShared:
		envID := strings.TrimSpace(spec.GetEnvId())
		if envID == "" {
			return "", fmt.Errorf("env_mode=%q requires env_id", mode)
		}
		dir := m.envDir(envID)
		// Idempotent: an existing environment is left untouched. A retried
		// dispatch (or two sessions racing on the same env) must not wipe what
		// a previous task installed.
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("env_mode=%q prepare %s: %w", mode, dir, err)
		}
		return dir, nil
	default:
		return "", fmt.Errorf("unsupported env_mode %q (want %q|%q|%q)", mode, envModeIsolated, envModeShared, envModeSystem)
	}
}

// isSystemEnv reports whether this session runs against the node operator's real
// home. In that mode the node must NOT rewrite editor config into that home
// (mcp/skills/plugins/llm disk files) — the operator owns ~/.claude, ~/.codex,
// ~/.agents, and an exact-set rewrite would clobber their own setup. The LLM
// still reaches the editor via buildEnv's env vars, so system mode is usable
// without any on-disk config; it just cannot apply task-selected skills/MCPs.
func isSystemEnv(spec *agentcomposev2.NodeCreateSession) bool {
	return strings.EqualFold(strings.TrimSpace(spec.GetEnvMode()), envModeSystem)
}

// isSharedEnv reports whether this session runs against a named shared
// environment. Resource *installation* in that mode belongs to the environment
// (NodeSyncEnvironment), not to the session: a task must never rewrite the
// shared HOME's skill/plugin dirs, because removing a skill from one task would
// otherwise uninstall it for every other task using the same environment.
// The session only selects which installed resources to activate.
func isSharedEnv(spec *agentcomposev2.NodeCreateSession) bool {
	return strings.EqualFold(strings.TrimSpace(spec.GetEnvMode()), envModeShared)
}

// ownsHomeResources reports whether a session may write the skill/plugin trees
// under its HOME. Only the isolated tier does: its HOME is a throwaway dir the
// node created for this session alone. system and shared both share a HOME with
// something outside the session's lifetime (the operator, or other tasks), so
// their resource sets are managed elsewhere and left alone here.
func ownsHomeResources(spec *agentcomposev2.NodeCreateSession) bool {
	return !isSystemEnv(spec) && !isSharedEnv(spec)
}

// activeSkillNames is the subset of the environment's installed skills this
// session turns on. An explicit list wins; empty means "activate everything the
// environment has", which is resolved by listing the env's installed skills so a
// task that selected nothing still gets the environment as configured.
func (m *sessionManager) activeSkillNames(spec *agentcomposev2.NodeCreateSession) []string {
	if names := spec.GetActiveSkills(); len(names) > 0 {
		return names
	}
	if !isSharedEnv(spec) {
		return nil
	}
	inventory, err := m.inspectEnvironment(&agentcomposev2.NodeInspectEnvironment{EnvId: spec.GetEnvId()})
	if err != nil {
		m.logger.Warn("environment skill enumeration failed",
			"env_id", spec.GetEnvId(), "error", err)
		return nil
	}
	var names []string
	for _, entry := range inventory {
		if entry.GetKind() == "skill" {
			names = append(names, entry.GetName())
		}
	}
	return names
}

// manageEnvironment provisions or removes a named shared environment on this
// host, dispatched via a NodeManageEnvironment downstream frame. CREATE is
// idempotent (an existing dir is left as-is); REMOVE deletes the whole tree
// so disk is reclaimed when an environment is retired server-side.
func (m *sessionManager) manageEnvironment(ctx context.Context, frame *agentcomposev2.NodeManageEnvironment) error {
	envID := strings.TrimSpace(frame.GetEnvId())
	if envID == "" {
		return fmt.Errorf("manage environment: env_id is required")
	}
	dir := m.envDir(envID)
	switch frame.GetAction() {
	case agentcomposev2.EnvironmentAction_ENVIRONMENT_ACTION_CREATE:
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create environment %s: %w", envID, err)
		}
		m.logger.Info("environment ensured", "env_id", envID, "dir", dir)
		return nil
	case agentcomposev2.EnvironmentAction_ENVIRONMENT_ACTION_REMOVE:
		// Best-effort removal: a missing dir is success (already gone). Refusing
		// to remove a non-existent env would make the server-side delete flow
		// non-idempotent after a node restart that lost the dir.
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove environment %s: %w", envID, err)
		}
		m.logger.Info("environment removed", "env_id", envID, "dir", dir)
		return nil
	default:
		return fmt.Errorf("manage environment %s: unsupported action %v", envID, frame.GetAction())
	}
}

// envSyncSession fabricates the minimal nodeSession fetchResource needs so
// environment maintenance can reuse the exact same download/extract/cache path
// as a session's own resource sync. There is no real session here: an
// environment install is a node-level operation with no task behind it, so this
// carries only a context and the env's dirs.
//
// provider is deliberately empty: the environment's on-disk layout must be
// provider-neutral (a shared env can be used by claude AND codex tasks), so
// resources land in the provider-agnostic `.agents` tree that every runner
// reads, never in a provider-specific dir like `.claude/skills`.
func (m *sessionManager) envSyncSession(ctx context.Context, envID string) *nodeSession {
	home := m.envDir(envID)
	return &nodeSession{
		id:         "env-" + sanitizeSessionDir(envID),
		home:       home,
		workDir:    home,
		baseCtx:    ctx,
		skillsDir:  filepath.Join(home, ".agent-compose", "skills"),
		pluginsDir: filepath.Join(home, ".agent-compose", "plugins"),
	}
}

// envResourceDir is where an environment keeps a given resource kind. This is
// the provider-neutral runtime discovery path (`<home>/.agents/<kind>`) that the
// runtime's own skill catalog and the non-claude runners read directly.
func envResourceDir(home, kind string) string {
	return filepath.Join(home, ".agents", kind)
}

// syncEnvironment makes an environment's installed skill/plugin set match the
// desired list exactly. This is the real install/uninstall — the counterpart to
// a task merely *activating* a subset, which never touches these files.
//
// Exact-set semantics apply only to the managed dirs the node itself writes
// (`.agents/skills`, `.agents/plugins`). Anything an operator installed by hand
// elsewhere in the environment HOME is untouched: it shows up in the inventory
// as an extra rather than being silently deleted.
func (m *sessionManager) syncEnvironment(ctx context.Context, frame *agentcomposev2.NodeSyncEnvironment) error {
	envID := strings.TrimSpace(frame.GetEnvId())
	if envID == "" {
		return fmt.Errorf("sync environment: env_id is required")
	}
	home := m.envDir(envID)
	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("sync environment %s: prepare dir: %w", envID, err)
	}
	session := m.envSyncSession(ctx, envID)

	skillsRoot := envResourceDir(home, "skills")
	desiredSkills := map[string]bool{}
	for _, skill := range frame.GetSkills() {
		name := strings.TrimSpace(skill.GetName())
		if name == "" {
			continue
		}
		safe := sanitizeSessionDir(name)
		desiredSkills[safe] = true
		if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
			return fmt.Errorf("sync environment %s: prepare skills dir: %w", envID, err)
		}
		if err := m.fetchResource(session, skillSource(skill), filepath.Join(skillsRoot, safe)); err != nil {
			return fmt.Errorf("sync environment %s: skill %s: %w", envID, name, err)
		}
	}
	if err := pruneResourceDir(skillsRoot, desiredSkills); err != nil {
		return fmt.Errorf("sync environment %s: prune skills: %w", envID, err)
	}

	pluginsRoot := envResourceDir(home, "plugins")
	desiredPlugins := map[string]bool{}
	for _, plugin := range frame.GetPlugins() {
		name := strings.TrimSpace(plugin.GetName())
		if name == "" {
			continue
		}
		safe := sanitizeSessionDir(name)
		desiredPlugins[safe] = true
		if err := os.MkdirAll(pluginsRoot, 0o755); err != nil {
			return fmt.Errorf("sync environment %s: prepare plugins dir: %w", envID, err)
		}
		dest := filepath.Join(pluginsRoot, safe)
		src := resourceSource{url: strings.TrimSpace(plugin.GetUrl())}
		if err := m.fetchResource(session, src, dest); err != nil {
			return fmt.Errorf("sync environment %s: plugin %s: %w", envID, name, err)
		}
	}
	if err := pruneResourceDir(pluginsRoot, desiredPlugins); err != nil {
		return fmt.Errorf("sync environment %s: prune plugins: %w", envID, err)
	}

	m.logger.Info("environment synced",
		"env_id", envID, "skills", len(desiredSkills), "plugins", len(desiredPlugins))
	return nil
}

// inspectEnvironment reports what is physically installed in an environment
// HOME. It lists the managed `.agents` dirs, so it sees both what a sync put
// there AND what an operator dropped in by hand from a maintenance shell — the
// server diffs this against the configured set to show 已安装/待安装/多余.
//
// A missing environment dir is not an error: it reports an empty inventory, so
// an environment that exists only in the ledger (created while the node was
// offline) reads as "nothing installed yet" rather than failing the call.
func (m *sessionManager) inspectEnvironment(frame *agentcomposev2.NodeInspectEnvironment) ([]*agentcomposev2.NodeEnvironmentEntry, error) {
	envID := strings.TrimSpace(frame.GetEnvId())
	if envID == "" {
		return nil, fmt.Errorf("inspect environment: env_id is required")
	}
	home := m.envDir(envID)
	var out []*agentcomposev2.NodeEnvironmentEntry
	for _, pair := range []struct{ kind, dir string }{
		{"skill", envResourceDir(home, "skills")},
		{"plugin", envResourceDir(home, "plugins")},
	} {
		entries, err := os.ReadDir(pair.dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("inspect environment %s: read %s: %w", envID, pair.dir, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			out = append(out, &agentcomposev2.NodeEnvironmentEntry{
				Kind:    pair.kind,
				Name:    entry.Name(),
				Version: readResourceVersion(filepath.Join(pair.dir, entry.Name())),
			})
		}
	}
	return out, nil
}

// readResourceVersion is a best-effort version probe for an installed resource.
// Skills carry no version (SKILL.md frontmatter has name/description only), and
// plugin packages vary by provider, so an unknown version is normal and reported
// as empty rather than guessed.
func readResourceVersion(dir string) string {
	for _, candidate := range []string{
		filepath.Join(dir, "package.json"),
		filepath.Join(dir, ".claude-plugin", "plugin.json"),
		filepath.Join(dir, ".codex-plugin", "plugin.json"),
	} {
		raw, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		var doc struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(raw, &doc) == nil {
			if v := strings.TrimSpace(doc.Version); v != "" {
				return v
			}
		}
	}
	return ""
}

