import { describe, expect, it, vi } from "vitest";

import {
  RETRY_BASE_MS,
  RETRY_CAP_MS,
  RETRY_MAX_ATTEMPTS,
  UpstreamStatusError,
  backoffMs,
  isRetryableStatus,
  retryAfterMs,
  statusOf,
  withTurnRetry,
} from "../src/retry.js";

describe("retry policy", () => {
  it("retries rate limits and connection failures, not client errors", () => {
    expect(isRetryableStatus(429)).toBe(true);
    // No status at all = the request never got a response (socket dropped).
    expect(isRetryableStatus(null)).toBe(true);
    // A 4xx describes the request, and re-sending the same request cannot fix it.
    expect(isRetryableStatus(400)).toBe(false);
    expect(isRetryableStatus(401)).toBe(false);
    expect(isRetryableStatus(403)).toBe(false);
    // 5xx is the gateway's job: it fails over between accounts on those, so
    // waiting here would stack a slow retry on top of a fast one.
    expect(isRetryableStatus(500)).toBe(false);
    expect(isRetryableStatus(503)).toBe(false);
  });

  it("reads a status off carriers and plain SDK error objects", () => {
    expect(statusOf(new UpstreamStatusError("limited", 429))).toBe(429);
    expect(statusOf({ status: 429 })).toBe(429);
    expect(statusOf({ statusCode: 503 })).toBe(503);
    expect(statusOf(new Error("just a message"))).toBeNull();
  });

  it("grows the delay exponentially and honours the ceiling", () => {
    // Jitter is ±20%, so assert the band rather than an exact value.
    const within = (value: number, target: number) => {
      expect(value).toBeGreaterThanOrEqual(Math.floor(target * 0.8));
      expect(value).toBeLessThanOrEqual(Math.ceil(target * 1.2));
    };
    within(backoffMs(1), RETRY_BASE_MS);
    within(backoffMs(2), RETRY_BASE_MS * 2);
    within(backoffMs(3), RETRY_BASE_MS * 4);
    // Far past the ceiling, the wait stays at the ceiling (plus jitter).
    within(backoffMs(20), RETRY_CAP_MS);
  });

  it("prefers an explicit Retry-After over the computed backoff", () => {
    // The server knows its own window; guessing shorter just burns quota.
    expect(backoffMs(1, 5000)).toBe(5000);
    expect(retryAfterMs("7")).toBe(7000);
    expect(retryAfterMs(3)).toBe(3000);
    expect(retryAfterMs("nonsense")).toBeNull();
    expect(retryAfterMs(0)).toBeNull();
  });

  it("retries a rate-limited call until it succeeds", async () => {
    vi.useFakeTimers();
    try {
      let calls = 0;
      const notices: number[] = [];
      const promise = withTurnRetry(
        async () => {
          calls += 1;
          if (calls < 3) throw new UpstreamStatusError("rate limited", 429);
          return "ok";
        },
        { canRetry: () => true, onRetry: (notice) => notices.push(notice.attempt) },
      );
      await vi.runAllTimersAsync();
      expect(await promise).toBe("ok");
      expect(calls).toBe(3);
      // One notice per wait, so the UI can say which attempt is running.
      expect(notices).toEqual([1, 2]);
    } finally {
      vi.useRealTimers();
    }
  });

  it("gives up once the turn has produced output", async () => {
    // A turn that already ran tool calls has written files; replaying it would
    // repeat those side effects, so a failure after any output is final.
    let calls = 0;
    await expect(withTurnRetry(
      async () => {
        calls += 1;
        throw new UpstreamStatusError("rate limited", 429);
      },
      { canRetry: () => false },
    )).rejects.toThrow("rate limited");
    expect(calls).toBe(1);
  });

  it("does not retry an error the request itself caused", async () => {
    let calls = 0;
    await expect(withTurnRetry(
      async () => {
        calls += 1;
        throw new UpstreamStatusError("bad request", 400);
      },
      { canRetry: () => true },
    )).rejects.toThrow("bad request");
    expect(calls).toBe(1);
  });

  it("stops after the attempt budget and rethrows the last failure", async () => {
    vi.useFakeTimers();
    try {
      let calls = 0;
      const promise = withTurnRetry(
        async () => {
          calls += 1;
          throw new UpstreamStatusError("still limited", 429);
        },
        { canRetry: () => true },
      );
      const assertion = expect(promise).rejects.toThrow("still limited");
      await vi.runAllTimersAsync();
      await assertion;
      expect(calls).toBe(RETRY_MAX_ATTEMPTS + 1);
    } finally {
      vi.useRealTimers();
    }
  });
});
