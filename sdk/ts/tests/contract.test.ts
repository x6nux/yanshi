// sdk/ts/tests/contract.test.ts
//
// Schema-and-fixture driven contract test. Reads the shared provisional
// schema at ../../schema/v1/agent-api.schema.json and proves every fixture
// validates against it, then checks the IDE/diff recovery invariants the SDK
// actually depends on (sequence monotonicity, structuredResult presence,
// additive unknown-field tolerance).

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

const schemaRoot = new URL("../../schema/v1/", import.meta.url);
const schema = JSON.parse(readFileSync(new URL("agent-api.schema.json", schemaRoot), "utf8"));
const threadStart = JSON.parse(readFileSync(new URL("fixtures/thread-start.response.json", schemaRoot), "utf8"));
const threadResume = JSON.parse(readFileSync(new URL("fixtures/thread-resume.response.json", schemaRoot), "utf8"));
const turnStart = JSON.parse(readFileSync(new URL("fixtures/turn-start.response.json", schemaRoot), "utf8"));
const interrupt = JSON.parse(readFileSync(new URL("fixtures/interrupt.response.json", schemaRoot), "utf8"));
const items = readFileSync(new URL("fixtures/items.jsonl", schemaRoot), "utf8")
  .trim().split("\n").map((line) => JSON.parse(line));
const unknownItem = JSON.parse(readFileSync(new URL("fixtures/unknown-item.json", schemaRoot), "utf8"));

describe("v1 contract fixtures", () => {
  const ajv = new Ajv2020({ strict: false, allowUnionTypes: true });
  addFormats(ajv);
  const validate = ajv.compile(schema);

  it("validates every response fixture, including additive server-added fields", () => {
    for (const fixture of [threadStart, threadResume, turnStart, interrupt]) {
      expect(validate(fixture), JSON.stringify(validate.errors)).toBe(true);
    }
  });

  it("validates each item in the shared items.jsonl fixture", () => {
    for (const item of items) {
      expect(validate(item), JSON.stringify(validate.errors)).toBe(true);
    }
  });

  it("keeps item sequences strictly monotonic in the fixture", () => {
    expect(items.map((i) => i.sequence)).toEqual([1, 2, 3, 4, 5, 6, 7]);
  });

  it("delivers structuredResult on the structured.result item", () => {
    const structured = items.find((i) => i.type === "structured.result");
    expect(structured?.structuredResult).toEqual({ ok: true, summary: "done", artifacts: ["main.go"] });
  });

  it("preserves unknown item types and their payloads (forward-compat)", () => {
    expect(validate(unknownItem)).toBe(true);
    expect(unknownItem.type).toBe("event.future_telemetry");
    expect(unknownItem.futurePayload).toEqual({ preserveSequence: true });
  });

  it("rejects items with bad sequence or missing threadId", () => {
    expect(validate({ ...items[0], sequence: 0 })).toBe(false);
    expect(validate({ ...items[0], threadId: "" })).toBe(false);
    expect(validate({ ...items[0], version: "v2" })).toBe(false);
  });
});
