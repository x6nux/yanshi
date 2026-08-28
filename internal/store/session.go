package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// jsonMarshal is a test-visible indirection over json.Marshal that allows
// tests to inject marshaling failures. Production behavior is identical.
var jsonMarshal = json.Marshal

// Message is a stored chat message belonging to a session.
//
// Role is one of the store.Role* constants. The four C1 fields are empty for
// prose messages and for every row written before the durable log existed:
//   - ToolCallID links a RoleToolResult back to its RoleToolCall.
//   - ToolName / ToolArgs carry the call itself.
//   - DedupKey is the row's stable identity within the session; see
//     AppendMessages for why it exists and store.AssignDedupKeys for how it is
//     derived. Empty means "written by the legacy single-message path", which
//     the partial unique index deliberately exempts from deduplication.
type Message struct {
	ID         string
	SessionID  string
	Seq        int
	Role       string
	Content    string
	ToolCallID string
	ToolName   string
	ToolArgs   string
	DedupKey   string
	CreatedAt  int64
}

// CreateSession inserts a new session and returns its id. The title is
// redacted before persistence so secret substrings never reach SQLite.
func (s *Store) CreateSession(title string) (string, error) {
	id := newID()
	now := time.Now().Unix()
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			"INSERT INTO sessions (id, title, created_at, updated_at) VALUES (?, ?, ?, ?)",
			id, s.redact(title), now, now,
		)
		return e
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// AppendMessage adds a message to a session at the given sequence number.
// The content is redacted before persistence so secret substrings never reach
// SQLite. The role is operator-controlled (user/assistant/tool/system) and
// is not redacted; the session id is server-generated.
// Uses WriteTx for atomicity: the message INSERT and session updated_at UPDATE
// either both succeed or both roll back.
func (s *Store) AppendMessage(sessionID string, seq int, role, content string) error {
	id := newID()
	now := time.Now().Unix()
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		// Same rule as AppendMessages: a compressed session gets its rows back
		// before one is written on top of them (thawColdSessionTx). The caller
		// picks seq here, and SessionMessageCount — where every caller gets it
		// from — counts the archived rows too, so the two halves agree.
		if err := thawColdSessionTx(tx, sessionID); err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO messages (id, session_id, seq, role, content, created_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			id, sessionID, seq, role, s.redact(content), now,
		); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE sessions SET updated_at = ? WHERE id = ?", now, sessionID)
		return err
	})
}

// UpdateSessionTitle updates the title of a session after redaction.
func (s *Store) UpdateSessionTitle(sessionID, title string) error {
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE sessions SET title = ? WHERE id = ?", s.redact(title), sessionID)
		return err
	})
}

// BillingMeta carries the per-session cumulative billable tokens and cost
// produced by einollm.Ledger. CostKnown=false means the session used at least
// one model not in the pricing table; renderers must show N/A, not $0. Pass
// BillingMeta{} when billing is disabled (all fields default to zero, which
// reads back as "no spend recorded").
type BillingMeta struct {
	InputTokens  int
	CachedTokens int
	OutputTokens int
	CostUSD      float64
	CostKnown    bool
}

// UpdateSessionMeta updates the session metadata (model, thinking, token
// counters, turn count). cached and reasoning (Task A6) carry the cumulative
// prompt-cache-hit and reasoning-model-spend totals so /cost can surface
// them; pass 0 when the provider does not report them. billing (C4 COST1)
// carries the cumulative billable ledger; pass BillingMeta{} to leave the
// billed columns unchanged from their default zeros. The billed columns are
// kept SEPARATE from tokens_in/out (which have overwrite semantics for the
// live context-bar display, not running-sum semantics).
func (s *Store) UpdateSessionMeta(sessionID, model, thinking string, tokensIn, tokensOut, turns, cached, reasoning int, billing BillingMeta) error {
	known := 0
	if billing.CostKnown {
		known = 1
	}
	now := time.Now().Unix()
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE sessions SET model = ?, thinking = ?, tokens_in = ?, tokens_out = ?, turns = ?, cached_tokens = ?, reasoning_tokens = ?, "+
				"billed_input_tokens = ?, billed_cached_tokens = ?, billed_output_tokens = ?, cost_usd = ?, cost_known = ?, updated_at = ? WHERE id = ?",
			model, thinking, tokensIn, tokensOut, turns, cached, reasoning,
			billing.InputTokens, billing.CachedTokens, billing.OutputTokens, billing.CostUSD, known,
			now, sessionID,
		)
		return err
	})
}

// SetSessionArchived (V10) hides a session from the active list (archived=true)
// or restores it (archived=false). The session row and its messages are NOT
// deleted — only the archived flag flips, so unarchive is lossless.
func (s *Store) SetSessionArchived(sessionID string, archived bool) error {
	v := 0
	if archived {
		v = 1
	}
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE sessions SET archived = ? WHERE id = ?", v, sessionID)
		return err
	})
}

// DeleteSession deletes a session and all its messages in a single transaction,
// so a crash between the two DELETEs cannot orphan messages under a gone session
// row (the FK on messages.session_id is unenforced in SQLite by default).
func (s *Store) DeleteSession(sessionID string) error {
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM messages WHERE session_id = ?", sessionID); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM sessions WHERE id = ?", sessionID)
		return err
	})
}

// SessionMessageCount returns the number of messages in a session, INCLUDING
// the ones a compression moved into cold storage (W-D-04).
//
// Counting only `messages` reports 0 for every compressed session, and all
// three callers are hurt by that in a different way: the TUI session list shows
// every archived conversation as empty, connSession.loadSession takes it as the
// log coordinate a later compaction boundary is computed from, and api/v1 uses
// it as the seq to write the next turn AT — which lands the new row on top of
// seq 0 of the archived transcript. That last one is a live resume path, not
// the revert machinery an earlier note mistook it for.
//
// The cold half is answered from the stored max_seq rather than by decompressing
// the blob, so listing sessions stays O(1) per row. max_seq+1 is the row count
// because seq is dense: AppendMessages assigns it from MAX(seq)+1 one row at a
// time, and the only deletes are whole sessions and suffixes
// (TruncateSessionForRevert). It is also, exactly, the next free seq — which is
// what two of the three callers actually want from it.
func (s *Store) SessionMessageCount(sessionID string) (int, error) {
	var count int
	if err := s.DB.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE session_id = ?", sessionID,
	).Scan(&count); err != nil {
		return 0, err
	}
	if count > 0 {
		return count, nil
	}
	cold, ok, err := s.coldMaxSeq(sessionID)
	if err != nil {
		return 0, err
	}
	if ok {
		return cold + 1, nil
	}
	return 0, nil
}

// Messages returns ALL of a session's messages ordered by sequence.
//
// Unbounded on purpose: the restore/fork/snapshot paths need the exact whole
// session and nothing less. Anything a MODEL drives — recall, search, history
// paging — must use MessagesPage instead, because the durable log is expected
// to outgrow any context window that could hold it.
func (s *Store) Messages(sessionID string) ([]Message, error) {
	return s.messagesInWindow(sessionID, 0, nil)
}

// messagesInWindow is the shared reader behind Messages and ProjectWindow: the
// kept tail from fromSeq, unioned with the scattered pinned rows below it.
//
// THE DEGENERATE CASES EMIT THE ORIGINAL STATEMENT, not an equivalent one.
// fromSeq <= 0 appends no predicate whatsoever, so a session with no compaction
// boundary does not merely return the same rows as Messages — it runs the
// identical query, and pins are redundant there because the whole log is already
// in the window. An empty pin list likewise falls back to the plain range, which
// is also what stops the builder from emitting "seq IN ()", a SQLite syntax
// error rather than an empty match. ADR-0015's second constraint is therefore a
// property of the code, and there is one place to add a new message column.
//
// A ZERO-ROW RESULT FALLS BACK TO COLD STORAGE (W-D-04). The fallback lives
// here, at the shared reader, rather than in each of Messages / ProjectWindow /
// the fork and snapshot paths, so compression cannot be transparent for one
// caller and invisible for another. filterWindow re-applies the same boundary
// to the decompressed slice — see its doc for why skipping that would undo
// every compaction the cold session ever ran.
func (s *Store) messagesInWindow(sessionID string, fromSeq int, pinned []int) ([]Message, error) {
	q := "SELECT " + messageColumns + " FROM messages WHERE session_id = ?"
	args := []any{sessionID}
	switch {
	case fromSeq > 0 && len(pinned) > 0:
		q += " AND (seq >= ? OR seq IN (" + placeholders(len(pinned)) + "))"
		args = append(args, fromSeq)
		for _, p := range pinned {
			args = append(args, p)
		}
	case fromSeq > 0:
		q += " AND seq >= ?"
		args = append(args, fromSeq)
	}
	q += " ORDER BY seq ASC"
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	msgs, err := scanMessages(rows)
	if err != nil || len(msgs) > 0 {
		return msgs, err
	}
	cold, ok, err := s.coldMessages(sessionID)
	if err != nil || !ok {
		return msgs, err
	}
	return filterWindow(cold, fromSeq, pinned), nil
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SessionRevertSnapshot is the exact durable session state before a rollback
// transition. Besides in-process compensation, it is JSON-encoded into the
// returned pre-revert seam so undo remains possible after reconnect (D2).
type SessionRevertSnapshot struct {
	Meta     SessionSummary
	Messages []Message
	// HiddenSeq / PinnedSeqs capture the context-window boundary (INF3) as it
	// stood when the snapshot was taken, so restoring the rows also restores
	// which of them the model sees. Without them the restore is asymmetric: a
	// truncation pops the boundary by appending undo events, and putting only
	// the rows back leaves those undos standing — the whole transcript returns
	// to the window and the next turn pays for a fresh summary.
	//
	// Zero means "no boundary", which is both the honest reading of a snapshot
	// taken before any compaction and what a pre-INF3 JSON payload decodes to.
	HiddenSeq  int
	PinnedSeqs []int
}

// EncodeSessionRevertSnapshot serializes the exact durable state saved on an
// undo seam. An empty session id is never a valid destructive undo payload.
func EncodeSessionRevertSnapshot(snap SessionRevertSnapshot) ([]byte, error) {
	if snap.Meta.ID == "" {
		return nil, fmt.Errorf("store: empty session revert snapshot")
	}
	blob, err := jsonMarshal(snap)
	if err != nil {
		return nil, fmt.Errorf("store: encode session revert snapshot: %w", err)
	}
	return blob, nil
}

// DecodeSessionRevertSnapshot decodes a durable undo payload fail-closed.
func DecodeSessionRevertSnapshot(blob []byte) (SessionRevertSnapshot, error) {
	if len(blob) == 0 {
		return SessionRevertSnapshot{}, fmt.Errorf("store: seam has no session revert snapshot")
	}
	var snap SessionRevertSnapshot
	if err := json.Unmarshal(blob, &snap); err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: decode session revert snapshot: %w", err)
	}
	if snap.Meta.ID == "" {
		return SessionRevertSnapshot{}, fmt.Errorf(
			"store: decoded session revert snapshot has empty session id")
	}
	return snap, nil
}

// snapshotSessionTx captures one exact session row and all of its messages.
// Both read-only snapshot and truncate reuse it; do not duplicate this query.
func snapshotSessionTx(tx *sql.Tx, sessionID string) (SessionRevertSnapshot, error) {
	var snap SessionRevertSnapshot
	m := &snap.Meta
	var archived, costKnown int
	if err := tx.QueryRow(
		`SELECT id, title, created_at, updated_at, model, thinking,
		        tokens_in, tokens_out, turns, cached_tokens,
		        reasoning_tokens, archived,
		        billed_input_tokens, billed_cached_tokens, billed_output_tokens,
		        cost_usd, cost_known
		 FROM sessions WHERE id=?`, sessionID,
	).Scan(&m.ID, &m.Title, &m.CreatedAt, &m.UpdatedAt, &m.Model,
		&m.Thinking, &m.TokensIn, &m.TokensOut, &m.Turns,
		&m.CachedTokens, &m.ReasoningTokens, &archived,
		&m.BilledInputTokens, &m.BilledCachedTokens, &m.BilledOutputTokens,
		&m.CostUSD, &costKnown); err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: snapshot session: %w", err)
	}
	m.Archived = archived != 0
	m.CostKnown = costKnown != 0
	rows, err := tx.Query(
		`SELECT `+messageColumns+`
		 FROM messages WHERE session_id=? ORDER BY seq ASC`, sessionID)
	if err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: snapshot messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var msg Message
		if err := rows.Scan(scanTargets(&msg)...); err != nil {
			return SessionRevertSnapshot{}, fmt.Errorf("store: scan snapshot message: %w", err)
		}
		snap.Messages = append(snap.Messages, msg)
	}
	if err := rows.Err(); err != nil {
		return SessionRevertSnapshot{}, fmt.Errorf("store: iterate snapshot messages: %w", err)
	}
	b, err := boundaryTx(tx, sessionID)
	if err != nil {
		return SessionRevertSnapshot{}, err
	}
	snap.HiddenSeq, snap.PinnedSeqs = b.HiddenSeq, b.PinnedSeqs
	return snap, nil
}

// SnapshotSessionForRevert returns an exact durable snapshot without mutation.
// The undo path uses it as compensation state before expanding older history.
func (s *Store) SnapshotSessionForRevert(sessionID string) (SessionRevertSnapshot, error) {
	if sessionID == "" {
		return SessionRevertSnapshot{}, fmt.Errorf("store: empty session id")
	}
	var snap SessionRevertSnapshot
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		var e error
		snap, e = snapshotSessionTx(tx, sessionID)
		return e
	})
	if err != nil {
		return SessionRevertSnapshot{}, err
	}
	return snap, nil
}

// TruncateSessionForRevert atomically snapshots a session, deletes messages
// with seq >= fromSeq, and updates turns. Any failure rolls back both changes.
//
// It also compensates the context-event log in the same transaction (INF3).
// Deleting rows a compaction boundary points at leaves that boundary describing
// a position past the end of the log, and the projection then selects ZERO rows:
// the model comes back from the next restore with no conversation at all and no
// error to show for it. Measured before this was wired: hidden_seq 9, two rows
// surviving, window empty. The compensation is an append (constraint 1 forbids
// editing the events), so the revert and the undo are one atomic act.
func (s *Store) TruncateSessionForRevert(
	sessionID string, fromSeq, turns int,
) (SessionRevertSnapshot, error) {
	if sessionID == "" || fromSeq < 0 || turns < 0 {
		return SessionRevertSnapshot{}, fmt.Errorf(
			"store: invalid revert boundary session=%q seq=%d turns=%d",
			sessionID, fromSeq, turns)
	}
	var snap SessionRevertSnapshot
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		var e error
		snap, e = snapshotSessionTx(tx, sessionID)
		if e != nil {
			return e
		}
		if _, e := tx.Exec(
			"DELETE FROM messages WHERE session_id=? AND seq>=?", sessionID, fromSeq,
		); e != nil {
			return fmt.Errorf("store: truncate messages: %w", e)
		}
		if e := undoBoundariesAtOrAfterTx(tx, sessionID, fromSeq); e != nil {
			return e
		}
		res, e := tx.Exec(
			"UPDATE sessions SET turns=?, updated_at=? WHERE id=?",
			turns, time.Now().Unix(), sessionID)
		if e != nil {
			return fmt.Errorf("store: update truncated meta: %w", e)
		}
		n, e := res.RowsAffected()
		if e != nil {
			return fmt.Errorf("store: update truncated meta rows: %w", e)
		}
		if n != 1 {
			return fmt.Errorf(
				"store: update truncated meta affected %d rows", n)
		}
		return nil
	})
	if err != nil {
		return SessionRevertSnapshot{}, err
	}
	return snap, nil
}

// RestoreSessionAfterFailedRevert atomically replaces the session row metadata
// and message set with an exact snapshot. The handler uses it both to compensate
// a failed VCS phase and to expand durable history when restoring a pre-revert
// undo seam. Any failure is fatal and surfaced to the client.
//
// It restores the context-window boundary along with the rows (INF3). Putting
// only the rows back is not a restore: the truncation this compensates already
// popped the boundary by appending undo events, and those events do not go away,
// so the window silently reverted to the full transcript — measured at 4
// messages before a failed revert and 11 after. See store.restoreBoundaryTx.
func (s *Store) RestoreSessionAfterFailedRevert(snap SessionRevertSnapshot) error {
	if snap.Meta.ID == "" {
		return fmt.Errorf("store: empty session compensation snapshot")
	}
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec("DELETE FROM messages WHERE session_id=?", snap.Meta.ID); err != nil {
			return fmt.Errorf("store: clear truncated messages: %w", err)
		}
		for _, msg := range snap.Messages {
			if msg.SessionID != snap.Meta.ID {
				return fmt.Errorf("store: snapshot message %s belongs to %s", msg.ID, msg.SessionID)
			}
			if _, err := tx.Exec(
				`INSERT INTO messages
				   (id, session_id, seq, role, content, tool_call_id, tool_name, tool_args, dedup_key, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				msg.ID, msg.SessionID, msg.Seq, msg.Role, msg.Content,
				msg.ToolCallID, msg.ToolName, msg.ToolArgs, msg.DedupKey, msg.CreatedAt,
			); err != nil {
				return fmt.Errorf("store: restore message %s: %w", msg.ID, err)
			}
		}
		m := snap.Meta
		known := 0
		if m.CostKnown {
			known = 1
		}
		res, err := tx.Exec(
			`UPDATE sessions SET title=?, created_at=?, updated_at=?, model=?, thinking=?,
			 tokens_in=?, tokens_out=?, turns=?, cached_tokens=?, reasoning_tokens=?, archived=?,
			 billed_input_tokens=?, billed_cached_tokens=?, billed_output_tokens=?, cost_usd=?, cost_known=?
			 WHERE id=?`,
			m.Title, m.CreatedAt, m.UpdatedAt, m.Model, m.Thinking, m.TokensIn,
			m.TokensOut, m.Turns, m.CachedTokens, m.ReasoningTokens, m.Archived,
			m.BilledInputTokens, m.BilledCachedTokens, m.BilledOutputTokens, m.CostUSD, known, m.ID)
		if err != nil {
			return fmt.Errorf("store: restore session meta: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: restore session meta rows: %w", err)
		}
		if n != 1 {
			return fmt.Errorf("store: restore session meta affected %d rows", n)
		}
		return restoreBoundaryTx(tx, m.ID, contextBoundary{
			HiddenSeq: snap.HiddenSeq, PinnedSeqs: snap.PinnedSeqs,
		})
	})
}
