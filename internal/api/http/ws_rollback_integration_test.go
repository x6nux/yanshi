package http

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// TestRB1_EndToEnd_Deterministic covers file/head rollback, durable truncation,
// full-head confirmation, and D2's history-aware undo seam round trip.
func TestRB1_EndToEnd_Deterministic(t *testing.T) {
	h := newRollbackIntegrationHarness(t)

	// Seed is v0. Turn 1 seals pre/post seams at v0, then external deterministic
	// edit advances main to v1. Turn 2 seals at v1, then advance to v2.
	h.turn("first turn")
	h.advance("v1")
	h.turn("second turn")
	h.advance("v2")

	listed := h.listSeams()
	require.NotEmpty(t, listed.Head, "D6: list must bind the full main_head")
	v2Head, err := h.v.RepoMainHead(h.repoID)
	require.NoError(t, err)
	require.Equal(t, v2Head, listed.Head, "Head must be full id, not CommitShort")

	var pre1 proto.SeamInfo
	for i := len(listed.Seams) - 1; i >= 0; i-- { // newest-first list
		if listed.Seams[i].Kind == string(vcs.SeamPreTurn) {
			pre1 = listed.Seams[i]
			break
		}
	}
	require.NotEmpty(t, pre1.ID, "oldest pre-turn seam")
	target, err := h.v.FindSeam(pre1.ID)
	require.NoError(t, err)

	sessions, err := h.st.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	sid := sessions[0].ID
	beforeMessages, err := h.st.Messages(sid)
	require.NoError(t, err)
	beforeMeta, err := h.st.GetSession(sid)
	require.NoError(t, err)
	require.NotNil(t, beforeMeta)
	require.Greater(t, len(beforeMessages), target.HistoryLen)

	// First restore: use the FULL list binding; the reply also carries the new
	// full head for the next destructive confirmation.
	restored := h.restore(pre1.ID, listed.Head)
	require.NotEmpty(t, restored.ID, "returned pre-revert undo seam id")
	require.NotEmpty(t, restored.Head, "seam_restored must return full current head")

	got, err := os.ReadFile(h.file)
	require.NoError(t, err)
	require.Equal(t, "v0", string(got))
	headAfter, err := h.v.RepoMainHead(h.repoID)
	require.NoError(t, err)
	require.Equal(t, target.CommitID, headAfter)
	require.Equal(t, headAfter, restored.Head)
	require.Empty(t, h.v.Uncommitted("main", h.repoID))

	truncated, err := h.st.Messages(sid)
	require.NoError(t, err)
	require.Len(t, truncated, target.HistoryLen)
	truncatedMeta, err := h.st.GetSession(sid)
	require.NoError(t, err)
	require.Equal(t, target.TurnSeq, truncatedMeta.Turns)

	// D2: the returned undo seam preserves both integer boundaries and the exact
	// durable snapshot that was deleted by the first restore.
	undo, err := h.v.FindSeam(restored.ID)
	require.NoError(t, err)
	require.Equal(t, vcs.SeamPreRevert, undo.Kind)
	require.Equal(t, len(beforeMessages), undo.PrevHistoryLen)
	require.Equal(t, beforeMeta.Turns, undo.PrevTurnSeq)
	undoSnap, err := store.DecodeSessionRevertSnapshot(undo.HistorySnapshot)
	require.NoError(t, err)
	require.Equal(t, beforeMessages, undoSnap.Messages)
	require.Equal(t, beforeMeta.Turns, undoSnap.Meta.Turns)

	// Restore the undo seam using the first reply's FULL head. This must expand
	// history from the durable blob (simple slicing cannot recover deleted rows).
	undone := h.restore(restored.ID, restored.Head)
	require.NotEmpty(t, undone.ID)
	require.Equal(t, v2Head, undone.Head)
	got, err = os.ReadFile(h.file)
	require.NoError(t, err)
	require.Equal(t, "v2", string(got))
	finalHead, err := h.v.RepoMainHead(h.repoID)
	require.NoError(t, err)
	require.Equal(t, v2Head, finalHead)

	afterUndoMessages, err := h.st.Messages(sid)
	require.NoError(t, err)
	require.Equal(t, beforeMessages, afterUndoMessages,
		"undo must restore exact ids/seq/content, not only message count")
	afterUndoMeta, err := h.st.GetSession(sid)
	require.NoError(t, err)
	require.Equal(t, beforeMeta.Turns, afterUndoMeta.Turns)
}

type rollbackIntegrationHarness struct {
	t      *testing.T
	c      *websocket.Conn
	v      *vcs.VCS
	st     *store.Store
	repoID string
	file   string
}

func newRollbackIntegrationHarness(t *testing.T) *rollbackIntegrationHarness {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))

	// Keep SQLite and generated worktrees OUTSIDE repo root; InitRepo must not
	// accidentally snapshot its own live DB/WAL files.
	st, err := store.Open(filepath.Join(base, "state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	file := filepath.Join(root, "a.txt")
	require.NoError(t, os.WriteFile(file, []byte("v0"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "test", file, []byte("v0")))
	_, err = v.CommitMain(repoID, "test", "seed v0")
	require.NoError(t, err)

	o, err := orchestrator.New(orchestrator.Config{
		Model: einollm.NewFakeModel([]string{"ok", "ok"}, nil),
	})
	require.NoError(t, err)
	s := New(Config{Store: st, VCS: v, RepoID: repoID})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	url := "ws" + ts.URL[len("http"):] + "/api/v1/chat/ws"
	c := dial(t, url)
	t.Cleanup(func() { _ = c.Close() })

	return &rollbackIntegrationHarness{
		t: t, c: c, v: v, st: st, repoID: repoID, file: file,
	}
}

func (h *rollbackIntegrationHarness) turn(text string) {
	h.t.Helper()
	require.NoError(h.t, h.c.WriteJSON(proto.NewUserMessage(text)))
	for {
		var f proto.ServerFrame
		require.NoError(h.t, h.c.ReadJSON(&f))
		if f.Type == "error" {
			h.t.Fatalf("turn error: %s", f.Text)
		}
		if f.Type == "done" {
			return
		}
	}
}

func (h *rollbackIntegrationHarness) advance(content string) {
	h.t.Helper()
	require.NoError(h.t, os.WriteFile(h.file, []byte(content), 0o644))
	require.NoError(h.t,
		h.v.RecordEditMain(h.repoID, "test", h.file, []byte(content)))
	_, err := h.v.CommitMain(h.repoID, "test", content)
	require.NoError(h.t, err)
}

func (h *rollbackIntegrationHarness) listSeams() proto.ServerFrame {
	h.t.Helper()
	for {
		require.NoError(h.t, h.c.WriteJSON(proto.NewListSeams()))
		var f proto.ServerFrame
		require.NoError(h.t, h.c.ReadJSON(&f))
		if f.Type == "error" && contains(f.Text, "while a turn is running") {
			continue // transient: closure's defer chain hasn't finished yet
		}
		if f.Type == "error" {
			h.t.Fatalf("list_seams error: %s", f.Text)
		}
		if f.Type == "seams" {
			return f
		}
	}
}

func (h *rollbackIntegrationHarness) restore(seamID, fullHead string) proto.ServerFrame {
	h.t.Helper()
	require.NotEmpty(h.t, fullHead, "D6 confirmed_head")
	for {
		require.NoError(h.t, h.c.WriteJSON(proto.NewRestoreTurn(seamID, fullHead)))
		var restored proto.ServerFrame
		for {
			var f proto.ServerFrame
			require.NoError(h.t, h.c.ReadJSON(&f))
			if f.Type == "error" {
				if contains(f.Text, "while a turn is running") {
					break // retry
				}
				h.t.Fatalf("restore_turn error: %s", f.Text)
			}
			if f.Type == "seam_restored" {
				restored = f
			}
			if f.Type == "done" {
				require.Equal(h.t, "seam_restored", restored.Type)
				return restored
			}
		}
	}
}
