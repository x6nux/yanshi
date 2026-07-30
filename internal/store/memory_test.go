package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
