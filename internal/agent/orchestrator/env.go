package orchestrator

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
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
func buildEnvInfo() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("OS: %s %s (%s)\n", runtime.GOOS, runtime.GOARCH, hostOS()))
	b.WriteString(fmt.Sprintf("Shell: %s\n", detectShell()))
	b.WriteString(shellOptions())
	b.WriteString(fmt.Sprintf("Go: %s\n", runtime.Version()[2:]))

	if v := probe("go", "version"); v != "" {
		b.WriteString("Go: " + v + "\n")
	}
	if v := probe("node", "--version"); v != "" {
		b.WriteString("Node.js: " + v + "\n")
	}
	if v := probe("python3", "--version"); v != "" {
		b.WriteString("Python3: " + v + "\n")
	}
	if v := probe("python", "--version"); v != "" && !strings.Contains(b.String(), "Python3:") {
		b.WriteString("Python: " + v + "\n")
	}
	if v := probe("rustc", "--version"); v != "" {
		b.WriteString("Rust: " + v + "\n")
	}
	if v := probe("cargo", "--version"); v != "" {
		b.WriteString("Cargo: " + v + "\n")
	}
	if v := probe("java", "-version"); v != "" {
		b.WriteString("Java: " + v + "\n")
	}
	if v := probe("gcc", "--version"); v != "" {
		b.WriteString("GCC: " + v + "\n")
	}
	if v := probe("clang", "--version"); v != "" {
		b.WriteString("Clang: " + v + "\n")
	}
	if v := probe("git", "--version"); v != "" {
		b.WriteString("Git: " + v + "\n")
	}
	if v := probe("npm", "--version"); v != "" {
		b.WriteString("npm: " + v + "\n")
	}
	if v := probe("dotnet", "--version"); v != "" {
		b.WriteString(".NET: " + v + "\n")
	}
	if v := probe("dart", "--version"); v != "" {
		b.WriteString("Dart: " + v + "\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// probe runs cmd with args and returns the first line of its combined output,
// trimmed. java -version writes to stderr, so CombinedOutput is used for all.
// Returns "" on any error (silent skip).
func probe(cmd string, args ...string) string {
	c := exec.Command(cmd, args...)
	out, err := c.CombinedOutput()
	if err != nil {
		return ""
	}
	lines := strings.SplitN(string(out), "\n", 2)
	return strings.TrimSpace(lines[0])
}

func hostOS() string {
	switch runtime.GOOS {
	case "windows":
		if v := probe("cmd", "/c", "ver"); v != "" {
			return "Windows " + v
		}
		return "Windows"
	case "darwin":
		if v := probe("sw_vers", "-productVersion"); v != "" {
			return "macOS " + v
		}
		return "macOS"
	case "linux":
		if v := probe("uname", "-r"); v != "" {
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
