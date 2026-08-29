package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Shell state capture and restore.
//
// # The problem
//
// yanshi's own environment is whatever launched it. Started from a desktop
// launcher, a dock icon or a systemd unit, that is a minimal environment: PATH
// is /usr/bin:/bin and nothing else, no NVM shim, no pyenv, no rustup, no
// Homebrew prefix. Every child yanshi spawns inherits that (see
// childLaunchPosture.env), so `node`, `python` and `cargo` are "command not
// found" for the model while working perfectly in the operator's terminal two
// windows over.
//
// The operator's real environment lives in their shell's startup files, and
// the only thing that can evaluate those files is that shell.
//
// # What this does
//
// Runs the operator's login shell ONCE at startup, reads back the environment
// it produced, and layers it under every child launch. Each of the four shells
// the acceptance names needs its own way to be asked — zsh/bash/sh answer
// `env`, PowerShell answers `Get-ChildItem Env:` — which is why this is a
// switch rather than one command.
//
// # Restore failure does not break anything
//
// Every failure path here returns the zero Snapshot, and the zero Snapshot's
// Apply is the identity function. A shell that is not installed, one whose rc
// file exits non-zero, one that hangs until the context deadline, one that
// prints something unparseable: all of them mean children launch with exactly
// the environment they would have had if this file did not exist. That is the
// acceptance's "还原失败不影响会话", and it is why Capture returns a Snapshot
// alongside its error rather than only an error.
//
// ponytail: environment variables only. A shell snapshot could also carry
// aliases and functions, which would need a sourced script file and would put
// text the guard never read into the child's startup path. Upgrade path if
// aliases are ever wanted: write the dump to a file and publish BASH_ENV/ENV
// (never by rewriting the authorized command string).

// Snapshot is a captured shell environment. The zero value is valid and means
// "nothing was captured"; Apply on it changes nothing.
type Snapshot struct {
	// Shell is the interpreter that produced Env, for diagnostics.
	Shell string
	Env   map[string]string
}

// Empty reports whether this snapshot has nothing to contribute.
func (s Snapshot) Empty() bool { return len(s.Env) == 0 }

// Apply layers the snapshot under base and returns the combined environment.
//
// Direction matters and is the opposite of what "restore" might suggest: base
// WINS. base is yanshi's own process environment plus whatever the caller set
// deliberately, and a value the operator exported for the server has to beat a
// value their rc file happened to set months ago. The snapshot fills in the
// names base does not mention at all — which is the entire failure this exists
// to fix, since the missing-PATH case is a PATH that is present but poor.
//
// PATH is the one exception and it is handled as one: an inherited PATH is
// prepended to the captured one rather than replacing it, so the launcher's
// directories stay reachable and the operator's toolchains become reachable.
func (s Snapshot) Apply(base []string) []string {
	if s.Empty() {
		return base
	}
	present := make(map[string]bool, len(base))
	basePath := ""
	for _, item := range base {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		present[envKey(key)] = true
		if envKey(key) == envKey("PATH") {
			basePath = value
		}
	}
	// Sorted so the produced slice is deterministic; map iteration order would
	// make two identical captures produce different child environments and
	// make any test over this a coin flip.
	names := make([]string, 0, len(s.Env))
	for name := range s.Env {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]string, 0, len(base)+len(names))
	out = append(out, base...)
	for _, name := range names {
		value := s.Env[name]
		if envKey(name) == envKey("PATH") {
			if basePath != "" {
				value = mergePath(basePath, value)
			}
			// Appended last so exec's dedup (last wins) picks the merged
			// value over the inherited one.
			out = append(out, name+"="+value)
			continue
		}
		if present[envKey(name)] {
			continue
		}
		out = append(out, name+"="+value)
	}
	return out
}

// mergePath puts base's entries first, then the captured ones that base does
// not already have. Order is preserved on both sides: PATH is a precedence
// list, and reordering it silently changes which `python` runs.
func mergePath(base, captured string) string {
	sep := string(os.PathListSeparator)
	seen := map[string]bool{}
	out := make([]string, 0, 16)
	for _, part := range strings.Split(base, sep) {
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	for _, part := range strings.Split(captured, sep) {
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return strings.Join(out, sep)
}

// envKey folds a variable name for comparison. Windows environment variables
// are case-insensitive; POSIX ones are not, and folding them would merge PATH
// with a legitimately distinct Path.
func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

// CaptureSnapshot runs shellName as a login shell and reads back the
// environment its startup files produced.
//
// shellName accepts the same tokens as ShellArgv plus "" / "auto", which
// resolves to $SHELL on unix and to powershell on Windows. "cmd" is rejected:
// cmd.exe has no startup file to evaluate, so "capture what the operator's
// startup files set" has no meaning there and running it would report an empty
// success rather than an honest refusal.
//
// The returned Snapshot is usable on every path — an error always comes with
// the zero Snapshot, never with a partial one. Callers are expected to log the
// error and carry on; see the file header.
//
// ctx MUST carry a deadline. An rc file that blocks on a network call or waits
// for input would otherwise hold up startup indefinitely, and this runs on the
// path to the first prompt.
func CaptureSnapshot(ctx context.Context, shellName string) (Snapshot, error) {
	program, args, err := snapshotArgv(shellName)
	if err != nil {
		return Snapshot{}, err
	}
	cmd := exec.CommandContext(ctx, program, args...)
	// Child env: yanshi's own, verbatim. This spawn is the operator's own
	// login shell reading the operator's own startup files, so there is
	// nothing to withhold from it — and withholding HOME in particular would
	// make it read a different user's files, or none. It is also the reason
	// the result is scrubbed downstream rather than here: whatever secrets the
	// rc files export are removed by childLaunchPosture.env's
	// ScrubCredentials pass before any child sees them, on the same code path
	// that scrubs yanshi's own environment.
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return Snapshot{}, fmt.Errorf("shell: capture %s environment: %w", program, err)
	}
	env := parseEnvDump(string(out))
	if len(env) == 0 {
		return Snapshot{}, fmt.Errorf("shell: %s produced no readable environment", program)
	}
	return Snapshot{Shell: program, Env: env}, nil
}

// snapshotArgv resolves the interpreter and the command that makes it print
// its environment.
//
// The POSIX shells get "-l -c env": login so the profile files are read,
// -c so it exits instead of waiting for input. Interactive mode (-i) is
// deliberately NOT requested even though it would pick up ~/.bashrc too — an
// interactive shell with no terminal is a well-known way to hang, and the
// startup files that set up toolchains are the login ones.
func snapshotArgv(shellName string) (string, []string, error) {
	resolved := strings.TrimSpace(shellName)
	if resolved == "" || resolved == "auto" {
		resolved = defaultSnapshotShell()
	}
	switch resolved {
	case "powershell", "pwsh":
		// -NonInteractive so a profile that prompts fails instead of hanging;
		// the profile itself is still loaded (that is the point), which is why
		// -NoProfile is absent.
		return resolved, []string{"-NonInteractive", "-Command",
			"Get-ChildItem Env: | ForEach-Object { \"$($_.Name)=$($_.Value)\" }"}, nil
	case "bash", "zsh", "sh":
		return resolved, []string{"-l", "-c", "env"}, nil
	case "cmd":
		return "", nil, fmt.Errorf("shell: cmd has no startup environment to capture")
	}
	return "", nil, fmt.Errorf("shell: unknown env %q", shellName)
}

// defaultSnapshotShell picks the operator's shell. $SHELL is the login shell
// the OS records for this user, which is the one whose startup files describe
// their toolchain; falling back to sh only when it names something this
// package cannot drive.
func defaultSnapshotShell() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	switch base := baseName(os.Getenv("SHELL")); base {
	case "bash", "zsh", "sh":
		return base
	}
	return "sh"
}

// baseName is filepath.Base for a POSIX-style $SHELL value. It is written out
// rather than imported so a Windows build does not apply Windows separator
// rules to a variable that only ever holds a unix path.
func baseName(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// envAssignRe matches the start of an environment entry in the dump. It is
// what makes multi-line values survive: a line that does NOT look like a new
// assignment is appended to the previous value rather than dropped.
//
// ponytail: a value whose own second line happens to look like NAME= is split
// in two. That is the known ceiling of every line-oriented env dump; the exact
// alternative is a NUL-delimited dump, which needs `env -0` (GNU only — BSD
// and macOS env have no such flag).
var envAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_()]*=`)

// skippedEnvNames are captured names that must never reach a child.
//
// These are the shell's own bookkeeping, and each one is actively wrong in a
// child: PWD/OLDPWD name the login shell's directory rather than the project,
// SHLVL is a depth counter, "_" is the last command the capture shell ran, and
// the prompt variables are terminal decoration. Proxy and certificate
// variables are absent from this list on purpose — PrepareEnvFor strips and
// republishes those later on the same path, and naming them twice is two
// places to disagree.
var skippedEnvNames = map[string]bool{
	"PWD": true, "OLDPWD": true, "SHLVL": true, "_": true,
	"PS1": true, "PS2": true, "PROMPT": true,
}

// parseEnvDump turns the captured stdout into a name→value map.
func parseEnvDump(out string) map[string]string {
	env := map[string]string{}
	lastName := ""
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if !envAssignRe.MatchString(line) {
			if lastName != "" {
				env[lastName] += "\n" + line
			}
			continue
		}
		name, value, _ := strings.Cut(line, "=")
		if skippedEnvNames[strings.ToUpper(name)] {
			lastName = ""
			continue
		}
		env[name] = value
		lastName = name
	}
	// A trailing newline in the dump produces one empty continuation line on
	// the last value; the shells all emit it, so trimming it is not optional.
	if lastName != "" {
		env[lastName] = strings.TrimSuffix(env[lastName], "\n")
	}
	return env
}
