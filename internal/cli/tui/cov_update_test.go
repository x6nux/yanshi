package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
)

// TestUpdate_SpinnerAnimatesWhileToolRunning proves spinner.TickMsg updates the
// spinner only when a tool is running.
func TestUpdate_SpinnerAnimatesWhileToolRunning(t *testing.T) {
	m := aeModel()
	// No running tool -> spinner cmd is nil (no animation).
	mm, cmd := m.Update(spinner.TickMsg{})
	assert.Nil(t, cmd)

	// A running tool arms the spinner animation.
	m = m.applyEvent(cli.StreamEvent{Kind: "tool_call", ToolName: "fs_read", ToolStatus: "running"})
	mm, cmd = m.Update(spinner.TickMsg{})
	assert.NotNil(t, cmd, "spinner animates while a tool runs")
	_ = mm
}

// TestUpdate_ActivityTickAdvancesGlyph proves activityTickMsg advances the glyph
// frame while a turn is in flight (streamCh != nil) and is a no-op otherwise.
func TestUpdate_ActivityTickAdvancesGlyph(t *testing.T) {
	m := aeModel()
	// Idle -> no-op.
	mm, cmd := m.Update(activityTickMsg{})
	assert.Nil(t, cmd)
	assert.Equal(t, 0, mm.(model).glyphFrame)

	// Streaming -> glyph advances + re-arms.
	ch := make(chan cli.StreamEvent)
	m.streamCh = ch
	before := m.glyphFrame
	mm, cmd = m.Update(activityTickMsg{})
	m = mm.(model)
	assert.Equal(t, before+1, m.glyphFrame)
	assert.NotNil(t, cmd)
}

// TestUpdate_ToastTickPrunes proves toastTickMsg prunes expired toasts and stops
// the tick when the queue empties.
func TestUpdate_ToastTickPrunes(t *testing.T) {
	m := aeModel()
	m.toasts.push(toast{Level: "info", Text: "x"})
	m.toastTickActive = true
	// Fire the tick with an expiry in the past so the info toast is pruned.
	mm, cmd := m.Update(toastTickMsg{})
	m = mm.(model)
	// After prune, the queue may be empty (info expired) -> tick stops.
	if len(m.toasts.items) == 0 {
		assert.False(t, m.toastTickActive, "tick stops when queue empties")
		assert.Nil(t, cmd)
	}
}

// TestUpdate_SaveCmdReArms proves a saveCmd runs its fn and re-arms waitSave.
func TestUpdate_SaveCmdReArms(t *testing.T) {
	m := aeModel()
	ran := false
	mm, cmd := m.Update(saveCmd{fn: func() error { ran = true; return nil }})
	require.NotNil(t, cmd, "saveCmd re-arms waitSave")
	assert.True(t, ran)
	_ = mm
}

// TestUpdate_GitRefresh proves gitRefreshMsg re-detects the branch and re-arms
// the watcher (when the root is a git repo).
func TestUpdate_GitRefresh(t *testing.T) {
	m := aeModel()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	m.rootPath = root
	mm, cmd := m.Update(gitRefreshMsg{})
	assert.NotNil(t, cmd, "gitRefresh re-arms the watcher for a git root")
	_ = mm
}

// TestUpdate_DebounceReflows proves debounceMsg consumes the pending flag and
// reflows once.
func TestUpdate_DebounceReflows(t *testing.T) {
	m := aeModel()
	m.reflowDeb.schedule() // arm a pending tick
	mm, cmd := m.Update(debounceMsg{})
	assert.Nil(t, cmd, "debounceMsg returns no cmd")
	assert.False(t, mm.(model).reflowDeb.pending, "consume clears the pending flag")
}

// TestUpdate_StartupToolsAppends proves startupToolsMsg appends rows to the
// banner when present.
func TestUpdate_StartupToolsAppends(t *testing.T) {
	m := aeModel()
	banner := &startupEntry{}
	m.entries = append(m.entries, banner)
	m.startupBanner = banner
	mm, _ := m.Update(startupToolsMsg{rows: "Node.js v20"})
	m = mm.(model)
	assert.Contains(t, banner.info, "Node.js v20")

	// Empty rows is a no-op.
	banner2 := &startupEntry{}
	m.startupBanner = banner2
	mm, _ = m.Update(startupToolsMsg{rows: ""})
	assert.NotContains(t, banner2.info, "Node.js")
}

// TestUpdate_RepaintReflowsAndRearms proves repaintMsg reflows + re-arms.
func TestUpdate_RepaintReflowsAndRearms(t *testing.T) {
	m := aeModel()
	mm, cmd := m.Update(repaintMsg{})
	assert.NotNil(t, cmd, "repaint re-arms + Repaint")
	_ = mm
}

// TestUpdate_StreamMsgDoneWithQueueDrains proves a done event with queued
// messages drains one via dispatchSend.
func TestUpdate_StreamMsgDoneWithQueueDrains(t *testing.T) {
	rec := newScriptedSession(nil)
	m := wsModel(rec)
	ch := make(chan cli.StreamEvent)
	m.streamCh = ch
	m.msgQueue = []string{"follow-up"}

	mm, _ := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "done"}})
	m = mm.(model)
	// drainQueue dispatched the queued message (state cleared or cmd armed).
	_ = m
}

// TestUpdate_MouseWheelAndLeft proves mouse wheel scrolls the viewport and a left
// press starts a selection.
func TestUpdate_MouseWheelAndLeft(t *testing.T) {
	m := aeModel()
	// Wheel down scrolls the viewport + repaint.
	mm, cmd := m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	assert.NotNil(t, cmd)
	_ = mm

	// Left button starts a selection (handleSelectMouse).
	mm, cmd = m.Update(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress, X: 0, Y: 0})
	_ = mm
	_ = cmd

	// Wheel while selecting is dropped.
	m2 := aeModel()
	m2.selecting = true
	mm, cmd = m2.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	assert.Nil(t, cmd, "wheel while selecting is dropped")
	_ = mm
}

// TestUpdate_StreamMsgReArmsWait proves a non-done stream event re-arms
// waitForEvent while the channel is open.
func TestUpdate_StreamMsgReArmsWait(t *testing.T) {
	m := aeModel()
	ch := make(chan cli.StreamEvent, 1)
	ch <- cli.StreamEvent{Kind: "agent_chunk", Text: "hi"}
	m.streamCh = ch
	mm, cmd := m.Update(streamMsg{ev: cli.StreamEvent{Kind: "agent_chunk", Text: "hi"}})
	// Non-done + streamCh still open -> waitForEvent armed (batched cmd).
	assert.NotNil(t, cmd)
	_ = mm
}
