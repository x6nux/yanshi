// Package acp implements an ACP (Agent Client Protocol) client.
package acp

import (
	"encoding/json"
	"fmt"
)

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 envelopes
// ---------------------------------------------------------------------------

// Request is a JSON-RPC request (has an id).
type Request struct {
	JSONRPC string `json:"jsonrpc"` // "2.0"
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// Response is a JSON-RPC response (has an id + result or error).
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the JSON-RPC error object.
type RPCError struct {
	Code    int
	Message string
	Data    any
}

// Error implements the error interface.
func (e *RPCError) Error() string {
	return fmt.Sprintf("acp: rpc error %d: %s", e.Code, e.Message)
}

// Notification is a JSON-RPC notification (no id).
type Notification struct {
	JSONRPC string `json:"jsonrpc"` // "2.0"
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// RawMessage is any inbound line (decide kind by fields).
type RawMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsResponse reports whether the message is a JSON-RPC response
// (has an id and either a result or an error).
func (r RawMessage) IsResponse() bool {
	return r.ID != nil && (r.Result != nil || r.Error != nil)
}

// IsRequest reports whether the message is a JSON-RPC request
// (has an id and a method).
func (r RawMessage) IsRequest() bool {
	return r.ID != nil && r.Method != ""
}

// IsNotification reports whether the message is a JSON-RPC notification
// (no id, but has a method).
func (r RawMessage) IsNotification() bool {
	return r.ID == nil && r.Method != ""
}

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

// InitParams are the params for the "initialize" request.
type InitParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         ClientInfo         `json:"clientInfo"`
}

// ClientCapabilities advertises which optional capabilities the client supports.
type ClientCapabilities struct {
	FS       *FSCap `json:"fs,omitempty"`
	Terminal bool   `json:"terminal,omitempty"`
}

// FSCap describes filesystem read/write capabilities.
type FSCap struct{ ReadTextFile, WriteTextFile bool }

// ClientInfo identifies the client to the agent.
type ClientInfo struct{ Name, Title, Version string }

// InitResult is the result of the "initialize" request.
type InitResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities json.RawMessage   `json:"agentCapabilities"`
	AgentInfo         AgentInfo         `json:"agentInfo"`
	AuthMethods       []json.RawMessage `json:"authMethods"`
}

// AgentInfo identifies the agent to the client.
type AgentInfo struct{ Name, Title, Version string }

// ---------------------------------------------------------------------------
// session/new
// ---------------------------------------------------------------------------

// NewSessionParams are the params for the "session/new" request.
//
// McpServers is a map (per the ACP spec's Record<string, McpServerSchema>) and
// is serialized as `{}` when empty — current adapters (e.g.
// @agentclientprotocol/claude-agent-acp) reject session/new with -32602 if the
// field is absent, so it MUST be present and non-nil.
type NewSessionParams struct {
	Cwd                   string                     `json:"cwd"`
	McpServers            map[string]json.RawMessage `json:"mcpServers"`
	AdditionalDirectories []string                   `json:"additionalDirectories,omitempty"`
}

// NewSessionResult is the result of the "session/new" request.
type NewSessionResult struct {
	SessionID string          `json:"sessionId"`
	Modes     json.RawMessage `json:"modes,omitempty"`
}

// ---------------------------------------------------------------------------
// session/prompt
// ---------------------------------------------------------------------------

// PromptParams are the params for the "session/prompt" request.
type PromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// PromptResult is the result of the "session/prompt" request.
//
// Usage is where ACP carries token accounting. In the v1 schema the only
// `usage: Option<Usage>` field in the whole protocol hangs off PromptResponse
// (agent-client-protocol, agent-client-protocol-schema/src/v1/agent.rs), gated
// behind the `unstable_end_turn_token_usage` feature — so agents that predate
// it simply omit the field and Usage stays nil. It is NOT a session/update:
// the "usage_update" discriminator that does exist reports context-window
// occupancy ({used, size, cost}), not tokens spent.
type PromptResult struct {
	StopReason string `json:"stopReason"`
	Usage      *Usage `json:"usage,omitempty"`
}

// ContentBlock is a single content block in a prompt or update.
type ContentBlock struct {
	Type string `json:"type"` // "text"
	Text string `json:"text,omitempty"`
}

// ---------------------------------------------------------------------------
// session/update notification
// ---------------------------------------------------------------------------

// UpdateParams are the params for a "session/update" notification.
type UpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    Update `json:"update"`
}

// Update is the inner "update" payload of a session/update notification.
type Update struct {
	SessionUpdate string `json:"sessionUpdate"` // discriminator

	// one-of fields (only relevant ones populated)
	Content    []ContentBlock `json:"content,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	Title      string         `json:"title,omitempty"`
	Kind       string         `json:"kind,omitempty"`
	Status     string         `json:"status,omitempty"`
}

// Diff content block (appears inside tool_call content arrays).
type Diff struct {
	Type    string  `json:"type"` // "diff"
	Path    string  `json:"path"`
	OldText *string `json:"oldText"`
	NewText string  `json:"newText"`
}

// Usage carries the ACP token accounting reported on a session/prompt result.
// Fields are best-effort: adapters vary (codex vs claudecode vs future), so any
// subset may be populated; callers must tolerate zero values.
type Usage struct {
	InputTokens  int `json:"inputTokens,omitempty"`
	OutputTokens int `json:"outputTokens,omitempty"`
	TotalTokens  int `json:"totalTokens,omitempty"`
}

// ---------------------------------------------------------------------------
// session/cancel notification
// ---------------------------------------------------------------------------

// CancelParams are the params for the "session/cancel" notification.
type CancelParams struct {
	SessionID string `json:"sessionId"`
}

// ---------------------------------------------------------------------------
// server -> client requests
// ---------------------------------------------------------------------------

// RequestPermissionParams are the params for "session/request_permission".
type RequestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  ToolCallRef        `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// ToolCallRef references a tool call for a permission request.
type ToolCallRef struct {
	ToolCallID string `json:"toolCallId"`
}

// PermissionOption is one selectable option in a permission request.
type PermissionOption struct {
	OptionID, Name string
	Kind           string
}

// PermissionOutcome is the result of a permission request.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"` // "selected" | "cancelled"
	OptionID string `json:"optionId,omitempty"`
}

// FSReadParams are the params for "fs/read_text_file".
type FSReadParams struct {
	SessionID, Path string
	Line, Limit     *int
}

// FSReadResult is the result of "fs/read_text_file".
type FSReadResult struct {
	Content string `json:"content"`
}

// FSWriteParams are the params for "fs/write_text_file".
type FSWriteParams struct{ SessionID, Path, Content string }
