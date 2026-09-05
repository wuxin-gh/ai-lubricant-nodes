import { createInterface } from "node:readline/promises";
import type { Readable, Writable } from "node:stream";
import { runRuntimeCommand, type RuntimeCommandRequest } from "./command.js";
import { decodeFrame, encodeFrame, FRAME_VERSION, type StreamFrame } from "./frame.js";
import { createInteractiveSession, type InteractiveSession, type TurnSnapshot } from "./interactive.js";

export interface RunStreamOptions {
  stdin?: Readable;
  stdout?: Writable;
  stderr?: Writable;
}

export async function runStreamCommand(options: RunStreamOptions = {}): Promise<void> {
  const stdin = options.stdin || process.stdin;
  const stdout = options.stdout || process.stdout;
  const stderr = options.stderr || process.stderr;
  let outputSeq = 0;
  let session: InteractiveSession | undefined;
  let finished = false;

  const emit = (type: string, fields: object = {}) => {
    stdout.write(encodeFrame({ v: FRAME_VERSION, seq: outputSeq++, type, ...fields }));
  };
  const emitError = (error: unknown, inputSeq?: number, messageId?: string) => {
    emit("error", structuredError(error, inputSeq, messageId));
  };

  const lines = createInterface({ input: stdin, crlfDelay: Infinity });

  // ── Interactive turn lifecycle ─────────────────────────────────────────────
  // A turn runs in the background while the read loop keeps consuming frames —
  // that is what makes "cancel the current turn" actually interrupt it instead
  // of queueing behind the running prompt (the old loop awaited each turn, so a
  // cancel frame sat unread until the turn finished by itself, and cancelling
  // then tore down the whole session). Only eof ends the session; cancel stops
  // one turn and returns the stream to idle for the next human_message.
  //
  // Delivery idempotency: (messageId, attempt) keys are remembered per stream;
  // replays of the same key are ACKed without executing a second time, while a
  // failed/cancelled turn retried with a bumped attempt still runs.
  const seenTurnKeys = new Set<string>();
  let activeTurn: Promise<void> | null = null;

  const drainActiveTurn = async (): Promise<void> => {
    if (!activeTurn) return;
    try {
      await activeTurn;
    } catch {
      // The turn already reported its own failure/error frame.
    }
  };

  for await (const line of lines) {
    if (!line.trim()) {
      continue;
    }
    let frame: StreamFrame;
    try {
      frame = decodeFrame(line);
    } catch (error) {
      emitError(error);
      continue;
    }

    try {
      switch (frame.type) {
        case "start":
          if (session) {
            throw new Error("stream has already been started");
          }
          if (frame.mode === "command") {
            emit("started", { mode: "command" });
            emit("result", await runCommandFrame(frame, emit));
            finished = true;
            lines.close();
            break;
          }
          session = await createInteractiveSession({
            provider: stringField(frame, "provider"),
            stateRoot: stringField(frame, "stateRoot"),
            workspace: stringField(frame, "workspace"),
            home: stringField(frame, "home"),
            model: stringField(frame, "model"),
            mode: stringField(frame, "mode"),
            outputSchemaFile: stringField(frame, "outputSchemaFile"),
            sessionScope: stringField(frame, "editorSessionId") || stringField(frame, "sessionId"),
          }, emit);
          break;
        case "human_message": {
          if (!session) {
            throw new Error("stream has not been started");
          }
          const messageId = stringField(frame, "messageId") || "";
          const attempt = numberField(frame, "deliveryAttempt") || 1;
          const turnKey = messageId ? `${messageId}:${attempt}` : "";
          if (turnKey && seenTurnKeys.has(turnKey)) {
            // Transport-level replay of a frame we already accepted: ACK receipt
            // again so the gateway can advance state, but never execute twice.
            emit("input_status", { messageId, deliveryAttempt: attempt, status: "received" });
            break;
          }
          if (turnKey) seenTurnKeys.add(turnKey);
          emit("input_status", { messageId, deliveryAttempt: attempt, status: "received" });
          const text = messageText(frame);
          const snapshot = turnSnapshot(frame);
          const previous = activeTurn;
          // Preserve the historical multi-message contract: frames already in
          // stdin execute sequentially in arrival order. The read loop itself
          // never awaits this queue, so a later cancel frame is still consumed
          // immediately and aborts only the CURRENT turn; queued turns start
          // after it settles. Same (messageId, attempt) frames were filtered
          // above, so queueing cannot duplicate one logical delivery.
          const queued = (previous ? previous.catch(() => undefined) : Promise.resolve())
            .then(() => session!.runHumanMessage(text, snapshot, messageId, attempt));
          let tracked: Promise<void>;
          tracked = queued
            .catch((error: unknown) => {
              // runHumanMessage reports cancelled/failed states itself; any
              // error escaping here is an unexpected runtime fault.
              emitError(error, frame.seq, messageId);
            })
            .finally(() => {
              if (activeTurn === tracked) activeTurn = null;
            });
          activeTurn = tracked;
          break;
        }
        case "command":
          if (session) {
            throw new Error("command frames are not supported after interactive start");
          }
          emit("started", { mode: "command" });
          emit("result", await runCommandFrame(frame, emit));
          finished = true;
          lines.close();
          break;
        case "cancel":
          if (!session) {
            emit("result", { stopReason: "cancelled" });
            finished = true;
            lines.close();
            break;
          }
          // Cancel ONLY the in-flight turn. The session stays alive: the node
          // relays the next human_message and it executes normally. The turn's
          // own completion reports status=cancelled (see interactive.ts). A
          // cancel with nothing running is a harmless no-op.
          session.cancelCurrentTurn();
          break;
        case "eof": {
          if (!session) {
            emit("result", { stopReason: "eof" });
          } else {
            // End of session: stop any in-flight turn first so its state writes
            // finish before the terminal result frame goes out.
            session.cancelCurrentTurn();
            await drainActiveTurn();
            emit("result", await session.finish("eof"));
          }
          finished = true;
          lines.close();
          break;
        }
        default:
          throw new Error(`unsupported input frame type ${frame.type}`);
      }
    } catch (error) {
      emitError(error, frame.seq);
    }
  }

  if (!finished && session) {
    // stdin closed without eof (node process restarted / pipe broken): stop the
    // in-flight turn, then report the terminal result as before.
    try {
      session.cancelCurrentTurn();
      await drainActiveTurn();
      emit("result", await session.finish("eof"));
    } catch (error) {
      stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
      emitError(error);
    }
  }
}

async function runCommandFrame(
  frame: StreamFrame,
  emit: (type: string, fields?: object) => void,
) {
  const request = commandRequest(frame);
  return runRuntimeCommand({
    request,
    artifactDir: request.artifactDir || stringField(frame, "artifactDir"),
    stateRoot: stringField(frame, "stateRoot"),
    workspace: stringField(frame, "workspace"),
    home: stringField(frame, "home"),
    onStdout(chunk) {
      emitTextFrame(emit, "stdout", chunk);
      emitOutputFrame(emit, "stdout", chunk);
    },
    onStderr(chunk) {
      emitTextFrame(emit, "stderr", chunk);
      emitOutputFrame(emit, "stderr", chunk);
    },
  });
}

// turnSnapshot reads the optional per-turn model/mode/llm fields off a
// human_message frame. The node stamps these so the runtime re-prepares the
// provider with the latest config for this turn (no separate hot-update).
function turnSnapshot(frame: StreamFrame): TurnSnapshot | undefined {
  const snapshot: TurnSnapshot = {};
  if (typeof frame.model === "string" && frame.model.trim()) {
    snapshot.model = frame.model;
  }
  if (typeof frame.mode === "string" && frame.mode.trim()) {
    snapshot.mode = frame.mode;
  }
  const llm = frame.llm;
  if (isRecord(llm)) {
    snapshot.llm = {
      endpoint: stringField(llm, "endpoint"),
      apiKey: stringField(llm, "apiKey"),
      model: stringField(llm, "model"),
      protocol: stringField(llm, "protocol"),
    };
  }
  return Object.keys(snapshot).length > 0 ? snapshot : undefined;
}

function commandRequest(frame: StreamFrame): RuntimeCommandRequest {
  if (isRecord(frame.request)) {
    return frame.request as unknown as RuntimeCommandRequest;
  }
  throw new Error(`${frame.type} frame in command mode requires request object`);
}

function emitTextFrame(
  emit: (type: string, fields?: object) => void,
  type: "stdout" | "stderr",
  chunk: Buffer,
) {
  emit(type, { text: chunk.toString("utf8") });
}

function emitOutputFrame(
  emit: (type: string, fields?: object) => void,
  source: "stdout" | "stderr",
  chunk: Buffer,
) {
  emit("output", { source, text: chunk.toString("utf8") });
}

function stringField(record: Record<string, unknown>, field: string): string | undefined {
  const value = record[field];
  return typeof value === "string" ? value : undefined;
}

function numberField(record: Record<string, unknown>, field: string): number | undefined {
  const value = record[field];
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function messageText(frame: StreamFrame): string {
  if (typeof frame.message === "string") {
    return frame.message;
  }
  if (typeof frame.text === "string") {
    return frame.text;
  }
  throw new Error("human_message frame requires message");
}

function structuredError(error: unknown, inputSeq?: number, messageId?: string): Record<string, unknown> {
  const record: Record<string, unknown> = {
    code: "runtime_stream_error",
    message: error instanceof Error ? error.message : String(error),
  };
  if (inputSeq !== undefined) {
    record.inputSeq = inputSeq;
  }
  if (messageId) {
    record.message_id = messageId;
  }
  return record;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
