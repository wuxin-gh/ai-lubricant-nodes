import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { claudeSettingsLlmEnv } from "../src/runners/claude.js";

/**
 * The settings-env override is the ONLY thing that keeps a task turn
 * authenticated as the task's own child key when HOME is the operator's real
 * HOME (system-env mode): `~/.claude/settings.json` carries an `env` block whose
 * ANTHROPIC_AUTH_TOKEN / ANTHROPIC_BASE_URL outrank the process env, so the
 * turn must pin its own values into the settings layer.
 *
 * The shape is the whole point. The SDK's `Settings` type nests these under
 * `env`; a flat map spread onto the settings root is silently ignored, which is
 * exactly how this regressed once already — the override looked present in the
 * code and did nothing at runtime. These tests pin the nesting.
 */
describe("claudeSettingsLlmEnv", () => {
  const KEYS = ["LLM_API_ENDPOINT", "LLM_API_KEY", "LLM_MODEL"] as const;
  const saved: Record<string, string | undefined> = {};

  beforeEach(() => {
    for (const key of KEYS) {
      saved[key] = process.env[key];
      delete process.env[key];
    }
  });

  afterEach(() => {
    for (const key of KEYS) {
      if (saved[key] === undefined) delete process.env[key];
      else process.env[key] = saved[key];
    }
  });

  it("nests the LLM snapshot under `env`, not at the settings root", () => {
    process.env.LLM_API_ENDPOINT = "http://127.0.0.1:8001";
    process.env.LLM_API_KEY = "sk-task-child";
    process.env.LLM_MODEL = "opus11";

    const settings = claudeSettingsLlmEnv();

    // The nesting is what makes the SDK read these at all.
    expect(Object.keys(settings)).toEqual(["env"]);
    expect(settings.env.ANTHROPIC_BASE_URL).toBe("http://127.0.0.1:8001");
    expect(settings.env.ANTHROPIC_AUTH_TOKEN).toBe("sk-task-child");
    expect(settings.env.ANTHROPIC_API_KEY).toBe("sk-task-child");
    expect(settings.env.ANTHROPIC_MODEL).toBe("opus11");
  });

  it("never leaks a top-level ANTHROPIC_* key (the silent-no-op regression)", () => {
    process.env.LLM_API_KEY = "sk-task-child";

    const settings = claudeSettingsLlmEnv() as Record<string, unknown>;

    expect(settings.ANTHROPIC_AUTH_TOKEN).toBeUndefined();
    expect(settings.ANTHROPIC_API_KEY).toBeUndefined();
  });

  it("omits absent values rather than blanking the operator's own config", () => {
    // No LLM_* planted (native-login session): the block must not carry empty
    // strings, which would override the operator's settings.json with nothing.
    const settings = claudeSettingsLlmEnv();

    expect(settings.env.ANTHROPIC_BASE_URL).toBeUndefined();
    expect(settings.env.ANTHROPIC_AUTH_TOKEN).toBeUndefined();
    expect(settings.env.ANTHROPIC_MODEL).toBeUndefined();
    // The task-privacy switches are pinned unconditionally.
    expect(settings.env.DISABLE_TELEMETRY).toBe("1");
  });
});
