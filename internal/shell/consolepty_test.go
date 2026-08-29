package shell

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ptyProbe drives a real PTY-backed child and records everything it printed.
//
// It exists because every interesting property of a PTY is a property of a
// CONVERSATION — the child prints a prompt, the caller types, the line
// discipline echoes, the child answers — and a single Read/Write pair cannot
// express that. Callers use expect() to advance the conversation and transcript()
// to attach the whole exchange to a failure message.
type ptyProbe struct {
	t       *testing.T
	console Console
	proc    Process

	mu   sync.Mutex
	seen strings.Builder
	done chan struct{}
}

// newPTYProbe starts program under a real PTY, or skips when this host cannot
// allocate one.
//
// The skip is CONDITIONAL on the capability probe rather than on GOOS: a Linux
// container without /dev/ptmx is a real deployment and reports Available=false
// through exactly the same path production callers consult. What must never be
// skipped is a host that says it CAN — that is where the assertions below have
// to run.
func newPTYProbe(t *testing.T, spec LaunchSpec) *ptyProbe {
	t.Helper()
	if cap := PlatformPTYCapability(); !cap.Available {
		t.Skipf("no PTY on this host: backend=%s reason=%s", cap.Backend, cap.Reason)
	}
	proc, console, err := StartPTYProcess(context.Background(), spec)
	if err != nil {
		t.Fatalf("StartPTYProcess(%q): %v", spec.Program, err)
	}
	if !console.PTY() {
		t.Fatalf("console.PTY() = false for a pty spawn")
	}
	p := &ptyProbe{t: t, console: console, proc: proc, done: make(chan struct{})}
	go p.drain()
	t.Cleanup(func() {
		_ = proc.Kill()
		_ = console.Close()
	})
	return p
}

// drain copies the child's output into the transcript until the master reports
// end-of-session.
func (p *ptyProbe) drain() {
	defer close(p.done)
	buf := make([]byte, 4096)
	for {
		n, err := p.console.Read(buf)
		if n > 0 {
			p.mu.Lock()
			p.seen.Write(buf[:n])
			p.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (p *ptyProbe) transcript() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen.String()
}

// typeLine writes one line to the child exactly as a keyboard would.
func (p *ptyProbe) typeLine(s string) {
	p.t.Helper()
	if _, err := p.console.Write([]byte(s + "\n")); err != nil {
		p.t.Fatalf("write %q to pty: %v", s, err)
	}
}

// expect waits for want to appear in the transcript.
//
// Polling rather than a blocking read because the drain goroutine owns the
// console; two readers on one master would split lines between them and make
// every assertion order-dependent.
func (p *ptyProbe) expect(want string, timeout time.Duration) {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(p.transcript(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.t.Fatalf("timed out waiting for %q\n--- transcript ---\n%s\n--- end ---", want, p.transcript())
}

// waitEOF waits for the master to reach end-of-session and reaps the child.
func (p *ptyProbe) waitEOF(timeout time.Duration) error {
	p.t.Helper()
	select {
	case <-p.done:
	case <-time.After(timeout):
		p.t.Fatalf("pty master never reached EOF after the child exited\n--- transcript ---\n%s\n--- end ---", p.transcript())
	}
	return p.proc.Wait()
}

// ptyShellEnv is the smallest environment an interactive `sh` needs to behave
// predictably: a fixed prompt so the assertions do not depend on the operator's
// PS1, and a dumb terminal so no readline library redraws the line with cursor
// motion the transcript would then have to parse.
func ptyShellEnv() []string {
	return []string{"PS1=REPL> ", "PS2=> ", "TERM=dumb", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
}

// TestPTYChildSeesATerminal is the property no pipe pair can have: the child's
// own isatty() answers true.
//
// This is the whole reason LaunchSpec.PTY exists. Before this backend landed,
// StartPTYProcess returned ErrPTYUnavailable and shell_start with pty=true fell
// back to a pipe, so every REPL, pager and progress meter ran in its
// non-interactive mode and shell_write_stdin had nothing to type into.
func TestPTYChildSeesATerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this probe drives /bin/sh; the ConPTY path is exercised by TestPTYConsoleIsWiredOnEveryPlatform")
	}
	p := newPTYProbe(t, LaunchSpec{
		Program: "/bin/sh",
		Args:    []string{"-c", "if [ -t 0 ] && [ -t 1 ]; then echo TTY-YES; else echo TTY-NO; fi"},
		Env:     ptyShellEnv(),
		PTY:     true,
	})
	p.expect("TTY-YES", 10*time.Second)
	if err := p.waitEOF(10 * time.Second); err != nil {
		t.Fatalf("child exited with error: %v", err)
	}
}

// TestPTYRunsAnInteractiveREPL is the acceptance the spec asks for in words a
// unit test can check: a real read-eval-print loop, driven by typing.
//
// Three separate things are asserted and each fails differently:
//
//   - the prompt appears at all, which only happens when the shell decides it
//     is interactive;
//   - the typed line is ECHOED back, which is the kernel line discipline and is
//     structurally impossible over a pipe;
//   - the command's output arrives, so the loop really evaluated it rather than
//     buffering input nobody read.
func TestPTYRunsAnInteractiveREPL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this probe drives /bin/sh; the ConPTY path is exercised by TestPTYConsoleIsWiredOnEveryPlatform")
	}
	p := newPTYProbe(t, LaunchSpec{
		Program: "/bin/sh",
		Args:    []string{"-i"},
		Env:     ptyShellEnv(),
		PTY:     true,
	})
	p.expect("REPL>", 10*time.Second)

	p.typeLine("echo hello-from-repl")
	// The echo of what was typed. A pipe-backed console never produces this
	// line, so it is the assertion that distinguishes a real terminal from a
	// child that merely printed the right answer.
	p.expect("echo hello-from-repl", 10*time.Second)
	p.expect("hello-from-repl", 10*time.Second)

	// A second turn: a REPL that answers once and then wedges would pass every
	// assertion above.
	p.typeLine("echo second-turn-$((6*7))")
	p.expect("second-turn-42", 10*time.Second)

	p.typeLine("exit")
	_ = p.waitEOF(10 * time.Second)
	t.Logf("REPL transcript:\n%s", p.transcript())
}

// TestPTYResizeReachesTheChild pins that Resize is a real ioctl and not a
// no-op that returns nil.
//
// `stty size` reads TIOCGWINSZ from the child's own controlling terminal, so
// the number it prints is the kernel's, not this process's idea of it. A Resize
// that silently did nothing would leave the default 24x80 and this fails.
func TestPTYResizeReachesTheChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stty is not available on windows")
	}
	p := newPTYProbe(t, LaunchSpec{
		Program: "/bin/sh",
		Args:    []string{"-i"},
		Env:     ptyShellEnv(),
		PTY:     true,
	})
	p.expect("REPL>", 10*time.Second)

	p.typeLine("stty size")
	p.expect("24 80", 10*time.Second)

	if err := p.console.Resize(40, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	p.typeLine("stty size")
	p.expect("40 120", 10*time.Second)

	p.typeLine("exit")
	_ = p.waitEOF(10 * time.Second)
}

// TestPTYSessionEndsAtEOFNotAtEIO pins the errno translation.
//
// On Linux the master returns EIO when the last slave closes, which is the
// normal end of every session. Manager.pump breaks its read loop on ANY error
// and then reaps, so a raw EIO would not hang — it would silently mark every
// completed PTY session as having failed, and the operator would see a session
// that produced correct output and reported an error.
func TestPTYSessionEndsAtEOFNotAtEIO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this probe drives /bin/sh")
	}
	if cap := PlatformPTYCapability(); !cap.Available {
		t.Skipf("no PTY on this host: %s", cap.Reason)
	}
	proc, console, err := StartPTYProcess(context.Background(), LaunchSpec{
		Program: "/bin/sh",
		Args:    []string{"-c", "echo done"},
		Env:     ptyShellEnv(),
		PTY:     true,
	})
	if err != nil {
		t.Fatalf("StartPTYProcess: %v", err)
	}
	defer console.Close()

	var got strings.Builder
	buf := make([]byte, 1024)
	var readErr error
	for {
		n, err := console.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			readErr = err
			break
		}
	}
	if !errors.Is(readErr, io.EOF) {
		t.Fatalf("end of session reported as %v, want io.EOF (output so far: %q)", readErr, got.String())
	}
	if !strings.Contains(got.String(), "done") {
		t.Fatalf("child output lost: %q", got.String())
	}
	if err := proc.Wait(); err != nil {
		t.Fatalf("child exited with error: %v", err)
	}
}

// TestPTYConsoleIsWiredOnEveryPlatform is the cross-platform half: it asserts
// that this GOOS either has a working PTY or says so with the sentinel, and
// that the two answers are CONSISTENT with each other.
//
// The consistency check is the point. A capability report that advertises
// Available=true while StartPTYProcess returns ErrPTYUnavailable is the exact
// over-claim the report exists to prevent, and it is the shape a half-finished
// platform port takes: the constant gets flipped and the syscall sequence does
// not land.
func TestPTYConsoleIsWiredOnEveryPlatform(t *testing.T) {
	cap := PlatformPTYCapability()
	if cap.Backend == "" || cap.Reason == "" {
		t.Fatalf("capability fields missing: %#v", cap)
	}
	if cap.Platform != runtime.GOOS {
		t.Fatalf("capability platform = %q, want %q", cap.Platform, runtime.GOOS)
	}

	proc, console, err := StartPTYProcess(context.Background(), LaunchSpec{
		Program: ptySelfTestProgram(),
		PTY:     true,
	})
	if err == nil {
		_ = proc.Kill()
		_ = console.Close()
		_ = proc.Wait()
		if !cap.Available {
			t.Fatalf("StartPTYProcess succeeded while the capability report says unavailable: %#v", cap)
		}
		return
	}
	if cap.Available && errors.Is(err, ErrPTYUnavailable) {
		t.Fatalf("capability report claims a PTY backend but StartPTYProcess returned the sentinel: %#v", cap)
	}
	if !cap.Available && !errors.Is(err, ErrPTYUnavailable) {
		t.Fatalf("unavailable platform must return ErrPTYUnavailable, got %v", err)
	}
}

// TestManagerPTYSessionIsInteractiveEndToEnd drives the SHIPPED path — the one
// shell_start with pty=true reaches — rather than StartPTYProcess directly.
//
// The difference is not cosmetic. Between the tool and the syscall sit
// SecureLaunchFactory, childLaunchPosture.prepare (which rebuilds the
// environment and may hand the spec to a sandbox backend that rewrites argv),
// and Manager.pump. Every one of those copies LaunchSpec field by field, and a
// copy that forgets PTY produces a session that starts, accepts writes, and
// never prompts — which is exactly what shell_start did before this backend
// landed, and is indistinguishable from "the command is just slow".
func TestManagerPTYSessionIsInteractiveEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this probe drives /bin/sh")
	}
	if cap := PlatformPTYCapability(); !cap.Available {
		t.Skipf("no PTY on this host: %s", cap.Reason)
	}
	mgr := NewManager(Config{
		MaxOutputBytes: 1 << 16,
		Factory:        NewSecureLaunchFactory(SecureLaunchFactory{}),
	})
	t.Cleanup(func() { _ = mgr.Close() })

	sess, err := mgr.Start(context.Background(), LaunchSpec{
		Program: "/bin/sh",
		Args:    []string{"-i"},
		Command: "sh -i",
		Env:     ptyShellEnv(),
		PTY:     true,
	})
	if err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Cancel(sess.ID) })
	if !sess.PTY {
		t.Fatal("Session.PTY is false: the flag was dropped somewhere between the caller and the console")
	}

	if _, err := mgr.Write(sess.ID, []byte("echo managed-pty-ok\n")); err != nil {
		t.Fatalf("Manager.Write: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		out, err := mgr.Read(sess.ID, 1<<16)
		if err != nil {
			t.Fatalf("Manager.Read: %v", err)
		}
		if strings.Contains(out, "managed-pty-ok") {
			t.Logf("managed PTY session output:\n%s", out)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("managed PTY session never echoed the command back\n--- output ---\n%s\n--- end ---", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ptySelfTestProgram is a program that exists on the host and exits promptly.
func ptySelfTestProgram() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	return "/bin/echo"
}
