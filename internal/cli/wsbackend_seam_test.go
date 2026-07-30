package cli

import (
	"testing"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestIsControlReply_SeamFrames verifies isControlReply closes the control
// channel on seam replies (otherwise SendFrame would hang forever waiting for
// a done that never comes — these are single-frame replies).
func TestIsControlReply_SeamFrames(t *testing.T) {
	for _, kind := range []string{"seams", "seam_restored"} {
		if !isControlReply(kind) {
			t.Errorf("isControlReply(%q) = false, want true", kind)
		}
	}
}

// TestToStreamEvent_SeamsFrame verifies the seams reply's Seams slice and
// CommitShort/Head propagate through toStreamEvent (D6).
func TestToStreamEvent_SeamsFrame(t *testing.T) {
	f := proto.NewSeams(
		[]proto.SeamInfo{{ID: "s1", Kind: "pre-turn", CommitShort: "abc12345"}},
		"abc12345",
		"fullheadabcdef0123456789",
	)
	ev := toStreamEvent(f)
	if ev.Kind != "seams" {
		t.Errorf("ev.Kind = %q, want %q", ev.Kind, "seams")
	}
	if len(ev.Seams) != 1 || ev.Seams[0].ID != "s1" {
		t.Errorf("ev.Seams = %+v, want one entry s1", ev.Seams)
	}
	if ev.CommitShort != "abc12345" {
		t.Errorf("ev.CommitShort = %q, want %q", ev.CommitShort, "abc12345")
	}
	if ev.Head != "fullheadabcdef0123456789" {
		t.Errorf("ev.Head = %q, want full id", ev.Head)
	}
}

// TestToStreamEvent_SeamRestoredFrame verifies the seam_restored reply's ID is
// mapped to UndoSeamID (the load-bearing field for "undo this revert" UX).
func TestToStreamEvent_SeamRestoredFrame(t *testing.T) {
	f := proto.NewSeamRestored("undo-xyz", "deadbeef", "fullpostreverthead01", "summary")
	ev := toStreamEvent(f)
	if ev.UndoSeamID != "undo-xyz" {
		t.Errorf("ev.UndoSeamID = %q, want %q", ev.UndoSeamID, "undo-xyz")
	}
	if ev.CommitShort != "deadbeef" {
		t.Errorf("ev.CommitShort = %q, want %q", ev.CommitShort, "deadbeef")
	}
}
