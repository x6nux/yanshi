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
//  1. Strip inherited proxy vars and (when Policy is set) append the managed
//     HTTP_PROXY/HTTPS_PROXY/NO_PROXY entries via netpolicy.PrepareEnv.
//  2. Build a LaunchSpec that forwards the cleaned env (CB1/CB2 fix).
//  3. (Phase 0) Leave the Sandbox seam explicit — a real Phase 1+ adapter
//     would call f.Sandbox.Prepare(ctx, cmd, CommandSpec{Tier: spec.UseSandboxTier})
//     here.
//  4. Delegate to the OS factory and wrap the Console into the io.Reader
//     shape secproc.StartedProcess exposes.
//
// Fails closed when OS is nil — never silently skips the spawn.
func (f DefaultSecureFactory) Start(ctx context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	if f.OS == nil {
		return nil, fmt.Errorf("shell: DefaultSecureFactory.OS is nil (fail-closed)")
	}
	env := spec.Env
	if f.Policy != nil {
		proxyURL := f.ProxyURL
		if proxyURL == "" {
			proxyURL = "http://127.0.0.1:0"
		}
		env = netpolicy.PrepareEnv(env, proxyURL)
	} else {
		// Even without a policy we strip inherited proxy vars — silently
		// inheriting a developer's http_proxy is a known TOCTOU vector.
		env = netpolicy.PrepareEnv(env, "")
	}
	launch := LaunchSpec{
		ShellName: spec.Shell,
		Env:       env,
		Command:   spec.Shell,
		Program:   spec.Program,
		Args:      spec.Args,
		Dir:       spec.Dir,
		PTY:       false,
	}
	// Phase 0: Sandbox.Prepare is a no-op. Keep the seam explicit so a real
	// adapter (A1c follow-up) just calls f.Sandbox.Prepare here without
	// changing the call-site shape.
	_ = f.Sandbox
	proc, console, err := f.OS.Start(ctx, launch)
	if err != nil {
		return nil, err
	}
	return &secproc.StartedProcess{
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
