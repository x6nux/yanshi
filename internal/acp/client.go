package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Event represents a single session/update event received during a Prompt turn.
type Event struct {
	Kind       string // "agent_message_chunk" or "tool_call"
	Text       string // populated for agent_message_chunk
	ToolCallID string // populated for tool_call
	Title      string // populated for tool_call
	Kind_      string // populated for tool_call (the Update.Kind discriminator)
	Status     string // populated for tool_call
	// Usage is populated only on the synthetic "usage" event Prompt emits from
	// the session/prompt result; it is nil on every session/update, because no
	// session/update carries token accounting. Callers must nil-check.
	Usage *Usage
}

// Client is an ACP client that communicates with an ACP agent over a Transport.
type Client struct {
	tr             *Transport
	writer         io.Writer // the transport's writer (stdin pipe), closed in Close if possible
	agentInfo      AgentInfo
	initialized    bool
	readLoopCtx    context.Context
	readLoopCancel context.CancelFunc
	readLoopDone   chan struct{}

	// Per-call event handler for the active Prompt. Guarded by eventMu.
	// onNotify runs in the transport's ReadLoop goroutine; Prompt runs Call
	// (blocking for the response) on the caller's goroutine. The mutex makes
	// setting/clearing the handler safe relative to its invocation.
	eventMu        sync.Mutex
	currentOnEvent func(Event)

	// deliverMu serializes handler invocations. Until token usage moved to the
	// prompt RESULT, every delivery came from the single ReadLoop goroutine and
	// was serialized for free; Prompt now delivers the result's usage from the
	// caller's goroutine, which can overlap a notification the ReadLoop is
	// still dispatching. Handlers in this repo append to slices without locking
	// (see the tests, and goalloop's usageForwarder), so the overlap would be a
	// real race rather than a theoretical one.
	//
	// ⚠️ UNPINNED DESIGN CHOICE. Measured 2026-08-05: deleting this mutex
	// produces no DATA RACE under `go test -race ./internal/acp`, because no
	// test arranges a notification still in flight when the prompt response
	// lands. The reasoning above is therefore argued, not evidenced — exactly
	// the shape 0-bis in docs/superpowers/review-checklist.md forbids leaving
	// unlabelled. Closing it needs a test that holds a session/update inside
	// the handler while the response resolves; until then, do not treat the
	// absence of a red test as evidence the mutex is unnecessary.
	deliverMu sync.Mutex

	// Policy gates inbound server->client requests (fs/terminal/permission).
	// May be nil; when nil, inbound requests get a method-not-found error.
	policyMu sync.Mutex
	policy   Policy

	// sessionCwds tracks the working directory for each session, so the
	// fs/read_text_file and fs/write_text_file handlers can resolve relative
	// paths against the session's Cwd.
	sessionMu   sync.Mutex
	sessionCwds map[string]string

	// worktreeID + recordEdit enable auto-tracking of agent file edits into a
	// VCS worktree. When worktreeID is "" or recordEdit is nil, tracking is off.
	// recordEdit is a callback to avoid a hard dep on the vcs package.
	worktreeID string
	recordEdit func(worktreeID, agent, absPath string, content []byte) error
}

// NewClient creates a Client reading from in and writing to out.
// The transport ReadLoop is started lazily by Initialize.
// Close will close out if it implements io.Closer (e.g. an exec.Cmd stdin pipe).
func NewClient(in io.Reader, out io.Writer) *Client {
	return &Client{
		tr:          NewTransport(in, out),
		writer:      out,
		sessionCwds: map[string]string{},
	}
}

// SetPolicy installs the Policy used to gate inbound server->client requests
// (fs/read_text_file, fs/write_text_file, terminal/create, session/request_permission).
// Must be called before Prompt. When no policy is set, inbound requests receive
// a JSON-RPC error response.
func (c *Client) SetPolicy(p Policy) {
	c.policyMu.Lock()
	defer c.policyMu.Unlock()
	c.policy = p
}

// SetVCSTracking configures auto-tracking of file edits (from applyDiffContent)
// into the given VCS worktree via the recorder callback. Either value empty/nil
// disables tracking.
func (c *Client) SetVCSTracking(worktreeID string, recorder func(worktreeID, agent, absPath string, content []byte) error) {
	c.worktreeID = worktreeID
	c.recordEdit = recorder
}

// Initialize sends the "initialize" request and starts the transport ReadLoop.
// The ReadLoop is started exactly once (on the first Initialize call).
func (c *Client) Initialize(ctx context.Context, caps ClientCapabilities) (AgentInfo, error) {
	// Start the ReadLoop once.
	if c.readLoopDone == nil {
		c.readLoopCtx, c.readLoopCancel = context.WithCancel(context.Background())
		c.readLoopDone = make(chan struct{})
		// Install the persistent onNotify + onRequest handlers before ReadLoop starts.
		c.tr.SetHandlers(c.handleNotify, c.handleInboundRequest)
		go func() {
			c.tr.ReadLoop(c.readLoopCtx)
			close(c.readLoopDone)
		}()
	}

	params := InitParams{
		ProtocolVersion:    1,
		ClientCapabilities: caps,
		ClientInfo:         ClientInfo{Name: "yanshi"},
	}
	result, err := c.tr.Call(ctx, "initialize", params)
	if err != nil {
		return AgentInfo{}, err
	}

	var initResult InitResult
	if err := json.Unmarshal(result, &initResult); err != nil {
		return AgentInfo{}, fmt.Errorf("acp: unmarshal init result: %w", err)
	}

	c.agentInfo = initResult.AgentInfo
	c.initialized = true
	return c.agentInfo, nil
}

// NewSession sends a "session/new" request and returns the allocated session ID.
// mcpServers is the map serialized as the session/new "mcpServers" field (name →
// MCP stdio server config); pass nil to send an empty map. Spawn builds this
// from SpawnOptions.MCPCommand via buildMcpServers, so most callers go through
// Spawn rather than calling NewSession directly.
func (c *Client) NewSession(ctx context.Context, cwd string, extraDirs []string, mcpServers map[string]json.RawMessage) (string, error) {
	if mcpServers == nil {
		mcpServers = map[string]json.RawMessage{} // present-as-{} required by current adapters
	}
	params := NewSessionParams{
		Cwd:                   cwd,
		AdditionalDirectories: extraDirs,
		McpServers:            mcpServers,
	}
	result, err := c.tr.Call(ctx, "session/new", params)
	if err != nil {
		return "", err
	}

	var sessionResult NewSessionResult
	if err := json.Unmarshal(result, &sessionResult); err != nil {
		return "", fmt.Errorf("acp: unmarshal session result: %w", err)
	}

	c.sessionMu.Lock()
	c.sessionCwds[sessionResult.SessionID] = cwd
	c.sessionMu.Unlock()

	return sessionResult.SessionID, nil
}

// Prompt sends a "session/prompt" request and streams session/update events
// to onEvent until the agent resolves the prompt. Returns the stopReason
// (e.g. "end_turn", "cancelled").
//
// After the turn resolves, Prompt delivers one synthetic Event{Kind: "usage"}
// carrying the token accounting the agent reported on the prompt RESULT — the
// only place ACP puts it. Agents that omit the optional field produce no such
// event, and neither does a report whose every counter is zero.
//
// Concurrency model: onNotify runs in the transport's ReadLoop goroutine,
// while Call blocks on the caller's goroutine waiting for the response.
// A per-Client mutex guards the currentOnEvent pointer so that the handler
// is safely installed before Call and cleared after — ensuring every update
// that arrives before the response is delivered to onEvent. The usage event is
// produced on the caller's goroutine, so deliverMu serializes it against any
// notification the ReadLoop is still dispatching.
func (c *Client) Prompt(ctx context.Context, sessionID, text string, onEvent func(Event)) (stopReason string, err error) {
	// Install the per-call event handler. The transport's onNotify is already
	// permanently wired to c.handleNotify (set in Initialize); handleNotify
	// checks currentOnEvent under a mutex and forwards Events when non-nil.
	c.eventMu.Lock()
	c.currentOnEvent = onEvent
	c.eventMu.Unlock()
	defer func() {
		c.eventMu.Lock()
		c.currentOnEvent = nil
		c.eventMu.Unlock()
	}()

	params := PromptParams{
		SessionID: sessionID,
		Prompt:    []ContentBlock{{Type: "text", Text: text}},
	}
	result, err := c.tr.Call(ctx, "session/prompt", params)
	if err != nil {
		return "", err
	}

	stopReason, usage, err := decodePromptResult(result)
	if err != nil {
		return "", err
	}
	if usage != nil {
		c.deliver(Event{Kind: "usage", Usage: usage})
	}
	return stopReason, nil
}

// decodePromptResult reads a session/prompt result into its stop reason and the
// turn's token usage, returning a nil usage when there is nothing worth
// accounting for.
//
// Two rules live here, both about not letting the optional field damage the
// required one:
//
//   - A usage that does not parse is dropped, and the turn still reports its
//     stop reason. ACP marks these fields DefaultOnError, so an agent that
//     types one wrong (a string where a number belongs) costs us that turn's
//     accounting — not the turn itself, which by then has already run.
//   - An all-zero usage is dropped too. UsageSink accumulates whatever it is
//     handed, so agents that report {0,0,0} every turn would be indistinguishable
//     from a run that genuinely spent nothing — the one reading that can never
//     trip the budget.
func decodePromptResult(result json.RawMessage) (stopReason string, usage *Usage, err error) {
	var pr PromptResult
	if err := json.Unmarshal(result, &pr); err != nil {
		var bare struct {
			StopReason string `json:"stopReason"`
		}
		if err2 := json.Unmarshal(result, &bare); err2 != nil {
			return "", nil, fmt.Errorf("acp: unmarshal prompt result: %w", err)
		}
		return bare.StopReason, nil, nil
	}
	u := pr.Usage
	if u != nil && u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 {
		u = nil
	}
	return pr.StopReason, u, nil
}

// handleNotify is the transport-level onNotify callback. It parses
// session/update notifications into Events and forwards them to the
// current per-call handler (if any). Other notifications are ignored.
func (c *Client) handleNotify(method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}

	var upd UpdateParams
	if err := json.Unmarshal(params, &upd); err != nil {
		return
	}

	ev := Event{Kind: upd.Update.SessionUpdate}
	switch upd.Update.SessionUpdate {
	case "agent_message_chunk":
		if len(upd.Update.Content) > 0 {
			ev.Text = upd.Update.Content[0].Text
		}
	case "tool_call", "tool_call_update":
		ev.ToolCallID = upd.Update.ToolCallID
		ev.Title = upd.Update.Title
		ev.Kind_ = upd.Update.Kind
		ev.Status = upd.Update.Status
		// Track the tool call so OnPermission can consult the guard.
		c.policyMu.Lock()
		if gp, ok := c.policy.(*GuardPolicy); ok {
			gp.TrackToolCall(upd.Update.ToolCallID, upd.Update)
		}
		c.policyMu.Unlock()
		// Apply file-change diffs the agent announces via a tool_call update.
		// Per the ACP edit model, the client is responsible for materializing
		// the edit on disk (gated by the policy); the agent itself does not
		// write files when the client advertised fs capability.
		if upd.Update.SessionUpdate == "tool_call" || upd.Update.SessionUpdate == "tool_call_update" {
			// The diff content (path + newText) may arrive on either the initial
			// tool_call or a subsequent tool_call_update; apply whichever carries it.
			c.applyDiffContent(upd.SessionID, params)
		}
	default:
		// Pass through other discriminators with whatever fields are set.
		ev.ToolCallID = upd.Update.ToolCallID
		ev.Title = upd.Update.Title
		ev.Kind_ = upd.Update.Kind
		ev.Status = upd.Update.Status
	}

	c.deliver(ev)
}

// deliver hands ev to the active per-call handler, if any. All deliveries go
// through here so that they are serialized against each other regardless of
// which goroutine produced the event.
func (c *Client) deliver(ev Event) {
	c.eventMu.Lock()
	handler := c.currentOnEvent
	c.eventMu.Unlock()
	if handler == nil {
		return
	}
	c.deliverMu.Lock()
	defer c.deliverMu.Unlock()
	handler(ev)
}

// applyDiffContent extracts "diff" content blocks from a tool_call update's
// raw params and applies each to disk: the newText is written to the (resolved)
// path, gated by the policy's OnFSWrite. A diff with non-nil oldText on an
// existing file is treated as an in-place replace of oldText with newText.
func (c *Client) applyDiffContent(sessionID string, raw json.RawMessage) {
	c.policyMu.Lock()
	p := c.policy
	c.policyMu.Unlock()
	if p == nil {
		return
	}
	var probe struct {
		Update struct {
			Content []struct {
				Type    string  `json:"type"`
				Path    string  `json:"path"`
				OldText *string `json:"oldText"`
				NewText string  `json:"newText"`
			} `json:"content"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return
	}
	for _, d := range probe.Update.Content {
		if d.Type != "diff" || d.Path == "" {
			continue
		}
		resolved := c.resolveFSPath(sessionID, d.Path)
		if err := p.OnFSWrite(resolved); err != nil {
			continue
		}
		// If oldText is provided and present in the file, replace just that
		// span; otherwise write newText as the whole file (create/overwrite).
		var content []byte
		if d.OldText != nil {
			if existing, err := os.ReadFile(resolved); err == nil {
				content = []byte(strings.Replace(string(existing), *d.OldText, d.NewText, 1))
			} else {
				content = []byte(d.NewText)
			}
		} else {
			content = []byte(d.NewText)
		}
		if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(resolved, content, 0o644); err != nil {
			continue
		}
		if c.worktreeID != "" && c.recordEdit != nil {
			_ = c.recordEdit(c.worktreeID, "acp", resolved, content) // best-effort
		}
	}
}

// handleInboundRequest dispatches inbound server->client requests to the
// installed Policy. It runs in the transport's ReadLoop goroutine.
//
// Dispatch by method:
//   - session/request_permission -> Policy.OnPermission, marshal PermissionOutcome
//   - fs/read_text_file          -> Policy.OnFSRead(path); on allow return FSReadResult{Content:""}
//     (a real client would read the file; we return empty as a stub)
//   - fs/write_text_file         -> Policy.OnFSWrite(path); on allow return nil (null result)
//   - terminal/create            -> Policy.OnTerminal(cmd); on allow return nil
//   - unknown                    -> method-not-found error (-32601)
//
// When the policy denies (returns non-nil error) or no policy is set, the
// transport writes a JSON-RPC error response so the agent sees the denial.
func (c *Client) handleInboundRequest(req inboundRequest) (json.RawMessage, error) {
	c.policyMu.Lock()
	p := c.policy
	c.policyMu.Unlock()

	if p == nil {
		return nil, &RPCError{Code: -32601, Message: "no policy set"}
	}

	switch req.Method {
	case "session/request_permission":
		var params RequestPermissionParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
		}
		outcome := p.OnPermission(params)
		data, err := json.Marshal(outcome)
		if err != nil {
			return nil, &RPCError{Code: -32603, Message: err.Error()}
		}
		return data, nil

	case "fs/read_text_file":
		var params FSReadParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
		}
		resolved := c.resolveFSPath(params.SessionID, params.Path)
		if err := p.OnFSRead(resolved); err != nil {
			return nil, &RPCError{Code: -32603, Message: err.Error()}
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return nil, &RPCError{Code: -32603, Message: "read: " + err.Error()}
		}
		result := FSReadResult{Content: string(data)}
		out, err := json.Marshal(result)
		if err != nil {
			return nil, &RPCError{Code: -32603, Message: err.Error()}
		}
		return out, nil

	case "fs/write_text_file":
		var params FSWriteParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
		}
		resolved := c.resolveFSPath(params.SessionID, params.Path)
		if err := p.OnFSWrite(resolved); err != nil {
			return nil, &RPCError{Code: -32603, Message: err.Error()}
		}
		if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
			return nil, &RPCError{Code: -32603, Message: "mkdir: " + err.Error()}
		}
		if err := os.WriteFile(resolved, []byte(params.Content), 0o644); err != nil {
			return nil, &RPCError{Code: -32603, Message: "write: " + err.Error()}
		}
		return json.RawMessage("null"), nil

	case "terminal/create":
		var params struct {
			SessionID string   `json:"sessionId"`
			Command   string   `json:"command"`
			Args      []string `json:"args"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, &RPCError{Code: -32602, Message: "invalid params: " + err.Error()}
		}
		cmd := params.Command
		if len(params.Args) > 0 {
			for _, a := range params.Args {
				cmd += " " + a
			}
		}
		if err := p.OnTerminal(cmd); err != nil {
			return nil, &RPCError{Code: -32603, Message: err.Error()}
		}
		// Stub: a real client would create a terminal and return a terminalId.
		return json.RawMessage(`{"terminalId":"stub"}`), nil

	default:
		return nil, &RPCError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

// resolveFSPath resolves an agent-supplied path against the session's Cwd. An
// already-absolute path is cleaned and used as-is; a relative path is joined to
// the Cwd. The guard's OnFSRead/OnFSWrite should be checked against the
// resolved (absolute) path.
func (c *Client) resolveFSPath(sessionID, path string) string {
	c.sessionMu.Lock()
	cwd, ok := c.sessionCwds[sessionID]
	c.sessionMu.Unlock()
	if ok && cwd != "" && !filepath.IsAbs(path) {
		return filepath.Clean(filepath.Join(cwd, path))
	}
	return filepath.Clean(path)
}

// Cancel sends a "session/cancel" notification for the given session.
// The agent should finalise any pending session/prompt with stopReason:"cancelled".
// ctx is checked for cancellation before sending; the notification itself is a
// synchronous write (no response is expected).
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.tr.Notify("session/cancel", CancelParams{SessionID: sessionID})
}

// Close shuts down the client: cancels the transport ReadLoop context and
// closes the writer (stdin pipe) so the agent sees EOF and can exit.
// If the writer implements io.Closer (e.g. exec.Cmd stdin pipe) it is closed;
// otherwise only the ReadLoop context is cancelled.
func (c *Client) Close() {
	if c.readLoopCancel != nil {
		c.readLoopCancel()
	}
	if closer, ok := c.writer.(io.Closer); ok {
		closer.Close()
	}
}
