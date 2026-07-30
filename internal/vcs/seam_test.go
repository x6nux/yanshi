package vcs

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/x6nux/yanshi/internal/store"
)

// setupSeamTestRepo mirrors newSeamRaceRepo but lives here so both files compile
// independently. Returns a VCS with a seeded repo + initial commit.
func setupSeamTestRepo(t *testing.T) (*VCS, string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	st, err := store.Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	v := New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	seedPath := filepath.Join(root, "a.txt")
	if err := os.WriteFile(seedPath, []byte("v0"), 0o644); err != nil {
		t.Fatalf("seed WriteFile: %v", err)
	}
	if err := v.RecordEditMain(repoID, "test", seedPath, []byte("v0")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := v.CommitMain(repoID, "test", "seed"); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return v, repoID, root
}

// mustMainHead reads the repo's current main_head; helper for tests.
func mustMainHead(t *testing.T, v *VCS, repoID string) string {
	t.Helper()
	h, err := v.RepoMainHead(repoID)
	if err != nil {
		t.Fatalf("RepoMainHead: %v", err)
	}
	return h
}

func TestSealMainTurnSeam_RecordsSeamAtCurrentHead(t *testing.T) {
	v, repoID, root := setupSeamTestRepo(t)
	head0 := mustMainHead(t, v, repoID)

	// Pending edit must be folded into a new commit, then sealed.
	if err := v.RecordEditMain(repoID, "u", filepath.Join(root, "a.txt"), []byte("v1")); err != nil {
		t.Fatalf("RecordEditMain: %v", err)
	}
	seamID, err := v.SealMainTurnSeam(repoID, "s1", 1, 2, SeamPostTurn, "post-turn:1")
	if err != nil {
		t.Fatalf("SealMainTurnSeam: %v", err)
	}
	if seamID == "" {
		t.Fatal("seamID 空串")
	}
	head1 := mustMainHead(t, v, repoID)
	if head1 == head0 {
		t.Fatal("pending edit 未被 SealMainTurnSeam fold 成新 commit")
	}
	seam, err := v.FindSeam(seamID)
	if err != nil {
		t.Fatalf("FindSeam: %v", err)
	}
	if seam.CommitID != head1 {
		t.Errorf("seam.CommitID = %s, want %s", seam.CommitID, head1)
	}
	if seam.SessionID != "s1" {
		t.Errorf("seam.SessionID = %q, want %q", seam.SessionID, "s1")
	}
	if seam.TurnSeq != 1 || seam.HistoryLen != 2 {
		t.Errorf("seam.TurnSeq=%d HistoryLen=%d, want 1/2", seam.TurnSeq, seam.HistoryLen)
	}
	if seam.Kind != SeamPostTurn {
		t.Errorf("seam.Kind = %q, want %q", seam.Kind, SeamPostTurn)
	}
}

func TestSealMainTurnSeam_NoPendingUsesCurrentHead(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	head := mustMainHead(t, v, repoID)
	seamID, err := v.SealMainTurnSeam(repoID, "s1", 1, 2, SeamPreTurn, "pre-turn:1")
	if err != nil {
		t.Fatalf("SealMainTurnSeam: %v", err)
	}
	seam, _ := v.FindSeam(seamID)
	if seam.CommitID != head {
		t.Errorf("no-pending seam.CommitID = %s, want %s", seam.CommitID, head)
	}
}

// TestListSeams_OrderedBySeqDesc inserts 3 seams in the same second and asserts
// they come back latest-first. 必修项 H: seq must be the SOLE sort key.
func TestListSeams_OrderedBySeqDesc(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	ids := []string{}
	for i := 0; i < 3; i++ {
		id, err := v.SealMainTurnSeam(repoID, "s1", i, i, SeamPreTurn, "p"+strconv.Itoa(i))
		if err != nil {
			t.Fatalf("SealMainTurnSeam[%d]: %v", i, err)
		}
		ids = append(ids, id)
	}
	seams, err := v.ListSeams(repoID, "s1", 0)
	if err != nil {
		t.Fatalf("ListSeams: %v", err)
	}
	if len(seams) != 3 {
		t.Fatalf("ListSeams returned %d seams, want 3", len(seams))
	}
	// Latest-first: ids[2], ids[1], ids[0]
	for i, wantReversed := range []string{ids[2], ids[1], ids[0]} {
		if seams[i].ID != wantReversed {
			t.Errorf("seams[%d].ID = %s, want %s", i, seams[i].ID, wantReversed)
		}
	}
}

// TestListSeams_FiltersBySession — 必修项 J: seams from other sessions must
// NOT leak.
func TestListSeams_FiltersBySession(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	_, _ = v.SealMainTurnSeam(repoID, "session-A", 1, 1, SeamPreTurn, "a1")
	_, _ = v.SealMainTurnSeam(repoID, "session-B", 1, 1, SeamPreTurn, "b1")
	_, _ = v.SealMainTurnSeam(repoID, "session-A", 2, 2, SeamPreTurn, "a2")
	a, _ := v.ListSeams(repoID, "session-A", 0)
	if len(a) != 2 {
		t.Errorf("session-A: got %d seams, want 2", len(a))
	}
	b, _ := v.ListSeams(repoID, "session-B", 0)
	if len(b) != 1 {
		t.Errorf("session-B: got %d seams, want 1", len(b))
	}
}

func TestRepoMainHead_Exported(t *testing.T) {
	v, repoID, _ := setupSeamTestRepo(t)
	head, err := v.RepoMainHead(repoID)
	if err != nil {
		t.Fatalf("RepoMainHead: %v", err)
	}
	if head == "" {
		t.Fatal("RepoMainHead 返回空 head")
	}
}
