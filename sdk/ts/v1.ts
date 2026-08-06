// Yanshi Agent API v1 — hand-maintained TypeScript mirror of the wire contract.
//
// NOT generated, despite what this header said for a long time. cmd/api-schema
// claimed to emit this file and in fact carried a hardcoded copy of it as a Go
// string literal; running the "generator" and diffing produced IDENTICAL
// because both sides were the same transcription. That half of the command has
// been deleted — sdk/python's models have always said "hand-mirrored", and now
// this file says the same thing.
//
// What keeps it honest is internal/api/v1/parity_test.go: it compares the
// field sets of the Go structs, the JSON Schema, this file and the Python
// models, and requires every difference to be listed with a reason. Edit this
// file by hand when the contract changes; the parity test will tell you if you
// missed a source.

export type AgentApiVersion = "v1";

export type ItemType =
  | "turn.started"
  | "message.delta"
  | "reasoning.delta"
  | "tool.call"
  | "tool.result"
  | "tool.progress"
  | "structured.result"
  | "turn.error"
  | "turn.completed";

export interface Thread {
  version: AgentApiVersion;
  id: string;
  status: string;
  title?: string;
  createdAt: number;
  updatedAt: number;
  model?: string;
  thinking?: string;
  turns?: Turn[];
}

export interface Turn {
  version: AgentApiVersion;
  id: string;
  threadId: string;
  status: string;
  input: string;
  startedAt: number;
  completedAt?: number;
}

export interface Item {
  version: AgentApiVersion;
  id: string;
  sequence: number;
  threadId: string;
  turnId: string;
  type: ItemType | `event.${string}`;
  text?: string;
  toolName?: string;
  toolArgs?: string;
  status?: string;
  error?: string;
  structuredResult?: unknown;
}

export interface ThreadStartParams {
  version?: AgentApiVersion;
  title?: string;
  model?: string;
  thinking?: string;
}

export interface ThreadResumeParams {
  version?: AgentApiVersion;
  threadId: string;
}

export interface ThreadInterruptParams {
  version?: AgentApiVersion;
  threadId: string;
  turnId?: string;
}

export interface TurnStartParams {
  version?: AgentApiVersion;
  threadId: string;
  input: string;
  model?: string;
  thinking?: string;
  outputSchema?: unknown;
}

export interface ThreadStartResponse {
  version: AgentApiVersion;
  thread: Thread;
}

export interface ThreadResumeResponse {
  version: AgentApiVersion;
  thread: Thread;
}

export interface TurnStartResponse {
  version: AgentApiVersion;
  turn: Turn;
}

export interface InterruptResponse {
  version: AgentApiVersion;
  ok: boolean;
  threadId: string;
  turnId?: string;
}

export interface ItemUpdatedNotification {
  jsonrpc: "2.0";
  method: "item/updated";
  params: Item;
}

export interface Capabilities {
  version: AgentApiVersion;
  methods: string[];
  itemTypes: string[];
  unknownFields: "ignored";
  stream: "item/updated";
}
