/**
 * Hard gate against `git stash` in task workspaces.
 *
 * A task workspace is a git worktree cut from a SHARED checkout, so `refs/stash`
 * is shared across every worktree of that checkout and the main checkout itself:
 * one `git stash` pollutes the operator's own stack and a later
 * `git stash pop` in a different tree applies the WRONG tree's changes. The soft
 * rule lives in system-context.ts; this is the hard one for the Claude runner.
 *
 * Why a PreToolUse hook rather than a permission check: the task runtime runs
 * with `bypassPermissions`, and the SDK itself documents that
 * `canUseTool` is NOT consulted in that mode ("auto-approves every tool call
 * (except explicit deny rules) ... To gate every tool call, use a PreToolUse
 * hook instead"). Hooks are independent of the permission mode, so this is the
 * only place that actually stops it.
 *
 * The deny reason is fed back to the model, so the refusal is also the
 * instruction: the model reads why it was blocked and the WIP-commit
 * alternative, and usually corrects itself on the next attempt.
 */
import type { HookInput, HookJSONOutput } from "@anthropic-ai/claude-agent-sdk";

/**
 * `git stash` in any of its forms:
 * - `git stash`, `git stash pop|push|apply|drop|clear|list`
 * - with flags/positionals between: `git -C /path stash`, `git --no-pager stash`
 * - chained into a longer command: `cd x && git stash pop`
 * - nested in quotes/subshell: `bash -lc "git stash push"`, `$(git stash list)`
 *
 * Deliberately NOT a bare `stash` substring match — a file or variable named
 * "stash" (`ls stash/`) must not be blocked. The leading boundary accepts quote
 * chars too so a `git` that opens a quoted command (`"git stash"`) is caught,
 * not just one after whitespace/`;`/`&&`.
 */
const GIT_STASH_PATTERN = /(^|[\s;&|(`"'“])(?:command\s+)?git(\s+-{1,2}[A-Za-z][\w-]*(?:[= ]\S*)?)*\s+stash(?:\s|$|["'’])/;

/** True when a Bash command would touch the shared stash stack. */
export function commandUsesGitStash(command: string): boolean {
  return GIT_STASH_PATTERN.test(command);
}

const DENY_REASON = [
  "Blocked: `git stash` is not allowed in this workspace.",
  "The workspace is a git worktree cut from a shared checkout, and the stash",
  "stack (refs/stash) is SHARED across every worktree AND the main checkout —",
  "stashing here pollutes other trees, and a later `git stash pop` can apply",
  "the wrong tree's changes.",
  "",
  "Do this instead:",
  "- Set changes aside: `git add -A && git commit -m \"WIP: <note>\"`",
  "- Bring them back later: `git reset --soft HEAD~1`",
  "",
  "Nothing is lost by committing — the commit lives on this worktree's own",
  "branch, isolated from every other tree.",
].join("\n");

/**
 * PreToolUse hook for Bash. Denies any command matching `git stash` and tells
 * the model the WIP-commit alternative. Every other command passes through
 * untouched.
 */
export async function gitStashGuardHook(input: HookInput): Promise<HookJSONOutput> {
  if (input.hook_event_name !== "PreToolUse") {
    return {};
  }
  if (input.tool_name !== "Bash") {
    return {};
  }
  const toolInput = input.tool_input as { command?: unknown } | undefined;
  const command = typeof toolInput?.command === "string" ? toolInput.command : "";
  if (!command || !commandUsesGitStash(command)) {
    return {};
  }
  return {
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "deny",
      permissionDecisionReason: DENY_REASON,
    },
  };
}
