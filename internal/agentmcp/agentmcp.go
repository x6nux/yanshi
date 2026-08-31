// Package agentmcp exposes yanshi's own orchestrator as a general-purpose MCP
// server over stdio (W-F-06): another agent — claudecode, codex, anything
// that speaks MCP — adds `yanshi mcp` as a tool server and drives yanshi as a
// sub-agent.
//
// Relationship to the rest of the tree:
//
//   - internal/vcs/mcp is the narrow sibling: 5 read-mostly VCS tools scoped
//     to one worktree. This server is the general surface — session-ful
//     orchestrator turns — which is exactly what "not just the 5 VCS tools"
//     asks for.
//   - internal/acpserver serves the same v1 service but speaks ACP for hosts
//     like Zed. Different protocol, same engine; a turn run here is the turn
//     HTTP and app-server run.
//   - The framing loop is deliberately this package's own (newline-delimited
//     JSON-RPC, one object per line) rather than a shared server kit:
//     acpserver, appserver and vcs/mcp each own their loop for the same
//     reason — the loop is small, and the interesting variance (method
//     vocabulary, one-response-vs-streaming) lives one layer up where a
//     shared abstraction would only be in the way.
//
// Sessions: agent_prompt returns the v1 thread id as session_id; passing it
// back continues the same conversation (Resume), so an external agent can
// hold a multi-turn relationship with yanshi instead of one-shot calls.
package agentmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/x6nux/yanshi/internal/api/v1"
	"github.com/x6nux/yanshi/internal/mcp"
)

// Server drives the shared v1 thread/turn/item service over MCP on stdio.
type Server struct {
	api *v1.Service
}

// New builds a Server on the given v1 service (the composition root passes
// the same App.AgentAPI the HTTP transport serves).
func New(api *v1.Service) *Server {
	return &Server{api: api}
}

// request is a JSON-RPC request. id is absent (or null) for notifications.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether req carries no usable id (absent or null —
// the null form exists for robustness across client implementations).
func isNotification(req *request) bool {
	id := strings.TrimSpace(string(req.ID))
	return id == "" || id == "null"
}

// RPCError is a JSON-RPC error object. Codes follow JSON-RPC/MCP convention:
// -32601 method not found, -32602 invalid params, -32000 server-defined.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// response is a JSON-RPC response; id is always echoed from the request. A
// success carries result and no error; a failure carries error and no result.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Serve reads newline-delimited JSON-RPC requests from r until EOF, writing
// one response line per request to w. stdout carries nothing but protocol
// frames; diagnostics belong on stderr (the host parses stdout line by
// line). Malformed JSON is skipped — there is no recoverable id to answer.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if isNotification(&req) {
			// Notifications get no response; notifications/initialized is the
			// only one the protocol sends us and it needs no action.
			continue
		}
		result, rpcErr := s.handleMethod(ctx, req.Method, req.Params)
		if err := writeResponse(w, req.ID, result, rpcErr); err != nil {
			return err
		}
	}
	return sc.Err()
}

// handleMethod routes one request. Unknown methods yield -32601.
func (s *Server) handleMethod(ctx context.Context, method string, params json.RawMessage) (any, *RPCError) {
	switch method {
	case "initialize":
		return s.initialize(params)
	case "notifications/initialized":
		return nil, nil
	case "tools/list":
		return map[string]any{"tools": toolDescriptors()}, nil
	case "tools/call":
		return s.handleToolsCall(ctx, params)
	case "ping":
		return map[string]any{}, nil
	default:
		return nil, &RPCError{Code: -32601, Message: "method not found: " + method}
	}
}

// initialize answers the MCP handshake. The requested protocol revision is
// echoed when this server speaks it, else the newest one — negotiation is
// shared with the client half (mcp.AnswerProtocolVersion), so both ends of a
// yanshi-to-yanshi connection agree by construction.
func (s *Server) initialize(params json.RawMessage) (any, *RPCError) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	return map[string]any{
		"protocolVersion": mcp.AnswerProtocolVersion(p.ProtocolVersion),
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "yanshi-agent", "version": "0.1"},
	}, nil
}

// toolDescriptor is the MCP tool descriptor as served here.
type toolDescriptor struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	InputSchema toolSchema `json:"inputSchema"`
}

// toolSchema is a minimal JSON-Schema object.
type toolSchema struct {
	Type       string          `json:"type"`
	Properties map[string]prop `json:"properties"`
	Required   []string        `json:"required,omitempty"`
}

// prop is one property in a toolSchema.
type prop struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// toolDescriptors advertises the two tools an external sub-agent drives
// yanshi with.
func toolDescriptors() []toolDescriptor {
	return []toolDescriptor{
		{
			Name:        "agent_prompt",
			Description: "Send a prompt to yanshi's agent and wait for the finished turn. Pass the session_id from a previous answer to continue that conversation.",
			InputSchema: toolSchema{
				Type: "object",
				Properties: map[string]prop{
					"prompt":     {Type: "string", Description: "the instruction for yanshi"},
					"session_id": {Type: "string", Description: "omit to start a new session; pass a previous session_id to continue it"},
				},
				Required: []string{"prompt"},
			},
		},
		{
			Name:        "agent_interrupt",
			Description: "Interrupt yanshi's currently running turn in a session. Idempotent when the session has no active turn.",
			InputSchema: toolSchema{
				Type: "object",
				Properties: map[string]prop{
					"session_id": {Type: "string", Description: "the session whose active turn should be cancelled"},
				},
				Required: []string{"session_id"},
			},
		},
	}
}

// toolsCallParams are the MCP tools/call params.
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// handleToolsCall dispatches a tools/call. Unknown tools yield -32602.
func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var p toolsCallParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &RPCError{Code: -32602, Message: "invalid tools/call params: " + err.Error()}
		}
	}
	switch p.Name {
	case "agent_prompt":
		return s.callPrompt(ctx, p.Arguments)
	case "agent_interrupt":
		return s.callInterrupt(p.Arguments)
	default:
		return nil, &RPCError{Code: -32602, Message: "unknown tool: " + p.Name}
	}
}

// promptArgs are agent_prompt's arguments.
type promptArgs struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id"`
}

// promptResult is the tool result: the session to continue, the turn that
// ran, how it ended, and the assistant's answer.
type promptResult struct {
	SessionID string `json:"session_id"`
	TurnID    string `json:"turn_id"`
	Status    string `json:"status"`
	Text      string `json:"text"`
}

// callPrompt runs one orchestrator turn to completion. A new session is
// minted when session_id is empty; a given one is resumed, so the turn sees
// the conversation so far.
func (s *Server) callPrompt(ctx context.Context, args json.RawMessage) (any, *RPCError) {
	var a promptArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, &RPCError{Code: -32602, Message: "invalid agent_prompt arguments: " + err.Error()}
		}
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return nil, &RPCError{Code: -32602, Message: "prompt is required"}
	}

	threadID := strings.TrimSpace(a.SessionID)
	if threadID == "" {
		th, err := s.api.Start(ctx, v1.ThreadStartParams{Title: "MCP agent session"})
		if err != nil {
			return nil, &RPCError{Code: -32000, Message: "start session: " + err.Error()}
		}
		threadID = th.ID
	} else if _, err := s.api.Resume(ctx, v1.ThreadResumeParams{ThreadID: threadID}); err != nil {
		return nil, &RPCError{Code: -32000, Message: fmt.Sprintf("unknown session %q (sessions die with the server unless a store is configured)", threadID)}
	}

	resp, items, err := s.api.StartTurn(ctx, v1.TurnStartParams{ThreadID: threadID, Input: a.Prompt})
	if err != nil {
		return nil, &RPCError{Code: -32000, Message: "start turn: " + err.Error()}
	}
	var text strings.Builder
	status := ""
	for it := range items {
		switch it.Type {
		case v1.ItemMessageDelta:
			text.WriteString(it.Text)
		case v1.ItemTurnError:
			// A failed/interrupted turn is a valid tool RESULT (the caller
			// can read what happened and retry); it is not a transport error.
			status = it.Status
		}
	}
	if status == "" {
		status = v1.TurnStatusCompleted
	}
	return textResult(promptResult{
		SessionID: threadID,
		TurnID:    resp.Turn.ID,
		Status:    status,
		Text:      text.String(),
	}), nil
}

// interruptArgs are agent_interrupt's arguments.
type interruptArgs struct {
	SessionID string `json:"session_id"`
}

// callInterrupt cancels a session's active turn. Idempotent: a session with
// no active turn is answered ok, mirroring v1's Interrupt contract.
func (s *Server) callInterrupt(args json.RawMessage) (any, *RPCError) {
	var a interruptArgs
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, &RPCError{Code: -32602, Message: "invalid agent_interrupt arguments: " + err.Error()}
		}
	}
	if strings.TrimSpace(a.SessionID) == "" {
		return nil, &RPCError{Code: -32602, Message: "session_id is required"}
	}
	if err := s.api.Interrupt(context.Background(), v1.ThreadInterruptParams{ThreadID: a.SessionID}); err != nil {
		return nil, &RPCError{Code: -32000, Message: "interrupt: " + err.Error()}
	}
	return textResult(map[string]any{"session_id": a.SessionID, "interrupted": true}), nil
}

// textResult wraps a value as a single text-block tools/call result (the
// value is JSON-encoded into the text block).
func textResult(v any) map[string]any {
	data, err := json.Marshal(v)
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("marshal result: %v", err)}},
			"isError": true,
		}
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(data)}}}
}

// writeResponse marshals resp as a single JSON line. A request answered with
// neither result nor error is answered with an empty result so its id is not
// dropped (a host waiting on that id would otherwise hang).
func writeResponse(w io.Writer, id json.RawMessage, result any, rpcErr *RPCError) error {
	resp := response{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr}
	if rpcErr == nil && result == nil {
		resp.Result = map[string]any{}
	}
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("agentmcp: marshal response: %w", err)
	}
	_, err = w.Write(append(data, '\n'))
	return err
}
