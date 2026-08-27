package sandbox

import (
	"context"
	"os/exec"
	"testing"
)

// TestAdapterReportIsSelfConsistent is the honesty contract every platform
// adapter owes the rest of the system, in the only form that survives some
// platforms having a real backend and others not.
//
// It used to assert Phase 0 literally — Effective==DegradedHostGuard on every
// platform. That assertion became a liability the moment one platform grew a
// real backend: it would have had to be deleted (losing the honesty check
// everywhere) or exempted per-GOOS (making it pass vacuously on whichever
// platform was next to be implemented). The invariant that survives is the one
// that mattered all along: Effective and Enforced must AGREE, and a backend
// must never claim OS isolation while doing nothing.
//
// Both directions are checked because both are real failure modes. Claiming
// OSIsolated while Enforced=false is the over-claim this layer exists to
// prevent. Claiming DegradedHostGuard while Enforced=true is the mirror bug —
// tools.SandboxEnforcing ANDs the two fields, so an adapter in that state has
// its refusals silently misread as ordinary command failures and no escalation
// prompt is ever shown.
func TestAdapterReportIsSelfConsistent(t *testing.T) {
	sb := New(Config{Enabled: true, WorkspaceRoot: t.TempDir(), Tier: WorkspaceWrite})
	report := sb.Report()
	switch report.Effective {
	case OSIsolated:
		if !report.Enforced {
			t.Fatalf("adapter claims OS isolation but does not enforce: %#v", report)
		}
	case DegradedHostGuard, Disabled:
		if report.Enforced {
			t.Fatalf("adapter reports %s but claims to enforce: %#v", report.Effective, report)
		}
		if report.Reason == "" {
			t.Fatalf("a non-enforcing adapter must say why: %#v", report)
		}
	default:
		t.Fatalf("adapter reported an unknown effective mode: %#v", report)
	}
	if report.Backend == "" {
		t.Fatalf("adapter must name its backend so violation matching can route: %#v", report)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("Close must not error: %v", err)
	}
}

// TestPrepareLeavesTheLaunchPathUsable pins that Prepare never fails a spawn
// for an ordinary command, on any platform.
//
// A degraded adapter must leave the host-guard path intact rather than
// refusing every launch; an enforcing one must rewrite the command without
// erroring. Either way the caller gets a runnable *exec.Cmd back. The command
// is never started — this asserts the seam's contract, not the OS behaviour,
// which is what the platform-specific process tests are for.
func TestPrepareLeavesTheLaunchPathUsable(t *testing.T) {
	sb := New(Config{Enabled: true, WorkspaceRoot: t.TempDir(), Tier: WorkspaceWrite})
	for _, tier := range []AccessTier{ReadOnly, WorkspaceWrite, FullAccess} {
		t.Run(tier.String(), func(t *testing.T) {
			cmd := exec.CommandContext(context.Background(), "does-not-run", "--flag", "value")
			before := len(cmd.Args)
			if err := sb.Prepare(context.Background(), cmd, CommandSpec{Path: cmd.Path, Tier: tier}); err != nil {
				t.Fatalf("Prepare must leave the launch path usable: %v", err)
			}
			if cmd.Path == "" {
				t.Fatal("Prepare left the command with no program to run")
			}
			if len(cmd.Args) < before {
				t.Fatalf("Prepare dropped arguments: had %d, now %d (%v)", before, len(cmd.Args), cmd.Args)
			}
		})
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
