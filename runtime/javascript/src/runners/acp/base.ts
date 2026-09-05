import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import readline from "node:readline";
import { bindAbortToChild } from "../../abort.js";
import { appendDelta, TranscriptWriter } from "../../transcript.js";
import { flattenEnvMap } from "../../mcp-config.js";
import { readStoredThread, writeStoredThread } from "../../session-state.js";
import type { AgentResult, RunnerOptions } from "../../types.js";

export type AcpSpawnSpec = {
  command: string;
  args?: string[];
  env?: NodeJS.ProcessEnv;
};

type RpcMessage = {
  jsonrpc?: string;
  id?: number | string;
  method?: string;
  params?: unknown;
  result?: unknown;
  error?: { code?: number; message?: string; data?: unknown };
};

type Pending = {
  resolve: (value: unknown) => void;
  reject: (error: Error) => void;
};

type Item = Record<string, unknown> & { id: string; type: string };

/** Common ACP client implementation. Subclasses only provide agent-specific process behavior. */
export abstract class BaseAcpRunner {
  protected readonly writer = new TranscriptWriter();
  private readonly itemText = new Map<string, string>();
  private child?: ChildProcessWithoutNullStreams;
  private lineLoop?: Promise<void>;
  private nextRequestId = 1;
  private readonly pending = new Map<number | string, Pending>();
  private closed = false;
  private finalText = "";
  private stopReason = "completed";

  constructor(protected readonly options: RunnerOptions) {}

  protected abstract readonly acpProvider: AgentResult["provider"];
  protected abstract spawnSpec(): Promise<AcpSpawnSpec> | AcpSpawnSpec;

  protected authMethodId(): string | undefined {
    return undefined;
  }

  protected initializeParams(): Record<string, unknown> {
    return {
      protocolVersion: 1,
      clientCapabilities: {
        fs: { readTextFile: false, writeTextFile: false },
        terminal: false,
      },
      clientInfo: { name: "agent-compose-runtime", version: "0.6.0" },
    };
  }

  protected sessionParams(storedThreadId: string): Record<string, unknown> {
    const params: Record<string, unknown> = {
      cwd: this.options.workspace,
      mcpServers: this.options.mcpConfig ? this.toAcpMcpServers(this.options.mcpConfig) : [],
    };
    if (this.options.mode) params.mode = this.options.mode;
    if (storedThreadId && this.supportsSessionLoad()) {
      params.sessionId = storedThreadId;
    }
    return params;
  }

  protected supportsSessionLoad(): boolean {
    return true;
  }

  protected async prepare(): Promise<void> {}

  protected async authenticate(): Promise<void> {
    const methodId = this.authMethodId();
    if (methodId) await this.rpc("authenticate", { methodId });
  }

  /** Handle an agent-specific request/notification. Return true when handled. */
  protected async handleExtension(
    _message: RpcMessage,
    _respond: (result: unknown) => void,
  ): Promise<boolean> {
    return false;
  }

  /** Default is unattended execution: permit a requested operation. */
  protected permissionResponse(_params: unknown): unknown {
    return { outcome: { outcome: "selected", optionId: "allow-always" } };
  }

  async runPrompt(promptText: string): Promise<AgentResult> {
    if (this.options.outputSchema) {
      throw new Error(`${this.acpProvider} ACP runner does not support structured JSON output`);
    }
    await this.prepare();
    const stored = await readStoredThread(this.options.stateRoot, this.acpProvider, this.options.sessionScope);
    const result: AgentResult = {
      provider: this.acpProvider,
      threadId: stored?.threadId || "",
      stopReason: "completed",
      finalText: "",
      transcript: "",
      stderr: "",
    };

    try {
      await this.startProcess();
      const child = this.child;
      const detachAbort = child ? bindAbortToChild(child, this.options.abortController) : () => undefined;
      try {
        await this.rpc("initialize", this.initializeParams());
        await this.authenticate();
        const session = stored?.threadId && this.supportsSessionLoad()
          ? await this.rpc("session/load", { sessionId: stored.threadId, cwd: this.options.workspace })
          : await this.rpc("session/new", this.sessionParams(stored?.threadId || ""));
        result.threadId = this.stringField(session, "sessionId", "session_id") || result.threadId;
        if (!result.threadId && stored?.threadId) result.threadId = stored.threadId;
        const promptResult = await this.rpc("session/prompt", {
          sessionId: result.threadId,
          prompt: [{ type: "text", text: this.options.systemContext ? `${this.options.systemContext}\n\n${promptText}` : promptText }],
        });
        result.stopReason = this.stringField(promptResult, "stopReason", "stop_reason") || this.stopReason;
        result.finalText = this.finalText;
      } finally {
        detachAbort();
      }
    } finally {
      await this.stopProcess();
    }

    result.transcript = this.writer.transcript();
    result.finalText ||= this.finalText || result.transcript;
    await writeStoredThread(this.options.stateRoot, this.acpProvider, result.threadId, undefined, this.options.sessionScope);
    return result;
  }

  protected async startProcess(): Promise<void> {
    const spec = await this.spawnSpec();
    const env = { ...process.env, ...(spec.env || {}) };
    this.child = spawn(spec.command, spec.args || [], {
      cwd: this.options.workspace,
      env,
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
    });
    this.child.stderr.on("data", (chunk) => this.writer.write(String(chunk || "")));
    this.lineLoop = this.readMessages(this.child);
  }

  private async readMessages(child: ChildProcessWithoutNullStreams): Promise<void> {
    const lines = readline.createInterface({ input: child.stdout, crlfDelay: Infinity });
    try {
      for await (const line of lines) {
        if (!line.trim()) continue;
        let message: RpcMessage;
        try {
          message = JSON.parse(line) as RpcMessage;
        } catch {
          this.writer.line(line);
          continue;
        }
        if (message.id !== undefined && (message.result !== undefined || message.error !== undefined)) {
          const waiter = this.pending.get(message.id);
          if (!waiter) continue;
          this.pending.delete(message.id);
          if (message.error) waiter.reject(new Error(message.error.message || "ACP request failed"));
          else waiter.resolve(message.result);
          continue;
        }
        if (message.method) await this.handleMessage(message);
      }
    } finally {
      lines.close();
      if (!this.closed) this.rejectPending(new Error(`${this.acpProvider} ACP process closed`));
    }
  }

  private async handleMessage(message: RpcMessage): Promise<void> {
    const respond = (result: unknown) => {
      if (message.id !== undefined) this.send({ jsonrpc: "2.0", id: message.id, result });
    };
    if (message.method === "session/update") {
      this.handleSessionUpdate(message.params);
      return;
    }
    if (message.method === "session/request_permission") {
      respond(this.permissionResponse(message.params));
      return;
    }
    if (await this.handleExtension(message, respond)) return;
    // Unknown notifications are intentionally harmless; preserve diagnostics without corrupting stdout.
    if (message.id !== undefined) respond({});
  }

  private handleSessionUpdate(params: unknown): void {
    const outer = this.asRecord(params) ?? {};
    const update = this.asRecord(outer.update) ?? outer;
    const kind = this.stringField(update, "sessionUpdate", "type", "kind");
    const content = this.asRecord(update.content);
    const id = this.stringField(update, "toolCallId", "tool_call_id", "id") || `${this.acpProvider}:message`;
    if (kind === "agent_message_chunk" || kind === "agent_message") {
      const text = this.extractText(content) || this.stringField(update, "text", "delta");
      if (text) this.emitItem({ id, type: "agent_message", text }, true);
      return;
    }
    if (kind === "agent_thought_chunk" || kind === "agent_thought" || kind === "reasoning") {
      const text = this.extractText(content) || this.stringField(update, "text", "delta");
      if (text) this.emitItem({ id, type: "reasoning", text }, true);
      return;
    }
    if (kind === "tool_call" || kind === "tool_call_update") {
      const title = this.stringField(update, "title", "name", "toolName") || "tool";
      const input = update.rawInput ?? update.input ?? {};
      const output = update.rawOutput ?? update.output ?? update.content;
      const status = this.stringField(update, "status", "statusText") || (output !== undefined ? "done" : "running");
      this.emitItem({ id, type: "tool_call", title, input, ...(output !== undefined ? { output } : {}), status });
      return;
    }
    if (kind === "plan") {
      this.emitItem({ id, type: "plan", plan: update.entries ?? update.plan ?? content ?? {} });
      return;
    }
    if (kind === "error") {
      const text = this.extractText(update) || "ACP agent error";
      this.emitItem({ id, type: "error", text });
      return;
    }
    const text = this.extractText(update);
    if (text) this.writer.write(text);
  }

  private emitItem(item: Item, delta = false): void {
    const text = typeof item.text === "string" ? item.text : "";
    if (delta && text) {
      const previous = this.itemText.get(item.id) || "";
      const next = text.startsWith(previous) ? text : previous + text;
      this.itemText.set(item.id, next);
      item.text = next;
      if (item.type === "agent_message") this.finalText = next;
      appendDelta(this.writer, this.itemText, `${item.id}:transcript`, next);
    }
    this.options.emit?.("agent_event", { agent_id: "", item });
  }

  protected async stopProcess(): Promise<void> {
    this.closed = true;
    this.rejectPending(new Error(`${this.acpProvider} ACP process stopped`));
    const child = this.child;
    if (!child) return;
    if (!child.killed) child.kill("SIGTERM");
    try { await this.lineLoop; } catch { /* original turn error wins */ }
    this.child = undefined;
  }

  protected send(message: RpcMessage): void {
    if (!this.child?.stdin.writable) throw new Error(`${this.acpProvider} ACP stdin is unavailable`);
    this.child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", ...message })}\n`);
  }

  protected rpc(method: string, params: unknown): Promise<any> {
    const id = this.nextRequestId++;
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      try { this.send({ id, method, params }); } catch (error) {
        this.pending.delete(id);
        reject(error instanceof Error ? error : new Error(String(error)));
      }
    });
  }

  private rejectPending(error: Error): void {
    for (const waiter of this.pending.values()) waiter.reject(error);
    this.pending.clear();
  }

  private toAcpMcpServers(config: Record<string, unknown>): unknown[] {
    const servers: unknown[] = [];
    for (const [name, value] of Object.entries(config)) {
      const server = this.asRecord(value);
      if (!server) continue;
      if (server.type === "local") {
        servers.push({
          name,
          command: server.command,
          args: Array.isArray(server.args) ? server.args : [],
          env: flattenEnvMap(server.env as Record<string, { value: string }> | undefined) || {},
        });
      } else {
        servers.push({
          name,
          url: server.url,
          headers: flattenEnvMap(server.headers as Record<string, { value: string }> | undefined) || {},
        });
      }
    }
    return servers;
  }

  private extractText(value: unknown): string {
    if (typeof value === "string") return value;
    const record = this.asRecord(value);
    if (!record) return "";
    return this.stringField(record, "text", "value", "delta", "message");
  }

  private stringField(value: unknown, ...keys: string[]): string {
    const record = this.asRecord(value);
    if (!record) return "";
    for (const key of keys) if (typeof record[key] === "string" && record[key]) return record[key] as string;
    return "";
  }

  private asRecord(value: unknown): Record<string, any> | null {
    return typeof value === "object" && value !== null && !Array.isArray(value) ? value as Record<string, any> : null;
  }
}
