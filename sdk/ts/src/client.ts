// sdk/ts/src/client.ts
//
// AgentClient is the public lifecycle entry point. It owns no network state
// beyond the TransportOptions (baseUrl, token, fetch/ws factories) and exposes
// the v1 lifecycle methods mapped to D1's flat routes:
//
//   start(params?)                 -> POST /api/v1/thread/start
//   resume(threadId)               -> POST /api/v1/thread/resume
//   interrupt(threadId, turnId?)   -> POST /api/v1/thread/interrupt
//   run(threadId, params, opts?)   -> POST /api/v1/turn/start (SSE response)
//
// All paths are isolated here so D1's route adapter is a single replace point
// if the canonical V14 contract changes. The IDE never constructs URLs.
//
// Streaming model: D1 emits one SSE response per turn/start that contains
// both the metadata (`event: turn` -> TurnStartResponse) and the item stream
// (`event: item` -> Item, repeated). The client's `run()` async generator
// yields Items and delivers the TurnStartResponse through an `onStarted`
// callback before the first item so the IDE can wire up cancel/diff handlers.
//
// Recovery: D1 does not yet implement cursor-based stream replay. When the
// connection drops mid-stream the SDK surfaces StreamDisconnectedError with
// the last item's sequence; the IDE layer can call resume() to retrieve any
// items the server still has in memory and decide whether to retry.

import type {
  InterruptResponse,
  Item,
  Thread,
  ThreadResumeResponse,
  ThreadStartParams,
  ThreadStartResponse,
  ThreadInterruptParams,
  TurnStartParams,
  TurnStartResponse,
} from "./generated.js";
import type { ContextItem } from "./extensions.js";
import { ApiVersionError, HttpError, ProtocolError, StreamDisconnectedError } from "./errors.js";
import {
  makeUrl,
  parseItem,
  requestJson,
  type FetchLike,
  type TransportOptions,
} from "./transport.js";
import { isValidVersion } from "./validators.js";

// AgentClientOptions used to carry streamTransport?: "sse" | "ws". The ws
// value pointed the client at /api/v1/threads/{id}/stream, which the server
// has never served; SSE is the only stream v1 has, so the option had one legal
// value and one that returned 404.
export type AgentClientOptions = TransportOptions;

export interface RunTurnParams {
  input: string;
  context?: ContextItem[];
  model?: string;
  thinking?: string;
  outputSchema?: unknown;
}

export interface RunOptions {
  signal?: AbortSignal;
  onStarted?: (turn: TurnStartResponse) => void;
}

/**
 * Build the POST body for turn/start. Centralized so run() (SSE) and
 * runMetadata() (SSE-then-parse) never disagree about wire shape.
 */
function buildTurnStartBody(threadId: string, params: RunTurnParams): TurnStartParams {
  const body: TurnStartParams = {
    threadId,
    input: params.input,
  };
  if (params.model !== undefined) body.model = params.model;
  if (params.thinking !== undefined) body.thinking = params.thinking;
  if (params.outputSchema !== undefined) body.outputSchema = params.outputSchema;
  // ContextItem is a D2-provisional field. D1 ignores unknown fields today;
  // when D1 adds ContextItem awareness this becomes honored server-side
  // without an SDK change. Attached via cast to avoid changing the
  // D1-generated TurnStartParams interface before D1 ships the field.
  if (params.context !== undefined) {
    (body as unknown as { context?: ContextItem[] }).context = params.context;
  }
  return body;
}

export class AgentClient {
  private readonly options: AgentClientOptions;

  constructor(options: AgentClientOptions) {
    this.options = {
      ...options,
      baseUrl: options.baseUrl.replace(/\/$/, ""),
      supportedVersions: options.supportedVersions ?? ["v1"],
    };
  }

  /** POST /api/v1/thread/start — create a thread. */
  async start(params: ThreadStartParams = {}): Promise<ThreadStartResponse> {
    return requestJson<ThreadStartResponse>(this.options, "/api/v1/thread/start", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(params ?? {}),
    });
  }

  /** POST /api/v1/thread/resume — load a thread by id (with items snapshot). */
  async resume(threadId: string): Promise<ThreadResumeResponse> {
    if (!threadId) throw new ProtocolError("threadId is required");
    const body: ThreadInterruptParams = { threadId };
    return requestJson<ThreadResumeResponse>(this.options, "/api/v1/thread/resume", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  }

  /** POST /api/v1/thread/interrupt — idempotent cancel of a thread's active turn. */
  async interrupt(threadId: string, turnId?: string): Promise<InterruptResponse> {
    if (!threadId) throw new ProtocolError("threadId is required");
    const body: ThreadInterruptParams = { threadId };
    if (turnId) body.turnId = turnId;
    return requestJson<InterruptResponse>(this.options, "/api/v1/thread/interrupt", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
  }

  /** Alias for interrupt(); kept for naming symmetry with the lifecycle. */
  async cancel(threadId: string, turnId?: string): Promise<InterruptResponse> {
    return this.interrupt(threadId, turnId);
  }

  /**
   * POST /api/v1/turn/start and stream items. D1 emits the SSE response
   * directly: one `event: turn` block carries the TurnStartResponse, then
   * zero or more `event: item` blocks carry Items until the stream closes.
   *
   * onStarted is invoked with the TurnStartResponse before the first item is
   * yielded so callers can wire cancel/diff UI. If onStarted throws, the
   * generator aborts the fetch (best-effort) and re-throws.
   */
  async *run(
    threadId: string,
    params: RunTurnParams,
    options: RunOptions = {},
  ): AsyncGenerator<Item> {
    if (!threadId) throw new ProtocolError("threadId is required");
    if (!params.input?.trim()) throw new ProtocolError("turn input must not be empty");

    const body = buildTurnStartBody(threadId, params);

    // SSE: read metadata + items from one response. We cannot use the simple
    // transport.readSse helper directly because we also need the TurnStart
    // block; do the framing here.
    const fetchImpl: FetchLike = this.options.fetch ?? fetch;
    const response = await fetchImpl(makeUrl(this.options.baseUrl, "/api/v1/turn/start"), {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "text/event-stream",
        ...(this.options.token ? { Authorization: `Bearer ${this.options.token}` } : {}),
      },
      body: JSON.stringify(body),
      signal: options.signal,
    });
    if (!response.ok || !response.body) {
      const text = await response.text().catch(() => "");
      let parsed: unknown = undefined;
      try { parsed = text ? JSON.parse(text) : undefined; } catch { parsed = text; }
      throw new HttpError(response.status, parsed);
    }
    const headerVersion = response.headers.get("X-Yanshi-API-Version") ?? undefined;
    const supported = this.options.supportedVersions ?? ["v1"];
    // Validate header version if present (D1 always sets it). isValidVersion
    // already rejects "v2" / "garbage" / missing — surface it as
    // ApiVersionError so callers see the same failure shape as requestJson().
    if (headerVersion && !isValidVersion(headerVersion)) {
      throw new ApiVersionError(headerVersion, supported);
    }

    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    let dataLines: string[] = [];
    let eventName: string | undefined;
    let started: TurnStartResponse | undefined;
    let lastSequence: number | undefined;
    const flush = (): Item | undefined => {
      if (dataLines.length === 0) { eventName = undefined; return undefined; }
      const payload = dataLines.join("\n");
      dataLines = [];
      const name = eventName;
      eventName = undefined;
      if (name === "turn") {
        try {
          started = JSON.parse(payload) as TurnStartResponse;
        } catch (cause) {
          throw new ProtocolError("invalid JSON in turn/start metadata block", { cause });
        }
        return undefined;
      }
      if (name !== "item") return undefined;
      const item = parseItem(payload);
      lastSequence = item.sequence;
      return item;
    };
    try {
      while (true) {
        if (options.signal?.aborted) return;
        const chunk = await reader.read();
        if (chunk.done) break;
        buffer += decoder.decode(chunk.value, { stream: true });
        const lines = buffer.split(/\r?\n/);
        buffer = lines.pop() ?? "";
        for (const line of lines) {
          if (line === "") {
            const item = flush();
            if (started && options.onStarted) {
              const cb = options.onStarted;
              options.onStarted = undefined; // fire exactly once
              cb(started);
            }
            if (item) yield item;
          } else if (line.startsWith("event: ")) {
            eventName = line.slice(7).trim();
          } else if (line.startsWith("data: ")) {
            dataLines.push(line.slice(6));
          }
        }
      }
      const item = flush();
      if (started && options.onStarted) {
        const cb = options.onStarted;
        options.onStarted = undefined;
        cb(started);
      }
      if (item) yield item;
    } catch (cause) {
      if (cause instanceof StreamDisconnectedError || cause instanceof ProtocolError || cause instanceof HttpError) throw cause;
      throw new StreamDisconnectedError(lastSequence, { cause });
    } finally {
      reader.releaseLock();
    }
  }

  /**
   * Issue turn/start and return only the TurnStartResponse metadata. D1's
   * response is SSE and contains both metadata and items; this helper drains
   * and discards the item stream so callers who only want the metadata (e.g.
   * to read the turn id before issuing a separate cancel call) get a clean
   * promise. Most callers should prefer the run() generator.
   */
  async startTurnMetadata(threadId: string, params: RunTurnParams): Promise<TurnStartResponse> {
    let captured: TurnStartResponse | undefined;
    for await (const _ of this.run(threadId, params, {
      onStarted: (started) => { captured = started; },
    })) {
      // Drain the rest; the caller has asked for metadata only. We still
      // consume the items so the server's producer goroutine is not blocked.
      void _;
      break;
    }
    if (!captured) throw new ProtocolError("turn/start did not emit metadata");
    return captured;
  }
}

export type { Thread };
