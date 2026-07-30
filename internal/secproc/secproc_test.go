package secproc_test

import (
	"context"
	"errors"
	"testing"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/sandbox"
	"github.com/x6nux/yanshi/internal/secproc"
	"github.com/x6nux/yanshi/internal/securityctx"
	"github.com/x6nux/yanshi/internal/tools"
)

type spyFactory struct {
	calls int
	last  secproc.SecureProcessSpec
}

func (s *spyFactory) Start(_ context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	s.calls++
	s.last = spec
	return &secproc.StartedProcess{PID: 999}, nil
}

func TestLaunchFailsClosedWhenNoAuthorizer(t *testing.T) {
	// When no authorizer is registered, Launch must fail closed with
	// ErrNoAuthorizer (production init in internal/tools registers the real
	// one; this test temporarily nils it to prove the fail-closed branch).
	saved := secproc.SwapAuthorizer(nil)
	defer func() { secproc.SwapAuthorizer(saved) }()
	_, err := secproc.Launch(context.Background(), secproc.SecureProcessSpec{Tool: "shell_run"})
	if !errors.Is(err, secproc.ErrNoAuthorizer) {
		t.Fatalf("want ErrNoAuthorizer, got %v", err)
	}
}

func TestLaunchFailsClosedWhenNoFactory(t *testing.T) {
	// With an authorizer registered (TestMain or init in internal/tools) but
	// no Factory in context, Launch fails closed with a factory-missing error.
	// Use a profile that would otherwise allow the tool so the only reason for
	// failure is the missing factory.
	ctx := tools.WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go test"}},
	})
	_, err := secproc.Launch(ctx, secproc.SecureProcessSpec{Tool: "shell_run", Shell: "go test"})
	if err == nil {
		t.Fatal("missing factory must fail closed")
	}
}

// TestLaunchAuthorizesBeforeStartAndHardDenyShortCircuits verifies the
// HardDeny firewall at the launcher seam: an empty Tools.Allow is a HardDeny
// (guard_test Task 1), and Launch must short-circuit before the factory is
// invoked. Use the real tools.Authorize path via WithProfile so the test
// exercises the same verdict switch as production.
func TestLaunchAuthorizesBeforeStartAndHardDenyShortCircuits(t *testing.T) {
	var spy spyFactory
	allowCtx := tools.WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"shell_run"}},
		Shell: guard.ShellPerm{Policy: "allowlist", Patterns: []string{"go test"}},
	})
	allowCtx = securityctx.WithNetworkPolicy(allowCtx, &netpolicy.Policy{Default: "allow"})
	allowCtx = securityctx.WithSandbox(allowCtx, sandbox.New(sandbox.Config{Enabled: true, Tier: sandbox.ReadOnly}))
	allowCtx = secproc.WithFactory(allowCtx, &spy)
	if _, err := secproc.Launch(allowCtx, secproc.SecureProcessSpec{Tool: "shell_run", Shell: "go test", Program: "sh", Args: []string{"-c", "go test"}}); err != nil {
		t.Fatalf("allow path failed: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("factory must be called once on Allow, got %d", spy.calls)
	}

	// HardDeny: empty Tools.Allow — no factory wiring needed, but keep the spy
	// installed to prove it is NOT consulted.
	hardCtx := tools.WithProfile(context.Background(), guard.PermissionProfile{})
	hardCtx = secproc.WithFactory(hardCtx, &spy)
	_, err := secproc.Launch(hardCtx, secproc.SecureProcessSpec{Tool: "shell_run"})
	if err == nil {
		t.Fatal("HardDeny must block Launch")
	}
	if spy.calls != 1 {
		t.Fatalf("factory must not be called on HardDeny: %d", spy.calls)
	}
	// Make sure Launch surfaces the typed guard denial. HardDeny is returned as
	// *tools.DenyErr by Authorize; string matching would be both redundant and
	// brittle.
	if !tools.IsDenyErr(err) {
		t.Fatalf("err must be a *tools.DenyErr, got %T %v", err, err)
	}
}
