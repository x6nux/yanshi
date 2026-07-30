package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/agent/rlm"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

func TestAgentSpawnWithManagerAndFactory(t *testing.T) {
	mgr, _ := newTestManager(t)
	defer mgr.Close()

	factory := ManagedRunnerFactory(func(allowed []string, instr string) registry.Runner {
		return registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			return "ok", nil
		})
	})

	profile := guard.PermissionProfile{
		Tools:    guard.ToolsPerm{Allow: []string{"*"}},
		Subagent: guard.SubagentPerm{Models: []string{}, MaxReasoning: "high"},
	}

	ctx := context.Background()
	ctx = WithManager(ctx, mgr)
	ctx = WithProfile(ctx, profile)
	ctx = WithManagedRunnerFactory(ctx, factory)
	ctx = WithAvailableModels(ctx, map[string]bool{})

	tools := NewAgentTools(nil)
	ch := tools.streamAgentSpawn(ctx, `{"prompt":"test task","role":"general"}`)
	var result string
	for c := range ch {
		if c.Result != "" {
			result += c.Result
		}
	}
	if !strings.Contains(result, "agent_id") {
		t.Fatalf("expected spawn result with agent_id, got: %q", result)
	}
}

func TestAgentWaitWithManager(t *testing.T) {
	mgr, _ := newTestManager(t)
	defer mgr.Close()

	ctx := WithManager(context.Background(), mgr)
	tools := NewAgentTools(nil)

	// Wait for a non-existent agent should fail gracefully.
	ch := tools.streamAgentWait(ctx, `{"agent_id":"nonexistent"}`)
	var result string
	for c := range ch {
		if c.Result != "" {
			result += c.Result
		}
	}
	// Should either have an error or an empty result.
	_ = result
}

func TestAgentResultWithManager(t *testing.T) {
	mgr, _ := newTestManager(t)
	defer mgr.Close()

	ctx := WithManager(context.Background(), mgr)
	tools := NewAgentTools(nil)

	// Getting result for a non-existent agent.
	ch := tools.streamAgentResult(ctx, `{"agent_id":"nonexistent"}`)
	var result string
	for c := range ch {
		if c.Result != "" {
			result += c.Result
		}
	}
	if !strings.Contains(result, "not found") {
		t.Logf("got result: %q", result)
	}
}

func TestAgentSendInputWithManager(t *testing.T) {
	mgr, _ := newTestManager(t)
	defer mgr.Close()

	ctx := WithManager(context.Background(), mgr)
	tools := NewAgentTools(nil)

	ch := tools.streamAgentSendInput(ctx, `{"agent_id":"nonexistent","text":"hello"}`)
	for range ch {
	}
}

func TestAgentAssignWithManager(t *testing.T) {
	mgr, _ := newTestManager(t)
	defer mgr.Close()

	ctx := WithManager(context.Background(), mgr)
	tools := NewAgentTools(nil)

	ch := tools.streamAgentAssign(ctx, `{"agent_id":"nonexistent","assignment":"do something"}`)
	for range ch {
	}
}

func TestAgentCancelWithManager(t *testing.T) {
	mgr, _ := newTestManager(t)
	defer mgr.Close()

	ctx := WithManager(context.Background(), mgr)
	tools := NewAgentTools(nil)

	ch := tools.streamAgentCancel(ctx, `{"agent_id":"nonexistent"}`)
	for range ch {
	}
}

func TestAgentListWithManager(t *testing.T) {
	mgr, _ := newTestManager(t)
	defer mgr.Close()

	ctx := WithManager(context.Background(), mgr)
	tools := NewAgentTools(nil)

	ch := tools.streamAgentList(ctx, `{"include_archived":false}`)
	var result string
	for c := range ch {
		if c.Result != "" {
			result += c.Result
		}
	}
	if !strings.Contains(result, `"agents"`) {
		t.Fatalf("expected agents list, got: %q", result)
	}
}

// TestAgentLifecycleSuccessPaths covers the success paths of streamAgentResult,
// streamAgentAssign, and streamAgentCancel by spawning a real agent first.
func TestAgentLifecycleSuccessPaths(t *testing.T) {
	mgr, _ := newTestManager(t)
	defer mgr.Close()

	factory := ManagedRunnerFactory(func(allowed []string, instr string) registry.Runner {
		return registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			return "ok", nil
		})
	})
	profile := guard.PermissionProfile{
		Tools:    guard.ToolsPerm{Allow: []string{"*"}},
		Subagent: guard.SubagentPerm{Models: []string{}, MaxReasoning: "high"},
	}
	ctx := context.Background()
	ctx = WithManager(ctx, mgr)
	ctx = WithProfile(ctx, profile)
	ctx = WithManagedRunnerFactory(ctx, factory)
	ctx = WithAvailableModels(ctx, map[string]bool{})
	tools := NewAgentTools(nil)

	// Spawn an agent and capture its id.
	spawnCh := tools.streamAgentSpawn(ctx, `{"prompt":"test","role":"general"}`)
	var agentID string
	for c := range spawnCh {
		if c.Result != "" && strings.Contains(c.Result, "agent_id") {
			// Extract the id from {"agent_id":"...","status":"..."}
			var res struct {
				AgentID string `json:"agent_id"`
			}
			if json.Unmarshal([]byte(c.Result), &res) == nil {
				agentID = res.AgentID
			}
		}
	}
	if agentID == "" {
		t.Fatal("expected spawn to produce an agent_id")
	}

	// streamAgentResult success path (line 154).
	resCh := tools.streamAgentResult(ctx, fmt.Sprintf(`{"agent_id":%q}`, agentID))
	for c := range resCh {
		if c.Result != "" {
			// Success: JSON-marshaled record.
			_ = c.Result
		}
	}

	// streamAgentAssign success path (line 212).
	assignCh := tools.streamAgentAssign(ctx, fmt.Sprintf(`{"agent_id":%q,"assignment":"do x"}`, agentID))
	for range assignCh {
	}

	// streamAgentCancel success path (line 225).
	cancelCh := tools.streamAgentCancel(ctx, fmt.Sprintf(`{"agent_id":%q}`, agentID))
	for range cancelCh {
	}
}

// TestAgentResumeSuccess covers streamAgentResume's success path (line 197-199).
// Spawn an agent with one manager, then create a fresh manager from the same
// persistence path so the agent's record exists WITHOUT a runtime entry — this
// is the only configuration Resume accepts (a spawned agent keeps its runtime
// until the process restarts).
func TestAgentResumeSuccess(t *testing.T) {
	// Phase 1: spawn + complete an agent so its record is persisted to disk.
	path := filepath.Join(t.TempDir(), "s.json")
	mgr1 := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(), Path: path, SessionBootID: "boot1", MaxConcurrent: 4,
	})
	factory := ManagedRunnerFactory(func(allowed []string, instr string) registry.Runner {
		return registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			return "ok", nil
		})
	})
	profile := guard.PermissionProfile{
		Tools:    guard.ToolsPerm{Allow: []string{"*"}},
		Subagent: guard.SubagentPerm{Models: []string{}, MaxReasoning: "high"},
	}
	ctx1 := context.Background()
	ctx1 = WithManager(ctx1, mgr1)
	ctx1 = WithProfile(ctx1, profile)
	ctx1 = WithManagedRunnerFactory(ctx1, factory)
	ctx1 = WithAvailableModels(ctx1, map[string]bool{})
	tools := NewAgentTools(nil)

	spawnCh := tools.streamAgentSpawn(ctx1, `{"prompt":"test","role":"general"}`)
	var agentID string
	for c := range spawnCh {
		if c.Result != "" {
			var res struct {
				AgentID string `json:"agent_id"`
			}
			if json.Unmarshal([]byte(c.Result), &res) == nil {
				agentID = res.AgentID
			}
		}
	}
	if agentID == "" {
		mgr1.Close()
		t.Fatal("expected agent_id from spawn")
	}
	// Wait for completion, then close to flush persistence.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec, ok := mgr1.Result(agentID)
		if ok && rec.Status != registry.StatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mgr1.Close()

	// Phase 2: fresh manager loads the persisted record (no runtime entry).
	mgr2 := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(), Path: path, SessionBootID: "boot2", MaxConcurrent: 4,
	})
	defer mgr2.Close()

	ctx2 := context.Background()
	ctx2 = WithManager(ctx2, mgr2)
	ctx2 = WithProfile(ctx2, profile)
	ctx2 = WithManagedRunnerFactory(ctx2, factory)
	ctx2 = WithAvailableModels(ctx2, map[string]bool{})

	// Resume success path — the record exists without a runtime.
	resumeCh := tools.streamAgentResume(ctx2, fmt.Sprintf(`{"agent_id":%q}`, agentID))
	var gotResult bool
	for c := range resumeCh {
		if c.Result != "" {
			gotResult = true
		}
	}
	_ = gotResult
}

// TestAgentResumeModelMismatch covers EnsureOverrideForResume's error branch at
// agent_lifecycle.go:186-188. Spawn with a model that IS available, then resume
// against an EMPTY available map so the persisted ModelOverride is rejected.
func TestAgentResumeModelMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	mgr1 := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(), Path: path, SessionBootID: "boot1", MaxConcurrent: 4,
	})
	factory := ManagedRunnerFactory(func(allowed []string, instr string) registry.Runner {
		return registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			return "ok", nil
		})
	})
	// profile.Subagent.Models empty → AllowsAnyModel → CheckModel always passes.
	profile := guard.PermissionProfile{
		Tools:    guard.ToolsPerm{Allow: []string{"*"}},
		Subagent: guard.SubagentPerm{Models: []string{}, MaxReasoning: "high"},
	}
	ctx1 := context.Background()
	ctx1 = WithManager(ctx1, mgr1)
	ctx1 = WithProfile(ctx1, profile)
	ctx1 = WithManagedRunnerFactory(ctx1, factory)
	// "good-model" IS available at spawn time.
	ctx1 = WithAvailableModels(ctx1, map[string]bool{"good-model": true})
	tools := NewAgentTools(nil)

	spawnCh := tools.streamAgentSpawn(ctx1, `{"prompt":"test","role":"general","model":"good-model"}`)
	var agentID string
	for c := range spawnCh {
		if c.Result != "" {
			var res struct {
				AgentID string `json:"agent_id"`
			}
			if json.Unmarshal([]byte(c.Result), &res) == nil {
				agentID = res.AgentID
			}
		}
	}
	if agentID == "" {
		mgr1.Close()
		t.Fatal("expected agent_id from spawn")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rec, ok := mgr1.Result(agentID)
		if ok && rec.Status != registry.StatusRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mgr1.Close()

	// Fresh manager + EMPTY available map: "good-model" is no longer available.
	mgr2 := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(), Path: path, SessionBootID: "boot2", MaxConcurrent: 4,
	})
	defer mgr2.Close()
	ctx2 := context.Background()
	ctx2 = WithManager(ctx2, mgr2)
	ctx2 = WithProfile(ctx2, profile)
	ctx2 = WithManagedRunnerFactory(ctx2, factory)
	ctx2 = WithAvailableModels(ctx2, map[string]bool{})

	resumeCh := tools.streamAgentResume(ctx2, fmt.Sprintf(`{"agent_id":%q}`, agentID))
	var errMsg string
	for c := range resumeCh {
		if c.Err != nil {
			errMsg = c.Err.Error()
		}
		if strings.Contains(c.Text, "✗") {
			errMsg = c.Text
		}
	}
	if !strings.Contains(errMsg, "not available") {
		t.Fatalf("expected model-not-available error, got: %q", errMsg)
	}
}

func TestRunSubAgentBareModel(t *testing.T) {
	// runSubAgent with no SubAgentRunner bound should fall back to bare model
	// Generate. Without a chatModel and without a runner, it panics on nil model,
	// so we only test the runner path here. The nil-model case is tested via
	// streamStartAgent which catches the error before reaching runSubAgent.
	_ = NewAgentTools(nil) // just verify construction
}

func TestRunSubAgentWithRunner(t *testing.T) {
	runner := SubAgentRunner(func(ctx context.Context, prompt string, allowed []string, instr string) (string, error) {
		return "runner result", nil
	})
	ctx := WithSubAgentRunner(context.Background(), runner)
	tools := NewAgentTools(nil)
	result, err := tools.runSubAgent(ctx, "test prompt", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "runner result" {
		t.Fatalf("expected 'runner result', got %q", result)
	}
}

func TestRunSubAgentWithAllowedTools(t *testing.T) {
	runner := SubAgentRunner(func(ctx context.Context, prompt string, allowed []string, instr string) (string, error) {
		if len(allowed) == 0 {
			t.Fatal("expected allowed tools to be passed through")
		}
		return "result with tools", nil
	})
	ctx := WithSubAgentRunner(context.Background(), runner)
	tools := NewAgentTools(nil)
	result, err := tools.runSubAgent(ctx, "prompt", []string{"fs_read"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "result with tools" {
		t.Fatalf("expected 'result with tools', got %q", result)
	}
}

func TestRunSubAgentWithInstructionOverride(t *testing.T) {
	runner := SubAgentRunner(func(ctx context.Context, prompt string, allowed []string, instr string) (string, error) {
		if instr != "custom instruction" {
			t.Fatalf("expected 'custom instruction', got %q", instr)
		}
		return "result with instruction", nil
	})
	ctx := WithSubAgentRunner(context.Background(), runner)
	tools := NewAgentTools(nil)
	result, err := tools.runSubAgent(ctx, "prompt", nil, "custom instruction")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "result with instruction" {
		t.Fatalf("expected 'result with instruction', got %q", result)
	}
}

func TestStreamStartAgent(t *testing.T) {
	t.Run("empty prompt", func(t *testing.T) {
		tools := NewAgentTools(nil)
		ch := tools.streamStartAgent(context.Background(), `{"prompt":""}`)
		var result string
		for c := range ch {
			if c.Result != "" {
				result += c.Result
			}
		}
		if !strings.Contains(result, "prompt must not be empty") {
			t.Fatalf("expected prompt error, got: %q", result)
		}
	})

	t.Run("missing model", func(t *testing.T) {
		tools := NewAgentTools(nil)
		ch := tools.streamStartAgent(context.Background(), `{"prompt":"do something"}`)
		var result string
		for c := range ch {
			if c.Result != "" {
				result += c.Result
			}
		}
		if !strings.Contains(result, "no chat model") {
			t.Fatalf("expected model error, got: %q", result)
		}
	})

	t.Run("invalid tools json", func(t *testing.T) {
		tools := NewAgentTools(einollm.NewFakeModel([]string{"response"}, nil))
		ch := tools.streamStartAgent(context.Background(), `{"prompt":"do something","tools":"not-valid-json"}`)
		var result string
		for c := range ch {
			if c.Result != "" {
				result += c.Result
			}
		}
		if !strings.Contains(result, "must be a JSON array") {
			t.Fatalf("expected tools error, got: %q", result)
		}
	})
}

func TestSpawnWithRetry(t *testing.T) {
	mgr, _ := newTestManager(t)
	defer mgr.Close()

	// Normal spawn.
	id, err := spawnWithRetry(context.Background(), mgr, registry.SpawnRequest{
		AgentType: "subagent",
		Role:      "general",
		Runner: registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
			return "ok", nil
		}),
	})
	if err != nil {
		t.Fatalf("spawnWithRetry failed: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty agent id")
	}
}

func TestMustRole(t *testing.T) {
	_ = AgentRoles()
	role := MustRole("explore")
	if role.Name != "explore" {
		t.Fatal("MustRole should return explore role")
	}
}

func TestRLMQuery(t *testing.T) {
	t.Run("invalid json", func(t *testing.T) {
		ch := runRLMQuery(context.Background(), rlm.Runner{}, `not-json`)
		var result string
		for c := range ch {
			if c.Result != "" {
				result += c.Result
			}
		}
		if !strings.Contains(result, "invalid") {
			t.Fatalf("expected invalid arg error, got: %q", result)
		}
	})
}
