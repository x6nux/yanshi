package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectionFixture writes n plain messages to a fresh session and returns the
// store and the session id. Kept tiny on purpose: every assertion below is
// about which rows come back, so the rows themselves only need to be
// distinguishable.
func projectionFixture(t *testing.T, n int) (*Store, string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ce.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	sid, err := s.CreateSession("projection")
	require.NoError(t, err)
	msgs := make([]Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, Message{Role: RoleUser, Content: "message " + string(rune('a'+i))})
	}
	_, _, err = s.AppendMessages(sid, msgs)
	require.NoError(t, err)
	return s, sid
}

// TestProjectWindow_NoEventsIsIdenticalToMessages is ADR-0015's second
// constraint as a resident regression. Old sessions are the overwhelming
// majority; a projection that differs from Messages for them has not fixed a
// bug, it has handed every existing user a different history. Whole-slice
// equality, not a length check — a projection that returned the right NUMBER of
// different rows would pass that and be just as wrong.
func TestProjectWindow_NoEventsIsIdenticalToMessages(t *testing.T) {
	s, sid := projectionFixture(t, 10)

	all, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, all, 10)

	window, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	require.Equal(t, all, window)

	hidden, err := s.HiddenSeq(sid)
	require.NoError(t, err)
	assert.Equal(t, 0, hidden, "a session with no events has no boundary")
}

// TestProjectWindow_CompactHidesEverythingBelowTheBoundary is the fix itself:
// the rows are still in the log, they simply stop entering the window.
func TestProjectWindow_CompactHidesEverythingBelowTheBoundary(t *testing.T) {
	s, sid := projectionFixture(t, 10)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5, nil))

	window, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	require.Len(t, window, 5)
	for i, m := range window {
		assert.Equal(t, i+5, m.Seq)
	}

	// Superseded is not deleted: the originals stay recoverable.
	all, err := s.Messages(sid)
	require.NoError(t, err)
	assert.Len(t, all, 10)
}

// TestProjectWindow_UndoRestoresTheFullTranscript: reverting a compaction is an
// append. After it the projection must be indistinguishable from the pre-event
// read, because "undo past the last boundary" means the raw transcript.
func TestProjectWindow_UndoRestoresTheFullTranscript(t *testing.T) {
	s, sid := projectionFixture(t, 10)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5, nil))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventUndo, 0, nil))

	all, err := s.Messages(sid)
	require.NoError(t, err)
	window, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	require.Equal(t, all, window)
}

// TestHiddenSeq_UndoPopsOneLayerNotAll: two compactions and one undo must land
// on the FIRST boundary. A last-write-wins implementation would jump straight
// back to the raw history and silently re-expand a window the user only asked
// to step back once.
func TestHiddenSeq_UndoPopsOneLayerNotAll(t *testing.T) {
	s, sid := projectionFixture(t, 10)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 3, nil))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 7, nil))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventUndo, 0, nil))

	hidden, err := s.HiddenSeq(sid)
	require.NoError(t, err)
	assert.Equal(t, 3, hidden)

	window, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	require.Len(t, window, 7)
	assert.Equal(t, 3, window[0].Seq)
}

// TestHiddenSeq_UndoOnEmptyStackIsSilent: undoing past the beginning asks for
// the original transcript, which is what an empty stack already yields.
func TestHiddenSeq_UndoOnEmptyStackIsSilent(t *testing.T) {
	s, sid := projectionFixture(t, 4)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventUndo, 0, nil))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventUndo, 0, nil))

	hidden, err := s.HiddenSeq(sid)
	require.NoError(t, err)
	assert.Equal(t, 0, hidden)

	// And a later compact still works: the stack was emptied, not corrupted.
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 2, nil))
	hidden, err = s.HiddenSeq(sid)
	require.NoError(t, err)
	assert.Equal(t, 2, hidden)
}

// TestAppendContextEvent_RejectsBadInput: the kind set is closed. An unknown
// kind is skipped by the fold, so accepting it would let a caller record a
// boundary that never moves and report success.
func TestAppendContextEvent_RejectsBadInput(t *testing.T) {
	s, sid := projectionFixture(t, 2)

	require.Error(t, s.AppendContextEvent(sid, "checkpoint", 1, nil))
	require.Error(t, s.AppendContextEvent(sid, "", 1, nil))
	require.Error(t, s.AppendContextEvent("", ContextEventCompact, 1, nil))
	require.Error(t, s.AppendContextEvent(sid, ContextEventCompact, -1, nil))

	events, err := s.ContextEvents(sid)
	require.NoError(t, err)
	assert.Empty(t, events, "a rejected event must not reach the table")
}

// TestContextEvents_ReturnsTheLogInInsertionOrder: the fold's correctness rests
// on the order, and created_at (one-second resolution) cannot supply it.
func TestContextEvents_ReturnsTheLogInInsertionOrder(t *testing.T) {
	s, sid := projectionFixture(t, 4)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 1, nil))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 2, nil))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventUndo, 0, nil))

	events, err := s.ContextEvents(sid)
	require.NoError(t, err)
	require.Len(t, events, 3)
	assert.Equal(t, []string{ContextEventCompact, ContextEventCompact, ContextEventUndo},
		[]string{events[0].Kind, events[1].Kind, events[2].Kind})
	assert.Equal(t, []int{1, 2, 0},
		[]int{events[0].HiddenSeq, events[1].HiddenSeq, events[2].HiddenSeq})
	assert.Less(t, events[0].ID, events[1].ID)
	assert.Less(t, events[1].ID, events[2].ID)
	for _, e := range events {
		assert.Equal(t, sid, e.SessionID)
		assert.NotZero(t, e.CreatedAt)
	}
}

// TestProjectWindow_IsScopedToOneSession: hidden_seq is per session, and the
// index is (session_id, id). A fold that ignored the scope would let one
// conversation's compaction truncate another's window.
func TestProjectWindow_IsScopedToOneSession(t *testing.T) {
	s, sid := projectionFixture(t, 6)
	other, err := s.CreateSession("other")
	require.NoError(t, err)
	_, _, err = s.AppendMessages(other, []Message{{Role: RoleUser, Content: "elsewhere"}})
	require.NoError(t, err)

	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 4, nil))

	window, err := s.ProjectWindow(other)
	require.NoError(t, err)
	require.Len(t, window, 1, "another session's boundary must not apply here")

	hidden, err := s.HiddenSeq(other)
	require.NoError(t, err)
	assert.Equal(t, 0, hidden)
}

// TestContextEventsTableIsAppendOnly is ADR-0015's first constraint, enforced
// by machine rather than by review.
//
// It fails on any UPDATE or DELETE aimed at context_events, wherever the
// statement is spelled — including one built by string concatenation, since it
// inspects the source text of every string literal rather than a formatted
// query. The rule matters because the obvious "fix" for a wrong boundary is to
// correct the row, and the moment that happens the log stops being a record of
// what happened: concurrent connections overwrite each other, and undo has
// nothing to pop.
//
// IT SCANS ALL OF internal/ AND cmd/, NOT JUST THIS PACKAGE. Store.DB is an
// EXPORTED field and several packages already reach through it to run their own
// statements (internal/vcs does, for other tables), so a package-local scan
// leaves the constraint enforced exactly where it is least likely to be broken.
// The wider scan is also the machine enforcement ADR-0015's compensation rule
// has otherwise lacked: a future cold-archive path that moves message rows out
// from another package is now at least visible to this gate if it tries to
// clear the events instead of compensating for them.
//
// Quoting is normalised away. SQLite accepts "context_events", `context_events`
// and [context_events], and a check that only matched the bare identifier would
// be passed by the first thing anyone tries when a plain DELETE gets rejected.
//
// There is deliberately no DeleteSession cascade to exempt. See the DDL comment
// in store.go for why orphan events are the cheaper side of that trade. Note
// also that ARCHIVING a session is not deleting it: a later work package moves
// cold message rows out of the hot table, and it must compensate the boundary
// (see undoBoundariesAtOrAfterTx) rather than delete these rows.
func TestContextEventsTableIsAppendOnly(t *testing.T) {
	fset := token.NewFileSet()
	var scanned int
	for _, root := range []string{"..", filepath.Join("..", "..", "cmd")} {
		require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				// A concurrently-edited file elsewhere in the tree must not turn
				// this gate into a flake about someone else's work in progress.
				t.Logf("skipping unparseable %s: %v", path, perr)
				return nil
			}
			scanned++
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				sql := normalizeSQL(lit.Value)
				for _, forbidden := range []string{"UPDATE CONTEXT_EVENTS", "DELETE FROM CONTEXT_EVENTS"} {
					if strings.Contains(sql, forbidden) {
						t.Errorf("%s: %q mutates context_events; ADR-0015 constraint 1 makes "+
							"this table INSERT-only — express the change by appending an event",
							fset.Position(lit.Pos()), forbidden)
					}
				}
				return true
			})
			return nil
		}))
	}
	require.Greater(t, scanned, 100, "the scan found almost no sources; it would pass vacuously")
}

// normalizeSQL folds a Go string literal into a single upper-case line with
// SQLite's three identifier quotings stripped, so whitespace and quoting cannot
// hide a statement from the scan above.
func normalizeSQL(lit string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case '"', '`', '[', ']':
			return -1
		}
		return r
	}, lit)
	return strings.Join(strings.Fields(strings.ToUpper(cleaned)), " ")
}

// ---------------------------------------------------------------------------
// INF3 round 2: pinned survivors, and compensating for rows that go away
// ---------------------------------------------------------------------------

// TestProjectWindow_PinsSurviveBelowTheBoundary is ADR-0015 constraint 5(a).
//
// A compacted window is a set WITH HOLES: ctxcompact.Plan pins the user's
// opening request wherever it sits, and everything between it and the kept tail
// gets evicted. hidden_seq alone can only name a suffix, so the first cut of
// this design silently dropped that request — the model came back from a restore
// not knowing what it had been asked to do.
func TestProjectWindow_PinsSurviveBelowTheBoundary(t *testing.T) {
	s, sid := projectionFixture(t, 10)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 8, []int{0, 3}))

	window, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	seqs := make([]int, 0, len(window))
	for _, m := range window {
		seqs = append(seqs, m.Seq)
	}
	assert.Equal(t, []int{0, 3, 8, 9}, seqs,
		"the window is the kept tail UNION the pins, in log order")
}

// TestProjectWindow_EmptyPinsDegradeToThePlainRange: the union is additive, so
// a boundary with no pins has to behave exactly like one from before the column
// existed — and must not build "seq IN ()", which SQLite rejects outright.
func TestProjectWindow_EmptyPinsDegradeToThePlainRange(t *testing.T) {
	s, sid := projectionFixture(t, 10)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 6, nil))

	window, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	require.Len(t, window, 4)
	assert.Equal(t, 6, window[0].Seq)

	events, err := s.ContextEvents(sid)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Nil(t, events[0].PinnedSeqs, "no pins must round-trip as no pins, not as []")
}

// TestAppendContextEvent_PinsAreStoredAsASet: sorted and de-duplicated, so the
// stored value is a function of the SET and an equality assertion cannot pass or
// fail on the order the caller happened to collect them in.
func TestAppendContextEvent_PinsAreStoredAsASet(t *testing.T) {
	s, sid := projectionFixture(t, 10)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 9, []int{5, 1, 5, 3, 1}))

	events, err := s.ContextEvents(sid)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, []int{1, 3, 5}, events[0].PinnedSeqs)
}

// TestHiddenSeq_UndoRestoresThePreviousPins: the pins belong to the event, so
// popping a layer has to bring the previous layer's pins back with it. Clearing
// them instead would leave the window at the older boundary but missing the
// messages that boundary depended on.
func TestHiddenSeq_UndoRestoresThePreviousPins(t *testing.T) {
	s, sid := projectionFixture(t, 12)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 6, []int{0}))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 10, []int{0, 6}))

	window, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 6, 10, 11}, seqsOf(window))

	require.NoError(t, s.AppendContextEvent(sid, ContextEventUndo, 0, nil))
	window, err = s.ProjectWindow(sid)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 6, 7, 8, 9, 10, 11}, seqsOf(window),
		"undo must restore the earlier boundary AND its pin")
}

func seqsOf(msgs []Message) []int {
	out := make([]int, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Seq)
	}
	return out
}

// TestTruncateForRevertCompensatesTheBoundary is the CRITICAL regression.
//
// /restore-turn deletes messages at or above a seq. Before this was wired it did
// not touch context_events, so a compacted session kept a boundary pointing past
// the end of the surviving log and the projection selected ZERO rows. Measured:
// hidden_seq 9, two rows left, window empty. That is not a smaller context — it
// is an agent that has forgotten the entire conversation and reports nothing
// wrong. It was strictly worse than the bug this whole change set fixes, where
// the context merely came back too large.
//
// The compensation is an APPEND (constraint 1 forbids editing events) and it
// runs inside the truncation's own transaction, so the deletion and the undo
// cannot come apart.
func TestTruncateForRevertCompensatesTheBoundary(t *testing.T) {
	s, sid := projectionFixture(t, 12)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 9, []int{0}))

	_, err := s.TruncateSessionForRevert(sid, 2, 1)
	require.NoError(t, err)

	window, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	all, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, all, 2, "the revert kept the first two rows")
	assert.Equal(t, all, window,
		"after a revert the window must be the surviving transcript, not empty")

	hidden, err := s.HiddenSeq(sid)
	require.NoError(t, err)
	assert.Equal(t, 0, hidden, "a boundary past the end of the log must have been undone")
}

// TestTruncateForRevertKeepsBoundariesItDidNotInvalidate: the compensation pops
// only the layers that described deleted rows. A revert above every boundary
// must leave compaction intact, or every undo would silently re-expand the whole
// history and charge for the summary again on the next turn.
func TestTruncateForRevertKeepsBoundariesItDidNotInvalidate(t *testing.T) {
	s, sid := projectionFixture(t, 12)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 4, []int{0}))

	_, err := s.TruncateSessionForRevert(sid, 9, 1)
	require.NoError(t, err)

	hidden, err := s.HiddenSeq(sid)
	require.NoError(t, err)
	assert.Equal(t, 4, hidden, "this boundary still points inside the surviving log")

	window, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 4, 5, 6, 7, 8}, seqsOf(window))
}

// TestTruncateForRevertPopsEveryInvalidatedLayer: two stacked compactions, one
// revert below both. Popping a single layer would leave the older boundary also
// pointing past the end.
func TestTruncateForRevertPopsEveryInvalidatedLayer(t *testing.T) {
	s, sid := projectionFixture(t, 12)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5, nil))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 9, nil))

	_, err := s.TruncateSessionForRevert(sid, 3, 1)
	require.NoError(t, err)

	hidden, err := s.HiddenSeq(sid)
	require.NoError(t, err)
	assert.Equal(t, 0, hidden)
	window, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 1, 2}, seqsOf(window))
}

// TestProjectWindow_BackstopNeverReturnsAnEmptyWindow covers the SECOND layer of
// the fix above, by forging the state the first layer exists to prevent.
//
// This is a backstop, not the mechanism — a path that removes rows still has to
// append its own compensation, or every restore in between hands back the whole
// transcript. It is here because the failure it catches is silent and total.
func TestProjectWindow_BackstopNeverReturnsAnEmptyWindow(t *testing.T) {
	s, sid := projectionFixture(t, 4)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 999, nil))

	window, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	all, err := s.Messages(sid)
	require.NoError(t, err)
	assert.Equal(t, all, window,
		"a boundary past the end of the log must degrade to the full transcript, "+
			"never to an empty context")
}

// TestForkInheritsTheCompactionBoundary: a fork branches the CONVERSATION, so it
// inherits the conversation's state, and after a compaction that state is the
// compacted window.
//
// The alternative — starting the fork from the raw transcript — was the status
// quo, and it is this change set's own bug reached through a different door: the
// new session opens with every original the summary already replaced, and the
// first turn pays for that summary a second time. Nothing is lost by
// inheriting; the fork copies the whole log either way, and an undo on the fork
// still reaches the full transcript.
func TestForkInheritsTheCompactionBoundary(t *testing.T) {
	s, sid := projectionFixture(t, 10)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 8, []int{0}))

	forkID, err := s.ForkSession(sid, -1)
	require.NoError(t, err)

	all, err := s.Messages(forkID)
	require.NoError(t, err)
	require.Len(t, all, 10, "the fork still copies the whole log")

	window, err := s.ProjectWindow(forkID)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 8, 9}, seqsOf(window),
		"the fork opens on the window its source had, not on the raw transcript")

	// The source is untouched, and an undo on the fork reaches everything it
	// copied — a new branch's undo should not have to walk another branch's
	// compaction history.
	require.NoError(t, s.AppendContextEvent(forkID, ContextEventUndo, 0, nil))
	window, err = s.ProjectWindow(forkID)
	require.NoError(t, err)
	assert.Equal(t, all, window)
	srcHidden, err := s.HiddenSeq(sid)
	require.NoError(t, err)
	assert.Equal(t, 8, srcHidden)
}

// TestForkBelowTheBoundaryInheritsNothing: forking from a point that predates
// the compaction copies none of the kept tail, so inheriting the boundary would
// select nothing. The fork point is older than the compaction, and the honest
// window there is everything it copied.
func TestForkBelowTheBoundaryInheritsNothing(t *testing.T) {
	s, sid := projectionFixture(t, 10)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 8, []int{0}))

	forkID, err := s.ForkSession(sid, 4)
	require.NoError(t, err)

	all, err := s.Messages(forkID)
	require.NoError(t, err)
	require.Len(t, all, 5)
	window, err := s.ProjectWindow(forkID)
	require.NoError(t, err)
	assert.Equal(t, all, window)
}

// TestContextEventsUpgradeAddsPinnedSeqs: pinned_seqs arrived AFTER
// context_events shipped, so every database the first round touched already has
// the table — and CREATE TABLE IF NOT EXISTS skips it. Without the
// addColumnIfMissing line in migrate() the column would exist only on fresh
// installs and every upgraded database would fail on the first projection.
//
// The fixture builds the pre-column table by hand rather than checking out the
// old code, so it keeps working when nothing remembers what the old DDL was.
func TestContextEventsUpgradeAddsPinnedSeqs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	old, err := Open(path)
	require.NoError(t, err)
	_, err = old.DB.Exec("DROP TABLE context_events")
	require.NoError(t, err)
	_, err = old.DB.Exec(`CREATE TABLE context_events (
	    id          INTEGER PRIMARY KEY AUTOINCREMENT,
	    session_id  TEXT    NOT NULL,
	    kind        TEXT    NOT NULL,
	    hidden_seq  INTEGER NOT NULL,
	    created_at  INTEGER NOT NULL)`)
	require.NoError(t, err)
	_, err = old.DB.Exec(
		"INSERT INTO context_events (session_id, kind, hidden_seq, created_at) VALUES ('s1','compact',5,1)")
	require.NoError(t, err)
	require.NoError(t, old.Close())

	upgraded, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = upgraded.Close() })

	events, err := upgraded.ContextEvents("s1")
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, 5, events[0].HiddenSeq)
	assert.Nil(t, events[0].PinnedSeqs,
		"a pre-column row must read back as 'no pins', which is what it meant")
}
