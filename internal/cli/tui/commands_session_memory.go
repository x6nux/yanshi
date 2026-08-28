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

// checkpointUsage is the one place the command's shape is written down, so the
// error path and the help entry cannot describe different commands.
const checkpointUsage = "usage: /checkpoint [create [label] | plan <id> <session|memory|files> | " +
	"restore <id> <session|memory|files> yes]"

// cmdCheckpoint implements /checkpoint (W-D-06):
//
//	/checkpoint                          — list recent checkpoints
//	/checkpoint create [label]           — snapshot session + memories + files
//	/checkpoint plan <id> <dimension>    — dry run: what a restore WOULD do
//	/checkpoint restore <id> <dim> yes   — restore, after an automatic snapshot
//
// RESTORE TAKES cmdDelete's "yes", plan does not. The asymmetry is the point of
// having both: a plan is free and reversible, a restore replaces state. The
// server takes its own snapshot first regardless, so a confirmed mistake is
// recoverable — but a mistake nobody confirmed is one nobody knows to recover.
//
// The dimension is validated here as well as on the server. Not defence in
// depth for its own sake: a typo caught locally costs a round trip and prints
// the three valid words, where the same typo reaching a handler that guessed
// would restore the wrong half of the user's state.
func cmdCheckpoint(m model, args []string) (tea.Model, tea.Cmd) {
	reject := func(text string) (tea.Model, tea.Cmd) {
		m.entries = append(m.entries, errorEntry{text: text})
		m.refresh()
		m.viewport.GotoBottom()
		return m, nil
	}
	if len(args) == 0 {
		return m.sendControlFrame(proto.NewCheckpoint(proto.CheckpointList, "", "", ""))
	}
	switch args[0] {
	case proto.CheckpointList:
		return m.sendControlFrame(proto.NewCheckpoint(proto.CheckpointList, "", "", ""))
	case proto.CheckpointCreate:
		return m.sendControlFrame(proto.NewCheckpoint(
			proto.CheckpointCreate, "", "", strings.Join(args[1:], " ")))
	case proto.CheckpointPlan, proto.CheckpointRestore:
		if len(args) < 3 {
			return reject(checkpointUsage)
		}
		id, dim := args[1], args[2]
		if !validCheckpointDim(dim) {
			return reject("unknown dimension " + dim + " — " + checkpointUsage)
		}
		if args[0] == proto.CheckpointPlan {
			if len(args) != 3 {
				return reject(checkpointUsage)
			}
			return m.sendControlFrame(proto.NewCheckpoint(proto.CheckpointPlan, id, dim, ""))
		}
		if len(args) != 4 || args[3] != "yes" {
			return reject("⚠ restoring replaces your current " + dim +
				" state. Preview it with: /checkpoint plan " + id + " " + dim +
				"\nto confirm, run: /checkpoint restore " + id + " " + dim + " yes")
		}
		return m.sendControlFrame(proto.NewCheckpoint(proto.CheckpointRestore, id, dim, ""))
	default:
		return reject(checkpointUsage)
	}
}

// validCheckpointDim reports whether dim is one of the three dimensions.
//
// It reads proto.CheckpointDimensions rather than repeating the three words, so
// a fourth dimension cannot be accepted by the server and rejected here. proto
// and not store: this package is a thin client that only ever sends the word
// over the wire, and reaching into the storage layer for a string constant
// would give the TUI a dependency on how memories are stored.
func validCheckpointDim(dim string) bool {
	for _, d := range proto.CheckpointDimensions() {
		if d == dim {
			return true
		}
	}
	return false
}
