// ide/vscode/src/output.ts
//
// Stream item renderer. Writes to a single OutputChannel; tracks a `terminal`
// flag that the cancel guard checks so a late cancel cannot overwrite the
// completed-turn UI. The renderer is the only place where v1 Item types map
// to user-visible strings, so a future item-type catalog change only touches
// here.
//
// Token / prompts / file contents NEVER pass through this layer in clear text:
// output.ts consumes only the structured fields of Item payloads. Errors print
// `error.message` only, never request bodies or headers.

import * as vscode from "vscode";
import type { Item } from "@x6nux/yanshi-sdk";

export class OutputRenderer {
  private terminal = false;

  constructor(readonly channel: vscode.OutputChannel = vscode.window.createOutputChannel("Yanshi")) {}

  dispose(): void {
    this.channel.dispose();
  }

  /** True once turn.completed or cancel/cancelled has terminated the stream. */
  isTerminal(): boolean {
    return this.terminal;
  }

  begin(threadId: string, turnId: string): void {
    this.terminal = false;
    this.channel.appendLine(`[yanshi] thread=${threadId} turn=${turnId}`);
    this.channel.show(true);
  }

  consume(item: Item): void {
    // Item types are D1's known values plus "event.<legacyType>" for unknown
    // server frames. The renderer must never throw on an unknown type.
    switch (item.type) {
      case "turn.started":
        // Already echoed in begin(); do not duplicate.
        break;
      case "message.delta":
        if (item.text) this.channel.append(item.text);
        break;
      case "reasoning.delta":
        if (item.text) this.channel.appendLine(`\n[thinking] ${item.text}`);
        break;
      case "tool.call":
        this.channel.appendLine(`\n[tool] ${item.toolName ?? "<unknown>"} ${item.toolArgs ?? ""}`);
        break;
      case "tool.result":
        this.channel.appendLine(`\n[tool/result] ${item.toolName ?? "<unknown>"}: ${item.text ?? ""}`);
        break;
      case "tool.progress":
        if (item.text) this.channel.appendLine(`\n[tool/progress] ${item.toolName ?? ""}: ${item.text}`);
        break;
      case "structured.result": {
        const structured = item.structuredResult as
          | { summary?: string; ok?: boolean }
          | undefined
          | null;
        const fallback = structured && typeof structured === "object" && "ok" in structured
          ? (structured.ok ? "ok" : "failed")
          : "n/a";
        this.channel.appendLine(`\n[result] ${structured?.summary ?? fallback}`);
        break;
      }
      case "turn.error":
        this.channel.appendLine(`\n[error] ${item.error ?? "<unknown error>"}`);
        break;
      case "turn.completed":
        this.terminal = true;
        this.channel.appendLine(`\n[done] ${item.status ?? "completed"}`);
        break;
      default:
        // Unknown / event.<legacyType>: keep the type visible so the user can
        // see that the server emitted something the IDE does not understand.
        this.channel.appendLine(`\n[event] ignored ${item.type} at sequence ${item.sequence}`);
        break;
    }
  }

  /**
   * Mark the active turn as cancelled. Returns false if the stream already
   * terminated (turn.completed or a prior cancel); true if this call flipped
   * the flag. Callers use the return value to decide whether to send the
   * remote interrupt.
   */
  markCancelled(): boolean {
    if (this.terminal) return false;
    this.terminal = true;
    this.channel.appendLine("\n[cancelled]");
    return true;
  }

  reconnecting(attempt: number): void {
    this.channel.appendLine(`\n[reconnecting] attempt=${attempt}`);
  }

  reconnected(): void {
    this.channel.appendLine("\n[reconnected]");
  }

  fail(error: unknown): void {
    const message = error instanceof Error ? error.message : String(error);
    this.channel.appendLine(`\n[connection error] ${message}`);
  }
}
