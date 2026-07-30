//go:build !windows && !linux && !darwin

package sandbox

// newPlatformSandbox returns the fallback Phase 0 adapter for platforms no
// reviewed OS-sandbox adapter exists for (BSDs, illumos, etc.). The host
// guard carries policy; isolation is explicitly absent.
func newPlatformSandbox(cfg Config) Sandbox {
	return phase0(cfg, "unsupported-platform",
		"no reviewed OS sandbox adapter; host guard only")
}
