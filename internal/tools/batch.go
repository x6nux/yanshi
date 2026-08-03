package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/agent/batch"
	"github.com/x6nux/yanshi/internal/agent/registry"
)

// BatchTools 聚合 M07 的 agent_batch 工具。Manager 由 bootstrap 注入（B1 的
// *registry.Manager）；SubAgentRunner 由 orchestrator 经 context 注入。
//
// agent_batch 的描述向模型承诺 "Approval required"，且它一次调用会扇出 N 个子 agent
// （成本远高于单次工具调用），因此经 NewApprovalGuardedTool → AuthorizeApprovalRequired
// 构造：每次调用都要用户逐次批准，profile allowlist 与 yolo/auto 模式都绕不过。
// 后果同 AutomationTools：无 permission callback 时恒被拒，故只在 WS/TUI 上可用。
type BatchTools struct {
	AgentBatch *GuardedTool
	Manager    *registry.Manager
}

// NewBatchTools 构造 batch 工具集。manager 非 nil（B1 必须落地）。
func NewBatchTools(manager *registry.Manager) *BatchTools {
	set := &BatchTools{Manager: manager}
	set.AgentBatch = NewApprovalGuardedTool(
		"agent_batch",
		"Batch agent jobs",
		"Run one sub-agent job per CSV or structured row through the B1 M04 lifecycle/registry and its unified concurrency cap (SpawnErrCap non-blocking backpressure). Returns per-row results and a summary; this is higher-cost than cost-class cheap rlm_query. Approval required.",
		10*time.Minute,
		params(map[string]*schema.ParameterInfo{
			"input": {
				Type: schema.String,
				Desc: "JSON object with prompt and exactly one of csv or rows",
			},
		}),
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAgentBatch(ctx, set, args)
		},
	)
	return set
}

// runAgentBatch 是 agent_batch 的执行体。错误作为 ToolChunk.Result 回喂模型。
func runAgentBatch(ctx context.Context, set *BatchTools, args string) <-chan ToolChunk {
	out := make(chan ToolChunk, 1)
	go func() {
		defer close(out)
		var envelope struct {
			Input string `json:"input"`
		}
		if err := json.Unmarshal([]byte(args), &envelope); err != nil {
			out <- ToolChunk{Result: err.Error()}
			return
		}
		var input struct {
			Prompt              string              `json:"prompt"`
			CSV                 string              `json:"csv"`
			Rows                []map[string]string `json:"rows"`
			AllowedTools        []string            `json:"allowed_tools"`
			InstructionOverride string              `json:"instruction_override"`
		}
		if err := json.Unmarshal([]byte(envelope.Input), &input); err != nil {
			out <- ToolChunk{Result: err.Error()}
			return
		}
		if input.Prompt == "" {
			out <- ToolChunk{Result: "prompt is required"}
			return
		}
		// 二选一：恰好提供 csv 或 rows 之一。
		hasCSV := input.CSV != ""
		hasRows := len(input.Rows) > 0
		if hasCSV == hasRows {
			out <- ToolChunk{Result: "provide exactly one of csv or rows"}
			return
		}
		var rows []batch.Row
		var parseErr error
		if hasCSV {
			rows, parseErr = batch.ParseCSV(input.CSV)
		} else {
			rows, parseErr = batch.ParseStructured(input.Rows)
		}
		if parseErr != nil {
			out <- ToolChunk{Result: parseErr.Error()}
			return
		}
		spawn := SubAgentRunnerFromContext(ctx)
		if spawn == nil {
			out <- ToolChunk{Result: "sub-agent runner is not bound in context"}
			return
		}
		if set.Manager == nil {
			out <- ToolChunk{Result: "B1 registry manager is not configured"}
			return
		}
		runner := batch.Runner{
			Spawn:         batch.SpawnFunc(spawn),
			Manager:       set.Manager,
			WaitTimeout:   5 * time.Minute,
			CappedBackoff: 100 * time.Millisecond,
			CappedRetries: 50,
		}
		report, err := runner.Run(ctx, batch.Input{
			Prompt:              input.Prompt,
			Rows:                rows,
			AllowedTools:        input.AllowedTools,
			InstructionOverride: input.InstructionOverride,
		})
		if err != nil {
			out <- ToolChunk{Result: err.Error()}
			return
		}
		encoded, err := json.Marshal(report)
		if err != nil {
			out <- ToolChunk{Result: fmt.Sprintf("encode report: %v", err)}
			return
		}
		out <- ToolChunk{Result: string(encoded)}
	}()
	return out
}
