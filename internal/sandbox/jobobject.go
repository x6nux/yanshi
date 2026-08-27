package sandbox

import (
	"fmt"
	"strings"
)

// This file holds the platform-neutral half of the Windows Job Object backend:
// the probe RESULT type and the pure function that turns one into a
// CapabilityReport.
//
// It carries no build tag on purpose, for the same reason sbpl.go does not: the
// decision "given what the self-check observed, what may this backend honestly
// claim?" is a pure function, and a pure function that only compiles on Windows
// is a pure function nobody on a macOS or Linux workstation ever executes. The
// enforcement mechanism needs Windows; deciding what to SAY about it does not,
// and saying the wrong thing is the failure mode this whole package exists to
// prevent.
//
// Nothing here imports golang.org/x/sys/windows, touches a handle, or spawns a
// process. sandbox_windows.go does all of that and then calls in here for the
// verdict.

// Backend identifiers this file stamps into CapabilityReport.Backend.
//
// tools.BackendKindFor matches these by SUBSTRING ("jobobject" is one of the
// tokens it looks for), so the two halves stay coupled through a token rather
// than through an exact string — renaming this to e.g. "windows-jobobject-v2"
// would keep working, renaming it to "winjob" would not.
const (
	jobBackend            = "windows-jobobject"
	jobBackendUnavailable = "windows-jobobject-unavailable"
)

// jobProbe records what the Windows self-check actually OBSERVED, as opposed to
// which API calls returned success.
//
// The three booleans are deliberately separate rather than one "ok" flag,
// because they fail for different reasons and an operator reading the degraded
// Reason needs to know which one: CreateJobObject failing means the host
// refused a job outright (rare, usually a policy or handle-quota problem);
// SetInformationJobObject failing means the limit structure was rejected; and
// KillOnCloseObserved failing means both API calls SUCCEEDED and the kernel
// still did not kill the child — the Windows equivalent of macOS's
// "sandbox-exec ran a program under (deny default)". That last one is the only
// field that can catch a job object which exists but does not contain, and it
// is the reason the probe spawns a real process instead of checking return
// codes.
type jobProbe struct {
	// Created reports that CreateJobObject returned a usable handle.
	Created bool
	// LimitsApplied reports that SetInformationJobObject accepted
	// JOBOBJECT_EXTENDED_LIMIT_INFORMATION with KILL_ON_JOB_CLOSE.
	LimitsApplied bool
	// KillOnCloseObserved reports that a real child, verified alive after
	// assignment, actually died when the job handle was closed.
	KillOnCloseObserved bool
	// Detail is the operator-facing explanation of whichever step failed. It
	// is empty when nothing failed.
	Detail string
}

// enforcing reports whether all three observations held, i.e. whether this host
// really does give us a containing job object.
//
// All three are ANDed rather than trusting the last one alone: a probe that
// somehow reported KillOnCloseObserved without having created a job is a bug in
// the probe, and the permissive reading of a self-inconsistent probe is exactly
// the reading that must not win.
func (p jobProbe) enforcing() bool {
	return p.Created && p.LimitsApplied && p.KillOnCloseObserved
}

// windowsJobReport is the honesty decision for the Windows backend.
//
// # Why Effective is DegradedHostGuard even when the job object works
//
// This is the load-bearing judgement in the whole file, and it goes against the
// intuition that "a working OS mechanism means os-isolated".
//
// EffectiveMode is not read as "is some OS mechanism active". Its one
// programmatic consumer is tools.SandboxEnforcing, which ANDs Effective ==
// OSIsolated with Enforced and uses the answer to decide whether a failed
// command's stderr may be interpreted as an ACCESS REFUSAL by the sandbox. A
// job object refuses no access whatsoever: it caps lifetime and resources, and
// a child inside one reads and writes exactly what the parent's token allows.
//
// So claiming OSIsolated here would arm tools.ClassifySandboxViolation's
// Windows pattern table — which matches "Access is denied", "error 5",
// "Permission denied" and 拒绝访问 — against every failing command on the host.
// On Windows those strings are the ordinary spelling of "another process holds
// this file", "this directory is read-only", "you are not an administrator".
// Each false positive turns into an escalation prompt asking the operator to
// approve a higher access tier that cannot possibly fix the problem, because
// nothing denied it in the first place. That is worse than reporting nothing:
// it trains the operator to approve escalations reflexively.
//
// The mirror error is real too and is why this does not simply report Disabled:
// a job object IS doing something, and Backend/Reason/CanKillTree say what.
// DegradedHostGuard is the accurate statement — "no OS-level ACCESS isolation,
// the host guard is the boundary" — and CanKillTree carries the one guarantee
// this backend genuinely adds.
//
// # Why CanKillTree may be true while Enforced is false
//
// They answer different questions. Enforced is about access control; CanKillTree
// is about whether the OS reaps descendants. Windows has no process groups and
// no cgroups: without a job object, killing a shell leaves its grandchildren
// running with no way to find them. With one, closing the handle kills every
// process in it, transitively, including ones that already double-forked.
// Those two facts are independent, and collapsing them into a single flag is
// what would force a lie in one direction or the other.
func windowsJobReport(cfg Config, p jobProbe) CapabilityReport {
	rep := CapabilityReport{
		// Hard-coded rather than runtime.GOOS so this function produces the
		// same answer when a test on any workstation drives it. A report that
		// only says "windows" on Windows is a report no one can test.
		Platform:  "windows",
		Requested: cfg.Tier,
		Effective: DegradedHostGuard,
		Enforced:  false,
	}
	if !p.enforcing() {
		rep.Backend = jobBackendUnavailable
		rep.CanKillTree = false
		rep.Reason = fmt.Sprintf(
			"Job Object containment unavailable (%s); this backend applies no "+
				"filesystem, network or process-tree control at all and the host "+
				"guard is the only boundary", jobProbeDetail(p))
		return rep
	}
	rep.Backend = jobBackend
	rep.CanKillTree = true
	rep.Reason = "Job Object containment (kill-on-job-close): every child this " +
		"sandbox prepares is bound to one job, so Close — and this process " +
		"exiting or crashing — terminates the entire descendant tree, which " +
		"Windows cannot otherwise guarantee. This is a LIFETIME control, not an " +
		"access control: no filesystem and no network isolation is applied, and " +
		"the host guard remains the only thing deciding what a child may touch" +
		windowsUnenforcedNote(cfg)
	return rep
}

// jobProbeDetail returns a non-empty explanation for a failed probe.
//
// The fallback matters rather than being defensive noise: every non-enforcing
// adapter in this package owes callers a Reason, types_test.go's
// TestAdapterReportIsSelfConsistent fails an empty one, and a probe that
// returned false without filling Detail would otherwise produce a report that
// says "unavailable ()" — technically honest, operationally useless.
func jobProbeDetail(p jobProbe) string {
	if strings.TrimSpace(p.Detail) != "" {
		return strings.TrimSpace(p.Detail)
	}
	switch {
	case !p.Created:
		return "CreateJobObject did not yield a usable job"
	case !p.LimitsApplied:
		return "the job rejected JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE"
	default:
		return "a child assigned to the job survived closing the job handle"
	}
}

// windowsUnenforcedNote discloses the configuration the operator asked for and
// this backend does not deliver.
//
// This is the Windows counterpart to the darwin backend's vacuity note, and it
// is the inverse shape. On macOS the profile usually restricts something and the
// note fires only in the one configuration where it does not; here the backend
// NEVER restricts access, so the note fires whenever the operator asked for a
// restriction at all. A note that appeared unconditionally would be noise
// operators learn to skip, so tier=full-access with no network denial — the
// configuration that asked for nothing and got nothing — gets no note.
//
// The tier is spelled out rather than abbreviated because this string is what
// bootstrap logs and what the doctor row prints, and "tier not enforced" without
// naming the tier leaves the reader to go look up what they configured.
func windowsUnenforcedNote(cfg Config) string {
	var unmet []string
	if cfg.Tier != FullAccess {
		unmet = append(unmet, "tier="+cfg.Tier.String())
	}
	if cfg.NetworkDeny {
		unmet = append(unmet, "network-deny")
	}
	if len(unmet) == 0 {
		return ""
	}
	return " — NOTE: the following is configured but NOT enforced by this " +
		"backend: " + strings.Join(unmet, ", ")
}
