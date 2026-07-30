// Package mcp implements a minimal MCP (Model Context Protocol) server over
// newline-delimited JSON-RPC 2.0 on stdio. It exposes yanshi's vcs_* tools,
// scoped to a single ACP session's worktree, so an external agent client
// (claudecode/codex) can drive version-control operations through MCP.
//
// The wire framing mirrors internal/acp/transport.go: one JSON object per line.
// Requests carry an `id` and receive a response; notifications have no `id` and
// receive none. Only the MCP methods needed for tool discovery and invocation
// are handled: initialize, notifications/initialized, tools/list, tools/call.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/x6nux/yanshi/internal/vcs"
)

// protocolVersion is the MCP protocol version advertised on initialize.
const protocolVersion = "2025-06-18"

// Server is a minimal MCP server exposing vcs_* tools, scoped to a worktree.
// All tool calls operate on worktreeID (commit/log/diff/restore); vcs_merge
// merges that worktree into main.
type Server struct {
	vcs        *vcs.VCS
	repoID     string
	worktreeID string
	agent      string
}

// New builds a Server scoped to the given worktree (the ACP session's worktree).
// agent is the author attribution stamped onto commits and merges.
func New(v *vcs.VCS, repoID, worktreeID, agent string) *Server {
	return &Server{vcs: v, repoID: repoID, worktreeID: worktreeID, agent: agent}
}

// Serve reads newline-delimited JSON-RPC requests from r until EOF, writing one
// response line per request to w. Notifications (no id) are dispatched but get
// no response. A scan error (e.g. a line exceeding the buffer) is returned; a
// clean EOF returns nil. ctx cancellation between lines aborts the loop.
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
		if err := s.handleLine(ctx, line, w); err != nil {
			return err
		}
	}
	return sc.Err()
}

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 envelopes
// ---------------------------------------------------------------------------

// request is a JSON-RPC request. id is nil/absent for notifications.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // nil/absent => notification
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// response is a JSON-RPC response. id is always echoed back from the request.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *respError      `json:"error,omitempty"`
}

// respError is the JSON-RPC error object.
type respError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// contentBlock is a single MCP content block (tools/call results are a list of
// these). Only the "text" type is used by this server.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// callResult is the standard MCP tools/call result: a list of content blocks.
type callResult struct {
	Content []contentBlock `json:"content"`
}

// isNotification reports whether the request is a JSON-RPC notification
// (no id member, or an explicit null id — both are treated as notification
// for robustness across MCP client implementations).
func (r request) isNotification() bool {
	id := strings.TrimSpace(string(r.ID))
	return id == "" || id == "null"
}

// handleLine decodes one JSON-RPC line, dispatches it, and writes a response
// unless the line is a notification. Malformed JSON is skipped silently (there
// is no recoverable id to echo back), mirroring internal/acp's behavior.
func (s *Server) handleLine(ctx context.Context, line []byte, w io.Writer) error {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return nil
	}
	notif := req.isNotification()
	result, rpcErr := s.handleMethod(ctx, req.Method, req.Params)
	if notif {
		return nil
	}
	return writeResponse(w, req.ID, result, rpcErr)
}

// handleMethod routes a method to its handler, returning either a result or a
// JSON-RPC error. Unknown methods yield -32601.
func (s *Server) handleMethod(_ context.Context, method string, params json.RawMessage) (any, *respError) {
	switch method {
	case "initialize":
		return s.handleInitialize(), nil
	case "notifications/initialized":
		// Acknowledged notification — nothing to do.
		return nil, nil
	case "tools/list":
		return map[string]any{"tools": toolDescriptors()}, nil
	case "tools/call":
		return s.handleToolsCall(params)
	default:
		return nil, &respError{Code: -32601, Message: "method not found: " + method}
	}
}

// handleInitialize returns the MCP initialize result: protocol version, tool
// capability, and server identity.
func (s *Server) handleInitialize() any {
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "yanshi-vcs",
			"version": "0.1",
		},
	}
}

// ---------------------------------------------------------------------------
// tools/list
// ---------------------------------------------------------------------------

// toolDescriptor is the MCP tool descriptor: name, human description, and a
// JSON-Schema-typed inputSchema.
type toolDescriptor struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	InputSchema toolSchema `json:"inputSchema"`
}

// toolSchema is a minimal JSON-Schema object describing a tool's arguments.
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

// toolDescriptors returns the five vcs_* tool descriptors advertised by this
// server. Each operates on the session worktree; vcs_merge targets main.
func toolDescriptors() []toolDescriptor {
	return []toolDescriptor{
		{
			Name:        "vcs_commit",
			Description: "Snapshot the session worktree's pending edits as a commit (author = the session agent).",
			InputSchema: toolSchema{
				Type: "object",
				Properties: map[string]prop{
					"message": {Type: "string", Description: "commit message"},
				},
				Required: []string{"message"},
			},
		},
		{
			Name:        "vcs_log",
			Description: "List commits on the session worktree (newest-first).",
			InputSchema: toolSchema{
				Type: "object",
				Properties: map[string]prop{
					"limit": {Type: "integer", Description: "max entries (default 20)"},
				},
			},
		},
		{
			Name:        "vcs_diff",
			Description: "Show file-level changes between two commits in the session repo. Empty refs resolve to the worktree tip.",
			InputSchema: toolSchema{
				Type: "object",
				Properties: map[string]prop{
					"ref_a": {Type: "string", Description: "from commit id (default: worktree tip)"},
					"ref_b": {Type: "string", Description: "to commit id (default: worktree tip)"},
				},
			},
		},
		{
			Name:        "vcs_restore",
			Description: "Restore a file from a commit into the session worktree's working copy.",
			InputSchema: toolSchema{
				Type: "object",
				Properties: map[string]prop{
					"ref":  {Type: "string", Description: "commit id"},
					"path": {Type: "string", Description: "repo-relative file path"},
				},
				Required: []string{"ref", "path"},
			},
		},
		{
			Name:        "vcs_merge",
			Description: "Merge the session worktree into main (tree-level 3-way). Returns conflicts; force lets the worktree version win.",
			InputSchema: toolSchema{
				Type: "object",
				Properties: map[string]prop{
					"force": {Type: "boolean", Description: "on conflict, let the worktree version win"},
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// tools/call dispatch
// ---------------------------------------------------------------------------

// toolsCallParams are the MCP tools/call params.
type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// handleToolsCall dispatches a tools/call to the named vcs_* method. Unknown
// tool names yield -32602 (invalid params, mirroring MCP's "unknown tool").
func (s *Server) handleToolsCall(params json.RawMessage) (any, *respError) {
	var p toolsCallParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &respError{Code: -32602, Message: "invalid tools/call params: " + err.Error()}
		}
	}
	switch p.Name {
	case "vcs_commit":
		return s.callCommit(p.Arguments)
	case "vcs_log":
		return s.callLog(p.Arguments)
	case "vcs_diff":
		return s.callDiff(p.Arguments)
	case "vcs_restore":
		return s.callRestore(p.Arguments)
	case "vcs_merge":
		return s.callMerge(p.Arguments)
	default:
		return nil, &respError{Code: -32602, Message: "unknown tool: " + p.Name}
	}
}

// textResult wraps a string as a single-block MCP tools/call result.
func textResult(text string) callResult {
	return callResult{Content: []contentBlock{{Type: "text", Text: text}}}
}

// marshalText marshals v as JSON and returns it as a text content result. A
// marshal failure is reported as an internal-error JSON-RPC error.
func marshalText(v any) (any, *respError) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, &respError{Code: -32603, Message: "marshal result: " + err.Error()}
	}
	return textResult(string(data)), nil
}

// decodeArgs unmarshals raw into target. An empty raw yields no error (the
// target stays zero-valued — all-optional args). A non-empty but malformed raw
// yields -32602.
func decodeArgs(raw json.RawMessage, target any) *respError {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return &respError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	return nil
}

// worktreeTip returns the latest commit id on the session worktree ("" if none).
func (s *Server) worktreeTip() string {
	commits, err := s.vcs.LogWorktree(s.worktreeID, 1)
	if err != nil || len(commits) == 0 {
		return ""
	}
	return commits[0].ID
}

// --- vcs_commit ---

type commitArgs struct {
	Message string `json:"message"`
}

func (s *Server) callCommit(args json.RawMessage) (any, *respError) {
	var a commitArgs
	if e := decodeArgs(args, &a); e != nil {
		return nil, e
	}
	if a.Message == "" {
		return nil, &respError{Code: -32602, Message: "message is required"}
	}
	id, err := s.vcs.CommitWorktree(s.worktreeID, s.agent, a.Message)
	if err != nil {
		return nil, serverErr(err)
	}
	return textResult(id), nil
}

// --- vcs_log ---

type logArgs struct {
	Limit int `json:"limit"`
}

func (s *Server) callLog(args json.RawMessage) (any, *respError) {
	var a logArgs
	if e := decodeArgs(args, &a); e != nil {
		return nil, e
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 20
	}
	commits, err := s.vcs.LogWorktree(s.worktreeID, limit)
	if err != nil {
		return nil, serverErr(err)
	}
	return marshalText(commits)
}

// --- vcs_diff ---

type diffArgs struct {
	RefA string `json:"ref_a"`
	RefB string `json:"ref_b"`
}

func (s *Server) callDiff(args json.RawMessage) (any, *respError) {
	var a diffArgs
	if e := decodeArgs(args, &a); e != nil {
		return nil, e
	}
	// Empty refs resolve to the worktree tip (per task spec: resolve empty refs
	// via LogWorktree(...,1)). If both are empty the diff is tip-vs-tip (empty),
	// which is the correct "no changes" answer for a default invocation.
	tip := s.worktreeTip()
	refA, refB := a.RefA, a.RefB
	if refA == "" {
		refA = tip
	}
	if refB == "" {
		refB = tip
	}
	diffs, err := s.vcs.Diff(s.repoID, refA, refB)
	if err != nil {
		return nil, serverErr(err)
	}
	return marshalText(diffs)
}

// --- vcs_restore ---

type restoreArgs struct {
	Ref  string `json:"ref"`
	Path string `json:"path"`
}

func (s *Server) callRestore(args json.RawMessage) (any, *respError) {
	var a restoreArgs
	if e := decodeArgs(args, &a); e != nil {
		return nil, e
	}
	if a.Ref == "" || a.Path == "" {
		return nil, &respError{Code: -32602, Message: "ref and path are required"}
	}
	destDir, err := s.vcs.WorktreePath(s.worktreeID)
	if err != nil {
		return nil, serverErr(err)
	}
	if err := s.vcs.Restore(a.Ref, a.Path, destDir); err != nil {
		return nil, serverErr(err)
	}
	return textResult("restored " + a.Path), nil
}

// --- vcs_merge ---

type mergeArgs struct {
	Force bool `json:"force"`
}

func (s *Server) callMerge(args json.RawMessage) (any, *respError) {
	var a mergeArgs
	if e := decodeArgs(args, &a); e != nil {
		return nil, e
	}
	merged, conflicts, mErr := s.vcs.MergeToMain(s.worktreeID, s.agent, a.Force)
	// ErrConflicts is a normal result: the tool ran and found conflicts. Only
	// surface a JSON-RPC error for genuine failures.
	if mErr != nil && !errors.Is(mErr, vcs.ErrConflicts) {
		return nil, serverErr(mErr)
	}
	if conflicts == nil {
		conflicts = []string{}
	}
	return marshalText(map[string]any{
		"merged":    merged,
		"conflicts": conflicts,
	})
}

// serverErr wraps a VCS-level error as a JSON-RPC server error (-32000).
func serverErr(err error) *respError {
	return &respError{Code: -32000, Message: err.Error()}
}

// writeResponse marshals resp as a single JSON line and writes it to w.
func writeResponse(w io.Writer, id json.RawMessage, result any, rpcErr *respError) error {
	data, err := json.Marshal(response{JSONRPC: "2.0", ID: id, Result: result, Error: rpcErr})
	if err != nil {
		return fmt.Errorf("mcp: marshal response: %w", err)
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}
