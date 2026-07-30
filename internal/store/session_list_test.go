package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListSessions_OrderAndCount(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id1, err := s.CreateSession("first")
	require.NoError(t, err)

	time.Sleep(time.Second) // ensure updated_at differs

	id2, err := s.CreateSession("second")
	require.NoError(t, err)

	// Touch the first session so it becomes most-recently-updated.
	require.NoError(t, s.AppendMessage(id1, 0, "user", "hello"))

	list, err := s.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// Most-recently-updated first → id1 was touched last.
	assert.Equal(t, id1, list[0].ID)
	assert.Equal(t, "first", list[0].Title)
	assert.Equal(t, id2, list[1].ID)

	// Limit works.
	one, err := s.ListSessions(1)
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, id1, one[0].ID)
}

// TestListSessions_ZeroSessions proves a fresh store lists nothing.
func TestListSessions_ZeroSessions(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	list, err := s.ListSessions(0)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// TestGetSession_Missing proves GetSession on an absent id returns (nil, nil)
// with no error (session_list.go:102 — the missing case simply returns nil, nil).
func TestGetSession_Missing(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	got, err := s.GetSession("nope")
	assert.NoError(t, err) // actual code returns (nil, nil) on missing
	assert.Nil(t, got)
}

// TestListArchivedSessions_RoundTrip proves archiving hides from ListSessions
// and surfaces in ListArchivedSessions; unarchive reverses it.
func TestListArchivedSessions_RoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("to-archive")
	require.NoError(t, err)

	// Initially: active list has it, archived list empty.
	active, err := s.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, active, 1)
	archived, err := s.ListArchivedSessions(0)
	require.NoError(t, err)
	assert.Empty(t, archived)

	// Archive: moves between lists.
	require.NoError(t, s.SetSessionArchived(id, true))
	active, err = s.ListSessions(0)
	require.NoError(t, err)
	assert.Empty(t, active)
	archived, err = s.ListArchivedSessions(0)
	require.NoError(t, err)
	require.Len(t, archived, 1)
	assert.Equal(t, id, archived[0].ID)

	// Unarchive: reverses.
	require.NoError(t, s.SetSessionArchived(id, false))
	active, err = s.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, active, 1)
	archived, err = s.ListArchivedSessions(0)
	require.NoError(t, err)
	assert.Empty(t, archived)
}
