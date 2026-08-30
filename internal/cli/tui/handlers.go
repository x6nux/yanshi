// Package tui — Bubble Tea TUI model for the Yanshi LLM agent.
//
// This file holds handler methods extracted from model.go's Update method to
// keep the main Update dispatch below 1000 pure code lines (Phase B of Task 6).
//
// No behavioral changes; this is a pure extract-method refactoring verified by
// the golden test (update_golden_test.go).
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/keymap"
	"github.com/x6nux/yanshi/internal/proto"
)

// handleKeyMsg is the extracted tea.KeyMsg case from Update. It returns the
// updated model, any command, and a boolean indicating whether the key was
// fully handled (true) or should fall through to the default input handler
// (false). This enables the golden test to verify zero behavioral change.
//
// nolint:gocyclo // this is a 1:1 extraction of known hotkey dispatch logic
func (m model) handleKeyMsg(msg tea.KeyMsg) (model, tea.Cmd, bool) {
	// W-E-03: fullscreen pager (Ctrl+T) is modal — checked FIRST, ahead of
	// every other modal below, because it owns the whole screen: a stray
	// Up/Down must scroll the pager, not move a picker cursor that cannot
	// even be on screen right now. Ctrl+T itself is deliberately NOT handled
	// in this block: it falls through (empty case, no return) all the way to
	// the shared open/close toggle in the switch at the bottom of this
	// function — the same pattern F1/helpVisible uses, where the modal's own
	// hotkey closes it via the SAME case that opened it, not a copy living
	// inside the modal's dedicated block.
	if m.pagerVisible {
		switch msg.Type {
		case tea.KeyCtrlT:
			// Falls through — see the comment above.
		case tea.KeyEscape:
			cmd := m.closePager()
			m.reflow()
			return m, cmd, true
		case tea.KeyHome:
			m.viewport.GotoTop()
			return m, nil, true
		case tea.KeyEnd:
			m.viewport.GotoBottom()
			return m, nil, true
		case tea.KeyRunes:
			switch string(msg.Runes) {
			case "q":
				cmd := m.closePager()
				m.reflow()
				return m, cmd, true
			case "r":
				return m, m.togglePagerRawCopy(), true
			}
			// Any other rune: forward to the viewport, which owns its own
			// vi-style bindings (j/k/f/b/u/d — bubbles/viewport's
			// DefaultKeyMap) rather than re-deriving that keymap here.
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd, true
		default:
			// ↑↓ PgUp/PgDn and anything else the viewport recognises;
			// unrecognised key types are a harmless no-op cmd. Absorbed
			// either way so nothing leaks into a textarea the pager has
			// hidden from view.
			var cmd tea.Cmd
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd, true
		}
	}
	// C2 — UX2: help panel is read-only modal. Consume ↑↓/Enter/Esc +
	// printable/backspace so query search works without leaking keystrokes
	// into the textarea. F1/Esc close; everything else falls through after
	// the switch (which returns immediately for unknown keys).
	if m.helpVisible {
		switch msg.Type {
		case tea.KeyEscape:
			m.helpVisible = false
			m.helpQuery = ""
			m.helpCursor = 0
			m.helpRendered = ""
			m.reflow()
			return m, nil, true
		case tea.KeyDown, tea.KeyUp, tea.KeyPgDown, tea.KeyPgUp:
			// Scrolling. The panel is modal and absorbs everything, so without
			// these four the window computed in renderHelp could never move
			// and clipping alone would just lose the tail instead of the head.
			m.helpCursor = moveHelpCursor(m.helpCursor, len(m.rankedHelpEntries()), msg.Type)
			m.helpRendered = m.helpPopup()
			m.reflow()
			return m, nil, true
		case tea.KeyRunes:
			m.helpQuery += string(msg.Runes)
			// A new query produces a different list; an old cursor pointing
			// past its end would render an empty window.
			m.helpCursor = 0
			m.helpRendered = m.helpPopup()
			m.reflow()
			return m, nil, true
		case tea.KeyBackspace:
			m.helpQuery = dropLastRune(m.helpQuery)
			m.helpCursor = 0
			m.helpRendered = m.helpPopup()
			m.reflow()
			return m, nil, true
		default:
			// F1 toggles close below; all other keys absorbed while modal.
		}
	}
	// Restore session picker: capture Up/Down/Enter/Escape.
	if len(m.restoreSessions) > 0 {
		switch msg.Type {
		case tea.KeyUp:
			if m.restoreCursor > 0 {
				m.restoreCursor--
			}
			return m, nil, true
		case tea.KeyDown:
			if m.restoreCursor < len(m.restoreSessions)-1 {
				m.restoreCursor++
			}
			return m, nil, true
		case tea.KeyEnter:
			sel := m.restoreSessions[m.restoreCursor]
			m.restoreSessions = nil
			m.entries = append(m.entries, &restoreEntry{})
			m.refresh()
			m.viewport.GotoBottom()
			mm, cmd := m.sendControlFrame(proto.NewRestoreSession(sel.ID))
			return mm.(model), cmd, true
		case tea.KeyEscape:
			m.restoreSessions = nil
			m.reflow()
			return m, nil, true
		}
		// All other keys are consumed while picker is active.
		return m, nil, true
	}

	// W-E-11: Esc-Esc rollback picker. Capture Up/Down/Enter/Escape like the
	// restore-session picker above; all other keys are swallowed while it is
	// open so nothing leaks into the textarea underneath it, and — because
	// this block runs before the bottom-level KeyEscape case below — no
	// other modal's own hotkey (Ctrl+T/E/S/K, F1, Alt+R, …) can open while
	// the picker owns the keyboard.
	if m.rollback != nil {
		switch msg.Type {
		case tea.KeyUp:
			if m.rollback.cursor > 0 {
				m.rollback.cursor--
			}
			return m, nil, true
		case tea.KeyDown:
			if m.rollback.cursor < len(m.rollback.items)-1 {
				m.rollback.cursor++
			}
			return m, nil, true
		case tea.KeyEnter:
			mm, cmd := m.rollbackConfirm()
			return mm.(model), cmd, true
		case tea.KeyEscape:
			m.rollback = nil
			m.reflow()
			return m, nil, true
		}
		// All other keys are consumed while the picker is active.
		return m, nil, true
	}

	// C2 — UX1: action palette is modal when open. Capture ↑↓/Enter/Esc
	// + printable runes / backspace so the user can search, navigate, and
	// confirm without the textarea eating the keystrokes. All other keys
	// fall through to existing handlers (so Ctrl+C still quits, etc.).
	if m.action != nil {
		switch msg.Type {
		case tea.KeyEscape:
			m.closeActionPopup()
			m.reflow()
			return m, nil, true
		case tea.KeyUp:
			m.actionMove(-1)
			return m, nil, true
		case tea.KeyDown:
			m.actionMove(1)
			return m, nil, true
		case tea.KeyEnter:
			mm, cmd := m.actionConfirm()
			out := mm.(model)
			out.reflow()
			return out, cmd, true
		case tea.KeyRunes:
			m.action.query += string(msg.Runes)
			m.action.items = m.rankedActions()
			m.action.cursor = 0
			return m, nil, true
		case tea.KeyBackspace:
			m.action.query = dropLastRune(m.action.query)
			m.action.items = m.rankedActions()
			m.action.cursor = 0
			return m, nil, true
		}
	}

	// C2 — UX6: history search popup is modal when open. ↑↓ navigate,
	// Enter restores the selected prompt to the textarea (replacing the
	// current draft), Esc closes without modifying the draft, Runes/
	// Backspace update the fuzzy query.
	if m.historySearch != nil {
		switch msg.Type {
		case tea.KeyEscape:
			m.historySearch = nil
			m.reflow()
			return m, nil, true
		case tea.KeyUp, tea.KeyDown:
			n := len(m.historySearch.items)
			if n > 0 {
				delta := 1
				if msg.Type == tea.KeyUp {
					delta = -1
				}
				m.historySearch.cursor = (m.historySearch.cursor + delta + n) % n
			}
			return m, nil, true
		case tea.KeyEnter:
			if n := len(m.historySearch.items); n > 0 {
				m.input.SetValue(m.historySearch.items[m.historySearch.cursor].Text)
				m.growInput()
			}
			m.historySearch = nil
			m.reflow()
			return m, nil, true
		case tea.KeyRunes:
			m.historySearch.query += string(msg.Runes)
			m.historySearch.cursor = 0
			m.refreshHistorySearch()
			m.reflow()
			return m, nil, true
		case tea.KeyBackspace:
			m.historySearch.query = dropLastRune(m.historySearch.query)
			m.historySearch.cursor = 0
			m.refreshHistorySearch()
			m.reflow()
			return m, nil, true
		default:
			return m, nil, true
		}
	}

	// C2 — UX6: Alt+R opens the history search popup. Bubble Tea surfaces
	// Alt+R as a KeyRunes with Alt=true and a single 'r' rune.
	if msg.Alt && msg.Type == tea.KeyRunes && len(msg.Runes) == 1 &&
		(msg.Runes[0] == 'r' || msg.Runes[0] == 'R') {
		m.historySearch = &historyState{visible: true}
		m.refreshHistorySearch()
		m.reflow()
		return m, nil, true
	}
	// C2 — UX6: Alt+↑ pops the last queued message back into the editor.
	// Must be intercepted BEFORE the main KeyUp handler (which moves into
	// the textarea cursor). No-op when the queue is empty.
	if msg.Alt && msg.Type == tea.KeyUp {
		if m.editLastQueued() {
			return m, m.pushToast("info", "last queued message moved back to editor"), true
		}
		return m, nil, true
	}

	// Interactive command picker (/model, /mode, /theme): capture
	// Up/Down/Enter/Escape. Also accept j/k as vim-style navigation
	// for terminals that don't deliver arrow keys reliably (e.g. Git
	// Bash / MinTTY on Windows).
	//
	// Navigation wraps cyclically: pressing Down at the last item
	// wraps to the first, and pressing Up at the first wraps to the
	// last, so the user never hits a dead end — handy for long model
	// lists where overshooting means scrolling back through all items.
	if m.pickerKind != "" {
		n := len(m.pickerItems)
		switch msg.Type {
		case tea.KeyUp:
			m.pickerCursor = (m.pickerCursor - 1 + n) % n
			return m, nil, true
		case tea.KeyDown:
			m.pickerCursor = (m.pickerCursor + 1) % n
			return m, nil, true
		case tea.KeyEnter:
			mm, cmd := m.pickerConfirm()
			return mm.(model), cmd, true
		case tea.KeyEscape:
			m.pickerKind = ""
			m.pickerItems = nil
			m.reflow()
			return m, nil, true
		case tea.KeyRunes:
			switch string(msg.Runes) {
			case "j":
				m.pickerCursor = (m.pickerCursor + 1) % n
				return m, nil, true
			case "k":
				m.pickerCursor = (m.pickerCursor - 1 + n) % n
				return m, nil, true
			}
		}
		// All other keys are consumed while picker is active.
		return m, nil, true
	}

	if m.yoloConfirm > 0 && msg.Type != tea.KeyEnter && msg.Type != tea.KeyShiftTab {
		m.yoloConfirm = 0
		return m, nil, true
	}

	// C15: vim modal editing, ahead of everything else — it owns raw
	// keystrokes when on, and nil (the default) makes this a single nil check.
	if action, consumed := m.vimKey(msg); consumed {
		if action != keymap.ActionNone {
			if mm, cmd, handled := m.runKeyAction(action); handled {
				return mm.(model), cmd, true
			}
		}
		// Consumed with no action is a mode transition (i/a/o/v/Esc). Swallow
		// it: letting it fall through types the letter into the textarea.
		return m, nil, true
	}

	// C15: user keybindings, consulted before the built-in switch below.
	//
	// Only keys the user REBOUND are routed here. keymap's built-in defaults
	// deliberately mirror what the switch already does (enter/ctrl+enter/
	// ctrl+c/pgup/pgdown/f1), so letting defaults through would change nothing
	// while duplicating five branches; letting them through and then getting
	// one of them subtly wrong is the realistic failure. remappedKey therefore
	// returns an action only when this key is NOT one the switch already owns.
	//
	// Without this the whole keymap package is inert: tui.bindings parses,
	// validates, produces diagnostics, and never affects a single keystroke.
	if action, ok := m.remappedKey(msg); ok {
		if mm, cmd, handled := m.runKeyAction(action); handled {
			return mm.(model), cmd, true
		}
	}

	switch msg.Type {
	case tea.KeyEscape:
		// W-E-11: a second Esc landing within escDoublePressWindow of the
		// first opens the rollback picker instead of repeating whatever a
		// lone Esc does. A SINGLE Esc is byte-for-byte unaffected: m.lastEsc
		// starts zero-valued (and is re-zeroed after every consumed pair),
		// so the first press of any Esc-Esc pair always takes the
		// isDouble==false branch below — it only records the timestamp and
		// falls through to the pre-existing toast-dismiss/no-op logic.
		now := time.Now()
		isDouble := !m.lastEsc.IsZero() && now.Sub(m.lastEsc) < escDoublePressWindow
		m.lastEsc = now
		if isDouble {
			m.lastEsc = time.Time{} // consume the pair; a third press starts fresh
			// RE-19: same transport gate cmdDiff (commands.go) uses for
			// list_workspace_diff, and for the same reason — fork_session
			// is a control frame SSE has no server-side handling for (SSE
			// is stateless; there is no persistent session to fork). Gated
			// BEFORE opening the picker, not after the user picks a turn
			// to roll back to: RE-13 already stops a rejected fork's
			// "error" reply from leaking pendingRollback state forever,
			// but that path still walks the user through picking a
			// candidate only to fail at the end with a transport-generic
			// message. This mirrors /diff's UX exactly — refuse
			// immediately, name the transport, name why — instead of
			// leaving Esc-Esc as the one gesture in this package that
			// finds out the hard way.
			if m.sess.Mode() == "sse" {
				m.entries = append(m.entries, errorEntry{
					text: "rollback requires the WebSocket transport (SSE is stateless)",
				})
				m.refresh()
				m.viewport.GotoBottom()
				return m, nil, true
			}
			if items := m.rollbackCandidates(); m.streamCh == nil && len(items) > 0 {
				m.rollback = &rollbackState{items: items}
				m.reflow()
				return m, nil, true
			}
			// Nothing to roll back to (no user turns yet), or a turn is
			// streaming (mirrors /fork's own mid-stream block — submit()
			// drops slash commands while m.streamCh != nil): fall through to
			// ordinary single-Esc handling instead of silently swallowing
			// the keypress.
		}
		// C2 — UX7: dismiss the most-recent error toast (if any) on Esc.
		// Returns immediately when there is no error toast so Esc keeps
		// its existing meaning elsewhere (close picker, etc.). The toast
		// tick handler prunes the now-empty queue and stops the tick on
		// the next 500ms cadence.
		if m.hasErrorToast() {
			m.toasts.dismissLastError()
			m.reflow()
			return m, nil, true
		}
	case tea.KeyShiftTab:
		mm, cmd := m.cycleMode()
		return mm.(model), cmd, true
	case tea.KeyCtrlC:
		// First Ctrl-C during a stream cancels the turn; a second press
		// (or any Ctrl-C when idle) quits.
		if m.streamCh != nil && !m.canceling {
			_ = m.sess.CancelCurrent()
			m.canceling = true
			return m, nil, true
		}
		return m, tea.Quit, true
	case tea.KeyPgUp, tea.KeyPgDown:
		// Page scrolling: route to the viewport (before the textarea) so the
		// transcript scrolls. Arrow keys (Up/Down/Left/Right) are deliberately
		// left to the textarea for cursor movement within pasted multi-line
		// input; PgUp/PgDn + mouse wheel cover scrolling.
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		// Force a full repaint: the diff renderer can leave the footer/input
		// stale after a viewport scroll, so the bottom disappears until
		// something else redraws it.
		return m, tea.Batch(cmd, tea.Repaint), true
	case tea.KeyUp:
		// A pending permission prompt is modal: Up/Down move its option
		// selection. Otherwise the command palette navigates; otherwise
		// fall through to the textarea for cursor movement.
		if m.pendingPermission() != nil {
			m.permMove(-1)
			return m, nil, true
		}
		if m.paletteOpen() {
			m.paletteMove(-1)
			return m, nil, true
		}
	case tea.KeyDown:
		if m.pendingPermission() != nil {
			m.permMove(1)
			return m, nil, true
		}
		if m.paletteOpen() {
			m.paletteMove(1)
			return m, nil, true
		}
	case tea.KeyTab:
		// Tab completes the selected command name into the input (and closes
		// the palette) when the palette is open.
		if m.pendingPermission() != nil {
			return m, nil, true // modal: Tab does nothing
		}
		if m.paletteOpen() {
			m.paletteComplete()
			m.reflow()
			return m, nil, true
		}
	case tea.KeyEnter:
		if m.yoloConfirm > 0 {
			if m.yoloConfirm >= 2 {
				m.yoloConfirm = 0
				m.permMode = guard.ModeYOLO
				mm, cmd := m.sendMode()
				return mm.(model), cmd, true
			}
			m.yoloConfirm = 2
			return m, nil, true
		}
		// A pending permission prompt: Enter confirms the highlighted option.
		if m.pendingPermission() != nil {
			m.respondPermission(permissionOptions(m.pendingPermission())[m.permSel].decision)
			m.reflow()
			return m, nil, true
		}
		// Enter sends the turn. If the command palette is open and no args
		// are typed yet, run the SELECTED command (so Up/Down + Enter picks
		// a command without typing its full name); otherwise submit the
		// input as typed.
		if m.paletteOpen() && !strings.Contains(strings.TrimPrefix(m.input.Value(), "/"), " ") {
			sel := m.paletteItems[m.paletteSel]
			m.input.SetValue("/" + sel.name)
			m.paletteItems = nil
		}
		mm, cmd := m.submit()
		return mm.(model), cmd, true
	case tea.KeyCtrlEnter:
		// Ctrl+Enter inserts a newline at the cursor for manual multi-line
		// input (the bubbletea fork makes it detectable on Windows).
		m.input.InsertString("\n")
		m.growInput()
		m.reflow()
		return m, nil, true
	case tea.KeyCtrlV:
		// Tier G entry-A: paste a clipboard IMAGE as an attachment for the
		// next turn. Ctrl+V is also the terminal's ordinary text-paste chord,
		// so a clipboard without an image must be a SILENT no-op: no toast,
		// no error, and the key falls through to the textarea handler. Only a
		// real attach reports back (see clipboard.go).
		if m.attachClipboardImage() {
			m.reflow()
			return m, m.pushToast("info", fmt.Sprintf("image attached (%d pending)", len(m.pendingImages))), true
		}
	case tea.KeyCtrlO:
		// ctrl+o expands/collapses the most recent collapsible block — a
		// thinking block, a resolved tool result, or a long user paste (the
		// unified expand binding; the earlier bare-"e" was removed because it
		// shadowed typing). A no-op when there's nothing to expand.
		if m.toggleExpand() {
			m.refresh()
		}
		return m, nil, true
	case tea.KeyCtrlK:
		// C2 — UX1: global action palette. Toggle: if already open, close;
		// otherwise open with the current model cache, then fire a list_models
		// refresh (real reply clears actionLoadingModels in applyEvent).
		if m.action != nil {
			m.closeActionPopup()
			m.reflow()
			return m, nil, true
		}
		m.openActionPopup()
		m.reflow()
		// applyEvent cannot send frames (its contract is "render a reply"),
		// so the list_models request goes through Update here. Do NOT return
		// a local "loaded" Msg — wait for the real reply, then applyEvent's
		// case "models" populates m.models + refreshes the popup in place.
		// Two refreshes, one per cached source. Both caches go stale for the
		// same reason (another window changed something) and neither can be
		// fetched lazily: the palette renders synchronously.
		mm, modelsCmd := m.sendControlFrame(proto.NewListModels())
		m = mm.(model)
		mm2, sessionsCmd := m.sendControlFrame(proto.NewSessionList())
		return mm2.(model), tea.Batch(modelsCmd, sessionsCmd), true
	case tea.KeyCtrlS:
		// C2 — UX5: stash current textarea as a draft. No-op when any popup
		// is open (the popup owns keystrokes; /stash handles its own saves).
		// Empty textarea warns; otherwise push + clear + persist via the
		// single-worker saveQueue (never bypass with a direct saveCmd Msg).
		if m.paletteOpen() || m.pickerKind != "" || m.action != nil || m.helpVisible {
			return m, nil, true
		}
		text := m.input.Value()
		if strings.TrimSpace(text) == "" {
			return m, m.pushToast("warn", "nothing to stash (textarea is empty)"), true
		}
		m.stash.Push(text)
		m.input.Reset()
		m.growInput()
		m.reflow()
		m.enqueueSave(m.stash.Save)
		return m, m.pushToast("info", "draft stashed"), true
	case tea.KeyCtrlE:
		// W-E-12: open $VISUAL/$EDITOR (or the platform default) on the
		// current input text. Blocked while a modal owns the keyboard,
		// matching Ctrl+S's guard — the editor takes over the whole
		// terminal, which no popup can survive being drawn under.
		//
		// pendingPermission() is in this guard (RE-16) even though it isn't
		// one of the modal-priority `if` blocks above: the permission popup
		// is rendered as an ordinary block inside renderScreen, not behind
		// its own early-return block, so nothing else stopped Ctrl+E from
		// shelling out to $EDITOR over an unresolved permission prompt —
		// the prompt would still be queued in m.pendingPermissions but
		// invisible and unanswerable until the editor exited.
		if m.paletteOpen() || m.pickerKind != "" || m.action != nil || m.helpVisible || m.pendingPermission() != nil {
			return m, nil, true
		}
		cmd, err := startExternalEditor(m.input.Value())
		if err != nil {
			return m, m.pushToast("warn", "could not start editor: "+err.Error()), true
		}
		return m, cmd, true
	case tea.KeyCtrlT:
		// W-E-03: fullscreen transcript pager. Reached on close too — the
		// pagerVisible block above deliberately lets Ctrl+T fall through to
		// here so open and close share one case, mirroring F1/help.
		if m.pagerVisible {
			cmd := m.closePager()
			m.reflow()
			return m, cmd, true
		}
		// Blocked while another modal owns the keyboard, matching Ctrl+E's
		// guard — the pager takes over the whole screen, which no popup can
		// survive being drawn under.
		//
		// pendingPermission() is in this guard for the same reason it's in
		// Ctrl+E's (RE-16): the permission popup has no early-return modal
		// block of its own, so without this check the pager could take the
		// whole screen over a request the user still has to answer — the
		// prompt keeps waiting in m.pendingPermissions but is drawn nowhere
		// and unreachable by `y`/`a`/`n` until Ctrl+T is pressed again.
		if m.paletteOpen() || m.pickerKind != "" || m.action != nil || m.helpVisible || m.pendingPermission() != nil {
			return m, nil, true
		}
		m.pagerVisible = true
		m.reflow()
		return m, nil, true
	case tea.KeyF1:
		// C2 — UX2: toggle the F1 help panel. If already open, close it
		// (acts as Esc); otherwise clear any prior query and open fresh.
		if m.helpVisible {
			m.helpVisible = false
			m.helpQuery = ""
			m.helpRendered = ""
		} else {
			m.helpVisible = true
			m.helpQuery = ""
			m.helpRendered = m.helpPopup()
		}
		m.reflow()
		return m, nil, true
	case tea.KeyRunes:
		// Permission prompt (modal): y/a/n resolve it (in addition to the
		// popup's ↑↓ + enter). Checked before text input so the keystroke
		// is never typed into the box. Unknown keys are ignored while the
		// prompt is up.
		if m.pendingPermission() != nil {
			switch string(msg.Runes) {
			case "y":
				m.respondPermission("allow")
			case "a":
				m.respondPermission("always_allow")
			case "n":
				m.respondPermission("deny")
			default:
				return m, nil, true
			}
			m.reflow()
			return m, nil, true
		}
		// Paste detection (B9/T11): bracketed paste sets msg.Paste (the
		// terminal wraps the pasted bytes in ESC[200~ … ESC[201~ and
		// bubbletea surfaces that flag on the KeyRunes it produces). Some
		// terminals/IMEs don't emit the bracket sequence, so a single
		// KeyRunes with >50 runes is treated as a bulk drop too. Either
		// signal marks the in-progress input as pasted, which submit()
		// threads into userEntry.pasted so a long paste collapses to
		// "[粘贴 #id]" while a long TYPED message renders in full.
		if msg.Paste || len(msg.Runes) > bulkPasteRuneThreshold {
			m.inputPasted = true
		}
		// (Letters are typed into the input — no bare-letter hotkeys, since
		// they would shadow typing. Block expand is unified on ctrl+o:
		// thinking, tool results, and long pastes.)
	}

	// Not fully handled — fall through to the default input.Update path.
	return m, nil, false
}
