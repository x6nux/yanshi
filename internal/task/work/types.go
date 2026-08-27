// Package work 提供 durable task 工作单元的领域模型、事件与 port。
//
// 本包只承担工作单元语义（创建/列表/读取/取消、timeline、checklist、gates、artifacts），
// 不导入 internal/tools 或 internal/proto —— 前者会反向依赖，后者会成环。
// 与 internal/task.Broker 的协作仅通过 Dispatcher port 完成。
package work

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Status 是 WorkTask 的状态枚举。
type Status string

// WorkTask lifecycle statuses.
const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// IsTerminal 报告该状态是否为终态（不可再转移）。
func (s Status) IsTerminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCancelled
}

// legalTransitions is the state machine, written out edge by edge.
//
// The predecessor accepted ANY known status from any non-terminal one, which
// made "状态机正确" a claim about the enum rather than about the machine:
// pending→completed was legal, so a task could report success without ever
// having run. Enumerating the edges is what makes the name true.
//
// running→pending is deliberately IN the set: it is how an interrupted task is
// returned to the queue, both by the broker's retry path and by
// Manager.RecoverInterrupted after a restart. pending→completed and
// pending→failed are OUT — a task that was never dispatched has nothing to
// report — with one consequence worth stating: a dispatch failure leaves the
// task pending and records the reason on the timeline (Manager.Create already
// does exactly that) rather than moving it to failed.
var legalTransitions = map[Status]map[Status]bool{
	StatusPending: {
		StatusRunning:   true,
		StatusCancelled: true,
	},
	StatusRunning: {
		StatusCompleted: true,
		StatusFailed:    true,
		StatusCancelled: true,
		StatusPending:   true,
	},
}

// KnownStatuses returns every legal Status value. It exists so tests can walk
// the whole matrix instead of listing the values a second time — a second list
// drifts, and the drift is silent in the direction that matters (a status added
// to the enum but not to legalTransitions would simply never be reachable).
func KnownStatuses() []Status {
	return []Status{StatusPending, StatusRunning, StatusCompleted, StatusFailed, StatusCancelled}
}

func (s Status) isKnown() bool {
	for _, k := range KnownStatuses() {
		if s == k {
			return true
		}
	}
	return false
}

// ErrIllegalTransition wraps every rejection from CanTransitionTo, so callers
// can tell "the state machine said no" from "the database said no".
//
// The distinction is load-bearing for the broker mirror: a durable task
// cancelled from the work side while its broker row is still finishing will
// reject the incoming terminal transition, and that is the correct outcome
// rather than a fault to log.
var ErrIllegalTransition = errors.New("work: illegal status transition")

// CanTransitionTo reports whether s may move to next, per legalTransitions.
// Every non-nil return wraps ErrIllegalTransition.
func (s Status) CanTransitionTo(next Status) error {
	switch {
	case !next.isKnown():
		return fmt.Errorf("%w: unknown target status %q", ErrIllegalTransition, next)
	case s.IsTerminal():
		return fmt.Errorf("%w: terminal status %q cannot move to %q", ErrIllegalTransition, s, next)
	case !legalTransitions[s][next]:
		return fmt.Errorf("%w: %q -> %q", ErrIllegalTransition, s, next)
	}
	return nil
}

// ChecklistItemStatus 是 ChecklistItem 的状态枚举。
type ChecklistItemStatus string

// Checklist item statuses.
const (
	ChecklistPending    ChecklistItemStatus = "pending"
	ChecklistInProgress ChecklistItemStatus = "in_progress"
	ChecklistDone       ChecklistItemStatus = "done"
)

// ChecklistItem 是清单中的一条。
//
// Verify (L7) is the machine-checkable condition that decides Status, when one
// is set. Without it the checklist is the model's own account of its progress,
// which is exactly the claim a completion gate must not take on trust — see
// checklistgate.go. The zero value means "no condition", so every item written
// before L7 keeps its recorded status.
type ChecklistItem struct {
	ID      int                 `json:"id"`
	Content string              `json:"content"`
	Status  ChecklistItemStatus `json:"status"`
	Verify  ChecklistCondition  `json:"verify,omitzero"`
}

// Checklist 是一个任务的可选清单。
type Checklist struct {
	Items []ChecklistItem `json:"items"`
}

// CompletionPct 返回已完成的百分比（0-100）。
// 空清单的完成率为 0（与"显示 100% 会让用户误以为全部完成"相比，0 更保守）。
func (c Checklist) CompletionPct() int {
	if len(c.Items) == 0 {
		return 0
	}
	done := 0
	for _, item := range c.Items {
		if item.Status == ChecklistDone {
			done++
		}
	}
	return done * 100 / len(c.Items)
}

// Evidence 是一次 gate（测试/lint/build）执行的结构化证据。
// 命令失败是结构化证据而非 Go error —— 只有启动/持久化失败才是 Go error。
type Evidence struct {
	ID             string `json:"id"`
	Gate           string `json:"gate"`
	Command        string `json:"command"`
	Cwd            string `json:"cwd"`
	ExitCode       int    `json:"exit_code"`
	DurationMs     int64  `json:"duration_ms"`
	Classification string `json:"classification"`
	Summary        string `json:"summary"`
	LogArtifactID  string `json:"log_artifact_id,omitempty"`
	RecordedAt     int64  `json:"recorded_at"`
}

// ClassificationFromExitCode 把退出码映射成 pass/fail/error 三类。
// 0 -> pass；>0 -> fail（命令跑了但失败）；<0 -> error（启动/信号等）。
func ClassificationFromExitCode(exitCode int) string {
	if exitCode == 0 {
		return "pass"
	}
	if exitCode > 0 {
		return "fail"
	}
	return "error"
}

// Artifact 是一个任务的只读持久工件（如大日志/输出快照）。
// 持久内容位于 .yanshi/artifacts/，与临时 spillover 分离。
type Artifact struct {
	ID         string `json:"id"`
	TaskID     string `json:"task_id"`
	Label      string `json:"label"`
	Summary    string `json:"summary"`
	ContentRef string `json:"content_ref"`
	Size       int64  `json:"size"`
	CreatedAt  int64  `json:"created_at"`
}

// TimelineEntry 是任务时间线上的一条事件（创建/状态转移/dispatch_failed/...）。
type TimelineEntry struct {
	At               time.Time `json:"at"`
	Kind             string    `json:"kind"`
	Summary          string    `json:"summary"`
	DetailArtifactID string    `json:"detail_artifact_id,omitempty"`
}

// WorkTask 是 durable task 工作单元的核心领域模型。
type WorkTask struct {
	ID           string          `json:"id"`
	Title        string          `json:"title"`
	Prompt       string          `json:"prompt"`
	Status       Status          `json:"status"`
	ThreadID     string          `json:"thread_id,omitempty"`
	TurnID       string          `json:"turn_id,omitempty"`
	BrokerTaskID string          `json:"broker_task_id,omitempty"`
	Checklist    Checklist       `json:"checklist"`
	Gates        []Evidence      `json:"gates"`
	Artifacts    []Artifact      `json:"artifacts"`
	Timeline     []TimelineEntry `json:"timeline"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// Summary 是 WorkTask 的精简视图，用于列表展示。
type Summary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    Status    `json:"status"`
	ThreadID  string    `json:"thread_id,omitempty"`
	TurnID    string    `json:"turn_id,omitempty"`
	Pct       int       `json:"pct"`
	GateCount int       `json:"gate_count"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreateReq 是 Create 的请求。
// Dispatch=true 时才会经 Dispatcher 投递到 broker。
type CreateReq struct {
	Title    string
	Prompt   string
	ThreadID string
	TurnID   string
	Dispatch bool
}

// EventKind 标识一次工作单元事件的本类。
type EventKind string

// Work unit event kinds broadcast by Manager.
const (
	EventTaskUpdate      EventKind = "task_update"
	EventPlanUpdate      EventKind = "plan_update"
	EventChecklistUpdate EventKind = "checklist_update"
)

// Event 是 Manager 向外广播的领域事件。
// Task 非 nil 时携带完整快照；TaskID 始终填充；Checklist 在 checklist 事件时填充。
type Event struct {
	Kind      EventKind
	Task      *WorkTask
	TaskID    string
	Checklist Checklist
}

// ManagerLike 是工作单元管理的 port；真实实现是 Manager，Fake 实现是 FakeManager。
// 该 port 也是后续 B3 等批次依赖的稳定公共 API，签名不应轻易改动。
type ManagerLike interface {
	Create(context.Context, CreateReq) (*WorkTask, error)
	List(context.Context, int, string) ([]Summary, error)
	Read(context.Context, string) (*WorkTask, error)
	Start(context.Context, string) error
	Finish(context.Context, string, Status, string) error
	Cancel(context.Context, string, string) (*WorkTask, error)
	SetChecklist(context.Context, string, Checklist) (*WorkTask, error)
	AddChecklistItem(context.Context, string, string) (*WorkTask, error)
	PatchChecklistItem(context.Context, string, int, string, ChecklistItemStatus) (*WorkTask, error)
	RecordGate(context.Context, string, Evidence) error
	WriteArtifact(context.Context, string, string, []byte, string) (Artifact, error)
	ReadArtifact(context.Context, string) (Artifact, error)
}

// Dispatcher 是 Manager 向 broker 投递的领域 port。
// 真实实现 BrokerAdapter 只把这两个动作委托给 internal/task.Broker，
// 不复制 claim/heartbeat/result/retry（那些职责仍属于 broker）。
type Dispatcher interface {
	Submit(string, string, string) (string, error)
	Cancel(string) error
}

// ArtifactPolicy 是 Manager 写 artifact 时的 quota/TTL 策略。
// 零值经 withDefaults() 转成默认值（DefaultArtifactQuota / DefaultArtifactTTL）。
// ReadSize/MaxReadSize 用于 artifact_read（Task 9）的 authorize-before-I/O。
type ArtifactPolicy struct {
	QuotaBytes  int64
	TTL         time.Duration
	ReadSize    int64
	MaxReadSize int64
}

// withDefaults 补齐零值为默认值；调用方在 NewManager 时调用一次。
func (p ArtifactPolicy) withDefaults() ArtifactPolicy {
	if p.QuotaBytes <= 0 {
		p.QuotaBytes = DefaultArtifactQuota
	}
	if p.TTL <= 0 {
		p.TTL = DefaultArtifactTTL
	}
	if p.ReadSize <= 0 {
		p.ReadSize = DefaultArtifactReadSize
	}
	if p.MaxReadSize <= 0 {
		p.MaxReadSize = MaxArtifactReadSize
	}
	return p
}

// 默认值常量（与 plan 已决策约束 §8 对齐）：
//   - 每 task 默认 64 MiB 总配额；
//   - 单次 artifact_read 默认 64 KiB / 最大 1 MiB；
//   - TTL 7 天。
const (
	DefaultArtifactQuota    int64 = 64 << 20
	DefaultArtifactTTL            = 7 * 24 * time.Hour
	DefaultArtifactReadSize       = 64 << 10
	MaxArtifactReadSize           = 1 << 20
)
