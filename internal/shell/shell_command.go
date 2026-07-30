// Package shell is the runtime backing yanshi's shell v2 (Task 16+) and the
// SecureProcessFactory (Task 14/19). It owns the parts that have to be
// sharable between tools/shell.go (legacy shell_run), tools/shell_v2.go
// (Task 20), and the future TUI /jobs panel:
//
//   - ShellArgv: resolves the interpreter argv for a given env + command;
//   - Manager (Task 17): owns persistent sessions with ring-buffered output;
//   - Console (Task 18): platform-specific PTY capability boundary.
//
// All of these are deliberately distinct from the legacy shell_run path in
// internal/tools/shell.go, which stays as a thin wrapper so the existing
// transports keep working while the v2 surface comes online.
package shell

import (
	"fmt"
	"runtime"
)

// ShellArgv resolves the interpreter argv for the given shell environment and
// command string. The argv is passed verbatim to SecureProcessFactory — no
// re-marshalling through a shell happens at this layer.
//
// Resolution rules:
//   - env == "" or "auto": platform default (cmd on Windows, sh elsewhere).
//   - env == "bash"/"zsh"/"sh"/"cmd"/"powershell": explicit interpreter.
//   - any other value: error (fail-closed — an unknown interpreter cannot be
//     silently mapped to sh because the user may have a non-POSIX shell in
//     mind, and falling through to sh would surprise them).
//
// The returned argv is always [flag, command]: cmd /c, powershell -Command,
// sh/bash/zsh -c. Callers append/modify as needed (e.g. bash's "-l" for a
// login shell is a Manager-layer concern in Task 17).
func ShellArgv(env, command string) (string, []string, error) {
	if command == "" {
		return "", nil, fmt.Errorf("shell: empty command")
	}
	resolved := env
	if resolved == "" || resolved == "auto" {
		if runtime.GOOS == "windows" {
			resolved = "cmd"
		} else {
			resolved = "sh"
		}
	}
	switch resolved {
	case "cmd":
		return "cmd", []string{"/c", command}, nil
	case "powershell":
		return "powershell", []string{"-Command", command}, nil
	case "bash":
		return "bash", []string{"-c", command}, nil
	case "zsh":
		return "zsh", []string{"-c", command}, nil
	case "sh":
		return "sh", []string{"-c", command}, nil
	}
	return "", nil, fmt.Errorf("shell: unknown env %q", env)
}
