// Package secproc is the single subprocess-launch entry point yanshi uses.
// Every exec.CommandContext in the codebase that spawns an untrusted program
// (shell_run, ACP agents, future shell v2) MUST go through secproc.Launch so
// the Authorize firewall is enforced uniformly. Internal packages that need
// to spawn (tools, shell, acp) read the Factory from context and delegate to
// it; the concrete factory (DefaultSecureFactory, Task 19) lives in
// internal/shell to keep the dep cycle broken.
//
// Dep direction:
//
//	secproc ←── guard
//	         ←── sandbox
//	tools   → secproc       (registers real Authorizer at init)
//	shell   → secproc       (DefaultSecureFactory implements Factory)
//
// secproc never imports tools — the Authorizer seam is a function variable
// populated by tools' package init, so secproc stays a leaf package.
package secproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/sandbox"
)

// SecureProcessSpec is the single shape callers use to describe a process
// spawn. Tool/Shell/Interpreter echo the guard.Action fields so the Authorizer
// can match them against the profile; Program/Args/Dir/Env are the exec payload;
// UseSandboxTier tells the factory which access class the sandbox should
// enforce for THIS invocation (may be more permissive than the global
// Config.Tier when a privileged helper is allowed).
type SecureProcessSpec struct {
	Tool           string
	Shell          string
	Program        string
	Args           []string
	Dir            string
	Env            []string
	UseSandboxTier sandbox.AccessTier

	// Workdir is the in-scope boundary the guard's destructive-deletion
	// dimension compares deletion targets against (guard.Action.Workdir). It is
	// SEPARATE from Dir on purpose: Dir is where the child actually runs and a
	// caller may legitimately point it anywhere, while Workdir is the project
	// root the operator's policy is written about. Feeding Dir here would let a
	// tool argument move the boundary — `{"workdir":"/","command":"rm -rf /x"}`
	// would become an in-scope deletion.
	//
	// Empty means "unknown", which the classifier treats as fail-safe (every
	// absolute target is out of scope).
	Workdir string

	// Interpreter is the shell LANGUAGE Shell is written in — the resolved
	// interpreter program name, which for a shell spawn is exactly Program.
	// It is a separate field rather than a reuse of Program because Program is
	// an arbitrary executable for every other caller (an ACP agent CLI, a
	// diagnostics helper), and the guard must not read those as shell
	// languages. Empty means "a POSIX shell", the pre-W-B-05 behaviour.
	//
	// It travels to guard.Action.Interpreter, which is what picks the
	// segmenter. See that field for why it matters.
	Interpreter string

	// ArgsJSON is the tool's raw argument JSON, forwarded to the Authorizer so
	// the approval dialog shows the operator what they are approving. Display
	// only: it is never written to the audit log (see tools.Authorize).
	ArgsJSON string

	// AllowEnv names the credential-bearing environment variables THIS
	// invocation legitimately needs. Empty (the zero value) means the child
	// gets no credentials at all, which is the correct default for every
	// untrusted program: shell_run, ACP agents and the model-reachable
	// diagnostics tools have no business reading the operator's provider keys.
	//
	// Declared per-spawn rather than per-profile because the need is a property
	// of the PROGRAM, not of the permission level: `gh` cannot authenticate
	// without GH_TOKEN no matter how restrictive the profile, and shell_run has
	// no claim on it no matter how permissive. See netpolicy.CredentialPolicy
	// for why the allowlist is by NAME and can never be by value shape.
	AllowEnv []string
}

// StartedProcess is what Factory.Start returns: a reaper the caller must
// invoke to collect the exit status, the PID for logging, and the child's two
// output streams.
//
// Stdout and Stderr are SEPARATE on purpose, because this type has two classes
// of consumer with incompatible needs:
//
//   - Parsers (git porcelain-v2 -z, `go test -json`, `gh --json`) read Stdout
//     alone. A single byte of stderr spliced into that buffer is not noise, it
//     is a fabricated record: git's own "warning: …" line, split on NUL by the
//     porcelain parser, becomes an extra changed file that does not exist.
//   - Display (shell_run streaming to the TUI) wants both streams, stderr
//     included rather than dropped. Those callers use MergedOutput. Note that
//     this is approximately, not exactly, what a terminal would show: the
//     merge preserves line integrity but not the child's write order — see
//     MergeOutput.
//
// A Factory that cannot separate the two MUST put everything on Stdout rather
// than duplicating it — Stderr is allowed to be an empty stream, never a copy.
//
// Wait is a func rather than an *exec.Cmd because not every Factory is
// exec-backed (the PTY path hands back a shell.Process implementation that
// owns its own reaping). Factories MUST populate it: a nil Wait means the
// process can never be reaped, and callers fail closed rather than deref it.
// Contract: Wait returns nil on a clean exit and *exec.ExitError on a
// non-zero one, so callers can recover the exit code via errors.As.
type StartedProcess struct {
	Wait   func() error
	PID    int
	Stdout io.Reader
	Stderr io.Reader

	// Stdin is the child's standard input, or nil when the Factory could not
	// supply one. It is OPTIONAL because most callers here are one-shot
	// capture spawns (git, go test, gh) that write nothing to the child, and
	// requiring every Factory — including the test doubles — to mint a writer
	// for a stream nobody uses would be pure ceremony.
	//
	// It exists because one class of untrusted program is inherently
	// bidirectional: an ACP agent speaks newline-delimited JSON-RPC over
	// stdin/stdout for the whole life of the session. Without a writer here
	// that program cannot go through Launch at all, which is how
	// internal/acp ended up with its own exec.CommandContext outside the
	// Authorize firewall. Callers that need it MUST nil-check and fail closed
	// rather than assume it is present.
	//
	// CLOSE SEMANTICS ARE FACTORY-DEFINED AND MAY NOT BE A HALF-CLOSE. The
	// production factory hands back the console itself, so Close tears down
	// stdout and stderr along with stdin rather than merely signalling EOF to
	// the child. A caller that wants to close stdin and keep draining output
	// must not use this; the ACP path closes it only at teardown, where the
	// wider effect is what it wanted anyway.
	Stdin io.WriteCloser
}

// MergedOutput returns the display stream: stdout and stderr interleaved as
// they arrive, for callers that render the child's output rather than parse
// it. See MergeOutput for the concurrency contract, and StartedProcess for why
// parsers must not use this.
func (p *StartedProcess) MergedOutput() io.ReadCloser {
	return MergeOutput(p.Stdout, p.Stderr)
}

// MergeOutput interleaves two process streams into the single line stream a
// human (or a model) reads.
//
// Reading them CONCURRENTLY is the whole point. io.MultiReader(stdout, stderr)
// does not merge, it concatenates: stderr is not read at all until stdout hits
// EOF. That reorders every diagnostic to the end of the output, and a child
// that fills its stderr pipe buffer (64 KiB on Linux) before closing stdout
// blocks forever on write — the caller only notices when its timeout fires and
// kills the process. Copying both halves into an io.Pipe reads each as it
// arrives; io.Pipe serializes Writes, so one stream's chunk is never spliced
// into the middle of the other's.
//
// The returned reader reaches EOF only after BOTH sources have, which is what
// preserves the drain-to-EOF-then-Wait ordering callers rely on: (*exec.Cmd).
// Wait closes the child's pipes, so a caller that reaps before draining reads
// "file already closed" and silently truncates the output.
//
// What "interleaved" does and does not promise:
//
//   - It does NOT reproduce the child's chronological write order. Two
//     independent copier goroutines race, so a stdout chunk read first can be
//     written second. Measured against a child alternating numbered lines
//     between the two streams: ~440 of 4000 lines come out ahead of a line the
//     child wrote earlier, where the single-fd path scores exactly 0. Callers
//     that need true ordering must ask the OS factory for one fd (PTY /
//     SeparateStderr=false), not merge afterwards.
//   - It does guarantee no SPLICING: io.Pipe serializes Writes, so one
//     stream's chunk never lands inside the other's, and lines are not cut in
//     half (0 split lines in the same measurement).
//
// Closing the returned reader unblocks a copier parked in Write, but NOT one
// parked in its source's Read — that one exits when the source closes. In
// production the source is the child's pipe and (*exec.Cmd).Wait is what
// closes it, so the copiers are reclaimed by reaping, not by Close. A caller
// that abandons the stream without ever reaping leaks one goroutine per
// source; the fix is to call Wait, which every caller on this path does on
// every exit path. A nil source is skipped.
func MergeOutput(stdout, stderr io.Reader) io.ReadCloser {
	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	for _, src := range []io.Reader{stdout, stderr} {
		if src == nil {
			continue
		}
		wg.Add(1)
		go func(r io.Reader) {
			defer wg.Done()
			_, _ = io.Copy(pw, r)
		}(src)
	}
	go func() {
		wg.Wait()
		_ = pw.Close()
	}()
	return pr
}

// Factory is the per-process-launch strategy. The production implementation
// (DefaultSecureFactory, Task 19) wires netpolicy.PrepareEnv + Sandbox.Prepare
// + exec.Start; tests substitute a spy to assert the launch pipeline without
// spawning real processes.
type Factory interface {
	Start(context.Context, SecureProcessSpec) (*StartedProcess, error)
}

type factoryKey struct{}

// WithFactory binds f to ctx so Launch can find it. A nil f is a no-op
// (Launch then fails closed with "no Factory in context").
func WithFactory(ctx context.Context, f Factory) context.Context {
	if f == nil {
		return ctx
	}
	return context.WithValue(ctx, factoryKey{}, f)
}

// FromContext reads back a Factory bound by WithFactory.
func FromContext(ctx context.Context) (Factory, bool) {
	f, ok := ctx.Value(factoryKey{}).(Factory)
	return f, ok && f != nil
}

// Authorizer is the seam that binds the launcher to tools.Authorize without a
// dependency cycle: internal/tools registers its real Authorize once at boot
// (secproc.RegisterAuthorizer(tools.Authorize)), and secproc uses it through
// this interface. In tests the default zero-func returns ErrNoAuthorizer which
// makes Launch fail closed — no accidental bypass.
type Authorizer func(ctx context.Context, action guard.Action, argsJSON string) error

// ErrNoAuthorizer is returned by Launch when no Authorizer has been
// registered. The zero value of currentAuthorizer is nil, so the very first
// Launch in a process that didn't run tools' init fails closed rather than
// silently bypassing authorization.
var ErrNoAuthorizer = errors.New("secproc: no authorizer registered (fail-closed)")

// currentAuthorizer is the process-wide Authorizer. Set once by tools.init.
// Package-level rather than per-ctx because tools.Authorize already threads
// the profile / approval manager via context — the indirection here exists
// only to avoid the import cycle (secproc → tools would close tools → secproc
// via the launcher helper, so we invert via a function variable).
var currentAuthorizer Authorizer

// RegisterAuthorizer is called once from tools.init to bind the production
// Authorize. Tests that want to prove the no-authorizer fail-closed path
// typically use SwapAuthorizer (which returns the previous value) so they can
// restore it in a defer.
func RegisterAuthorizer(a Authorizer) { currentAuthorizer = a }

// SwapAuthorizer atomically replaces the current Authorizer and returns the
// previous one. The test package is external (secproc_test) so it cannot
// touch currentAuthorizer directly; this helper is the supported way to
// simulate "no authorizer registered" without leaving global state dirty.
func SwapAuthorizer(a Authorizer) Authorizer {
	prev := currentAuthorizer
	currentAuthorizer = a
	return prev
}

// Launch is the single entry point for any subprocess spawn in yanshi.
// Pipeline (each step is a fail-closed check):
//  1. Authorize via the registered Authorizer — HardDeny never reaches the
//     factory; Prompt may record an approval rule through the same path
//     tools.Authorize already uses. This is the ONLY authorization a caller
//     needs: a caller that also Authorizes the same guard.Action itself asks
//     the operator twice for one command, which is why shell_run hands its
//     Workdir/ArgsJSON down through the spec instead of keeping its own call.
//  2. If no Factory is in context, fail closed (returns a factory-missing
//     error rather than silently skipping the spawn).
//  3. Strip credential-bearing variables out of spec.Env under spec.AllowEnv,
//     so a caller cannot route a credential past the scrub by handing it in
//     explicitly. The Factory applies the same policy to the host-environment
//     baseline it builds — that is where the credentials actually come from,
//     since almost no caller populates Env.
//  4. Factory.Start receives the scrubbed spec; spec.Program/Args MUST come
//     from shell.ShellArgv (Task 15) when spec.Shell is set.
//
// The fail-closed gates are the entire security value of this type: they
// guarantee that no spawn site can forget to Authorize, no spawn site can
// silently proceed when the Factory wiring is missing, and no spawn site can
// hand an untrusted program a credential it did not declare.
func Launch(ctx context.Context, spec SecureProcessSpec) (*StartedProcess, error) {
	if currentAuthorizer == nil {
		return nil, ErrNoAuthorizer
	}
	if err := currentAuthorizer(ctx, guard.Action{
		Tool:        spec.Tool,
		Shell:       spec.Shell,
		Workdir:     spec.Workdir,
		Interpreter: spec.Interpreter,
	}, spec.ArgsJSON); err != nil {
		return nil, err
	}
	f, ok := FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("secproc: no Factory in context (fail-closed)")
	}
	spec, dropped := scrubSpecEnv(spec)
	auditCredentialScrub(spec, dropped)
	return f.Start(ctx, spec)
}
