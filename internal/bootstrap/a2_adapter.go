// Package bootstrap is the composition root containing C1 adapter code.
package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/task"
	"github.com/x6nux/yanshi/internal/task/work"
)

// BrokerSubmitter 是 *task.Broker 中 Submit 方法的窄接口，便于测试用 fake 替身。
type BrokerSubmitter interface {
	Submit(typ, input, parent string) (string, error)
}

// WorkLookup 扩展 work.ManagerLike 的读取语义。
type WorkLookup interface {
	Read(ctx context.Context, id string) (*work.WorkTask, error)
}

// A2Adapter 实现 automation.QueuePort。它组合 A2 work.ManagerLike.Create（模型可见
// 工作单元）和 task.Broker.Submit（分发执行），并通过 store.KVGet/KVSet 维护
// idempotency key → WorkTaskID 的映射。
type A2Adapter struct {
	work   work.ManagerLike
	broker BrokerSubmitter
	kv     KVStore
	mu     sync.Mutex
}

// KVStore 是 *store.Store 的 KVGet/KVSet 窄接口，便于测试。
type KVStore interface {
	KVGet(key string) (string, bool, error)
	KVSet(key, value string) error
}

// NewA2Adapter 构造 adapter。work/broker/kv 都不得为 nil。
func NewA2Adapter(w work.ManagerLike, b BrokerSubmitter, kv KVStore) *A2Adapter {
	return &A2Adapter{work: w, broker: b, kv: kv}
}

// 编译期断言：*A2Adapter 满足 automation.QueuePort；*task.Broker 满足 BrokerSubmitter。
var (
	_ automation.QueuePort = (*A2Adapter)(nil)
	_ BrokerSubmitter      = (*task.Broker)(nil)
	_ KVStore              = (*store.Store)(nil)
)

// idempotencyPrefix 避免 KV 命名空间冲突。
const idempotencyPrefix = "automation:idem:"

// SubmitRun 实现 automation.QueuePort。先查 KV 幂等记录；未命中则双写 broker + work。
func (a *A2Adapter) SubmitRun(ctx context.Context, p automation.RunPayload) (automation.RunReceipt, error) {
	if a.work == nil || a.broker == nil || a.kv == nil {
		return automation.RunReceipt{}, fmt.Errorf("A2Adapter: work/broker/kv must not be nil")
	}
	kvKey := idempotencyPrefix + p.IdempotencyKey
	if v, ok, err := a.kv.KVGet(kvKey); err == nil && ok && v != "" {
		return automation.RunReceipt{WorkTaskID: v}, nil
	}
	// 先提交到 broker（Custom JSON payload with automation context）。
	brokerPayload := struct {
		AutomationID string   `json:"automation_id"`
		RunID        string   `json:"run_id"`
		Prompt       string   `json:"prompt"`
		Cwds         []string `json:"cwds,omitempty"`
	}{p.AutomationID, p.RunID, p.Prompt, p.Cwds}
	payloadJSON, err := json.Marshal(brokerPayload)
	if err != nil {
		return automation.RunReceipt{}, err
	}
	brokerID, err := a.broker.Submit("automation.run", string(payloadJSON), p.ParentTaskID)
	if err != nil {
		return automation.RunReceipt{}, fmt.Errorf("broker submit: %w", err)
	}
	// 再创建 durable work task（Dispatch: false — broker 已单独提交）。
	w, err := a.work.Create(ctx, work.CreateReq{
		Title:    "automation: " + p.AutomationID,
		Prompt:   p.Prompt,
		ThreadID: p.ThreadID,
	})
	if err != nil {
		return automation.RunReceipt{BrokerTaskID: brokerID}, fmt.Errorf("work create: %w", err)
	}
	if err := a.kv.KVSet(kvKey, w.ID); err != nil {
		return automation.RunReceipt{WorkTaskID: w.ID, BrokerTaskID: brokerID}, fmt.Errorf("kv set idempotency: %w", err)
	}
	return automation.RunReceipt{WorkTaskID: w.ID, BrokerTaskID: brokerID}, nil
}

// Lookup 实现 automation.QueuePort。底层 work.ManagerLike.Read 返回的 work.WorkTask
// 的 Status（A2 的 double-l "cancelled" 或 "pending" 等）经 automation.MapTaskStatus
// 映射到 C1 的 Run 常量（单-l "canceled" 等）。
func (a *A2Adapter) Lookup(ctx context.Context, workTaskID string) (automation.RunStatus, error) {
	w, err := a.work.Read(ctx, workTaskID)
	if err != nil {
		return automation.RunStatus{}, err
	}
	mapped, ok := automation.MapTaskStatus[string(w.Status)]
	if !ok {
		mapped = automation.RunFailed // fail-closed：未知状态视为失败
	}
	return automation.RunStatus{Status: mapped}, nil
}
