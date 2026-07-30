// ide/vscode/src/extension.ts
//
// TurnController wires VS Code commands to the SDK's AgentClient. It owns:
//   - the per-turn OutputRenderer + DiffStore
//   - the active AbortController (so cancel can stop the local stream)
//   - the persisted (threadId, lastSequence) state in workspaceState
//
// The controller is deliberately small: real recovery (cursor replay,
// reconnect backoff, duplicate eventId dedup) lives in recovery.ts so it can
// be tested without a VS Code runtime.

import * as vscode from "vscode";
import { AgentClient, type Item } from "@x6nux/yanshi-sdk";
import { clientOptions, readConfig, type ExtensionConfig } from "./config.js";
import { collectContext } from "./context.js";
import { DiffStore } from "./diff.js";
import { OutputRenderer } from "./output.js";

interface PersistedState {
  threadId?: string;
  lastSequence?: number;
}

type ClientFactory = (config: ExtensionConfig) => AgentClient;

export class TurnController {
  private client: AgentClient | undefined;
  private active: { threadId: string; turnId: string; cancel: AbortController } | undefined;
  private state: PersistedState;

  constructor(
    private readonly context: vscode.ExtensionContext,
    private readonly output: OutputRenderer = new OutputRenderer(),
    private readonly diffs: DiffStore = new DiffStore(),
    private readonly clientFactory: ClientFactory = (config) => new AgentClient(clientOptions(config)),
  ) {
    this.state = context.workspaceState.get<PersistedState>("yanshi.state", {}) ?? {};
  }

  dispose(): void {
    this.output.dispose();
  }

  /** yanshi.run: prompt the user, start a turn, stream items to OutputChannel. */
  async run(): Promise<void> {
    if (this.active) {
      void vscode.window.showInformationMessage("Yanshi: a turn is already running");
      return;
    }
    const editor = vscode.window.activeTextEditor;
    const prompt = await vscode.window.showInputBox({ prompt: "Send a turn to Yanshi", ignoreFocusOut: true });
    if (!prompt?.trim()) return;
    const config = await readConfig(this.context);
    this.client = this.clientFactory(config);

    let threadId = this.state.threadId;
    try {
      if (!threadId) {
        const started = await this.client.start({
          title: "VS Code",
          model: undefined,
        });
        threadId = started.thread.id;
        await this.save({ threadId, lastSequence: undefined });
      } else {
        await this.client.resume(threadId);
      }
    } catch (error) {
      this.output.fail(error);
      return;
    }

    const abort = new AbortController();
    let capturedTurnId: string | undefined;
    try {
      const documents = vscode.workspace.textDocuments;
      const context = collectContext(
        { maxOpenFiles: config.maxOpenFiles, maxContextBytes: config.maxContextBytes },
        editor,
        documents,
      );
      for await (const item of this.client.run(threadId, { input: prompt, context }, {
        signal: abort.signal,
        transport: config.streamTransport,
        onStarted: (started) => {
          capturedTurnId = started.turn.id;
          this.active = { threadId, turnId: started.turn.id, cancel: abort };
          this.output.begin(threadId, started.turn.id);
        },
      })) {
        await this.consume(item);
      }
    } catch (error) {
      if (!abort.signal.aborted) this.output.fail(error);
    } finally {
      this.active = undefined;
    }
  }

  /** yanshi.cancel: send interrupt if and only if the stream is not terminal. */
  async cancel(): Promise<void> {
    const active = this.active;
    if (!active || !this.client) {
      void vscode.window.showInformationMessage("Yanshi: no active turn");
      return;
    }
    if (!this.output.markCancelled()) {
      // Stream already terminated; do not send a redundant remote interrupt.
      return;
    }
    active.cancel.abort();
    try {
      await this.client.cancel(active.threadId, active.turnId);
    } catch (error) {
      this.output.fail(error);
    }
  }

  /** yanshi.showDiff: open the most recent FileChange in a diff editor. */
  async showLastDiff(): Promise<void> {
    await this.diffs.show();
  }

  private async consume(item: Item): Promise<void> {
    this.output.consume(item);
    await this.save({ threadId: item.threadId, lastSequence: item.sequence });
    // fileChange is a D2-provisional field; D1's Item type does not yet expose
    // it canonically, but the wire's `extra="allow"` contract means it round-
    // trips when present. Cast through unknown for the optional field access.
    const maybeChange = (item as unknown as { fileChange?: import("@x6nux/yanshi-sdk").FileChange }).fileChange;
    if (maybeChange) {
      this.diffs.remember(maybeChange, item.sequence);
    }
  }

  private async save(state: PersistedState): Promise<void> {
    this.state = state;
    await this.context.workspaceState.update("yanshi.state", state);
  }
}

export function activate(context: vscode.ExtensionContext): void {
  const controller = new TurnController(context);
  context.subscriptions.push(
    vscode.commands.registerCommand("yanshi.run", () => controller.run()),
    vscode.commands.registerCommand("yanshi.cancel", () => controller.cancel()),
    vscode.commands.registerCommand("yanshi.showDiff", () => controller.showLastDiff()),
    { dispose: () => controller.dispose() },
  );
}

export function deactivate(): void {
  // No-op: the SDK's http transport cleans up via GC once the extension
  // deactivates. VS Code calls deactivate on extension unload/down.
}
