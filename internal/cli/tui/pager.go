// Package tui — fullscreen transcript pager (W-E-03).
//
// Ctrl+T takes the whole screen over with the SAME m.viewport the normal
// layout already scrolls (see model.go's pagerVisible doc comment for why a
// second viewport would just be two copies of one truth to keep in sync).
// reflow/renderScreen (view.go) do the actual layout swap and the key
// dispatch lives in handlers.go; this file only holds the pager's own
// render fragment and the two state transitions (close, raw-copy toggle)
// that need more than a one-line assignment.
package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// pagerHint renders the pager's single control-line footer, styled through
// the shared palette (toolMeta — the same grey used for every other
// "(ctrl+o expand)"-style hint in this package, see entries.go) so it
// degrades under NO_COLOR/ANSI exactly like the rest of the screen instead
// of hand-rolling escapes.
//
// RE-20: when !m.mouseEnabled, `r` is a no-op (togglePagerRawCopy's own
// doc comment explains why — nothing was ever turned on to disable). A
// toast can't carry that explanation: renderScreen's pagerVisible branch
// bypasses the toast stack entirely (see its RE-16 comment), so a toast
// pushed from inside the pager would never actually render. This line is
// the only thing drawn alongside the viewport while paging, so it says so
// permanently instead — visible before the user ever presses `r`, not just
// after, and it doesn't need togglePagerRawCopy to return a Cmd for it.
func (m model) pagerHint() string {
	rawHint := "r raw copy mode: off"
	if m.pagerRawCopy {
		rawHint = "r raw copy mode: on"
	}
	if !m.mouseEnabled {
		rawHint = "raw copy mode: unavailable (this terminal has no mouse mode)"
	}
	return toolMeta.Render(fmt.Sprintf(
		"q/Esc/Ctrl+T close · ↑↓ PgUp/PgDn Home/End scroll · %s", rawHint))
}

// closePager exits the pager and, if raw-copy mode had disabled the app's
// own in-app mouse-drag text selection, re-enables it. Returns nil when
// there is nothing to re-enable — raw copy was already off, or mouseEnabled
// is false (see model.go's mouseEnabled doc comment: nothing was ever
// disabled on a terminal that never had mouse mode turned on).
func (m *model) closePager() tea.Cmd {
	wasRaw := m.pagerRawCopy
	m.pagerVisible = false
	m.pagerRawCopy = false
	if wasRaw && m.mouseEnabled {
		return tea.EnableMouseCellMotion
	}
	return nil
}

// togglePagerRawCopy flips raw-copy mode and returns the tea.Cmd that
// actually enables/disables terminal mouse reporting — the mechanism the
// spec's "raw 复制模式" asks for: turning mouse reporting off hands text
// selection back to the terminal itself, working around the app's own
// in-app selection (which needs mouse reporting ON) fighting an
// alt-screen session's native copy/paste.
//
// A no-op (nil Cmd, no state change) when mouseEnabled is false: mouse
// cell-motion was never turned on for this terminal in the first place
// (programOptions gates it on cap.AltScreen, RE-D), so there is nothing to
// disable, and sending the escape sequence anyway is exactly the noise
// RE-D exists to avoid — this is the pager's graceful-degradation path for
// terminals without alt-screen support.
func (m *model) togglePagerRawCopy() tea.Cmd {
	if !m.mouseEnabled {
		return nil
	}
	m.pagerRawCopy = !m.pagerRawCopy
	if m.pagerRawCopy {
		return tea.DisableMouse
	}
	return tea.EnableMouseCellMotion
}
