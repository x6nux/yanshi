package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/x6nux/yanshi/internal/execbroker"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
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
	// SOCKSURL and CAFile carry the other two halves of the managed proxy: the
	// SOCKS endpoint for clients that do not read http_proxy, and the
	// inspection root for children that must trust the forged chain. Both are
	// empty unless bootstrap started the corresponding facility.
	SOCKSURL string
	CAFile   string
	Sandbox  sandbox.Sandbox
	// Snapshot is the operator's login-shell environment (W-B-21). The zero
	// value means capture was off or failed, and layering it is then a no-op.
	Snapshot Snapshot
}

// proxy resolves the managed-proxy endpoints published to the child.
//
// No placeholder. A policy with no proxy behind it used to publish
// http://127.0.0.1:0, which looked like enforcement and was a black hole: it
// broke proxy-aware clients, let everything else straight out, and recorded no
// decision anywhere. Half-enforcement is worse than none, because it reads as
// containment.
//
// Either a real managed proxy is running and children are pointed at it, or
// nothing is published and the posture is honestly unenforced.
//
// # What the published variables buy, and what they do not
//
// When ProxyURL is set, security.network IS evaluated per host for every
// request a child makes THROUGH the proxy, and every decision is audited.
// SOCKSURL widens which clients that covers — ALL_PROXY reaches ssh
// ProxyCommand and the many clients that never read http_proxy — and CAFile,
// when HTTPS inspection is on, is what lets the proxy judge method as well as
// host.
//
// It is still an environment variable, not a boundary:
//
//   - It only stops programs that HONOR the variables. A raw socket, its own
//     DNS, or a proxy set from a config file wins over anything published
//     here. On Linux the seccomp filter under a landlock sandbox closes the
//     raw-socket half (ADR-0014's amendment); on darwin and Windows it is
//     open.
//   - It reaches only THESE launchers. ACP agent CLIs (internal/acp/spawn.go),
//     stdio MCP servers (internal/mcp/manager.go), LSP servers
//     (internal/lsp/manager.go), the skills installer and `gh`/`git` spawned
//     from cmd/yanshi all build their env from os.Environ() directly, so they
//     see no managed proxy AND inherit whatever proxy the operator's shell had.
//     Two of those (ACP, MCP-over-stdio to a remote model) need real egress to
//     function at all, which is why they were never simply cut off.
//
// The http variables are published in both upper and lower case — that is a
// correctness requirement rather than caution, because curl ignores uppercase
// HTTP_PROXY for plain http:// URLs. See netpolicy.PrepareEnvFor.
//
// The policy is ALSO enforced for yanshi's own in-process HTTP (web_fetch and
// web_search go through netpolicy.NewTransport/PolicyDialer); that path never
// depended on these variables.
func (p childLaunchPosture) proxy() netpolicy.ManagedProxy {
	return netpolicy.ManagedProxy{HTTPURL: p.ProxyURL, SOCKSURL: p.SOCKSURL, CAFile: p.CAFile}
}

// env builds the child environment: the host env as the baseline, caller
// entries layered on top (exec keeps the last duplicate, so they win),
// credential-bearing variables stripped under allowEnv, and
// netpolicy.PrepareEnvFor run over the result so any inherited or smuggled-in
// proxy variable is stripped and replaced by the managed ones.
//
// Starting from the host is deliberate and is the whole point of this helper:
// a child that cannot resolve `go`, `node` or `gh` through PATH — or read
// ~/.config/gh through HOME — is not "sandboxed", it is broken.
//
// Inheriting the host env used to cost nothing because the guard layer was the
// whole isolation boundary. It cost exactly one thing: every provider API key,
// cloud credential and VCS token in yanshi's own process environment reached
// every untrusted child, so `printenv` from shell_run put them in the model's
// transcript and therefore into the next request to the provider. The guard's
// auto-approval prompt has named that risk category since it was written; the
// scrub is the code side of it. Structural variables (PATH, HOME, LANG, …) are
// never candidates — see secrets.ScrubEnv.
//
// allowEnv is the caller's per-spawn declaration of the credentials THIS
// program legitimately needs (gh wants GH_TOKEN; shell_run wants nothing).
// Empty means strip everything, which is the correct default.
//
// The credential scrub runs ONCE, over host+caller entries, BEFORE the managed
// proxy variables are appended. Calling netpolicy.PrepareEnvFor first and
// netpolicy.ScrubCredentials over ITS result reads equivalently and is not:
// the scrub would then re-inspect the proxy entries this function just
// published, and a proxy URL carrying inline basic-auth credentials
// (http://user:pass@proxy) is a shape LooksLikeCredentialValue recognises —
// so the managed variables would be stripped from the child that needs them
// to reach the proxy at all. See internal/shell/posture_egress_test.go's
// TestProxyCredentialsSurviveTheScrub, which pins exactly this ordering
// against this method.
//
// The proxy variables this function appends are NOT a containment boundary:
// see proxy() for exactly which clients and which launchers they reach.
//
// The login-shell snapshot (W-B-21) is layered in BEFORE the credential scrub,
// which is the only correct order: an rc file that exports an API key is one
// of the likeliest sources of a credential in this environment, and applying
// the snapshot afterwards would hand the child exactly the secrets the scrub
// exists to remove. Sitting under the scrub means the snapshot is subject to
// the same policy as yanshi's own environment, with no second allowlist.
func (p childLaunchPosture) env(callerEnv []string, allowEnv []string) []string {
	base := os.Environ()
	if len(callerEnv) > 0 {
		base = append(base, callerEnv...)
	}
	base = p.Snapshot.Apply(base)
	kept, _ := netpolicy.ScrubCredentials(base, netpolicy.CredentialPolicy{AllowEnv: allowEnv})
	return netpolicy.PrepareEnvFor(kept, p.proxy())
}

// prepare computes the spec handed to the OS factory without spawning
// anything, so the env and sandbox decisions are assertable on their own.
// Order matters: env first (the sandbox may then add to it), sandbox second
// (it may rewrite argv, e.g. wrap the command in bwrap/sandbox-exec).
//
// tier is the access class requested for THIS invocation; callers that have no
// per-invocation tier pass the sandbox's globally requested one.
func (p childLaunchPosture) prepare(ctx context.Context, spec LaunchSpec, tier sandbox.AccessTier) (LaunchSpec, error) {
	spec.Env = p.env(spec.Env, spec.AllowEnv)
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
	// The Windows backend's mutation is not in argv or the environment: a
	// restricted token is an argument to process creation, so it arrives on the
	// stand-in's SysProcAttr. Without this line it would be discarded here and
	// every child would run under the unrestricted token while Report() claimed
	// os-isolated — the shape sandbox/poststart.go documents for CreationFlags,
	// which is still open. sandboxTokenFromCmd returns 0 on every non-Windows
	// platform, so this is an unconditional assignment of a zero.
	spec.ProcessToken = sandboxTokenFromCmd(cmd)
	return spec, nil
}

// interceptElevation installs the elevation shims for one launch and returns
// the spec pointed at them, plus the closer that tears the broker down.
//
// # Why the two production factories both call it here
//
// The shim only works if it is on the child's PATH, and PATH is decided by
// prepare() — so this has to run after it and before the spawn. Putting it in
// prepare() itself would have been tidier and is wrong: prepare has no teardown
// and this owns a listener, a goroutine and a temp directory. The closer is
// therefore returned, and both Start methods wire it into the reaper.
//
// # workdir is the outer launch's, not the shim's
//
// The shim reports its own working directory, which is where the script line
// ran. That travels into the approval dialog because it is what an operator
// wants to see. It deliberately does NOT become guard.Action.Workdir: that
// field is the project boundary the destructive classifier measures deletions
// against, and letting a child move it by cd'ing first would turn
// `sudo rm -rf /x` into an in-scope deletion. An empty workdir — which is what
// the shell v2 path has, since LaunchSpec carries no equivalent — means
// "unknown", and the classifier treats every absolute target as out of scope.
// That is the fail-safe direction.
//
// # Failure is not fatal
//
// A platform without symlinks, a temp dir that cannot be created, a socket that
// will not bind: none of those are reasons to refuse to run the command the
// operator approved. The launch proceeds with no shims, which is exactly the
// behaviour that existed before this control, and the reason goes to stderr
// once rather than into an error the caller would have to classify.
func (p childLaunchPosture) interceptElevation(
	ctx context.Context, spec LaunchSpec, workdir string,
) (LaunchSpec, func()) {

	noop := func() {}
	exe, err := os.Executable()
	if err != nil {
		return spec, noop
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return spec, noop
	}
	server, err := execbroker.Listen(ctx, exe, elevationDecider(workdir))
	if err != nil {
		if !errors.Is(err, execbroker.ErrUnsupported) {
			warnElevationOnce.Do(func() {
				fmt.Fprintf(os.Stderr,
					"yanshi: nested privilege elevation is NOT being intercepted: %v\n", err)
			})
		}
		return spec, noop
	}
	spec.Env = execbroker.PrependShimDir(append(spec.Env, server.Env()...), server.ShimDir())
	return spec, func() { _ = server.Close() }
}

// warnElevationOnce keeps a host that simply cannot host the shims from
// printing a line per spawn.
var warnElevationOnce sync.Once

// elevationDecider adjudicates one intercepted elevation through the same
// authorizer a top-level tool call goes through.
//
// It calls secproc.Authorize rather than reimplementing a check, so the nested
// `sudo` is judged by the operator's profile, reaches the same approval
// callback, and is classified by the same destructive-deletion rules. A second
// decision path here would be a second, quieter policy.
//
// The error is returned verbatim to the child, which prints it: an operator
// reading a script's output sees why line 3 failed rather than a bare 126.
func elevationDecider(workdir string) execbroker.Decider {
	return func(ctx context.Context, req execbroker.Request) error {
		display, err := json.Marshal(struct {
			Program string   `json:"program"`
			Args    []string `json:"args"`
			Dir     string   `json:"dir"`
			Nested  bool     `json:"nested_elevation"`
		}{req.Program, req.Args, req.Dir, true})
		if err != nil {
			// The dialog loses its detail; the decision must still be made.
			display = []byte("{}")
		}
		return secproc.Authorize(ctx, guard.Action{
			Tool:    "shell_run",
			Shell:   execbroker.CommandLine(req.Program, req.Args),
			Workdir: workdir,
		}, string(display))
	}
}

// postStart runs the sandbox's optional post-spawn step for a process that has
// just started and has NOT yet been reaped.
//
// # Why this exists at all
//
// prepare() hands the backend a command that has not been started, which suits
// an argv-rewriting backend (macOS re-heads it with sandbox-exec) and cannot
// suit a backend whose mechanism is a kernel object a RUNNING process must be
// attached to. Windows Job Objects are the second kind: AssignProcessToJobObject
// takes a process handle, so it has nothing to take until the process exists.
//
// The obvious alternative — CREATE_SUSPENDED, assign, resume — is not reachable
// from prepare(): the *exec.Cmd it builds is a stand-in and only Program, Dir,
// Env and Args are copied back into the LaunchSpec. SysProcAttr is not among
// them and LaunchSpec has no field to carry it, so CreationFlags set there would
// be silently dropped. See sandbox.PostStartSandbox's file header.
//
// # Ordering
//
// Callers MUST call this after Start and BEFORE anything reaps the process.
// On Windows a pid stays reserved only while a handle to the process object is
// open, and the Go runtime holds one from Start until Wait; call this after the
// reap and the pid may already name a different process, which the backend
// would then bind into a kill-on-close job and terminate at shutdown.
//
// # Failure is fatal to the launch
//
// A non-nil error means the child is NOT contained, and the backend has already
// terminated it (that is its half of the contract). Callers must tear down the
// console and propagate: returning a running process while Report() claims
// CanKillTree is the over-claim the sandbox package exists to prevent.
//
// A backend that does not implement the optional interface — darwin, linux, the
// degraded stub, every test double — makes this a no-op via the failed type
// assertion, which is why adding the seam did not have to touch them.
func (p childLaunchPosture) postStart(pid int) error {
	if p.Sandbox == nil {
		return nil
	}
	ps, ok := p.Sandbox.(sandbox.PostStartSandbox)
	if !ok {
		return nil
	}
	return ps.PostStart(pid)
}
