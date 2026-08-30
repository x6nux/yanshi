package tui

import (
	"reflect"
	"strings"
	"testing"
)

// TestWindowTitleCmd_DisabledEmitsNothing guards W-E-05's TERM=dumb contract:
// a session that detected no alt-screen support must never call
// tea.SetWindowTitle at all, matching third_party/bubbletea's
// TestTitleUntouchedLeavesNoStackNoise (a nil Cmd inside tea.Batch writes zero
// title-stack bytes). If titleEnabled stopped gating this, a TERM=dumb
// session would start emitting OSC 2 / XTWINOPS escapes — a fourth
// capability-detection bypass path alongside the three E1 already closed.
func TestWindowTitleCmd_DisabledEmitsNothing(t *testing.T) {
	m := newModel(nil, "/proj")
	m.titleEnabled = false
	if cmd := m.windowTitleCmd(false); cmd != nil {
		t.Fatalf("windowTitleCmd(false) with titleEnabled=false: expected nil Cmd, got non-nil")
	}
	if cmd := m.windowTitleCmd(true); cmd != nil {
		t.Fatalf("windowTitleCmd(true) with titleEnabled=false: expected nil Cmd, got non-nil")
	}
}

// TestWindowTitleCmd_TextReflectsProjectAndBusyState guards the acceptance
// criterion itself ("标题显示项目 + 会话状态"): the emitted title must contain
// the project's directory name in both states, and the busy/idle wording must
// differ so a user watching the OS taskbar/tab can tell a turn is in flight.
//
// tea.SetWindowTitle's setWindowTitleMsg type is unexported in package tea,
// so the returned Msg is read back via reflect.Value.String() (its Kind is
// String regardless of the inaccessible named type) instead of a type
// assertion.
func TestWindowTitleCmd_TextReflectsProjectAndBusyState(t *testing.T) {
	m := newModel(nil, "/some/path/my-project")
	m.titleEnabled = true

	idleCmd := m.windowTitleCmd(false)
	busyCmd := m.windowTitleCmd(true)
	if idleCmd == nil || busyCmd == nil {
		t.Fatalf("expected non-nil Cmds with titleEnabled=true, got idle=%v busy=%v", idleCmd, busyCmd)
	}

	idleTitle := reflect.ValueOf(idleCmd()).String()
	busyTitle := reflect.ValueOf(busyCmd()).String()

	const project = "my-project"
	if !strings.Contains(idleTitle, project) {
		t.Fatalf("idle title %q does not contain project name %q", idleTitle, project)
	}
	if !strings.Contains(busyTitle, project) {
		t.Fatalf("busy title %q does not contain project name %q", busyTitle, project)
	}
	if idleTitle == busyTitle {
		t.Fatalf("idle and busy titles must differ so a session-status change is visible, both were %q", idleTitle)
	}
}
