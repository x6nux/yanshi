package sandbox

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// This file tests the platform-neutral half of the Windows restricted-token
// backend, and it is the half where the defects live.
//
// The Win32 calls in restrictedtoken_windows.go either work on a Windows host
// or fail loudly at construction, and the probe there turns "they failed" into
// a degraded report. What no host can catch by running is a report that CLAIMS
// the wrong thing: an Effective=OSIsolated on a backend that confines nothing
// arms tools.ClassifySandboxViolation against every failing command, and the
// operator sees escalation prompts for problems no privilege can fix. That
// decision is arithmetic over two booleans, so it is tested here, on every leg
// of the matrix rather than only on Windows.

// workingJob and workingToken are the probe values a healthy host produces.
var (
	workingJob   = jobProbe{Created: true, LimitsApplied: true, KillOnCloseObserved: true}
	workingToken = restrictedTokenProbe{
		Attempted: true, TokenCreated: true, ACLsApplied: true,
		WorkspaceWritable: true, OutsideDenied: true,
	}
)

// TestRestrictedTokenProbeRequiresEveryObservation pins the truth table of
// enforcing().
//
// Each field is dropped in turn from an otherwise-passing probe and the answer
// must go false. That is not pedantry about a five-way AND: the two observation
// fields are opposites and dropping either one produces a probe that still
// looks positive. A probe missing OutsideDenied saw a token that restricts
// nothing; one missing WorkspaceWritable saw a token that restricts everything,
// which would break every command the operator runs. Both must degrade, and a
// single "ok" boolean is what would have let one of them through.
func TestRestrictedTokenProbeRequiresEveryObservation(t *testing.T) {
	if !workingToken.enforcing() {
		t.Fatal("the all-observations-held probe must be enforcing, or every case below is vacuous")
	}
	for _, tc := range []struct {
		name   string
		break_ func(*restrictedTokenProbe)
	}{
		{"never attempted", func(p *restrictedTokenProbe) { p.Attempted = false }},
		{"no token", func(p *restrictedTokenProbe) { p.TokenCreated = false }},
		{"grants not written", func(p *restrictedTokenProbe) { p.ACLsApplied = false }},
		{"workspace not writable", func(p *restrictedTokenProbe) { p.WorkspaceWritable = false }},
		{"outside not denied", func(p *restrictedTokenProbe) { p.OutsideDenied = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := workingToken
			tc.break_(&p)
			if p.enforcing() {
				t.Fatalf("a probe with %s reported enforcing: %+v", tc.name, p)
			}
			if strings.TrimSpace(restrictedTokenDetail(p)) == "" {
				t.Error("a non-enforcing probe must carry a reason; an empty one renders " +
					"as \"unavailable ()\" in the line bootstrap logs")
			}
		})
	}
}

// TestWindowsReportUpgradesOnlyWithAConfiningToken is the load-bearing
// assertion in this file: it pins which of the four probe combinations may
// claim OS-level isolation.
//
// Effective=OSIsolated is not decoration. tools.SandboxEnforcing ANDs it with
// Enforced and uses the answer to decide whether a failed command's output may
// be read as an access refusal, and on Windows the strings that table matches
// ("Access is denied", "error 5") are the ordinary spelling of "another process
// holds this file" and "you are not an administrator". Claiming it without a
// confining token turns every one of those into a prompt asking the operator to
// approve a higher tier that cannot fix the problem.
//
// The reverse error is pinned too: a working token with a broken job object
// must still claim isolation, because the two mechanisms are independent and
// discarding a real write boundary because tree-kill is unavailable would be
// throwing away the stronger of the two.
func TestWindowsReportUpgradesOnlyWithAConfiningToken(t *testing.T) {
	cfg := Config{Enabled: true, Tier: WorkspaceWrite, WorkspaceRoot: `C:\work`, NetworkDeny: true}
	deadJob := jobProbe{Created: true, LimitsApplied: true}
	deadToken := restrictedTokenProbe{Attempted: true, TokenCreated: true, ACLsApplied: true}

	for _, tc := range []struct {
		name        string
		job         jobProbe
		tok         restrictedTokenProbe
		wantIso     bool
		wantKill    bool
		wantBackend string
	}{
		{"both work", workingJob, workingToken, true, true, jobPlusRestrictedBackend},
		{"token only", deadJob, workingToken, true, false, restrictedTokenBackend},
		{"job only", workingJob, deadToken, false, true, jobBackend},
		{"neither", deadJob, deadToken, false, false, jobBackendUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := windowsReport(cfg, tc.job, tc.tok)
			gotIso := rep.Effective == OSIsolated
			if gotIso != tc.wantIso || rep.Enforced != tc.wantIso {
				t.Errorf("Effective=%q Enforced=%t, want OSIsolated=%t on both",
					rep.Effective, rep.Enforced, tc.wantIso)
			}
			if rep.CanKillTree != tc.wantKill {
				t.Errorf("CanKillTree=%t, want %t", rep.CanKillTree, tc.wantKill)
			}
			if rep.Backend != tc.wantBackend {
				t.Errorf("Backend=%q, want %q", rep.Backend, tc.wantBackend)
			}
			if strings.TrimSpace(rep.Reason) == "" {
				t.Error("every report owes the operator a Reason")
			}
		})
	}
}

// TestWindowsBackendNamesCarryTheClassifierToken keeps the two halves of a
// coupling that has no compiler behind it.
//
// tools.BackendKindFor matches these strings by SUBSTRING to pick which
// diagnostic table a failed command's output is searched against. Renaming a
// backend to something without "restrictedtoken" or "jobobject" in it does not
// break any build; it silently routes Windows denials to the union matcher,
// which matches the POSIX and Seatbelt shapes too and over-reports.
func TestWindowsBackendNamesCarryTheClassifierToken(t *testing.T) {
	for _, name := range []string{
		restrictedTokenBackend, jobPlusRestrictedBackend, jobBackend, jobBackendUnavailable,
	} {
		if !strings.Contains(name, "restrictedtoken") && !strings.Contains(name, "jobobject") {
			t.Errorf("backend name %q carries neither token tools.BackendKindFor looks "+
				"for, so a denial under it is matched against every backend's table", name)
		}
	}
}

// TestWindowsRestrictedDeclarationStopsAtTheWriteBoundary is the W-B-13 field
// table for this backend.
//
// The declaration is a positive list subtracted from what the operator asked
// for, so naming a field here that the backend does not enforce silently
// REMOVES a warning. Both halves are asserted because they fail in opposite
// directions: tier and workspace_root must be in it (or a confining sandbox
// warns about the thing it is enforcing, and the warning list stops being read)
// and network_deny must not be (or `network_deny: true` reads as honoured on a
// backend that installs no WFP filter at all).
func TestWindowsRestrictedDeclarationStopsAtTheWriteBoundary(t *testing.T) {
	got := windowsRestrictedEnforcedFields()
	want := []string{FieldTier, FieldWorkspaceRoot}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enforcement declaration = %v, want %v", got, want)
	}
	for _, never := range []string{FieldNetworkDeny, FieldProxyURL} {
		if slices.Contains(got, never) {
			t.Errorf("the declaration claims %q, but nothing on this backend carries it: "+
				"WFP is not installed and the managed proxy is env-var level", never)
		}
	}

	// End to end through the report: an operator who configured network denial
	// on a fully working host must still see it listed as unenforced.
	cfg := Config{Enabled: true, Tier: WorkspaceWrite, WorkspaceRoot: `C:\w`,
		NetworkDeny: true, ProxyURL: "http://127.0.0.1:1"}
	rep := windowsReport(cfg, workingJob, workingToken)
	if !slices.Contains(rep.Unenforced, FieldNetworkDeny) {
		t.Errorf("a confining Windows sandbox did not warn about network_deny: %v", rep.Unenforced)
	}
	if slices.Contains(rep.Unenforced, FieldTier) {
		t.Errorf("a confining Windows sandbox warned about the tier it enforces: %v", rep.Unenforced)
	}
}

// TestWindowsConfiningReasonNamesTheControlsItDoesNotApply is the operator-facing
// half of the same honesty.
//
// Unenforced is machine-readable and Reason is what bootstrap logs and doctor
// prints. An operator reading "os-isolated" on Windows will reasonably conclude
// the sandbox is doing everything it was configured to; the two controls W-B-25
// names and this backend cannot deliver have to be in that line, because there
// is nowhere else they would look.
func TestWindowsConfiningReasonNamesTheControlsItDoesNotApply(t *testing.T) {
	cfg := Config{Enabled: true, Tier: WorkspaceWrite, WorkspaceRoot: `C:\w`, NetworkDeny: true}
	reason := windowsReport(cfg, workingJob, workingToken).Reason
	for _, must := range []string{"desktop", "WFP", "raw socket"} {
		if !strings.Contains(reason, must) {
			t.Errorf("the Reason an operator reads does not mention %q, so a control "+
				"W-B-25 asks for is missing with no disclosure: %q", must, reason)
		}
	}
	// ...and it must still say what it DOES do, in terms the operator configured,
	// plus the two holes in it. READS being unrestricted and Everyone-writable
	// objects staying writable are both real, and an operator reading
	// "os-isolated" would otherwise assume neither.
	for _, must := range []string{"WRITE_RESTRICTED", "workspace-write", "READS", "Everyone"} {
		if !strings.Contains(reason, must) {
			t.Errorf("the Reason does not state the boundary that IS in force (%q): %q", must, reason)
		}
	}
}

// TestWindowsScratchPathsFromDeduplicatesAndSkipsMissing pins the list that
// keeps a confined child able to run a compiler.
//
// It is tested through the injectable core because no darwin or linux host has
// a %TEMP%, so driving windowsScratchPaths directly would assert only that the
// running machine has none of these — which is how a scratch list ends up
// silently empty on the one platform it was written for.
//
// The de-duplication is the part that matters rather than tidiness: %TEMP% and
// %LOCALAPPDATA%\Temp are the same directory on a default Windows install, and
// each surviving spelling becomes a second ACE written onto one object and a
// second entry Close has to revoke.
func TestWindowsScratchPathsFromDeduplicatesAndSkipsMissing(t *testing.T) {
	env := map[string]string{
		"TEMP":         `C:\Users\x\AppData\Local\Temp`,
		"TMP":          `C:\Users\x\AppData\Local\TEMP\`,
		"LOCALAPPDATA": `C:\Users\x\AppData\Local`,
	}
	got := windowsScratchPathsFrom(
		func(k string) string { return env[k] },
		func(string) bool { return true },
	)
	if len(got) != 1 {
		t.Fatalf("three spellings of one directory produced %d entries: %v", len(got), got)
	}

	// A path that does not exist is dropped: SetNamedSecurityInfo on it would
	// fail and take the whole construction down to degraded.
	got = windowsScratchPathsFrom(
		func(k string) string { return env[k] },
		// Matched through denyReadKey rather than a literal suffix: the three
		// spellings above differ in case and in a trailing separator, and a
		// literal comparison would leave one of them "existing" and make the
		// assertion below pass for the wrong reason.
		func(p string) bool { return !strings.HasSuffix(denyReadKey(p), "local/temp") },
	)
	if len(got) != 0 {
		t.Fatalf("a scratch path that does not exist was kept: %v", got)
	}

	// An environment with none of the variables set yields nothing rather than
	// an entry for "".
	if got := windowsScratchPathsFrom(func(string) string { return "" },
		func(string) bool { return true }); len(got) != 0 {
		t.Fatalf("an empty environment produced scratch paths: %v", got)
	}
}
