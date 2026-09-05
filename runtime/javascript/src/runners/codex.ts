import { resolveCodexPath } from "../codex-path.js";
import { sessionEnv } from "../env.js";
import { existsSync, readdirSync, statSync } from "node:fs";
import { spawnSync } from "node:child_process";
import path from "node:path";
import { resolveCodexMode } from "./mode.js";
import { uniqueDirectories } from "../paths.js";
import { readStoredThread, writeStoredThread } from "../session-state.js";
import { extractText, jsonString } from "../text.js";
import { appendDelta, TranscriptWriter, type TextWriter } from "../transcript.js";
import type { AgentResult, RunnerOptions } from "../types.js";

interface CodexItemState {
  commandStarted?: boolean;
  commandOutput?: string;
  fileChangeEmitted?: boolean;
  mcpStarted?: boolean;
  mcpResultEmitted?: boolean;
  mcpErrorEmitted?: boolean;
  webSearchEmitted?: boolean;
}

interface TranscriptRecorder extends TextWriter {
  transcript(): string;
}

function webSearchQuery(item: Record<string, unknown>): string {
  if (typeof item.query === "string" && item.query.trim()) {
    return item.query;
  }
  const action = item.action as Record<string, unknown> | undefined;
  if (typeof action?.query === "string" && action.query.trim()) {
    return action.query;
  }
  if (Array.isArray(action?.queries)) {
    return action.queries.filter((entry) => typeof entry === "string" && entry.trim()).join(", ");
  }
  return "";
}

export class CodexRunner {
  private readonly itemState = new Map<string, string | CodexItemState>();

  constructor(
    private readonly options: RunnerOptions,
    private readonly writer: TranscriptRecorder = new TranscriptWriter(),
  ) {}

  transcript(): string {
    return this.writer.transcript();
  }

  // threadOptions are re-supplied on every startThread/resumeThread call, so a
  // model/mode change lands on the next turn without restarting anything. model
  // MUST be included: the SDK turns it into `codex exec --model <model>`, and
  // omitting it silently pins the session to the CLI/config default.
  threadOptions(): Record<string, unknown> {
    const mode = resolveCodexMode(this.options.mode);
    const model = String(this.options.model || "").trim();
    return {
      ...(model ? { model } : {}),
      workingDirectory: this.options.workspace,
      additionalDirectories: uniqueDirectories([this.options.stateRoot, this.options.home, this.options.runtimeRoot]),
      skipGitRepoCheck: true,
      sandboxMode: mode.sandboxMode,
      approvalPolicy: mode.approvalPolicy,
      networkAccessEnabled: mode.networkAccessEnabled,
    };
  }

  // codexEnv binds HOME (and USERPROFILE on Windows) to the session home so the
  // spawned Codex CLI reads THIS session's ~/.codex/config.toml — where the node
  // wrote the session-scoped MCP servers. The Codex SDK exposes no MCP option, so
  // this env binding is the only channel that makes session MCP isolation real.
  codexEnv(): Record<string, string> {
    return sessionEnv(this.options.home);
  }

  emitCommand(item: Record<string, unknown> & { id: string }): void {
    const state = (this.itemState.get(item.id) || {}) as CodexItemState;
    if (!state.commandStarted) {
      this.writer.line(`\n$ ${item.command}`);
      state.commandStarted = true;
      this.itemState.set(item.id, state);
    }
    appendDelta(this.writer, this.itemState as Map<string, string>, `${item.id}:command`, String(item.aggregated_output || ""));
    state.commandOutput = String(item.aggregated_output || "");
    this.itemState.set(item.id, state);
  }

  emitFileChange(item: Record<string, unknown> & { id: string }): void {
    const changes = Array.isArray(item.changes) ? item.changes : [];
    if (changes.length === 0) {
      return;
    }
    const state = (this.itemState.get(item.id) || {}) as CodexItemState;
    if (state.fileChangeEmitted) {
      return;
    }
    this.writer.line("\n[file_change]");
    for (const change of changes) {
      const record = change as Record<string, unknown>;
      this.writer.line(`${record.kind}: ${record.path}`);
    }
    state.fileChangeEmitted = true;
    this.itemState.set(item.id, state);
  }

  emitMcp(item: Record<string, unknown> & { id: string }): void {
    const state = (this.itemState.get(item.id) || {}) as CodexItemState;
    if (!state.mcpStarted) {
      this.writer.line(`\n[mcp:${item.server}/${item.tool}]`);
      if (item.arguments !== undefined) {
        this.writer.line(jsonString(item.arguments));
      }
      state.mcpStarted = true;
    }
    const error = item.error as Record<string, unknown> | undefined;
    if (item.status === "completed" && item.result && !state.mcpResultEmitted) {
      const content = extractText(item.result);
      if (content.trim()) {
        this.writer.line(content);
      }
      state.mcpResultEmitted = true;
    }
    if (item.status === "failed" && typeof error?.message === "string" && !state.mcpErrorEmitted) {
      this.writer.line(error.message);
      state.mcpErrorEmitted = true;
    }
    this.itemState.set(item.id, state);
  }

  emitTodo(item: Record<string, unknown> & { id: string }): void {
    const lines = Array.isArray(item.items)
      ? item.items.map((entry) => {
        const record = entry as Record<string, unknown>;
        return `${record.completed ? "[x]" : "[ ]"} ${record.text}`;
      })
      : [];
    const nextText = lines.length > 0 ? `\n[todo]\n${lines.join("\n")}\n` : "";
    appendDelta(this.writer, this.itemState as Map<string, string>, item.id, nextText);
  }

  emitWebSearch(item: Record<string, unknown> & { id: string }, eventType: unknown): void {
    const state = (this.itemState.get(item.id) || {}) as CodexItemState;
    if (state.webSearchEmitted) {
      return;
    }
    const query = webSearchQuery(item);
    // Match Codex CLI transcript behavior: if the search completes without an
    // exposed query, still emit the marker so the tool use remains visible.
    if (!query && eventType !== "item.completed") {
      return;
    }
    this.writer.line(`\n[web_search] ${query}`);
    state.webSearchEmitted = true;
    this.itemState.set(item.id, state);
  }

  handleEvent(event: Record<string, unknown>, result: AgentResult): void {
    if (event.type === "thread.started") {
      result.threadId = String(event.thread_id || result.threadId);
      return;
    }
    if (event.type === "turn.failed") {
      const error = event.error as Record<string, unknown> | undefined;
      throw new Error(String(error?.message || "codex turn failed"));
    }
    if (!event.item || typeof event.item !== "object") {
      return;
    }
    const item = event.item as Record<string, unknown> & { id: string; type: string };
    switch (item.type) {
      case "agent_message":
        appendDelta(this.writer, this.itemState as Map<string, string>, item.id, String(item.text || ""));
        if (event.type === "item.completed") {
          result.finalText = String(item.text || result.finalText);
        }
        break;
      case "reasoning":
        appendDelta(this.writer, this.itemState as Map<string, string>, item.id, String(item.text || ""));
        break;
      case "command_execution":
        this.emitCommand(item);
        break;
      case "file_change":
        this.emitFileChange(item);
        break;
      case "mcp_tool_call":
        this.emitMcp(item);
        break;
      case "web_search":
        this.emitWebSearch(item, event.type);
        break;
      case "todo_list":
        this.emitTodo(item);
        break;
      case "error":
        this.writer.line(String(item.message || "codex item error"));
        break;
      default:
        break;
    }
  }

  async runPrompt(promptText: string): Promise<AgentResult> {
    // Register plugin marketplaces the node synced (home/.agents/plugins/*)
    // before starting the thread. Codex's plugin loader is marketplace-driven
    // and has no SDK option, so this CLI call is what makes synced plugins reach
    // the session. Idempotent and non-fatal on failure.
    registerCodexMarketplaces(this.options.home);

    const { Codex } = await import("@openai/codex-sdk");
    const stored = await readStoredThread(this.options.stateRoot, "codex", this.options.sessionScope);
    const codex = new Codex({
      codexPathOverride: resolveCodexPath(),
      env: this.codexEnv(),
      // `config` (the `--config key=value` overrides) is a CodexOptions field on the
      // constructor; it is NOT read from ThreadOptions/startThread. Injecting the combined
      // Agent Identity + MPI system context here applies to both start and resume flows.
      ...(this.options.systemContext
        ? { config: { developer_instructions: this.options.systemContext } }
        : {}),
    });
    const thread = stored?.threadId
      ? codex.resumeThread(stored.threadId, this.threadOptions())
      : codex.startThread(this.threadOptions());

    const result: AgentResult = {
      provider: "codex",
      threadId: stored?.threadId || "",
      stopReason: "completed",
      finalText: "",
      transcript: "",
      stderr: "",
    };

    const turnOptions = {
      ...(this.options.outputSchema ? { outputSchema: this.options.outputSchema } : {}),
      ...(this.options.abortController ? { signal: this.options.abortController.signal } : {}),
    };
    const { events } = await thread.runStreamed(
      promptText,
      Object.keys(turnOptions).length > 0 ? turnOptions : undefined,
    );
    for await (const event of events) {
      this.handleEvent(event as Record<string, unknown>, result);
    }
    result.threadId = thread.id || result.threadId;
    result.transcript = this.writer.transcript();
    if (!result.finalText && result.transcript) {
      result.finalText = result.transcript;
    }
    await writeStoredThread(this.options.stateRoot, "codex", result.threadId, undefined, this.options.sessionScope);
    return result;
  }
}

/**
 * Discover marketplace roots the node laid down under home/.agents/plugins.
 *
 * A Codex marketplace is any directory that carries `.agents/plugins/marketplace.json`
 * at its root. Packages like obra/superpowers ship that manifest themselves and
 * double as their own marketplace root (source.url "./"). For a bare plugin
 * package (only `.codex-plugin/plugin.json`) the node generates a wrapper
 * marketplace.json so Codex can still load it.
 */
function discoverCodexMarketplaces(home: string): string[] {
  const root = path.join(home, ".agents", "plugins");
  let entries: string[];
  try {
    entries = readdirSync(root);
  } catch {
    return [];
  }
  const roots: string[] = [];
  for (const entry of entries) {
    const dir = path.join(root, entry);
    try {
      if (!statSync(dir).isDirectory()) continue;
    } catch {
      continue;
    }
    if (existsSync(path.join(dir, ".agents", "plugins", "marketplace.json"))) {
      roots.push(dir);
    }
  }
  return roots;
}

/**
 * Register each marketplace with `codex plugin marketplace add <path>`.
 *
 * Codex's plugin loader is marketplace-driven: the CLI reads the manifest, pulls
 * the declared plugin sources, and loads hooks/skills itself — there is no SDK
 * option for it. Running this before startThread/resumeThread lets the spawned
 * Codex CLI see the session's plugins the same way opencode's config rewrite
 * does. Idempotent: re-adding an already-added marketplace is a no-op (Codex
 * reports `already_added`). Failures are non-fatal — a bad marketplace should
 * not block the session, only that plugin won't load.
 */
function registerCodexMarketplaces(home: string): void {
  const roots = discoverCodexMarketplaces(home);
  if (roots.length === 0) {
    return;
  }
  const codexBin = resolveCodexPath();
  if (!codexBin) {
    return;
  }
  const env = sessionEnv(home);
  for (const root of roots) {
    try {
      spawnSync(codexBin, ["plugin", "marketplace", "add", root, "--json"], {
        cwd: root,
        env,
        encoding: "utf8",
        windowsHide: true,
      });
    } catch {
      // Non-fatal: the plugin just won't load; the session still runs.
    }
  }
}
