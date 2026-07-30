package http

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// TestSSE_PreAndPostTurnSeamsCreated verifies that a single SSE chat turn
// creates both pre-turn and post-turn seams (transport parity with WS, 必修项 D).
func TestSSE_PreAndPostTurnSeamsCreated(t *testing.T) {
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
	srv := New(Config{Store: st, VCS: v, RepoID: repoID})
	// CB5: real deterministic Fake orchestrator — never pass nil (the handler
	// dereferences it in the runner path). Helper is defined in Task 9's
	// ws_seam_test.go and available to all tests in package http.
	o := newFakeOrchestrator(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/chat",
		strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	// CB5: call the extracted core directly — no mux re-registration.
	srv.handleSSEInternal(rec, req, o, nil, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	seams, err := v.ListSeams(repoID, "", 0)
	if err != nil {
		t.Fatalf("ListSeams: %v", err)
	}
	if len(seams) < 2 {
		t.Errorf("SSE turn should create >=2 seams, got %d", len(seams))
	}
}
