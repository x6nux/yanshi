//go:build !windows

package shell

import "os/exec"

// This file is the non-Windows half of the restricted-token carrier.
//
// Both functions are no-ops because syscall.SysProcAttr has no Token field
// outside Windows: a process token that another process assigns at creation
// time is a Windows concept with no Unix equivalent (setuid is not it — it
// changes identity rather than carrying a restricted view of the same one).
//
// They exist rather than being #ifdef'd out at the call sites so that
// childLaunchPosture.prepare and OSProcessFactory.Start stay single-platform
// files. A caller that had to branch on GOOS would have to keep doing so, and
// the first person to add a second carrier field would put the branch in only
// one of the two places.

// sandboxTokenFromCmd always reports "no token" on this platform.
func sandboxTokenFromCmd(*exec.Cmd) uintptr { return 0 }

// applySandboxToken does nothing on this platform.
//
// It deliberately does NOT return an error for a non-zero token. The only
// producer is the Windows sandbox backend, which cannot run here, so a non-zero
// value would mean a caller invented one — and failing every spawn is a worse
// answer to that than ignoring a field no mechanism on this platform reads.
func applySandboxToken(*exec.Cmd, uintptr) {}
