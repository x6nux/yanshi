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
