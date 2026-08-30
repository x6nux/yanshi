package cli

import (
	"testing"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestIsControlReply_WorkspaceDiffFrame verifies isControlReply closes the
// control channel on the workspace_diff reply (W-E-13) — otherwise deliver
// would never judge the frame terminal, so the channel SendFrame returned
// would never close and a caller ranging over it would hang forever waiting
// for a done that never comes, since workspace_diff is a single-frame reply
// like seams/seam_restored.
func TestIsControlReply_WorkspaceDiffFrame(t *testing.T) {
	if !isControlReply("workspace_diff") {
		t.Error("isControlReply(\"workspace_diff\") = false, want true")
	}
}

// TestToStreamEvent_WorkspaceDiffFrame verifies the workspace_diff reply's
// file list propagates through toStreamEvent unchanged.
func TestToStreamEvent_WorkspaceDiffFrame(t *testing.T) {
	f := proto.NewWorkspaceDiff([]proto.WorkspaceDiffFile{
		{Path: "a.go", Op: "modified", OldText: "old", NewText: "new"},
	})
	ev := toStreamEvent(f)
	if ev.Kind != "workspace_diff" {
		t.Errorf("ev.Kind = %q, want %q", ev.Kind, "workspace_diff")
	}
	if len(ev.WorkspaceDiff) != 1 || ev.WorkspaceDiff[0].Path != "a.go" {
		t.Errorf("ev.WorkspaceDiff = %+v, want one entry a.go", ev.WorkspaceDiff)
	}
}
