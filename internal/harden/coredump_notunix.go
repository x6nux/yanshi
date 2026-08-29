//go:build !unix

package harden

// disableCoreDumps has no Unix rlimit to lower here.
//
// On Windows the equivalent exposure is a Windows Error Reporting crash dump,
// which is governed by machine-wide registry policy (HKLM\...\Windows Error
// Reporting\LocalDumps) rather than by anything a process can set for itself.
// Saying so is the honest report; returning success would claim a defence that
// does not exist, and returning an error would make every Windows start print a
// failure the operator cannot act on.
func disableCoreDumps() Step {
	return Step{
		Name:   "core-dumps",
		Detail: "not applicable on this platform (no RLIMIT_CORE); crash dumps are governed by host policy",
	}
}
