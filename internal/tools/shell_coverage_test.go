package tools

import (
	"testing"
)

func TestFirstWord(t *testing.T) {
	tests := []struct {
		cmd  string
		want string
	}{
		{"ls -la", "ls"},
		{"/usr/bin/gcc -o test test.c", "gcc"},
		{"/bin/sh -c 'echo hi'", "sh"},
		{`C:\Windows\system32\cmd.exe /c dir`, "cmd"},
		{"", ""},
		{"   ", ""},
		{"git status", "git"},
		{"node.exe --version", "node"},
		{"go.exe build .", "go"},
	}
	for _, tc := range tests {
		got := firstWord(tc.cmd)
		if got != tc.want {
			t.Errorf("firstWord(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

func TestHasShellMetachar(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"ls", false},
		{"ls && rm -rf /", true},
		{"ls || echo hi", true},
		{"cat foo; rm bar", true},
		{"cat foo | grep bar", true},
		{"echo `whoami`", true},
		{"echo $(pwd)", true},
		{"cat foo > bar", true},
		{"echo hello < input", true},
		{"grep pattern file", false},
	}
	for _, tc := range tests {
		got := hasShellMetachar(tc.cmd)
		if got != tc.want {
			t.Errorf("hasShellMetachar(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}
