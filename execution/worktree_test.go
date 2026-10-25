package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
)

// Worktree-backed workspaces: one shared checkout per environment, one worktree
// per task.
//
// The behavior worth locking down is the *sharing*: a second task on the same
// environment must reuse the first checkout's object database rather than
// re-cloning, and each task must still get its own isolated working tree.
// These tests use a real local git repository, so they exercise the actual git
// commands rather than a mock of them.

// initOrigin creates a real git repo to clone from, with one commit on `main`.
func initOrigin(t *testing.T) string {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatalf("mkdir origin: %v", err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = origin
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	run("add", ".")
	run("commit", "-m", "init")
	return origin
}

func worktreeSpec(envID, taskID, origin string) *agentcomposev2.NodeCreateSession {
	return &agentcomposev2.NodeCreateSession{
		SessionId:  "sess-" + taskID,
		TaskId:     taskID,
		Provider:   "claude",
		DeferStart: true,
		Tags: map[string]string{
			workspaceModeTag: workspaceModeWorktree,
			envIDTag:         envID,
		},
		Git: &agentcomposev2.NodeGitSpec{Url: origin},
	}
}

// provisionManaged stands in for the environment's OWN provisioning
// (manageEnvironment CREATE): the shared checkout exists before any task
// dispatches. prepareWorktree deliberately does not clone on demand — a task
// whose environment was never provisioned must fail loudly, not stall.
func provisionManaged(t *testing.T, m *sessionManager, envID, origin string) {
	t.Helper()
	if err := m.ensureEnvRepo(t.Context(), m.envRepoDir(envID), &agentcomposev2.NodeGitSpec{Url: origin}); err != nil {
		t.Fatalf("provision env %s: %v", envID, err)
	}
}

func TestUsesWorktreeRequiresBothModeAndEnv(t *testing.T) {
	base := func() *agentcomposev2.NodeCreateSession {
		return &agentcomposev2.NodeCreateSession{Tags: map[string]string{}}
	}
	cases := []struct {
		name string
		tags map[string]string
		want bool
	}{
		{"neither", map[string]string{}, false},
		{"mode only", map[string]string{workspaceModeTag: workspaceModeWorktree}, false},
		{"env only", map[string]string{envIDTag: "e1"}, false},
		{"both", map[string]string{workspaceModeTag: workspaceModeWorktree, envIDTag: "e1"}, true},
		{"other mode", map[string]string{workspaceModeTag: "clone", envIDTag: "e1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := base()
			spec.Tags = tc.tags
			if got := usesWorktree(spec); got != tc.want {
				t.Fatalf("usesWorktree = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPrepareWorktreeSharesCheckoutAcrossTasks(t *testing.T) {
	origin := initOrigin(t)
	m, _ := newTestManager(t)
	// The environment's checkout is put in place by provisioning, not by the task.
	provisionManaged(t, m, "env-1", origin)

	specA := worktreeSpec("env-1", "task-a", origin)
	sessA := &nodeSession{id: specA.GetSessionId(), taskID: "task-a", worktreeEnvID: "env-1"}
	pathA, err := m.prepareWorktree(t.Context(), sessA, specA, specA.GetGit())
	if err != nil {
		t.Fatalf("prepare worktree A: %v", err)
	}

	repoDir := m.envRepoDir("env-1")
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		t.Fatalf("shared checkout missing .git: %v", err)
	}
	// The task's own tree exists and is a real checkout of the origin content.
	if _, err := os.Stat(filepath.Join(pathA, "README.md")); err != nil {
		t.Fatalf("worktree A missing checked-out file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pathA, ".git")); err != nil {
		// In a worktree `.git` is a FILE pointing at the shared gitdir.
		t.Fatalf("worktree A missing .git pointer: %v", err)
	}

	// A second task on the same environment must share the object database —
	// that is the whole reason for this layout — while getting its own tree.
	specB := worktreeSpec("env-1", "task-b", origin)
	sessB := &nodeSession{id: specB.GetSessionId(), taskID: "task-b", worktreeEnvID: "env-1"}
	pathB, err := m.prepareWorktree(t.Context(), sessB, specB, specB.GetGit())
	if err != nil {
		t.Fatalf("prepare worktree B: %v", err)
	}
	if pathA == pathB {
		t.Fatalf("two tasks shared one worktree: %s", pathA)
	}
	// Both worktrees are registered against the one shared checkout.
	out, err := exec.Command("git", "-C", repoDir, "worktree", "list").CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v\n%s", err, out)
	}
	if n := strings.Count(string(out), "\n"); n < 3 {
		t.Fatalf("expected 3 worktree entries (main + 2 tasks), got %d:\n%s", n, out)
	}
	// Each task is on its own branch, so their commits cannot collide.
	branchA := currentBranch(t, pathA)
	branchB := currentBranch(t, pathB)
	if branchA == branchB {
		t.Fatalf("both worktrees on branch %q; tasks must not share a branch", branchA)
	}
	if branchA != "task/task-a" || branchB != "task/task-b" {
		t.Fatalf("unexpected branches: %q %q", branchA, branchB)
	}
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse in %s: %v", dir, err)
	}
	return strings.TrimSpace(string(out))
}

func TestPrepareWorktreeReusesExistingTree(t *testing.T) {
	origin := initOrigin(t)
	m, _ := newTestManager(t)
	provisionManaged(t, m, "env-1", origin)
	spec := worktreeSpec("env-1", "task-a", origin)
	sess := &nodeSession{id: spec.GetSessionId(), taskID: "task-a", worktreeEnvID: "env-1"}

	path, err := m.prepareWorktree(t.Context(), sess, spec, spec.GetGit())
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	// Uncommitted work must survive a re-dispatch: the agent's changes are the
	// only copy, and re-cloning over them would destroy them.
	scratch := filepath.Join(path, "work-in-progress.txt")
	if err := os.WriteFile(scratch, []byte("keep me\n"), 0o644); err != nil {
		t.Fatalf("write scratch: %v", err)
	}

	again, err := m.prepareWorktree(t.Context(), sess, spec, spec.GetGit())
	if err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if again != path {
		t.Fatalf("second prepare moved the worktree: %s -> %s", path, again)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("re-prepare destroyed uncommitted work: %v", err)
	}
}

func TestPrepareWorktreeAttachesExistingBranch(t *testing.T) {
	origin := initOrigin(t)
	m, _ := newTestManager(t)
	provisionManaged(t, m, "env-1", origin)
	spec := worktreeSpec("env-1", "task-a", origin)
	sess := &nodeSession{id: spec.GetSessionId(), taskID: "task-a", worktreeEnvID: "env-1"}

	path, err := m.prepareWorktree(t.Context(), sess, spec, spec.GetGit())
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	// Simulate the worktree being removed while its branch survives — what a
	// session delete does, since the branch is deliberately kept.
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}
	if err := runNodeGit(t.Context(), m.envRepoDir("env-1"), "worktree", "prune"); err != nil {
		t.Fatalf("prune: %v", err)
	}

	// `worktree add -b task/task-a` now fails (branch exists); the fallback must
	// attach the existing branch instead of failing the whole dispatch.
	if _, err := m.prepareWorktree(t.Context(), sess, spec, spec.GetGit()); err != nil {
		t.Fatalf("re-add with existing branch: %v", err)
	}
}

func TestRemoveWorktreeKeepsBranch(t *testing.T) {
	origin := initOrigin(t)
	m, _ := newTestManager(t)
	provisionManaged(t, m, "env-1", origin)
	spec := worktreeSpec("env-1", "task-a", origin)
	sess := &nodeSession{id: spec.GetSessionId(), taskID: "task-a", worktreeEnvID: "env-1"}
	if _, err := m.prepareWorktree(t.Context(), sess, spec, spec.GetGit()); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	m.removeWorktree("env-1", "task-a", "")

	if _, err := os.Stat(m.envWorktreeDir("env-1", "task-a")); !os.IsNotExist(err) {
		t.Fatalf("worktree dir survived removal: %v", err)
	}
	// The branch is the task's only durable output; it must outlive the tree.
	out, err := exec.Command("git", "-C", m.envRepoDir("env-1"), "branch", "--list", "task/task-a").Output()
	if err != nil {
		t.Fatalf("branch --list: %v", err)
	}
	if !strings.Contains(string(out), "task/task-a") {
		t.Fatalf("branch was deleted with the worktree:\n%s", out)
	}
}

func TestRemoveWorktreePrunesRegistration(t *testing.T) {
	origin := initOrigin(t)
	m, _ := newTestManager(t)
	provisionManaged(t, m, "env-1", origin)
	spec := worktreeSpec("env-1", "task-a", origin)
	sess := &nodeSession{id: spec.GetSessionId(), taskID: "task-a", worktreeEnvID: "env-1"}
	if _, err := m.prepareWorktree(t.Context(), sess, spec, spec.GetGit()); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	m.removeWorktree("env-1", "task-a", "")

	// A stale registration would make a later add at the same path fail, so the
	// list must be back to just the main checkout.
	out, _ := exec.Command("git", "-C", m.envRepoDir("env-1"), "worktree", "list").CombinedOutput()
	if strings.Contains(string(out), "task-a") {
		t.Fatalf("worktree registration survived removal:\n%s", out)
	}
}

func TestPrepareWorktreeRequiresTaskID(t *testing.T) {
	origin := initOrigin(t)
	m, _ := newTestManager(t)
	// No task id: there is nothing to name the worktree or its branch after.
	spec := &agentcomposev2.NodeCreateSession{
		SessionId: "s1", Provider: "claude", DeferStart: true,
		Tags: map[string]string{workspaceModeTag: workspaceModeWorktree, envIDTag: "env-1"},
		Git:  &agentcomposev2.NodeGitSpec{Url: origin},
	}
	sess := &nodeSession{id: "s1", worktreeEnvID: "env-1"}
	if _, err := m.prepareWorktree(t.Context(), sess, spec, spec.GetGit()); err == nil {
		t.Fatal("expected an error when the spec carries no task id")
	}
}

// TestPrepareWorktreeRefusesUnprovisionedEnv locks the no-on-demand-clone rule:
// the clone belongs to the environment's provisioning, so a task dispatched to
// an environment whose checkout was never put in place fails LOUDLY (the
// environment page shows the failure and offers a retry) — it must not stall
// inside a minute-long git clone on the task path.
func TestPrepareWorktreeRefusesUnprovisionedEnv(t *testing.T) {
	origin := initOrigin(t)
	m, _ := newTestManager(t)

	spec := worktreeSpec("env-missing", "task-a", origin)
	sess := &nodeSession{id: spec.GetSessionId(), taskID: "task-a", worktreeEnvID: "env-missing"}
	_, err := m.prepareWorktree(t.Context(), sess, spec, spec.GetGit())
	if err == nil {
		t.Fatal("expected an error for an unprovisioned environment")
	}
	if !strings.Contains(err.Error(), "not provisioned") {
		t.Fatalf("unexpected error: %v", err)
	}
	// The failed dispatch must not have cloned anything as a side effect.
	if _, statErr := os.Stat(m.envRepoDir("env-missing")); !os.IsNotExist(statErr) {
		t.Fatalf("task path must not clone the environment's checkout: %v", statErr)
	}
}

// TestPrepareWorktreeCutsFromImportedCheckout locks the imported-environment
// shape: the worktree is cut from the operator's EXISTING tree (external_workdir),
// nothing is cloned, and the environment's own managed clone is never created.
// This is what makes "an adopted directory + an isolated per-task worktree"
// work: the operator's tree gains only a worktree registration under its .git.
func TestPrepareWorktreeCutsFromImportedCheckout(t *testing.T) {
	origin := initOrigin(t)
	m, _ := newTestManager(t)

	// The operator's imported tree: a real checkout that already exists.
	imported := filepath.Join(t.TempDir(), "op-checkout")
	if out, err := exec.Command("git", "clone", origin, imported).CombinedOutput(); err != nil {
		t.Fatalf("clone imported: %v\n%s", err, out)
	}

	spec := &agentcomposev2.NodeCreateSession{
		SessionId: "sess-imp", TaskId: "task-imp", Provider: "claude", DeferStart: true,
		Tags: map[string]string{
			workspaceModeTag: workspaceModeWorktree,
			envIDTag:         "env-imp",
			externalPathTag:  imported,
		},
		// No git spec on purpose: an imported checkout must not be cloned/fetched.
	}
	sess := &nodeSession{id: "sess-imp", taskID: "task-imp", worktreeEnvID: "env-imp"}

	path, err := m.prepareWorktree(t.Context(), sess, spec, spec.GetGit())
	if err != nil {
		t.Fatalf("prepare worktree from imported checkout: %v", err)
	}
	// The task's tree exists, has the checked-out content, and is NOT the
	// operator's tree (isolation preserved).
	if _, err := os.Stat(filepath.Join(path, "README.md")); err != nil {
		t.Fatalf("imported worktree missing checked-out file: %v", err)
	}
	if path == imported {
		t.Fatal("worktree must not BE the imported checkout — that would defeat isolation")
	}
	// The environment's managed clone was never created: adoption clones nothing.
	if _, err := os.Stat(m.envRepoDir("env-imp")); !os.IsNotExist(err) {
		t.Fatalf("managed clone must not exist for an imported environment: %v", err)
	}
	// The operator's own checkout still works (only a .git/worktrees entry added).
	if out, err := exec.Command("git", "-C", imported, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("imported checkout left unusable: %v\n%s", err, out)
	}
}

func TestLinkSharedDepsSkipsMissingAndExisting(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	worktree := filepath.Join(root, "worktree")
	for _, dir := range []string{source, worktree} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// One linkable dir, one that does not exist in the source, and one the
	// worktree already has (which must not be touched).
	if err := os.MkdirAll(filepath.Join(source, "node_modules"), 0o755); err != nil {
		t.Fatalf("mkdir node_modules: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, ".venv"), 0o755); err != nil {
		t.Fatalf("mkdir .venv: %v", err)
	}

	linkSharedDeps(worktree, source, []string{"node_modules", "missing", ".venv"})

	if _, err := os.Stat(filepath.Join(worktree, "node_modules")); err != nil {
		t.Fatalf("expected node_modules to be linked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "missing")); !os.IsNotExist(err) {
		t.Fatalf("a missing source dir must not be linked: %v", err)
	}
	// The pre-existing directory is left as-is rather than replaced.
	info, err := os.Lstat(filepath.Join(worktree, ".venv"))
	if err != nil {
		t.Fatalf("stat .venv: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("an existing directory must not be replaced by a link")
	}
}

// --- external mode: the session runs in a directory that already exists ---

func TestExternalWorktreePathValidation(t *testing.T) {
	// An absolute path is platform-defined: "/data/repo" is absolute on POSIX
	// but drive-less on Windows, where filepath.IsAbs is (correctly) false. Derive
	// the "plain absolute" case from a temp dir so it is absolute by the same
	// rule the code uses, whatever the OS calls absolute.
	absPath := filepath.Join(t.TempDir(), "repo")
	cases := []struct {
		name string
		tags map[string]string
		want string
	}{
		{"not external mode", map[string]string{externalPathTag: absPath}, ""},
		{"no path", map[string]string{workspaceModeTag: workspaceModeExternal}, ""},
		{"relative path", map[string]string{workspaceModeTag: workspaceModeExternal, externalPathTag: "rel/repo"}, ""},
		{"parent traversal", map[string]string{workspaceModeTag: workspaceModeExternal, externalPathTag: absPath + string(filepath.Separator) + ".." + string(filepath.Separator) + "escape"}, ""},
		{"plain absolute", map[string]string{workspaceModeTag: workspaceModeExternal, externalPathTag: absPath}, filepath.Clean(absPath)},
		{"windows absolute", map[string]string{workspaceModeTag: workspaceModeExternal, externalPathTag: `D:\code\repo`}, filepath.Clean(`D:\code\repo`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &agentcomposev2.NodeCreateSession{Tags: tc.tags}
			if got := externalWorktreePath(spec); got != tc.want {
				t.Fatalf("externalWorktreePath = %q, want %q", got, tc.want)
			}
		})
	}
}

// The server stores an environment's external_path in the virtual form the file
// browser speaks ("/D:/code/x"). The node adopts a directory only when
// filepath.IsAbs accepts the tag, and on Windows that virtual form is NOT
// absolute — so the session silently fell back to a per-task clone (the
// "session pulls the repo again" bug). nativeNodePath strips the virtual slash
// on Windows only.
func TestNativeNodePath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/D:/code/x", "D:/code/x"},
		{"/c:/code/x", "c:/code/x"},
		{`D:\code\x`, `D:\code\x`},
		{"D:/code/x", "D:/code/x"},
		{"/home/x", "/home/x"},
		{"", ""},
		{"/", "/"},
		{"/D", "/D"}, // too short to be a drive
		{"/1:/x", "/1:/x"},
	}
	for _, tc := range cases {
		got := nativeNodePath(tc.in)
		if runtime.GOOS == "windows" {
			if got != tc.want {
				t.Fatalf("nativeNodePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
			continue
		}
		// Off Windows the virtual form is a legitimate POSIX path: untouched.
		if got != tc.in {
			t.Fatalf("nativeNodePath(%q) = %q, want it unchanged on %s", tc.in, got, runtime.GOOS)
		}
	}
}

// An adopted checkout must be used AS the working tree: no clone, nothing
// created inside it, and deleting the session must leave it untouched.
func TestCreateSessionExternalWorkdir(t *testing.T) {
	origin := initOrigin(t)
	m, _ := newTestManager(t)

	// The "operator's" checkout sits outside the manager's workRoot entirely.
	checkout := filepath.Join(t.TempDir(), "my-checkout")
	if err := runNodeGit(t.Context(), "", "clone", origin, checkout); err != nil {
		t.Fatalf("clone origin into checkout: %v", err)
	}

	spec := &agentcomposev2.NodeCreateSession{
		SessionId:  "sess-ext",
		TaskId:     "task-ext",
		Provider:   "claude",
		DeferStart: true,
		Tags: map[string]string{
			workspaceModeTag: workspaceModeExternal,
			envIDTag:         "env-1",
			externalPathTag:  checkout,
		},
		Git: &agentcomposev2.NodeGitSpec{Url: "https://example.invalid/repo.git"},
	}
	if err := m.create(t.Context(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	waitProvisioned(t, m, "sess-ext")

	sess, err := m.lookupSession("sess-ext")
	if err != nil {
		t.Fatalf("session not found: %v", err)
	}
	// The session runs in the operator's tree, and nothing was cloned into it
	// (the git URL above is unreachable on purpose: cloning it would fail the
	// session — so reaching provisioned status proves no clone was attempted).
	if sess.workDir != filepath.Clean(checkout) {
		t.Fatalf("workDir = %s, want the adopted checkout %s", sess.workDir, checkout)
	}
	if _, err := os.Stat(filepath.Join(sess.workDir, ".agent-compose")); !os.IsNotExist(err) {
		t.Fatalf("runtime state must not be written inside the adopted tree: %v", err)
	}
	// Runtime state lives under the environment's own dir instead.
	if _, err := os.Stat(sess.runtimeDir); err != nil {
		t.Fatalf("runtime dir outside the adopted tree missing: %v", err)
	}

	// Delete must clean its runtime state but never touch the adopted tree.
	scratch := filepath.Join(sess.workDir, "scratch.txt")
	if err := os.WriteFile(scratch, []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("write scratch: %v", err)
	}
	if err := m.delete("sess-ext"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("delete destroyed the adopted tree: %v", err)
	}
	if _, err := os.Stat(sess.runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("runtime state survived delete: %v", err)
	}
}

// A missing adopted directory is the task's failure and must surface as a
// provisioning error — silently MkdirAll'ing it would strand the agent in an
// empty tree with nothing to work on.
func TestCreateSessionExternalWorkdirMissing(t *testing.T) {
	m, _ := newTestManager(t)
	missing := filepath.Join(t.TempDir(), "nope")
	spec := &agentcomposev2.NodeCreateSession{
		SessionId: "sess-ext2", TaskId: "task-ext2", Provider: "claude", DeferStart: true,
		Tags: map[string]string{
			workspaceModeTag: workspaceModeExternal,
			envIDTag:         "env-1",
			externalPathTag:  missing,
		},
		Git: &agentcomposev2.NodeGitSpec{Url: "https://example.invalid/repo.git"},
	}
	if err := m.create(t.Context(), spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := m.lookupSession("sess-ext2"); err != nil {
			break // removeFailedSession dropped it: the failure was reported
		}
		if time.Now().After(deadline) {
			t.Fatal("session with a missing adopted checkout was never failed")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
