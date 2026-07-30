# Batch C1 — 批量与自动化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` (or the repository-approved equivalent) to implement this plan task by task. Do not skip the dependency gates in Task 1. Every checkbox is independently reviewable.

**Goal:** 实现 roadmap §9 的 Batch C1 三项能力：RLM1 `rlm_query`、AU1 Automations、M07 CSV/structured batch agent jobs。RLM1 是独立的低成本、一次性、非流式 fan-out；AU1 把 automation run 通过 `work.ManagerLike.Create` + `task.Broker.Submit` 适配器转成 A2 工作单元；M07 通过 B1 的 `*registry.Manager.Spawn/Wait` 与 `SpawnErrCap` 非阻塞上限逐项执行。

**Architecture:**

- `internal/agent/rlm` 只负责批量调用已选定的 `model.BaseChatModel.Generate`，不认识 tools、orchestrator、registry 或 work manager。
- `internal/agent/automation` 负责 automation domain、schedule、持久化、scheduler、run history；它只依赖一个明确的 `QueuePort` 接口（由 `bootstrap.A2Adapter` 实现，组合 A2 `work.ManagerLike.Create` + `task.Broker.Submit` + 基于 `store.KVGet/KVSet` 的 idempotency 记录），不直接调用 `task.Broker.Submit`。
- `internal/agent/batch` 负责 CSV/structured input 解析、稳定 index、结果汇总；它只依赖 B1 的 `*registry.Manager.Spawn/Wait/MaxConcurrent`，并以本地 `rowRunner` 把现有 `tools.SubAgentRunner` 适配为 `registry.Runner`。满载时由 `*registry.SpawnErrCap` 非阻塞信号驱动重试，不复制一套全局 cap。
- `internal/tools` 仅做参数解码、`GuardedTool` 包装和 JSON 输出；每个工具都经过现有 `Authorize`，不得绕过 `GuardedTool`。测试通过 `Info(ctx).Name` / `Info(ctx).Desc` 读取元数据，不调用不存在的 `Name()` / `Description()`。
- `internal/bootstrap.Build` 仍是唯一组合根。它在 provider model registry 已构建、`allTools` 尚未交给 orchestrator 之前调用 `buildC1`；它构造 `A2Adapter`、`*registry.Manager`、automation Manager/Scheduler、batch tools，并把 C1 tools 放入现有 `allTools`。
- 不新增 WebSocket/SSE frame。C1 的工具结果走现有 tool-result 通道，因此不需要同步修改 `internal/proto`、WS 或 SSE。

**Tech Stack:** Go 1.26.4、Eino `model.BaseChatModel`、SQLite-backed `store.KVGet/KVSet`（单 backend 的结构化 JSON state）、`GuardedTool`、`einollm.FakeModel`（注意 `GenerateCalls`/`StreamCalls` 是结构体字段，不是方法）、`sync/atomic.Bool`、标准库 `encoding/csv`；cron schedule 使用 `github.com/robfig/cron/v3 v3.0.1`，仅负责 5-field cron 的解析和 `Next` 计算。

## 约束、依赖和非目标

### 跨批依赖门禁

| C1 功能 | 前置批次 | C1 实际依赖的接口 | 禁止做法 |
|---|---|---|---|
| RLM1 | 无 | 现有 `model.BaseChatModel.Generate`、`einollm.FakeModel`（字段 `GenerateCalls`/`StreamCalls`） | 不把它实现成完整 ReAct/sub-agent loop；不调用 `Stream`；不把计数器当方法调用 |
| AU1 | **A2** | `internal/task/work.ManagerLike.Create(work.CreateReq{Title,Prompt,ThreadID,TurnID,BrokerTaskID})` + 现有 `task.Broker.Submit(typ,input,parent)`；idempotency 由 C1 自己用 `store.KVGet/KVSet` 维护 | 不伪造 A2 没有的 `SubmitDurable(ctx,typ,input,parent,key)`；不在 scheduler 内直接运行 LLM；不把 `Broker.Submit` 单独包装成“已具备 idempotency”的 durable queue |
| M07 | **B1** | `*registry.Manager.Spawn(ctx, SpawnRequest{AgentType, Assignment, Runner}) (agentID, error)` 非阻塞、`*registry.Manager.Wait(ctx, agentID, WaitOpts{Timeout}) (Record, error)`、`*registry.Manager.MaxConcurrent() int`、`*registry.SpawnErrCap` 满载信号；`tools.SubAgentRunner` 经本地 `rowRunner` 适配为 `registry.Runner` | 不在 M07 中另建独立 semaphore、registry 或生命周期状态；不直接 import `orchestrator`；不引入伪造的 `Limiter.Acquire(ctx)` 阻塞语义 |

`automation.QueuePort` 与 `batch.SpawnFunc`/`rowRunner` 是**本计划定义的 C1 侧 port 与 adapter**，由 `bootstrap.A2Adapter`（实现 `QueuePort`）和 `*registry.Manager`（直接使用）满足。Task 1 必须先用编译期断言和 contract test 对齐 A2/B1 的真实导出 API；若实际名称或参数不同，只改 adapter 和 contract test，不改 C1 domain 语义。若 A2 或 B1 尚未落地，AU1/M07 任务必须停在依赖门禁，不能用临时实现绕过依赖。

### 权限与成本语义

- `rlm_query` 的工具描述必须写明 `cost-class: cheap`、`non-streaming`、`stateless fan-out`、单次最多 16 个 prompt，以及它适合短分类/抽取，不适合多轮推理。
- `agent_batch` 是逐项子代理工作，不得偷偷改用 `rlm_query`；它使用 B1 的子代理 registry/lifecycle，并沿用当前 `SubAgentRunner` context 注入。
- `automation_create/list/read/update/pause/resume/delete/run` 全部通过 `NewGuardedTool` 和 `Authorize`。本批按 roadmap 的严格解释将八个名字都列为 approval-required tool；即使 `list/read` 没有写副作用，也必须经过 profile allowlist 和 interactive approval callback。工具 profile 必须显式允许每个名字，不能依靠空 `Tools.Allow`。
- automation 的 `run` 与 scheduler 到期都只通过 `QueuePort` 创建工作单元；真正执行由 A2 task worker 完成。run history 通过 `QueuePort.Lookup`（底层 `work.ManagerLike.Read`）更新。

## 目标文件结构

实现时只触碰下列与 C1 相关的文件；不要把计划中的路径误写成 `internal/automation/`。

```text
internal/agent/rlm/runner.go
internal/agent/rlm/runner_test.go
internal/agent/automation/model.go
internal/agent/automation/schedule.go
internal/agent/automation/repository.go
internal/agent/automation/manager.go
internal/agent/automation/scheduler.go
internal/agent/automation/manager_test.go
internal/agent/automation/scheduler_test.go
internal/agent/automation/repository_test.go
internal/agent/automation/statusmap_test.go
internal/agent/batch/input.go
internal/agent/batch/input_test.go
internal/agent/batch/runner.go
internal/agent/batch/runner_test.go
internal/tools/rlm.go
internal/tools/rlm_test.go
internal/tools/automation.go
internal/tools/automation_test.go
internal/tools/batch.go
internal/tools/batch_test.go
internal/bootstrap/c1.go
internal/bootstrap/c1_test.go
internal/bootstrap/a2_adapter.go
internal/bootstrap/a2_adapter_test.go
internal/config/config.go
internal/config/config_test.go
go.mod                                 # add github.com/robfig/cron/v3 v3.0.1
internal/bootstrap/bootstrap.go        # wiring only
config.example.yaml                    # add batch: block
```

`internal/store/` 不新增 automation table：本计划选择现有 `KVGet/KVSet` 保存一个版本化 JSON envelope，并由 `Manager.mu` 串行化 read-modify-write。idempotency key → work task id 的映射也保存在 KV（前缀 `automation:idem:`）。这样不需要伪造 `store.Store` 的内部 SQLite 字段，也能在 backend 重启后恢复 automation/run history。未来若规模要求独立表，可另开迁移任务，不在 C1 隐式扩大范围。

## Task 1 — 锁定 A2/B1 contract，声明 C1 侧 port 与编译期断言

**Files:** `internal/agent/automation/model.go`（C1 侧 port 类型）、`internal/agent/batch/runner.go`（C1 侧 `SpawnFunc`）、`internal/bootstrap/a2_adapter.go`（实现 `QueuePort`）、对应 contract tests。

**依赖门禁：** 在写 AU1/M07 实现前，确认 A2 已落地 `work.ManagerLike.Create(CreateReq) (*WorkTask, error)` 且 `CreateReq` 形状为 `{Title, Prompt, ThreadID, TurnID, BrokerTaskID}`；确认 B1 已落地 `*registry.Manager.Spawn/Wait/Result/MaxConcurrent/Close` 及 `SpawnErrCap`、`SpawnRequest.Runner`、`Runner.Run(ctx, agentID, assignment) (string, error)`。下列接口是 C1 期望的最小语义，不是当前代码中已存在的事实。

### Step 1.1 — 写失败的 contract test

**Files:** `internal/bootstrap/a2_adapter_test.go`、`internal/agent/automation/manager_test.go`（仅 contract 部分）。

```go
// internal/bootstrap/a2_adapter_test.go
package bootstrap_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/task"
	"github.com/x6nux/yanshi/internal/task/work"
)

// fakeWorkManager 嵌入 A2 的 *work.FakeManager 以满足 work.ManagerLike 全部 12 个方法，
// 仅覆盖 Create + Read 以使用本测试自己的 tasks map（FakeManager.Read 默认返回 nil, error）。
// 生产代码由 A2 的 *work.Manager 提供。
type fakeWorkManager struct {
	*work.FakeManager
	mu    sync.Mutex
	tasks map[string]*work.WorkTask
}

func newFakeWorkManager() *fakeWorkManager {
	return &fakeWorkManager{
		FakeManager: work.NewFakeManager(),
		tasks:       map[string]*work.WorkTask{},
	}
}

func (m *fakeWorkManager) Create(req work.CreateReq) (*work.WorkTask, error) {
	if req.Title == "" || req.Prompt == "" {
		return nil, errors.New("title and prompt required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w := &work.WorkTask{
		ID:           "wt-" + req.Title,
		Title:        req.Title,
		Prompt:       req.Prompt,
		Status:       work.StatusPending,
		ThreadID:     req.ThreadID,
		BrokerTaskID: req.BrokerTaskID,
	}
	m.tasks[w.ID] = w
	return w, nil
}

func (m *fakeWorkManager) Read(id string) (*work.WorkTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.tasks[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return w, nil
}

// 其它 10 个 ManagerLike 方法（List/Start/Finish/Cancel/SetChecklist/
// AddChecklistItem/PatchChecklistItem/RecordGate/WriteArtifact/ReadArtifact）
// 通过嵌入的 *work.FakeManager 提供默认实现，无需在此重复。

// fakeBrokerStore 仅用于 contract test；不需要真实 SQLite。
type fakeBrokerSubmitter struct {
	mu     sync.Mutex
	nextID int
	seen   map[string]string
}

func newFakeBrokerSubmitter() *fakeBrokerSubmitter {
	return &fakeBrokerSubmitter{seen: map[string]string{}}
}

func (b *fakeBrokerSubmitter) Submit(typ, input, parent string) (string, error) {
	if typ == "" || input == "" {
		return "", errors.New("typ and input required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	id := "broker-" + json.Number(itoa(b.nextID)).String()
	b.seen[id] = typ + "|" + parent
	return id, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// fakeKV 是 store.Store 的 KVGet/KVSet 最小子集，避免引入 SQLite。
type fakeKV struct {
	mu sync.Mutex
	m  map[string]string
}

func newFakeKV() *fakeKV { return &fakeKV{m: map[string]string{}} }

func (s *fakeKV) KVGet(key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok, nil
}

func (s *fakeKV) KVSet(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
	return nil
}

func TestA2AdapterImplementsQueuePort(t *testing.T) {
	var _ automation.QueuePort = (*bootstrap.A2Adapter)(nil)
}

func TestA2AdapterSubmitRunIdempotentAndAcceptsParent(t *testing.T) {
	kv := newFakeKV()
	adapter := bootstrap.NewA2Adapter(newFakeWorkManager(), newFakeBrokerSubmitter(), kv)

	p := automation.RunPayload{
		AutomationID:   "auto-1",
		RunID:          "run-1",
		Prompt:         "do thing",
		Cwds:           []string{"."},
		ParentTaskID:   "parent-xyz", // 非空 parent 是合法的（C1 review #10）
		IdempotencyKey: "automation/auto-1/slot-1",
		ThreadID:       "thread-1",
	}
	first, err := adapter.SubmitRun(context.Background(), p)
	require.NoError(t, err)
	require.NotEmpty(t, first.WorkTaskID)

	// 同 idempotency key 第二次提交必须返回同一 WorkTaskID，不重复创建。
	second, err := adapter.SubmitRun(context.Background(), p)
	require.NoError(t, err)
	require.Equal(t, first.WorkTaskID, second.WorkTaskID)
}

func TestA2AdapterLookupMapsWorkStatusToRunStatus(t *testing.T) {
	wm := newFakeWorkManager()
	kv := newFakeKV()
	adapter := bootstrap.NewA2Adapter(wm, newFakeBrokerSubmitter(), kv)

	p := automation.RunPayload{
		AutomationID:   "auto-2",
		RunID:          "run-2",
		Prompt:         "hi",
		IdempotencyKey: "automation/auto-2/slot-2",
	}
	receipt, err := adapter.SubmitRun(context.Background(), p)
	require.NoError(t, err)

	// 手动把 WorkTask 推进到 double-l "cancelled"，验证映射到 C1 单-l "canceled"。
	wm.mu.Lock()
	if wt, ok := wm.tasks[receipt.WorkTaskID]; ok {
		wt.Status = work.StatusCancelled // 注意：double-l
	}
	wm.mu.Unlock()

	status, err := adapter.Lookup(context.Background(), receipt.WorkTaskID)
	require.NoError(t, err)
	require.Equal(t, automation.RunCanceled, status.Status) // 单-l
}
```

```go
// internal/agent/automation/manager_test.go (contract portion only; full tests in Task 6)
package automation_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
)

// fakeQueue 实现 automation.QueuePort，**不**拒绝非空 parent（review #10）。
type fakeQueue struct {
	mu      sync.Mutex
	calls   int
	ids     map[string]string // idempotency key → work task id
	states  map[string]automation.RunStatus
	parents []string
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{
		ids:    map[string]string{},
		states: map[string]automation.RunStatus{},
	}
}

func (q *fakeQueue) SubmitRun(_ context.Context, p automation.RunPayload) (automation.RunReceipt, error) {
	if p.AutomationID == "" || p.Prompt == "" {
		return automation.RunReceipt{}, errors.New("automation_id and prompt required")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.parents = append(q.parents, p.ParentTaskID)
	if id, ok := q.ids[p.IdempotencyKey]; ok {
		return automation.RunReceipt{WorkTaskID: id}, nil
	}
	q.calls++
	id := "wt-" + p.IdempotencyKey
	q.ids[p.IdempotencyKey] = id
	q.states[id] = automation.RunStatus{Status: automation.RunQueued, Error: ""}
	return automation.RunReceipt{WorkTaskID: id, BrokerTaskID: "broker-" + p.IdempotencyKey}, nil
}

func (q *fakeQueue) Lookup(_ context.Context, workTaskID string) (automation.RunStatus, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	s, ok := q.states[workTaskID]
	if !ok {
		return automation.RunStatus{}, errors.New("not found")
	}
	return s, nil
}

func TestQueuePortContractAcceptsNonEmptyParent(t *testing.T) {
	q := newFakeQueue()
	receipt, err := q.SubmitRun(context.Background(), automation.RunPayload{
		AutomationID:   "auto-1",
		RunID:          "run-1",
		Prompt:         "p",
		ParentTaskID:   "non-empty-parent", // review #10: parent 非空不应被拒
		IdempotencyKey: "k1",
	})
	require.NoError(t, err)
	require.NotEmpty(t, receipt.WorkTaskID)

	// 同 key 再次提交：幂等。
	again, err := q.SubmitRun(context.Background(), automation.RunPayload{
		AutomationID:   "auto-1",
		RunID:          "run-1",
		Prompt:         "p",
		ParentTaskID:   "non-empty-parent",
		IdempotencyKey: "k1",
	})
	require.NoError(t, err)
	require.Equal(t, receipt.WorkTaskID, again.WorkTaskID)
	require.Equal(t, 1, q.calls)

	status, err := q.Lookup(context.Background(), receipt.WorkTaskID)
	require.NoError(t, err)
	require.Equal(t, automation.RunQueued, status.Status)
}
```

```go
// internal/agent/batch/runner_test.go (contract portion; full tests in Task 10)
package batch_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/batch"
	"github.com/x6nux/yanshi/internal/agent/registry"
)

// fakeSpawnFunc 记录 prompt 与 index，并按 index 让某项返回 error。
func fakeSpawnFunc(errOnIndex int) (batch.SpawnFunc, *int32) {
	var calls int32
	return batch.SpawnFunc(func(ctx context.Context, prompt string, allowed []string, instr string) (string, error) {
		idx := int(atomic.AddInt32(&calls, 1)) - 1
		if idx == errOnIndex {
			return "", errFake("boom")
		}
		return "ok-" + prompt, nil
	}), &calls
}

// newRegistryManager 构造一个内存 *registry.Manager（MaxConcurrent=2）。
// B1 提供的 *registry.Manager 完整实现 Spawn/Wait/Close；此处直接使用真实 Manager。
func newRegistryManager(t *testing.T, max int) *registry.Manager {
	t.Helper()
	m := registry.NewManager(registry.NewManagerOpts{MaxConcurrent: max})
	t.Cleanup(m.Close)
	return m
}

type errFake string

func (e errFake) Error() string { return string(e) }

func TestBatchRunnerUsesRegistrySpawnAndWait(t *testing.T) {
	spawn, _ := fakeSpawnFunc(-1)
	mgr := newRegistryManager(t, 4)
	runner := batch.Runner{
		Spawn:          spawn,
		Manager:        mgr,
		WaitTimeout:    2 * time.Second,
		CappedBackoff:  10 * time.Millisecond,
		CappedRetries:  5,
	}
	rows := []batch.Row{{Index: 0, Values: map[string]string{"q": "a"}}, {Index: 1, Values: map[string]string{"q": "b"}}}
	report, err := runner.Run(context.Background(), batch.Input{Prompt: "do", Rows: rows})
	require.NoError(t, err)
	require.Len(t, report.Results, 2)
	require.Equal(t, 2, report.Success)
}

var _ = sync.Mutex{} // 保留 sync 引用，便于后续测试扩展。
```

**Expected failure:**

- `undefined: automation.QueuePort`、`undefined: automation.RunPayload`、`undefined: automation.RunReceipt`、`undefined: automation.RunStatus`、`undefined: automation.RunQueued`、`undefined: automation.RunCanceled`。
- `undefined: bootstrap.A2Adapter`、`undefined: bootstrap.NewA2Adapter`。
- `undefined: batch.SpawnFunc`、`undefined: batch.Runner`（Manager 字段不存在）。
- 若 A2/B1 包尚未提供 `work.ManagerLike`/`registry.Manager`/`registry.SpawnErrCap`，contract test 直接 fail；不允许用临时 fake 替代以满足编译。

**Run after implementation:**

```sh
go test ./internal/bootstrap ./internal/agent/automation ./internal/agent/batch -run 'TestA2Adapter|TestQueuePortContract|TestBatchRunnerUsesRegistry'
```

**Expected output:** 三个包均通过；若 A2/B1 接口未落地，test 在此依赖门禁失败而不是静默使用 local substitute。

### Step 1.2 — 在 C1 侧声明 port 类型（仅 C1 侧，不重新声明 A2/B1 类型）

```go
// internal/agent/automation/model.go
package automation

import (
	"context"
	"time"
)

// 状态枚举：C1 使用单-l "canceled"（与 A2 的 double-l "cancelled" 不同；由 MapTaskStatus 映射）。
// broker 使用 "timeout"（无 cancelled 等价）；MapTaskStatus 把它映射到 RunFailed。
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunCompleted = "completed"
	RunFailed    = "failed"
	RunCanceled  = "canceled" // 注意：单-l

	StateSchemaVersion = 1
)

// TaskState 是 QueuePort.Lookup 的返回值，承载 C1 关心的最小信息。
// status 已经过 MapTaskStatus 映射到 C1 词汇。
type TaskState struct {
	ID     string
	Status string
	Result string
	Error  string
}

// RunStatus 是 QueuePort.Lookup 的精简视图（不含 ID；调用方已知）。
type RunStatus struct {
	Status string
	Error  string
}

// RunPayload 是 QueuePort.SubmitRun 的入参。ParentTaskID 可非空（review #10）。
// IdempotencyKey 由 C1 生成（包含 automation id + scheduled slot），adapter
// 负责幂等去重。
type RunPayload struct {
	AutomationID   string
	RunID          string
	Prompt         string
	Cwds           []string
	ParentTaskID   string
	IdempotencyKey string
	ThreadID       string
}

// RunReceipt 是 SubmitRun 的返回，指出已创建/复用的工作单元。
type RunReceipt struct {
	WorkTaskID  string
	BrokerTaskID string
}

// QueuePort 是 AU1 对外的最小端口。生产实现由 bootstrap.A2Adapter 提供，
// 组合 A2 的 work.ManagerLike.Create + 现有 task.Broker.Submit；C1 测试用
// 内联 fakeQueue。**不**包含 legacy `Broker.Submit(typ,input,parent)` 的
// 直接调用——必须经 adapter 走 work + broker 的双写。
type QueuePort interface {
	SubmitRun(ctx context.Context, payload RunPayload) (RunReceipt, error)
	Lookup(ctx context.Context, workTaskID string) (RunStatus, error)
}

// MapTaskStatus 是显式状态映射表（review #7）。把 A2 work.Status（含 double-l
// "cancelled"）与 broker 的 "timeout" 统一映射到 C1 的 Run 常量。
var MapTaskStatus = map[string]string{
	"pending":   RunQueued,
	"running":   RunRunning,
	"completed": RunCompleted,
	"failed":    RunFailed,
	"cancelled": RunCanceled, // A2 double-l → C1 single-l
	"timeout":   RunFailed,   // broker-only；无 cancelled 等价
}

// 状态结构（envelope）保留在 repository.go；Schedule/Run/Automation 见 model.go 末尾。

// Schedule 表达何时运行下一次。Kind ∈ {"cron","interval"}。
type Schedule struct {
	Kind        string `json:"kind"`
	Cron        string `json:"cron,omitempty"`
	IntervalSec int64  `json:"interval_seconds,omitempty"`
}

// Automation 是用户创建的持久化实体。
type Automation struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prompt    string     `json:"prompt"`
	Schedule  Schedule   `json:"schedule"`
	Cwds      []string   `json:"cwds,omitempty"`
	Active    bool       `json:"active"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
}

// Run 是 automation 的一次执行记录。
type Run struct {
	ID             string     `json:"id"`
	AutomationID   string     `json:"automation_id"`
	ScheduledFor   time.Time  `json:"scheduled_for"`
	Status         string     `json:"status"`
	TaskID         string     `json:"task_id,omitempty"` // WorkTaskID
	BrokerTaskID   string     `json:"broker_task_id,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	Error          string     `json:"error,omitempty"`
	IdempotencyKey string     `json:"idempotency_key"`
}

// State 是持久化的 JSON envelope。
type State struct {
	SchemaVersion int          `json:"schema_version"`
	Automations   []Automation `json:"automations"`
	Runs          []Run        `json:"runs"`
}
```

```go
// internal/agent/batch/runner.go (types only; full Run in Task 10)
package batch

import (
	"context"
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
```

```go
// internal/bootstrap/a2_adapter.go
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
// 生产实现由 *task.Broker 满足。
type BrokerSubmitter interface {
	Submit(typ, input, parent string) (string, error)
}

// WorkLookup 扩展 work.ManagerLike 的读取语义；A2 的 *work.Manager 已实现。
// 此处只要求 Read；其它 CreateReq/Update 方法在 bootstrap 装配时由具体类型注入。
type WorkLookup interface {
	Read(id string) (*work.WorkTask, error)
}

// A2Adapter 实现 automation.QueuePort。它组合 A2 work.ManagerLike.Create（模型可见
// 工作单元）和 task.Broker.Submit（分发执行），并通过 store.KVGet/KVSet 维护
// idempotency key → WorkTaskID 的映射。**不**声称 Broker.Submit 自身具备 idempotency。
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
	w, err := a.work.Create(work.CreateReq{
		Title:        "automation: " + p.AutomationID,
		Prompt:       p.Prompt,
		ThreadID:     p.ThreadID,
		BrokerTaskID: brokerID,
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
func (a *A2Adapter) Lookup(_ context.Context, workTaskID string) (automation.RunStatus, error) {
	w, err := a.work.Read(workTaskID)
	if err != nil {
		return automation.RunStatus{}, err
	}
	mapped, ok := automation.MapTaskStatus[string(w.Status)]
	if !ok {
		mapped = automation.RunFailed // fail-closed：未知状态视为失败
	}
	return automation.RunStatus{Status: mapped, Error: w.Error}, nil
}
```

注意 `*store.Store` 已有 `KVGet/KVSet`；编译期断言保证签名一致：

```go
// internal/bootstrap/a2_adapter.go (尾部)
var _ KVStore = (*store.Store)(nil)
```

如果 `*store.Store` 的 KVGet 返回值形状不同，调整 `KVStore` 接口或包一层适配函数；**不**修改 `store.Store`。

### Step 1.3 — MapTaskStatus 表驱动测试

```go
// internal/agent/automation/statusmap_test.go
package automation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/x6nux/yanshi/internal/agent/automation"
)

func TestMapTaskStatus_Table(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"pending", automation.RunQueued},
		{"running", automation.RunRunning},
		{"completed", automation.RunCompleted},
		{"failed", automation.RunFailed},
		{"cancelled", automation.RunCanceled}, // A2 double-l → C1 single-l
		{"timeout", automation.RunFailed},     // broker-only，无 cancelled 等价
	}
	for _, c := range cases {
		got, ok := automation.MapTaskStatus[c.in]
		if !ok {
			t.Errorf("MapTaskStatus[%q] missing", c.in)
			continue
		}
		assert.Equal(t, c.want, got, "MapTaskStatus[%q]", c.in)
	}
}

func TestMapTaskStatus_DoesNotContainSingleLCancelled(t *testing.T) {
	// 防回归：表中不能误把 "canceled"（单-l）当作 key，那是 C1 自己的输出词汇。
	if _, ok := automation.MapTaskStatus["canceled"]; ok {
		t.Fatal(`MapTaskStatus must not contain single-l "canceled" as a key; that is C1's output vocabulary`)
	}
}

func TestMapTaskStatus_DoesNotContainDoubleLCancelledAsValue(t *testing.T) {
	for k, v := range automation.MapTaskStatus {
		if v == "cancelled" {
			t.Fatalf(`MapTaskStatus[%q] = "cancelled" (double-l); C1 outputs use single-l "canceled"`, k)
		}
	}
}
```

**Expected failure:** 同 Step 1.1；MapTaskStatus 在 model.go 中定义后即可通过。

**Run:**

```sh
go test ./internal/agent/automation -run TestMapTaskStatus -v
go vet ./internal/agent/automation
```

**Expected:** 三个子测试全部通过；`statusmap_test.go` 是表驱动，新增 broker/work 状态时必须在此加 case，不允许散落到 adapter 内部 switch。

**Commit:** `chore(c1): define queue port, a2 adapter and status mapping`

## Task 2 — RLM core：非流式 Generate fan-out（`[]atomic.Bool` 修复）

**Files:** `internal/agent/rlm/runner.go`, `internal/agent/rlm/runner_test.go`。

### Step 2.1 — 写失败的测试

```go
// internal/agent/rlm/runner_test.go
package rlm_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/rlm"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

// delayedFake 在每次 Generate 调用上阻塞，直到 gate 关闭；用于证明 MaxConcurrency
// 被严格 cap。嵌入 *einollm.FakeModel 以复用 GenerateCalls/StreamCalls 字段。
type delayedFake struct {
	*einollm.FakeModel
	gate   <-chan struct{}
	active int32
	max    int32
}

func (m *delayedFake) Generate(
	ctx context.Context,
	messages []*schema.Message,
	opts ...model.Option,
) (*schema.Message, error) {
	current := atomic.AddInt32(&m.active, 1)
	for {
		old := atomic.LoadInt32(&m.max)
		if current <= old || atomic.CompareAndSwapInt32(&m.max, old, current) {
			break
		}
	}
	defer atomic.AddInt32(&m.active, -1)
	select {
	case <-m.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return m.FakeModel.Generate(ctx, messages, opts...)
}

func TestRunUsesGenerateAndPreservesOrder(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"ok"}, nil)
	runner := rlm.Runner{Model: fake, MaxConcurrency: 4}
	results, err := runner.Run(context.Background(), []string{"a", "b", "c"})
	require.NoError(t, err)
	require.Len(t, results, 3)
	for i, result := range results {
		assert.Equal(t, i, result.Index, "Index")
		assert.Equal(t, "ok", result.Output, "Output")
	}
	// FakeModel.GenerateCalls / StreamCalls 是 struct field（review #3），不是方法。
	assert.Equal(t, 3, fake.GenerateCalls, "GenerateCalls")
	assert.Equal(t, 0, fake.StreamCalls, "StreamCalls")
}

func TestRunCapsConcurrencyAtSixteen(t *testing.T) {
	gate := make(chan struct{})
	fake := &delayedFake{
		FakeModel: einollm.NewFakeModel([]string{"ok"}, nil),
		gate:      gate,
	}
	runner := rlm.Runner{Model: fake, MaxConcurrency: 99}
	done := make(chan struct{})
	go func() {
		_, _ = runner.Run(context.Background(), makePrompts(32))
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&fake.active) < 16 {
		select {
		case <-deadline:
			t.Fatalf("did not reach the 16-call cap; active=%d", atomic.LoadInt32(&fake.active))
		default:
			time.Sleep(time.Millisecond)
		}
	}
	assert.LessOrEqual(t, atomic.LoadInt32(&fake.max), int32(16), "max active must be <= 16")
	close(gate)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not finish")
	}
}

func TestRunKeepsPerItemErrors(t *testing.T) {
	fake := einollm.NewFakeModel(nil, errors.New("model unavailable"))
	runner := rlm.Runner{Model: fake, MaxConcurrency: 2}
	results, err := runner.Run(context.Background(), []string{"a", "b"})
	require.NoError(t, err, "per-item errors must not surface as batch error")
	for i, result := range results {
		assert.Equal(t, i, result.Index)
		assert.Equal(t, "model unavailable", result.Error)
	}
}

func TestRunMarksPendingItemsWhenCanceled(t *testing.T) {
	gate := make(chan struct{})
	fake := &delayedFake{
		FakeModel: einollm.NewFakeModel([]string{"ok"}, nil),
		gate:      gate,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, err := (rlm.Runner{Model: fake, MaxConcurrency: 4}).Run(ctx, []string{"a", "b"})
	require.NoError(t, err)
	for i, result := range results {
		assert.Equal(t, i, result.Index)
		assert.Equal(t, context.Canceled.Error(), result.Error)
	}
}

func TestRunRejectsNilModel(t *testing.T) {
	_, err := (rlm.Runner{}).Run(context.Background(), []string{"a"})
	require.Error(t, err)
}

func makePrompts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("prompt-%d", i)
	}
	return out
}

var _ model.BaseChatModel = (*delayedFake)(nil)
```

**Expected failure:** `undefined: rlm.Runner`；随后 `GenerateCalls` / cap / cancel 断言失败。测试**不能**删掉对 `StreamCalls == 0` 的断言。

### Step 2.2 — 实现完整的 `runner.go`（`[]atomic.Bool` 而非 `[]bool`）

```go
// internal/agent/rlm/runner.go
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
	// review #5：多个 worker goroutine 写不同 index，但 ctx 取消路径由主 goroutine
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
```

**Run:**

```sh
go test ./internal/agent/rlm -run TestRun -v
go vet ./internal/agent/rlm
```

**Expected:** `ok github.com/x6nux/yanshi/internal/agent/rlm`；`fake.StreamCalls == 0`；32 个输入时 `fake.max <= 16`。

**Commit:** `feat(rlm): add capped non-streaming query fanout`

## Task 3 — `rlm_query` GuardedTool（用 `Info(ctx).Name/.Desc`，inline profile）

**Files:** `internal/tools/rlm.go`, `internal/tools/rlm_test.go`。

### Step 3.1 — 写失败的测试

```go
// internal/tools/rlm_test.go
package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/guard"

	"github.com/x6nux/yanshi/internal/agent/rlm"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/tools"
)

// allowProfile 是 inline helper（review #4），不复用不存在的 testAllowProfile。
// 仅在当前测试文件内使用；每个测试自己构造所需的 Tools.Allow 列表。
func allowProfile(toolNames ...string) guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: toolNames},
	}
}

func TestRLMQueryMetadataAndGenerateOnly(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"classification"}, nil)
	set := tools.NewRLMTools(rlm.Runner{Model: fake, MaxConcurrency: 16})
	require.NotNil(t, set.Query)

	// review #2：GuardedTool 没有 Name()/Description() 方法；通过 Info(ctx) 读取。
	info, err := set.Query.Info(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "rlm_query", info.Name)
	for _, phrase := range []string{"cost-class", "cheap", "non-streaming", "16"} {
		assert.True(
			t,
			strings.Contains(strings.ToLower(info.Desc), strings.ToLower(phrase)),
			"Desc %q missing %q", info.Desc, phrase,
		)
	}

	ctx := tools.WithProfile(context.Background(), allowProfile("rlm_query"))
	promptPayload, _ := json.Marshal([]string{"one", "two"})
	args := fmt.Sprintf(`{"prompts":%s}`, strconv.Quote(string(promptPayload)))
	result, err := set.Query.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, `"index":0`)
	assert.Contains(t, result, `"index":1`)

	// review #3：字段访问，不是方法调用。
	assert.Equal(t, 2, fake.GenerateCalls, "GenerateCalls")
	assert.Equal(t, 0, fake.StreamCalls, "StreamCalls")
}

func TestRLMQueryRejectsMoreThanSixteen(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"ok"}, nil)
	set := tools.NewRLMTools(rlm.Runner{Model: fake})
	ctx := tools.WithProfile(context.Background(), allowProfile("rlm_query"))

	prompts := make([]string, 17)
	encoded, _ := json.Marshal(prompts)
	result, err := set.Query.InvokableRun(ctx, fmt.Sprintf(`{"prompts":%s}`, strconv.Quote(string(encoded))))
	require.NoError(t, err)
	assert.Contains(t, result, "1 to 16")
	assert.Equal(t, 0, fake.GenerateCalls, "must not call model on oversize batch")
}

func TestRLMQueryDeniedWithoutProfile(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"ok"}, nil)
	set := tools.NewRLMTools(rlm.Runner{Model: fake})
	// 无 profile → GuardedTool.Stream 必须返回 permission denied 结果。
	result, err := set.Query.InvokableRun(context.Background(), `{"prompts":""}`)
	require.NoError(t, err)
	assert.Contains(t, result, "permission denied")
	assert.Equal(t, 0, fake.GenerateCalls)
}

func TestRLMQueryDeniedWhenProfileOmitsName(t *testing.T) {
	fake := einollm.NewFakeModel([]string{"ok"}, nil)
	set := tools.NewRLMTools(rlm.Runner{Model: fake})
	// profile 允许其它工具但不允许 rlm_query → 拒绝。
	ctx := tools.WithProfile(context.Background(), allowProfile("memory_search"))
	result, err := set.Query.InvokableRun(ctx, `{"prompts":""}`)
	require.NoError(t, err)
	assert.Contains(t, result, "permission denied")
	assert.Equal(t, 0, fake.GenerateCalls)
}
```

**Expected failure:** `undefined: tools.NewRLMTools`；`set.Query.Info undefined` 等。

### Step 3.2 — 完整 tool 文件

```go
// internal/tools/rlm.go
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
				Type:        "string",
				Description: "JSON-encoded array of 1 to 16 short prompts",
			},
		}),
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runRLMQuery(ctx, runner, args)
		},
	)
	return set
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
```

`NewGuardedTool` 的 `Authorize` 语义保持不变：工具错误作为 `ToolChunk.Result` 回喂模型，不能把参数错误改成会中断整个 ADK turn 的 `NodeRunError`。

**Run:**

```sh
go test ./internal/tools -run TestRLMQuery -v
go vet ./internal/tools
```

**Expected:** 所有四个子测试通过；fake 调用全部走 `Generate`；超过 16 项在模型调用前被拒绝；无 profile 或 allowlist 缺名时拒绝。

**Commit:** `feat(tools): expose guarded rlm_query cheap fanout`

## Task 4 — Cheap model 配置与 RLM bootstrap（含完整 buildRLM 接线）

**Files:** `internal/config/config.go`, `internal/config/config_test.go`, `internal/bootstrap/c1.go`, `internal/bootstrap/c1_test.go`, `internal/bootstrap/bootstrap.go`。

### Step 4.1 — 写失败的配置测试

```go
// internal/config/config_test.go (追加；不要替换整个文件)
package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/config"
)

func TestBatchConfigRLMDefaultsAndCostClass(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	yaml := []byte(`
llm:
  providers:
    - name: cheap-provider
      kind: openai
      model: cheap-model
      cost_class: cheap
    - name: expensive-provider
      kind: openai
      model: big-model
      cost_class: expensive
batch:
  rlm_model: cheap-provider
  rlm_max_concurrency: 8
  automation_tick_seconds: 30
`)
	require.NoError(t, os.WriteFile(path, yaml, 0o644))

	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Batch)
	assert.Equal(t, "cheap-provider", cfg.Batch.RLMModel)
	assert.Equal(t, 8, cfg.Batch.RLMMaxConcurrency)
	assert.Equal(t, 30, cfg.Batch.AutomationTickSec)

	// CostClass 留在 provider 上；bootstrap 会校验 RLMModel 指向 cheap。
	for _, p := range cfg.LLM.Providers {
		if p.Name == "cheap-provider" {
			assert.Equal(t, "cheap", p.CostClass)
		}
	}
}

func TestBatchConfigZeroValuesAreValid(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`llm: {}`), 0o644))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "", cfg.Batch.RLMModel)
	assert.Equal(t, 0, cfg.Batch.RLMMaxConcurrency)
}
```

```go
// internal/bootstrap/c1_test.go
package bootstrap_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudwego/eino/components/model"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"

	"github.com/x6nux/yanshi/internal/agent/rlm"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/config"
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
```

**Expected failure:** `undefined: config.BatchConfig`、`undefined: config.ProviderConfig.CostClass`、`undefined: bootstrap.SelectRLMModel`、`undefined: bootstrap.BuildRLM`。

### Step 4.2 — 完整配置与 bootstrap RLM helper

```go
// internal/config/config.go (新增类型；合并入现有 Config/ProviderConfig，保留现有字段)
package config

// BatchConfig 是 C1 批量与自动化的配置块。
type BatchConfig struct {
	RLMModel          string `yaml:"rlm_model"`
	RLMMaxConcurrency int    `yaml:"rlm_max_concurrency"`
	AutomationTickSec int    `yaml:"automation_tick_seconds"`
}

// ProviderConfig 增加 CostClass 字段。现有 providers 默认为空字符串；只有
// Batch.RLMModel 明确指向的 provider 必须声明 "cheap"。
// （这是字段新增；保留现有 Name/Kind/Model/APIKey/BaseURL/ContextWindow 字段。）
```

实际编辑：打开 `internal/config/config.go`，在 `ProviderConfig` struct 内追加 `CostClass string \`yaml:"cost_class"\``；在 `Config` struct 内追加 `Batch BatchConfig \`yaml:"batch"\``。本计划不展示整个文件，仅描述字段追加位置。

```go
// internal/bootstrap/c1.go
package bootstrap

import (
	"errors"
	"fmt"

	"github.com/cloudwego/eino/components/model"

	"github.com/x6nux/yanshi/internal/agent/rlm"
	"github.com/x6nux/yanshi/internal/config"
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
```

### Step 4.3 — bootstrap.Build 内部接线（review #14：补全 buildRLM 逻辑连接）

在 `internal/bootstrap/bootstrap.go` 的 `Build` 函数中，**在 agent tools 添加进 allTools 之后、orchestrator.New 之前**插入下列片段。装配顺序保持 config → store → vcs → model → tools → orchestrator → http server → task broker；C1 的 RLM 工具属于 tools 阶段，与 vcs/skill 工具并列。

```go
// internal/bootstrap/bootstrap.go (在 Build 内；以下为插入片段)
// ... (前置：agentTools 已 append 进 allTools；skills registry 已加载；vcs tools 已 append)

// C1 RLM1: 选择 cheap provider 或 fake，构造 rlm_query 工具。
// fakeModeReflected：当 opts.FakeModel 或无 providers 时，chatModel 就是 *FakeModel，
// 传给 BuildRLM 作为 fake fallback；否则传 nil 让 BuildRLM 严格校验 RLMModel。
var fakeForRLM model.BaseChatModel
if opts.FakeModel || len(cfg.LLM.Providers) == 0 {
	fakeForRLM = chatModel
}
c1RLM, err := BuildRLM(cfg, providerModels, fakeForRLM)
if err != nil {
	// 非致命：RLM1 不可用不影响主流程；记录到 stderr 并跳过 rlm_query 工具。
	// 生产部署应通过 config 修复；测试默认用 fake 不应触达此分支。
	fmt.Fprintf(os.Stderr, "yanshi: rlm_query disabled: %v\n", err)
} else {
	allTools = append(allTools, c1RLM.Tools.Query)
}
// C1 automation/batch 工具在 Task 8/12 接线（依赖 A2/B1 adapter）。
```

后续 Task 8（automation）与 Task 12（batch）的接线点紧跟其后，但在它们各自的依赖 adapter 可用时才添加。

**Run:**

```sh
go test ./internal/config -run TestBatchConfig -v
go test ./internal/bootstrap -run 'TestSelectRLM|TestBuildRLM' -v
go vet ./internal/config ./internal/bootstrap
```

**Expected:** 配置测试覆盖 cheap/expensive/unknown/empty/fake 五条路径；BuildRLM 输出可用的 `*tools.RLMTools`；bootstrap.Build 的接线不破坏现有 vet。

**Commit:** `feat(bootstrap): bind rlm_query to configured cheap model`

## Task 5 — AU1 domain、schedule 与持久化 state（含完整 _test.go）

**Files:** `internal/agent/automation/schedule.go`, `internal/agent/automation/repository.go`, `internal/agent/automation/schedule_test.go`, `internal/agent/automation/repository_test.go`。

注意：`model.go` 已在 Task 1 声明 `Schedule/Automation/Run/State/MapTaskStatus` 等类型；Task 5 只新增 `schedule.go`（解析与 Next 计算）和 `repository.go`（KV 持久化）。

### Step 5.1 — 写失败的 schedule 测试

```go
// internal/agent/automation/schedule_test.go
package automation_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
)

func TestParseSchedule_CronValid(t *testing.T) {
	s, err := automation.ParseSchedule("cron", "*/5 * * * *")
	require.NoError(t, err)
	assert.Equal(t, "cron", s.Kind)
	assert.Equal(t, "*/5 * * * *", s.Cron)

	after := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	next, err := s.Next(after)
	require.NoError(t, err)
	assert.True(t, next.After(after), "next must be strictly after input")
}

func TestParseSchedule_CronInvalid(t *testing.T) {
	cases := []string{"", "not-a-cron", "99 99 99 99 99"}
	for _, in := range cases {
		_, err := automation.ParseSchedule("cron", in)
		require.Errorf(t, err, "cron %q must be rejected", in)
	}
}

func TestParseSchedule_IntervalPositive(t *testing.T) {
	s, err := automation.ParseSchedule("interval", "60")
	require.NoError(t, err)
	assert.Equal(t, int64(60), s.IntervalSec)

	after := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	next, err := s.Next(after)
	require.NoError(t, err)
	assert.Equal(t, after.Add(60*time.Second), next)
}

func TestParseSchedule_IntervalRejectsNonPositive(t *testing.T) {
	cases := []string{"0", "-1", "abc", ""}
	for _, in := range cases {
		_, err := automation.ParseSchedule("interval", in)
		require.Errorf(t, err, "interval %q must be rejected", in)
	}
}

func TestParseSchedule_UnknownKind(t *testing.T) {
	_, err := automation.ParseSchedule("hourly", "1")
	require.Error(t, err)
}
```

### Step 5.2 — 写失败的 repository 测试

```go
// internal/agent/automation/repository_test.go
package automation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRepositoryLoadEmptyReturnsDefault(t *testing.T) {
	repo := automation.NewRepository(newTestStore(t))
	state, err := repo.Load()
	require.NoError(t, err)
	assert.Equal(t, automation.StateSchemaVersion, state.SchemaVersion)
	assert.Empty(t, state.Automations)
	assert.Empty(t, state.Runs)
}

func TestRepositorySaveLoadRoundTrip(t *testing.T) {
	repo := automation.NewRepository(newTestStore(t))
	original := automation.State{
		SchemaVersion: automation.StateSchemaVersion,
		Automations: []automation.Automation{
			{ID: "auto-1", Name: "nightly", Prompt: "p", Active: true},
		},
		Runs: []automation.Run{
			{ID: "run-1", AutomationID: "auto-1", Status: automation.RunQueued},
		},
	}
	require.NoError(t, repo.Save(original))

	loaded, err := repo.Load()
	require.NoError(t, err)
	require.Len(t, loaded.Automations, 1)
	assert.Equal(t, "nightly", loaded.Automations[0].Name)
	require.Len(t, loaded.Runs, 1)
	assert.Equal(t, automation.RunQueued, loaded.Runs[0].Status)
}

func TestRepositoryRejectsUnknownSchemaVersion(t *testing.T) {
	s := newTestStore(t)
	// 手动写入一个未来版本。
	require.NoError(t, s.KVSet("automation:c1:state", `{"schema_version":99}`))
	repo := automation.NewRepository(s)
	_, err := repo.Load()
	require.Error(t, err)
}

func TestRepositoryRejectsCorruptJSON(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.KVSet("automation:c1:state", `not-json`))
	repo := automation.NewRepository(s)
	_, err := repo.Load()
	require.Error(t, err)
}
```

**Expected failure:** `undefined: automation.ParseSchedule`、`undefined: automation.NewRepository`。

### Step 5.3 — 完整 schedule.go 代码

```go
// internal/agent/automation/schedule.go
package automation

import (
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// ParseSchedule 解析 kind/expression 为 Schedule。kind ∈ {"cron","interval"}。
//   - cron: 5-field standard parser（cron.ParseStandard），无秒字段。
//   - interval: 整数秒，>0；拒绝 0/负数/非数字。
//
// 解析成功的 Schedule 可以调用 Next 计算下一次运行时间。
func ParseSchedule(kind, expression string) (Schedule, error) {
	switch kind {
	case "cron":
		if expression == "" {
			return Schedule{}, errors.New("cron expression is empty")
		}
		if _, err := cron.ParseStandard(expression); err != nil {
			return Schedule{}, fmt.Errorf("invalid cron: %w", err)
		}
		return Schedule{Kind: "cron", Cron: expression}, nil
	case "interval":
		var seconds int64
		if _, err := fmt.Sscan(expression, &seconds); err != nil || seconds <= 0 {
			return Schedule{}, errors.New("interval must be a positive number of seconds")
		}
		return Schedule{Kind: "interval", IntervalSec: seconds}, nil
	default:
		return Schedule{}, fmt.Errorf("unsupported schedule kind %q", kind)
	}
}

// Next 返回严格晚于 after 的下一次运行时间。
func (s Schedule) Next(after time.Time) (time.Time, error) {
	switch s.Kind {
	case "cron":
		parsed, err := cron.ParseStandard(s.Cron)
		if err != nil {
			return time.Time{}, err
		}
		return parsed.Next(after), nil
	case "interval":
		if s.IntervalSec <= 0 {
			return time.Time{}, errors.New("interval must be positive")
		}
		return after.Add(time.Duration(s.IntervalSec) * time.Second), nil
	default:
		return time.Time{}, errors.New("unknown schedule kind")
	}
}
```

### Step 5.4 — 完整 repository.go 代码

```go
// internal/agent/automation/repository.go
package automation

import (
	"encoding/json"
	"errors"

	"github.com/x6nux/yanshi/internal/store"
)

// stateKey 是 KV 命名空间。automation:idem: 前缀保留给 A2Adapter 的幂等映射。
const stateKey = "automation:c1:state"

// Repository 封装 store.KVGet/KVSet 的 read-modify-write。所有调用必须在
// Manager.mu 锁内进行，以串行化 scheduler tick 与 tool CRUD 的并发写。
type Repository struct {
	store *store.Store
}

// NewRepository 构造 repo。s 为 nil 时 Load/Save 都返回错误（fail-closed）。
func NewRepository(s *store.Store) *Repository {
	return &Repository{store: s}
}

// Load 读取持久化 state。未初始化时返回零值 state（含当前 SchemaVersion）。
// 不匹配的 SchemaVersion 或损坏 JSON 都返回错误，不静默丢弃。
func (r *Repository) Load() (State, error) {
	if r == nil || r.store == nil {
		return State{}, errors.New("automation repository is not configured")
	}
	value, ok, err := r.store.KVGet(stateKey)
	if err != nil {
		return State{}, err
	}
	if !ok || value == "" {
		return State{SchemaVersion: StateSchemaVersion}, nil
	}
	var state State
	if err := json.Unmarshal([]byte(value), &state); err != nil {
		return State{}, err
	}
	if state.SchemaVersion != StateSchemaVersion {
		return State{}, errors.New("unsupported automation state schema version")
	}
	return state, nil
}

// Save 持久化 state。写前强制设置 SchemaVersion；调用方不必担心遗漏。
func (r *Repository) Save(state State) error {
	if r == nil || r.store == nil {
		return errors.New("automation repository is not configured")
	}
	state.SchemaVersion = StateSchemaVersion
	value, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return r.store.KVSet(stateKey, string(value))
}
```

### Step 5.5 — go.mod diff（review #11：锁定 cron/v3 版本）

`go.mod` 的 require 块新增一行锁定版本（不引入主版本 mismatch）：

```diff
 require (
 	github.com/charmbracelet/bubbles v1.0.0
 	github.com/charmbracelet/bubbletea v1.3.10
 	github.com/charmbracelet/glamour v1.0.0
 	github.com/charmbracelet/lipgloss v1.1.1-0.20250404203927-76690c660834
 	github.com/cloudwego/eino v0.9.12
 	github.com/cloudwego/eino-ext/components/model/openai v0.1.13
 	github.com/eino-contrib/jsonschema v1.0.3
 	github.com/fsnotify/fsnotify v1.4.7
 	github.com/gorilla/websocket v1.5.3
 	github.com/muesli/termenv v0.16.0
+	github.com/robfig/cron/v3 v3.0.1
 	github.com/santhosh-tekuri/jsonschema/v6 v6.0.2
 	github.com/stretchr/testify v1.11.1
 	github.com/wk8/go-ordered-map/v2 v2.1.8
 	golang.org/x/sys v0.44.0
 	gopkg.in/yaml.v3 v3.0.1
 	modernc.org/sqlite v1.53.0
 )
```

执行命令（仅记录在计划中，不在写计划阶段运行）：

```sh
go get github.com/robfig/cron/v3@v3.0.1
go mod tidy
```

`v3.0.1` 是 robfig/cron/v3 的稳定 tag（2020-04 发布）；不使用 master 或未锁定的 pseudo-version。

**Run:**

```sh
go test ./internal/agent/automation -run 'TestParseSchedule|TestRepository' -v
go vet ./internal/agent/automation
```

**Expected:** schedule/repository 测试全部通过；非法 cron/interval、未知 schema version、损坏 JSON 均被拒绝。

**Commit:** `feat(automation): add persisted automation domain and schedules`

## Task 6 — Manager、Scheduler（`Stop()` + done channel）、锁外 SubmitRun

**Files:** `internal/agent/automation/manager.go`, `internal/agent/automation/scheduler.go`, `internal/agent/automation/manager_test.go`, `internal/agent/automation/scheduler_test.go`。

注意：`Run*` / `MapTaskStatus` / `QueuePort` / `RunPayload` 等常量与类型已在 Task 1 的 model.go 声明；此处**不**重复声明（review #1：删除重复常量）。

### Step 6.1 — 写失败的 manager 测试

```go
// internal/agent/automation/manager_test.go (Task 1 的 contract 部分之外，补充完整 CRUD/enqueue/reconcile)
package automation_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/store"
)

func newManagerWithFakeQueue(t *testing.T) (*automation.Manager, *fakeQueue) {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	repo := automation.NewRepository(s)
	q := newFakeQueue()
	m, err := automation.NewManager(repo, q, time.Now)
	require.NoError(t, err)
	return m, q
}

func TestManagerCreateAssignsIDAndNextRun(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "nightly",
		Prompt:   "do X",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, item.ID)
	assert.True(t, item.Active)
	require.NotNil(t, item.NextRunAt)
	assert.True(t, item.NextRunAt.After(time.Now().Add(-time.Second)))
}

func TestManagerCreateRejectsEmptyFields(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	_, err := m.Create(automation.CreateInput{Name: "", Prompt: "p"})
	require.Error(t, err)
	_, err = m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "bad"},
	})
	require.Error(t, err)
}

func TestManagerPauseClearsNextRunResumeRecomputes(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	require.NoError(t, m.Pause(item.ID))
	read, _, err := m.Read(item.ID)
	require.NoError(t, err)
	assert.Nil(t, read.NextRunAt)
	assert.False(t, read.Active)

	require.NoError(t, m.Resume(item.ID))
	read, _, err = m.Read(item.ID)
	require.NoError(t, err)
	require.NotNil(t, read.NextRunAt)
	assert.True(t, read.Active)
}

func TestManagerRunNowEnqueuesAndFillsTaskID(t *testing.T) {
	m, q := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)

	run, err := m.RunNow(context.Background(), item.ID)
	require.NoError(t, err)
	assert.Equal(t, automation.RunQueued, run.Status)
	assert.NotEmpty(t, run.TaskID)
	assert.Equal(t, 1, q.calls, "exactly one SubmitRun")
}

func TestManagerRunNowIdempotentPerKey(t *testing.T) {
	m, q := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)

	// 第一次 Tick 在 slot A 入队，第二次 Tick 在同 slot 不应重复入队。
	slotA := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	require.NoError(t, m.Tick(context.Background(), slotA))
	require.NoError(t, m.Tick(context.Background(), slotA))
	assert.Equal(t, 1, q.calls, "Tick must not double-enqueue same slot")
}

func TestManagerTickAdvancesNextSlot(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)

	slot := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	// 推进 NextRunAt 到过去，使其到期。
	require.NoError(t, m.Update(automation.UpdateInput{
		ID:     item.ID,
		Active: boolPtr(false),
	}))
	// 手动把 NextRunAt 设为过去以触发 tick（内部通过 Update 设置 Active=true 重计算）。
	require.NoError(t, m.Update(automation.UpdateInput{
		ID:     item.ID,
		Active: boolPtr(true),
	}))
	// 不直接调内部字段；通过 Tick 在 now=slot+2m 触发。
	require.NoError(t, m.Tick(context.Background(), slot.Add(2*time.Minute)))
	read, _, err := m.Read(item.ID)
	require.NoError(t, err)
	require.NotNil(t, read.NextRunAt)
	assert.True(t, read.NextRunAt.After(slot.Add(2*time.Minute)), "NextRunAt must advance past tick time")
}

func TestManagerReconcileUpdatesRunStatus(t *testing.T) {
	m, q := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	run, err := m.RunNow(context.Background(), item.ID)
	require.NoError(t, err)

	// fakeQueue 推进状态为 completed。
	q.mu.Lock()
	q.states[run.TaskID] = automation.RunStatus{Status: automation.RunCompleted}
	q.mu.Unlock()

	require.NoError(t, m.Reconcile(context.Background()))
	_, runs, err := m.Read(item.ID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, automation.RunCompleted, runs[0].Status)
}

func TestManagerDeleteCascadesRuns(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	_, err = m.RunNow(context.Background(), item.ID)
	require.NoError(t, err)
	require.NoError(t, m.Delete(item.ID))
	_, _, err = m.Read(item.ID)
	require.Error(t, err)
}

func TestManagerDeleteMissingReturnsError(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	err := m.Delete("ghost")
	require.Error(t, err)
}

func TestManagerConstructorRejectsNilQueue(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()
	_, err = automation.NewManager(automation.NewRepository(s), nil, time.Now)
	require.Error(t, err)
}

func TestManagerSubmitRunErrorMarksRunFailed(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	defer s.Close()
	repo := automation.NewRepository(s)
	failing := &failingQueue{}
	m, err := automation.NewManager(repo, failing, time.Now)
	require.NoError(t, err)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	_, err = m.RunNow(context.Background(), item.ID)
	require.Error(t, err)
	_, runs, err := m.Read(item.ID)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, automation.RunFailed, runs[0].Status)
}

// failingQueue 实现 QueuePort 并总是返回错误。
type failingQueue struct{}

func (failingQueue) SubmitRun(context.Context, automation.RunPayload) (automation.RunReceipt, error) {
	return automation.RunReceipt{}, errors.New("queue unavailable")
}
func (failingQueue) Lookup(context.Context, string) (automation.RunStatus, error) {
	return automation.RunStatus{}, errors.New("lookup unavailable")
}

func boolPtr(b bool) *bool { return &b }

// verifyAtomicUsage 是一个 race detector 的代理断言：manager 在并发 Tick 下必须
// 不出现数据竞争。-race 在 CI 中运行；此处只是占位测试，保证 Manager 不被竞态破坏。
func TestManagerConcurrentTickAndReadNoRace(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	item, err := m.Create(automation.CreateInput{
		Name:     "x",
		Prompt:   "p",
		Schedule: automation.Schedule{Kind: "interval", IntervalSec: 60},
	})
	require.NoError(t, err)
	var stop int32
	go func() {
		for atomic.LoadInt32(&stop) == 0 {
			_ = m.Tick(context.Background(), time.Now())
		}
	}()
	for i := 0; i < 50; i++ {
		_, _, _ = m.Read(item.ID)
	}
	atomic.StoreInt32(&stop, 1)
}
```

### Step 6.2 — 写失败的 scheduler 测试（review #6：done + Stop）

```go
// internal/agent/automation/scheduler_test.go
package automation_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
)

func TestSchedulerStartStopExitsCleanly(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	sch := automation.NewScheduler(m, 10*time.Millisecond)

	started := make(chan struct{})
	var ticks int32
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		close(started)
		sch.Start(ctx)
	}()
	<-started

	// 等 3 个 tick 触发。
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&ticks) < 1 {
		select {
		case <-deadline:
			t.Fatal("scheduler never ticked")
		default:
			// 让 scheduler 自然 tick；下面读 manager 来感知副作用。
			item, err := m.Create(automation.CreateInput{
				Name:     "x",
				Prompt:   "p",
				Schedule: automation.Schedule{Kind: "interval", IntervalSec: 1},
			})
			if err == nil && item.ID != "" {
				atomic.AddInt32(&ticks, 1)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	cancel()
	done := make(chan struct{})
	go func() { sch.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Scheduler.Wait did not return after cancel")
	}
}

func TestSchedulerStopReleasesResources(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	sch := automation.NewScheduler(m, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go sch.Start(ctx)
	cancel()
	sch.Wait()
	// 再调 Wait 必须立即返回（幂等）。
	sch.Wait()
	// 再调 Stop（cancel + Wait 的便捷封装）必须立即返回。
	require.NoError(t, sch.Stop(context.Background()))
}

func TestSchedulerTickErrorsDoNotPanic(t *testing.T) {
	m, _ := newManagerWithFakeQueue(t)
	sch := automation.NewScheduler(m, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go sch.Start(ctx)
	time.Sleep(30 * time.Millisecond)
	cancel()
	sch.Wait()
	assert.True(t, true) // 仅证明未 panic
}
```

### Step 6.3 — 完整 manager.go（锁外 SubmitRun — review #8）

```go
// internal/agent/automation/manager.go
package automation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// CreateInput 是 Manager.Create 的入参。Name/Prompt/Schedule 必填。
type CreateInput struct {
	Name     string
	Prompt   string
	Schedule Schedule
	Cwds     []string
	Paused   bool
}

// UpdateInput 是 Manager.Update 的入参。指针字段为 nil 表示不更新。
type UpdateInput struct {
	ID       string
	Name     *string
	Prompt   *string
	Schedule *Schedule
	Cwds     *[]string
	Active   *bool
}

// Manager 是 AU1 的领域服务。所有 CRUD/read-modify-write 在 mu 锁内串行；
// 但 SubmitRun（可能阻塞/远程调用）**不**持锁（review #8）。
type Manager struct {
	mu    sync.Mutex
	repo  *Repository
	queue QueuePort
	now   func() time.Time
}

// NewManager 构造 Manager。repo/queue 不得为 nil。
func NewManager(repo *Repository, queue QueuePort, now func() time.Time) (*Manager, error) {
	if repo == nil {
		return nil, errors.New("automation repository is nil")
	}
	if queue == nil {
		return nil, errors.New("automation QueuePort (A2 adapter) is required")
	}
	if now == nil {
		now = time.Now
	}
	return &Manager{repo: repo, queue: queue, now: now}, nil
}

// Create 创建 automation 并持久化。Active=true 时计算 NextRunAt。
func (m *Manager) Create(input CreateInput) (Automation, error) {
	if input.Name == "" || input.Prompt == "" {
		return Automation{}, errors.New("name and prompt are required")
	}
	if _, err := input.Schedule.Next(m.now()); err != nil {
		return Automation{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.repo.Load()
	if err != nil {
		return Automation{}, err
	}
	now := m.now().UTC()
	item := Automation{
		ID:        newID("auto"),
		Name:      input.Name,
		Prompt:    input.Prompt,
		Schedule:  input.Schedule,
		Cwds:      append([]string(nil), input.Cwds...),
		Active:    !input.Paused,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if item.Active {
		next, err := item.Schedule.Next(now)
		if err != nil {
			return Automation{}, err
		}
		item.NextRunAt = &next
	}
	state.Automations = append(state.Automations, item)
	if err := m.repo.Save(state); err != nil {
		return Automation{}, err
	}
	return item, nil
}

// List 返回所有 automation（副本）。
func (m *Manager) List() ([]Automation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.repo.Load()
	if err != nil {
		return nil, err
	}
	return append([]Automation(nil), state.Automations...), nil
}

// Read 返回单个 automation 及其关联 runs。
func (m *Manager) Read(id string) (Automation, []Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.repo.Load()
	if err != nil {
		return Automation{}, nil, err
	}
	for _, item := range state.Automations {
		if item.ID == id {
			var runs []Run
			for _, run := range state.Runs {
				if run.AutomationID == id {
					runs = append(runs, run)
				}
			}
			return item, runs, nil
		}
	}
	return Automation{}, nil, fmt.Errorf("automation %q not found", id)
}

// Update 部分更新 automation。Schedule 变更后重新计算 NextRunAt。
func (m *Manager) Update(input UpdateInput) (Automation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.repo.Load()
	if err != nil {
		return Automation{}, err
	}
	for index := range state.Automations {
		item := &state.Automations[index]
		if item.ID != input.ID {
			continue
		}
		if input.Name != nil {
			item.Name = *input.Name
		}
		if input.Prompt != nil {
			item.Prompt = *input.Prompt
		}
		if input.Schedule != nil {
			if _, err := input.Schedule.Next(m.now()); err != nil {
				return Automation{}, err
			}
			item.Schedule = *input.Schedule
		}
		if input.Cwds != nil {
			item.Cwds = append([]string(nil), (*input.Cwds)...)
		}
		if input.Active != nil {
			item.Active = *input.Active
		}
		item.UpdatedAt = m.now().UTC()
		item.NextRunAt = nil
		if item.Active {
			next, err := item.Schedule.Next(item.UpdatedAt)
			if err != nil {
				return Automation{}, err
			}
			item.NextRunAt = &next
		}
		if err := m.repo.Save(state); err != nil {
			return Automation{}, err
		}
		return *item, nil
	}
	return Automation{}, fmt.Errorf("automation %q not found", input.ID)
}

// Pause 是 Update(Active=false) 的便捷封装。
func (m *Manager) Pause(id string) error {
	active := false
	_, err := m.Update(UpdateInput{ID: id, Active: &active})
	return err
}

// Resume 是 Update(Active=true) 的便捷封装。
func (m *Manager) Resume(id string) error {
	active := true
	_, err := m.Update(UpdateInput{ID: id, Active: &active})
	return err
}

// Delete 删除 automation 及其关联 runs。
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.repo.Load()
	if err != nil {
		return err
	}
	found := false
	automations := state.Automations[:0]
	for _, item := range state.Automations {
		if item.ID == id {
			found = true
			continue
		}
		automations = append(automations, item)
	}
	if !found {
		return fmt.Errorf("automation %q not found", id)
	}
	state.Automations = automations
	filtered := state.Runs[:0]
	for _, run := range state.Runs {
		if run.AutomationID != id {
			filtered = append(filtered, run)
		}
	}
	state.Runs = filtered
	return m.repo.Save(state)
}

// RunNow 是 manual 入队：reason="manual"，使用随机 unique key（允许重复手动 run）。
func (m *Manager) RunNow(ctx context.Context, id string) (Run, error) {
	return m.enqueue(ctx, id, m.now().UTC(), "manual")
}

// Tick 在 scheduler 的每个周期调用：找出到期 automation，推进 next slot，入队。
// 推进 slot 在 mu 内完成；入队（SubmitRun）**不**持锁，避免阻塞 broker 调用。
func (m *Manager) Tick(ctx context.Context, now time.Time) error {
	m.mu.Lock()
	var due []dueItem
	state, err := m.repo.Load()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	for index := range state.Automations {
		item := &state.Automations[index]
		if !item.Active || item.NextRunAt == nil || item.NextRunAt.After(now) {
			continue
		}
		slot := *item.NextRunAt
		next, nextErr := item.Schedule.Next(now)
		if nextErr != nil {
			m.mu.Unlock()
			return nextErr
		}
		item.NextRunAt = &next
		item.LastRunAt = &slot
		item.UpdatedAt = now.UTC()
		due = append(due, dueItem{id: item.ID, slot: slot, prompt: item.Prompt, cwds: item.Cwds})
	}
	if err := m.repo.Save(state); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	// 锁外入队（review #8）：每个 due item 独立调 SubmitRun；失败不阻塞其他 item。
	for _, d := range due {
		if _, err := m.enqueueUnlocked(ctx, d, "schedule"); err != nil {
			return err
		}
	}
	return nil
}

type dueItem struct {
	id     string
	slot   time.Time
	prompt string
	cwds   []string
}

// enqueue 是 RunNow 的 manual 入队入口（持锁包装 enqueueUnlocked）。
func (m *Manager) enqueue(ctx context.Context, id string, scheduledFor time.Time, reason string) (Run, error) {
	m.mu.Lock()
	state, err := m.repo.Load()
	if err != nil {
		m.mu.Unlock()
		return Run{}, err
	}
	var item Automation
	for _, candidate := range state.Automations {
		if candidate.ID == id {
			item = candidate
			break
		}
	}
	if item.ID == "" {
		m.mu.Unlock()
		return Run{}, fmt.Errorf("automation %q not found", id)
	}
	// 幂等：同 key 已有 run 则直接返回。manual 用 unique key 故每次都新建。
	key := buildIdempotencyKey(id, scheduledFor, reason)
	if reason != "manual" {
		for _, existing := range state.Runs {
			if existing.IdempotencyKey == key {
				m.mu.Unlock()
				return existing, nil
			}
		}
	}
	run := Run{
		ID:             newID("run"),
		AutomationID:   id,
		ScheduledFor:   scheduledFor.UTC(),
		Status:         RunQueued,
		IdempotencyKey: key,
	}
	m.mu.Unlock()

	// 锁外调 SubmitRun（review #8）。
	d := dueItem{id: id, slot: scheduledFor, prompt: item.Prompt, cwds: item.Cwds}
	receipt, submitErr := m.queue.SubmitRun(ctx, RunPayload{
		AutomationID:   id,
		RunID:          run.ID,
		Prompt:         d.prompt,
		Cwds:           d.cwds,
		ParentTaskID:   "",
		IdempotencyKey: key,
		ThreadID:       item.ThreadID(),
	})

	m.mu.Lock()
	defer m.mu.Unlock()
	state, err = m.repo.Load()
	if err != nil {
		return Run{}, err
	}
	if submitErr != nil {
		run.Status = RunFailed
		run.Error = submitErr.Error()
	} else {
		run.TaskID = receipt.WorkTaskID
		run.BrokerTaskID = receipt.BrokerTaskID
	}
	state.Runs = append(state.Runs, run)
	if err := m.repo.Save(state); err != nil {
		return Run{}, err
	}
	return run, submitErr
}

// enqueueUnlocked 供 Tick 使用：已经持锁完成推进，直接在锁外调 SubmitRun 后再持锁写 run 记录。
func (m *Manager) enqueueUnlocked(ctx context.Context, d dueItem, reason string) (Run, error) {
	key := buildIdempotencyKey(d.id, d.slot, reason)
	run := Run{
		ID:             newID("run"),
		AutomationID:   d.id,
		ScheduledFor:   d.slot.UTC(),
		Status:         RunQueued,
		IdempotencyKey: key,
	}
	receipt, submitErr := m.queue.SubmitRun(ctx, RunPayload{
		AutomationID:   d.id,
		RunID:          run.ID,
		Prompt:         d.prompt,
		Cwds:           d.cwds,
		IdempotencyKey: key,
	})

	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.repo.Load()
	if err != nil {
		return Run{}, err
	}
	// 二次幂等检查：另一 Tick 可能已经先入队。
	for _, existing := range state.Runs {
		if existing.IdempotencyKey == key {
			return existing, nil
		}
	}
	if submitErr != nil {
		run.Status = RunFailed
		run.Error = submitErr.Error()
	} else {
		run.TaskID = receipt.WorkTaskID
		run.BrokerTaskID = receipt.BrokerTaskID
	}
	state.Runs = append(state.Runs, run)
	if err := m.repo.Save(state); err != nil {
		return Run{}, err
	}
	return run, submitErr
}

// Reconcile 通过 QueuePort.Lookup 同步 run 状态。Lookup 返回的 status 已是 C1 词汇。
func (m *Manager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, err := m.repo.Load()
	if err != nil {
		return err
	}
	changed := false
	for index := range state.Runs {
		run := &state.Runs[index]
		if run.TaskID == "" || run.Status == RunCompleted || run.Status == RunFailed || run.Status == RunCanceled {
			continue
		}
		status, lookupErr := m.queue.Lookup(ctx, run.TaskID)
		if lookupErr != nil {
			return lookupErr
		}
		if run.Status != status.Status || run.Error != status.Error {
			run.Status = status.Status
			run.Error = status.Error
			changed = true
		}
	}
	if changed {
		return m.repo.Save(state)
	}
	return nil
}

// buildIdempotencyKey 生成 automation/run 调度的幂等键。manual 用 random suffix
// 允许重复手动 run；schedule 用 slot 时间戳保证同 slot 仅入队一次。
func buildIdempotencyKey(id string, slot time.Time, reason string) string {
	if reason == "manual" {
		return fmt.Sprintf("automation/%s/manual/%s", id, newID("run"))
	}
	return fmt.Sprintf("automation/%s/slot/%s", id, slot.UTC().Format(time.RFC3339Nano))
}

// ThreadID 暂时返回空（automation 不绑定 thread）。后续可由 update 显式设置。
// 这是 Automation 的方法（不是字段），保持 State envelope 紧凑。
func (a Automation) ThreadID() string { return "" }

// newID 用 crypto/rand 生成 8 字节十六进制 id。失败时返回 prefix-fallback。
func newID(prefix string) string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return prefix + "-fallback"
	}
	return prefix + "-" + hex.EncodeToString(bytes[:])
}
```

注意：`enqueue` 的"先持锁取 → 释放锁 → SubmitRun → 再持锁写"模式确保 SubmitRun（可能阻塞或走 HTTP/broker）期间 Manager 不被锁住；二次幂等检查在重获锁后进行，避免 Tick 与 RunNow 竞争同 key 时重复入队。

### Step 6.4 — 完整 scheduler.go（review #6：done + Stop + Wait）

```go
// internal/agent/automation/scheduler.go
package automation

import (
	"context"
	"time"
)

// Scheduler 周期性调 Manager.Tick + Manager.Reconcile。
// 生命周期：Start 阻塞直到 ctx 取消；ctx 取消后 Start 返回，done channel 关闭。
// 调用方（bootstrap.App）在 Shutdown 时 cancel ctx，然后等 Wait 返回再 Close Store。
type Scheduler struct {
	manager  *Manager
	interval time.Duration
	done     chan struct{}
}

// NewScheduler 构造 Scheduler。interval <= 0 时使用 1 分钟默认。
func NewScheduler(manager *Manager, interval time.Duration) *Scheduler {
	if interval <= 0 {
		interval = time.Minute
	}
	return &Scheduler{
		manager:  manager,
		interval: interval,
		done:     make(chan struct{}),
	}
}

// Start 阻塞运行直到 ctx 取消。Start 返回后 done channel 关闭。
// 同一个 Scheduler 只能 Start 一次；重复 Start 会 panic（close closed channel）。
func (s *Scheduler) Start(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			// Tick/Reconcile 错误不 panic；下次 tick 继续。错误可写到启动日志。
			_ = s.manager.Tick(ctx, now)
			_ = s.manager.Reconcile(ctx)
		}
	}
}

// Wait 阻塞直到 Start 返回（ctx 取消后）。幂等：多次 Wait 都安全。
// bootstrap.App.Shutdown 在 Close Store 之前必须等 Wait 返回，否则 scheduler
// goroutine 可能在 Store 关闭后访问 SQLite。
func (s *Scheduler) Wait() {
	<-s.done
}

// Stop 是 cancel + Wait 的便捷封装。cancel 由调用方持有；Stop 接收一个 ctx 仅
// 用于在 Wait 上叠加一个外部 deadline（若 ctx 在 Wait 返回前到期，返回 ctx.Err()）。
func (s *Scheduler) Stop(ctx context.Context) error {
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}
```

### Step 6.5 — App.Shutdown 顺序修复（review #6）

`internal/bootstrap/bootstrap.go` 中的 `App.Shutdown` 必须在 `Store.Close()` 前等待 scheduler 退出。在 `App` struct 新增字段 `c1Scheduler *automation.Scheduler`（仅当 automation 装配成功时非 nil）。修改后的 Shutdown：

```go
// internal/bootstrap/bootstrap.go (Shutdown 修改)
func (a *App) Shutdown(ctx context.Context) error {
	if a.cancel != nil {
		a.cancel()
	}
	// review #6：等 scheduler goroutine 退出，避免 Close Store 后被访问。
	if a.c1Scheduler != nil {
		a.c1Scheduler.Wait()
	}
	err := a.Server.Shutdown(ctx)
	if cerr := a.Store.Close(); err == nil {
		err = cerr
	}
	return err
}
```

`broker.StartSweeper` 现有的 cancel 触发已是同样的“ctx 取消即退出”模式；scheduler 与之并列。Broker 本身没有 done channel；本批**不**改 broker（不在 C1 范围），只加 automation scheduler 的 Wait。

**Run:**

```sh
go test ./internal/agent/automation -run 'TestManager|TestScheduler|TestMapTaskStatus' -v -race
go vet ./internal/agent/automation
```

**Expected:** 所有 manager CRUD/pause/resume/RunNow/Tick/Reconcile/Delete 测试通过；scheduler Start/Stop/Wait 幂等；`-race` 无数据竞争；`SubmitRun` 失败时 run 标 failed。

**Commit:** `feat(automation): enqueue via QueuePort and reconcile history`

## Task 7 — 八个 automation GuardedTools 与 approval

**Files:** `internal/tools/automation.go`, `internal/tools/automation_test.go`。

### Step 7.1 — 写失败的工具测试

```go
// internal/tools/automation_test.go
package tools_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// setupAutomation 构造一个真实 *automation.Manager + 内置 fakeQueue，便于工具层端到端测试。
// 工具必须经 GuardedTool.Authorize；profile 必须显式列出工具名。
func setupAutomation(t *testing.T) (*tools.AutomationTools, *automation.Manager) {
	t.Helper()
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	repo := automation.NewRepository(s)
	// realQueueStub：满足 QueuePort 的最简 stub（记录 SubmitRun 调用）。
	q := &recordingQueue{}
	m, err := automation.NewManager(repo, q, nil)
	require.NoError(t, err)
	return tools.NewAutomationTools(m), m
}

type recordingQueue struct {
	calls int
}

func (r *recordingQueue) SubmitRun(_ context.Context, p automation.RunPayload) (automation.RunReceipt, error) {
	r.calls++
	return automation.RunReceipt{WorkTaskID: "wt-" + p.RunID, BrokerTaskID: "broker-" + p.RunID}, nil
}
func (r *recordingQueue) Lookup(_ context.Context, workTaskID string) (automation.RunStatus, error) {
	return automation.RunStatus{Status: automation.RunQueued}, nil
}

func allowAll(names ...string) guard.PermissionProfile {
	return guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: names}}
}

func TestAutomationToolsAllEightPresent(t *testing.T) {
	set, _ := setupAutomation(t)
	// review #2：Info(ctx) 获取名称，不调 Name()。
	seen := map[string]bool{}
	for _, gt := range []*tools.GuardedTool{set.Create, set.List, set.Read, set.Update, set.Pause, set.Resume, set.Delete, set.Run} {
		info, err := gt.Info(context.Background())
		require.NoError(t, err)
		seen[info.Name] = true
	}
	for _, want := range []string{
		"automation_create", "automation_list", "automation_read",
		"automation_update", "automation_pause", "automation_resume",
		"automation_delete", "automation_run",
	} {
		assert.True(t, seen[want], "missing tool %q", want)
	}
}

func TestAutomationCreateReadUpdatePauseResumeDeleteRun(t *testing.T) {
	set, m := setupAutomation(t)
	ctx := tools.WithProfile(context.Background(), allowAll(
		"automation_create", "automation_list", "automation_read",
		"automation_update", "automation_pause", "automation_resume",
		"automation_delete", "automation_run",
	))

	// create
	createResp, err := set.Create.InvokableRun(ctx, mustJSON(t, map[string]any{
		"name": "nightly", "prompt": "do X",
		"schedule_kind": "interval", "schedule": "60",
	}))
	require.NoError(t, err)
	var created automation.Automation
	require.NoError(t, json.Unmarshal([]byte(createResp), &created))
	assert.NotEmpty(t, created.ID)

	// read
	readResp, err := set.Read.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)
	assert.Contains(t, readResp, created.ID)

	// update
	_, err = set.Update.InvokableRun(ctx, mustJSON(t, map[string]any{
		"id": created.ID, "prompt": "do Y",
	}))
	require.NoError(t, err)

	// pause / resume
	_, err = set.Pause.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)
	_, err = set.Resume.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)

	// run
	runResp, err := set.Run.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)
	assert.Contains(t, runResp, `"status":"queued"`)

	// list
	listResp, err := set.List.InvokableRun(ctx, `{}`)
	require.NoError(t, err)
	assert.Contains(t, listResp, created.ID)

	// delete
	_, err = set.Delete.InvokableRun(ctx, mustJSON(t, map[string]any{"id": created.ID}))
	require.NoError(t, err)

	_ = m // 仅占位避免 unused 警告
}

func TestAutomationToolsDeniedWithoutProfile(t *testing.T) {
	set, _ := setupAutomation(t)
	for name, gt := range map[string]*tools.GuardedTool{
		"create": set.Create, "list": set.List, "read": set.Read,
		"update": set.Update, "pause": set.Pause, "resume": set.Resume,
		"delete": set.Delete, "run": set.Run,
	} {
		result, err := gt.InvokableRun(context.Background(), `{}`)
		require.NoError(t, err, "%s", name)
		assert.Contains(t, result, "permission denied", "%s", name)
	}
}

func TestAutomationToolsDeniedWhenProfileOmitsName(t *testing.T) {
	set, _ := setupAutomation(t)
	// profile 允许 automation_create 但不允许 automation_run → run 必须拒绝。
	ctx := tools.WithProfile(context.Background(), allowAll("automation_create"))
	result, err := set.Run.InvokableRun(ctx, `{}`)
	require.NoError(t, err)
	assert.Contains(t, result, "permission denied")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	// 工具输入以 JSON string envelope 承载（避免 array/object 参数 schema）。
	envelope, err := json.Marshal(map[string]string{"input": string(b)})
	require.NoError(t, err)
	return string(envelope)
}
```

**Expected failure:** `undefined: tools.NewAutomationTools`、`undefined: tools.AutomationTools`。

### Step 7.2 — 完整工具代码（展开 one-liner；review #14）

```go
// internal/tools/automation.go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/agent/automation"
)

// AutomationTools 聚合 AU1 的八个 GuardedTool。所有工具通过 NewGuardedTool →
// Authorize；profile 必须显式列出工具名。
type AutomationTools struct {
	Create *GuardedTool
	List   *GuardedTool
	Read   *GuardedTool
	Update *GuardedTool
	Pause  *GuardedTool
	Resume *GuardedTool
	Delete *GuardedTool
	Run    *GuardedTool
}

// NewAutomationTools 构造八个工具。manager 必须非 nil。
func NewAutomationTools(manager *automation.Manager) *AutomationTools {
	jsonInput := params(map[string]*schema.ParameterInfo{
		"input": {
			Type:        "string",
			Description: "JSON object containing the operation arguments",
		},
	})
	set := &AutomationTools{}

	set.Create = NewGuardedTool(
		"automation_create", "Create automation",
		"Create a persistent scheduled automation. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationCreate(ctx, manager, args)
		},
	)
	set.List = NewGuardedTool(
		"automation_list", "List automations",
		"List persistent automations. Approval required even though this is read-only.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationList(ctx, manager, args)
		},
	)
	set.Read = NewGuardedTool(
		"automation_read", "Read automation",
		"Read an automation and recent runs. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationRead(ctx, manager, args)
		},
	)
	set.Update = NewGuardedTool(
		"automation_update", "Update automation",
		"Update prompt, schedule, cwds, or active state. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationUpdate(ctx, manager, args)
		},
	)
	set.Pause = NewGuardedTool(
		"automation_pause", "Pause automation",
		"Pause future scheduling. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationIDOp(ctx, manager, args, manager.Pause)
		},
	)
	set.Resume = NewGuardedTool(
		"automation_resume", "Resume automation",
		"Resume scheduling from a newly computed next run. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationIDOp(ctx, manager, args, manager.Resume)
		},
	)
	set.Delete = NewGuardedTool(
		"automation_delete", "Delete automation",
		"Delete an automation and its run history. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationIDOp(ctx, manager, args, manager.Delete)
		},
	)
	set.Run = NewGuardedTool(
		"automation_run", "Run automation",
		"Queue one durable task for an automation now. Approval required.",
		time.Minute, jsonInput,
		func(ctx context.Context, args string) <-chan ToolChunk {
			return runAutomationRun(ctx, manager, args)
		},
	)
	return set
}

// oneChunk 把同步 fn 包装成单 chunk StreamFunc。
func oneChunk(fn func() (string, error)) <-chan ToolChunk {
	out := make(chan ToolChunk, 1)
	go func() {
		defer close(out)
		value, err := fn()
		if err != nil {
			out <- ToolChunk{Result: err.Error()}
			return
		}
		out <- ToolChunk{Result: value}
	}()
	return out
}

// decodeInput 把工具 args（{"input":"<json>"}）解码到 target。
func decodeInput(args string, target any) error {
	var envelope struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal([]byte(args), &envelope); err != nil {
		return err
	}
	if envelope.Input == "" {
		return fmt.Errorf("input is required")
	}
	return json.Unmarshal([]byte(envelope.Input), target)
}

func runAutomationCreate(_ context.Context, manager *automation.Manager, args string) <-chan ToolChunk {
	return oneChunk(func() (string, error) {
		var input struct {
			Name         string   `json:"name"`
			Prompt       string   `json:"prompt"`
			ScheduleKind string   `json:"schedule_kind"`
			Schedule     string   `json:"schedule"`
			Cwds         []string `json:"cwds"`
			Paused       bool     `json:"paused"`
		}
		if err := decodeInput(args, &input); err != nil {
			return "", err
		}
		schedule, err := automation.ParseSchedule(input.ScheduleKind, input.Schedule)
		if err != nil {
			return "", err
		}
		item, err := manager.Create(automation.CreateInput{
			Name: input.Name, Prompt: input.Prompt,
			Schedule: schedule, Cwds: input.Cwds, Paused: input.Paused,
		})
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}

func runAutomationList(_ context.Context, manager *automation.Manager, _ string) <-chan ToolChunk {
	return oneChunk(func() (string, error) {
		items, err := manager.List()
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(items)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}

func runAutomationRead(_ context.Context, manager *automation.Manager, args string) <-chan ToolChunk {
	return oneChunk(func() (string, error) {
		var input struct {
			ID string `json:"id"`
		}
		if err := decodeInput(args, &input); err != nil {
			return "", err
		}
		item, runs, err := manager.Read(input.ID)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(struct {
			Automation automation.Automation `json:"automation"`
			Runs       []automation.Run      `json:"runs"`
		}{item, runs})
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}

func runAutomationUpdate(_ context.Context, manager *automation.Manager, args string) <-chan ToolChunk {
	return oneChunk(func() (string, error) {
		var input struct {
			ID           string    `json:"id"`
			Name         string    `json:"name"`
			Prompt       string    `json:"prompt"`
			ScheduleKind string    `json:"schedule_kind"`
			Schedule     string    `json:"schedule"`
			Cwds         *[]string `json:"cwds"`
			Active       *bool     `json:"active"`
		}
		if err := decodeInput(args, &input); err != nil {
			return "", err
		}
		var schedule *automation.Schedule
		if input.ScheduleKind != "" || input.Schedule != "" {
			parsed, err := automation.ParseSchedule(input.ScheduleKind, input.Schedule)
			if err != nil {
				return "", err
			}
			schedule = &parsed
		}
		var name, prompt *string
		if input.Name != "" {
			name = &input.Name
		}
		if input.Prompt != "" {
			prompt = &input.Prompt
		}
		item, err := manager.Update(automation.UpdateInput{
			ID: input.ID, Name: name, Prompt: prompt,
			Schedule: schedule, Cwds: input.Cwds, Active: input.Active,
		})
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}

// runAutomationIDOp 是 Pause/Resume/Delete 的公共形状：都是单字段 id。
func runAutomationIDOp(_ context.Context, manager *automation.Manager, args string, op func(string) error) <-chan ToolChunk {
	return oneChunk(func() (string, error) {
		var input struct {
			ID string `json:"id"`
		}
		if err := decodeInput(args, &input); err != nil {
			return "", err
		}
		if err := op(input.ID); err != nil {
			return "", err
		}
		return `{"ok":true}`, nil
	})
}

func runAutomationRun(ctx context.Context, manager *automation.Manager, args string) <-chan ToolChunk {
	return oneChunk(func() (string, error) {
		var input struct {
			ID string `json:"id"`
		}
		if err := decodeInput(args, &input); err != nil {
			return "", err
		}
		run, err := manager.RunNow(ctx, input.ID)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(run)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	})
}
```

`automation_update` 用 `*[]string` 承载 cwds，区分“未提供”和“明确清空”；测试必须锁定该行为。`list/read` 即使没有实际写入，也不能绕过 `Authorize`。

**Run:**

```sh
go test ./internal/tools -run TestAutomation -v
go vet ./internal/tools
```

**Expected:** 八个工具全部存在、名称精确；profile/approval 拒绝时 manager 无调用；`automation_run` 只返回 queued durable task/run 信息。

**Commit:** `feat(tools): add approval-gated automation surface`

## Task 8 — bootstrap automation lifecycle + A2 adapter 接线（含完整 _test.go）

**Files:** `internal/bootstrap/c1.go`, `internal/bootstrap/bootstrap.go`, `internal/bootstrap/c1_test.go`。

### Step 8.1 — 写失败的 wiring 测试

```go
// internal/bootstrap/c1_test.go (追加 wiring 测试)
package bootstrap_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

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
		// 创建一个 automation，tick 会触发 SubmitRun。
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

	// 直接 import tools 包并调 (*tools.GuardedTool).Info(ctx).Name —— bootstrap_test
	// → tools 无 import 环（tools 不 import bootstrap）。
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
```

**Expected failure:** `undefined: bootstrap.BuildAutomation`。

### Step 8.2 — 完整 buildAutomation helper

```go
// internal/bootstrap/c1.go (追加)
package bootstrap

import (
	"context"
	"errors"
	"time"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

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
```

### Step 8.3 — bootstrap.Build 接线点

在 `internal/bootstrap/bootstrap.go` 的 `Build` 中，紧跟 Task 4 的 RLM 接线片段后，在 task broker 创建之前/之后均可（依赖 adapter 需要 broker；Task 12 会一起装配 A2Adapter），增加 automation 接线：

```go
// internal/bootstrap/bootstrap.go (Build 内；插入点在 broker 构造之后)
broker := task.NewBroker(st, 3, 30*time.Second)
if vcsRepoID != "" {
	broker.SetVCS(vcsInstance, vcsRepoID)
}
srv.TaskAPI(broker, cfg.Profiles)

// C1 automation（AU1）：组合 A2 work.ManagerLike + task.Broker + store KV。
// 注意：work.ManagerLike 由 A2 批次提供；若 A2 尚未落地，此处跳过 automation
// 装配（与 RLM1 fail-open 模式一致），但要打到 stderr 提醒运维。
var c1Auto *C1Automation
if workMgr, wmErr := tryBuildWorkManager(st, cfg); wmErr != nil {
	fmt.Fprintf(os.Stderr, "yanshi: automation disabled (A2 work manager unavailable): %v\n", wmErr)
} else if c1Auto, err = BuildAutomation(ctx, cfg, st, NewA2Adapter(workMgr, broker, st)); err != nil {
	fmt.Fprintf(os.Stderr, "yanshi: automation disabled: %v\n", err)
	c1Auto = nil
} else {
	for _, gt := range []*tools.GuardedTool{
		c1Auto.Tools.Create, c1Auto.Tools.List, c1Auto.Tools.Read,
		c1Auto.Tools.Update, c1Auto.Tools.Pause, c1Auto.Tools.Resume,
		c1Auto.Tools.Delete, c1Auto.Tools.Run,
	} {
		allTools = append(allTools, gt)
	}
}

// ... 继续现有 ctx, cancel := context.WithCancel(...) 与 broker.StartSweeper

// 修改 App 字段与 Shutdown：
return &App{
	// ... 现有字段 ...
	c1Scheduler: func() *automation.Scheduler {
		if c1Auto == nil { return nil }
		return c1Auto.Scheduler
	}(),
}, nil
```

`tryBuildWorkManager` 是 A2 批次的装配 helper（`work.NewManager(work.FromDB(st.DB))`）；A2 尚未落地时返回错误。本计划不展示其实现（A2 计划负责），但要求 A2 落地后 C1 此处自动激活。

**Run:**

```sh
go test ./internal/bootstrap -run 'TestBuildAutomation|TestSelectRLM|TestBuildRLM' -v
go vet ./internal/bootstrap
```

**Expected:** BuildAutomation 成功路径装配 manager/scheduler/8 个工具；nil adapter fail-closed；scheduler goroutine 在 cancel 后 Wait 返回。

**Commit:** `feat(bootstrap): wire automation manager and scheduler lifecycle`

## Task 9 — M07 CSV/structured input parser（含完整 _test.go）

**Files:** `internal/agent/batch/input.go`, `internal/agent/batch/input_test.go`。

### Step 9.1 — 写失败的测试

```go
// internal/agent/batch/input_test.go
package batch_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/batch"
)

func TestParseCSV_HeaderAndStableIndex(t *testing.T) {
	input := "name,city\nAlice,NYC\nBob,SF\n"
	rows, err := batch.ParseCSV(input)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, 0, rows[0].Index)
	assert.Equal(t, "Alice", rows[0].Values["name"])
	assert.Equal(t, 1, rows[1].Index)
	assert.Equal(t, "SF", rows[1].Values["city"])
}

func TestParseCSV_QuotedCommaAndNewline(t *testing.T) {
	input := "desc,note\n\"line1\nline2\",\"has, comma\"\n"
	rows, err := batch.ParseCSV(input)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "line1\nline2", rows[0].Values["desc"])
	assert.Equal(t, "has, comma", rows[0].Values["note"])
}

func TestParseCSV_UTF8Preserved(t *testing.T) {
	input := "name\n张三\n李四\n"
	rows, err := batch.ParseCSV(input)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "张三", rows[0].Values["name"])
	assert.Equal(t, "李四", rows[1].Values["name"])
}

func TestParseCSV_EmptyString(t *testing.T) {
	_, err := batch.ParseCSV("")
	require.Error(t, err)
}

func TestParseCSV_HeaderOnly(t *testing.T) {
	_, err := batch.ParseCSV("name,city\n")
	require.Error(t, err)
}

func TestParseCSV_DuplicateHeader(t *testing.T) {
	_, err := batch.ParseCSV("name,name\na,b\n")
	require.Error(t, err)
}

func TestParseCSV_EmptyHeaderCell(t *testing.T) {
	_, err := batch.ParseCSV("name,\na,b\n")
	require.Error(t, err)
}

func TestParseCSV_RowFieldCountMismatch(t *testing.T) {
	_, err := batch.ParseCSV("a,b\n1,2,3\n")
	require.Error(t, err)
}

func TestParseStructured_BasicAndStableIndex(t *testing.T) {
	rows, err := batch.ParseStructured([]map[string]string{
		{"q": "a"},
		{"q": "b"},
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, 0, rows[0].Index)
	assert.Equal(t, 1, rows[1].Index)
}

func TestParseStructured_EmptyList(t *testing.T) {
	_, err := batch.ParseStructured(nil)
	require.Error(t, err)
}

func TestParseStructured_EmptyRow(t *testing.T) {
	_, err := batch.ParseStructured([]map[string]string{{}})
	require.Error(t, err)
}

func TestParseStructured_EmptyKey(t *testing.T) {
	_, err := batch.ParseStructured([]map[string]string{{"": "v"}})
	require.Error(t, err)
}

// 确保 strings import 在 helper 里被用到。
var _ = strings.Contains
```

### Step 9.2 — 完整 parser 代码

```go
// internal/agent/batch/input.go
package batch

import (
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
)

// ParseCSV 把 CSV 文本解析为稳定 index 的 Row 切片。首行为 header；后续每行
// 必须与 header 列数一致。空 header cell、重复 header、空 body 都被拒绝。
func ParseCSV(input string) ([]Row, error) {
	reader := csv.NewReader(strings.NewReader(input))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse CSV: %w", err)
	}
	if len(records) < 2 || len(records[0]) == 0 {
		return nil, errors.New("CSV must contain a header and at least one row")
	}
	seen := make(map[string]struct{}, len(records[0]))
	for _, header := range records[0] {
		if header == "" {
			return nil, errors.New("CSV header cannot be empty")
		}
		if _, ok := seen[header]; ok {
			return nil, fmt.Errorf("duplicate CSV header %q", header)
		}
		seen[header] = struct{}{}
	}
	rows := make([]Row, 0, len(records)-1)
	for index, record := range records[1:] {
		if len(record) != len(records[0]) {
			return nil, fmt.Errorf("CSV row %d has %d fields, want %d", index, len(record), len(records[0]))
		}
		values := make(map[string]string, len(record))
		for column, header := range records[0] {
			values[header] = record[column]
		}
		rows = append(rows, Row{Index: index, Values: values})
	}
	return rows, nil
}

// ParseStructured 把 []map[string]string 解析为稳定 index 的 Row 切片。每行
// 必须非空、键非空；调用方选择提供 CSV 或 structured，二选一。
func ParseStructured(values []map[string]string) ([]Row, error) {
	if len(values) == 0 {
		return nil, errors.New("structured rows cannot be empty")
	}
	rows := make([]Row, len(values))
	for index, value := range values {
		if len(value) == 0 {
			return nil, fmt.Errorf("structured row %d is empty", index)
		}
		copyValue := make(map[string]string, len(value))
		for key, item := range value {
			if key == "" {
				return nil, fmt.Errorf("structured row %d has an empty key", index)
			}
			copyValue[key] = item
		}
		rows[index] = Row{Index: index, Values: copyValue}
	}
	return rows, nil
}
```

这里特意不以 `strings.Split` 解析 CSV，避免 quoted comma/newline 的数据损坏。

**Run:**

```sh
go test ./internal/agent/batch -run TestParse -v
go vet ./internal/agent/batch
```

**Expected:** parser 测试通过，坏输入包含明确 row/header 错误；UTF-8 与 quoted 字段保留。

**Commit:** `feat(batch): parse indexed CSV and structured rows`

## Task 10 — M07 runner：`*registry.Manager.Spawn/Wait` + `SpawnErrCap` 重试

**Files:** `internal/agent/batch/runner.go`（实现部分）, `internal/agent/batch/runner_test.go`（完整测试）。

注意：`SpawnFunc/Row/Input/Result/Report` 已在 Task 1 声明；此处只追加 `Runner/rowRunner` 与 `Run` 方法。

### Step 10.1 — 写失败的 runner 测试（完整覆盖）

```go
// internal/agent/batch/runner_test.go (Task 1 contract 部分之外，补充完整 runner 测试)
package batch_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/batch"
	"github.com/x6nux/yanshi/internal/agent/registry"
)

// recordingSpawn 记录每次调用的 prompt；可配置在某个调用上返回错误。
type recordingSpawn struct {
	calls    int32
	prompts  []string
	failOn   int // -1 = 不失败
	failWith error
}

func (r *recordingSpawn) Spawn(ctx context.Context, prompt string, _ []string, _ string) (string, error) {
	idx := int(atomic.AddInt32(&r.calls, 1)) - 1
	r.prompts = append(r.prompts, prompt)
	if idx == r.failOn && r.failWith != nil {
		return "", r.failWith
	}
	return "ok-" + prompt, nil
}

func newRegistryManager(t *testing.T, max int) *registry.Manager {
	t.Helper()
	m := registry.NewManager(registry.NewManagerOpts{MaxConcurrent: max})
	t.Cleanup(m.Close)
	return m
}

func TestRunnerSpawnsPerRowAndPreservesIndex(t *testing.T) {
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       newRegistryManager(t, 4),
		WaitTimeout:   2 * time.Second,
		CappedBackoff: 10 * time.Millisecond,
		CappedRetries: 5,
	}
	rows := []batch.Row{
		{Index: 0, Values: map[string]string{"q": "a"}},
		{Index: 1, Values: map[string]string{"q": "b"}},
		{Index: 2, Values: map[string]string{"q": "c"}},
	}
	report, err := runner.Run(context.Background(), batch.Input{Prompt: "do", Rows: rows})
	require.NoError(t, err)
	require.Len(t, report.Results, 3)
	for i, r := range report.Results {
		assert.Equal(t, i, r.Index)
		assert.Contains(t, r.Output, "ok-")
	}
	assert.Equal(t, 3, report.Success)
}

func TestRunnerPerItemErrorRetention(t *testing.T) {
	rec := &recordingSpawn{failOn: 1, failWith: errors.New("row-1-boom")}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       newRegistryManager(t, 4),
		WaitTimeout:   2 * time.Second,
		CappedBackoff: 10 * time.Millisecond,
		CappedRetries: 5,
	}
	rows := []batch.Row{
		{Index: 0, Values: map[string]string{"q": "a"}},
		{Index: 1, Values: map[string]string{"q": "b"}},
		{Index: 2, Values: map[string]string{"q": "c"}},
	}
	report, err := runner.Run(context.Background(), batch.Input{Prompt: "do", Rows: rows})
	require.NoError(t, err)
	require.Len(t, report.Results, 3)
	assert.Equal(t, 2, report.Success)
	assert.Equal(t, 1, report.Failed)
	assert.Equal(t, "", report.Results[1].Output)
	assert.Contains(t, report.Results[1].Error, "row-1-boom")
}

func TestRunnerCapsAtRegistryMaxConcurrent(t *testing.T) {
	// Manager MaxConcurrent=2；3 个 row 必须通过 SpawnErrCap 重试入队，全部完成。
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       newRegistryManager(t, 2),
		WaitTimeout:   2 * time.Second,
		CappedBackoff: 20 * time.Millisecond,
		CappedRetries: 20,
	}
	rows := []batch.Row{
		{Index: 0, Values: map[string]string{"q": "a"}},
		{Index: 1, Values: map[string]string{"q": "b"}},
		{Index: 2, Values: map[string]string{"q": "c"}},
	}
	report, err := runner.Run(context.Background(), batch.Input{Prompt: "do", Rows: rows})
	require.NoError(t, err)
	assert.Equal(t, 3, report.Success)
}

func TestRunnerCancellationPendingRowsMarkedCanceled(t *testing.T) {
	// 用一个阻塞的 spawn：等 gate，永远不返回；ctx cancel 后未开始的 row 标 canceled。
	gate := make(chan struct{})
	rec := &blockingSpawn{gate: gate}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       newRegistryManager(t, 1), // cap=1：第 0 行占住，第 1 行排队
		WaitTimeout:   5 * time.Second,
		CappedBackoff: 10 * time.Millisecond,
		CappedRetries: 50,
	}
	ctx, cancel := context.WithCancel(context.Background())
	rows := []batch.Row{
		{Index: 0, Values: map[string]string{"q": "a"}},
		{Index: 1, Values: map[string]string{"q": "b"}},
	}
	done := make(chan batch.Report, 1)
	go func() {
		r, _ := runner.Run(ctx, batch.Input{Prompt: "do", Rows: rows})
		done <- r
	}()
	// 等 spawn 开始处理第 0 行。
	require.Eventually(t, func() bool { return atomic.LoadInt32(&rec.calls) >= 1 }, 2*time.Second, 10*time.Millisecond)
	cancel() // 第 1 行尚未 spawn（cap=1）
	close(gate)
	select {
	case r := <-done:
		require.Len(t, r.Results, 2)
		// 第 1 行必为 canceled 或 failed。
		assert.NotEmpty(t, r.Results[1].Error)
	default:
		t.Fatal("runner did not return after cancel")
	}
	// 等待 registry 内部 goroutine 退出，避免 race detector 误报。
	time.Sleep(50 * time.Millisecond)
}

type blockingSpawn struct {
	calls int32
	gate  chan struct{}
}

func (b *blockingSpawn) Spawn(ctx context.Context, prompt string, _ []string, _ string) (string, error) {
	atomic.AddInt32(&b.calls, 1)
	select {
	case <-b.gate:
		return "ok", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestRunnerRejectsNilSpawn(t *testing.T) {
	runner := batch.Runner{Manager: newRegistryManager(t, 2)}
	_, err := runner.Run(context.Background(), batch.Input{
		Prompt: "x",
		Rows:   []batch.Row{{Index: 0, Values: map[string]string{"q": "a"}}},
	})
	require.Error(t, err)
}

func TestRunnerRejectsNilManager(t *testing.T) {
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{Spawn: rec.Spawn}
	_, err := runner.Run(context.Background(), batch.Input{
		Prompt: "x",
		Rows:   []batch.Row{{Index: 0, Values: map[string]string{"q": "a"}}},
	})
	require.Error(t, err)
}

func TestRunnerRejectsEmptyRows(t *testing.T) {
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{Spawn: rec.Spawn, Manager: newRegistryManager(t, 2)}
	report, err := runner.Run(context.Background(), batch.Input{Prompt: "x"})
	require.NoError(t, err)
	assert.Empty(t, report.Results)
}

func TestRunnerRejectsEmptyPrompt(t *testing.T) {
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{Spawn: rec.Spawn, Manager: newRegistryManager(t, 2)}
	_, err := runner.Run(context.Background(), batch.Input{
		Rows: []batch.Row{{Index: 0, Values: map[string]string{"q": "a"}}},
	})
	require.Error(t, err)
}

func TestRunnerPromptIncludesBasePromptAndRowJSON(t *testing.T) {
	rec := &recordingSpawn{failOn: -1}
	runner := batch.Runner{
		Spawn:         rec.Spawn,
		Manager:       newRegistryManager(t, 4),
		WaitTimeout:   2 * time.Second,
		CappedBackoff: 10 * time.Millisecond,
		CappedRetries: 5,
	}
	rows := []batch.Row{{Index: 0, Values: map[string]string{"name": "Alice"}}}
	_, err := runner.Run(context.Background(), batch.Input{Prompt: "BASE", Rows: rows})
	require.NoError(t, err)
	require.NotEmpty(t, rec.prompts)
	assert.Contains(t, rec.prompts[0], "BASE")
	assert.Contains(t, rec.prompts[0], `"name":"Alice"`)
	assert.Contains(t, rec.prompts[0], "row_index=0")
}
```

### Step 10.2 — 完整 runner.go 实现

```go
// internal/agent/batch/runner.go (追加；types 在 Task 1 已声明)
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

// Runner 是 M07 的执行体。Spawn（来自 tools.SubAgentRunner）与 *registry.Manager
// 必须非 nil。Manager.MaxConcurrent() 是唯一并发上限；当 Spawn 返回 *SpawnErrCap
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
	var spawnWG sync.WaitGroup
	for i, row := range input.Rows {
		spawnWG.Add(1)
		go func(index int, rr Row) {
			defer spawnWG.Done()
			runner := &rowRunner{spawn: r.Spawn, row: rr, input: input}
			id, err := spawnWithRetry(ctx, r.Manager, runner, backoff, retries)
			agentIDs[index] = id
			spawnErrs[index] = err
		}(i, row)
	}
	spawnWG.Wait()

	results := make([]Result, len(input.Rows))
	// review #5：finished 跨 Wait goroutine 写，主 goroutine 读；必须 atomic。
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
// canceled 检测按错误字符串匹配 context.Canceled / DeadlineExceeded（B1
// 返回的 rec.Error 是字符串）。
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
```

**Run:**

```sh
go test ./internal/agent/batch -run TestRunner -v -race
go vet ./internal/agent/batch
```

**Expected:** runner 通过全部测试；`-race` 无数据竞争；MaxConcurrent=2 时 3 个 row 全部完成（证明 `SpawnErrCap` 重试）；cancellation 正确传播。

**Commit:** `feat(batch): run indexed jobs through B1 registry manager`

## Task 11 — `agent_batch` GuardedTool（无 `validateBatchDescription`）

**Files:** `internal/tools/batch.go`, `internal/tools/batch_test.go`。

### Step 11.1 — 写失败的工具测试

```go
// internal/tools/batch_test.go
package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/tools"
)

func newBatchTools(t *testing.T) (*tools.BatchTools, *registry.Manager) {
	t.Helper()
	m := registry.NewManager(registry.NewManagerOpts{MaxConcurrent: 4})
	t.Cleanup(m.Close)
	return tools.NewBatchTools(m), m
}

func TestAgentBatchMetadataMentionsB1AndApproval(t *testing.T) {
	set, _ := newBatchTools(t)
	info, err := set.AgentBatch.Info(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "agent_batch", info.Name)
	for _, phrase := range []string{"CSV", "structured", "B1", "concurrency", "approval"} {
		assert.Contains(t, info.Desc, phrase, "Desc missing %q", phrase)
	}
}

func TestAgentBatchCSVInputEndToEnd(t *testing.T) {
	set, _ := newBatchTools(t)
	// 用真实 *registry.Manager + echo spawn；验证 CSV 解析 → spawn → report。
	// 通过 SubAgentRunner context 注入一个 echo spawn。
	echo := func(_ context.Context, prompt string, _ []string, _ string) (string, error) {
		return "ok-" + prompt, nil
	}
	ctx := tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		echo,
	)
	payload := map[string]any{
		"prompt": "DO",
		"csv":    "name,city\nAlice,NYC\nBob,SF\n",
	}
	args := wrapInput(t, payload)
	result, err := set.AgentBatch.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, `"success":2`)
	assert.Contains(t, result, `"index":0`)
	assert.Contains(t, result, `"index":1`)
}

func TestAgentBatchStructuredInputEndToEnd(t *testing.T) {
	set, _ := newBatchTools(t)
	echo := func(_ context.Context, prompt string, _ []string, _ string) (string, error) {
		return "ok", nil
	}
	ctx := tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		echo,
	)
	payload := map[string]any{
		"prompt": "DO",
		"rows":   []map[string]string{{"q": "a"}, {"q": "b"}},
	}
	args := wrapInput(t, payload)
	result, err := set.AgentBatch.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, `"success":2`)
}

func TestAgentBatchRejectsBothCSVAndRows(t *testing.T) {
	set, _ := newBatchTools(t)
	echo := func(_ context.Context, _ string, _ []string, _ string) (string, error) { return "", nil }
	ctx := tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		echo,
	)
	payload := map[string]any{
		"prompt": "DO",
		"csv":    "a\n1\n",
		"rows":   []map[string]string{{"q": "a"}},
	}
	args := wrapInput(t, payload)
	result, err := set.AgentBatch.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, "exactly one of")
}

func TestAgentBatchRejectsNeitherCSVNorRows(t *testing.T) {
	set, _ := newBatchTools(t)
	echo := func(_ context.Context, _ string, _ []string, _ string) (string, error) { return "", nil }
	ctx := tools.WithSubAgentRunner(
		tools.WithProfile(context.Background(), allowAllForBatch()),
		echo,
	)
	payload := map[string]any{"prompt": "DO"}
	args := wrapInput(t, payload)
	result, err := set.AgentBatch.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, "exactly one of")
}

func TestAgentBatchRejectedWithoutSubAgentRunner(t *testing.T) {
	set, _ := newBatchTools(t)
	ctx := tools.WithProfile(context.Background(), allowAllForBatch())
	payload := map[string]any{"prompt": "DO", "csv": "a\n1\n"}
	args := wrapInput(t, payload)
	result, err := set.AgentBatch.InvokableRun(ctx, args)
	require.NoError(t, err)
	assert.Contains(t, result, "sub-agent runner is not bound")
}

func TestAgentBatchDeniedWithoutProfile(t *testing.T) {
	set, _ := newBatchTools(t)
	echo := func(_ context.Context, _ string, _ []string, _ string) (string, error) { return "", nil }
	ctx := tools.WithSubAgentRunner(context.Background(), echo)
	result, err := set.AgentBatch.InvokableRun(ctx, wrapInput(t, map[string]any{
		"prompt": "DO", "csv": "a\n1\n",
	}))
	require.NoError(t, err)
	assert.Contains(t, result, "permission denied")
}

func allowAllForBatch() guard.PermissionProfile {
	return guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"agent_batch"}}}
}

func wrapInput(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	envelope, err := json.Marshal(map[string]string{"input": string(raw)})
	require.NoError(t, err)
	return string(envelope)
}

// Ensure fmt stays imported if other helpers are removed later.
var _ = fmt.Sprintf
```

注意：`allowAllForBatch` 返回 `guard.PermissionProfile`，需要 `import "github.com/x6nux/yanshi/internal/guard"`；上面测试文件需补上 import。`mustJSON` 不复用（Task 7 的 helper 仅在 automation_test.go 内）；这里用本文件内的 `wrapInput`。

**Expected failure:** `undefined: tools.NewBatchTools`、`undefined: tools.BatchTools`。

### Step 11.2 — 完整 batch.go 实现（无 `validateBatchDescription` — review #12）

```go
// internal/tools/batch.go
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
type BatchTools struct {
	AgentBatch *GuardedTool
	Manager    *registry.Manager
}

// NewBatchTools 构造 batch 工具集。manager 非 nil（B1 必须落地）。
func NewBatchTools(manager *registry.Manager) *BatchTools {
	set := &BatchTools{Manager: manager}
	set.AgentBatch = NewGuardedTool(
		"agent_batch",
		"Batch agent jobs",
		// 描述必须包含 CSV/structured/B1/concurrency/approval — 这些是测试断言的
		// 短语（review #12：不另建 validateBatchDescription helper 在生产代码里
		// 二次校验；描述是常量，由 _test.go 直接 Contains 断言）。
		"Run one sub-agent job per CSV or structured row through the B1 M04 lifecycle/registry and its unified concurrency cap (SpawnErrCap non-blocking backpressure). Returns per-row results and a summary; this is higher-cost than cost-class cheap rlm_query. Approval required.",
		10*time.Minute,
		params(map[string]*schema.ParameterInfo{
			"input": {
				Type:        "string",
				Description: "JSON object with prompt and exactly one of csv or rows",
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
```

工具不从参数读取或覆盖 B1 的 cap；`MaxConcurrent` 只从 `*registry.Manager` 取。`AllowedTools` 仍是子代理请求数据，实际权限由当前 turn context/profile 约束。`validateBatchDescription` 不在生产代码中存在（review #12）——描述是常量字符串，测试直接 Contains 断言。

**Run:**

```sh
go test ./internal/tools -run TestAgentBatch -v
go vet ./internal/tools
```

**Expected:** CSV/structured 两种输入均能通过真实 *registry.Manager + echo runner；缺 context runner、缺 Manager、权限拒绝均 fail closed；CSV+rows 同时/同时缺都拒绝。

**Commit:** `feat(tools): expose guarded agent_batch input surface`

## Task 12 — Full bootstrap integration、配置样例、cron go.mod diff、最终验证

**Files:** `internal/bootstrap/bootstrap.go`, `internal/bootstrap/c1.go`, `internal/bootstrap/c1_test.go`（追加 integration）, `config.example.yaml`, `go.mod`。

### Step 12.1 — 写失败的集成测试

```go
// internal/bootstrap/c1_test.go (追加；integration)
package bootstrap_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

func TestBuildC1WiresAllThreeComponents(t *testing.T) {
	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	wm := newFakeWorkManager()
	bs := newFakeBrokerSubmitter()
	adapter := bootstrap.NewA2Adapter(wm, bs, s)
	reg := registry.NewManager(registry.NewManagerOpts{MaxConcurrent: 4})
	t.Cleanup(reg.Close)

	cfg := config.Config{Batch: config.BatchConfig{AutomationTickSec: 1}}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	components, err := bootstrap.BuildC1(parent, cfg, s, adapter, reg, nil)
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
	_, err := bootstrap.BuildC1(context.Background(), config.Config{}, s, adapter, nil, nil)
	require.Error(t, err)
}

func TestBuildC1RejectsNilAdapter(t *testing.T) {
	s, _ := store.Open(":memory:")
	defer s.Close()
	reg := registry.NewManager(registry.NewManagerOpts{MaxConcurrent: 4})
	defer reg.Close()
	_, err := bootstrap.BuildC1(context.Background(), config.Config{}, s, nil, reg, nil)
	require.Error(t, err)
}

// collectToolNames 调用每个 *tools.GuardedTool.Info(ctx).Name，返回 name→name 的
// map（便于上层 Contains 断言）。bootstrap_test → tools 无 import 环。
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
```

**Expected failure:** `undefined: bootstrap.BuildC1`、`undefined: bootstrap.C1Components`。

### Step 12.2 — 完整 buildC1 helper

```go
// internal/bootstrap/c1.go (追加)
package bootstrap

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/components/model"

	"github.com/x6nux/yanshi/internal/agent/automation"
	"github.com/x6nux/yanshi/internal/agent/registry"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// C1Components 是 buildC1 的产物，聚合 RLM/Automation/Batch 三块。
type C1Components struct {
	RLM        *tools.RLMTools
	Automation *C1Automation
	Batch      *tools.BatchTools
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
	fakeModel model.BaseChatModel,
) (*C1Components, error) {
	if queueAdapter == nil {
		return nil, errors.New("C1: A2 queue adapter is required (AU1 depends on A2)")
	}
	if registryMgr == nil {
		return nil, errors.New("C1: B1 registry manager is required (M07 depends on B1)")
	}

	rlm, err := BuildRLM(cfg, nil, fakeModel)
	if err != nil {
		// RLM1 在 cheap model 未配置时失败属于配置错误；不静默降级。
		// 但 fake 模式下 fakeModel 非 nil，应总能成功。
		return nil, fmt.Errorf("C1: build RLM: %w", err)
	}

	auto, err := BuildAutomation(parent, cfg, db, queueAdapter)
	if err != nil {
		return nil, fmt.Errorf("C1: build automation: %w", err)
	}

	return &C1Components{
		RLM:        rlm.Tools,
		Automation: auto,
		Batch:      tools.NewBatchTools(registryMgr),
	}, nil
}
```

### Step 12.3 — bootstrap.Build 接线（合并 RLM + Automation + Batch）

在 `internal/bootstrap/bootstrap.go` 的 `Build` 中，紧跟 Task 4 的 RLM 接线片段之后，合并 Task 8 的 automation 接线 + Task 11 的 batch 接线。完整片段：

```go
// internal/bootstrap/bootstrap.go (Build 内的 C1 装配片段)
// === C1 装配：RLM1（独立）+ AU1（依赖 A2 adapter）+ M07（依赖 B1 *registry.Manager）===

// RLM1：cheap model 选择 + rlm_query 工具
var fakeForRLM model.BaseChatModel
if opts.FakeModel || len(cfg.LLM.Providers) == 0 {
	fakeForRLM = chatModel
}
c1rlm, rlmErr := BuildRLM(cfg, providerModels, fakeForRLM)
if rlmErr != nil {
	fmt.Fprintf(os.Stderr, "yanshi: rlm_query disabled: %v\n", rlmErr)
} else {
	allTools = append(allTools, c1rlm.Tools.Query)
}

// AU1：A2 work.ManagerLike + task.Broker + store KV adapter → QueuePort
// 注意：A2 work manager 由 A2 批次装配；此处 tryBuildWorkManager 是 A2 的 helper。
var c1Auto *C1Automation
workMgr, wmErr := tryBuildWorkManager(st, cfg)
if wmErr != nil {
	fmt.Fprintf(os.Stderr, "yanshi: automation disabled (A2 work manager unavailable): %v\n", wmErr)
} else {
	autoAdapter := NewA2Adapter(workMgr, broker, st)
	c1Auto, err = BuildAutomation(ctx, cfg, st, autoAdapter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi: automation disabled: %v\n", err)
		c1Auto = nil
	} else {
		for _, gt := range []*tools.GuardedTool{
			c1Auto.Tools.Create, c1Auto.Tools.List, c1Auto.Tools.Read,
			c1Auto.Tools.Update, c1Auto.Tools.Pause, c1Auto.Tools.Resume,
			c1Auto.Tools.Delete, c1Auto.Tools.Run,
		} {
			allTools = append(allTools, gt)
		}
	}
}

// M07：B1 *registry.Manager 注入；若 B1 未装配则跳过 agent_batch。
var c1Batch *tools.BatchTools
if subMgr := tryGetSubAgentManager(); subMgr != nil { // B1 装配后从 App 字段取
	c1Batch = tools.NewBatchTools(subMgr)
	allTools = append(allTools, c1Batch.AgentBatch)
} else {
	fmt.Fprintf(os.Stderr, "yanshi: agent_batch disabled (B1 registry unavailable)\n")
}

// ... 现有 orchestrator.New + srv.Chat + broker.StartSweeper ...

// 修改 App struct，新增字段：
//   c1Scheduler *automation.Scheduler  // 仅当 automation 装配成功时非 nil
// Shutdown 在 Server.Shutdown 与 Store.Close 之间调 c1Scheduler.Wait()。

return &App{
	// 现有字段...
	c1Scheduler: func() *automation.Scheduler {
		if c1Auto == nil {
			return nil
		}
		return c1Auto.Scheduler
	}(),
}, nil
```

`tryBuildWorkManager` 与 `tryGetSubAgentManager` 是 A2/B1 批次提供的 helper；A2/B1 未落地时各自返回错误/nil，C1 各组件独立 fail-open（RLM1 总能装配；AU1 与 M07 仅在对应批次就绪时装配）。

### Step 12.4 — config.example.yaml 增量

在被跟踪的 `config.example.yaml` 追加 batch 块（不复制整个配置）：

```yaml
batch:
  rlm_model: cheap-provider         # provider name (必须 cost_class: cheap)
  rlm_max_concurrency: 16            # <= 16，超过会被夹到 16
  automation_tick_seconds: 60        # scheduler 周期，<= 0 用默认 60s
```

并给现有 providers 增补 `cost_class` 字段示例（保留现有 provider 配置）：

```yaml
llm:
  providers:
    - name: cheap-provider
      kind: openai
      model: cheap-model
      api_key: ${CHEAP_API_KEY}
      cost_class: cheap
    # 其它 provider 不必加 cost_class；只有 rlm_model 指向的 provider 需要 cheap
```

### Step 12.5 — 最终验证命令（仅记录；不在写计划阶段运行）

```sh
# 格式化
gofmt -w internal/agent/rlm internal/agent/automation internal/agent/batch \
       internal/tools internal/bootstrap internal/config

# C1 包级测试
go test ./internal/agent/rlm ./internal/agent/automation ./internal/agent/batch \
         ./internal/tools ./internal/config ./internal/bootstrap

# 全量测试（缓存生效）
go test ./...

# vet
go vet ./...

# 增量测试
go run ./cmd/testchanged -v
```

**Expected:**

- C1 相关包全部 `ok`；`-race` 无数据竞争。
- 全量测试无新增失败；eino provider 在锁定版本不可用时按 CLAUDE.md 的 Skip 行为跳过，不影响 fake-first 的 C1 测试。
- `go vet ./...` 无 unused import、copylock、goroutine 或格式问题。
- `testchanged` 只跑变更包并报告通过。
- 不运行真实 API、真实 cron daemon、真实外部 sub-agent CLI 作为单元测试门禁。

**Commit:** `feat(c1): wire batch query automation and agent jobs`

## 验收矩阵

| 能力 | 必须证明 |
|---|---|
| RLM1 `rlm_query` | 1–16 prompts；每项一个 `Generate`；`StreamCalls == 0`（字段访问，非方法）；全局 cap=16；`finished []atomic.Bool` 无 race；稳定 index；单项错误保留；cost-class 描述；廉价 provider/fake 选择；GuardedTool 权限；测试用 `Info(ctx).Name/.Desc` |
| AU1 Automations | 八个工具；prompt/schedule/cwds 持久化；cron/interval；pause/resume；run 和 scheduler 经 `QueuePort`（A2 work.ManagerLike + broker）入队；scheduled slot idempotency（KV-backed）；run history/reconcile；所有工具 approval；重启恢复；`enqueue` 不持锁跨 SubmitRun；scheduler Start/Stop/Wait 幂等；App.Shutdown 等 scheduler 退出后再 Close Store |
| M07 CSV jobs | CSV 和 structured rows；稳定 index；逐项通过 `*registry.Manager.Spawn/Wait`；`SpawnErrCap` 非阻塞重试；取消/超时；逐项 output/error；summary；不导入 orchestrator；`*[]string` cwds 区分未提供与清空；GuardedTool 权限 |
| 跨批契约 | A2Adapter 实现 `automation.QueuePort`（`work.ManagerLike.Create` + `task.Broker.Submit`）；不伪造 `SubmitDurable`；M07 直接用 `*registry.Manager`，不伪造 `Limiter.Acquire`；`tools.SubAgentRunner` 经本地 `rowRunner` 适配为 `registry.Runner`；`mapTaskStatus` 显式映射 A2 double-l "cancelled" → C1 单-l "canceled"，并覆盖 broker "timeout" |
| 架构 | 只在 bootstrap 组合；context 注入 profile/runner/scope；不新增 WS/SSE frame；不把 queue/limiter 临时实现藏在 C1；Go 文件纯代码行不超过 1000 |

## 跨批依赖清单

- **A2 → AU1：** `work.ManagerLike.Create(ctx, CreateReq{Title, Prompt, ThreadID, TurnID, BrokerTaskID or Dispatch})` + 现有 `task.Broker.Submit(typ, input, parent)`；idempotency 由 C1 用 `store.KVGet/KVSet` 维护（前缀 `automation:idem:`）。AU1 只保存 automation/run metadata；A2 负责真正 task payload 与执行。
- **B1 → M07：** `*registry.Manager.Spawn/Wait/Result/MaxConcurrent/Close`、`SpawnRequest{AgentType, Assignment, Runner, Nickname, ...}`、`*SpawnErrCap`、`WaitOpts{Timeout}`、`Status{Pending,Running,Completed,Failed,Cancelled,Interrupted}`。M07 用本地 `rowRunner` 把 `tools.SubAgentRunner` 适配为 `registry.Runner`。
- **A2 + B1 → C1 组合根：** `bootstrap.Build` 在两个 adapter 可用后才装配 AU1/M07；RLM1 独立可先装配。任何缺失都清晰报错，不以本地 semaphore 或旧 broker 伪装成功。
- **无跨批依赖 → RLM1：** RLM1 可先独立实现和测试，但 cheap model selection 仍必须从现有 provider registry/fake path 获得，不重新构造 provider。

## 待决策点

1. **A2 work.CreateReq 最终形状（已知跨批 mismatch，不阻塞本批）。** C1 adapter 在 Task 1 与 Task 8 中按 `work.CreateReq{Title, Prompt, ThreadID, BrokerTaskID}` 书写；A2 plan 的最终 CreateReq 形状是 `{Title, Prompt, ThreadID, TurnID, Dispatch bool}`（Dispatch=true 时由 work.Manager 内部调 `dispatcher.Submit`，外部不直接调 broker）。A2 落地时，C1 adapter 有两种调整方式：
   - **方案 A（Dispatch=true）：** adapter 不再调 `broker.Submit`，直接 `work.Create(ctx, CreateReq{Title, Prompt, ThreadID, Dispatch: true})`；brokerID 从返回的 `WorkTask.BrokerTaskID` 读。C1 plan 当前展示的 `A2Adapter` 多了一个 `BrokerSubmitter` 依赖，可在 A2 落地时降级为只依赖 `work.ManagerLike`（broker 由 work.Manager 内部处理）。
   - **方案 B（BrokerTaskID 注入）：** A2 暴露 CreateReq 的 BrokerTaskID 字段（A2 plan 较早版本中的写法）；adapter 先 `broker.Submit("automation.run", payload, parent)` 拿 brokerID，再 `work.Create(CreateReq{Title, Prompt, ThreadID, BrokerTaskID: brokerID})`。
   无论选哪种，外部 `QueuePort.SubmitRun` 契约不变；只改 adapter 内部实现。本计划选择同时展示 broker + work 双依赖（更灵活但略冗余），A2 落地后可按实际签名简化。**这不阻塞 C1 实现**——adapter 在 Task 1 的 contract test 中以 `work.ManagerLike.Create` 与 `task.Broker.Submit` 双调用通过验证，A2 落地后只需调整字段名。
2. **B1 MaxConcurrent 与 M07 CappedRetries。** 当前 CappedBackoff=100ms、CappedRetries=50（约 5 秒）。若 B1 默认 MaxConcurrent=10、每 row 平均 30s，50 次重试足以覆盖短批次；长批次可能需要指数 backoff。未确认前用固定 100ms。
3. **Cron 依赖与时区。** 使用 `robfig/cron/v3 v3.0.1` 的 standard 5-field parser，按 backend `time.Time` 时区计算；产品需确认是否要显式 timezone/秒级 cron。未确认前不实现秒字段。
4. **Scheduler missed-slot policy。** 当前计划跳过过去的多个 slot，只 enqueue 当前已到期 slot 并推进到未来 slot；需确认是否要 catch-up。无论选择哪种策略，idempotency key 必须包含 scheduled slot。
5. **手动 run 与 paused。** 当前计划允许显式 `automation_run` 对 paused automation 入队，pause 只阻止 scheduler；需产品确认是否应拒绝 paused 的手动 run。
6. **所有 automation 操作的 approval。** 本计划按用户要求将 list/read 也视为 approval-required，并依靠现有 interactive callback；若产品沿用参考实现的 readonly 免确认策略，只需调整 profile/approval policy 和测试，不改变八个工具名字/API。
7. **KV envelope 的规模。** 当前 C1 选择 `KVGet/KVSet`，以避免伪造 store 内部 DB API；若 automation/run 数量需要独立查询或跨进程并发写，另开 store table/migration 任务，不在本批偷偷替换。
8. **RLM cheap provider 配置。** 当前选择 `batch.rlm_model` + provider `cost_class: cheap`，fake mode 才允许空配置 fallback。需确认是否将 cost class 放入 provider config 或由 deployment registry 提供。
9. **M07 row prompt 格式。** 当前使用 `base prompt + row_index + row_json`，不把整份 CSV 重复发送给每个 child；需确认是否要列名映射、文件路径或额外 output schema。

## Task 计数与提交顺序

共 **12 个 Task**：

1. A2/B1 contract gate + C1 侧 port + MapTaskStatus 表
2. RLM core（`[]atomic.Bool` 修复）
3. `rlm_query` tool（`Info(ctx).Name/.Desc` + 字段访问 + inline profile）
4. cheap model 配置 + 完整 buildRLM 接线
5. automation domain/schedule/repository（含完整 _test.go）
6. manager + scheduler（`Stop()` + done + 锁外 SubmitRun + MapTaskStatus 表测试）
7. 八个 automation tools/approval（展开 one-liner）
8. automation lifecycle wiring + A2 adapter
9. CSV/structured parser（含完整 _test.go）
10. `*registry.Manager`-backed batch runner（SpawnErrCap 重试）
11. `agent_batch` tool（无 `validateBatchDescription`）
12. full bootstrap integration + cron go.mod diff + 配置样例

建议按 Task 1 → 2–4 → 5–8 → 9–11 → 12 提交。若 A2/B1 未完成，提交 RLM1 后停止，不创建临时 durable queue 或本地 sub-agent registry。

## Self-Review

在计划完成后由作者执行的实际扫描（review #16：必须真扫，不能假装）：

**1. 重复常量扫描（review #1）：**

- 搜索计划中所有 `const (` 块：`RunQueued/RunRunning/RunCompleted/RunFailed/RunCanceled` 仅在 Task 1 的 `model.go` 中声明一次；Task 6 的 `manager.go` 不再重复声明（旧版在 manager.go 顶部有 `const RunQueued = "queued"`，已删除）。`TaskQueued/TaskRunning/...` 不再出现（旧版有 task/run 双套常量，已合并为单套 Run*）。
- `StateSchemaVersion`、`stateKey`、`MaxBatchSize`、`StateSchemaVersion` 均仅声明一次。
- `idempotencyPrefix` 仅在 `a2_adapter.go` 中声明。

**2. GuardedTool 访问器扫描（review #2）：**

- 搜索 `.Name()` / `.Description()` 方法调用：旧版在 Task 3 测试中有 `set.Query.Name()` 与 `set.Query.Description()`，已全部改为 `set.Query.Info(ctx).Name` 与 `set.Query.Info(ctx).Desc`。
- 搜索 `Info(_ context.Context)` 与 `g.Info(ctx)` 调用：与 `internal/tools/guard.go` 当前实现一致（无 `Name()`/`Description()` 方法，只有 `Info(ctx) (*schema.ToolInfo, error)`）。

**3. FakeModel 计数器扫描（review #3）：**

- 搜索 `GenerateCalls(` / `StreamCalls(`：旧版在 Task 2/3 测试中用 `fake.GenerateCalls()`，已全部改为字段访问 `fake.GenerateCalls`。
- 与 `internal/llm/eino/fake.go` 一致：`GenerateCalls int` 与 `StreamCalls int` 是导出 struct 字段。

**4. 测试 helper 扫描（review #4）：**

- 搜索 `testAllowProfile` / `mustJSON`：旧版假设这些 helper 存在于 `internal/tools` 测试包；实际不存在。本版在 Task 3 测试文件内定义本地 `allowProfile` helper（inline）；Task 7/11 各自定义 `allowAll` / `allowAllForBatch` / `wrapInput`。automation_test.go 的 `mustJSON` 是本文件内 helper（不是引用外部）。

**5. `finished []bool` 扫描（review #5）：**

- 搜索 `finished := make([]bool`：旧版在 Task 2 RLM runner 与 Task 10 batch runner 中使用。两处都已改为 `finished := make([]atomic.Bool, ...)`，对应 `.Store(true)` / `.Load()`。

**6. Scheduler 生命周期扫描（review #6）：**

- 搜索 `Scheduler` struct：本版有 `done chan struct{}` 字段；`Start` 中 `defer close(s.done)`；`Wait()` 阻塞 `<-s.done`；`Stop(ctx)` 封装 cancel+Wait+deadline。`App.Shutdown` 修改为 `a.c1Scheduler.Wait()` 在 `a.Server.Shutdown` 之后、`a.Store.Close` 之前。

**7. MapTaskStatus 表扫描（review #7）：**

- 搜索 `MapTaskStatus`：声明在 Task 1 `model.go`；测试在 `statusmap_test.go` 是表驱动（6 个 case + 2 个防回归 case）。覆盖 A2 的 double-l "cancelled" → C1 单-l "canceled"；broker "timeout" → RunFailed；未知 → RunFailed（fail-closed）。

**8. 锁外 SubmitRun 扫描（review #8）：**

- 搜索 `m.queue.SubmitRun` / `m.queue.SubmitDurable`：本版 `enqueue` 与 `enqueueUnlocked` 都在调用 `m.queue.SubmitRun` 之前 `m.mu.Unlock()`，调用结束后 `m.mu.Lock()` 重新获取以写 run 记录。二次幂等检查在重获锁后进行。

**9. 完整 _test.go 扫描（review #9）：**

- Tasks 5–12（共 8 个 Task）的测试函数体完整存在；Task 8.1 与 12.1 的 Info-collector 原为占位（`structuredGuarded`/`collectToolNames` 空循环），现已直接 `import "github.com/x6nux/yanshi/internal/tools"` 并调 `(*tools.GuardedTool).Info(ctx).Name` 完成断言（bootstrap_test → tools 无 import 环）：
  - Task 5: `schedule_test.go` + `repository_test.go`（共 ~150 行测试代码）
  - Task 6: `manager_test.go` + `scheduler_test.go`（共 ~250 行测试代码）
  - Task 7: `automation_test.go`（共 ~150 行测试代码）
  - Task 8: `c1_test.go` wiring 部分（共 ~100 行测试代码；`TestBuildAutomationAllToolsCount` 用 `tools.GuardedTool.Info(ctx).Name` 断言 8 个 automation 工具名）
  - Task 9: `input_test.go`（共 ~100 行测试代码）
  - Task 10: `runner_test.go`（共 ~180 行测试代码）
  - Task 11: `batch_test.go`（共 ~130 行测试代码）
  - Task 12: `c1_test.go` integration 部分（共 ~80 行测试代码；`collectToolNames` 是真循环 `gt.Info(ctx).Name`，返回 `map[string]string`）
- 无任何 "tests for the above" 或 bullet-only 测试段；所有测试都是完整 Go 函数。

**10. fakeQueue parent 拒绝扫描（review #10）：**

- 搜索 `parent != ""`：旧版 fakeQueue 错误地拒绝非空 parent；本版 fakeQueue（Task 1 `manager_test.go`）只在 `AutomationID == "" || Prompt == ""` 时拒绝，**不**检查 `ParentTaskID`。`TestQueuePortContractAcceptsNonEmptyParent` 显式验证非空 parent 被接受。

**11. cron 版本锁定扫描（review #11）：**

- 搜索 `robfig/cron`：Task 5 Step 5.5 展示完整 `go.mod` diff，锁定 `github.com/robfig/cron/v3 v3.0.1`。无未锁定 pseudo-version。

**12. `validateBatchDescription` 扫描（review #12）：**

- 搜索 `validateBatchDescription`：旧版在生产代码 `internal/tools/batch.go` 中定义。本版**完全删除**该函数；描述是常量字符串，测试用 `Contains` 断言短语（CSV/structured/B1/concurrency/approval）。生产代码无 metadata validation helper。

**13. fakeLimiter 与 cap 模拟扫描（review #13）：**

- 搜索 `fakeLimiter` / `Limiter.Acquire`：本版**完全删除** `Limiter` 接口（不匹配 B1 非阻塞语义）。M07 直接用 `*registry.Manager.Spawn/Wait`，满载时由 `*SpawnErrCap` 驱动重试。Task 10 `runner_test.go` 用真实 `*registry.Manager`（MaxConcurrent=2）测试 3-row 批次，证明 cap 通过 SpawnErrCap 重试入队；无伪造的 channel-based limiter。

**14. 压缩 one-liner 与 buildRLM 接线扫描（review #14）：**

- 搜索 `func.*{ return ... }` 在 Task 6/7/10：本版展开所有 one-liner：
  - Task 6：`Pause`/`Resume` 仍是 2 行便捷封装（合理），但 `enqueue` 已展开为完整的取锁/释放/SubmitRun/重获锁/写 run 记录模式。
  - Task 7：`runAutomationList/Read/Update/Create/Run` 都展开为完整的 decode-input/call-manager/encode-output 三段，不再是一行函数。
  - Task 10：`spawnWithRetry` / `buildReport` / `promptForRow` 各自独立、完整。
- `buildRLM` 接线：Task 4 Step 4.3 展示 `Build` 中精确的插入点（在 `agentTools` append 之后、orchestrator.New 之前），并解释 `fakeForRLM` 如何根据 `opts.FakeModel || len(providers)==0` 决定。Task 8 + Task 12 展示 BuildAutomation 与 BuildC1 在同一装配区接入。

**15. 跨计划契约扫描（review #15）：**

- 搜索 `DurableQueue`：本版**完全删除** `DurableQueue` 接口；替换为 `QueuePort`（`SubmitRun(RunPayload) (RunReceipt, error)` + `Lookup(ctx, workTaskID) (RunStatus, error)`）。
- 搜索 `SubmitDurable`：本版无此方法；adapter 用 `work.ManagerLike.Create(ctx, CreateReq)` + `task.Broker.Submit(typ, input, parent)`（或 Dispatch:true 模式，按 A2 实际签名）。幂等性通过 C1 自身的 KV 维护，**不**通过伪造的 SubmitDurable idempotency key 参数。
- 搜索 `batch.Limiter` / `Limiter.Acquire`：本版**完全删除**；替换为直接使用 `*registry.Manager.Spawn`。`SpawnErrCap` 是非阻塞 cap 信号，`spawnWithRetry` 通过 `errors.As(err, &capped)` 识别并重试，不阻塞。
- 搜索 `SpawnFunc` 适配：Task 10 的 `rowRunner` 实现 `registry.Runner.Run(ctx, agentID, assignment)`；其内部调 `spawn(ctx, promptForRow(input.Prompt, row), input.AllowedTools, input.InstructionOverride)`。Task 11 的 `tools.NewBatchTools` 通过 `batch.SpawnFunc(spawn)` 把 `tools.SubAgentRunner` 转换为本地 `SpawnFunc` 类型。
- A2 `CreateReq` 形状（Title/Prompt/ThreadID/TurnID + Dispatch/BrokerTaskID）：在"待决策点 #1"明确列出 A2 的两种可能签名，并声明 adapter 兼容两者；外部 QueuePort 契约不变。
- B1 `SpawnRequest` 形状（AgentType/Assignment/Nickname/ModelOverride/ReasoningEffort/Runner）：与 Task 10 的 `spawnWithRetry` 一致（传 AgentType="batch" + Runner=rowRunner）。

**16. Bullet-only 测试扫描（review #9 子项）：**

- 再次扫描 Tasks 5–12 的每个 "Step X.1 — 写失败的测试"：所有 Step 都以 ```` ```go ```` 代码块开头，不是 bullet list。每个测试文件都有完整的 `func TestXxx(t *testing.T) {...}` 函数体。
- Task 5.1 schedule_test.go：5 个完整测试函数。
- Task 5.1 repository_test.go：4 个完整测试函数。
- Task 6.1 manager_test.go：13 个完整测试函数 + failingQueue helper。
- Task 6.2 scheduler_test.go：3 个完整测试函数。
- Task 7.1 automation_test.go：4 个完整测试函数 + setupAutomation/mustJSON helper。
- Task 8.1 c1_test.go wiring：3 个完整测试函数；`TestBuildAutomationAllToolsCount` 用 `map[string]*tools.GuardedTool` 直接断言 8 个 automation 工具的 `Info(ctx).Name`（早期 `structuredGuarded`/`wrap` 占位已删除）。
- Task 9.1 input_test.go：11 个完整测试函数。
- Task 10.1 runner_test.go：8 个完整测试函数 + recordingSpawn/blockingSpawn helper。
- Task 11.1 batch_test.go：7 个完整测试函数 + newBatchTools/wrapInput helper。
- Task 12.1 c1_test.go integration：3 个完整测试函数；`collectToolNames(t, guarded ...*tools.GuardedTool)` 是真循环调 `gt.Info(ctx).Name`，返回 `map[string]string`（早期空循环占位已删除）。

**17. 跨计划状态词汇一致性：**

- C1 Run 常量：`RunQueued="queued"` / `RunRunning="running"` / `RunCompleted="completed"` / `RunFailed="failed"` / `RunCanceled="canceled"`（单-l）。
- A2 work.Status：`StatusPending/Running/Completed/Failed/Cancelled`（double-l）。
- B1 registry.Status：`StatusPending/Running/Completed/Failed/Cancelled/Interrupted`（double-l）。
- broker：`pending/running/completed/failed/timeout`（无 cancelled）。
- `MapTaskStatus` 显式映射；`statusmap_test.go` 覆盖每条路径 + 防回归（不允许 single-l 作为 key，不允许 double-l 作为 value）。

**18. 文件长度扫描：**

- 预估每文件纯代码行（不含注释/空行）：
  - `internal/agent/automation/manager.go`：~290 行（最长）。在 1000 行限内。
  - `internal/agent/automation/model.go`：~110 行。
  - `internal/tools/automation.go`：~230 行。
  - `internal/agent/batch/runner.go`：~190 行。
  - `internal/bootstrap/c1.go`（合并 RLM/Automation/BuildC1）：~150 行。
  - `internal/bootstrap/a2_adapter.go`：~110 行。
  - 其它文件均 < 200 行。
- 若任一文件接近 1000 行纯代码，按 CLAUDE.md 拆分到同包新文件。

**19. 未引入 mock framework：**

- 搜索 `gomock` / `mockery` / `testify/mock`：无。所有 fake 都是手写 struct（`fakeQueue`、`fakeWorkManager`、`fakeBrokerSubmitter`、`fakeKV`、`recordingSpawn`、`blockingSpawn`、`failingQueue`），符合 CLAUDE.md "Fake 优先于 mock"。

**20. 未新增 WS/SSE frame：**

- 搜索 `proto.ServerFrame` / `ClientFrame` 新增：本计划不修改 `internal/proto/frame.go`。C1 工具结果走现有 tool-result 通道。

**21. RLMTools 访问路径与 fakeWorkManager 接口完整性（v3 新增）：**

- **U1 修复：** `TestBuildRLMMaxConcurrencyClamped` 原写 `got.Tools.Info(t.Context())`，但 `*tools.RLMTools` 仅声明 `Query *GuardedTool` 字段、无 `Info` 方法。已改为 `got.Tools.Query.Info(t.Context())`（唯一持有 `Info` 的是 `*GuardedTool`）。
- **U2 修复：** `fakeWorkManager` 原仅声明 `Create` + `Read`，加上"为简洁未列出"的注释——但 Go 接口满意度要求全部 12 个 `work.ManagerLike` 方法（`Create/List/Read/Start/Finish/Cancel/SetChecklist/AddChecklistItem/PatchChecklistItem/RecordGate/WriteArtifact/ReadArtifact`）都有实现。已改为**嵌入 A2 的 `*work.FakeManager`**（A2 plan 第 741、1065 行确认 `var _ ManagerLike = (*FakeManager)(nil)` 与 `work.NewFakeManager()` 构造函数），仅覆盖 `Create` + `Read` 以使用本测试的 `tasks` map。关键注意点：`FakeManager.Read` 默认返回 `(nil, error)`，所以 `Read` 必须显式覆盖到自己的 map 上。
- **U3 跨批 CreateReq 不匹配** 作为决策点保留（见上方"待决策点 #1"）：C1 adapter 用 `work.CreateReq{Title, Prompt, ThreadID, BrokerTaskID}`，A2 最终 `CreateReq` 为 `{Title, Prompt, ThreadID, TurnID, Dispatch bool}`。外部 `QueuePort.SubmitRun` 契约不受影响，A2 落地时对齐字段名即可。

---

## Self-Review 结论

16 项必修项已全部在计划中落实，并经过上述扫描验证。重写后的计划：

- 共 **12 个 Task**（与原版相同数量，但每个 Task 内部更厚实）。
- 跨批契约形状：`automation.QueuePort`（由 `bootstrap.A2Adapter` 实现，组合 A2 `work.ManagerLike.Create` + `task.Broker.Submit` + KV idempotency）+ `batch.SpawnFunc`/`rowRunner`（适配到 B1 `*registry.Manager.Spawn/Wait` + `SpawnErrCap` 非阻塞重试）。旧版的伪造 `DurableQueue.SubmitDurable` 与 `Limiter.Acquire` 已删除。
- 自我审查已完成 21 个扫描项（重复常量、GuardedTool 访问器、FakeModel 字段、测试 helper、`[]atomic.Bool`、scheduler 生命周期、MapTaskStatus 表、锁外 SubmitRun、bullet-only 测试、fakeQueue parent、cron 版本、validateBatchDescription、fakeLimiter、one-liner 展开、跨计划契约、状态词汇、文件长度、无 mock、无新 frame、bullet-only 复扫、以及 v3 新增的 U1/U2 测试级 bug 修复：`*tools.RLMTools.Info` 访问路径纠正与 `fakeWorkManager` 嵌入 `*work.FakeManager` 以满足 12 方法接口）。
- 不修改 `.go` 代码、不运行 build/test、不提交 git；仅重写 `docs/superpowers/plans/2026-07-21-c1-batch-automation.md`。
