/**
 * Message delivery protocol over the interactive stream:
 *
 * - a (messageId, deliveryAttempt) frame executes exactly once; replays are
 *   ACKed without executing
 * - a bumped attempt (explicit retry after failure) executes again
 * - cancel interrupts only the in-flight turn and the session accepts the next
 *   human_message — cancellation never terminates the stream
 */
import path from "node:path";
import { Readable, Writable } from "node:stream";
import { afterEach, describe, expect, it, vi } from "vitest";
import { FRAME_VERSION } from "../src/frame.js";
import { runStreamCommand } from "../src/stream.js";
import { withTempSession } from "./helpers.js";

type OutputFrame = Record<string, unknown> & { type: string; seq: number };

const runInputs: { input: string; signal?: AbortSignal }[] = [];

const thread = {
  id: "thread-1",
  async runStreamed(input: string, options?: { signal?: AbortSignal }) {
    runInputs.push({ input, signal: options?.signal });
    const events = (async function* generator() {
      yield { type: "thread.started", thread_id: "thread-1" };
      yield {
        type: "item.completed",
        item: { id: `msg-${runInputs.length}`, type: "agent_message", text: `answer ${runInputs.length}` },
      };
    })();
    return { events };
  },
};

const startThread = vi.fn(() => thread);
const resumeThread = vi.fn(() => thread);

vi.mock("@openai/codex-sdk", () => ({
  Codex: vi.fn(function CodexMock(this: object) {
    Object.assign(this, { startThread, resumeThread });
  }),
}));

afterEach(() => {
  runInputs.length = 0;
  startThread.mockClear();
  resumeThread.mockClear();
  vi.restoreAllMocks();
});

class MemoryWritable extends Writable {
  chunks: string[] = [];
  _write(chunk: Buffer, _encoding: string, callback: (error?: Error | null) => void) {
    this.chunks.push(chunk.toString());
    callback(null);
  }
  text(): string {
    return this.chunks.join("");
  }
}

function frame(seq: number, type: string, fields: Record<string, unknown> = {}) {
  return JSON.stringify({ v: FRAME_VERSION, seq, type, ...fields }) + "\n";
}

function parseFrames(stdout: MemoryWritable): OutputFrame[] {
  return stdout.text().trim().split("\n").filter(Boolean).map((line) => JSON.parse(line) as OutputFrame);
}

function collectStdout(): { stdout: MemoryWritable; frames(): OutputFrame[] } {
  const stdout = new MemoryWritable();
  return { stdout, frames: () => parseFrames(stdout) };
}

describe("message delivery idempotency", () => {
  it("executes a same (messageId, attempt) frame once and ACKs the replay", async () => {
    await withTempSession(async (root) => {
      const { stdout, frames } = collectStdout();
      await runStreamCommand({
        stdin: Readable.from([
          frame(0, "start", { provider: "codex", stateRoot: path.join(root, "state") }),
          frame(1, "human_message", { message: "hello", messageId: "m-1", deliveryAttempt: 1 }),
          frame(2, "human_message", { message: "hello", messageId: "m-1", deliveryAttempt: 1 }),
          frame(3, "eof"),
        ]),
        stdout,
      });

      const output = frames();
      const acks = output.filter((entry) => entry.type === "input_status");
      // Both sends are ACKed (the gateway can advance its state row), but the
      // turn executed exactly once.
      expect(acks.map((entry) => entry.status)).toEqual(["received", "received"]);
      const turns = output.filter((entry) => entry.type === "agent_turn_started");
      expect(turns).toHaveLength(1);
      expect(runInputs.map((entry) => entry.input)).toEqual(["hello"]);
    });
  });

  it("executes again when the attempt is bumped (explicit retry)", async () => {
    await withTempSession(async (root) => {
      const { stdout, frames } = collectStdout();
      await runStreamCommand({
        stdin: Readable.from([
          frame(0, "start", { provider: "codex", stateRoot: path.join(root, "state") }),
          frame(1, "human_message", { message: "hello", messageId: "m-1", deliveryAttempt: 1 }),
          frame(2, "human_message", { message: "hello", messageId: "m-1", deliveryAttempt: 2 }),
          frame(3, "eof"),
        ]),
        stdout,
      });

      const turns = frames().filter((entry) => entry.type === "agent_turn_started");
      expect(turns).toHaveLength(2);
      expect(runInputs.map((entry) => entry.input)).toEqual(["hello", "hello"]);
    });
  });
});

describe("cancel semantics", () => {
  it("cancel with no session ends the stream; cancel with a session keeps it alive", async () => {
    await withTempSession(async (root) => {
      const { stdout, frames } = collectStdout();
      await runStreamCommand({
        stdin: Readable.from([
          frame(0, "start", { provider: "codex", stateRoot: path.join(root, "state") }),
          frame(1, "human_message", { message: "first" }),
          frame(2, "cancel"),
          frame(3, "human_message", { message: "second" }),
          frame(4, "eof"),
        ]),
        stdout,
      });

      const output = frames();
      // Cancel did NOT produce a result frame: the session survived it and ran
      // the next message.
      const results = output.filter((entry) => entry.type === "result");
      expect(results).toHaveLength(1);
      expect(results[0]).toMatchObject({ stopReason: "eof" });
      expect(runInputs.map((entry) => entry.input)).toEqual(["first", "second"]);
      expect(output.filter((entry) => entry.type === "agent_turn_started")).toHaveLength(2);
    });
  });

  it("cancels the in-flight turn via the abort signal and accepts the next message", async () => {
    await withTempSession(async (root) => {
      const { stdout, frames } = collectStdout();
      let release: (() => void) | null = null;
      let turnCancelled = false;
      const threadHangingTurn = {
        id: "thread-1",
        async runStreamed(input: string, options?: { signal?: AbortSignal }) {
          runInputs.push({ input, signal: options?.signal });
          const signal = options?.signal;
          const events = (async function* generator() {
            yield { type: "thread.started", thread_id: "thread-1" };
            if (signal && input === "slow") {
              // Block the turn until cancel aborts the signal.
              await new Promise<void>((resolve, reject) => {
                release = resolve;
                signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true });
              }).catch((error: unknown) => {
                turnCancelled = true;
                throw error;
              });
            }
            yield {
              type: "item.completed",
              item: { id: `msg-${runInputs.length}`, type: "agent_message", text: `answer ${runInputs.length}` },
            };
          })();
          return { events };
        },
      };
      vi.mocked(startThread).mockImplementation(() => threadHangingTurn as never);

      const stdin = Readable.from((async function* frames() {
        yield frame(0, "start", { provider: "codex", stateRoot: path.join(root, "state") });
        yield frame(1, "human_message", { message: "slow", messageId: "m-slow" });
        // Wait until the hanging turn is actually inside its await.
        await vi.waitFor(() => {
          if (!release) throw new Error("turn not started yet");
        });
        yield frame(2, "cancel");
        await vi.waitFor(() => {
          if (!turnCancelled) throw new Error("turn not cancelled yet");
        });
        yield frame(3, "human_message", { message: "after", messageId: "m-after" });
        yield frame(4, "eof");
      })());

      await runStreamCommand({ stdin, stdout });

      const output = frames();
      // The cancelled turn reports cancelled with its message id, the next
      // message still executes, and the only terminal result is the eof one.
      const statusFrames = output.filter((entry) => entry.type === "input_status");
      expect(statusFrames).toEqual(
        expect.arrayContaining([
          expect.objectContaining({ messageId: "m-slow", status: "received" }),
          expect.objectContaining({ messageId: "m-slow", status: "cancelled" }),
          expect.objectContaining({ messageId: "m-after", status: "received" }),
        ]),
      );
      expect(runInputs.map((entry) => entry.input)).toEqual(["slow", "after"]);
      const results = output.filter((entry) => entry.type === "result");
      expect(results).toHaveLength(1);
      expect(results[0]).toMatchObject({ stopReason: "eof" });
    });
  });
});
