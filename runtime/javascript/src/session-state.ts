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
