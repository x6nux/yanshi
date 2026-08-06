//go:build unix

package shell

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so the whole tree can
// be signalled at once.
//
// Without it, Process.Kill signals the direct child only. A shell command like
// `sh -c 'sleep 300 & wait'` leaves the grandchild running with its parent
// reaped — the caller sees a clean cancellation and the work carries on,
// holding whatever the process held. Capabilities() has advertised CanKillTree
// on Unix from the start, so this is what makes that claim true rather than a
// promise the platform could have kept.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessTree signals the child's whole process group.
//
// The negative pid is the group form of kill(2). It is attempted first and the
// single-process kill is the fallback: a child that exec'd into something that
// called setsid() has left the group, and killing the leader we know about is
// better than killing nothing.
//
// Errors from the group kill are deliberately not surfaced when the fallback
// succeeds — ESRCH here means the group is already gone, which is the outcome
// the caller wanted.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil && pgid == cmd.Process.Pid {
		// Only signal the group when the child really is its own leader.
		// Otherwise the group is the caller's own, and -pgid would kill the
		// test runner (or the yanshi server) along with it.
		if kerr := syscall.Kill(-pgid, syscall.SIGKILL); kerr == nil {
			return nil
		}
	}
	return cmd.Process.Kill()
}
