//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// This file is the Landlock enforcement backend: the FALLBACK used when
// bubblewrap cannot run. It rewrites each prepared exec.Cmd into
//
//	/proc/self/exe __landlock_exec <rules> -- <original program> <original args>
//
// so yanshi's own binary re-execs, confines itself with landlock_restrict_self,
// and then execve()s the target in place. See landlockrules.go for why the
// confinement has to happen in the child, and landlock_linux.go for the
// syscall sequence.
//
// # What this backend does NOT do, and why saying so matters
//
// Landlock is a filesystem LSM. It has no process isolation and -- at the ABI
// levels this code targets -- no network control. Concretely, a process
// confined here can still:
//
//   - open sockets and reach any host the routing table allows, regardless of
//     Config.NetworkDeny;
//   - see, signal and ptrace-adjacent-inspect every other process owned by the
//     same uid, because there is no PID namespace;
//   - exhaust memory, fds and pids, because there is no cgroup.
//
// The Reason string states this verbatim, and that is load-bearing rather than
// documentation politeness: internal/tools' SandboxEnforcing check consults
// the report to decide whether a failure it observed was a sandbox denial. If
// this backend claimed network enforcement, a plain connection refusal -- a
// down server, a DNS failure, a proxy hiccup -- would be classified as a
// sandbox denial and would raise an escalation prompt asking the operator to
// approve more privilege for something privilege cannot fix. The honest Reason
// is what keeps that prompt from firing on unrelated network noise.
//
// # ABI 4 network support is deliberately not used
//
// Landlock ABI 4 (kernel 6.7) adds TCP bind/connect restrictions, which would
// let this backend honour NetworkDeny on new enough kernels. It is not wired
// up, because a control that works only on 6.7+ would make the SAME
// configuration enforce network policy on one host and silently not on
// another, and the capability report has one Reason string for both. Adding it
// means reporting the negotiated ABI in a machine-readable field so callers
// can branch, which is a larger change than this one. Until then the answer to
// "does this backend deny network?" is an unambiguous no.

// landlockProbeTimeout bounds the enforcement self-check. Same rationale as
// the bubblewrap probe: this runs during bootstrap and a hung probe must
// degrade rather than block startup.
const landlockProbeTimeout = 10 * time.Second

// landlockBackend is the Landlock-backed Sandbox. Immutable after
// construction; the mutex guards only the one-shot warning.
type landlockBackend struct {
	cfg    Config
	report CapabilityReport

	// selfExe is the resolved path to this binary, captured at construction.
	//
	// Resolved from /proc/self/exe rather than os.Args[0]: argv[0] is
	// attacker-influenced and may be a bare name, a relative path, or a
	// cosmetic lie, and using it would decide WHICH BINARY performs the
	// confinement based on a string the caller supplied.
	selfExe string

	// abi is the negotiated Landlock ABI version, reported so an operator can
	// see which rights are restrictable on this kernel.
	abi int

	workspace string
	scratch   []string

	warnOnce sync.Once
}

// newLandlock builds the Landlock backend, running a real enforcement
// self-check first. It returns nil plus a reason when Landlock cannot be used,
// so newPlatformSandbox can fall through to an honest degraded report.
func newLandlock(cfg Config) (Sandbox, string) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Sprintf("cannot resolve own executable for re-exec (%v)", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return nil, fmt.Sprintf("cannot absolutise own executable path (%v)", err)
	}

	abi, err := landlockABI()
	if err != nil {
		return nil, err.Error()
	}
	if reason, ok := probeLandlockAt(exe); !ok {
		return nil, reason
	}

	sb := &landlockBackend{
		cfg:       cfg,
		selfExe:   exe,
		abi:       abi,
		workspace: ResolvePath(cfg.WorkspaceRoot),
		scratch:   BwrapScratchPaths(),
	}
	sb.report = CapabilityReport{
		Platform:  "linux",
		Requested: cfg.Tier,
		Effective: OSIsolated,
		Backend:   "landlock",
		Reason: fmt.Sprintf(
			"landlock ABI %d enforcing FILESYSTEM access only (tier=%s); "+
				"NOT enforced by this backend: network egress (Config.NetworkDeny=%t is "+
				"ignored here), process/pid isolation, and resource limits — "+
				"bubblewrap was unavailable%s",
			abi, cfg.Tier, cfg.NetworkDeny, sb.vacuityNote()),
		Enforced: true,
		// No PID namespace, no cgroup: this backend cannot enumerate or
		// terminate a process tree. Killing trees stays the shell layer's job.
		CanKillTree: false,
	}
	return sb, ""
}

// probeLandlockAt verifies that Landlock actually confines on this host by
// running the real helper against a policy that must deny.
//
// A successful landlockABI() call is NOT sufficient, for the same reason
// `bwrap --version` is not: it proves the syscall exists, not that restriction
// takes effect. Landlock can be present and report an ABI while being
// ineffective -- most commonly inside a container whose seccomp profile
// returns success for the landlock syscalls, or under a security policy that
// stubs them. The probe therefore re-execs the real helper with a read-only
// policy and requires a write to fail.
//
// It exercises the SAME code path production uses -- the same argv grammar,
// the same encoder, the same helper, the same applyLandlock -- rather than a
// reimplementation. That seam is what keeps the check honest: a test or a
// refactor that broke the helper's fail-closed contract would break this probe
// too, whereas a probe that called applyLandlock directly would keep passing
// while the argv path it is supposed to validate was broken.
func probeLandlockAt(exe string) (string, bool) {
	// A read-only policy over "/", with the device writes that every tier
	// grants. /bin/true must still run: this establishes that the re-exec, the
	// decode and the restrict all work and that a policy which SHOULD permit
	// does.
	readOnly := BuildLandlockRules(BwrapInput{Tier: ReadOnly}, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
	token, err := EncodeLandlockRules(readOnly)
	if err != nil {
		return fmt.Sprintf("cannot encode a landlock probe policy (%v)", err), false
	}

	if err := runLandlockProbe(exe, token, "/bin/true"); err != nil {
		return fmt.Sprintf(
			"landlock helper could not run a permitted command (%v); the re-exec "+
				"path or landlock_restrict_self is not working on this host", err), false
	}

	// The enforcement assertion. Writing under a read-only "/" must fail. The
	// target is inside /tmp because it is the one directory guaranteed to be
	// writable WITHOUT Landlock, so a failure here can only be Landlock: if
	// the probe wrote somewhere unwritable anyway, a completely inert sandbox
	// would pass this check.
	probeFile := filepath.Join(os.TempDir(), "yanshi-landlock-probe")
	if err := runLandlockProbe(exe, token,
		"/bin/sh", "-c", "exec 3>"+probeFile); err == nil {
		_ = os.Remove(probeFile)
		return "landlock accepted a write under a read-only policy; it is not enforcing", false
	}
	_ = os.Remove(probeFile)
	return "", true
}

// runLandlockProbe re-execs the helper once and reports the outcome.
//
// The environment is emptied for the same reason the bubblewrap probe empties
// it: it cannot affect the result and inheriting the operator's would push a
// credential into a child for no purpose.
func runLandlockProbe(exe, token string, target ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), landlockProbeTimeout)
	defer cancel()
	args := append([]string{landlockHelperArg, token, "--"}, target...)
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = []string{}
	return cmd.Run()
}

// Prepare rewrites cmd to run under the Landlock re-exec helper.
//
// The rewrite mirrors the other two backends: cmd.Path becomes this binary and
// cmd.Args is re-headed with the helper token plus the encoded policy,
// preserving the original program and its arguments verbatim after `--`. The
// program comes from cmd.Path rather than cmd.Args[0], so a cosmetic argv[0]
// cannot redirect execution.
//
// Unlike the bubblewrap backend, cmd.Dir is LEFT ALONE. There is no mount
// namespace here, so the helper's working directory is the host's and the Go
// runtime's chdir before exec is both correct and necessary -- the target
// program must start where the caller said. The rules are computed against
// that same directory only insofar as it appears in the workspace/scratch
// lists.
//
// An encoding failure returns an error and prepares nothing. Falling through
// would exec the target unconfined while the report says os-isolated.
func (l *landlockBackend) Prepare(_ context.Context, cmd *exec.Cmd, spec CommandSpec) error {
	if !l.report.Enforced {
		l.warnDegraded()
		return nil
	}
	if cmd == nil {
		return fmt.Errorf("sandbox: landlock Prepare received a nil command")
	}
	program := cmd.Path
	if program == "" {
		program = spec.Path
	}
	if program == "" {
		return fmt.Errorf("sandbox: landlock Prepare received a command with no program")
	}
	if program == l.selfExe {
		// Re-execing ourselves to sandbox ourselves running ourselves nests a
		// second ruleset inside the first. Landlock rulesets compose by
		// intersection so it is not an escalation, but the argv becomes
		// unreadable and an exit status is attributable to the wrong layer.
		return fmt.Errorf("sandbox: refusing to wrap %s in itself", l.selfExe)
	}

	args := cmd.Args
	if len(args) == 0 {
		args = append([]string{program}, spec.Args...)
	}

	token, err := EncodeLandlockRules(l.rulesFor(spec.Tier))
	if err != nil {
		return err
	}

	full := make([]string, 0, len(args)+4)
	full = append(full, l.selfExe, landlockHelperArg, token, "--", program)
	full = append(full, args[1:]...)

	cmd.Path = l.selfExe
	cmd.Args = full
	return nil
}

// rulesFor computes the policy for one invocation.
//
// spec.Tier, not cfg.Tier, for the same reason as the other backends: the
// per-invocation tier is what secproc's UseSandboxTier exists to express, and
// reading the global one would collapse every tool onto whatever the operator
// configured.
//
// Extracted so Prepare and vacuityNote are answered from the SAME input; a
// second hand-built value would drift the first time a field is added and the
// honesty note would then describe a policy nobody runs.
func (l *landlockBackend) rulesFor(tier AccessTier) LandlockRules {
	return BuildLandlockRules(BwrapInput{
		Tier:          tier,
		WorkspaceRoot: l.workspace,
		ScratchPaths:  l.scratch,
	}, func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
}

// vacuityNote discloses a configured tier whose policy restricts nothing.
//
// As on the other backends this is a note rather than a downgrade: the
// mechanism is live, every spawn still goes through the helper, and a
// per-invocation tier below FullAccess still gets a real read-only policy.
func (l *landlockBackend) vacuityNote() string {
	if !LandlockRestrictsNothing(l.rulesFor(l.cfg.Tier)) {
		return ""
	}
	return " — NOTE: at this tier the policy grants full rights on / and restricts " +
		"no files; per-invocation tiers below full-access are still enforced"
}

// warnDegraded prints the degradation reason once per sandbox.
func (l *landlockBackend) warnDegraded() {
	l.warnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "yanshi: sandbox not enforcing on linux: %s\n",
			strings.TrimSpace(l.report.Reason))
	})
}

// Report returns the capability report computed at construction time.
func (l *landlockBackend) Report() CapabilityReport { return l.report }

// Close releases sandbox-wide resources. There are none: the policy travels in
// argv and the ruleset fd is closed inside the child before it execs.
func (l *landlockBackend) Close() error { return nil }
