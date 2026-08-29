//go:build darwin

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// This file is the macOS enforcement backend. It rewrites each prepared
// exec.Cmd into
//
//	/usr/bin/sandbox-exec -p <profile> -- <original program> <original args>
//
// so the kernel's Sandbox (TrustedBSD MAC) module applies the profile before
// the target program's first instruction. sandbox-exec execs the target in
// place — measured, the shell it launches reports the SAME pid as the
// sandbox-exec process — so this wrapping does not insert an extra process
// between yanshi and the child. That matters for the shell layer's kill-tree
// and for exit-status propagation, both of which were verified: `exit 42`
// comes back as 42 and a self-TERM comes back as 143.
//
// # sandbox-exec is deprecated, and what that does and does not mean
//
// `man 1 sandbox-exec` on macOS 15.7.7 (24G720) opens with "The sandbox-exec
// command is DEPRECATED" and points developers at App Sandbox instead. That
// deprecation notice has been in the manual page since OS X 10.8 and the
// binary is still present and still functional on the current release — it is
// what Chromium, Bazel and every other tool in this space uses, because Apple
// ships no supported replacement for the "sandbox an arbitrary child process I
// did not sign" use case. App Sandbox is not that: it requires the sandboxed
// program to be code-signed with an entitlement, which cannot apply to
// `go build` or a shell one-liner.
//
// So the honest reading is: this is a supported-in-practice, unsupported-on-
// paper mechanism. The concrete signals that it has actually gone away, each
// of which this backend detects at construction time and degrades on rather
// than assuming:
//
//   - /usr/bin/sandbox-exec is absent (Apple finally removed the binary).
//   - The self-check profile is rejected (the SBPL dialect changed).
//   - A default-deny profile no longer denies (the kernel module was removed
//     and the shim became a pass-through).
//
// Because the check runs a real sandbox-exec against a real profile rather
// than testing for the file's existence, a future macOS that keeps the binary
// as a no-op stub is caught by the enforcement probe below rather than
// mistaken for a working sandbox. That is why the probe asserts a DENIAL and
// not merely a success.

// sandboxExecPath is the absolute path to the Seatbelt launcher.
//
// Hard-coded rather than resolved through PATH on purpose: this program is the
// entire enforcement boundary, and letting a PATH entry decide which binary
// plays that role would let anything that can write to the environment
// substitute a pass-through shim. /usr/bin is on the read-only signed system
// volume on every macOS this backend supports.
const sandboxExecPath = "/usr/bin/sandbox-exec"

// probeTimeout bounds each self-check. The checks exec /usr/bin/true under a
// profile and complete in single-digit milliseconds on a healthy system
// (measured: three sequential permissive launches in 18 ms total). A bound is
// still required because a hung probe at construction time would hang
// bootstrap, and the fail-closed answer to "the sandbox did not respond" is to
// report degraded, not to wait.
const probeTimeout = 10 * time.Second

// seatbelt is the macOS Sandbox implementation. It is immutable after
// construction: the capability report is computed once by newPlatformSandbox
// and the profile inputs are fixed, so Prepare needs no locking for its own
// state. The mutex guards only the one-shot warning.
type seatbelt struct {
	cfg    Config
	report CapabilityReport

	// workspace is the resolved WorkspaceRoot. Resolved once here rather than
	// per-Prepare because EvalSymlinks hits the filesystem and the answer
	// cannot change for the lifetime of the sandbox.
	workspace string
	// scratch is the resolved scratch/cache set from ScratchPaths, likewise
	// computed once.
	scratch []string

	warnOnce sync.Once
}

// newPlatformSandbox builds the macOS backend, running the enforcement
// self-check before deciding what to report.
//
// The self-check runs at CONSTRUCTION rather than at first Prepare for two
// reasons. Operationally, bootstrap logs the security posture during startup
// and an operator reading "os-isolated" needs that to have been true at the
// time it was printed. Structurally, Prepare has no channel to tell an
// operator "I could not sandbox this" other than failing the spawn, and
// failing every spawn on a host where sandbox-exec is missing would be a
// denial of service rather than a degradation.
//
// When the check fails this returns a degraded report carrying the specific
// reason, and Prepare then leaves the command untouched. It does NOT claim
// OSIsolated: a report saying "os-isolated" while nothing is enforced is the
// exact failure this whole layer exists to make impossible.
func newPlatformSandbox(cfg Config) Sandbox {
	sb := &seatbelt{
		cfg:       cfg,
		workspace: ResolvePath(cfg.WorkspaceRoot),
		scratch:   ScratchPaths(),
	}
	if reason, ok := probeSeatbelt(); !ok {
		sb.report = CapabilityReport{
			Platform:    "darwin",
			Requested:   cfg.Tier,
			Effective:   DegradedHostGuard,
			Backend:     "seatbelt-unavailable",
			Reason:      reason,
			Enforced:    false,
			CanKillTree: false,
			Unenforced:  UnenforcedFields(cfg),
		}
		return sb
	}
	sb.report = CapabilityReport{
		Platform:  "darwin",
		Requested: cfg.Tier,
		Effective: OSIsolated,
		Backend:   "seatbelt",
		Reason: fmt.Sprintf("%s enforcing SBPL profile (tier=%s, network-deny=%t)%s",
			sandboxExecPath, cfg.Tier, cfg.NetworkDeny, sb.vacuityNote()),
		Enforced: true,
		// Seatbelt is a per-process access policy, not a process container.
		// It has no equivalent of a Job Object or a cgroup: it cannot enumerate
		// or terminate a process tree, and a child that double-forks away is
		// still subject to the policy but is no longer reachable from here.
		// Killing trees is the shell layer's job (CanKillTreeOnPlatform), and
		// claiming it here would have SecureProcessFactory advertise a
		// guarantee this backend cannot supply.
		CanKillTree: false,
		// The SBPL profile carries all four: the tier and workspace root become
		// file-write rules, NetworkDeny becomes (deny network*), and a non-empty
		// ProxyURL becomes the loopback re-permit that keeps the managed proxy
		// reachable while everything else is denied (see sbplInput below).
		Unenforced: UnenforcedFields(cfg,
			FieldTier, FieldWorkspaceRoot, FieldNetworkDeny, FieldProxyURL),
	}
	return sb
}

// probeSeatbelt determines whether Seatbelt is actually enforcing on this host,
// returning a reason string when it is not.
//
// It delegates to probeLauncher with the real launcher path. The split is not
// decoration: it is what lets a test drive the SAME logic against a stand-in
// launcher. /usr/bin/sandbox-exec lives on the read-only signed system volume
// and cannot be replaced, so without this seam a test would have to
// REIMPLEMENT the two probes -- and a copy silently stops describing the
// original. Measured: while the test asserted a copy, deleting the enforcement
// assertion from this function left the whole suite green, which is exactly
// the over-claim (report OSIsolated, enforce nothing) this layer exists to
// prevent.
func probeSeatbelt() (string, bool) { return probeLauncher(sandboxExecPath) }

// probeLauncher runs the two-probe enforcement check against launcher.
//
// Two probes, and the second is the one that matters:
//
//  1. A permissive profile `(version 1)(allow default)` must run /usr/bin/true
//     successfully. This establishes that the binary exists, that the SBPL
//     dialect is understood, and that a profile which SHOULD permit does.
//
//  2. A default-deny profile `(version 1)(deny default)` must FAIL to run the
//     same binary. This is the enforcement assertion, and probe 1 cannot
//     substitute for it: a stub that accepted -p and exec'd the target
//     unconditionally would sail through probe 1 while enforcing nothing, and
//     this backend would then report OSIsolated on a host with no sandbox at
//     all. Measured on a working host, probe 2 exits 71 with "execvp() ...
//     failed: Operation not permitted" -- the sandbox refuses the exec itself.
//
// Ordering matters for the diagnostics, not the logic: probe 1 first means a
// missing binary is reported as "not found" rather than as "does not enforce".
func probeLauncher(launcher string) (string, bool) {
	if _, err := os.Stat(launcher); err != nil {
		return fmt.Sprintf("%s not present (%v); no OS isolation", launcher, err), false
	}
	if err := runProbe(launcher, "(version 1)(allow default)"); err != nil {
		return fmt.Sprintf("%s rejected a permissive profile (%v); SBPL dialect changed or sandbox unavailable",
			launcher, err), false
	}
	if err := runProbe(launcher, "(version 1)(deny default)"); err == nil {
		return fmt.Sprintf("%s ran a program under (deny default); the sandbox is not enforcing",
			launcher), false
	}
	return "", true
}

// runProbe execs /usr/bin/true under profile via launcher and reports the
// outcome.
//
// /usr/bin/true is chosen because it is on the signed system volume, links
// against libSystem (so the deny-default probe genuinely exercises the dyld
// path the sandbox blocks), takes no arguments and writes nothing.
func runProbe(launcher, profile string) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, launcher, "-p", profile, "--", "/usr/bin/true")
	// The probe's own environment is irrelevant to the result and inheriting
	// the operator's could leak a credential into a child for no reason.
	cmd.Env = []string{}
	return cmd.Run()
}

// Prepare rewrites cmd to run under sandbox-exec.
//
// The rewrite is argv-level: cmd.Path becomes sandbox-exec and cmd.Args is
// re-headed with the launcher plus the profile, preserving the original
// program and every one of its arguments verbatim. The program is taken from
// cmd.Path rather than from cmd.Args[0], so a caller that set a cosmetic
// argv[0] cannot cause a different binary to be executed.
//
// `--` separates the launcher's options from the target. It is required
// whenever the target program's path could begin with a dash; measured,
// sandbox-exec accepts it and passes dash-leading ARGUMENTS through untouched
// either way.
//
// The profile is passed with -p (inline) rather than -f (file). A temp file
// would be a TOCTOU window — the profile IS the policy, and anything that can
// write to that file between our write and the kernel's read rewrites the
// policy. Inline text has no such window. Size is not a constraint: profiles
// of 260 KB were accepted in testing and the ones generated here are under
// 2 KB, well inside this platform's 1 MB ARG_MAX.
//
// When the backend is degraded this returns nil having changed nothing. That
// keeps the host-guard path usable rather than failing every spawn, and it is
// only safe because Report() already told the truth: nothing downstream can
// read "os-isolated" from a degraded backend and act on it.
func (s *seatbelt) Prepare(_ context.Context, cmd *exec.Cmd, spec CommandSpec) error {
	if !s.report.Enforced {
		s.warnDegraded()
		return nil
	}
	if cmd == nil {
		return fmt.Errorf("sandbox: seatbelt Prepare received a nil command")
	}
	program := cmd.Path
	if program == "" {
		program = spec.Path
	}
	if program == "" {
		return fmt.Errorf("sandbox: seatbelt Prepare received a command with no program")
	}
	if program == sandboxExecPath {
		// Double-wrapping would nest one profile inside another. The inner
		// profile cannot widen the outer one (Seatbelt policies compose by
		// intersection) so this is not a privilege escalation, but it IS an
		// argv nobody can debug and an exit status attributable to the wrong
		// layer. Refuse rather than silently produce it.
		return fmt.Errorf("sandbox: refusing to wrap %s in itself", sandboxExecPath)
	}

	args := cmd.Args
	if len(args) == 0 {
		args = append([]string{program}, spec.Args...)
	}

	profile := s.profileFor(spec.Tier)
	cmd.Path = sandboxExecPath
	cmd.Args = append([]string{sandboxExecPath, "-p", profile, "--", program}, args[1:]...)
	return nil
}

// profileFor renders the SBPL text for one invocation's tier.
//
// spec.Tier, not cfg.Tier: the per-invocation tier is the whole point of
// secproc's UseSandboxTier, and reading the global one here would make
// git_diff's declared ReadOnly and run_tests' declared WorkspaceWrite
// indistinguishable — every tool would silently run at whatever the operator
// configured globally.
//
// AllowLoopback is tied to the presence of a proxy URL: denying all IP while
// pointing the child's http_proxy at a loopback listener would produce a child
// that cannot reach the one egress path it was given. When no proxy is
// configured there is nothing on loopback the child is entitled to and the
// narrower posture applies.
func (s *seatbelt) profileFor(tier AccessTier) string {
	return BuildProfile(s.profileInput(tier))
}

// profileInput builds the generator input for one tier.
//
// Extracted so profileFor and vacuityNote cannot disagree: the honesty check
// must be asked about the SAME input that will actually be rendered, and a
// second hand-built ProfileInput would drift the first time a field is added
// (the new field would be missing here, and the vacuity answer would quietly
// describe a profile nobody runs).
func (s *seatbelt) profileInput(tier AccessTier) ProfileInput {
	return ProfileInput{
		Tier:          tier,
		WorkspaceRoot: s.workspace,
		WritablePaths: s.scratch,
		NetworkDeny:   s.cfg.NetworkDeny,
		AllowLoopback: s.cfg.ProxyURL != "",
	}
}

// vacuityNote appends a disclosure to the Reason when the CONFIGURED tier
// renders a profile that denies nothing.
//
// The mechanism is genuinely live in this state — every spawn is still wrapped
// in sandbox-exec, and a per-invocation spec.Tier below FullAccess still gets a
// real (deny default) profile, which is why this is a note on the Reason rather
// than a downgrade to DegradedHostGuard. Downgrading would be its own lie in
// the opposite direction: it would tell the escalation classifier in
// internal/tools that no denial it sees can be a sandbox denial, and those
// denials are real.
//
// But an operator reading "os-isolated" for a full-access, network-permitted
// configuration would reasonably conclude something is being restricted, and
// nothing is: the profile is literally `(version 1)(allow default)`. Reason is
// the field that carries the caveat, and bootstrap logs it verbatim.
func (s *seatbelt) vacuityNote() string {
	if !ProfileRestrictsNothing(s.profileInput(s.cfg.Tier)) {
		return ""
	}
	return " — NOTE: at this tier the profile is (allow default) and restricts " +
		"nothing; per-invocation tiers below full-access are still enforced"
}

// Once per spawn would put a line in front of the operator for every git
// invocation the model makes, which is how a real warning becomes scroll-back
// nobody reads. bootstrap already logs the posture through slog at startup;
// this exists for the case where a sandbox is constructed outside that path.
func (s *seatbelt) warnDegraded() {
	s.warnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "yanshi: sandbox not enforcing on darwin: %s\n",
			strings.TrimSpace(s.report.Reason))
	})
}

// Report returns the capability report computed at construction time.
//
// Cached rather than re-probed because a caller may ask on every spawn (the
// escalation path in internal/tools does) and re-running two subprocesses per
// question would be a measurable cost for an answer that cannot change: the
// presence and behaviour of /usr/bin/sandbox-exec is a property of the
// installed OS, not of the moment.
func (s *seatbelt) Report() CapabilityReport { return s.report }

// Close releases sandbox-wide resources. There are none: the profile is passed
// inline per exec, so no temp file, no daemon and no kernel handle outlives a
// child. The method exists to satisfy the interface and to keep the shape
// stable if a future backend does acquire something.
func (s *seatbelt) Close() error { return nil }
