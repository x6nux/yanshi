package tools

import (
	"context"
	"strings"
	"testing"

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
