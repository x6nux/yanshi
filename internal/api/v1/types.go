// Package v1 implements the Yanshi Agent API v1 resource layer: thread, turn,
// and item resources consumed by both the HTTP/SSE transport and the JSON-RPC
// app-server. The package is the single source of truth for the v1 wire shapes
// (camelCase JSON tags, provisional version envelope, stable item types); the
// legacy proto.ClientFrame/ServerFrame vocabulary is mapped into these types
// by events.go and is never re-encoded by callers.
package v1

import (
	"encoding/json"

	"github.com/x6nux/yanshi/internal/proto"
)

// Version is the provisional resource contract version. The string "v1" is the
// only accepted value for the `version` field on every v1 resource. HTTP
// additionally advertises it via the X-Yanshi-API-Version header; JSON-RPC
// carries it inside the result/envelope (jsonrpc:"2.0" is the transport
// version and does NOT replace this resource version). D1-DEC-1 chose option A
// (per-resource version field + HTTP header).
const Version = "v1"

// Resource status constants. Stable string values are part of the wire
// contract; renaming any of them is a breaking change.
const (
	ThreadStatusActive    = "active"
	ThreadStatusArchived  = "archived"
	TurnStatusInProgress  = "inProgress"
	TurnStatusCompleted   = "completed"
	TurnStatusInterrupted = "interrupted"
	TurnStatusFailed      = "failed"

	// Item type constants. Every v1 stream item carries one of these in Item.Type
	// (or an "event.<legacyType>" fallback for frames the v1 mapping does not
	// know — unknown types must never be silently dropped, per the contract).
	ItemMessageDelta     = "message.delta"
	ItemReasoningDelta   = "reasoning.delta"
	ItemToolCall         = "tool.call"
	ItemToolResult       = "tool.result"
	ItemToolProgress     = "tool.progress"
	ItemStructuredResult = "structured.result"
	ItemTurnStarted      = "turn.started"
	ItemTurnError        = "turn.error"
	ItemTurnCompleted    = "turn.completed"
)

// Thread is the persistent conversation resource. v1 reuses the existing
// SQLite sessions row as its backing store: the session id IS the thread id.
// Title and Model mirror the session row; Turns carries the lightweight turn
// history when a snapshot includes it (otherwise it is omitted).
type Thread struct {
	Version   string `json:"version"`
	ID        string `json:"id"`
	Status    string `json:"status"`
	Title     string `json:"title,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	Model     string `json:"model,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Turns     []Turn `json:"turns,omitempty"`
}

// Turn is one user input's lifecycle. A thread allows at most one active turn
// at a time; CompletedAt is zero (omitted) until the turn reaches a terminal
// status (completed/interrupted/failed).
type Turn struct {
	Version     string `json:"version"`
	ID          string `json:"id"`
	ThreadID    string `json:"threadId"`
	Status      string `json:"status"`
	Input       string `json:"input"`
	StartedAt   int64  `json:"startedAt"`
	CompletedAt int64  `json:"completedAt,omitempty"`
}

// Item is the minimum streamable event. Every Item in a thread has a monotonic
// Sequence starting at 1; clients consume the stream in order. Unknown fields
// are ignored on decode; unknown Item.Type values must be ignored by clients
// but preserved by servers (as event.<legacyType>).
type Item struct {
	Version          string          `json:"version"`
	ID               string          `json:"id"`
	Sequence         int64           `json:"sequence"`
	ThreadID         string          `json:"threadId"`
	TurnID           string          `json:"turnId"`
	Type             string          `json:"type"`
	Text             string          `json:"text,omitempty"`
	ToolName         string          `json:"toolName,omitempty"`
	ToolArgs         string          `json:"toolArgs,omitempty"`
	Status           string          `json:"status,omitempty"`
	Error            string          `json:"error,omitempty"`
	StructuredResult json.RawMessage `json:"structuredResult,omitempty"`
}

// ThreadSnapshot is the resume payload: the current Thread.
//
// It used to carry historical Items too, and Service.snapshot never set them —
// both transports forwarded a field that was always absent. Filling it would
// have meant minting a turnId, a per-thread sequence and a streaming-vocabulary
// type for each stored MESSAGE, none of which exist for a message read back
// from the store. v1 does not promise cross-process event replay (see
// Capabilities); the contract now says so by omission rather than by a field
// that never arrives.
type ThreadSnapshot struct {
	Version string `json:"version"`
	Thread  Thread `json:"thread"`
}

// ThreadStartParams are the inputs to thread/start. All fields are optional;
// the service fills defaults (title="New thread", status="active", etc.).
type ThreadStartParams struct {
	Version  string `json:"version,omitempty"`
	Title    string `json:"title,omitempty"`
	Model    string `json:"model,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

// ThreadResumeParams load a thread by id. Required: ThreadID.
type ThreadResumeParams struct {
	Version  string `json:"version,omitempty"`
	ThreadID string `json:"threadId"`
}

// ThreadInterruptParams cancel a thread's active turn. TurnID is optional;
// when set, the service refuses to cancel a different turn (defense against
// stale client state). Repeated interrupt calls are idempotent.
type ThreadInterruptParams struct {
	Version  string `json:"version,omitempty"`
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
}

// TurnStartParams begin a turn on a thread. Required: ThreadID, Input.
// OutputSchema is an optional JSON Schema for structured turns (mirrors the
// legacy ClientFrame.OutputSchema feature).
type TurnStartParams struct {
	Version      string          `json:"version,omitempty"`
	ThreadID     string          `json:"threadId"`
	Input        string          `json:"input"`
	Model        string          `json:"model,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	// Images carries optional image attachments (Tier G, ADDITIVE).
	Images []proto.ImageAttach `json:"images,omitempty"`
}

// ThreadStartResponse is the thread/start result. Carries the freshly-created
// Thread resource.
type ThreadStartResponse struct {
	Version string `json:"version"`
	Thread  Thread `json:"thread"`
}

// ThreadResumeResponse is the thread/resume result. Carries the Thread; see
// ThreadSnapshot for why item history is not part of it.
type ThreadResumeResponse struct {
	Version string `json:"version"`
	Thread  Thread `json:"thread"`
}

// TurnStartResponse is the turn/start result. Carries the freshly-started Turn.
// Items produced during the turn are streamed via SSE/JSON-RPC notifications.
type TurnStartResponse struct {
	Version string `json:"version"`
	Turn    Turn   `json:"turn"`
}

// InterruptResponse is the thread/interrupt result. OK=true means the interrupt
// was accepted (or the thread had no active turn to interrupt — idempotent).
type InterruptResponse struct {
	Version  string `json:"version"`
	OK       bool   `json:"ok"`
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
}

// Capabilities describes the v1 surface to a client. It is returned by the
// `capabilities` method and at the top of the schema document so clients can
// introspect supported methods/item types without out-of-band docs. The
// UnknownFields policy is always "ignored" in v1 (per the wire contract).
type Capabilities struct {
	Version       string   `json:"version"`
	Methods       []string `json:"methods"`
	ItemTypes     []string `json:"itemTypes"`
	UnknownFields string   `json:"unknownFields"`
	Stream        string   `json:"stream"`
}
