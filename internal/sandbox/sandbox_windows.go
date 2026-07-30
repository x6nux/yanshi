//go:build windows

package sandbox

// newPlatformSandbox returns the Windows Phase 0 adapter. The selection gate
// (restricted token vs Job Object vs both) is intentionally undecided — both
// paths exist in the Windows sandboxing literature and the right answer
// depends on operator constraints (does the host need to run privileged
// helpers? is the model allowed to spawn GUI apps?). For now we honestly
// report DegradedHostGuard so callers cannot act on a phantom guarantee.
func newPlatformSandbox(cfg Config) Sandbox {
	return phase0(cfg, "windows-selection-gate",
		"restricted token vs Job Object not decided; host guard only")
}
