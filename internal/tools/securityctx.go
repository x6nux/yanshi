package tools

import (
	"context"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/securityctx"
)

// WithSandbox binds sb to ctx via the shared securityctx package. This is the
// write-side wrapper tools/web.go and friends use; secproc/shell read the
// value back via securityctx.Sandbox directly (without importing tools).
// Returns ctx unchanged when sb is nil.
func WithSandbox(ctx context.Context, sb sandbox.Sandbox) context.Context {
	return securityctx.WithSandbox(ctx, sb)
}

// SandboxFromContext reads a sandbox bound by WithSandbox. Equivalent to
// securityctx.Sandbox — kept here so existing tools callers don't have to
// add a new import while we migrate.
func SandboxFromContext(ctx context.Context) (sandbox.Sandbox, bool) {
	return securityctx.Sandbox(ctx)
}

// WithNetworkPolicy binds policy to ctx via the shared securityctx package.
// Returns ctx unchanged when policy is nil.
func WithNetworkPolicy(ctx context.Context, policy *netpolicy.Policy) context.Context {
	return securityctx.WithNetworkPolicy(ctx, policy)
}

// NetworkPolicyFromContext reads a policy bound by WithNetworkPolicy.
func NetworkPolicyFromContext(ctx context.Context) (*netpolicy.Policy, bool) {
	return securityctx.NetworkPolicy(ctx)
}
