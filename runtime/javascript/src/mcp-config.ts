import { readFileSync } from "node:fs";
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
 * Provider-native MCP config files, in ascending precedence order (a later file
 * overrides an earlier one). Keep in sync with the node's systemEnvMCPSources
 * (nodes/execution/systemenv.go), which enumerates the same files for the
 * console's inventory — when the two drift, the console reports a server the
 * runtime never loads (or vice versa) and the mismatch is invisible from either
 * side.
 *
 * claude reads two files, and both must be here. `~/.claude.json` is where
 * `claude mcp add` writes (top-level `mcpServers`); `~/.mcp.json` is the
 * project-level file. Reading only the latter left every server registered
 * through the CLI's own command invisible to a system-env session, even though
 * the console listed it as available.
 */
function nativeMCPFiles(provider: string, home: string): { file: string; field: string }[] {
  const normalized = provider.trim().toLowerCase();
  if (normalized === "claude") {
    return [
      { file: path.join(home, ".claude.json"), field: "mcpServers" },
      { file: path.join(home, ".mcp.json"), field: "mcpServers" },
    ];
  }
  if (normalized === "gemini") {
    return [{ file: path.join(home, ".gemini", "settings.json"), field: "mcpServers" }];
  }
  if (normalized === "opencode") {
    // opencode keeps its MCP map under the top-level `mcp` key, not `mcpServers`.
    return [{ file: path.join(home, ".config", "opencode", "opencode.json"), field: "mcp" }];
  }
  return [];
}

/**
 * Read a provider's native MCP config from a system HOME, read-only, and
 * normalize it into the runtime's own RuntimeMCPServer shape. Only the
 * providers whose CLI reads a JSON config file are covered; codex's native
 * MCP lives in TOML (~/.codex/config.toml) and is deliberately not parsed
 * here — it has no task-level MCP channel in system mode.
 */
export async function readNativeMCPConfig(provider: string, home: string): Promise<RuntimeMCPConfig> {
  const files = nativeMCPFiles(provider, home);
  // A provider with no known config file yields `{}` (no `mcps` key) rather than
  // an empty map, so callers can tell "this provider has no native config
  // channel" from "it has one and it is empty".
  if (files.length === 0) return {};
  const mcps: Record<string, RuntimeMCPServer> = {};
  for (const { file, field } of files) {
    Object.assign(mcps, await readMCPServersFromFile(file, field));
  }
  return { mcps };
}

/**
 * Read the server map out of one JSON config file. A missing, unreadable, or
 * malformed file yields an empty map rather than throwing: an operator's own
 * config must never be able to fail a task session, and every caller merges the
 * result under the task's own servers anyway.
 */
async function readMCPServersFromFile(file: string, field: string): Promise<Record<string, RuntimeMCPServer>> {
  let parsed: Record<string, unknown>;
  try {
    parsed = JSON.parse(await fs.readFile(file, "utf-8")) as Record<string, unknown>;
  } catch {
    return {};
  }
  const source = parsed[field];
  if (!source || typeof source !== "object" || Array.isArray(source)) return {};
  const mcps: Record<string, RuntimeMCPServer> = {};
  for (const [name, value] of Object.entries(source as Record<string, unknown>)) {
    const server = normalizeMCPServer(value);
    if (server) mcps[name] = server;
  }
  return mcps;
}

/**
 * Project one raw JSON MCP entry onto the runtime's server shape, or null when
 * it names neither a command nor a url (a config file may legitimately carry
 * keys that are not servers).
 */
function normalizeMCPServer(value: unknown): RuntimeMCPServer | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
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
    return {
      type: "local",
      command,
      ...(args && args.length ? { args } : {}),
      env: toEnvVars(server.env || server.environment),
    };
  }
  if (url) {
    return {
      type: "remote",
      url,
      transport: typeof server.type === "string" && server.type === "sse" ? "sse" : "http",
      headers: toEnvVars(server.headers),
    };
  }
  return null;
}

/**
 * Read the MCP servers a plugin package declares — from its `.mcp.json` (the
 * conventional file) and its `.claude-plugin/plugin.json` manifest
 * (`mcpServers`). Both are merged; a name declared in both takes the manifest's
 * value, matching the CLI's own precedence.
 *
 * Why the runtime resolves these instead of letting the CLI do it: the session
 * runs with `strictMcpConfig`, which makes the CLI ignore every MCP source
 * except the ones passed explicitly — including plugins. That flag is load
 * bearing (it is what stops a task's cloned repo from shipping a `.mcp.json`
 * that injects servers into the agent), so it cannot simply be turned off. The
 * CLI's own answer is `skipMcpDiscovery`: the host owns the plugin's MCP
 * connections and passes them in. That is what happens here.
 *
 * `${CLAUDE_PLUGIN_ROOT}` / `${PLUGIN_ROOT}` expand to the plugin's own
 * directory, which is what the CLI would substitute when it loads the plugin
 * itself. `${VAR:-default}` forms are left verbatim for the command's own shell
 * — several plugins ship a self-resolving `sh -c` wrapper that finds its own
 * root at run time, and rewriting those is both unnecessary and unsafe.
 */
export function readPluginMCPServers(pluginDirs: string[]): Record<string, RuntimeMCPServer> {
  const out: Record<string, RuntimeMCPServer> = {};
  for (const dir of pluginDirs) {
    for (const [name, value] of Object.entries(readPluginDeclaredServers(dir))) {
      const server = normalizeMCPServer(value);
      if (server) {
        out[name] = expandPluginPaths(server, dir);
      }
    }
  }
  return out;
}

function readPluginDeclaredServers(pluginDir: string): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const files: [string, string][] = [
    [path.join(pluginDir, ".mcp.json"), "mcpServers"],
    [path.join(pluginDir, ".claude-plugin", "plugin.json"), "mcpServers"],
  ];
  for (const [file, field] of files) {
    let parsed: Record<string, unknown>;
    try {
      parsed = JSON.parse(readFileSync(file, "utf-8")) as Record<string, unknown>;
    } catch {
      // A plugin without this file, or with a malformed one, declares no servers
      // here; the other file may still carry them. Neither may fail the session.
      continue;
    }
    const servers = parsed[field];
    if (servers && typeof servers === "object" && !Array.isArray(servers)) {
      Object.assign(out, servers as Record<string, unknown>);
    }
  }
  return out;
}

function expandPluginPaths(server: RuntimeMCPServer, pluginRoot: string): RuntimeMCPServer {
  const expand = (value: string): string => expandPluginRoot(value, pluginRoot);
  const expandVars = (
    vars?: Record<string, RuntimeMCPEnvVar>,
  ): Record<string, RuntimeMCPEnvVar> | undefined => {
    if (!vars) return undefined;
    const next: Record<string, RuntimeMCPEnvVar> = {};
    for (const [key, item] of Object.entries(vars)) {
      next[key] = { ...item, value: expand(item.value) };
    }
    return next;
  };
  return {
    ...server,
    ...(server.command ? { command: expand(server.command) } : {}),
    ...(server.args ? { args: server.args.map(expand) } : {}),
    ...(server.url ? { url: expand(server.url) } : {}),
    ...(server.headers ? { headers: expandVars(server.headers) } : {}),
    // CLAUDE_PLUGIN_ROOT is injected into the server's own environment, which is
    // what the CLI does when IT loads a plugin's MCP. It matters for the same
    // reason it matters there: a plugin package that self-resolves its root
    // (claude-mem's `sh -c` wrapper is the canonical case) reads this variable,
    // and its fallback search path points at the operator's ~/.claude, which an
    // isolated session's synced copy does not live under. Setting it here means
    // the wrapper's own `${CLAUDE_PLUGIN_ROOT:-…}` resolves to the right place
    // without this code rewriting a single line of the script.
    //
    // Only local (stdio) servers get it: a remote server has no child process to
    // read the variable, and its `env` block is a flattened header map, so
    // planting paths there would ship them as HTTP headers.
    ...(server.type === "local" ? {
      env: {
        ...expandVars(server.env),
        CLAUDE_PLUGIN_ROOT: { value: pluginRoot },
        PLUGIN_ROOT: { value: pluginRoot },
      },
    } : {}),
  };
}

/**
 * Expand the bare `${CLAUDE_PLUGIN_ROOT}` / `${PLUGIN_ROOT}` / `$CLAUDE_PLUGIN_ROOT`
 * forms to the plugin's own directory. Anything carrying a shell default
 * (`${VAR:-…}`) is deliberately left untouched: those appear inside `sh -c`
 * wrappers that resolve their own root at run time, and the nested braces make
 * any regex rewrite unsafe — a half-replaced `${A:-${B:-}}` corrupts the script.
 * Leaving them verbatim lets the shell's own fallback do its job.
 */
function expandPluginRoot(value: string, pluginRoot: string): string {
  if (!pluginRoot || !value.includes("PLUGIN_ROOT")) return value;
  return value
    .replace(/\$\{(?:CLAUDE_)?PLUGIN_ROOT\}/g, pluginRoot)
    .replace(/\$(?:CLAUDE_)?PLUGIN_ROOT\b/g, pluginRoot);
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
