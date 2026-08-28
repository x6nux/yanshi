package tui

import (
	"strconv"
	"strings"

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

// cmdDistill triggers one memory-consolidation pass over the active session's
// stored memories (A2/W-A-05). The reply (memories_distilled) carries a
// considered/merged summary; applyEvent renders it as a one-line ack. It
// takes no args — the pass always runs over the current session's memories,
// mirroring cmdMemory/cmdFork's no-argument-picks-current-scope shape.
func cmdDistill(m model, _ []string) (tea.Model, tea.Cmd) {
	return m.sendControlFrame(proto.NewDistillMemories())
}

// cmdMemoryClear implements /memory-clear (W-D-12):
//
//	/memory-clear                        — usage
//	/memory-clear <scope> [agent-id]     — confirmation prompt, NOTHING sent
//	/memory-clear <scope> [agent-id] yes — clear_memories frame
//
// A SLASH COMMAND RATHER THAN A TOOL, deliberately. Wiping long-term memory is
// a user's decision about their own data; a model that can reach it can also be
// talked into reaching it, and the blast radius of the "all" scope is every
// memory the project ever accumulated.
//
// The confirmation is cmdDelete's, verbatim in shape: a trailing "yes" token,
// no frame, no model state, no new protocol. That was the existing pattern for
// an irreversible action and inventing a second one would leave two confirmation
// mechanisms to keep correct.
func cmdMemoryClear(m model, args []string) (tea.Model, tea.Cmd) {
	usage := "usage: /memory-clear <session|agent <id>|all> yes"
	reject := func(text string) (tea.Model, tea.Cmd) {
		m.entries = append(m.entries, errorEntry{text: text})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	if len(args) == 0 {
		return reject(usage)
	}
	scope, agentID := args[0], ""
	rest := args[1:]
	switch scope {
	case proto.MemoryClearSession, proto.MemoryClearAll:
	case proto.MemoryClearAgent:
		if len(rest) == 0 {
			return reject(usage)
		}
		agentID, rest = rest[0], rest[1:]
	default:
		return reject(usage)
	}
	if len(rest) != 1 || rest[0] != "yes" {
		return reject("⚠ clearing memories is irreversible. To confirm, run: /memory-clear " +
			strings.TrimSpace(scope+" "+agentID) + " yes")
	}
	return m.sendControlFrame(proto.NewClearMemories(scope, agentID))
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
