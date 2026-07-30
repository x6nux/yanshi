// Package batch provides M07 CSV/structured batch processing: indexed row
// parsing and a Runner that delegates each row to the B1 registry manager.
package batch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/x6nux/yanshi/internal/agent/registry"
)

// SpawnFunc 镜像 tools.SubAgentRunner 签名，避免 batch → tools 的 import 环。
type SpawnFunc func(
	ctx context.Context,
	prompt string,
	allowedTools []string,
	instructionOverride string,
) (string, error)

// Row 是 CSV/structured 解析后的稳定行。
type Row struct {
	Index  int               `json:"index"`
	Values map[string]string `json:"values"`
}

// Input 是 batch.Runner.Run 的入参。
type Input struct {
	Prompt              string
	Rows                []Row
	AllowedTools        []string
	InstructionOverride string
}

// Result 是单行结果；Index 稳定对应 input.Rows[i].Index。
type Result struct {
	Index  int    `json:"index"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Report 是 Runner.Run 的返回；Results 按 index 排序，不按完成顺序。
type Report struct {
	Results  []Result `json:"results"`
	Total    int      `json:"total"`
	Success  int      `json:"success"`
	Failed   int      `json:"failed"`
	Canceled int      `json:"canceled"`
}

// Runner 是 M07 的执行体。Spawn（来自 tools.SubAgentRunner）与 *registry.Manager
// 必须非 nil。Manager 的 MaxConcurrent 是唯一并发上限；当 Spawn 返回 *SpawnErrCap
// 时，runner 按 CappedBackoff 重试到 CappedRetries 次上限。
type Runner struct {
	Spawn         SpawnFunc
	Manager       *registry.Manager
	WaitTimeout   time.Duration
	CappedBackoff time.Duration
	CappedRetries int
}

// rowRunner 把单行 + Spawn 适配为 registry.Runner。每个 row 各自构造一个；
// registry 在其 goroutine 中调一次 Run。
type rowRunner struct {
	spawn SpawnFunc
	row   Row
	input Input
}

// Run 实现 registry.Runner。忽略 agentID/assignment（assignment 在 spawn 时已嵌入）。
func (r *rowRunner) Run(ctx context.Context, _, _ string) (string, error) {
	return r.spawn(ctx, promptForRow(r.input.Prompt, r.row), r.input.AllowedTools, r.input.InstructionOverride)
}

// Run 是 runner 的唯一入口。Spawn 阶段：每个 row 起 goroutine 调 Manager.Spawn，
// 满载时重试；Wait 阶段：按 row index 顺序收集结果。结果按 index 排序，不按完成顺序。
// nil Spawn/Manager、empty rows/prompt 都返回 batch error。
func (r Runner) Run(ctx context.Context, input Input) (Report, error) {
	if r.Spawn == nil {
		return Report{}, errors.New("batch spawn function is nil")
	}
	if r.Manager == nil {
		return Report{}, errors.New("B1 registry manager is required")
	}
	if len(input.Rows) == 0 {
		return Report{Results: []Result{}}, nil
	}
	if input.Prompt == "" {
		return Report{}, errors.New("batch prompt is required")
	}
	if r.CappedBackoff <= 0 {
		r.CappedBackoff = 50 * time.Millisecond
	}
	if r.CappedRetries <= 0 {
		r.CappedRetries = 10
	}

	backoff := r.CappedBackoff
	retries := r.CappedRetries
	waitTimeout := r.WaitTimeout

	agentIDs := make([]string, len(input.Rows))
	spawnErrs := make([]error, len(input.Rows))
	for i, row := range input.Rows {
		runner := &rowRunner{spawn: r.Spawn, row: row, input: input}
		id, err := spawnWithRetry(ctx, r.Manager, runner, backoff, retries)
		agentIDs[i] = id
		spawnErrs[i] = err
	}

	results := make([]Result, len(input.Rows))
	// 多个 Wait goroutine 写不同 index，但 ctx 取消路径由主 goroutine
	// 读 finished[i]；race-free 必须用 atomic.Bool。
	finished := make([]atomic.Bool, len(input.Rows))
	for i := range input.Rows {
		results[i].Index = i
	}

	var waitWG sync.WaitGroup
	for i := range input.Rows {
		waitWG.Add(1)
		go func(index int) {
			defer waitWG.Done()
			if spawnErrs[index] != nil {
				results[index] = Result{Index: index, Error: spawnErrs[index].Error()}
				finished[index].Store(true)
				return
			}
			rec, err := r.Manager.Wait(ctx, agentIDs[index], registry.WaitOpts{Timeout: waitTimeout})
			if err != nil {
				results[index] = Result{Index: index, Error: err.Error()}
				finished[index].Store(true)
				return
			}
			res := Result{Index: index, Output: rec.Result}
			switch rec.Status {
			case registry.StatusFailed:
				res.Output = ""
				res.Error = rec.Error
			case registry.StatusCancelled, registry.StatusInterrupted:
				res.Output = ""
				res.Error = context.Canceled.Error()
			}
			results[index] = res
			finished[index].Store(true)
		}(i)
	}
	waitWG.Wait()

	if err := ctx.Err(); err != nil {
		for i := range results {
			if !finished[i].Load() {
				results[i] = Result{Index: i, Error: err.Error()}
			}
		}
	}

	return buildReport(results), nil
}

// spawnWithRetry 重试 Spawn 直到成功、上下文取消、或耗尽 retries。
// 仅 *registry.SpawnErrCap 触发重试；其它错误立即返回。
func spawnWithRetry(
	ctx context.Context,
	mgr *registry.Manager,
	r registry.Runner,
	backoff time.Duration,
	retries int,
) (string, error) {
	var lastErr error
	for attempt := 0; attempt < retries; attempt++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		id, err := mgr.Spawn(ctx, registry.SpawnRequest{
			AgentType: "batch",
			Runner:    r,
		})
		if err == nil {
			return id, nil
		}
		var capped *registry.SpawnErrCap
		if !errors.As(err, &capped) {
			return "", err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
	}
	if lastErr == nil {
		lastErr = errors.New("batch spawn retries exhausted")
	}
	return "", lastErr
}

// buildReport 从 results 统计 success/failed/canceled，并返回 Report。
func buildReport(results []Result) Report {
	report := Report{
		Results: results,
		Total:   len(results),
	}
	for _, r := range results {
		switch {
		case r.Error == context.Canceled.Error() || r.Error == context.DeadlineExceeded.Error():
			report.Canceled++
		case r.Error != "":
			report.Failed++
		default:
			report.Success++
		}
	}
	return report
}

// promptForRow 把 base prompt + row_index + row JSON 拼成单行 prompt。
// 不把整份 CSV 发给每个 child；child 只看到自己的行。
func promptForRow(base string, row Row) string {
	encoded, err := json.Marshal(row.Values)
	if err != nil {
		return fmt.Sprintf("%s\nrow_index=%d\nrow_json={}", base, row.Index)
	}
	return fmt.Sprintf("%s\nrow_index=%d\nrow_json=%s", base, row.Index, encoded)
}
