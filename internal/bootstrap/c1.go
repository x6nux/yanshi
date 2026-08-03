package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/components/model"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/agent/rlm"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// C1RLM 是 buildRLM 的产物；bootstrap.Build 把 Tools.Query 加入 allTools。
type C1RLM struct {
	Tools *tools.RLMTools
}

// SelectRLMModel 选择 RLM1 的 cheap provider。
//   - cfg.Batch.RLMModel 为空时，若 fake 非 nil 则用 fake（允许 --fake-model / 无 providers 启动）；
//     否则返回错误，**不**静默退回主模型。
//   - RLMModel 必须存在于 models 注册表，且对应 provider 的 CostClass 必须是 "cheap"。
//
// fake 参数显式传入：bootstrap.Build 只在 fake 模式下传非 nil；非 fake 模式下传 nil。
func SelectRLMModel(
	cfg config.Config,
	models map[string]model.BaseChatModel,
	fake model.BaseChatModel,
) (model.BaseChatModel, error) {
	if cfg.Batch.RLMModel == "" {
		if fake != nil {
			return fake, nil
		}
		return nil, errors.New("batch.rlm_model is required when fake model is disabled")
	}
	selected, ok := models[cfg.Batch.RLMModel]
	if !ok || selected == nil {
		return nil, fmt.Errorf("batch.rlm_model %q is not in provider registry", cfg.Batch.RLMModel)
	}
	for _, provider := range cfg.LLM.Providers {
		if provider.Name == cfg.Batch.RLMModel && provider.CostClass != "cheap" {
			return nil, fmt.Errorf("provider %q must have cost_class=cheap for rlm_query", provider.Name)
		}
	}
	return selected, nil
}

// BuildRLM 构造 RLM1 工具集，包含 model 选择与 MaxConcurrency 夹断。
// 在 bootstrap.Build 中，当 fake 模式开启时（opts.FakeModel || len(providers)==0），
// fake 参数传 chatModel（此时 chatModel 就是 *einollm.FakeModel）；非 fake 模式传 nil。
func BuildRLM(
	cfg config.Config,
	models map[string]model.BaseChatModel,
	fake model.BaseChatModel,
) (*C1RLM, error) {
	selected, err := SelectRLMModel(cfg, models, fake)
	if err != nil {
		return nil, err
	}
	limit := cfg.Batch.RLMMaxConcurrency
	if limit <= 0 {
		limit = rlm.MaxBatchSize
	}
	if limit > rlm.MaxBatchSize {
		limit = rlm.MaxBatchSize
	}
	return &C1RLM{
		Tools: tools.NewRLMTools(rlm.Runner{
			Model:          selected,
			MaxConcurrency: limit,
		}),
	}, nil
}

// C1Automation 是 buildAutomation 的产物；bootstrap.Build 把 Tools.* 加进 allTools，
// Scheduler.Wait 接入 App.Shutdown。
type C1Automation struct {
	Manager   *automation.Manager
	Scheduler *automation.Scheduler
	Tools     *tools.AutomationTools
}

// BuildAutomation 构造 manager + scheduler + tools，并启动 scheduler goroutine。
// adapter 必须非 nil（即 A2Adapter 必须注入）；store 非 nil。
// parent 是 scheduler ctx 的父 ctx；App.cancel cancel 它。
func BuildAutomation(
	parent context.Context,
	cfg config.Config,
	db *store.Store,
	adapter automation.QueuePort,
) (*C1Automation, error) {
	if db == nil {
		return nil, errors.New("automation: store is required")
	}
	if adapter == nil {
		return nil, errors.New("automation: A2 QueuePort adapter is required (AU1 depends on A2)")
	}
	repo := automation.NewRepository(db)
	manager, err := automation.NewManager(repo, adapter, time.Now)
	if err != nil {
		return nil, err
	}
	interval := time.Duration(cfg.Batch.AutomationTickSec) * time.Second
	if interval <= 0 {
		interval = time.Minute
	}
	// scheduler ctx 派生自 parent；App.cancel(parent) 即可触发 scheduler 退出。
	scheduler := automation.NewScheduler(manager, interval)
	go scheduler.Start(parent)
	return &C1Automation{
		Manager:   manager,
		Scheduler: scheduler,
		Tools:     tools.NewAutomationTools(manager),
	}, nil
}

// C1Components 是 buildC1 的产物，聚合 RLM/Automation/Batch 三块。
//
// RLM 可能为 nil：rlm_query 依赖一个显式配置的 cheap provider（batch.rlm_model），
// 未配置时 RLM 单独禁用而非拖垮整个 C1 —— 否则任何没配 rlm_model 的部署会连带
// 失去 8 个 automation 工具与 agent_batch。调用方必须在使用前判空。
//
// Warnings 收集这类非致命降级的原因；bootstrap.Build 把它们打到 stderr 并继续，
// 与 VCS 初始化失败、插件发现失败采用的同一套软降级模式一致。
type C1Components struct {
	RLM        *tools.RLMTools
	Automation *C1Automation
	Batch      *tools.BatchTools
	Warnings   []string
}

// BuildC1 是 C1 的总装 helper。它不直接构造 A2 的 work.Manager 或 B1 的
// *registry.Manager —— 这些作为已构造的 adapter / *registry.Manager 注入。
// 若任一为 nil，返回错误（fail-closed）。
//
// fake 参数同 BuildRLM：仅 fake 模式下传非 nil，否则传 nil 让 RLM 严格校验。
func BuildC1(
	parent context.Context,
	cfg config.Config,
	db *store.Store,
	queueAdapter automation.QueuePort,
	registryMgr *registry.Manager,
	models map[string]model.BaseChatModel,
	fakeModel model.BaseChatModel,
) (*C1Components, error) {
	if queueAdapter == nil {
		return nil, errors.New("C1: A2 queue adapter is required (AU1 depends on A2)")
	}
	if registryMgr == nil {
		return nil, errors.New("C1: B1 registry manager is required (M07 depends on B1)")
	}

	// RLM 是可降级的：缺少 cheap provider 只让 rlm_query 缺席，automation 与
	// agent_batch 照常装配（详见 C1Components 的 doc 注释）。
	var (
		rlmTools *tools.RLMTools
		warnings []string
	)
	c1rlm, err := BuildRLM(cfg, models, fakeModel)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("C1: rlm_query disabled: %v", err))
	} else {
		rlmTools = c1rlm.Tools
	}

	auto, err := BuildAutomation(parent, cfg, db, queueAdapter)
	if err != nil {
		return nil, fmt.Errorf("C1: build automation: %w", err)
	}

	return &C1Components{
		RLM:        rlmTools,
		Automation: auto,
		Batch:      tools.NewBatchTools(registryMgr),
		Warnings:   warnings,
	}, nil
}
