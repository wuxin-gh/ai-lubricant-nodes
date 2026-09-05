import { resolveCursorMode } from "../mode.js";
import type { AgentResult, RunnerOptions } from "../../types.js";
import { BaseAcpRunner, type AcpSpawnSpec } from "./base.js";

type RpcMessage = {
  jsonrpc?: string;
  id?: number | string;
  method?: string;
  params?: any;
};

export class CursorAcpRunner extends BaseAcpRunner {
  protected readonly acpProvider = "cursor" as const;

  constructor(options: RunnerOptions) {
    super(options);
  }

  protected spawnSpec(): AcpSpawnSpec {
    const command = process.env.CURSOR_AGENT_EXECUTABLE || "agent";
    const args = ["acp"];
    return { command, args };
  }

  protected authMethodId(): string | undefined {
    return "cursor_login";
  }

  protected sessionParams(storedThreadId: string): Record<string, unknown> {
    return { ...super.sessionParams(storedThreadId), mode: resolveCursorMode(this.options.mode) };
  }

  protected async handleExtension(
    message: RpcMessage,
    respond: (result: unknown) => void,
  ): Promise<boolean> {
    switch (message.method) {
      case "cursor/ask_question": {
        respond({ outcome: { outcome: "skipped", reason: "runtime non-interactive" } });
        return true;
      }
      case "cursor/create_plan": {
        const params = message.params || {};
        this.options.emit?.("agent_event", {
          agent_id: "",
          item: { id: `cursor:plan:${message.id ?? "latest"}`, type: "plan", plan: params.plan || "" },
        });
        respond({ outcome: { outcome: "accepted" } });
        return true;
      }
      case "cursor/update_todos":
      case "cursor/task":
      case "cursor/generate_image":
        return true;
      default:
        return false;
    }
  }
}
