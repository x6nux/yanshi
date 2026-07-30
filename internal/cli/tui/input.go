package tui

import "github.com/charmbracelet/bubbles/textarea"

func newInput() textarea.Model {
	ta := textarea.New()
	// Default 1 line; growInput() (called from Update) raises this with content
	// up to max 3 lines, and the textarea scrolls internally past that. Keeping
	// the default small maximizes transcript space when the input is empty.
	ta.SetHeight(1)
	ta.Placeholder = "Send a message…  (Enter = send · Ctrl+Enter = newline · Ctrl+C = cancel · wheel/PgUp/PgDn = scroll · Shift+drag = select)"
	ta.Focus()
	return ta
}
