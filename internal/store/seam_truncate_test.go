package store

import (
	"encoding/json"
	"fmt"
	"testing"
)

func seedRevertSession(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	sid, err := s.CreateSession("truncate test")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for i := 0; i < 5; i++ {
		if err := s.AppendMessage(sid, i, "user", fmt.Sprintf("msg-%d", i)); err != nil {
			t.Fatalf("AppendMessage[%d]: %v", i, err)
		}
	}
	if err := s.UpdateSessionMeta(sid, "fake", "off", 10, 20, 4, 2, 3, BillingMeta{}); err != nil {
		t.Fatalf("UpdateSessionMeta: %v", err)
	}
	return s, sid
}

func TestTruncateSessionForRevert_IsAtomicAndReturnsSnapshot(t *testing.T) {
	s, sid := seedRevertSession(t)
	snap, err := s.TruncateSessionForRevert(sid, 3, 1)
	if err != nil {
		t.Fatalf("TruncateSessionForRevert: %v", err)
	}
	if len(snap.Messages) != 5 || snap.Meta.ID != sid || snap.Meta.Turns != 4 {
		t.Fatalf("snapshot = %+v / %d messages; want original turns=4/messages=5",
			snap.Meta, len(snap.Messages))
	}
	msgs, err := s.Messages(sid)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("persisted messages after truncate = %d, err=%v; want 3", len(msgs), err)
	}
	meta, err := s.GetSession(sid)
	if err != nil || meta == nil || meta.Turns != 1 {
		t.Fatalf("meta after truncate = %+v, err=%v; want turns=1", meta, err)
	}
}

func TestSessionRevertSnapshot_JSONRoundTripForUndo(t *testing.T) {
	s, sid := seedRevertSession(t)
	snap, err := s.SnapshotSessionForRevert(sid)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := EncodeSessionRevertSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(blob) {
		t.Fatal("encoded undo snapshot is not valid JSON")
	}
	got, err := DecodeSessionRevertSnapshot(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.Meta.ID != sid || got.Meta.Turns != 4 || len(got.Messages) != 5 {
		t.Fatalf("decoded snapshot = %+v/%d messages", got.Meta, len(got.Messages))
	}
}

func TestRestoreSessionAfterFailedRevert_RestoresMessagesAndMeta(t *testing.T) {
	s, sid := seedRevertSession(t)
	snap, err := s.TruncateSessionForRevert(sid, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreSessionAfterFailedRevert(snap); err != nil {
		t.Fatalf("RestoreSessionAfterFailedRevert: %v", err)
	}
	msgs, err := s.Messages(sid)
	if err != nil || len(msgs) != 5 {
		t.Fatalf("restored messages = %d, err=%v; want 5", len(msgs), err)
	}
	for i, msg := range msgs {
		if msg.ID != snap.Messages[i].ID || msg.Seq != i ||
			msg.Content != fmt.Sprintf("msg-%d", i) {
			t.Fatalf("message[%d] not exactly restored: %+v", i, msg)
		}
	}
	meta, err := s.GetSession(sid)
	if err != nil || meta == nil || meta.Turns != 4 || meta.UpdatedAt != snap.Meta.UpdatedAt {
		t.Fatalf("restored meta = %+v, err=%v; want snapshot %+v", meta, err, snap.Meta)
	}
}

func TestTruncateSessionForRevert_DeleteFailureRollsBackMetaToo(t *testing.T) {
	s, sid := seedRevertSession(t)
	// Deterministic failure injection inside the truncation tx.
	trigger := fmt.Sprintf(`
		CREATE TRIGGER fail_history_truncate
		BEFORE DELETE ON messages
		WHEN OLD.session_id = '%s' AND OLD.seq >= 3
		BEGIN SELECT RAISE(ABORT, 'injected truncate failure'); END;
	`, sid)
	if _, err := s.DB.Exec(trigger); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if _, err := s.TruncateSessionForRevert(sid, 3, 1); err == nil {
		t.Fatal("expected injected truncation failure")
	}
	msgs, err := s.Messages(sid)
	if err != nil || len(msgs) != 5 {
		t.Fatalf("messages changed after failed tx: %d, err=%v", len(msgs), err)
	}
	meta, err := s.GetSession(sid)
	if err != nil || meta == nil || meta.Turns != 4 {
		t.Fatalf("meta changed after failed tx: %+v, err=%v", meta, err)
	}
}

func TestTruncateSessionForRevert_RejectsMissingSession(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.TruncateSessionForRevert("missing", 0, 0); err == nil {
		t.Fatal("missing session must be an error, not a silent no-op")
	}
}
