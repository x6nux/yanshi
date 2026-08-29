package shell

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
)

// DefaultSecureFactory implements secproc.Factory by wiring together the OS
// ProcessFactory (Task 18), the sandbox adapter (Task 10, Phase 0 stub), and
// the network policy (Task 11). It is the only production secproc.Factory —
// tests inject spy factories through the same interface.
//
// Dep direction: shell → secproc / sandbox / netpolicy. No cycle is created
// because secproc is a leaf (it only imports guard + sandbox), and the
// Authorizer seam in secproc lets tools register its real Authorize without
// secproc importing tools.
type DefaultSecureFactory struct {
	OS       ProcessFactory
	Policy   *netpolicy.Policy
	ProxyURL string
	// SOCKSURL, CAFile and Snapshot are the rest of the launch posture; see
	// childLaunchPosture for what each one does and when it is empty.
	SOCKSURL string
	CAFile   string
	Sandbox  sandbox.Sandbox
	Snapshot Snapshot
}

// posture builds the shared launch posture, in one place, for the same reason
// SecureLaunchFactory.posture exists: a second struct literal is a second
// thing to forget a field in, and a forgotten field here is a child launched
// under a posture nobody declared.
func (f DefaultSecureFactory) posture() childLaunchPosture {
	return childLaunchPosture{
		Policy:   f.Policy,
		ProxyURL: f.ProxyURL,
		SOCKSURL: f.SOCKSURL,
		CAFile:   f.CAFile,
		Sandbox:  f.Sandbox,
		Snapshot: f.Snapshot,
	}
}

// Start runs the full spawn pipeline:
//  1. Build the child environment through the shared childLaunchPosture: host
//     env as the baseline, caller-supplied spec.Env layered on top, credential
//     variables stripped unless spec.AllowEnv names them, inherited proxy vars
//     stripped and the managed HTTP_PROXY/HTTPS_PROXY/NO_PROXY appended.
//  2. Run the Sandbox seam with THIS invocation's spec.UseSandboxTier (the
//     field every secproc caller already populates; it used to be dropped on
//     the floor together with f.Sandbox).
//  3. Delegate to the OS factory and wrap the Console into the io.Reader
//     shape secproc.StartedProcess exposes.
//
// Step 3 requests SeparateStderr: every secproc caller on this path either
// parses the child's stdout (git_status, git_diff, run_tests, diagnostics,
// github_*) or re-merges the two itself for display (shell_run). This factory
// used to report Stderr as a discardReader while the OS factory concatenated
// stderr onto stdout, so commandResult.Stderr was permanently empty in
// production and git's warnings were parsed as porcelain records.
//
// Step 1 previously started from spec.Env alone. No secproc caller populates
// Env, so the child ended up with three proxy variables and nothing else — no
// PATH, no HOME, no GOMODCACHE — which is why `go version` used to answer
// "command not found" from shell_run. See childLaunchPosture for the full
// story.
//
// Fails closed when OS is nil — never silently skips the spawn.
func (f DefaultSecureFactory) Start(ctx context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	if f.OS == nil {
		return nil, fmt.Errorf("shell: DefaultSecureFactory.OS is nil (fail-closed)")
	}
	posture := f.posture()
	launch, err := posture.prepare(ctx, LaunchSpec{
		ShellName:      spec.Shell,
		Env:            spec.Env,
		AllowEnv:       spec.AllowEnv,
		Command:        spec.Shell,
		Program:        spec.Program,
		Args:           spec.Args,
		Dir:            spec.Dir,
		PTY:            false,
		SeparateStderr: true,
	}, spec.UseSandboxTier)
	if err != nil {
		return nil, err
	}
	// Step 1b: install the elevation shims. After prepare because it edits the
	// PATH prepare built; before the spawn because the child reads that PATH at
	// exec. See childLaunchPosture.interceptElevation.
	launch, stopBroker := posture.interceptElevation(ctx, launch, spec.Workdir)
	proc, console, err := f.OS.Start(ctx, launch)
	if err != nil {
		stopBroker()
		return nil, err
	}
	// Step 2b: the post-spawn half of the sandbox seam, for backends whose
	// mechanism attaches to a running process rather than rewriting its argv
	// (Windows Job Objects). This is the only correct window — the process
	// exists, and proc.Wait has not run, so the pid is unambiguous.
	//
	// A failure means the child could not be contained and the backend has
	// already terminated it. The console is torn down and the error returned
	// rather than handing back a StartedProcess the capability report claims is
	// inside a container. See childLaunchPosture.postStart.
	if err := posture.postStart(proc.PID()); err != nil {
		stopBroker()
		if console != nil {
			_ = console.Close()
		}
		return nil, err
	}
	// proc.Wait is the reaper: Process.Wait already guarantees the
	// *exec.ExitError-on-non-zero contract StartedProcess.Wait documents, so
	// it forwards verbatim. Dropping it here (as an earlier revision did)
	// leaves callers with an unreapable process — see
	// TestRunSecureCaptureWithProductionFactory.
	//
	// Stdin is the Console itself, which is a ReadWriteCloser over the child's
	// stdin pipe. Closing it therefore tears down the whole console rather than
	// half-closing stdin; secproc.StartedProcess.Stdin documents that as a
	// factory-defined property, and the one caller that writes to it (the ACP
	// delegation path) only closes at teardown.
	// The broker outlives the spawn and dies with the reap. Tying it to Wait
	// rather than to ctx alone is what bounds the temp directory and the
	// listener goroutine: on the shell v2 path the launch context is
	// deliberately detached from the turn (context.WithoutCancel in
	// Manager.Start), so a ctx-only teardown would keep one of each alive per
	// session until the whole manager shut down.
	return &secproc.StartedProcess{
		Wait:   func() error { defer stopBroker(); return proc.Wait() },
		PID:    proc.PID(),
		Stdout: consoleReader{console},
		Stderr: separatedStderr(console),
		Stdin:  console,
	}, nil
}

// separatedStderr returns the child's own stderr stream when the OS factory
// honored LaunchSpec.SeparateStderr, and an empty stream otherwise.
//
// The fallback is correct rather than merely tolerable: a factory that ignored
// the flag put BOTH streams on the console, so those bytes already reach the
// caller through StartedProcess.Stdout. Returning a second live reader here
// would duplicate them; returning the empty one keeps
// secproc.StartedProcess's "Stderr is never a copy" contract.
func separatedStderr(console Console) io.Reader {
	if sc, ok := console.(StderrConsole); ok {
		if r := sc.Stderr(); r != nil {
			return r
		}
	}
	return discardReader{}
}

// discardReader is the empty stderr stream handed to callers when the OS
// factory merged both halves into the console (PTY backends, test doubles
// that ignore LaunchSpec.SeparateStderr). It satisfies io.Reader so
// StartedProcess.Stderr stays a uniform, never-nil type.
type discardReader struct{}

func (discardReader) Read(p []byte) (int, error) { return 0, io.EOF }

// consoleReader adapts shell.Console to io.Reader so secproc.StartedProcess
// callers (e.g. legacy shell_run streamer) can pump stdout without knowing
// about the shell.Console interface. Non-EOF errors from Console.Read are
// preserved verbatim so a real I/O failure does not get rewritten to EOF
// (which would silently truncate the stream from the caller's perspective).
type consoleReader struct {
	r Console
}

func (c consoleReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	if err != nil {
		return n, err
	}
	return n, nil
}

// UnsandboxedSecureFactory is the secproc.Factory used when no sandbox and no
// network policy have been configured: a real spawn through the same
// DefaultSecureFactory pipeline, with the two optional seams left out.
//
// It exists so "no factory in context" can stop being a representable state.
// Before W-B-02 an unbound factory made shell_run fall back to its own raw
// exec.Cmd pipe, which meant a composition root that simply forgot to pass the
// field produced a spawn with no credential scrub and no managed proxy env —
// indistinguishable, from the outside, from a deployment that had chosen not to
// sandbox. Substituting this factory keeps the credential scrub, the env
// baseline and the reaper contract while omitting only what the caller has not
// configured.
//
// It is NOT a bypass of authorization: every caller reaches a Factory through
// secproc.Launch, which Authorizes before any factory is consulted.
func UnsandboxedSecureFactory() secproc.Factory {
	return DefaultSecureFactory{OS: OSProcessFactory{}}
}
