//go:build darwin

package harden

import "golang.org/x/sys/unix"

// denyDebugger asks the kernel to refuse every future ptrace attach to this
// process (PT_DENY_ATTACH), which is what stops a same-uid process from reading
// yanshi's heap — provider keys, tokens, the whole transcript — out of a live
// process without needing a crash.
//
// It is irreversible for the lifetime of the process, and a debugger that
// attaches afterwards gets the process killed rather than a refusal. That is
// the cost of the measure, and it is why DisableEnv exists: a maintainer
// running `dlv exec ./yanshi` needs an exit that is not a source edit.
//
// There is no read-back. macOS exposes no "am I attach-denied?" query, so this
// step reports only that the kernel accepted the request. That is weaker than
// the core-dump step's verification and is stated rather than papered over.
func denyDebugger() Step {
	if err := unix.PtraceDenyAttach(); err != nil {
		return Step{Name: "debugger", Detail: "ptrace(PT_DENY_ATTACH)", Err: err.Error()}
	}
	return Step{Name: "debugger", Detail: "ptrace(PT_DENY_ATTACH) accepted (no read-back exists on darwin)"}
}
