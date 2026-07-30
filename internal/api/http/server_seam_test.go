package http

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// TestServer_StoresVCSAndRepoID verifies that New wires the VCS + repoID
// supplied via Config, so downstream handlers (ChatWS / Chat) can use them.
func TestServer_StoresVCSAndRepoID(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	st, err := store.Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	if err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	srv := New(Config{
		Store:  st,
		VCS:    v,
		RepoID: repoID,
	})
	if srv.vcs != v {
		t.Errorf("srv.vcs not wired")
	}
	if srv.repoID != repoID {
		t.Errorf("srv.repoID = %q, want %q", srv.repoID, repoID)
	}
}

// TestServer_SealTurnBoundary_NoopsWithoutVCS verifies the silent no-op path
// when VCS is unconfigured (the common test path for handlers built without
// a real VCS).
func TestServer_SealTurnBoundary_NoopsWithoutVCS(t *testing.T) {
	srv := New(Config{Store: nil, VCS: nil, RepoID: ""})
	// Must not panic / log.
	srv.sealTurnBoundary("s1", 0, 0, "pre-turn", "no-vcs")
}

// TestServer_ShortHead_NoVCS verifies shortHead returns "" when VCS is nil.
func TestServer_ShortHead_NoVCS(t *testing.T) {
	srv := New(Config{})
	if got := srv.shortHead(); got != "" {
		t.Errorf("shortHead() = %q, want empty", got)
	}
}

// TestServer_ShortHead_WithVCS verifies shortHead returns a non-empty short
// hash when VCS is configured.
func TestServer_ShortHead_WithVCS(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Config{Store: st, VCS: v, RepoID: repoID})
	if got := srv.shortHead(); len(got) < 8 {
		t.Errorf("shortHead() = %q (len %d), want >=8 chars", got, len(got))
	}
	if got, err := v.RepoMainHead(repoID); err != nil || srv.fullHead() != got {
		t.Errorf("fullHead() = %q, RepoMainHead=%q err=%v", srv.fullHead(), got, err)
	}
}
