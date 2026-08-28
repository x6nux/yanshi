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
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
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
// HiddenSeq and PinnedSeqs are meaningful for ContextEventCompact only;
// ContextEventUndo carries whatever the caller passed but the fold ignores it,
// since "undo" means "go back to the previous boundary", not "go to this one".
type ContextEvent struct {
	ID        int64
	SessionID string
	Kind      string
	// HiddenSeq is the start of the KEPT TAIL, not the whole window. Rows below
	// it enter the window only if PinnedSeqs names them.
	HiddenSeq int
	// PinnedSeqs are the surviving rows BELOW HiddenSeq — the messages
	// ctxcompact.Plan pinned somewhere in the middle of the history. See
	// contextBoundary for why one watermark cannot express them.
	PinnedSeqs []int
	CreatedAt  int64
}

// contextBoundary is one folded entry of the event stack: where the kept tail
// starts, plus the scattered survivors below it.
//
// TWO FIELDS, NOT ONE, AND THE SECOND IS NOT AN OPTIMISATION. The first cut of
// this design carried only a watermark and asserted that a compacted window is
// a suffix of the log. That is false: ctxcompact.Plan pins every user-original
// message wherever it sits, plus anything touching the working set or carrying
// an error/diff marker, so the window is a set WITH HOLES. A watermark alone
// gets it wrong in both directions at once — it drops the user's opening
// request (the thing Plan judged least droppable) and simultaneously readmits a
// message compaction had just paid a model call to replace. It can also cut a
// tool_call away from its tool_result, and providers reject an orphan result
// outright. ADR-0015's fifth constraint exists because of that reversal.
type contextBoundary struct {
	HiddenSeq  int
	PinnedSeqs []int
}

// AppendContextEvent records one context event. It is the ONLY writer of this
// table, and it only ever INSERTs — ADR-0015's first constraint. Corrections
// and rollbacks are expressed by appending a further event, so the log is a
// record of what happened rather than of what someone last believed.
//
// An unknown kind is an error rather than a no-op: the fold would skip it, the
// boundary would silently stay put, and the caller would go on believing it had
// recorded a compaction that it had not.
//
// pinnedSeqs may be nil, which is what every ContextEventUndo passes and what a
// compaction whose window happens to be a clean suffix passes too. It is stored
// as the empty string in that case, not as "[]", so the degenerate row is
// byte-identical to one written before the column existed.
//
// CONCURRENCY: this appends, it never reads-then-writes, so two connections
// compacting the same session cannot corrupt each other — the fold happens at
// READ time and the stack top is simply the last event written. What can
// diverge is the boundary VALUE, because each connection computes it from its
// own in-memory cs.seq. The loser's boundary is off by however many rows the
// other connection flushed in between; both windows stay internally consistent
// and both are re-derived on the next restore. Serialising here would not help
// anyway, since the divergence is in the caller's state, not in this table.
func (s *Store) AppendContextEvent(sessionID, kind string, hiddenSeq int, pinnedSeqs []int) error {
	if sessionID == "" {
		return fmt.Errorf("store: append context event: empty session id")
	}
	if kind != ContextEventCompact && kind != ContextEventUndo {
		return fmt.Errorf("store: append context event: unknown kind %q", kind)
	}
	if hiddenSeq < 0 {
		return fmt.Errorf("store: append context event: negative hidden seq %d", hiddenSeq)
	}
	encoded, err := encodePinnedSeqs(pinnedSeqs)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		return appendContextEventTx(tx, sessionID, kind, hiddenSeq, encoded, now)
	})
}

// appendContextEventTx is the single INSERT, shared by the public writer and by
// the in-transaction compensations (revert, fork) that must append atomically
// with the message rows they are reacting to.
func appendContextEventTx(tx *sql.Tx, sessionID, kind string, hiddenSeq int, pinned string, now int64) error {
	_, err := tx.Exec(
		`INSERT INTO context_events (session_id, kind, hidden_seq, pinned_seqs, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		sessionID, kind, hiddenSeq, pinned, now,
	)
	return err
}

// encodePinnedSeqs renders the pin list for storage: sorted, de-duplicated, and
// empty-as-empty-string. Sorting is not cosmetic — it makes the stored value a
// function of the SET, so an equality assertion in a test cannot pass or fail on
// the order the caller happened to collect them in.
func encodePinnedSeqs(seqs []int) (string, error) {
	if len(seqs) == 0 {
		return "", nil
	}
	uniq := make([]int, 0, len(seqs))
	seen := make(map[int]bool, len(seqs))
	for _, s := range seqs {
		if s < 0 || seen[s] {
			continue
		}
		seen[s] = true
		uniq = append(uniq, s)
	}
	if len(uniq) == 0 {
		return "", nil
	}
	sort.Ints(uniq)
	b, err := json.Marshal(uniq)
	if err != nil {
		return "", fmt.Errorf("store: encode pinned seqs: %w", err)
	}
	return string(b), nil
}

// decodePinnedSeqs is the inverse. A malformed value is treated as "no pins"
// rather than as an error: the pins are an ADDITION to the kept tail, so losing
// them degrades the window to the suffix it would have been under the original
// single-watermark design — smaller than intended, never wrong-in-content, and
// never empty. Failing the whole restore instead would take the session down
// over a column nobody could repair by hand.
func decodePinnedSeqs(raw string) []int {
	if raw == "" {
		return nil
	}
	var out []int
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		slog.Warn("context event has unreadable pinned_seqs; projecting the kept tail only",
			"value", raw, "error", err)
		return nil
	}
	return out
}

// ContextEvents returns a session's context log in insertion order.
//
// Ordered by id, not created_at: created_at has one-second resolution and two
// compactions of a fast session land in the same second, which would make the
// fold order — and therefore the current boundary — depend on how SQLite
// happened to return the rows.
func (s *Store) ContextEvents(sessionID string) ([]ContextEvent, error) {
	rows, err := s.DB.Query(contextEventsQuery, sessionID)
	if err != nil {
		return nil, err
	}
	return scanContextEvents(rows)
}

// contextEventsQuery is the one SELECT behind every reader of this table, so a
// future column cannot be added to the public path and forgotten on the
// in-transaction one.
const contextEventsQuery = `SELECT id, session_id, kind, hidden_seq, pinned_seqs, created_at
	   FROM context_events WHERE session_id = ? ORDER BY id ASC`

func scanContextEvents(rows *sql.Rows) ([]ContextEvent, error) {
	defer rows.Close()
	var out []ContextEvent
	for rows.Next() {
		var e ContextEvent
		var pinned string
		if err := rows.Scan(&e.ID, &e.SessionID, &e.Kind, &e.HiddenSeq, &pinned, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.PinnedSeqs = decodePinnedSeqs(pinned)
		out = append(out, e)
	}
	return out, rows.Err()
}

// foldContextEvents replays the log into the stack of live boundaries.
//
// A stack rather than last-write-wins so that undo is LAYERED. Two successive
// compactions push two boundaries; one undo must return the window to what the
// first compaction left, not all the way to the raw history. An undo on an
// empty stack is silently ignored — "undo past the beginning" asks for the
// original transcript, which is what an empty stack already means, so erroring
// would turn a harmless click into a failure.
func foldContextEvents(events []ContextEvent) []contextBoundary {
	var stack []contextBoundary
	for _, e := range events {
		switch e.Kind {
		case ContextEventCompact:
			stack = append(stack, contextBoundary{HiddenSeq: e.HiddenSeq, PinnedSeqs: e.PinnedSeqs})
		case ContextEventUndo:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return stack
}

// boundary returns the session's current window boundary. The zero value means
// "no boundary", i.e. the whole log is the window.
func (s *Store) boundary(sessionID string) (contextBoundary, error) {
	events, err := s.ContextEvents(sessionID)
	if err != nil {
		return contextBoundary{}, err
	}
	stack := foldContextEvents(events)
	if len(stack) == 0 {
		return contextBoundary{}, nil
	}
	return stack[len(stack)-1], nil
}

// HiddenSeq returns where the current window's KEPT TAIL starts. Zero means the
// whole log is the window.
//
// It deliberately does not surface the pins: a caller that only wants to know
// "has this session been compacted, and from where" gets a number, and a caller
// that wants the window itself should call ProjectWindow rather than reassemble
// it from parts.
func (s *Store) HiddenSeq(sessionID string) (int, error) {
	b, err := s.boundary(sessionID)
	if err != nil {
		return 0, err
	}
	return b.HiddenSeq, nil
}

// maxMessageSeq returns the highest seq in a session's durable log, or -1 for an
// empty session. It is what makes "this boundary describes rows that are no
// longer there" answerable, rather than being inferred from an empty result.
//
// It consults cold storage (W-D-04) when `messages` is empty, because
// compression is a path that removes rows and the backstop below exists for
// exactly those. Reading only `messages` would report -1 for every compressed
// session, and -1 short-circuits the backstop as "empty session, nothing to be
// stale about" — disarming the check on the one shape it was written for. The
// stored max_seq answers this without inflating the blob.
func (s *Store) maxMessageSeq(sessionID string) (int, error) {
	var maxSeq sql.NullInt64
	if err := s.DB.QueryRow(
		"SELECT MAX(seq) FROM messages WHERE session_id = ?", sessionID,
	).Scan(&maxSeq); err != nil {
		return 0, err
	}
	if maxSeq.Valid {
		return int(maxSeq.Int64), nil
	}
	cold, ok, err := s.coldMaxSeq(sessionID)
	if err != nil {
		return 0, err
	}
	if ok {
		return cold, nil
	}
	return -1, nil
}

// placeholders renders n comma-separated SQL parameter markers. Callers must
// guarantee n > 0: "IN ()" is a syntax error in SQLite, so an empty list has to
// be handled by skipping the clause, never by building an empty one.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
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
// separate reverse-scan implementation. The pins add an equality lookup per
// pinned row on the same index.
func (s *Store) ProjectWindow(sessionID string) ([]Message, error) {
	b, err := s.boundary(sessionID)
	if err != nil {
		return nil, err
	}
	msgs, err := s.messagesInWindow(sessionID, b.HiddenSeq, b.PinnedSeqs)
	if err != nil {
		return nil, err
	}
	if b.HiddenSeq == 0 {
		return msgs, nil
	}

	// BACKSTOP, NOT THE MECHANISM. The mechanism is that every path which
	// removes message rows compensates the boundary in the SAME transaction —
	// undoBoundariesAtOrAfterTx on the way down, restoreBoundaryTx on the way
	// back up. This branch exists because the failure it catches is silent: a
	// window that lost its conversation is not a smaller context, it is an agent
	// that has forgotten what it was doing and reports nothing wrong.
	//
	// THE TEST IS "DOES THE BOUNDARY STILL DESCRIBE ROWS THAT EXIST", not "did
	// the query come back empty". Those are different questions and the
	// difference was measured: a boundary can point past the end of the log
	// while ONE pinned row below it survives, and an emptiness test then sees a
	// one-row result, reports nothing, and hands the model a single message out
	// of a conversation.
	//
	// Do NOT read this as "the clamp handles it". A new path that removes rows
	// still has to compensate, or every restore in between gets the whole
	// transcript back and pays for a fresh summary on the next turn.
	maxSeq, err := s.maxMessageSeq(sessionID)
	if err != nil {
		return nil, err
	}
	if maxSeq < 0 {
		return msgs, nil // empty session: nothing to be stale about
	}
	if b.HiddenSeq <= maxSeq {
		// The tail is still inside the log. Individual pins may not be — that
		// costs one message rather than the conversation, so it is reported and
		// not escalated.
		if missing := countMissingPins(b.PinnedSeqs, msgs); missing > 0 {
			slog.Warn("context boundary pins rows that are gone; those messages are missing from the window",
				"session", sessionID, "missing", missing, "hidden_seq", b.HiddenSeq,
				"action", "expected after a history edit; the next compaction rewrites the boundary")
		}
		return msgs, nil
	}
	all, err := s.Messages(sessionID)
	if err != nil {
		return nil, err
	}
	slog.Warn("context boundary points past the end of the log; projecting the full transcript",
		"session", sessionID, "hidden_seq", b.HiddenSeq, "max_seq", maxSeq, "messages", len(all),
		"action", "a path removed message rows without compensating the boundary; "+
			"the window is oversized until the next compaction rewrites it")
	return all, nil
}

// countMissingPins reports how many pinned seqs did not come back in the
// projection, i.e. name rows that no longer exist.
func countMissingPins(pins []int, msgs []Message) int {
	if len(pins) == 0 {
		return 0
	}
	present := make(map[int]bool, len(msgs))
	for _, m := range msgs {
		present[m.Seq] = true
	}
	var missing int
	for _, p := range pins {
		if !present[p] {
			missing++
		}
	}
	return missing
}

// restoreBoundaryTx puts a previously captured boundary back.
//
// It is the PUSH half of undoBoundariesAtOrAfterTx's pop, and the asymmetry is
// what made it necessary: a truncation appends undo events, and a compensating
// restore that only put the message rows back left those undos standing, so the
// boundary stayed at zero and the whole transcript came back into the window —
// this design's original bug, entering through the rollback door. Measured:
// hidden 9 before the failed revert, 0 after, window 4 messages to 11.
//
// Expressed by appending, like everything else here (constraint 1). It compares
// before writing so a compensation that changed nothing appends nothing, which
// keeps the stack depth stable across repeated failures.
func restoreBoundaryTx(tx *sql.Tx, sessionID string, want contextBoundary) error {
	rows, err := tx.Query(contextEventsQuery, sessionID)
	if err != nil {
		return fmt.Errorf("store: read context events: %w", err)
	}
	events, err := scanContextEvents(rows)
	if err != nil {
		return fmt.Errorf("store: scan context events: %w", err)
	}
	stack := foldContextEvents(events)
	var current contextBoundary
	if len(stack) > 0 {
		current = stack[len(stack)-1]
	}
	if current.HiddenSeq == want.HiddenSeq && slices.Equal(current.PinnedSeqs, want.PinnedSeqs) {
		return nil
	}
	now := time.Now().Unix()
	if want.HiddenSeq == 0 {
		// The snapshot predates any compaction: pop back to the raw transcript.
		for i := len(stack); i > 0; i-- {
			if err := appendContextEventTx(tx, sessionID, ContextEventUndo, 0, "", now); err != nil {
				return fmt.Errorf("store: restore context boundary: %w", err)
			}
		}
		return nil
	}
	encoded, err := encodePinnedSeqs(want.PinnedSeqs)
	if err != nil {
		return err
	}
	return appendContextEventTx(tx, sessionID, ContextEventCompact, want.HiddenSeq, encoded, now)
}

// boundaryTx folds the session's live boundary inside a caller's transaction, so
// a snapshot taken before a mutation records the boundary that mutation is about
// to invalidate.
func boundaryTx(tx *sql.Tx, sessionID string) (contextBoundary, error) {
	rows, err := tx.Query(contextEventsQuery, sessionID)
	if err != nil {
		return contextBoundary{}, fmt.Errorf("store: read context events: %w", err)
	}
	events, err := scanContextEvents(rows)
	if err != nil {
		return contextBoundary{}, fmt.Errorf("store: scan context events: %w", err)
	}
	stack := foldContextEvents(events)
	if len(stack) == 0 {
		return contextBoundary{}, nil
	}
	return stack[len(stack)-1], nil
}

// undoBoundariesAtOrAfterTx compensates, by appending, for message rows about to
// disappear at or above fromSeq.
//
// Every live boundary that describes a position inside the doomed range gets an
// undo, popping back to a boundary that still lands inside the surviving log.
// It runs in the caller's transaction so the deletion and the compensation are
// one atomic act: a boundary that outlived the rows it points at selects zero
// rows, and a zero-row window is an agent with no memory of the conversation and
// no error to show for it.
//
// This is the mechanism ADR-0015 requires for the general case — ANY future path
// that moves message rows out of the table (cold-session archival is the one
// already planned) has to call this or do the same thing itself. Appending is
// also the only option available: constraint 1 forbids editing the events.
func undoBoundariesAtOrAfterTx(tx *sql.Tx, sessionID string, fromSeq int) error {
	rows, err := tx.Query(contextEventsQuery, sessionID)
	if err != nil {
		return fmt.Errorf("store: read context events: %w", err)
	}
	events, err := scanContextEvents(rows)
	if err != nil {
		return fmt.Errorf("store: scan context events: %w", err)
	}
	stack := foldContextEvents(events)
	now := time.Now().Unix()
	for len(stack) > 0 && stack[len(stack)-1].HiddenSeq >= fromSeq {
		if err := appendContextEventTx(tx, sessionID, ContextEventUndo, 0, "", now); err != nil {
			return fmt.Errorf("store: undo context boundary: %w", err)
		}
		stack = stack[:len(stack)-1]
	}
	return nil
}

// copyBoundaryTx gives a fork the window its source had at the fork point.
//
// A fork branches the CONVERSATION, so it inherits the conversation's state, and
// after a compaction that state is the compacted window. Starting the fork from
// the raw transcript instead would hand the new session every original the
// summary already replaced — this task's own bug, reached through a different
// door — and the next turn would pay for the same summary again. The fork keeps
// the originals in its copied log either way, so nothing is lost; what is
// inherited is only which of them the model sees first.
//
// The inherited boundary is flattened to ONE event rather than replaying the
// source's history. A fork is a new branch: undoing on it should reach the full
// transcript of what it copied, not walk back through compactions that happened
// on a different line of the conversation.
//
// maxSeq is the highest seq actually copied. A fork taken from BELOW the
// boundary copies none of the kept tail, so the boundary would select nothing;
// inheriting nothing is correct there — the fork point predates the compaction.
func copyBoundaryTx(tx *sql.Tx, srcID, forkID string, maxSeq int) error {
	rows, err := tx.Query(contextEventsQuery, srcID)
	if err != nil {
		return fmt.Errorf("ForkSession: read context events: %w", err)
	}
	events, err := scanContextEvents(rows)
	if err != nil {
		return fmt.Errorf("ForkSession: scan context events: %w", err)
	}
	stack := foldContextEvents(events)
	if len(stack) == 0 {
		return nil
	}
	b := stack[len(stack)-1]
	if b.HiddenSeq == 0 || b.HiddenSeq > maxSeq {
		return nil
	}
	var pins []int
	for _, s := range b.PinnedSeqs {
		if s <= maxSeq {
			pins = append(pins, s)
		}
	}
	encoded, err := encodePinnedSeqs(pins)
	if err != nil {
		return fmt.Errorf("ForkSession: %w", err)
	}
	return appendContextEventTx(tx, forkID, ContextEventCompact, b.HiddenSeq, encoded, time.Now().Unix())
}
