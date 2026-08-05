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
//
// ⚠️ Phase 0: that dead port is the ONLY case in production. bootstrap.Build
// constructs a netpolicy.Policy unconditionally and leaves ProxyURL empty, and
// netpolicy.Proxy is never started, so every child launched through these two
// factories gets http(s)_proxy=http://127.0.0.1:0. This is a BLACK HOLE, not
// an egress policy:
//
//   - It consults nothing. security.network's default/allow/deny/allow_private
//     do not reach this decision — `default: allow` with `allow: ["*"]` still
//     gets the dead port.
//   - It only stops programs that honor the proxy variables (curl, gh, go mod
//     download, npm, git-over-HTTP). Anything speaking raw sockets, SSH or its
//     own DNS is unaffected, so it blocks the well-behaved tools and leaves the
//     actual exfiltration paths open.
//   - It produces no decision record, so a blocked fetch surfaces to the
//     operator only as "connect to 127.0.0.1 port 0 failed".
//   - It reaches only THESE launchers. ACP agent CLIs (internal/acp/spawn.go),
//     stdio MCP servers (internal/mcp/manager.go), LSP servers
//     (internal/lsp/manager.go), the skills installer and `gh`/`git` spawned
//     from cmd/yanshi all build their env from os.Environ() directly, so they
//     see no managed proxy AND inherit whatever proxy the operator's shell had.
//     Two of those (ACP, MCP-over-stdio to a remote model) need real egress to
//     function at all, which is why the dead port is not simply applied there.
//
// Even within these launchers it only reaches those that use env at all: an
// invocation that sets its own proxy via a config file or CLI flag wins.
//
// The variables are published in both upper and lower case — that is a
// correctness requirement rather than caution, because curl ignores uppercase
// HTTP_PROXY for plain http:// URLs. See netpolicy.PrepareEnv.
//
// The policy IS enforced for yanshi's own in-process HTTP (web_fetch and
// web_search go through netpolicy.NewTransport/PolicyDialer) — the gap is
// subprocess-only. Closing it means actually starting netpolicy.Proxy and
// setting ProxyURL, which is W5's work package; do not paper over it here.
func (p childLaunchPosture) proxy() string {
	// No placeholder. A policy with no proxy behind it used to publish
	// http://127.0.0.1:0, which looked like enforcement and was a black hole:
	// it broke proxy-aware clients, let everything else straight out, and
	// recorded no decision anywhere. Half-enforcement is worse than none,
	// because it reads as containment.
	//
	// Either a real managed proxy is running and children are pointed at it,
	// or nothing is published and the posture is honestly unenforced.
	return p.ProxyURL
}

// env builds the child environment: the host env as the baseline, caller
// entries layered on top (exec keeps the last duplicate, so they win), and
// netpolicy.PrepareEnv run over the result so any inherited or smuggled-in
// proxy variable is stripped and replaced by the managed ones.
//
// Starting from the host is deliberate and is the whole point of this helper:
// a child that cannot resolve `go`, `node` or `gh` through PATH — or read
// ~/.config/gh through HOME — is not "sandboxed", it is broken.
//
// The isolation boundary yanshi enforces today is the guard layer, and that is
// the whole list. It does not depend on an empty environment, which is why
// inheriting the host env costs nothing. The proxy variables this function
// appends are NOT a second boundary: see proxy() for why they are a black hole
// rather than an egress policy in Phase 0.
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
