import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { spawnSync } from "node:child_process";
import process from "node:process";
import { basename, join } from "node:path";
import { flattenEnvMap, readPluginMCPServers } from "../mcp-config.js";
import { resolveClaudeMode } from "./mode.js";
import { uniqueDirectories } from "../paths.js";
import { readStoredThread, writeStoredThread } from "../session-state.js";
import { UpstreamStatusError } from "../retry.js";
import { jsonString } from "../text.js";
import { TranscriptWriter } from "../transcript.js";
import { gitStashGuardHook } from "../workspace-guard.js";
import type { AgentResult, RunnerOptions, StoredThread } from "../types.js";

type PendingToolUse = {
  name: string;
  partialJson: string;
};

/**
 * One normalized conversation item, the shared vocabulary across providers.
 *
 * Canonical envelope fields (all optional for compatibility):
 * - `logical_event_id`: stable lifecycle id — sparse completion frames reuse the
 *   opening frame's id so downstream can merge instead of guessing.
 * - `tool_name`: canonical tool name (Agent/Edit/Bash/…). Present on BOTH the
 *   opening and completion frames of a tool call.
 * - `subagent_id`: empty = root/main transcript; non-empty = sub-agent detail
 *   ONLY. This is the single routing rule downstream.
 * - `phase`: start|update|complete|error.
 */
type StreamItem = Record<string, unknown> & { id: string; type: string };

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function textOf(value: unknown): string {
  return typeof value === "string" ? value : "";
}

/**
 * One human-readable line describing what a tool call is doing.
 *
 * The console shows this as the timeline node's caption, so the shape is "the
 * object this step acts on" rather than a sentence: a file path for Read/Edit,
 * the command for Bash, the pattern for Grep/Glob. Claude's own `description`
 * parameter (present on Bash) is preferred when set — it is the model's own
 * statement of intent and reads better than anything derived from the input.
 *
 * Returns undefined when nothing useful can be derived, so the field is simply
 * absent rather than an empty string the UI would have to special-case.
 */
function toolStepSummary(toolName: string, input: Record<string, unknown>): string | undefined {
  const description = textOf(input.description);
  if (description) return description;
  const path = textOf(input.file_path) || textOf(input.notebook_path);
  const pattern = textOf(input.pattern);
  switch (toolName) {
    case "Read":
    case "Edit":
    case "MultiEdit":
    case "Write":
    case "NotebookEdit":
      return path || undefined;
    case "Bash":
      return textOf(input.command) || undefined;
    case "Grep":
    case "Glob":
      return pattern ? (path ? `${pattern} · ${path}` : pattern) : undefined;
    case "WebFetch":
      return textOf(input.url) || undefined;
    case "WebSearch":
      return textOf(input.query) || undefined;
    case "Task":
      return textOf(input.description) || undefined;
    default:
      return undefined;
  }
}

/**
 * Project a Claude Agent-SDK message onto zero or more normalized items.
 *
 * Mapping, by SDK message type:
 * - `assistant`: each content block becomes its own item — `text` blocks are
 *   `agent_message`, `thinking` blocks are `reasoning`, `tool_use` blocks are a
 *   `tool_call` carrying the tool's input. Block ids come from the tool_use id or
 *   the message uuid plus the block index, so a re-sent block updates in place
 *   rather than appending a duplicate.
 * - `user`: the SDK reports tool results here. Each `tool_result` block updates
 *   the matching `tool_call` (same id as the originating tool_use) with its
 *   output, so a tool call and its result stay one entry.
 * - `result`: never produces an item. A success repeats text the assistant blocks
 *   already carried; a failure is raised as an UpstreamStatusError and surfaces as
 *   the stream's own `error` frame. Emitting either would duplicate it.
 * - `stream_event`: never emitted. These are per-token deltas; the completed
 *   assistant message carries the full text.
 */
function claudeMessageToItems(
  message: Record<string, unknown>,
  toolNamesById?: Map<string, string>,
): StreamItem[] {
  const msgType = textOf(message.type);
  const uuid = textOf(message.uuid) || "msg";

  if (msgType === "assistant") {
    const inner = isRecord(message.message) ? message.message : message;
    const content = Array.isArray(inner.content) ? inner.content : [];
    const items: StreamItem[] = [];
    // The SDK's assistant API message carries the model and per-call usage on
    // every assistant message. Stamp them onto each block item so the console
    // can show "model · N tokens" next to a message time without a separate
    // usage frame (the context-gauge usage_update carries only window fill).
    const model = textOf(inner.model) || undefined;
    const usage = assistantUsage(inner);
    content.forEach((rawBlock, index) => {
      if (!isRecord(rawBlock)) return;
      const blockType = textOf(rawBlock.type);
      if (blockType === "text") {
        const text = textOf(rawBlock.text);
        if (text) items.push({
          id: `${uuid}:${index}`,
          type: "agent_message",
          text,
          logical_event_id: `${uuid}:${index}`,
          event_kind: "message",
          phase: "complete",
          status: "done",
          ...(model ? { model } : {}),
          ...(usage ? { usage } : {}),
        });
        return;
      }
      if (blockType === "thinking") {
        const text = textOf(rawBlock.thinking) || textOf(rawBlock.text);
        if (text) items.push({
          id: `${uuid}:${index}`,
          type: "reasoning",
          text,
          logical_event_id: `${uuid}:${index}`,
          event_kind: "reasoning",
          phase: "complete",
          status: "done",
          ...(model ? { model } : {}),
          ...(usage ? { usage } : {}),
        });
        return;
      }
      if (blockType === "tool_use") {
        const id = textOf(rawBlock.id) || `${uuid}:${index}`;
        const toolName = textOf(rawBlock.name) || "工具调用";
        toolNamesById?.set(id, toolName);
        const input = isRecord(rawBlock.input) ? rawBlock.input : {};
        // One human-readable line for what this step is doing. The console renders
        // it as the timeline node's caption; without it every step reads as a bare
        // tool name and the reader has to expand the card to learn anything.
        // Derived here (the layer that knows the provider's tool vocabulary)
        // rather than in the UI, which would need a table per provider.
        const summary = toolStepSummary(toolName, input);
        items.push({
          id,
          type: "tool_call",
          title: toolName,
          tool_name: toolName,
          logical_event_id: id,
          event_kind: "tool",
          phase: "start",
          input,
          status: "running",
          ...(summary ? { summary } : {}),
          ...(model ? { model } : {}),
          ...(usage ? { usage } : {}),
        });
      }
    });
    return items;
  }

  if (msgType === "user") {
    const inner = isRecord(message.message) ? message.message : message;
    const content = Array.isArray(inner.content) ? inner.content : [];
    const items: StreamItem[] = [];
    for (const rawBlock of content) {
      if (!isRecord(rawBlock) || textOf(rawBlock.type) !== "tool_result") continue;
      const toolUseId = textOf(rawBlock.tool_use_id);
      if (!toolUseId) continue;
      // The completion frame is sparse by design — but it MUST still carry the
      // canonical routing fields (logical_event_id + tool_name), otherwise a
      // downstream consumer that missed the opening frame cannot route it.
      // The tool name is recovered from the opening frame's toolNamesById.
      const toolName = toolNamesById?.get(toolUseId) || "";
      const isError = rawBlock.is_error === true;
      items.push({
        id: toolUseId,
        type: "tool_call",
        title: toolName,
        tool_name: toolName,
        logical_event_id: toolUseId,
        event_kind: "tool",
        phase: isError ? "error" : "complete",
        output: rawBlock.content ?? "",
        status: isError ? "failed" : "done",
      });
    }
    return items;
  }

  // A failed `result` is NOT turned into an error item. `runPrompt` throws an
  // UpstreamStatusError on the very same message, and the stream layer turns that
  // throw into a top-level `error` frame — so emitting an item here reported the
  // one failure twice: once as an error bubble, once as the retryable error card,
  // with identical text ("API Error: ... 429 ..."). The throw is the canonical
  // path (it carries the upstream status code), so this stays silent.
  return [];
}

/**
 * Project the SDK's `system` messages onto the runtime's own out-of-band frames.
 *
 * These are not conversation content, so they must not become `item`s — but they
 * carry the two things a user most needs to see when a turn appears stuck:
 * that the SDK is retrying a rate-limited call, and that it paused to compact
 * the context. The SDK already does both; the runtime used to drop every
 * `type:'system'` message on the floor, so from the outside a 30s retry looked
 * identical to a hang.
 *
 * Returns the frame to emit, or null for system messages with no consumer.
 */
function claudeSystemFrame(message: Record<string, unknown>): { type: string; fields: object } | null {
  switch (textOf(message.subtype)) {
    case "api_retry": {
      // SDKAPIRetryMessage: the SDK is about to retry a failed API call.
      // error_status is null for connection errors that never got a response.
      const error = isRecord(message.error) ? message.error : {};
      return {
        type: "llm_call_retry",
        fields: {
          attempt: Number(message.attempt || 0),
          max_retries: Number(message.max_retries || 0),
          retry_delay_ms: Number(message.retry_delay_ms || 0),
          error_status: typeof message.error_status === "number" ? message.error_status : null,
          message: textOf(error.message) || textOf(message.error) || "模型调用失败",
        },
      };
    }
    case "compact_boundary": {
      // SDKCompactBoundaryMessage: compaction finished. `trigger` distinguishes
      // the SDK's own threshold-driven pass from a user-typed /compact.
      const meta = isRecord(message.compact_metadata) ? message.compact_metadata : {};
      return {
        type: "compact_status",
        fields: {
          status: "ended",
          trigger: textOf(meta.trigger) || "auto",
          pre_tokens: Number(meta.pre_tokens || 0),
          post_tokens: Number(meta.post_tokens || 0),
        },
      };
    }
    case "status": {
      // SDKStatusMessage: `compacting` is the only status worth surfacing — it is
      // the leading edge of a compact_boundary, so the UI can say "compressing"
      // while it runs instead of only reporting it after the fact.
      if (textOf(message.status) !== "compacting") return null;
      return { type: "compact_status", fields: { status: "started", trigger: "auto" } };
    }
    case "task_started":
    case "task_progress":
    case "task_notification": {
      // Background/Task lifecycle (subagents). The Agent tool launches them
      // async: the root turn's `result` arrives while the child is still
      // running. These frames carry the id pair the UI needs to keep the
      // sub-agent card's spinner honest and to gate turn completion on real
      // child termination instead of the root's early exit.
      const subtype = textOf(message.subtype);
      const toolUseId = textOf(message.tool_use_id);
      const status = subtype === "task_notification"
        ? textOf(message.status) || "completed"
        : subtype === "task_started" ? "started" : "progress";
      return {
        type: "subagent_status",
        fields: {
          task_id: textOf(message.task_id),
          tool_use_id: toolUseId,
          subagent_id: toolUseId,
          status,
          description: textOf(message.description),
          subagent_type: textOf(message.subagent_type),
          ...(message.summary !== undefined ? { summary: textOf(message.summary) } : {}),
        },
      };
    }
    case "background_tasks_changed": {
      // Level signal: the full set of live background tasks after a change.
      // REPLACE semantics; consumed by the runner to decide when closing the
      // SDK query is safe (see runPrompt).
      const tasks = Array.isArray(message.tasks) ? message.tasks : [];
      return {
        type: "background_tasks",
        fields: { count: tasks.length },
      };
    }
    default:
      return null;
  }
}

/**
 * Extract the turn's context-window occupancy from a `result` message.
 *
 * `modelUsage[model].contextWindow` is the window size and the input tokens are
 * what currently fills it; without this pair the client's context gauge has no
 * data source at all (the runtime never emitted usage, so the "context is
 * filling up" hint in the UI could never fire). Cache reads count toward the
 * window, so they are included.
 */
function claudeUsageFrame(message: Record<string, unknown>): object | null {
  const modelUsage = isRecord(message.modelUsage) ? message.modelUsage : null;
  if (!modelUsage) return null;
  let used = 0;
  let size = 0;
  for (const entry of Object.values(modelUsage)) {
    if (!isRecord(entry)) continue;
    used += Number(entry.inputTokens || 0)
      + Number(entry.cacheReadInputTokens || 0)
      + Number(entry.cacheCreationInputTokens || 0)
      + Number(entry.outputTokens || 0);
    size = Math.max(size, Number(entry.contextWindow || 0));
  }
  if (used <= 0 && size <= 0) return null;
  return { used, size };
}

/**
 * Compact a Claude assistant message's per-call API usage into the shape the
 * console stamps onto items. The SDK reports Anthropic API `usage` (snake_case
 * `input_tokens` etc.); we normalise to short field names and a pre-summed
 * total so the frontend renders without knowing the SDK's naming. Returns
 * undefined when the message carries no usable numbers (assistant prefill
 * fragments, or older SDK shapes without usage).
 */
function assistantUsage(inner: Record<string, unknown>):
  | { input: number; output: number; cache_read: number; cache_creation: number; total: number }
  | undefined {
  const usageRaw = isRecord(inner.usage) ? inner.usage : null;
  if (!usageRaw) return undefined;
  const input = Number(usageRaw.input_tokens ?? usageRaw.inputTokens ?? 0);
  const output = Number(usageRaw.output_tokens ?? usageRaw.outputTokens ?? 0);
  const cacheRead = Number(usageRaw.cache_read_input_tokens ?? usageRaw.cacheReadInputTokens ?? 0);
  const cacheCreation = Number(usageRaw.cache_creation_input_tokens ?? usageRaw.cacheCreationInputTokens ?? 0);
  if (!input && !output && !cacheRead && !cacheCreation) return undefined;
  return { input, output, cache_read: cacheRead, cache_creation: cacheCreation, total: input + output + cacheRead + cacheCreation };
}

function hasOwn(object: object, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(object, key);
}

function contentBlockKey(event: Record<string, unknown>, fallback = ""): string {
  for (const key of ["index", "content_block_index", "contentBlockIndex"]) {
    const value = event[key];
    if (typeof value === "string" || typeof value === "number") {
      return String(value);
    }
  }
  return fallback;
}

function claudeExecutable(): string | undefined {
  const configured = process.env.CLAUDE_CODE_EXECUTABLE || process.env.CLAUDE_CODE_PATH;
  if (configured && existsSync(configured)) {
    return configured;
  }

  const candidates = process.platform === "win32"
    ? [
        process.env.ProgramW6432 ? join(process.env.ProgramW6432, "nodejs", "claude.exe") : "",
        process.env.APPDATA ? join(process.env.APPDATA, "npm", "node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe") : "",
        process.env.LOCALAPPDATA ? join(process.env.LOCALAPPDATA, "npm", "node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe") : "",
      ]
    : ["/usr/bin/claude", "/usr/local/bin/claude"];
  for (const candidate of candidates) {
    if (candidate && existsSync(candidate)) {
      return candidate;
    }
  }

  // npm global installs are commonly exposed through a platform shim. Resolve
  // the real executable from PATH so the SDK never falls back to its optional
  // platform package, which is intentionally omitted from runtime archives.
  const command = process.platform === "win32" ? "where.exe" : "sh";
  const args = process.platform === "win32" ? ["claude.exe"] : ["-lc", "command -v claude || true"];
  const probe = spawnSync(command, args, { encoding: "utf8", windowsHide: true });
  const resolved = String(probe.stdout || "")
    .split(/\r?\n/)
    .map((line) => line.trim())
    .find(Boolean);
  return resolved && existsSync(resolved) ? resolved : undefined;
}

function claudeEnvironment(): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...process.env, IS_SANDBOX: "1" };
  // The LLM_* snapshot is the task's per-turn source of truth. Unconditionally
  // overwrite any system-level ANTHROPIC_* residue: the previous "only fill if
  // missing" fallback let stale machine-global credentials win, silently
  // repointing the session at the wrong endpoint/key.
  if (env.LLM_API_KEY) {
    env.ANTHROPIC_API_KEY = env.LLM_API_KEY;
  }
  if (env.LLM_API_ENDPOINT) {
    env.ANTHROPIC_BASE_URL = env.LLM_API_ENDPOINT;
  }
  return env;
}

/**
 * claudeSettingsLlmEnv lifts the per-turn LLM snapshot (the LLM_* vars the
 * node's llmEnv / the runtime's plantLlmEnv plant into process.env) into the
 * SDK ``settings`` env block. Settings env outranks user settings.json —
 * which is the ONLY way to beat an operator-managed ~/.claude/settings.json
 * in system-env mode: that file is the operator's real HOME config, tools
 * like ccswitch write their own env block (endpoint + key) into it, and
 * settings.json env overrides process env — so without this every task turn
 * was silently re-pointed at the operator's proxy and billed to their key
 * instead of the task's. The ANTHROPIC_* trio stays empty when no LLM
 * snapshot is planted, so native-login sessions keep working untouched.
 *
 * Scope: settings env only covers TASK-SEMANTIC keys (endpoint/key/model plus
 * task-privacy switches). Every other operator settings.json key passes
 * through untouched. The DISABLE_* / NONESSENTIAL_TRAFFIC switches are pinned
 * here with fixed values so an operator's settings.json env block cannot
 * re-enable telemetry for task turns — task turns must never leak the
 * operator's HOME context.
 */
/**
 * claudeSettingsLlmEnv returns the settings-env block that pins this turn's LLM
 * snapshot above the session HOME's own settings.json.
 *
 * **Shape matters**: the SDK's `Settings` type puts environment variables under
 * a nested `env` key (`settings.env`), matching settings.json's own layout. This
 * used to return a FLAT map that was spread onto the settings root
 * (`settings: { ...claudeSettingsLlmEnv() }`), which made every entry an unknown
 * top-level settings field — the SDK ignores those silently, so the override
 * never happened. In system-env mode (HOME = the operator's real HOME) the CLI
 * then read `~/.claude/settings.json`'s `env` block, whose ANTHROPIC_AUTH_TOKEN
 * outranks the process env the node planted, and every task turn authenticated
 * with the operator's key instead of the task's child key.
 *
 * Returned as `{ env: {...} }` so the caller spreads it at the settings root
 * where the SDK expects to find it.
 */
export function claudeSettingsLlmEnv(): { env: Record<string, string> } {
  const endpoint = String(process.env.LLM_API_ENDPOINT || "").trim();
  const key = String(process.env.LLM_API_KEY || "").trim();
  const model = String(process.env.LLM_MODEL || "").trim();
  const env: Record<string, string> = {};
  if (endpoint) env.ANTHROPIC_BASE_URL = endpoint;
  if (key) {
    env.ANTHROPIC_API_KEY = key;
    env.ANTHROPIC_AUTH_TOKEN = key;
  }
  if (model) env.ANTHROPIC_MODEL = model;
  // 模型映射（任务侧 model_mapping → llm.extra → 进程 env）的四个角色档：
  // claude code 按角色把 Sonnet/Opus/Fable/Haiku 映射到实际模型。提进 settings
  // env 块而不只留在进程 env，是同一个系统档场景——操作者的 ccswitch 在
  // ~/.claude/settings.json 里也写了这些键，settings.json env 压过进程 env，
  // 不提进来就会被操作者的映射盖掉（与 ANTHROPIC_MODEL 一样）。只有配了的档
  // 才写，缺的档由 claude code 自己回落到上一级/兜底。
  for (const role of ["SONNET", "OPUS", "FABLE", "HAIKU"]) {
    const envKey = `ANTHROPIC_DEFAULT_${role}_MODEL`;
    const value = String(process.env[envKey] || "").trim();
    if (value) env[envKey] = value;
  }
  // Fixed task-privacy switches (see doc comment above).
  env.DISABLE_TELEMETRY = "1";
  env.DISABLE_ERROR_REPORTING = "1";
  env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = "1";
  return { env };
}

/**
 * Discover the plugin packages this session should load.
 *
 * Two trees are swept, and a plugin is identified by its manifest `name` in
 * both, so the same plugin present in each is loaded once rather than twice
 * (a duplicate registration of every skill, command and hook it ships):
 *
 * - `home/.agents/plugins` — provider-neutral packages the node synced. Only
 *   the isolated tier writes this tree, so it is empty elsewhere.
 * - claude's own marketplace tree under the HOME, read from its install ledger.
 *   A system-env session runs against the operator's real HOME, where every
 *   plugin they installed lives; the node never writes there, so without this
 *   sweep those plugins simply do not exist for the session.
 *
 * activePlugins narrows the result: a non-empty list means only those package
 * names load this run (the env-tier activation subset); empty/undefined keeps
 * the historic "load everything present" behaviour. A name is matched against
 * the manifest name and against the ledger's `<name>@<marketplace>` key.
 */
function discoverLocalPlugins(home: string, activePlugins?: string[]): string[] {
  const wanted = activePlugins && activePlugins.length > 0
    ? new Set(activePlugins.map((name) => name.trim()).filter(Boolean))
    : null;
  const byName = new Map<string, string>();
  const add = (name: string, dir: string, aliases: string[] = []): void => {
    const key = name.trim();
    if (!key || byName.has(key)) return;
    if (wanted && !wanted.has(key) && !aliases.some((alias) => wanted.has(alias))) return;
    byName.set(key, dir);
  };
  for (const dir of readPluginDirs(join(home, ".agents", "plugins"))) {
    add(claudePluginName(dir) || basename(dir), dir, [basename(dir)]);
  }
  for (const { key, dir } of readClaudeInstalledPlugins(home)) {
    add(claudePluginName(dir) || key.split("@")[0], dir, [key, key.split("@")[0]]);
  }
  return [...byName.values()];
}

/** Immediate child directories of `root`, or [] when it does not exist. */
function readPluginDirs(root: string): string[] {
  let entries: string[];
  try {
    entries = readdirSync(root);
  } catch {
    return [];
  }
  const out: string[] = [];
  for (const entry of entries) {
    const dir = join(root, entry);
    try {
      if (!statSync(dir).isDirectory()) continue;
    } catch {
      continue;
    }
    if (existsSync(join(dir, ".claude-plugin", "plugin.json"))) {
      out.push(dir);
    }
  }
  return out;
}

/**
 * Read claude's own plugin ledger: `~/.claude/plugins/installed_plugins.json`,
 * which maps `"<name>@<marketplace>"` to per-scope install records. This is what
 * `claude plugin install` writes, and it — not `enabledPlugins` in settings — is
 * the authoritative list of what the operator has installed.
 *
 * Deduped by plugin NAME, not by ledger key: the same plugin installed from two
 * marketplaces is two ledger entries pointing at two identical copies, and
 * loading both would register every skill, command and hook twice.
 */
function readClaudeInstalledPlugins(home: string): { key: string; dir: string }[] {
  const ledger = join(home, ".claude", "plugins", "installed_plugins.json");
  let parsed: { plugins?: Record<string, { installPath?: string }[]> };
  try {
    parsed = JSON.parse(readFileSync(ledger, "utf-8"));
  } catch {
    return [];
  }
  const records = parsed?.plugins;
  if (!records || typeof records !== "object") return [];
  const out: { key: string; dir: string }[] = [];
  const seenNames = new Set<string>();
  for (const [key, scopes] of Object.entries(records)) {
    if (!Array.isArray(scopes) || scopes.length === 0) continue;
    const dir = String(scopes[0]?.installPath || "").trim();
    if (!dir || !existsSync(join(dir, ".claude-plugin", "plugin.json"))) continue;
    const name = claudePluginName(dir) || key.split("@")[0];
    if (!name || seenNames.has(name)) continue;
    seenNames.add(name);
    out.push({ key: key.trim(), dir });
  }
  return out;
}

/** The manifest's own `name`, which is what the plugin calls itself. */
function claudePluginName(dir: string): string {
  try {
    const manifest = JSON.parse(
      readFileSync(join(dir, ".claude-plugin", "plugin.json"), "utf-8"),
    ) as { name?: unknown };
    return typeof manifest?.name === "string" ? manifest.name.trim() : "";
  } catch {
    return "";
  }
}

function toClaudeMCPConfig(config: Record<string, unknown> | undefined): Record<string, unknown> | undefined {
	if (!config || typeof config !== "object") {
		return undefined;
	}
	const mapped: Record<string, unknown> = {};
	for (const [name, server] of Object.entries(config)) {
		if (!server || typeof server !== "object") {
			continue;
		}
		const record = server as Record<string, unknown>;
		if (record.type === "local") {
			mapped[name] = {
				type: "stdio",
				command: record.command,
				args: Array.isArray(record.args) ? record.args : [],
				env: flattenEnvMap(record.env as Record<string, { value: string }> | undefined),
			};
			continue;
		}
		if (record.type === "remote") {
			mapped[name] = {
				type: record.transport === "sse" ? "sse" : "http",
				url: record.url,
				headers: flattenEnvMap(record.headers as Record<string, { value: string }> | undefined),
			};
		}
	}
	return Object.keys(mapped).length > 0 ? mapped : undefined;
}

export class ClaudeRunner {
  private readonly writer = new TranscriptWriter();
  private readonly pendingToolUses = new Map<string, PendingToolUse>();
  /** tool_use_id → tool name，让稀疏 tool_result 帧也能带上规范工具名。 */
  private readonly toolNamesById = new Map<string, string>();
  /** SDK-reported level set of still-running background tasks. */
  private backgroundTaskCount = 0;

  constructor(private readonly options: RunnerOptions) {}

  queryOptions(stored: StoredThread | null): Record<string, unknown> {
    const executable = claudeExecutable();
    const permissionMode = resolveClaudeMode(this.options.mode);
    // Plugin packages the node synced for this session. The SDK loads each one
    // wholesale from its .claude-plugin/plugin.json, so a package's own skills
    // arrive without being listed in `skills` below.
    const localPlugins = discoverLocalPlugins(this.options.home, this.options.plugins);
    // Plugin-declared MCP servers are resolved by the runtime and merged into
    // `mcpServers`, not left to the CLI: the session runs with
    // `strictMcpConfig`, under which the CLI ignores every MCP source except
    // the ones passed explicitly — plugins included. The flag stays on because
    // it is what stops a task's cloned repo from shipping a `.mcp.json` that
    // injects servers into the agent. Task servers win on a name collision:
    // they carry this run's freshly minted tokens.
    const mcpServers = {
      ...readPluginMCPServers(localPlugins),
      ...(toClaudeMCPConfig(this.options.mcpConfig as Record<string, unknown> | undefined) ?? {}),
    };
    const hasMcpServers = Object.keys(mcpServers).length > 0;
    return {
      cwd: this.options.workspace,
      env: claudeEnvironment(),
      ...(executable ? { pathToClaudeCodeExecutable: executable } : {}),
      additionalDirectories: uniqueDirectories([this.options.stateRoot, this.options.home, this.options.runtimeRoot]),
      includePartialMessages: true,
      forwardSubagentText: true,
      permissionMode,
      allowDangerouslySkipPermissions: permissionMode === "bypassPermissions",
      // The task's chosen model must be passed explicitly. Claude Code does not
      // read LLM_MODEL (the only model variable the node plants), so without
      // this the CLI silently fell back to its own default — a task created
      // with gpt-oss-120b ran as claude-opus-4-8 instead.
      ...(this.options.model ? { model: this.options.model } : {}),
      resume: stored?.threadId,
      ...(hasMcpServers ? {
        mcpServers,
        strictMcpConfig: true,
      } : {}),
      // `skipMcpDiscovery` because this runner already read each plugin's own
      // .mcp.json above; letting the CLI read it too would double-register the
      // servers under a second name. A plugin like claude-mem is built around
      // its MCP tools, so losing them silently is worse than the small
      // duplication this avoids.
      ...(localPlugins.length > 0 ? {
        plugins: localPlugins.map((path) => ({ type: "local" as const, path, skipMcpDiscovery: true })),
      } : {}),
      // `settingSources: ['user']` is what makes the SDK read ~/.claude (the
      // session home), where the node mirrors synced skills. Plugin-provided
      // skills are enabled by loading the plugin, so when a session only has
      // plugins we still opt into the user source without filtering `skills`.
      ...(this.options.skills && this.options.skills.length > 0 ? {
        settingSources: ["user"],
        skills: this.options.skills,
      } : localPlugins.length > 0 ? {
        settingSources: ["user"],
      } : {}),
      ...(this.options.outputSchema ? {
        outputFormat: {
          type: "json_schema",
          schema: this.options.outputSchema,
        },
      } : {}),
      ...(this.options.abortController ? { abortController: this.options.abortController } : {}),
      // Hard gate: `git stash` in a task workspace pollutes the SHARED stash
      // stack (refs/stash lives in the checkout every worktree of that
      // environment shares). bypassPermissions does NOT consult canUseTool —
      // the SDK documents that a PreToolUse hook is the only gate that runs —
      // so this hook is what actually stops it. The deny reason teaches the
      // model the WIP-commit alternative (see workspace-guard.ts).
      hooks: {
        PreToolUse: [
          {
            matcher: "Bash",
            hooks: [gitStashGuardHook],
          },
        ],
      },
      ...(this.options.systemContext ? {
        systemPrompt: {
          type: "preset",
          preset: "claude_code",
          append: this.options.systemContext,
        },
      } : {}),
      // Auto-compaction is the SDK's own, done inside the call chain, so a
      // compaction pass never costs the turn its completed tool calls. It has to
      // be requested explicitly: the CLI default is not something a server-side
      // session should inherit silently, and not every provider we drive has an
      // equivalent, so the task decides. `autoCompactWindow` is the token budget
      // the SDK compacts toward; omitted means "SDK default".
      settings: {
        isAutoCompactEnabled: this.options.autoCompact !== false,
        autoCompactEnabled: this.options.autoCompact !== false,
        ...(this.options.autoCompactWindow && this.options.autoCompactWindow > 0
          ? { autoCompactWindow: this.options.autoCompactWindow }
          : {}),
        // Task LLM config rides the settings `env` block so it outranks the
        // session HOME's own settings.json (ccswitch's env block in system-env
        // mode). Spread at the root: the helper returns `{ env: {...} }`, the
        // shape the SDK's Settings type expects. See claudeSettingsLlmEnv for
        // why a flat map silently did nothing.
        ...claudeSettingsLlmEnv(),
      },
    };
  }

  handleStreamEvent(message: Record<string, unknown>): void {
    const event = message.event as Record<string, unknown> | undefined;
    if (!event || typeof event !== "object") {
      return;
    }
    if (event.type === "content_block_start") {
      const block = event.content_block as Record<string, unknown> | undefined;
      if (typeof block?.name === "string" && block.name) {
        const input = block.input;
        if (input && typeof input === "object" && Object.keys(input).length > 0) {
          this.writer.line(`\n[tool:${block.name}]`);
          this.writer.line(jsonString(input));
          this.writer.line();
          return;
        }
        if (input && typeof input === "object") {
          this.pendingToolUses.set(contentBlockKey(event, String(block.id ?? this.pendingToolUses.size)), {
            name: block.name,
            partialJson: "",
          });
          return;
        }
        this.writer.line(`\n[tool:${block.name}]`);
        this.writer.line();
      }
      return;
    }
    if (event.type === "content_block_stop") {
      const key = contentBlockKey(event);
      const pending = this.pendingToolUses.get(key);
      if (pending) {
        this.pendingToolUses.delete(key);
        this.writer.line(`\n[tool:${pending.name}]`);
        if (pending.partialJson.trim()) {
          try {
            this.writer.line(jsonString(JSON.parse(pending.partialJson)));
          } catch {
            this.writer.line(pending.partialJson);
          }
          this.writer.line();
        } else {
          this.writer.line();
        }
      }
      return;
    }
    if (event.type !== "content_block_delta") {
      return;
    }
    const delta = event.delta as Record<string, unknown> | undefined;
    if (delta?.type === "input_json_delta" && typeof delta.partial_json === "string") {
      const pending = this.pendingToolUses.get(contentBlockKey(event));
      if (pending) {
        pending.partialJson += delta.partial_json;
      }
      return;
    }
    if (delta?.type === "text_delta" && typeof delta.text === "string") {
      this.writer.write(delta.text);
      return;
    }
    if (delta?.type === "thinking_delta" && typeof delta.thinking === "string") {
      this.writer.write(delta.thinking);
    }
  }

  /**
   * Normalize one Claude Agent-SDK message into the shared `item` frames every
   * consumer downstream already understands.
   *
   * The `item` shape (item.type = agent_message / reasoning / tool_call / error,
   * plus a stable `id` so incremental updates collapse onto one entry) is the
   * vocabulary the Codex runner's SDK emits natively and the one the node,
   * gateway and frontend were all written against. Forwarding the raw Claude SDK
   * message instead made this runner the odd one out: consumers had to re-parse a
   * provider-specific shape, so assistant text was dropped, tool calls were
   * invisible, and per-token stream deltas each became their own entry.
   *
   * Only COMPLETED blocks are emitted — never stream deltas. One logical block
   * (a message, a thought, a tool call) is one item, which is what keeps a turn
   * from fragmenting into hundreds of rows. `agent_id` carries sub-agent
   * attribution: empty means the root agent, non-empty is the sub-agent spawned
   * by that Task tool_use.
   */
  forwardMessage(message: Record<string, unknown>): void {
    const emit = this.options.emit;
    if (!emit) return;
    // Out-of-band SDK signals (retrying a rate-limited call, compacting the
    // context) are not conversation items — route them to their own frames
    // before the item mapping, which would otherwise discard them.
    if (textOf(message.type) === "system") {
      // Track the live background-task count so runPrompt can keep the SDK
      // query open while async subagents are still running (see result case).
      switch (textOf(message.subtype)) {
        case "background_tasks_changed": {
          const tasks = Array.isArray(message.tasks) ? message.tasks : [];
          this.backgroundTaskCount = tasks.length;
          break;
        }
        case "task_started":
          this.backgroundTaskCount += 1;
          break;
        case "task_notification":
          this.backgroundTaskCount = Math.max(0, this.backgroundTaskCount - 1);
          break;
        default:
          break;
      }
      const frame = claudeSystemFrame(message);
      if (frame) emit(frame.type, frame.fields);
      return;
    }
    const parentToolUseId = message.parent_tool_use_id;
    const agentId = typeof parentToolUseId === "string" ? parentToolUseId : "";
    // Sub-agent attribution metadata sits at the SDK message top level; carry it
    // onto the item so a replaying client can name the sub-agent and its task
    // without re-reading raw SDK frames.
    const agentName = typeof message.subagent_type === "string" ? message.subagent_type : "";
    const agentTask = typeof message.task_description === "string" ? message.task_description : "";
    for (const item of claudeMessageToItems(message, this.toolNamesById)) {
      // subagent_id 是唯一的路由事实：空 = 根（主对话）；非空 = 子 Agent 详情
      // 专属。同时把 canonical 字段提升到帧顶层，让消费者不解析 item 也能路由。
      item.subagent_id = agentId;
      if (agentId) {
        item.agent_name = agentName;
        item.task = agentTask;
      }
      emit("agent_event", {
        agent_id: agentId,
        subagent_id: agentId,
        logical_event_id: String(item.logical_event_id ?? item.id),
        event_name: "agent_event",
        event_kind: String(item.event_kind ?? ""),
        tool_name: String(item.tool_name ?? ""),
        phase: String(item.phase ?? ""),
        item,
      });
    }
    // A result carries the turn's token totals; publish them so the client's
    // context gauge (and any threshold-driven compaction) has real numbers.
    if (textOf(message.type) === "result") {
      const usage = claudeUsageFrame(message);
      if (usage) emit("usage_update", usage);
    }
  }

  async runPrompt(promptText: string): Promise<AgentResult> {
    const { query: claudeQuery } = await import("@anthropic-ai/claude-agent-sdk");
    const stored = await readStoredThread(this.options.stateRoot, "claude", this.options.sessionScope);
    const queryOpts = this.queryOptions(stored);
    // Diagnostic trace: exactly what the SDK receives for this turn — model,
    // resume thread, permission mode. process.env.ANTHROPIC_* at query time is
    // what the CLI actually inherits (settings env is inside queryOpts.settings).
    process.stderr.write(
      `[claude-query] model=${String(queryOpts.model || "<none>")} resume=${stored?.threadId || "<none>"} `
      + `env.ANTHROPIC_MODEL=${process.env.ANTHROPIC_MODEL || "<none>"} env.LLM_MODEL=${process.env.LLM_MODEL || "<none>"} `
      + `mode=${String((queryOpts as Record<string, unknown>).permissionMode || "<none>")}\n`,
    );
    const stream = claudeQuery({
      prompt: promptText,
      options: queryOpts,
    });

    const result: AgentResult = {
      provider: "claude",
      threadId: stored?.threadId || "",
      stopReason: "completed",
      finalText: "",
      transcript: "",
      stderr: "",
    };
    let rootResultSeen = false;

    try {
      messages: for await (const rawMessage of stream) {
        const message = rawMessage as Record<string, unknown>;
        const newThreadId = String(message.session_id || "");
        if (newThreadId && newThreadId !== result.threadId) {
          // Diagnostic trace: the CLI's actual session id as it changes mid-turn
          // (resume may fork a new session). This is the value the CLI sends to
          // the gateway as x-claude-code-session-id — if it diverges from the
          // task's bound thread, the gateway 403s the next task key request.
          process.stderr.write(
            `[claude-session] old=${result.threadId || "<none>"} new=${newThreadId} msgType=${textOf(message.type)}\n`,
          );
          result.threadId = newThreadId;
        }
        // Pipe every SDK message through untouched (stream mode only). The
        // transcript handling below is unchanged; this just stops the runtime
        // from discarding the SDK's own structure and sub-agent attribution.
        this.forwardMessage(message);
        if (rootResultSeen && this.backgroundTaskCount <= 0) {
          // The terminal task_notification/background_tasks_changed arrived
          // after the root result: all child output has now been consumed.
          stream.close?.();
          break messages;
        }
        switch (message.type) {
          case "stream_event":
            this.handleStreamEvent(message);
            break;
          case "assistant": {
            if (!result.finalText) {
              const assistantMessage = message.message as Record<string, unknown> | undefined;
              const content = assistantMessage?.content;
              const textBlocks = Array.isArray(content)
                ? content
                  .filter((item) => (item as Record<string, unknown>)?.type === "text")
                  .map((item) => String((item as Record<string, unknown>).text || ""))
                  .join("")
                : "";
              if (textBlocks) {
                result.finalText = textBlocks;
              }
            }
            break;
          }
          case "tool_use_summary":
            if (typeof message.summary === "string" && message.summary.trim()) {
              this.writer.line(`\n${message.summary}`);
            }
            break;
          case "auth_status":
            if (Array.isArray(message.output) && message.output.length > 0) {
              this.writer.line(message.output.join("\n"));
            }
            if (message.error) {
              this.writer.line(String(message.error));
            }
            break;
          case "system":
            if (message.subtype === "local_command_output" && typeof message.content === "string") {
              this.writer.line(message.content);
            }
            break;
          case "result":
            result.stopReason = String(message.stop_reason || result.stopReason);
            // The Claude Code SDK reports API/runtime failures with
            // subtype:"success" but is_error:true (e.g. an upstream 405/429/500
            // becomes {subtype:"success", is_error:true, api_error_status:405,
            // result:"API Error: ..."}). Keying only on subtype meant every such
            // failure was swallowed as the turn's final text — the task looked
            // like it "returned nothing" while the real cause sat unread in the
            // transcript. Treat is_error (or a non-success subtype) as a failed
            // turn so it is raised and surfaces as a structured error frame.
            if (message.subtype === "success" && !message.is_error) {
              result.finalText = hasOwn(message, "structured_output")
                ? JSON.stringify(message.structured_output)
                : String(message.result || result.finalText);
              // Async subagents (Agent tool, run_in_background) outlive the root
              // turn: closing the query here killed the CLI — and with it every
              // still-running child. The child's upstream request kept going,
              // then hit a dead downstream and got logged as `cancelled`; its
              // final message never persisted and the weather result vanished.
              // Keep draining until the SDK level signal says no background
              // work remains (or the stream ends on its own).
              if (this.backgroundTaskCount <= 0) {
                stream.close?.();
                break messages;
              }
              // Record that the root itself is done; the loop continues to
              // consume child frames (task_notification / agent events with
              // parent_tool_use_id) until the last child settles.
              rootResultSeen = true;
              break;
            } else {
              const errors = Array.isArray(message.errors)
                ? message.errors.filter(Boolean).join("; ")
                : "";
              const apiStatus = message.api_error_status
                ? `API 错误 ${message.api_error_status}`
                : "";
              const errorText = typeof message.result === "string" && message.result.trim()
                ? message.result
                : errors || apiStatus || "claude execution failed";
              // Carry the upstream status so the stream layer can label the error
              // frame (`status_code`) instead of leaving consumers to pattern-match
              // "API 错误 429" out of a human-readable sentence.
              throw new UpstreamStatusError(
                errorText,
                typeof message.api_error_status === "number" ? message.api_error_status : null,
              );
            }
            break;
          default:
            break;
        }
      }
    } finally {
      stream.close?.();
      // Persist the thread id even when the turn FAILED. The CLI may have
      // created or forked a session before erroring (result.threadId is filled
      // from the streamed session_id above), and the gateway binds a Task's
      // provider thread on its FIRST request. If that first turn fails and we
      // skip this write, the runtime has no stored thread to resume, so the next
      // turn forks a brand-new session — while the gateway still enforces the
      // first one and 403s ("Task API Key 与 provider 会话不匹配"). Writing here
      // (not only on the success path below) keeps the stored thread aligned
      // with whatever the CLI actually used.
      if (result.threadId) {
        try {
          await writeStoredThread(
            this.options.stateRoot,
            "claude",
            result.threadId,
            undefined,
            this.options.sessionScope,
          );
        } catch (persistError) {
          // Best-effort: never let a state-write failure mask the turn's own
          // error (this runs inside finally while an exception propagates).
          process.stderr.write(
            `[claude-session] persist thread on failure path failed: ${persistError instanceof Error ? persistError.message : String(persistError)}\n`,
          );
        }
      }
    }

    result.transcript = this.writer.transcript();
    if (!result.finalText && result.transcript) {
      result.finalText = result.transcript;
    }
    await writeStoredThread(this.options.stateRoot, "claude", result.threadId, undefined, this.options.sessionScope);
    return result;
  }
}
