// ide/vscode/src/test/context.test.ts
//
// Pure-function tests for budget.ts. No `vscode` runtime needed; runs in plain
// vitest/Node so it works in CI without VS Code's test-electron harness.

import { describe, expect, it } from "vitest";
import { applyContextBudget, truncateUtf8 } from "../budget.js";

describe("context bounds", () => {
  it("keeps multibyte UTF-8 output within the byte limit", () => {
    const result = truncateUtf8("中文".repeat(100), 7);
    expect(Buffer.byteLength(result.text, "utf8")).toBeLessThanOrEqual(7);
    expect(result.truncated).toBe(true);
  });

  it("does not mark short content as truncated", () => {
    expect(truncateUtf8("package main", 100)).toEqual({ text: "package main", truncated: false });
  });

  it("returns empty when maxBytes is zero or negative", () => {
    expect(truncateUtf8("hi", 0)).toEqual({ text: "", truncated: true });
    expect(truncateUtf8("", 0)).toEqual({ text: "", truncated: false });
  });

  it("shares one byte and item budget between selection and open files", () => {
    const items = applyContextBudget(
      { path: "a.ts", content: "中文中文", range: { start: 0, end: 4 } },
      [
        { path: "b.ts", content: "1234567890" },
        { path: "c.ts", content: "ignored" },
      ],
      { maxOpenFiles: 1, maxContextBytes: 16 },
    );
    expect(items).toHaveLength(2);
    expect(items[0]?.kind).toBe("selection");
    expect((items[0] as { truncated?: boolean }).truncated).toBe(false);
    const totalBytes = items.reduce(
      (sum, item) => sum + Buffer.byteLength(item.content, "utf8"),
      0,
    );
    expect(totalBytes).toBeLessThanOrEqual(16);
    expect(items.filter((item) => item.kind === "openFile")).toHaveLength(1);
  });

  it("stops when selection alone exceeds the byte budget", () => {
    const items = applyContextBudget(
      { path: "a.ts", content: "x".repeat(200), range: { start: 0, end: 200 } },
      [{ path: "b.ts", content: "y".repeat(200) }],
      { maxOpenFiles: 4, maxContextBytes: 32 },
    );
    // Selection is truncated to 32 bytes; open files get 0 bytes → not included.
    expect(items).toHaveLength(1);
    expect(items[0]?.kind).toBe("selection");
    expect((items[0] as { truncated?: boolean }).truncated).toBe(true);
  });

  it("caps open files even when the byte budget is generous", () => {
    const items = applyContextBudget(
      undefined,
      [
        { path: "a.ts", content: "1" },
        { path: "b.ts", content: "2" },
        { path: "c.ts", content: "3" },
        { path: "d.ts", content: "4" },
      ],
      { maxOpenFiles: 2, maxContextBytes: 4096 },
    );
    expect(items).toHaveLength(2);
    expect(items.every((item) => item.kind === "openFile")).toBe(true);
  });
});
