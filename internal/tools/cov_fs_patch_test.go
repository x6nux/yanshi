package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// applyOneEdit — test all branches: empty context, exact match, multiple
// matches, lenient match, not found.
// ---------------------------------------------------------------------------

func TestApplyOneEdit_EmptyContext(t *testing.T) {
	_, err := applyOneEdit("hello world", "", "new")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty context")
}

func TestApplyOneEdit_ExactMatch(t *testing.T) {
	result, err := applyOneEdit("hello world", "world", "there")
	require.NoError(t, err)
	assert.Equal(t, "hello there", result)
}

func TestApplyOneEdit_MultipleExactMatches(t *testing.T) {
	_, err := applyOneEdit("a a a", "a", "b")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "matches 3 sites")
}

func TestApplyOneEdit_NotFound(t *testing.T) {
	_, err := applyOneEdit("hello world", "xyz", "abc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "context not found")
}

func TestApplyOneEdit_LenientMatch(t *testing.T) {
	// Lenient match: source has trailing whitespace that old pattern lacks.
	// The exact substring match fails (the src has extra spaces before \n),
	// but normTabTrailing strips trailing whitespace and the lines match.
	// The replacement covers the full original byte range (including trailing ws).
	src := "hello world   \nsecond line"
	old := "hello world\nsecond line"
	result, err := applyOneEdit(src, old, "hi world\nsecond line")
	require.NoError(t, err)
	assert.Equal(t, "hi world\nsecond line", result)
}

func TestApplyOneEdit_LenientMatchTrailingWhitespace(t *testing.T) {
	// Source has tab indentation, old has 4-space indentation.
	// normTabTrailing replaces tab → 4 spaces so they match.
	src := "    indented code\n"
	old := "\tindented code\n"
	result, err := applyOneEdit(src, old, "    replaced code\n")
	require.NoError(t, err)
	assert.Equal(t, "    replaced code\n", result)
}

// ---------------------------------------------------------------------------
// renderPatchDiff — add, delete, update paths
// ---------------------------------------------------------------------------

func TestRenderPatchDiff_Add(t *testing.T) {
	staged := []stagedChange{
		{rel: "new.go", origExisted: false, orig: nil, final: []byte("line1\nline2\n")},
	}
	out := renderPatchDiff(staged)
	assert.Contains(t, out, "--- /dev/null")
	assert.Contains(t, out, "+++ new.go")
	assert.Contains(t, out, "+line1")
	assert.Contains(t, out, "+line2")
}

func TestRenderPatchDiff_Delete(t *testing.T) {
	staged := []stagedChange{
		{rel: "old.go", origExisted: true, orig: []byte("content\n"), final: nil},
	}
	out := renderPatchDiff(staged)
	assert.Contains(t, out, "--- old.go")
	assert.Contains(t, out, "+++ /dev/null")
	assert.Contains(t, out, "-content")
}

func TestRenderPatchDiff_Update(t *testing.T) {
	staged := []stagedChange{
		{rel: "mod.go", origExisted: true, orig: []byte("old line\n"), final: []byte("new line\n")},
	}
	out := renderPatchDiff(staged)
	assert.Contains(t, out, "--- mod.go")
	assert.Contains(t, out, "+++ mod.go")
}

func TestRenderPatchDiff_MultipleHunks(t *testing.T) {
	staged := []stagedChange{
		{rel: "a.go", origExisted: false, final: []byte("x\n")},
		{rel: "b.go", origExisted: true, orig: []byte("y\n"), final: nil},
		{rel: "c.go", origExisted: true, orig: []byte("1\n"), final: []byte("2\n")},
	}
	out := renderPatchDiff(staged)
	assert.Contains(t, out, "a.go")
	assert.Contains(t, out, "b.go")
	assert.Contains(t, out, "c.go")
}

func TestRenderPatchDiff_Empty(t *testing.T) {
	out := renderPatchDiff(nil)
	assert.Equal(t, "", out)
}

// ---------------------------------------------------------------------------
// countLines — edge cases
// ---------------------------------------------------------------------------

func TestCountLines_Empty(t *testing.T) {
	assert.Equal(t, 0, countLines([]byte("")))
}

func TestCountLines_NoTrailingNewline(t *testing.T) {
	assert.Equal(t, 1, countLines([]byte("hello")))
}

func TestCountLines_WithTrailingNewline(t *testing.T) {
	assert.Equal(t, 2, countLines([]byte("line1\nline2\n")))
}

// ---------------------------------------------------------------------------
// preparePatch — test the in-memory validation pipeline
// ---------------------------------------------------------------------------

func TestPreparePatch_AddNewFile(t *testing.T) {
	tmp := t.TempDir()
	ft := NewFSTools(tmp)
	ops := []patchOp{{kind: opAdd, path: "new.txt", addBody: "content"}}
	staged, err := ft.preparePatch(context.Background(), ops)
	require.NoError(t, err)
	require.Len(t, staged, 1)
	assert.Equal(t, "new.txt", staged[0].rel)
	assert.False(t, staged[0].origExisted)
	assert.Equal(t, "content", string(staged[0].final))
}

func TestPreparePatch_AddExistingFails(t *testing.T) {
	tmp := t.TempDir()
	existing := filepath.Join(tmp, "exists.txt")
	require.NoError(t, os.WriteFile(existing, []byte("old"), 0o644))
	ft := NewFSTools(tmp)
	ops := []patchOp{{kind: opAdd, path: "exists.txt", addBody: "new"}}
	_, err := ft.preparePatch(context.Background(), ops)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestPreparePatch_UpdateNonExistingFails(t *testing.T) {
	tmp := t.TempDir()
	ft := NewFSTools(tmp)
	ops := []patchOp{{kind: opUpdate, path: "nope.txt", updOld: "a", updNew: "b"}}
	_, err := ft.preparePatch(context.Background(), ops)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

func TestPreparePatch_DeleteNonExistingFails(t *testing.T) {
	tmp := t.TempDir()
	ft := NewFSTools(tmp)
	ops := []patchOp{{kind: opDelete, path: "nope.txt"}}
	_, err := ft.preparePatch(context.Background(), ops)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

func TestPreparePatch_MoveNonExistingFails(t *testing.T) {
	tmp := t.TempDir()
	ft := NewFSTools(tmp)
	ops := []patchOp{{kind: opMove, from: "src.txt", path: "dst.txt"}}
	_, err := ft.preparePatch(context.Background(), ops)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
}

func TestPreparePatch_MoveToExistingFails(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src.txt")
	dst := filepath.Join(tmp, "dst.txt")
	require.NoError(t, os.WriteFile(src, []byte("data"), 0o644))
	require.NoError(t, os.WriteFile(dst, []byte("existing"), 0o644))
	ft := NewFSTools(tmp)
	ops := []patchOp{{kind: opMove, from: "src.txt", path: "dst.txt"}}
	_, err := ft.preparePatch(context.Background(), ops)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestPreparePatch_SuccessfulMove(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src.txt")
	require.NoError(t, os.WriteFile(src, []byte("data"), 0o644))
	ft := NewFSTools(tmp)
	ops := []patchOp{{kind: opMove, from: "src.txt", path: "dst.txt"}}
	staged, err := ft.preparePatch(context.Background(), ops)
	require.NoError(t, err)
	require.Len(t, staged, 2) // delete src + add dst
}

func TestPreparePatch_UpdateSuccess(t *testing.T) {
	tmp := t.TempDir()
	existing := filepath.Join(tmp, "edit.txt")
	require.NoError(t, os.WriteFile(existing, []byte("hello world"), 0o644))
	ft := NewFSTools(tmp)
	ops := []patchOp{{kind: opUpdate, path: "edit.txt", updOld: "world", updNew: "there"}}
	staged, err := ft.preparePatch(context.Background(), ops)
	require.NoError(t, err)
	require.Len(t, staged, 1)
	assert.Equal(t, "hello there", string(staged[0].final))
}

func TestPreparePatch_DeleteSuccess(t *testing.T) {
	tmp := t.TempDir()
	existing := filepath.Join(tmp, "del.txt")
	require.NoError(t, os.WriteFile(existing, []byte("content"), 0o644))
	ft := NewFSTools(tmp)
	ops := []patchOp{{kind: opDelete, path: "del.txt"}}
	staged, err := ft.preparePatch(context.Background(), ops)
	require.NoError(t, err)
	require.Len(t, staged, 1)
	assert.Nil(t, staged[0].final)
}

func TestPreparePatch_NoOpUpdate(t *testing.T) {
	tmp := t.TempDir()
	existing := filepath.Join(tmp, "same.txt")
	require.NoError(t, os.WriteFile(existing, []byte("abc"), 0o644))
	ft := NewFSTools(tmp)
	// Update that results in no net change: old→new, then new→old
	ops := []patchOp{
		{kind: opUpdate, path: "same.txt", updOld: "abc", updNew: "xyz"},
		{kind: opUpdate, path: "same.txt", updOld: "xyz", updNew: "abc"},
	}
	staged, err := ft.preparePatch(context.Background(), ops)
	require.NoError(t, err)
	assert.Len(t, staged, 0) // no net change
}

func TestPreparePatch_CreateThenDelete(t *testing.T) {
	tmp := t.TempDir()
	ft := NewFSTools(tmp)
	ops := []patchOp{
		{kind: opAdd, path: "temp.txt", addBody: "data"},
		{kind: opDelete, path: "temp.txt"},
	}
	staged, err := ft.preparePatch(context.Background(), ops)
	require.NoError(t, err)
	assert.Len(t, staged, 0) // created then deleted = no net change
}

func TestPreparePatch_UnknownKind(t *testing.T) {
	tmp := t.TempDir()
	ft := NewFSTools(tmp)
	ops := []patchOp{{kind: 99, path: "x.txt"}}
	_, err := ft.preparePatch(context.Background(), ops)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown kind")
}

// ---------------------------------------------------------------------------
// stagedRelPaths / opWritePaths
// ---------------------------------------------------------------------------

func TestStagedRelPaths(t *testing.T) {
	staged := []stagedChange{
		{rel: "a.go"},
		{rel: "b.go"},
	}
	paths := stagedRelPaths(staged)
	assert.Equal(t, []string{"a.go", "b.go"}, paths)
}

func TestOpWritePaths(t *testing.T) {
	ops := []patchOp{
		{kind: opAdd, path: "a.go"},
		{kind: opMove, from: "b.go", path: "c.go"},
		{kind: opDelete, path: "d.go"},
	}
	paths := opWritePaths(ops)
	assert.Contains(t, paths, "a.go")
	assert.Contains(t, paths, "b.go")
	assert.Contains(t, paths, "c.go")
	assert.Contains(t, paths, "d.go")
}
