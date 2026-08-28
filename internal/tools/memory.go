package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/store"
)

// MemoryTools exposes memory_search / memory_recall / memory_write /
// memory_source as GuardedTools.
type MemoryTools struct {
	store  *store.Store
	Search *GuardedTool
	Recall *GuardedTool
	Write  *GuardedTool
	Source *GuardedTool
}

// maxSourceRows bounds what memory_source renders. The provenance is a POSITION
// in the log, so the slice it resolves to grows with the conversation and has no
// natural end — an unbounded render would hand a long session's whole transcript
// back through a tool whose question was "where did this note come from".
const maxSourceRows = 10

// NewMemoryTools builds memory tools backed by s.
func NewMemoryTools(s *store.Store) *MemoryTools {
	mt := &MemoryTools{store: s}

	mt.Search = NewGuardedTool(
		"memory_search", "Memory Search", "Full-text search over stored memories.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"query": {Type: schema.String, Desc: "search terms", Required: true},
			"limit": {Type: schema.Integer, Desc: "max results (default 10)"},
			"scope": {Type: schema.String, Desc: "'all' (default, every conversation and agent), 'session' (this conversation), or 'agent' (memories written by this agent)"},
		}),
		SyncStream(mt.runSearch),
	)

	mt.Recall = NewGuardedTool(
		"memory_recall", "Memory Recall", "Return most-recent memories.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"limit": {Type: schema.Integer, Desc: "max results (default 10)"},
			"scope": {Type: schema.String, Desc: "'all' (default, every conversation and agent), 'session' (this conversation), or 'agent' (memories written by this agent)"},
		}),
		SyncStream(mt.runRecall),
	)

	mt.Write = NewGuardedTool(
		"memory_write", "Memory Write", "Store a memory for later recall.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"content": {Type: schema.String, Desc: "memory text", Required: true},
			"kind":    {Type: schema.String, Desc: "category, e.g. note/pref/fact"},
		}),
		SyncStream(mt.runWrite),
	)

	mt.Source = NewGuardedTool(
		"memory_source", "Memory Source",
		"Show the conversation a memory was derived from, given its id (the id= field in memory_search / memory_recall output).",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"id": {Type: schema.String, Desc: "memory id", Required: true},
		}),
		SyncStream(mt.runSource),
	)

	return mt
}

// Tools returns all memory tools as a slice for convenience.
func (m *MemoryTools) Tools() []*GuardedTool {
	return []*GuardedTool{m.Search, m.Recall, m.Write, m.Source}
}

// --- arg types ---

type searchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
	Scope string `json:"scope"`
}

type recallArgs struct {
	Limit int    `json:"limit"`
	Scope string `json:"scope"`
}

type writeArgs struct {
	Content string `json:"content"`
	Kind    string `json:"kind"`
}

type sourceArgs struct {
	ID string `json:"id"`
}

// --- C14 retrieval dimensions ---

// Memory scope names accepted by memory_search / memory_recall.
const (
	// MemoryScopeAll searches every session and agent. The DEFAULT, and
	// deliberately so: sub-agents and the goalloop have always shared one table,
	// and making a narrower scope the default would make memories that were
	// findable yesterday vanish today without a single error to notice.
	MemoryScopeAll = "all"
	// MemoryScopeSession restricts to the current conversation.
	MemoryScopeSession = "session"
	// MemoryScopeAgent restricts to memories written by the acting agent.
	MemoryScopeAgent = "agent"
)

// memoryDims resolves the write-side dimensions for the current call from the
// same context values everything else in the tool layer reads: the session from
// the approval context (with the thread link as fallback, since the WS turn
// sets ThreadID to the session id), and the acting agent from the VCS scope,
// which already carries it for commit attribution.
//
// Empty is a legitimate answer — the SSE path has no session, a bare sub-agent
// has no VCS scope — and is stored as the empty string rather than a
// placeholder, because the empty string is precisely the value an unscoped
// query matches and a made-up id is not.
func memoryDims(ctx context.Context) store.MemoryFilter {
	var f store.MemoryFilter
	if ac, ok := approvalFromContext(ctx); ok {
		f.SessionID = ac.SessionID
	}
	if f.SessionID == "" {
		if link, ok := ThreadLinkFromContext(ctx); ok {
			f.SessionID = link.ThreadID
		}
	}
	if scope, ok := VCSScopeFromContext(ctx); ok {
		f.AgentID = scope.Agent
	}
	return f
}

// memoryFilterFor turns a caller-supplied scope name into a query filter.
//
// An unknown scope is an ERROR rather than a silent fallback to "all": a model
// that asks for `scope:"this-session"` and is quietly given every agent's
// memories has been answered wrongly with no way to tell. Asking for a
// dimension the context cannot supply is likewise an error, for the same
// reason — an empty SessionID would match the rows written with an empty
// dimension by callers that had no session, which is the opposite of "restrict
// to mine".
func memoryFilterFor(ctx context.Context, scope string) (store.MemoryFilter, error) {
	dims := memoryDims(ctx)
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "", MemoryScopeAll:
		return store.MemoryFilter{}, nil
	case MemoryScopeSession:
		if dims.SessionID == "" {
			return store.MemoryFilter{}, fmt.Errorf(
				"memory scope %q is unavailable: this call has no recorded conversation", MemoryScopeSession)
		}
		return store.MemoryFilter{SessionID: dims.SessionID}, nil
	case MemoryScopeAgent:
		if dims.AgentID == "" {
			return store.MemoryFilter{}, fmt.Errorf(
				"memory scope %q is unavailable: no acting agent is bound to this call", MemoryScopeAgent)
		}
		return store.MemoryFilter{AgentID: dims.AgentID}, nil
	default:
		return store.MemoryFilter{}, fmt.Errorf(
			"unknown memory scope %q (want %q, %q or %q)",
			scope, MemoryScopeAll, MemoryScopeSession, MemoryScopeAgent)
	}
}

// --- run functions ---

func (m *MemoryTools) runSearch(ctx context.Context, argsJSON string) (string, error) {
	var a searchArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	filter, err := memoryFilterFor(ctx, a.Scope)
	if err != nil {
		return "", err
	}
	ms, err := m.store.SearchMemoryScoped(a.Query, a.Limit, filter)
	if err != nil {
		return "", err
	}
	return formatMemories(ms), nil
}

func (m *MemoryTools) runRecall(ctx context.Context, argsJSON string) (string, error) {
	var a recallArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	filter, err := memoryFilterFor(ctx, a.Scope)
	if err != nil {
		return "", err
	}
	ms, err := m.store.RecallMemoryScoped(a.Limit, filter)
	if err != nil {
		return "", err
	}
	return formatMemories(ms), nil
}

func (m *MemoryTools) runWrite(ctx context.Context, argsJSON string) (string, error) {
	var a writeArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	kind := a.Kind
	if kind == "" {
		kind = "note"
	}
	// C14: tag the row with whatever dimensions this call actually has. Writing
	// is where the dimensions must be captured — a memory whose origin was not
	// recorded can never be filtered later, however good the query side gets.
	// W-D-07 rides on the same call: provenance is captured at write time for
	// the same reason the dimensions are — the log position that produced a
	// memory is knowable now and unrecoverable later.
	id, err := m.store.WriteMemoryFromSession(kind, a.Content, memoryDims(ctx))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Stored as %s [%s]", id, kind), nil
}

// runSource answers "where did this memory come from" (W-D-07).
//
// IT IS THE READ SIDE OF THE PROVENANCE COLUMNS, and it exists because the
// write side alone is not the feature: source_session_id / source_seq were
// captured by both production writers and no user, model, tool, frame or
// endpoint could read them back, so "every memory traces to the log position
// that produced it" was true only inside _test.go files.
//
// The three answers are kept apart on purpose, because store.MemorySource
// distinguishes them and collapsing them would throw that away:
//   - no provenance recorded (ErrNoMemorySource) is PERMANENT — every row
//     written before W-D-07, and every memory written without a session.
//   - a resolved source with no rows means history that should still be there
//     is gone, which is a different and much worse thing to be told.
//   - otherwise, the conversation from that position on.
func (m *MemoryTools) runSource(_ context.Context, argsJSON string) (string, error) {
	var a sourceArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.ID) == "" {
		return "", fmt.Errorf("memory_source: id is required")
	}
	msgs, err := m.store.MemorySource(a.ID)
	if errors.Is(err, store.ErrNoMemorySource) {
		return "No source was recorded for this memory. Memories written before " +
			"provenance existed, and memories written outside a conversation, " +
			"carry none — this will not change.", nil
	}
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "This memory names a source position, but no messages remain there — " +
			"the conversation it came from has been truncated or deleted.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Derived from session %s, from seq %d (%d message(s) follow it):\n",
		msgs[0].SessionID, msgs[0].Seq, len(msgs))
	shown := msgs
	if len(shown) > maxSourceRows {
		shown = shown[:maxSourceRows]
	}
	for _, msg := range shown {
		text := msg.Content
		if text == "" && msg.ToolName != "" {
			text = msg.ToolName + " " + msg.ToolArgs
		}
		fmt.Fprintf(&b, "  #%d %s: %s\n", msg.Seq, msg.Role, strings.TrimRight(text, "\n"))
	}
	if len(shown) < len(msgs) {
		fmt.Fprintf(&b, "  … %d more; read them with history_read\n", len(msgs)-len(shown))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// formatMemories renders a slice of Memory as human-readable text. Each entry
// shows kind, date, and content lines. Multiple entries are separated by a blank
// line. The caller (TUI transcript) already wraps the result in a tool-entry
// block with expand/collapse (ctrl+o), so we return the full text here and let
// the rendering layer handle truncation.
func formatMemories(ms []store.Memory) string {
	if len(ms) == 0 {
		return "No memories found."
	}
	var b strings.Builder
	for i, m := range ms {
		if i > 0 {
			b.WriteByte('\n')
		}
		// Header line: [kind] 2006-01-02 id=…
		//
		// The id is here because memory_source takes one, and a listing that
		// withholds it makes the trace reachable only for a memory the caller
		// wrote itself this turn (memory_write echoes the id). W-D-07's whole
		// claim is that ANY memory can be traced back to the log position that
		// produced it.
		ts := time.Unix(m.CreatedAt, 0).Format("2006-01-02")
		b.WriteString(fmt.Sprintf("[%s] %s id=%s\n", m.Kind, ts, m.ID))
		// Content body: indented, preserves line breaks
		body := strings.TrimRight(m.Content, "\n")
		if body != "" {
			indented := "    " + strings.ReplaceAll(body, "\n", "\n    ")
			b.WriteString(indented)
		}
	}
	return b.String()
}
