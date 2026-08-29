//go:build !darwin && !linux

package harden

// denyDebugger has no self-applied anti-attach mechanism on this platform.
//
// Windows is the case that matters here, and it genuinely has none a normal
// process can use: DebugActiveProcess is gated on SeDebugPrivilege, which an
// administrator holds and a process cannot take away from them, and the
// documented mitigations (ProcessDynamicCodePolicy and friends) address code
// injection rather than a debugger reading memory. The report says so instead
// of claiming a defence, because the capability report is what an operator
// reasons about when deciding whether the host is a safe place to hold keys.
func denyDebugger() Step {
	return Step{
		Name:   "debugger",
		Detail: "not applicable on this platform (no self-applied anti-attach mechanism exists)",
	}
}
