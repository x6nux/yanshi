package http

import (
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
)

// newMemClearServer wires a real WS server over st and returns a dialed
// connection. Going over the wire rather than calling handleClearMemories
// directly is deliberate: the dispatch case in ws.go is the half a direct call
// cannot cover, and a frame that is never routed looks exactly like a wipe that
// deleted nothing.
func newMemClearServer(t *testing.T, st *store.Store) *websocket.Conn {
	t.Helper()
	o, err := orchestrator.New(orchestrator.Config{Model: einollm.NewFakeModel([]string{"hi"}, nil)})
	require.NoError(t, err)
	s := New(Config{Token: "t", Store: st})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	c := dial(t, dialWSURL(t, ts))
	t.Cleanup(func() { c.Close() })
	return c
}

// TestClearMemoriesFrame_UnknownScopeDeletesNothing pins the fail-closed half
// of W-D-12's server side.
//
// store.ClearMemories reads a zero MemoryFilter as "delete everything", so the
// only thing standing between an unrecognised scope word and a full wipe is
// handleClearMemories' explicit switch. A default branch that fell through to
// the zero filter would pass every other test in this package.
func TestClearMemoriesFrame_UnknownScopeDeletesNothing(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	for _, content := range []string{"one", "two", "three"} {
		_, err := st.WriteMemoryScoped("note", content, store.MemoryFilter{})
		require.NoError(t, err)
	}

	c := newMemClearServer(t, st)
	require.NoError(t, c.WriteJSON(proto.NewClearMemories("everything", "")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
	assert.Contains(t, f.Text, "unknown scope")

	left, err := st.RecallMemory(10)
	require.NoError(t, err)
	assert.Len(t, left, 3, "an unknown scope must not have cleared anything")
}

// TestClearMemoriesFrame_AllWipesTheStore is the positive control for the test
// above: the same connection, the same shape, one word different, and the rows
// really do go. Without it "nothing was deleted" would also be satisfied by a
// handler that never deletes at all.
func TestClearMemoriesFrame_AllWipesTheStore(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	for _, content := range []string{"one", "two", "three"} {
		_, err := st.WriteMemoryScoped("note", content, store.MemoryFilter{})
		require.NoError(t, err)
	}

	c := newMemClearServer(t, st)
	require.NoError(t, c.WriteJSON(proto.NewClearMemories(proto.MemoryClearAll, "")))
	f := readFrame(t, c)
	require.Equal(t, "memories_cleared", f.Type)
	assert.Contains(t, f.Text, "cleared 3")

	left, err := st.RecallMemory(10)
	require.NoError(t, err)
	assert.Empty(t, left)
}

// TestClearMemoriesFrame_AgentScopeNeedsAnID: a chat connection has no acting
// agent, so an agent-scoped wipe with no id cannot be resolved. Resolving it to
// the empty AgentID would match every row written without one — which is most
// of them — and report success.
func TestClearMemoriesFrame_AgentScopeNeedsAnID(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	_, err = st.WriteMemoryScoped("note", "unscoped", store.MemoryFilter{})
	require.NoError(t, err)
	_, err = st.WriteMemoryScoped("note", "mine", store.MemoryFilter{AgentID: "a1"})
	require.NoError(t, err)

	c := newMemClearServer(t, st)
	require.NoError(t, c.WriteJSON(proto.NewClearMemories(proto.MemoryClearAgent, "")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
	left, err := st.RecallMemory(10)
	require.NoError(t, err)
	assert.Len(t, left, 2)

	require.NoError(t, c.WriteJSON(proto.NewClearMemories(proto.MemoryClearAgent, "a1")))
	f = readFrame(t, c)
	require.Equal(t, "memories_cleared", f.Type)
	left, err = st.RecallMemory(10)
	require.NoError(t, err)
	require.Len(t, left, 1)
	assert.Equal(t, "unscoped", left[0].Content, "only the named agent's row goes")
}

// TestClearMemoriesFrame_NamesTheCopiesItDidNotReach.
//
// "cleared 3 memories" reads as erasure and is not one: W-D-06 gzips the whole
// memories table into every checkpoint, so a cleared memory — a credential
// pasted by mistake, say — is still on disk and comes back verbatim from a plain
// `/checkpoint restore <id> memory yes`. Measured on a memory whose content was
// an API key, in the same batch that wrote the "the bytes stop existing"
// comment this reply now makes honest.
//
// Both directions, because a warning that is always printed stops being read:
// with a checkpoint the reply names it, without one the reply is exactly what it
// always was.
func TestClearMemoriesFrame_NamesTheCopiesItDidNotReach(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	_, err = st.WriteMemoryScoped("note", "MY-API-KEY-sk-secret-12345", store.MemoryFilter{})
	require.NoError(t, err)

	c := newMemClearServer(t, st)
	require.NoError(t, c.WriteJSON(proto.NewClearMemories(proto.MemoryClearAll, "")))
	f := readFrame(t, c)
	require.Equal(t, "memories_cleared", f.Type)
	assert.Contains(t, f.Text, "cleared 1")
	assert.NotContains(t, f.Text, "checkpoint",
		"with no checkpoint there is no surviving copy to warn about")

	// Now with a checkpoint holding a copy.
	id, err := st.WriteMemoryScoped("note", "MY-API-KEY-sk-secret-12345", store.MemoryFilter{})
	require.NoError(t, err)
	cp, err := st.CreateCheckpoint("before the wipe", "", "")
	require.NoError(t, err)

	require.NoError(t, c.WriteJSON(proto.NewClearMemories(proto.MemoryClearAll, "")))
	f = readFrame(t, c)
	require.Equal(t, "memories_cleared", f.Type)
	assert.Contains(t, f.Text, "cleared 1")
	assert.Contains(t, f.Text, "/checkpoint list",
		"the reply must name the copy it left behind and how to reach it")

	// And the copy really is reachable, which is what makes the note true rather
	// than a defensive hedge.
	_, err = st.RestoreCheckpoint(cp.ID, store.CheckpointMemory)
	require.NoError(t, err)
	back, err := st.RecallMemory(10)
	require.NoError(t, err)
	require.Len(t, back, 1)
	assert.Equal(t, id, back[0].ID)
	assert.Equal(t, "MY-API-KEY-sk-secret-12345", back[0].Content)
}
