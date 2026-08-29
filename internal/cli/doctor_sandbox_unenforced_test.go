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

// TestDoctorAndRuntimeReportTheSameSandboxPosture is the fix for W-B
// fix-b57 finding 2: checkSandbox and bootstrap.Build construct the same
// operator-facing sandbox.Config (same tier / workspace_root /
// network_deny) but historically differed in exactly one field —
// bootstrap always threads the managed proxy's address into ProxyURL,
// doctor never does because it never starts a proxy. Before the fix,
// ProxyURL was read by requestedFields, so on any backend that does not
// specially wire it (every one except darwin's seatbelt) the SAME
// `security.sandbox:` stanza rendered "ok" from doctor and "warn ... NOT
// enforced by this backend: proxy_url" from the runtime — a warning about a
// key that does not exist under security.sandbox at all
// (config.example.yaml only has enabled/tier/network_deny there).
//
// The enforced-field declaration below is landlock/bwrap/windows'
// declaration, not darwin's (see sandbox_darwin.go — it is the one backend
// that reads ProxyURL for a loopback re-permit and so is the one place this
// disagreement never showed up). Reproducing it directly through
// UnenforcedFields, rather than calling sandbox.New and depending on which
// platform runs the test, is what the review used to catch this on a
// darwin host in the first place — sandbox.New here would have to run on
// linux or windows to ever go red.
func TestDoctorAndRuntimeReportTheSameSandboxPosture(t *testing.T) {
	doctorCfg := sandbox.Config{ // mirrors checkSandbox's sandbox.Config literal
		Enabled: true, WorkspaceRoot: "/w", Tier: sandbox.WorkspaceWrite, NetworkDeny: true,
	}
	runtimeCfg := doctorCfg                        // same operator config...
	runtimeCfg.ProxyURL = "http://127.0.0.1:38080" // ...plus bootstrap's own wiring doctor never sets.

	enforced := []string{sandbox.FieldTier, sandbox.FieldWorkspaceRoot, sandbox.FieldNetworkDeny}
	report := func(cfg sandbox.Config) sandbox.CapabilityReport {
		return sandbox.CapabilityReport{
			Platform: "linux", Requested: cfg.Tier, Effective: sandbox.OSIsolated, Backend: "landlock+seccomp",
			Enforced: true, Unenforced: sandbox.UnenforcedFields(cfg, enforced...),
		}
	}
	doctorRow := sandboxCheckResult(report(doctorCfg))
	runtimeRow := sandboxCheckResult(report(runtimeCfg))

	if doctorRow != runtimeRow {
		t.Fatalf("doctor and runtime disagree about the SAME security.sandbox config:\ndoctor:  %+v\nruntime: %+v",
			doctorRow, runtimeRow)
	}
}
