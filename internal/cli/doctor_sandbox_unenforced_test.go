package cli

import (
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/sandbox"
)

// TestSandboxRowShowsEveryUnenforcedField is the doctor half of W-B-13
// ("doctor 能显示"), and it exists because a mutation probe found nothing
// holding it: deleting the warning from the row left internal/cli green.
//
// The first case is the one the whole per-field report was built for. Enforced
// is TRUE and Effective is os-isolated — both accurate statements about the
// filesystem — while `network_deny: true` does nothing at all. Every signal the
// row had BEFORE this field says the configuration is fine.
func TestSandboxRowShowsEveryUnenforcedField(t *testing.T) {
	t.Run("enforced but not entirely", func(t *testing.T) {
		row := sandboxCheckResult(sandbox.CapabilityReport{
			Platform: "linux", Requested: sandbox.WorkspaceWrite,
			Effective: sandbox.OSIsolated, Backend: "landlock",
			Enforced: true, Unenforced: []string{sandbox.FieldNetworkDeny},
		})
		if row.Status != StatusWarn {
			t.Fatalf("a partially-enforcing backend reported %v; StatusOK here tells the "+
				"operator the config is fine while naming the part that is inert", row.Status)
		}
		if !strings.Contains(row.Message, sandbox.FieldNetworkDeny) {
			t.Fatalf("the row does not name the inert field: %q", row.Message)
		}
	})

	t.Run("fully enforcing stays OK and quiet", func(t *testing.T) {
		row := sandboxCheckResult(sandbox.CapabilityReport{
			Platform: "linux", Requested: sandbox.WorkspaceWrite,
			Effective: sandbox.OSIsolated, Backend: "bubblewrap", Enforced: true,
		})
		if row.Status != StatusOK {
			t.Fatalf("a fully enforcing backend reported %v", row.Status)
		}
		if strings.Contains(row.Message, "NOT enforced") {
			t.Fatalf("a warning appeared with no unenforced fields: %q", row.Message)
		}
	})

	t.Run("degraded still names the fields", func(t *testing.T) {
		// The warning must survive the !Enforced arm too: "nothing is enforced"
		// and "these three things you configured are not enforced" are different
		// amounts of information, and the second is the actionable one.
		row := sandboxCheckResult(sandbox.CapabilityReport{
			Platform: "windows", Requested: sandbox.ReadOnly,
			Effective: sandbox.DegradedHostGuard, Backend: "job-object",
			Enforced: false, CanKillTree: true,
			Unenforced: []string{sandbox.FieldTier, sandbox.FieldWorkspaceRoot, sandbox.FieldNetworkDeny},
		})
		if row.Status != StatusWarn {
			t.Fatalf("a degraded backend reported %v", row.Status)
		}
		for _, f := range []string{sandbox.FieldTier, sandbox.FieldWorkspaceRoot, sandbox.FieldNetworkDeny} {
			if !strings.Contains(row.Message, f) {
				t.Fatalf("the degraded row drops %q: %q", f, row.Message)
			}
		}
	})
}
