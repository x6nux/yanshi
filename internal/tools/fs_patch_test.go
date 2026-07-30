package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/lsp"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// patchCtx builds a context that allows fs_* + apply_patch full read/write under
// dir. Note apply_patch is the tool NAME, so a profile granting only "fs_*" would
// NOT match it — it must be listed explicitly (or "*").
func patchCtx(dir string) context.Context {
	return WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*", "apply_patch"}},
		FS:    guard.FSPerm{Write: []string{dir + "/**"}, Read: []string{dir + "/**"}},
	})
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

func readBack(t *testing.T, fs *FSTools, ctx context.Context, name string) string {
	t.Helper()
	out, err := runTool(ctx, fs.Read, toJSON(map[string]any{"path": name}))
	require.NoError(t, err)
	return out
}

func TestApplyPatch_AddUpdateDeleteMove(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := patchCtx(dir)
	writeFile(t, dir, "upd.go", "line1\nfunc A() {}\nline3\n")
	writeFile(t, dir, "del.go", "bye\n")
	writeFile(t, dir, "mvsrc.go", "moving\n")

	patch := "*** Begin Patch\n" +
		"*** Add File: new.go\n" +
		"package new\n" +
		"*** Update File: upd.go\n" +
		" line1\n" +
		"-func A() {}\n" +
		"+func A() { return }\n" +
		" line3\n" +
		"*** Delete File: del.go\n" +
		"*** Move File: mvsrc.go\n" +
		"*** To: mvdst.go\n" +
		"*** End Patch"

	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err)
	assert.Contains(t, out, "applied")

	// add
	assert.Contains(t, readBack(t, fs, ctx, "new.go"), "package new")
	// update
	assert.Contains(t, readBack(t, fs, ctx, "upd.go"), "func A() { return }")
	assert.NotContains(t, readBack(t, fs, ctx, "upd.go"), "func A() {}")
	// delete
	_, derr := os.Stat(filepath.Join(dir, "del.go"))
	assert.True(t, os.IsNotExist(derr), "del.go must be removed")
	// move: src gone, dst has content
	_, serr := os.Stat(filepath.Join(dir, "mvsrc.go"))
	assert.True(t, os.IsNotExist(serr), "mvsrc.go must be removed")
	assert.Contains(t, readBack(t, fs, ctx, "mvdst.go"), "moving")
}

func TestApplyPatch_DryRunNoWrite(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := patchCtx(dir)

	patch := "*** Begin Patch\n*** Add File: ghost.txt\nboo\n*** End Patch"
	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch, "dry_run": true}))
	require.NoError(t, err)
	assert.Contains(t, out, "dry_run")
	assert.Contains(t, out, "+++ ghost.txt", "dry-run must return a unified-style diff")
	// The file must NOT exist on disk (dry-run never writes).
	_, ferr := os.Stat(filepath.Join(dir, "ghost.txt"))
	assert.True(t, os.IsNotExist(ferr), "dry-run must not create files")
}

func TestApplyPatch_AtomicRollbackOnBadOp(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := patchCtx(dir)
	writeFile(t, dir, "a.go", "original\n")
	// Good update on a.go, then a delete of a file that does not exist. The
	// missing-file op must fail prepare and NOTHING is written — a.go is unchanged.
	patch := "*** Begin Patch\n" +
		"*** Update File: a.go\n" +
		"-original\n" +
		"+changed\n" +
		"*** Delete File: missing.go\n" +
		"*** End Patch"
	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err, "validation failure surfaces as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "does not exist")

	data, rerr := os.ReadFile(filepath.Join(dir, "a.go"))
	require.NoError(t, rerr)
	assert.Equal(t, "original\n", string(data), "a.go must be unchanged (atomic: no half-applied patch)")
}

func TestApplyPatch_PermissionDenied(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	// Profile grants write only under dir/allowed/**; targeting dir/secret.go
	// must be denied at the batch guard check — nothing is written.
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*", "apply_patch"}},
		FS:    guard.FSPerm{Write: []string{dir + "/allowed/**"}, Read: []string{dir + "/**"}},
	})
	patch := "*** Begin Patch\n*** Add File: secret.go\ns3cr3t\n*** End Patch"
	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err)
	assert.Contains(t, out, "permission denied")
	_, ferr := os.Stat(filepath.Join(dir, "secret.go"))
	assert.True(t, os.IsNotExist(ferr), "denied patch must not write")
}

// TestApplyPatch_TracksToVCS proves successful edits flow into autoVCS: add /
// update / move-destination are tracked as content (blob present), and delete /
// move-source are tracked via the new trackDelete path (op="deleted"). The op
// value itself is asserted in the vcs-package test (Task 1); here we assert the
// rows exist in the changeset (Uncommitted returns the path keys, including
// deleted ones whose blob_hash is empty).
func TestApplyPatch_TracksToVCS(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "upd.go", "line1\nfunc A() {}\nline3\n")
	writeFile(t, root, "del.go", "bye\n")
	writeFile(t, root, "mvsrc.go", "moving\n")

	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	v := vcs.New(st, t.TempDir())
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	fs := NewFSTools(root)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*", "apply_patch"}},
		FS:    guard.FSPerm{Write: []string{root + "/**"}, Read: []string{root + "/**"}},
	})
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"})

	patch := "*** Begin Patch\n" +
		"*** Add File: new.go\n" +
		"package new\n" +
		"*** Update File: upd.go\n" +
		" line1\n" +
		"-func A() {}\n" +
		"+func A() { return }\n" +
		" line3\n" +
		"*** Delete File: del.go\n" +
		"*** Move File: mvsrc.go\n" +
		"*** To: mvdst.go\n" +
		"*** End Patch"
	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err)
	assert.Contains(t, out, "applied")

	pending := v.Uncommitted("main", repoID)
	// content-tracked (add / update / move destination)
	for _, p := range []string{"new.go", "upd.go", "mvdst.go"} {
		assert.Contains(t, pending, p, "%s must be tracked", p)
	}
	// delete-tracked (Uncommitted returns deleted paths with empty blob_hash;
	// their key presence proves trackDelete fired).
	for _, p := range []string{"del.go", "mvsrc.go"} {
		assert.Contains(t, pending, p, "%s must be tracked as deleted", p)
	}
}

func TestApplyPatch_EmptyPatchIsError(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := patchCtx(dir)
	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": "*** Begin Patch\n*** End Patch"}))
	require.NoError(t, err)
	assert.Contains(t, out, "✗")
}

// TestApplyPatch_CommitRollbackRestoresDisk covers the COMMIT half of atomicity,
// which TestApplyPatch_AtomicRollbackOnBadOp does not reach: that test fails
// during prepare (an op references a missing file), so nothing is ever written.
// Here the whole batch validates in memory; the first two staged changes are
// written to disk; a THIRD staged change then hits a real I/O error mid-commit,
// and the already-written changes must be rolled back so the tree ends exactly as
// it began — the rollback path in commitPatch (fs_patch.go:259) that was previously
// only argued by code review.
//
// Fault injection is OS-level, with no test seam (per "Fake preferred over mock"):
// `blocker` is a regular FILE and the third op adds a file UNDER it
// (blocker/x.go). applyStaged calls os.MkdirAll(filepath.Dir(abs)); when that dir
// component is a file, MkdirAll returns ENOTDIR (Go stdlib, cross-platform).
// prepare never calls MkdirAll, so it stages the change happily; only commit walks
// into the wall — exactly the mid-commit failure path under test.
//
// The first two ops exercise both rollback branches: `c.go` is newly created
// (rollback must Remove it) and `e.go` is a pre-existing updated file (rollback
// must restore its original bytes).
func TestApplyPatch_CommitRollbackRestoresDisk(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := patchCtx(dir)
	writeFile(t, dir, "e.go", "E\n")
	writeFile(t, dir, "blocker", "BLOCKER\n") // a FILE; MkdirAll under it fails

	patch := "*** Begin Patch\n" +
		"*** Add File: c.go\n" +
		"C\n" +
		"*** Update File: e.go\n" +
		"-E\n" +
		"+E2\n" +
		"*** Add File: blocker/x.go\n" +
		"X\n" +
		"*** End Patch"

	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err, "commit I/O failure surfaces as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "rolled back", "commit-time rollback must be surfaced")

	// c.go was created then rolled back → gone.
	_, cerr := os.Stat(filepath.Join(dir, "c.go"))
	assert.True(t, os.IsNotExist(cerr), "newly-created c.go must be removed on rollback")

	// e.go was updated then restored → original bytes, not E2.
	data, rerr := os.ReadFile(filepath.Join(dir, "e.go"))
	require.NoError(t, rerr)
	assert.Equal(t, "E\n", string(data), "updated e.go must be restored to original content")

	// blocker untouched; blocker/x.go never landed.
	bdata, _ := os.ReadFile(filepath.Join(dir, "blocker"))
	assert.Equal(t, "BLOCKER\n", string(bdata))
	_, xerr := os.Stat(filepath.Join(dir, "blocker", "x.go"))
	// Stat'ing a path under a file yields NotExist on Windows but ENOTDIR on
	// Linux — either way the file is absent, so any stat error (not only
	// IsNotExist) proves the blocked write did not land.
	assert.True(t, xerr != nil, "the blocked write must not have landed")
}

// TestApplyPatch_WithLSP_AppendsDiagnosticsField 验证 patch 写盘后,每个写过的
// .go 文件的诊断被汇总进 JSON "diagnostics" 字段(seam 在 runPatch return 前)。
func TestApplyPatch_WithLSP_AppendsDiagnosticsField(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := patchCtxWithLSP(dir, &fakeLSPManager{diags: []lsp.Diagnostic{
		{Line: 1, Column: 1, Severity: lsp.SeverityError, Message: "bad", Source: "gopls"},
	}})

	writeFile(t, dir, "upd.go", "old\n")
	patch := "*** Begin Patch\n" +
		"*** Update File: upd.go\n" +
		"-old\n" +
		"+new\n" +
		"*** End Patch"

	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err, "patch 执行失败, out=%s", out)

	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), "JSON 解析失败, out=%q", out)
	d, present := m["diagnostics"]
	require.True(t, present, "patch 写盘后应有 diagnostics 字段,out=%s", out)
	assert.Contains(t, fmt.Sprint(d), "bad", "diagnostics 应含 fake 诊断")
}

// TestApplyPatch_NoLSP_NoDiagnosticsField 验证无 Manager 时 patch 结果无该字段。
func TestApplyPatch_NoLSP_NoDiagnosticsField(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := patchCtx(dir) // 无 WithLSP
	writeFile(t, dir, "upd.go", "old\n")
	patch := "*** Begin Patch\n*** Update File: upd.go\n-old\n+new\n*** End Patch"
	out, err := runTool(ctx, fs.Patch, toJSON(map[string]any{"patch": patch}))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	_, present := m["diagnostics"]
	assert.False(t, present, "无 Manager 时不应有 diagnostics 字段,out=%s", out)
}

// patchCtxWithLSP 是 patchCtx + WithLSP 的组合 helper(测试用)。
func patchCtxWithLSP(dir string, mgr LSPManager) context.Context {
	return WithLSP(patchCtx(dir), mgr)
}
