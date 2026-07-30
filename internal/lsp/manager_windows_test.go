//go:build windows

package lsp

import (
	"strings"
	"testing"
)

// TestPathToURL_WindowsDrive covers the Windows drive-letter file-URL shape:
// "D:\..." becomes "file:///D:/..." with spaces and non-ASCII percent-escaped.
// Posix escape behavior is covered cross-platform by TestPathToURL_Escape in
// manager_test.go.
func TestPathToURL_WindowsDrive(t *testing.T) {
	got := pathToURL(`D:\code\my proj\中文.go`)
	if !strings.HasPrefix(got, "file:///D:/code/my%20proj/") {
		t.Errorf("Windows 盘符/空格 escape 失败: %q", got)
	}
	if !strings.Contains(got, "%E4%B8%AD%E6%96%87.go") {
		t.Errorf("中文 escape 失败: %q", got)
	}
}
