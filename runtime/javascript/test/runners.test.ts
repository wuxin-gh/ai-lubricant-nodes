import { afterEach, describe, expect, it, vi } from "vitest";
import fs from "node:fs/promises";
import path from "node:path";
import { ClaudeRunner } from "../src/runners/claude.js";
import { CodexRunner } from "../src/runners/codex.js";
import { GeminiRunner } from "../src/runners/gemini.js";
import { OpenCodeRunner } from "../src/runners/opencode.js";
import { captureStdio, runnerOptions, withTempSession } from "./helpers.js";

afterEach(() => {
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
});

describe("CodexRunner", () => {
  it("exposes Codex thread options without constructor-only config", async () => {
    await withTempSession(async (root) => {
      const systemContext = "## MPI Catalog\n\ncatalog body";
      const runner = new CodexRunner(runnerOptions(root, systemContext));

      const threadOptions = runner.threadOptions();

      expect(threadOptions.additionalDirectories).toEqual([
        `${root}/state`,
        `${root}/home`,
        `${root}/runtime`,
      ]);
      expect(threadOptions).not.toHaveProperty("config");
    });
  });

  it("translates common events into transcript and final text", async () => {
    await withTempSession(async (root) => {
      const runner = new CodexRunner(runnerOptions(root));
      const result = {
        provider: "codex" as const,
        threadId: "",
        stopReason: "completed",
        finalText: "",
        transcript: "",
        stderr: "",
      };
      const stdio = captureStdio();
      try {
        runner.handleEvent({ type: "thread.started", thread_id: "thread-1" }, result);
        runner.handleEvent({ type: "item.updated", item: { id: "m1", type: "agent_message", text: "hello" } }, result);
        runner.handleEvent({ type: "item.completed", item: { id: "m1", type: "agent_message", text: "hello!" } }, result);
        runner.handleEvent({ type: "item.updated", item: { id: "r1", type: "reasoning", text: " because" } }, result);
        runner.handleEvent({
          type: "item.updated",
          item: { id: "cmd", type: "command_execution", command: "pwd", aggregated_output: "/work\n" },
        }, result);
        runner.handleEvent({
          type: "item.updated",
          item: { id: "cmd", type: "command_execution", command: "pwd", aggregated_output: "/work\nnext\n" },
        }, result);
        runner.handleEvent({
          type: "item.completed",
          item: { id: "fc", type: "file_change", changes: [{ kind: "edit", path: "a.ts" }] },
        }, result);
        runner.handleEvent({
          type: "item.completed",
          item: {
            id: "mcp",
            type: "mcp_tool_call",
            server: "srv",
            tool: "lookup",
            arguments: { query: "docs" },
            status: "completed",
            result: { text: "mcp result" },
          },
        }, result);
        runner.handleEvent({
          type: "item.completed",
          item: { id: "mcp2", type: "mcp_tool_call", server: "srv", tool: "lookup", status: "failed", error: { message: "mcp failed" } },
        }, result);
        runner.handleEvent({
          type: "item.updated",
          item: { id: "todo", type: "todo_list", items: [{ text: "one", completed: true }, { text: "two", completed: false }] },
        }, result);
        runner.handleEvent({ type: "item.completed", item: { id: "err", type: "error", message: "item bad" } }, result);
        runner.handleEvent({ type: "item.completed", item: { id: "unknown", type: "new_type" } }, result);
        runner.handleEvent({ type: "item.completed" }, result);
        runner.handleEvent({ type: "item.started", item: { id: "ws", type: "web_search" } }, result);
        runner.handleEvent({ type: "item.completed", item: { id: "ws", type: "web_search", action: { query: "docs" } } }, result);
      } finally {
        stdio.restore();
      }

      expect(result.threadId).toBe("thread-1");
      expect(result.finalText).toBe("hello!");
      expect(stdio.stderr).toContain("hello");
      expect(stdio.stderr).toContain("$ pwd");
      expect(stdio.stderr).toContain("[file_change]");
      expect(stdio.stderr).toContain("edit: a.ts");
      expect(stdio.stderr).toContain("[mcp:srv/lookup]");
      expect(stdio.stderr).toContain("\"query\": \"docs\"");
      expect(stdio.stderr).toContain("mcp result");
      expect(stdio.stderr).toContain("[mcp:srv/lookup]\n{\n  \"query\": \"docs\"\n}\nmcp result");
      expect(stdio.stderr).toContain("mcp failed");
      expect(stdio.stderr).toContain("[todo]");
      expect(stdio.stderr).toContain("[x] one");
      expect(stdio.stderr).toContain("item bad");
      expect(stdio.stderr).toContain("[web_search] docs");
      expect(stdio.stderr).not.toContain("[web_search] \n");
    });
  });

  it("skips duplicate and empty Codex event payloads", async () => {
    await withTempSession(async (root) => {
      const runner = new CodexRunner(runnerOptions(root));
      const result = {
        provider: "codex" as const,
        threadId: "",
        stopReason: "completed",
        finalText: "",
        transcript: "",
        stderr: "",
      };
      const stdio = captureStdio();
      try {
        runner.emitFileChange({ id: "empty", changes: [] });
        runner.emitFileChange({ id: "fc", changes: [{ kind: "create", path: "a" }] });
        runner.emitFileChange({ id: "fc", changes: [{ kind: "create", path: "a" }] });
        runner.emitMcp({ id: "mcp", server: "srv", tool: "tool", status: "completed", result: { text: "" } });
        runner.emitMcp({ id: "mcp", server: "srv", tool: "tool", status: "completed", result: { text: "ignored" } });
        runner.emitMcp({ id: "mcp2", server: "srv", tool: "tool", status: "failed", error: {} });
        runner.emitTodo({ id: "todo-empty", items: [] });
        runner.handleEvent({ type: "item.completed", item: { id: "msg", type: "agent_message", text: "" } }, result);
      } finally {
        stdio.restore();
      }

      expect(stdio.stderr.match(/\[file_change\]/g)).toHaveLength(1);
      expect(stdio.stderr).toContain("[mcp:srv/tool]");
      expect(result.finalText).toBe("");
    });
  });

  it("keeps the Codex web search marker when no query is exposed", async () => {
    await withTempSession(async (root) => {
      const runner = new CodexRunner(runnerOptions(root));
      const result = {
        provider: "codex" as const,
        threadId: "",
        stopReason: "completed",
        finalText: "",
        transcript: "",
        stderr: "",
      };
      const stdio = captureStdio();
      try {
        runner.handleEvent({ type: "item.started", item: { id: "ws", type: "web_search" } }, result);
        runner.handleEvent({ type: "item.completed", item: { id: "ws", type: "web_search" } }, result);
      } finally {
        stdio.restore();
      }

      expect(stdio.stderr).toBe("\n[web_search] \n");
    });
  });

  it("throws turn failure messages", async () => {
    await withTempSession(async (root) => {
      const runner = new CodexRunner(runnerOptions(root));
      const result = {
        provider: "codex" as const,
        threadId: "",
        stopReason: "completed",
        finalText: "",
        transcript: "",
        stderr: "",
      };

      expect(() => runner.handleEvent({ type: "turn.failed", error: { message: "bad" } }, result)).toThrow("bad");
    });
  });
});

describe("ClaudeRunner", () => {
  it("bridges generic LLM settings into Claude SDK environment variables", async () => {
    await withTempSession(async (root) => {
      vi.stubEnv("LLM_API_KEY", "generic-key");
      vi.stubEnv("LLM_API_ENDPOINT", "https://llm.example.invalid");
      vi.stubEnv("ANTHROPIC_API_KEY", "");
      vi.stubEnv("ANTHROPIC_BASE_URL", "");
      const runner = new ClaudeRunner(runnerOptions(root));

      const env = runner.queryOptions(null).env as NodeJS.ProcessEnv;

      expect(env.ANTHROPIC_API_KEY).toBe("generic-key");
      expect(env.ANTHROPIC_BASE_URL).toBe("https://llm.example.invalid");
    });
  });

  it("keeps explicit Claude SDK environment variables over generic LLM settings", async () => {
    await withTempSession(async (root) => {
      vi.stubEnv("LLM_API_KEY", "generic-key");
      vi.stubEnv("LLM_API_ENDPOINT", "https://llm.example.invalid");
      vi.stubEnv("ANTHROPIC_API_KEY", "anthropic-key");
      vi.stubEnv("ANTHROPIC_BASE_URL", "https://anthropic.example.invalid");
      const runner = new ClaudeRunner(runnerOptions(root));

      const env = runner.queryOptions(null).env as NodeJS.ProcessEnv;

      expect(env.ANTHROPIC_API_KEY).toBe("anthropic-key");
      expect(env.ANTHROPIC_BASE_URL).toBe("https://anthropic.example.invalid");
    });
  });

  it("passes explicit Claude executable paths and bypass permissions for non-root users", async () => {
    await withTempSession(async (root) => {
      vi.stubEnv("CLAUDE_CODE_EXECUTABLE", "/custom/bin/claude");
      vi.spyOn(process, "getuid").mockReturnValue(501);
      const runner = new ClaudeRunner(runnerOptions(root));

      expect(runner.queryOptions(null)).toMatchObject({
        pathToClaudeCodeExecutable: "/custom/bin/claude",
        permissionMode: "bypassPermissions",
        allowDangerouslySkipPermissions: true,
      });
    });
  });

  it("passes executable paths and bypass permissions when running as root", async () => {
    await withTempSession(async (root) => {
      vi.stubEnv("CLAUDE_CODE_EXECUTABLE", "/usr/bin/claude");
      vi.spyOn(process, "getuid").mockReturnValue(0);
      const runner = new ClaudeRunner(runnerOptions(root));

      expect(runner.queryOptions(null)).toMatchObject({
        pathToClaudeCodeExecutable: "/usr/bin/claude",
        permissionMode: "bypassPermissions",
        allowDangerouslySkipPermissions: true,
      });
    });
  });

  it("appends system context and exposes runtime directory", async () => {
    await withTempSession(async (root) => {
      const systemContext = "## MPI Catalog\n\ncatalog body";
      const runner = new ClaudeRunner({ ...runnerOptions(root, systemContext), skills: ["pdf"] });

      const queryOptions = runner.queryOptions({
        provider: "claude",
        threadId: "existing-session",
      });

      expect(queryOptions.additionalDirectories).toEqual([
        `${root}/state`,
        `${root}/home`,
        `${root}/runtime`,
      ]);
      expect(queryOptions.systemPrompt).toEqual({
        type: "preset",
        preset: "claude_code",
        append: systemContext,
      });
      expect(queryOptions.resume).toBe("existing-session");
      expect(queryOptions.settingSources).toEqual(["user"]);
      expect(queryOptions.skills).toEqual(["pdf"]);
    });
  });

  it("flattens Claude MCP env and headers for SDK options", async () => {
    await withTempSession(async (root) => {
      const runner = new ClaudeRunner({
        ...runnerOptions(root, "", "claude"),
        mcpConfig: {
          localFs: {
            type: "local",
            command: "npx",
            args: ["-y", "server"],
            env: {
              TOKEN: { value: "secret", secret: true },
            },
          },
          docs: {
            type: "remote",
            transport: "http",
            url: "https://docs.example.invalid/mcp",
            headers: {
              Authorization: { value: "Bearer token", secret: true },
            },
          },
        },
      });

      expect(runner.queryOptions(null)).toMatchObject({
        strictMcpConfig: true,
        mcpServers: {
          localFs: {
            type: "stdio",
            command: "npx",
            args: ["-y", "server"],
            env: {
              TOKEN: "secret",
            },
          },
          docs: {
            type: "http",
            url: "https://docs.example.invalid/mcp",
            headers: {
              Authorization: "Bearer token",
            },
          },
        },
      });
    });
  });

  it("handles stream text and tool starts", async () => {
    await withTempSession(async (root) => {
      const runner = new ClaudeRunner(runnerOptions(root));
      const stdio = captureStdio();
      try {
        runner.handleStreamEvent({
          event: { type: "content_block_start", content_block: { name: "Edit" } },
        });
        runner.handleStreamEvent({
          event: { type: "content_block_delta", delta: { type: "text_delta", text: "answer" } },
        });
        runner.handleStreamEvent({
          event: { type: "content_block_delta", delta: { type: "thinking_delta", thinking: "thinking" } },
        });
      } finally {
        stdio.restore();
      }

      expect(stdio.stderr).toContain("[tool:Edit]");
      expect(stdio.stderr).toContain("answer");
      expect(stdio.stderr).toContain("thinking");
    });
  });

  it("forwards Claude SDK messages with canonical sub-agent attribution", async () => {
    await withTempSession(async (root) => {
      const emitted: Array<{ type: string; fields?: Record<string, unknown> }> = [];
      const runner = new ClaudeRunner({
        ...runnerOptions(root),
        emit: (type, fields) => emitted.push({ type, fields: fields as Record<string, unknown> }),
      });

      // Root agent: parent_tool_use_id null -> agent_id/subagent_id empty.
      // Per-token deltas (`stream_event`) are never forwarded raw — the
      // completed assistant message carries the full text as a normalized item.
      const rootMessage = {
        type: "stream_event",
        parent_tool_use_id: null,
        uuid: "u-root",
        event: { type: "content_block_delta", delta: { type: "text_delta", text: "root" } },
      };
      // Sub-agent: the SDK already carries attribution; we must not rewrite it.
      const childMessage = {
        type: "assistant",
        parent_tool_use_id: "call_abc",
        subagent_type: "researcher",
        task_description: "inspect files",
        uuid: "u-child",
        message: { content: [{ type: "text", text: "child" }] },
      };
      const rootAssistant = {
        type: "assistant",
        parent_tool_use_id: null,
        uuid: "u-root-a",
        message: { content: [{ type: "text", text: "root full" }] },
      };

      runner.forwardMessage(rootMessage);
      runner.forwardMessage(rootAssistant);
      runner.forwardMessage(childMessage);

      // The delta yields nothing; each assistant message yields exactly one
      // agent_event whose canonical fields are promoted to the frame top level.
      const events = emitted.filter((entry) => entry.type === "agent_event");
      expect(events).toHaveLength(2);
      // Root: empty subagent_id = main transcript.
      expect(events[0].fields?.agent_id).toBe("");
      expect(events[0].fields?.subagent_id).toBe("");
      const rootItem = events[0].fields?.item as Record<string, unknown>;
      expect(rootItem.type).toBe("agent_message");
      expect(rootItem.text).toBe("root full");
      expect(rootItem.logical_event_id).toBe("u-root-a:0");
      expect(rootItem.event_kind).toBe("message");
      // Child: non-empty subagent_id = sub-agent detail only.
      expect(events[1].fields?.agent_id).toBe("call_abc");
      expect(events[1].fields?.subagent_id).toBe("call_abc");
      const childItem = events[1].fields?.item as Record<string, unknown>;
      expect(childItem.text).toBe("child");
      expect(childItem.agent_name).toBe("researcher");
      expect(childItem.task).toBe("inspect files");
    });
  });

  it("does not forward Claude messages outside stream mode", async () => {
    await withTempSession(async (root) => {
      // One-shot prompt runs have no emit; forwarding must stay a no-op.
      const runner = new ClaudeRunner(runnerOptions(root));
      expect(() => runner.forwardMessage({ type: "assistant", parent_tool_use_id: "call_abc" })).not.toThrow();
    });
  });

  it("emits a canonical envelope where a tool call's sparse completion still routes", async () => {
    await withTempSession(async (root) => {
      const emitted: Array<{ type: string; fields?: Record<string, unknown> }> = [];
      const runner = new ClaudeRunner({
        ...runnerOptions(root),
        emit: (type, fields) => emitted.push({ type, fields: fields as Record<string, unknown> }),
      });

      // Opening: assistant tool_use carries the tool name + input.
      runner.forwardMessage({
        type: "assistant",
        uuid: "u-tool",
        message: { content: [{ type: "tool_use", id: "toolu_01", name: "Edit", input: { file_path: "a.ts" } }] },
      });
      // Completion: the SDK reports tool results on `user` messages carrying
      // ONLY {tool_use_id, content, is_error} — no tool name, no input.
      runner.forwardMessage({
        type: "user",
        uuid: "u-tool-result",
        message: { content: [{ type: "tool_result", tool_use_id: "toolu_01", content: "applied" }] },
      });

      const events = emitted.filter((entry) => entry.type === "agent_event");
      expect(events).toHaveLength(2);
      const [open, close] = events.map((entry) => entry.fields as Record<string, unknown>);
      // Both halves share the logical lifecycle id = the tool_use id.
      expect(open.logical_event_id).toBe("toolu_01");
      expect(close.logical_event_id).toBe("toolu_01");
      // The sparse completion still names the tool — recovered from the
      // session's opening report, so any single frame alone can route.
      expect(close.tool_name).toBe("Edit");
      const closeItem = close.item as Record<string, unknown>;
      expect(closeItem.title).toBe("Edit");
      expect(closeItem.status).toBe("done");
      expect(closeItem.phase).toBe("complete");
      // Root attribution on both halves.
      expect(open.subagent_id).toBe("");
      expect(close.subagent_id).toBe("");
    });
  });

  it("prints Claude tool input after streamed JSON deltas complete", async () => {
    await withTempSession(async (root) => {
      const runner = new ClaudeRunner(runnerOptions(root));
      const stdio = captureStdio();
      try {
        runner.handleStreamEvent({
          event: { type: "content_block_start", index: 0, content_block: { type: "tool_use", name: "WebSearch", input: {} } },
        });
        runner.handleStreamEvent({
          event: { type: "content_block_delta", index: 0, delta: { type: "input_json_delta", partial_json: "{\"query\":" } },
        });
        runner.handleStreamEvent({
          event: { type: "content_block_delta", index: 0, delta: { type: "input_json_delta", partial_json: "\"baidu news\"}" } },
        });
        expect(stdio.stderr).not.toContain("[tool:WebSearch]");
        runner.handleStreamEvent({ event: { type: "content_block_stop", index: 0 } });
      } finally {
        stdio.restore();
      }

      expect(stdio.stderr).toContain("[tool:WebSearch]");
      expect(stdio.stderr).toContain("\"query\": \"baidu news\"");
      expect(stdio.stderr).not.toContain("{}");
    });
  });

  it("ignores unrelated Claude stream events", async () => {
    await withTempSession(async (root) => {
      const runner = new ClaudeRunner(runnerOptions(root));
      const stdio = captureStdio();
      try {
        runner.handleStreamEvent({});
        runner.handleStreamEvent({ event: { type: "content_block_start", content_block: {} } });
        runner.handleStreamEvent({ event: { type: "message_stop" } });
        runner.handleStreamEvent({ event: { type: "content_block_delta", delta: { type: "other" } } });
      } finally {
        stdio.restore();
      }
      expect(stdio.stderr).toBe("");
    });
  });
});

describe("GeminiRunner", () => {
  it("can be constructed with compatible options", async () => {
    await withTempSession(async (root) => {
      const runner = new GeminiRunner(runnerOptions(root, "", "gemini"));
      expect(runner).toBeInstanceOf(GeminiRunner);
    });
  });

  it("removes stale Gemini MCP settings when current config is empty", async () => {
    await withTempSession(async (root) => {
      const settingsDir = path.join(root, "home", ".gemini");
      await fs.mkdir(settingsDir, { recursive: true });
      const settingsPath = path.join(settingsDir, "settings.json");
      await fs.writeFile(settingsPath, JSON.stringify({ theme: "dark", mcpServers: { stale: { url: "http://stale" } } }, null, 2) + "\n", "utf-8");

      const runner = new GeminiRunner({
        ...runnerOptions(root, "", "gemini"),
        mcpConfig: {},
      });
      await runner.writeSettingsFile();

      const settings = JSON.parse(await fs.readFile(settingsPath, "utf-8")) as Record<string, unknown>;
      expect(settings.theme).toBe("dark");
      expect(settings).not.toHaveProperty("mcpServers");
    });
  });
});

describe("OpenCodeRunner", () => {
  it("builds non-interactive args for fresh and resumed sessions", async () => {
    await withTempSession(async (root) => {
      const runner = new OpenCodeRunner({
        ...runnerOptions(root, "be concise", "opencode"),
        model: "anthropic/claude-sonnet-4-5",
      });

      expect(runner.buildArgs("review", null)).toEqual([
        "run",
        "be concise\n\nreview",
        "--format",
        "json",
        "--dir",
        `${root}/workspace`,
        "--dangerously-skip-permissions",
        "--model",
        "anthropic/claude-sonnet-4-5",
      ]);
      expect(runner.buildArgs("review", { provider: "opencode", threadId: "ses_1" })).toContain("ses_1");
    });
  });

  it("keeps explicit OpenCode auto-update environment overrides", async () => {
    await withTempSession(async (root) => {
      vi.stubEnv("OPENCODE_DISABLE_AUTOUPDATE", "false");
      const runner = new OpenCodeRunner(runnerOptions(root, "", "opencode"));

      await expect(runner.environment()).resolves.toMatchObject({ OPENCODE_DISABLE_AUTOUPDATE: "false" });
    });
  });

  it("disables OpenCode model catalog fetch by default", async () => {
    await withTempSession(async (root) => {
      const runner = new OpenCodeRunner(runnerOptions(root, "", "opencode"));

      await expect(runner.environment()).resolves.toMatchObject({ OPENCODE_DISABLE_MODELS_FETCH: "1" });

      vi.stubEnv("OPENCODE_DISABLE_MODELS_FETCH", "0");
      await expect(runner.environment()).resolves.toMatchObject({ OPENCODE_DISABLE_MODELS_FETCH: "0" });
    });
  });

  it("writes run-scoped OpenCode skills config", async () => {
    await withTempSession(async (root) => {
      const runner = new OpenCodeRunner({ ...runnerOptions(root, "", "opencode"), skills: ["pdf"] });

      const env = await runner.environment();

      expect(env.OPENCODE_CONFIG).toBeTruthy();
      const config = JSON.parse(await import("node:fs/promises").then((fs) => fs.readFile(String(env.OPENCODE_CONFIG), "utf8")));
      expect(config.skills.paths).toEqual([`${root}/home/.agents/skills`]);
    });
  });

  it("cleans temporary OpenCode skills config", async () => {
    await withTempSession(async (root) => {
      const runner = new OpenCodeRunner({ ...runnerOptions(root, "", "opencode"), skills: ["pdf"] });

      const env = await runner.environment();
      const configPath = String(env.OPENCODE_CONFIG);
      await expect(fs.stat(configPath)).resolves.toBeTruthy();

      await runner.cleanupSkillsConfig();

      await expect(fs.stat(path.dirname(configPath))).rejects.toMatchObject({ code: "ENOENT" });
    });
  });

  it("does not throw when OpenCode skills cleanup fails", async () => {
    await withTempSession(async (root) => {
      const runner = new OpenCodeRunner({ ...runnerOptions(root, "", "opencode"), skills: ["pdf"] });
      await runner.environment();
      const rmSpy = vi.spyOn(fs, "rm").mockRejectedValueOnce(new Error("busy"));
      const stdio = captureStdio();
      try {
        await expect(runner.cleanupSkillsConfig()).resolves.toBeUndefined();
      } finally {
        stdio.restore();
        rmSpy.mockRestore();
      }
      expect(stdio.stderr).toContain("[opencode cleanup]");
      expect(stdio.stderr).toContain("busy");
    });
  });

  it("merges OpenCode skills config with an existing config file", async () => {
    await withTempSession(async (root) => {
      const fs = await import("node:fs/promises");
      const path = await import("node:path");
      const baseConfigPath = path.join(root, "home", ".config", "opencode", "opencode.json");
      await fs.mkdir(path.dirname(baseConfigPath), { recursive: true });
      await fs.writeFile(baseConfigPath, JSON.stringify({
        "$schema": "https://opencode.ai/config.json",
        provider: { local: { npm: "@ai-sdk/openai-compatible" } },
        model: "local/test",
        skills: { paths: ["/existing/skills"] },
      }), "utf8");
      vi.stubEnv("OPENCODE_CONFIG", baseConfigPath);
      const runner = new OpenCodeRunner({ ...runnerOptions(root, "", "opencode"), skills: ["pdf"] });

      const env = await runner.environment();

      expect(env.OPENCODE_CONFIG).not.toBe(baseConfigPath);
      const config = JSON.parse(await fs.readFile(String(env.OPENCODE_CONFIG), "utf8"));
      expect(config.provider).toEqual({ local: { npm: "@ai-sdk/openai-compatible" } });
      expect(config.model).toBe("local/test");
      expect(config.skills.paths).toEqual(["/existing/skills", `${root}/home/.agents/skills`]);
    });
  });

  it("merges OpenCode skills config with the default home config", async () => {
    await withTempSession(async (root) => {
      const fs = await import("node:fs/promises");
      const path = await import("node:path");
      const baseConfigPath = path.join(root, "home", ".config", "opencode", "opencode.json");
      await fs.mkdir(path.dirname(baseConfigPath), { recursive: true });
      await fs.writeFile(baseConfigPath, JSON.stringify({
        "$schema": "https://opencode.ai/config.json",
        provider: { openai: { npm: "@ai-sdk/openai" } },
        model: "openai/gpt-test",
      }), "utf8");
      vi.stubEnv("OPENCODE_CONFIG", "");
      const runner = new OpenCodeRunner({ ...runnerOptions(root, "", "opencode"), skills: ["pdf"] });

      const env = await runner.environment();

      expect(env.OPENCODE_CONFIG).not.toBe(baseConfigPath);
      const config = JSON.parse(await fs.readFile(String(env.OPENCODE_CONFIG), "utf8"));
      expect(config.provider).toEqual({ openai: { npm: "@ai-sdk/openai" } });
      expect(config.model).toBe("openai/gpt-test");
      expect(config.skills.paths).toEqual([`${root}/home/.agents/skills`]);
    });
  });

  it("translates OpenCode JSON events into transcript and final text", async () => {
    await withTempSession(async (root) => {
      const runner = new OpenCodeRunner(runnerOptions(root, "", "opencode"));
      const result = {
        provider: "opencode" as const,
        threadId: "",
        stopReason: "completed",
        finalText: "",
        transcript: "",
        stderr: "",
      };
      const stdio = captureStdio();
      try {
        runner.handleEvent({ type: "message", sessionId: "session-1", message: { content: "hello" } }, result);
        runner.handleEvent({ type: "text", part: { type: "text", text: " from part" } }, result);
        runner.handleEvent({ type: "tool_use", name: "bash" }, result);
        runner.handleEvent({ type: "tool_result", result: { text: "tool output" } }, result);
        runner.handleEvent({ type: "result", response: "done", stopReason: "completed" }, result);
      } finally {
        stdio.restore();
      }

      expect(result.threadId).toBe("session-1");
      expect(result.finalText).toBe("done");
      expect(stdio.stderr).toContain("hello");
      expect(stdio.stderr).toContain(" from part");
      expect(stdio.stderr).toContain("[tool:bash]");
      expect(stdio.stderr).toContain("tool output");
    });
  });

  it("throws OpenCode event errors and rejects structured output", async () => {
    await withTempSession(async (root) => {
      const runner = new OpenCodeRunner(runnerOptions(root, "", "opencode"));
      expect(() => runner.handleEvent({ type: "error", error: { text: "bad" } }, {
        provider: "opencode",
        threadId: "",
        stopReason: "completed",
        finalText: "",
        transcript: "",
        stderr: "",
      })).toThrow("bad");

      const schemaRunner = new OpenCodeRunner({
        ...runnerOptions(root, "", "opencode"),
        outputSchema: { type: "object" },
      });
      await expect(schemaRunner.runPrompt("hello")).rejects.toThrow("structured JSON output is not supported by opencode runner");
    });
  });
});
