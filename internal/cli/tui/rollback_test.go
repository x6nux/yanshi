package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

// ---- rollbackCandidates ----

func TestRollbackCandidates_MixedEntriesMostRecentFirst(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.entries = []entry{
		&userEntry{text: "first"},         // index 0, oldest user turn
		&assistantEntry{},                 // index 1, not a user turn
		ackEntry{text: "forked"},          // index 2, not a user turn
		&userEntry{text: "second"},        // index 3
		&assistantEntry{},                 // index 4
		&userEntry{text: "third, latest"}, // index 5, most recent user turn
	}

	items := m.rollbackCandidates()
	require.Len(t, items, 3)

	assert.Equal(t, "third, latest", items[0].text)
	assert.Equal(t, 1, items[0].turnsBack)
	assert.Equal(t, 5, items[0].entryIndex)

	assert.Equal(t, "second", items[1].text)
	assert.Equal(t, 2, items[1].turnsBack)
	assert.Equal(t, 3, items[1].entryIndex)

	assert.Equal(t, "first", items[2].text)
	assert.Equal(t, 3, items[2].turnsBack)
	assert.Equal(t, 0, items[2].entryIndex)
}

func TestRollbackCandidates_NoUserTurnsIsEmpty(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.entries = []entry{&assistantEntry{}, ackEntry{text: "x"}}
	assert.Empty(t, m.rollbackCandidates())

	m2 := newModel(&fakeSession{}, "/proj")
	assert.Empty(t, m2.rollbackCandidates(), "brand new session has nothing to roll back to")
}

// ---- Esc-Esc detection (positive + negative) ----

func TestEscEsc_OpensPickerWithinWindow(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.entries = []entry{&userEntry{text: "hello"}}

	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.False(t, ok, "first Esc: no toast, no double-press yet — falls through like pre-W-E-11")
	assert.Nil(t, mm.rollback, "first Esc must not open the picker")
	assert.False(t, mm.lastEsc.IsZero(), "first Esc records the timestamp")

	mm2, ok2 := handleKey(mm, tea.KeyMsg{Type: tea.KeyEscape})
	assert.True(t, ok2)
	if assert.NotNil(t, mm2.rollback, "second Esc within the window opens the picker") {
		require.Len(t, mm2.rollback.items, 1)
		assert.Equal(t, "hello", mm2.rollback.items[0].text)
	}
	assert.True(t, mm2.lastEsc.IsZero(), "the pair is consumed so a third press starts fresh")
}

func TestEscEsc_NoUserTurnsFallsThrough(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	// No user entries at all: the picker must not open even on a fast double press.
	mm, _ := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
	mm2, ok := handleKey(mm, tea.KeyMsg{Type: tea.KeyEscape})
	assert.False(t, ok, "no candidates: falls through to ordinary (unhandled) Esc, same as no toast")
	assert.Nil(t, mm2.rollback, "nothing to roll back to: Esc-Esc is a no-op, not a picker")
}

func TestEscEsc_StreamingFallsThrough(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.entries = []entry{&userEntry{text: "hello"}}
	m.streamCh = make(chan cli.StreamEvent) // non-nil: a turn is in flight

	mm, _ := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
	mm2, ok := handleKey(mm, tea.KeyMsg{Type: tea.KeyEscape})
	assert.False(t, ok, "streaming: falls through to ordinary (unhandled) Esc, same as no toast")
	assert.Nil(t, mm2.rollback, "mirrors /fork's own mid-stream block: no picker while streaming")
}

func TestEscEsc_OutsideWindowIsTwoSinglePresses(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.entries = []entry{&userEntry{text: "hello"}}
	// Simulate a stale first press (older than escDoublePressWindow) rather than
	// sleeping in the test: handlers.go only reads m.lastEsc, it never depends
	// on wall-clock time having actually elapsed.
	m.lastEsc = time.Now().Add(-2 * escDoublePressWindow)

	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.False(t, ok, "too late to pair: treated as a fresh first press, same as no toast")
	assert.Nil(t, mm.rollback, "second press arrived too late to pair with the first")
	assert.False(t, mm.lastEsc.IsZero(), "this press now starts a fresh pairing window")
}

// TestSingleEsc_Unaffected pins "单次 Esc 的现有行为不变": a lone Esc (no
// repeat) must neither open the rollback picker nor otherwise deviate from
// its pre-W-E-11 behavior, whether or not an error toast is showing and
// whether or not there is a user turn to roll back to.
func TestSingleEsc_Unaffected(t *testing.T) {
	// No error toast, a user turn exists: single Esc is a pure no-op (falls
	// all the way through handleKeyMsg's switch with nothing left to do).
	m := newModel(&fakeSession{}, "/proj")
	m.entries = []entry{&userEntry{text: "hello"}}
	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.False(t, ok, "no toast, no repeat: falls through to the default input handler, same as pre-W-E-11")
	assert.Nil(t, mm.rollback)

	// An error toast is showing: single Esc still dismisses it (pre-existing
	// C2 — UX7 behavior), same as TestHandleKeyMsg_EscapeDismissesErrorToast.
	m = newModel(&fakeSession{}, "/proj")
	m.entries = []entry{&userEntry{text: "hello"}}
	m.toasts.push(toast{Level: "error", Text: "boom"})
	mm, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.True(t, ok)
	assert.Nil(t, mm.rollback)
	assert.False(t, mm.hasErrorToast(), "single Esc still dismisses the error toast")
}

// ---- rollback picker modal dispatch ----

func TestRollbackPicker_ModalDispatch(t *testing.T) {
	mk := func() model {
		m := newModel(&recordingSession{}, "/proj")
		m.rollback = &rollbackState{items: []rollbackItem{
			{text: "a", turnsBack: 2, entryIndex: 0},
			{text: "b", turnsBack: 1, entryIndex: 2},
		}}
		return m
	}

	// Down advances the cursor.
	mm, ok := handleKey(mk(), tea.KeyMsg{Type: tea.KeyDown})
	assert.True(t, ok)
	assert.Equal(t, 1, mm.rollback.cursor)

	// Down again clamps at the last item (no wrap, matching restoreSessions).
	m2 := mk()
	m2.rollback.cursor = 1
	mm, ok = handleKey(m2, tea.KeyMsg{Type: tea.KeyDown})
	assert.True(t, ok)
	assert.Equal(t, 1, mm.rollback.cursor, "Down at the last item stays put")

	// Up clamps at 0 (no wrap).
	mm, ok = handleKey(mk(), tea.KeyMsg{Type: tea.KeyUp})
	assert.True(t, ok)
	assert.Equal(t, 0, mm.rollback.cursor, "Up at the first item stays put")

	// Escape cancels without sending anything.
	rec := &recordingSession{}
	m3 := newModel(rec, "/proj")
	m3.rollback = &rollbackState{items: []rollbackItem{{text: "a", turnsBack: 1}}}
	mm, ok = handleKey(m3, tea.KeyMsg{Type: tea.KeyEscape})
	assert.True(t, ok)
	assert.Nil(t, mm.rollback)
	assert.Empty(t, rec.frames)

	// Any other key is swallowed (picker stays open, no textarea leak).
	mm, ok = handleKey(mk(), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	assert.True(t, ok)
	assert.NotNil(t, mm.rollback)
	assert.Empty(t, mm.input.Value(), "keystroke must not leak into the textarea")
}

// TestRollbackPicker_BlocksOtherModalHotkeys pins the ordering claim in
// handlers.go's W-E-11 comment: because the rollback block runs before every
// other modal's own-hotkey handling, Ctrl+T/Ctrl+K etc. cannot open a second
// modal while the picker owns the keyboard.
func TestRollbackPicker_BlocksOtherModalHotkeys(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.rollback = &rollbackState{items: []rollbackItem{{text: "a", turnsBack: 1}}}

	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyCtrlT})
	assert.True(t, ok)
	assert.False(t, mm.pagerVisible, "Ctrl+T must not open the pager while rollback owns the keyboard")
	assert.NotNil(t, mm.rollback, "picker stays open")
}

// ---- rollbackConfirm ----

func TestRollbackConfirm_SendsForkSessionFrame(t *testing.T) {
	rec := &recordingSession{}
	m := wsModel(rec)
	m.rollback = &rollbackState{items: []rollbackItem{
		{text: "picked text", turnsBack: 3, entryIndex: 7},
	}}

	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, ok)
	assert.Nil(t, mm.rollback, "confirm closes the picker")

	require.Len(t, rec.frames, 1)
	f := rec.frames[0]
	assert.Equal(t, "fork_session", f.Type)
	assert.Equal(t, 3, f.TurnsBack)

	assert.True(t, mm.pendingRollback)
	assert.Equal(t, "picked text", mm.pendingRollbackText)
	assert.Equal(t, 7, mm.pendingRollbackIndex)
}

// ---- session_forked applyEvent integration ----

func TestSessionForked_RollbackTruncatesAndRefillsInput(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.entries = []entry{
		&userEntry{text: "keep me"},         // index 0
		&assistantEntry{},                   // index 1
		&userEntry{text: "roll back to me"}, // index 2 — the picked turn
		&assistantEntry{},                   // index 3, must be dropped
	}
	m.pendingRollback = true
	m.pendingRollbackText = "roll back to me"
	m.pendingRollbackIndex = 2

	m = m.applyEvent(cli.StreamEvent{Kind: "session_forked", SessionID: "fork-1"})

	// Truncated to entryIndex, then the ack appended.
	require.Len(t, m.entries, 3)
	ue, ok := m.entries[0].(*userEntry)
	require.True(t, ok)
	assert.Equal(t, "keep me", ue.text)
	_, isAssistant := m.entries[1].(*assistantEntry)
	assert.True(t, isAssistant)
	ack, isAck := m.entries[2].(ackEntry)
	require.True(t, isAck)
	assert.Contains(t, ack.text, "fork-1")

	assert.Equal(t, "fork-1", m.sessionID)
	assert.Equal(t, "roll back to me", m.input.Value(), "spec: 确认后 fork 并把原 prompt 自动填回编辑框")

	// Pending fields cleared so a later NORMAL /fork doesn't misfire this path.
	assert.False(t, m.pendingRollback)
	assert.Empty(t, m.pendingRollbackText)
	assert.Equal(t, 0, m.pendingRollbackIndex)
}

// TestSessionForked_NormalForkUnaffected is the critical regression guard:
// plain /fork (cmdFork, commands_session_memory.go) never sets pendingRollback,
// so its session_forked reply must not truncate m.entries or touch the input —
// exactly the pre-W-E-11 behavior.
func TestSessionForked_NormalForkUnaffected(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.entries = []entry{
		&userEntry{text: "a"},
		&assistantEntry{},
		&userEntry{text: "b"},
	}
	m.input.SetValue("draft in progress")
	// pendingRollback intentionally left false — the normal /fork path.

	m = m.applyEvent(cli.StreamEvent{Kind: "session_forked", SessionID: "fork-2"})

	require.Len(t, m.entries, 4, "no truncation: original 3 entries + the ack")
	ack, ok := m.entries[3].(ackEntry)
	require.True(t, ok)
	assert.Contains(t, ack.text, "fork-2")
	assert.Equal(t, "fork-2", m.sessionID)
	assert.Equal(t, "draft in progress", m.input.Value(), "normal /fork must not touch the textarea")
}

// ---- the mandatory 7-Esc-site pinning test ----

// TestEscEsc_AllSevenExistingEscSitesStillWork enumerates, by name, every one
// of the 7 places handleKeyMsg reads tea.KeyEscape (per the file's own
// ordering) and asserts each still produces its pre-W-E-11 single-press
// behavior. This exists as its own test — separate from the fact that each
// site's pre-existing dedicated test continues to pass — because the report
// requires an explicit, self-contained pin of "single Esc unchanged" across
// every site, not just an absence of regressions in scattered tests.
func TestEscEsc_AllSevenExistingEscSitesStillWork(t *testing.T) {
	// Site 1: pagerVisible (W-E-03 fullscreen pager) — Esc closes the pager.
	t.Run("1_pager", func(t *testing.T) {
		m := newModel(&fakeSession{}, "/proj")
		m.pagerVisible = true
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.False(t, mm.pagerVisible)
	})

	// Site 2: helpVisible (F1 panel) — Esc closes help.
	t.Run("2_help", func(t *testing.T) {
		m := newModel(&fakeSession{}, "/proj")
		m.helpVisible = true
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.False(t, mm.helpVisible)
	})

	// Site 3: restoreSessions picker — Esc dismisses it.
	t.Run("3_restore_picker", func(t *testing.T) {
		m := newModel(&fakeSession{}, "/proj")
		m.restoreSessions = []proto.SessionInfo{{ID: "a"}}
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.Nil(t, mm.restoreSessions)
	})

	// Site 4: action palette (Ctrl+K) — Esc closes it.
	t.Run("4_action_palette", func(t *testing.T) {
		m := newModel(&fakeSession{}, "/proj")
		m.action = &actionState{visible: true}
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.Nil(t, mm.action)
	})

	// Site 5: history search popup (Alt+R) — Esc closes it without touching the draft.
	t.Run("5_history_search", func(t *testing.T) {
		m := newModel(&fakeSession{}, "/proj")
		m.historySearch = &historyState{visible: true, items: []historyItem{{Text: "x"}}}
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.Nil(t, mm.historySearch)
	})

	// Site 6: interactive command picker (/model, /mode, /theme) — Esc closes it.
	t.Run("6_command_picker", func(t *testing.T) {
		m := newModel(&fakeSession{}, "/proj")
		m.pickerKind = "theme"
		m.pickerItems = []pickerItem{{name: "dark"}}
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.Equal(t, "", mm.pickerKind)
	})

	// Site 7: the bottom-level KeyEscape case itself — a single press with an
	// error toast dismisses it; a single press with none is a handled no-op.
	// Neither opens the rollback picker.
	t.Run("7_bottom_level_toast_dismiss", func(t *testing.T) {
		m := newModel(&fakeSession{}, "/proj")
		m.toasts.push(toast{Level: "error", Text: "boom"})
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.False(t, mm.hasErrorToast())
		assert.Nil(t, mm.rollback)
	})
	t.Run("7_bottom_level_noop", func(t *testing.T) {
		m := newModel(&fakeSession{}, "/proj")
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.False(t, ok, "no toast, no modal: single Esc falls through to the default input handler, matching pre-W-E-11 behavior")
		assert.Nil(t, mm.rollback)
	})
}
