//go:build darwin

package sandbox

// newPlatformSandbox returns the macOS Phase 0 adapter. Seatbelt (sandboxd)
// is the obvious candidate but applying it requires either a parent helper
// signed with the right entitlements or running the entire yanshi process
// inside a profile — selection deferred until operator requirements are clear.
func newPlatformSandbox(cfg Config) Sandbox {
	return phase0(cfg, "macos-selection-gate",
		"Seatbelt vs signed helper not decided; host guard only")
}
