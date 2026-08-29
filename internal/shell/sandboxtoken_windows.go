//go:build windows

package shell

import (
	"os/exec"
	"syscall"
)

// This file is the Windows half of the restricted-token carrier: two
// conversions between LaunchSpec.ProcessToken and syscall.SysProcAttr.Token.
//
// It is split by build tag rather than guarded by runtime.GOOS because
// SysProcAttr has no Token field on Unix — the struct is genuinely different
// per platform, so this is a compile-time fork, not a stylistic one.

// sandboxTokenFromCmd reads the token a sandbox backend put on a stand-in
// command.
//
// Called by childLaunchPosture.prepare on the copy-back, which is the single
// point where a mutation made to the stand-in either survives into the spec or
// is lost. Returning 0 for "no token" makes the caller's copy-back
// unconditional: there is no state in which assigning the result is wrong.
func sandboxTokenFromCmd(cmd *exec.Cmd) uintptr {
	if cmd == nil || cmd.SysProcAttr == nil {
		return 0
	}
	return uintptr(cmd.SysProcAttr.Token)
}

// applySandboxToken puts tok onto a command that is about to be started.
//
// SysProcAttr is merged rather than replaced. Both spawn paths in this package
// set fields on it for their own reasons — the process group on Unix, and
// whatever a caller configured here — and an assignment would drop those
// silently, producing a symptom (a console window, a lost cancellation) that
// nobody would trace back to a sandbox change.
func applySandboxToken(cmd *exec.Cmd, tok uintptr) {
	if cmd == nil || tok == 0 {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Token = syscall.Token(tok)
}
