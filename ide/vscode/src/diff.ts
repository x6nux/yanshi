// ide/vscode/src/diff.ts
//
// VS Code-backed diff viewer. Uses the pure DiffModel for state and turns
// FileChange payloads into in-memory untitled documents. NEVER writes to
// workspace files: the IDE's job is to show what the agent changed, not to
// apply it (autoVCS / agent fs tools handle application through the server).

import * as path from "node:path";
import * as vscode from "vscode";
import type { FileChange } from "@x6nux/yanshi-sdk";
import { DiffModel, diffContents } from "./diff-model.js";

const LANGUAGE_BY_EXT: Record<string, string> = {
  ".go": "go",
  ".py": "python",
  ".ts": "typescript",
  ".tsx": "typescriptreact",
  ".js": "javascript",
  ".jsx": "javascriptreact",
  ".json": "json",
  ".md": "markdown",
  ".diff": "diff",
  ".patch": "diff",
  ".yaml": "yaml",
  ".yml": "yaml",
};

function languageFor(filePath: string): string | undefined {
  const ext = path.extname(filePath).toLowerCase();
  return LANGUAGE_BY_EXT[ext];
}

export class DiffStore {
  private readonly model = new DiffModel();

  remember(change: FileChange, sequence: number): void {
    this.model.remember(change, sequence);
  }

  latest(): FileChange | undefined {
    return this.model.latest()?.change;
  }

  clear(): void {
    this.model.clear();
  }

  async show(change: FileChange | undefined = this.latest()): Promise<void> {
    if (!change) {
      void vscode.window.showInformationMessage("Yanshi: no file diff received yet");
      return;
    }
    const contents = diffContents(change);
    const language = change.unifiedDiff && change.before === undefined && change.after === undefined
      ? "diff"
      : languageFor(change.path);
    const left = await vscode.workspace.openTextDocument({ content: contents.before, language });
    const right = await vscode.workspace.openTextDocument({ content: contents.after, language });
    await vscode.commands.executeCommand(
      "vscode.diff",
      left.uri,
      right.uri,
      `Yanshi: ${change.path}`,
      { preview: false },
    );
  }
}
