package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
)

// editTestCtx builds a context that allows fs_* tools full read/write under dir.
func editTestCtx(dir string) context.Context {
	return WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{dir + "/**"}, Read: []string{dir + "/**"}},
	})
}

// writeTestFile writes content to name under dir and returns the fs tools + ctx.
func writeTestFile(t *testing.T, dir, name, content string) (*FSTools, context.Context) {
	t.Helper()
	fs := NewFSTools(dir)
	ctx := editTestCtx(dir)
	_, err := runTool(ctx, fs.Write, toJSON(map[string]any{"path": name, "content": content}))
	require.NoError(t, err)
	return fs, ctx
}

// TestFS_EditLenient_TabVsSpace proves an old_string with spaces matches a file
// line indented with a tab (the most common model slip: wrong indent width).
func TestFS_EditLenient_TabVsSpace(t *testing.T) {
	dir := t.TempDir()
	// File uses a TAB indent; the model's old_string uses 4 spaces.
	fs, ctx := writeTestFile(t, dir, "g.go", "func main() {\n\tfoo()\n\tbar()\n}\n")

	out, err := runTool(ctx, fs.Edit, toJSON(map[string]any{
		"path":       "g.go",
		"old_string": "func main() {\n    foo()\n    bar()\n}",
		"new_string": "func main() {\n    foo()\n    baz()\n}",
	}))
	require.NoError(t, err)
	assert.Contains(t, out, "replacements")

	got, err := runTool(ctx, fs.Read, toJSON(map[string]any{"path": "g.go"}))
	require.NoError(t, err)
	assert.Contains(t, got, "baz()")
	assert.NotContains(t, got, "bar()")
}

// TestFS_EditLenient_TrailingWhitespace proves trailing whitespace on the file's
// lines doesn't block a match when old_string omits it.
func TestFS_EditLenient_TrailingWhitespace(t *testing.T) {
	dir := t.TempDir()
	// File lines have trailing spaces; old_string does not.
	fs, ctx := writeTestFile(t, dir, "t.txt", "line one   \nline two   \n")

	out, err := runTool(ctx, fs.Edit, toJSON(map[string]any{
		"path":       "t.txt",
		"old_string": "line one\nline two",
		"new_string": "first\nsecond",
	}))
	require.NoError(t, err)
	assert.Contains(t, out, "replacements")

	got, err := runTool(ctx, fs.Read, toJSON(map[string]any{"path": "t.txt"}))
	require.NoError(t, err)
	assert.Contains(t, got, "first")
	assert.Contains(t, got, "second")
}

// TestFS_EditLenient_CRLF proves \r\n in the file matches a \n-only old_string.
func TestFS_EditLenient_CRLF(t *testing.T) {
	dir := t.TempDir()
	fs, ctx := writeTestFile(t, dir, "c.txt", "alpha\r\nbeta\r\n")

	out, err := runTool(ctx, fs.Edit, toJSON(map[string]any{
		"path":       "c.txt",
		"old_string": "alpha\nbeta",
		"new_string": "ALPHA\nBETA",
	}))
	require.NoError(t, err)
	assert.Contains(t, out, "replacements")

	got, err := runTool(ctx, fs.Read, toJSON(map[string]any{"path": "c.txt"}))
	require.NoError(t, err)
	assert.Contains(t, got, "ALPHA")
	assert.Contains(t, got, "BETA")
}

// TestFS_EditLenient_IndentWidthMismatch proves the content-only fallback
// catches an indent mismatch that fixed-width tab expansion can't (model wrote
// 2 spaces, file has a tab): the bare-content comparison still lands the edit.
func TestFS_EditLenient_IndentWidthMismatch(t *testing.T) {
	dir := t.TempDir()
	// File: tab indent. Model: 2-space indent (tab=4 expansion won't reconcile).
	fs, ctx := writeTestFile(t, dir, "w.go", "package x\n\nfunc f() {\n\treturn 1\n}\n")

	out, err := runTool(ctx, fs.Edit, toJSON(map[string]any{
		"path":       "w.go",
		"old_string": "func f() {\n  return 1\n}",
		"new_string": "func f() {\n  return 2\n}",
	}))
	require.NoError(t, err)
	assert.Contains(t, out, "replacements")

	got, err := runTool(ctx, fs.Read, toJSON(map[string]any{"path": "w.go"}))
	require.NoError(t, err)
	assert.Contains(t, got, "return 2")
}

// TestFS_EditLenient_AmbiguousRejects proves two lenient matches without
// replace_all surface the "multiple matches" error (lenient doesn't widen a
// unique-match contract into a silent multi-replace).
func TestFS_EditLenient_AmbiguousRejects(t *testing.T) {
	dir := t.TempDir()
	// Two identical lines, both tab-indented; old_string uses spaces → both
	// match leniently.
	fs, ctx := writeTestFile(t, dir, "a.txt", "\tfoo\n\tfoo\n")

	out, err := runTool(ctx, fs.Edit, toJSON(map[string]any{
		"path":       "a.txt",
		"old_string": "    foo",
		"new_string": "    bar",
	}))
	require.NoError(t, err, "ambiguity surfaces as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "multiple")

	// replace_all resolves both.
	out, err = runTool(ctx, fs.Edit, toJSON(map[string]any{
		"path":        "a.txt",
		"old_string":  "    foo",
		"new_string":  "    bar",
		"replace_all": true,
	}))
	require.NoError(t, err)
	assert.Contains(t, out, "replacements")
	got, err := runTool(ctx, fs.Read, toJSON(map[string]any{"path": "a.txt"}))
	require.NoError(t, err)
	assert.NotContains(t, got, "foo")
}

// TestFS_EditNotFound_ShowsActualContent proves the not-found error includes the
// real file content so the model can self-correct instead of looping.
func TestFS_EditNotFound_ShowsActualContent(t *testing.T) {
	dir := t.TempDir()
	fs, ctx := writeTestFile(t, dir, "n.txt", "the real content\nis here\n")

	out, err := runTool(ctx, fs.Edit, toJSON(map[string]any{
		"path":       "n.txt",
		"old_string": "nothing like this",
		"new_string": "x",
	}))
	require.NoError(t, err)
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "not found")
	assert.Contains(t, out, "the real content", "error must surface the actual file content")
	assert.Contains(t, out, "fs_read", "error must tell the model to re-read")
}

// --- unit tests for the matching helpers ---

func TestLenientFind_TabVsSpace(t *testing.T) {
	data := []byte("func main() {\n\tfoo()\n\tbar()\n}\n")
	old := "func main() {\n    foo()\n    bar()\n}"
	ranges := lenientFind(data, old)
	require.Len(t, ranges, 1)
	assert.Equal(t, "func main() {\n\tfoo()\n\tbar()\n}", string(data[ranges[0].Start:ranges[0].End]))
}

func TestLenientFind_NoMatch(t *testing.T) {
	data := []byte("alpha\nbeta\n")
	assert.Nil(t, lenientFind(data, "completely different"))
}

func TestTerminatorLen(t *testing.T) {
	assert.Equal(t, 2, terminatorLen("x\r\ny", 1)) // \r\n
	assert.Equal(t, 1, terminatorLen("x\ny", 1))   // \n
	assert.Equal(t, 1, terminatorLen("x\ry", 1))   // lone \r
	assert.Equal(t, 0, terminatorLen("xy", 2))     // EOF, no terminator
}
