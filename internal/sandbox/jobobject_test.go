package sandbox

import (
	"strings"
	"testing"
)

// This file is the Windows backend's decision logic under test, and it carries
// NO build tag on purpose.
//
// The Job Object mechanism needs Windows. Deciding what the backend may
// honestly CLAIM about that mechanism does not, and that decision is the part
// with a history of going wrong: an adapter that over-claims arms
// tools.ClassifySandboxViolation against every failing command on the host.
// Putting windowsJobReport in a platform-neutral file and testing it here means
// these assertions execute on every CI leg — including the developer
// workstations where this backend was written, which are not Windows.
//
// What this file therefore does NOT prove: that CreateJobObject works, that
// AssignProcessToJobObject binds anything, or that closing a handle kills a
// tree. Those live in jobobject_windows_test.go behind //go:build windows and
// were not executed while this was written. The split is deliberate — a test
// that cannot run is not a reason to leave the reportable logic untested too.

// TestWindowsJobReportNeverClaimsOSIsolation is the over-claim guard, and it is
// the single most important assertion about this backend.
//
// A Job Object caps lifetime and resources. It refuses no access: a child inside
// one holds the parent's token and reads and writes exactly what the parent
// could. tools.SandboxEnforcing (Effective==OSIsolated && Enforced) is what
// decides whether a failed command's stderr may be read as a sandbox REFUSAL, and
// the Windows pattern table it gates matches "Access is denied", "error 5" and
// 拒绝访问 — the ordinary Windows spelling of "this file is locked", "this
// directory is read-only", "you are not an administrator".
//
// So a report of OSIsolated from this backend would turn every one of those
// everyday failures into an escalation prompt asking the operator to approve a
// higher access tier that cannot fix the problem, because nothing denied it.
// This test fails if any configuration — including a fully successful probe —
// ever produces that claim.
func TestWindowsJobReportNeverClaimsOSIsolation(t *testing.T) {
	working := jobProbe{Created: true, LimitsApplied: true, KillOnCloseObserved: true}
	for _, tier := range []AccessTier{ReadOnly, WorkspaceWrite, FullAccess} {
		for _, deny := range []bool{false, true} {
			cfg := Config{Enabled: true, Tier: tier, NetworkDeny: deny}
			rep := windowsJobReport(cfg, working)
			if rep.Effective == OSIsolated {
				t.Fatalf("tier=%s network-deny=%t: the Job Object backend claimed "+
					"os-isolated. A job object enforces no access control, so this "+
					"would make tools.SandboxEnforcing read every 'Access is denied' "+
					"on the host as a sandbox refusal: %#v", tier, deny, rep)
			}
			if rep.Enforced {
				t.Fatalf("tier=%s network-deny=%t: the backend claimed Enforced while "+
					"applying no access control: %#v", tier, deny, rep)
			}
		}
	}
}

// TestWindowsJobReportSelfConsistency applies the package-wide honesty contract
// (the one types_test.go's TestAdapterReportIsSelfConsistent applies to the
// live adapter) to every probe outcome, not just whichever one this host
// produces.
//
// On a non-Windows workstation the live adapter is never a jobobject, so that
// test can say nothing about this code. Driving the report function directly is
// what makes the contract checkable off-platform.
func TestWindowsJobReportSelfConsistency(t *testing.T) {
	for name, probe := range map[string]jobProbe{
		"fully working":       {Created: true, LimitsApplied: true, KillOnCloseObserved: true},
		"create failed":       {Detail: "CreateJobObject failed: access denied"},
		"limits rejected":     {Created: true, Detail: "SetInformationJobObject failed"},
		"does not contain":    {Created: true, LimitsApplied: true},
		"inconsistent probe":  {KillOnCloseObserved: true},
		"empty (zero value)":  {},
		"detail-only failure": {Detail: "something went wrong"},
	} {
		t.Run(name, func(t *testing.T) {
			rep := windowsJobReport(Config{Enabled: true, Tier: WorkspaceWrite}, probe)
			if rep.Platform != "windows" {
				t.Errorf("report must name its platform: %#v", rep)
			}
			if rep.Backend == "" {
				t.Error("report must name its backend so violation matching can route")
			}
			if rep.Reason == "" {
				t.Error("a non-enforcing adapter must say why")
			}
			if rep.Effective == OSIsolated || rep.Enforced {
				t.Errorf("this backend must never claim OS isolation: %#v", rep)
			}
			if rep.Requested != WorkspaceWrite {
				t.Errorf("report dropped the requested tier: %#v", rep)
			}
		})
	}
}

// TestWindowsCanKillTreeTracksTheObservedOutcome pins the one guarantee this
// backend does buy, in both directions.
//
// CanKillTree is not decoration: shell.Manager and the TUI render tree-kill
// availability from the capability chain, and an operator told "the tree was
// reaped" when it was not will leave a build running against a directory they
// then delete. So a probe that did not OBSERVE a kill must not produce the
// claim, and — the mirror failure, equally real — a probe that did observe one
// must not withhold it, because withholding leaves Windows with no tree-kill
// story at all.
//
// The three partial-failure rows are what make this test more than a tautology:
// each one has some of the probe's evidence present, and the claim must still
// be false. That is the shape a wrong implementation actually takes — trusting
// the API return codes (Created && LimitsApplied) and skipping the observation.
func TestWindowsCanKillTreeTracksTheObservedOutcome(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe jobProbe
		want  bool
	}{
		{"all three observed", jobProbe{Created: true, LimitsApplied: true, KillOnCloseObserved: true}, true},
		{"apis succeeded but nothing died", jobProbe{Created: true, LimitsApplied: true}, false},
		{"limits rejected", jobProbe{Created: true}, false},
		{"nothing worked", jobProbe{}, false},
		{"self-inconsistent: killed without a job", jobProbe{KillOnCloseObserved: true}, false},
		{"self-inconsistent: killed without limits", jobProbe{Created: true, KillOnCloseObserved: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := windowsJobReport(Config{Enabled: true, Tier: ReadOnly}, tc.probe)
			if rep.CanKillTree != tc.want {
				t.Fatalf("CanKillTree=%t, want %t for probe %#v\nreason: %s",
					rep.CanKillTree, tc.want, tc.probe, rep.Reason)
			}
		})
	}
}

// TestWindowsBackendStringRoutesViolationMatching pins the coupling between the
// Backend strings this file produces and the substring table in
// internal/tools.BackendKindFor.
//
// That table matches by substring ("jobobject" among them), which is what lets
// the sandbox package rename its backends without breaking classification. The
// coupling is invisible: renaming jobBackend to "winjob" compiles, passes every
// other test, and silently routes Windows diagnostics to BackendUnknown.
//
// The assertion is made here rather than in internal/tools because the tools
// package cannot see these unexported constants, and duplicating the literals
// there is precisely the copy that stops describing the original.
func TestWindowsBackendStringRoutesViolationMatching(t *testing.T) {
	// The tokens internal/tools.BackendKindFor looks for on the Windows leg.
	// Kept as literals so this test fails when EITHER side moves.
	toolsTokens := []string{"appcontainer", "restrictedtoken", "jobobject", "job object"}
	for _, backend := range []string{jobBackend, jobBackendUnavailable} {
		matched := false
		for _, tok := range toolsTokens {
			if strings.Contains(strings.ToLower(backend), tok) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("Backend %q matches none of the tokens internal/tools.BackendKindFor "+
				"recognises (%v). Windows sandbox diagnostics would be routed to "+
				"BackendUnknown.", backend, toolsTokens)
		}
	}
}

// TestWindowsReasonDisclosesWhatIsNotEnforced is the operator-facing half of the
// honesty contract, and it is the reason a caveat is not enough to put only in a
// Go comment.
//
// Reason is the field bootstrap logs through slog and the `doctor` row prints
// verbatim. An operator who configured tier=read-only and network-deny on
// Windows, and reads a line saying the sandbox is working, will conclude their
// configuration is in force. It is not: a job object applies neither. The Reason
// has to say so, naming the specific settings.
//
// The negative half is equally load-bearing: the note must NOT appear for
// full-access with no network denial, because that configuration asked for
// nothing and got nothing, and there is no gap to disclose. A note that appears
// unconditionally is noise operators learn to skip, and it would stop
// distinguishing the configuration that actually has a gap.
func TestWindowsReasonDisclosesWhatIsNotEnforced(t *testing.T) {
	working := jobProbe{Created: true, LimitsApplied: true, KillOnCloseObserved: true}

	for _, tc := range []struct {
		name    string
		cfg     Config
		mustSay []string
	}{
		{"read-only", Config{Enabled: true, Tier: ReadOnly}, []string{"NOT enforced", "read-only"}},
		{"workspace-write", Config{Enabled: true, Tier: WorkspaceWrite}, []string{"NOT enforced", "workspace-write"}},
		{"network denial only", Config{Enabled: true, Tier: FullAccess, NetworkDeny: true}, []string{"NOT enforced", "network-deny"}},
		{"both", Config{Enabled: true, Tier: ReadOnly, NetworkDeny: true}, []string{"read-only", "network-deny"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := windowsJobReport(tc.cfg, working).Reason
			for _, want := range tc.mustSay {
				if !strings.Contains(reason, want) {
					t.Errorf("the Reason an operator reads does not mention %q, so a "+
						"configured-but-unenforced setting is invisible: %q", want, reason)
				}
			}
		})
	}

	// The configuration with nothing to disclose must stay quiet.
	quiet := windowsJobReport(Config{Enabled: true, Tier: FullAccess, NetworkDeny: false}, working).Reason
	if strings.Contains(quiet, "NOT enforced") {
		t.Errorf("the unenforced-settings note fired for a configuration that asked "+
			"for no restriction; an unconditional note is noise: %q", quiet)
	}
	// ...but it must still say what the backend IS and is not doing, because
	// "job object active" alone reads as containment to anyone who has not read
	// this package.
	for _, want := range []string{"kill-on-job-close", "not an access control"} {
		if !strings.Contains(quiet, want) {
			t.Errorf("the always-on part of the Reason does not say %q: %q", want, quiet)
		}
	}
}

// TestWindowsDegradedReasonNamesTheFailingStep checks that a degraded report
// tells an operator which step failed, not merely that something did.
//
// The three failures need different responses: a refused CreateJobObject is
// usually a handle-quota or policy problem, a refused SetInformationJobObject
// means the kernel would not take the limit structure, and a surviving child
// means both calls succeeded and the job still does not contain — the nested-job
// or container case. "Job objects unavailable" sends all three to the same dead
// end.
func TestWindowsDegradedReasonNamesTheFailingStep(t *testing.T) {
	for _, tc := range []struct {
		name    string
		probe   jobProbe
		mustSay string
	}{
		{"create failed", jobProbe{}, "CreateJobObject"},
		{"limits rejected", jobProbe{Created: true}, "KILL_ON_JOB_CLOSE"},
		{"child survived", jobProbe{Created: true, LimitsApplied: true}, "survived closing the job handle"},
		{"explicit detail wins", jobProbe{Detail: "ERROR_NOT_ENOUGH_QUOTA"}, "ERROR_NOT_ENOUGH_QUOTA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := windowsJobReport(Config{Enabled: true, Tier: ReadOnly}, tc.probe)
			if !strings.Contains(rep.Reason, tc.mustSay) {
				t.Errorf("the degraded Reason does not name the failing step %q: %q",
					tc.mustSay, rep.Reason)
			}
			if rep.Backend != jobBackendUnavailable {
				t.Errorf("a degraded report must not carry the working backend name: %q", rep.Backend)
			}
		})
	}
}

// TestJobProbeDetailIsNeverEmpty guards the fallback in jobProbeDetail.
//
// Every non-enforcing adapter in this package owes callers a non-empty Reason —
// types_test.go's TestAdapterReportIsSelfConsistent fails an empty one — and the
// Reason is built by interpolating this string. A probe that returned false
// without filling Detail (which the zero value does) would otherwise produce
// "unavailable ()": technically honest, operationally useless.
func TestJobProbeDetailIsNeverEmpty(t *testing.T) {
	for _, p := range []jobProbe{
		{},
		{Created: true},
		{Created: true, LimitsApplied: true},
		{Detail: "   "},
		{Created: true, LimitsApplied: true, Detail: "\n\t "},
	} {
		if got := strings.TrimSpace(jobProbeDetail(p)); got == "" {
			t.Errorf("jobProbeDetail returned nothing for %#v", p)
		}
	}
}

// TestWindowsUnenforcedNoteIsExhaustiveOverConfig pins that the disclosure
// covers every restriction Config can express.
//
// This is the test that should fail when a field is ADDED to Config. Config
// currently carries two settings a sandbox could enforce (Tier and NetworkDeny)
// and this backend enforces neither; a third one — a scratch-path list, a
// process-count cap, anything — would be silently undisclosed, and the operator
// would read a Reason that implicitly claims it is handled by not mentioning it.
//
// The count is asserted against the struct's own field list rather than written
// down, so the failure arrives at the moment the field is added rather than
// whenever someone next re-reads this file.
func TestWindowsUnenforcedNoteIsExhaustiveOverConfig(t *testing.T) {
	// Fields of Config that describe a RESTRICTION this backend would have to
	// disclose. Enabled is not one (a disabled sandbox never reaches here),
	// WorkspaceRoot is a location rather than a restriction, and ProxyURL is an
	// egress route rather than a denial.
	known := map[string]bool{
		"Enabled":       true,
		"WorkspaceRoot": true,
		"Tier":          true,
		"NetworkDeny":   true,
		"ProxyURL":      true,
	}
	for _, name := range configFieldNames() {
		if !known[name] {
			t.Errorf("Config grew a field %q that windowsUnenforcedNote has not been "+
				"taught about. Decide whether this backend enforces it; if not, add it "+
				"to the disclosure — an undisclosed setting reads as an enforced one.", name)
		}
	}
	// And the two restrictions that exist are each individually disclosed,
	// which is what makes the field census above meaningful rather than
	// bookkeeping.
	if note := windowsUnenforcedNote(Config{Tier: ReadOnly, NetworkDeny: false}); !strings.Contains(note, "read-only") {
		t.Errorf("tier alone is not disclosed: %q", note)
	}
	if note := windowsUnenforcedNote(Config{Tier: FullAccess, NetworkDeny: true}); !strings.Contains(note, "network-deny") {
		t.Errorf("network denial alone is not disclosed: %q", note)
	}
	if note := windowsUnenforcedNote(Config{Tier: FullAccess, NetworkDeny: false}); note != "" {
		t.Errorf("a configuration with no restriction produced a disclosure: %q", note)
	}
}
