package tui

import (
	"strconv"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/x6nux/yanshi/internal/proto"
)

// cmdMemory shows the active memory file path. It sends get_status; the status
// reply populates m.memoryPath via applyStatus, and the footer renders it. No
// transcript entry — the footer is the canonical surface (avoids duplication).
func cmdMemory(m model, _ []string) (tea.Model, tea.Cmd) {
	return m.sendControlFrame(proto.NewGetStatus())
}

// cmdFork forks the current session. /fork with no arg forks all messages
// (seq=-1); /fork N forks messages[0..N] inclusive. /fork's reply
// (session_forked) carries the new id; applyEvent updates m.sessionID and
// renders an ack. ID-prefix matching is NOT supported in MVP — only an
// optional seq.
func cmdFork(m model, args []string) (tea.Model, tea.Cmd) {
	seq := -1
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n < -1 {
			m.entries = append(m.entries, errorEntry{
				text: "usage: /fork [seq] — seq must be an integer (-1 or >=0)",
			})
			m.refresh()
			m.viewport.GotoBottom()
			return m, nil
		}
		seq = n
	}
	return m.sendControlFrame(proto.NewForkSession(seq))
}

// cmdSide enters an ephemeral side conversation. The server pushes the current
// state and clears sessionID; side turns never write DB. /btw is an alias.
func cmdSide(m model, _ []string) (tea.Model, tea.Cmd) {
	return m.sendControlFrame(proto.NewEnterSide())
}

// cmdMain exits the current side conversation. MVP always discards the side's
// history (restores the snapshotted main-thread state). A future "keep" mode
// (append side's last assistant message to main history) is documented as a
// polish item.
func cmdMain(m model, args []string) (tea.Model, tea.Cmd) {
	if len(args) > 0 {
		// Scope-cut honesty: never interpret `/main keep` as discard.
		m.entries = append(m.entries, errorEntry{
			text: "usage: /main (discard only; keep is not implemented)",
		})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	return m.sendControlFrame(proto.NewExitSide())
}
