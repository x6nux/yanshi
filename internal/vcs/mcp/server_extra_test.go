package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestServe_CtxCancelled covers ctx.Err() at the top of the serve loop.
func TestServe_CtxCancelled(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	out := &bytes.Buffer{}
	err := srv.Serve(ctx, in, out)
	require.ErrorIs(t, err, context.Canceled)
}

// TestServe_EmptyLineSkips covers the blank-line skip in Serve.
func TestServe_EmptyLineSkips(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	srv := New(v, repoID, wt.ID, "agent")
	resps := runServe(t, srv, ``, `   `,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	require.Len(t, resps, 1)
}

// TestServe_BadJSONLine covers json.Unmarshal error in handleLine.
func TestServe_BadJSONLine(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv,
		`not json at all`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	require.Len(t, resps, 1)
}

// TestServe_BadToolsCallParams covers params unmarshal error in handleToolsCall.
func TestServe_BadToolsCallParams(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"not-an-object"}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
	require.Equal(t, -32602, resps[0].Error.Code)
}

// TestServe_CommitEmptyMessage covers the required-message check in callCommit.
func TestServe_CommitEmptyMessage(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_commit","arguments":{"message":""}}}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
	require.Equal(t, -32602, resps[0].Error.Code)
}

// TestServe_CommitInvalidWorktree covers CommitWorktree error.
func TestServe_CommitInvalidWorktree(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "nonexistent-wt", "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_commit","arguments":{"message":"test"}}}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
	require.Equal(t, -32000, resps[0].Error.Code)
}

// TestServe_CommitBadArgs covers decodeArgs error in callCommit.
func TestServe_CommitBadArgs(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_commit","arguments":"not-an-object"}}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
}

// TestServe_LogInvalidArgs covers decodeArgs error in callLog.
func TestServe_LogInvalidArgs(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_log","arguments":"bad"}}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
}

// TestServe_LogWithDefaultLimit covers limit <= 0 -> 20 in callLog.
func TestServe_LogWithDefaultLimit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	srv := New(v, repoID, wt.ID, "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_log"}}`)
	require.Len(t, resps, 1)
	require.Nil(t, resps[0].Error)
}

// TestServe_DiffInvalidArgs covers decodeArgs error in callDiff.
func TestServe_DiffInvalidArgs(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_diff","arguments":"bad"}}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
}

// TestServe_DiffEmptyRefs exercises the worktreeTip resolution path.
func TestServe_DiffEmptyRefs(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	srv := New(v, repoID, wt.ID, "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_diff","arguments":{"ref_a":"","ref_b":""}}}`)
	require.Len(t, resps, 1)
	require.Nil(t, resps[0].Error)
}

// TestServe_RestoreInvalidArgs covers decodeArgs error in callRestore.
func TestServe_RestoreInvalidArgs(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_restore","arguments":"bad"}}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
}

// TestServe_RestoreMissingFields covers required-fields check in callRestore.
func TestServe_RestoreMissingFields(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_restore","arguments":{}}}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
	require.Equal(t, -32602, resps[0].Error.Code)
}

// TestServe_RestoreInvalidWorktree covers WorktreePath error in callRestore.
func TestServe_RestoreInvalidWorktree(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "nonexistent-wt", "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_restore","arguments":{"ref":"abc123","path":"a.go"}}}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
	require.Equal(t, -32000, resps[0].Error.Code)
}

// TestServe_RestoreBadRef covers Restore error.
func TestServe_RestoreBadRef(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	srv := New(v, repoID, wt.ID, "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_restore","arguments":{"ref":"badref","path":"a.go"}}}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
	require.Equal(t, -32000, resps[0].Error.Code)
}

// TestServe_MergeConflictFree covers the conflicts=nil to empty-slice path.
func TestServe_MergeConflictFree(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	srv := New(v, repoID, wt.ID, "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_merge","arguments":{}}}`)
	require.Len(t, resps, 1)
	require.Nil(t, resps[0].Error)
}

// TestServe_MergeInvalidArgs covers decodeArgs error in callMerge.
func TestServe_MergeInvalidArgs(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_merge","arguments":"bad"}}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
}

// TestServe_ToolsCallNoArguments covers len(params) == 0 branch.
func TestServe_ToolsCallNoArguments(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_commit"}}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
}

// TestServe_WorktreeTip_InvalidWorktree covers empty tip return.
func TestServe_WorktreeTip_InvalidWorktree(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "nonexistent-wt", "agent")
	tip := srv.worktreeTip()
	if tip != "" {
		t.Fatalf("expected empty tip for invalid worktree, got %q", tip)
	}
}

// TestMarshalText_BadValue covers json.Marshal error in marshalText.
func TestMarshalText_BadValue(t *testing.T) {
	type badJSON struct{ C chan int }
	_, rpcErr := marshalText(badJSON{C: make(chan int)})
	require.NotNil(t, rpcErr)
	require.Equal(t, -32603, rpcErr.Code)
}

// TestWriteResponse_MarshalError covers json.Marshal error in writeResponse.
func TestWriteResponse_MarshalError(t *testing.T) {
	var buf bytes.Buffer
	err := writeResponse(&buf, json.RawMessage(`1`), map[string]any{"bad": make(chan int)}, nil)
	require.Error(t, err)
}

// TestWriteResponse_WriteError covers w.Write error in writeResponse.
func TestWriteResponse_WriteError(t *testing.T) {
	err := writeResponse(failWriter{}, json.RawMessage(`1`), "ok", nil)
	require.Error(t, err)
}

// TestServe_WriteError verifies write failure propagates through Serve.
func TestServe_WriteError(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	line := `{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n"
	in := bytes.NewBufferString(line)
	err := srv.Serve(context.Background(), in, failWriter{})
	require.Error(t, err)
}

type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) { return 0, errWriteFail }

var errWriteFail = testError("write failed")

type testError string

func (e testError) Error() string { return string(e) }
