import path from "node:path";
import process from "node:process";

export function stringEnv(source: NodeJS.ProcessEnv = process.env): Record<string, string> {
  return Object.fromEntries(
    Object.entries(source).filter((entry): entry is [string, string] => typeof entry[1] === "string"),
  );
}

/**
 * Env for a provider CLI spawned on behalf of ONE session.
 *
 * Provider CLIs discover their own config through HOME (codex reads
 * `$HOME/.codex/config.toml`, which is where the node writes the session's
 * managed MCP block). The runtime process itself inherits the node operator's
 * HOME, so passing `stringEnv()` straight through makes the CLI read the
 * operator's config instead of this session's — session-scoped MCP servers then
 * silently never reach the agent. Bind HOME (and USERPROFILE, which is what
 * Windows resolves the home dir from) to the session home so the CLI picks up
 * exactly the config that was prepared for it.
 */
export function sessionEnv(
  home: string,
  source: NodeJS.ProcessEnv = process.env,
): Record<string, string> {
  const env = stringEnv(source);
  const sessionHome = String(home || "").trim();
  if (!sessionHome) {
    return env;
  }
  const resolved = path.resolve(sessionHome);
  env.HOME = resolved;
  env.USERPROFILE = resolved;
  return env;
}
