package tools

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/lsp"
	"github.com/x6nux/yanshi/internal/mcp"
	"github.com/x6nux/yanshi/internal/shell"
)

// ---------------------------------------------------------------------------
// Tools() factory methods — verify they return expected counts & non-nil refs
// ---------------------------------------------------------------------------

func TestFSTools_Tools(t *testing.T) {
	ft := NewFSTools("")
	got := ft.Tools()
	assert.Len(t, got, 7, "FSTools should have 7 tools")
	for _, g := range got {
		assert.NotNil(t, g)
	}
}

func TestMemoryTools_Tools(t *testing.T) {
	mt := &MemoryTools{
		Search: NewGuardedTool("memory_search", "x", "x", time.Second, nil, nil),
		Recall: NewGuardedTool("memory_recall", "x", "x", time.Second, nil, nil),
		Write:  NewGuardedTool("memory_write", "x", "x", time.Second, nil, nil),
	}
	got := mt.Tools()
	assert.Len(t, got, 3)
}

func TestPlanTools_Tools(t *testing.T) {
	pt := NewPlanTools()
	got := pt.Tools()
	assert.Len(t, got, 9, "PlanTools should have 9 tools")
	for _, g := range got {
		assert.NotNil(t, g)
	}
}

func TestTaskTools_Tools(t *testing.T) {
	tt := NewTaskTools()
	got := tt.Tools()
	assert.Len(t, got, 4, "TaskTools should have 4 tools")
	for _, g := range got {
		assert.NotNil(t, g)
	}
}

func TestGateTools_Tools(t *testing.T) {
	gt := NewGateTools()
	got := gt.Tools()
	assert.Len(t, got, 1)
	assert.NotNil(t, got[0])
}

func TestArtifactTools_Tools(t *testing.T) {
	at := NewArtifactTools()
	got := at.Tools()
	assert.Len(t, got, 1)
	assert.NotNil(t, got[0])
}

func TestAgentTools_Tools(t *testing.T) {
	at := NewAgentTools(nil)
	got := at.Tools()
	assert.Len(t, got, 12, "AgentTools should have 12 tools (4 legacy + 8 managed lifecycle)")
	for _, g := range got {
		assert.NotNil(t, g)
	}
}

// ---------------------------------------------------------------------------
// WithAvailableModels / AvailableModelsFromContext
// ---------------------------------------------------------------------------

func TestAvailableModelsContext(t *testing.T) {
	models := map[string]bool{"gpt-4": true, "claude-3": true}
	ctx := WithAvailableModels(context.Background(), models)
	got := AvailableModelsFromContext(ctx)
	assert.Equal(t, models, got)

	// No models set → nil.
	assert.Nil(t, AvailableModelsFromContext(context.Background()))
}

// ---------------------------------------------------------------------------
// WithManagedRunnerFactory / ManagedRunnerFactoryFromContext
// ---------------------------------------------------------------------------

func TestManagedRunnerFactoryContext(t *testing.T) {
	f := ManagedRunnerFactory(func(allowed []string, instr string) registry.Runner {
		return nil
	})
	ctx := WithManagedRunnerFactory(context.Background(), f)
	got := ManagedRunnerFactoryFromContext(ctx)
	assert.NotNil(t, got)

	// Unbound → nil.
	assert.Nil(t, ManagedRunnerFactoryFromContext(context.Background()))
}

// ---------------------------------------------------------------------------
// WithUsageSink / UsageSinkFrom
// ---------------------------------------------------------------------------

func TestUsageSinkContext(t *testing.T) {
	var called bool
	sink := UsageSink(func(u registry.Usage) { called = true })
	ctx := WithUsageSink(context.Background(), sink)
	got := UsageSinkFrom(ctx)
	got(registry.Usage{})
	assert.True(t, called)

	// Unbound → nil.
	assert.Nil(t, UsageSinkFrom(context.Background()))
}

// ---------------------------------------------------------------------------
// WithPermissionCallback / WithApprovalManager / WithShellManager
// (permctx.go)
// ---------------------------------------------------------------------------

func TestWithPermissionCallback(t *testing.T) {
	cb := func(req PermissionRequest) PermissionDecision {
		return PermissionAllow
	}
	ctx := WithPermissionCallback(context.Background(), cb)
	got, ok := permissionCallback(ctx)
	assert.True(t, ok)
	assert.NotNil(t, got)

	// nil callback is a no-op.
	base := context.Background()
	assert.Equal(t, base, WithPermissionCallback(base, nil))
}

func TestWithApprovalManager(t *testing.T) {
	// Nil manager is a no-op.
	base := context.Background()
	assert.Equal(t, base, WithApprovalManager(base, nil, ""))
	_, ok := approvalFromContext(base)
	assert.False(t, ok)

	// Empty session ID is a no-op.
	assert.Equal(t, base, WithApprovalManager(base, &approval.Manager{}, ""))
	_, ok = approvalFromContext(base)
	assert.False(t, ok)
}

func TestWithShellManager(t *testing.T) {
	m := &shell.Manager{}
	ctx := WithShellManager(context.Background(), m)
	got, ok := ShellManagerFromContext(ctx)
	assert.True(t, ok)
	assert.Same(t, m, got)
}

// ---------------------------------------------------------------------------
// ValidateOverride edge cases
// ---------------------------------------------------------------------------

func TestValidateOverride_EmptyIsOK(t *testing.T) {
	err := ValidateOverride(context.Background(), "", "", guard.PermissionProfile{}, nil)
	assert.NoError(t, err)
}

func TestValidateOverride_ModelUnavailable(t *testing.T) {
	err := ValidateOverride(context.Background(), "nonexistent", "", guard.PermissionProfile{}, map[string]bool{"claude-3": true})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not available")
}

func TestValidateOverride_ReasoningDenied(t *testing.T) {
	// MaxReasoning="low" means high is denied.
	profile := guard.PermissionProfile{
		Subagent: guard.SubagentPerm{MaxReasoning: "low"},
	}
	err := ValidateOverride(context.Background(), "", "high", profile, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "override denied")
}

func TestValidateOverride_ModelAllowed(t *testing.T) {
	profile := guard.PermissionProfile{
		Subagent: guard.SubagentPerm{Models: []string{"gpt-4"}},
	}
	err := ValidateOverride(context.Background(), "gpt-4", "", profile, map[string]bool{"gpt-4": true, "claude-3": true})
	assert.NoError(t, err)
}

func TestValidateOverride_ModelContextFallback(t *testing.T) {
	ctx := WithAvailableModels(context.Background(), map[string]bool{"claude-3": true})
	profile := guard.PermissionProfile{
		Subagent: guard.SubagentPerm{Models: []string{"claude-3"}},
	}
	err := ValidateOverride(ctx, "claude-3", "", profile, nil)
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// ParentWorkingSetHint / extractResultSection / matchResultSection / protoFromRegistryEvent
// (subagent.go)
// ---------------------------------------------------------------------------

func TestParentWorkingSetHint_NoEvidence(t *testing.T) {
	result := "just a simple result"
	got := ParentWorkingSetHint(result)
	assert.Equal(t, result, got)
}

func TestParentWorkingSetHint_WithEvidence(t *testing.T) {
	result := `SUMMARY: done

EVIDENCE:
- file a.go was changed
- function foo was added

RISKS: none`
	got := ParentWorkingSetHint(result)
	assert.Contains(t, got, "[parent working-set hint")
	assert.Contains(t, got, "file a.go was changed")
}

func TestExtractResultSection(t *testing.T) {
	result := `SUMMARY: task done
EVIDENCE:
- item 1
- item 2
RISKS: none`
	ev := extractResultSection(result, "EVIDENCE")
	assert.Contains(t, ev, "item 1")
	assert.NotContains(t, ev, "SUMMARY")
}

func TestMatchResultSection(t *testing.T) {
	assert.Equal(t, "SUMMARY", matchResultSection("SUMMARY:"))
	assert.Equal(t, "EVIDENCE", matchResultSection("EVIDENCE"))
	assert.Equal(t, "", matchResultSection("UNKNOWN: x"))
}

func TestProtoFromRegistryEvent(t *testing.T) {
	ev := registry.Event{
		AgentID: "a1", Role: "explorer",
		Type: registry.EventStarted, Status: registry.StatusRunning, Text: "starting work",
	}
	frame := protoFromRegistryEvent(ev)
	assert.Equal(t, "a1", frame.AgentID)
	assert.Equal(t, "explorer", frame.AgentRole)
	assert.Equal(t, "started", frame.Event)
}

// ---------------------------------------------------------------------------
// inferRoleFromTools (subagent.go)
// ---------------------------------------------------------------------------

func TestInferRoleFromTools(t *testing.T) {
	assert.Equal(t, "explore", inferRoleFromTools([]string{"fs_read", "fs_search"}))
	assert.Equal(t, "explore", inferRoleFromTools([]string{"fs_read", "shell_run"}))
	assert.Equal(t, "general", inferRoleFromTools([]string{"*"}))
	assert.Equal(t, "general", inferRoleFromTools(nil))
	assert.Equal(t, "general", inferRoleFromTools([]string{"fs_write"}))
}

// ---------------------------------------------------------------------------
// EnsureOverrideForResume (overridepolicy.go)
// ---------------------------------------------------------------------------

func TestEnsureOverrideForResume(t *testing.T) {
	err := EnsureOverrideForResume(context.Background(), "", "", guard.PermissionProfile{}, nil)
	assert.NoError(t, err)
}

// ---------------------------------------------------------------------------
// WithLSP / WithMCP (lspctx.go / mcpctx.go)
// ---------------------------------------------------------------------------

func TestWithLSP_Nil(t *testing.T) {
	base := context.Background()
	ctx := WithLSP(base, nil)
	assert.Equal(t, base, ctx)
	_, ok := LSPFromContext(ctx)
	assert.False(t, ok)
}

// stubLSP is a minimal LSPManager implementation for testing.
type stubLSP struct{}

func (s *stubLSP) Enabled() bool                                             { return false }
func (s *stubLSP) DidChange(path, content string)                            {}
func (s *stubLSP) Diagnostics(path string, _ time.Duration) []lsp.Diagnostic { return nil }

func TestWithLSP_Value(t *testing.T) {
	mgr := &stubLSP{}
	ctx := WithLSP(context.Background(), mgr)
	got, ok := LSPFromContext(ctx)
	assert.True(t, ok)
	assert.Same(t, mgr, got)

	// Unbound context → ok=false.
	_, ok = LSPFromContext(context.Background())
	assert.False(t, ok)
}

func TestWithMCP_Nil(t *testing.T) {
	base := context.Background()
	ctx := WithMCP(base, nil)
	assert.Equal(t, base, ctx)
	_, ok := MCPFromContext(ctx)
	assert.False(t, ok)
}

func TestWithMCP_Value(t *testing.T) {
	mgr := mcp.NewManager(nil) // nil map = no servers, but still a valid *mcp.Manager
	ctx := WithMCP(context.Background(), mgr)
	got, ok := MCPFromContext(ctx)
	assert.True(t, ok)
	assert.Same(t, mgr, got)

	// Unbound context.
	_, ok = MCPFromContext(context.Background())
	assert.False(t, ok)
}

// ---------------------------------------------------------------------------
// aliasGuardedTool (plan.go)
// ---------------------------------------------------------------------------

func TestAliasGuardedTool(t *testing.T) {
	pt := NewPlanTools()
	assert.NotNil(t, pt.ChecklistWrite)
	assert.NotNil(t, pt.TodoWrite)
	assert.NotNil(t, pt.TodoAdd)
	assert.NotNil(t, pt.TodoUpdate)
	assert.NotNil(t, pt.TodoList)
}

// ---------------------------------------------------------------------------
// requireTaskManager (task.go)
// ---------------------------------------------------------------------------

func TestRequireTaskManager_Unbound(t *testing.T) {
	_, err := requireTaskManager(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "task manager unavailable")
}
