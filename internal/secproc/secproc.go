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
	"os/exec"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/sandbox"
)

// SecureProcessSpec is the single shape callers use to describe a process
// spawn. Tool/Shell echo the guard.Action fields so the Authorizer can match
// them against the profile; Program/Args/Dir/Env are the exec payload;
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
}

// StartedProcess is what Factory.Start returns: the underlying *exec.Cmd for
// Wait/Kill, the PID for logging, and merged/-separated stdout/stderr readers
// the caller can pump into a ring buffer or stream back to the TUI.
type StartedProcess struct {
	Cmd    *exec.Cmd
	PID    int
	Stdout io.Reader
	Stderr io.Reader
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
//     tools.Authorize already uses.
//  2. If no Factory is in context, fail closed (returns a factory-missing
//     error rather than silently skipping the spawn).
//  3. Factory.Start receives spec verbatim; spec.Program/Args MUST come from
//     shell.ShellArgv (Task 15) when spec.Shell is set.
//
// The two fail-closed gates are the entire security value of this type: they
// guarantee that no spawn site can forget to Authorize, and no spawn site can
// silently proceed when the Factory wiring is missing.
func Launch(ctx context.Context, spec SecureProcessSpec) (*StartedProcess, error) {
	if currentAuthorizer == nil {
		return nil, ErrNoAuthorizer
	}
	if err := currentAuthorizer(ctx, guard.Action{Tool: spec.Tool, Shell: spec.Shell}, ""); err != nil {
		return nil, err
	}
	f, ok := FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("secproc: no Factory in context (fail-closed)")
	}
	return f.Start(ctx, spec)
}
