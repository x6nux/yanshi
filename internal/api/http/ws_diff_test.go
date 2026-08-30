package http

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
	"github.com/x6nux/yanshi/internal/vcs"
)

// TestCov_HandleWorkspaceDiff_VcsNil covers the VCS-unconfigured guard: an
// error reply (RE-6, not an empty WorkspaceDiff — that would be
// indistinguishable from "VCS enabled, nothing pending"), never a panic on
// the nil *vcs.VCS receiver.
func TestCov_HandleWorkspaceDiff_VcsNil(t *testing.T) {
	wc, client, cleanup := newWSPair(t)
	defer cleanup()

	handleWorkspaceDiff(&Server{}, wc, &connSession{})

	_, msg, err := client.ReadMessage()
	require.NoError(t, err)
	var sf proto.ServerFrame
	require.NoError(t, json.Unmarshal(msg, &sf))
	require.Equal(t, "error", sf.Type)
	assert.Contains(t, sf.Text, "workspace_diff")
}

// TestCov_HandleWorkspaceDiff_StoreError covers the SessionBaseline error
// branch by closing the DB before the call — mirrors
// TestCov_ListSeams_StoreError's technique for the seam handler. cs.sessionID
// must be non-empty: an empty one short-circuits to an empty (non-error)
// reply before SessionBaseline ever touches the closed DB (RE-1's own
// cs.sessionID=="" guard, mirroring handleListSeams).
//
// Asserts sf.Type == "error" specifically, not just Contains(msg,
// "workspace_diff") — that substring is present on BOTH the error branch
// ("workspace_diff: "+err.Error(), proto.NewError) and the success branch
// ({"type":"workspace_diff",...}, proto.NewWorkspaceDiff), so the old
// assertion could not tell a store failure from an empty-but-successful
// reply. Proven by mutation: swapping the error-branch write for
// conn.write(proto.NewWorkspaceDiff(nil)) still passed the old assertion.
func TestCov_HandleWorkspaceDiff_StoreError(t *testing.T) {
	db, err := store.Open(":memory:")
	require.NoError(t, err)
	db.Close()

	wc, client, cleanup := newWSPair(t)
	defer cleanup()

	s := &Server{vcs: vcs.New(db, t.TempDir()), repoID: "test-repo"}
	handleWorkspaceDiff(s, wc, &connSession{sessionID: "sess-1"})

	_, msg, err := client.ReadMessage()
	require.NoError(t, err)
	var sf proto.ServerFrame
	require.NoError(t, json.Unmarshal(msg, &sf))
	require.Equal(t, "error", sf.Type)
	assert.Contains(t, sf.Text, "workspace_diff")
}

// TestWS_WorkspaceDiff_RealTurnBoundary drives a REAL turn boundary (RE-1),
// replacing the old TestWS_WorkspaceDiff_EndToEnd, which seeded its pending
// edit via RecordEditMain called directly from the test goroutine — a path
// that never touches sealTurnBoundary at all, so it could not have caught
// the bug it was meant to guard: vcs_uncommitted is folded into a commit and
// emptied by SealMainTurnSeam both immediately BEFORE and immediately AFTER
// every turn, so it is structurally empty at essentially every moment a user
// could actually type /diff (see handleWorkspaceDiff's doc comment).
//
// This test instead: (1) opens a real WS connection over a real VCS-backed
// repo with a real orchestrator wired with fs_write and an injected
// VCSScope — the same tool + scope-injection path production agent edits
// take (CLAUDE.md's autoVCS section); (2) sends a real user_message and
// drains the whole turn, so sealTurnBoundary(pre-turn) and
// sealTurnBoundary(post-turn) both run for real around the fake model's
// scripted fs_write call; (3) only THEN sends list_workspace_diff — the
// exact moment a user could actually send it, with vcs_uncommitted already
// folded back to empty by the post-turn seal.
//
// Reverting handleWorkspaceDiff to its pre-RE-1 vcs_uncommitted-only
// implementation makes this test fail (sf.WorkspaceDiff empty instead of
// containing hello.go) — see fix-e2-report.md for the pasted red run.
func TestWS_WorkspaceDiff_RealTurnBoundary(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	require.NoError(t, os.MkdirAll(root, 0o755))
	st, err := store.Open(filepath.Join(base, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	v := vcs.New(st, filepath.Join(base, "worktrees"))
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	fs := tools.NewFSTools(root)
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "fs_write",
			Arguments: `{"path":"hello.go","content":"package main\n"}`,
		}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	o, err := orchestrator.New(orchestrator.Config{
		Model: mdl,
		Tools: []orchestrator.BaseTool{fs.Write},
		Profile: guard.PermissionProfile{
			Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
			FS:    guard.FSPerm{Write: []string{root + "/**"}},
		},
		VCSScope: tools.VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"},
		WorkRoot: root,
	})
	require.NoError(t, err)

	s := New(Config{Store: st, VCS: v, RepoID: repoID})
	s.ChatWS(o, nil, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	url := "ws" + ts.URL[len("http"):] + "/api/v1/chat/ws"

	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewUserMessage("write hello.go")))
	drainTurn(t, c)

	require.NoError(t, c.WriteJSON(proto.NewListWorkspaceDiff()))
	var sf proto.ServerFrame
	require.NoError(t, c.ReadJSON(&sf))
	require.Equal(t, "workspace_diff", sf.Type)
	require.Len(t, sf.WorkspaceDiff, 1)
	got := sf.WorkspaceDiff[0]
	assert.Equal(t, "hello.go", got.Path)
	assert.Equal(t, "added", got.Op)
	assert.Equal(t, "", got.OldText)
	assert.Equal(t, "package main\n", got.NewText)
}

// TestWS_WorkspaceDiff_EmptyWhenNothingPending proves a connection that has
// not yet sent a single user_message — cs.sessionID still "" — gets an
// empty (not nil-panicking, not error) reply rather than an error: there is
// no seam namespace yet to anchor a baseline against, which is a different,
// equally valid "nothing to show" from VCS being unconfigured (RE-6 keeps
// those two responses distinct: the former is workspace_diff with an empty
// list, the latter an error frame).
func TestWS_WorkspaceDiff_EmptyWhenNothingPending(t *testing.T) {
	url, _, _ := setupSeamServer(t)
	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewListWorkspaceDiff()))
	var sf proto.ServerFrame
	require.NoError(t, c.ReadJSON(&sf))
	require.Equal(t, "workspace_diff", sf.Type)
	assert.Empty(t, sf.WorkspaceDiff)
}
