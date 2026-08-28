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

// newCheckpointServer dials a real WS server over st. Over the wire rather than
// calling handleCheckpoint directly, because the dispatch case in ws.go is the
// half a direct call cannot reach — and a frame that is never routed produces
// the same silence as one whose handler did nothing.
func newCheckpointServer(t *testing.T, st *store.Store) *websocket.Conn {
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

// TestCheckpointFrame_CreatePlanRestoreRoundTrip drives the whole /checkpoint
// grammar over the wire and asserts the memory dimension really moves.
//
// The restore is checked against the STORE, not against the reply text: a
// handler that answered "restored" without calling the store would satisfy any
// assertion made on its own message.
func TestCheckpointFrame_CreatePlanRestoreRoundTrip(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	for _, c := range []string{"one", "two"} {
		_, err := st.WriteMemoryScoped("note", c, store.MemoryFilter{})
		require.NoError(t, err)
	}

	c := newCheckpointServer(t, st)

	require.NoError(t, c.WriteJSON(proto.NewCheckpoint(proto.CheckpointCreate, "", "", "before")))
	f := readFrame(t, c)
	require.Equalf(t, "checkpoint_result", f.Type, "got %s: %s", f.Type, f.Text)
	assert.Contains(t, f.Text, "memories 2")

	cps, err := st.Checkpoints(10)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	id := cps[0].ID
	assert.Equal(t, "before", cps[0].Label)

	// Drift the memory table.
	_, err = st.ClearMemories(store.MemoryFilter{})
	require.NoError(t, err)

	require.NoError(t, c.WriteJSON(proto.NewCheckpoint(proto.CheckpointPlan, id, "memory", "")))
	f = readFrame(t, c)
	require.Equalf(t, "checkpoint_result", f.Type, "got %s: %s", f.Type, f.Text)
	assert.Contains(t, f.Text, "0 → 2 rows")
	assert.Contains(t, f.Text, "nothing has been written")

	left, err := st.RecallMemory(10)
	require.NoError(t, err)
	require.Empty(t, left, "the plan must not have restored anything")

	require.NoError(t, c.WriteJSON(proto.NewCheckpoint(proto.CheckpointRestore, id, "memory", "")))
	f = readFrame(t, c)
	require.Equalf(t, "checkpoint_result", f.Type, "got %s: %s", f.Type, f.Text)
	assert.Contains(t, f.Text, "undo with:")

	left, err = st.RecallMemory(10)
	require.NoError(t, err)
	assert.Len(t, left, 2, "the restore really ran")

	require.NoError(t, c.WriteJSON(proto.NewCheckpoint(proto.CheckpointList, "", "", "")))
	f = readFrame(t, c)
	require.Equal(t, "checkpoint_result", f.Type)
	assert.Contains(t, f.Text, id)
}

// TestCheckpointFrame_UnknownActionAndDimensionAreErrors: every action but list
// snapshots or destroys, so an unrecognised word must be refused rather than
// resolved to a default.
func TestCheckpointFrame_UnknownActionAndDimensionAreErrors(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	_, err = st.WriteMemory("note", "keep me")
	require.NoError(t, err)

	c := newCheckpointServer(t, st)
	require.NoError(t, c.WriteJSON(proto.NewCheckpoint(proto.CheckpointCreate, "", "", "cp")))
	require.Equal(t, "checkpoint_result", readFrame(t, c).Type)
	cps, err := st.Checkpoints(10)
	require.NoError(t, err)
	require.Len(t, cps, 1)

	require.NoError(t, c.WriteJSON(proto.NewCheckpoint("wobble", "", "", "")))
	f := readFrame(t, c)
	assert.Equal(t, "error", f.Type)
	assert.Contains(t, f.Text, "unknown action")

	require.NoError(t, c.WriteJSON(
		proto.NewCheckpoint(proto.CheckpointRestore, cps[0].ID, "everything", "")))
	f = readFrame(t, c)
	assert.Equal(t, "error", f.Type)

	left, err := st.RecallMemory(10)
	require.NoError(t, err)
	assert.Len(t, left, 1, "nothing was restored over a bad request")
}

// TestCheckpointFrame_FilesDimensionNeedsARepo: with no repository configured
// the file dimension must say so. Reporting success would tell the user their
// working copy was rolled back when nothing touched it.
func TestCheckpointFrame_FilesDimensionNeedsARepo(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	c := newCheckpointServer(t, st)
	require.NoError(t, c.WriteJSON(proto.NewCheckpoint(proto.CheckpointCreate, "", "", "cp")))
	require.Equal(t, "checkpoint_result", readFrame(t, c).Type)
	cps, err := st.Checkpoints(10)
	require.NoError(t, err)
	require.Len(t, cps, 1)

	for _, action := range []string{proto.CheckpointPlan, proto.CheckpointRestore} {
		require.NoError(t, c.WriteJSON(proto.NewCheckpoint(action, cps[0].ID, "files", "")))
		f := readFrame(t, c)
		assert.Equalf(t, "error", f.Type, "%s must refuse without a repo", action)
		assert.Contains(t, f.Text, "repository")
	}
}

// TestCheckpointDimensionVocabulariesAgree pins the wire words to the storage
// words.
//
// They are declared twice on purpose — proto has no dependencies so the TUI can
// validate a dimension without importing the storage layer — and this package
// is one of the few that can see both. Without this the two lists drift the
// first time a dimension is added: the client keeps refusing a word the server
// understands, or sends one it does not, and neither side reports anything
// wrong.
func TestCheckpointDimensionVocabulariesAgree(t *testing.T) {
	wire := proto.CheckpointDimensions()
	stored := store.CheckpointDimensions()
	require.Len(t, wire, len(stored), "the two dimension lists differ in length")
	for i := range wire {
		assert.Equal(t, string(stored[i]), wire[i], "dimension %d differs", i)
	}
	// And every wire word is one the server's own switch accepts, which the
	// length check alone would not establish.
	assert.Equal(t, string(store.CheckpointSession), proto.CheckpointDimSession)
	assert.Equal(t, string(store.CheckpointMemory), proto.CheckpointDimMemory)
	assert.Equal(t, string(store.CheckpointFiles), proto.CheckpointDimFiles)
}
