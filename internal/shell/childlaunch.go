package shell

import (
	"context"
	"os/exec"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
)

// childLaunchPosture is the security posture the two production factories
// (DefaultSecureFactory on the secproc path, SecureLaunchFactory on the shell
// v2 path) apply to every child process. Both used to carry their own copy of
// this logic and they DRIFTED: SecureLaunchFactory built the child env from
// os.Environ(), DefaultSecureFactory built it from the caller-supplied
// spec.Env — and no secproc caller populates Env, so every process launched
// through shell_run / run_tests / github_* / diagnostics got an environment
// consisting of exactly HTTP_PROXY, HTTPS_PROXY and NO_PROXY. With no PATH,
// no HOME and no GOMODCACHE, `go version` answered "command not found" and
// `go test` answered "module cache not found".
//
// The env semantics MUST be identical on both paths: both spawn the same class
// of untrusted program on behalf of the same model, and a variable that is
// safe to expose on one path is safe on the other. Unifying them here is what
// makes that statement checkable rather than aspirational.
type childLaunchPosture struct {
	// Policy being non-nil means an egress policy is in force, so a proxy URL
	// must be published to the child even when ProxyURL is empty.
	Policy   *netpolicy.Policy
	ProxyURL string
	Sandbox  sandbox.Sandbox
}

// proxy resolves the proxy endpoint published to the child. A policy with no
// endpoint configured must still not let the child talk directly, so it points
// at a dead local port rather than leaving the vars empty (which exec would
// hand to the child as "no proxy").
func (p childLaunchPosture) proxy() string {
	if p.Policy != nil && p.ProxyURL == "" {
		return "http://127.0.0.1:0"
	}
	return p.ProxyURL
}

// env builds the child environment: the host env as the baseline, caller
// entries layered on top (exec keeps the last duplicate, so they win), and
// netpolicy.PrepareEnv run over the result so any inherited or smuggled-in
// proxy variable is stripped and replaced by the managed ones.
//
// Starting from the host is deliberate and is the whole point of this helper:
// a child that cannot resolve `go`, `node` or `gh` through PATH — or read
// ~/.config/gh through HOME — is not "sandboxed", it is broken. The isolation
// boundary yanshi enforces today is the guard layer plus the network policy,
// neither of which depends on an empty environment.
func (p childLaunchPosture) env(callerEnv []string) []string {
	proxy := p.proxy()
	env := netpolicy.ManagedEnv(proxy)
	if len(callerEnv) > 0 {
		env = netpolicy.PrepareEnv(append(env, callerEnv...), proxy)
	}
	return env
}

// prepare computes the spec handed to the OS factory without spawning
// anything, so the env and sandbox decisions are assertable on their own.
// Order matters: env first (the sandbox may then add to it), sandbox second
// (it may rewrite argv, e.g. wrap the command in bwrap/sandbox-exec).
//
// tier is the access class requested for THIS invocation; callers that have no
// per-invocation tier pass the sandbox's globally requested one.
func (p childLaunchPosture) prepare(ctx context.Context, spec LaunchSpec, tier sandbox.AccessTier) (LaunchSpec, error) {
	spec.Env = p.env(spec.Env)
	if p.Sandbox == nil {
		return spec, nil
	}
	// The sandbox seam speaks *exec.Cmd, but the spawn happens inside the OS
	// factory. Prepare a stand-in Cmd carrying the same fields, let the
	// backend mutate it, then copy the mutations back into the spec — that is
	// what makes an argv-wrapping or env-injecting Phase 1+ backend take
	// effect here. Phase 0's Prepare is a no-op, so the copy-back is identity.
	cmd := &exec.Cmd{Path: spec.Program, Args: append([]string{spec.Program}, spec.Args...), Dir: spec.Dir, Env: spec.Env}
	cs := sandbox.CommandSpec{Path: spec.Program, Args: spec.Args, Dir: spec.Dir, Tier: tier}
	if err := p.Sandbox.Prepare(ctx, cmd, cs); err != nil {
		return LaunchSpec{}, err
	}
	spec.Program, spec.Dir, spec.Env = cmd.Path, cmd.Dir, cmd.Env
	if len(cmd.Args) > 0 {
		spec.Args = cmd.Args[1:]
	}
	return spec, nil
}
