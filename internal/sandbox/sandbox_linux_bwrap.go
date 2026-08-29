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

// This file is the Linux bubblewrap enforcement backend. It rewrites each
// prepared exec.Cmd into
//
//	bwrap <mount plan...> -- <original program> <original args>
//
// so the kernel applies the mount, PID and network namespaces before the
// target program's first instruction.
//
// # bwrap does NOT exec in place, and that has consequences
//
// This is the sharpest difference from the darwin backend. sandbox-exec execs
// the target and the child reports the same pid; bubblewrap does not. bwrap
// forks: with --unshare-pid it must remain alive as pid 1 of the new PID
// namespace to reap orphans, so there is genuinely an extra process between
// yanshi and the target. What follows from that, and what does not:
//
//   - Exit status IS propagated. bwrap waits for the target and exits with the
//     target's code; a signalled child is reported as 128+signo. So Wait() on
//     the returned cmd still yields the target's outcome.
//   - Signals sent to the returned cmd's pid reach BWRAP, not the target.
//     --die-with-parent covers the case that matters (yanshi dies, sandbox
//     dies) but a targeted SIGTERM to "the command" terminates the namespace
//     rather than politely asking the program to shut down. That is why this
//     backend reports CanKillTree=false: it is not that killing is impossible,
//     it is that the tree semantics differ from what the shell layer means by
//     the term, and the shell layer's own process-group logic is the thing
//     that must stay authoritative.
//   - Anything reading the child's pid to correlate with /proc gets bwrap's
//     pid, which is in the host namespace and does exist -- so nothing breaks,
//     but the pid is one level up from the program the operator named.
//
// # Why the probe must attempt a real denial
//
// `bwrap --version` succeeding proves nothing, and this is the single most
// common way a Linux sandbox silently becomes a no-op. Bubblewrap is packaged
// on essentially every distribution, so the binary is usually present -- but
// unprivileged user namespaces, which non-setuid bwrap requires, are disabled
// by default or by policy on a long list of real hosts:
//
//   - Debian/Ubuntu with kernel.unprivileged_userns_clone=0
//   - RHEL/CentOS with user.max_user_namespaces=0
//   - Ubuntu 24.04+ AppArmor restricting unprivileged userns by profile
//   - Most container runtimes, unless explicitly granted
//
// On all of those, `bwrap --version` prints a version and exits 0 while every
// actual sandbox launch fails with "bwrap: No permissions to creating new
// namespace". A backend that probed only the version would report os-isolated
// on a host where nothing is isolated at all, which is exactly the over-claim
// this whole layer exists to prevent. So probeBwrapAt launches a real sandbox
// and requires a write that MUST fail to actually fail.

// bwrapProbeTimeout bounds each self-check launch.
//
// A bound is mandatory rather than defensive: the probe runs during bootstrap,
// and bwrap on a host with a wedged FUSE mount under / can block indefinitely
// in the bind walk. The fail-closed answer to "the sandbox did not respond" is
// to report degraded, never to wait.
const bwrapProbeTimeout = 10 * time.Second

// bubblewrap is the Linux bwrap-backed Sandbox. Immutable after construction:
// the report and the resolved paths are computed once, so Prepare needs no
// locking for its own state. The mutex guards only the one-shot warning.
type bubblewrap struct {
	cfg    Config
	report CapabilityReport

	// bwrapPath is the ABSOLUTE path resolved from PATH at construction time.
	// Resolved once and then used verbatim so that a PATH mutation between
	// construction (when the probe ran) and Prepare (when it matters) cannot
	// swap in a different binary than the one that was actually verified to
	// enforce.
	bwrapPath string

	// workspace and scratch are resolved once; EvalSymlinks hits the
	// filesystem and the answer cannot change for the sandbox's lifetime.
	workspace string
	scratch   []string

	warnOnce sync.Once
}

// newBubblewrap builds the bwrap backend, running the enforcement self-check
// before deciding what to report. It returns nil when bwrap cannot be used, so
// newPlatformSandbox can fall through to the Landlock backend; the reason for
// the refusal is returned alongside for the eventual degraded report.
//
// The self-check runs at CONSTRUCTION, matching the darwin backend, for the
// same two reasons: bootstrap logs the security posture at startup and an
// operator reading "os-isolated" needs that to have been true when printed;
// and Prepare has no channel to report "I could not sandbox this" other than
// failing the spawn, which on a host without working user namespaces would be
// a denial of service rather than a degradation.
func newBubblewrap(cfg Config) (Sandbox, string) {
	path, err := exec.LookPath(bwrapProgram)
	if err != nil {
		return nil, fmt.Sprintf("%s not found on PATH (%v)", bwrapProgram, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Sprintf("%s resolved to an unusable path %q (%v)", bwrapProgram, path, err)
	}
	if reason, ok := probeBwrapAt(abs); !ok {
		return nil, reason
	}

	sb := &bubblewrap{
		cfg:       cfg,
		bwrapPath: abs,
		workspace: ResolvePath(cfg.WorkspaceRoot),
		scratch:   BwrapScratchPaths(),
	}
	sb.report = CapabilityReport{
		Platform:  "linux",
		Requested: cfg.Tier,
		Effective: OSIsolated,
		Backend:   "bubblewrap",
		Reason: fmt.Sprintf(
			"%s enforcing mount+pid namespaces (tier=%s, network-deny=%t); "+
				"filesystem, process and network isolation active%s",
			abs, cfg.Tier, cfg.NetworkDeny, sb.vacuityNote()),
		Enforced: true,
		// See the file header: bwrap is pid 1 of the new namespace and a
		// signal to the returned pid hits bwrap rather than the target. The
		// shell layer's process-group logic stays authoritative for kill-tree,
		// and claiming the capability here would have SecureProcessFactory
		// advertise semantics this backend does not supply.
		CanKillTree: false,
		// bwrap binds the workspace tier through its mount arguments and drops
		// the network namespace outright for NetworkDeny (see bwrapargs.go).
		// ProxyURL is not in the list: nothing in the bwrap argument builder
		// reads it, so egress through the managed proxy is env-var level here
		// exactly as it is outside the sandbox.
		Unenforced: UnenforcedFields(cfg, FieldTier, FieldWorkspaceRoot, FieldNetworkDeny),
	}
	return sb, ""
}

// probeBwrapAt determines whether bubblewrap actually enforces on this host,
// returning a reason string when it does not.
//
// Two probes, and as on darwin the second is the one that matters:
//
//  1. A minimal sandbox must be able to run /bin/true. This establishes that
//     the binary works, that unprivileged user namespaces are permitted, and
//     that a plan which SHOULD succeed does. Failure here is the common case
//     on hardened hosts and produces the specific "namespaces unavailable"
//     diagnosis operators need.
//
//  2. A read-only sandbox must FAIL to write. Probe 1 cannot substitute for
//     it: a wrapper script named bwrap that dropped its arguments and exec'd
//     the target would sail through probe 1 while isolating nothing, and this
//     backend would then report OSIsolated on a host with no sandbox. This
//     probe writes to a path under the read-only root and requires a nonzero
//     exit.
//
// Ordering is for diagnostics, not logic: probe 1 first means a host without
// user namespaces is reported as such rather than as "does not enforce".
func probeBwrapAt(bwrapPath string) (string, bool) {
	if err := runBwrapProbe(bwrapPath, []string{
		"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"--unshare-all", "--", "/bin/true",
	}); err != nil {
		return fmt.Sprintf(
			"%s could not create a sandbox (%v); unprivileged user namespaces are "+
				"likely disabled (kernel.unprivileged_userns_clone / "+
				"user.max_user_namespaces / AppArmor userns restriction)",
			bwrapPath, err), false
	}

	// The write target must be a path that exists on every Linux and that a
	// working read-only bind genuinely refuses. /proc is bound read-only by
	// --ro-bind / / above and is not remounted here on purpose: writing into
	// it must fail because of the bind, not because of procfs semantics.
	if err := runBwrapProbe(bwrapPath, []string{
		"--ro-bind", "/", "/", "--dev", "/dev",
		"--unshare-all", "--",
		"/bin/sh", "-c", "exec 3>/yanshi-sandbox-probe",
	}); err == nil {
		return fmt.Sprintf(
			"%s allowed a write under a read-only bind; the sandbox is not enforcing",
			bwrapPath), false
	}
	return "", true
}

// runBwrapProbe launches one probe sandbox and reports the outcome.
//
// The probe's environment is emptied: it is irrelevant to the result, and
// inheriting the operator's would push a credential into a child process for
// no reason. Output is discarded rather than captured because the only signal
// consumed is the exit status, and bwrap's diagnostics on a hardened host are
// long enough to be noise in a startup log.
func runBwrapProbe(bwrapPath string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), bwrapProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bwrapPath, args...)
	cmd.Env = []string{}
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// Prepare rewrites cmd to run under bubblewrap.
//
// The rewrite is argv-level and mirrors the darwin backend: cmd.Path becomes
// the resolved bwrap and cmd.Args is re-headed with the launcher plus the
// mount plan, preserving the original program and every one of its arguments
// verbatim. The program is taken from cmd.Path rather than cmd.Args[0], so a
// caller that set a cosmetic argv[0] cannot cause a different binary to run.
//
// `--` terminates bwrap's own option parsing. It is load-bearing rather than
// stylistic here: without it a target program whose path begins with a dash
// would be consumed as a bwrap flag, and a target named e.g. `--bind` would
// let the callee edit its own mount plan.
//
// cmd.Dir is cleared after being folded into --chdir. Leaving it set would
// have the Go runtime chdir the bwrap process in the HOST namespace before the
// sandbox exists; when the sandbox then makes that path invisible (a directory
// under the private /tmp) the launch fails for a reason with no visible
// connection to the cause.
//
// When the backend is degraded this returns nil having changed nothing, which
// keeps the host-guard path usable rather than failing every spawn. That is
// only safe because Report() already told the truth.
func (b *bubblewrap) Prepare(_ context.Context, cmd *exec.Cmd, spec CommandSpec) error {
	if !b.report.Enforced {
		b.warnDegraded()
		return nil
	}
	if cmd == nil {
		return fmt.Errorf("sandbox: bubblewrap Prepare received a nil command")
	}
	program := cmd.Path
	if program == "" {
		program = spec.Path
	}
	if program == "" {
		return fmt.Errorf("sandbox: bubblewrap Prepare received a command with no program")
	}
	if program == b.bwrapPath || filepath.Base(program) == bwrapProgram {
		// Double-wrapping nests one namespace set inside another. It is not a
		// privilege escalation (namespaces compose by intersection) but it is
		// an argv nobody can debug, and the inner bwrap needs privileges the
		// outer one just removed, so it fails in a way attributable to the
		// wrong layer. Refuse rather than silently produce it.
		return fmt.Errorf("sandbox: refusing to wrap %s in itself", bwrapProgram)
	}

	args := cmd.Args
	if len(args) == 0 {
		args = append([]string{program}, spec.Args...)
	}

	dir := cmd.Dir
	if dir == "" {
		dir = spec.Dir
	}
	plan := BuildBwrapArgs(b.inputFor(spec.Tier, dir))

	full := make([]string, 0, len(plan)+len(args)+2)
	full = append(full, b.bwrapPath)
	full = append(full, plan...)
	full = append(full, "--", program)
	full = append(full, args[1:]...)

	cmd.Path = b.bwrapPath
	cmd.Args = full
	cmd.Dir = ""
	return nil
}

// inputFor builds the generator input for one invocation.
//
// spec.Tier, not cfg.Tier: the per-invocation tier is the entire point of
// secproc's UseSandboxTier, and reading the global one here would make a
// tool's declared ReadOnly and another's declared WorkspaceWrite
// indistinguishable -- every tool would silently run at whatever the operator
// configured globally.
//
// Extracted so Prepare and vacuityNote cannot disagree: the honesty check must
// be asked about the SAME input that will be rendered, and a second hand-built
// BwrapInput would drift the first time a field is added, quietly describing a
// plan nobody runs.
func (b *bubblewrap) inputFor(tier AccessTier, dir string) BwrapInput {
	return BwrapInput{
		Tier:          tier,
		WorkspaceRoot: b.workspace,
		ScratchPaths:  b.scratch,
		NetworkDeny:   b.cfg.NetworkDeny,
		Chdir:         ResolvePath(dir),
	}
}

// vacuityNote appends a disclosure to the Reason when the CONFIGURED tier
// renders a plan that restricts no files.
//
// As on darwin the mechanism is genuinely live in this state -- every spawn is
// still wrapped, the PID namespace is still unshared, and a per-invocation
// tier below FullAccess still gets a real read-only root -- which is why this
// is a note rather than a downgrade to DegradedHostGuard. Downgrading would be
// its own lie in the opposite direction: it would tell the escalation
// classifier in internal/tools that no denial it sees can be a sandbox denial,
// and those denials are real.
func (b *bubblewrap) vacuityNote() string {
	if !BwrapRestrictsNothing(b.inputFor(b.cfg.Tier, "")) {
		return ""
	}
	return " — NOTE: at this tier the mount plan binds / read-write and restricts " +
		"no files; only pid/process isolation applies, and per-invocation tiers " +
		"below full-access are still enforced"
}

// warnDegraded prints the degradation reason once per sandbox.
//
// Once per spawn would put a line in front of the operator for every git
// invocation the model makes, which is how a real warning becomes scroll-back
// nobody reads.
func (b *bubblewrap) warnDegraded() {
	b.warnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "yanshi: sandbox not enforcing on linux: %s\n",
			strings.TrimSpace(b.report.Reason))
	})
}

// Report returns the capability report computed at construction time.
//
// Cached rather than re-probed: a caller may ask on every spawn (the
// escalation path in internal/tools does) and re-running two sandbox launches
// per question would be a measurable cost for an answer that cannot change
// within a process lifetime.
func (b *bubblewrap) Report() CapabilityReport { return b.report }

// Close releases sandbox-wide resources. There are none: the mount plan is
// passed as argv per exec, so no temp file, no daemon and no kernel handle
// outlives a child.
func (b *bubblewrap) Close() error { return nil }
