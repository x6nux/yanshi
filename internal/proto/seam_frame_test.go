package proto

import (
	"encoding/json"
	"testing"
)

// TestNewListSeams_FrameShape verifies the list_seams request frame.
func TestNewListSeams_FrameShape(t *testing.T) {
	f := NewListSeams()
	if f.Type != "list_seams" {
		t.Errorf("Type = %q, want %q", f.Type, "list_seams")
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Round-trip through ClientFrame to confirm it decodes cleanly.
	var cf ClientFrame
	if err := json.Unmarshal(b, &cf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cf.Type != "list_seams" {
		t.Errorf("round-trip Type = %q", cf.Type)
	}
}

// TestNewRestoreTurn_FrameWithConfirmedHead verifies the restore_turn frame
// carries the seam_id and the confirmed_head binding (D6: FULL commit id).
func TestNewRestoreTurn_FrameWithConfirmedHead(t *testing.T) {
	f := NewRestoreTurn("abc123", "fullcommitid0123456789abcdef")
	if f.Type != "restore_turn" {
		t.Errorf("Type = %q", f.Type)
	}
	if f.ID != "abc123" {
		t.Errorf("ID = %q, want %q", f.ID, "abc123")
	}
	if f.ConfirmedHead != "fullcommitid0123456789abcdef" {
		t.Errorf("ConfirmedHead = %q, want full id", f.ConfirmedHead)
	}
	b, _ := json.Marshal(f)
	var cf ClientFrame
	_ = json.Unmarshal(b, &cf)
	if cf.ID != "abc123" || cf.ConfirmedHead != "fullcommitid0123456789abcdef" {
		t.Errorf("round-trip lost binding: ID=%q ConfirmedHead=%q", cf.ID, cf.ConfirmedHead)
	}
}

// TestNewSeams_FrameShape verifies the seams reply carries the list + the
// server's current head (short for display, full for binding — D6).
func TestNewSeams_FrameShape(t *testing.T) {
	items := []SeamInfo{
		{ID: "s1", Kind: "pre-turn", TurnSeq: 1, HistoryLen: 2, CommitShort: "abc12345", Label: "pre-turn:1"},
		{ID: "s2", Kind: "post-turn", TurnSeq: 1, HistoryLen: 3, CommitShort: "def67890", Label: "post-turn:1"},
	}
	f := NewSeams(items, "abc12345", "fullheadabcdef0123456789")
	if f.Type != "seams" {
		t.Errorf("Type = %q", f.Type)
	}
	if len(f.Seams) != 2 {
		t.Errorf("len(Seams) = %d, want 2", len(f.Seams))
	}
	if f.CommitShort != "abc12345" {
		t.Errorf("CommitShort = %q, want %q", f.CommitShort, "abc12345")
	}
	if f.Head != "fullheadabcdef0123456789" {
		t.Errorf("Head = %q, want full id", f.Head)
	}
	b, _ := json.Marshal(f)
	var sf ServerFrame
	_ = json.Unmarshal(b, &sf)
	if sf.Type != "seams" || len(sf.Seams) != 2 || sf.CommitShort != "abc12345" || sf.Head != "fullheadabcdef0123456789" {
		t.Errorf("round-trip lost: %+v", sf)
	}
}

// TestNewSeamRestored_FrameShape verifies the seam_restored reply's ID is the
// undo seam id and the frame carries the post-revert head (short + full — D6).
func TestNewSeamRestored_FrameShape(t *testing.T) {
	f := NewSeamRestored("undo-id", "abc12345", "fullpostreverthead0123456", "restored to pre-turn:1")
	if f.Type != "seam_restored" {
		t.Errorf("Type = %q", f.Type)
	}
	if f.ID != "undo-id" {
		t.Errorf("ID = %q, want undo-id", f.ID)
	}
	if f.CommitShort != "abc12345" {
		t.Errorf("CommitShort = %q", f.CommitShort)
	}
	if f.Head != "fullpostreverthead0123456" {
		t.Errorf("Head = %q, want full id", f.Head)
	}
	if f.Text != "restored to pre-turn:1" {
		t.Errorf("Text = %q", f.Text)
	}
}

// TestSeamInfo_OmitsEmptyFields verifies the SeamInfo JSON omits unset fields
// (so a future field addition does not bloat the wire format).
func TestSeamInfo_OmitsEmptyFields(t *testing.T) {
	s := SeamInfo{ID: "x", Kind: "pre-turn"}
	b, _ := json.Marshal(s)
	// CommitShort / Label should be omitted (omitempty).
	var m map[string]interface{}
	_ = json.Unmarshal(b, &m)
	if _, ok := m["commit_short"]; ok {
		t.Errorf("commit_short 应当 omitempty")
	}
	if _, ok := m["label"]; ok {
		t.Errorf("label 应当 omitempty")
	}
}
