import { describe, expect, it } from "vitest";
import { commandUsesGitStash, gitStashGuardHook } from "../src/workspace-guard.js";

describe("commandUsesGitStash", () => {
  it("catches the plain and subcommand forms", () => {
    expect(commandUsesGitStash("git stash")).toBe(true);
    expect(commandUsesGitStash("git stash pop")).toBe(true);
    expect(commandUsesGitStash("git stash push -m note")).toBe(true);
    expect(commandUsesGitStash("git stash clear")).toBe(true);
    expect(commandUsesGitStash("git stash list")).toBe(true);
  });

  it("catches git with global flags before the subcommand", () => {
    expect(commandUsesGitStash("git -C /tmp/repo stash pop")).toBe(true);
    expect(commandUsesGitStash("git --no-pager stash")).toBe(true);
    expect(commandUsesGitStash("git -c core.editor=vim stash push")).toBe(true);
  });

  it("catches chained/subshell commands", () => {
    expect(commandUsesGitStash("cd /tmp/x && git stash")).toBe(true);
    expect(commandUsesGitStash("echo hi; git stash pop")).toBe(true);
    expect(commandUsesGitStash("$(git stash list)")).toBe(true);
    expect(commandUsesGitStash("bash -lc \"git stash push\"")).toBe(true);
  });

  it("does NOT match a bare 'stash' substring", () => {
    expect(commandUsesGitStash("ls stash/")).toBe(false);
    expect(commandUsesGitStash("cat stash.txt")).toBe(false);
    expect(commandUsesGitStash("git status")).toBe(false);
    expect(commandUsesGitStash("git commit -m 'stash the idea'")).toBe(false);
  });
});

describe("gitStashGuardHook", () => {
  it("denies a Bash git-stash command and tells the model what to do", async () => {
    const result = await gitStashGuardHook({
      hook_event_name: "PreToolUse",
      session_id: "s",
      transcript_path: "/tmp/t",
      cwd: "/tmp",
      tool_name: "Bash",
      tool_input: { command: "git stash pop" },
      tool_use_id: "t1",
    } as never);
    const specific = (result as { hookSpecificOutput?: Record<string, unknown> }).hookSpecificOutput;
    expect(specific?.permissionDecision).toBe("deny");
    expect(String(specific?.permissionDecisionReason)).toContain("WIP");
  });

  it("passes through a Bash command that does not stash", async () => {
    const result = await gitStashGuardHook({
      hook_event_name: "PreToolUse",
      session_id: "s",
      transcript_path: "/tmp/t",
      cwd: "/tmp",
      tool_name: "Bash",
      tool_input: { command: "git status --short" },
      tool_use_id: "t2",
    } as never);
    expect(result).toEqual({});
  });

  it("ignores non-Bash tools", async () => {
    const result = await gitStashGuardHook({
      hook_event_name: "PreToolUse",
      session_id: "s",
      transcript_path: "/tmp/t",
      cwd: "/tmp",
      tool_name: "Read",
      tool_input: { file_path: "/tmp/x" },
      tool_use_id: "t3",
    } as never);
    expect(result).toEqual({});
  });
});
