package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/store"
)

func newMemTools(t *testing.T) (*store.Store, context.Context) {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	prof := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"memory_*"}}}
	return s, WithProfile(context.Background(), prof)
}

func TestMemoryTool_WriteSearch(t *testing.T) {
	s, ctx := newMemTools(t)
	mt := NewMemoryTools(s)

	// Write a memory.
	out, err := runTool(ctx, mt.Write, `{"content":"user likes Go","kind":"pref"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "Stored as")
	assert.Contains(t, out, "[pref]")

	// Search should find it.
	out, err = runTool(ctx, mt.Search, `{"query":"Go","limit":5}`)
	require.NoError(t, err)
	assert.Contains(t, out, "user likes Go")
}

func TestMemoryTool_Recall(t *testing.T) {
	s, ctx := newMemTools(t)
	mt := NewMemoryTools(s)

	// Write two memories.
	_, err := runTool(ctx, mt.Write, `{"content":"first memory","kind":"note"}`)
	require.NoError(t, err)
	_, err = runTool(ctx, mt.Write, `{"content":"second memory","kind":"note"}`)
	require.NoError(t, err)

	// Recall returns recent memories in human-readable format.
	out, err := runTool(ctx, mt.Recall, `{"limit":10}`)
	require.NoError(t, err)
	assert.Contains(t, out, "first memory")
	assert.Contains(t, out, "second memory")
	assert.Contains(t, out, "[note]")
	// Must NOT be JSON (no leading [).
	assert.NotContains(t, out, `[{"ID":"`)
}

func TestMemoryTool_DenyOutOfProfile(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })

	// Profile allows only "other.*" — memory tools should be denied.
	prof := guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"other.*"}}}
	ctx := WithProfile(context.Background(), prof)

	mt := NewMemoryTools(s)
	out, err := runTool(ctx, mt.Search, `{"query":"x"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

// TestMemoryTool_SourceTracesAMemoryBackToItsLog is W-D-07's READ side, which
// had no production caller at all: source_session_id / source_seq were written
// by both writers and readable only from _test.go, so "every memory traces back
// to the log position that produced it" was a column, not a feature.
//
// It goes through the tool the model actually calls, with the memory id taken
// from memory_recall's own output — the two halves of the trace, since a listing
// that withheld the id would leave the tool unreachable for anything the caller
// did not write itself this turn.
func TestMemoryTool_SourceTracesAMemoryBackToItsLog(t *testing.T) {
	s, ctx := newMemTools(t)
	sid, err := s.CreateSession("traced")
	require.NoError(t, err)
	_, _, err = s.AppendMessages(sid, []store.Message{
		{Role: store.RoleUser, Content: "we always deploy on Tuesday"},
		{Role: store.RoleAssistant, Content: "noted"},
	})
	require.NoError(t, err)

	id, err := s.WriteMemoryFromSession("note", "deploys happen on Tuesday",
		store.MemoryFilter{SessionID: sid})
	require.NoError(t, err)

	mt := NewMemoryTools(s)

	listed, err := runTool(ctx, mt.Recall, `{"limit":5}`)
	require.NoError(t, err)
	require.Contains(t, listed, "id="+id,
		"the listing must name the id memory_source takes, or the trace is unreachable")

	out, err := runTool(ctx, mt.Source, `{"id":"`+id+`"}`)
	require.NoError(t, err)
	assert.Contains(t, out, sid)
	assert.Contains(t, out, "we always deploy on Tuesday",
		"the source slice must be the conversation the note came from")

	// The archived case is the one the resolver was built for: the rows have
	// been deleted and live in a gzip blob, and a private SELECT would report
	// "the source is gone" instead of "the source moved".
	packed, err := s.CompressSession(sid)
	require.NoError(t, err)
	require.Equal(t, 2, packed)
	out, err = runTool(ctx, mt.Source, `{"id":"`+id+`"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "we always deploy on Tuesday")
}

// TestMemoryTool_SourceKeepsTheTwoAbsencesApart: store.ErrNoMemorySource
// distinguishes "nobody wrote down where this came from" (permanent) from "the
// source resolved but its rows are gone" (history was removed). Rendering both
// as one message would throw that distinction away at the only layer a user
// ever sees it.
func TestMemoryTool_SourceKeepsTheTwoAbsencesApart(t *testing.T) {
	s, ctx := newMemTools(t)
	mt := NewMemoryTools(s)

	unscoped, err := s.WriteMemory("note", "written with no conversation")
	require.NoError(t, err)
	out, err := runTool(ctx, mt.Source, `{"id":"`+unscoped+`"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "No source was recorded")

	sid, err := s.CreateSession("emptied")
	require.NoError(t, err)
	_, _, err = s.AppendMessages(sid, []store.Message{{Role: store.RoleUser, Content: "gone soon"}})
	require.NoError(t, err)
	traced, err := s.WriteMemoryFromSession("note", "about that", store.MemoryFilter{SessionID: sid})
	require.NoError(t, err)
	require.NoError(t, s.DeleteSession(sid))

	out, err = runTool(ctx, mt.Source, `{"id":"`+traced+`"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "no messages remain there")

	// A missing id, and a blank one, are FAILURES — neither is a flavour of
	// absence, and reporting them as "no source recorded" would tell the caller
	// a fact about a memory that does not exist.
	out, err = runTool(ctx, mt.Source, `{"id":"no-such-memory"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "not found")
	out, err = runTool(ctx, mt.Source, `{"id":"  "}`)
	require.NoError(t, err)
	assert.Contains(t, out, "id is required")
}
