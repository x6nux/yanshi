// internal/store/message_log.go
//
// The durable conversation log: the half of a session that survives context
// eviction.
//
// Before C1 the only writer was AppendMessage, which persisted one flat
// user/assistant pair per turn. The tool_call and tool_result messages the
// ReAct loop produced — the test logs, the diffs, the compiler errors, i.e.
// most of what a turn actually LEARNED — existed only inside the live context
// window, so compaction did not summarise them, it destroyed them.
//
// Two invariants make this file's shape non-obvious, and both are load-bearing:
//
//  1. APPENDS ARE ATOMIC AND IDEMPOTENT. AppendMessages writes a whole batch in
//     one transaction, and every row carries a DedupKey unique per session. A
//     re-flush of a history window that is already durable inserts nothing and
//     reports success. That is what lets the WS layer flush the ENTIRE live
//     window before every eviction without tracking a per-connection watermark:
//     re-persisting is free and cannot duplicate. The idea (and the ON CONFLICT
//     DO NOTHING implementation) is taken from QwenPaw's conversation_history,
//     whose ux_dedup index serves the same purpose.
//
//  2. SEQ IS ASSIGNED BY THE STORE, INSIDE THE TRANSACTION. Callers do not pick
//     sequence numbers for batch appends. The live window and the durable log
//     diverge the moment anything is evicted — the window shrinks, the log does
//     not — so a caller-side counter would drift and start overwriting history.
//     MaxSeq+1 read under the same write transaction cannot drift.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Message roles used by the durable log. The chat roles (user / assistant) are
// operator-facing strings that predate this file and are kept verbatim; the two
// tool roles are new with C1.
const (
	// RoleUser is a message the human typed.
	RoleUser = "user"
	// RoleAssistant is model-authored prose.
	RoleAssistant = "assistant"
	// RoleToolCall is the model's request to run a tool. Content is normally
	// empty; ToolName and ToolArgs carry the payload.
	RoleToolCall = "tool_call"
	// RoleToolResult is what the tool returned. Content is the result text and
	// ToolCallID links it back to its RoleToolCall row.
	RoleToolResult = "tool_result"
)

// maxDedupSourceBytes caps how much message text feeds the dedup fingerprint.
// A 4 MiB tool result would otherwise be hashed in full on every flush. The
// prefix plus the byte length is enough to separate distinct messages, and a
// false MATCH would only merge two rows that share a multi-kilobyte prefix AND
// an exact length AND a position in the same flush — while a false MISS (which
// this bound cannot cause) is the only direction that could lose data.
const maxDedupSourceBytes = 4096

// dedupKeyFor derives the stable per-session identity of a message.
//
// Eino's schema.Message has no id, so there is nothing to key on directly. The
// fingerprint is the content that defines the message plus `ordinal`, which the
// caller sets to the number of BYTE-IDENTICAL messages preceding it in the same
// batch. Without the ordinal an assistant that answers "ok" twice in one turn
// would collapse into a single row.
func dedupKeyFor(m Message, ordinal int) string {
	h := sha256.New()
	write := func(s string) {
		if len(s) > maxDedupSourceBytes {
			s = s[:maxDedupSourceBytes]
		}
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	write(m.Role)
	write(m.ToolCallID)
	write(m.ToolName)
	write(m.ToolArgs)
	write(m.Content)
	write(strconv.Itoa(len(m.Content)))
	write(strconv.Itoa(len(m.ToolArgs)))
	write(strconv.Itoa(ordinal))
	return hex.EncodeToString(h.Sum(nil))
}

// AssignDedupKeys fills the DedupKey of every message that has none, using
// position-among-identical-siblings as the disambiguator.
//
// Exported because the caller that builds the batch (the WS layer) is the only
// one that knows the batch is complete, and because a caller that supplies its
// own keys — a replay, a fork — must be able to keep them. Messages that
// already carry a key are left untouched.
func AssignDedupKeys(msgs []Message) {
	seen := make(map[string]int, len(msgs))
	for i := range msgs {
		if msgs[i].DedupKey != "" {
			continue
		}
		base := dedupKeyFor(msgs[i], 0)
		n := seen[base]
		seen[base] = n + 1
		if n == 0 {
			msgs[i].DedupKey = base
		} else {
			msgs[i].DedupKey = dedupKeyFor(msgs[i], n)
		}
	}
}

// AppendMessages persists a batch of messages atomically and idempotently.
//
// Semantics that callers depend on:
//   - ALL-OR-NOTHING. One transaction. A failure on the last row rolls back the
//     first, so a caller that treats a non-nil error as "nothing is durable" is
//     correct. This is what makes "persist before evict, do not evict on write
//     failure" implementable: a partial write would leave the caller unable to
//     tell which messages it is still holding.
//   - IDEMPOTENT. Rows whose (session_id, dedup_key) already exists are skipped
//     via ON CONFLICT DO NOTHING. Re-appending a window is a successful no-op.
//   - SEQ IS ASSIGNED HERE, from MAX(seq)+1 read inside the same transaction.
//     Caller-supplied Seq values are ignored for that reason.
//
// Content and ToolArgs are redacted before they reach SQLite (same rule as
// AppendMessage). Role, ToolName and ToolCallID are structural identifiers, not
// user text, and are stored verbatim.
//
// Returns the number of rows actually inserted (duplicates excluded) and the
// session's next free sequence number after the batch.
//
// The predicate on the ON CONFLICT clause,
//
//	WHERE dedup_key <> ''
//
// is not decoration: SQLite resolves a conflict target against an index, and
// the dedup index is partial, so omitting the predicate makes the target match
// no index at all and every insert fails with "ON CONFLICT clause does not
// match any PRIMARY KEY or UNIQUE constraint". AssignDedupKeys guarantees the
// predicate holds for every row this function writes.
func (s *Store) AppendMessages(sessionID string, msgs []Message) (inserted int, nextSeq int, err error) {
	if sessionID == "" {
		return 0, 0, fmt.Errorf("store: append messages: empty session id")
	}
	batch := make([]Message, len(msgs))
	copy(batch, msgs)
	AssignDedupKeys(batch)

	now := time.Now().Unix()
	err = s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		var maxSeq sql.NullInt64
		if e := tx.QueryRow(
			"SELECT MAX(seq) FROM messages WHERE session_id = ?", sessionID,
		).Scan(&maxSeq); e != nil {
			return fmt.Errorf("store: read message watermark: %w", e)
		}
		seq := 0
		if maxSeq.Valid {
			seq = int(maxSeq.Int64) + 1
		}
		for _, m := range batch {
			res, e := tx.Exec(
				`INSERT INTO messages
				   (id, session_id, seq, role, content, tool_call_id, tool_name, tool_args, dedup_key, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT(session_id, dedup_key) WHERE dedup_key <> '' DO NOTHING`,
				newID(), sessionID, seq, m.Role, s.redact(m.Content),
				m.ToolCallID, m.ToolName, s.redact(m.ToolArgs), m.DedupKey, now,
			)
			if e != nil {
				return fmt.Errorf("store: append message seq=%d: %w", seq, e)
			}
			n, e := res.RowsAffected()
			if e != nil {
				return fmt.Errorf("store: append message rows: %w", e)
			}
			if n == 0 {
				continue // already durable; keep seq for the next new row
			}
			inserted++
			seq++
		}
		nextSeq = seq
		if _, e := tx.Exec("UPDATE sessions SET updated_at = ? WHERE id = ?", now, sessionID); e != nil {
			return fmt.Errorf("store: touch session: %w", e)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return inserted, nextSeq, nil
}

// MessageRange selects a half-open sequence window plus a hard row cap.
//
// The cap is not a convenience: a session that has been running for hours holds
// more text than any context window, and Messages() returning all of it is how
// a recall tool turns into an out-of-memory. FromSeq/ToSeq are inclusive/
// exclusive respectively; ToSeq <= 0 means "to the end".
type MessageRange struct {
	SessionID string
	FromSeq   int
	ToSeq     int // exclusive; <= 0 means unbounded
	Limit     int // <= 0 applies DefaultMessagePageSize
	// Newest, when true, returns the LAST Limit rows of the range instead of
	// the first. Results are always ordered by ascending seq either way — only
	// which end gets truncated changes.
	Newest bool
}

// DefaultMessagePageSize bounds an unspecified MessageRange.Limit.
const DefaultMessagePageSize = 50

// MaxMessagePageSize bounds an over-specified MessageRange.Limit, so a caller
// (or a model choosing a tool argument) cannot ask for the whole log.
const MaxMessagePageSize = 500

func clampLimit(n int) int {
	if n <= 0 {
		return DefaultMessagePageSize
	}
	if n > MaxMessagePageSize {
		return MaxMessagePageSize
	}
	return n
}

// MessagesPage returns one bounded page of a session's durable log.
//
// This is the paging counterpart to Messages(), which returns everything and is
// kept for the restore paths that genuinely need the whole session. Anything
// driven by a model — a recall tool, a search result expansion — must come
// through here, because the whole point of the durable log is that it is bigger
// than the window that can hold it.
func (s *Store) MessagesPage(r MessageRange) ([]Message, error) {
	if r.SessionID == "" {
		return nil, fmt.Errorf("store: messages page: empty session id")
	}
	limit := clampLimit(r.Limit)
	var q strings.Builder
	q.WriteString("SELECT " + messageColumns + " FROM messages WHERE session_id = ?")
	args := []any{r.SessionID}
	if r.FromSeq > 0 {
		q.WriteString(" AND seq >= ?")
		args = append(args, r.FromSeq)
	}
	if r.ToSeq > 0 {
		q.WriteString(" AND seq < ?")
		args = append(args, r.ToSeq)
	}
	if r.Newest {
		q.WriteString(" ORDER BY seq DESC LIMIT ?")
	} else {
		q.WriteString(" ORDER BY seq ASC LIMIT ?")
	}
	args = append(args, limit)

	rows, err := s.DB.Query(q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if r.Newest {
		// The DESC query selected the correct rows; the caller always wants
		// them in conversation order.
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

// MessageSearchHit is one full-text match with enough context to page around it.
type MessageSearchHit struct {
	Message
	// Snippet is the FTS5-generated excerpt with the matched terms marked by
	// "«" / "»". It is what a recall tool should show; Content may be megabytes.
	Snippet string
}

// SearchMessages runs an FTS5 query over the durable log.
//
// sessionID scopes the search to one conversation. An EMPTY sessionID means
// "every conversation on the box" rather than an error — this is what makes
// "how did we fix that bug last week" answerable at all, since the caller
// asking that question does not know, and should not have to know, which past
// session the fix lives in. Scoping is still available (and is what
// history_search uses, per historySessionID's doc comment) for the
// authorisation-sensitive case: a model must not be able to read a session it
// was never attached to just by guessing its id.
//
// The search does NOT skip messages a compaction has since hidden from any
// live context window (see ProjectWindow). That is deliberate, not an
// oversight: the entire point of cross-session recall is finding text that
// compaction summarised away, so filtering by what is currently "visible"
// would make the feature unable to do the one thing it exists for.
//
// Both the prose (content) and the tool arguments (tool_args) are indexed, so
// "the command that failed" is findable by the path it touched and not only by
// the words in the error.
//
// Ranking is relevance first (FTS5's bm25-derived rank), recency second: rows
// tied on rank are ordered by created_at DESC, then by seq DESC as a final,
// deterministic tiebreak for rows that share a created_at second (Unix()
// resolution is one second, and a batch append can write many rows in one).
// A weighted blend of the two signals was considered and rejected — there is
// no calibration data to derive weights from, and two-level ordering is the
// simplest shape that is still fully explainable.
func (s *Store) SearchMessages(sessionID, query string, limit int) ([]MessageSearchHit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("store: search messages: empty query")
	}
	if hasCJK(query) {
		return s.searchMessagesCJK(sessionID, query, limit)
	}
	var q strings.Builder
	q.WriteString(`SELECT ` + prefixed(messageColumns, "m.") + `,
	        snippet(messages_fts, -1, '«', '»', ' … ', 24)
	 FROM messages_fts f
	 JOIN messages m ON m.rowid = f.rowid
	 WHERE messages_fts MATCH ?`)
	args := []any{query}
	if sessionID != "" {
		q.WriteString(" AND m.session_id = ?")
		args = append(args, sessionID)
	}
	q.WriteString(" ORDER BY rank, m.created_at DESC, m.seq DESC LIMIT ?")
	args = append(args, clampLimit(limit))

	rows, err := s.DB.Query(q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageSearchHit
	for rows.Next() {
		var h MessageSearchHit
		if err := rows.Scan(scanTargets(&h.Message, &h.Snippet)...); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// searchMessagesCJK is the CJK fallback path for SearchMessages.
//
// Why it is needed: messages_fts uses tokenize='porter unicode61', which does
// not segment Chinese words, so an entire sentence collapses into a single
// token. Measured on a real store: "截止日期" / "项目" / "周二" / "张伟" all
// return ZERO hits against a message containing every one of them — only the
// whole sentence matches. That makes history_search / SearchMemory /
// memory_autorecall dead in Chinese all at once, and Chinese is this repo's
// own working language.
//
// Why LIKE instead of swapping the tokenizer: switching to trigram would
// require rebuilding both FTS tables (a risky one-shot migration) and would
// cost English porter stemming. The LIKE fallback is purely additive,
// reversible, and zero-impact on the English path — English queries never
// reach this function (see hasCJK).
//
// The cost is explicit: no bm25 ranking, so results are ordered by recency
// (newest first — see the sessionID paragraph below for the exact columns);
// no FTS5 snippet(), so cjkSnippet builds one by hand. Both beat the status
// quo of zero hits. maxCJKFallbackRows bounds the scan on top of ORDER BY,
// because LIKE '%…%' is a full table scan.
//
// query is FTS5 MATCH syntax, same as the non-CJK path — history_search's own
// error message tells the model to "use double quotes for phrases, OR / NOT
// for boolean terms", so a CJK history_search query can arrive quoted and
// OR'd exactly like memory_autorecall's. parseFTSTerms recovers the terms and
// likeAnyTermClause matches a row if any of them is found in either column,
// which is the LIKE-side equivalent of MATCH's OR.
//
// sessionID is optional here for the same reason it is optional in
// SearchMessages: an empty value means "every session", which this path must
// honour rather than silently staying single-session, or a Chinese
// cross-session query would return zero hits while its English sibling
// worked. Ordering is by created_at DESC (this path has no bm25 rank to sort
// by first) with seq DESC as a tiebreak for rows sharing a created_at second;
// within one session that reproduces the plain seq-DESC order this path used
// before cross-session search existed, since seq only ever increases with
// created_at.
func (s *Store) searchMessagesCJK(sessionID, query string, limit int) ([]MessageSearchHit, error) {
	terms := parseFTSTerms(query)
	clause, args := likeAnyTermClause([]string{"m.content", "m.tool_args"}, terms)
	n := clampLimit(limit)
	if n > maxCJKFallbackRows {
		n = maxCJKFallbackRows
	}
	var q strings.Builder
	q.WriteString(`SELECT ` + prefixed(messageColumns, "m.") + `
	 FROM messages m
	 WHERE (` + clause + `)`)
	all := args
	if sessionID != "" {
		q.WriteString(" AND m.session_id = ?")
		all = append(all, sessionID)
	}
	q.WriteString(" ORDER BY m.created_at DESC, m.seq DESC LIMIT ?")
	all = append(all, n)

	rows, err := s.DB.Query(q.String(), all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageSearchHit
	for rows.Next() {
		var h MessageSearchHit
		if err := rows.Scan(scanTargets(&h.Message)...); err != nil {
			return nil, err
		}
		h.Snippet = snippetForTerms(h.Content, terms)
		out = append(out, h)
	}
	return out, rows.Err()
}

// messageColumns is the canonical SELECT list for the durable log. Every reader
// goes through it and through scanMessages, so a new column cannot be added to
// one query and forgotten in another.
const messageColumns = "id, session_id, seq, role, content, tool_call_id, tool_name, tool_args, dedup_key, created_at"

// prefixed qualifies every column in a comma-separated list with a table alias.
func prefixed(cols, alias string) string {
	parts := strings.Split(cols, ", ")
	for i, c := range parts {
		parts[i] = alias + c
	}
	return strings.Join(parts, ", ")
}

// scanTargets returns the Scan destinations for messageColumns, plus any extra
// trailing destinations the query appended.
func scanTargets(m *Message, extra ...any) []any {
	base := []any{
		&m.ID, &m.SessionID, &m.Seq, &m.Role, &m.Content,
		&m.ToolCallID, &m.ToolName, &m.ToolArgs, &m.DedupKey, &m.CreatedAt,
	}
	return append(base, extra...)
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(scanTargets(&m)...); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
