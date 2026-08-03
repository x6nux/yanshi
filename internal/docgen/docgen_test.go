package docgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/testutil"
)

func TestWrap(t *testing.T) {
	got := Wrap("abc", "inner\n  content")
	if !strings.Contains(got, "BEGIN") {
		t.Fatalf("missing begin marker: %q", got)
	}
	if !strings.Contains(got, "END") {
		t.Fatalf("missing end marker: %q", got)
	}
}

func TestRewriteBlock_ReplaceExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.md")
	if err := os.WriteFile(path, []byte("before\n"+Wrap("x", "old")+"\nafter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RewriteBlock(path, "x", "new"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	if !strings.Contains(got, "new") {
		t.Fatalf("replacement not found: %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("surrounding text lost: %q", got)
	}
}

func TestRewriteBlock_AppendToEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.md")
	if err := RewriteBlock(path, "y", "content"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "content") {
		t.Fatalf("content not appended: %q", string(data))
	}
}

func TestRewriteBlock_AppendToFileWithSuffix(t *testing.T) {
	t.Run("with_newline", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "n.md")
		if err := os.WriteFile(path, []byte("existing\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := RewriteBlock(path, "z", "appended"); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		if !strings.Contains(string(data), "appended") {
			t.Fatalf("content not appended: %q", string(data))
		}
	})
	t.Run("with_double_newline", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "dn.md")
		if err := os.WriteFile(path, []byte("existing\n\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := RewriteBlock(path, "a", "appended"); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		if !strings.Contains(string(data), "appended") {
			t.Fatalf("content not appended: %q", string(data))
		}
	})
	t.Run("without_newline", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "wn.md")
		if err := os.WriteFile(path, []byte("no trailing newline"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := RewriteBlock(path, "b", "appended"); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		if !strings.Contains(string(data), "appended") {
			t.Fatalf("content not appended: %q", string(data))
		}
	})
}

// TestRewriteBlock_ReadDirAsFile covers the ReadFile error branch at
// docgen.go:38-39. Passing a directory as the path triggers "is a
// directory" from os.ReadFile.
func TestRewriteBlock_ReadDirAsFile(t *testing.T) {
	dir := t.TempDir()
	err := RewriteBlock(dir, "x", "content")
	if err == nil || !strings.Contains(err.Error(), "read ") {
		t.Fatalf("expected read error for directory, got: %v", err)
	}
}

// TestRewriteBlock_WriteDirAsTarget covers the WriteFile error branch at
// docgen.go:65. Create a file then set it read-only, so os.WriteFile fails.
func TestRewriteBlock_WriteReadOnlyTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ro.md")
	if err := os.WriteFile(path, []byte(Wrap("x", "old")), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the file read-only so WriteFile fails.
	testutil.SkipIfRoot(t) // root bypasses the 0444 guard below
	if err := os.Chmod(path, 0o444); err != nil {
		t.Skipf("chmod failed: %v", err)
	}
	err := RewriteBlock(path, "x", "new")
	if err == nil {
		t.Fatalf("expected write error for read-only file, got: %v", err)
	}
}
