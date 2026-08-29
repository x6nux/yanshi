//go:build linux

package harden

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// denyDebugger clears the dumpable attribute, which is what Linux checks before
// letting anything ptrace this process or read /proc/PID/mem.
//
// PR_SET_DUMPABLE=0 is the same switch that suppresses core dumps, so it
// overlaps with disableCoreDumps by design rather than by accident: the two
// mechanisms fail differently (a container can forbid setrlimit while allowing
// prctl, and Yama's ptrace_scope can be set either way), and having both means
// the surviving one still closes the hole. Reporting them as separate steps is
// what makes the overlap visible when one of them fails.
//
// Read-back via PR_GET_DUMPABLE, for the same reason the rlimit is read back: a
// seccomp profile that returns 0 for prctl produces a step that claims success
// on a host where the attribute is untouched.
//
// The cost is real and is accepted: with dumpable cleared, /proc/self is owned
// by root, so a same-uid process (including a debugger, a profiler, or `gdb -p`)
// can no longer inspect yanshi. DisableEnv is the escape hatch.
//
// # Why this does not break the bubblewrap sandbox
//
// The obvious fear is that it would. task_dump_owner() re-owns every
// /proc/PID/ entry to root when dumpable is 0, and unprivileged bubblewrap
// works by unshare(CLONE_NEWUSER) followed by a write to /proc/self/uid_map —
// a write an unprivileged process cannot perform on a root-owned file, because
// the new namespace maps no uid that would let CAP_DAC_OVERRIDE apply. Were the
// attribute inherited by bwrap, the strongest Linux sandbox backend would
// silently degrade to Landlock and the capability Reason would blame the host.
//
// It is not inherited, because bwrap is reached through execve and
// begin_new_exec() re-derives dumpability for the new mm: SUID_DUMP_USER for an
// ordinary exec with no credential change, suid_dumpable only for a secure one.
// prctl(2)'s wording enumerates the cases that reset the attribute to
// suid_dumpable and is easy to read as "otherwise preserved"; the kernel source
// is the authority, and it sets 1. So the protection covers exactly the process
// that holds the keys, and every child starts fresh.
//
// This reasoning was NOT verified on a running kernel — the development host is
// darwin. What the ubuntu CI leg does verify is the read-back below; the
// bwrap interaction is verified there only indirectly, by the sandbox probe
// continuing to select bubblewrap.
func denyDebugger() Step {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return Step{Name: "debugger", Detail: "prctl(PR_SET_DUMPABLE, 0)", Err: err.Error()}
	}
	got, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil {
		return Step{Name: "debugger", Detail: "set, but could not read back", Err: err.Error()}
	}
	if got != 0 {
		return Step{
			Name:   "debugger",
			Detail: fmt.Sprintf("prctl reported success but PR_GET_DUMPABLE is %d", got),
			Err:    "the kernel did not clear the dumpable attribute",
		}
	}
	return Step{Name: "debugger", Detail: "PR_SET_DUMPABLE=0 (verified by read-back)"}
}
