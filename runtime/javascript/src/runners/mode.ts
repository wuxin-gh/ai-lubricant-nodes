// Per-provider permission/approval mode resolution. Each editor exposes its own
// vocabulary for how much autonomy the agent gets (sandboxing, approval prompts,
// edit acceptance). The node passes a provider-native mode string through from
// the server; this module maps it to the concrete runner settings.
//
// Backward compatibility: an empty/unknown mode resolves to the historical
// all-open default for that provider, so callers that never set a mode keep the
// previous behavior (codex danger-full-access, claude bypassPermissions, gemini
// yolo, opencode skip-permissions).

// ── codex ─────────────────────────────────────────────────────────────────────
// codex couples two dimensions: sandboxMode (filesystem/network reach) and
// approvalPolicy (when it pauses for a human). The mode key selects a preset
// spanning read-only → full-access.
export interface CodexModeConfig {
  sandboxMode: string;
  approvalPolicy: string;
  networkAccessEnabled: boolean;
}

const CODEX_MODES: Record<string, CodexModeConfig> = {
  "read-only": { sandboxMode: "read-only", approvalPolicy: "on-request", networkAccessEnabled: false },
  "workspace-write": { sandboxMode: "workspace-write", approvalPolicy: "on-request", networkAccessEnabled: true },
  auto: { sandboxMode: "workspace-write", approvalPolicy: "on-failure", networkAccessEnabled: true },
  "danger-full-access": { sandboxMode: "danger-full-access", approvalPolicy: "never", networkAccessEnabled: true },
};

// Aliases map cross-editor normalized names onto codex presets.
const CODEX_ALIASES: Record<string, string> = {
  readonly: "read-only",
  plan: "read-only",
  edit: "workspace-write",
  full: "danger-full-access",
  yolo: "danger-full-access",
};

export const CODEX_DEFAULT_MODE = "danger-full-access";

export function resolveCodexMode(mode?: string): CodexModeConfig {
  const key = (mode || "").trim();
  const resolved = CODEX_MODES[key] || CODEX_MODES[CODEX_ALIASES[key] || ""] || CODEX_MODES[CODEX_DEFAULT_MODE];
  return resolved;
}

// ── claude ──────────────────────────────────────────────────────────────────
// claude uses a single permissionMode enum. allowDangerouslySkipPermissions is
// only meaningful with bypassPermissions.
const CLAUDE_MODES = new Set(["default", "plan", "acceptEdits", "bypassPermissions"]);
const CLAUDE_ALIASES: Record<string, string> = {
  readonly: "plan",
  "read-only": "plan",
  edit: "acceptEdits",
  auto: "acceptEdits",
  full: "bypassPermissions",
  "danger-full-access": "bypassPermissions",
  yolo: "bypassPermissions",
};

export const CLAUDE_DEFAULT_MODE = "bypassPermissions";

export function resolveClaudeMode(mode?: string): string {
  const key = (mode || "").trim();
  if (CLAUDE_MODES.has(key)) {
    return key;
  }
  return CLAUDE_ALIASES[key] || CLAUDE_DEFAULT_MODE;
}

// ── gemini ────────────────────────────────────────────────────────────────────
// gemini's --approval-mode flag: default (prompt), auto_edit (auto-approve edits),
// yolo (auto-approve everything).
const GEMINI_MODES = new Set(["default", "auto_edit", "yolo"]);
const GEMINI_ALIASES: Record<string, string> = {
  readonly: "default",
  "read-only": "default",
  plan: "default",
  edit: "auto_edit",
  auto: "auto_edit",
  full: "yolo",
  "danger-full-access": "yolo",
};

export const GEMINI_DEFAULT_MODE = "yolo";

export function resolveGeminiMode(mode?: string): string {
  const key = (mode || "").trim();
  if (GEMINI_MODES.has(key)) {
    return key;
  }
  return GEMINI_ALIASES[key] || GEMINI_DEFAULT_MODE;
}

// ── opencode ──────────────────────────────────────────────────────────────────
// opencode's CLI exposes --dangerously-skip-permissions (no prompts). Absent the
// flag, opencode falls back to its config's permission gating. We model this as a
// boolean: full/yolo skip permissions (default for backward compat), read-only /
// plan / edit leave the flag off so opencode's own permission prompts apply.
const OPENCODE_SKIP_MODES = new Set(["full", "danger-full-access", "yolo", ""]);

export function resolveOpenCodeSkipPermissions(mode?: string): boolean {
  const key = (mode || "").trim();
  return OPENCODE_SKIP_MODES.has(key);
}

// ── cursor (ACP) ──────────────────────────────────────────────────────────────
// Cursor ACP sessions accept the same core modes as its CLI: agent (full tool
// access, the default matching the runtime's unattended all-open posture), plan
// (read-only planning) and ask (read-only Q&A).
const CURSOR_MODES = new Set(["agent", "plan", "ask"]);
const CURSOR_ALIASES: Record<string, string> = {
  readonly: "plan",
  "read-only": "plan",
  edit: "agent",
  auto: "agent",
  full: "agent",
  "danger-full-access": "agent",
  yolo: "agent",
};

export const CURSOR_DEFAULT_MODE = "agent";

export function resolveCursorMode(mode?: string): string {
  const key = (mode || "").trim();
  if (CURSOR_MODES.has(key)) {
    return key;
  }
  return CURSOR_ALIASES[key] || CURSOR_DEFAULT_MODE;
}
