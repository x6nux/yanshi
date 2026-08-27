// internal/tools/sandboxviolation.go
//
// S12, half one: telling "the sandbox refused this" apart from "the command
// failed on its own".
//
// Without this distinction a sandboxed denial reaches the model as an ordinary
// non-zero exit with a line of stderr, and the model's only recourse is to
// guess: it retries the same command, edits the path, or gives up. None of
// those is the right move, because the right move — ask the operator for a
// higher access tier — is not something the model can even express.
//
// # Why the detection is stderr scraping, and what that costs
//
// None of the four OS backends reports a structured refusal to the parent.
// Seatbelt writes a kernel diagnostic to the child's stderr; bubblewrap writes
// its own `bwrap:` line or lets the child report EACCES; Landlock produces a
// plain EACCES/EPERM indistinguishable at the syscall layer from a chmod 000
// file; Windows AppContainer produces "Access is denied" / error 5. So the
// signal is text, and text is a heuristic. The patterns below are lifted from
// reference/QwenPaw's four backends rather than invented here, because the
// shapes they match are the ones that were actually observed in the wild
// (`deny(1) file-read-data`, `^bwrap: `, `0x80070005`), not the ones that read
// plausibly.
//
// The heuristic's failure mode is a FALSE POSITIVE: an ordinary program that
// prints "Permission denied" for its own reasons (a chmod 000 file the user
// created, a git hook, an npm cache) would be read as a sandbox refusal and
// would trigger an escalation prompt for a tier increase that fixes nothing.
//
// # The guard against that is CapabilityReport, not a better regex
//
// classify() refuses to report a violation unless the sandbox says it is
// actually enforcing something (Effective == OSIsolated && Enforced). On a
// host with no OS backend — every Phase 0 deployment, every unit test, every
// Windows box without AppContainer — the answer is a flat "not a sandbox
// violation" no matter what the text says, because in that configuration the
// sandbox provably did not deny anything: it did not run.
//
// That is the one check that cannot be fooled by output, and it is why this
// file reads the report instead of only the streams. A report that lies (says
// OSIsolated while enforcing nothing) breaks it — which is exactly why the
// sandbox package is required to degrade honestly.
package tools

import (
	"regexp"
	"runtime"
	"strings"

	"github.com/x6nux/yanshi/internal/sandbox"
)

// SandboxBackendKind names the isolation mechanism a violation was attributed
// to. It is derived from CapabilityReport.Backend (with the platform as a
// fallback) rather than taken from it verbatim, so the pattern table stays
// keyed by mechanism even as the sandbox package renames its backend strings.
type SandboxBackendKind string

// The four mechanisms yanshi can run under, plus the honest "we could not tell"
// value. Unknown still gets the union of every pattern set: mis-attributing a
// real refusal to the wrong backend is harmless (the escalation is identical),
// while missing it entirely is the failure this file exists to prevent.
const (
	BackendSeatbelt     SandboxBackendKind = "seatbelt"
	BackendBubblewrap   SandboxBackendKind = "bubblewrap"
	BackendLandlock     SandboxBackendKind = "landlock"
	BackendAppContainer SandboxBackendKind = "appcontainer"
	BackendUnknown      SandboxBackendKind = "unknown"
)

// SandboxViolation describes one access the OS sandbox refused.
//
// Resource is BEST EFFORT and frequently empty: only some diagnostics name the
// path they refused. An empty Resource is reported as such rather than being
// filled with a guess, because the string is shown to the operator in the
// escalation prompt and a wrong path there is worse than no path — it invites
// approval of an access nobody actually requested.
type SandboxViolation struct {
	// Backend is the mechanism credited with the refusal.
	Backend SandboxBackendKind
	// Tier is the access tier the refused attempt ran under.
	Tier sandbox.AccessTier
	// Evidence is the matched diagnostic text, truncated. It is the raw
	// justification an operator reads before approving anything.
	Evidence string
	// Resource is the path or object the diagnostic named, or "" when it
	// named none.
	Resource string
}

// maxViolationEvidence bounds the diagnostic text carried out of this package.
// A sandboxed build that fails on every file can produce megabytes of
// "Permission denied"; the escalation prompt needs a sample, not a corpus.
const maxViolationEvidence = 1024

// violationPatterns maps a backend to the diagnostic shapes it produces.
//
// Ported from reference/QwenPaw: macos_sandbox._SEATBELT_VIOLATION_RE,
// bubblewrap_sandbox._BWRAP_VIOLATION_RE, linux_sandbox's inline substring set
// and windows_unelevated_sandbox._WC.VIOLATION_RE. The Windows set keeps the
// Chinese-locale alternatives, which matter for exactly the reason the repo's
// own removal_test.go regex keeps them: a localized host is where an
// English-only matcher fails silently.
//
// ONE DELIBERATE DIVERGENCE FROM THE REFERENCE, and it was found by a test
// rather than by reading. QwenPaw's Seatbelt pattern deliberately EXCLUDES the
// generic "Permission denied" / EACCES family, on the stated grounds that
// matching it is "far too lossy". That reasoning is sound for QwenPaw, which
// has no other filter — but it makes the Seatbelt matcher miss the most common
// real refusal there is. A Seatbelt `file-read-data` denial reaches the child
// as a plain EACCES; the kernel's own `deny(1)` line goes to the system log,
// not to the child's stderr. So all the child prints is `cat: /x: Permission
// denied`, which QwenPaw's macOS regex does not match.
// TestRealProcessDenialIsClassified spawns a real denied `cat` on darwin and
// caught this on the first run.
//
// Including it here is safe in a way it is not there, because
// ClassifySandboxViolation gates every match behind SandboxEnforcing: on a
// host with no OS isolation the answer is "not a violation" regardless of the
// text, so the lossy pattern can only fire in a configuration where the
// sandbox really is denying things. That gate is the thing QwenPaw lacks, and
// it is what buys back the precision the broader pattern spends.
//
// Anchored alternatives use (?m) so `^bwrap:` matches at the start of any line
// rather than only the first, which is how these diagnostics actually arrive
// (interleaved with the child's own output).
var violationPatterns = map[SandboxBackendKind]*regexp.Regexp{
	BackendSeatbelt: regexp.MustCompile(`(?im)\bdeny\(\d+\)` +
		`|^Sandbox:\s` +
		`|^sandbox-exec:\s` +
		`|\bOperation not permitted\b` +
		`|\bPermission denied\b` +
		`|\bEACCES\b`),
	BackendBubblewrap: regexp.MustCompile(`(?im)\bPermission denied\b` +
		`|^bwrap:\s` +
		`|\bOperation not permitted\b` +
		`|\bEACCES\b`),
	BackendLandlock: regexp.MustCompile(`(?im)\bPermission denied\b` +
		`|\bOperation not permitted\b` +
		`|\blandlock\b` +
		`|\bEACCES\b` +
		`|\bEPERM\b`),
	BackendAppContainer: regexp.MustCompile(`(?im)Access is denied` +
		`|\berror 5\b` +
		`|0x80070005` +
		`|\bPermission denied\b` +
		`|拒绝访问` +
		`|权限不足` +
		`|系统无法执行指定的程序`),
}

// resourcePatterns pull the refused path out of a diagnostic when it names
// one. Each has exactly one capture group.
//
// Ordered most-specific first: Seatbelt's own `deny(1) file-read-data /path`
// form is unambiguous, while the generic `prog: /path: Permission denied`
// shape is the POSIX convention every coreutils program follows and matches
// far more loosely.
var resourcePatterns = []*regexp.Regexp{
	// Seatbelt: `deny(1) file-write-data /Users/x/.ssh/id_rsa`
	regexp.MustCompile(`(?i)deny\(\d+\)\s+\S+\s+(\S+)`),
	// bubblewrap: `bwrap: Can't bind mount /nix on /nix: No such file`
	regexp.MustCompile(`(?im)^bwrap:[^\n]*?\s(/\S+)`),
	// POSIX: `cat: /etc/shadow: Permission denied`
	regexp.MustCompile(`(?im)^[^:\n]*:\s*(\S+):\s*(?:Permission denied|Operation not permitted)`),
	// Windows: `Access is denied` preceded by a path-ish token.
	regexp.MustCompile(`(?i)([A-Za-z]:\\[^\s"']+)[^\n]{0,40}Access is denied`),
}

// BackendKindFor maps a CapabilityReport onto the mechanism whose diagnostics
// should be matched.
//
// Backend is consulted first and by SUBSTRING, so a sandbox package that
// reports "seatbelt(sandbox-exec)" or "landlock+seccomp" still lands on the
// right table. The platform is the fallback, and an unrecognised platform
// yields BackendUnknown rather than a default guess — the union matcher then
// applies, so nothing is lost.
func BackendKindFor(rep sandbox.CapabilityReport) SandboxBackendKind {
	b := strings.ToLower(rep.Backend)
	switch {
	case strings.Contains(b, "seatbelt"), strings.Contains(b, "sandbox-exec"):
		return BackendSeatbelt
	case strings.Contains(b, "bwrap"), strings.Contains(b, "bubblewrap"):
		return BackendBubblewrap
	case strings.Contains(b, "landlock"), strings.Contains(b, "seccomp"):
		return BackendLandlock
	case strings.Contains(b, "appcontainer"), strings.Contains(b, "restrictedtoken"),
		strings.Contains(b, "jobobject"), strings.Contains(b, "job object"):
		return BackendAppContainer
	}
	platform := rep.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	switch strings.ToLower(platform) {
	case "darwin":
		return BackendSeatbelt
	case "linux":
		return BackendLandlock
	case "windows":
		return BackendAppContainer
	}
	return BackendUnknown
}

// SandboxEnforcing reports whether rep describes a sandbox that is actually
// denying things right now.
//
// Both fields are checked, not either: Effective is the operator-facing verdict
// and Enforced is the adapter's own claim, and a report where they disagree is
// a bug in that adapter which must not be resolved in favour of the permissive
// reading. A degraded or disabled sandbox denies nothing, so no output it
// produces can be a sandbox violation.
func SandboxEnforcing(rep sandbox.CapabilityReport) bool {
	return rep.Effective == sandbox.OSIsolated && rep.Enforced
}

// ClassifySandboxViolation decides whether a finished command was refused by
// the OS sandbox.
//
// The three conditions are ANDed and each one is load-bearing:
//
//  1. The sandbox must be enforcing (SandboxEnforcing). This is what keeps a
//     chmod 000 file on an unsandboxed host from being read as isolation.
//  2. The command must have failed. A refusal the program recovered from is
//     not something to escalate for — the command produced its answer.
//  3. The output must carry a diagnostic shape from the backend's table, or
//     from any table when the backend is unknown.
//
// stderr is searched before stdout because that is where every backend writes,
// and a match in stdout alone is accepted only for the Windows shapes, which
// routinely arrive on stdout from cmd.exe. Searching stdout unconditionally
// would flag any tool that merely PRINTS the words — a grep over a log, a test
// asserting a permission error — which is a large and boring false-positive
// class.
func ClassifySandboxViolation(rep sandbox.CapabilityReport, exitCode int,
	stdout, stderr string) (SandboxViolation, bool) {

	if !SandboxEnforcing(rep) {
		return SandboxViolation{}, false
	}
	if exitCode == 0 {
		return SandboxViolation{}, false
	}
	kind := BackendKindFor(rep)
	if m := matchViolation(kind, stderr); m != "" {
		return buildViolation(kind, rep, m, stderr), true
	}
	if kind == BackendAppContainer {
		if m := matchViolation(kind, stdout); m != "" {
			return buildViolation(kind, rep, m, stdout), true
		}
	}
	return SandboxViolation{}, false
}

// matchViolation returns the matched diagnostic substring, or "" for no match.
// An unknown backend is matched against every table, on the reasoning given at
// SandboxBackendKind: over-attribution is free, under-detection is not.
func matchViolation(kind SandboxBackendKind, text string) string {
	if text == "" {
		return ""
	}
	if re, ok := violationPatterns[kind]; ok {
		return re.FindString(text)
	}
	for _, re := range violationPatterns {
		if m := re.FindString(text); m != "" {
			return m
		}
	}
	return ""
}

// buildViolation assembles the reported violation from the matched diagnostic
// and the surrounding text.
//
// Evidence is the whole (truncated) stream rather than just the matched token,
// because "Permission denied" on its own tells an operator nothing about what
// was denied — the surrounding line is the part they have to read to decide.
// The bare match is kept only as the fallback when trimming leaves nothing.
func buildViolation(kind SandboxBackendKind, rep sandbox.CapabilityReport,
	match, text string) SandboxViolation {

	evidence := strings.TrimSpace(text)
	if evidence == "" {
		evidence = match
	}
	if len(evidence) > maxViolationEvidence {
		evidence = evidence[len(evidence)-maxViolationEvidence:]
	}
	return SandboxViolation{
		Backend:  kind,
		Tier:     rep.Requested,
		Evidence: evidence,
		Resource: extractViolationResource(text),
	}
}

// extractViolationResource pulls the refused path out of a diagnostic, or
// returns "" when none of the known shapes matches.
//
// Returning "" is a real answer, not a failure: the escalation prompt renders
// "an unnamed resource" in that case, which is honest, whereas substituting
// the working directory or the program name would put a specific and false
// claim in front of the person granting the privilege.
func extractViolationResource(text string) string {
	for _, re := range resourcePatterns {
		if m := re.FindStringSubmatch(text); len(m) > 1 {
			r := strings.Trim(m[1], `"'`)
			if r != "" && r != ":" {
				return r
			}
		}
	}
	return ""
}

// NextSandboxTier returns the next more permissive access tier, and false when
// there is none.
//
// This is the ONLY place the escalation ladder is written down. Escalating by
// one rung rather than jumping to FullAccess is deliberate: a command denied a
// write inside the workspace needs WorkspaceWrite, and granting it network and
// whole-filesystem access as well would make every escalation prompt a request
// for the maximum.
//
// FullAccess returning false is what makes "retry at most once, and only when
// the tier really rose" a property of the type rather than of the caller's
// discipline — a caller that loops on this cannot loop forever, and a caller
// that asks to escalate an already-maximal tier gets a refusal instead of a
// pointless re-run of the identical command.
func NextSandboxTier(t sandbox.AccessTier) (sandbox.AccessTier, bool) {
	switch t {
	case sandbox.ReadOnly:
		return sandbox.WorkspaceWrite, true
	case sandbox.WorkspaceWrite:
		return sandbox.FullAccess, true
	default:
		return t, false
	}
}
