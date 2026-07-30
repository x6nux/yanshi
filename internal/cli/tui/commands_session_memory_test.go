package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
)

// TestCommand_Memory_SendsGetStatus proves /memory only sends get_status.
func TestCommand_Memory_SendsGetStatus(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/memory")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "get_status", rec.frames[0].Type)
}

// TestApplyStatus_PopulatesMemoryPath proves a status reply's MemoryPath is
// written to m.memoryPath.
func TestApplyStatus_PopulatesMemoryPath(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind:       "status",
		Model:      "x",
		MemoryPath: "/home/u/.yanshi/memory.md",
	})
	assert.Equal(t, "/home/u/.yanshi/memory.md", m.memoryPath)
}

// TestApplyStatus_PopulatesSessionID proves a status reply syncs sessionID.
func TestApplyStatus_PopulatesSessionID(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind:      "status",
		SessionID: "sess-abc",
	})
	assert.Equal(t, "sess-abc", m.sessionID)
}

// FN5: restore has its own applyEvent branch that does NOT go through
// applyStatus; m.sessionID must be synced explicitly. Pre-seed a stale value
// so a missed assignment guarantees test failure.
func TestApplyEvent_SessionRestoredSyncsSessionID(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.sessionID = "stale-session"
	m = m.applyEvent(cli.StreamEvent{
		Kind:      "session_restored",
		SessionID: "restored-session",
		Model:     "x",
	})
	assert.Equal(t, "restored-session", m.sessionID)
}

// TestCommand_Fork_DefaultSeqAll proves /fork with no args sends
// fork_session{seq:-1}.
func TestCommand_Fork_DefaultSeqAll(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/fork")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "fork_session", rec.frames[0].Type)
	assert.Equal(t, -1, rec.frames[0].Seq)
}

// TestCommand_Fork_ParsesSeq proves /fork 5 sends fork_session{seq:5}.
func TestCommand_Fork_ParsesSeq(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/fork 5")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "fork_session", rec.frames[0].Type)
	assert.Equal(t, 5, rec.frames[0].Seq)
}

// TestCommand_Fork_RejectsInvalidInput proves both non-numeric input and
// values below -1 are rejected locally, matching the store/WS GB5 contract.
func TestCommand_Fork_RejectsInvalidInput(t *testing.T) {
	for _, input := range []string{"/fork abc", "/fork -2"} {
		rec := &recordingSession{}
		m := newModel(rec, "/proj")
		mm, _ := m.runCommand(input)
		m = mm.(model)
		assert.Empty(t, rec.frames, "invalid args must not send a frame: %s", input)
	}
}

// TestApplyEvent_SessionForked_UpdatesSessionID proves a session_forked frame
// updates m.sessionID.
func TestApplyEvent_SessionForked_UpdatesSessionID(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind:      "session_forked",
		SessionID: "fork-xyz",
	})
	assert.Equal(t, "fork-xyz", m.sessionID)
	ack, ok := m.entries[len(m.entries)-1].(ackEntry)
	require.True(t, ok)
	assert.Contains(t, ack.text, "switched")
	assert.NotContains(t, ack.text, "/restore", "server already switched before ack")
}

// TestCommand_Side_SendsEnterSide proves /side sends an enter_side frame.
func TestCommand_Side_SendsEnterSide(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/side")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "enter_side", rec.frames[0].Type)
}

// TestCommand_BTW_AliasForSide proves /btw is an alias for /side.
func TestCommand_BTW_AliasForSide(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/btw")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "enter_side", rec.frames[0].Type)
}

// TestCommand_Main_SendsExitSide proves /main sends an exit_side frame.
func TestCommand_Main_SendsExitSide(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/main")
	_ = mm.(model)
	require.Len(t, rec.frames, 1)
	assert.Equal(t, "exit_side", rec.frames[0].Type)
}

// TestCommand_Main_RejectsKeep proves the explicit scope cut is fail-closed:
// `/main keep` must NOT silently discard side history.
func TestCommand_Main_RejectsKeep(t *testing.T) {
	rec := &recordingSession{}
	m := newModel(rec, "/proj")
	mm, _ := m.runCommand("/main keep")
	m = mm.(model)
	assert.Empty(t, rec.frames)
	_, ok := m.entries[len(m.entries)-1].(errorEntry)
	assert.True(t, ok)
}

// TestApplyEvent_SideState_UpdatesSideDepth proves a side_state frame updates
// m.sideDepth.
func TestApplyEvent_SideState_UpdatesSideDepth(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m = m.applyEvent(cli.StreamEvent{
		Kind:      "side_state",
		SideDepth: 1,
	})
	assert.Equal(t, 1, m.sideDepth)
}
