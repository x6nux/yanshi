package store

import (
	"encoding/json"
	"testing"
)

// TestVCSSeamsSchema_LocksColumnsAndIndexes validates the vcs_seams table shape.
// It prevents accidental schema drift: a drop of any column or index would
// change the PRAGMA / sqlite_master output and fail this test.
func TestVCSSeamsSchema_LocksColumnsAndIndexes(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	wantCols := map[string]bool{
		"seq": true, "id": true, "repo_id": true, "session_id": true,
		"commit_id": true, "turn_seq": true, "history_len": true,
		"prev_turn_seq": true, "prev_history_len": true,
		"history_snapshot": true,
		"kind": true, "label": true, "created_at": true,
	}
	rows, err := s.DB.Query("PRAGMA table_info(vcs_seams)")
	if err != nil {
		t.Fatalf("vcs_seams 不存在或不可读: %v", err)
	}
	gotCols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		gotCols[name] = true
		if name == "seq" && (typ != "INTEGER" || pk != 1) {
			t.Errorf("seq 列必须是 INTEGER PRIMARY KEY,实际 typ=%s pk=%d", typ, pk)
		}
	}
	rows.Close()
	for c := range wantCols {
		if !gotCols[c] {
			t.Errorf("vcs_seams 缺少列 %q", c)
		}
	}

	idxRows, err := s.DB.Query("SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='vcs_seams'")
	if err != nil {
		t.Fatalf("查询 vcs_seams 索引失败: %v", err)
	}
	gotIdx := map[string]bool{}
	for idxRows.Next() {
		var n string
		_ = idxRows.Scan(&n)
		gotIdx[n] = true
	}
	idxRows.Close()
	for _, want := range []string{"idx_vcs_seams_repo_seq", "idx_vcs_seams_session"} {
		if !gotIdx[want] {
			t.Errorf("vcs_seams 缺少索引 %q", want)
		}
	}
}

// TestOpen_VCSeamsIdempotent verifies the vcs_seams DDL is idempotent.
func TestOpen_VCSeamsIdempotent(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := s.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	s.Close()
}

// TestSessionRevertSnapshotCodec_RoundTripsExactState locks the D2 payload
// contract before vcs.RevertToSeam starts depending on it in Task 5.
func TestSessionRevertSnapshotCodec_RoundTripsExactState(t *testing.T) {
	snap := SessionRevertSnapshot{
		Meta: SessionSummary{ID: "session-1", Turns: 2},
		Messages: []Message{
			{ID: "m1", SessionID: "session-1", Seq: 0, Role: "user", Content: "hello", CreatedAt: 11},
			{ID: "m2", SessionID: "session-1", Seq: 1, Role: "assistant", Content: "world", CreatedAt: 12},
		},
	}
	blob, err := EncodeSessionRevertSnapshot(snap)
	if err != nil {
		t.Fatalf("EncodeSessionRevertSnapshot: %v", err)
	}
	if !json.Valid(blob) {
		t.Fatal("encoded snapshot is not valid JSON")
	}
	got, err := DecodeSessionRevertSnapshot(blob)
	if err != nil {
		t.Fatalf("DecodeSessionRevertSnapshot: %v", err)
	}
	if got.Meta.ID != snap.Meta.ID || got.Meta.Turns != snap.Meta.Turns ||
		len(got.Messages) != 2 || got.Messages[0] != snap.Messages[0] ||
		got.Messages[1] != snap.Messages[1] {
		t.Fatalf("round-trip mismatch: got=%+v want=%+v", got, snap)
	}
}

// TestSessionRevertSnapshotCodec_RejectsEmptyPayload locks fail-closed decode.
func TestSessionRevertSnapshotCodec_RejectsEmptyPayload(t *testing.T) {
	if _, err := DecodeSessionRevertSnapshot(nil); err == nil {
		t.Fatal("empty undo snapshot must be rejected")
	}
}
