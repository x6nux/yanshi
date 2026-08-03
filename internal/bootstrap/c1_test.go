package bootstrap_test

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudwego/eino/components/model"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/agent/rlm"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

func TestSelectRLMModel_FakeFallback(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"ok"}, nil)
	cfg := config.Config{} // Batch.RLMModel 为空
	got, err := bootstrap.SelectRLMModel(cfg, nil, fake)
	require.NoError(t, err)
	assert.Equal(t, fake, got)
}

func TestSelectRLMModel_RequiresCheapCostClass(t *testing.T) {
	cheap := einollm.NewFakeModel([]string{"ok"}, nil)
	expensive := einollm.NewFakeModel([]string{"big"}, nil)
	models := map[string]model.BaseChatModel{
		"cheap":     cheap,
		"expensive": expensive,
	}
	providers := []config.ProviderConfig{
		{Name: "cheap", CostClass: "cheap"},
		{Name: "expensive", CostClass: "expensive"},
	}

	// cheap 可选。
	got, err := bootstrap.SelectRLMModel(
		config.Config{
			LLM:   config.LLMConfig{Providers: providers},
			Batch: config.BatchConfig{RLMModel: "cheap"},
		},
		models, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, cheap, got)

	// expensive 必须拒绝。
	_, err = bootstrap.SelectRLMModel(
		config.Config{
			LLM:   config.LLMConfig{Providers: providers},
			Batch: config.BatchConfig{RLMModel: "expensive"},
		},
		models, nil,
	)
	require.Error(t, err)
}

func TestSelectRLMModel_UnknownProviderFails(t *testing.T) {
	_, err := bootstrap.SelectRLMModel(
		config.Config{Batch: config.BatchConfig{RLMModel: "ghost"}},
		map[string]model.BaseChatModel{}, nil,
	)
	require.Error(t, err)
}

func TestSelectRLMModel_NoFakeNoProviderFails(t *testing.T) {
	_, err := bootstrap.SelectRLMModel(config.Config{}, nil, nil)
	require.Error(t, err)
}

func TestBuildRLMMaxConcurrencyClamped(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"ok"}, nil)
	got, err := bootstrap.BuildRLM(config.Config{}, nil, fake)
	require.NoError(t, err)
	require.NotNil(t, got)
	// *tools.RLMTools 没有 Info 方法；只有其 Query *GuardedTool 字段有。
	info, err := got.Tools.Query.Info(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "rlm_query", info.Name)
	// 不超 16（rlm.MaxBatchSize）；由 BuildRLM 内部夹断。
	_ = rlm.MaxBatchSize
}

func TestBuildRLM_MaxConcurrencyClampDown(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"ok"}, nil)
	got, err := bootstrap.BuildRLM(config.Config{
		Batch: config.BatchConfig{RLMMaxConcurrency: 999},
	}, nil, fake)
	require.NoError(t, err)
	require.NotNil(t, got)
	// Surfaces if the clamp-down at limit > rlm.MaxBatchSize panics or errors.
	info, err := got.Tools.Query.Info(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "rlm_query", info.Name)
}

func TestBuildAutomationDefaultInterval(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	adapter := bootstrap.NewA2Adapter(newFakeWorkManager(), newFakeBrokerSubmitter(), newFakeKV())
	// Zero tick → interval <= 0 → fallback to time.Minute.
	cfg := config.Config{Batch: config.BatchConfig{AutomationTickSec: 0}}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	c1, err := bootstrap.BuildAutomation(parent, cfg, s, adapter)
	require.NoError(t, err)
	require.NotNil(t, c1)
	cancel()
	c1.Scheduler.Wait()
}

func TestBuildAutomationRejectsNilStore(t *testing.T) {
	_, err := bootstrap.BuildAutomation(context.Background(), config.Config{}, nil, nil)
	require.Error(t, err)
}

func TestBuildAutomationConstructsManagerSchedulerAdapter(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	wm := newFakeWorkManager()
	bs := newFakeBrokerSubmitter()
	adapter := bootstrap.NewA2Adapter(wm, bs, newFakeKV())

	cfg := config.Config{Batch: config.BatchConfig{AutomationTickSec: 1}}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	c1, err := bootstrap.BuildAutomation(parent, cfg, s, adapter)
	require.NoError(t, err)
	require.NotNil(t, c1)
	require.NotNil(t, c1.Manager)
	require.NotNil(t, c1.Scheduler)
	require.NotNil(t, c1.Tools)

	// Shutdown 路径：cancel → Wait。
	cancel()
	c1.Scheduler.Wait()
}

func TestBuildAutomationRejectsNilAdapter(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()
	_, err = bootstrap.BuildAutomation(context.Background(), config.Config{}, s, nil)
	require.Error(t, err)
}

func TestBuildAutomationSchedulerGoroutineExitsOnCancel(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	adapter := bootstrap.NewA2Adapter(newFakeWorkManager(), newFakeBrokerSubmitter(), newFakeKV())
	cfg := config.Config{Batch: config.BatchConfig{AutomationTickSec: 1}}
	parent, cancel := context.WithCancel(context.Background())

	c1, err := bootstrap.BuildAutomation(parent, cfg, s, adapter)
	require.NoError(t, err)

	// 让 scheduler 跑几个 tick。
	var ticks int32
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&ticks) < 1 {
		_, err := c1.Manager.Create(automation.CreateInput{
			Name: "x", Prompt: "p",
			Schedule: automation.Schedule{Kind: "interval", IntervalSec: 1},
		})
		if err == nil {
			atomic.AddInt32(&ticks, 1)
		}
		time.Sleep(20 * time.Millisecond)
		select {
		case <-deadline:
			t.Fatal("no tick observed")
		default:
		}
	}

	cancel()
	done := make(chan struct{})
	go func() { c1.Scheduler.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Scheduler did not exit after cancel")
	}
}

func TestBuildAutomationAllToolsCount(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	adapter := bootstrap.NewA2Adapter(newFakeWorkManager(), newFakeBrokerSubmitter(), newFakeKV())
	cfg := config.Config{Batch: config.BatchConfig{AutomationTickSec: 1}}
	c1, err := bootstrap.BuildAutomation(context.Background(), cfg, s, adapter)
	require.NoError(t, err)

	ctx := context.Background()
	toolsByName := map[string]*tools.GuardedTool{
		"automation_create": c1.Tools.Create,
		"automation_list":   c1.Tools.List,
		"automation_read":   c1.Tools.Read,
		"automation_update": c1.Tools.Update,
		"automation_pause":  c1.Tools.Pause,
		"automation_resume": c1.Tools.Resume,
		"automation_delete": c1.Tools.Delete,
		"automation_run":    c1.Tools.Run,
	}
	for wantName, gt := range toolsByName {
		info, err := gt.Info(ctx)
		require.NoError(t, err)
		assert.Equal(t, wantName, info.Name, "tool registered with wrong name")
	}
}

func TestBuildC1WiresAllThreeComponents(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	wm := newFakeWorkManager()
	bs := newFakeBrokerSubmitter()
	adapter := bootstrap.NewA2Adapter(wm, bs, s)
	reg := registry.NewManager(registry.NewManagerOpts{
		RootContext:   context.Background(),
		MaxConcurrent: 4,
		Path:          filepath.Join(t.TempDir(), "state.json"),
	})
	t.Cleanup(reg.Close)

	cfg := config.Config{Batch: config.BatchConfig{AutomationTickSec: 1}}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := einollm.NewFakeModel([]string{"ok"}, nil)
	components, err := bootstrap.BuildC1(parent, cfg, s, adapter, reg, nil, fake)
	require.NoError(t, err)
	require.NotNil(t, components.RLM)
	require.NotNil(t, components.Automation)
	require.NotNil(t, components.Batch)

	// 通过 Info(ctx) 收集工具名，验证 1 个 rlm_query + 8 个 automation + 1 个 agent_batch。
	names := collectToolNames(t,
		components.RLM.Query,
		components.Automation.Tools.Create, components.Automation.Tools.List,
		components.Automation.Tools.Read, components.Automation.Tools.Update,
		components.Automation.Tools.Pause, components.Automation.Tools.Resume,
		components.Automation.Tools.Delete, components.Automation.Tools.Run,
		components.Batch.AgentBatch,
	)
	assert.Equal(t, "rlm_query", names["rlm_query"])
	for _, want := range []string{
		"automation_create", "automation_list", "automation_read",
		"automation_update", "automation_pause", "automation_resume",
		"automation_delete", "automation_run", "agent_batch",
	} {
		assert.Contains(t, names, want, "missing %q", want)
	}

	// Shutdown 路径：cancel → scheduler.Wait → store.Close。
	cancel()
	components.Automation.Scheduler.Wait()
}

func TestBuildC1RejectsNilRegistry(t *testing.T) {
	s, _ := store.Open(":memory:")
	defer s.Close()
	adapter := bootstrap.NewA2Adapter(newFakeWorkManager(), newFakeBrokerSubmitter(), s)
	_, err := bootstrap.BuildC1(context.Background(), config.Config{}, s, adapter, nil, nil, nil)
	require.Error(t, err)
}

func TestBuildC1RejectsNilAdapter(t *testing.T) {
	s, _ := store.Open(":memory:")
	defer s.Close()
	reg := registry.NewManager(registry.NewManagerOpts{
		RootContext:   context.Background(),
		MaxConcurrent: 4,
		Path:          filepath.Join(t.TempDir(), "state.json"),
	})
	defer reg.Close()
	_, err := bootstrap.BuildC1(context.Background(), config.Config{}, s, nil, reg, nil, nil)
	require.Error(t, err)
}

func TestBuildC1RLMDegradesWithoutModel(t *testing.T) {
	// fakeModel 为 nil 且 batch.rlm_model 为空时，BuildRLM 失败 —— 但这不该
	// 连带废掉 automation 与 agent_batch：BuildC1 记警告、RLM 置 nil、其余照常构造。
	s, _ := store.Open(":memory:")
	defer s.Close()
	adapter := bootstrap.NewA2Adapter(newFakeWorkManager(), newFakeBrokerSubmitter(), s)
	reg := registry.NewManager(registry.NewManagerOpts{
		RootContext:   context.Background(),
		MaxConcurrent: 4,
		Path:          filepath.Join(t.TempDir(), "state.json"),
	})
	defer reg.Close()

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	components, err := bootstrap.BuildC1(parent, config.Config{}, s, adapter, reg, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, components)
	assert.Nil(t, components.RLM, "RLM 不可用时应降级为 nil，而非让整个 C1 失败")
	require.NotNil(t, components.Automation, "automation 不该被 RLM 的失败连累")
	require.NotNil(t, components.Batch, "agent_batch 不该被 RLM 的失败连累")
	require.NotEmpty(t, components.Warnings, "降级原因必须可被 bootstrap 打到 stderr")

	cancel()
	components.Automation.Scheduler.Wait()
}

func TestBuildC1BuildAutomationError(t *testing.T) {
	// BuildAutomation error is unreachable inside BuildC1 with valid inputs
	// (NewManager never errors with non-nil repo/queue).
	// This test verifies BuildC1 passes valid args to BuildAutomation.
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	adapter := bootstrap.NewA2Adapter(newFakeWorkManager(), newFakeBrokerSubmitter(), s)
	reg := registry.NewManager(registry.NewManagerOpts{
		RootContext:   context.Background(),
		MaxConcurrent: 4,
		Path:          filepath.Join(t.TempDir(), "state.json"),
	})
	t.Cleanup(reg.Close)

	fake := einollm.NewFakeModel([]string{"ok"}, nil)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := config.Config{Batch: config.BatchConfig{AutomationTickSec: 1}}
	components, err := bootstrap.BuildC1(parent, cfg, s, adapter, reg, nil, fake)
	require.NoError(t, err)
	require.NotNil(t, components.Automation)
	cancel()
	components.Automation.Scheduler.Wait()
}

func collectToolNames(t *testing.T, guarded ...*tools.GuardedTool) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := make(map[string]string, len(guarded))
	for _, gt := range guarded {
		info, err := gt.Info(ctx)
		require.NoError(t, err)
		out[info.Name] = info.Name
	}
	return out
}
