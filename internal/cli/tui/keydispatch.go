package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/keymap"
)

// builtinKeys are the key messages the hardcoded switch in handlers.go already
// owns with the same meaning keymap assigns them by default.
//
// A rebinding of one of these is still honoured — the user's key maps to some
// action and gets routed — but the DEFAULT binding is left to the switch, so
// wiring the keymap changes nothing for anyone who did not configure bindings.
// This is the conservative half of a change that could otherwise silently move
// every key in the product.
var builtinKeys = map[tea.KeyType]bool{
	tea.KeyEnter:  true,
	tea.KeyCtrlC:  true,
	tea.KeyPgUp:   true,
	tea.KeyPgDown: true,
	tea.KeyF1:     true,
}

// remappedKey reports the action a user-configured binding assigns to msg.
//
// It returns false for keys that carry their default binding, so the built-in
// switch keeps handling them. It also returns false when the resolved action
// is one this dispatcher cannot perform, rather than swallowing the key: a
// binding that silently eats a keystroke is worse than one that does nothing.
func (m model) remappedKey(msg tea.KeyMsg) (keymap.Action, bool) {
	if m.keys == nil || builtinKeys[msg.Type] {
		return keymap.ActionNone, false
	}
	action := m.keys.Lookup(msg)
	if action == keymap.ActionNone {
		return keymap.ActionNone, false
	}
	return action, true
}

// runKeyAction performs one keymap action, reusing the same code paths the
// built-in switch uses. handled=false means the action has no implementation
// here and the key should fall through.
func (m model) runKeyAction(action keymap.Action) (tea.Model, tea.Cmd, bool) {
	switch action {
	case keymap.ActionCancel:
		// Same two-stage semantics as Ctrl+C: cancel an in-flight turn, quit
		// when idle. Duplicating the rule rather than sharing it would let the
		// rebound key and the default drift apart.
		if m.streamCh != nil && !m.canceling {
			_ = m.sess.CancelCurrent()
			m.canceling = true
			return m, nil, true
		}
		return m, tea.Quit, true
	case keymap.ActionQuit:
		return m, tea.Quit, true
	case keymap.ActionScrollUp:
		m.viewport.PageUp()
		return m, nil, true
	case keymap.ActionScrollDown:
		m.viewport.PageDown()
		return m, nil, true
	case keymap.ActionHelp:
		m.helpVisible = !m.helpVisible
		if !m.helpVisible {
			m.helpQuery = ""
		}
		m.reflow()
		return m, nil, true
	case keymap.ActionClear:
		mm, cmd := m.runCommand("/clear")
		return mm, cmd, true
	case keymap.ActionCommandMode:
		m.input.SetValue("/")
		m.refresh()
		return m, nil, true
	}
	// send and newline are not routed here: both depend on the input widget's
	// current state (queue mode, yolo confirmation, palette selection) and the
	// switch already resolves that in order. Rebinding them is accepted by the
	// keymap and takes effect through the switch's own Enter handling.
	return m, nil, false
}
