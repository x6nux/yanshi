// ide/vscode/src/context.ts
//
// IDE-side context collector. Reads the active editor's selection (if any) and
// the workspace's open text documents, then routes them through the pure
// `budget.ts` so unit tests can exercise the byte/item bounds without a VS
// Code runtime.

import * as path from "node:path";
import type * as vscode from "vscode";
import type { ContextItem } from "@x6nux/yanshi-sdk";
import {
  applyContextBudget,
  type ContextLimits,
  type OpenFileCandidate,
  type SelectionCandidate,
} from "./budget.js";

function isWorkspaceFile(document: vscode.TextDocument): boolean {
  return document.uri.scheme === "file" && !document.isUntitled;
}

/**
 * Collect the v1 `context` field for a turn. Exposed for unit testing with
 * an injected editor (the real entry point in extension.ts passes VS Code's
 * activeTextEditor).
 */
export function collectContext(
  limits: ContextLimits,
  editor: vscode.TextEditor | undefined,
  documents: readonly vscode.TextDocument[] = [],
): ContextItem[] {
  let selection: SelectionCandidate | undefined;
  if (editor && !editor.selection.isEmpty) {
    selection = {
      path: path.normalize(editor.document.uri.fsPath),
      content: editor.document.getText(editor.selection),
      range: {
        start: editor.document.offsetAt(editor.selection.start),
        end: editor.document.offsetAt(editor.selection.end),
      },
    };
  }
  const openFiles: OpenFileCandidate[] = documents
    .filter(isWorkspaceFile)
    .map((document) => ({
      path: path.normalize(document.uri.fsPath),
      content: document.getText(),
    }));
  return applyContextBudget(selection, openFiles, limits);
}
