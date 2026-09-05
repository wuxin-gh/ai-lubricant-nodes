package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// noopEmit satisfies emitFunc for an isolated session manager under test.
func noopEmit(*agentcomposev2.NodeUpstreamFrame) error { return nil }

func newTestManager(t *testing.T) (*sessionManager, string) {
	t.Helper()
	workRoot := t.TempDir()
	m := newSessionManager(
		sessionOptions{workRoot: workRoot, providers: []string{"claude", "codex", "opencode"}},
		testLogger(),
		noopEmit, noopEmit, noopEmit, noopEmit,
	)
	return m, workRoot
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: (slog.Level)(127)}))
}

// twoClaudeSessions provisions two sessions under one editor workdir (the shared
// editor-workdir shape the platform uses), each with its own MCP list. It does
// not start the editor (defer_start), so no agent-compose-runtime binary is needed.
func twoClaudeSessions(t *testing.T, m *sessionManager) (sessA, sessB *nodeSession) {
	t.Helper()
	specA := &agentcomposev2.NodeCreateSession{
		SessionId:  "sess-A",
		EditorId:   "ed_test",
		Provider:   "claude",
		DeferStart: true,
		Tags:       map[string]string{"editor_workdir": "editors/ed_test"},
		Mcps: []*agentcomposev2.MCPServerSpec{
			{Name: "issue-workflow", Type: "sse", Url: "https://gw/mcp/issue-workflow/sse?token=TOKEN-A"},
			{Name: "shared-x", Type: "sse", Url: "https://gw/x/sse"},
		},
	}
	specB := &agentcomposev2.NodeCreateSession{
		SessionId:  "sess-B",
		EditorId:   "ed_test",
		Provider:   "claude",
		DeferStart: true,
		Tags:       map[string]string{"editor_workdir": "editors/ed_test"},
		Mcps: []*agentcomposev2.MCPServerSpec{
			{Name: "issue-workflow", Type: "sse", Url: "https://gw/mcp/issue-workflow/sse?token=TOKEN-B"},
		},
	}
	if err := m.create(context.Background(), specA); err != nil {
		t.Fatalf("create sess-A: %v", err)
	}
	if err := m.create(context.Background(), specB); err != nil {
		t.Fatalf("create sess-B: %v", err)
	}
	// Wait for async provisioning to complete (create() now returns immediately
	// and runs provisioning in a background goroutine).
	waitProvisioned(t, m, "sess-A")
	waitProvisioned(t, m, "sess-B")
	a, _ := m.lookupSession("sess-A")
	b, _ := m.lookupSession("sess-B")
	return a, b
}

// waitProvisioned polls until the session transitions out of provisioning state.
func waitProvisioned(t *testing.T, m *sessionManager, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		sess, ok := m.sessions[sessionID]
		m.mu.Unlock()
		if !ok {
			t.Fatalf("session %s not found", sessionID)
		}
		sess.mu.Lock()
		state := sess.state
		sess.mu.Unlock()
		if state != sessionProvisioning {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session %s still provisioning after 5s", sessionID)
}

func readMcpServers(t *testing.T, home string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".mcp.json"))
	if err != nil {
		t.Fatalf("read .mcp.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse .mcp.json: %v", err)
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	return servers
}

func urlOf(servers map[string]any, name string) string {
	entry, ok := servers[name].(map[string]any)
	if !ok {
		return ""
	}
	return entry["url"].(string)
}

func readRuntimeMcpServers(t *testing.T, stateRoot string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateRoot, "agents", "mcp", "config.json"))
	if err != nil {
		t.Fatalf("read runtime MCP config: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse runtime MCP config: %v", err)
	}
	servers, _ := doc["mcps"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	return servers
}

func runtimeURLOf(servers map[string]any, name string) string {
	entry, ok := servers[name].(map[string]any)
	if !ok {
		return ""
	}
	return entry["url"].(string)
}

// TestMCPConfigIsPerSessionHome confirms the on-disk MCP config and its token
// land in each session's own home dir, not a shared location.
func TestMCPConfigIsPerSessionHome(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, sessB := twoClaudeSessions(t, m)

	serversA := readMcpServers(t, sessA.home)
	serversB := readMcpServers(t, sessB.home)

	if got := urlOf(serversA, "issue-workflow"); got != "https://gw/mcp/issue-workflow/sse?token=TOKEN-A" {
		t.Fatalf("sess-A issue-workflow url = %q, want TOKEN-A", got)
	}
	if got := urlOf(serversB, "issue-workflow"); got != "https://gw/mcp/issue-workflow/sse?token=TOKEN-B" {
		t.Fatalf("sess-B issue-workflow url = %q, want TOKEN-B", got)
	}

	// Home dirs are distinct files.
	if sessA.home == sessB.home {
		t.Fatalf("sessions share the same home: %s", sessA.home)
	}
	// sess-A picked up shared-x, sess-B did not — confirming they are independent copies.
	if _, ok := serversB["shared-x"]; ok {
		t.Fatalf("sess-B must not contain shared-x")
	}
	if _, ok := serversA["shared-x"]; !ok {
		t.Fatalf("sess-A must contain shared-x")
	}

	// The runtime-read input carries the same per-session tokens. This is the
	// authoritative channel the JS runtime actually consumes.
	runtimeA := readRuntimeMcpServers(t, sessA.stateRoot)
	runtimeB := readRuntimeMcpServers(t, sessB.stateRoot)
	if got := runtimeURLOf(runtimeA, "issue-workflow"); got != "https://gw/mcp/issue-workflow/sse?token=TOKEN-A" {
		t.Fatalf("runtime sess-A issue-workflow url = %q, want TOKEN-A", got)
	}
	if got := runtimeURLOf(runtimeB, "issue-workflow"); got != "https://gw/mcp/issue-workflow/sse?token=TOKEN-B" {
		t.Fatalf("runtime sess-B issue-workflow url = %q, want TOKEN-B", got)
	}
	if _, ok := runtimeB["shared-x"]; ok {
		t.Fatalf("runtime sess-B must not contain shared-x")
	}
	if _, ok := runtimeA["shared-x"]; !ok {
		t.Fatalf("runtime sess-A must contain shared-x")
	}
}

// TestApplyMCPsRewritesExactly confirms applyMCPs is an exact-set rewrite:
// removing a server from the desired list must delete it from disk. (This is
// the property the server-side resync relies on.)
func TestApplyMCPsRewritesExactly(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, _ := twoClaudeSessions(t, m)

	// Editor resync sends only the base editor mcp_config, WITHOUT the session's
	// per-issue-workflow entry — the bug scenario.
	if _, err := m.applyMCPs("sess-A", 2, []*agentcomposev2.MCPServerSpec{
		{Name: "shared-x", Type: "sse", Url: "https://gw/x/sse"},
	}); err != nil {
		t.Fatalf("applyMCPs: %v", err)
	}
	servers := readMcpServers(t, sessA.home)
	if _, ok := servers["issue-workflow"]; ok {
		t.Errorf("issue-workflow survived editor resync (token leak risk)")
	}
	if urlOf(servers, "shared-x") != "https://gw/x/sse" {
		t.Errorf("shared-x not in rewritten config")
	}
	_ = sessA
}

// TestSyncSkillsRemovesOrphans confirms skill sync is a full desired-set
// replacement: removed skills disappear and an empty desired list clears the
// session skill dirs.
func TestSyncSkillsRemovesOrphans(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, _ := twoClaudeSessions(t, m)

	// Seed two skills via local-path copy by pointing SkillSpec.Path at temp dirs.
	sk1 := t.TempDir()
	os.WriteFile(filepath.Join(sk1, "SKILL.md"), []byte("skill1"), 0o644)
	sk2 := t.TempDir()
	os.WriteFile(filepath.Join(sk2, "SKILL.md"), []byte("skill2"), 0o644)

	if err := m.syncSkills(sessA, []*agentcomposev2.SkillSpec{
		{Name: "sk1", Path: sk1},
		{Name: "sk2", Path: sk2},
	}); err != nil {
		t.Fatalf("syncSkills first: %v", err)
	}
	// Now "remove" sk1 by syncing only sk2.
	if err := m.syncSkills(sessA, []*agentcomposev2.SkillSpec{
		{Name: "sk2", Path: sk2},
	}); err != nil {
		t.Fatalf("syncSkills second: %v", err)
	}
	// sk1 must be removed from both node-managed and runtime-read locations.
	if _, err := os.Stat(filepath.Join(sessA.skillsDir, "sk1", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("sk1 should have been removed from node skills dir, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeSkillsDir(sessA.home, sessA.provider), "sk1", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("sk1 should have been removed from runtime skills dir, stat err=%v", err)
	}
	if err := m.syncSkills(sessA, nil); err != nil {
		t.Fatalf("syncSkills empty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessA.skillsDir, "sk2", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("sk2 should have been removed after empty sync, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeSkillsDir(sessA.home, sessA.provider), "sk2", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("runtime sk2 should have been removed after empty sync, stat err=%v", err)
	}
}

// TestSkillsArePerSessionDir confirms skill files land in each session's own
// skills dir, never the other session's.
func TestSkillsArePerSessionDir(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, sessB := twoClaudeSessions(t, m)

	skA := t.TempDir()
	os.WriteFile(filepath.Join(skA, "SKILL.md"), []byte("only-for-A"), 0o644)

	if err := m.syncSkills(sessA, []*agentcomposev2.SkillSpec{{Name: "skA", Path: skA}}); err != nil {
		t.Fatalf("syncSkills A: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessA.skillsDir, "skA")); err != nil {
		t.Fatalf("sess-A missing skA in node skills dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runtimeSkillsDir(sessA.home, sessA.provider), "skA")); err != nil {
		t.Fatalf("sess-A missing skA in runtime skills dir: %v", err)
	}
	// sess-B must not see skA in either of its skill dirs.
	if _, err := os.Stat(filepath.Join(sessB.skillsDir, "skA")); !os.IsNotExist(err) {
		t.Fatalf("sess-B leaked skA into its node skills dir")
	}
	if _, err := os.Stat(filepath.Join(runtimeSkillsDir(sessB.home, sessB.provider), "skA")); !os.IsNotExist(err) {
		t.Fatalf("sess-B leaked skA into its runtime skills dir")
	}
	if sessA.skillsDir == sessB.skillsDir || runtimeSkillsDir(sessA.home, sessA.provider) == runtimeSkillsDir(sessB.home, sessB.provider) {
		t.Fatalf("skills dirs collide")
	}
}

// TestPluginDirsArePerSession confirms plugin files land in each session's own
// plugins dir. We exercise the local-path copy branch (Path is not on NodePluginSpec,
// so we mirror by writing a file the way fetchResource would for a path source via
// a manual copy into the session plugins dir, then assert isolation).
func TestPluginDirsArePerSession(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, sessB := twoClaudeSessions(t, m)

	if sessA.pluginsDir == sessB.pluginsDir {
		t.Fatalf("plugin dirs collide: %s", sessA.pluginsDir)
	}
	// Simulate a synced plugin file in A.
	dest := filepath.Join(sessA.pluginsDir, "plg-A")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	os.WriteFile(filepath.Join(dest, "plugin.toml"), []byte("p"), 0o644)
	// B must not see it.
	if _, err := os.Stat(filepath.Join(sessB.pluginsDir, "plg-A")); !os.IsNotExist(err) {
		t.Fatalf("sess-B leaked plg-A into its plugins dir")
	}
}

// TestWorkspaceSharedButRuntimeIsolated confirms the one thing that IS shared:
// the editor workdir (source tree). Two sessions of one editor share workDir.
func TestWorkspaceSharedButRuntimeIsolated(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, sessB := twoClaudeSessions(t, m)

	if sessA.workDir != sessB.workDir {
		t.Fatalf("sessions of one editor must share workDir; got A=%s B=%s", sessA.workDir, sessB.workDir)
	}
	if sessA.home == sessB.home || sessA.skillsDir == sessB.skillsDir || sessA.pluginsDir == sessB.pluginsDir {
		t.Fatalf("runtime dirs must be distinct across sessions")
	}
}

// TestNodeOutputPopulatesRuntimeMCPInput checks the actual node→runtime
// contract. agent-compose-runtime reads <stateRoot>/agents/mcp/config.json, so
// the node must write that file in addition to any provider-native config.
func TestNodeOutputPopulatesRuntimeMCPInput(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, _ := twoClaudeSessions(t, m)

	// Provider-native compatibility file still exists.
	if _, err := os.Stat(filepath.Join(sessA.home, ".mcp.json")); err != nil {
		t.Fatalf("node native MCP config missing: %v", err)
	}
	// Runtime's real input path exists and contains the same session token.
	runtimeInput := filepath.Join(sessA.stateRoot, "agents", "mcp", "config.json")
	if _, err := os.Stat(runtimeInput); err != nil {
		t.Fatalf("runtime MCP input missing: %v", err)
	}
	servers := readRuntimeMcpServers(t, sessA.stateRoot)
	if got := runtimeURLOf(servers, "issue-workflow"); got != "https://gw/mcp/issue-workflow/sse?token=TOKEN-A" {
		t.Fatalf("runtime issue-workflow url = %q, want TOKEN-A", got)
	}
}

// TestNodeSkillOutputMatchesRuntimeSkillPath checks the real skill path
// contract. The runtime runners discover skills at <home>/.agents/skills, so
// syncSkills mirrors node-managed skills there.
func TestNodeSkillOutputMatchesRuntimeSkillPath(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, _ := twoClaudeSessions(t, m)

	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte("---\nname: demo\ndescription: demo\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.syncSkills(sessA, []*agentcomposev2.SkillSpec{{Name: "demo", Path: source}}); err != nil {
		t.Fatalf("syncSkills: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessA.skillsDir, "demo", "SKILL.md")); err != nil {
		t.Fatalf("node skill output missing: %v", err)
	}
	runtimeSkill := filepath.Join(runtimeSkillsDir(sessA.home, sessA.provider), "demo", "SKILL.md")
	if _, err := os.Stat(runtimeSkill); err != nil {
		t.Fatalf("runtime skill path missing: %v", err)
	}
}

// TestProviderPromptArgsCarriesSessionHome confirms the editor CLI is launched
// with the per-session home, not a shared one (the linchpin of MCP isolation).
func TestProviderPromptArgsCarriesSessionHome(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, _ := twoClaudeSessions(t, m)

	args := promptArgs(sessA.provider, "model-x", "", sessA.stateRoot, sessA.workDir, sessA.home, nil)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--home "+sessA.home) {
		t.Fatalf("prompt args do not pass the session's home: %s", joined)
	}
	if !strings.Contains(joined, "--state-root "+sessA.stateRoot) {
		t.Fatalf("prompt args do not pass the session's stateRoot: %s", joined)
	}
	if !strings.Contains(joined, "--workspace "+sessA.workDir) {
		t.Fatalf("prompt args do not pass the session's workDir: %s", joined)
	}
}

// TestBuildEnvPinsSessionHome confirms buildEnv sets HOME/USERPROFILE to the
// session home so provider CLIs (codex) read the session's own config files.
func TestBuildEnvPinsSessionHome(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, _ := twoClaudeSessions(t, m)

	env := m.buildEnv(sessA)
	want := map[string]string{
		"HOME":        sessA.home,
		"USERPROFILE": sessA.home,
		"WORKSPACE":   sessA.workDir,
	}
	got := map[string]string{}
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			got[parts[0]] = parts[1]
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("buildEnv %s = %q, want %q", k, got[k], v)
		}
	}
}

// envSpec builds a CreateSession with a given env_mode/env_id for the
// environment-tier tests below. DeferStart keeps it from starting the editor.
func envSpec(sessionID, mode, envID string) *agentcomposev2.NodeCreateSession {
	return &agentcomposev2.NodeCreateSession{
		SessionId:  sessionID,
		Provider:   "claude",
		DeferStart: true,
		EnvMode:    mode,
		EnvId:      envID,
	}
}

// TestResolveHomeIsolatedPinsPerSessionHome confirms the default (empty/isolated)
// mode still nests HOME under the per-session runtime tree, so the historic
// isolation TestBuildEnvPinsSessionHome relies on is unchanged.
func TestResolveHomeIsolatedPinsPerSessionHome(t *testing.T) {
	m, _ := newTestManager(t)
	spec := envSpec("env-iso", "", "")
	base := filepath.Join(t.TempDir(), "runtime", "base")
	home, err := m.resolveHome(spec, base)
	if err != nil {
		t.Fatalf("resolveHome: %v", err)
	}
	if want := filepath.Join(base, "home"); home != want {
		t.Fatalf("isolated home = %q, want %q", home, want)
	}
}

// TestResolveHomeSharedPointsAtEnvDir confirms the shared mode points HOME at a
// stable, id-derived directory under the work root — and that two sessions on
// the same env_id share it (the whole point of the mode).
func TestResolveHomeSharedPointsAtEnvDir(t *testing.T) {
	m, workRoot := newTestManager(t)
	spec := envSpec("env-sh", "shared", "prod-keys")
	home, err := m.resolveHome(spec, "/runtime/base")
	if err != nil {
		t.Fatalf("resolveHome: %v", err)
	}
	want := filepath.Join(workRoot, "envs", "prod-keys")
	if home != want {
		t.Fatalf("shared home = %q, want %q", home, want)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("shared env dir not created: %v", err)
	}
	// A second session on the same env_id lands on the same dir — shared by design.
	home2, err := m.resolveHome(envSpec("env-sh2", "shared", "prod-keys"), "/runtime/other")
	if err != nil {
		t.Fatalf("resolveHome 2: %v", err)
	}
	if home2 != home {
		t.Fatalf("second session on same env_id got %q, want shared %q", home2, home)
	}
}

// TestResolveHomeSharedRequiresEnvID confirms shared mode with no env_id is
// rejected up front rather than silently landing somewhere.
func TestResolveHomeSharedRequiresEnvID(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.resolveHome(envSpec("env-sh-noid", "shared", ""), "/runtime/base"); err == nil {
		t.Fatalf("shared with empty env_id must be rejected")
	}
}

// TestResolveHomeSystemRefusedUnlessAllowed confirms the node refuses
// env_mode=system by default (it hands the operator's whole home to the
// agent), and allows it only when the node opted in via the flag.
func TestResolveHomeSystemRefusedUnlessAllowed(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.resolveHome(envSpec("env-sys", "system", ""), "/runtime/base"); err == nil {
		t.Fatalf("system mode must be refused when not allowed")
	}
	// Opt the node in by flipping the option the flag sets.
	m.opts.systemEnvAllowed = true
	home, err := m.resolveHome(envSpec("env-sys2", "system", ""), "/runtime/base")
	if err != nil {
		t.Fatalf("resolveHome system allowed: %v", err)
	}
	if want, _ := os.UserHomeDir(); home != want {
		t.Fatalf("system home = %q, want %q", home, want)
	}
}

// TestResolveHomeRejectsUnknownMode confirms an unsupported env_mode fails
// provisioning clearly rather than falling through to a default.
func TestResolveHomeRejectsUnknownMode(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.resolveHome(envSpec("env-bad", "kubernetes", ""), "/runtime/base"); err == nil {
		t.Fatalf("unknown env_mode must be rejected")
	}
}

// TestManageEnvironmentCreateIdempotent confirms a retried CREATE leaves an
// existing environment's contents intact (a wipe would be data loss).
func TestManageEnvironmentCreateIdempotent(t *testing.T) {
	m, workRoot := newTestManager(t)
	spec := &agentcomposev2.NodeManageEnvironment{
		EnvId:  "prod",
		Action: agentcomposev2.EnvironmentAction_ENVIRONMENT_ACTION_CREATE,
	}
	if err := m.manageEnvironment(context.Background(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	dir := filepath.Join(workRoot, "envs", "prod")
	marker := filepath.Join(dir, "installed.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Retry create: must not wipe the marker.
	if err := m.manageEnvironment(context.Background(), spec); err != nil {
		t.Fatalf("create retry: %v", err)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "keep" {
		t.Fatalf("idempotent create wiped env: got=%q err=%v", got, err)
	}
}

// TestManageEnvironmentRemoveReclaimsDisk confirms REMOVE deletes the env tree.
func TestManageEnvironmentRemoveReclaimsDisk(t *testing.T) {
	m, workRoot := newTestManager(t)
	if err := m.manageEnvironment(context.Background(), &agentcomposev2.NodeManageEnvironment{
		EnvId:  "gone",
		Action: agentcomposev2.EnvironmentAction_ENVIRONMENT_ACTION_CREATE,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	dir := filepath.Join(workRoot, "envs", "gone")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("env dir missing after create: %v", err)
	}
	if err := m.manageEnvironment(context.Background(), &agentcomposev2.NodeManageEnvironment{
		EnvId:  "gone",
		Action: agentcomposev2.EnvironmentAction_ENVIRONMENT_ACTION_REMOVE,
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("env dir survived remove: %v", err)
	}
}

// TestSystemEnvSkipsHomeWritingConfig confirms env_mode=system skips the
// MCP/skills/plugins disk writes that would clobber the operator's real home,
// while still recording the revision so the config ack succeeds.
func TestSystemEnvSkipsHomeWritingConfig(t *testing.T) {
	m, workRoot := newTestManager(t)
	m.opts.systemEnvAllowed = true
	spec := envSpec("env-sys-cfg", "system", "")
	spec.Mcps = []*agentcomposev2.MCPServerSpec{{Name: "x", Type: "sse", Url: "https://gw/x/sse"}}
	spec.Skills = []*agentcomposev2.SkillSpec{{Name: "sk", Path: t.TempDir()}}
	if err := m.create(context.Background(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Wait for async provisioning to complete before accessing session fields.
	waitProvisioned(t, m, "env-sys-cfg")
	// Nothing under the operator's real home should have been touched by
	// node-managed config. The per-session runtime tree still exists for state.
	sess, _ := m.lookupSession("env-sys-cfg")
	home, _ := os.UserHomeDir()
	if sess.home != home {
		t.Fatalf("system session home = %q, want %q", sess.home, home)
	}
	// applyMCPs in system mode must succeed (ack ok) without writing to the
	// operator's home mcp config.
	res, err := m.applyMCPs("env-sys-cfg", 2, spec.Mcps)
	if err != nil || res.restartRequired {
		t.Fatalf("applyMCPs system: res=%+v err=%v", res, err)
	}
	// Per-session state root was still prepared (runtime state is isolated).
	if sess.stateRoot == "" {
		t.Fatalf("system session lost its per-session state root")
	}
	_ = workRoot
}

func TestResetRuntimeStatePreservesOnlyMCPConfig(t *testing.T) {
	stateRoot := t.TempDir()
	mcpPath := runtimeMCPConfigPath(stateRoot)
	if err := os.MkdirAll(filepath.Dir(mcpPath), 0o755); err != nil {
		t.Fatal(err)
	}
	mcpConfig := []byte(`{"mcps":{"issue-workflow":{"type":"remote"}}}`)
	if err := os.WriteFile(mcpPath, mcpConfig, 0o644); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(stateRoot, "conversations", "turn.json")
	if err := os.MkdirAll(filepath.Dir(stalePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := resetRuntimeState(stateRoot); err != nil {
		t.Fatalf("resetRuntimeState: %v", err)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("conversation state survived fresh reset: %v", err)
	}
	got, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatalf("read preserved MCP config: %v", err)
	}
	if string(got) != string(mcpConfig) {
		t.Fatalf("MCP config = %q, want %q", got, mcpConfig)
	}
}

func TestResetRuntimeStateWithoutMCPRecreatesEmptyRoot(t *testing.T) {
	stateRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateRoot, "state.json"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := resetRuntimeState(stateRoot); err != nil {
		t.Fatalf("resetRuntimeState: %v", err)
	}
	entries, err := os.ReadDir(stateRoot)
	if err != nil {
		t.Fatalf("read reset state root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("reset state root contains %d entries, want 0", len(entries))
	}
}

// TestConfigWritesReportNoRestart confirms config writes (LLM/MCP/skills/plugins)
// never report restart-required: the runtime re-prepares from the per-turn
// snapshot, so a config change takes effect on the next turn.
func TestConfigWritesReportNoRestart(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, _ := twoClaudeSessions(t, m)

	llm := &agentcomposev2.NodeLLMConfig{Endpoint: "https://llm", ApiKey: "k", Model: "m"}
	if res, err := m.configureLLM(sessA.id, 1, llm); err != nil || res.restartRequired {
		t.Fatalf("configureLLM: res=%+v err=%v", res, err)
	}
	if res, err := m.applyMCPs(sessA.id, 2, []*agentcomposev2.MCPServerSpec{
		{Name: "x", Type: "sse", Url: "https://gw/x/sse"},
	}); err != nil || res.restartRequired {
		t.Fatalf("applyMCPs: res=%+v err=%v", res, err)
	}
	if res, err := m.applySkills(sessA.id, 3, nil); err != nil || res.restartRequired {
		t.Fatalf("applySkills: res=%+v err=%v", res, err)
	}
	if res, err := m.applyPlugins(sessA.id, 4, nil); err != nil || res.restartRequired {
		t.Fatalf("applyPlugins: res=%+v err=%v", res, err)
	}
	if res, err := m.configureMode(sessA.id, 5, "yolo"); err != nil || res.restartRequired {
		t.Fatalf("configureMode: res=%+v err=%v", res, err)
	}
}

func TestDeliverInputMessageIDIsIdempotentPerAttempt(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, _ := twoClaudeSessions(t, m)

	// Route the real session through a captured stream executor and capture the
	// structured receipt ACKs the manager emits upstream.
	var stdin strings.Builder
	exec := &streamExecutor{mgr: m, stdin: nopWriteCloser{&stdin}}
	sessA.mu.Lock()
	sessA.executor = exec
	sessA.seenMessageIDs = map[string]bool{}
	sessA.mu.Unlock()
	var upstream []*agentcomposev2.NodeUpstreamFrame
	m.emitStructured = func(frame *agentcomposev2.NodeUpstreamFrame) error {
		upstream = append(upstream, frame)
		return nil
	}

	input := &agentcomposev2.NodeSessionInput{
		SessionId:       sessA.id,
		Kind:            "human_message",
		Text:            "hello",
		ClientMessageId: "message-1",
		DeliveryAttempt: 1,
	}
	m.deliverInput(input)
	m.deliverInput(input) // transport replay: ACK again, never write twice

	if got := strings.Count(stdin.String(), `"type":"human_message"`); got != 1 {
		t.Fatalf("same id+attempt delivered %d times; stdin=%s", got, stdin.String())
	}
	if !strings.Contains(stdin.String(), `"messageId":"message-1"`) ||
		!strings.Contains(stdin.String(), `"deliveryAttempt":1`) {
		t.Fatalf("runtime frame missing idempotency fields: %s", stdin.String())
	}
	if len(upstream) != 2 {
		t.Fatalf("expected first + replay ACK, got %d", len(upstream))
	}
	for _, frame := range upstream {
		event := frame.GetSessionEvent()
		if event.GetEventType() != "input_status" || !strings.Contains(event.GetPayloadJson(), `"message_id":"message-1"`) {
			t.Fatalf("bad receipt ACK: %+v", event)
		}
	}

	// Explicit retry bumps the attempt: same logical message executes again.
	m.deliverInput(&agentcomposev2.NodeSessionInput{
		SessionId:       sessA.id,
		Kind:            "human_message",
		Text:            "hello",
		ClientMessageId: "message-1",
		DeliveryAttempt: 2,
	})
	if got := strings.Count(stdin.String(), `"type":"human_message"`); got != 2 {
		t.Fatalf("bumped attempt should execute again, got %d writes", got)
	}
}

// TestDeliverInputStampsSnapshot confirms the node fills in the session's
// current model/mode/llm on a human_message that arrived without a snapshot, so
// the runtime re-prepares the provider with the latest config for the turn.
func TestDeliverInputStampsSnapshot(t *testing.T) {
	m, _ := newTestManager(t)
	sessA, _ := twoClaudeSessions(t, m)
	// Give the session a model + llm to stamp.
	llm := &agentcomposev2.NodeLLMConfig{Endpoint: "https://llm", ApiKey: "k", Model: "m"}
	if _, err := m.configureLLM(sessA.id, 1, llm); err != nil {
		t.Fatalf("configureLLM: %v", err)
	}

	// The stream executor's deliver writes the frame to stdin; capture it by
	// swapping the stdin pipe for a buffer. We drive the executor directly.
	exec := &streamExecutor{mgr: m}
	var buf strings.Builder
	exec.stdin = nopWriteCloser{&buf}
	exec.mu = sync.Mutex{}

	// Bare human_message, no snapshot on the wire.
	input := &agentcomposev2.NodeSessionInput{
		SessionId: sessA.id,
		Kind:      "human_message",
		Text:      "hi",
	}
	// deliverInput stamps the snapshot from the session mirror before delivering.
	m.deliverInput(input)
	// Re-deliver through the executor to observe the frame (deliverInput routed
	// to the real session's executor, not our buffer; assert the stamping side).
	if input.GetModel() != "m" {
		t.Fatalf("deliverInput did not stamp model: %q", input.GetModel())
	}
	if input.GetLlm() == nil || input.GetLlm().GetEndpoint() != "https://llm" {
		t.Fatalf("deliverInput did not stamp llm: %+v", input.GetLlm())
	}
	// And the executor itself writes model/llm into the frame.
	if err := exec.deliver(input); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	frame := buf.String()
	if !strings.Contains(frame, `"model":"m"`) {
		t.Fatalf("human_message frame missing model snapshot: %s", frame)
	}
	if !strings.Contains(frame, `"endpoint":"https://llm"`) {
		t.Fatalf("human_message frame missing llm snapshot: %s", frame)
	}
}

type nopWriteCloser struct{ *strings.Builder }

func (nopWriteCloser) Close() error { return nil }
