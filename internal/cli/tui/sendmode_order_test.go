package tui

import (
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
)

// TestSendModeAnnouncesTheModeBeforeAnsweringOnIt pins a wire ORDER that a
// server-side security check depends on.
//
// permTracker.deliver refuses an `allow` for a profile-denied request when the
// mode has changed since the prompt was sent. It reads the current mode from
// the connection state, which the reader goroutine updates when a set_mode
// frame arrives -- and that goroutine handles set_mode and permission_response
// in arrival order. So if sendMode auto-resolves BEFORE announcing the new
// mode, the server evaluates those responses against the OLD mode, the
// comparison finds no change, and the check never fires. The defence would be
// dead code in the exact scenario it was written for.
//
// This is the kind of coupling that no gate can see: two files, no shared
// symbol, and both sides individually correct. The order is the contract, so
// the order is what gets asserted.
func TestSendModeAnnouncesTheModeBeforeAnsweringOnIt(t *testing.T) {
	rec := &recordingSession{}
	m := newTestModel(t)
	m.sess = rec
	m.permMode = guard.ModeAllowEdits
	m.pendingPermissions = []*permissionEntry{
		{id: "1", tool: "fs_write"},
		{id: "2", tool: "apply_patch"},
	}

	_, _ = m.sendMode()

	var setModeAt = -1
	var firstResponseAt = -1
	for i, f := range rec.frames {
		switch f.Type {
		case "set_mode":
			if setModeAt < 0 {
				setModeAt = i
			}
		case "permission_response":
			if firstResponseAt < 0 {
				firstResponseAt = i
			}
		}
	}

	if setModeAt < 0 {
		t.Fatalf("sendMode sent no set_mode frame: %+v", rec.frames)
	}
	if firstResponseAt < 0 {
		t.Fatalf("the pending edit requests were not auto-resolved: %+v", rec.frames)
	}
	if setModeAt > firstResponseAt {
		t.Fatalf("set_mode (index %d) must precede the auto-resolved permission_response (index %d): "+
			"the server evaluates the response against whichever mode it knows about at that moment",
			setModeAt, firstResponseAt)
	}
}
