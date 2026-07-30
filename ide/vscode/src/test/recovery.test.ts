// ide/vscode/src/test/recovery.test.ts
//
// Unit tests for the cursor-recovery state machine. No `vscode` import; uses
// fake hooks so the entire flow (stream → drop → resume → dedup → complete)
// runs deterministically.

import { describe, expect, it } from "vitest";
import { runWithRecovery } from "../recovery.js";
import type { Item } from "@x6nux/yanshi-sdk";

function item(id: string, sequence: number, type: Item["type"]): Item {
  return {
    version: "v1",
    id,
    sequence,
    type,
    threadId: "thread-001",
    turnId: "thread-001-turn-1",
  };
}

describe("runWithRecovery", () => {
  it("resumes from sequence 1, persists before consume, and ignores duplicate item ids", async () => {
    const requested: Array<number | undefined> = [];
    const consumed: string[] = [];
    const saved: number[] = [];
    const reconnecting: number[] = [];
    const reconnected: number[] = [];
    let attempts = 0;

    const result = await runWithRecovery({
      stream(afterSequence) {
        requested.push(afterSequence);
        attempts += 1;
        if (attempts === 1) {
          return (async function* () {
            yield item("item-1", 1, "message.delta");
            throw new Error("socket dropped");
          })();
        }
        return (async function* () {
          yield item("item-1", 1, "message.delta"); // duplicate id
          yield item("item-2", 2, "message.delta");
          yield item("item-3", 3, "turn.completed");
        })();
      },
      resume: async () => undefined,
      consume: (value) => { consumed.push(value.id); },
      save: async (value) => { saved.push(value); },
      sleep: async () => undefined,
      onReconnecting: (attempt) => reconnecting.push(attempt),
      onReconnected: () => reconnected.push(1),
    });

    expect(result).toBe(3);
    expect(requested).toEqual([undefined, 1]);
    expect(consumed).toEqual(["item-1", "item-2", "item-3"]);
    expect(saved).toEqual([1, 1, 2, 3]);
    expect(reconnecting).toEqual([1]);
    expect(reconnected).toEqual([1]);
  });

  it("does not call resume when aborted during sleep", async () => {
    const controller = new AbortController();
    let resumed = 0;
    const result = await runWithRecovery({
      stream: async function* () { throw new Error("dropped"); },
      resume: async () => { resumed += 1; },
      consume: () => undefined,
      save: async () => undefined,
      sleep: async () => { controller.abort(); },
      signal: controller.signal,
    });
    expect(result).toBeUndefined();
    expect(resumed).toBe(0);
  });

  it("rethrows after maxAttempts failed reconnects", async () => {
    let attempts = 0;
    await expect(async () => {
      await runWithRecovery({
        stream() {
          attempts += 1;
          return (async function* () {
            throw new Error("always drops");
          })();
        },
        resume: async () => undefined,
        consume: () => undefined,
        save: async () => undefined,
        sleep: async () => undefined,
      }, undefined, 2);
    }).rejects.toThrow("always drops");
    expect(attempts).toBe(2);
  });

  it("aborts mid-stream yield cleanly without consuming more", async () => {
    const controller = new AbortController();
    const consumed: string[] = [];
    const result = await runWithRecovery({
      stream: async function* () {
        yield item("item-1", 1, "message.delta");
        controller.abort();
        yield item("item-2", 2, "message.delta"); // should not be consumed
      },
      resume: async () => undefined,
      consume: (value) => { consumed.push(value.id); },
      save: async () => undefined,
      sleep: async () => undefined,
      signal: controller.signal,
    });
    expect(result).toBe(1);
    expect(consumed).toEqual(["item-1"]);
  });
});
