//go:build linux

package sandbox

// newPlatformSandbox returns the Linux Phase 0 adapter. Landlock ABI version,
// bubblewrap vs a hand-rolled seccomp filter, and whether to use both are
// the open selection questions; until decided the host guard carries the
// policy load alone.
func newPlatformSandbox(cfg Config) Sandbox {
	return phase0(cfg, "linux-selection-gate",
		"Landlock ABI/bubblewrap/seccomp strategy not decided; host guard only")
}
