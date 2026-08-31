package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/proto"
)

// escDoublePressWindow is how close together two Esc presses must land for
// handlers.go's bottom KeyEscape case to treat them as the W-E-11 "Esc-Esc"
// gesture rather than two independent single presses. 600ms is a common
// double-tap timing (comparable to a double-click), picked so a deliberate
// fast tap-tap reads as one gesture while two presses separated by normal
// reading/thinking time do not.
const escDoublePressWindow = 600 * time.Millisecond

// rollbackItem is one candidate turn shown by the Esc-Esc rollback picker
// (W-E-11): rolling back to it forks the session to just before this user
// message and refills its text into the editor. entryIndex is this
// userEntry's position in m.entries at the moment the picker opened, used by
// applyEvent's case "session_forked" to truncate the LOCAL transcript to
// match the server's shorter forked history once the reply arrives.
type rollbackItem struct {
	text       string
	turnsBack  int // 1-based: how many user turns back from the latest
	entryIndex int
}

// rollbackState is the Esc-Esc rollback picker's popup state. Modeled on
// historyState (history.go): a cursor over a fixed candidate list built once
// when the picker opens (rollbackCandidates), not a live search — there is
// nothing to type here, only to navigate.
type rollbackState struct {
	items  []rollbackItem
	cursor int
}

// rollbackCandidates walks m.entries from the end and returns one
// rollbackItem per user turn, most-recent first (index 0 == the most recent
// turn, turnsBack=1). A nil/empty result means there is nothing to roll back
// to (e.g. a brand new session) — handlers.go's Esc-Esc branch checks this
// before opening the picker.
func (m model) rollbackCandidates() []rollbackItem {
	var items []rollbackItem
	turnsBack := 0
	for i := len(m.entries) - 1; i >= 0; i-- {
		ue, ok := m.entries[i].(*userEntry)
		if !ok {
			continue
		}
		turnsBack++
		items = append(items, rollbackItem{text: ue.text, turnsBack: turnsBack, entryIndex: i})
	}
	return items
}

// rollbackConfirm sends the fork_session{turns_back} request for the
// selected candidate and stashes its text + local transcript position so
// applyEvent's case "session_forked" can truncate m.entries and refill the
// editor once the server replies. Mirrors cmdFork's sendControlFrame call
// (commands_session_memory.go) — same "reuse fork_session, don't invent a
// new frame" shape, just with TurnsBack instead of a caller-supplied Seq
// (see proto.NewForkSessionRollback's doc comment).
func (m model) rollbackConfirm() (tea.Model, tea.Cmd) {
	sel := m.rollback.items[m.rollback.cursor]
	m.rollback = nil
	m.pendingRollback = true
	m.pendingRollbackText = sel.text
	m.pendingRollbackIndex = sel.entryIndex
	m.reflow()
	return m.sendControlFrame(proto.NewForkSessionRollback(sel.turnsBack))
}

// rollbackPopup renders the Esc-Esc rollback picker: one line per candidate
// user turn, most-recent first, modeled on historyPopup's layout
// (history.go) — same 8-row scrolling window so the picker's height
// contribution to reflow stays predictable and bounded regardless of session
// length. Returns "" when the picker is closed (m.rollback == nil).
func (m model) rollbackPopup() string {
	if m.rollback == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(paletteStyle.Render(fmt.Sprintf("Rollback — %d turn(s)", len(m.rollback.items))) + "\n")
	start := 0
	const maxRows = 8
	if m.rollback.cursor >= maxRows {
		start = m.rollback.cursor - maxRows + 1
	}
	end := start + maxRows
	if end > len(m.rollback.items) {
		end = len(m.rollback.items)
	}
	for i := start; i < end; i++ {
		it := m.rollback.items[i]
		preview := strings.ReplaceAll(strings.TrimSpace(it.text), "\n", " ")
		preview = truncateToast(preview, max(8, m.width-16))
		line := fmt.Sprintf("  -%d  %s", it.turnsBack, preview)
		if i == m.rollback.cursor {
			line = selPaletteStyle.Render("▶ " + strings.TrimLeft(line, " "))
		} else {
			line = paletteStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(toolMeta.Render("  ↑↓ navigate · enter fork & refill · esc cancel") + "\n")
	return inputBorder.Render(strings.TrimRight(b.String(), "\n"))
}
