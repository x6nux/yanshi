// sdk/ts/src/errors.ts
//
// SDK error hierarchy. Every failure mode the transport surfaces maps to one of
// these classes so callers can branch on `instanceof` without parsing messages.
//   - ApiVersionError       : server reported an incompatible or missing version
//   - HttpError             : non-2xx HTTP response; body captured for diagnostics
//   - ProtocolError         : malformed JSON, missing required envelope fields
//   - StreamDisconnectedError: SSE/WS stream ended before turn.completed; carries
//                              lastSequence so the IDE recovery layer can resume
//   - YanshiSdkError        : base class for `catch (e instanceof YanshiSdkError)`

export class YanshiSdkError extends Error {
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = "YanshiSdkError";
  }
}

export class ApiVersionError extends YanshiSdkError {
  readonly received: string | undefined;
  readonly supported: readonly string[];

  constructor(received: string | undefined, supported: readonly string[]) {
    super(`unsupported Yanshi API version: received=${received ?? "missing"}, supported=${supported.join(",")}`);
    this.name = "ApiVersionError";
    this.received = received;
    this.supported = supported;
  }
}

export class HttpError extends YanshiSdkError {
  readonly status: number;
  readonly body: unknown;

  constructor(status: number, body: unknown) {
    const message = typeof body === "object" && body !== null && "message" in body
      ? String((body as { message?: unknown }).message)
      : `HTTP ${status}`;
    super(message);
    this.name = "HttpError";
    this.status = status;
    this.body = body;
  }
}

export class ProtocolError extends YanshiSdkError {
  constructor(message: string, options?: { cause?: unknown }) {
    super(message, options);
    this.name = "ProtocolError";
  }
}

export class StreamDisconnectedError extends YanshiSdkError {
  readonly lastSequence: number | undefined;

  constructor(lastSequence: number | undefined, options?: { cause?: unknown }) {
    super(`Yanshi item stream disconnected at sequence ${lastSequence ?? "<none>"}`, options);
    this.name = "StreamDisconnectedError";
    this.lastSequence = lastSequence;
  }
}
