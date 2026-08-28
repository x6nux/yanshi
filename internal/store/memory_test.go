package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTracedSession lays down a six-row log, compacts it at seq 3, and returns
// the session id. Shared by the two provenance tests so "before archival" and
// "after archival" are demonstrably the same starting state.
func seedTracedSession(t *testing.T, s *Store) string {
	t.Helper()
	sid, err := s.CreateSession("provenance")
	require.NoError(t, err)
	for i := 0; i < 6; i++ {
		require.NoError(t, s.AppendMessage(sid, i, "user", "line "+string(rune('a'+i))))
	}
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 3, nil))
	return sid
}

// TestMemory_TracesBackToSourceLog: every memory written with a session records
// the log position it came from, and resolving it returns that slice.
//
// W-D-07 clause 1. The negative half is in the same test on purpose: a
// provenance API that answered "here are some messages" for a row nobody
// recorded an origin for would satisfy the positive assertion while telling the
// caller something untrue.
func TestMemory_TracesBackToSourceLog(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid := seedTracedSession(t, s)

	id, err := s.WriteMemoryFromSession("note", "tabs, not spaces", MemoryFilter{SessionID: sid})
	require.NoError(t, err)

	src, err := s.MemorySource(id)
	require.NoError(t, err)
	require.Len(t, src, 3, "the source is the window the writer saw, not the whole log")
	assert.Equal(t, 3, src[0].Seq)
	assert.Equal(t, 5, src[2].Seq)

	// A memory written without a session records nothing and says so, rather
	// than resolving to an unrelated slice.
	plain, err := s.WriteMemory("note", "no session here")
	require.NoError(t, err)
	_, err = s.MemorySource(plain)
	assert.ErrorIs(t, err, ErrNoMemorySource)

	// So does a pre-W-D-07 row: WriteMemoryScoped carries the dimension but no
	// origin, which is exactly the shape every existing row upgrades into.
	scoped, err := s.WriteMemoryScoped("note", "scoped but untraced", MemoryFilter{SessionID: sid})
	require.NoError(t, err)
	_, err = s.MemorySource(scoped)
	assert.ErrorIs(t, err, ErrNoMemorySource)

	_, err = s.MemorySource("nope")
	assert.Error(t, err, "an unknown id is an error, not an empty source")
}

// TestMemory_TraceResolvesAfterArchive: W-D-07 clause 2. The archival is the
// REAL one (CompressSession, W-D-04) rather than a fabricated flag, because the
// failure this guards against is precisely that the rows stop being where a
// private SELECT would look for them.
func TestMemory_TraceResolvesAfterArchive(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	sid := seedTracedSession(t, s)
	id, err := s.WriteMemoryFromSession("note", "tabs, not spaces", MemoryFilter{SessionID: sid})
	require.NoError(t, err)

	before, err := s.MemorySource(id)
	require.NoError(t, err)

	packed, err := s.CompressSession(sid)
	require.NoError(t, err)
	require.Equal(t, 6, packed, "the archive must actually have run")
	n, err := s.SessionMessageCount(sid)
	require.NoError(t, err)
	require.Zero(t, n, "the rows must really be gone, or the test proves nothing")

	after, err := s.MemorySource(id)
	require.NoError(t, err)
	assert.Equal(t, before, after, "provenance must survive archival byte for byte")
}

// TestClearMemories_ScopedByDimension: W-D-12's "optionally clear by dimension".
// The zero filter clearing everything is asserted last, because it is the
// behaviour the confirmation gate upstream exists for.
func TestClearMemories_ScopedByDimension(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	write := func(content string, dims MemoryFilter) string {
		id, err := s.WriteMemoryScoped("note", content, dims)
		require.NoError(t, err)
		return id
	}
	write("session a", MemoryFilter{SessionID: "a"})
	write("session b", MemoryFilter{SessionID: "b"})
	write("agent x", MemoryFilter{AgentID: "x"})
	unscoped := write("no dimensions at all", MemoryFilter{})

	// A superseded row must go too: it is invisible to a default search, so
	// leaving it behind would make "cleared" mean "hidden".
	dead := write("superseded", MemoryFilter{SessionID: "a"})
	require.NoError(t, s.WriteTx(t.Context(), func(tx *sql.Tx) error {
		_, e := tx.Exec("UPDATE memories SET superseded_by = 'later' WHERE id = ?", dead)
		return e
	}))

	n, err := s.ClearMemories(MemoryFilter{SessionID: "a"})
	require.NoError(t, err)
	assert.Equal(t, 2, n, "both session-a rows, including the superseded one")

	left, err := s.RecallMemoryScoped(50, MemoryFilter{IncludeSuperseded: true})
	require.NoError(t, err)
	assert.Len(t, left, 3, "the other dimensions are untouched")

	n, err = s.ClearMemories(MemoryFilter{AgentID: "x"})
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	n, err = s.ClearMemories(MemoryFilter{})
	require.NoError(t, err)
	assert.Equal(t, 2, n, "an empty filter clears everything that is left")

	_, err = s.MemoryUseCount(unscoped)
	assert.Error(t, err, "the unscoped row is gone with the rest")

	// The FTS shadow table has to have followed the base rows out; otherwise a
	// search still returns the cleared content.
	hits, err := s.SearchMemory("session dimensions superseded agent", 50)
	require.NoError(t, err)
	assert.Empty(t, hits, "a cleared memory must not come back from the index")
}

func TestMemory_WriteSearchRecall(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.WriteMemory("note", "The user prefers tabs over spaces for Go.")
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	got, err := s.SearchMemory("tabs spaces go", 5)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Contains(t, got[0].Content, "tabs")

	all, err := s.RecallMemory(10)
	require.NoError(t, err)
	require.Len(t, all, 1)
}

func TestMemory_SearchNoMatch(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	_, err = s.WriteMemory("note", "hello world")
	require.NoError(t, err)

	got, err := s.SearchMemory("xyzzy", 5)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestMemory_RecallOrdersNewestFirstLimit proves RecallMemory returns rows
// newest-first and honors limit. It writes 3 memories with distinct
// created_at, then asserts RecallMemory(2) returns exactly the 2 newest.
func TestMemory_RecallOrdersNewestFirstLimit(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	// created_at comes from time.Now().Unix(); space writes apart so the
	// ordering is unambiguous even at 1s resolution.
	contents := []string{"oldest", "middle", "newest"}
	for _, c := range contents {
		_, err := s.WriteMemory("note", c)
		require.NoError(t, err)
		time.Sleep(1100 * time.Millisecond)
	}

	got, err := s.RecallMemory(2)
	require.NoError(t, err)
	require.Len(t, got, 2, "limit must be honored")
	assert.Equal(t, "newest", got[0].Content, "newest first")
	assert.Equal(t, "middle", got[1].Content)

	all, err := s.RecallMemory(0) // limit<=0 → default 10
	require.NoError(t, err)
	require.Len(t, all, 3, "limit<=0 must fall back to default and return all")
}

// TestMemory_SearchMatchesMultipleTerms proves FTS5 MATCH finds a memory by
// any indexed word and returns newest-first.
func TestMemory_SearchMatchesMultipleTerms(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	_, err = s.WriteMemory("pref", "The user prefers tabs over spaces for Go.")
	require.NoError(t, err)

	for _, q := range []string{"tabs", "spaces", "go", "user prefers"} {
		got, err := s.SearchMemory(q, 5)
		require.NoError(t, err)
		require.Lenf(t, got, 1, "query %q must match the memory", q)
		assert.Contains(t, got[0].Content, "tabs")
	}
}

// TestMemory_WriteReturnsID proves WriteMemory returns a non-empty id that is
// stable across reads (the id is the row primary key).
func TestMemory_WriteReturnsID(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.WriteMemory("note", "x")
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	// Two writes produce distinct ids.
	id2, err := s.WriteMemory("note", "y")
	require.NoError(t, err)
	assert.NotEqual(t, id, id2)
}
