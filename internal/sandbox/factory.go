package sandbox

import (
	"context"
	"os/exec"
	"runtime"
)

// degraded is the single Phase 0 implementation used by every platform
// adapter. It answers Prepare/Report/Close without touching the exec.Cmd —
// the host guard layer remains the sole enforcement point. Phase 1+ will
// swap this out for platform-specific real backends.
type degraded struct {
	report CapabilityReport
}

// Prepare is intentionally a no-op in Phase 0. Real backends will mutate the
// exec.Cmd here (e.g. set SysProcAttr with a Job Object / Landlock / Seatbelt
// profile). Returning nil keeps the host guard path usable so callers do not
// need a sandbox-present / sandbox-absent code fork.
func (d *degraded) Prepare(context.Context, *exec.Cmd, CommandSpec) error { return nil }

// Report returns the cached CapabilityReport built at construction time.
func (d *degraded) Report() CapabilityReport { return d.report }

// Close releases any sandbox-wide resources. No-op in Phase 0.
func (d *degraded) Close() error { return nil }

// New is the single entry point callers use. Disabled → Effective=Disabled
// (the operator asked to turn it off); Enabled → defer to newPlatformSandbox,
// which Phase 0 implements as a DegradedHostGuard report with a per-OS reason
// string.
func New(cfg Config) Sandbox {
	if !cfg.Enabled {
		return &degraded{report: CapabilityReport{
			Platform:    runtime.GOOS,
			Requested:   cfg.Tier,
			Effective:   Disabled,
			Backend:     "none",
			Reason:      "sandbox disabled by configuration",
			Enforced:    false,
			CanKillTree: false,
			// A disabled sandbox enforces nothing, so every field the operator
			// configured under security.sandbox is inert. Saying so is the
			// point: `enabled: false` next to `network_deny: true` is a
			// coherent-looking config in which one line does nothing.
			Unenforced: UnenforcedFields(cfg),
		}}
	}
	return newPlatformSandbox(cfg)
}

// phase0 is the shared helper per-platform files delegate to. It bakes the
// platform-specific Backend/Reason into a CapabilityReport that honestly
// describes the current state: the operator asked for isolation, the sandbox
// is "on" in the abstract, but no OS-level enforcement is in effect yet.
func phase0(cfg Config, backend, reason string) Sandbox {
	return &degraded{report: CapabilityReport{
		Platform:    runtime.GOOS,
		Requested:   cfg.Tier,
		Effective:   DegradedHostGuard,
		Backend:     backend,
		Reason:      reason,
		Enforced:    false,
		CanKillTree: false,
		Unenforced:  UnenforcedFields(cfg),
	}}
}
