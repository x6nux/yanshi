package shell

import (
	"runtime"
	"testing"
)

func TestShellArgvSelectsCorrectInterpreter(t *testing.T) {
	// Platform default for "" / "auto": cmd on Windows, sh elsewhere.
	defaultProg, defaultFlag := "sh", "-c"
	if runtime.GOOS == "windows" {
		defaultProg, defaultFlag = "cmd", "/c"
	}
	cases := []struct {
		env, command, wantProg string
		wantFirstArg           string
	}{
		{"", "go test", defaultProg, defaultFlag},
		{"auto", "go test", defaultProg, defaultFlag},
		{"bash", "go test", "bash", "-c"},
		{"zsh", "go test", "zsh", "-c"},
		{"sh", "go test", "sh", "-c"},
		{"powershell", "Get-Date", "powershell", "-Command"},
		{"cmd", "dir", "cmd", "/c"},
	}
	for _, tc := range cases {
		prog, args, err := ShellArgv(tc.env, tc.command)
		if err != nil {
			t.Fatalf("ShellArgv(%q): %v", tc.env, err)
		}
		if prog != tc.wantProg || len(args) < 2 || args[0] != tc.wantFirstArg || args[len(args)-1] != tc.command {
			t.Fatalf("env=%q got prog=%q args=%v", tc.env, prog, args)
		}
	}
}

func TestShellArgvRejectsUnknownEnv(t *testing.T) {
	if _, _, err := ShellArgv("fish", "go test"); err == nil {
		t.Fatal("unknown env must fail closed")
	}
}

func TestShellArgvRejectsEmptyCommand(t *testing.T) {
	if _, _, err := ShellArgv("sh", ""); err == nil {
		t.Fatal("empty command must fail closed")
	}
}
