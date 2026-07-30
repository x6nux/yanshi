package securityctx

import (
	"context"
	"testing"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
)

// TestRoundTrip verifies the context seam works at the securityctx level.
// The tools/secproc/shell wrappers all delegate here, so this is the lowest
// common test of the contract.
func TestRoundTrip(t *testing.T) {
	p := &netpolicy.Policy{Default: "deny"}
	sb := sandbox.New(sandbox.Config{Enabled: true, Tier: sandbox.ReadOnly})
	ctx := WithSandbox(context.Background(), sb)
	ctx = WithNetworkPolicy(ctx, p)
	if got, ok := Sandbox(ctx); !ok || got != sb {
		t.Fatal("sandbox round trip failed")
	}
	if got, ok := NetworkPolicy(ctx); !ok || got != p {
		t.Fatal("network policy round trip failed")
	}
}

// TestNilValueIsNoOp verifies that passing a nil sandbox/policy does not
// bind anything — readers see ok=false rather than ok=true with a nil value.
// This is the contract callers rely on to skip a "no sandbox configured" path
// cleanly.
func TestNilValueIsNoOp(t *testing.T) {
	ctx := WithSandbox(context.Background(), nil)
	if _, ok := Sandbox(ctx); ok {
		t.Fatal("nil sandbox must not be bound")
	}
	ctx = WithNetworkPolicy(ctx, nil)
	if _, ok := NetworkPolicy(ctx); ok {
		t.Fatal("nil policy must not be bound")
	}
}
