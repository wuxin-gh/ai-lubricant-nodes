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
    // system-env sessions share the operator's real HOME. The runtime stateRoot
    // carries the task MCP, so never rewrite the operator's native config here;
    // it rides the per-run temp config in writeRuntimeConfig instead.
    if (this.options.systemEnv) {
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
    config.mcp = this.toOpenCodeMcp(mcps);
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
    const localPlugins = await discoverOpenCodePlugins(this.options.home, this.options.plugins);
    const hasMcp = Boolean(this.options.mcpConfig && Object.keys(this.options.mcpConfig).length > 0);
    const hasOmoConfig = Boolean(process.env.OPENCODE_AGENT_CONFIG && String(process.env.OPENCODE_AGENT_CONFIG).trim());
    // Only rewrite config when there is something to add — skills.paths, a
    // local plugin package, the OMO agent/category model slots, or (system-env
    // only) task MCP that must NOT land in the operator's own opencode.json.
    // Sessions with neither stay on the untouched base config, exactly as before.
    if (hasSkills || localPlugins.length > 0 || hasOmoConfig || (this.options.systemEnv && hasMcp)) {
      const { configPath, omoWritten } = await this.writeRuntimeConfig(
        this.baseConfigPath(process.env.OPENCODE_CONFIG),
        hasSkills,
        localPlugins,
        hasOmoConfig,
      );
      env.OPENCODE_CONFIG = configPath;
      env.AGENT_COMPOSE_OPENCODE_CONFIG = configPath;
      // OMO（oh-my-openagent）读的是 opencode 配置目录下独立的 oh-my-openagent.json，
      // 经 OPENCODE_CONFIG_DIR 定位。只有真正写了 OMO 文件才指向临时目录——否则
      // OMO 会在空目录里找不到配置，回落内置默认；操作者 HOME 的配置不受影响。
      if (omoWritten) {
        env.OPENCODE_CONFIG_DIR = path.dirname(configPath);
      }
    }
    return env;
  }

  baseConfigPath(configPath?: string): string {
    const trimmed = String(configPath || "").trim();
    return trimmed || path.join(this.options.home, ".config", "opencode", "opencode.json");
  }

  async writeRuntimeConfig(
    baseConfigPath: string,
    includeSkills: boolean,
    localPlugins: string[],
    includeOmo = false,
  ): Promise<{ configPath: string; omoWritten: boolean }> {
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
    // system-env only: the task's MCP (already merged with the operator's own
    // servers by resolveEffectiveMCPConfig) rides this per-run temp config —
    // writeMCPConfig never touched the operator's native opencode.json. The temp
    // config is derived FROM the base config, so the operator's other mcp
    // entries are preserved under the merged set.
    if (this.options.systemEnv) {
      const mcps = this.options.mcpConfig as Record<string, Record<string, unknown>> | undefined;
      if (mcps && Object.keys(mcps).length > 0) {
        config.mcp = this.toOpenCodeMcp(mcps);
      }
    }
    // 模型映射（任务侧 model_mapping → llm.extra["OPENCODE_MODELS"] → 进程 env）：
    // {平台模型id: {name, limit:{context}}}。合并而非覆盖——base 配置里可能已有
    // 操作者自己的 models 段。opencode 的 /model 菜单按这份表展示，缺了它就只能
    // 用 CLI 自带的模型列表（往往不含网关的自定义模型名）。
    const rawModels = String(process.env.OPENCODE_MODELS || "").trim();
    if (rawModels) {
      try {
        const parsed = JSON.parse(rawModels) as Record<string, unknown>;
        if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
          config.models = { ...(isRecord(config.models) ? config.models : {}), ...parsed };
        }
      } catch {
        // 非法 JSON 不阻断会话：opencode 用自带模型列表继续跑。
      }
    }
    await fs.writeFile(configPath, JSON.stringify(config, null, 2) + "\n", "utf8");
    // OMO（oh-my-openagent）按它自己的两张表（agents / categories）选模型：切换
    // 模式/任务类型时 OMO 按 agents.<名>.model / categories.<名>.model 落点。后端
    // 经 OPENCODE_AGENT_CONFIG 传整包 JSON {agents:{...}, categories:{...}}，每槽
    // {model:"agent-compose/<平台模型>", fallback_models:[...]}。直接写进临时配置
    // 目录的 oh-my-openagent.json（与 opencode.json 并存），OMO 经
    // OPENCODE_CONFIG_DIR 定位到它。system-env 档写的是临时目录而非操作者 HOME，
    // 操作者自己的 OMO 配置不受影响。
    let omoWritten = false;
    if (includeOmo) {
      const rawOmo = String(process.env.OPENCODE_AGENT_CONFIG || "").trim();
      if (rawOmo) {
        try {
          const parsed = JSON.parse(rawOmo) as unknown;
          if (isRecord(parsed) && (isRecord(parsed.agents) || isRecord(parsed.categories))) {
            const omoPath = path.join(dir, "oh-my-openagent.json");
            await fs.writeFile(omoPath, JSON.stringify(parsed, null, 2) + "\n", "utf8");
            omoWritten = true;
          }
        } catch {
          // 非法 JSON 不阻断会话：OMO 按内置默认槽表跑。
        }
      }
    }
    return { configPath, omoWritten };
  }

  toOpenCodeMcp(mcps: Record<string, Record<string, unknown>>): Record<string, unknown> {
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
    return mcp;
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
 *
 * activePlugins narrows the result: a non-empty list means only those package
 * names load this run (the env-tier activation subset); empty/undefined keeps
 * the historic "load everything present" behaviour.
 */
async function discoverOpenCodePlugins(home: string, activePlugins?: string[]): Promise<string[]> {
  const root = path.join(home, ".agents", "plugins");
  let entries: string[];
  try {
    entries = readdirSync(root);
  } catch {
    return [];
  }
  const wanted = activePlugins && activePlugins.length > 0
    ? new Set(activePlugins.map((name) => name.trim()).filter(Boolean))
    : null;
  const plugins: string[] = [];
  for (const entry of entries) {
    if (wanted && !wanted.has(entry.trim())) {
      continue;
    }
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
