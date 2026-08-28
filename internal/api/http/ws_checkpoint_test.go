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
	return newCheckpointServerWithModel(t, st, einollm.NewFakeModel([]string{"hi"}, nil))
}

// newCheckpointServerWithModel is the same, with the caller's fake — the live
// window is only observable through what the NEXT turn hands the model.
func newCheckpointServerWithModel(t *testing.T, st *store.Store, m *einollm.FakeModel) *websocket.Conn {
	t.Helper()
	o, err := orchestrator.New(orchestrator.Config{Model: m})
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

// TestCheckpointFrame_SessionRestoreReachesTheLiveWindow is the regression for
// a restore that was durable and invisible.
//
// The session dimension moved the stored boundary and left connSession.history
// alone, so the very next turn on the SAME connection still sent the model the
// exchange the user had just rolled back — under a reply saying "session
// restored". The memory and files dimensions do not have this failure mode
// because both are re-read from disk on use; the conversation is an in-memory
// copy.
//
// The assertion is on WHAT THE MODEL WAS HANDED, not on the reply text or the
// store: the reply was already truthful-looking while wrong, and the store was
// already correct. BETATWO is the positive control — without it an empty
// history would satisfy "did not see ALPHAONE" and prove nothing.
//
// Deleting the cs.history assignment in restoreCheckpoint makes this red.
func TestCheckpointFrame_SessionRestoreReachesTheLiveWindow(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	fm := einollm.NewFakeModel([]string{"REPLYONE", "REPLYTWO", "REPLYTHREE"}, nil)
	fm.RecordMessages = true
	c := newCheckpointServerWithModel(t, st, fm)

	turn := func(text string) {
		require.NoError(t, c.WriteJSON(proto.ClientFrame{Type: "user_message", Text: text}))
		for readFrame(t, c).Type != "done" {
		}
	}
	turn("ALPHAONE")
	turn("BETATWO")

	sessions, err := st.ListSessions(10)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	sid := sessions[0].ID
	rows, err := st.Messages(sid)
	require.NoError(t, err)
	require.Len(t, rows, 4, "two turns of prose, one row each")

	// A compaction hides the first exchange; the checkpoint captures that
	// boundary; then the boundary is undone, as a /compact undo would do.
	require.NoError(t, st.AppendContextEvent(sid, store.ContextEventCompact, 2, nil))
	require.NoError(t, c.WriteJSON(proto.NewCheckpoint(proto.CheckpointCreate, "", "", "B")))
	f := readFrame(t, c)
	require.Equalf(t, "checkpoint_result", f.Type, "got %s: %s", f.Type, f.Text)
	cps, err := st.Checkpoints(10)
	require.NoError(t, err)
	require.Len(t, cps, 1)
	require.NoError(t, st.AppendContextEvent(sid, store.ContextEventUndo, 0, nil))

	window, err := st.ProjectWindow(sid)
	require.NoError(t, err)
	require.Len(t, window, 4, "the undo put the whole transcript back in the window")

	require.NoError(t, c.WriteJSON(proto.NewCheckpoint(proto.CheckpointRestore, cps[0].ID, "session", "")))
	f = readFrame(t, c)
	require.Equalf(t, "checkpoint_result", f.Type, "got %s: %s", f.Type, f.Text)

	window, err = st.ProjectWindow(sid)
	require.NoError(t, err)
	require.Len(t, window, 2, "the durable half of the restore")

	turn("GAMMA")
	var sawAlpha, sawBeta bool
	for _, m := range fm.ReceivedMessages {
		switch {
		case m == nil:
		case m.Content == "ALPHAONE":
			sawAlpha = true
		case m.Content == "BETATWO":
			sawBeta = true
		}
	}
	assert.True(t, sawBeta, "the kept tail must still be in the live window")
	assert.False(t, sawAlpha,
		"the live connection kept sending the model an exchange the restore rolled back")
}
