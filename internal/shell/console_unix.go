//go:build unix

package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// This file is the Unix PTY backend. A PTY (rather than a pipe pair) is what
// makes an interactive child behave the way it does in a terminal:
//
//   - isatty(3) on its stdin/stdout answers true, so REPLs print their banner
//     and their prompt, `git`/`ls`/`grep` colourise, and progress meters
//     redraw instead of emitting one line per update;
//   - the kernel's line discipline echoes typed bytes back, which is what a
//     caller pumping the console renders as "the user's input appears";
//   - the child becomes a session leader with a controlling terminal, so
//     Ctrl-C style signalling and job control work at all.
//
// None of that is reachable through the pipeConsole path in process.go, which
// is why LaunchSpec.PTY exists as a separate request rather than an optimisation.

// defaultPTYRows and defaultPTYCols are the window size a freshly allocated PTY
// is given before the child starts.
//
// A PTY opens at 0x0, and a zero-sized terminal is not a neutral default: it is
// a size, and programs that lay out against it (less, vim, anything using
// ncurses, and `stty size`-driven scripts) either refuse to run or wrap every
// line at column zero. 24x80 is the historical VT100 default and the size
// almost every terminal emulator still starts at, so a caller that never calls
// Resize gets output shaped the way its author expected.
const (
	defaultPTYRows = 24
	defaultPTYCols = 80
)

// PlatformPTYCapability reports the Unix PTY backend state by actually
// allocating a pair and closing it again.
//
// A real probe rather than a compile-time constant, because "this binary has a
// PTY implementation for this GOOS" and "this process can open a PTY right now"
// are different questions and only the second one is useful to the caller. A
// container built without /dev/ptmx, a devpts that is not mounted, and an
// exhausted pty limit (RLIMIT_NOFILE, or /proc/sys/kernel/pty/max) all produce
// a host where the code below is correct and the answer is still no. Reporting
// Available=true there would put the failure at the first shell_start with
// pty=true instead of in the capability report the operator can read up front.
func PlatformPTYCapability() PTYCapability {
	master, name, err := openPTY()
	if err != nil {
		return PTYCapability{
			Platform:  runtime.GOOS,
			Backend:   ptyBackend,
			Reason:    err.Error(),
			Available: false,
		}
	}
	_ = master.Close()
	return PTYCapability{
		Platform:  runtime.GOOS,
		Backend:   ptyBackend,
		Reason:    "allocated a pty pair (" + name + ") and released it",
		Available: true,
	}
}

// StartPTYProcess spawns spec.Program on a real pseudo-terminal.
//
// The slave is handed to the child as all three standard descriptors and the
// parent keeps only the master, which is both halves of the Console: writing to
// it is typing, reading from it is the screen.
//
// Three details are load-bearing and each has a failure mode that looks like
// something else:
//
//   - Setsid + Setctty. isatty() answering true is NOT the same as having a
//     controlling terminal, and it is the second one that gives the pty a
//     foreground process group — without which the line discipline turns ^C
//     into a data byte instead of a SIGINT. Ctty is 0 because it names a CHILD
//     descriptor, after os/exec has dup'd Stdin into place. Setpgid is
//     deliberately NOT also set: setpgid(2) returns EPERM for a session leader,
//     so asking for both makes every PTY spawn fail in the child between fork
//     and exec.
//
//     Measured caveat: on darwin the kernel assigns the controlling terminal to
//     the session leader regardless, so removing Setctty here changes nothing
//     observable on that platform — TestPTYCtrlCReachesTheChild stays green.
//     Linux does not, which is where that test discriminates. Do not read the
//     darwin result as "this flag is redundant".
//
//   - The parent closes the slave once the child owns it. Holding it open
//     means the master never reaches end-of-file, and Manager.pump reads to EOF
//     before it reaps — so the session would hang forever on a child that has
//     already exited.
//
//   - spec.SeparateStderr is ignored, and ptyConsole deliberately does not
//     implement StderrConsole. A terminal has exactly one stream by
//     construction; there is no fd to hand back. Callers that parse stdout must
//     not request a PTY, and the type assertion in separatedStderr is what
//     makes ignoring it safe rather than lossy.
func StartPTYProcess(ctx context.Context, spec LaunchSpec) (Process, Console, error) {
	program, args := spec.Program, spec.Args
	if program == "" {
		return nil, nil, fmt.Errorf("shell: spec.Program required")
	}
	master, slaveName, err := openPTY()
	if err != nil {
		return nil, nil, err
	}
	slave, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("shell: open pty slave %q: %w", slaveName, err)
	}
	if err := setWinsize(master, defaultPTYRows, defaultPTYCols); err != nil {
		_ = slave.Close()
		_ = master.Close()
		return nil, nil, err
	}

	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = spec.Dir
	if len(spec.Env) > 0 {
		cmd.Env = spec.Env
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	// Same reasoning as the pipe path: CommandContext's default cancel calls
	// Process.Kill, which reaps the session leader and orphans everything it
	// started. Both cancellation paths go through the process-group kill.
	cmd.Cancel = func() error { return killProcessTree(cmd) }

	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		_ = master.Close()
		return nil, nil, err
	}
	_ = slave.Close()
	return &osProcess{cmd: cmd}, &ptyConsole{f: master, name: slaveName}, nil
}

// setWinsize applies a row/column geometry to a PTY master.
//
// Shared by the initial sizing and by Resize so the two cannot disagree about
// which ioctl or which field order the kernel expects.
func setWinsize(master *os.File, rows, cols uint16) error {
	ws := &unix.Winsize{Row: rows, Col: cols}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, ws); err != nil {
		return fmt.Errorf("shell: set pty window size: %w", err)
	}
	return nil
}

// ptyConsole is the Console over a PTY master. Read is the child's screen,
// Write is the keyboard, Resize is a real window-size change the child receives
// as SIGWINCH.
type ptyConsole struct {
	f *os.File
	// name is the slave device path, carried for error messages: "pty read:
	// /dev/pts/7" tells an operator which of several sessions failed, where a
	// bare errno does not.
	name string
}

// Read serves the child's output, translating the end-of-session errno into
// io.EOF.
//
// On Linux, reading a master whose last slave has closed returns EIO rather
// than a zero-length read. That is the NORMAL end of every PTY session, and
// surfacing it verbatim makes Manager.pump record a failed session for every
// command that merely finished. macOS returns EOF for the same event, so the
// translation is what makes the two platforms agree.
func (c *ptyConsole) Read(b []byte) (int, error) {
	n, err := c.f.Read(b)
	if err != nil && errors.Is(err, syscall.EIO) {
		return n, io.EOF
	}
	return n, err
}

func (c *ptyConsole) Write(b []byte) (int, error) { return c.f.Write(b) }

// Resize changes the terminal geometry, which the kernel delivers to the child
// as SIGWINCH so full-screen programs redraw.
func (c *ptyConsole) Resize(rows, cols uint16) error { return setWinsize(c.f, rows, cols) }

// PTY reports true: this is a real terminal, so callers may render it as one.
func (c *ptyConsole) PTY() bool { return true }

// Close releases the master. The child then sees EOF on its stdin and,
// depending on what it is, exits or keeps running until killed — closing the
// console is not a substitute for Process.Kill.
func (c *ptyConsole) Close() error { return c.f.Close() }

// CanKillTreeOnPlatform reports whether the OS factory can kill a process and
// all its descendants. On Unix this is true via setpgid/setsid plus
// kill(-pgid); see killtree_unix.go for the fallback when the child left the
// group under its own steam.
func CanKillTreeOnPlatform() bool { return true }
