// session.go — the tmux interaction layer of tuidbg.
//
// Why tmux rather than a hand-rolled pty + VT100 emulator: the escape-sequence
// state machine, alt-screen switching and cursor redraw are all tmux's job and
// none of it is implemented here. A second benefit falls out of that choice —
// the session outlives this process, so every command can be an independent
// short-lived invocation, which is what makes it usable from an agent loop.
//
// Every constraint encoded below was measured against a real tmux, not
// inferred from the manual. Each one, when violated, reintroduces a bug that
// silently reports failure as success — see the individual doc comments.
//
// This comment is deliberately detached from the package clause (note the
// blank line): it describes this file, not the package.

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	// sessionPrefix namespaces every session this tool creates, so a stray
	// `stop` can never reach a session belonging to somebody else.
	sessionPrefix = "tuidbg-"

	// exitMarker is printed by the command wrapper (see cmdStart) once the
	// program under test has exited, carrying its exit code onto the screen.
	// The screen is the only channel available: the pane keeps running after
	// the program dies, so there is no process exit status left to read.
	exitMarker = "__TUIDBG_EXIT__"

	// tmuxMissingRC is returned when tmux could not be executed at all (not
	// on PATH, not executable). It is distinct from any exit code tmux itself
	// produces, and follows the shell convention for "command not found".
	tmuxMissingRC = 127
)

// sessionTarget builds the -t argument naming a whole session.
//
// The "=" prefix forces an exact match. tmux's -t is PREFIX-matching by
// default, so without it `stop tuidbg` also matches — and kills — a session
// named `tuidbg-x`. Measured on tmux 3.7c: with only `tuidbg-neighbour` alive,
// `has-session -t tuidbg-neigh` returns 0, while `has-session -t
// '=tuidbg-neigh'` correctly returns 1.
func sessionTarget(name string) string {
	return "=" + sessionPrefix + name
}

// paneTarget builds the -t argument naming the session's single pane.
//
// send-keys and capture-pane address a PANE, not a session, and will not
// resolve a bare session name: measured, `send-keys -t '=tuidbg-foo'` fails
// with "can't find pane" while `-t '=tuidbg-foo:0.0'` succeeds. The converse
// also holds — has-session wants the bare session form — which is why these
// are two functions and not one with a flag.
func paneTarget(name string) string {
	return "=" + sessionPrefix + name + ":0.0"
}

// tmuxRun executes one tmux command, capturing both streams as strings and
// returning tmux's exit code.
//
// Both streams are captured rather than inherited because callers decide what
// to surface: cmdStop, for instance, relays tmux's own stderr verbatim (see
// its doc comment). A failure to start tmux at all is reported as
// tmuxMissingRC with the reason in stderr, so callers never have to
// distinguish "tmux said no" from "tmux never ran" by inspecting an error
// value they might forget to check.
func tmuxRun(args ...string) (stdout, stderr string, rc int) {
	cmd := exec.Command("tmux", args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	switch e := err.(type) {
	case nil:
		rc = 0
	case *exec.ExitError:
		rc = e.ExitCode()
	default:
		// tmux could not be started (not on PATH, permission denied, ...).
		// Report it through the same channels as any other failure so no
		// caller can accidentally treat it as success.
		return outBuf.String(), "tuidbg: cannot run tmux: " + err.Error(), tmuxMissingRC
	}
	return outBuf.String(), errBuf.String(), rc
}

// sessionExists reports whether this tool's session for name is alive.
//
// Uses sessionTarget, so it answers about exactly that session and not about
// any session merely sharing its name as a prefix.
func sessionExists(name string) bool {
	_, _, rc := tmuxRun("has-session", "-t", sessionTarget(name))
	return rc == 0
}

// shellQuote wraps s in single quotes for safe interpolation into a /bin/sh
// command string, escaping embedded single quotes the only way sh allows:
// close the quote, emit an escaped quote, reopen.
//
// This exists because cmdStart must hand tmux ONE command string while the
// caller supplies an argv slice. Joining with spaces and hoping is how a path
// containing a space, or an argument containing a semicolon, turns into
// arbitrary command execution.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cmdStart launches a detached session running argv, and returns a
// process-style exit code (0 on success).
//
// The program under test is wrapped rather than run directly:
//
//	<quoted argv>; printf '__TUIDBG_EXIT__=%s\n' $?; exec sleep 100000
//
// The wrapper paints the exit code onto the screen and then blocks forever so
// the pane never dies and its final frame stays capturable. tmux's own
// remain-on-exit was tried and rejected: it paints a "Pane is dead (status N,
// ...)" line that pushes real screen content out of view.
//
// When cwd is non-empty it is passed as new-session -c. Without it the pane
// inherits the TMUX SERVER's working directory — which belongs to whichever
// process happened to start the server, not to this caller — and a relative
// command path then fails with "command not found". Note that this is done
// with -c and not by prepending a `cd` to the command string: a cd would have
// to be quoted and error-checked, and its failure mode is to silently run the
// program in the wrong directory.
//
// Boundary: an argv beginning with `exec` replaces the wrapping shell, so no
// marker is printed and the pane dies. Debugging a TUI never looks like that,
// so it is documented rather than guarded.
func cmdStart(name string, cols, rows int, argv []string, cwd string) int {
	// Never silently reuse or recreate an existing session. Either would let
	// the caller believe they are looking at a freshly started process while
	// they are in fact looking at an old one — the single most misleading
	// state this tool can be in.
	if sessionExists(name) {
		fmt.Fprintf(os.Stderr,
			"tuidbg: session %s%s already exists. Stop it first, or pick another name.\n"+
				"        (Refusing to reuse or recreate it: either would show you a stale\n"+
				"         process while you believe it is a fresh one.)\n",
			sessionPrefix, name)
		return 1
	}

	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	inner := strings.Join(quoted, " ")
	wrapped := inner + "; printf '" + exitMarker + `=%s\n' $?; exec sleep 100000`

	args := []string{
		"new-session", "-d",
		"-s", sessionPrefix + name,
		"-x", strconv.Itoa(cols),
		"-y", strconv.Itoa(rows),
	}
	if cwd != "" {
		args = append(args, "-c", cwd)
	}
	args = append(args, wrapped)

	_, stderr, rc := tmuxRun(args...)
	if rc != 0 {
		fmt.Fprintf(os.Stderr, "tuidbg: failed to start session: %s\n", strings.TrimSpace(stderr))
		return rc
	}
	return 0
}

// cmdStop kills exactly this tool's session for name.
//
// The "=" in sessionTarget is what keeps `stop foo` from also killing
// `foo-bar`; see sessionTarget.
//
// tmux's own stderr is relayed instead of a hard-coded "session not found".
// When the tmux SERVER is not running at all, the real error is something like
// "error connecting to /private/tmp/tmux-501/default (No such file or
// directory)" — a hard-coded message would state a cause that is not the
// actual one and send the reader looking for a session that was never the
// problem.
func cmdStop(name string) int {
	_, stderr, rc := tmuxRun("kill-session", "-t", sessionTarget(name))
	if rc != 0 {
		fmt.Fprintf(os.Stderr, "tuidbg: %s\n", strings.TrimSpace(stderr))
		return 1
	}
	return 0
}

// cmdSend types text into the pane without pressing Enter.
//
// -l sends the text LITERALLY, so tmux key names are typed as words rather
// than interpreted: without it, sending "Escape" delivers an escape key and
// the screen shows ^[ instead of the word.
//
// -- is not optional. tmux keeps parsing options after -t, so a text argument
// beginning with "-" would be consumed as a flag. Measured: `send-keys -t X
// -R` silently clears the screen and still returns rc=0 — a corruption with no
// error attached to it.
func cmdSend(name, text string) int {
	_, stderr, rc := tmuxRun("send-keys", "-t", paneTarget(name), "-l", "--", text)
	if rc != 0 {
		fmt.Fprintf(os.Stderr, "tuidbg: send failed: %s\n", strings.TrimSpace(stderr))
		return 1
	}
	return 0
}

// cmdKey sends key presses using tmux's key vocabulary: Enter, Escape, Tab,
// Up, Down, BSpace, C-c, C-u and so on.
//
// -- is required here for the same measured reason as in cmdSend, and it bites
// harder: key names legitimately look like flags. `-R` reaches tmux as an
// option and silently clears the screen at rc=0 rather than being delivered as
// a key.
func cmdKey(name string, keys []string) int {
	args := append([]string{"send-keys", "-t", paneTarget(name), "--"}, keys...)
	_, stderr, rc := tmuxRun(args...)
	if rc != 0 {
		fmt.Fprintf(os.Stderr, "tuidbg: key failed: %s\n", strings.TrimSpace(stderr))
		return 1
	}
	return 0
}

// capture reads the pane's current screen. With ansi set, SGR colour sequences
// are preserved (-e); otherwise plain text comes back.
//
// -J is not optional. Without it capture-pane returns the PHYSICAL screen, so
// a line that wrapped at the right edge comes back as two lines. When the
// cursor happens to sit near the edge the wrapper's exit marker is split —
// measured as `__TUIDBG_E` on one line and `XIT__=3` on the next — the marker
// regex then finds nothing, and a program that crashed with status 3 is
// reported as a clean success. -J rejoins wrapped lines into logical ones.
//
// THE TWO RETURN VALUES MUST BE KEPT DISTINCT. An empty screen is NOT an
// error: a freshly started session legitimately renders as "". The Python
// original collapsed the two by writing `capture(...) or ""`, which routed a
// capture FAILURE into the same branch as a blank screen and returned success
// with empty output — the caller then reports "all fine" about a session that
// has vanished. This is the same defect class as dropping -J: treating "could
// not read" and "read nothing" as one result. Callers must therefore branch on
// err, never on len(screen).
func capture(name string, ansi bool) (string, error) {
	args := []string{"capture-pane", "-p"}
	if ansi {
		args = append(args, "-e")
	}
	args = append(args, "-J", "-t", paneTarget(name))

	stdout, stderr, rc := tmuxRun(args...)
	if rc != 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = "tmux capture-pane exited with status " + strconv.Itoa(rc)
		}
		return "", fmt.Errorf("capture %s%s: %s", sessionPrefix, name, msg)
	}
	return stdout, nil
}
