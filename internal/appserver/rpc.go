// Package appserver implements the yanshi app-server: a JSON-RPC 2.0 stdio
// transport that exposes the v1 Agent API to local supervisor processes (an
// IDE extension, a CLI wrapper, a notebook runtime). It shares the same
// *v1.Service as the HTTP/SSE transport so thread/turn/item semantics cannot
// drift between transports.
package appserver

import (
	"encoding/json"
	"fmt"
)

// Standard JSON-RPC 2.0 error codes (per the spec). The dispatcher maps every
// error path to one of these so clients can branch on a small, stable set.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// RPCRequest is the inbound JSON-RPC request envelope. ID is a RawMessage so
// the server can echo string, number, or null ids byte-identical. A missing ID
// marks the message as a notification (no response).
type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCResponse is the outbound response envelope. Exactly one of Result or
// Error is set per the spec; the dispatcher enforces this.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCNotification is the outbound notification envelope (no ID, no response).
// Used to stream item/updated events for an active turn.
type RPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// RPCError is the JSON-RPC error object. Error() returns Message so wrapping
// the error preserves the human-readable text (Data carries the diagnostic
// detail when needed).
type RPCError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error returns the error message text for the JSON-RPC error.
func (e *RPCError) Error() string { return e.Message }

// parseRPCLine decodes one inbound JSON line into an RPCRequest and validates
// the spec-required fields (jsonrpc=="2.0", non-empty method). Returns a
// spec-shaped *RPCError on failure so Serve can write a -32700/-32600 response.
func parseRPCLine(line []byte) (RPCRequest, error) {
	var req RPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return RPCRequest{}, &RPCError{Code: codeParseError, Message: "parse error", Data: err.Error()}
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return req, &RPCError{Code: codeInvalidRequest, Message: "invalid request"}
	}
	if len(req.Params) == 0 {
		req.Params = json.RawMessage(`{}`)
	}
	return req, nil
}

// rpcResponse builds a success response with a non-null ID. A missing ID
// (notification) is replaced with null per the JSON-RPC spec.
func rpcResponse(id json.RawMessage, result any) RPCResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return RPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

// rpcErrorResponse builds an error response with the given code/message/data.
func rpcErrorResponse(id json.RawMessage, code int64, message string, data any) RPCResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return RPCResponse{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message, Data: data}}
}

// decodeParams unmarshals request params into dst, treating empty/null params
// as {} so methods with optional params (e.g. thread/start) work without an
// explicit empty object. Unknown fields are ignored (per the v1 contract).
func decodeParams(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	return nil
}
