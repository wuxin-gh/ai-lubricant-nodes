import fs from "node:fs/promises";
import path from "node:path";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { captureStdio, runnerOptions, withTempSession } from "./helpers.js";

type Request = { id?: number; method?: string; params?: any; result?: unknown };
type Script = (request: Request, child: FakeChild) => void;

class FakeChild extends EventEmitter {
  stdout = new PassThrough();
  stderr = new PassThrough();
  stdin = new PassThrough();
  killed = false;
  private buffer = "";

  constructor(private readonly script: Script) {
    super();
    this.stdin.on("data", (chunk: Buffer) => {
      acpState.requests.push(JSON.parse(chunk.toString("utf8")) as Request);
      this.buffer += chunk.toString("utf8");
      let newline: number;
      while ((newline = this.buffer.indexOf("\n")) >= 0) {
        const line = this.buffer.slice(0, newline);
        this.buffer = this.buffer.slice(newline + 1);
        if (line.trim()) this.script(JSON.parse(line) as Request, this);
      }
    });
  }

  reply(id: number, result: unknown) { this.stdout.write(`${JSON.stringify({ jsonrpc: "2.0", id, result })}\n`); }
  error(id: number, message: string) { this.stdout.write(`${JSON.stringify({ jsonrpc: "2.0", id, error: { message } })}\n`); }
  notify(method: string, params: unknown) { this.stdout.write(`${JSON.stringify({ jsonrpc: "2.0", method, params })}\n`); }
  request(method: string, params: unknown, id = 9000) { this.stdout.write(`${JSON.stringify({ jsonrpc: "2.0", id, method, params })}\n`); }
  kill() { this.killed = true; this.stdout.end(); this.stderr.end(); }
}

const acpState = vi.hoisted(() => ({
  requests: [] as Request[],
  children: [] as FakeChild[],
  script: null as Script | null,
}));

vi.mock("node:child_process", async (importOriginal) => ({
  ...(await importOriginal<typeof import("node:child_process")>()),
  spawn: vi.fn((command: string, args: string[], options: Record<string, unknown>) => {
    acpState.requests.push({ method: "__spawn__", params: { command, args, options } });
    const child = new FakeChild(acpState.script || (() => undefined));
    acpState.children.push(child);
    return child;
  }),
}));

function standardScript(sessionId: string): Script {
  return (request, child) => {
    if (request.id === undefined || !request.method) return;
    if (request.method === "initialize" || request.method === "authenticate") {
      child.reply(request.id, {});
    } else if (request.method === "session/new") {
      child.reply(request.id, { sessionId });
    } else if (request.method === "session/load") {
      child.reply(request.id, { sessionId: request.params?.sessionId });
    } else if (request.method === "session/prompt") {
      child.notify("session/update", { update: { sessionUpdate: "agent_message_chunk", content: { type: "text", text: "Hello " } } });
      child.notify("session/update", { update: { sessionUpdate: "agent_message_chunk", content: { type: "text", text: "world" } } });
      child.notify("session/update", { update: { sessionUpdate: "agent_thought_chunk", content: { type: "text", text: "thinking" } } });
      child.notify("session/update", { update: { sessionUpdate: "tool_call", toolCallId: "call-1", title: "read_file", rawInput: { path: "a.ts" } } });
      child.notify("session/update", { update: { sessionUpdate: "tool_call_update", toolCallId: "call-1", output: "file body", status: "done" } });
      child.reply(request.id, { stopReason: "end_turn" });
    } else {
      child.reply(request.id, {});
    }
  };
}

describe("ACP runners", () => {
  beforeEach(() => { acpState.requests = []; acpState.children = []; acpState.script = null; });

  it("runs Cursor ACP and normalizes streamed updates", async () => {
    const { CursorAcpRunner } = await import("../src/runners/acp/cursor.js");
    acpState.script = standardScript("session-1");
    await withTempSession(async (root) => {
      const events: any[] = [];
      const stdio = captureStdio();
      try {
        const result = await new CursorAcpRunner({ ...runnerOptions(root, "", "cursor"), emit: (type, fields) => events.push({ type, fields }) }).runPrompt("hi");
        expect(result).toMatchObject({ provider: "cursor", threadId: "session-1", stopReason: "end_turn", finalText: "Hello world" });
        expect(result.transcript).toContain("Hello world");
      } finally { stdio.restore(); }
      expect(acpState.requests.find((r) => r.method === "__spawn__")?.params).toMatchObject({ command: "agent", args: ["acp"] });
      const messages = events.filter((e) => e.type === "agent_event").map((e) => e.fields.item).filter((i) => i.type === "agent_message");
      expect(messages.at(-1)?.text).toBe("Hello world");
      expect(events.some((e) => e.fields?.item?.type === "reasoning")).toBe(true);
      expect(events.some((e) => e.fields?.item?.id === "call-1" && e.fields.item.status === "done")).toBe(true);
    });
  });

  it("persists and loads an ACP session on the next run", async () => {
    const { CursorAcpRunner } = await import("../src/runners/acp/cursor.js");
    acpState.script = standardScript("session-2");
    await withTempSession(async (root) => {
      const stdio = captureStdio();
      try {
        await new CursorAcpRunner(runnerOptions(root, "", "cursor")).runPrompt("one");
        await new CursorAcpRunner(runnerOptions(root, "", "cursor")).runPrompt("two");
      } finally { stdio.restore(); }
      expect(acpState.requests.filter((r) => r.method === "session/new")).toHaveLength(1);
      expect(acpState.requests.filter((r) => r.method === "session/load")).toHaveLength(1);
      const stored = JSON.parse(await fs.readFile(path.join(root, "state", "agents", "providers", "cursor.json"), "utf8"));
      expect(stored.threadId).toBe("session-2");
    });
  });

  it("uses cursor_login and resolves readonly to plan", async () => {
    const { CursorAcpRunner } = await import("../src/runners/acp/cursor.js");
    acpState.script = standardScript("session-3");
    await withTempSession(async (root) => {
      const stdio = captureStdio();
      try { await new CursorAcpRunner({ ...runnerOptions(root, "", "cursor"), mode: "readonly" }).runPrompt("hi"); } finally { stdio.restore(); }
      expect(acpState.requests.find((r) => r.method === "authenticate")?.params).toEqual({ methodId: "cursor_login" });
      expect(acpState.requests.find((r) => r.method === "session/new")?.params).toMatchObject({ mode: "plan" });
    });
  });

  it("answers permission requests with allow-always", async () => {
    const { CursorAcpRunner } = await import("../src/runners/acp/cursor.js");
    let promptId = -1;
    acpState.script = (request, child) => {
      if (request.method === "session/prompt") {
        promptId = request.id!;
        child.request("session/request_permission", { options: [] }, 7001);
        return;
      }
      // The runner answers the permission request (id 7001) over stdin; once it
      // does, complete the prompt so runPrompt can return.
      if (request.id === 7001 && request.result !== undefined) {
        child.reply(promptId, { stopReason: "end_turn" });
        return;
      }
      if (request.id === undefined || !request.method) return;
      if (request.method === "initialize" || request.method === "authenticate") child.reply(request.id, {});
      else if (request.method === "session/new") child.reply(request.id, { sessionId: "permission-session" });
      else child.reply(request.id, {});
    };
    await withTempSession(async (root) => {
      const stdio = captureStdio();
      try {
        const result = await new CursorAcpRunner(runnerOptions(root, "", "cursor")).runPrompt("hi");
        expect(result.stopReason).toBe("end_turn");
      } finally { stdio.restore(); }
      const answer = acpState.requests.find((r) => r.id === 7001 && r.result !== undefined);
      expect(answer?.result).toEqual({ outcome: { outcome: "selected", optionId: "allow-always" } });
    });
  });

  it("propagates RPC errors and kills the ACP child", async () => {
    const { CursorAcpRunner } = await import("../src/runners/acp/cursor.js");
    acpState.script = (request, child) => request.method === "initialize" ? child.error(request.id!, "not authenticated") : undefined;
    await withTempSession(async (root) => {
      const stdio = captureStdio();
      try { await expect(new CursorAcpRunner(runnerOptions(root, "", "cursor")).runPrompt("hi")).rejects.toThrow("not authenticated"); } finally { stdio.restore(); }
      expect(acpState.children[0]?.killed).toBe(true);
    });
  });

  it("ignores unknown notifications and answers unknown requests without failing the turn", async () => {
    const { CursorAcpRunner } = await import("../src/runners/acp/cursor.js");
    acpState.script = (request, child) => {
      if (request.id === undefined || !request.method) return;
      if (request.method === "session/prompt") {
        child.notify("vendor/unknown_notification", { whatever: 1 });
        child.request("vendor/unknown_request", { whatever: 2 }, 7002);
        child.reply(request.id, { stopReason: "end_turn" });
        return;
      }
      if (request.method === "initialize" || request.method === "authenticate") child.reply(request.id, {});
      else if (request.method === "session/new") child.reply(request.id, { sessionId: "unknown-session" });
      else child.reply(request.id, {});
    };
    await withTempSession(async (root) => {
      const stdio = captureStdio();
      try {
        const result = await new CursorAcpRunner(runnerOptions(root, "", "cursor")).runPrompt("hi");
        expect(result.stopReason).toBe("end_turn");
      } finally { stdio.restore(); }
      // The unknown server request (id 7002) must still have been answered with {}.
      expect(acpState.requests.some((r) => r.id === 7002 && r.result !== undefined)).toBe(true);
    });
  });

  it("routes non-JSON agent stdout and stderr into the transcript without crashing", async () => {
    const { CursorAcpRunner } = await import("../src/runners/acp/cursor.js");
    acpState.script = (request, child) => {
      if (request.id === undefined || !request.method) return;
      if (request.method === "initialize") {
        child.stderr.write("acp warning\n");
        child.reply(request.id, {});
        return;
      }
      if (request.method === "session/prompt") {
        child.stdout.write("not-json-noise\n");
        child.reply(request.id, { stopReason: "end_turn" });
        return;
      }
      if (request.method === "authenticate") child.reply(request.id, {});
      else if (request.method === "session/new") child.reply(request.id, { sessionId: "noise-session" });
      else child.reply(request.id, {});
    };
    await withTempSession(async (root) => {
      const stdio = captureStdio();
      try {
        const result = await new CursorAcpRunner(runnerOptions(root, "", "cursor")).runPrompt("hi");
        expect(result.transcript).toContain("not-json-noise");
        expect(result.transcript).toContain("acp warning");
      } finally { stdio.restore(); }
    });
  });

  it("maps cursor extensions: ask_question skipped, create_plan forwarded as a plan item", async () => {
    const { CursorAcpRunner } = await import("../src/runners/acp/cursor.js");
    let promptId = -1;
    acpState.script = (request, child) => {
      if (request.method === "session/prompt") {
        promptId = request.id!;
        child.request("cursor/ask_question", { questions: [] }, 7003);
        child.request("cursor/create_plan", { plan: "1. do things" }, 7004);
        child.notify("cursor/update_todos", { todos: [] });
        return;
      }
      if ((request.id === 7003 || request.id === 7004) && request.result !== undefined) {
        // Complete the prompt once both extension requests are answered.
        const answered = acpState.requests.filter((r) => r.result !== undefined && (r.id === 7003 || r.id === 7004)).length;
        if (answered >= 2) child.reply(promptId, { stopReason: "end_turn" });
        return;
      }
      if (request.id === undefined || !request.method) return;
      if (request.method === "initialize" || request.method === "authenticate") child.reply(request.id, {});
      else if (request.method === "session/new") child.reply(request.id, { sessionId: "ext-session" });
      else child.reply(request.id, {});
    };
    await withTempSession(async (root) => {
      const events: any[] = [];
      const stdio = captureStdio();
      try {
        const result = await new CursorAcpRunner({
          ...runnerOptions(root, "", "cursor"),
          emit: (type, fields) => events.push({ type, fields }),
        }).runPrompt("hi");
        expect(result.stopReason).toBe("end_turn");
      } finally { stdio.restore(); }
      expect(acpState.requests.find((r) => r.id === 7003)?.result).toEqual({ outcome: { outcome: "skipped", reason: "runtime non-interactive" } });
      expect(acpState.requests.find((r) => r.id === 7004)?.result).toEqual({ outcome: { outcome: "accepted" } });
      const plan = events.filter((e) => e.type === "agent_event").map((e) => e.fields.item).find((i) => i.type === "plan");
      expect(plan?.plan).toContain("do things");
    });
  });
});
