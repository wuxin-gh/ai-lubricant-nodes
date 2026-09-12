import fs from "node:fs/promises";
import path from "node:path";

export type RuntimeMCPEnvVar = {
  value: string;
  secret?: boolean;
};

export type RuntimeMCPServer = {
  type: "local" | "remote";
  transport?: "sse" | "http";
  command?: string;
  args?: string[];
  env?: Record<string, RuntimeMCPEnvVar>;
  url?: string;
  headers?: Record<string, RuntimeMCPEnvVar>;
};

export type RuntimeMCPConfig = {
  mcps?: Record<string, RuntimeMCPServer>;
};

const mcpConfigRelativePath = path.join("agents", "mcp", "config.json");

export function agentMCPConfigPath(stateRoot: string): string {
  return path.join(stateRoot, mcpConfigRelativePath);
}

export async function readMCPConfig(stateRoot: string): Promise<RuntimeMCPConfig> {
  const configPath = agentMCPConfigPath(stateRoot);
  try {
    const raw = await fs.readFile(configPath, "utf-8");
    const parsed = JSON.parse(raw) as RuntimeMCPConfig;
    return parsed && typeof parsed === "object" ? parsed : {};
  } catch (error) {
    if ((error as NodeJS.ErrnoException)?.code === "ENOENT") {
      return {};
    }
    throw error;
  }
}

/**
 * Read a provider's native MCP config from a system HOME, read-only, and
 * normalize it into the runtime's own RuntimeMCPServer shape. Only the
 * three providers whose CLI reads a JSON config file are covered; codex's
 * native MCP lives in TOML (~/.codex/config.toml) and is deliberately not
 * parsed here — it has no task-level MCP channel in system mode.
 */
export async function readNativeMCPConfig(provider: string, home: string): Promise<RuntimeMCPConfig> {
  const normalized = provider.trim().toLowerCase();
  const candidates: Record<string, string[]> = {
    claude: [path.join(home, ".mcp.json")],
    gemini: [path.join(home, ".gemini", "settings.json")],
    opencode: [path.join(home, ".config", "opencode", "opencode.json")],
  };
  const filePath = candidates[normalized]?.[0];
  if (!filePath) return {};
  let parsed: Record<string, unknown>;
  try {
    parsed = JSON.parse(await fs.readFile(filePath, "utf-8")) as Record<string, unknown>;
  } catch {
    return {};
  }
  const source = normalized === "opencode" ? parsed.mcp : parsed.mcpServers;
  if (!source || typeof source !== "object" || Array.isArray(source)) return {};
  const mcps: Record<string, RuntimeMCPServer> = {};
  for (const [name, value] of Object.entries(source as Record<string, unknown>)) {
    if (!value || typeof value !== "object" || Array.isArray(value)) continue;
    const server = value as Record<string, unknown>;
    const command = typeof server.command === "string"
      ? server.command
      : Array.isArray(server.command) && typeof server.command[0] === "string" ? server.command[0] : undefined;
    const rest = Array.isArray(server.command)
      ? server.command.slice(1).filter((item): item is string => typeof item === "string")
      : undefined;
    const args = Array.isArray(server.args)
      ? server.args.filter((item): item is string => typeof item === "string")
      : rest;
    const url = typeof server.url === "string" ? server.url : typeof server.httpUrl === "string" ? server.httpUrl : undefined;
    if (command) {
      mcps[name] = {
        type: "local",
        command,
        ...(args && args.length ? { args } : {}),
        env: toEnvVars(server.env || server.environment),
      };
    } else if (url) {
      mcps[name] = {
        type: "remote",
        url,
        transport: typeof server.type === "string" && server.type === "sse" ? "sse" : "http",
        headers: toEnvVars(server.headers),
      };
    }
  }
  return { mcps };
}

/**
 * Resolve the effective MCP server set for a provider session.
 *
 * system-env sessions run against the node operator's real HOME, whose
 * provider-native MCP config (~/.mcp.json, ~/.gemini/settings.json, …) is read
 * here as-is and layered UNDER the task's own stateRoot config (the task's
 * freshly minted tokens) — never overwriting it, so the operator's own servers
 * stay available and the task's per-session servers land on top. Other tiers
 * return the task config unchanged (the node already wrote the native copy for
 * them, or the HOME is session-private). Codex has no per-task MCP channel in
 * system mode and is skipped (returns task config as-is).
 */
export async function resolveEffectiveMCPConfig(
  stateRoot: string,
  provider: string,
  systemEnv: boolean,
  home: string,
): Promise<Record<string, RuntimeMCPServer>> {
  const taskConfig = await readMCPConfig(stateRoot);
  const taskMcps = taskConfig.mcps || {};
  if (!systemEnv || provider.trim().toLowerCase() === "codex") {
    return taskMcps;
  }
  const nativeConfig = await readNativeMCPConfig(provider, home);
  const nativeMcps = nativeConfig.mcps || {};
  // Task tokens win on name collisions: the node wrote the task's per-session
  // config with this run's own freshly minted credentials, so they are the
  // authoritative source for any shared server name.
  return { ...nativeMcps, ...taskMcps };
}

function toEnvVars(value: unknown): Record<string, RuntimeMCPEnvVar> | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const out: Record<string, RuntimeMCPEnvVar> = {};
  for (const [key, item] of Object.entries(value as Record<string, unknown>)) {
    const inner = typeof item === "object" && item !== null && !Array.isArray(item)
      ? (item as Record<string, unknown>)
      : null;
    out[key] = { value: String(inner && "value" in inner ? inner.value : item ?? "") };
  }
  return Object.keys(out).length ? out : undefined;
}

export function flattenEnvMap(values?: Record<string, RuntimeMCPEnvVar>): Record<string, string> | undefined {
  if (!values || typeof values !== "object") {
    return undefined;
  }
  const entries = Object.entries(values)
    .filter(([key]) => key.trim() !== "")
    .map(([key, value]) => [key, String(value?.value ?? "")]);
  if (entries.length === 0) {
    return undefined;
  }
  return Object.fromEntries(entries);
}

// ── skill/plugin activation list ──────────────────────────────────────────────

const activationConfigRelativePath = path.join("agents", "activation.json");

export type RuntimeActivationConfig = {
  skills: string[];
  plugins: string[];
};

/**
 * Read the task's skill/plugin activation list from
 * stateRoot/agents/activation.json. The node writes it from applySkills /
 * applyPlugins in system mode (where installing files is forbidden — the
 * operator's HOME is shared, so selection is the only channel), and the
 * interactive runtime re-reads it every turn: the same stateRoot-file
 * hot-switch channel the MCP config uses above.
 *
 * Both arrays default to empty. A missing file (nothing was ever narrowed) and
 * an unparsable file (torn write) both yield the empty list — with a warning
 * for the latter — so a bad file can never narrow a session on its own.
 *
 * Empty-array semantics, platform rule: an empty list is the platform's "no
 * selection" encoding and means "no narrowing = full set", NEVER "activate
 * nothing". Every runner gates on length > 0, so [] and undefined are
 * identical downstream and an empty list can never zero a session out — see
 * optionsForTurn in interactive.ts for how the overlay consumes this.
 */
export async function readRuntimeActivationConfig(stateRoot: string): Promise<RuntimeActivationConfig> {
  const configPath = path.join(stateRoot, activationConfigRelativePath);
  let raw: string;
  try {
    raw = await fs.readFile(configPath, "utf-8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException)?.code === "ENOENT") {
      return { skills: [], plugins: [] };
    }
    console.warn(`[activation] failed to read ${configPath}:`, error);
    return { skills: [], plugins: [] };
  }
  try {
    const parsed = JSON.parse(raw) as Partial<RuntimeActivationConfig>;
    return {
      skills: toStringArray(parsed?.skills),
      plugins: toStringArray(parsed?.plugins),
    };
  } catch (error) {
    console.warn(`[activation] failed to parse ${configPath}:`, error);
    return { skills: [], plugins: [] };
  }
}

function toStringArray(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  return value.filter((item): item is string => typeof item === "string");
}
