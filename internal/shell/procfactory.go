package shell

import (
	"context"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
)

// SecureLaunchFactory is the production shell.Config.Factory: it fills the
// child environment, runs the sandbox seam, and delegates the actual spawn to
// the OS factory. Without it Manager.Start fails with "no process factory
// configured", which is why every shell v2 tool needs this wired first.
//
// Why NOT build on DefaultSecureFactory: the two serve different interfaces.
// Config.Factory is a ProcessFactory (LaunchSpec → Process+Console) whereas
// DefaultSecureFactory.Start implements secproc.Factory (SecureProcessSpec →
// *StartedProcess) — different spec, different return, so one type cannot
// carry both under the same method name. DefaultSecureFactory also already
// holds an OSProcessFactory of its own, so wrapping it would nest two
// pipelines rather than reuse one. This type is deliberately a thin sibling
// on the same primitives (netpolicy + sandbox + OSProcessFactory), and
// DefaultSecureFactory is left untouched so the two paths stay debuggable
// independently.
//
// Why the env starts from the host: LaunchSpec.Env is whatever the caller
// passed, and shell v2 tools pass none. Building the child env from that alone
// leaves the child with only the managed proxy variables — no PATH — so every
// tool that resolves a binary through PATH (run_tests, github_*) fails to
// spawn. That prediction came true on the secproc path, which had its own copy
// of this logic; both now share childLaunchPosture, so the two cannot drift
// apart again.
type SecureLaunchFactory struct {
	// OS is the spawn backend; nil means OSProcessFactory (the constructor
	// normalizes it, and Start defends against struct-literal construction).
	OS ProcessFactory
	// Policy being non-nil means an egress policy is in force, so a proxy URL
	// must be published to the child even if the caller left ProxyURL empty.
	Policy   *netpolicy.Policy
	ProxyURL string
	// SOCKSURL, CAFile and Snapshot are the rest of the launch posture; see
	// childLaunchPosture for what each one does and when it is empty.
	SOCKSURL string
	CAFile   string
	Sandbox  sandbox.Sandbox
	Snapshot Snapshot
}

// SecureLaunchFactory must satisfy the Manager's factory seam; the assertion
// keeps a signature drift in ProcessFactory from silently un-wiring shell v2.
var _ ProcessFactory = SecureLaunchFactory{}

// NewSecureLaunchFactory normalizes the zero values so callers can pass only
// what they have (bootstrap has a policy and a sandbox; tests often have
// neither) without every call site repeating the OSProcessFactory default.
func NewSecureLaunchFactory(f SecureLaunchFactory) SecureLaunchFactory {
	if f.OS == nil {
		f.OS = OSProcessFactory{}
	}
	return f
}

// posture builds the shared launch posture this factory applies to every child.
//
// Extracted so prepareLaunch and Start cannot disagree about it: the post-spawn
// sandbox step must be asked of the SAME posture that prepared the command, and
// a second struct literal in Start would drift the first time a field is added
// there (the new field would be missing here, and the sandbox seam would run
// against a posture nothing was launched under).
func (f SecureLaunchFactory) posture() childLaunchPosture {
	return childLaunchPosture{
		Policy:   f.Policy,
		ProxyURL: f.ProxyURL,
		SOCKSURL: f.SOCKSURL,
		CAFile:   f.CAFile,
		Sandbox:  f.Sandbox,
		Snapshot: f.Snapshot,
	}
}

// prepareLaunch delegates to the shared childLaunchPosture. There is no
// per-invocation access tier on this path (LaunchSpec has no equivalent of
// secproc's UseSandboxTier), so the sandbox's globally requested tier is the
// one handed to Prepare.
func (f SecureLaunchFactory) prepareLaunch(ctx context.Context, spec LaunchSpec) (LaunchSpec, error) {
	tier := sandbox.ReadOnly
	if f.Sandbox != nil {
		tier = f.Sandbox.Report().Requested
	}
	return f.posture().prepare(ctx, spec, tier)
}

// Start prepares the spec and delegates. A sandbox refusal aborts the spawn
// (fail-closed) rather than falling through to an unsandboxed process.
//
// The post-spawn sandbox step runs between the OS factory returning and this
// function returning, which is the only window in which it is correct: the
// process exists (so it has a pid to bind) and nothing has reaped it yet (so the
// pid is unambiguous). A failure there means the child could not be contained
// and has already been terminated by the backend, so the console is torn down
// and the error propagated rather than handing back a process the capability
// report claims is inside a job.
func (f SecureLaunchFactory) Start(ctx context.Context, spec LaunchSpec) (Process, Console, error) {
	posture := f.posture()
	prepared, err := f.prepareLaunch(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	backend := f.OS
	if backend == nil {
		backend = OSProcessFactory{}
	}
	// The elevation shims, on the same seam and for the same reason as on the
	// secproc path. The workdir handed to the decider is empty here because
	// LaunchSpec carries no project boundary — see interceptElevation for why
	// the shim's own directory must not be substituted for it.
	prepared, stopBroker := posture.interceptElevation(ctx, prepared, "")
	proc, console, err := backend.Start(ctx, prepared)
	if err != nil {
		stopBroker()
		return nil, nil, err
	}
	if err := posture.postStart(proc.PID()); err != nil {
		stopBroker()
		if console != nil {
			_ = console.Close()
		}
		return nil, nil, err
	}
	return &brokeredProcess{Process: proc, stop: stopBroker}, console, nil
}

// brokeredProcess ties an elevation broker's lifetime to the reap of the
// process it was installed for.
//
// Manager.pump always calls Wait, on every exit path, so this is where the
// listener goroutine and the shim directory are reclaimed. The ctx watchdog
// inside execbroker.Listen is the backstop for a caller that abandons the
// process without reaping, not the primary mechanism: the shell v2 launch
// context is detached from the turn, so it may not be cancelled for hours.
type brokeredProcess struct {
	Process
	stop func()
}

func (p *brokeredProcess) Wait() error {
	defer p.stop()
	return p.Process.Wait()
}
