// sdk/ts/tests/transport.test.ts
import { describe, expect, it, vi } from "vitest";
import {
  KNOWN_ITEM_TYPES,
  makeUrl,
  parseItem,
  readSse,
  readWebSocket,
  versionIsSupported,
  type WebSocketLike,
} from "../src/transport.js";
import { isValidVersion } from "../src/validators.js";
import { ApiVersionError, ProtocolError } from "../src/errors.js";

describe("transport helpers", () => {
  it("strips duplicate slashes when building urls", () => {
    expect(makeUrl("http://localhost/", "/api/v1/thread/start")).toBe("http://localhost/api/v1/thread/start");
    expect(makeUrl("http://localhost", "api/v1/thread/start")).toBe("http://localhost/api/v1/thread/start");
  });

  it("accepts v1 and future v1.x but rejects v2/empty/garbage", () => {
    expect(isValidVersion("v1")).toBe(true);
    expect(isValidVersion("v1.1")).toBe(true);
    expect(isValidVersion("v2")).toBe(false);
    expect(isValidVersion("")).toBe(false);
    expect(isValidVersion(undefined)).toBe(false);
    expect(versionIsSupported("v1", ["v1"])).toBe(true);
    expect(versionIsSupported("v1.1", ["v1"])).toBe(true);
    expect(versionIsSupported("v2", ["v1"])).toBe(false);
    expect(versionIsSupported("", ["v1"])).toBe(false);
    expect(versionIsSupported("v1.", ["v1"])).toBe(false);
    expect(versionIsSupported("garbage", ["v1"])).toBe(false);
  });

  it("parseItem accepts a canonical v1 item", () => {
    const item = parseItem(JSON.stringify({
      version: "v1",
      id: "item-1",
      sequence: 1,
      threadId: "thread-001",
      turnId: "thread-001-turn-1",
      type: "message.delta",
      text: "hello",
    }));
    expect(item.sequence).toBe(1);
    expect(item.type).toBe("message.delta");
  });

  it("parseItem preserves unknown item types rather than dropping them", () => {
    const item = parseItem(JSON.stringify({
      version: "v1",
      id: "item-99",
      sequence: 99,
      threadId: "thread-001",
      turnId: "thread-001-turn-1",
      type: "event.future_telemetry",
      futurePayload: { keep: true },
    }));
    expect(item.type).toBe("event.future_telemetry");
    expect((item as unknown as { futurePayload: { keep: boolean } }).futurePayload).toEqual({ keep: true });
    // Known types are still all present in the catalog (regression guard).
    for (const known of ["turn.started", "message.delta", "reasoning.delta", "tool.call", "tool.result", "tool.progress", "structured.result", "turn.error", "turn.completed"]) {
      expect(KNOWN_ITEM_TYPES.has(known)).toBe(true);
    }
  });

  it.each([
    ["missing version", { id: "x", sequence: 1, threadId: "t", turnId: "r", type: "message.delta" }],
    ["bad sequence", { version: "v1", id: "x", sequence: 0, threadId: "t", turnId: "r", type: "message.delta" }],
    ["missing threadId", { version: "v1", id: "x", sequence: 1, turnId: "r", type: "message.delta" }],
    ["empty type", { version: "v1", id: "x", sequence: 1, threadId: "t", turnId: "r", type: "" }],
  ])("parseItem rejects %s", (_name, payload) => {
    expect(() => parseItem(JSON.stringify(payload))).toThrow(ProtocolError);
  });

  it("reads SSE items, skips the turn event, and normalizes close", async () => {
    const payload = [
      "event: turn\n",
      "data: {\"version\":\"v1\",\"turn\":{\"id\":\"turn-1\",\"threadId\":\"t\",\"status\":\"inProgress\",\"input\":\"hi\",\"version\":\"v1\",\"startedAt\":1}}\n",
      "\n",
      "event: item\n",
      "data: {\"version\":\"v1\",\"id\":\"item-1\",\"sequence\":1,\"threadId\":\"t\",\"turnId\":\"turn-1\",\"type\":\"turn.started\"}\n",
      "\n",
      "event: item\n",
      "data: {\"version\":\"v1\",\"id\":\"item-2\",\"sequence\":2,\"threadId\":\"t\",\"turnId\":\"turn-1\",\"type\":\"message.delta\",\"text\":\"hi\"}\n",
      "\n",
      "event: item\n",
      "data: {\"version\":\"v1\",\"id\":\"item-3\",\"sequence\":3,\"threadId\":\"t\",\"turnId\":\"turn-1\",\"type\":\"event.future_kind\",\"futurePayload\":{\"keep\":true}}\n",
      "\n",
    ];
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        const encoder = new TextEncoder();
        for (const line of payload) controller.enqueue(encoder.encode(line));
        controller.close();
      },
    });
    const response = new Response(body, {
      status: 200,
      headers: { "Content-Type": "text/event-stream", "X-Yanshi-API-Version": "v1" },
    });
    const items = [];
    for await (const item of readSse(response, ["v1"])) items.push(item);
    expect(items.map((item) => item.sequence)).toEqual([1, 2, 3]);
    expect(items.map((item) => item.type)).toEqual(["turn.started", "message.delta", "event.future_kind"]);
    expect((items[2] as unknown as { futurePayload: { keep: boolean } }).futurePayload).toEqual({ keep: true });
  });

  it("rejects incompatible version from the SSE header before reading the body", async () => {
    const body = new ReadableStream<Uint8Array>({
      start(controller) { controller.close(); },
    });
    const response = new Response(body, {
      status: 200,
      headers: { "Content-Type": "text/event-stream", "X-Yanshi-API-Version": "v2" },
    });
    await expect(async () => {
      for await (const _ of readSse(response, ["v1"])) { /* drain */ }
    }).rejects.toBeInstanceOf(ApiVersionError);
  });

  it("accepts narrow callback handlers through socketOn under strictFunctionTypes", async () => {
    const listeners = new Map<string, (...args: unknown[]) => void>();
    const socket: WebSocketLike = {
      readyState: 1,
      send: vi.fn(),
      close: vi.fn(),
      on(name, listener) { listeners.set(name, listener); },
    };
    const iterator = readWebSocket(socket, ["v1"], { turnId: "turn-1" });
    const pending = iterator.next();
    await Promise.resolve();
    listeners.get("message")?.({
      data: JSON.stringify({
        version: "v1", id: "item-1", sequence: 1,
        threadId: "t", turnId: "turn-1", type: "turn.started",
      }),
    });
    listeners.get("close")?.({ code: 1000, reason: "done" });
    expect((await pending).value).toMatchObject({ sequence: 1 });
    await iterator.return(undefined);
  });
});
