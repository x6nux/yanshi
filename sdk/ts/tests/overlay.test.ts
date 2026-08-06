// sdk/ts/tests/overlay.test.ts
//
// Proves the v1.1 overlay's delta actually constrains something, using the
// validator the SDK really ships with.
//
// v1.1 is an allOf/$ref layer over v1 whose one addition is Item gaining
// reasoningTokens with minimum 0. That delta was DEAD: v1's root anyOf refers
// to "#/$defs/Item", and a JSON Pointer resolves in the $id scope of the
// document that wrote it — v1's own — so it could never reach v1.1's local
// $defs.Item. Measured before the fix: register v1, compile v1.1, feed
// reasoningTokens: -5 → valid, errors null. The one thing the overlay existed
// to say was unsayable, and no test in the repo compared the two documents.
//
// internal/api/v1 has the structural half of this (every local $defs must be
// $ref'd), which is what runs on a machine with no Node. This file is the
// behavioural half: a schema can be structurally reachable and still not
// reject what it claims to reject.

import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import Ajv2020 from "ajv/dist/2020.js";

const load = (p: string) => JSON.parse(readFileSync(new URL(p, import.meta.url), "utf8"));
const v1 = load("../../schema/v1/agent-api.schema.json");
const v11 = load("../../schema/v1.1/agent-api.schema.json");

function compileOverlay() {
  const ajv = new Ajv2020({ strict: false });
  // The relative $ref in v1.1's allOf is the id v1 must be registered under.
  ajv.addSchema(v1, "../v1/agent-api.schema.json");
  return ajv.compile(v11);
}

const item = {
  version: "v1",
  id: "i1",
  sequence: 1,
  threadId: "t1",
  turnId: "u1",
  type: "text",
};

describe("v1.1 additive overlay", () => {
  it("rejects a negative reasoningTokens", () => {
    const validate = compileOverlay();
    expect(validate({ ...item, reasoningTokens: -5 })).toBe(false);
    // Pinning the schemaPath is what distinguishes "the overlay rejected it"
    // from "v1 happened to reject it for some unrelated reason" — the whole
    // defect was that this branch was never consulted.
    expect(JSON.stringify(validate.errors)).toContain("#/$defs/Item");
  });

  it("accepts a non-negative reasoningTokens", () => {
    // The positive control. Without it, an overlay that rejected everything
    // would pass the test above, and "rejects what it should" would be
    // satisfied by a schema that is simply broken.
    const validate = compileOverlay();
    expect(validate({ ...item, reasoningTokens: 5 })).toBe(true);
  });

  it("still accepts a v1 item that says nothing about reasoningTokens", () => {
    // Additive means additive: an existing v1 payload must keep validating.
    const validate = compileOverlay();
    expect(validate(item)).toBe(true);
  });
});
