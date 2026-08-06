package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestHelpPopupFitsTheTerminalAndScrolls pins the defect that made F1 useless
// on an ordinary terminal.
//
// helpPopup rendered every entry in one go -- 60-odd lines across four
// sections. view.go accounts for the block's height but does not clip it, so
// the final trim happens in bubbletea's renderer, which keeps the LAST r.height
// lines. The title, the "Commands:" section header and the first ~35 commands
// were therefore off the top of the screen, with no way to scroll to them: the
// panel absorbs every key except printable search characters.
//
// Both halves are asserted because either alone is satisfiable by a broken
// implementation: clipping without a cursor loses the tail forever, and a
// cursor without clipping keeps the original bug.
//
// ledger: C2/UX2#3 内容自动生成不漂移
func TestHelpPopupFitsTheTerminalAndScrolls(t *testing.T) {
	m := newModel(&recordingSession{}, "/proj")
	m.height = 40
	m.width = 120
	m.helpVisible = true

	total := len(m.collectHelpEntries())
	if total < 40 {
		t.Fatalf("only %d help entries: this test needs more entries than fit "+
			"on screen or it proves nothing", total)
	}

	out := stripANSI(m.helpPopup())
	lines := strings.Count(out, "\n") + 1
	if lines > m.height {
		t.Errorf("help panel is %d lines on a %d-line terminal: bubbletea keeps the "+
			"LAST height lines, so the title and the first entries fall off the top",
			lines, m.height)
	}
	if !strings.Contains(out, "Help") {
		t.Error("the panel title is not in the rendered output")
	}

	first := m.collectHelpEntries()[0]
	if !strings.Contains(out, first.Label) {
		t.Errorf("the FIRST help entry %q is not visible at cursor 0", first.Label)
	}

	// Scrolling must reach the tail. Drive it through the key handler, because
	// the panel absorbs keys and a cursor nothing can move is not a cursor.
	last := m.collectHelpEntries()[total-1]
	mm := m
	for i := 0; i < total+5; i++ {
		next, _, handled := mm.handleKeyMsg(tea.KeyMsg{Type: tea.KeyDown})
		if !handled {
			t.Fatalf("the help panel does not handle Down: it absorbs every other key, "+
				"so nothing can scroll it (iteration %d)", i)
		}
		mm = next
	}
	tail := stripANSI(mm.helpPopup())
	if !strings.Contains(tail, last.Label) {
		t.Errorf("scrolling to the bottom never reveals the LAST entry %q", last.Label)
	}
	if got := strings.Count(tail, "\n") + 1; got > m.height {
		t.Errorf("scrolled panel is %d lines on a %d-line terminal", got, m.height)
	}
}
