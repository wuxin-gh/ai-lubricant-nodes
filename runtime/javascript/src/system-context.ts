import path from "node:path";
import { readText } from "./fs.js";
import { warn } from "./mpi.js";

const agentSystemPromptRelativePath = path.join("agents", "system-prompts", "system-prompt.txt"); // keep in sync with pkg/execution/agent_files.go

/**
 * Git worktree safety rules, appended to EVERY session's system context.
 *
 * The task workspace is a git worktree cut from a SHARED checkout, so the stash
 * stack (refs/stash) is shared across every worktree of that checkout AND the
 * main checkout itself — one `git stash` pollutes the operator's own stack, and
 * a `git stash pop` in another tree applies the WRONG tree's changes. The
 * prompt rule is the soft layer; the hard gate is the Bash PreToolUse hook in
 * runners/claude.ts (they must stay consistent with each other).
 */
export const WORKSPACE_GIT_RULES = [
  "## Workspace git rules",
  "",
  "The working directory is a git worktree cut from a shared checkout. The stash stack (refs/stash) is SHARED across all worktrees of that checkout and the main checkout itself — one stray `git stash` pollutes every other tree, and a `git stash pop` may apply the wrong tree's changes.",
  "",
  "- NEVER use `git stash`, `git stash pop`, `git stash apply`, or `git stash clear`.",
  "- To temporarily set changes aside, commit them instead: `git add -A && git commit -m \"WIP: <note>\"`.",
  "- To bring them back later: `git reset --soft HEAD~1` (changes return to the index, nothing is lost).",
  "- Never run destructive whole-tree commands (`git reset --hard`, `git checkout .`, `git clean -fd`) — the worktree may hold uncommitted work that only exists here.",
].join("\n");

/** Convention path for agent identity under --state-root (MPI uses the same discovery pattern). */
export function agentSystemPromptPath(stateRoot: string): string {
  return path.join(stateRoot, agentSystemPromptRelativePath);
}

export function buildSystemContext(agentPrompt: string, mpiContext: string): string {
  const agent = agentPrompt.trim();
  if (!agent) {
    return mpiContext.trim() === "" ? WORKSPACE_GIT_RULES : `${mpiContext}\n\n${WORKSPACE_GIT_RULES}`;
  }

  const parts: string[] = ["## Agent Identity", "", agent];
  const mpi = mpiContext.trim();
  if (mpi) {
    parts.push("", mpi);
  }
  parts.push("", WORKSPACE_GIT_RULES);
  return parts.join("\n").trim();
}

export async function readSystemPromptFile(path?: string): Promise<string> {
  if (!path) {
    return "";
  }
  try {
    return (await readText(path)).trim();
  } catch (error) {
    if ((error as NodeJS.ErrnoException)?.code === "ENOENT") {
      return "";
    }
    warn(`could not read agent system prompt ${path}: ${(error as Error)?.message || error}`);
    return "";
  }
}
