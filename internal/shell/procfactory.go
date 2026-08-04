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
	Sandbox  sandbox.Sandbox
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

// prepareLaunch delegates to the shared childLaunchPosture. There is no
// per-invocation access tier on this path (LaunchSpec has no equivalent of
// secproc's UseSandboxTier), so the sandbox's globally requested tier is the
// one handed to Prepare.
func (f SecureLaunchFactory) prepareLaunch(ctx context.Context, spec LaunchSpec) (LaunchSpec, error) {
	posture := childLaunchPosture{Policy: f.Policy, ProxyURL: f.ProxyURL, Sandbox: f.Sandbox}
	tier := sandbox.ReadOnly
	if f.Sandbox != nil {
		tier = f.Sandbox.Report().Requested
	}
	return posture.prepare(ctx, spec, tier)
}

// Start prepares the spec and delegates. A sandbox refusal aborts the spawn
// (fail-closed) rather than falling through to an unsandboxed process.
func (f SecureLaunchFactory) Start(ctx context.Context, spec LaunchSpec) (Process, Console, error) {
	prepared, err := f.prepareLaunch(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	backend := f.OS
	if backend == nil {
		backend = OSProcessFactory{}
	}
	return backend.Start(ctx, prepared)
}
