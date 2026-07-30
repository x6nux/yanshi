// ide/vscode/src/diff-model.ts
//
// Pure diff-state model. NO `vscode` import so unit tests can run without
// VS Code's runtime. The model tracks the highest-cursor FileChange it has
// seen so out-of-order events (network reordering, recovery replay) cannot
// replace a newer diff with an older one.
//
// Today the v1 Item payload has no `fileChange` field (it's D2-provisional),
// so the renderer falls back to `item.text` for tool results that contain
// edits. When D1 starts emitting fileChange items, the model picks them up
// automatically — no IDE change required.

import type { FileChange } from "@x6nux/yanshi-sdk";

export interface OrderedChange {
  change: FileChange;
  sequence: number;
}

function compareSequence(left: number, right: number): number {
  if (left < right) return -1;
  if (left > right) return 1;
  return 0;
}

export class DiffModel {
  private latestChange: OrderedChange | undefined;

  /**
   * Record a file change observed at `sequence`. Replaces the cached latest
   * only when `sequence` is >= the previous latest, so out-of-order replay
   * cannot downgrade the diff the user sees.
   */
  remember(change: FileChange, sequence: number): void {
    if (!this.latestChange || compareSequence(sequence, this.latestChange.sequence) >= 0) {
      this.latestChange = { change, sequence };
    }
  }

  latest(): OrderedChange | undefined {
    return this.latestChange;
  }

  clear(): void {
    this.latestChange = undefined;
  }
}

/**
 * Pick the before/after contents for a FileChange. D1's wire shape today has
 * neither field; the IDE falls back to unifiedDiff-only display when that's
 * the only populated field, or to a "no diff content" placeholder when all
 * three are absent (which would indicate a server bug, not a normal state).
 */
export function diffContents(change: FileChange): { before: string; after: string } {
  if (change.before !== undefined || change.after !== undefined) {
    return { before: change.before ?? "", after: change.after ?? "" };
  }
  if (change.unifiedDiff) {
    return { before: "", after: change.unifiedDiff };
  }
  return { before: "", after: `No diff content was supplied for ${change.path}` };
}
