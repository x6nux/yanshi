package http

import (
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestDeliverRejectsModeSwitchAllowOnProfileDeny pins the server as the
// authority on profile policy.
//
// The hole: in ModeAuto, a request the risk scorer cannot rate comes back
// unresolved and is sent to the client as an ordinary permission_request --
// the ProfileHardDeny fact never reaches the wire. If the user then switches
// to allow-edits, the TUI's autoResolvePendingByMode answers `allow` for any
// IsEditTool without consulting the server, and deliver used to take that at
// face value. The very same request, re-entering resolvePermissionMode under
// allow-edits, would have been denied (`ProfileHardDeny` -> deny, resolved).
// A client-side mode switch therefore overrode a server-side profile policy.
//
// The distinguishing fact is not "who sent the allow" (the wire cannot tell a
// human click from an auto-resolve) but "did the mode change since we asked".
// A human clicking allow in auto mode is answering the question we asked and
// must be honoured; an allow arriving under a mode we never asked in was
// produced by a rule, and the server owns that rule.
func TestDeliverRejectsModeSwitchAllowOnProfileDeny(t *testing.T) {
	cases := []struct {
		name       string
		hardDeny   bool
		modeAtSend guard.PermissionMode
		modeNow    guard.PermissionMode
		want       tools.PermissionDecision
	}{
		{
			name:       "mode switch to allow-edits cannot override a profile deny",
			hardDeny:   true,
			modeAtSend: guard.ModeAuto,
			modeNow:    guard.ModeAllowEdits,
			want:       tools.PermissionDeny,
		},
		{
			name:       "human allow in the same mode we asked in is honoured",
			hardDeny:   true,
			modeAtSend: guard.ModeAuto,
			modeNow:    guard.ModeAuto,
			want:       tools.PermissionAllow,
		},
		{
			name:       "switching to yolo does allow it (the server's own gate says so)",
			hardDeny:   true,
			modeAtSend: guard.ModeAuto,
			modeNow:    guard.ModeYOLO,
			want:       tools.PermissionAllow,
		},
		{
			name:       "an ordinary request is unaffected by a mode switch",
			hardDeny:   false,
			modeAtSend: guard.ModeDefault,
			modeNow:    guard.ModeAllowEdits,
			want:       tools.PermissionAllow,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pt := newPermTracker()
			id := pt.newID()
			ch := make(chan tools.PermissionDecision, 1)
			pt.register(id, ch, tools.PermissionRequest{ProfileHardDeny: tc.hardDeny}, tc.modeAtSend)

			pt.deliver(id, tools.PermissionAllow, tc.modeNow)

			select {
			case got := <-ch:
				if got != tc.want {
					t.Fatalf("delivered %v, want %v", got, tc.want)
				}
			default:
				t.Fatal("nothing delivered")
			}
		})
	}
}

// TestDeliverNeverUpgradesADeny is the other direction: the re-check may only
// ever tighten. A client that answers deny gets a deny under every mode,
// including yolo -- the user said no.
func TestDeliverNeverUpgradesADeny(t *testing.T) {
	pt := newPermTracker()
	id := pt.newID()
	ch := make(chan tools.PermissionDecision, 1)
	pt.register(id, ch, tools.PermissionRequest{ProfileHardDeny: true}, guard.ModeAuto)

	pt.deliver(id, tools.PermissionDeny, guard.ModeYOLO)

	if got := <-ch; got != tools.PermissionDeny {
		t.Fatalf("deliver upgraded a deny to %v", got)
	}
}
