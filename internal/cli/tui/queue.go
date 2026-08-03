package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// C07 — message queue dispatch. This file holds the enqueue/drain core that
// turns the previously-stubbed model.msgQueue + model.queueMode into working
// behavior. The queue is PURE CLIENT STATE: the server only sees a sequence of
// user_message turns, so all three strategies are implemented in the TUI.
//
// submit() routes a mid-turn submission to enqueue (see model.go); Update's
// streamMsg case calls drainQueue on a "done" event. dispatchSend is the shared
// "start one user turn" kernel reused by the manual idle path and the drain.
//
// The queue's VISIBLE presence is the queuePreview block rendered above the
// input composer (see view.go) — NOT a transcript marker. Keeping it out of the
// transcript means it never reads as already-sent content: it sits above the
// composer, clearly pending, mirroring codex's PendingInputPreview. The live
// count also rides in the footer's statusHeader segment.

// dispatchSend is the shared "send text as a new user turn" core used by both
// the manual submit path and the queue drain. It appends a userEntry, resets
// pending assistant text, starts the activity status line, opens the stream,
// and arms waitForEvent + the glyph animation tick. Factored out so manual and
// drained turns share identical plumbing (DRY).
func (m model) dispatchSend(text string, pasted bool) (model, tea.Cmd) {
	m.entries = append(m.entries, &userEntry{text: text, pasteID: pasteID(text), pasted: pasted})
	m.pending = ""
	m.startTurn()
	// C2 — UX6: history records ONLY actually-sent prompts. Recording here
	// (not in submit/enqueue) means queued-but-unsent text stays editable via
	// Alt+↑ without polluting history, and only truly-dispatched text enters
	// the FIFO. Persistence is threaded through the single-worker saveQueue.
	if m.history != nil {
		m.history.Add(text)
	}
	// Tier G entry-A: a turn with clipboard attachments goes out as a full
	// user_message FRAME (buildSendFrame) rather than through the text-only
	// Send. Draining here — the single point where a turn actually leaves the
	// TUI — means every send path (manual submit and queue drain alike) both
	// carries the images once and clears them once.
	if imgs := m.takePendingImages(); len(imgs) > 0 {
		m.streamCh = m.sess.SendFrame(buildSendFrame(text, imgs))
	} else {
		m.streamCh = m.sess.Send(text)
	}
	if m.history != nil {
		m.enqueueSave(m.history.Save)
	}
	m.reflow()
	m.viewport.GotoBottom()
	return m, tea.Batch(m.waitForEvent(), activityTick())
}

// editLastQueued (C2 — UX6 Alt+↑) pops the most recently queued message back
// into the textarea and removes it from the queue. Returns false when the
// queue is empty (no-op — never overwrites the user's current draft).
func (m *model) editLastQueued() bool {
	if len(m.msgQueue) == 0 {
		return false
	}
	last := m.msgQueue[len(m.msgQueue)-1]
	m.msgQueue = m.msgQueue[:len(m.msgQueue)-1]
	m.input.SetValue(last)
	m.growInput()
	m.reflow()
	return true
}

// enqueue stashes text in the message queue while a turn is in flight and applies
// the active queue mode's mid-turn behavior. The queue drains when the turn ends
// (see drainQueue, hooked in Update's streamMsg case on a "done" event).
//
//   - queue / batch: buffer the message; the user's follow-up runs after the
//     current turn ends (queue: one per turn in order; batch: all merged).
//   - single: buffer the message AND cancel the running turn so the follow-up is
//     handled immediately (interrupt semantics). The cancel-induced done drains
//     the queue, which for single sends only the LATEST buffered message.
//
// The input box is cleared (the user's keystroke was consumed). The backlog is
// shown via queuePreview above the composer (not as a transcript entry).
func (m model) enqueue(text string) (tea.Model, tea.Cmd) {
	m.msgQueue = append(m.msgQueue, text)
	m.input.Reset()
	m.paletteItems = nil
	m.growInput()
	if m.queueMode == QueueModeSingle && !m.canceling {
		_ = m.sess.CancelCurrent()
		m.canceling = true
	}
	m.reflow()
	m.viewport.GotoBottom()
	return m, nil
}

// drainQueue sends queued messages when a turn ends, per the active queue mode:
//
//   - queue: send the HEAD only (FIFO). Each sent message starts its own turn,
//     whose done drains the next — so the queue unwinds one turn at a time.
//   - batch: join ALL queued messages with a blank line and send as ONE turn.
//   - single: send only the LAST queued message (earlier ones are discarded);
//     this is the message that interrupted the turn at enqueue time.
//
// Returns a nil Cmd when the queue is empty (nothing to drain). On a real drain
// the returned Cmd (from dispatchSend) arms waitForEvent + activityTick for the
// next turn; Update uses the non-nil Cmd to skip its own waitForEvent arming so
// the stream is not read twice.
func (m model) drainQueue() (model, tea.Cmd) {
	if len(m.msgQueue) == 0 {
		return m, nil
	}
	var text string
	switch m.queueMode {
	case QueueModeBatch:
		text = strings.Join(m.msgQueue, "\n\n")
		m.msgQueue = nil
	case QueueModeSingle:
		text = m.msgQueue[len(m.msgQueue)-1]
		m.msgQueue = nil
	default: // QueueModeQueue
		text = m.msgQueue[0]
		m.msgQueue = m.msgQueue[1:]
	}
	return m.dispatchSend(text, false)
}
