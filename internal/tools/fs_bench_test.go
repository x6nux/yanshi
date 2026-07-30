package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"
)

// BenchmarkFSEdit measures the fs_edit path (read file → find/replace → write
// file). Two sub-cases: ExactReplace on a small file, and ExactReplaceLarge on a
// ~500-line file. Each iteration rewrites the baseline file (excluded from the
// timer) and then performs the edit. Zero external deps.
func BenchmarkFSEdit(b *testing.B) {
	root := b.TempDir()
	path := filepath.Join(root, "target.go")
	content := []byte(`package main

func foo() {
	// some line to replace
	_ = 1
	_ = 2
	_ = 3
	_ = 4
	_ = 5
}
`)
	require.NoError(b, os.WriteFile(path, content, 0o644))

	fs := NewFSTools(root)
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Write: []string{root + "/**"}, Read: []string{root + "/**"}},
	})

	b.Run("ExactReplace", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			// Each iteration rewrites the file back then edits it.
			_ = os.WriteFile(path, content, 0o644)
			args := fmt.Sprintf(`{"path":%q,"old_string":"some line to replace","new_string":"replaced line"}`, path)
			if _, err := fs.runEdit(ctx, args); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("ExactReplaceLarge", func(b *testing.B) {
		// Build a larger file for bench.
		large := []byte("package main\n\n")
		for j := 0; j < 500; j++ {
			large = append(large, []byte(fmt.Sprintf("var x%d = %d\n", j, j))...)
		}
		target := filepath.Join(root, "large.go")
		_ = os.WriteFile(target, large, 0o644)

		for i := 0; i < b.N; i++ {
			_ = os.WriteFile(target, large, 0o644)
			args := fmt.Sprintf(`{"path":%q,"old_string":"var x0 = 0","new_string":"var y0 = 0"}`, target)
			if _, err := fs.runEdit(ctx, args); err != nil {
				b.Fatal(err)
			}
		}
	})
}
