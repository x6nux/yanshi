package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// pastTurnStart returns a turnStart old enough to clear
// notifyLongTaskThreshold, so tests can exercise notifyCmd's gates
// independently of a real timer.
func pastTurnStart() time.Time {
	return time.Now().Add(-2 * notifyLongTaskThreshold)
}

// TestNotifyCmd_DisabledEmitsNothing guards the config tui.notify=false
// default (W-E-04's "avoid an obnoxious default-notifying agent" brief): a
// session that never opted in must never call tea.Notify or tea.Bell, on any
// terminal.
func TestNotifyCmd_DisabledEmitsNothing(t *testing.T) {
	for _, titleEnabled := range []bool{true, false} {
		m := newModel(nil, "/proj")
		m.notifyEnabled = false
		m.titleEnabled = titleEnabled
		m.turnStart = pastTurnStart()
		if cmd := m.notifyCmd(); cmd != nil {
			t.Fatalf("notifyEnabled=false, titleEnabled=%v: expected nil Cmd, got non-nil", titleEnabled)
		}
	}
}

// TestNotifyCmd_ShortTurnEmitsNothing guards notifyLongTaskThreshold: a turn
// that finished quickly must not notify even when the preference is on,
// since a round-trip the user is still looking at the terminal for doesn't
// need a desktop ping.
func TestNotifyCmd_ShortTurnEmitsNothing(t *testing.T) {
	m := newModel(nil, "/proj")
	m.notifyEnabled = true
	m.titleEnabled = true
	m.turnStart = time.Now()
	if cmd := m.notifyCmd(); cmd != nil {
		t.Fatalf("turn under threshold: expected nil Cmd, got non-nil")
	}
}

// TestNotifyCmd_CapableTerminalUsesOSC9 guards the top tier: enabled + long
// turn + a terminal titleEnabled trusts with escape sequences must produce
// tea.Notify (OSC 9), not tea.Bell — and the message must name the project,
// same contract as windowTitleCmd's busy/idle text.
func TestNotifyCmd_CapableTerminalUsesOSC9(t *testing.T) {
	m := newModel(nil, "/some/path/my-project")
	m.notifyEnabled = true
	m.titleEnabled = true
	m.turnStart = pastTurnStart()

	cmd := m.notifyCmd()
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd")
	}
	msg := cmd()
	if got := fmt.Sprintf("%T", msg); got != "tea.notifyMsg" {
		t.Fatalf("expected tea.notifyMsg (OSC 9), got %s", got)
	}
	text := reflect.ValueOf(msg).String()
	if !strings.Contains(text, "my-project") {
		t.Fatalf("notification text %q does not contain project name", text)
	}
}

// TestNotifyCmd_DumbTerminalUsesBell guards the degrade tier: enabled + long
// turn + titleEnabled==false (TERM=dumb — see that field's doc comment) must
// produce tea.Bell, never the OSC 9 tea.Notify escape a dumb terminal isn't
// trusted to parse cleanly.
func TestNotifyCmd_DumbTerminalUsesBell(t *testing.T) {
	m := newModel(nil, "/proj")
	m.notifyEnabled = true
	m.titleEnabled = false
	m.turnStart = pastTurnStart()

	cmd := m.notifyCmd()
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd")
	}
	msg := cmd()
	if got := fmt.Sprintf("%T", msg); got != "tea.bellMsg" {
		t.Fatalf("expected tea.bellMsg (BEL degrade), got %s", got)
	}
}
