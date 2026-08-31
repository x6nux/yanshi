package tui

import tea "github.com/charmbracelet/bubbletea"

// windowTitleCmd returns the tea.Cmd that sets the terminal window title to
// the project name plus session status (W-E-05), or nil when titleEnabled is
// false (TERM=dumb — see the model struct's titleEnabled doc comment). A nil
// Cmd inside tea.Batch(...) is a documented no-op, so gating here — rather
// than inside SetWindowTitle itself — is what keeps a disabled session from
// emitting the OSC 2 / XTWINOPS push it would otherwise still pay for; see
// third_party/bubbletea's TestTitleUntouchedLeavesNoStackNoise for the proof
// that a program which never calls SetWindowTitle writes zero title-stack
// bytes.
//
// busy distinguishes the two states this batch's acceptance criterion asks
// for ("项目 + 会话状态"): true while dispatchSend's turn is in flight, false
// once Update's streamMsg case observes ev.Kind == "done" — the same two
// seams startTurn/applyEvent already use as the turn boundary, so no new
// state is introduced to track it.
func (m model) windowTitleCmd(busy bool) tea.Cmd {
	if !m.titleEnabled {
		return nil
	}
	// m.workDir is already the basename of rootPath (set once in
	// newModelWithPrefs for the footer's directory indicator) — reused here
	// rather than recomputing dirName(m.rootPath) a second time.
	vars := map[string]string{"project": m.workDir}
	key := "tui.title.idle"
	if busy {
		key = "tui.title.busy"
	}
	return tea.SetWindowTitle(m.bundle.GetF(key, vars))
}
