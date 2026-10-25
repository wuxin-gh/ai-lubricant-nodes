import fs from "node:fs/promises";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { formatMpiContext } from "../src/mpi.js";
import { agentSystemPromptPath, buildSystemContext, readSystemPromptFile } from "../src/system-context.js";
import { captureStdio, withTempSession } from "./helpers.js";

describe("system-context", () => {
  it("buildSystemContext places agent identity before mpi catalog", () => {
    const mpiFormatted = formatMpiContext("# Email tools\n", "/data/runtime/mpi/resources");
    const combined = buildSystemContext("Reply only in Chinese", mpiFormatted);

    expect(combined.indexOf("## Agent Identity")).toBeLessThan(combined.indexOf("## MPI Catalog"));
    expect(combined).toContain("Reply only in Chinese");
    expect(combined).toContain("# Email tools");
  });

  it("buildSystemContext returns agent-only context when mpi is empty", () => {
    const combined = buildSystemContext("Agent prompt", "");
    expect(combined).toContain("## Agent Identity");
    expect(combined).toContain("Agent prompt");
    expect(combined).not.toContain("## MPI Catalog");
  });

  it("buildSystemContext returns mpi-only context when agent prompt is empty", () => {
    const mpiFormatted = formatMpiContext("# Email tools\n", "/data/runtime/mpi/resources");
    const combined = buildSystemContext("", mpiFormatted);
    // mpi catalog comes first; the always-on workspace git rules are appended.
    expect(combined.startsWith(mpiFormatted)).toBe(true);
    expect(combined).toContain("## Workspace git rules");
    expect(combined).not.toContain("## Agent Identity");
  });

  it("buildSystemContext always appends the workspace git rules", () => {
    // Even with no agent prompt and no mpi, the stash-safety rules must be
    // present: the workspace is a shared-checkout worktree in every session.
    const combined = buildSystemContext("   ", "\n");
    expect(combined).toContain("## Workspace git rules");
    expect(combined).toContain("git stash");
    expect(combined).toContain("WIP");
  });

  it("agentSystemPromptPath resolves under state root", () => {
    expect(agentSystemPromptPath("/data/state")).toBe(
      path.join("/data/state", "agents", "system-prompts", "system-prompt.txt"),
    );
  });

  it("readSystemPromptFile reads trimmed content", async () => {
    await withTempSession(async (root) => {
      const promptPath = agentSystemPromptPath(path.join(root, "state"));
      await fs.mkdir(path.dirname(promptPath), { recursive: true });
      await fs.writeFile(promptPath, "  Reply only in Chinese  ", "utf8");

      await expect(readSystemPromptFile(promptPath)).resolves.toBe("Reply only in Chinese");
    });
  });

  it("readSystemPromptFile returns empty string for missing files", async () => {
    await withTempSession(async (root) => {
      await expect(readSystemPromptFile(path.join(root, "missing.txt"))).resolves.toBe("");
      await expect(readSystemPromptFile()).resolves.toBe("");
    });
  });

  it("warns and returns empty string when system prompt cannot be read", async () => {
    await withTempSession(async (root) => {
      const promptPath = agentSystemPromptPath(path.join(root, "state"));
      await fs.mkdir(promptPath, { recursive: true });
      const stdio = captureStdio();
      try {
        await expect(readSystemPromptFile(promptPath)).resolves.toBe("");
      } finally {
        stdio.restore();
      }
      expect(stdio.stderr).toMatch(/warning: could not read agent system prompt/);
    });
  });
});
