import fs from "node:fs/promises";
import path from "node:path";
import { ensureDir, readText } from "./fs.js";
import type { Provider, StoredThread } from "./types.js";

export function providerStatePath(stateRoot: string, provider: Provider, sessionScope?: string): string {
  const scope = sanitizeScope(sessionScope);
  return scope
    ? path.join(stateRoot, "agents", "sessions", scope, "providers", `${provider}.json`)
    : path.join(stateRoot, "agents", "providers", `${provider}.json`);
}

export async function readStoredThread(
  stateRoot: string,
  provider: Provider,
  sessionScope?: string,
): Promise<StoredThread | null> {
  try {
    const raw = await readText(providerStatePath(stateRoot, provider, sessionScope));
    const payload = JSON.parse(raw);
    if (typeof payload?.threadId === "string") {
      return payload;
    }
    if (typeof payload?.sessionId === "string") {
      return { ...payload, threadId: payload.sessionId };
    }
    return null;
  } catch {
    return null;
  }
}

export async function writeStoredThread(
  stateRoot: string,
  provider: Provider,
  threadId: string,
  now: Date = new Date(),
  sessionScope?: string,
): Promise<void> {
  if (!threadId) {
    return;
  }
  const target = providerStatePath(stateRoot, provider, sessionScope);
  await ensureDir(path.dirname(target));
  const payload = {
    provider,
    threadId,
    updatedAt: now.toISOString(),
  };
  await fs.writeFile(target, `${JSON.stringify(payload, null, 2)}\n`, "utf8");
}

function sanitizeScope(scope?: string): string {
  const value = String(scope || "").trim();
  if (!value) {
    return "";
  }
  return Buffer.from(value, "utf8").toString("base64url");
}

/**
 * Seed the provider thread from an adopt-resume hint, without ever overruling
 * a thread the runner already recorded.
 *
 * Why this exists: adopting an external editor session into a platform task
 * gives the session a NEW runtime session id, so the state file under that scope
 * is absent and the runner's `resume` would start a blank conversation. The hint
 * (tags["resume_thread_id"] from the data service) is written here so the
 * existing resume chain works unchanged, for every provider, with no per-runner
 * change.
 *
 * The "never overrule" half is the part that matters on re-dispatch: a resume
 * can fork a new provider thread, and the runner records that forked id after the
 * turn. If a later dispatch re-sent the ORIGINAL hint and we clobbered the file,
 * the task would silently fall back to the pre-adoption history and lose every
 * turn taken since. Returns whether a seed actually happened.
 */
export async function seedResumeThread(
  stateRoot: string,
  provider: Provider,
  hint: string,
  sessionScope?: string,
): Promise<boolean> {
  const threadId = String(hint || "").trim();
  if (!threadId) {
    return false;
  }
  const stored = await readStoredThread(stateRoot, provider, sessionScope);
  if (stored?.threadId) {
    return false;
  }
  await writeStoredThread(stateRoot, provider, threadId, new Date(), sessionScope);
  return true;
}
