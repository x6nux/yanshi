// sdk/ts/tests/version-matrix.test.ts
//
// Cross-version compatibility matrix. Drives the public AgentClient against
// every version shape the wire contract considers load-bearing:
//   - v1         -> accepted
//   - v1.1       -> accepted (additive minor; future field tolerated)
//   - v2         -> ApiVersionError (major bump)
//   - undefined  -> ApiVersionError (missing)
//   - garbage    -> ApiVersionError (invalid)
//   - "v1."      -> ApiVersionError (malformed minor)
//
// The shape of "v1 + unknown item type" (preserved, not dropped) is covered
// in contract.test.ts; duplicate item sequence delivery is covered in
// transport.test.ts.

import { describe, expect, it } from "vitest";
import { AgentClient } from "../src/client.js";
import { ApiVersionError } from "../src/errors.js";
import type { FetchLike } from "../src/transport.js";

const baseThread = {
  version: "v1",
  thread: {
    version: "v1",
    id: "thread-001",
    status: "active",
    createdAt: 1,
    updatedAt: 1,
  },
};

// Shape that omits BOTH the top-level version and the in-thread version, so
// the SDK truly cannot determine a version (neither header nor body has it).
const versionlessThread = {
  thread: {
    id: "thread-001",
    status: "active",
    createdAt: 1,
    updatedAt: 1,
  },
};

function response(version: string | undefined, body: unknown): Response {
  const headers = new Headers({ "Content-Type": "application/json" });
  if (version) headers.set("X-Yanshi-API-Version", version);
  const payload = version && typeof body === "object" && body !== null
    ? { version, ...(body as Record<string, unknown>) }
    : body;
  return new Response(JSON.stringify(payload), { status: 200, headers });
}

describe("cross-version matrix", () => {
  it("accepts v1 as the canonical version", async () => {
    const fetchImpl: FetchLike = async () => response("v1", baseThread);
    const client = new AgentClient({ baseUrl: "http://localhost", fetch: fetchImpl });
    const result = await client.start();
    expect(result.version).toBe("v1");
    expect(result.thread.id).toBe("thread-001");
  });

  it("accepts v1.1 as an additive minor version through the public client", async () => {
    const fetchImpl: FetchLike = async () =>
      response("v1.1", { ...baseThread, futureField: { additive: true } });
    const client = new AgentClient({ baseUrl: "http://localhost", fetch: fetchImpl });
    const result = await client.start();
    expect(result.thread.id).toBe("thread-001");
    expect((result as unknown as { futureField: { additive: boolean } }).futureField).toEqual({ additive: true });
  });

  it.each([
    ["v2", baseThread],
    [undefined, versionlessThread],
    ["garbage", baseThread],
    ["v1.", baseThread],
  ] as const)(
    "rejects incompatible version %s through the public client",
    async (version, body) => {
      const fetchImpl: FetchLike = async () => response(version, body);
      const client = new AgentClient({ baseUrl: "http://localhost", fetch: fetchImpl });
      await expect(client.start()).rejects.toBeInstanceOf(ApiVersionError);
    },
  );

  it("tolerates unknown server-added fields on every response (forward-compat)", async () => {
    const fetchImpl: FetchLike = async () =>
      response("v1", { ...baseThread, serverRubric: { future: true }, anotherUnknown: 42 });
    const client = new AgentClient({ baseUrl: "http://localhost", fetch: fetchImpl });
    const result = await client.start();
    expect((result as unknown as { serverRubric: { future: boolean } }).serverRubric).toEqual({ future: true });
    expect((result as unknown as { anotherUnknown: number }).anotherUnknown).toBe(42);
  });
});
