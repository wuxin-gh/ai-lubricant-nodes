import type { ChildProcess } from "node:child_process";

/**
 * Kill a spawned provider CLI when the interactive turn is cancelled.
 * Returns a cleanup that removes the listener after the child exits. Native
 * SDK runners use their own AbortSignal options and do not call this helper.
 */
export function bindAbortToChild(
  child: ChildProcess,
  controller?: AbortController,
): () => void {
  const signal = controller?.signal;
  if (!signal) return () => undefined;
  const abort = () => {
    if (!child.killed) child.kill();
  };
  if (signal.aborted) abort();
  else signal.addEventListener("abort", abort, { once: true });
  return () => signal.removeEventListener("abort", abort);
}
