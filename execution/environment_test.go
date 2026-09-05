package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// sharedSpec builds a shared-tier CreateSession for the isolation checks below.
func sharedSpec(sessionID, envID string) *agentcomposev2.NodeCreateSession {
	return &agentcomposev2.NodeCreateSession{
		SessionId:  sessionID,
		Provider:   "claude",
		DeferStart: true,
		EnvMode:    "shared",
		EnvId:      envID,
	}
}

// TestSyncEnvironmentInstallsAndPrunes confirms environment maintenance is an
// exact-set install into the env's provider-neutral .agents tree.
func TestSyncEnvironmentInstallsAndPrunes(t *testing.T) {
	m, workRoot := newTestManager(t)
	skA, skB := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(skA, "SKILL.md"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(skB, "SKILL.md"), []byte("b"), 0o644)

	if err := m.syncEnvironment(context.Background(), &agentcomposev2.NodeSyncEnvironment{
		EnvId:  "prod",
		Skills: []*agentcomposev2.SkillSpec{{Name: "sk-a", Path: skA}, {Name: "sk-b", Path: skB}},
	}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	skillsRoot := filepath.Join(workRoot, "envs", "prod", ".agents", "skills")
	for _, name := range []string{"sk-a", "sk-b"} {
		if _, err := os.Stat(filepath.Join(skillsRoot, name, "SKILL.md")); err != nil {
			t.Fatalf("%s not installed: %v", name, err)
		}
	}
	// Re-sync with only sk-b: sk-a must be pruned (real uninstall).
	if err := m.syncEnvironment(context.Background(), &agentcomposev2.NodeSyncEnvironment{
		EnvId:  "prod",
		Skills: []*agentcomposev2.SkillSpec{{Name: "sk-b", Path: skB}},
	}); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillsRoot, "sk-a")); !os.IsNotExist(err) {
		t.Fatalf("sk-a survived environment uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillsRoot, "sk-b", "SKILL.md")); err != nil {
		t.Fatalf("sk-b lost on resync: %v", err)
	}
}

// TestInspectEnvironmentSeesHandInstalled confirms the inventory reports what is
// physically there — including entries no sync placed — so the server can show
// them as extras instead of silently deleting them.
func TestInspectEnvironmentSeesHandInstalled(t *testing.T) {
	m, workRoot := newTestManager(t)
	skillsRoot := filepath.Join(workRoot, "envs", "prod", ".agents", "skills")
	if err := os.MkdirAll(filepath.Join(skillsRoot, "by-hand"), 0o755); err != nil {
		t.Fatal(err)
	}
	inventory, err := m.inspectEnvironment(&agentcomposev2.NodeInspectEnvironment{EnvId: "prod"})
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	found := false
	for _, entry := range inventory {
		if entry.GetKind() == "skill" && entry.GetName() == "by-hand" {
			found = true
		}
	}
	if !found {
		t.Fatalf("hand-installed skill missing from inventory: %+v", inventory)
	}
}

// TestInspectMissingEnvironmentIsEmptyNotError confirms an env that exists only
// in the ledger (created while the node was offline) reads as "nothing
// installed" rather than failing the call.
func TestInspectMissingEnvironmentIsEmptyNotError(t *testing.T) {
	m, _ := newTestManager(t)
	inventory, err := m.inspectEnvironment(&agentcomposev2.NodeInspectEnvironment{EnvId: "never-made"})
	if err != nil {
		t.Fatalf("inspect missing env must not error: %v", err)
	}
	if len(inventory) != 0 {
		t.Fatalf("expected empty inventory, got %+v", inventory)
	}
}

// TestSharedSessionDoesNotRewriteEnvironmentSkills is the core "删掉≠卸载"
// guarantee: a shared-tier session must never touch the environment's installed
// skill files, so one task dropping a skill cannot uninstall it for others.
func TestSharedSessionDoesNotRewriteEnvironmentSkills(t *testing.T) {
	m, workRoot := newTestManager(t)
	// Environment has one installed skill.
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("env-owned"), 0o644)
	if err := m.syncEnvironment(context.Background(), &agentcomposev2.NodeSyncEnvironment{
		EnvId:  "prod",
		Skills: []*agentcomposev2.SkillSpec{{Name: "keep-me", Path: src}},
	}); err != nil {
		t.Fatalf("env sync: %v", err)
	}
	installed := filepath.Join(workRoot, "envs", "prod", ".agents", "skills", "keep-me", "SKILL.md")

	// A session on that env carrying NO skills would, under the old per-session
	// exact-set rewrite, have pruned the env's skills dir to empty.
	if err := m.create(context.Background(), sharedSpec("task-1", "prod")); err != nil {
		t.Fatalf("create shared session: %v", err)
	}
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("session wiped the environment's installed skill: %v", err)
	}
}

// TestSharedSessionSkipsNativeMCPCopy confirms the provider-native MCP file
// (which carries this task's token) is NOT written into a shared HOME, while the
// per-session stateRoot copy the runtime actually reads still is.
func TestSharedSessionSkipsNativeMCPCopy(t *testing.T) {
	m, workRoot := newTestManager(t)
	spec := sharedSpec("task-mcp", "prod")
	spec.Mcps = []*agentcomposev2.MCPServerSpec{
		{Name: "issue-workflow", Type: "sse", Url: "https://gw/mcp/sse?token=TASK-SECRET"},
	}
	if err := m.create(context.Background(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Wait for async provisioning to complete before accessing session fields.
	waitProvisioned(t, m, "task-mcp")
	sess, _ := m.lookupSession("task-mcp")

	// The shared HOME must not contain the native copy with the token.
	if _, err := os.Stat(filepath.Join(sess.home, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("token-bearing .mcp.json written into shared HOME: %v", err)
	}
	// The runtime's authoritative per-session input still has it.
	servers := readRuntimeMcpServers(t, sess.stateRoot)
	if got := runtimeURLOf(servers, "issue-workflow"); got != "https://gw/mcp/sse?token=TASK-SECRET" {
		t.Fatalf("runtime MCP input lost the session's server: %q", got)
	}
	_ = workRoot
}

// TestActiveSkillsSubsetWins confirms an explicit active list is what the task
// activates, and that an empty list falls back to the environment's full set —
// both without touching any files.
func TestActiveSkillsSubsetWins(t *testing.T) {
	m, _ := newTestManager(t)
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("x"), 0o644)
	if err := m.syncEnvironment(context.Background(), &agentcomposev2.NodeSyncEnvironment{
		EnvId: "prod",
		Skills: []*agentcomposev2.SkillSpec{
			{Name: "sk-1", Path: src}, {Name: "sk-2", Path: src},
		},
	}); err != nil {
		t.Fatalf("env sync: %v", err)
	}

	// Explicit subset: only what the task asked for.
	narrowed := sharedSpec("t-narrow", "prod")
	narrowed.ActiveSkills = []string{"sk-1"}
	if got := m.activeSkillNames(narrowed); len(got) != 1 || got[0] != "sk-1" {
		t.Fatalf("explicit subset = %v, want [sk-1]", got)
	}

	// Empty: everything the environment has.
	all := m.activeSkillNames(sharedSpec("t-all", "prod"))
	if len(all) != 2 {
		t.Fatalf("empty active list should activate the env's full set, got %v", all)
	}
}
