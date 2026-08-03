package orchestrator

import (
	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
	"github.com/x6nux/yanshi/internal/shell"
)

// SecurityReport is a read-only snapshot of the five security subsystems an
// Orchestrator actually holds. The field types mirror the corresponding
// Config fields exactly, so a report field is nil if and only if the
// matching tools.With* injection in bindExecutionContext is skipped.
//
// It exists because Config is taken BY VALUE by New and the package exposes
// no setters: assigning to a Config field after New returns writes to a copy
// nobody reads, and — with the values otherwise unreachable from outside the
// package — that mistake is invisible to any test. bootstrap's wiring test
// asserts every field here is non-nil, which is the only way to prove the
// composition root's security posture reached the ReAct loop rather than a
// discarded struct copy.
//
// The second consumer is `yanshi doctor` (W7), which reports the effective
// security posture of a running instance: it must describe what the
// orchestrator holds, not what the composition root intended to pass.
type SecurityReport struct {
	// Sandbox is the filesystem/exec isolation posture (tools.WithSandbox).
	Sandbox sandbox.Sandbox
	// NetworkPolicy is the host allow/deny policy (tools.WithNetworkPolicy).
	NetworkPolicy *netpolicy.Policy
	// Approvals is the persistent/session approval rule manager
	// (tools.WithApprovalManager; also requires a connection session id).
	Approvals *approval.Manager
	// ShellManager is the persistent shell session manager
	// (tools.WithShellManager).
	ShellManager *shell.Manager
	// SecureFactory is the hardened process launch pipeline
	// (tools.WithSecureProcessFactory).
	SecureFactory secproc.Factory
}

// SecurityReport returns the security subsystems this Orchestrator holds.
//
// Callers must treat a nil field as "this subsystem is not injected into tool
// calls" — see the SecurityReport type doc for why reading them back is the
// only reliable check.
func (o *Orchestrator) SecurityReport() SecurityReport {
	return SecurityReport{
		Sandbox:       o.sandbox,
		NetworkPolicy: o.networkPolicy,
		Approvals:     o.approvals,
		ShellManager:  o.shellManager,
		SecureFactory: o.secureFactory,
	}
}
