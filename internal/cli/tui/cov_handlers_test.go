package tui

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/proto"
)

// ---- view.go pure helpers ----

func TestHighlightLines_Spans(t *testing.T) {
	// Single-row span highlights the [0,3) columns of row 0.
	out := highlightLines("hello\nworld", selPos{row: 0, col: 0}, selPos{row: 0, col: 3})
	assert.Contains(t, out, "hel")

	// Multi-row span: first row from lo.col to end, last row start to hi.col.
	out = highlightLines("aaaa\nbbbb\ncccc", selPos{row: 0, col: 2}, selPos{row: 2, col: 2})
	assert.NotEmpty(t, out)

	// lo.row beyond the screen -> nothing happens (no panic).
	out = highlightLines("x", selPos{row: 5, col: 0}, selPos{row: 6, col: 1})
	assert.Equal(t, "x", out)

	// start >= end on a row -> nothing highlighted on that row.
	out = highlightLines("abc", selPos{row: 0, col: 3}, selPos{row: 0, col: 3})
	assert.Equal(t, "abc", out)
}

func TestCopyClipboard_ReturnsCmd(t *testing.T) {
	cmd := copyClipboard("hello")
	require.NotNil(t, cmd)
	// Executing the Cmd writes the OSC 52 sequence to stdout and returns nil. We
	// cannot easily capture stdout, but running it exercises the body.
	msg := cmd()
	assert.Nil(t, msg)
}

func TestStripANSI(t *testing.T) {
	assert.Equal(t, "plain", stripANSI("plain"))
	assert.Equal(t, "hello", stripANSI("\x1b[31mhello\x1b[0m"))
}

// ---- waitForEvent ----

func TestWaitForEvent_ClosedChannelReturnsDone(t *testing.T) {
	m := wsModel(&recordingSession{})
	ch := make(chan cli.StreamEvent)
	close(ch)
	m.streamCh = ch
	cmd := m.waitForEvent()
	require.NotNil(t, cmd)
	msg := cmd()
	sm, ok := msg.(streamMsg)
	require.True(t, ok)
	assert.Equal(t, "done", sm.ev.Kind, "closed channel -> done event")
}

func TestWaitForEvent_EventPassthrough(t *testing.T) {
	m := wsModel(&recordingSession{})
	ch := make(chan cli.StreamEvent, 1)
	ch <- cli.StreamEvent{Kind: "agent_chunk", Text: "hi"}
	close(ch)
	m.streamCh = ch
	cmd := m.waitForEvent()
	msg := cmd()
	sm, ok := msg.(streamMsg)
	require.True(t, ok)
	assert.Equal(t, "agent_chunk", sm.ev.Kind)
	assert.Equal(t, "hi", sm.ev.Text)
}

// ---- formatSessionAck ----

func TestFormatSessionAck_AllActions(t *testing.T) {
	assert.Contains(t, formatSessionAck("renamed", "1234567890abc", "Title"), "renamed")
	assert.Contains(t, formatSessionAck("renamed", "1234567890abc", "Title"), "12345678") // truncated to 8
	assert.Contains(t, formatSessionAck("archived", "abc", ""), "archived")
	assert.Contains(t, formatSessionAck("unarchive", "abc", ""), "restored")
	assert.Contains(t, formatSessionAck("deleted", "abc", ""), "deleted")
	assert.Contains(t, formatSessionAck("custom", "abc", ""), "custom") // default branch
}

// ---- yoloPopup ----

func TestYoloPopup_Stages(t *testing.T) {
	m := wsModel(&recordingSession{})
	assert.Equal(t, "", m.yoloPopup(), "no gate -> empty")
	m.yoloConfirm = 1
	assert.Contains(t, m.yoloPopup(), "YOLO")
	m.yoloConfirm = 2
	assert.Contains(t, m.yoloPopup(), "Confirm")
}

// ---- sendControlFrame reply path ----

func TestSendControlFrame_ReplyChannelArmsWait(t *testing.T) {
	// A replySession returns a channel -> sendControlFrame sets streamCh + arms
	// waitForEvent + activityTick (returns a batched Cmd).
	m := wsModel(&replySession{ch: closedReplyCh()})
	mm, cmd := m.sendControlFrame(proto.NewGetStatus())
	m = mm.(model)
	assert.NotNil(t, m.streamCh, "reply channel armed streamCh")
	require.NotNil(t, cmd)

	// A fakeSession returns nil -> sendControlFrame is a fire-and-forget (no cmd).
	m = wsModel(&fakeSession{})
	mm, cmd = m.sendControlFrame(proto.NewGetStatus())
	assert.Nil(t, mm.(model).streamCh)
	// cmd may be nil or a no-op refresh; the key assertion is no panic + nil streamCh.
}

// ---- cmdReview ----

func TestCmdReview_UsageAndDispatch(t *testing.T) {
	// No args -> usage error, no dispatch.
	rec := &recordingSession{}
	m := wsModel(rec)
	mm, _ := runCommandOn(model(m), "/review")
	_ = mm.(model)
	assert.Empty(t, rec.sentText, "no args -> usage error, no user_message")

	// With a diff arg -> dispatchSend (user_message with /review prefix).
	rec = &recordingSession{}
	m = wsModel(rec)
	mm, _ = runCommandOn(model(m), "/review some diff")
	_ = mm.(model)
	require.NotEmpty(t, rec.sentText)
	assert.Contains(t, rec.sentText[0], "/review")
}

// ---- loadPreferences ----

func TestLoadPreferences_MissingValidInvalid(t *testing.T) {
	dir := t.TempDir()

	// Missing file -> empty layer, no error.
	p, err := loadPreferences(filepath.Join(dir, "absent.json"))
	require.NoError(t, err)
	assert.Equal(t, "", p.ThemeName)

	// Valid JSON -> decoded layer.
	path := filepath.Join(dir, "prefs.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"theme":"muted","vim":true}`), 0o644))
	p, err = loadPreferences(path)
	require.NoError(t, err)
	assert.Equal(t, "muted", p.ThemeName)
	require.NotNil(t, p.Vim)
	assert.True(t, *p.Vim)

	// Invalid JSON -> error.
	badPath := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(badPath, []byte("{not json"), 0o644))
	_, err = loadPreferences(badPath)
	require.Error(t, err)
}

// ---- handleKeyMsg via Update ----

func TestHandleKeyMsg_HelpPanel(t *testing.T) {
	m := wsModel(&fakeSession{})
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyF1}) // open help
	m = lastModel(mm)
	require.True(t, m.helpVisible)

	// Type a query rune.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	m = lastModel(mm)
	assert.Contains(t, m.helpQuery, "c")

	// Backspace drops it.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	m = lastModel(mm)
	assert.Equal(t, "", m.helpQuery)

	// Esc closes.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = lastModel(mm)
	assert.False(t, m.helpVisible)
}

func TestHandleKeyMsg_ActionPaletteKeys(t *testing.T) {
	m := wsModel(&fakeSession{})
	m.openActionPopup()
	require.NotNil(t, m.action)

	// Down/Up move the cursor.
	start := m.action.cursor
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = mm.(model)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	assert.Equal(t, start, mm.(model).action.cursor, "down then up returns to start")

	// Runes extend the query.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")})
	assert.Contains(t, mm.(model).action.query, "z")

	// Esc closes.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	assert.Nil(t, mm.(model).action)
}

func TestHandleKeyMsg_CommandPickerKeys(t *testing.T) {
	m := wsModel(&fakeSession{})
	m.pickerKind = "model"
	m.pickerItems = []pickerItem{{name: "a"}, {name: "b"}, {name: "c"}}
	m.pickerCursor = 0

	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = lastModel(mm)
	assert.Equal(t, 1, m.pickerCursor)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = lastModel(mm)
	assert.Equal(t, 2, m.pickerCursor, "j moves down")
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = lastModel(mm)
	assert.Equal(t, 1, m.pickerCursor, "k moves up")
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	assert.Equal(t, "", lastModel(mm).pickerKind, "Esc closes picker")
}

func TestHandleKeyMsg_PermissionPromptKeys(t *testing.T) {
	rec := &recordingSession{}
	m := wsModel(rec)
	m.pendingPermissions = []*permissionEntry{{tool: "fs_read", id: "p1"}}

	// Up/Down move the selection.
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = mm.(model)
	assert.NotEqual(t, 0, m.permSel)

	// y resolves with allow.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	m = mm.(model)
	assert.Empty(t, m.pendingPermissions, "y resolves the prompt")
	require.NotEmpty(t, rec.frames)
	assert.Equal(t, "permission_response", rec.frames[0].Type)
}

func TestHandleKeyMsg_YoloGateEnterSequence(t *testing.T) {
	rec := &recordingSession{}
	m := wsModel(rec)
	m.yoloConfirm = 1

	// First Enter -> stage 2.
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(model)
	assert.Equal(t, 2, m.yoloConfirm)

	// Second Enter -> confirm yolo, send set_mode.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = mm.(model)
	assert.Equal(t, 0, m.yoloConfirm)
	assert.Equal(t, guard.ModeYOLO, m.permMode)
	require.Contains(t, frameTypes(rec.frames), "set_mode")

	// Non-Enter at stage 1 cancels the gate.
	m2 := wsModel(&recordingSession{})
	m2.yoloConfirm = 1
	mm, _ = m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	assert.Equal(t, 0, mm.(model).yoloConfirm, "non-Enter cancels yolo gate")
}

func TestHandleKeyMsg_ShiftTabCyclesMode(t *testing.T) {
	rec := &recordingSession{}
	m := wsModel(rec)
	before := m.permMode
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = mm.(model)
	// Shift+Tab cycles (may arm the yolo gate); assert the mode changed or gate armed.
	assert.True(t, m.permMode != before || m.yoloConfirm > 0, "shift+tab advances mode")
}

func TestHandleKeyMsg_CtrlEnterNewline(t *testing.T) {
	m := wsModel(&fakeSession{})
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlEnter})
	m = mm.(model)
	assert.Contains(t, m.input.Value(), "\n", "ctrl+enter inserts a newline")
}

func TestHandleKeyMsg_CtrlKCtrlSCtrlO(t *testing.T) {
	// CtrlK toggles the action palette.
	m := wsModel(&fakeSession{})
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	require.NotNil(t, mm.(model).action, "ctrl+k opens action palette")
	mm, _ = mm.(model).Update(tea.KeyMsg{Type: tea.KeyCtrlK})
	assert.Nil(t, mm.(model).action, "second ctrl+k closes it")

	// CtrlS on empty textarea warns; on non-empty stashes.
	m = wsModel(&fakeSession{})
	m.stash = newTestStash(t)
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	_ = mm.(model) // empty -> warn toast, no panic

	m = wsModel(&fakeSession{})
	m.stash = newTestStash(t)
	m.input.SetValue("a draft")
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = mm.(model)
	assert.Equal(t, "", m.input.Value(), "ctrl+s clears the textarea after stashing")
	assert.Len(t, m.stash.List(), 1)

	// CtrlO is a no-op when nothing is expandable.
	m = wsModel(&fakeSession{})
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	_ = mm.(model) // no panic
}

func TestHandleKeyMsg_EscapeDismissesErrorToast(t *testing.T) {
	m := wsModel(&fakeSession{})
	m.toasts.push(toast{Level: "error", Text: "boom"})
	require.True(t, m.hasErrorToast())
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = mm.(model)
	assert.False(t, m.hasErrorToast(), "Esc dismisses the error toast")
}

func TestHandleKeyMsg_AltROpensHistory(t *testing.T) {
	m := wsModel(&fakeSession{})
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Alt: true, Runes: []rune{'r'}})
	m = mm.(model)
	require.NotNil(t, m.historySearch, "Alt+R opens history search")

	// Esc closes it.
	mm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	assert.Nil(t, mm.(model).historySearch)
}

func TestHandleKeyMsg_CtrlCQuitWhenIdle(t *testing.T) {
	m := wsModel(&fakeSession{})
	mm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	assert.NotNil(t, cmd, "Ctrl+C when idle quits")
	_ = mm
}

func TestHandleKeyMsg_PaletteTabComplete(t *testing.T) {
	m := wsModel(&fakeSession{})
	m.input.SetValue("/mo")
	m.updatePalette()
	require.True(t, m.paletteOpen())
	mm, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = mm.(model)
	assert.Nil(t, m.paletteItems, "Tab completes and closes the palette")
	assert.Contains(t, m.input.Value(), "/")
}

// lastModel extracts the model from a tea.Model (asserts it is the concrete type).
func lastModel(m tea.Model) model { return m.(model) }
