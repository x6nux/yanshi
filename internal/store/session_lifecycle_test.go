package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSession_ArchiveHidesFromList(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	idActive, err := s.CreateSession("active")
	require.NoError(t, err)
	idArchived, err := s.CreateSession("to-be-archived")
	require.NoError(t, err)

	// Archive one session.
	require.NoError(t, s.SetSessionArchived(idArchived, true))

	active, err := s.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, idActive, active[0].ID)
	assert.False(t, active[0].Archived, "active list must report Archived=false")

	archived, err := s.ListArchivedSessions(0)
	require.NoError(t, err)
	require.Len(t, archived, 1)
	assert.Equal(t, idArchived, archived[0].ID)
	assert.True(t, archived[0].Archived, "archived list must report Archived=true")
}

func TestSession_UnarchiveRestoresToList(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("x")
	require.NoError(t, err)
	require.NoError(t, s.SetSessionArchived(id, true))
	require.NoError(t, s.SetSessionArchived(id, false))

	active, err := s.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, id, active[0].ID)
}

func TestSession_GetSessionReportsArchived(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("x")
	require.NoError(t, err)
	require.NoError(t, s.SetSessionArchived(id, true))

	ss, err := s.GetSession(id)
	require.NoError(t, err)
	require.NotNil(t, ss)
	assert.True(t, ss.Archived)
}

func TestSession_DeleteIsTransactional(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	id, err := s.CreateSession("doomed")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(id, 0, "user", "hi"))
	require.NoError(t, s.AppendMessage(id, 1, "assistant", "hello"))

	require.NoError(t, s.DeleteSession(id))

	// Session row gone.
	ss, err := s.GetSession(id)
	require.NoError(t, err)
	assert.Nil(t, ss, "session row must be deleted")

	// Associated messages gone.
	msgs, err := s.Messages(id)
	require.NoError(t, err)
	assert.Empty(t, msgs, "messages must be cleaned up")

	// Archived list also empty.
	archived, err := s.ListArchivedSessions(0)
	require.NoError(t, err)
	assert.Empty(t, archived)
}
