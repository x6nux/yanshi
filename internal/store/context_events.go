// internal/store/context_events.go
//
// INF3 (ADR-0015): the active context window as a projection over an
// append-only event log.
//
// Before this file the window was a lossy single copy. C1 made the durable log
// complete — every message the model saw is written before anything is evicted
// — but nobody read that log through a projection: the restore paths issued a
// flat "SELECT … ORDER BY seq ASC", which returns the pre-compaction originals
// AND the summary that replaced them. Measured on the production path: 11
// messages before compaction, 4 after, 11 again after a restore. The window
// came back LARGER than it went in, the summary was paid for and thrown away,
// and the next turn compacted the same history again.
//
// The fix is not to mutate the log. context_events records WHERE the boundary
// is; ProjectWindow reads the log from there. That keeps three things true at
// once: the originals stay recoverable (history_search still finds them),
// undoing a compaction is an append rather than a rewrite, and a session with
// no events projects through the exact statement it always did.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Context event kinds. The set is closed: AppendContextEvent rejects anything
// else, because an unrecognised kind would be silently ignored by the fold in
// HiddenSeq and a boundary that does not move is indistinguishable from one
// that was never written.
const (
	// ContextEventCompact marks a compaction boundary: rows with
	// seq < HiddenSeq no longer enter the active window.
	ContextEventCompact = "compact"
	// ContextEventUndo pops the most recent compaction boundary, restoring the
	// window to what it was before that compaction. The compact event itself is
	// left in place — this log is never rewritten.
	ContextEventUndo = "undo"
)

// ContextEvent is one row of the append-only context log.
//
// HiddenSeq is meaningful for ContextEventCompact only; ContextEventUndo
// carries the caller's value but the fold ignores it, since "undo" means "go
// back to the previous boundary", not "go to this boundary".
type ContextEvent struct {
	ID        int64
	SessionID string
	Kind      string
	HiddenSeq int
	CreatedAt int64
}

// AppendContextEvent records one context event. It is the ONLY writer of this
// table, and it only ever INSERTs — ADR-0015's first constraint. Corrections
// and rollbacks are expressed by appending a further event, so the log is a
// record of what happened rather than of what someone last believed.
//
// An unknown kind is an error rather than a no-op: the fold would skip it, the
// boundary would silently stay put, and the caller would go on believing it had
// recorded a compaction that it had not.
func (s *Store) AppendContextEvent(sessionID, kind string, hiddenSeq int) error {
	if sessionID == "" {
		return fmt.Errorf("store: append context event: empty session id")
	}
	if kind != ContextEventCompact && kind != ContextEventUndo {
		return fmt.Errorf("store: append context event: unknown kind %q", kind)
	}
	if hiddenSeq < 0 {
		return fmt.Errorf("store: append context event: negative hidden seq %d", hiddenSeq)
	}
	now := time.Now().Unix()
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO context_events (session_id, kind, hidden_seq, created_at)
			 VALUES (?, ?, ?, ?)`,
			sessionID, kind, hiddenSeq, now,
		)
		return err
	})
}

// ContextEvents returns a session's context log in insertion order.
//
// Ordered by id, not created_at: created_at has one-second resolution and two
// compactions of a fast session land in the same second, which would make the
// fold order — and therefore the current boundary — depend on how SQLite
// happened to return the rows.
func (s *Store) ContextEvents(sessionID string) ([]ContextEvent, error) {
	rows, err := s.DB.Query(
		`SELECT id, session_id, kind, hidden_seq, created_at
		   FROM context_events WHERE session_id = ? ORDER BY id ASC`,
		sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContextEvent
	for rows.Next() {
		var e ContextEvent
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Kind, &e.HiddenSeq, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// HiddenSeq folds the event log into the session's current window boundary:
// rows with seq below it are superseded and do not enter the window. Zero means
// "no boundary", i.e. the whole log is the window.
//
// The fold is a stack rather than a last-write-wins scan so that undo is
// LAYERED. Two successive compactions push two boundaries; one undo must return
// the window to what the first compaction left, not all the way to the raw
// history. An undo on an empty stack is silently ignored — "undo past the
// beginning" is a request for the original transcript, which is what an empty
// stack already means, so erroring would only turn a harmless click into a
// failure.
func (s *Store) HiddenSeq(sessionID string) (int, error) {
	events, err := s.ContextEvents(sessionID)
	if err != nil {
		return 0, err
	}
	var stack []int
	for _, e := range events {
		switch e.Kind {
		case ContextEventCompact:
			stack = append(stack, e.HiddenSeq)
		case ContextEventUndo:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if len(stack) == 0 {
		return 0, nil
	}
	return stack[len(stack)-1], nil
}

// ProjectWindow rebuilds a session's active context window from the durable log
// plus its context events. It is what the restore paths must read instead of
// Messages, which returns the whole transcript and therefore undoes every
// compaction the session ever ran.
//
// Two steps, deliberately kept apart rather than folded into one join: the
// boundary is derived first, the messages are fetched second. The archival work
// package will need to satisfy the second half from a different source (rows
// moved into a blob) for cold sessions, and a single clever statement would
// have to be taken apart again to get there.
//
// Cost is a range scan on idx_messages_session bounded by the WINDOW, not by
// the session's total length — which is what W-D-05 asks for, without a
// separate reverse-scan implementation.
func (s *Store) ProjectWindow(sessionID string) ([]Message, error) {
	hidden, err := s.HiddenSeq(sessionID)
	if err != nil {
		return nil, err
	}
	return s.messagesFromSeq(sessionID, hidden)
}
