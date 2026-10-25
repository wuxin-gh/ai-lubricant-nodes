import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { describe, expect, it } from "vitest";
import {
  agentMCPConfigPath,
  readNativeMCPConfig,
  readPluginMCPServers,
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

  it("reads claude's .claude.json as well as .mcp.json", async () => {
    await withTemp(async (root) => {
      const home = path.join(root, "home");
      await fs.mkdir(home, { recursive: true });
      // `claude mcp add` writes the top-level mcpServers map here, NOT ~/.mcp.json.
      // Reading only the latter left every CLI-registered server invisible to a
      // system-env session while the console listed it as available.
      await fs.writeFile(
        path.join(home, ".claude.json"),
        JSON.stringify({ mcpServers: { codegraph: { type: "stdio", command: "codegraph", args: ["serve"] } } }),
      );
      await fs.writeFile(
        path.join(home, ".mcp.json"),
        JSON.stringify({ mcpServers: { projectLevel: { command: "node" } } }),
      );
      const config = await readNativeMCPConfig("claude", home);
      expect(Object.keys(config.mcps ?? {}).sort()).toEqual(["codegraph", "projectLevel"]);
      expect(config.mcps?.codegraph).toMatchObject({ type: "local", command: "codegraph" });
    });
  });

  it("lets .mcp.json win over .claude.json on a shared name", async () => {
    await withTemp(async (root) => {
      const home = path.join(root, "home");
      await fs.mkdir(home, { recursive: true });
      await fs.writeFile(
        path.join(home, ".claude.json"),
        JSON.stringify({ mcpServers: { dup: { command: "old" } } }),
      );
      await fs.writeFile(
        path.join(home, ".mcp.json"),
        JSON.stringify({ mcpServers: { dup: { command: "new" } } }),
      );
      const config = await readNativeMCPConfig("claude", home);
      expect(config.mcps?.dup).toMatchObject({ type: "local", command: "new" });
    });
  });

  it("survives a malformed native config file", async () => {
    await withTemp(async (root) => {
      const home = path.join(root, "home");
      await fs.mkdir(home, { recursive: true });
      await fs.writeFile(path.join(home, ".claude.json"), "{ not json");
      await fs.writeFile(
        path.join(home, ".mcp.json"),
        JSON.stringify({ mcpServers: { ok: { command: "node" } } }),
      );
      // The operator's own broken file must never fail a task session.
      const config = await readNativeMCPConfig("claude", home);
      expect(Object.keys(config.mcps ?? {})).toEqual(["ok"]);
    });
  });
});

describe("readPluginMCPServers", () => {
  /** Lay down a plugin package the way the node syncs one. */
  async function writePlugin(
    root: string,
    name: string,
    files: Record<string, unknown>,
  ): Promise<string> {
    const dir = path.join(root, name);
    await fs.mkdir(path.join(dir, ".claude-plugin"), { recursive: true });
    for (const [rel, body] of Object.entries(files)) {
      const target = path.join(dir, rel);
      await fs.mkdir(path.dirname(target), { recursive: true });
      await fs.writeFile(target, JSON.stringify(body));
    }
    return dir;
  }

  it("reads a plugin's .mcp.json and expands ${CLAUDE_PLUGIN_ROOT}", async () => {
    await withTemp(async (root) => {
      const dir = await writePlugin(root, "claude-mem", {
        ".claude-plugin/plugin.json": { name: "claude-mem", version: "1.0.0" },
        ".mcp.json": {
          mcpServers: {
            "mcp-search": {
              type: "stdio",
              command: "node",
              args: ["${CLAUDE_PLUGIN_ROOT}/scripts/mcp-server.cjs"],
              env: { MEM_HOME: "${CLAUDE_PLUGIN_ROOT}/data" },
            },
          },
        },
      });
      const servers = readPluginMCPServers([dir]);
      expect(servers["mcp-search"]).toMatchObject({ type: "local", command: "node" });
      // Substitution is textual, exactly as the CLI does it: the plugin author's
      // own separators stay as written, so a POSIX-style arg keeps its forward
      // slash after the (Windows) root is spliced in. Node resolves mixed
      // separators fine; normalizing would rewrite the author's intent.
      expect(servers["mcp-search"].args).toEqual([`${dir}/scripts/mcp-server.cjs`]);
      expect(servers["mcp-search"].env?.MEM_HOME.value).toBe(`${dir}/data`);
      // Injected so a self-resolving plugin wrapper reads the right root; its
      // own fallback search points at the operator's ~/.claude, which a synced
      // copy under an isolated session's home does not live under.
      expect(servers["mcp-search"].env?.CLAUDE_PLUGIN_ROOT.value).toBe(dir);
      expect(servers["mcp-search"].env?.PLUGIN_ROOT.value).toBe(dir);
    });
  });

  it("also reads mcpServers from the plugin manifest", async () => {
    await withTemp(async (root) => {
      const dir = await writePlugin(root, "manifest-only", {
        ".claude-plugin/plugin.json": {
          name: "manifest-only",
          mcpServers: { fromManifest: { command: "node", args: ["x.js"] } },
        },
      });
      const servers = readPluginMCPServers([dir]);
      expect(Object.keys(servers)).toEqual(["fromManifest"]);
    });
  });

  it("leaves shell-default forms verbatim for the command's own shell", async () => {
    await withTemp(async (root) => {
      const dir = await writePlugin(root, "wrapper", {
        ".claude-plugin/plugin.json": { name: "wrapper" },
        ".mcp.json": {
          mcpServers: {
            wrapper: {
              type: "stdio",
              command: "sh",
              // The real claude-mem shape: a self-resolving sh -c wrapper. A
              // half-rewritten `${A:-${B:-}}` would corrupt the script.
              args: ["-c", '_E="${CLAUDE_PLUGIN_ROOT:-}"; exec node "$_E/s.cjs"'],
            },
          },
        },
      });
      const servers = readPluginMCPServers([dir]);
      expect(servers.wrapper.args?.[1]).toContain("${CLAUDE_PLUGIN_ROOT:-}");
    });
  });

  it("yields nothing for a plugin that declares no servers or is malformed", async () => {
    await withTemp(async (root) => {
      const bare = await writePlugin(root, "bare", {
        ".claude-plugin/plugin.json": { name: "bare" },
      });
      const broken = path.join(root, "broken");
      await fs.mkdir(broken, { recursive: true });
      await fs.writeFile(path.join(broken, ".mcp.json"), "{ not json");
      expect(readPluginMCPServers([bare, broken])).toEqual({});
    });
  });
});
