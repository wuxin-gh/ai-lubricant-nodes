import { spawn } from "node:child_process";
import { existsSync, readdirSync, statSync } from "node:fs";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import readline from "node:readline";
import { bindAbortToChild } from "../abort.js";
import { formatError, TurnCancelledError } from "../errors.js";
import { readStoredThread, writeStoredThread } from "../session-state.js";
import { extractText, jsonString } from "../text.js";
import { TranscriptWriter } from "../transcript.js";
import type { AgentResult, RunnerOptions, StoredThread } from "../types.js";
import { flattenEnvMap } from "../mcp-config.js";
import { resolveOpenCodeSkipPermissions } from "./mode.js";

export class OpenCodeRunner {
  private readonly writer = new TranscriptWriter();
  private skillsConfigDir?: string;

  constructor(private readonly options: RunnerOptions) {}

  async writeMCPConfig(): Promise<void> {
    const mcps = this.options.mcpConfig as Record<string, Record<string, unknown>> | undefined;
    if (!mcps || Object.keys(mcps).length === 0) {
      return;
    }
    const configPath = process.env.OPENCODE_CONFIG || path.join(this.options.home, ".config", "opencode", "opencode.json");
    await fs.mkdir(path.dirname(configPath), { recursive: true });
    let config: Record<string, unknown> = {};
    try {
      config = JSON.parse(await fs.readFile(configPath, "utf-8"));
    } catch {
      config = {};
    }
    const mcp: Record<string, unknown> = {};
    for (const [name, server] of Object.entries(mcps)) {
      if (server.type === "local") {
        mcp[name] = {
          type: "local",
          command: [server.command, ...(Array.isArray(server.args) ? server.args : [])],
          environment: flattenEnvMap(server.env as Record<string, { value: string }> | undefined),
        };
      } else if (server.type === "remote") {
        mcp[name] = {
          type: "remote",
          url: server.url,
          headers: flattenEnvMap(server.headers as Record<string, { value: string }> | undefined),
        };
      }
    }
    config.mcp = mcp;
    await fs.writeFile(configPath, JSON.stringify(config, null, 2) + "\n", "utf-8");
  }

  buildArgs(promptText: string, stored: StoredThread | null): string[] {
    const userPrompt = this.options.systemContext
      ? `${this.options.systemContext}\n\n${promptText}`
      : promptText;
    const args = [
      "run",
      userPrompt,
      "--format", "json",
      "--dir", this.options.workspace,
    ];
    if (resolveOpenCodeSkipPermissions(this.options.mode)) {
      args.push("--dangerously-skip-permissions");
    }
    const model = String(this.options.model || "").trim();
    if (model) {
      args.push("--model", model);
    }
    if (stored?.threadId) {
      args.push("--session", stored.threadId);
    }
    return args;
  }

  async environment(): Promise<NodeJS.ProcessEnv> {
    const env: NodeJS.ProcessEnv = {
      ...process.env,
      OPENCODE_DISABLE_AUTOUPDATE: process.env.OPENCODE_DISABLE_AUTOUPDATE || "true",
      OPENCODE_DISABLE_MODELS_FETCH: process.env.OPENCODE_DISABLE_MODELS_FETCH || "1",
    };
    const hasSkills = Boolean(this.options.skills && this.options.skills.length > 0);
    const localPlugins = await discoverOpenCodePlugins(this.options.home);
    // Only rewrite config when there is something to add — skills.paths or a
    // local plugin package. Sessions with neither stay on the untouched base
    // config, exactly as before.
    if (hasSkills || localPlugins.length > 0) {
      const configPath = await this.writeRuntimeConfig(
        this.baseConfigPath(process.env.OPENCODE_CONFIG),
        hasSkills,
        localPlugins,
      );
      env.OPENCODE_CONFIG = configPath;
      env.AGENT_COMPOSE_OPENCODE_CONFIG = configPath;
    }
    return env;
  }

  baseConfigPath(configPath?: string): string {
    const trimmed = String(configPath || "").trim();
    return trimmed || path.join(this.options.home, ".config", "opencode", "opencode.json");
  }

  async writeRuntimeConfig(baseConfigPath: string, includeSkills: boolean, localPlugins: string[]): Promise<string> {
    await this.cleanupSkillsConfig();
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), "agent-compose-opencode-"));
    this.skillsConfigDir = dir;
    const configPath = path.join(dir, "opencode.json");
    const config = await readOpenCodeConfig(baseConfigPath);
    if (includeSkills) {
      // OpenCode discovers loose SKILL.md dirs from skills.paths; point it at the
      // node's per-session skills mirror (home/.agents/skills).
      const skillsRoot = path.join(this.options.home, ".agents", "skills");
      const existingSkills = isRecord(config.skills) ? config.skills : {};
      const existingPaths = Array.isArray(existingSkills.paths)
        ? existingSkills.paths.filter((value): value is string => typeof value === "string" && value.trim() !== "")
        : [];
      config.skills = {
        ...existingSkills,
        paths: uniqueStrings([...existingPaths, skillsRoot]),
      };
    }
    if (localPlugins.length > 0) {
      // OpenCode's `plugin` array accepts a local package directory; each synced
      // package (with a package.json) registers its own skills/commands through
      // OpenCode's plugin manager. See obra/superpowers .opencode/INSTALL.md.
      const existingPlugins = Array.isArray(config.plugin)
        ? config.plugin.filter((value): value is string => typeof value === "string" && value.trim() !== "")
        : [];
      config.plugin = uniqueStrings([...existingPlugins, ...localPlugins]);
    }
    await fs.writeFile(configPath, JSON.stringify(config, null, 2) + "\n", "utf8");
    return configPath;
  }

  async cleanupSkillsConfig(): Promise<void> {
    const dir = this.skillsConfigDir;
    this.skillsConfigDir = undefined;
    if (!dir) {
      return;
    }
    try {
      await fs.rm(dir, { recursive: true, force: true });
    } catch (error) {
      this.writer.line(`[opencode cleanup] ${formatError(error)}`);
    }
  }

  handleEvent(event: Record<string, unknown>, result: AgentResult): void {
    const providerThreadID = stringField(event, "sessionID", "sessionId", "session_id");
    if (providerThreadID) {
      result.threadId = providerThreadID;
    }

    const eventType = String(event.type || event.event || "");
    if (eventType === "error") {
      const errorText = extractText(event.error) || extractText(event.message) || jsonString(event);
      this.writer.line(errorText);
      throw new Error(errorText);
    }

    if (eventType === "tool_use" || eventType === "tool") {
      const tool = event.tool as Record<string, unknown> | undefined;
      const toolName = stringField(event, "name", "toolName") || String(tool?.name || "tool");
      this.writer.line(`\n[tool:${toolName}]`);
      return;
    }

    if (eventType === "tool_result") {
      const text = extractText(event.result) || extractText(event.content) || jsonString(event.result || event);
      if (text.trim()) {
        this.writer.line(text);
      }
      return;
    }

    const text = extractText(event.message) ||
      extractText(event.content) ||
      extractText(event.part) ||
      extractText(event.text) ||
      extractText(event.delta);
    if (text) {
      this.writer.write(text);
    }

    if (eventType === "result" || eventType === "complete" || eventType === "completed") {
      const finalText = extractText(event.response) || extractText(event.result) || text;
      if (finalText) {
        result.finalText = finalText;
      }
      result.stopReason = stringField(event, "stopReason", "stop_reason", "finishReason", "finish_reason") || result.stopReason;
    }
  }

  async runPrompt(promptText: string): Promise<AgentResult> {
    await this.writeMCPConfig();
    if (this.options.outputSchema) {
      throw new Error("structured JSON output is not supported by opencode runner");
    }

    const stored = await readStoredThread(this.options.stateRoot, "opencode", this.options.sessionScope);
    const result: AgentResult = {
      provider: "opencode",
      threadId: stored?.threadId || "",
      stopReason: "completed",
      finalText: "",
      transcript: "",
      stderr: "",
    };

    try {
      const child = spawn("opencode", this.buildArgs(promptText, stored), {
        cwd: this.options.workspace,
        env: await this.environment(),
        stdio: ["ignore", "pipe", "pipe"],
      });
      const detachAbort = bindAbortToChild(child, this.options.abortController);

      const stderrChunks: string[] = [];
      child.stderr?.on("data", (chunk) => {
        const text = String(chunk || "");
        stderrChunks.push(text);
        this.writer.write(text);
      });

      const rl = readline.createInterface({ input: child.stdout, crlfDelay: Infinity });
      try {
        for await (const line of rl) {
          if (!line.trim()) {
            continue;
          }
          let event: Record<string, unknown>;
          try {
            event = JSON.parse(line) as Record<string, unknown>;
          } catch {
            this.writer.line(line);
            continue;
          }
          this.handleEvent(event, result);
        }
      } catch (error) {
        child.kill("SIGTERM");
        throw error;
      }

      const exitCode = await new Promise<number>((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", (code) => resolve(code ?? 1));
      });
      detachAbort();
      if (exitCode !== 0) {
        if (this.options.abortController?.signal.aborted) {
          throw new TurnCancelledError("opencode turn cancelled");
        }
        throw new Error(`opencode exited with code ${exitCode}: ${stderrChunks.join("")}`);
      }
    } finally {
      await this.cleanupSkillsConfig();
    }

    result.transcript = this.writer.transcript();
    if (!result.finalText && result.transcript) {
      result.finalText = result.transcript;
    }
    await writeStoredThread(this.options.stateRoot, "opencode", result.threadId, undefined, this.options.sessionScope);
    return result;
  }
}

function stringField(record: Record<string, unknown>, ...keys: string[]): string {
  for (const key of keys) {
    const value = record[key];
    if (typeof value === "string" && value.trim()) {
      return value.trim();
    }
  }
  return "";
}

async function readOpenCodeConfig(configPath?: string): Promise<Record<string, unknown>> {
  const trimmed = String(configPath || "").trim();
  if (!trimmed) {
    return {};
  }
  try {
    const content = await fs.readFile(trimmed, "utf8");
    const parsed = JSON.parse(content) as unknown;
    return isRecord(parsed) ? parsed : {};
  } catch (error) {
    const cause = error as NodeJS.ErrnoException;
    if (cause.code === "ENOENT") {
      return {};
    }
    throw error;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function uniqueStrings(values: string[]): string[] {
  return Array.from(new Set(values));
}

/**
 * Discover local plugin packages the node laid down under home/.agents/plugins.
 * OpenCode's `plugin` array accepts a local package directory; a package with a
 * package.json registers its skills/commands through OpenCode's plugin manager
 * (see obra/superpowers .opencode/INSTALL.md). Returns absolute paths; a missing
 * dir yields an empty list.
 */
async function discoverOpenCodePlugins(home: string): Promise<string[]> {
  const root = path.join(home, ".agents", "plugins");
  let entries: string[];
  try {
    entries = readdirSync(root);
  } catch {
    return [];
  }
  const plugins: string[] = [];
  for (const entry of entries) {
    const dir = path.join(root, entry);
    try {
      if (!statSync(dir).isDirectory()) continue;
    } catch {
      continue;
    }
    if (existsSync(path.join(dir, "package.json"))) {
      plugins.push(dir);
    }
  }
  return plugins;
}
