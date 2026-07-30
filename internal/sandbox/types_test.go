package sandbox

import (
	"context"
	"os/exec"
	"testing"
)

// TestPhase0AdapterIsHonestDegraded (Task 10): the Phase 0 adapter must report
// the truth — no OS-level isolation is in effect, only the host guard. This
// is the contract that lets the rest of the system label itself correctly
// ("host-guard-degraded") rather than over-claiming isolation that isn't there.
func TestPhase0AdapterIsHonestDegraded(t *testing.T) {
	sb := New(Config{Enabled: true, WorkspaceRoot: t.TempDir(), Tier: WorkspaceWrite})
	report := sb.Report()
	if report.Effective != DegradedHostGuard || report.Enforced || report.CanKillTree {
		t.Fatalf("Phase 0 must report degraded/non-enforced/no-kill-tree: %#v", report)
	}
	cmd := exec.CommandContext(context.Background(), "does-not-run")
	if err := sb.Prepare(context.Background(), cmd, CommandSpec{Tier: WorkspaceWrite}); err != nil {
		t.Fatalf("degraded adapter must leave host-guard path usable: %v", err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("degraded Close must not error: %v", err)
	}
}

// TestAccessTierOrdering pins the tier iota order so callers can compare tiers
// by value (ReadOnly < WorkspaceWrite < FullAccess). Reordering the constants
// would silently change policy semantics elsewhere.
func TestAccessTierOrdering(t *testing.T) {
	if !(ReadOnly < WorkspaceWrite && WorkspaceWrite < FullAccess) {
		t.Fatalf("tier ordering is wrong: %d %d %d", ReadOnly, WorkspaceWrite, FullAccess)
	}
}

// TestDisabledReportsDisabled covers the Enabled=false path: the sandbox must
// not claim "degraded host guard" when it's actually turned off — operators
// reading the report should see "disabled by configuration" instead.
func TestDisabledReportsDisabled(t *testing.T) {
	sb := New(Config{Enabled: false, Tier: ReadOnly})
	report := sb.Report()
	if report.Effective != Disabled {
		t.Fatalf("disabled sandbox must report Disabled, got %#v", report)
	}
}
