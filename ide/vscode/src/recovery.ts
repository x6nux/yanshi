// ide/vscode/src/recovery.ts
//
// Pure cursor-recovery state machine. NO `vscode` import so the logic is
// unit-testable in plain vitest/Node.
//
// D1 does not yet implement cursor replay on the server side, so this state
// machine is the SDK + IDE contract for when D1 adds it. Today:
//   - on disconnect, the SDK raises StreamDisconnectedError with the last item
//     sequence; the IDE catches it and tries to resume via `thread/resume`
//     (which returns the in-memory item snapshot);
//   - after resume, the IDE restarts the stream from the highest sequence it
//     has seen.
//
// `seen` is preserved across reconnect attempts within one recovery session
// so duplicate item IDs delivered during replay do not duplicate the rendered
// output. The SDK itself does not de-duplicate.

import type { Item } from "@x6nux/yanshi-sdk";

export interface RecoveryHooks {
  /** Start (or resume) the item stream from `afterSequence`. */
  stream(afterSequence: number | undefined): AsyncIterable<Item>;
  /** Refetch the thread snapshot via `thread/resume`. */
  resume(): Promise<void>;
  /** Consume one non-duplicate item. */
  consume(item: Item): Promise<void> | void;
  /** Persist the new high-water mark. */
  save(sequence: number): Promise<void>;
  /** Backoff sleep between reconnect attempts. */
  sleep(ms: number): Promise<void>;
  onReconnecting?(attempt: number): void;
  onReconnected?(): void;
  onError?(error: unknown): void;
  signal?: AbortSignal;
}

/**
 * Run an item stream with reconnect support. Yields each non-duplicate item
 * to hooks.consume, persists the high-water sequence via hooks.save before
 * consume (so a crash mid-consume does not lose the cursor), and retries up
 * to `maxAttempts` times on disconnect. Aborts are checked between every
 * async hop so a cancel during sleep results in zero extra resume() calls.
 */
export async function runWithRecovery(
  hooks: RecoveryHooks,
  initialSequence?: number,
  maxAttempts = 5,
): Promise<number | undefined> {
  const seen = new Set<string>();
  let sequence = initialSequence;

  for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
    if (hooks.signal?.aborted) return sequence;
    try {
      for await (const item of hooks.stream(sequence)) {
        if (hooks.signal?.aborted) return sequence;
        sequence = item.sequence;
        await hooks.save(sequence);
        if (seen.has(item.id)) continue;
        seen.add(item.id);
        await hooks.consume(item);
        if (item.type === "turn.completed") return sequence;
      }
      return sequence;
    } catch (error) {
      if (hooks.signal?.aborted) return sequence;
      hooks.onError?.(error);
      if (attempt === maxAttempts - 1) throw error;
      hooks.onReconnecting?.(attempt + 1);
      const delay = Math.min(8000, 250 * 2 ** attempt);
      await hooks.sleep(delay);
      if (hooks.signal?.aborted) return sequence;
      await hooks.resume();
      if (hooks.signal?.aborted) return sequence;
      hooks.onReconnected?.();
    }
  }
  return sequence;
}
