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
	Sandbox  sandbox.Sandbox
}

// Start runs the full spawn pipeline:
//  1. Build the child environment through the shared childLaunchPosture: host
//     env as the baseline, caller-supplied spec.Env layered on top, inherited
//     proxy vars stripped and the managed HTTP_PROXY/HTTPS_PROXY/NO_PROXY
//     appended.
//  2. Run the Sandbox seam with THIS invocation's spec.UseSandboxTier (the
//     field every secproc caller already populates; it used to be dropped on
//     the floor together with f.Sandbox).
//  3. Delegate to the OS factory and wrap the Console into the io.Reader
//     shape secproc.StartedProcess exposes.
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
	posture := childLaunchPosture{Policy: f.Policy, ProxyURL: f.ProxyURL, Sandbox: f.Sandbox}
	launch, err := posture.prepare(ctx, LaunchSpec{
		ShellName: spec.Shell,
		Env:       spec.Env,
		Command:   spec.Shell,
		Program:   spec.Program,
		Args:      spec.Args,
		Dir:       spec.Dir,
		PTY:       false,
	}, spec.UseSandboxTier)
	if err != nil {
		return nil, err
	}
	proc, console, err := f.OS.Start(ctx, launch)
	if err != nil {
		return nil, err
	}
	// proc.Wait is the reaper: Process.Wait already guarantees the
	// *exec.ExitError-on-non-zero contract StartedProcess.Wait documents, so
	// it forwards verbatim. Dropping it here (as an earlier revision did)
	// leaves callers with an unreapable process — see
	// TestRunSecureCaptureWithProductionFactory.
	return &secproc.StartedProcess{
		Wait:   proc.Wait,
		PID:    proc.PID(),
		Stdout: consoleReader{console},
		Stderr: discardReader{},
	}, nil
}

// discardReader is the no-op stderr sink for spawns that merge stdout+stderr
// at the OS layer (the OSProcessFactory path uses io.MultiReader). It
// satisfies io.Reader so StartedProcess.Stderr stays a uniform type.
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
