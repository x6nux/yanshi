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

// ---- view.go wiring: reflow()/renderScreen() dual-wiring ----

// TestRollbackPopup_ReflowAndRenderScreenWiring pins the two-sided wiring
// CLAUDE.md calls out for every new popup: reflow() must subtract its height
// from the viewport, and renderScreen() must actually append it — missing
// either produces either viewport overflow or an invisible-but-space-
// reserved popup.
func TestRollbackPopup_ReflowAndRenderScreenWiring(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = mm.(model)
	heightBefore := m.viewport.Height

	m.rollback = &rollbackState{items: []rollbackItem{{text: "hello", turnsBack: 1}}}
	m.reflow()
	assert.Less(t, m.viewport.Height, heightBefore, "an open rollback popup must shrink the viewport (reflow wiring)")

	out := m.renderScreen()
	assert.Contains(t, out, "Rollback", "an open rollback popup must be rendered (renderScreen wiring)")
}

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

// ---- RE-13 (fix-e3a): a rejected rollback fork must disarm pendingRollback ----

// TestApplyEvent_ErrorClearsPendingRollback pins the fix directly at the
// case "error" site: rollbackConfirm arms pendingRollback/*Text/*Index BEFORE
// the fork_session frame is even sent (rollback.go), so a server rejection —
// no "session_forked" reply ever arrives — must disarm them the same way the
// line right above already does for pendingSeamRestore.
func TestApplyEvent_ErrorClearsPendingRollback(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.pendingRollback = true
	m.pendingRollbackText = "stale prompt"
	m.pendingRollbackIndex = 3

	m = m.applyEvent(cli.StreamEvent{Kind: "error", Text: "fork: nope"})

	assert.False(t, m.pendingRollback, "a rejected fork must disarm pendingRollback")
	assert.Empty(t, m.pendingRollbackText)
	assert.Equal(t, 0, m.pendingRollbackIndex)
}

// TestSessionForked_PendingRollbackDoesNotLeakAcrossFailedFork is the
// end-to-end regression: reproduces the review's repro almost verbatim.
// Without the case "error" fix, a rejected Esc-Esc rollback leaves
// pendingRollback armed; the NEXT successful "session_forked" this session
// ever sees — even an ordinary /fork issued long after, with an unrelated
// draft sitting in the textarea — consumes the stale state: truncates
// m.entries at the old (now meaningless) index and overwrites the user's
// draft with the old picked prompt. This is reachable without compaction:
// two of the review's three documented paths are a side session
// (/exit-side clears cs.sessionID, so the server's handleForkSession replies
// with a generic "error" for what the TUI still thinks is a live rollback)
// and SSE (control frames are rejected outright on that transport) — this
// test uses a plain rejection to stay transport-agnostic, since the fix is
// in the shared case "error" handler both paths land on.
func TestSessionForked_PendingRollbackDoesNotLeakAcrossFailedFork(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.entries = []entry{
		&userEntry{text: "old turn"},
		&assistantEntry{},
		&userEntry{text: "second old turn"},
	}
	m.rollback = &rollbackState{items: []rollbackItem{{text: "old turn", turnsBack: 1, entryIndex: 0}}}

	// rollbackConfirm arms pendingRollback and sends fork_session — the
	// server rejects it (side session / SSE / out-of-range turns_back).
	mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEnter})
	require.True(t, ok)
	m = mm
	require.True(t, m.pendingRollback, "rollbackConfirm must have armed pendingRollback")

	m = m.applyEvent(cli.StreamEvent{Kind: "error", Text: "fork: nope"})
	require.False(t, m.pendingRollback, "prerequisite: the error must have disarmed pendingRollback")

	// User keeps working: types a fresh draft, then issues a PLAIN /fork
	// (pendingRollback intentionally not set — this mirrors cmdFork).
	m.input.SetValue("half-typed draft I do not want to lose")
	m = m.applyEvent(cli.StreamEvent{Kind: "session_forked", SessionID: "fork-9"})

	// No truncation: the 3 original entries survive, plus the error entry
	// the rejection appended, plus the session_forked ack — NOT cut down to
	// the stale rollback's entryIndex (which would have left only entries
	// before index 0, i.e. none, before the two appends).
	require.Len(t, m.entries, 5)
	_, stillUser0 := m.entries[0].(*userEntry)
	_, stillAssistant := m.entries[1].(*assistantEntry)
	_, stillUser1 := m.entries[2].(*userEntry)
	assert.True(t, stillUser0 && stillAssistant && stillUser1, "the 3 original entries must be untouched, not truncated at the stale rollback index")
	assert.Equal(t, "half-typed draft I do not want to lose", m.input.Value(),
		"a normal /fork after a rejected rollback must not overwrite the user's draft with the stale picked prompt")
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

// ---- the mandatory Esc-site pinning test ----

// TestEscEsc_AllExistingEscSitesStillWork enumerates, by name, every place
// handleKeyMsg reads tea.KeyEscape (per the file's own ordering) and asserts
// each still produces its pre-Esc-Esc single-press behavior.
//
// RE-18 (review-e3): this used to claim "7" and be named accordingly, but
// only 6 of these are actually pre-existing — `git grep -c 'tea\.KeyEsc'
// fa0cfed -- 'internal/cli/tui/*.go'` (fa0cfed is this whole batch's start
// commit, before W-E-03 and W-E-11 both landed) counts 6 in handlers.go.
// Site 1 (pagerVisible) is NOT one of them: W-E-03's fullscreen pager landed
// earlier in THIS SAME batch, before the Esc-Esc gesture (W-E-11) landed on
// top of it, so it has no pre-batch behavior to regress — it is pinned here
// because Esc-Esc must not swallow the pager's own Esc either, not because
// it predates this work. The set below is honestly 6 pre-existing + 1
// introduced earlier in this batch, not 7 pre-existing.
//
// This exists as its own test — separate from the fact that each site's
// dedicated test continues to pass — because the report requires an
// explicit, self-contained pin of "single Esc unchanged" across every site,
// not just an absence of regressions in scattered tests.
func TestEscEsc_AllExistingEscSitesStillWork(t *testing.T) {
	// RE-11 (fix-e3a): every subtest below now seeds m.entries with a real
	// user turn. Without it, m.rollbackCandidates() is unconditionally empty
	// and the Esc-Esc branch this test claims to guard against (`if items :=
	// m.rollbackCandidates(); m.streamCh == nil && len(items) > 0`) can never
	// fire no matter where it lives or how its double-press window is
	// judged — two independent mutations (widening the window to always-true,
	// and hoisting the whole gesture ahead of every modal check below) both
	// left all 8 subtests green. Seeding a real candidate is what makes "the
	// gesture fired where it should not have" observable.
	withCandidate := func() model {
		m := newModel(&fakeSession{}, "/proj")
		m.entries = []entry{&userEntry{text: "hello"}}
		return m
	}

	// Site 1: pagerVisible (W-E-03 fullscreen pager — new in this batch, NOT
	// a pre-existing site; see RE-18 note on the test's doc comment) — Esc
	// closes the pager.
	t.Run("1_pager", func(t *testing.T) {
		m := withCandidate()
		m.pagerVisible = true
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.False(t, mm.pagerVisible)
	})

	// Site 2: helpVisible (F1 panel) — Esc closes help.
	t.Run("2_help", func(t *testing.T) {
		m := withCandidate()
		m.helpVisible = true
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.False(t, mm.helpVisible)
	})

	// Site 3: restoreSessions picker — Esc dismisses it.
	t.Run("3_restore_picker", func(t *testing.T) {
		m := withCandidate()
		m.restoreSessions = []proto.SessionInfo{{ID: "a"}}
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.Nil(t, mm.restoreSessions)
	})

	// Site 4: action palette (Ctrl+K) — Esc closes it.
	t.Run("4_action_palette", func(t *testing.T) {
		m := withCandidate()
		m.action = &actionState{visible: true}
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.Nil(t, mm.action)
	})

	// Site 5: history search popup (Alt+R) — Esc closes it without touching the draft.
	t.Run("5_history_search", func(t *testing.T) {
		m := withCandidate()
		m.historySearch = &historyState{visible: true, items: []historyItem{{Text: "x"}}}
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.Nil(t, mm.historySearch)
	})

	// Site 6: interactive command picker (/model, /mode, /theme) — Esc closes it.
	t.Run("6_command_picker", func(t *testing.T) {
		m := withCandidate()
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
		m := withCandidate()
		m.toasts.push(toast{Level: "error", Text: "boom"})
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.True(t, ok)
		assert.False(t, mm.hasErrorToast())
		assert.Nil(t, mm.rollback)
	})
	t.Run("7_bottom_level_noop", func(t *testing.T) {
		m := withCandidate()
		mm, ok := handleKey(m, tea.KeyMsg{Type: tea.KeyEscape})
		assert.False(t, ok, "no toast, no modal: single Esc falls through to the default input handler, matching pre-W-E-11 behavior")
		assert.Nil(t, mm.rollback)
	})
}
