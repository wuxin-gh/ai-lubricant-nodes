/**
 * Turn-level backoff retry for rate-limited LLM calls.
 *
 * Why this lives in the runtime and not in the gateway it calls: 429 means "come
 * back later", and only the caller knows how long it can afford to wait. A
 * gateway holding an HTTP request cannot sit idle for 30s, and its retry budget
 * is configured by whoever operates it — a consumer that outsources its own
 * reliability to a callee's configuration has none. The runtime drives an agent
 * turn that already takes minutes, so waiting is free here and nowhere else.
 *
 * Scope: providers whose SDK does not retry internally (codex / opencode /
 * gemini). Claude's SDK retries per API call and reports each attempt as an
 * `api_retry` message, which is strictly better — finer grained, no lost tool
 * calls — so this layer only catches what leaks past it.
 */

/** First delay, in ms. 429 one second after a 429 is almost always another 429. */
export const RETRY_BASE_MS = 2000;
/** Multiplier per attempt. */
export const RETRY_FACTOR = 2;
/** Ceiling for a single wait: past this a user reads the turn as hung. */
export const RETRY_CAP_MS = 32_000;
/** Attempts after the initial call. base*2^n capped ⇒ 2+4+8+16+32 = 62s total. */
export const RETRY_MAX_ATTEMPTS = 5;
/** Jitter fraction, ±20%: keeps many sessions from re-firing in lockstep. */
export const RETRY_JITTER = 0.2;

/**
 * An upstream failure that carries the HTTP status the provider reported.
 *
 * Runners throw plain Errors whose message happens to mention a status; matching
 * that text to decide whether to retry is guesswork that breaks the moment a
 * provider rewords its error. Carrying the status as a field makes the decision
 * exact, and lets the stream layer put `status_code` on the error frame so the
 * browser can distinguish "rate limited" from "your config is wrong".
 */
export class UpstreamStatusError extends Error {
  readonly statusCode: number | null;
  readonly retryAfterMs: number | null;

  constructor(message: string, statusCode: number | null, retryAfterMs: number | null = null) {
    super(message);
    this.name = "UpstreamStatusError";
    this.statusCode = statusCode;
    this.retryAfterMs = retryAfterMs;
  }
}

/** Status of an error if it carries one, else null. */
export function statusOf(error: unknown): number | null {
  if (error instanceof UpstreamStatusError) {
    return error.statusCode;
  }
  // Provider SDKs commonly hang `status` or `statusCode` off their error object.
  if (error && typeof error === "object") {
    for (const key of ["status", "statusCode", "response_status"]) {
      const value = (error as Record<string, unknown>)[key];
      if (typeof value === "number" && Number.isFinite(value)) {
        return value;
      }
    }
  }
  return null;
}

/**
 * Whether waiting could plausibly change the outcome.
 *
 * 429 is the case this exists for. Connection-class failures (no status at all)
 * are included because a dropped socket is transient by nature. 5xx is left out
 * on purpose: the gateway in front of us already fails over between accounts on
 * those, so retrying here would stack a slow retry on a fast one. Every other
 * 4xx is a statement about the request, and the request will not change.
 */
export function isRetryableStatus(status: number | null): boolean {
  return status === 429 || status === null;
}

/** Parse a Retry-After value (seconds, or an HTTP date) into ms. */
export function retryAfterMs(value: unknown): number | null {
  if (typeof value === "number" && Number.isFinite(value) && value > 0) {
    return Math.round(value * 1000);
  }
  if (typeof value === "string" && value.trim()) {
    const seconds = Number(value);
    if (Number.isFinite(seconds) && seconds > 0) {
      return Math.round(seconds * 1000);
    }
    const date = Date.parse(value);
    if (Number.isFinite(date)) {
      const delta = date - Date.now();
      return delta > 0 ? delta : null;
    }
  }
  return null;
}

/**
 * Delay before attempt `attempt` (1-based).
 *
 * An explicit Retry-After wins outright — the server knows its own window, and
 * guessing shorter just burns quota. Otherwise exponential from RETRY_BASE_MS,
 * capped, then jittered. The cap applies before jitter so the ceiling is a real
 * ceiling plus at most the jitter band.
 */
export function backoffMs(attempt: number, explicitRetryAfterMs: number | null = null): number {
  if (explicitRetryAfterMs !== null && explicitRetryAfterMs > 0) {
    return Math.min(explicitRetryAfterMs, RETRY_CAP_MS * 2);
  }
  const raw = RETRY_BASE_MS * RETRY_FACTOR ** Math.max(0, attempt - 1);
  const capped = Math.min(raw, RETRY_CAP_MS);
  const jitter = capped * RETRY_JITTER * (Math.random() * 2 - 1);
  return Math.max(0, Math.round(capped + jitter));
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Reported to the consumer before each wait, mirroring Claude's `api_retry`. */
export interface RetryNotice {
  attempt: number;
  max_retries: number;
  retry_delay_ms: number;
  error_status: number | null;
  message: string;
}

/**
 * Run `attemptFn` and retry it on rate-limit / connection failures.
 *
 * `canRetry` is the guard that keeps this safe at turn granularity: a turn that
 * has already run tool calls has written files and executed commands, so
 * replaying it would repeat those side effects. Callers pass a predicate that
 * reports whether anything has been emitted yet; once it has, a failure is
 * final and the user decides whether to re-send.
 */
export async function withTurnRetry<T>(
  attemptFn: () => Promise<T>,
  options: {
    canRetry: () => boolean;
    onRetry?: (notice: RetryNotice) => void;
    maxAttempts?: number;
  },
): Promise<T> {
  const maxAttempts = options.maxAttempts ?? RETRY_MAX_ATTEMPTS;
  for (let attempt = 1; ; attempt += 1) {
    try {
      return await attemptFn();
    } catch (error) {
      const status = statusOf(error);
      if (attempt > maxAttempts || !isRetryableStatus(status) || !options.canRetry()) {
        throw error;
      }
      const explicit = error instanceof UpstreamStatusError ? error.retryAfterMs : null;
      const delay = backoffMs(attempt, explicit);
      options.onRetry?.({
        attempt,
        max_retries: maxAttempts,
        retry_delay_ms: delay,
        error_status: status,
        message: error instanceof Error ? error.message : String(error),
      });
      await sleep(delay);
    }
  }
}
