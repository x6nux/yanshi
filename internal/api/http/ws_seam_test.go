package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
	"github.com/x6nux/yanshi/internal/vcs"
)

// newFakeOrchestrator builds a real orchestrator over a deterministic FakeModel,
// matching the pattern in ws_test.go (NOT a nil orchestrator — chat.go panics on
// nil). Used by setupSeamServer and the in-turn rejection test.
func newFakeOrchestrator(t *testing.T) *orchestrator.Orchestrator {
	t.Helper()
	o, err := orchestrator.New(orchestrator.Config{
		Model: einollm.NewFakeModel([]string{"ok"}, nil),
	})
	require.NoError(t, err)
	return o
}

// newBlockingOrchestrator uses the repository's real BlockingModel test fake.
// Started removes timing sleeps; Block controls deterministic completion.
func newBlockingOrchestrator(t *testing.T) (
	*orchestrator.Orchestrator, *einollm.BlockingModel,
) {
	t.Helper()
	blocking := einollm.NewBlockingModel("ok")
	o, err := orchestrator.New(orchestrator.Config{Model: blocking})
	require.NoError(t, err)
	return o, blocking
}

// setupSeamServer boots a real httptest server with a real VCS + repo + Fake
// orchestrator, wired exactly the way bootstrap wires a production server.
func setupSeamServer(t *testing.T) (baseURL string, v *vcs.VCS, repoID string) {
	t.Helper()
	baseURL, v, repoID, _ = setupSeamServerFull(t)
	return baseURL, v, repoID
}

func setupSeamServerFull(t *testing.T) (
	baseURL string, v *vcs.VCS, repoID string, st *store.Store,
) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	var err error
	st, err = store.Open(filepath.Join(base, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	v = vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err = v.InitRepo(root)
	require.NoError(t, err)
	o := newFakeOrchestrator(t)
	s := New(Config{Store: st, VCS: v, RepoID: repoID})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return "ws" + ts.URL[len("http"):] + "/api/v1/chat/ws", v, repoID, st
}

// setupSeamServerWithOrchestrator installs a caller-supplied orchestrator and
// returns the real VCS/store so cancel and disconnect tests can inspect the
// concrete session's seams without an all-session query.
func setupSeamServerWithOrchestrator(t *testing.T, o *orchestrator.Orchestrator) (
	string, *vcs.VCS, string, *store.Store,
) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	st, err := store.Open(filepath.Join(base, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	s := New(Config{Store: st, VCS: v, RepoID: repoID})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return "ws" + ts.URL[len("http"):] + "/api/v1/chat/ws", v, repoID, st
}

// drainTurn reads frames until a "done" frame arrives (one full turn).
func drainTurn(t *testing.T, c *websocket.Conn) {
	t.Helper()
	for {
		var sf proto.ServerFrame
		require.NoError(t, c.ReadJSON(&sf))
		if sf.Type == "done" {
			return
		}
	}
}

// waitForSessionSeams obtains the concrete durable session id and polls the
// session-scoped list. Tests never use ListSeams(repoID, "", ...), because an
// empty filter would hide D7 regressions by admitting other sessions' seams.
func waitForSessionSeams(t *testing.T, st *store.Store, v *vcs.VCS,
	repoID string, want int,
) (string, []vcs.Seam) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sessions, err := st.ListSessions(0)
		require.NoError(t, err)
		if len(sessions) == 1 {
			seams, err := v.ListSeams(repoID, sessions[0].ID, 0)
			require.NoError(t, err)
			if len(seams) >= want {
				return sessions[0].ID, seams
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for session-scoped seams")
	return "", nil
}

func TestWS_PreAndPostTurnSeamsCreated(t *testing.T) {
	url, v, repoID, st := setupSeamServerFull(t)
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hello")))
	drainTurn(t, c)

	_, seams := waitForSessionSeams(t, st, v, repoID, 2)
	if len(seams) < 2 {
		t.Errorf("expected >=2 session seams after first turn, got %d", len(seams))
	}
}

func TestWS_ListSeamsWhileInTurnRejected(t *testing.T) {
	o, blocking := newBlockingOrchestrator(t)
	defer close(blocking.Block)
	url, _, _, _ := setupSeamServerWithOrchestrator(t, o)
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("thinking...")))
	select {
	case <-blocking.Started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking model did not start")
	}
	require.NoError(t, c.WriteJSON(proto.NewListSeams()))

	gotReject := false
	for !gotReject {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		var sf proto.ServerFrame
		if err := c.ReadJSON(&sf); err != nil {
			t.Fatalf("read: %v", err)
		}
		if sf.Type == "error" && contains(sf.Text, "while a turn is running") {
			gotReject = true
		}
		if sf.Type == "done" {
			t.Fatal("got done before the reject")
		}
	}
}

func TestWS_PostTurnSeamFiresOnCancel(t *testing.T) {
	o, blocking := newBlockingOrchestrator(t)
	t.Cleanup(func() { close(blocking.Block) })
	url, v, repoID, st := setupSeamServerWithOrchestrator(t, o)
	c := dial(t, url)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("long")))
	select {
	case <-blocking.Started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking model did not start")
	}
	require.NoError(t, c.WriteJSON(proto.NewCancel()))
	drainTurn(t, c)
	_, seams := waitForSessionSeams(t, st, v, repoID, 2)
	if len(seams) < 2 {
		t.Errorf("post-turn seam should still fire on cancel; got %d seams", len(seams))
	}
}

func TestWS_PostTurnSeamFiresOnDisconnect(t *testing.T) {
	o, blocking := newBlockingOrchestrator(t)
	t.Cleanup(func() { close(blocking.Block) })
	url, v, repoID, st := setupSeamServerWithOrchestrator(t, o)
	_, _, _ = v, repoID, st
	c := dial(t, url)
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("disconnect")))
	select {
	case <-blocking.Started:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking model did not start")
	}
	require.NoError(t, c.Close())
	_, seams := waitForSessionSeams(t, st, v, repoID, 2)
	if len(seams) < 2 {
		t.Errorf("post-turn seam should still fire on disconnect; got %d seams", len(seams))
	}
}

func TestWS_PostTurnSeamFiresOnModelError(t *testing.T) {
	o, err := orchestrator.New(orchestrator.Config{
		Model: einollm.NewFakeModel(nil, context.DeadlineExceeded),
	})
	require.NoError(t, err)
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	st, err := store.Open(filepath.Join(base, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	s := New(Config{Store: st, VCS: v, RepoID: repoID})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	url := "ws" + ts.URL[len("http"):] + "/api/v1/chat/ws"
	c := dial(t, url)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("trigger error")))
	drainTurn(t, c)
	_, seams := waitForSessionSeams(t, st, v, repoID, 2)
	if len(seams) < 2 {
		t.Errorf("post-turn seam should fire on model error; got %d seams", len(seams))
	}
}

func TestWS_RestoreTurn_HappyPath(t *testing.T) {
	url, v, _ := setupSeamServer(t)
	c := dial(t, url)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("first")))
	drainTurn(t, c)
	// list_seams may be rejected with "while a turn is running" while the
	// closure's postTurnSeam defer chain finishes (LIFO: postSeam → release →
	// setInTurn(false)). Retry until the server reaches idle.
	var seamsFrame proto.ServerFrame
	for {
		require.NoError(t, c.WriteJSON(proto.NewListSeams()))
		require.NoError(t, c.ReadJSON(&seamsFrame))
		if seamsFrame.Type == "seams" {
			break
		}
		if seamsFrame.Type == "error" && contains(seamsFrame.Text, "while a turn is running") {
			continue
		}
		t.Fatalf("unexpected frame: %+v", seamsFrame)
	}
	if len(seamsFrame.Seams) == 0 {
		t.Fatal("no seams returned")
	}
	confirmedHead := seamsFrame.Head
	if confirmedHead == "" {
		t.Fatal("seams frame missing full Head binding")
	}
	pre1 := seamsFrame.Seams[len(seamsFrame.Seams)-1]

	// restore_turn may also race with the defer chain; retry the same way.
	var restored proto.ServerFrame
	for {
		require.NoError(t, c.WriteJSON(proto.NewRestoreTurn(pre1.ID, confirmedHead)))
		require.NoError(t, c.ReadJSON(&restored))
		if restored.Type == "seam_restored" || (restored.Type == "error" && !contains(restored.Text, "while a turn is running")) {
			break
		}
	}
	if restored.Type == "error" {
		t.Fatalf("restore_turn error: %s", restored.Text)
	}
	if restored.ID == "" {
		t.Error("undo seam id (seam_restored.ID) is empty")
	}
	undoSeam, err := v.FindSeam(restored.ID)
	require.NoError(t, err)
	if undoSeam.Kind != vcs.SeamPreRevert {
		t.Errorf("undo seam.Kind = %q, want %q", undoSeam.Kind, vcs.SeamPreRevert)
	}
}

func TestWS_RestoreTurn_EmptyOrMismatchedHeadRejected(t *testing.T) {
	url, _, _, _ := setupSeamServerFull(t)
	c := dial(t, url)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("seed")))
	drainTurn(t, c)
	var sf proto.ServerFrame
	// restore_turn may transiently be rejected with "while a turn is running"
	// because setInTurn(false) runs in a defer AFTER the done frame is written
	// — a frame arriving in that window hits the reader's inline reject path
	// (error, no done). Retry until the handler actually processes it.
	for {
		require.NoError(t, c.WriteJSON(proto.NewRestoreTurn("some-seam", "")))
		require.NoError(t, c.ReadJSON(&sf))
		if sf.Type == "error" && !contains(sf.Text, "while a turn is running") {
			break
		}
	}
	// Drain any trailing frames (the handler sends error+done).
	for {
		require.NoError(t, c.ReadJSON(&sf))
		if sf.Type == "done" {
			break
		}
	}
	for {
		require.NoError(t, c.WriteJSON(proto.NewRestoreTurn("some-seam", "deadbeef-not-the-real-head")))
		require.NoError(t, c.ReadJSON(&sf))
		if sf.Type == "error" && !contains(sf.Text, "while a turn is running") {
			break
		}
	}
}

func TestWS_RestoreTurn_PersistFailureIsFatalBeforeVCS(t *testing.T) {
	url, v, repoID, st := setupSeamServerFull(t)
	c := dial(t, url)
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("first")))
	drainTurn(t, c)
	// list_seams may transiently reject while the closure's defer chain finishes.
	var listed proto.ServerFrame
	for {
		require.NoError(t, c.WriteJSON(proto.NewListSeams()))
		require.NoError(t, c.ReadJSON(&listed))
		if listed.Type == "seams" {
			break
		}
		if listed.Type == "error" && contains(listed.Text, "while a turn is running") {
			continue
		}
		t.Fatalf("unexpected frame: %+v", listed)
	}
	require.NotEmpty(t, listed.Seams)
	target := listed.Seams[len(listed.Seams)-1]
	headBefore, err := v.RepoMainHead(repoID)
	require.NoError(t, err)
	sessions, err := st.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	sid := sessions[0].ID
	messagesBefore, err := st.Messages(sid)
	require.NoError(t, err)
	trigger := fmt.Sprintf(`
		CREATE TRIGGER fail_ws_history_truncate
		BEFORE DELETE ON messages
		WHEN OLD.session_id = '%s'
		BEGIN SELECT RAISE(ABORT, 'injected ws truncate failure'); END;
	`, sid)
	_, err = st.DB.Exec(trigger)
	require.NoError(t, err)

	require.NoError(t, c.WriteJSON(proto.NewRestoreTurn(target.ID, listed.Head)))
	sawFatal, sawRestored := false, false
	for {
		var sf proto.ServerFrame
		require.NoError(t, c.ReadJSON(&sf))
		if sf.Type == "error" && contains(sf.Text, "FATAL: durable history truncation failed") {
			sawFatal = true
		}
		if sf.Type == "seam_restored" {
			sawRestored = true
		}
		if sf.Type == "done" {
			break
		}
	}
	if !sawFatal || sawRestored {
		t.Fatalf("fatal=%v seam_restored=%v; want true/false", sawFatal, sawRestored)
	}
	gotHead, err := v.RepoMainHead(repoID)
	require.NoError(t, err)
	if gotHead != headBefore {
		t.Fatalf("VCS head changed despite durable failure: got %s want %s", gotHead, headBefore)
	}
	messagesAfter, err := st.Messages(sid)
	require.NoError(t, err)
	if len(messagesAfter) != len(messagesBefore) {
		t.Fatalf("durable history changed despite failed tx: before=%d after=%d",
			len(messagesBefore), len(messagesAfter))
	}
}

func TestWS_RestoreTurn_CrossSessionRejected(t *testing.T) {
	url, _, _ := setupSeamServer(t)
	c1 := dial(t, url)
	defer c1.Close()
	c2 := dial(t, url)
	defer c2.Close()

	require.NoError(t, c1.WriteJSON(proto.NewUserMessage("c1")))
	drainTurn(t, c1)
	var c1List proto.ServerFrame
	for {
		require.NoError(t, c1.WriteJSON(proto.NewListSeams()))
		require.NoError(t, c1.ReadJSON(&c1List))
		if c1List.Type == "seams" {
			break
		}
		if c1List.Type == "error" && contains(c1List.Text, "while a turn is running") {
			continue
		}
		t.Fatalf("unexpected c1 frame: %+v", c1List)
	}
	require.NotEmpty(t, c1List.Seams)
	foreignID := c1List.Seams[0].ID

	require.NoError(t, c2.WriteJSON(proto.NewUserMessage("c2")))
	drainTurn(t, c2)
	var c2List proto.ServerFrame
	for {
		require.NoError(t, c2.WriteJSON(proto.NewListSeams()))
		require.NoError(t, c2.ReadJSON(&c2List))
		if c2List.Type == "seams" {
			break
		}
		if c2List.Type == "error" && contains(c2List.Text, "while a turn is running") {
			continue
		}
		t.Fatalf("unexpected c2 frame: %+v", c2List)
	}
	require.NoError(t, c2.WriteJSON(proto.NewRestoreTurn(foreignID, c2List.Head)))
	var sf proto.ServerFrame
	require.NoError(t, c2.ReadJSON(&sf))
	if sf.Type != "error" || !contains(sf.Text, "does not belong to this session") {
		t.Fatalf("cross-session restore = type %q text %q; want session error", sf.Type, sf.Text)
	}
}

// TestResolvePermissionRequest_ForceNeverAutoResolves proves D3 at the exact
// WS mode gate: a Force request is unresolved under every auto-allowing mode,
// so the callback must fall through to permission_request + explicit response.
func TestResolvePermissionRequest_ForceNeverAutoResolves(t *testing.T) {
	for _, mode := range []guard.PermissionMode{
		guard.ModeYOLO,
		guard.ModeAllowEdits,
		guard.ModeAuto,
	} {
		t.Run(string(mode), func(t *testing.T) {
			cs := connSession{perm: &permModeState{}}
			cs.perm.set(mode)
			decision, resolved := resolvePermissionRequest(
				context.Background(), &cs, nil,
				&tools.PermissionRequest{Tool: "fs_write", Force: true},
			)
			if resolved {
				t.Fatalf("Force request auto-resolved in mode %q with decision %v", mode, decision)
			}
		})
	}
}

// contains is a tiny substring helper.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// drainJSON exists for future tests that decode via a stream reader.
func drainJSON(t *testing.T, dec *json.Decoder) {
	t.Helper()
	for {
		var sf proto.ServerFrame
		if err := dec.Decode(&sf); err != nil {
			return
		}
		if sf.Type == "done" {
			return
		}
	}
}
