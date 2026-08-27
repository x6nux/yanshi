package acpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/x6nux/yanshi/internal/acp"
	v1 "github.com/x6nux/yanshi/internal/api/v1"
)

// Session lifecycle and the prompt turn.

// newSession creates a v1 thread and returns an ACP session id bound to it.
//
// The ACP session id is minted here rather than reusing the thread id, so the
// protocol-facing identifier and the storage identifier can diverge later
// without a wire break. The mapping is what makes session/load work.
func (s *Server) newSession(ctx context.Context, params json.RawMessage) (any, *acp.RPCError) {
	var p acp.NewSessionParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: "invalid session/new params: " + err.Error()}
		}
	}
	thread, err := s.agent.Start(ctx, v1.ThreadStartParams{Title: sessionTitle(p.Cwd)})
	if err != nil {
		return nil, &acp.RPCError{Code: -32603, Message: err.Error()}
	}
	s.mu.Lock()
	s.nextID++
	sessionID := "yanshi-" + strconv.Itoa(s.nextID)
	s.sessions[sessionID] = &acpSession{threadID: thread.ID}
	s.mu.Unlock()

	// mcpServers is accepted and IGNORED, and saying so beats silently
	// dropping it: yanshi's MCP servers are the ones its own config declares
	// and its own guard gates. Wiring a host-supplied server into a running
	// orchestrator would add tools the operator's profile never authorised.
	if len(p.McpServers) > 0 {
		fmt.Fprintf(s.diag, "acpserver: ignoring %d host-supplied mcpServers; "+
			"yanshi uses the servers in its own config, which its guard profile authorises\n",
			len(p.McpServers))
	}
	return acp.NewSessionResult{SessionID: sessionID}, nil
}

// sessionTitle names the thread after the directory the host opened, which is
// what makes a resumed session recognisable in `/sessions`.
func sessionTitle(cwd string) string {
	if cwd == "" {
		return "ACP session"
	}
	return "ACP: " + cwd
}

// loadSession re-binds an existing ACP session id.
//
// It refuses an id this process never minted rather than creating one. A host
// that reconnects after yanshi restarted has a stale id, and quietly handing
// back a FRESH empty session would present a conversation with no history as
// the resumed one — the user's context would be gone with nothing saying so.
func (s *Server) loadSession(ctx context.Context, params json.RawMessage) (any, *acp.RPCError) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &acp.RPCError{Code: -32602, Message: "invalid session/load params: " + err.Error()}
	}
	s.mu.Lock()
	sess, ok := s.sessions[p.SessionID]
	s.mu.Unlock()
	if !ok {
		return nil, &acp.RPCError{Code: -32602,
			Message: "unknown session " + p.SessionID + "; start a new one with session/new"}
	}
	if _, err := s.agent.Resume(ctx, v1.ThreadResumeParams{ThreadID: sess.threadID}); err != nil {
		return nil, &acp.RPCError{Code: -32603, Message: err.Error()}
	}
	return map[string]any{}, nil
}

// startPrompt begins a turn and hands the response off to a goroutine.
//
// It returns errDeferred on success: in ACP the session/prompt RESPONSE is the
// end of the turn, carrying the stop reason, while session/update
// notifications stream underneath it. Answering immediately would tell the host
// the turn had finished before the model produced a single token.
func (s *Server) startPrompt(ctx context.Context, w io.Writer, id int64, params json.RawMessage) *acp.RPCError {
	var p acp.PromptParams
	if err := json.Unmarshal(params, &p); err != nil {
		return &acp.RPCError{Code: -32602, Message: "invalid session/prompt params: " + err.Error()}
	}
	s.mu.Lock()
	sess, ok := s.sessions[p.SessionID]
	s.mu.Unlock()
	if !ok {
		return &acp.RPCError{Code: -32602, Message: "unknown session " + p.SessionID}
	}
	text := promptText(p.Prompt)
	if text == "" {
		return &acp.RPCError{Code: -32602, Message: "session/prompt carried no text content"}
	}

	turnCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	sess.cancel = cancel
	s.mu.Unlock()

	started, items, err := s.agent.StartTurn(turnCtx, v1.TurnStartParams{
		ThreadID: sess.threadID, Input: text,
	})
	if err != nil {
		cancel()
		s.clearCancel(sess)
		return &acp.RPCError{Code: -32603, Message: err.Error()}
	}
	s.inflight.Add(1)
	go func() {
		defer s.inflight.Done()
		defer cancel()
		defer s.clearCancel(sess)
		s.streamTurn(turnCtx, w, id, p.SessionID, started.Turn.ID, items)
	}()
	return errDeferred
}

// clearCancel drops a finished turn's cancel so a later session/cancel does not
// abort a turn that has already ended — or, worse, a DIFFERENT turn that
// happened to start in between.
func (s *Server) clearCancel(sess *acpSession) {
	s.mu.Lock()
	sess.cancel = nil
	s.mu.Unlock()
}

// promptText flattens ACP content blocks into the single string v1 takes.
//
// Non-text blocks are skipped rather than rendered as a placeholder: the
// initialize result declares image/audio/embeddedContext unsupported, so a host
// that sends one is already outside what was advertised, and inventing "[image]"
// would hand the model a description of something it cannot see.
func promptText(blocks []acp.ContentBlock) string {
	out := ""
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if out != "" {
				out += "\n"
			}
			out += b.Text
		}
	}
	return out
}

// streamTurn drains the v1 item channel, forwarding each item as a
// session/update notification, then answers the held session/prompt request.
//
// The channel is drained to completion even after a write failure. Abandoning
// it would leave the v1 producer blocked on backpressure until its context
// died, holding the thread's active-turn slot and making the host's next prompt
// fail with "turn already active" for a turn nobody is watching.
func (s *Server) streamTurn(ctx context.Context, w io.Writer, id int64, sessionID, turnID string, items <-chan v1.Item) {
	stopReason := "end_turn"
	writeFailed := false
	for item := range items {
		if reason, terminal := terminalStopReason(item); terminal {
			stopReason = reason
		}
		upd, ok := updateForItem(item)
		if !ok || writeFailed {
			continue
		}
		if err := s.writeNotify(w, "session/update", acp.UpdateParams{
			SessionID: sessionID, Update: upd,
		}); err != nil {
			// Keep draining; see the doc comment.
			fmt.Fprintf(s.diag, "acpserver: dropping session/update for turn %s: %v\n", turnID, err)
			writeFailed = true
		}
	}
	if ctx.Err() != nil {
		// A cancelled turn reports "cancelled" whatever the last item said,
		// because the items stop arriving the moment the context dies and the
		// final one is whatever happened to be in flight.
		stopReason = "cancelled"
	}
	if writeFailed {
		return
	}
	if err := s.writeResult(w, id, acp.PromptResult{StopReason: stopReason}); err != nil {
		fmt.Fprintf(s.diag, "acpserver: could not answer session/prompt for turn %s: %v\n", turnID, err)
	}
}

// terminalStopReason maps a terminal v1 item onto an ACP stop reason.
//
// ACP's vocabulary is narrower than v1's, and the mapping is the honest one:
// a turn that ERRORED is not a turn that completed, and reporting "end_turn"
// for it would make a failure indistinguishable from success to a host whose
// only signal is this field.
func terminalStopReason(item v1.Item) (string, bool) {
	switch item.Type {
	case v1.ItemTurnCompleted:
		return "end_turn", true
	case v1.ItemTurnError:
		return "refusal", true
	default:
		return "", false
	}
}

// updateForItem maps one v1 item onto an ACP session/update payload, reporting
// false for items ACP has no vocabulary for.
//
// turn.started and turn.completed are deliberately dropped: ACP marks the start
// of a turn with the session/prompt request itself and the end with its
// response, so forwarding them as updates would double-report both boundaries.
func updateForItem(item v1.Item) (acp.Update, bool) {
	switch item.Type {
	case v1.ItemMessageDelta:
		return acp.Update{
			SessionUpdate: "agent_message_chunk",
			Content:       []acp.ContentBlock{{Type: "text", Text: item.Text}},
		}, item.Text != ""
	case v1.ItemReasoningDelta:
		return acp.Update{
			SessionUpdate: "agent_thought_chunk",
			Content:       []acp.ContentBlock{{Type: "text", Text: item.Text}},
		}, item.Text != ""
	case v1.ItemToolCall:
		return acp.Update{
			SessionUpdate: "tool_call", ToolCallID: item.ID,
			Title: item.ToolName, Kind: "other", Status: "pending",
		}, true
	case v1.ItemToolProgress:
		return acp.Update{
			SessionUpdate: "tool_call_update", ToolCallID: item.ID,
			Title: item.ToolName, Status: "in_progress",
		}, true
	case v1.ItemToolResult:
		return acp.Update{
			SessionUpdate: "tool_call_update", ToolCallID: item.ID,
			Title: item.ToolName, Status: toolStatus(item),
		}, true
	case v1.ItemTurnError:
		// Surfaced as a message chunk as well as a stop reason: the stop reason
		// says the turn failed, and this says WHY. A host that only renders
		// message content would otherwise show a turn that stopped for no
		// visible reason.
		return acp.Update{
			SessionUpdate: "agent_message_chunk",
			Content:       []acp.ContentBlock{{Type: "text", Text: turnErrorText(item)}},
		}, true
	default:
		return acp.Update{}, false
	}
}

// toolStatus maps a v1 tool result onto ACP's tool-call status vocabulary.
func toolStatus(item v1.Item) string {
	if item.Error != "" || item.Status == "error" {
		return "failed"
	}
	return "completed"
}

// turnErrorText renders a turn.error item for display, falling back to a fixed
// string so an error item with neither field still says something.
func turnErrorText(item v1.Item) string {
	if item.Error != "" {
		return "turn failed: " + item.Error
	}
	if item.Text != "" {
		return "turn failed: " + item.Text
	}
	return "turn failed"
}
