//go:build unix

package harden

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// disableCoreDumps sets RLIMIT_CORE to zero and verifies the kernel took it.
//
// Both halves matter. Setting the SOFT limit alone would leave the hard limit
// where the operator's shell put it, and anything yanshi spawns could raise its
// own soft limit back up — a child dumping core is a smaller leak than yanshi
// dumping core, but it is still the address space of a process holding whatever
// the tool handed it. Lowering the hard limit is irreversible for this process
// and every descendant, which is exactly the property wanted.
//
// The read-back is not ceremony. setrlimit is one of the calls a container
// runtime, an LSM or a seccomp profile can turn into a silent success, and a
// hardening step that reports "applied" on a host where it did not is worse
// than one that reports failure: it is the report an operator would act on.
func disableCoreDumps() Step {
	want := unix.Rlimit{Cur: 0, Max: 0}
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &want); err != nil {
		return Step{Name: "core-dumps", Detail: "setrlimit(RLIMIT_CORE, 0)", Err: err.Error()}
	}
	var got unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_CORE, &got); err != nil {
		return Step{Name: "core-dumps", Detail: "set, but could not read back", Err: err.Error()}
	}
	if got.Cur != 0 || got.Max != 0 {
		return Step{
			Name:   "core-dumps",
			Detail: fmt.Sprintf("setrlimit reported success but the limit is cur=%d max=%d", got.Cur, got.Max),
			Err:    "the kernel did not apply RLIMIT_CORE=0",
		}
	}
	return Step{Name: "core-dumps", Detail: "RLIMIT_CORE=0 (verified by read-back)"}
}
