// internal/tools/history.go
//
// C2: the tools that turn eviction into PAGING instead of forgetting.
//
// Compaction removes messages from the live window. Before C1 that was the end
// of them; now they are still in the durable log, which means the model can be
// given a way back to them. Without such a tool the durable log is an archive
// nobody can read from inside the conversation that produced it — the write
// side would be done and the read side would not exist, which is this repo's
// most-repeated defect shape.
//
// Two tools, matching the two questions a model actually asks about its own
// past: "where did I see X" (history_search) and "show me what happened around
// there" (history_read). Both are hard-bounded: a recall tool that can return
// the whole log would re-fill the window it exists to relieve, so the size cap
// is not a nicety, it is the feature.
package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/store"
)

// HistoryTools exposes history_search / history_read as GuardedTools.
type HistoryTools struct {
	store  *store.Store
	Search *GuardedTool
	Read   *GuardedTool
}

// historySearchLimit is the default number of hits history_search returns when
// the model does not ask for a specific count.
const historySearchLimit = 8

// historyReadLimit is the default number of messages history_read returns.
// Deliberately small: the caller has a seq range and can page.
const historyReadLimit = 20

// historyBodyBudget caps the rendered characters of ONE message body. A single
// evicted tool result can be megabytes — the whole reason it was evicted — so
// returning it whole would undo the compaction that made the recall necessary.
// The truncation marker names the seq so the model can decide to narrow its
// range rather than silently reading a fragment.
const historyBodyBudget = 2000

// historyTotalBudget caps the rendered characters of a WHOLE tool result. Both
// caps are needed: the per-message cap alone still lets 20 messages × 2000
// chars back into the window at once.
const historyTotalBudget = 12000

// NewHistoryTools builds the session-history recall tools backed by s.
func NewHistoryTools(s *store.Store) *HistoryTools {
	ht := &HistoryTools{store: s}

	ht.Search = NewGuardedTool(
		"history_search", "History Search",
		"Full-text search over THIS conversation's complete history, including "+
			"messages already dropped from the context window (tool calls, command "+
			"output, diffs). Use it instead of asking the user to repeat something, "+
			"or when you remember doing work whose details are no longer visible. "+
			"Returns matching excerpts with their seq numbers; pass a seq to "+
			"history_read for the full text. Set all_sessions to also search past "+
			"conversations, e.g. for \"how did we fix that last week\".",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"query": {Type: schema.String, Desc: "SQLite FTS5 query, e.g. `panic` or `\"go test\"` or `guard AND deny`", Required: true},
			"limit": {Type: schema.Integer, Desc: fmt.Sprintf("max hits (default %d, max %d)", historySearchLimit, store.MaxMessagePageSize)},
			"all_sessions": {Type: schema.Boolean, Desc: "search every past conversation on this machine, not just this one. " +
				"Use for questions about earlier sessions, e.g. \"how did we fix that bug last week\". " +
				"Cannot target one specific other session by id — it is either this conversation or all of them. " +
				"Each hit names the session it came from; only hits from THIS conversation can be expanded with history_read."},
		}),
		SyncStream(ht.runSearch),
	)

	ht.Read = NewGuardedTool(
		"history_read", "History Read",
		"Read this conversation's stored messages by sequence number, including "+
			"ones no longer in the context window. Use after history_search to see "+
			"a hit in full, or to page back through earlier work. Long message "+
			"bodies are truncated; narrow the range to see more of each.",
		30*time.Second,
		params(map[string]*schema.ParameterInfo{
			"from_seq": {Type: schema.Integer, Desc: "first sequence number to return (inclusive, default 0)"},
			"to_seq":   {Type: schema.Integer, Desc: "stop before this sequence number (exclusive; omit for the end of the log)"},
			"limit":    {Type: schema.Integer, Desc: fmt.Sprintf("max messages (default %d, max %d)", historyReadLimit, store.MaxMessagePageSize)},
			"newest":   {Type: schema.Boolean, Desc: "when true and the range is larger than limit, return the LAST messages of the range instead of the first"},
		}),
		SyncStream(ht.runRead),
	)

	return ht
}

// Tools returns all history tools as a slice for convenience.
func (h *HistoryTools) Tools() []*GuardedTool { return []*GuardedTool{h.Search, h.Read} }

type historySearchArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
	// AllSessions widens history_search from "this conversation" to "every
	// conversation this store has ever recorded" (store.SearchMessages with
	// an empty session id). That IS a read-scope widening, not a harmless
	// convenience: historySessionID's doc comment explains why scoping
	// exists in the first place — a model that has merely SEEN another
	// session's id in a log line must not be able to read that session's
	// whole evicted-tool-output history just by naming it.
	//
	// What keeps this opt-in acceptable to expose to the model is that it is
	// a single global switch, not a targeting primitive: there is no session
	// id argument here, and there must never be one for the same reason
	// historySessionID takes none — the choice is "just mine" or
	// "everything", never "that one over there". A hit's originating session
	// is still surfaced (formatHistoryHits tags it when AllSessions is set),
	// so the widening is visible in the output, not silent, but it does mean
	// prose from OTHER projects sharing this store file can surface in a
	// search a model runs on this one.
	AllSessions bool `json:"all_sessions"`
}

type historyReadArgs struct {
	FromSeq int  `json:"from_seq"`
	ToSeq   int  `json:"to_seq"`
	Limit   int  `json:"limit"`
	Newest  bool `json:"newest"`
}

// historySessionID resolves the conversation these tools may read.
//
// It is taken from the context, never from a tool argument, and that is a
// SECURITY property rather than an ergonomic one: a session id parameter would
// let a model that has seen another session's id in a log line read that
// conversation's entire history, including its evicted tool output. The same
// reason WithVCS's scope overrides caller-supplied scope.
//
// The approval context is the primary source (it is where the WS connection's
// session id is bound into the tool layer); the thread link is the fallback,
// because the WS turn sets ThreadID to the session id and the approval manager
// is absent when approvals are unconfigured.
func historySessionID(ctx context.Context) (string, error) {
	if ac, ok := approvalFromContext(ctx); ok && ac.SessionID != "" {
		return ac.SessionID, nil
	}
	if link, ok := ThreadLinkFromContext(ctx); ok && link.ThreadID != "" {
		return link.ThreadID, nil
	}
	return "", fmt.Errorf("no conversation is being recorded, so there is no history to search")
}

func (h *HistoryTools) runSearch(ctx context.Context, argsJSON string) (string, error) {
	var a historySearchArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("history_search: query is required")
	}
	// Resolution branches on AllSessions rather than always calling
	// historySessionID and discarding its result on the true branch.
	// historySessionID hard-errors when ctx carries no conversation (see its
	// doc comment) — correct for the scoped path, wrong for this one. A
	// cross-session search has no such precondition: "search everything I
	// have ever recorded" must still work when there happens to be no
	// CURRENT session bound to ctx, which is exactly the shape of the
	// question this switch exists to answer ("what did we do last week", not
	// "what have we done so far right now").
	sessionID := ""
	if !a.AllSessions {
		sid, err := historySessionID(ctx)
		if err != nil {
			return "", err
		}
		sessionID = sid
	}
	limit := a.Limit
	if limit <= 0 {
		limit = historySearchLimit
	}
	hits, err := h.store.SearchMessages(sessionID, a.Query, limit)
	if err != nil {
		// An FTS5 syntax error is the model's mistake, not a system failure, and
		// it is recoverable by rephrasing — so it comes back as an instruction.
		return "", fmt.Errorf("history_search: %w (FTS5 syntax: bare words are ANDed, "+
			"use double quotes for phrases, OR / NOT for boolean terms)", err)
	}
	return formatHistoryHits(hits, a.AllSessions), nil
}

func (h *HistoryTools) runRead(ctx context.Context, argsJSON string) (string, error) {
	var a historyReadArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	if a.FromSeq < 0 {
		return "", fmt.Errorf("history_read: from_seq must not be negative")
	}
	if a.ToSeq > 0 && a.ToSeq <= a.FromSeq {
		return "", fmt.Errorf("history_read: to_seq (%d) must be greater than from_seq (%d); "+
			"to_seq is exclusive", a.ToSeq, a.FromSeq)
	}
	sessionID, err := historySessionID(ctx)
	if err != nil {
		return "", err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = historyReadLimit
	}
	msgs, err := h.store.MessagesPage(store.MessageRange{
		SessionID: sessionID,
		FromSeq:   a.FromSeq,
		ToSeq:     a.ToSeq,
		Limit:     limit,
		Newest:    a.Newest,
	})
	if err != nil {
		return "", err
	}
	return formatHistoryMessages(msgs), nil
}

// formatHistoryHits renders search results: one header line per hit carrying
// the seq (the handle for history_read) and the FTS snippet.
//
// allSessions must be the same flag that picked the search's scope. It exists
// because history_read only ever reads the CURRENT session (historySessionID
// is its only source of a session id — see that function's doc comment), so a
// hit's seq is only actionable there when the hit came from that session. A
// cross-session result set can contain seqs from other conversations that
// history_read cannot follow, and telling the model "use history_read with a
// seq" without qualification would be an instruction that silently does the
// wrong thing for most of the hits it was just given.
func formatHistoryHits(hits []store.MessageSearchHit, allSessions bool) string {
	if len(hits) == 0 {
		if allSessions {
			return "No matching messages in any recorded session."
		}
		return "No matching messages in this conversation's history."
	}
	var b strings.Builder
	if allSessions {
		fmt.Fprintf(&b, "%d match(es) across sessions. Only hits from THIS conversation's "+
			"session can be expanded with history_read; others are shown as excerpts only.\n", len(hits))
	} else {
		fmt.Fprintf(&b, "%d match(es). Use history_read with a seq to see one in full.\n", len(hits))
	}
	for _, hit := range hits {
		b.WriteByte('\n')
		b.WriteString(historyHeader(hit.Message))
		if allSessions {
			fmt.Fprintf(&b, " [session %s]", hit.SessionID)
		}
		snippet := strings.TrimSpace(collapseBlankLines(hit.Snippet))
		if snippet != "" {
			b.WriteString("\n    " + strings.ReplaceAll(snippet, "\n", "\n    "))
		}
	}
	return b.String()
}

// formatHistoryMessages renders full messages under both budgets. When the
// total budget stops the rendering it says so and names the seq to resume from
// — a silent cut would look like "the history ends here", which is a lie the
// model would act on.
func formatHistoryMessages(msgs []store.Message) string {
	if len(msgs) == 0 {
		return "No messages in that range."
	}
	var b strings.Builder
	for i, m := range msgs {
		if b.Len() > historyTotalBudget {
			fmt.Fprintf(&b, "\n\n[%d more message(s) not shown — call history_read again with from_seq=%d]",
				len(msgs)-i, m.Seq)
			break
		}
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(historyHeader(m))
		body := historyBody(m)
		if body != "" {
			b.WriteString("\n    " + strings.ReplaceAll(body, "\n", "\n    "))
		}
	}
	return b.String()
}

// historyHeader renders the one-line identity of a stored message: its seq (the
// handle for history_read), its role, the tool name for tool rows, and the date.
func historyHeader(m store.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d [%s", m.Seq, m.Role)
	if m.ToolName != "" {
		b.WriteString(" " + m.ToolName)
	}
	b.WriteString("] " + time.Unix(m.CreatedAt, 0).Format("2006-01-02 15:04"))
	return b.String()
}

// historyBody renders a message's payload under the per-message budget. For a
// tool_call the payload is the arguments; for everything else it is the
// content. Truncation is announced with the byte count that was dropped.
func historyBody(m store.Message) string {
	body := m.Content
	if m.Role == store.RoleToolCall && strings.TrimSpace(body) == "" {
		body = m.ToolArgs
	} else if m.ToolArgs != "" && m.Role == store.RoleToolCall {
		body = m.ToolArgs + "\n" + body
	}
	body = strings.TrimRight(body, "\n")
	if len(body) > historyBodyBudget {
		dropped := len(body) - historyBodyBudget
		body = body[:historyBodyBudget] +
			fmt.Sprintf("\n… [%d more characters truncated]", dropped)
	}
	return body
}

// collapseBlankLines squeezes runs of blank lines out of an FTS snippet, which
// stitches excerpts together with separators and can otherwise produce several
// empty lines per hit.
func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
