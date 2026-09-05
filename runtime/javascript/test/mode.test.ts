import { describe, expect, it } from "vitest";
import {
  CLAUDE_DEFAULT_MODE,
  CODEX_DEFAULT_MODE,
  GEMINI_DEFAULT_MODE,
  resolveClaudeMode,
  resolveCodexMode,
  resolveGeminiMode,
  resolveOpenCodeSkipPermissions,
} from "../src/runners/mode.js";

describe("per-provider mode resolution", () => {
  describe("codex", () => {
    it("defaults to full-access when mode is empty/unknown (backward compat)", () => {
      for (const mode of ["", undefined, "nonsense"]) {
        const cfg = resolveCodexMode(mode);
        expect(cfg.sandboxMode).toBe("danger-full-access");
        expect(cfg.approvalPolicy).toBe("never");
        expect(cfg.networkAccessEnabled).toBe(true);
      }
      expect(CODEX_DEFAULT_MODE).toBe("danger-full-access");
    });

    it("maps native codex presets", () => {
      expect(resolveCodexMode("read-only")).toEqual({
        sandboxMode: "read-only",
        approvalPolicy: "on-request",
        networkAccessEnabled: false,
      });
      expect(resolveCodexMode("workspace-write").sandboxMode).toBe("workspace-write");
      expect(resolveCodexMode("auto").approvalPolicy).toBe("on-failure");
    });

    it("maps normalized cross-editor aliases", () => {
      expect(resolveCodexMode("readonly").sandboxMode).toBe("read-only");
      expect(resolveCodexMode("plan").sandboxMode).toBe("read-only");
      expect(resolveCodexMode("edit").sandboxMode).toBe("workspace-write");
      expect(resolveCodexMode("full").sandboxMode).toBe("danger-full-access");
      expect(resolveCodexMode("yolo").sandboxMode).toBe("danger-full-access");
    });
  });

  describe("claude", () => {
    it("defaults to bypassPermissions (backward compat)", () => {
      expect(resolveClaudeMode("")).toBe("bypassPermissions");
      expect(resolveClaudeMode(undefined)).toBe("bypassPermissions");
      expect(resolveClaudeMode("nonsense")).toBe("bypassPermissions");
      expect(CLAUDE_DEFAULT_MODE).toBe("bypassPermissions");
    });

    it("passes native permission modes through", () => {
      for (const mode of ["default", "plan", "acceptEdits", "bypassPermissions"]) {
        expect(resolveClaudeMode(mode)).toBe(mode);
      }
    });

    it("maps normalized aliases", () => {
      expect(resolveClaudeMode("readonly")).toBe("plan");
      expect(resolveClaudeMode("edit")).toBe("acceptEdits");
      expect(resolveClaudeMode("full")).toBe("bypassPermissions");
    });
  });

  describe("gemini", () => {
    it("defaults to yolo (backward compat)", () => {
      expect(resolveGeminiMode("")).toBe("yolo");
      expect(resolveGeminiMode(undefined)).toBe("yolo");
      expect(resolveGeminiMode("nonsense")).toBe("yolo");
      expect(GEMINI_DEFAULT_MODE).toBe("yolo");
    });

    it("passes native approval modes through and maps aliases", () => {
      expect(resolveGeminiMode("default")).toBe("default");
      expect(resolveGeminiMode("auto_edit")).toBe("auto_edit");
      expect(resolveGeminiMode("readonly")).toBe("default");
      expect(resolveGeminiMode("plan")).toBe("default");
      expect(resolveGeminiMode("edit")).toBe("auto_edit");
      expect(resolveGeminiMode("full")).toBe("yolo");
    });
  });

  describe("opencode", () => {
    it("skips permissions for full/yolo/empty (backward compat) but gates for read-only/plan/edit", () => {
      expect(resolveOpenCodeSkipPermissions("")).toBe(true);
      expect(resolveOpenCodeSkipPermissions(undefined)).toBe(true);
      expect(resolveOpenCodeSkipPermissions("full")).toBe(true);
      expect(resolveOpenCodeSkipPermissions("yolo")).toBe(true);
      expect(resolveOpenCodeSkipPermissions("readonly")).toBe(false);
      expect(resolveOpenCodeSkipPermissions("plan")).toBe(false);
      expect(resolveOpenCodeSkipPermissions("edit")).toBe(false);
    });
  });
});
