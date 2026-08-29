package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// This file holds the platform-neutral half of the Windows restricted-token
// backend: the capability SID it runs under, the probe RESULT type, the pure
// planning of which paths receive a deny-read ACE, and the honesty decision
// that turns an observation into a CapabilityReport.
//
// It carries no build tag for the same reason jobobject.go and sbpl.go do not.
// Deciding WHICH paths to deny and WHAT the report may claim is arithmetic over
// strings; only the ACL and token calls need Windows. A pure function that
// compiles on one leg of the matrix is a pure function nobody executes on a
// developer's machine, and every defect this package has shipped has been in
// the deciding rather than in the calling.
//
// restrictedtoken_windows.go does the Win32 work and calls in here for the
// verdict.

// sandboxCapabilitySID is the principal every yanshi-sandboxed child runs as,
// in addition to the operator's own identity.
//
// # What it is used for
//
// The Windows restricted token is created with WRITE_RESTRICTED and this SID in
// its restricting list. Under WRITE_RESTRICTED, a WRITE access check succeeds
// only if the object's DACL grants the access to the normal token AND to one of
// the restricting SIDs; READ checks use the normal token alone. So a child
// holding this token writes the objects whose DACL names this SID, and reads
// stay unrestricted.
//
// It is not the ONLY restricting SID — the logon SID and Everyone are in the
// list too, because without them the child cannot open a window station or the
// null device and never starts. createRestrictedToken states what that costs.
//
// # Why a hard-coded capability SID rather than a generated one
//
// It has to be stable, because ACEs naming it are written onto real
// directories: a per-run SID would leave one dead ACE per run, and a DACL has a
// 64KB ceiling. It has to be collision-free, because a SID that resolved to a
// real account would grant that account write access to the workspace — which
// rules out the S-1-5-21-<random> shape a domain SID also occupies. S-1-15-3 is
// the app-capability authority: nothing in it is ever an account, so a fixed
// value there cannot be mistaken for a principal that can log in. The 1024
// prefix is the sub-range Windows itself uses for name-derived capabilities,
// and the eight words after it are a fixed 256-bit constant chosen once.
//
// The consequence of being fixed rather than per-install is that all yanshi
// sandboxes on one machine share this principal, so two concurrently sandboxed
// workspaces can write into each other's roots. That is the deliberate ceiling.
//
// ponytail: one machine-wide capability SID; derive a per-workspace SID from
// the workspace path if cross-workspace isolation is ever asked for.
const sandboxCapabilitySID = "S-1-15-3-1024-3216531420-1721937680-2843155174-" +
	"1176509923-3005430571-2470103524-1094058073-2938516349"

// restrictedTokenBackend names the two shapes this file can produce, stamped
// into CapabilityReport.Backend.
//
// tools.BackendKindFor already matches "restrictedtoken" by SUBSTRING, so both
// spellings land on the Windows diagnostic table. Renaming these to anything
// still containing that token keeps working; dropping it silently routes every
// Windows access denial to the union matcher.
const (
	restrictedTokenBackend      = "windows-restrictedtoken"
	jobPlusRestrictedBackend    = "windows-jobobject+restrictedtoken"
	restrictedTokenNotAttempted = "not attempted: the configured tier asks for no write restriction"
)

// restrictedTokenProbe records what the Windows restricted-token self-check
// actually OBSERVED, as opposed to which API calls returned success.
//
// The fields are separate rather than one boolean for the same reason jobProbe's
// are: they fail for different reasons and only one of them can catch the
// failure that matters. TokenCreated and ACLsApplied say the setup worked;
// WorkspaceWritable and OutsideDenied say the kernel actually behaved the way
// the setup was supposed to make it behave.
//
// Both observations are required, and requiring only one is the classic error
// in either direction: a token that denies everything passes OutsideDenied
// while making the sandbox useless (every command fails, and the operator
// reads it as yanshi being broken), and a token that denies nothing passes
// WorkspaceWritable while enforcing nothing at all.
type restrictedTokenProbe struct {
	// Attempted is false when the configuration asked for no write restriction,
	// which is not a failure and must not be reported as one.
	Attempted bool
	// TokenCreated reports that CreateRestrictedToken returned a usable primary
	// token restricted to sandboxCapabilitySID.
	TokenCreated bool
	// ACLsApplied reports that the workspace grant and every deny-read ACE were
	// written successfully.
	ACLsApplied bool
	// WorkspaceWritable reports that a real child holding the token wrote a file
	// under the granted root.
	WorkspaceWritable bool
	// OutsideDenied reports that the same child could NOT write outside it.
	OutsideDenied bool
	// Detail is the operator-facing explanation of whichever step failed, empty
	// when nothing did.
	Detail string
}

// enforcing reports whether this host really does confine writes.
//
// All four are ANDed rather than trusting the observations alone: a probe that
// reported OutsideDenied without having created a token is a bug in the probe,
// and the permissive reading of a self-inconsistent probe is exactly the one
// that must not win.
func (p restrictedTokenProbe) enforcing() bool {
	return p.Attempted && p.TokenCreated && p.ACLsApplied &&
		p.WorkspaceWritable && p.OutsideDenied
}

// restrictedTokenDetail returns a non-empty explanation for a probe that is not
// enforcing.
//
// Every non-enforcing adapter in this package owes callers a Reason and
// types_test.go's TestAdapterReportIsSelfConsistent fails an empty one, so the
// fallback names the first step that did not hold rather than producing
// "unavailable ()".
func restrictedTokenDetail(p restrictedTokenProbe) string {
	if strings.TrimSpace(p.Detail) != "" {
		return strings.TrimSpace(p.Detail)
	}
	switch {
	case !p.Attempted:
		return restrictedTokenNotAttempted
	case !p.TokenCreated:
		return "CreateRestrictedToken did not yield a usable primary token"
	case !p.ACLsApplied:
		return "the workspace grant or a deny-read ACE could not be written"
	case !p.WorkspaceWritable:
		return "a child holding the restricted token could not write inside its own " +
			"workspace, so the token would break every command rather than confine it"
	default:
		return "a child holding the restricted token wrote outside its workspace; " +
			"WRITE_RESTRICTED is not confining on this host"
	}
}

// denyReadKey normalises a path for comparison and de-duplication.
//
// Windows paths are case-insensitive and accept either separator, so the same
// directory arrives here spelled several ways: %TEMP% and %LOCALAPPDATA%\Temp
// are usually the same object in different cases, and either may or may not
// carry a trailing separator. Two spellings must collapse to one, because each
// distinct spelling that survives becomes a second ACE written onto the same
// object and a second entry to revoke on Close.
//
// The \\?\ prefix is stripped rather than preserved: it is a namespace escape
// that changes no part of the identity of the object.
func denyReadKey(p string) string {
	s := strings.ReplaceAll(strings.TrimSpace(p), `\`, "/")
	s = strings.TrimPrefix(s, "//?/UNC/")
	s = strings.TrimPrefix(s, "//?/")
	s = strings.TrimRight(s, "/")
	return strings.ToLower(s)
}

// windowsScratchPaths lists the directories a confined child must still be able
// to write for ordinary commands to work.
//
// The Windows counterpart to BwrapScratchPaths, and it exists for the same
// reason: a workspace-write sandbox that cannot write TEMP breaks the compiler,
// the package manager and git, and the failure looks like a broken toolchain
// rather than a policy. The linux backend grants its scratch paths at
// WorkspaceWrite and not at ReadOnly, and this follows that exactly.
//
// Granting the capability SID on the user's temp directory is not a widening:
// the directory is already writable by the operator, and the ACE only names a
// principal that no process other than a yanshi sandbox child ever holds.
func windowsScratchPaths() []string {
	return windowsScratchPathsFrom(os.Getenv, func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && st.IsDir()
	})
}

// windowsScratchPathsFrom is the injectable core of windowsScratchPaths.
//
// Split out for the same reason bwrapScratchPathsFrom is: no darwin or linux
// host has a %TEMP%, so testing the exported behaviour directly would only ever
// assert that the running machine has none of these — which is how a scratch
// list ends up silently empty on the one platform it was written for.
func windowsScratchPathsFrom(getenv func(string) string, isDir func(string) bool) []string {
	candidates := []string{getenv("TEMP"), getenv("TMP")}
	if local := getenv("LOCALAPPDATA"); local != "" {
		candidates = append(candidates, filepath.Join(local, "Temp"))
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		key := denyReadKey(c)
		if key == "" || seen[key] || !isDir(c) {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

// windowsRestrictedEnforcedFields is the restricted-token backend's enforcement
// declaration (W-B-13).
//
// The token confines WRITES, so tier and workspace_root are genuinely enforced
// by the OS. network_deny and proxy_url are NOT, and the reason is
// windowsStructuralGapNote: the mechanism that would carry them is WFP, which
// this backend does not install.
//
// It takes no argument and returns a constant list on purpose. The one field
// whose enforcement is conditional here is conditional on the PROBE, not on
// configuration, and the caller only reaches this function when the probe
// passed — a variant that took a flag would invite a caller to pass true on the
// degraded path, which is how a warning list silently loses an entry.
func windowsRestrictedEnforcedFields() []string {
	return []string{FieldTier, FieldWorkspaceRoot}
}

// windowsStructuralGapNote names the two controls W-B-25 asks for that this
// backend does not deliver, and says why each is absent.
//
// This is disclosure, not an apology, and it is in the Reason rather than only
// in a design document because the Reason is what bootstrap logs and what the
// doctor row prints. An operator who read "restricted token" in a release note
// and configured network_deny needs to learn here, once, that the network half
// is not carried — rather than from a request that went through.
//
// Both gaps are structural rather than unfinished:
//
//   - A separate desktop needs lpDesktop in STARTUPINFO. Go's
//     syscall.SysProcAttr on Windows exposes CreationFlags, Token, the two
//     SecurityAttributes, AdditionalInheritedHandles and ParentProcess, and the
//     runtime fills the STARTUPINFO itself. There is no seam, so delivering it
//     means a whole new spawn backend — the same blocker documented for
//     AppContainer in sandbox_windows.go, and for the same reason. Its value
//     here is also small: it defends against window-message injection, and the
//     children this sandbox launches are console processes with no windows.
//
//   - WFP filters need FwpmEngineOpen0 with administrative rights. yanshi runs
//     unelevated, and the reference implementation that does install them ships
//     a separate elevated setup binary with a requireAdministrator manifest to
//     do it. Egress control on this platform therefore stays with the managed
//     proxy in internal/netpolicy, which is env-var level and can be bypassed
//     by a program that opens a raw socket.
func windowsStructuralGapNote() string {
	return " — NOT applied by this backend: a separate desktop (Go's " +
		"syscall.SysProcAttr exposes no lpDesktop, so it needs a new spawn " +
		"backend rather than a sandbox adapter) and WFP network filtering " +
		"(FwpmEngineOpen0 requires administrative rights yanshi does not run " +
		"with); network egress is the managed proxy's job and a raw socket " +
		"still escapes it"
}

// windowsJobReport is the report for a host where the restricted token was not
// used, which is every FullAccess configuration and every host where the token
// probe declined.
//
// Kept as its own name because it is the shape the Windows backend has had
// since it was written and the tests that pin its honesty are written against
// it. It is windowsReport with the second axis switched off.
func windowsJobReport(cfg Config, p jobProbe) CapabilityReport {
	return windowsReport(cfg, p, restrictedTokenProbe{})
}

// windowsReport is the honesty decision for the Windows backend across both of
// its mechanisms.
//
// # Why the two probes are independent inputs
//
// They answer different questions and either can hold without the other. A job
// object caps LIFETIME and refuses no access; a restricted token confines
// ACCESS and reaps nothing. Collapsing them into one boolean would force a lie
// in one direction or the other, which is the same argument that already keeps
// CanKillTree separate from Enforced.
//
// # Why Effective becomes OSIsolated only with the token
//
// EffectiveMode's one programmatic consumer is tools.SandboxEnforcing, which
// ANDs it with Enforced to decide whether a failed command's stderr may be read
// as an ACCESS REFUSAL. Under a job object alone that reading is wrong in a way
// that costs the operator real money: on Windows "Access is denied" is the
// ordinary spelling of "another process holds this file" and "you are not an
// administrator", and every false positive becomes a prompt asking to approve a
// higher tier that cannot fix the problem. With a confining restricted token in
// place the same string genuinely can be the sandbox talking, which is the
// state the classifier was written for — so this is the one condition under
// which claiming OSIsolated on Windows stops being an over-claim.
func windowsReport(cfg Config, jp jobProbe, tp restrictedTokenProbe) CapabilityReport {
	confining, contained := tp.enforcing(), jp.enforcing()
	rep := CapabilityReport{
		// Hard-coded rather than runtime.GOOS so this function produces the same
		// answer when a test on any workstation drives it. A report that only
		// says "windows" on Windows is a report no one can test.
		Platform:    "windows",
		Requested:   cfg.Tier,
		Effective:   DegradedHostGuard,
		Enforced:    false,
		CanKillTree: contained,
		Unenforced:  UnenforcedFields(cfg),
	}
	if confining {
		rep.Effective = OSIsolated
		rep.Enforced = true
		rep.Unenforced = UnenforcedFields(cfg, windowsRestrictedEnforcedFields()...)
	}

	switch {
	case confining && contained:
		rep.Backend = jobPlusRestrictedBackend
	case confining:
		rep.Backend = restrictedTokenBackend
	case contained:
		rep.Backend = jobBackend
	default:
		rep.Backend = jobBackendUnavailable
	}
	rep.Reason = windowsReasonFor(cfg, jp, tp)
	return rep
}

// windowsReasonFor renders the operator-facing explanation.
//
// Split out so windowsReport reads as the decision it is, and so the four
// combinations of (contained, confining) each have one place that produces
// their prose rather than a chain of conditionals inside the struct literal.
func windowsReasonFor(cfg Config, jp jobProbe, tp restrictedTokenProbe) string {
	var b strings.Builder
	if jp.enforcing() {
		b.WriteString("Job Object containment (kill-on-job-close): every child this " +
			"sandbox prepares is bound to one job, so Close — and this process " +
			"exiting or crashing — terminates the entire descendant tree, which " +
			"Windows cannot otherwise guarantee")
	} else {
		fmt.Fprintf(&b, "Job Object containment unavailable (%s), so this backend "+
			"terminates no process tree", jobProbeDetail(jp))
	}
	if !tp.enforcing() {
		// The wording an operator has read since this backend shipped. A job
		// object is a lifetime control and says so; without the token there is
		// nothing here that refuses access, and "job object active" reads as
		// containment to anyone who has not read this package.
		fmt.Fprintf(&b, ". This is a LIFETIME control, not an access control: no "+
			"filesystem and no network isolation is applied (%s), and the host guard "+
			"remains the only thing deciding what a child may touch",
			restrictedTokenDetail(tp))
		b.WriteString(windowsUnenforcedNote(cfg))
		return b.String()
	}
	b.WriteString(". Writes are confined by a WRITE_RESTRICTED token to the " +
		"workspace root and the scratch directories (tier=" + cfg.Tier.String() +
		"). READS are NOT restricted — a WRITE_RESTRICTED token cannot carry a " +
		"deny-read — and any object whose ACL grants Everyone write access is " +
		"still writable, because Everyone has to be in the restricting list for " +
		"the child to reach the null device and its own pipes")
	b.WriteString(windowsStructuralGapNote())
	return b.String()
}
