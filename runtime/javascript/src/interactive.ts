import path from "node:path";
import process from "node:process";
import { TurnCancelledError } from "./errors.js";
import { readMCPConfig } from "./mcp-config.js";
import { buildPromptRuntimeOptions } from "./prompt.js";
import { ClaudeRunner } from "./runners/claude.js";
import { CodexRunner } from "./runners/codex.js";
import { GeminiRunner } from "./runners/gemini.js";
import { OpenCodeRunner } from "./runners/opencode.js";
import { CursorAcpRunner } from "./runners/acp/cursor.js";
import { readStoredThread, writeStoredThread } from "./session-state.js";
import { normalizeProvider } from "./provider.js";
import { withTurnRetry, type RetryNotice } from "./retry.js";
import type { AgentResult, Provider, RunnerOptions } from "./types.js";

export interface InteractiveStartOptions {
  provider?: string;
  stateRoot?: string;
  workspace?: string;
  home?: string;
  model?: string;
  mode?: string;
  outputSchemaFile?: string;
  sessionScope?: string;
}

export type EmitInteractiveFrame = (type: string, fields?: object) => void;

/**
 * One per-turn config snapshot. The node stamps this onto every human_message
 * frame so the runtime re-prepares the provider with the latest config (model /
 * mode / llm) instead of relying on a separate hot-update protocol. Any field
 * left undefined falls back to the start-frame value.
 */
export interface TurnSnapshot {
  model?: string;
  mode?: string;
  llm?: {
    endpoint?: string;
    apiKey?: string;
    model?: string;
    protocol?: string;
    headers?: Record<string, string>;
    extra?: Record<string, string>;
  };
  /** Off switches provider-side compaction and the runtime's own compact turn. */
  autoCompact?: boolean;
  /** Token budget to compact toward (claude only; others have no such knob). */
  autoCompactWindow?: number;
  /** Occupancy fraction that triggers the runtime's own `/compact` turn. */
  autoCompactThreshold?: number;
}

/**
 * Context occupancy at which the runtime injects its own compaction turn, for
 * providers whose SDK has none. Deliberately earlier than the ~95% a CLI with
 * built-in compaction uses: that only works because it can compact mid-call,
 * whereas the runtime can only act on a turn boundary, so it needs enough
 * headroom left for the turn it is about to run.
 */
export const DEFAULT_AUTO_COMPACT_THRESHOLD = 0.8;

/** Providers whose SDK compacts on its own; the runtime must not double up. */
const SDK_COMPACTS = new Set(["claude"]);

/** The shared shape the stream loop drives, regardless of provider. */
export interface InteractiveSession {
  runHumanMessage(
    message: string,
    snapshot?: TurnSnapshot,
    messageId?: string,
    deliveryAttempt?: number,
  ): Promise<void>;
  /**
   * Cancel the in-flight turn only. Returns false when no turn is running.
   * The session stays alive for the next human_message — cancellation must
   * never end the stream process.
   */
  cancelCurrentTurn(): boolean;
  finish(stopReason: string): Promise<AgentResult>;
}

/**
 * InteractiveSession drives every provider (claude/codex/opencode/gemini) through
 * the same per-turn loop: build RunnerOptions from the latest snapshot, hand them
 * to the provider runner (which prepares MCP/skill/plugin config, resumes the
 * stored thread, sends the message, and persists the new thread id). There is no
 * long-lived provider process state held here and no hot-switch protocol — a
 * config change takes effect on the next turn because the next turn rebuilds the
 * options from the new snapshot.
 */
export class PromptRunnerSession implements InteractiveSession {
  private readonly baseOptions: RunnerOptions;
  private readonly emit: EmitInteractiveFrame;
  private currentModel?: string;
  private currentMode?: string;
  private currentLlm?: TurnSnapshot["llm"];
  private currentAutoCompact?: boolean;
  private currentAutoCompactWindow?: number;
  private currentAutoCompactThreshold?: number;
  // Last-known provider thread id + accumulated transcript across turns, so
  // finish() can report a meaningful terminal result even though no provider
  // process is held live between turns.
  private lastThreadId = "";
  private transcriptParts: string[] = [];
  /**
   * Context occupancy after the most recent turn, from the runner's usage frame.
   * Compaction can only be decided between turns, so the previous turn's figure
   * is the only signal available when the next one starts.
   */
  private lastUsage: { used: number; size: number } | null = null;
  /** Abort handle for the in-flight turn; null when idle. */
  private currentAbort: AbortController | null = null;
  /** end-to-end id of the in-flight turn's user message ("" when none). */
  private currentMessageId = "";

  constructor(baseOptions: RunnerOptions, emit: EmitInteractiveFrame) {
    this.baseOptions = baseOptions;
    // Intercept usage frames on the way out: the session needs the numbers to
    // decide on compaction, and the consumer still gets the frame untouched.
    this.emit = (type: string, fields: object = {}) => {
      if (type === "usage_update") {
        const record = fields as { used?: unknown; size?: unknown };
        const used = Number(record.used || 0);
        const size = Number(record.size || 0);
        if (size > 0) this.lastUsage = { used, size };
      }
      emit(type, fields);
    };
    this.currentModel = baseOptions.model;
    this.currentMode = baseOptions.mode;
    this.currentAutoCompact = baseOptions.autoCompact;
    this.currentAutoCompactWindow = baseOptions.autoCompactWindow;
    this.currentAutoCompactThreshold = baseOptions.autoCompactThreshold;
  }

  async runHumanMessage(
    message: string,
    snapshot?: TurnSnapshot,
    messageId = "",
    deliveryAttempt = 1,
  ): Promise<void> {
    if (this.currentAbort) {
      throw new Error("another turn is already running");
    }
    const options = await this.optionsForTurn(snapshot);
    // The abort handle covers the whole turn — including the optional auto-compact
    // pass ahead of the prompt — so a cancel can interrupt compaction too, not
    // just the main run (currentAbort would otherwise be null while compacting
    // and cancelCurrentTurn would report "nothing running").
    const abortController = new AbortController();
    this.currentAbort = abortController;
    this.currentMessageId = messageId;
    // Retry is bounded by "has this turn produced anything yet": a turn that has
    // already run tool calls has touched the filesystem, so replaying it would
    // repeat those writes. Claude is excluded because its SDK retries per API
    // call, which keeps the completed work — strictly better than restarting the
    // turn, and doing both would multiply the wait.
    const sdkRetries = SDK_COMPACTS.has(options.provider);
    let produced = false;
    const trackingOptions: RunnerOptions = {
      ...options,
      abortController,
      emit: (type: string, fields: object = {}) => {
        if (type === "agent_event") produced = true;
        this.emit(type, fields);
      },
    };
    try {
      await this.maybeCompact({ ...options, abortController });
      this.emit("agent_turn_started", { provider: options.provider, messageId, deliveryAttempt });
      const result = await withTurnRetry(
        () => createRunner(trackingOptions).runPrompt(message),
        {
          // A cancelled turn is never auto-retried. The user may explicitly
          // retry it later, which increments delivery_attempt server-side.
          canRetry: () => !abortController.signal.aborted && !sdkRetries && !produced,
          onRetry: (notice: RetryNotice) => this.emit("llm_call_retry", notice),
        },
      );
      if (result.threadId) {
        this.lastThreadId = result.threadId;
      }
      if (result.transcript) {
        this.transcriptParts.push(result.transcript);
      }
      await writeStoredThread(
        options.stateRoot,
        options.provider,
        result.threadId,
        undefined,
        options.sessionScope,
      );
      this.emit("agent_turn_completed", {
        provider: options.provider,
        threadId: result.threadId,
        finalText: result.finalText,
        messageId,
        deliveryAttempt,
        status: "completed",
      });
    } catch (error) {
      if (abortController.signal.aborted || error instanceof TurnCancelledError) {
        // Cancel is a turn result, not a session result: report it with the
        // message id and return to idle so the stream accepts another turn.
        this.emit("input_status", { messageId, deliveryAttempt, status: "cancelled" });
        this.emit("agent_turn_completed", {
          provider: options.provider,
          messageId,
          deliveryAttempt,
          status: "cancelled",
        });
        return;
      }
      this.emit("input_status", {
        messageId,
        deliveryAttempt,
        status: "failed",
        error: error instanceof Error ? error.message : String(error),
      });
      throw error;
    } finally {
      if (this.currentAbort === abortController) {
        this.currentAbort = null;
        this.currentMessageId = "";
      }
    }
  }

  cancelCurrentTurn(): boolean {
    if (!this.currentAbort) return false;
    this.currentAbort.abort();
    return true;
  }

  /**
   * Run a `/compact` turn first when the context is nearly full.
   *
   * Only for providers whose SDK will not compact itself — doing it for Claude
   * would compact twice. The provider treats `/compact` as an ordinary prompt
   * (that is how the manual button already works), so this is the same mechanism
   * the user has, moved to a turn boundary where the runtime can see the numbers.
   * A failure here is swallowed: refusing to run the user's turn because the
   * optional housekeeping ahead of it failed would be worse than a full context.
   */
  private async maybeCompact(options: RunnerOptions): Promise<void> {
    if (options.autoCompact === false) return;
    if (SDK_COMPACTS.has(options.provider)) return;
    const usage = this.lastUsage;
    if (!usage || usage.size <= 0) return;
    const threshold = options.autoCompactThreshold ?? DEFAULT_AUTO_COMPACT_THRESHOLD;
    if (usage.used / usage.size < threshold) return;
    this.emit("compact_status", {
      status: "started",
      trigger: "auto",
      pre_tokens: usage.used,
    });
    try {
      await createRunner(options).runPrompt("/compact");
      this.emit("compact_status", { status: "ended", trigger: "auto", pre_tokens: usage.used });
    } catch (error) {
      if (options.abortController?.signal.aborted || error instanceof TurnCancelledError) {
        throw new TurnCancelledError("turn cancelled during auto-compact");
      }
      this.emit("compact_status", {
        status: "failed",
        trigger: "auto",
        message: error instanceof Error ? error.message : String(error),
      });
    }
    // The figure is spent either way: on success it is stale, on failure retrying
    // it every turn would stall the session.
    this.lastUsage = null;
  }

  async finish(stopReason: string): Promise<AgentResult> {
    // No live provider process to drain; report the last-known thread id and the
    // accumulated transcript across turns as the terminal result.
    return {
      provider: this.baseOptions.provider,
      threadId: this.lastThreadId,
      stopReason,
      finalText: "",
      transcript: this.transcriptParts.join("\n"),
      stderr: "",
    };
  }

  /**
   * Build the RunnerOptions for this turn by overlaying the snapshot on the
   * start-frame baseline, then re-reading the runtime MCP config from disk (so
   * MCP changes the node wrote between turns take effect immediately). The LLM
   * snapshot is planted into the process env the same way buildPromptRuntimeOptions
   * plants the start-frame LLM, so provider CLIs see the current endpoint/key.
   */
  private async optionsForTurn(snapshot?: TurnSnapshot): Promise<RunnerOptions> {
    const model = snapshot?.model ?? this.currentModel;
    const mode = snapshot?.mode ?? this.currentMode;
    const llm = snapshot?.llm ?? this.currentLlm;
    if (snapshot?.model !== undefined) this.currentModel = snapshot.model;
    if (snapshot?.mode !== undefined) this.currentMode = snapshot.mode;
    if (snapshot?.llm !== undefined) this.currentLlm = snapshot.llm;

    const mcpConfig = await readMCPConfig(this.baseOptions.stateRoot);
    const options: RunnerOptions = {
      ...this.baseOptions,
      model: model || this.baseOptions.model,
      mode: mode || this.baseOptions.mode,
      mcpConfig: mcpConfig.mcps,
      emit: this.emit,
    };
    if (llm) {
      plantLlmEnv(llm);
    }
    return options;
  }
}

function createRunner(options: RunnerOptions): Runner {
  const provider = options.provider;
  if (provider === "codex") return new CodexRunner(options);
  if (provider === "claude") return new ClaudeRunner(options);
  if (provider === "opencode") return new OpenCodeRunner(options);
  if (provider === "cursor") return new CursorAcpRunner(options);
  return new GeminiRunner(options);
}

interface Runner {
  runPrompt(prompt: string): Promise<AgentResult>;
}

/**
 * plantLlmEnv writes the per-turn LLM endpoint/key into process.env so the
 * provider runners (which read process.env at spawn time) see the current
 * service. This mirrors what buildPromptRuntimeOptions does for the start frame.
 */
function plantLlmEnv(llm: NonNullable<TurnSnapshot["llm"]>): void {
  const endpoint = (llm.endpoint || "").trim();
  const key = (llm.apiKey || "").trim();
  if (endpoint) {
    process.env.LLM_API_ENDPOINT = endpoint;
    process.env.OPENAI_BASE_URL = endpoint;
    process.env.ANTHROPIC_BASE_URL = endpoint;
  }
  if (key) {
    process.env.LLM_API_KEY = key;
    process.env.OPENAI_API_KEY = key;
    process.env.ANTHROPIC_API_KEY = key;
    process.env.ANTHROPIC_AUTH_TOKEN = key;
  }
  if (llm.model) {
    process.env.LLM_MODEL = llm.model;
    // Claude Code does not read LLM_MODEL; ANTHROPIC_MODEL is what the CLI uses.
    // Without this a task created with gpt-oss-120b ran as claude-opus-4-8.
    process.env.ANTHROPIC_MODEL = llm.model;
  }
  for (const [k, v] of Object.entries(llm.extra || {})) {
    if (k.trim()) {
      process.env[k.trim()] = v;
    }
  }
}

export async function createInteractiveSession(
  startOptions: InteractiveStartOptions,
  emit: EmitInteractiveFrame,
): Promise<PromptRunnerSession> {
  const options = await buildPromptRuntimeOptions(startOptions);
  const provider = normalizeProvider(options.provider) as Provider;
  const session = new PromptRunnerSession({ ...options, provider }, emit);
  const stored = await readStoredThread(options.stateRoot, provider, options.sessionScope);
  emit("started", {
    provider,
    threadId: stored?.threadId || "",
  });
  return session;
}