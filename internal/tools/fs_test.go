package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/lsp"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

func TestFS_Read(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "note.txt")
	require.NoError(t, os.WriteFile(p, []byte("hello\nworld\n"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir + "/**"}},
	})

	out, err := runTool(ctx, fs.Read, `{"path":"note.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "hello")
	assert.Contains(t, out, "world")
}

func TestFS_ReadDeniedOutsideReadList(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "secret.txt")
	require.NoError(t, os.WriteFile(p, []byte("nope"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{"/nonexistent/**"}}, // nothing allowed
	})
	out, err := runTool(ctx, fs.Read, `{"path":"secret.txt"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

func TestFS_ReadDeniedNoProfile(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	out, err := runTool(context.Background(), fs.Read, `{"path":"x"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

func TestFS_ReadWindowing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lines.txt")
	require.NoError(t, os.WriteFile(p, []byte("line1\nline2\nline3\nline4\nline5\n"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir + "/**"}},
	})

	t.Run("offset within range starts at requested line", func(t *testing.T) {
		out, err := runTool(ctx, fs.Read, `{"path":"lines.txt","offset":2}`)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(out, "2\tline2"), "got %q", out)
		assert.NotContains(t, out, "1\tline1")
	})

	t.Run("end only caps line range", func(t *testing.T) {
		out, err := runTool(ctx, fs.Read, `{"path":"lines.txt","end":2}`)
		require.NoError(t, err)
		assert.Equal(t, "1\tline1\n2\tline2", out)
	})

	t.Run("offset and end both respected", func(t *testing.T) {
		out, err := runTool(ctx, fs.Read, `{"path":"lines.txt","offset":2,"end":3}`)
		require.NoError(t, err)
		assert.Equal(t, "2\tline2\n3\tline3", out)
	})

	t.Run("offset beyond EOF returns empty without panic", func(t *testing.T) {
		out, err := runTool(ctx, fs.Read, `{"path":"lines.txt","offset":1000}`)
		require.NoError(t, err)
		assert.Equal(t, "", out)
	})

	t.Run("end less than offset errors", func(t *testing.T) {
		out, err := runTool(ctx, fs.Read, `{"path":"lines.txt","offset":3,"end":2}`)
		require.NoError(t, err)
		assert.Contains(t, out, "✗")
		assert.Contains(t, out, "end")
	})
}

func TestFS_Read_OversizeWindowErrors(t *testing.T) {
	dir := t.TempDir()
	// Build a file well over SpillThreshold (64 KiB): ~200 lines of ~697 B.
	var b strings.Builder
	for i := 0; i < 200; i++ {
		b.WriteString("L" + strings.Repeat("x", 695) + "\n")
	}
	content := b.String()
	require.Greater(t, len(content), SpillThreshold)

	p := filepath.Join(dir, "big.txt")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir + "/**"}},
	})

	// No narrow window → whole file → oversized → errorResult, NOT spilled.
	out, err := runTool(ctx, fs.Read, `{"path":"big.txt"}`)
	require.NoError(t, err, "oversize must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "exceeds")
	assert.Contains(t, out, "narrow offset/end")
	// Crucially: fs_read never spills to a temp file.
	_, derr := os.ReadDir(filepath.Join(dir, ".yanshi", "tmp", "spillover"))
	assert.True(t, os.IsNotExist(derr), "fs_read must not create a spill dir")

	// A narrow window on the same big file succeeds and returns just those lines.
	out, err = runTool(ctx, fs.Read, `{"path":"big.txt","offset":1,"end":3}`)
	require.NoError(t, err)
	assert.NotContains(t, out, "✗")
	assert.True(t, strings.HasPrefix(out, "1\tL"), "narrow window returns content, got %q", out)
}

func TestFS_WriteThenEdit(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{dir + "/**"}, Read: []string{dir + "/**"}},
	})

	out, err := runTool(ctx, fs.Write, `{"path":"a.txt","content":"alpha beta"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "wrote")

	out, err = runTool(ctx, fs.Edit, `{"path":"a.txt","old_string":"alpha","new_string":"ALPHA"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "replacements")

	got, err := runTool(ctx, fs.Read, `{"path":"a.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, got, "ALPHA beta")
}

func TestFS_EditRejectsNonUniqueWithoutReplaceAll(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{dir + "/**"}, Read: []string{dir + "/**"}},
	})
	_, err := runTool(ctx, fs.Write, `{"path":"d.txt","content":"x x x"}`)
	require.NoError(t, err)

	out, err := runTool(ctx, fs.Edit, `{"path":"d.txt","old_string":"x","new_string":"y"}`)
	require.NoError(t, err, "non-unique match must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "multiple")

	out, err = runTool(ctx, fs.Edit, `{"path":"d.txt","old_string":"x","new_string":"y","replace_all":true}`)
	require.NoError(t, err)
	assert.Contains(t, out, "replacements")
}

func TestFS_WriteDeniedByGuard(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir + "/**"}}, // write not granted
	})
	out, err := runTool(ctx, fs.Write, `{"path":"a.txt","content":"x"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

func TestFS_WriteCreatesSubdirs(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{dir + "/**"}, Read: []string{dir + "/**"}},
	})

	out, err := runTool(ctx, fs.Write, `{"path":"sub/deep/file.txt","content":"nested"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "wrote")

	got, err := runTool(ctx, fs.Read, `{"path":"sub/deep/file.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, got, "nested")
}

func TestFS_EditNotFound(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{dir + "/**"}, Read: []string{dir + "/**"}},
	})

	_, err := runTool(ctx, fs.Write, `{"path":"e.txt","content":"hello world"}`)
	require.NoError(t, err)

	out, err := runTool(ctx, fs.Edit, `{"path":"e.txt","old_string":"zzz","new_string":"yyy"}`)
	require.NoError(t, err, "not-found must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "not found")
}

func TestFS_EditIdempotentSameString(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{dir + "/**"}, Read: []string{dir + "/**"}},
	})

	_, err := runTool(ctx, fs.Write, `{"path":"f.txt","content":"hello same world"}`)
	require.NoError(t, err)

	out, err := runTool(ctx, fs.Edit, `{"path":"f.txt","old_string":"same","new_string":"same"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "replacements")

	got, err := runTool(ctx, fs.Read, `{"path":"f.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, got, "hello same world")
}

func TestFS_EditDeniedNoProfile(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	out, err := runTool(context.Background(), fs.Edit, `{"path":"x.txt","old_string":"a","new_string":"b"}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

func TestFS_ReadCRLF(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "crlf.txt")
	require.NoError(t, os.WriteFile(p, []byte("alpha\r\nbeta\r\n"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir + "/**"}},
	})

	out, err := runTool(ctx, fs.Read, `{"path":"crlf.txt"}`)
	require.NoError(t, err)
	assert.NotContains(t, out, "\r", "CRLF must be normalized; got %q", out)
	assert.Contains(t, out, "alpha")
	assert.Contains(t, out, "beta")
}

func TestFS_List(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})
	out, err := runTool(ctx, fs.List, `{"path":"."}`)
	require.NoError(t, err)
	assert.Contains(t, out, "a.go")
	assert.Contains(t, out, "sub")
}

func TestFS_Glob(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a_test.go"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pkg", "c_test.go"), []byte("x"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})

	// **/* requires a path separator, so it matches subdirectory files only
	// (guard.MatchGlob: "**/foo" matches "x/foo" but not bare "foo").
	t.Run("** requires separator", func(t *testing.T) {
		out, err := runTool(ctx, fs.Glob, `{"pattern":"**/*_test.go"}`)
		require.NoError(t, err)
		assert.Contains(t, out, "pkg/c_test.go")
		assert.NotContains(t, out, "b.go")
	})

	// *_test.go matches root-level files (* does not cross /).
	t.Run("root-level glob", func(t *testing.T) {
		out, err := runTool(ctx, fs.Glob, `{"pattern":"*_test.go"}`)
		require.NoError(t, err)
		assert.Contains(t, out, "a_test.go")
		assert.NotContains(t, out, "c_test.go")
		assert.NotContains(t, out, "b.go")
	})
}

func TestFS_ListWithPattern(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a_test.go"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), []byte("y"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})
	out, err := runTool(ctx, fs.List, `{"path":".","pattern":"*_test.go"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "a_test.go")
	assert.NotContains(t, out, "a.go")
	assert.NotContains(t, out, "b.go")
}

func TestFS_GlobNonMatching(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})
	out, err := runTool(ctx, fs.Glob, `{"pattern":"*.py"}`)
	require.NoError(t, err)
	assert.Equal(t, "[]", out)
}

func TestFS_ListDeniedOutsideReadList(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{"/nonexistent/**"}},
	})
	out, err := runTool(ctx, fs.List, `{"path":"."}`)
	require.NoError(t, err, "permission denial must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "permission denied")
}

func TestFS_Search(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package x\n// TODO fix\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("nothing here"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node_modules", "ignored.go"), []byte("// TODO leak"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})

	out, err := runTool(ctx, fs.Search, `{"pattern":"TODO","path":"."}`)
	require.NoError(t, err)
	assert.Contains(t, out, "a.go")
	assert.NotContains(t, out, "ignored.go")
	assert.NotContains(t, out, "b.txt")

	out, err = runTool(ctx, fs.Search, `{"pattern":"TODO","path":".","glob":"*.go","output_mode":"files_with_matches"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "a.go")
}

func TestFS_SearchInvalidRegex(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})
	out, err := runTool(ctx, fs.Search, `{"pattern":"(unclosed","path":"."}`)
	require.NoError(t, err, "bad regexp must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
}

func TestFS_SearchContentMode(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.go"), []byte("line1\nline2 TODO\nline3\nline4 TODO here\nline5\n"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})

	out, err := runTool(ctx, fs.Search, `{"pattern":"TODO","path":".","output_mode":"content"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "app.go")
	assert.Contains(t, out, `"line":2`)
	assert.Contains(t, out, `"line":4`)
	assert.NotContains(t, out, `"line":5`)
}

func TestFS_SearchCountMode(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("TODO first\nTODO second\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), []byte("TODO third\n"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})

	out, err := runTool(ctx, fs.Search, `{"pattern":"TODO","path":".","output_mode":"count"}`)
	require.NoError(t, err)
	assert.Contains(t, out, `"matches":3`)
	assert.Contains(t, out, `"files":2`)
}

func TestFS_SearchSkipsBinary(t *testing.T) {
	dir := t.TempDir()
	binData := []byte("TODO\x00more")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin.dat"), binData, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "text.txt"), []byte("TODO real text\n"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})

	out, err := runTool(ctx, fs.Search, `{"pattern":"TODO","path":".","output_mode":"files_with_matches"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "text.txt")
	assert.NotContains(t, out, "bin.dat")
}

func TestFS_SearchIgnoresDotGit(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("TODO secret\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("TODO visible\n"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})

	out, err := runTool(ctx, fs.Search, `{"pattern":"TODO","path":".","output_mode":"files_with_matches"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "main.go")
	assert.NotContains(t, out, ".git/config")
	assert.NotContains(t, out, "config")
}

func TestFS_SearchEmptyReturnsArray(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("nothing here"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})
	out, err := runTool(ctx, fs.Search, `{"pattern":"ZZZ_NOMATCH","path":"."}`)
	require.NoError(t, err)
	assert.Equal(t, "[]", out)
}

func TestFS_SearchSkipsOversizedFiles(t *testing.T) {
	dir := t.TempDir()

	// Create a file > 1 MiB with the pattern near the end (beyond the cap).
	big := make([]byte, (1<<20)+256)
	for i := range big {
		big[i] = 'x'
	}
	copy(big[len(big)-len("TODO oversized"):], []byte("TODO oversized"))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.txt"), big, 0o644))

	// Small file with the pattern.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "small.txt"), []byte("TODO small\n"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})

	out, err := runTool(ctx, fs.Search, `{"pattern":"TODO","path":".","output_mode":"files_with_matches"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "small.txt")
	assert.NotContains(t, out, "big.txt")
}

// TestFS_SearchPerFileAuth verifies that fs_search re-authorizes each file
// individually: a profile granting only the bare root dir (not dir/**) passes
// the single root-level check but must still skip every file under it, since
// none of the files match the bare-dir pattern. Without per-file auth the
// subtree files would be read, over-granting the profile.
func TestFS_SearchPerFileAuth(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("// TODO fix\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "b.go"), []byte("// TODO nested\n"), 0o644))

	fs := NewFSTools(dir)
	// Bare dir only — NO "dir/**" subtree grant. The root "." resolves to dir
	// (matches), but dir/a.go and dir/sub/b.go do not match bare "dir".
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir}},
	})

	out, err := runTool(ctx, fs.Search, `{"pattern":"TODO","path":"."}`)
	require.NoError(t, err)
	assert.Equal(t, "[]", out, "per-file auth must skip files not in the read-list")
	assert.NotContains(t, out, "a.go")
	assert.NotContains(t, out, "b.go")
}

func TestFS_GlobDefaultPath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})
	out, err := runTool(ctx, fs.Glob, `{"pattern":"a.go"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "a.go")
}

// newVCSTestRepo builds an in-memory VCS over a temp repo root pre-seeded with
// a.go, returning the VCS, the repo id, and the root. Shared by the auto-track
// hook tests.
func newVCSTestRepo(t *testing.T) (*vcs.VCS, string, string) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644))
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	v := vcs.New(st, t.TempDir())
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	return v, repoID, root
}

func TestFS_WriteTracksToMainScope(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	fs := NewFSTools(root)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{root + "/**"}, Read: []string{root + "/**"}},
	})
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"})

	_, err := runTool(ctx, fs.Write, `{"path":"a.go","content":"edited"}`)
	require.NoError(t, err)
	pending := v.Uncommitted("main", repoID)
	assert.Contains(t, pending, "a.go")
}

func TestFS_WriteTracksToWorktreeScope(t *testing.T) {
	v, repoID, _ := newVCSTestRepo(t)
	wt, err := v.AddWorktree(repoID, nil)
	require.NoError(t, err)

	// The agent's fs tools operate inside the worktree's working dir (wt.Path),
	// which is where AddWorktree materialized main's tree (a.go lives there).
	// RecordEditWorktree resolves edits against wt.Path, so the fs root + write
	// grant must be wt.Path for tracking to fire (the realistic agent flow).
	fs := NewFSTools(wt.Path)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{wt.Path + "/**"}, Read: []string{wt.Path + "/**"}},
	})
	ctx = WithVCS(ctx, VCSScope{VCS: v, WorktreeID: wt.ID, Agent: "worker-1"})

	_, err = runTool(ctx, fs.Write, `{"path":"a.go","content":"wt-edited"}`)
	require.NoError(t, err)
	assert.Contains(t, v.Uncommitted("worktree", wt.ID), "a.go")
	assert.Empty(t, v.Uncommitted("main", repoID), "worktree edit must not touch main")
}

func TestFS_WriteNoScopeNoTracking(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	fs := NewFSTools(root)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{root + "/**"}, Read: []string{root + "/**"}},
	})
	// NO WithVCS in context → tracking is a no-op.
	_, err := runTool(ctx, fs.Write, `{"path":"a.go","content":"y"}`)
	require.NoError(t, err)
	assert.Empty(t, v.Uncommitted("main", repoID))
}

// TestFS_EditTracksToMainScope verifies the fs_edit hook: after a successful
// edit, the main changeset reflects the POST-edit content (not the pre-edit
// original), proving trackEdit is wired to the updated bytes.
func TestFS_EditTracksToMainScope(t *testing.T) {
	v, repoID, root := newVCSTestRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("alpha beta"), 0o644))

	fs := NewFSTools(root)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{root + "/**"}, Read: []string{root + "/**"}},
	})
	ctx = WithVCS(ctx, VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"})

	_, err := runTool(ctx, fs.Edit, `{"path":"a.go","old_string":"alpha","new_string":"ALPHA"}`)
	require.NoError(t, err)

	pending := v.Uncommitted("main", repoID)
	assert.Contains(t, pending, "a.go")
	sum := sha256.Sum256([]byte("ALPHA beta"))
	assert.Equal(t, hex.EncodeToString(sum[:]), pending["a.go"],
		"changeset must record the post-edit content")
}

// --- path jail (Task: jail file paths to the work root) ----------------------

// permFS builds an FSTools over dir with a profile that grants all fs tools and
// full read/write under dir — so the ONLY thing that can reject a path is the
// jail in abs(), not the permission profile.
func permFS(t *testing.T, dir string) (*FSTools, context.Context) {
	t.Helper()
	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}, Write: []string{dir, dir + "/**"}},
	})
	return fs, ctx
}

func TestFS_PathJail_RejectsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "note.txt"), []byte("ok"), 0o644))
	fs, ctx := permFS(t, dir)

	// Absolute paths OUTSIDE the project root are rejected (POSIX- or Windows-
	// absolute). Absolute paths that anchor AT the root are accepted — see
	// TestFS_PathJail_AcceptsAbsolutePathUnderRoot.
	for _, p := range []string{
		"/etc/passwd",
		"C:\\Windows\\system32\\config\\sam",
		"C:/proj/note.txt",
	} {
		out, err := runTool(ctx, fs.Read, fmt.Sprintf(`{"path":%q}`, p))
		require.NoError(t, err, "jail rejection must surface as a result for %q", p)
		assert.Contains(t, out, "✗", "path %q must be rejected", p)
		assert.Contains(t, out, "outside the project root", "path %q", p)
	}
}

func TestFS_PathJail_AcceptsAbsolutePathUnderRoot(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "note.txt"), []byte("ok"), 0o644))
	fs, ctx := permFS(t, dir)

	// Absolute path that anchors at the project root is accepted (the root
	// prefix is stripped and the path is treated as relative). Either separator
	// form works — lets the model pass "reference/x.go" or the full
	// "D:\code\yanshi\reference\x.go" interchangeably.
	for _, p := range []string{
		filepath.Join(dir, "note.txt"),
		filepath.ToSlash(filepath.Join(dir, "note.txt")),
	} {
		out, err := runTool(ctx, fs.Read, fmt.Sprintf(`{"path":%q}`, p))
		require.NoError(t, err, "absolute path under root must not error for %q", p)
		assert.Contains(t, out, "ok", "path %q must read the file", p)
	}
}

func TestFS_PathJail_RejectsDotDotTraversal(t *testing.T) {
	dir := t.TempDir()
	// Place a sibling dir outside the root to prove ".." can't reach it.
	sibling := filepath.Join(filepath.Dir(dir), "yanshi_sibling_target")
	require.NoError(t, os.MkdirAll(sibling, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sibling, "leak.txt"), []byte("secret"), 0o644))

	fs, ctx := permFS(t, dir)

	out, err := runTool(ctx, fs.Read, `{"path":"../yanshi_sibling_target/leak.txt"}`)
	require.NoError(t, err, "jail rejection must surface as a result")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "..' is not allowed")
	// The sibling file must NOT be readable through the root.
	assert.NotContains(t, out, "secret")

	// Multi-level escape is rejected too.
	out, err = runTool(ctx, fs.Read, `{"path":"../../etc/passwd"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "✗")
}

func TestFS_PathJail_AllowsDotDotThatStaysUnderRoot(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "x.txt"), []byte("inner"), 0o644))

	fs, ctx := permFS(t, dir)

	// "sub/../sub/x" cleans to "sub/x" which stays under the root — allowed.
	out, err := runTool(ctx, fs.Read, `{"path":"sub/../sub/x.txt"}`)
	require.NoError(t, err)
	assert.NotContains(t, out, "error", "a '..' that stays under root must be allowed")
	assert.Contains(t, out, "inner")
}

func TestFS_PathJail_NormalRelativePathsWork(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "deep", "nest"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deep", "nest", "b.txt"), []byte("beta"), 0o644))

	fs, ctx := permFS(t, dir)

	out, err := runTool(ctx, fs.Read, `{"path":"a.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "alpha")

	out, err = runTool(ctx, fs.Read, `{"path":"deep/nest/b.txt"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "beta")

	// "." (the root itself) is allowed for list/search.
	out, err = runTool(ctx, fs.List, `{"path":"."}`)
	require.NoError(t, err)
	assert.NotContains(t, out, "✗")
}

// TestFS_ToolErrorBecomesResult proves a tool operational error is returned as a
// JSON {"error":...} result with a nil Go error — the property the ADK relies on
// to feed the failure back to the model so it can retry, instead of aborting the
// turn on a NodeRunError.
func TestFS_ToolErrorBecomesResult(t *testing.T) {
	dir := t.TempDir()
	fs, ctx := permFS(t, dir)

	out, err := runTool(ctx, fs.Read, `{"path":"does-not-exist.txt"}`)
	require.NoError(t, err, "tool error must surface as a result, not a Go error")
	assert.Contains(t, out, "✗")
	assert.Contains(t, out, "does-not-exist.txt", "result names the offending path")
}

func TestFS_Read_InjectsNestedAgentsMd(t *testing.T) {
	root := t.TempDir()
	// Root AGENTS.md is EXCLUDED (already in system prompt); nested one is injected.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("ROOT-INSTR"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "AGENTS.md"), []byte("PKG-INSTR"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "main.go"), []byte("package main"), 0o644))

	f := NewFSTools(root)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{root + "/**"}},
	})
	out, err := runTool(ctx, f.Read, `{"path":"pkg/main.go"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "package main", "file content still present")
	assert.Contains(t, out, "PKG-INSTR", "nested AGENTS.md injected")
	assert.NotContains(t, out, "ROOT-INSTR", "root AGENTS.md must NOT be duplicated")
}

func TestFS_Read_NoInjectionWhenNoNestedInstructions(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644))
	// Root-level AGENTS.md exists but NestedInstructions excludes root -> no injection.
	require.NoError(t, os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("ROOT-INSTR"), 0o644))

	f := NewFSTools(root)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{root + "/**"}},
	})
	out, err := runTool(ctx, f.Read, `{"path":"main.go"}`)
	require.NoError(t, err)
	assert.NotContains(t, out, "ROOT-INSTR", "root-only must not inject")
	assert.Contains(t, out, "package main")
}

func TestFS_Read_StreamingBoundedMemory(t *testing.T) {
	dir := t.TempDir()
	// ~5 MiB file of ~1 KiB lines; a narrow 3-line window must return exactly
	// those 3 lines (memory bounded by the window, not the file size).
	line := "data" + strings.Repeat("x", 1020) // ~1024 B/line
	var b strings.Builder
	for i := 0; i < 5000; i++ { // ~5 MiB
		b.WriteString(line + "\n")
	}
	p := filepath.Join(dir, "huge.txt")
	require.NoError(t, os.WriteFile(p, []byte(b.String()), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir + "/**"}},
	})
	out, err := runTool(ctx, fs.Read, `{"path":"huge.txt","offset":4000,"end":4002}`)
	require.NoError(t, err)
	assert.Equal(t, "4000\t"+line+"\n4001\t"+line+"\n4002\t"+line, out)
	assert.Less(t, len(out), SpillThreshold, "narrow window result must be well under the spill threshold")
}

func TestFS_ReadNoTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	// File with no trailing newline — ScanLines still yields the final partial
	// line at EOF.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nonl.txt"), []byte("line1\nline2"), 0o644))
	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir + "/**"}},
	})
	out, err := runTool(ctx, fs.Read, `{"path":"nonl.txt"}`)
	require.NoError(t, err)
	assert.Equal(t, "1\tline1\n2\tline2", out)
}

func TestFS_SearchIgnoresYanshiScratch(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".yanshi", "tmp", "spillover"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".yanshi", "tmp", "spillover", "shell_run-x.txt"), []byte("TODO leaked scratch\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("TODO visible\n"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})

	out, err := runTool(ctx, fs.Search, `{"pattern":"TODO","path":".","output_mode":"files_with_matches"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "main.go")
	assert.NotContains(t, out, "shell_run-x.txt", "spillover scratch must be hidden from search")
}

func TestFS_ListSkipsYanshiDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".yanshi"), 0o755))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})
	out, err := runTool(ctx, fs.List, `{"path":"."}`)
	require.NoError(t, err)
	assert.Contains(t, out, "a.go")
	assert.NotContains(t, out, ".yanshi", "list must hide the .yanshi scratch dir")
}

func TestFS_GlobSkipsYanshiDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".yanshi", "tmp", "spillover"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".yanshi", "tmp", "spillover", "shell_run-1.txt"), []byte("x"), 0o644))

	fs := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{dir, dir + "/**"}},
	})
	out, err := runTool(ctx, fs.Glob, `{"pattern":"**/*.txt"}`)
	require.NoError(t, err)
	assert.NotContains(t, out, "shell_run-1.txt", "glob must not descend into .yanshi")
}

// assertDiagField 解析工具返回的 JSON,断言 "diagnostics" 字段存在/缺失。
func assertDiagField(t *testing.T, out, want string, expectPresent bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("结果不是合法 JSON: %v (out=%q)", err, out)
	}
	_, present := m["diagnostics"]
	if expectPresent && !present {
		t.Errorf("应含 diagnostics 字段,out=%s", out)
	}
	if !expectPresent && present {
		t.Errorf("不应含 diagnostics 字段(无 Manager),out=%s", out)
	}
	if expectPresent && want != "" {
		if d, _ := m["diagnostics"].(string); !strings.Contains(d, want) {
			t.Errorf("diagnostics 应含 %q,得到 %q", want, d)
		}
	}
}

// permFSCtx 镜像现有 fs 测试:真实 profile ctx,授权 fs_* + 全读写。
func permFSCtx(root string) context.Context {
	return WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{root + "/**"}, Read: []string{root + "/**"}},
	})
}

// TestFS_WriteNoLSPManager_NoDiagnosticsField 验证无 Manager 时 JSON 不含
// diagnostics 字段(零行为变化,既有键不变)。
func TestFS_WriteNoLSPManager_NoDiagnosticsField(t *testing.T) {
	root := t.TempDir()
	fs := NewFSTools(root)
	ctx := permFSCtx(root) // 无 WithLSP
	out, err := runTool(ctx, fs.Write, `{"path":"a.go","content":"package main\n"}`)
	require.NoError(t, err)
	assertDiagField(t, out, "", false)
	// 既有契约不变:
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m))
	assert.Contains(t, m, "wrote")
}

// TestFS_EditWithLSP_AppendsDiagnosticsField 验证 fake Manager 的诊断被写进
// JSON "diagnostics" 字段。
func TestFS_EditWithLSP_AppendsDiagnosticsField(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(target, []byte("package main\n"), 0o644))

	fs := NewFSTools(root)
	ctx := permFSCtx(root)
	ctx = WithLSP(ctx, &fakeLSPManager{diags: []lsp.Diagnostic{
		{Line: 2, Column: 5, Severity: lsp.SeverityError, Message: "undefined: x", Source: "gopls"},
	}})

	out, err := runTool(ctx, fs.Edit, `{"path":"main.go","old_string":"package main","new_string":"package main\nvar _ = x"}`)
	require.NoError(t, err)
	assertDiagField(t, out, "undefined: x", true)
}

// TestFS_WriteWithLSP_AppendsDiagnosticsField 同上,覆盖 runWrite 路径。
func TestFS_WriteWithLSP_AppendsDiagnosticsField(t *testing.T) {
	root := t.TempDir()
	fs := NewFSTools(root)
	ctx := permFSCtx(root)
	ctx = WithLSP(ctx, &fakeLSPManager{diags: []lsp.Diagnostic{
		{Line: 1, Column: 1, Severity: lsp.SeverityWarning, Message: "fmt unused", Source: "gopls"},
	}})
	out, err := runTool(ctx, fs.Write, `{"path":"b.go","content":"package b\n"}`)
	require.NoError(t, err)
	assertDiagField(t, out, "fmt unused", true)
}

func TestFsReadImageFileReturnsStructuredRef(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "shot.png")
	require.NoError(t, os.WriteFile(pngPath, testPNGBytes(t, 4, 4), 0644))
	f := NewFSTools(dir)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_read"}},
		FS:    guard.FSPerm{Read: []string{"**"}},
	})
	out, err := runTool(ctx, f.Read, `{"path":"shot.png"}`)
	require.NoError(t, err)
	assert.Contains(t, out, "image")
	assert.Contains(t, out, "shot.png")
	// Must NOT embed raw image bytes
	assert.NotContains(t, out, string([]byte{0x89, 0x50, 0x4e, 0x47}))
}
