export type Provider = "codex" | "claude" | "gemini" | "opencode" | "cursor";
export type RuntimeJsonSchema = Record<string, unknown>;

export interface AgentResult {
  provider: Provider;
  threadId: string;
  stopReason: string;
  finalText: string;
  transcript: string;
  stderr: string;
}

/**
 * Emits one output frame on the stream protocol. Structurally identical to
 * interactive.ts's EmitInteractiveFrame; declared here so runners can take it
 * without types.ts importing from interactive.ts (which imports types.ts).
 */
export type RuntimeEmit = (type: string, fields?: object) => void;

export interface RunnerOptions {
  provider: Provider;
  model?: string;
  // Provider-native permission/approval mode string (e.g. codex
  // "workspace-write", claude "plan", gemini "auto_edit"). Empty resolves to the
  // provider's historical all-open default. See runners/mode.ts.
  mode?: string;
  stateRoot: string;
  sessionScope?: string;
  workspace: string;
  home: string;
  runtimeRoot: string;
  systemContext: string;
  mcpConfig?: Record<string, unknown>;
  skills?: string[];
  outputSchema?: RuntimeJsonSchema;
  /**
   * Whether the provider should compact its context automatically as it fills.
   * Undefined means "on" — the safe default for a long-running session — but it
   * stays a tri-state so a task can switch it off for a provider whose
   * compaction it does not want. Claude hands this to its SDK; the providers
   * without SDK support get a threshold-driven `/compact` turn instead.
   */
  autoCompact?: boolean;
  /** Token budget to compact toward. 0 / undefined leaves the provider default. */
  autoCompactWindow?: number;
  /**
   * Fraction of the context window at which the runtime injects its own
   * `/compact` turn, for providers whose SDK will not do it. Undefined uses
   * DEFAULT_AUTO_COMPACT_THRESHOLD.
   */
  autoCompactThreshold?: number;
  // Set only in stream mode: lets a runner emit structured frames (agent_event)
  // alongside the transcript. Absent for one-shot prompt runs, so every runner
  // must treat it as optional.
  emit?: RuntimeEmit;
  /**
   * Cancels THIS turn when aborted. Set by the interactive session per turn;
   * runners that have no native abort support fall back to killing the spawned
   * provider child process. Aborting must make runPrompt reject with a
   * TurnCancelledError (or an equivalent rejected/short result) — never hang.
   */
  abortController?: AbortController;
}

export interface StoredThread {
  provider: string;
  threadId: string;
  updatedAt?: string;
}
