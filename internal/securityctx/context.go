// Package securityctx owns the context keys used to thread the security
// posture (sandbox + network policy) through the orchestrator's turn
// context. It is intentionally tiny — only the keys live here — so multiple
// packages (tools, secproc, shell) can read/write the SAME value without
// forming an import cycle on any of them.
//
// Internal layout:
//
//	securityctx ←── sandbox
//	          ←── netpolicy
//	tools    → securityctx   (write side)
//	secproc  → securityctx   (read side, Task 14)
//	shell    → securityctx   (read side, Task 16)
//
// Keeping the keys here (rather than in tools) means secproc/shell do not
// need to import the tools package — they only import securityctx, sandbox,
// and netpolicy, all of which are leaf-ish.
package securityctx

import (
	"context"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
)

type sandboxKey struct{}
type networkPolicyKey struct{}

// WithSandbox binds sb to ctx so SecureProcessFactory (Task 14) and shell v2
// (Task 16) can pick it up at spawn time. A nil sb is a no-op so test paths
// that don't care about the sandbox simply skip the call.
func WithSandbox(ctx context.Context, sb sandbox.Sandbox) context.Context {
	if sb == nil {
		return ctx
	}
	return context.WithValue(ctx, sandboxKey{}, sb)
}

// Sandbox reads back a value set by WithSandbox. The bool is false when no
// sandbox (or a nil one) was bound.
func Sandbox(ctx context.Context) (sandbox.Sandbox, bool) {
	value, ok := ctx.Value(sandboxKey{}).(sandbox.Sandbox)
	return value, ok && value != nil
}

// WithNetworkPolicy binds policy to ctx so web_fetch (Task 13), the loopback
// proxy (Task 12), and SecureProcessFactory (Task 14) all consult the same
// host table. A nil policy is a no-op.
func WithNetworkPolicy(ctx context.Context, policy *netpolicy.Policy) context.Context {
	if policy == nil {
		return ctx
	}
	return context.WithValue(ctx, networkPolicyKey{}, policy)
}

// NetworkPolicy reads back a value set by WithNetworkPolicy.
func NetworkPolicy(ctx context.Context) (*netpolicy.Policy, bool) {
	value, ok := ctx.Value(networkPolicyKey{}).(*netpolicy.Policy)
	return value, ok && value != nil
}
