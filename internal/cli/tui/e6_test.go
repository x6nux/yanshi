package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// --- W-E-10: Ctrl+Z suspend / ResumeMsg restore ---

// TestSuspendKeyReturnsSuspendCmd verifies Ctrl+Z dispatches the tea.Suspend
// command (the fork's Program turns it into ReleaseTerminal + SIGTSTP +
// RestoreTerminal + ResumeMsg).
//
// Mutation: change the case to return nil — this fails because the Cmd no
// longer carries the suspend closure.
func TestSuspendKeyReturnsSuspendCmd(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	mm, cmd, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyCtrlZ})
	if !handled {
		t.Fatal("Ctrl+Z must be consumed (not typed into the input)")
	}
	if cmd == nil {
		t.Fatal("Ctrl+Z must return the tea.Suspend Cmd")
	}
	// tea.Suspend is a func() tea.Msg returning SuspendMsg; running it must
	// produce that message.
	msg := cmd()
	if _, ok := msg.(tea.SuspendMsg); !ok {
		t.Fatalf("the Cmd must produce tea.SuspendMsg, got %T", msg)
	}
	_ = mm
}

// TestResumeMsgTriggersReflowAndRepaint verifies the ResumeMsg handler runs a
// reflow and returns tea.Repaint — the model-side half of W-E-10's "fg
// restores the screen" contract.
//
// Mutation: make the ResumeMsg case a no-op (return m, nil) — this fails
// because the returned Cmd is nil instead of tea.Repaint.
func TestResumeMsgTriggersReflowAndRepaint(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	count := 0
	m.countReflow = &count
	mm, cmd := m.Update(tea.ResumeMsg{})
	if count == 0 {
		t.Fatal("ResumeMsg must trigger a reflow")
	}
	if cmd == nil {
		t.Fatal("ResumeMsg must return a Cmd (tea.Repaint)")
	}
	// The Repaint Cmd is a distinct closure from nil; run it — it produces
	// nil (RepaintMsg is renderer-side) but is non-nil.
	_ = cmd()
	_ = mm.(model)
}

// TestSuspendKeyDocumentedInCensus is enforced by
// keybindings_wiring_test.go's census (KeyCtrlZ ↔ "Ctrl+Z"); this test just
// pins the help-table half so the Label cannot silently vanish.
//
// Mutation: delete the Ctrl+Z row from keyBindings in help.go — this fails.
func TestSuspendKeyDocumentedInCensus(t *testing.T) {
	for _, kb := range keyBindings {
		if kb.Label == "Ctrl+Z" {
			return
		}
	}
	t.Fatal("keyBindings must document Ctrl+Z (suspend)")
}
