import { inspect } from "node:util";

/**
 * Raised by runners when the current turn was cancelled through
 * RunnerOptions.abortController. Distinct from provider errors so the stream
 * loop can report the turn as cancelled (session stays alive for the next
 * message) instead of surfacing it as a failure.
 */
export class TurnCancelledError extends Error {
  constructor(reason = "turn cancelled") {
    super(reason);
    this.name = "TurnCancelledError";
  }
}

export function formatError(error: unknown): string {
  if (error instanceof Error) {
    return error.stack || error.message;
  }
  try {
    return JSON.stringify(error, null, 2);
  } catch {
    return inspect(error, { depth: 8, breakLength: 120 });
  }
}
