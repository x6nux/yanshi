// Package automation provides the C1 batch automation domain: persistent
// schedules, run history, and a QueuePort for submitting work through the
// A2 work-manager + task-broker pipeline.
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
	WorkTaskID   string
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

// Schedule 表达何时运行下一次。Kind ∈ {"cron","interval"}.
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
