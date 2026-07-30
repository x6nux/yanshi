package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// newTestVCS builds an in-memory VCS for MCP tests (mirrors the vcs package's
// own test helper). The worktree dir is a per-test temp dir.
func newTestVCS(t *testing.T) *vcs.VCS {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return vcs.New(s, t.TempDir())
}

// rawResp is a test-only response view with Result as RawMessage for easy
// re-decoding of the content payload.
type rawResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *respError      `json:"error"`
}

// runServe writes the given request lines to an in-process buffer, drives Serve
// to EOF, and returns the decoded response lines. The input buffer's read side
// yields EOF once all lines are consumed, so Serve returns naturally.
func runServe(t *testing.T, srv *Server, reqLines ...string) []rawResp {
	t.Helper()
	in := &bytes.Buffer{}
	out := &bytes.Buffer{}
	for _, l := range reqLines {
		fmt.Fprintf(in, "%s\n", l)
	}
	require.NoError(t, srv.Serve(context.Background(), in, out))

	var resps []rawResp
	for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r rawResp
		require.NoErrorf(t, json.Unmarshal(line, &r), "decode response line: %s", line)
		resps = append(resps, r)
	}
	return resps
}

// contentText extracts the single text block from a tools/call result.
func contentText(t *testing.T, result json.RawMessage) string {
	t.Helper()
	var cr callResult
	require.NoError(t, json.Unmarshal(result, &cr))
	require.Len(t, cr.Content, 1, "expected exactly one content block")
	assert.Equal(t, "text", cr.Content[0].Type)
	return cr.Content[0].Text
}

func TestServe_Initialize(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	require.Len(t, resps, 1)
	r := resps[0]
	assert.Equal(t, "2.0", r.JSONRPC)
	assert.JSONEq(t, `1`, string(r.ID))
	require.Nil(t, r.Error)

	var init struct {
		ProtocolVersion string            `json:"protocolVersion"`
		Capabilities    map[string]any    `json:"capabilities"`
		ServerInfo      map[string]string `json:"serverInfo"`
	}
	require.NoError(t, json.Unmarshal(r.Result, &init))
	assert.Equal(t, protocolVersion, init.ProtocolVersion)
	assert.Contains(t, init.Capabilities, "tools", "capabilities must advertise tools")
	assert.Equal(t, "yanshi-vcs", init.ServerInfo["name"])
	assert.NotEmpty(t, init.ServerInfo["version"])
}

func TestServe_ToolsList(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	require.Len(t, resps, 1)
	require.Nil(t, resps[0].Error)

	var list struct {
		Tools []toolDescriptor `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(resps[0].Result, &list))
	require.Len(t, list.Tools, 5)

	names := map[string]bool{}
	for _, td := range list.Tools {
		names[td.Name] = true
		require.NotEmpty(t, td.Description)
		assert.Equal(t, "object", td.InputSchema.Type, "%s inputSchema must be an object", td.Name)
	}
	for _, want := range []string{"vcs_commit", "vcs_log", "vcs_diff", "vcs_restore", "vcs_merge"} {
		assert.True(t, names[want], "tool %q should be advertised", want)
	}

	// vcs_commit and vcs_restore declare required fields.
	var commitDesc, restoreDesc *toolDescriptor
	for i := range list.Tools {
		switch list.Tools[i].Name {
		case "vcs_commit":
			commitDesc = &list.Tools[i]
		case "vcs_restore":
			restoreDesc = &list.Tools[i]
		}
	}
	require.Contains(t, commitDesc.InputSchema.Required, "message")
	require.Contains(t, restoreDesc.InputSchema.Required, "ref")
	require.Contains(t, restoreDesc.InputSchema.Required, "path")
}

func TestServe_NotificationNoResponse(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	// A notification (no id) followed by a real request. Only the request
	// produces a response line.
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":7,"method":"initialize"}`,
	)
	require.Len(t, resps, 1, "notification must not produce a response")
	assert.JSONEq(t, `7`, string(resps[0].ID))
}

func TestServe_VCSCommitCall(t *testing.T) {
	// Seed: repo with a.go, a worktree branched from main, one pending edit.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package main"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	require.NoError(t, v.RecordEditWorktree(wt.ID, "agent", filepath.Join(wt.Path, "a.go"), []byte("edited")))

	srv := New(v, repoID, wt.ID, "agent")
	resps := runServe(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_commit","arguments":{"message":"edit a"}}}`)
	require.Len(t, resps, 1)
	require.Nil(t, resps[0].Error, "commit should succeed")
	commitID := contentText(t, resps[0].Result)
	require.NotEmpty(t, commitID)

	// Verify via the VCS directly: the worktree log's newest commit matches.
	log, err := v.LogWorktree(wt.ID, 5)
	require.NoError(t, err)
	require.NotEmpty(t, log)
	assert.Equal(t, commitID, log[0].ID, "returned id must match the worktree's newest commit")
	assert.Equal(t, "edit a", log[0].Message)
	assert.Equal(t, "agent", log[0].Author)
}

func TestServe_VCSLogCall(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	require.NoError(t, v.RecordEditWorktree(wt.ID, "agent", filepath.Join(wt.Path, "a.go"), []byte("v2")))
	_, err = v.CommitWorktree(wt.ID, "agent", "first wt edit")
	require.NoError(t, err)

	srv := New(v, repoID, wt.ID, "agent")
	resps := runServe(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_log","arguments":{"limit":5}}}`)
	require.Len(t, resps, 1)
	require.Nil(t, resps[0].Error)
	text := contentText(t, resps[0].Result)
	var commits []vcs.Commit
	require.NoError(t, json.Unmarshal([]byte(text), &commits))
	require.GreaterOrEqual(t, len(commits), 1)
	assert.Equal(t, "first wt edit", commits[0].Message)
}

func TestServe_VCSDiffCall(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("a1"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	// Capture the branched tip (== base) before editing.
	tipBefore := ""
	if l, e := v.LogWorktree(wt.ID, 1); e == nil && len(l) > 0 {
		tipBefore = l[0].ID
	}
	require.NoError(t, v.RecordEditWorktree(wt.ID, "agent", filepath.Join(wt.Path, "a.go"), []byte("a2")))
	_, err = v.CommitWorktree(wt.ID, "agent", "edit a")
	require.NoError(t, err)

	srv := New(v, repoID, wt.ID, "agent")
	resps := runServe(t, srv, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_diff","arguments":{"ref_a":%q,"ref_b":%q}}}`,
		tipBefore, srv.worktreeTip()))
	require.Len(t, resps, 1)
	require.Nil(t, resps[0].Error)
	text := contentText(t, resps[0].Result)
	var diffs []vcs.FileDiff
	require.NoError(t, json.Unmarshal([]byte(text), &diffs))
	require.Len(t, diffs, 1)
	assert.Equal(t, "a.go", diffs[0].Path)
	assert.Equal(t, "modified", diffs[0].Op)
}

func TestServe_VCSRestoreCall(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("original"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	// Worktree's a.go is "original" (from main snapshot). Clobber it on disk,
	// then restore from the branched tip via the MCP tool.
	tip := ""
	if l, e := v.LogWorktree(wt.ID, 1); e == nil && len(l) > 0 {
		tip = l[0].ID
	}
	require.NotEmpty(t, tip)
	require.NoError(t, os.WriteFile(filepath.Join(wt.Path, "a.go"), []byte("clobbered"), 0o644))

	srv := New(v, repoID, wt.ID, "agent")
	resps := runServe(t, srv, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_restore","arguments":{"ref":%q,"path":"a.go"}}}`,
		tip))
	require.Len(t, resps, 1)
	require.Nil(t, resps[0].Error)
	assert.Equal(t, "restored a.go", contentText(t, resps[0].Result))

	got, err := os.ReadFile(filepath.Join(wt.Path, "a.go"))
	require.NoError(t, err)
	assert.Equal(t, "original", string(got), "restore must write the commit's blob into the worktree")
}

func TestServe_VCSMergeCall(t *testing.T) {
	// Two-sided conflict: the session worktree edits a.go, main also edits a.go.
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("a1"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	require.NoError(t, v.RecordEditWorktree(wt.ID, "agent", filepath.Join(wt.Path, "a.go"), []byte("from-worktree")))
	_, err = v.CommitWorktree(wt.ID, "agent", "wt edits a")
	require.NoError(t, err)

	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("from-main")))
	_, err = v.CommitMain(repoID, "o", "main edits a")
	require.NoError(t, err)

	srv := New(v, repoID, wt.ID, "agent")
	resps := runServe(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_merge","arguments":{"force":false}}}`)
	require.Len(t, resps, 1)
	require.Nil(t, resps[0].Error, "conflicts are a normal result, not a JSON-RPC error")

	var merge struct {
		Merged    string   `json:"merged"`
		Conflicts []string `json:"conflicts"`
	}
	require.NoError(t, json.Unmarshal([]byte(contentText(t, resps[0].Result)), &merge))
	assert.Empty(t, merge.Merged, "no merge commit on conflict without force")
	assert.Contains(t, merge.Conflicts, "a.go")
}

func TestServe_VCSMergeCall_ForceResolves(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("a1"), 0o644))
	v := newTestVCS(t)
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)
	require.NoError(t, v.RecordEditWorktree(wt.ID, "agent", filepath.Join(wt.Path, "a.go"), []byte("from-worktree")))
	_, err = v.CommitWorktree(wt.ID, "agent", "wt edits a")
	require.NoError(t, err)
	require.NoError(t, v.RecordEditMain(repoID, "o", filepath.Join(root, "a.go"), []byte("from-main")))
	_, err = v.CommitMain(repoID, "o", "main edits a")
	require.NoError(t, err)

	srv := New(v, repoID, wt.ID, "agent")
	resps := runServe(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"vcs_merge","arguments":{"force":true}}}`)
	require.Len(t, resps, 1)
	require.Nil(t, resps[0].Error)

	var merge struct {
		Merged    string   `json:"merged"`
		Conflicts []string `json:"conflicts"`
	}
	require.NoError(t, json.Unmarshal([]byte(contentText(t, resps[0].Result)), &merge))
	require.NotEmpty(t, merge.Merged, "force must produce a merge commit")
	assert.Contains(t, merge.Conflicts, "a.go", "conflicts still reported even when forced")
}

func TestServe_UnknownToolError(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bogus"}}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
	assert.Equal(t, -32602, resps[0].Error.Code)
	assert.JSONEq(t, `1`, string(resps[0].ID))
}

func TestServe_UnknownMethodError(t *testing.T) {
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv, `{"jsonrpc":"2.0","id":9,"method":"sesquipedalian/foo"}`)
	require.Len(t, resps, 1)
	require.NotNil(t, resps[0].Error)
	assert.Equal(t, -32601, resps[0].Error.Code)
}

func TestServe_MultipleRequestsInOrder(t *testing.T) {
	// Three pipelined requests produce three responses in order.
	srv := New(newTestVCS(t), "repo", "wt", "agent")
	resps := runServe(t, srv,
		`{"jsonrpc":"2.0","id":"a","method":"initialize"}`,
		`{"jsonrpc":"2.0","id":"b","method":"tools/list"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"c","method":"initialize"}`,
	)
	require.Len(t, resps, 3, "notification yields no response; the three requests yield three")
	assert.JSONEq(t, `"a"`, string(resps[0].ID))
	assert.JSONEq(t, `"b"`, string(resps[1].ID))
	assert.JSONEq(t, `"c"`, string(resps[2].ID))
}
