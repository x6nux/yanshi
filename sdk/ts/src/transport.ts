// sdk/ts/src/transport.ts
//
// HTTP + SSE transports for the Yanshi v1 Agent API. All D1 paths
// are isolated in this file (the client and IDE never build URLs themselves).
//
// D1 contract served by this module (verified against
// internal/api/http/agent_v1.go and internal/api/v1/types.go):
//   POST /api/v1/thread/start     -> ThreadStartResponse
//   POST /api/v1/thread/resume    -> ThreadResumeResponse
//   POST /api/v1/thread/interrupt -> InterruptResponse
//   POST /api/v1/turn/start       -> SSE stream: `event: turn` once with the
//                                    TurnStartResponse, then `event: item` per
//                                    v1.Item, then closes.
//   GET  /api/v1/schema/agent-v1.json -> canonical JSON Schema
//
// Version handling: D1 emits exactly "v1" in the X-Yanshi-API-Version header
// and inside every response body. The SDK stays tolerant of future minor
// versions ("v1.1") since the wire types permit additive fields; a major bump
// ("v2") or missing version fails closed via ApiVersionError.
//
// Recovery identity: items carry a monotonic `sequence` (>= 1). The SDK records
// the highest sequence observed and surfaces it on StreamDisconnectedError so
// the IDE layer can call thread/resume + (future) cursor replay to continue.
// D1 does not yet implement cursor-based stream replay; recovery today is
// best-effort through thread/resume's items snapshot.

import {
  type Item,
  type ItemType,
} from "./generated.js";
import { isValidVersion } from "./validators.js";
import { ApiVersionError, HttpError, ProtocolError, StreamDisconnectedError } from "./errors.js";

export type FetchLike = (input: string | URL, init?: RequestInit) => Promise<Response>;
export interface TransportOptions {
  baseUrl: string;
  token?: string;
  fetch?: FetchLike;
  supportedVersions?: readonly string[];
}

const KNOWN_ITEM_TYPES: ReadonlySet<string> = new Set<ItemType | "unknown" | `event.${string}`>([
  "turn.started",
  "message.delta",
  "reasoning.delta",
  "tool.call",
  "tool.result",
  "tool.progress",
  "structured.result",
  "turn.error",
  "turn.completed",
]);

export function makeUrl(baseUrl: string, path: string): string {
  return `${baseUrl.replace(/\/$/, "")}/${path.replace(/^\//, "")}`;
}

export function versionIsSupported(version: string | undefined, supported: readonly string[]): boolean {
  if (typeof version !== "string" || version.length === 0) return false;
  // D1 emits exactly "v1". We accept "v1" or any "v1.<minor>" for forward
  // compatibility. Major bumps ("v2") or unrelated strings fail closed.
  const majorMatch = /^v([0-9]+)(?:\.[0-9]+)?$/.exec(version);
  if (!majorMatch) return false;
  const major = `v${majorMatch[1]}`;
  return supported.some((prefix) => prefix === major);
}

// isValidVersion is re-exported from validators so tests can reach the pattern
// without depending on internal TS state. The name is kept for parallelism
// with the Python SDK even though D1's identity is `sequence`.
export { isValidVersion };

function authHeaders(token: string | undefined, accept: string): Headers {
  const headers = new Headers({ Accept: accept });
  if (token) headers.set("Authorization", `Bearer ${token}`);
  return headers;
}

function extractVersion(response: Response, body: unknown): string | undefined {
  const header = response.headers.get("X-Yanshi-API-Version");
  if (header) return header;
  if (typeof body === "object" && body !== null && "version" in body) {
    const v = (body as { version?: unknown }).version;
    return typeof v === "string" ? v : undefined;
  }
  return undefined;
}

export async function requestJson<T>(
  options: TransportOptions,
  path: string,
  init: RequestInit,
): Promise<T> {
  const fetchImpl = options.fetch ?? fetch;
  const merged = new Headers(authHeaders(options.token, "application/json"));
  for (const [k, v] of Object.entries(init.headers ?? {})) {
    if (typeof v === "string") merged.set(k, v);
  }
  const response = await fetchImpl(makeUrl(options.baseUrl, path), { ...init, headers: merged });
  const text = await response.text();
  let body: unknown = undefined;
  if (text) {
    try {
      body = JSON.parse(text);
    } catch (cause) {
      throw new ProtocolError(`non-JSON response from ${path}`, { cause });
    }
  }
  if (!response.ok) throw new HttpError(response.status, body);
  if (typeof body !== "object" || body === null) throw new ProtocolError(`response from ${path} is not a JSON object`);
  const supported = options.supportedVersions ?? ["v1"];
  const version = extractVersion(response, body);
  if (!versionIsSupported(version, supported)) throw new ApiVersionError(version, supported);
  return body as T;
}

// KNOWN_ITEM_TYPES is exported for tests that want to assert the forward-compat
// boundary (unknown server types normalize to "event.<legacyType>" but are
// still yielded).
export { KNOWN_ITEM_TYPES };

/**
 * Parse one SSE `data:` payload into an Item. Enforces the required envelope
 * fields per the v1 schema: version, id, sequence (>= 1), threadId, turnId,
 * type. Unknown `type` strings are preserved as-is so the IDE can render them
 * (D1's contract says unknown item types must not be silently dropped).
 */
export function parseItem(raw: string): Item {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch (cause) {
    throw new ProtocolError("invalid JSON item payload", { cause });
  }
  if (typeof value !== "object" || value === null) throw new ProtocolError("item payload is not an object");
  const item = value as Record<string, unknown>;
  if (typeof item.version !== "string" || !isValidVersion(item.version)) {
    throw new ProtocolError("item version is missing or invalid");
  }
  if (typeof item.id !== "string" || item.id.length === 0) {
    throw new ProtocolError("item id is missing");
  }
  if (typeof item.sequence !== "number" || !Number.isInteger(item.sequence) || item.sequence < 1) {
    throw new ProtocolError("item sequence must be an integer >= 1");
  }
  if (typeof item.threadId !== "string" || item.threadId.length === 0 ||
      typeof item.turnId !== "string" || item.turnId.length === 0) {
    throw new ProtocolError("item is missing threadId or turnId");
  }
  if (typeof item.type !== "string" || item.type.length === 0) {
    throw new ProtocolError("item is missing type");
  }
  return item as unknown as Item;
}

/**
 * Read an SSE response body and yield v1.Item objects. Expects D1's framing:
 *   event: turn        -> exactly once, carries TurnStartResponse (skipped)
 *   event: item        -> zero or more, each `data:` line is one Item
 *   <blank line>       -> event boundary
 *
 * The first non-item event is silently skipped so callers only see Items. Any
 * malformed payload raises ProtocolError; on stream end the generator returns
 * normally. Network errors are wrapped in StreamDisconnectedError with the
 * last observed sequence so the IDE can resume.
 */
export async function* readSse(
  response: Response,
  supported: readonly string[],
): AsyncGenerator<Item> {
  if (!response.ok) throw new HttpError(response.status, await response.text());
  if (!response.body) throw new StreamDisconnectedError(undefined);
  const header = response.headers.get("X-Yanshi-API-Version") ?? undefined;
  if (header && !versionIsSupported(header, supported)) {
    throw new ApiVersionError(header, supported);
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let data: string[] = [];
  let eventName: string | undefined;
  let lastSequence: number | undefined;
  const flush = (): Item | undefined => {
    if (data.length === 0) { eventName = undefined; return undefined; }
    const payload = data.join("\n");
    data = [];
    const name = eventName;
    eventName = undefined;
    if (name !== "item") return undefined;
    const item = parseItem(payload);
    if (!versionIsSupported(item.version, supported)) {
      throw new ApiVersionError(item.version, supported);
    }
    lastSequence = item.sequence;
    return item;
  };
  try {
    while (true) {
      const chunk = await reader.read();
      if (chunk.done) break;
      buffer += decoder.decode(chunk.value, { stream: true });
      const lines = buffer.split(/\r?\n/);
      buffer = lines.pop() ?? "";
      for (const line of lines) {
        if (line === "") {
          const item = flush();
          if (item) yield item;
        } else if (line.startsWith("event: ")) {
          eventName = line.slice(7).trim();
        } else if (line.startsWith("data: ")) {
          data.push(line.slice(6));
        }
      }
    }
    const item = flush();
    if (item) yield item;
  } catch (cause) {
    if (cause instanceof ApiVersionError || cause instanceof ProtocolError) throw cause;
    throw new StreamDisconnectedError(lastSequence, { cause });
  } finally {
    reader.releaseLock();
  }
}

// WebSocket support was removed. The client's ws transport pointed at
// /api/v1/threads/{id}/stream, an endpoint the server has never registered
// (curl returned 404), and that branch had no test — the fake socket injected
// by callers meant nothing ever tried to reach the real path. SSE carries the
// item stream and POST /api/v1/thread/interrupt carries cancellation, so WS
// added no capability to v1; what it added was an advertised option that could
// not work. internal/archtest::TestSDKsOnlyReferenceRegisteredEndpoints keeps
// the phantom from coming back.
