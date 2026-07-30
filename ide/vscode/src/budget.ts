// ide/vscode/src/budget.ts
//
// Pure-function IDE context budget. NO `vscode` import so unit tests can run
// in plain Node without VS Code's runtime.
//
// The selection and openFiles share ONE byte+item budget so a long selection
// cannot crowd out all open files (and vice versa). UTF-8 truncation is byte
// safe: a multi-byte character is never cut mid-codepoint. The `truncated`
// flag is set whenever the original content was shortened.

import type { ContextItem, OpenFileContext, SelectionContext } from "@x6nux/yanshi-sdk";

export interface ContextLimits {
  maxOpenFiles: number;
  maxContextBytes: number;
}

export interface SelectionCandidate {
  path: string;
  content: string;
  range: { start: number; end: number };
}

export interface OpenFileCandidate {
  path: string;
  content: string;
}

const EMPTY: { text: string; truncated: boolean } = { text: "", truncated: false };

export function truncateUtf8(value: string, maxBytes: number): { text: string; truncated: boolean } {
  if (maxBytes <= 0) {
    return value.length === 0 ? EMPTY : { text: "", truncated: true };
  }
  if (Buffer.byteLength(value, "utf8") <= maxBytes) {
    return { text: value, truncated: false };
  }
  let end = value.length;
  while (end > 0 && Buffer.byteLength(value.slice(0, end), "utf8") > maxBytes) {
    end -= 1;
  }
  const text = value.slice(0, end);
  return { text, truncated: text !== value };
}

/**
 * Build the v1 `context` field for a turn. The selection (if any) is always
 * first; open files follow in their caller-provided order. The shared byte
 * budget is consumed across both; the shared item budget adds 1 for the
 * selection plus the open files up to maxOpenFiles.
 */
export function applyContextBudget(
  selection: SelectionCandidate | undefined,
  openFiles: readonly OpenFileCandidate[],
  limits: ContextLimits,
): ContextItem[] {
  const result: ContextItem[] = [];
  let remainingBytes = Math.max(0, limits.maxContextBytes);
  let remainingOpenFiles = Math.max(0, limits.maxOpenFiles);

  if (selection && remainingBytes > 0) {
    const bounded = truncateUtf8(selection.content, remainingBytes);
    if (bounded.text.length > 0 || selection.content.length === 0) {
      const item: SelectionContext = {
        kind: "selection",
        path: selection.path,
        content: bounded.text,
        range: selection.range,
        truncated: bounded.truncated,
      };
      result.push(item);
      remainingBytes -= Buffer.byteLength(item.content, "utf8");
    }
  }

  for (const candidate of openFiles) {
    if (remainingBytes <= 0 || remainingOpenFiles <= 0) break;
    const bounded = truncateUtf8(candidate.content, remainingBytes);
    if (bounded.text.length === 0 && candidate.content.length > 0) {
      // Even the first byte cannot fit; stop adding items.
      break;
    }
    const item: OpenFileContext = {
      kind: "openFile",
      path: candidate.path,
      content: bounded.text,
      truncated: bounded.truncated,
    };
    result.push(item);
    remainingBytes -= Buffer.byteLength(item.content, "utf8");
    remainingOpenFiles -= 1;
  }
  return result;
}
