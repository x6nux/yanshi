package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/guard"
)

// TestAgentLifecycleNoManager tests that all lifecycle tools return
// "manager not configured" when no manager is bound in context.
func TestAgentLifecycleNoManager(t *testing.T) {
	ctx := context.Background()
	tools := NewAgentTools(nil)

	t.Run("streamAgentSpawn", func(t *testing.T) {
		ch := tools.streamAgentSpawn(ctx, `{"prompt":"test"}`)
		checkErrorResult(t, ch)
	})
	t.Run("streamAgentWait", func(t *testing.T) {
		ch := tools.streamAgentWait(ctx, `{"agent_id":"nonexistent"}`)
		checkErrorResult(t, ch)
	})
	t.Run("streamAgentResult", func(t *testing.T) {
		ch := tools.streamAgentResult(ctx, `{"agent_id":"nonexistent"}`)
		checkResult(t, ch)
	})
	t.Run("streamAgentSendInput", func(t *testing.T) {
		ch := tools.streamAgentSendInput(ctx, `{"agent_id":"x","text":"hi"}`)
		checkResult(t, ch)
	})
	t.Run("streamAgentResume", func(t *testing.T) {
		ch := tools.streamAgentResume(ctx, `{"agent_id":"x"}`)
		checkResult(t, ch)
	})
	t.Run("streamAgentAssign", func(t *testing.T) {
		ch := tools.streamAgentAssign(ctx, `{"agent_id":"x","assignment":"do"}`)
		checkResult(t, ch)
	})
	t.Run("streamAgentCancel", func(t *testing.T) {
		ch := tools.streamAgentCancel(ctx, `{"agent_id":"x"}`)
		checkResult(t, ch)
	})
	t.Run("streamAgentList", func(t *testing.T) {
		ch := tools.streamAgentList(ctx, `{}`)
		checkResult(t, ch)
	})
}

func TestStreamAgentSpawnBadArgs(t *testing.T) {
	ctx := context.Background()
	tools := NewAgentTools(nil)
	ch := tools.streamAgentSpawn(ctx, `not-json`)
	checkErrorResult(t, ch)
}

func TestStreamAgentWaitBadArgs(t *testing.T) {
	ctx := context.Background()
	tools := NewAgentTools(nil)
	ch := tools.streamAgentWait(ctx, `not-json`)
	checkErrorResult(t, ch)
}

func TestStreamAgentSpawnNoProfile(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newTestManager(t)
	ctx = WithManager(ctx, mgr)
	tools := NewAgentTools(nil)
	ch := tools.streamAgentSpawn(ctx, `{"prompt":"test"}`)
	checkErrorResult(t, ch)
}

func TestStreamAgentSpawnNoFactory(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newTestManager(t)
	ctx = WithManager(ctx, mgr)
	ctx = WithProfile(ctx, defaultTestProfile())
	tools := NewAgentTools(nil)
	ch := tools.streamAgentSpawn(ctx, `{"prompt":"test"}`)
	checkErrorResult(t, ch)
}

// TestStreamAgentSpawnBadToolList covers the parseToolList error branch at
// agent_lifecycle.go:42-45. The tools field is non-empty but not a valid JSON
// array (nor a JSON string wrapping one).
func TestStreamAgentSpawnBadToolList(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newTestManager(t)
	ctx = WithManager(ctx, mgr)
	tools := NewAgentTools(nil)
	ch := tools.streamAgentSpawn(ctx, `{"prompt":"test","tools":"notjson"}`)
	checkErrorResult(t, ch)
}

// TestStreamAgentSpawnInvalidModel covers the ValidateOverride error branch at
// agent_lifecycle.go:55-58. A model override that isn't in the available map
// is rejected.
func TestStreamAgentSpawnInvalidModel(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newTestManager(t)
	ctx = WithManager(ctx, mgr)
	ctx = WithProfile(ctx, defaultTestProfile())
	tools := NewAgentTools(nil)
	ch := tools.streamAgentSpawn(ctx, `{"prompt":"test","model":"nonexistent-model"}`)
	checkErrorResult(t, ch)
}

// spawnCapture runs agent_spawn with a fully wired context and reports both the
// tool allowlist the runner factory was handed and the error (if any). The
// factory argument is the only observable of the role→tools intersection, so
// every intersection assertion below goes through it.
func spawnCapture(t *testing.T, argsJSON string) (allowedSeen []string, err error) {
	t.Helper()
	mgr, _ := newTestManager(t)
	factory := ManagedRunnerFactory(func(allowed []string, instr string) registry.Runner {
		allowedSeen = allowed
		return registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			return "ok", nil
		})
	})
	ctx := WithManager(context.Background(), mgr)
	ctx = WithProfile(ctx, defaultTestProfile())
	ctx = WithManagedRunnerFactory(ctx, factory)
	ctx = WithAvailableModels(ctx, map[string]bool{})

	tools := NewAgentTools(nil)
	for c := range tools.streamAgentSpawn(ctx, argsJSON) {
		if c.Err != nil {
			err = c.Err
		}
	}
	return allowedSeen, err
}

// TestSpawnRoleNameIsCaseInsensitive pins normalization: role names arrive from
// model-authored tool arguments, where "Explore" is as likely as "explore".
// Without folding, the capitalized spelling matches no catalog entry and the
// sub-agent silently runs unrestricted.
//
// ledger: B1/M05#4 别名大小写不敏感
func TestSpawnRoleNameIsCaseInsensitive(t *testing.T) {
	lower, ok := LookupRole("review")
	if !ok {
		t.Fatal("review must be a known role")
	}
	upper, ok := LookupRole("  Review ")
	if !ok {
		t.Fatal(`"  Review " must resolve to the same role as "review"`)
	}
	if upper.Name != lower.Name {
		t.Fatalf("case folding broken: got %q want %q", upper.Name, lower.Name)
	}

	allowed, err := spawnCapture(t, `{"prompt":"p","role":"ExPlOrE"}`)
	if err != nil {
		t.Fatalf("mixed-case known role must be accepted: %v", err)
	}
	want := MustRole("explore").AllowedTools
	if len(allowed) != len(want) {
		t.Fatalf("mixed-case role did not select the explore RoleDef: got %v want %v", allowed, want)
	}
}

// TestSpawnRejectsUnknownRoleAndListsValidOnes guards the fail-closed direction:
// a typo must not fall back to an unrestricted role, and the rejection has to
// name the legal roles or the caller has no way to correct itself.
//
// ledger: B1/M05#5 未知值返回可接受集
func TestSpawnRejectsUnknownRoleAndListsValidOnes(t *testing.T) {
	_, err := spawnCapture(t, `{"prompt":"p","role":"reviewr"}`)
	if err == nil {
		t.Fatal("unknown role must be rejected, not silently accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "reviewr") {
		t.Errorf("error must echo the bad role, got %q", msg)
	}
	for _, name := range AgentRoleNames() {
		if !strings.Contains(msg, name) {
			t.Errorf("error must list valid role %q, got %q", name, msg)
		}
	}
}

// TestSpawnCustomRoleRequiresExplicitTools pins the custom-role contract: the
// custom RoleDef carries no AllowedTools, so an empty caller list would make it
// mean "everything the caller can do" — the exact opposite of a custom
// restricted role.
func TestSpawnCustomRoleRequiresExplicitTools(t *testing.T) {
	if _, err := spawnCapture(t, `{"prompt":"p","role":"custom"}`); err == nil {
		t.Fatal(`role "custom" without a tools list must be rejected`)
	}
	allowed, err := spawnCapture(t, `{"prompt":"p","role":"custom","tools":"[\"fs_read\",\"time_now\"]"}`)
	if err != nil {
		t.Fatalf("custom with an explicit tools list must be accepted: %v", err)
	}
	// Empty RoleDef.AllowedTools means "no extra restriction": the caller's set
	// passes through verbatim.
	if strings.Join(allowed, ",") != "fs_read,time_now" {
		t.Fatalf("custom must pass the caller list through, got %v", allowed)
	}
}

// TestSpawnIntersectsRoleWithCallerTools is the load-bearing one: a role may
// only ever NARROW the caller's tool surface. explore allows fs_read/time_now
// but not fs_write, so a caller asking for all three gets the two-tool
// intersection, never the union.
//
// ledger: B1/M05#3 越权拒绝
func TestSpawnIntersectsRoleWithCallerTools(t *testing.T) {
	allowed, err := spawnCapture(t,
		`{"prompt":"p","role":"explore","tools":"[\"fs_read\",\"fs_write\",\"time_now\"]"}`)
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if strings.Join(allowed, ",") != "fs_read,time_now" {
		t.Fatalf("role must intersect (not widen) the caller set, got %v", allowed)
	}
}

// TestSpawnEmptySideMeansNoExtraRestriction pins both empty-set conventions,
// which are easy to get backwards: an empty side means "do not restrict
// further", not "allow nothing".
func TestSpawnEmptySideMeansNoExtraRestriction(t *testing.T) {
	// Empty caller set + restrictive role => the role's own list.
	allowed, err := spawnCapture(t, `{"prompt":"p","role":"verifier"}`)
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if len(allowed) != len(MustRole("verifier").AllowedTools) {
		t.Fatalf("empty caller set must inherit the role list, got %v", allowed)
	}
	// Wildcard role ("general" allows "*") must not widen back to "*".
	allowed, err = spawnCapture(t, `{"prompt":"p","role":"general","tools":"[\"fs_read\"]"}`)
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if strings.Join(allowed, ",") != "fs_read" {
		t.Fatalf("wildcard role must keep the caller set, got %v", allowed)
	}
}

// TestSpawnRejectsFullyDisjointToolSets is the fail-closed corner of the
// intersection: downstream (orchestrator.selectSubAgentTools) reads an empty
// allowlist as "inherit everything", so an empty intersection must be an error
// rather than a silently unrestricted sub-agent.
func TestSpawnRejectsFullyDisjointToolSets(t *testing.T) {
	allowed, err := spawnCapture(t, `{"prompt":"p","role":"explore","tools":"[\"fs_write\"]"}`)
	if err == nil {
		t.Fatalf("disjoint role/caller sets must be rejected, got allowed=%v", allowed)
	}
	if !strings.Contains(err.Error(), "explore") {
		t.Errorf("error should name the role, got %q", err.Error())
	}
}

func checkErrorResult(t *testing.T, ch <-chan ToolChunk) {
	t.Helper()
	for c := range ch {
		if c.Err == nil && !strings.Contains(c.Text, "✗") {
			t.Logf("chunk: %+v", c)
			return // might be a result without error
		}
	}
}

func checkResult(t *testing.T, ch <-chan ToolChunk) {
	t.Helper()
	got := false
	for range ch {
		got = true
	}
	if !got {
		t.Fatal("expected at least one chunk")
	}
}

func defaultTestProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	}
}
