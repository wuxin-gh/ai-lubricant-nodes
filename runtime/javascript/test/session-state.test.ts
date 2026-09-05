import fs from "node:fs/promises";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { providerStatePath, readStoredThread, writeStoredThread } from "../src/session-state.js";
import { withTempSession } from "./helpers.js";

describe("provider thread state", () => {
  it("uses the compatible provider state path", async () => {
    await withTempSession(async (root) => {
      expect(providerStatePath(path.join(root, "state"), "codex")).toBe(
        path.join(root, "state", "agents", "providers", "codex.json"),
      );
    });
  });

  it("returns null for absent or malformed state", async () => {
    await withTempSession(async (root) => {
      const stateRoot = path.join(root, "state");
      expect(await readStoredThread(stateRoot, "codex")).toBeNull();

      const target = providerStatePath(stateRoot, "codex");
      await fs.mkdir(path.dirname(target), { recursive: true });
      await fs.writeFile(target, "{\"threadId\": 3}", "utf8");

      expect(await readStoredThread(stateRoot, "codex")).toBeNull();

      await fs.writeFile(target, "{\"provider\":\"codex\"}", "utf8");

      expect(await readStoredThread(stateRoot, "codex")).toBeNull();
    });
  });

  it("characterizes current whitespace thread id compatibility", async () => {
    await withTempSession(async (root) => {
      const stateRoot = path.join(root, "state");
      const target = providerStatePath(stateRoot, "codex");
      await fs.mkdir(path.dirname(target), { recursive: true });
      await fs.writeFile(target, "{\"threadId\":\"   \",\"updatedAt\":\"2026-01-01T00:00:00.000Z\"}", "utf8");

      await expect(readStoredThread(stateRoot, "codex")).resolves.toEqual({
        threadId: "   ",
        updatedAt: "2026-01-01T00:00:00.000Z",
      });
    });
  });

  it("writes and reads thread id state", async () => {
    await withTempSession(async (root) => {
      const stateRoot = path.join(root, "state");
      const now = new Date("2026-01-01T00:00:00.000Z");

      await writeStoredThread(stateRoot, "claude", "thread-1", now);

      await expect(readStoredThread(stateRoot, "claude")).resolves.toEqual({
        provider: "claude",
        threadId: "thread-1",
        updatedAt: now.toISOString(),
      });
    });
  });

  it("reads legacy session id state", async () => {
    await withTempSession(async (root) => {
      const stateRoot = path.join(root, "state");
      const target = providerStatePath(stateRoot, "codex");
      await fs.mkdir(path.dirname(target), { recursive: true });
      await fs.writeFile(target, '{"provider":"codex","sessionId":"legacy-thread"}', "utf8");

      await expect(readStoredThread(stateRoot, "codex")).resolves.toEqual({
        provider: "codex",
        sessionId: "legacy-thread",
        threadId: "legacy-thread",
      });
    });
  });

  it("isolates provider state by session scope", async () => {
    await withTempSession(async (root) => {
      const stateRoot = path.join(root, "state");
      const now = new Date("2026-01-01T00:00:00.000Z");

      await writeStoredThread(stateRoot, "codex", "thread-a", now, "editor-session-a");
      await writeStoredThread(stateRoot, "codex", "thread-b", now, "editor-session-b");

      await expect(readStoredThread(stateRoot, "codex", "editor-session-a")).resolves.toMatchObject({
        threadId: "thread-a",
      });
      await expect(readStoredThread(stateRoot, "codex", "editor-session-b")).resolves.toMatchObject({
        threadId: "thread-b",
      });
      expect(providerStatePath(stateRoot, "codex", "editor-session-a")).not.toBe(
        providerStatePath(stateRoot, "codex", "editor-session-b"),
      );
    });
  });

  it("does not create state for an empty thread id", async () => {
    await withTempSession(async (root) => {
      const stateRoot = path.join(root, "state");

      await writeStoredThread(stateRoot, "gemini", "");

      await expect(fs.stat(providerStatePath(stateRoot, "gemini"))).rejects.toMatchObject({ code: "ENOENT" });
    });
  });
});
