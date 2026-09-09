import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { describe, expect, it } from "vitest";
import {
  agentMCPConfigPath,
  readNativeMCPConfig,
  resolveEffectiveMCPConfig,
} from "../src/mcp-config.js";

async function withTemp<T>(fn: (root: string) => Promise<T>): Promise<T> {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "mcp-config-test-"));
  try {
    return await fn(root);
  } finally {
    await fs.rm(root, { recursive: true, force: true });
  }
}

describe("resolveEffectiveMCPConfig", () => {
  it("returns the task config alone for non-system tiers", async () => {
    await withTemp(async (root) => {
      const stateRoot = path.join(root, "state");
      await fs.mkdir(path.dirname(agentMCPConfigPath(stateRoot)), { recursive: true });
      await fs.writeFile(
        agentMCPConfigPath(stateRoot),
        JSON.stringify({ mcps: { task: { type: "local", command: "node" } } }),
      );
      const mcps = await resolveEffectiveMCPConfig(stateRoot, "claude", false, path.join(root, "home"));
      expect(Object.keys(mcps)).toEqual(["task"]);
    });
  });

  it("layers the operator's native MCP under the task config for system tiers (claude)", async () => {
    await withTemp(async (root) => {
      const stateRoot = path.join(root, "state");
      await fs.mkdir(path.dirname(agentMCPConfigPath(stateRoot)), { recursive: true });
      await fs.writeFile(
        agentMCPConfigPath(stateRoot),
        JSON.stringify({ mcps: { shared: { type: "local", command: "node", env: { T: { value: "task" } } } } }),
      );
      // Operator's own native .mcp.json in the system HOME.
      const home = path.join(root, "home");
      await fs.mkdir(home, { recursive: true });
      await fs.writeFile(
        path.join(home, ".mcp.json"),
        JSON.stringify({
          mcpServers: {
            shared: { command: "node", args: ["old"] },
            operatorOnly: { type: "http", url: "https://op.example/mcp" },
          },
        }),
      );
      const mcps = await resolveEffectiveMCPConfig(stateRoot, "claude", true, home);
      // Task token wins on name collision; operator-only servers stay.
      expect(mcps.shared).toMatchObject({ type: "local", command: "node" });
      expect(mcps.shared.env?.T.value).toBe("task");
      expect(mcps.operatorOnly).toMatchObject({ type: "remote", url: "https://op.example/mcp" });
    });
  });

  it("skips the native merge for codex (no per-task channel in system mode)", async () => {
    await withTemp(async (root) => {
      const stateRoot = path.join(root, "state");
      await fs.mkdir(path.dirname(agentMCPConfigPath(stateRoot)), { recursive: true });
      await fs.writeFile(
        agentMCPConfigPath(stateRoot),
        JSON.stringify({ mcps: { task: { type: "local", command: "node" } } }),
      );
      const home = path.join(root, "home");
      await fs.mkdir(home, { recursive: true });
      await fs.writeFile(
        path.join(home, ".mcp.json"),
        JSON.stringify({ mcpServers: { operatorOnly: { command: "node" } } }),
      );
      const mcps = await resolveEffectiveMCPConfig(stateRoot, "codex", true, home);
      expect(Object.keys(mcps)).toEqual(["task"]);
    });
  });
});

describe("readNativeMCPConfig", () => {
  it("normalizes opencode's array-form command into command+args", async () => {
    await withTemp(async (root) => {
      const home = path.join(root, "home");
      const dir = path.join(home, ".config", "opencode");
      await fs.mkdir(dir, { recursive: true });
      await fs.writeFile(
        path.join(dir, "opencode.json"),
        JSON.stringify({
          mcp: {
            std: { type: "local", command: ["npx", "-y", "srv"], environment: { K: "v" } },
            remote: { type: "remote", url: "https://x.example/mcp" },
          },
        }),
      );
      const config = await readNativeMCPConfig("opencode", home);
      expect(config.mcps?.std).toMatchObject({ type: "local", command: "npx", args: ["-y", "srv"] });
      expect(config.mcps?.std.env?.K.value).toBe("v");
      expect(config.mcps?.remote).toMatchObject({ type: "remote", url: "https://x.example/mcp" });
    });
  });

  it("returns empty for a provider it does not know", async () => {
    await withTemp(async (root) => {
      const config = await readNativeMCPConfig("cursor", path.join(root, "home"));
      expect(config.mcps).toBeUndefined();
    });
  });
});
