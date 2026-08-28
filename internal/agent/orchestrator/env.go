package orchestrator

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/x6nux/yanshi/internal/execprobe"
)

// buildEnvInfo returns a multi-line string describing the runtime environment
// (OS, shell, language runtimes). Appended to the system instruction so the
// model knows what tools are available for shell_run calls.
//
// Everything here is STATIC for the life of the process, which is what makes it
// safe to render exactly once in New(): the OS does not change, and a toolchain
// installed mid-session is a trade this deliberately loses in exchange for not
// spawning a dozen probe subprocesses in front of every model call.
//
// The date used to be the first line and no longer is — it belongs to the
// volatile half (see sysprompt.go). Left here it froze at process start, so a
// server still up the next morning kept telling the model the wrong day with
// nothing in the transcript to reveal it.
//
// # Probes go through execprobe, and did not used to
//
// This file carried its own `probe` helper: execprobe.Run's body, line for
// line, with the timeout removed. execprobe exists because a Windows App
// Execution Alias blocks INSIDE CreateProcess, which a context deadline cannot
// kill — nothing has been created yet to cancel — so Run detaches the call at
// its deadline. None of that protected this function, while execprobe's own
// package comment claimed (since its first commit) that it did. A single stuck
// probe could hang New() indefinitely: server boot, and every sub-agent
// delegation, which calls New() while holding a concurrency slot.
//
// KNOWN COST, not fixed here: runSubAgentTurn calls New() per delegation, so
// each one re-runs the whole probe set (~230ms measured, now bounded by
// execprobe's deadline rather than unbounded). A process-wide memo would remove
// it but would also make the value leak between tests that move the
// environment; with the timeout attached the worst case is bounded, which is
// what made this the cheap half to do first.
func buildEnvInfo() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("OS: %s %s (%s)\n", runtime.GOOS, runtime.GOARCH, hostOS()))
	b.WriteString(fmt.Sprintf("Shell: %s\n", detectShell()))
	b.WriteString(shellOptions())
	b.WriteString(fmt.Sprintf("Go: %s\n", runtime.Version()[2:]))

	if v := execprobe.Run("go", "version"); v != "" {
		b.WriteString("Go: " + v + "\n")
	}
	if v := execprobe.Run("node", "--version"); v != "" {
		b.WriteString("Node.js: " + v + "\n")
	}
	if v := execprobe.Run("python3", "--version"); v != "" {
		b.WriteString("Python3: " + v + "\n")
	}
	if v := execprobe.Run("python", "--version"); v != "" && !strings.Contains(b.String(), "Python3:") {
		b.WriteString("Python: " + v + "\n")
	}
	if v := execprobe.Run("rustc", "--version"); v != "" {
		b.WriteString("Rust: " + v + "\n")
	}
	if v := execprobe.Run("cargo", "--version"); v != "" {
		b.WriteString("Cargo: " + v + "\n")
	}
	if v := execprobe.Run("java", "-version"); v != "" {
		b.WriteString("Java: " + v + "\n")
	}
	if v := execprobe.Run("gcc", "--version"); v != "" {
		b.WriteString("GCC: " + v + "\n")
	}
	if v := execprobe.Run("clang", "--version"); v != "" {
		b.WriteString("Clang: " + v + "\n")
	}
	if v := execprobe.Run("git", "--version"); v != "" {
		b.WriteString("Git: " + v + "\n")
	}
	if v := execprobe.Run("npm", "--version"); v != "" {
		b.WriteString("npm: " + v + "\n")
	}
	if v := execprobe.Run("dotnet", "--version"); v != "" {
		b.WriteString(".NET: " + v + "\n")
	}
	if v := execprobe.Run("dart", "--version"); v != "" {
		b.WriteString("Dart: " + v + "\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

func hostOS() string {
	switch runtime.GOOS {
	case "windows":
		if v := execprobe.Run("cmd", "/c", "ver"); v != "" {
			return "Windows " + v
		}
		return "Windows"
	case "darwin":
		if v := execprobe.Run("sw_vers", "-productVersion"); v != "" {
			return "macOS " + v
		}
		return "macOS"
	case "linux":
		if v := execprobe.Run("uname", "-r"); v != "" {
			return "Linux " + v
		}
		return "Linux"
	default:
		return runtime.GOOS
	}
}

func detectShell() string {
	sh := os.Getenv("SHELL")
	if sh != "" {
		return sh
	}
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "sh"
}

// shellOptions returns a line describing available shell environments for
// the shell_run tool's "env" parameter. Included in buildEnvInfo so the
// model knows which shells it can request.
func shellOptions() string {
	if runtime.GOOS == "windows" {
		return "Shell options (env parameter): auto (Windows→cmd), cmd, powershell, bash (Git Bash), zsh, sh\n"
	}
	return "Shell options (env parameter): auto (Unix→sh), sh, bash, zsh\n"
}
