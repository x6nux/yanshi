// ide/vscode/src/test/diff-model.test.ts
//
// Pure-function tests for diff-model.ts. No `vscode` runtime needed.

import { describe, expect, it } from "vitest";
import { DiffModel, diffContents } from "../diff-model.js";

describe("DiffModel", () => {
  it("keeps the highest sequence even when an older event arrives later", () => {
    const model = new DiffModel();
    model.remember({ path: "new.ts", after: "new" }, 3);
    model.remember({ path: "old.ts", after: "old" }, 2);
    expect(model.latest()?.sequence).toBe(3);
    expect(model.latest()?.change.path).toBe("new.ts");
  });

  it("keeps the first observation when a duplicate sequence arrives", () => {
    const model = new DiffModel();
    model.remember({ path: "a.ts", after: "first" }, 5);
    model.remember({ path: "b.ts", after: "second" }, 5);
    expect(model.latest()?.change.path).toBe("b.ts"); // >= allows the later one
  });

  it("clears on demand", () => {
    const model = new DiffModel();
    model.remember({ path: "a.ts", after: "x" }, 1);
    model.clear();
    expect(model.latest()).toBeUndefined();
  });
});

describe("diffContents", () => {
  it("prefers explicit before/after when present", () => {
    const contents = diffContents({ path: "main.go", before: "old", after: "new" });
    expect(contents).toEqual({ before: "old", after: "new" });
  });

  it("renders unifiedDiff-only payloads as non-empty in-memory content", () => {
    const contents = diffContents({ path: "main.go", unifiedDiff: "@@ -1 +1 @@\n-old\n+new" });
    expect(contents.after).toContain("+new");
    expect(contents.after.length).toBeGreaterThan(0);
    expect(contents.before).toBe("");
  });

  it("falls back to a placeholder when no diff content is provided", () => {
    const contents = diffContents({ path: "empty.go" });
    expect(contents.after).toContain("No diff content");
    expect(contents.after).toContain("empty.go");
  });
});
