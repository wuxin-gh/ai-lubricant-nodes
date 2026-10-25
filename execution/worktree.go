package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	agentcomposev2 "ai-lubricant-nodes/common/proto/agentcompose/v2"
	"ai-lubricant-nodes/common/workspaces"
)

// Worktree-backed workspaces: one shared checkout per environment, one git
// worktree per task.
//
// An environment owns a durable clone (“envs/<env_id>/repo“) whose “.git“ is
// shared by every worktree cut from it. A session then gets its own working tree
// at “envs/<env_id>/worktrees/<task_id>“ — a real checkout, but with the
// object database, refs and remotes shared, so creating one is near-instant and
// costs only the working files. That is the point: the alternative (a full clone
// per task) repeats the network transfer and the whole history every time.
//
// Isolation is unchanged — each task still gets its own directory — so this is
// purely about not paying for the same clone over and over.
const (
	// workspaceModeWorktree selects the shared-checkout + per-task worktree
	// layout. Absent/empty keeps the historic per-task clone.
	workspaceModeWorktree = "worktree"
	// workspaceModeExternal selects the ADOPT layout: the session runs directly
	// in a directory that already exists on this node (an operator's own
	// checkout). Nothing is cloned and nothing is written outside the runtime
	// state — the environment merely borrows the tree.
	workspaceModeExternal = "external"
	// workspaceModeFresh is ADOPT's counterpart: the directory is ours to
	// create, and the project's repo is cloned into it. Same tag
	// (external_workdir) carries the target path — the two differ only in
	// whether the tree is already there.
	workspaceModeFresh = "fresh"
	// workspaceModeTag is the NodeCreateSession tag carrying the mode. A tag
	// rather than a proto field: the session spec already carries an open
	// string map, and a new proto field would force a lockstep upgrade of every
	// node for a purely additive behavior.
	workspaceModeTag = "workspace_mode"
	// envIDTag carries the environment this session runs on. The env_id proto
	// field already exists for the HOME tier; this tag is what the worktree
	// layout keys off, so a session can use an environment's resources without
	// necessarily using its checkout (and vice versa).
	envIDTag = "env_id"
	// externalPathTag carries the absolute node-side path of an adopted
	// checkout. Only read when the mode is external — the path comes from the
	// environment row, never from a task, so a task cannot point a session at an
	// arbitrary directory.
	externalPathTag = "external_workdir"
)

// envRepoDir is the environment's durable clone. Its “.git“ is what every
// worktree shares.
func (m *sessionManager) envRepoDir(envID string) string {
	return filepath.Join(m.envDir(envID), "repo")
}

// envWorktreeDir is where a task's worktree is cut.
func (m *sessionManager) envWorktreeDir(envID, taskID string) string {
	return filepath.Join(m.envDir(envID), "worktrees", sanitizeSessionDir(taskID))
}

// workspaceMode reads the requested workspace layout off the session spec.
func workspaceMode(spec *agentcomposev2.NodeCreateSession) string {
	return strings.ToLower(strings.TrimSpace(spec.GetTags()[workspaceModeTag]))
}

// usesWorktree reports whether this session should run in an environment-backed
// worktree. Both pieces are required: a mode without an environment has no
// shared checkout to cut from, and an environment without the mode asked for the
// historic layout.
func usesWorktree(spec *agentcomposev2.NodeCreateSession) bool {
	return workspaceMode(spec) == workspaceModeWorktree && strings.TrimSpace(spec.GetTags()[envIDTag]) != ""
}

// externalWorktreePath returns the absolute path of an adopted checkout, or ""
// when this session is not in external mode.
//
// The path is validated here rather than trusted: it decides a working
// directory, so a relative path (resolved against whatever the node's cwd
// happens to be) or one containing ".." must never be honored. The environment
// row is the only writer, but the check is cheap and the failure mode is
// running the agent in the wrong tree.
func externalWorktreePath(spec *agentcomposev2.NodeCreateSession) string {
	if mode := workspaceMode(spec); mode != workspaceModeExternal && mode != workspaceModeFresh {
		// 两种模式共用同一条路径（external_workdir 标签）：导入 = 目录已存在，
		// 新建 = 目录由我们 clone 出来。区别在 provisionSession 是否动手。
		return ""
	}
	return importedCheckoutPath(spec)
}

// importedCheckoutPath returns the validated node-side path of the environment's
// imported checkout (the “external_workdir“ tag), independent of workspace
// mode. "" when the session has no imported checkout or the path is unsafe.
//
// It is read in BOTH shapes that use the tag:
//   - external/fresh: the session runs IN this directory (see externalWorktreePath);
//   - worktree: this directory is the SHARED CHECKOUT a task's worktree is cut
//     from, instead of the environment's own durable clone. A task on an
//     imported environment is isolated in a worktree while still sharing the
//     operator's checkout objects — nothing is cloned.
func importedCheckoutPath(spec *agentcomposev2.NodeCreateSession) string {
	path := nativeNodePath(strings.TrimSpace(spec.GetTags()[externalPathTag]))
	if path == "" || !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return ""
	}
	for _, part := range strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' }) {
		if part == ".." {
			return ""
		}
	}
	return filepath.Clean(path)
}

// nativeNodePath strips the virtual leading slash the server's file browser uses
// for Windows drives ("/D:/code/x" -> "D:/code/x").
//
// The server stores an environment's external_path in that virtual form. On
// Windows, filepath.IsAbs("/D:/code/x") is false (a leading slash makes the path
// drive-relative), so without this the adoption tag is judged invalid, the node
// silently ignores it, and the session falls back to a per-task clone — the
// "session pulls the repo again" bug. The server now normalizes before sending;
// this keeps an older server (or a hand-set tag) working too.
//
// Only the "/X:..." virtual form is rewritten, and only on Windows: on POSIX a
// path like "/D:/x" is a legitimate absolute path (a directory literally named
// "D:"), so stripping the slash there would be wrong. The server only produces
// the virtual form for Windows drives anyway.
func nativeNodePath(path string) string {
	if runtime.GOOS != "windows" {
		return path
	}
	if len(path) >= 3 && path[0] == '/' && isASCIILetter(path[1]) && path[2] == ':' {
		return path[1:]
	}
	return path
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// prepareWorktree cuts this task's worktree from the environment's checkout,
// which must already exist. Returns the worktree path.
//
// The shared checkout is put in place by the environment's own provisioning
// (async at env creation, retried from the environment page) — this path
// deliberately does NOT clone on demand. A task whose environment was never
// provisioned must fail loudly here, not stall for a minute inside a git clone:
// whether the directory is the operator's own tree (imported) or the platform's
// managed clone, neither belongs to a task to materialize.
//
// Serialized by the caller (workspaceMu): concurrent sessions on the same
// environment must not race the cut, and git takes its own index.lock per
// worktree — a second `worktree add` for the SAME task would fail there rather
// than corrupt anything, but serializing keeps the common path simple.
func (m *sessionManager) prepareWorktree(
	ctx context.Context,
	session *nodeSession,
	spec *agentcomposev2.NodeCreateSession,
	git *agentcomposev2.NodeGitSpec,
) (string, error) {
	envID := strings.TrimSpace(spec.GetTags()[envIDTag])
	taskID := strings.TrimSpace(spec.GetTaskId())
	if taskID == "" {
		// A worktree is per task by construction (that is what the branch name
		// and the directory key off). Without a task id there is nothing to name
		// it after, so fall back rather than inventing one.
		return "", fmt.Errorf("worktree workspace requires a task id")
	}
	// The shared checkout a worktree is cut from. Normally it is the environment's
	// own durable clone (envs/<id>/repo). On an IMPORTED environment the operator's
	// existing tree IS the shared checkout: we cut the worktree from it and clone
	// nothing — that is the whole point of adopting a directory. The worktree
	// itself still lands under envs/<id>/worktrees/<task>, so the operator's tree
	// only gains a worktree registration under its .git, never a working copy.
	repoDir := importedCheckoutPath(spec)
	fromImported := repoDir != ""
	if !fromImported {
		repoDir = m.envRepoDir(envID)
	}
	worktreeDir := m.envWorktreeDir(envID, taskID)

	if initialized, err := workspaces.HostWorkspaceInitialized(repoDir); err != nil {
		return "", err
	} else if !initialized {
		// 检出必须已在（provisioning 放的 / 操作者本来就有的）。绝不在任务路径上
		// 现场 clone——「环境没备好」是环境的问题，报在环境页让人重试。
		if fromImported {
			// 导入的树没了（或从来不是 git 目录）。拒绝胜过悄悄 clone 一份新的：
			// 那会让「采用」看起来成功了，实则替换了操作者的工作区。
			return "", fmt.Errorf("imported checkout %s is not a git working tree", repoDir)
		}
		return "", fmt.Errorf("environment checkout %s is not provisioned (prepare the environment first)", repoDir)
	}
	// An existing worktree is reused: a re-dispatch (or a node restart that lost
	// the session map) must land back in the SAME tree, or the agent would lose
	// every uncommitted change it had made.
	if initialized, err := workspaces.HostWorkspaceInitialized(worktreeDir); err != nil {
		return "", err
	} else if initialized {
		return worktreeDir, nil
	}
	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0o755); err != nil {
		return "", fmt.Errorf("prepare worktree parent: %w", err)
	}
	branch := strings.TrimSpace(git.GetNewBranch())
	if branch == "" {
		branch = "task/" + sanitizeSessionDir(taskID)
	}
	// A new branch is created from the requested base when one was given,
	// otherwise from whatever the shared checkout currently has.
	base := strings.TrimSpace(git.GetBranch())
	args := []string{"worktree", "add", "-b", branch, worktreeDir}
	if base != "" {
		args = append(args, base)
	}
	if err := runNodeGit(ctx, repoDir, args...); err != nil {
		// `-b` fails when the branch already exists — a re-dispatch whose
		// worktree was removed but whose branch survived. Attach the existing
		// branch instead of failing the whole dispatch.
		if retryErr := runNodeGit(ctx, repoDir, "worktree", "add", worktreeDir, branch); retryErr != nil {
			return "", fmt.Errorf("worktree add: %w", err)
		}
	}
	return worktreeDir, nil
}

// ensureEnvRepo clones the environment's checkout once, if it is not there yet.
func (m *sessionManager) ensureEnvRepo(ctx context.Context, repoDir string, git *agentcomposev2.NodeGitSpec) error {
	if initialized, err := workspaces.HostWorkspaceInitialized(repoDir); err != nil {
		return err
	} else if initialized {
		return nil
	}
	if err := workspaces.ResetStaleWorkspace(repoDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		return fmt.Errorf("prepare environment dir: %w", err)
	}
	cfg := workspaces.GitWorkspaceConfig{
		URL:          strings.TrimSpace(git.GetUrl()),
		Branch:       strings.TrimSpace(git.GetBranch()),
		Commit:       strings.TrimSpace(git.GetCommit()),
		Username:     strings.TrimSpace(git.GetUsername()),
		Password:     strings.TrimSpace(git.GetPassword()),
		CreateBranch: false, // the branch belongs to the worktree, not the shared checkout
	}
	if token := strings.TrimSpace(git.GetToken()); token != "" && cfg.Password == "" {
		cfg.Password = token
	}
	cloneURL := cfg.URL
	authUser, authPass := cfg.Username, cfg.Password
	if stripped, user, pass, ok := workspaces.SplitGitCredentialURL(cloneURL); ok {
		cloneURL = stripped
		if authUser == "" && authPass == "" {
			authUser, authPass = user, pass
		}
	}
	authArgs := workspaces.GitAuthHeaderArgs(authUser, authPass, cloneURL)
	args := append(authArgs, workspaces.GitCloneArgs(cloneURL, cfg, repoDir)...)
	if err := runNodeGit(ctx, "", args...); err != nil {
		return err
	}
	if cfg.Commit != "" {
		fetchArgs := append(authArgs, workspaces.GitCommitFetchArgs(cfg.Commit)...)
		if err := runNodeGit(ctx, repoDir, fetchArgs...); err == nil {
			_ = runNodeGit(ctx, repoDir, "checkout", "FETCH_HEAD")
		}
	}
	return nil
}

// removeWorktree detaches a task's worktree from the environment's checkout.
//
// `git worktree remove` is deliberately NOT used: it refuses a dirty tree, and
// losing a task's uncommitted work is worse than leaving a directory behind. The
// branch is kept too — a task's commits are the one thing here worth preserving.
//
// repoDir is the shared checkout the worktree was cut from (the managed clone or
// the operator's imported tree — see nodeSession.worktreeRepoDir): the stale
// registration lives in THAT repo's .git, so pruning must target it.
func (m *sessionManager) removeWorktree(envID, taskID, repoDir string) {
	if strings.TrimSpace(repoDir) == "" {
		repoDir = m.envRepoDir(envID)
	}
	worktreeDir := m.envWorktreeDir(envID, taskID)
	if err := os.RemoveAll(worktreeDir); err != nil {
		m.logger.Warn("worktree remove failed", "worktree", worktreeDir, "error", err)
	}
	// Prune so git forgets the entry; a stale registration makes a later
	// `worktree add` to the same path fail.
	if err := runNodeGit(context.Background(), repoDir, "worktree", "prune"); err != nil {
		m.logger.Warn("worktree prune failed", "repo", repoDir, "error", err)
	}
}

// sharedDepDirs are dependency directories linked from the environment's shared
// checkout into each worktree. They are gitignored, so a fresh worktree has none
// of them and would otherwise reinstall from scratch on every task.
var sharedDepDirs = []string{
	"node_modules",
	".venv",
	"venv",
	".tox",
	"target/debug",
	"target/release",
}

// linkSharedDeps links dependency directories from a source checkout into a
// worktree, so an isolated tree does not have to reinstall them.
//
// Windows note (measured, not assumed): a symlink created by Git Bash's `ln -s`
// is a COPY — a 485 MB .venv became two 485 MB trees. `mklink /J` creates a real
// junction at zero cost, so the platform split below is required, not cosmetic.
func linkSharedDeps(worktreeDir, sourceDir string, names []string) {
	for _, name := range names {
		src := filepath.Join(sourceDir, name)
		dst := filepath.Join(worktreeDir, name)
		if info, err := os.Stat(src); err != nil || !info.IsDir() {
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			continue // already linked or present
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			continue
		}
		if err := linkDir(src, dst); err != nil {
			// Non-fatal: a worktree without the link still runs, just slower to
			// warm up. Reporting it would be noise on every session.
			continue
		}
	}
}

func linkDir(src, dst string) error {
	if runtime.GOOS == "windows" {
		// A junction needs no elevation, unlike a symlink, which is why this is
		// the Windows path rather than os.Symlink.
		return exec.Command("cmd", "/c", "mklink", "/J", dst, src).Run()
	}
	return os.Symlink(src, dst)
}
