package shell

import (
	"context"
	"os/exec"

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
// passed, and shell v2 tools pass none. Building the child env from that
// alone (as the secproc path does, since its callers do populate Env) leaves
// the child with only the managed proxy variables — no PATH — so every tool
// that resolves a binary through PATH (run_tests, github_*) fails to spawn.
// netpolicy.ManagedEnv(proxy) is exactly PrepareEnv(os.Environ(), proxy):
// host env in, inherited proxy vars stripped, managed ones appended.
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

// prepareLaunch computes the spec that is handed to the OS factory without
// spawning anything, so the env and sandbox decisions are assertable on their
// own. Order matters: env first (the sandbox may then add to it), sandbox
// second (it may rewrite argv, e.g. wrap the command in bwrap/sandbox-exec).
func (f SecureLaunchFactory) prepareLaunch(ctx context.Context, spec LaunchSpec) (LaunchSpec, error) {
	proxy := f.ProxyURL
	if f.Policy != nil && proxy == "" {
		// A policy with no proxy endpoint must still not let the child talk
		// directly: point it at a dead local port rather than leave the vars
		// empty (which would mean "no proxy").
		proxy = "http://127.0.0.1:0"
	}
	env := netpolicy.ManagedEnv(proxy)
	if len(spec.Env) > 0 {
		// Caller entries win over the host's (exec keeps the last duplicate),
		// but re-running PrepareEnv strips any proxy vars they smuggled in.
		env = netpolicy.PrepareEnv(append(env, spec.Env...), proxy)
	}
	spec.Env = env
	if f.Sandbox == nil {
		return spec, nil
	}
	// The sandbox seam speaks *exec.Cmd, but the spawn happens inside the OS
	// factory. Prepare a stand-in Cmd carrying the same fields, let the
	// backend mutate it, then copy the mutations back into the spec — that is
	// what makes an argv-wrapping or env-injecting Phase 1+ backend take
	// effect here. Phase 0's Prepare is a no-op, so the copy-back is identity.
	cmd := &exec.Cmd{Path: spec.Program, Args: append([]string{spec.Program}, spec.Args...), Dir: spec.Dir, Env: spec.Env}
	cs := sandbox.CommandSpec{Path: spec.Program, Args: spec.Args, Dir: spec.Dir, Tier: f.Sandbox.Report().Requested}
	if err := f.Sandbox.Prepare(ctx, cmd, cs); err != nil {
		return LaunchSpec{}, err
	}
	spec.Program, spec.Dir, spec.Env = cmd.Path, cmd.Dir, cmd.Env
	if len(cmd.Args) > 0 {
		spec.Args = cmd.Args[1:]
	}
	return spec, nil
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
