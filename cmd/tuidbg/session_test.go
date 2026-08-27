// session_test.go — tests for the tmux interaction layer.
//
// The tmux-dependent tests here drive a REAL tmux server rather than a fake.
// That is deliberate: every constraint session.go encodes was discovered by
// measuring tmux's actual behaviour, and a fake would only ever replay the
// behaviour we already believe in — it cannot contradict us, which is the one
// thing these tests exist to be able to do.
//
// Session names are namespaced "gotest-*" beneath the tuidbg- prefix so they
// cannot collide with a human's own sessions, with the tool's own default
// session, or with a concurrently running agent. Teardown always names an
// exact "=" target: a test that cleaned up by prefix would be capable of
// killing the very neighbour sessions one of these tests is about.

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what
// was written to it.
//
// Needed because cmdStart's duplicate-session guard is not distinguishable
// from tmux's own duplicate rejection by exit code alone — both are 1 — so the
// only evidence that the guard ran is the message it prints. See
// TestSessionLifecycle.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w

	// Drain concurrently: a writer that fills the pipe buffer before anyone
	// reads would deadlock, and a test that hangs is worse than one that fails.
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	func() {
		defer func() {
			os.Stderr = saved
			_ = w.Close()
		}()
		fn()
	}()

	out := <-done
	_ = r.Close()
	return out
}

// requireTmux skips the calling test when tmux is not installed. These tests
// assert things about tmux's behaviour, so without tmux there is nothing to
// assert — a skip, not a failure.
func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH; skipping tmux-backed test")
	}
}

// killExact removes a session by exact name (already prefixed), ignoring the
// result. Used for teardown, where "it was already gone" is a fine outcome.
//
// The literal "=" here is the same exact-match guard sessionTarget applies;
// teardown must not prefix-match or it could reap a bystander.
func killExact(fullName string) {
	_, _, _ = tmuxRun("kill-session", "-t", "="+fullName)
}

// TestTargets pins the two target builders, including the case that motivates
// them: a bare name equal to the prefix's own stem still gets prefixed, with
// no special case.
func TestTargets(t *testing.T) {
	if got, want := sessionTarget("foo"), "=tuidbg-foo"; got != want {
		t.Errorf("sessionTarget(%q) = %q, want %q", "foo", got, want)
	}
	if got, want := paneTarget("foo"), "=tuidbg-foo:0.0"; got != want {
		t.Errorf("paneTarget(%q) = %q, want %q", "foo", got, want)
	}
	// A name that looks like the prefix is not an exception: it is prefixed
	// like everything else. Carving out an exception would produce a target
	// outside this tool's namespace.
	if got, want := sessionTarget("tuidbg"), "=tuidbg-tuidbg"; got != want {
		t.Errorf("sessionTarget(%q) = %q, want %q", "tuidbg", got, want)
	}
}

// TestSessionLifecycle covers start → exists → refuse-duplicate → stop against
// a real tmux.
//
// The duplicate-start leg is the load-bearing one: silently reusing or
// recreating a session would let a caller read a stale process's screen while
// believing it to be a fresh one.
//
// That leg asserts on cmdStart's MESSAGE, not merely on its exit code, and
// mutation testing is why. tmux rejects a duplicate `new-session` on its own
// with rc=1 ("duplicate session: <name>"), so an exit-code-only assertion
// passes just as well with cmdStart's guard deleted — measured: neutering the
// `if sessionExists(name)` branch left the whole package green. The exit code
// was pinning tmux's behaviour rather than ours. Two things make the guard
// worth having in its own right, and both are invisible to rc alone: it
// explains the refusal in this tool's terms, and it is the only layer that
// stays correct if the wrapping ever stops going through `new-session -s`.
func TestSessionLifecycle(t *testing.T) {
	requireTmux(t)

	const name = "gotest-lifecycle"
	full := sessionPrefix + name
	killExact(full) // clear any residue from an interrupted earlier run
	t.Cleanup(func() { killExact(full) })

	if rc := cmdStart(name, 60, 10, []string{"sleep", "300"}, ""); rc != 0 {
		t.Fatalf("cmdStart rc = %d, want 0", rc)
	}
	if !sessionExists(name) {
		t.Fatalf("sessionExists(%q) = false after a successful start", name)
	}

	// Second start with the same name must be refused by OUR guard, and must
	// leave the running session alone.
	var rc2 int
	stderr := captureStderr(t, func() {
		rc2 = cmdStart(name, 60, 10, []string{"sleep", "300"}, "")
	})
	if rc2 == 0 {
		t.Errorf("duplicate cmdStart rc = 0, want non-zero (it must refuse, not reuse)")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("duplicate cmdStart did not report the refusal itself; stderr = %q\n"+
			"(a bare non-zero rc here proves nothing: tmux rejects a duplicate "+
			"new-session by itself, so the guard can be deleted without failing rc)", stderr)
	}
	if strings.Contains(stderr, "duplicate session") {
		t.Errorf("the duplicate reached tmux; cmdStart's own guard did not fire. stderr = %q", stderr)
	}
	if !sessionExists(name) {
		t.Errorf("the refused duplicate start destroyed the existing session")
	}

	if rc := cmdStop(name); rc != 0 {
		t.Fatalf("cmdStop rc = %d, want 0", rc)
	}
	if sessionExists(name) {
		t.Errorf("sessionExists(%q) = true after stop", name)
	}
}

// TestStopDoesNotKillNeighbours is the entire reason the "=" prefix exists.
//
// The neighbour is named so that the session under test is a strict PREFIX of
// it, which is the shape tmux's default prefix-matching mishandles: measured
// on tmux 3.7c, `has-session -t tuidbg-gotest-neigh` returns 0 when only
// `tuidbg-gotest-neighbour` exists, so a "=" -less kill-session would reap the
// bystander and report success.
//
// Both directions are exercised, because they fail differently:
//   - stopping a name that does not exist must fail AND spare the neighbour
//     (a prefix-matching kill would instead succeed, by killing the wrong one);
//   - starting that same name must not believe it already exists (a
//     prefix-matching has-session would refuse to start), and stopping it
//     afterwards must again spare the neighbour.
func TestStopDoesNotKillNeighbours(t *testing.T) {
	requireTmux(t)

	// neighbour is created raw, not through cmdStart, so this test does not
	// depend on the code path it is auditing.
	const neighbour = sessionPrefix + "gotest-neighbour"
	const victim = "gotest-neigh" // a strict prefix of the neighbour's name

	killExact(neighbour)
	killExact(sessionPrefix + victim)
	t.Cleanup(func() {
		killExact(neighbour)
		killExact(sessionPrefix + victim)
	})

	if _, stderr, rc := tmuxRun("new-session", "-d", "-s", neighbour,
		"-x", "40", "-y", "10", "sleep 300"); rc != 0 {
		t.Fatalf("could not create the neighbour session: rc=%d stderr=%s", rc, strings.TrimSpace(stderr))
	}

	// Direction 1: stopping a session that was never started must fail, and
	// must not take the neighbour with it.
	if rc := cmdStop(victim); rc == 0 {
		t.Errorf("cmdStop(%q) rc = 0 for a session that does not exist "+
			"(it prefix-matched the neighbour)", victim)
	}
	if _, _, rc := tmuxRun("has-session", "-t", "="+neighbour); rc != 0 {
		t.Fatalf("neighbour %s was killed by cmdStop(%q)", neighbour, victim)
	}

	// Direction 2: a full start/stop cycle on the prefix name leaves the
	// neighbour untouched. The start also proves sessionExists is exact —
	// prefix-matching would make it report the neighbour as this session.
	if rc := cmdStart(victim, 40, 10, []string{"sleep", "300"}, ""); rc != 0 {
		t.Fatalf("cmdStart(%q) rc = %d, want 0 (sessionExists prefix-matched the neighbour)", victim, rc)
	}
	if rc := cmdStop(victim); rc != 0 {
		t.Errorf("cmdStop(%q) rc = %d, want 0", victim, rc)
	}
	if _, _, rc := tmuxRun("has-session", "-t", "="+neighbour); rc != 0 {
		t.Errorf("neighbour %s was killed by the start/stop cycle on %q", neighbour, victim)
	}
}

// TestCaptureDistinguishesFailureFromBlank pins constraint 9: a blank screen
// and a failed capture are different outcomes.
//
// Collapsing them (the Python original wrote `capture(...) or ""`) makes a
// vanished session indistinguishable from a session that has simply not
// painted anything yet, and the caller reports success with empty output.
func TestCaptureDistinguishesFailureFromBlank(t *testing.T) {
	requireTmux(t)

	const name = "gotest-capture"
	full := sessionPrefix + name
	killExact(full)
	t.Cleanup(func() { killExact(full) })

	// `sleep` paints nothing, so the pane is live but blank.
	if rc := cmdStart(name, 40, 10, []string{"sleep", "300"}, ""); rc != 0 {
		t.Fatalf("cmdStart rc = %d, want 0", rc)
	}

	screen, err := capture(name, false)
	if err != nil {
		t.Fatalf("capture of a live blank session returned an error: %v", err)
	}
	// If the screen were not actually blank this test would still pass its
	// main assertion while no longer exercising the blank case at all, so the
	// blankness itself is checked.
	if strings.TrimSpace(screen) != "" {
		t.Errorf("expected a blank screen so the blank-vs-failure distinction is "+
			"actually exercised; got %q", screen)
	}

	// A session that does not exist is a failure, and must not be reported as
	// a blank screen.
	missing, err := capture("gotest-no-such-session", false)
	if err == nil {
		t.Errorf("capture of a nonexistent session returned nil error (screen %q); "+
			"failure is being reported as a blank screen", missing)
	}
	if missing != "" {
		t.Errorf("failed capture returned screen %q, want \"\"", missing)
	}
}

// waitForScreen polls capture until fn accepts the screen or the attempts run
// out, returning the last screen seen. Polling rather than a fixed sleep keeps
// these tests from being flaky on a slow machine while staying fast on a quick
// one; the last screen is returned either way so failures can print it.
func waitForScreen(t *testing.T, name string, fn func(string) bool) string {
	t.Helper()
	var screen string
	for i := 0; i < 100; i++ {
		s, err := capture(name, false)
		if err != nil {
			t.Fatalf("capture failed mid-test: %v", err)
		}
		screen = s
		if fn(s) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	return screen
}

// TestCaptureRejoinsWrappedLines pins constraint 3: capture-pane must pass -J.
//
// Without -J the capture is the PHYSICAL screen, so a line that wrapped at the
// right edge comes back split. This test reproduces the exact measured case —
// 50 columns of padding in a 60-column pane pushes the wrapper's exit marker
// across the boundary, and the capture returns `X__TUIDBG_E` with `XIT__=3` on
// the following line. Any consumer scanning for the marker then finds nothing,
// and a program that died with status 3 is reported as a clean success.
//
// The assertion is that the marker appears WHOLE on one line, which is the
// property -J provides and the property every marker-scanning caller needs.
func TestCaptureRejoinsWrappedLines(t *testing.T) {
	requireTmux(t)

	const name = "gotest-wrap"
	full := sessionPrefix + name
	killExact(full)
	t.Cleanup(func() { killExact(full) })

	// printf pads X out to column 50; in a 60-wide pane the marker printed
	// straight after it straddles the right edge.
	if rc := cmdStart(name, 60, 10, []string{"bash", "-c", `printf "%50s" X; exit 3`}, ""); rc != 0 {
		t.Fatalf("cmdStart rc = %d, want 0", rc)
	}

	screen := waitForScreen(t, name, func(s string) bool {
		return strings.Contains(s, exitMarker+"=")
	})

	if !strings.Contains(screen, exitMarker+"=3") {
		t.Fatalf("the exit marker did not survive line wrapping (capture is missing -J).\n"+
			"want a line containing %q, screen was:\n%s", exitMarker+"=3", screen)
	}
	// Whole-on-one-line is the actual requirement: a marker reassembled only
	// across a newline is still invisible to a line-oriented scan.
	var found bool
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, exitMarker+"=3") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("the exit marker is present but split across lines; screen:\n%s", screen)
	}
}

// TestSendIsLiteralAndOptionSafe pins constraint 4 for cmdSend: -l and --.
//
// Two independent failures are covered, because each mutation survives the
// other's assertion:
//
//   - Without -l, tmux INTERPRETS key names, so sending the word "Escape"
//     delivers an escape key. Measured: the screen shows ^[ instead of the
//     word.
//   - Without --, text beginning with "-" is parsed as an option. Measured:
//     `send-keys -t X -R` silently clears the screen and still returns rc=0.
func TestSendIsLiteralAndOptionSafe(t *testing.T) {
	requireTmux(t)

	const name = "gotest-send"
	full := sessionPrefix + name
	killExact(full)
	t.Cleanup(func() { killExact(full) })

	// `cat` echoes whatever is typed, which makes the pane's contents a direct
	// readout of what actually arrived.
	if rc := cmdStart(name, 60, 10, []string{"cat"}, ""); rc != 0 {
		t.Fatalf("cmdStart rc = %d, want 0", rc)
	}

	// -l: the key NAME must arrive as the word, not as the key it names.
	if rc := cmdSend(name, "Escape"); rc != 0 {
		t.Fatalf("cmdSend rc = %d, want 0", rc)
	}
	if rc := cmdKey(name, []string{"Enter"}); rc != 0 {
		t.Fatalf("cmdKey(Enter) rc = %d, want 0", rc)
	}
	screen := waitForScreen(t, name, func(s string) bool {
		return strings.Contains(s, "Escape")
	})
	if !strings.Contains(screen, "Escape") {
		t.Errorf("cmdSend is missing -l: the key name was interpreted as a key "+
			"rather than typed as text. screen:\n%s", screen)
	}
	if strings.Contains(screen, "\x1b") {
		t.Errorf("an escape byte reached the screen; cmdSend interpreted the key name. screen: %q", screen)
	}

	// --: text starting with "-" must be typed, not parsed as an option. "-R"
	// is the measured case, because as an option it wipes the screen at rc=0.
	if rc := cmdSend(name, "-R"); rc != 0 {
		t.Fatalf("cmdSend(-R) rc = %d, want 0", rc)
	}
	if rc := cmdKey(name, []string{"Enter"}); rc != 0 {
		t.Fatalf("cmdKey(Enter) rc = %d, want 0", rc)
	}
	screen = waitForScreen(t, name, func(s string) bool {
		return strings.Contains(s, "-R")
	})
	if !strings.Contains(screen, "-R") {
		t.Errorf("cmdSend is missing --: %q was consumed as an option instead of typed. screen:\n%s", "-R", screen)
	}
	// The earlier text must still be there. If -R had been parsed as an
	// option it would have cleared the screen — silently, at rc=0.
	if !strings.Contains(screen, "Escape") {
		t.Errorf("the screen was cleared: -R reached tmux as an option. screen:\n%s", screen)
	}
}

// TestKeyPassesDoubleDash pins constraint 4 for cmdKey.
//
// cmdKey has no -l (it sends real keys by name), so -- is its only protection
// against a key name being eaten as an option. Measured: `send-keys -t X -R`
// clears the screen and returns 0, so the damage is silent — nothing but the
// screen contents can detect it.
func TestKeyPassesDoubleDash(t *testing.T) {
	requireTmux(t)

	const name = "gotest-key"
	full := sessionPrefix + name
	killExact(full)
	t.Cleanup(func() { killExact(full) })

	if rc := cmdStart(name, 60, 10, []string{"cat"}, ""); rc != 0 {
		t.Fatalf("cmdStart rc = %d, want 0", rc)
	}

	if rc := cmdSend(name, "KEEPME"); rc != 0 {
		t.Fatalf("cmdSend rc = %d, want 0", rc)
	}
	if rc := cmdKey(name, []string{"Enter"}); rc != 0 {
		t.Fatalf("cmdKey(Enter) rc = %d, want 0", rc)
	}
	if s := waitForScreen(t, name, func(s string) bool { return strings.Contains(s, "KEEPME") }); !strings.Contains(s, "KEEPME") {
		t.Fatalf("setup failed: the marker text never appeared. screen:\n%s", s)
	}

	// Sending "-R" as a KEY: with -- it is delivered to the pane (cat echoes
	// it); without --, tmux swallows it as an option and blanks the screen.
	if rc := cmdKey(name, []string{"-R"}); rc != 0 {
		t.Fatalf("cmdKey(-R) rc = %d, want 0", rc)
	}
	if rc := cmdKey(name, []string{"Enter"}); rc != 0 {
		t.Fatalf("cmdKey(Enter) rc = %d, want 0", rc)
	}

	screen := waitForScreen(t, name, func(s string) bool {
		return strings.Contains(s, "-R")
	})
	if !strings.Contains(screen, "KEEPME") {
		t.Errorf("cmdKey is missing --: -R was parsed as an option and silently "+
			"cleared the screen (rc was still 0). screen:\n%s", screen)
	}
	if !strings.Contains(screen, "-R") {
		t.Errorf("cmdKey is missing --: %q never reached the pane. screen:\n%s", "-R", screen)
	}
}

// TestStartUsesCwd pins constraint 6: new-session must pass -c when cwd is set.
//
// Without -c the pane inherits the TMUX SERVER's working directory — whichever
// directory the process that first started the server happened to be in, which
// is essentially arbitrary from the caller's point of view. A relative command
// path then fails with "command not found", and the failure is attributed to
// the command rather than to the directory.
//
// The test runs `pwd` and reads the answer off the screen, so it asserts the
// pane's real directory rather than the argument list that was built.
func TestStartUsesCwd(t *testing.T) {
	requireTmux(t)

	const name = "gotest-cwd"
	full := sessionPrefix + name
	killExact(full)
	t.Cleanup(func() { killExact(full) })

	// t.TempDir is somewhere no tmux server would ever be sitting by chance,
	// so a match cannot be a coincidence. On macOS it lives under a symlinked
	// /var, hence the -P and the EvalSymlinks below.
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}

	if rc := cmdStart(name, 100, 10, []string{"bash", "-c", "pwd -P"}, dir); rc != 0 {
		t.Fatalf("cmdStart rc = %d, want 0", rc)
	}

	screen := waitForScreen(t, name, func(s string) bool {
		return strings.Contains(s, exitMarker+"=")
	})
	if !strings.Contains(screen, real) {
		t.Errorf("the pane did not start in cwd (new-session is missing -c).\n"+
			"want a screen containing %q, got:\n%s", real, screen)
	}
}

// TestShellQuote pins constraint 5's quoting half.
//
// cmdStart has to hand tmux ONE command string while callers supply an argv
// slice, so each element must be quoted for /bin/sh. Joining unquoted is how
// an argument containing a semicolon stops being an argument and becomes a
// second command.
//
// The round-trip case is the one that matters: the assertions on exact output
// would still pass for a function that quoted correctly but corrupted the
// value, so the last subtest hands the result to a real shell and checks what
// comes back out.
func TestShellQuote(t *testing.T) {
	if got, want := shellQuote("plain"), "'plain'"; got != want {
		t.Errorf("shellQuote(%q) = %q, want %q", "plain", got, want)
	}
	if got, want := shellQuote("has space"), "'has space'"; got != want {
		t.Errorf("shellQuote(%q) = %q, want %q", "has space", got, want)
	}
	// The single quote is the only character sh cannot escape inside single
	// quotes; it has to be closed, escaped, and reopened.
	if got, want := shellQuote("it's"), `'it'\''s'`; got != want {
		t.Errorf("shellQuote(%q) = %q, want %q", "it's", got, want)
	}
	// Metacharacters must end up inert rather than being interpreted.
	if got, want := shellQuote("a; rm -rf /"), "'a; rm -rf /'"; got != want {
		t.Errorf("shellQuote(%q) = %q, want %q", "a; rm -rf /", got, want)
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH; skipping the round-trip leg")
	}
	// Round-trip through a real shell: whatever goes in must come back out
	// byte for byte, and must arrive as a SINGLE argument.
	for _, arg := range []string{
		"plain", "has space", "it's", "a; rm -rf /", "$(whoami)", "`id`",
		`back\slash`, `"double"`, "new\nline", "*", "~", "a'b'c",
	} {
		// printf %s with a single conversion prints exactly one argument, so
		// extra words would show up as extra output.
		out, err := exec.Command("bash", "-c", "printf %s "+shellQuote(arg)).Output()
		if err != nil {
			t.Errorf("round-tripping %q failed: %v", arg, err)
			continue
		}
		if string(out) != arg {
			t.Errorf("round-trip of %q came back as %q", arg, string(out))
		}
	}
}
