package http

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// TestCov_HandleWorkspaceDiff_VcsNil covers the VCS-unconfigured guard: an
// empty (nil) reply, never a panic on the nil *vcs.VCS receiver.
func TestCov_HandleWorkspaceDiff_VcsNil(t *testing.T) {
	wc, client, cleanup := newWSPair(t)
	defer cleanup()

	handleWorkspaceDiff(&Server{}, wc)

	_, msg, err := client.ReadMessage()
	require.NoError(t, err)
	assert.Contains(t, string(msg), "workspace_diff")
}

// TestCov_HandleWorkspaceDiff_StoreError covers the UncommittedDiff error
// branch by closing the DB before the call — mirrors
// TestCov_ListSeams_StoreError's technique for the seam handler.
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
	handleWorkspaceDiff(s, wc)

	_, msg, err := client.ReadMessage()
	require.NoError(t, err)
	var sf proto.ServerFrame
	require.NoError(t, json.Unmarshal(msg, &sf))
	require.Equal(t, "error", sf.Type)
	assert.Contains(t, sf.Text, "workspace_diff")
}

// TestWS_WorkspaceDiff_EndToEnd drives the real dispatch path (ws.go's
// "list_workspace_diff" case) over a live WS connection: seed a pending edit
// via RecordEditMain (the same code path fs_write instrumentation uses),
// request list_workspace_diff, and assert the reply carries the old/new text.
func TestWS_WorkspaceDiff_EndToEnd(t *testing.T) {
	url, v, repoID, _ := setupSeamServerFull(t)

	root, err := v.RepoRoot(repoID)
	require.NoError(t, err)
	origPath := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(origPath, []byte("package main"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "test", origPath, []byte("package main")))
	commitID, err := v.CommitMain(repoID, "test", "seed main.go")
	require.NoError(t, err)
	require.NotEmpty(t, commitID)
	require.NoError(t, v.RecordEditMain(repoID, "test", origPath, []byte("package main // edited")))

	c := dial(t, url)
	defer c.Close()

	require.NoError(t, c.WriteJSON(proto.NewListWorkspaceDiff()))
	var sf proto.ServerFrame
	require.NoError(t, c.ReadJSON(&sf))
	require.Equal(t, "workspace_diff", sf.Type)
	require.Len(t, sf.WorkspaceDiff, 1)
	got := sf.WorkspaceDiff[0]
	assert.Equal(t, "main.go", got.Path)
	assert.Equal(t, "modified", got.Op)
	assert.Equal(t, "package main", got.OldText)
	assert.Equal(t, "package main // edited", got.NewText)
}

// TestWS_WorkspaceDiff_EmptyWhenNothingPending proves an established session
// with no pending edits gets an empty (not nil-panicking, not error) reply.
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
