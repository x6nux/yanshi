package tools

import (
	"context"
	"testing"

	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
)

// TestSecurityContextRoundTrip (Task 13): the tools-package wrappers must
// delegate to internal/securityctx so the same context key is in effect
// regardless of which package wrote the value. This is what lets
// SecureProcessFactory (Task 14) read the bound sandbox/netpolicy without
// importing the tools package (and without forming an import cycle).
func TestSecurityContextRoundTrip(t *testing.T) {
	p := &netpolicy.Policy{Default: "deny", Allow: []string{"example.com"}}
	sb := sandbox.New(sandbox.Config{Enabled: true, Tier: sandbox.ReadOnly})
	ctx := WithSandbox(context.Background(), sb)
	ctx = WithNetworkPolicy(ctx, p)
	if got, ok := SandboxFromContext(ctx); !ok || got != sb {
		t.Fatal("sandbox context seam broken")
	}
	if got, ok := NetworkPolicyFromContext(ctx); !ok || got != p {
		t.Fatal("netpolicy context seam broken")
	}
}

// TestWebFetchPolicySourceIsRequired (Task 13): web_fetch must fail closed
// when no NetworkPolicy is bound in the context. Previously it consulted the
// guard profile's Net.Hosts allowlist directly; now it goes through the
// shared netpolicy.Policy so the host table is the single source of truth.
func TestWebFetchPolicySourceIsRequired(t *testing.T) {
	w := NewWebTools(1024, 0)
	if _, err := w.runFetch(context.Background(), `{"url":"http://example.com"}`); err == nil {
		t.Fatal("web_fetch must fail closed without NetworkPolicy context")
	}
}
