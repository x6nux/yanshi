// sdk/ts/tests/client.test.ts
import { describe, expect, it } from "vitest";
import { AgentClient } from "../src/client.js";
import type { FetchLike } from "../src/transport.js";
import { ApiVersionError, HttpError, StreamDisconnectedError } from "../src/errors.js";

function jsonResponse(body: unknown, status = 200, version = "v1"): Response {
  const headers = new Headers({ "Content-Type": "application/json" });
  if (version) headers.set("X-Yanshi-API-Version", version);
  return new Response(JSON.stringify(body), { status, headers });
}

function sseResponse(lines: string[], version = "v1"): Response {
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const line of lines) controller.enqueue(encoder.encode(line));
      controller.close();
    },
  });
  const headers = new Headers({ "Content-Type": "text/event-stream" });
  if (version) headers.set("X-Yanshi-API-Version", version);
  return new Response(body, { status: 200, headers });
}

describe("AgentClient v1 lifecycle", () => {
  it("start/resume/interrupt/cancel hit D1's flat routes with camelCase bodies", async () => {
    const calls: Array<{ path: string; method: string; body: string }> = [];
    const fetchImpl: FetchLike = async (input, init) => {
      const path = new URL(String(input)).pathname;
      calls.push({ path, method: init?.method ?? "GET", body: String(init?.body ?? "") });
      if (path === "/api/v1/thread/start") return jsonResponse({ version: "v1", thread: { version: "v1", id: "thread-001", status: "active", createdAt: 1, updatedAt: 1 } });
      if (path === "/api/v1/thread/resume") return jsonResponse({ version: "v1", thread: { version: "v1", id: "thread-001", status: "active", createdAt: 1, updatedAt: 2 }, items: [] });
      if (path === "/api/v1/thread/interrupt") return jsonResponse({ version: "v1", ok: true, threadId: "thread-001", turnId: "thread-001-turn-1" });
      throw new Error(`unexpected path ${path}`);
    };
    const client = new AgentClient({ baseUrl: "http://127.0.0.1:8080/", fetch: fetchImpl });
    const started = await client.start({ title: "x" });
    await client.resume(started.thread.id);
    const interrupted = await client.interrupt(started.thread.id, "thread-001-turn-1");
    await client.cancel(started.thread.id);
    expect(started.thread.id).toBe("thread-001");
    expect(interrupted.ok).toBe(true);
    expect(calls.map((c) => c.path)).toEqual([
      "/api/v1/thread/start",
      "/api/v1/thread/resume",
      "/api/v1/thread/interrupt",
      "/api/v1/thread/interrupt",
    ]);
    // Bodies are camelCase; unknown fields like context are not added unless set.
    expect(JSON.parse(calls[1].body).threadId).toBe("thread-001");
    expect(JSON.parse(calls[2].body).turnId).toBe("thread-001-turn-1");
    expect("turnId" in JSON.parse(calls[3].body)).toBe(false);
  });

  it("run() yields items from turn/start SSE and surfaces TurnStartResponse via onStarted", async () => {
    const calls: Array<{ path: string; method: string; body: string }> = [];
    const fetchImpl: FetchLike = async (input, init) => {
      const path = new URL(String(input)).pathname;
      calls.push({ path, method: init?.method ?? "GET", body: String(init?.body ?? "") });
      if (path === "/api/v1/turn/start") {
        return sseResponse([
          "event: turn\n",
          "data: {\"version\":\"v1\",\"turn\":{\"version\":\"v1\",\"id\":\"thread-001-turn-1\",\"threadId\":\"thread-001\",\"status\":\"inProgress\",\"input\":\"hello\",\"startedAt\":42}}\n",
          "\n",
          "event: item\n",
          "data: {\"version\":\"v1\",\"id\":\"item-1\",\"sequence\":1,\"threadId\":\"thread-001\",\"turnId\":\"thread-001-turn-1\",\"type\":\"turn.started\"}\n",
          "\n",
          "event: item\n",
          "data: {\"version\":\"v1\",\"id\":\"item-2\",\"sequence\":2,\"threadId\":\"thread-001\",\"turnId\":\"thread-001-turn-1\",\"type\":\"message.delta\",\"text\":\"hello\"}\n",
          "\n",
          "event: item\n",
          "data: {\"version\":\"v1\",\"id\":\"item-3\",\"sequence\":3,\"threadId\":\"thread-001\",\"turnId\":\"thread-001-turn-1\",\"type\":\"structured.result\",\"structuredResult\":{\"ok\":true,\"summary\":\"done\"}}\n",
          "\n",
          "event: item\n",
          "data: {\"version\":\"v1\",\"id\":\"item-4\",\"sequence\":4,\"threadId\":\"thread-001\",\"turnId\":\"thread-001-turn-1\",\"type\":\"turn.completed\",\"status\":\"completed\"}\n",
          "\n",
        ]);
      }
      throw new Error(`unexpected path ${path}`);
    };
    const client = new AgentClient({ baseUrl: "http://localhost", fetch: fetchImpl });
    const started: unknown[] = [];
    const items = [];
    for await (const item of client.run("thread-001", { input: "hello" }, {
      onStarted: (s) => { started.push(s); },
    })) {
      items.push(item);
    }
    expect(started).toHaveLength(1);
    expect((started[0] as { turn: { id: string } }).turn.id).toBe("thread-001-turn-1");
    expect(items.map((i) => i.sequence)).toEqual([1, 2, 3, 4]);
    expect(items.map((i) => i.type)).toEqual(["turn.started", "message.delta", "structured.result", "turn.completed"]);
    expect(calls.map((c) => c.path)).toEqual(["/api/v1/turn/start"]);
    // Body uses camelCase; ContextItem is omitted when not set.
    const body = JSON.parse(calls[0].body);
    expect(body.threadId).toBe("thread-001");
    expect(body.input).toBe("hello");
    expect("context" in body).toBe(false);
  });

  it("wraps non-2xx turn/start as HttpError", async () => {
    const fetchImpl: FetchLike = async () => jsonResponse({ version: "v1", error: { message: "turn already active" } }, 409);
    const client = new AgentClient({ baseUrl: "http://localhost", fetch: fetchImpl });
    await expect(async () => {
      for await (const _ of client.run("t", { input: "hi" })) { void _; }
    }).rejects.toBeInstanceOf(HttpError);
  });

  it("rejects incompatible or missing version on JSON responses", async () => {
    const fetchImpl: FetchLike = async () => new Response(JSON.stringify({ thread: { id: "t" } }), {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
    const client = new AgentClient({ baseUrl: "http://localhost", fetch: fetchImpl });
    await expect(client.start()).rejects.toBeInstanceOf(ApiVersionError);
  });

  it("rethrows StreamDisconnectedError as-is on mid-stream abort", async () => {
    let controllerRef: ReadableStreamController<Uint8Array> | undefined;
    const fetchImpl: FetchLike = async () => {
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          controllerRef = controller;
          controller.enqueue(new TextEncoder().encode(
            "event: turn\ndata: {\"version\":\"v1\",\"turn\":{\"version\":\"v1\",\"id\":\"t-1\",\"threadId\":\"t\",\"status\":\"inProgress\",\"input\":\"hi\",\"startedAt\":1}}\n\n" +
            "event: item\ndata: {\"version\":\"v1\",\"id\":\"item-1\",\"sequence\":1,\"threadId\":\"t\",\"turnId\":\"t-1\",\"type\":\"message.delta\",\"text\":\"x\"}\n\n",
          ));
          // Error on the next microtask so the consumer's first read() succeeds.
          setTimeout(() => controllerRef?.error(new Error("socket dropped")), 0);
        },
      });
      return new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream", "X-Yanshi-API-Version": "v1" } });
    };
    const client = new AgentClient({ baseUrl: "http://localhost", fetch: fetchImpl });
    let lastSeq: number | undefined;
    try {
      for await (const _ of client.run("t", { input: "hi" })) {
        lastSeq = _.sequence;
      }
    } catch (err) {
      expect(err).toBeInstanceOf(StreamDisconnectedError);
      expect((err as StreamDisconnectedError).lastSequence).toBe(1);
      expect(lastSeq).toBe(1);
      return;
    }
    throw new Error("expected StreamDisconnectedError");
  });
});
