package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/agent/rlm"
)

// RLMTools 聚合 RLM1 的 GuardedTool。当前只有 Query（rlm_query）。
type RLMTools struct {
	Query *GuardedTool
}

// NewRLMTools 构造 RLM1 工具集。Runner.Model 必须非 nil（由 bootstrap 选择 cheap
// provider 或 fake）。
func NewRLMTools(runner rlm.Runner) *RLMTools {
	set := &RLMTools{}
	set.Query = NewGuardedTool(
		"rlm_query",
		"RLM query",
		"cost-class: cheap; non-streaming stateless fan-out for short classification or extraction tasks. Submit 1-16 independent prompts per call. This is not a multi-turn sub-agent and does not stream.",
		2*time.Minute,
		params(map[string]*schema.ParameterInfo{
			"prompts": {
				Type: schema.String,
				Desc: "JSON-encoded array of 1 to 16 short prompts",
			},
		}),
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runRLMQuery(ctx, runner, args)
		},
	)
	return set
}

// Tools 返回 RLM 工具集，供 bootstrap 统一注册。
// 与 BatchTools.Tools 同理：接口统一，组合根不必区分单工具组和多工具组。
func (r *RLMTools) Tools() []*GuardedTool {
	if r.Query == nil {
		return nil
	}
	return []*GuardedTool{r.Query}
}

// runRLMQuery 是 rlm_query 的执行体。错误一律作为 ToolChunk.Result 回喂模型
// （而不是 Go error），这样 ADK 把错误回喂模型让其改路径重试，与现有 GuardedTool
// 的 fail-closed 语义一致。
func runRLMQuery(ctx context.Context, runner rlm.Runner, args string) <-chan ToolChunk {
	out := make(chan ToolChunk, 1)
	go func() {
		defer close(out)
		var envelope struct {
			Prompts string `json:"prompts"`
		}
		if err := json.Unmarshal([]byte(args), &envelope); err != nil {
			out <- ToolChunk{Result: fmt.Sprintf("invalid rlm_query arguments: %v", err)}
			return
		}
		var prompts []string
		if err := json.Unmarshal([]byte(envelope.Prompts), &prompts); err != nil {
			out <- ToolChunk{Result: fmt.Sprintf("invalid rlm_query prompts array: %v", err)}
			return
		}
		if len(prompts) == 0 || len(prompts) > rlm.MaxBatchSize {
			out <- ToolChunk{Result: fmt.Sprintf("rlm_query accepts 1 to %d prompts", rlm.MaxBatchSize)}
			return
		}
		results, err := runner.Run(ctx, prompts)
		if err != nil {
			out <- ToolChunk{Result: "rlm_query failed: " + err.Error()}
			return
		}
		encoded, err := json.Marshal(results)
		if err != nil {
			out <- ToolChunk{Result: "rlm_query encode failed: " + err.Error()}
			return
		}
		out <- ToolChunk{Result: string(encoded)}
	}()
	return out
}
