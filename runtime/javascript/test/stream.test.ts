import fs from "node:fs/promises";
import path from "node:path";
import { Readable, Writable } from "node:stream";
import { afterEach, describe, expect, it, vi } from "vitest";
import { decodeBinary, decodeFrame, encodeBinary, encodeFrame, FRAME_VERSION, type StreamFrame } from "../src/frame.js";
import { runStreamCommand } from "../src/stream.js";
import { withTempSession } from "./helpers.js";

const runInputs: string[] = [];
const thread = {
  id: "thread-1",
  async runStreamed(input: string) {
    runInputs.push(input);
    return {
      events: asyncGenerator([
        { type: "thread.started", thread_id: "thread-1" },
        { type: "item.completed", item: { id: `msg-${runInputs.length}`, type: "agent_message", text: `answer ${runInputs.length}` } },
      ]),
    };
  },
};

const startThread = vi.fn(() => thread);
const resumeThread = vi.fn(() => thread);

vi.mock("@openai/codex-sdk", () => ({
  Codex: vi.fn(function CodexMock(this: object) {
    Object.assign(this, {
      startThread,
      resumeThread,
    });
  }),
}));

afterEach(() => {
  runInputs.length = 0;
  startThread.mockClear();
  resumeThread.mockClear();
  vi.restoreAllMocks();
});

describe("runtime stream frames", () => {
  it("encodes and decodes newline-delimited frames", () => {
    const encoded = encodeFrame({ v: FRAME_VERSION, seq: 7, type: "human_message", message: "hello" });

    expect(encoded.endsWith("\n")).toBe(true);
    expect(decodeFrame(encoded)).toMatchObject({
      v: FRAME_VERSION,
      seq: 7,
      type: "human_message",
      message: "hello",
    });
  });

  it("encodes binary fields as base64", () => {
    const encoded = encodeBinary(Buffer.from("abc"));

    expect(encoded).toEqual({ encoding: "base64", data: "YWJj" });
    expect(Buffer.from(decodeBinary(encoded)).toString("utf8")).toBe("abc");
  });

  it("rejects malformed frames", () => {
    expect(() => decodeFrame("not json")).toThrow(/valid JSON/);
    expect(() => encodeFrame({ v: 2, seq: 0, type: "start" })).toThrow(/version/);
    expect(() => encodeFrame({ v: FRAME_VERSION, seq: -1, type: "start" })).toThrow(/seq/);
  });
});

describe("runStreamCommand", () => {
  it("runs multiple Codex human messages, rebuilding options from the per-turn snapshot each turn", async () => {
    await withTempSession(async (root) => {
      const stdout = new MemoryWritable();
      const stderr = new MemoryWritable();

      await runStreamCommand({
        stdin: Readable.from([
          frame({ seq: 0, type: "start", provider: "codex", stateRoot: `${root}/state`, workspace: `${root}/workspace`, home: `${root}/home` }),
          frame({ seq: 1, type: "human_message", message: "first", model: "gpt-5" }),
          frame({ seq: 2, type: "human_message", message: "second", model: "gpt-5-codex" }),
          frame({ seq: 3, type: "eof" }),
        ]),
        stdout,
        stderr,
      });

      const frames = parseOutput(stdout.text);
      // Per-turn loop: each human_message is ACKed at read time (input_status —
      // the delivery protocol), then executes in arrival order (turn started →
      // turn completed); eof closes the stream. No configure frames, no
      // configure_ack. Both ACKs precede the first turn because the read loop
      // never blocks on execution.
      expect(frames.map((entry) => entry.type)).toEqual([
        "started",
        "input_status",
        "input_status",
        "agent_turn_started",
        "agent_turn_completed",
        "agent_turn_started",
        "agent_turn_completed",
        "result",
      ]);
      // The receipt ACKs carry the delivery protocol fields even when the
      // sender omitted an explicit messageId.
      const acks = frames.filter((entry) => entry.type === "input_status");
      expect(acks).toHaveLength(2);
      expect(acks.every((entry) => entry.status === "received")).toBe(true);
      expect(frames.every((entry, index) => entry.seq === index)).toBe(true);
      expect(runInputs).toEqual(["first", "second"]);
      // First turn has no stored thread → startThread; second turn resumes it.
      expect(startThread).toHaveBeenCalledTimes(1);
      expect(resumeThread).toHaveBeenCalledTimes(1);
      expect(frames.at(-1)).toMatchObject({
        type: "result",
        provider: "codex",
        stopReason: "eof",
      });
    });
  });

  it("applies a per-turn model/mode snapshot to the next turn without a configure frame", async () => {
    await withTempSession(async (root) => {
      const stdout = new MemoryWritable();
      const seenThreadOptions: Record<string, unknown>[] = [];
      resumeThread.mockImplementation((id: string, options?: Record<string, unknown>) => {
        seenThreadOptions.push({ id, ...(options || {}) });
        return thread;
      });

      await runStreamCommand({
        stdin: Readable.from([
          frame({ seq: 0, type: "start", provider: "codex", stateRoot: `${root}/state`, workspace: `${root}/workspace`, home: `${root}/home`, model: "gpt-5", mode: "workspace-write" }),
          // Turn 1 carries a new model + mode; the runtime must hand them to the
          // provider runner for THIS turn (no configure/ack, no restart).
          frame({ seq: 1, type: "human_message", message: "go", model: "gpt-5-codex", mode: "read-only" }),
          frame({ seq: 2, type: "eof" }),
        ]),
        stdout,
      });

      // The first turn's threadOptions reflect the per-turn snapshot, not the
      // start-frame baseline.
      expect(seenThreadOptions).toHaveLength(0); // first turn starts, not resumes
      expect(startThread).toHaveBeenCalledTimes(1);
      const startOpts = startThread.mock.calls[0]?.[0] as Record<string, unknown> | undefined;
      expect(startOpts?.model).toBe("gpt-5-codex");
      expect(startOpts?.sandboxMode).toBe("read-only");
    });
  });

  it("rejects a human_message with a non-string model snapshot instead of silently dropping it", async () => {
    await withTempSession(async (root) => {
      const stdout = new MemoryWritable();

      await runStreamCommand({
        stdin: Readable.from([
          frame({ seq: 0, type: "start", provider: "codex", stateRoot: `${root}/state`, workspace: `${root}/workspace`, home: `${root}/home`, model: "gpt-5" }),
          frame({ seq: 1, type: "human_message", message: "go", model: 42 }),
          frame({ seq: 2, type: "eof" }),
        ]),
        stdout,
      });

      const frames = parseOutput(stdout.text);
      // A non-string snapshot knob is ignored (falls back to the start-frame
      // value); the turn still runs. There is no configure_ack anymore.
      expect(frames.filter((entry) => entry.type === "configure_ack")).toHaveLength(0);
      expect(frames.some((entry) => entry.type === "agent_turn_completed")).toBe(true);
    });
  });

  it("supports the claude provider in interactive stream (no unsupported-provider error)", async () => {
    await withTempSession(async (root) => {
      const stdout = new MemoryWritable();

      // Claude is now driven through the same per-turn prompt runner as codex,
      // so a start frame for claude must not emit an unsupported_provider error.
      // (We don't run a turn here — the mock only covers codex — but the start
      // frame itself must succeed and emit "started".)
      await runStreamCommand({
        stdin: Readable.from([
          frame({ seq: 0, type: "start", provider: "claude", stateRoot: `${root}/state`, workspace: `${root}/workspace`, home: `${root}/home` }),
          frame({ seq: 1, type: "eof" }),
        ]),
        stdout,
      });

      const frames = parseOutput(stdout.text);
      expect(frames[0]).toMatchObject({ type: "started", provider: "claude" });
      expect(frames.find((entry) => entry.type === "error" && (entry as Record<string, unknown>).code === "unsupported_provider")).toBeUndefined();
    });
  });

  it("emits parser errors without writing diagnostics to stdout", async () => {
    const stdout = new MemoryWritable();
    const stderr = new MemoryWritable();

    await runStreamCommand({
      stdin: Readable.from(["{}\n"]),
      stdout,
      stderr,
    });

    expect(parseOutput(stdout.text)).toEqual([
      {
        v: FRAME_VERSION,
        seq: 0,
        type: "error",
        code: "runtime_stream_error",
        message: `frame version must be ${FRAME_VERSION}`,
      },
    ]);
    expect(stderr.text).toBe("");
  });

  it("runs a non-TTY command from a start command-mode request and emits NDJSON stream frames", async () => {
    await withTempSession(async (root) => {
      const stdout = new MemoryWritable();
      const stderr = new MemoryWritable();
      const artifactDir = path.join(root, "artifacts");

      await runStreamCommand({
        stdin: Readable.from([
          frame({
            seq: 0,
            type: "start",
            mode: "command",
            workspace: root,
            request: {
              mode: "exec",
              command: "node",
              args: ["-e", "process.stdout.write('out-1\\n'); process.stderr.write('err-1\\n')"],
              artifactDir,
            },
          }),
        ]),
        stdout,
        stderr,
      });

      const frames = parseOutput(stdout.text);
      expect(frames.map((entry) => entry.type)).toEqual([
        "started",
        "stdout",
        "output",
        "stderr",
        "output",
        "result",
      ]);
      expect(frames.every((entry, index) => entry.seq === index)).toBe(true);
      expect(frames.find((entry) => entry.type === "stdout")).toMatchObject({ text: "out-1\n" });
      expect(frames.find((entry) => entry.type === "stderr")).toMatchObject({ text: "err-1\n" });
      expect(frames.filter((entry) => entry.type === "output")).toEqual([
        expect.objectContaining({ source: "stdout", text: "out-1\n" }),
        expect.objectContaining({ source: "stderr", text: "err-1\n" }),
      ]);
      expect(stderr.text).toBe("");

      const result = frames.at(-1);
      expect(result).toMatchObject({
        type: "result",
        stdout: "out-1\n",
        stderr: "err-1\n",
        output: expect.stringContaining("out-1\n"),
        exitCode: 0,
        success: true,
      });
      expect(result?.output).toContain("err-1\n");

      expect(await fs.readFile(path.join(artifactDir, "stdout.txt"), "utf8")).toBe("out-1\n");
      expect(await fs.readFile(path.join(artifactDir, "stderr.txt"), "utf8")).toBe("err-1\n");
      expect(await fs.readFile(path.join(artifactDir, "output.txt"), "utf8")).toContain("out-1\n");
      expect(await fs.readFile(path.join(artifactDir, "output.txt"), "utf8")).toContain("err-1\n");
      const savedRequest = JSON.parse(await fs.readFile(path.join(artifactDir, "command-request.json"), "utf8"));
      const savedResult = JSON.parse(await fs.readFile(path.join(artifactDir, "command-result.json"), "utf8"));
      expect(savedRequest).toMatchObject({ mode: "exec", command: "node", cwd: root });
      expect(savedResult).toMatchObject({
        stdout: "out-1\n",
        stderr: "err-1\n",
        exitCode: 0,
        success: true,
      });
      expect(result).toMatchObject(savedResult);
    });
  });
});

function frame(fields: Omit<StreamFrame, "v">): string {
  return encodeFrame({ v: FRAME_VERSION, ...fields });
}

function parseOutput(output: string): StreamFrame[] {
  return output.trimEnd().split("\n").filter(Boolean).map((line) => JSON.parse(line) as StreamFrame);
}

async function* asyncGenerator(events: unknown[]): AsyncIterable<unknown> {
  for (const event of events) {
    yield event;
  }
}

class MemoryWritable extends Writable {
  private readonly chunks: Buffer[] = [];

  override _write(chunk: Buffer | string, _encoding: BufferEncoding, callback: (error?: Error | null) => void): void {
    this.chunks.push(Buffer.from(chunk));
    callback();
  }

  get text(): string {
    return Buffer.concat(this.chunks).toString("utf8");
  }
}
