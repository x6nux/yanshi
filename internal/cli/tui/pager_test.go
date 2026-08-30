package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/termenv"

	"github.com/x6nux/yanshi/internal/cli"
)

// sized returns m after a WindowSizeMsg, the same setup TestModel_WindowSize-
// SetsDimensions (model_test.go) uses — reflow (and therefore the pager's own
// height math) needs a non-zero width/height to be meaningful.
func sized(t *testing.T, m model) model {
	t.Helper()
	mm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return mm.(model)
}

// TestCtrlT_OpensAndClosesPager proves the shared open/close toggle: the
// first Ctrl+T opens the pager, the second — routed back to the SAME case
// because the pagerVisible modal block in handleKeyMsg deliberately lets
// Ctrl+T fall through to it (mirroring F1/help) — closes it again.
func TestCtrlT_OpensAndClosesPager(t *testing.T) {
	m := sized(t, newTestModel(t))

	mm, _, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyCtrlT})
	if !handled {
		t.Fatalf("Ctrl+T should be handled")
	}
	if !mm.pagerVisible {
		t.Fatalf("first Ctrl+T should open the pager")
	}

	mm2, _, handled2 := mm.handleKeyMsg(tea.KeyMsg{Type: tea.KeyCtrlT})
	if !handled2 {
		t.Fatalf("Ctrl+T should be handled while the pager is open")
	}
	if mm2.pagerVisible {
		t.Fatalf("second Ctrl+T should close the pager")
	}
}

// TestCtrlT_BlockedWhileModalOpen mirrors TestCtrlE_BlockedWhileModalOpen
// (editor_test.go): the pager must not steal the screen out from under an
// already-open popup.
func TestCtrlT_BlockedWhileModalOpen(t *testing.T) {
	m := sized(t, newTestModel(t))
	m.helpVisible = true

	mm, cmd, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyCtrlT})
	if !handled {
		t.Fatalf("Ctrl+T should be handled (consumed) while a modal is open")
	}
	if cmd != nil {
		t.Fatalf("Ctrl+T should be a pure no-op while a modal is open, got a non-nil Cmd")
	}
	if mm.pagerVisible {
		t.Fatalf("pager must not open while another modal owns the keyboard")
	}
}

// TestCtrlT_BlockedWhilePermissionPending is RE-16's first half: unlike
// helpVisible/pickerKind/action, a pending permission is not one of the
// modal-priority `if` blocks at the top of handleKeyMsg — it is only
// checked ad hoc inside individual switch cases (KeyUp/KeyDown/KeyTab/
// KeyEnter/KeyRunes). Before this fix, Ctrl+T's own case never looked at
// it, so the pager could open right over an unresolved approval prompt and
// hide it (renderScreen's pagerVisible branch skips the permission popup
// entirely — see the reflow/renderScreen comments in view.go).
func TestCtrlT_BlockedWhilePermissionPending(t *testing.T) {
	m := sized(t, newTestModel(t))
	m.pendingPermissions = []*permissionEntry{{id: "p1", tool: "shell_run"}}

	mm, cmd, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyCtrlT})
	if !handled {
		t.Fatalf("Ctrl+T should be handled (consumed) while a permission is pending")
	}
	if cmd != nil {
		t.Fatalf("Ctrl+T should be a pure no-op while a permission is pending, got a non-nil Cmd")
	}
	if mm.pagerVisible {
		t.Fatalf("pager must not open over an unresolved permission prompt")
	}
}

// TestPermissionRequest_ForceClosesPager is RE-16's second half: opening the
// pager is guarded (TestCtrlT_BlockedWhilePermissionPending), but a
// permission_request is server-pushed and can land at any time, including
// while the pager is ALREADY open — no keypress-time guard can stop that.
// model.go's streamMsg handler force-closes the pager the instant one goes
// pending, so the popup starts rendering (and y/a/n start reaching
// respondPermission again) on the very next frame instead of staying hidden
// and unanswerable until the user happens to press Ctrl+T themselves. The
// pagerRawCopy=true setup also proves closePager's mouse-mode Cmd rides
// along, not just the state flip.
func TestPermissionRequest_ForceClosesPager(t *testing.T) {
	m := wsModel(newScriptedSession(nil))
	m.pagerVisible = true
	m.mouseEnabled = true
	m.pagerRawCopy = true

	mmRaw, cmd := m.Update(streamMsg{ev: cli.StreamEvent{
		Kind: "permission_request", ID: "p1", ToolName: "shell_run", Reason: "dangerous",
	}})
	mm := mmRaw.(model)

	if mm.pagerVisible {
		t.Fatalf("a pending permission must force-close the pager, not stay hidden behind it")
	}
	if mm.pendingPermission() == nil {
		t.Fatalf("closing the pager must not lose the permission request itself")
	}
	if mm.pagerRawCopy {
		// closePager's own contract: pagerRawCopy resets alongside
		// pagerVisible, not left dangling true on a closed pager.
		t.Fatalf("closing the pager must also clear pagerRawCopy (closePager's normal close contract)")
	}
	if cmd == nil {
		t.Fatalf("closing a raw-copy pager must re-enable mouse cell motion, got a nil Cmd")
	}
}

// TestPagerEscapeAndQ_BothClose proves both documented close keys (Esc, the
// `less`-style q) work, independent of the Ctrl+T toggle path.
func TestPagerEscapeAndQ_BothClose(t *testing.T) {
	for _, msg := range []tea.KeyMsg{
		{Type: tea.KeyEscape},
		{Type: tea.KeyRunes, Runes: []rune("q")},
	} {
		m := sized(t, newTestModel(t))
		m.pagerVisible = true

		mm, _, handled := m.handleKeyMsg(msg)
		if !handled {
			t.Fatalf("%v should be handled while the pager is open", msg)
		}
		if mm.pagerVisible {
			t.Fatalf("%v should close the pager", msg)
		}
	}
}

// TestPagerHomeEnd_JumpsTopAndBottom proves the pager's Home/End bindings —
// which bubbles/viewport's own DefaultKeyMap does not provide — actually
// move the shared viewport, using content taller than the viewport so top
// and bottom are distinguishable positions.
func TestPagerHomeEnd_JumpsTopAndBottom(t *testing.T) {
	m := sized(t, newTestModel(t))
	for i := 0; i < 200; i++ {
		m.entries = append(m.entries, &userEntry{text: fmt.Sprintf("line %d", i)})
	}
	m.pagerVisible = true
	m.reflow()
	m.viewport.GotoBottom()
	if m.viewport.AtTop() {
		t.Fatalf("setup: viewport should not already be at top")
	}

	mm, _, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyHome})
	if !handled || !mm.viewport.AtTop() {
		t.Fatalf("Home should jump the pager viewport to the top (handled=%v atTop=%v)", handled, mm.viewport.AtTop())
	}

	mm2, _, handled2 := mm.handleKeyMsg(tea.KeyMsg{Type: tea.KeyEnd})
	if !handled2 || !mm2.viewport.AtBottom() {
		t.Fatalf("End should jump the pager viewport to the bottom (handled=%v atBottom=%v)", handled2, mm2.viewport.AtBottom())
	}
}

// TestPagerArrowKey_ForwardsToViewport proves scroll keys reach the shared
// viewport instead of being silently swallowed or (worse) falling through to
// whatever the bottom switch's KeyDown case would otherwise do (move the
// permission/palette selection — neither of which can be open here, but the
// pager's own dedicated forwarding is what makes that true).
func TestPagerArrowKey_ForwardsToViewport(t *testing.T) {
	m := sized(t, newTestModel(t))
	for i := 0; i < 200; i++ {
		m.entries = append(m.entries, &userEntry{text: fmt.Sprintf("line %d", i)})
	}
	m.pagerVisible = true
	m.reflow()
	m.viewport.GotoTop()

	mm, _, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyDown})
	if !handled {
		t.Fatalf("Down should be handled while the pager is open")
	}
	if mm.viewport.YOffset == 0 {
		t.Fatalf("Down should have scrolled the pager viewport past the top, YOffset = %d", mm.viewport.YOffset)
	}
}

// TestPagerRawCopyToggle_SendsMouseCmdWhenEnabled proves 'r' flips
// pagerRawCopy and returns the matching bubbletea mouse-mode Cmd — this is
// the actual mechanism the spec's "raw 复制模式绕开 alt-screen 的选择失效"
// describes: turning mouse reporting off hands text selection back to the
// terminal itself.
func TestPagerRawCopyToggle_SendsMouseCmdWhenEnabled(t *testing.T) {
	m := sized(t, newTestModel(t))
	m.pagerVisible = true
	m.mouseEnabled = true

	mm, cmd, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if !handled {
		t.Fatalf("r should be handled while the pager is open")
	}
	if !mm.pagerRawCopy {
		t.Fatalf("r should turn raw copy mode on")
	}
	if cmd == nil || cmd() != tea.DisableMouse() {
		t.Fatalf("turning raw copy mode on should return tea.DisableMouse")
	}

	mm2, cmd2, handled2 := mm.handleKeyMsg(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if !handled2 {
		t.Fatalf("r should be handled the second time too")
	}
	if mm2.pagerRawCopy {
		t.Fatalf("second r should turn raw copy mode back off")
	}
	if cmd2 == nil || cmd2() != tea.EnableMouseCellMotion() {
		t.Fatalf("turning raw copy mode off should return tea.EnableMouseCellMotion")
	}
}

// TestPagerRawCopyToggle_NoopWithoutMouse is the alt-screen-less degradation
// path (W-E-03's "must degrade gracefully" requirement): programOptions
// (model.go) only ever turns mouse cell-motion on when the detected terminal
// capability allows alt-screen (RE-D), so a terminal that never had mouse
// mode on has nothing to disable — sending the escape sequence anyway would
// be exactly the noise RE-D was written to avoid. mouseEnabled defaults to
// false on every model built directly by a test (only NewProgram sets it),
// which is the same "nothing was ever turned on" state a dumb terminal
// leaves it in at runtime.
func TestPagerRawCopyToggle_NoopWithoutMouse(t *testing.T) {
	m := sized(t, newTestModel(t))
	m.pagerVisible = true
	// m.mouseEnabled left at its zero value: false.

	mm, cmd, handled := m.handleKeyMsg(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if !handled {
		t.Fatalf("r should still be handled (consumed) even when mouse is unavailable")
	}
	if mm.pagerRawCopy {
		t.Fatalf("raw copy mode should not turn on when mouseEnabled is false")
	}
	if cmd != nil {
		t.Fatalf("no mouse-mode Cmd should be sent when mouseEnabled is false")
	}
}

// TestClosePager_ReEnablesMouseOnlyIfRawCopyWasOn proves closePager's other
// branch: closing while raw copy mode is OFF must not send a spurious
// tea.EnableMouseCellMotion for a mouse mode that was never disabled.
func TestClosePager_ReEnablesMouseOnlyIfRawCopyWasOn(t *testing.T) {
	m := sized(t, newTestModel(t))
	m.pagerVisible = true
	m.mouseEnabled = true
	// pagerRawCopy left false: raw copy mode was never turned on.

	cmd := m.closePager()
	if cmd != nil {
		t.Fatalf("closing with raw copy mode already off should not send a mouse Cmd")
	}
}

// TestPagerRenderSuppressesColorUnderAscii proves acceptance criterion 1
// (NO_COLOR) for the pager specifically: it reuses renderBody() (via the
// shared m.viewport) and toolMeta for its own hint line — both already
// palette-aware — so under termenv.Ascii the fullscreen pager screen must
// emit not a single ANSI escape byte, the same guarantee W-E-01 pins for the
// normal screen.
func TestPagerRenderSuppressesColorUnderAscii(t *testing.T) {
	withColorProfile(t, termenv.Ascii)
	m := sized(t, newTestModel(t))
	m.entries = append(m.entries, &userEntry{text: "hello"})
	m.pagerVisible = true
	m.reflow()

	out := m.renderScreen()
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("Ascii profile: pager renderScreen still contains an ANSI escape byte: %q", out)
	}
}
