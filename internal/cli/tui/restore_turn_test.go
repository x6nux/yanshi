package tui

import (
	"testing"

	"github.com/x6nux/yanshi/internal/cli"
	"github.com/x6nux/yanshi/internal/proto"
)

// TestRestoreTurnCommand_NoArgs_ListsSeams verifies the no-arg form sends a
// list_seams control frame (via sendControlFrame).
func TestRestoreTurnCommand_NoArgs_ListsSeams(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	cmdRestoreTurn(m, []string{})
	if len(rs.frames) != 1 {
		t.Fatalf("expected 1 frame sent, got %d", len(rs.frames))
	}
	if rs.frames[0].Type != "list_seams" {
		t.Errorf("frame.Type = %q, want list_seams", rs.frames[0].Type)
	}
}

// TestRestoreTurnCommand_WithIDAndYes_SendsRestoreFrame verifies the
// "<id> yes" form sends a restore_turn frame with the seam id + the cached
// confirmed_head binding (D6: full id).
func TestRestoreTurnCommand_WithIDAndYes_SendsRestoreFrame(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	m.lastKnownHead = "fullheadabcdef0123456789"
	cmdRestoreTurn(m, []string{"seam-1", "yes"})
	if len(rs.frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(rs.frames))
	}
	f := rs.frames[0]
	if f.Type != "restore_turn" {
		t.Errorf("Type = %q, want restore_turn", f.Type)
	}
	if f.ID != "seam-1" {
		t.Errorf("ID = %q, want seam-1", f.ID)
	}
	if f.ConfirmedHead != "fullheadabcdef0123456789" {
		t.Errorf("ConfirmedHead = %q, want full id", f.ConfirmedHead)
	}
}

// TestRestoreTurnCommand_WithIDOnly_ShowsPrompt verifies the "<id>" form
// (without yes) shows an in-TUI confirmation prompt and does NOT send a frame.
func TestRestoreTurnCommand_WithIDOnly_ShowsPrompt(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	out, _ := cmdRestoreTurn(m, []string{"seam-1"})
	got, ok := out.(model)
	if !ok {
		t.Fatalf("cmdRestoreTurn returned %T, want model", out)
	}
	if len(rs.frames) != 0 {
		t.Errorf("no frame should be sent on single-arg form; got %d", len(rs.frames))
	}
	foundPrompt := false
	for _, entry := range got.entries {
		if _, ok := entry.(seamRestorePromptEntry); ok {
			foundPrompt = true
			break
		}
	}
	if !foundPrompt {
		t.Fatal("returned model must append seamRestorePromptEntry")
	}
}

// TestApplyEvent_SeamsPopulatesEntry verifies the seams reply fills the
// pending entry + caches the full head for the next restore (D6: Head, not
// CommitShort).
func TestApplyEvent_SeamsPopulatesEntry(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	seamsEv := cli.StreamEvent{
		Kind: "seams",
		Seams: []proto.SeamInfo{
			{ID: "s1", Kind: "pre-turn", CommitShort: "abc12345", Label: "pre-turn:1"},
		},
		CommitShort: "abc12345",
		Head:        "fullheadabcdef0123456789",
	}
	m = m.applyEvent(seamsEv)
	if m.lastKnownHead != "fullheadabcdef0123456789" {
		t.Errorf("lastKnownHead = %q, want full id", m.lastKnownHead)
	}
	foundSeamsEntry := false
	for _, e := range m.entries {
		if _, ok := e.(seamsEntry); ok {
			foundSeamsEntry = true
			break
		}
	}
	if !foundSeamsEntry {
		t.Error("seamsEntry not appended to entries")
	}
}

// TestApplyEvent_SeamRestoredClearsPending verifies seam_restored resolves the
// pending restore entry.
func TestApplyEvent_SeamRestoredClearsPending(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	m.pendingSeamRestore = &pendingSeamRestoreState{seamID: "s1"}
	ev := cli.StreamEvent{
		Kind: "seam_restored", UndoSeamID: "undo-x", Text: "ok",
		Head: "full-post-revert-head-0123456789",
	}
	m = m.applyEvent(ev)
	if m.pendingSeamRestore != nil {
		t.Error("pendingSeamRestore should be cleared after seam_restored")
	}
	if m.lastKnownHead != ev.Head {
		t.Errorf("lastKnownHead = %q, want seam_restored full Head %q", m.lastKnownHead, ev.Head)
	}
}

// TestApplyEvent_ErrorClearsPendingSeamRestore verifies the error branch clears
// the pending pointer (必修项 I: 不留下永久 reverting... pending 状态).
func TestApplyEvent_ErrorClearsPendingSeamRestore(t *testing.T) {
	rs := &recordingSession{}
	m := newModel(rs, "/proj")
	m.pendingSeamRestore = &pendingSeamRestoreState{seamID: "s1"}
	ev := cli.StreamEvent{Kind: "error", Text: "head changed"}
	m = m.applyEvent(ev)
	if m.pendingSeamRestore != nil {
		t.Error("pendingSeamRestore should be cleared on error event")
	}
}
