// Package rlm 是 RLM1（Ranked List Model 1）非流式批量 fan-out 的执行体。
// Runner 并发调用 model.BaseChatModel.Generate，每项独立保留结果/错误。
package rlm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// MaxBatchSize 是 spec §9 RLM1 的硬上限：单次调用最多 16 个独立 prompt。
// 大于此值的请求必须在工具层被拒绝（不进入 Runner）。
const MaxBatchSize = 16

// Result 是一次 prompt 的结果。Index 对应输入 prompts[i]；Error 非空表示该
// 单项失败（不冒泡为 batch error）。Output 是模型的 assistant 回复内容。
type Result struct {
	Index  int    `json:"index"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Runner 是 RLM1 的执行体。Model 必须非 nil；MaxConcurrency <= 0 或 > MaxBatchSize
// 时一律夹到 MaxBatchSize。Run 是唯一的入口，不暴露 Stream/StreamReader。
type Runner struct {
	Model          model.BaseChatModel
	MaxConcurrency int
}

// Run 并发调用 Model.Generate，最多 MaxBatchSize 个 prompt、最多 min(MaxConcurrency, 16)
// 个并发 worker。结果按输入 index 排序（不按完成顺序）。nil model 或配置错误返回
// batch error；每项的模型错误留在该单项的 Result.Error。
func (r Runner) Run(ctx context.Context, prompts []string) ([]Result, error) {
	if r.Model == nil {
		return nil, errors.New("rlm: model is nil")
	}
	if len(prompts) == 0 {
		return []Result{}, nil
	}
	limit := r.MaxConcurrency
	if limit <= 0 || limit > MaxBatchSize {
		limit = MaxBatchSize
	}
	if limit > len(prompts) {
		limit = len(prompts)
	}

	results := make([]Result, len(prompts))
	// 多个 worker goroutine 写不同 index，但 ctx 取消路径由主 goroutine
	// 读 finished[i]；race-free 必须用 atomic.Bool。普通 []bool 是数据竞争。
	finished := make([]atomic.Bool, len(prompts))
	for i := range results {
		results[i].Index = i
	}

	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < limit; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case index, ok := <-jobs:
					if !ok {
						return
					}
					message, err := r.Model.Generate(
						ctx,
						[]*schema.Message{schema.UserMessage(prompts[index])},
					)
					if err != nil {
						results[index].Error = err.Error()
					} else if message == nil {
						results[index].Error = "model returned nil message"
					} else {
						results[index].Output = message.Content
					}
					finished[index].Store(true)
				}
			}
		}()
	}

send:
	for index := range prompts {
		select {
		case <-ctx.Done():
			break send
		case jobs <- index:
		}
	}
	close(jobs)
	wg.Wait()

	if err := ctx.Err(); err != nil {
		for index := range results {
			if !finished[index].Load() {
				results[index].Error = err.Error()
			}
		}
	}
	return results, nil
}
