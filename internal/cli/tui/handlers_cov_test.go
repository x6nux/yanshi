package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/proto"
)

// handleKey calls handleKeyMsg and returns the model + handled flag, dropping
// the cmd (tests assert on state; the cmd is runtime-only and unused here).
func handleKey(m model, msg tea.KeyMsg) (model, bool) {
	mm, _, ok := m.handleKeyMsg(msg)
	return mm, ok
}

// ---- helpVisible modal (F1 panel) ----

func TestCov_HelpModal(t *testing.T) {
	// Esc closes.
	m := newModel(&fakeSession{}, "/proj")
	m.helpVisible = true
	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.True(t, ok)
	assert.False(t, mm.helpVisible, "Esc closes help")

	// Runes append to the query.
	m = newModel(&fakeSession{}, "/proj")
	m.helpVisible = true
	mm, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	assert.True(t, ok)
	assert.Equal(t, "x", mm.helpQuery)

	// Backspace drops the last rune.
	m = newModel(&fakeSession{}, "/proj")
	m.helpVisible = true
	m.helpQuery = "abc"
	mm, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyBackspace})
	assert.True(t, ok)
	assert.Equal(t, "ab", mm.helpQuery)

	// A non-Esc/Runes/Backspace key while help is visible hits the default
	// branch and falls through (KeyUp with no other modal → not handled).
	m = newModel(&fakeSession{}, "/proj")
	m.helpVisible = true
	_, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyUp})
	assert.False(t, ok, "default help key falls through")
}

// ---- restoreSessions picker ----

func TestCov_RestorePicker(t *testing.T) {
	// Down advances the cursor when more sessions exist.
	m := newModel(&fakeSession{}, "/proj")
	m.restoreSessions = []proto.SessionInfo{{ID: "a"}, {ID: "b"}}
	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyDown})
	assert.True(t, ok)
	assert.Equal(t, 1, mm.restoreCursor)

	// Up at cursor 0 stays at 0.
	m = newModel(&fakeSession{}, "/proj")
	m.restoreSessions = []proto.SessionInfo{{ID: "a"}}
	mm, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyUp})
	assert.True(t, ok)
	assert.Equal(t, 0, mm.restoreCursor)

	// Esc dismisses.
	m = newModel(&fakeSession{}, "/proj")
	m.restoreSessions = []proto.SessionInfo{{ID: "a"}}
	mm, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.True(t, ok)
	assert.Nil(t, mm.restoreSessions)

	// Enter confirms (sendControlFrame is nil-safe for fakeSession).
	m = newModel(&fakeSession{}, "/proj")
	m.restoreSessions = []proto.SessionInfo{{ID: "a"}}
	mm, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, ok)
	assert.Nil(t, mm.restoreSessions, "Enter clears the picker")

	// Any other key is consumed (handled, picker unchanged).
	m = newModel(&fakeSession{}, "/proj")
	m.restoreSessions = []proto.SessionInfo{{ID: "a"}}
	mm, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	assert.True(t, ok)
	assert.NotNil(t, mm.restoreSessions)
}

// ---- action palette (Ctrl+K) ----

func TestCov_ActionPalette(t *testing.T) {
	// Esc closes.
	m := newModel(&fakeSession{}, "/proj")
	m.action = &actionState{visible: true, cursor: 3}
	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.True(t, ok)
	assert.Nil(t, mm.action)

	// Up/Down move the cursor.
	m = newModel(&fakeSession{}, "/proj")
	m.action = &actionState{visible: true, cursor: 2, items: []actionItem{{}, {}, {}}}
	mm, _ = handleKey(m, tea.KeyMsg{Type: tea.KeyUp})
	assert.Equal(t, 1, mm.action.cursor)
	mm, _ = handleKey(m, tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, 2, mm.action.cursor)

	// Runes append to the query; Backspace drops.
	m = newModel(&fakeSession{}, "/proj")
	m.action = &actionState{visible: true}
	mm, _ = handleKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	assert.Equal(t, "q", mm.action.query)
	mm, _ = handleKey(m, tea.KeyMsg{Type: tea.KeyBackspace})
	assert.Equal(t, "", mm.action.query)
}

// ---- history search popup (Alt+R) ----

func TestCov_HistorySearch(t *testing.T) {
	items := []historyItem{{Text: "hello"}, {Text: "world"}}

	// Esc closes.
	m := newModel(&fakeSession{}, "/proj")
	m.historySearch = &historyState{visible: true, items: items}
	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
	assert.True(t, ok)
	assert.Nil(t, mm.historySearch)

	// Down/Up wrap cyclically.
	m = newModel(&fakeSession{}, "/proj")
	m.historySearch = &historyState{visible: true, items: items, cursor: 0}
	mm, _ = handleKey(m, tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, 1, mm.historySearch.cursor)
	mm, _ = handleKey(m, tea.KeyMsg{Type: tea.KeyDown}) // wrap → 0
	assert.Equal(t, 0, mm.historySearch.cursor)
	mm, _ = handleKey(m, tea.KeyMsg{Type: tea.KeyUp}) // wrap → 1
	assert.Equal(t, 1, mm.historySearch.cursor)

	// Enter restores the selected prompt into the input.
	m = newModel(&fakeSession{}, "/proj")
	m.historySearch = &historyState{visible: true, items: items, cursor: 1}
	mm, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, ok)
	assert.Nil(t, mm.historySearch)
	assert.Equal(t, "world", mm.input.Value())

	// Runes/Backspace update the query (nil history → refresh is a no-op).
	m = newModel(&fakeSession{}, "/proj")
	m.historySearch = &historyState{visible: true}
	mm, _ = handleKey(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	assert.Equal(t, "a", mm.historySearch.query)
	mm, _ = handleKey(m, tea.KeyMsg{Type: tea.KeyBackspace})
	assert.Equal(t, "", mm.historySearch.query)

	// Default consumes the key.
	m = newModel(&fakeSession{}, "/proj")
	m.historySearch = &historyState{visible: true}
	_, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyPgUp})
	assert.True(t, ok, "unknown key consumed while history search open")
}

// TestCov_AltRAndAltUp covers Alt+R (open history search) and Alt+Up (edit-last
// queued, no-op on an empty queue).
func TestCov_AltRAndAltUp(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, ok := handleKey(m, tea.KeyMsg{Alt: true, Type: tea.KeyRunes, Runes: []rune{'r'}})
	assert.True(t, ok)
	require := mm.historySearch
	if require == nil {
		t.Fatal("Alt+R opens history search")
	}

	// Alt+Up on empty queue → handled no-op.
	m = newModel(&fakeSession{}, "/proj")
	_, ok = handleKey(m, tea.KeyMsg{Alt: true, Type: tea.KeyUp})
	assert.True(t, ok)
}

// ---- interactive command picker (/model, /mode, /theme) ----

func TestCov_Picker(t *testing.T) {
	mk := func() model {
		m := newModel(&fakeSession{}, "/proj")
		m.pickerKind = "theme"
		m.pickerItems = []pickerItem{{name: "dark"}, {name: "light"}}
		m.pickerCursor = 0
		return m
	}
	// Up/Down wrap.
	mm, _ := handleKey(mk(), tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, 1, mm.pickerCursor)
	mm, _ = handleKey(mk(), tea.KeyMsg{Type: tea.KeyUp}) // wrap → last
	assert.Equal(t, 1, mm.pickerCursor)

	// j/k vim navigation.
	mm, _ = handleKey(mk(), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	assert.Equal(t, 1, mm.pickerCursor)
	mm, _ = handleKey(mk(), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}}) // wrap → last
	assert.Equal(t, 1, mm.pickerCursor)

	// Esc closes.
	mm, ok := handleKey(mk(), tea.KeyMsg{Type: tea.KeyEscape})
	assert.True(t, ok)
	assert.Equal(t, "", mm.pickerKind)

	// Enter confirms (pickerConfirm → sendControlFrame nil-safe for fake).
	mm, ok = handleKey(mk(), tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, ok)
	assert.Equal(t, "", mm.pickerKind, "Enter resolves the picker")

	// Other key consumed.
	mm, ok = handleKey(mk(), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	assert.True(t, ok)
	assert.Equal(t, "theme", mm.pickerKind, "unknown key keeps picker open")
}

// ---- main switch branches ----

// TestCov_YoloConfirmCancel: a non-Enter/ShiftTab key while a yolo confirm is
// pending cancels it.
func TestCov_YoloConfirmCancel(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.yoloConfirm = 2
	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyUp})
	assert.True(t, ok)
	assert.Equal(t, 0, mm.yoloConfirm)
}

// TestCov_ShiftTabCyclesMode: Shift+Tab cycles the permission mode.
func TestCov_ShiftTabCyclesMode(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	_, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	assert.True(t, ok)
}

// TestCov_ShiftUpDownAutoThreshold: Shift+Up/Down adjust the auto threshold
// when permMode is Auto.
func TestCov_ShiftUpDownAutoThreshold(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.permMode = guard.ModeAuto
	m.autoThreshold = 5
	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyShiftUp})
	assert.True(t, ok)
	assert.Equal(t, 6, mm.autoThreshold)
	// Chain on the updated model (permMode stays Auto while tuning threshold).
	mm, ok = handleKey(mm, tea.KeyMsg{Type: tea.KeyShiftDown})
	assert.True(t, ok)
	assert.Equal(t, 5, mm.autoThreshold)

	// Clamp at the min (1) on a fresh Auto model.
	mLow := newModel(&fakeSession{}, "/proj")
	mLow.permMode = guard.ModeAuto
	mLow.autoThreshold = 1
	mmLow, _ := handleKey(mLow, tea.KeyMsg{Type: tea.KeyShiftDown})
	assert.Equal(t, 1, mmLow.autoThreshold, "clamped to min 1")
}

// TestCov_PaletteNavTabEnter covers the command-palette paths: Up/Down move,
// Tab completes, Enter with an open palette and no args runs the selected cmd.
func TestCov_PaletteNavTabEnter(t *testing.T) {
	mk := func() model {
		m := newModel(&fakeSession{}, "/proj")
		m.paletteItems = []command{{name: "clear"}, {name: "help"}}
		m.paletteSel = 0
		return m
	}
	mm, _ := handleKey(mk(), tea.KeyMsg{Type: tea.KeyUp}) // wrap → last
	assert.Equal(t, 1, mm.paletteSel)
	mm, _ = handleKey(mk(), tea.KeyMsg{Type: tea.KeyDown})
	assert.Equal(t, 1, mm.paletteSel)

	// Tab completes the selected command into the input (with a trailing space
	// so the user can type args).
	mm, _ = handleKey(mk(), tea.KeyMsg{Type: tea.KeyTab})
	assert.Contains(t, mm.input.Value(), "clear")

	// Enter with palette open + no args runs the selected command: the path
	// sets the input then submit() consumes (and clears) it, so we assert the
	// key was handled rather than the post-submit input value.
	mm, ok := handleKey(mk(), tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, ok)
	_ = mm
}

// TestCov_CtrlS_PopupGuard: Ctrl+S is a no-op while any popup owns keystrokes.
func TestCov_CtrlS_PopupGuard(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.paletteItems = []command{{name: "clear"}} // palette open
	_, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyCtrlS})
	assert.True(t, ok)
}

// TestCov_F1Toggle covers F1 open and close.
func TestCov_F1Toggle(t *testing.T) {
	// Open.
	m := newModel(&fakeSession{}, "/proj")
	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyF1})
	assert.True(t, ok)
	assert.True(t, mm.helpVisible)
	// Close.
	mm, ok = handleKey(mm, tea.KeyMsg{Type: tea.KeyF1})
	assert.True(t, ok)
	assert.False(t, mm.helpVisible)
}

// TestCov_PasteDetection: a bracketed paste (or >50-rune bulk drop) marks the
// in-progress input as pasted so submit() later collapses long pastes.
func TestCov_PasteDetection(t *testing.T) {
	// Bracketed-paste flag.
	mm, _ := handleKey(newModel(&fakeSession{}, "/proj"), tea.KeyMsg{
		Type: tea.KeyRunes, Runes: []rune("x"), Paste: true,
	})
	assert.True(t, mm.inputPasted, "bracketed paste marks input")

	// Bulk drop (>bulkPasteRuneThreshold runes).
	mm, _ = handleKey(newModel(&fakeSession{}, "/proj"), tea.KeyMsg{
		Type:  tea.KeyRunes,
		Runes: []rune(strings.Repeat("a", bulkPasteRuneThreshold+1)),
	})
	assert.True(t, mm.inputPasted, "bulk rune drop marks input")
}

// TestCov_HandlerRemainingBranches mops up the last cold spots: restore-picker
// cursor decrement, action-palette Enter with no items, and the pending-
// permission KeyUp/Tab modal branches.
func TestCov_HandlerRemainingBranches(t *testing.T) {
	// restoreSessions: Up decrements when cursor > 0.
	m := newModel(&fakeSession{}, "/proj")
	m.restoreSessions = []proto.SessionInfo{{ID: "a"}, {ID: "b"}}
	m.restoreCursor = 1
	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyUp})
	assert.True(t, ok)
	assert.Equal(t, 0, mm.restoreCursor, "Up decrements the restore cursor")

	// action palette: Enter with no items hits actionConfirm's early return
	// (it returns *m unchanged — only the items-present path clears the popup).
	m = newModel(&fakeSession{}, "/proj")
	m.action = &actionState{visible: true} // empty items
	_, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.True(t, ok, "Enter handled by the action palette")

	// Pending permission: Up moves the selection (modal).
	m = newModel(&fakeSession{}, "/proj")
	m.pendingPermissions = []*permissionEntry{{tool: "fs_write"}}
	_, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyUp})
	assert.True(t, ok, "Up handled while permission prompt pending")

	// Pending permission: Tab is a modal no-op.
	m = newModel(&fakeSession{}, "/proj")
	m.pendingPermissions = []*permissionEntry{{tool: "fs_write"}}
	_, ok = handleKey(m, tea.KeyMsg{Type: tea.KeyTab})
	assert.True(t, ok, "Tab handled (no-op) while permission prompt pending")
}
