package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5))

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
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventUndo, 0))

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
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 3))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 7))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventUndo, 0))

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
	require.NoError(t, s.AppendContextEvent(sid, ContextEventUndo, 0))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventUndo, 0))

	hidden, err := s.HiddenSeq(sid)
	require.NoError(t, err)
	assert.Equal(t, 0, hidden)

	// And a later compact still works: the stack was emptied, not corrupted.
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 2))
	hidden, err = s.HiddenSeq(sid)
	require.NoError(t, err)
	assert.Equal(t, 2, hidden)
}

// TestAppendContextEvent_RejectsBadInput: the kind set is closed. An unknown
// kind is skipped by the fold, so accepting it would let a caller record a
// boundary that never moves and report success.
func TestAppendContextEvent_RejectsBadInput(t *testing.T) {
	s, sid := projectionFixture(t, 2)

	require.Error(t, s.AppendContextEvent(sid, "checkpoint", 1))
	require.Error(t, s.AppendContextEvent(sid, "", 1))
	require.Error(t, s.AppendContextEvent("", ContextEventCompact, 1))
	require.Error(t, s.AppendContextEvent(sid, ContextEventCompact, -1))

	events, err := s.ContextEvents(sid)
	require.NoError(t, err)
	assert.Empty(t, events, "a rejected event must not reach the table")
}

// TestContextEvents_ReturnsTheLogInInsertionOrder: the fold's correctness rests
// on the order, and created_at (one-second resolution) cannot supply it.
func TestContextEvents_ReturnsTheLogInInsertionOrder(t *testing.T) {
	s, sid := projectionFixture(t, 4)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 1))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 2))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventUndo, 0))

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

	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 4))

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
// It parses this package's non-test sources and fails on any UPDATE or DELETE
// aimed at context_events, wherever the statement is spelled — including one
// built by string concatenation, since it inspects the source text of every
// string literal rather than a formatted query. The rule matters because the
// obvious "fix" for a wrong boundary is to correct the row, and the moment that
// happens the log stops being a record of what happened: concurrent connections
// overwrite each other, and undo has nothing to pop.
//
// There is deliberately no DeleteSession cascade to exempt. See the DDL comment
// in store.go for why orphan events are the cheaper side of that trade. Note
// also that ARCHIVING a session is not deleting it: a later work package moves
// cold message rows out of the hot table, and it must leave these events alone
// or the archived session's window silently reverts to its raw transcript.
func TestContextEventsTableIsAppendOnly(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fset := token.NewFileSet()
	var scanned int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		scanned++
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			sql := strings.Join(strings.Fields(strings.ToUpper(lit.Value)), " ")
			for _, forbidden := range []string{"UPDATE CONTEXT_EVENTS", "DELETE FROM CONTEXT_EVENTS"} {
				if strings.Contains(sql, forbidden) {
					t.Errorf("%s: %q mutates context_events; ADR-0015 constraint 1 makes "+
						"this table INSERT-only — express the change by appending an event",
						fset.Position(lit.Pos()), forbidden)
				}
			}
			return true
		})
	}
	require.Greater(t, scanned, 1, "the scan found no sources; it would pass vacuously")
}
