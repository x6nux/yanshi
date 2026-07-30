// ide/vscode/src/test/cancel-race.test.ts
//
// Real OutputRenderer + TurnController terminal race test. Mocks the vscode
// module so vitest can drive the controller's public run()/cancel() surface
// without a VS Code test-electron harness.

import { beforeEach, describe, expect, it, vi } from "vitest";

// vi.hoisted runs before the vi.mock factory is evaluated, so the mock can
// reference the same variables the tests use. See:
// https://vitest.dev/api/vi.html#vi-mock
const { lines, showInformationMessage } = vi.hoisted(() => ({
  lines: [] as string[],
  showInformationMessage: vi.fn(),
}));

vi.mock("vscode", () => ({
  window: {
    activeTextEditor: undefined,
    createOutputChannel: () => ({
      append: (value: string) => lines.push(value),
      appendLine: (value: string) => lines.push(value),
      show: vi.fn(),
      dispose: vi.fn(),
    }),
    showInputBox: vi.fn(async () => "run turn"),
    showInformationMessage,
  },
  workspace: {
    workspaceFolders: [],
    textDocuments: [],
    getConfiguration: () => ({ get: (_key: string, fallback: unknown) => fallback }),
    openTextDocument: vi.fn(),
    workspaceState: undefined,
  },
  commands: { registerCommand: vi.fn(), executeCommand: vi.fn() },
}));

import type { AgentClient, Item } from "@x6nux/yanshi-sdk";
import { TurnController } from "../extension.js";
import { OutputRenderer } from "../output.js";
import { DiffStore } from "../diff.js";

const completed: Item = {
  version: "v1",
  id: "item-1",
  sequence: 1,
  type: "turn.completed",
  threadId: "thread-001",
  turnId: "thread-001-turn-1",
  status: "completed",
};

describe("terminal cancel guard", () => {
  beforeEach(() => {
    lines.length = 0;
    showInformationMessage.mockClear();
  });

  it("does not send remote cancel or overwrite completed output after turn.completed", async () => {
    let remoteCancels = 0;
    const fakeClient = {
      start: vi.fn(async () => ({
        version: "v1",
        thread: {
          version: "v1",
          id: "thread-001",
          status: "active",
          createdAt: 1,
          updatedAt: 1,
        },
      })),
      resume: vi.fn(async () => undefined),
      run: async function* () {
        yield completed;
      },
      cancel: vi.fn(async () => { remoteCancels += 1; }),
      interrupt: vi.fn(async () => ({ version: "v1", ok: true, threadId: "thread-001" })),
    };
    const state: Record<string, unknown> = {};
    const context = {
      secrets: { get: vi.fn(async () => undefined), store: vi.fn(), delete: vi.fn() },
      workspaceState: {
        get: vi.fn((_key: string, fallback: unknown) => fallback),
        update: vi.fn(async (key: string, value: unknown) => { state[key] = value; }),
      },
      subscriptions: [],
    };
    const output = new OutputRenderer();
    const controller = new TurnController(
      context as never,
      output,
      new DiffStore(),
      () => fakeClient as unknown as AgentClient,
    );
    await controller.run();
    await controller.cancel();
    expect(output.isTerminal()).toBe(true);
    expect(remoteCancels).toBe(0);
    expect(lines.some((line) => line.includes("[done]"))).toBe(true);
    expect(lines.some((line) => line.includes("[cancelled]"))).toBe(false);
    // After turn.completed, this.active is undefined, so cancel() falls through
    // to the "no active turn" informational message.
    expect(
      showInformationMessage.mock.calls.some(
        (call) => typeof call[0] === "string" && call[0].includes("no active turn"),
      ),
    ).toBe(true);
  });

  it("shows 'no active turn' when cancel is pressed before any run", async () => {
    const state: Record<string, unknown> = {};
    const context = {
      secrets: { get: vi.fn(async () => undefined), store: vi.fn() },
      workspaceState: {
        get: vi.fn((_key: string, fallback: unknown) => fallback),
        update: vi.fn(async (key: string, value: unknown) => { state[key] = value; }),
      },
      subscriptions: [],
    };
    const controller = new TurnController(context as never);
    await controller.cancel();
    expect(showInformationMessage).toHaveBeenCalledWith("Yanshi: no active turn");
  });
});
