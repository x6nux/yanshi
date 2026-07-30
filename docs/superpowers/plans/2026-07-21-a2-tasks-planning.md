# Batch A2 — 任务与计划模型 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task. 每个 Task 按 TDD 顺序执行：先写失败测试，再实现，再运行该 Task 指定的测试。不要并行修改同一文件。

**Goal:** 引入模型/用户可见、SQLite 持久化的 `WorkTask` 工作单元，提供任务、计划、清单、验证证据和大输出工件；新增真正只读的 Plan mode，并保证同一语义通过 WS 与 SSE 的统一帧词表传播。

**Architecture:** 新建 `internal/task/work` 作为 durable-task 领域层。现有 `internal/task.Broker` 保持“远程 worker 传输分发”职责；`work.Manager` 保持“工作单元语义”职责。两者仅通过小型 `Dispatcher` port 协作：可选地把 `WorkTask` 投递到 broker、记录 `BrokerTaskID`、取消对应 broker 行，不复制 claim/heartbeat/result/retry。Plan mode 是 **per-turn** 状态：HTTP 层将连接/请求状态写入 `orchestrator.TurnOpts`，orchestrator 注入 turn context；绝不在共享 `Orchestrator` 上保存 `threadLink` 或 `planActive`。ADK Runner cache key 扩展为 `{model pointer, toolMode}`；Plan Runner 只注册 plan-safe 工具，模式边界显式 `FlushRunners()`，所以下一 turn 会用对应工具子集重建。所有 durable-task 变更经 `work.Event` typed callback 下沉到 orchestrator，再由 WS/SSE 各自的同形 writer 发出共享 `proto.ServerFrame`。

**Tech Stack:** Go 1.26.4；`database/sql` + `modernc.org/sqlite`；Eino ADK；context value 注入；guard fail-closed；WebSocket + SSE；Bubble Tea；Fake 优先。

**Spec:** `docs/feature-roadmap-codex-deepseek.md` §0.3、§4 `[DT1][G05][DT2][DT3]`。

---

## 已决策约束

1. **per-turn 状态：** `ThreadID`、`TurnID`、`PlanMode` 只存在于 `TurnOpts`/turn context。删除并禁止 `SetThreadLink`、`SetPlanActive` 及 Orchestrator 上同类可变字段。
2. **Runner 工具集：** cache key 为 `{model, mode}`。`mode=plan` 时 ADK `ToolsNodeConfig.Tools` 使用过滤后的 plan-safe 子集；runtime `Authorize` 再做第二层 fail-closed 检查。模式边界仍调用 `FlushRunners()`，但 flush 的效果来自下一 turn 按新 key/新工具子集重建，而不是“同一工具列表重建”。
3. **Plan mode 历史连续：**切换模式不清空 `connSession.history`、SSE client history 或 token 状态。
4. **Plan mode 工具：**允许读取、任务元数据创建与计划/清单更新；拒绝 shell、文件写、网络写、VCS 写、`task_cancel`。`task_create` 是 plan-safe 元数据写，解决“进入 Plan mode 后没有 task_id”的自举问题。
5. **`task_cancel` approval-required：**通过 `forcePromptTools` 强制进入 callback，即使 profile 为 `Tools.Allow=["*"]` 也不能静态放行；SSE 没有 callback，因此 fail-closed。YOLO/Auto 也不能自动批准 force-prompt 请求。
6. **SSE 模式控制：**SSE 仍是 stateless POST，不新增 `set_mode` 控制通道。TUI 在 SSE 下执行 `/plan`、`/plan-off` 时立即显示本地错误，不发送服务端请求。
7. **`/plan-off`：**恢复进入 Plan 前的 permission mode 与 auto threshold；若没有记录，则回到 `ModeDefault`。Plan 不加入 Shift+Tab cycle。
8. **Artifact policy：**每 task 默认 64 MiB 总配额；单次 `artifact_read` 默认/最大 64 KiB/1 MiB；TTL 7 天；启动时执行一次清理并启动 6 小时间隔 janitor。持久内容位于 `.yanshi/artifacts/`，与临时 `.yanshi/tmp/spillover/` 分离。
9. **Gate 语义：**命令失败是结构化 `Evidence{classification:"fail"}`，不是 Go error；启动/持久化失败才是 Go error。`RecordGate` 失败必须返回，禁止 `_ =` 忽略；`AppendTimeline`（含 `dispatch_failed`）同样检查错误。
10. **Broker 复用：**`task_create{dispatch:true}` 才投递 broker；默认 local-only。broker 的 claim/heartbeat/result/retry 保持原样。
11. **pathjail 是唯一 canonical root-jail：**新建 `internal/pathjail` 导出 `WithinRootAbs(root, candidate string) (string, error)`。`internal/tools/securepath.go` 与 `internal/task/work/securepath.go` 都只是薄封装，禁止复制 symlink/volume/case 算法。这条解决"`work` 调用 `internal/tools`"的反向依赖（依赖方向只能 `tools → work`、`tools/work → pathjail`，不能 `work → tools`）。
12. **无悬空 helper：**`newID/truncate/summaryOf/summarizeArtifact/newEvidenceID/summarizeGateOutput/newClientThreadID` 7 个全部在计划中给出签名与行为契约（见末尾"Shared helpers"），实现期不得再引入新的未声明 helper。
13. **Store API 显式：**Manager 委托的每个 Store 原子方法（`AppendTimeline`/`AttachBrokerTask`/`Transition`/`PatchChecklistItem`/`RecordGate`/…）在本计划内都给出签名、SQL 骨架与对应测试，不允许"实现期再补"。
14. **`o.runner` 字段删除：**所有 turn 入口经 `runnerFor(model, plan)` 取得 runner；不再保留独立 default-runner 字段，也不再在 `runnerFor` 出错时回退到它。

---

## 职责边界与依赖方向

- `internal/task/broker.go`：transport dispatch；`Submit/Claim/Heartbeat/RecordResult/RequeueStale`。
- `internal/task/work`：domain semantics；`Create/List/Read/Cancel`、timeline、checklist、gates、artifacts。**禁止导入 `internal/tools` 或 `internal/proto`**——前者会反向依赖，后者会成环。
- `internal/pathjail`：唯一 canonical root-jail（`WithinRootAbs`）；`tools` 与 `work` 各自的 securepath 都是它的薄封装。
- `work.Dispatcher` 是领域 port；`work.BrokerAdapter` 是适配器。Manager 不读取 broker store、不实现 claim/retry。
- migration 由 `bootstrap.Build` 在 `store.Open` 后调用 `work.FromDB(st.DB)`；**不修改 `internal/store/store.go` 让 store 反向依赖 work**。
- `tools` 通过 context 获取 `TaskManagerCtx`、thread link、plan flag 和 event sink；工具参数不携带权限/profile。
- `bootstrap.Build` 仍是唯一组合根。

---

## File Structure

| 文件 | 动作 | 职责 |
|---|---|---|
| `internal/task/work/types.go` | Create | WorkTask/Checklist/Evidence/Artifact/Event/接口 |
| `internal/task/work/store.go` | Create | work schema、事务 CRUD、quota/TTL 元数据 |
| `internal/task/work/manager.go` | Create | 领域规则、broker port 协作、artifact 文件生命周期 |
| `internal/task/work/fake.go` | Create | deterministic FakeManager |
| `internal/task/work/*_test.go` | Create | 类型、SQLite、Manager、Fake 测试 |
| `internal/store/task.go` | Modify | broker 行的 guarded cancel；不依赖 work |
| `internal/task/broker.go` | Modify | `Cancel` adapter surface；保留 broker 语义 |
| `internal/tools/taskctx.go` | Create | per-turn context；保留 `TaskManagerCtx` 兼容别名 |
| `internal/tools/workevent.go` | Create | typed work event callback context |
| `internal/pathjail/pathjail.go` | Create | canonical `WithinRootAbs`（symlink/Windows volume/case）|
| `internal/pathjail/pathjail_test.go` | Create | jail 单测（被 tools 与 work 共用）|
| `internal/tools/securepath.go` | Create | `withinRootAbs` 薄封装，委托 `pathjail.WithinRootAbs` |
| `internal/task/work/securepath.go` | Create | `SecureArtifactPath` 薄封装，委托 `pathjail.WithinRootAbs` |
| `internal/tools/task.go` | Create | `task_create/list/read/cancel` |
| `internal/tools/plan.go` | Create | `update_plan`、`checklist_*`、`todo_*` aliases |
| `internal/tools/gate.go` | Create | `task_gate_run` + Evidence |
| `internal/tools/artifact.go` | Create | authorize-before-I/O `artifact_read` |
| `internal/tools/permctx.go` | Modify | Plan fail-closed + force prompt |
| `internal/guard/mode.go` | Modify | `ModePlan`、独立 `Modes()` 与 cycle |
| `internal/agent/orchestrator/orchestrator.go` | Modify | per-turn 注入、mode-aware Runner cache、flush |
| `internal/agent/orchestrator/workevents.go` | Create | `work.Event` → `proto.ServerFrame` |
| `internal/proto/frame.go` | Modify | task/plan/checklist 帧与构造器 |
| `internal/api/http/ws.go` | Modify | per-connection Plan 状态、TurnOpts、共享 emit |
| `internal/api/http/chat.go` | Modify | SSE TurnOpts、thread/turn、共享 emit |
| `internal/cli/ssebackend.go` | Modify | client thread/turn ID；Plan 控制仍 unsupported |
| `internal/cli/backend.go`、`wsbackend.go` | Modify | typed frame 映射 |
| `internal/cli/tui/commands.go`、`permissions.go` | Modify | `/plan`、`/plan-off` |
| `internal/cli/tui/model.go`、`events.go`、`entries.go` | Modify | typed update cases/rendering |
| `internal/bootstrap/bootstrap.go` | Modify | work migration/manager/tools/adapter/janitor 装配 |

---

## Task 1: 定义 WorkTask 领域模型、事件与 port

**Files:**
- Create: `internal/task/work/types.go`
- Create: `internal/task/work/types_test.go`

- [ ] **Step 1: 写失败测试**

测试完整覆盖：终态不可转移；pending→running、running→completed 可转移；空 checklist 完成率为 0；2/4 done 为 50；`ClassificationFromExitCode(0/1/-1)` 分别为 pass/fail/error。

```go
package work

import "testing"

func TestDomainRules(t *testing.T) {
	if err := StatusCompleted.CanTransitionTo(StatusRunning); err == nil {
		t.Fatal("terminal status must reject transitions")
	}
	if err := StatusPending.CanTransitionTo(StatusRunning); err != nil {
		t.Fatal(err)
	}
	if got := (Checklist{Items: []ChecklistItem{
		{ID: 1, Status: ChecklistDone},
		{ID: 2, Status: ChecklistPending},
		{ID: 3, Status: ChecklistInProgress},
		{ID: 4, Status: ChecklistDone},
	}}).CompletionPct(); got != 50 {
		t.Fatalf("CompletionPct=%d want 50", got)
	}
	if ClassificationFromExitCode(0) != "pass" || ClassificationFromExitCode(1) != "fail" || ClassificationFromExitCode(-1) != "error" {
		t.Fatal("unexpected evidence classification")
	}
}
```

- [ ] **Step 2: 实现完整领域类型**

```go
package work

import (
	"context"
	"fmt"
	"time"
)

type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

func (s Status) IsTerminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCancelled
}

func (s Status) CanTransitionTo(next Status) error {
	if s.IsTerminal() {
		return fmt.Errorf("work: terminal status %q cannot transition to %q", s, next)
	}
	switch next {
	case StatusPending, StatusRunning, StatusCompleted, StatusFailed, StatusCancelled:
		return nil
	default:
		return fmt.Errorf("work: unknown target status %q", next)
	}
}

type ChecklistItemStatus string

const (
	ChecklistPending    ChecklistItemStatus = "pending"
	ChecklistInProgress ChecklistItemStatus = "in_progress"
	ChecklistDone       ChecklistItemStatus = "done"
)

type ChecklistItem struct {
	ID      int                 `json:"id"`
	Content string              `json:"content"`
	Status  ChecklistItemStatus `json:"status"`
}

type Checklist struct {
	Items []ChecklistItem `json:"items"`
}

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

func ClassificationFromExitCode(exitCode int) string {
	if exitCode == 0 {
		return "pass"
	}
	if exitCode > 0 {
		return "fail"
	}
	return "error"
}

type Artifact struct {
	ID         string `json:"id"`
	TaskID     string `json:"task_id"`
	Label      string `json:"label"`
	Summary    string `json:"summary"`
	ContentRef string `json:"content_ref"`
	Size       int64  `json:"size"`
	CreatedAt  int64  `json:"created_at"`
}

type TimelineEntry struct {
	At               time.Time `json:"at"`
	Kind             string    `json:"kind"`
	Summary          string    `json:"summary"`
	DetailArtifactID string    `json:"detail_artifact_id,omitempty"`
}

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

type CreateReq struct {
	Title    string
	Prompt   string
	ThreadID string
	TurnID   string
	Dispatch bool
}

type EventKind string

const (
	EventTaskUpdate      EventKind = "task_update"
	EventPlanUpdate      EventKind = "plan_update"
	EventChecklistUpdate EventKind = "checklist_update"
)

type Event struct {
	Kind      EventKind
	Task      *WorkTask
	TaskID    string
	Checklist Checklist
}

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

type Dispatcher interface {
	Submit(string, string, string) (string, error)
	Cancel(string) error
}
```

- [ ] **Step 3: 运行测试**

Run: `go test ./internal/task/work -run TestDomainRules -v`
Expected: PASS。

---

## Task 2: 建立 SQLite schema 与无死锁 Store

**Files:**
- Create: `internal/task/work/store.go`
- Create: `internal/task/work/store_test.go`

- [ ] **Step 1: 写失败测试**

测试必须包含以下四个 load-bearing case：

1. `Create` 后重读能看到 Manager 预置的第一条 `created` timeline。
2. DB 使用 `SetMaxOpenConns(1)`，`List` 在 1 秒 context deadline 内返回，不挂死。
3. `Transition` 同一 transaction 更新 status 与追加 timeline。
4. 两个 goroutine 更新不同 checklist item 后，两项均保留；`PatchChecklistItem` 不做 read-modify-write。

```go
func TestStoreCreatePersistsInitialTimeline(t *testing.T) {
	s := openTestStore(t)
	now := time.Now().Truncate(time.Second)
	w := &WorkTask{ID: "wt-1", Title: "x", Prompt: "p", Status: StatusPending, CreatedAt: now, UpdatedAt: now,
		Timeline: []TimelineEntry{{At: now, Kind: "created", Summary: "x"}}}
	require.NoError(t, s.Create(context.Background(), w))
	got, err := s.Get(context.Background(), w.ID)
	require.NoError(t, err)
	require.Len(t, got.Timeline, 1)
	assert.Equal(t, "created", got.Timeline[0].Kind)
}

func TestStoreListSingleConnectionDoesNotDeadlock(t *testing.T) {
	s := openTestStore(t)
	require.NoError(t, s.Create(context.Background(), &WorkTask{ID: "wt-1", Title: "x", Prompt: "p", Status: StatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := s.List(ctx, 10, "")
	require.NoError(t, err)
	require.Len(t, got, 1)
}
```

- [ ] **Step 2: 建表；migration 只由 `work.FromDB` 调用**

`store.go` 定义 `FromDB(db *sql.DB) (*Store, error)` 并执行以下 schema。不要改 `internal/store/store.go`。

```sql
CREATE TABLE IF NOT EXISTS task_work (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    prompt TEXT NOT NULL,
    status TEXT NOT NULL,
    thread_id TEXT NOT NULL DEFAULT '',
    turn_id TEXT NOT NULL DEFAULT '',
    broker_task_id TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_task_work_thread_created ON task_work(thread_id, created_at DESC);

CREATE TABLE IF NOT EXISTS task_work_checklist (
    task_id TEXT NOT NULL,
    item_id INTEGER NOT NULL,
    content TEXT NOT NULL,
    status TEXT NOT NULL,
    PRIMARY KEY(task_id, item_id),
    FOREIGN KEY(task_id) REFERENCES task_work(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS task_work_gates (
    task_id TEXT NOT NULL,
    gate TEXT NOT NULL,
    id TEXT NOT NULL,
    command TEXT NOT NULL,
    cwd TEXT NOT NULL,
    exit_code INTEGER NOT NULL,
    duration_ms INTEGER NOT NULL,
    classification TEXT NOT NULL,
    summary TEXT NOT NULL,
    log_artifact_id TEXT NOT NULL DEFAULT '',
    recorded_at INTEGER NOT NULL,
    PRIMARY KEY(task_id, gate),
    FOREIGN KEY(task_id) REFERENCES task_work(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS task_work_artifacts (
    id TEXT PRIMARY KEY,
    task_id TEXT NOT NULL,
    label TEXT NOT NULL,
    summary TEXT NOT NULL,
    content_ref TEXT NOT NULL,
    size INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY(task_id) REFERENCES task_work(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_task_work_artifacts_task_created ON task_work_artifacts(task_id, created_at);

CREATE TABLE IF NOT EXISTS task_work_timeline (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id TEXT NOT NULL,
    at INTEGER NOT NULL,
    kind TEXT NOT NULL,
    summary TEXT NOT NULL,
    detail_artifact_id TEXT NOT NULL DEFAULT '',
    FOREIGN KEY(task_id) REFERENCES task_work(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_task_work_timeline_task_seq ON task_work_timeline(task_id, seq);
```

- [ ] **Step 3: `Create` 在同一 transaction 持久化初始 timeline**

```go
func (s *Store) Create(ctx context.Context, w *WorkTask) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO task_work
		(id,title,prompt,status,thread_id,turn_id,broker_task_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, w.ID, w.Title, w.Prompt, string(w.Status), w.ThreadID, w.TurnID,
		w.BrokerTaskID, w.CreatedAt.Unix(), w.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("work: insert task: %w", err)
	}
	for _, item := range w.Checklist.Items {
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_work_checklist(task_id,item_id,content,status) VALUES(?,?,?,?)`,
			w.ID, item.ID, item.Content, string(item.Status)); err != nil {
			return err
		}
	}
	for _, entry := range w.Timeline {
		if _, err = tx.ExecContext(ctx, `INSERT INTO task_work_timeline(task_id,at,kind,summary,detail_artifact_id) VALUES(?,?,?,?,?)`,
			w.ID, entry.At.Unix(), entry.Kind, entry.Summary, entry.DetailArtifactID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

- [ ] **Step 4: `List` 用单条聚合查询；禁止 rows 打开时 N+1 `QueryRow`**

```go
func (s *Store) List(ctx context.Context, limit int, threadID string) ([]Summary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT w.id,w.title,w.status,w.thread_id,w.turn_id,w.created_at,w.updated_at,
		CASE WHEN COALESCE(c.total,0)=0 THEN 0 ELSE c.done*100/c.total END,
		COALESCE(g.gate_count,0)
	FROM task_work w
	LEFT JOIN (
		SELECT task_id,COUNT(*) AS total,SUM(CASE WHEN status='done' THEN 1 ELSE 0 END) AS done
		FROM task_work_checklist GROUP BY task_id
	) c ON c.task_id=w.id
	LEFT JOIN (
		SELECT task_id,COUNT(*) AS gate_count FROM task_work_gates GROUP BY task_id
	) g ON g.task_id=w.id
	WHERE (?='' OR w.thread_id=?)
	ORDER BY w.created_at DESC,w.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, threadID, threadID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Summary, 0)
	for rows.Next() {
		var item Summary
		var status string
		var createdAt, updatedAt int64
		if err := rows.Scan(&item.ID, &item.Title, &status, &item.ThreadID, &item.TurnID,
			&createdAt, &updatedAt, &item.Pct, &item.GateCount); err != nil {
			return nil, err
		}
		item.Status = Status(status)
		item.CreatedAt = time.Unix(createdAt, 0)
		item.UpdatedAt = time.Unix(updatedAt, 0)
		out = append(out, item)
	}
	return out, rows.Err()
}
```

- [ ] **Step 5: 状态与 timeline 使用一个 Store transaction API**

```go
func (s *Store) Transition(ctx context.Context, id string, next Status, kind, summary string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM task_work WHERE id=?`, id).Scan(&current); err != nil {
		return err
	}
	if err := Status(current).CanTransitionTo(next); err != nil {
		return err
	}
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `UPDATE task_work SET status=?,updated_at=? WHERE id=? AND status=?`,
		string(next), now, id, current)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("work: status changed concurrently")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_work_timeline(task_id,at,kind,summary) VALUES(?,?,?,?)`,
		id, now, kind, summary); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 6: checklist patch 用单条 guarded UPDATE；gate 用 `INSERT OR REPLACE`**

```go
func (s *Store) PatchChecklistItem(ctx context.Context, taskID string, itemID int, content string, status ChecklistItemStatus) error {
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `UPDATE task_work_checklist
		SET content=CASE WHEN ?='' THEN content ELSE ? END,
		    status=CASE WHEN ?='' THEN status ELSE ? END
		WHERE task_id=? AND item_id=?`, content, content, string(status), string(status), taskID, itemID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("work: checklist item not found")
	}
	_, err = s.db.ExecContext(ctx, `UPDATE task_work SET updated_at=? WHERE id=?`, now, taskID)
	return err
}

func (s *Store) RecordGate(ctx context.Context, taskID string, evidence Evidence) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO task_work_gates
		(task_id,gate,id,command,cwd,exit_code,duration_ms,classification,summary,log_artifact_id,recorded_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, taskID, evidence.Gate, evidence.ID, evidence.Command, evidence.Cwd,
		evidence.ExitCode, evidence.DurationMs, evidence.Classification, evidence.Summary,
		evidence.LogArtifactID, evidence.RecordedAt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO task_work_timeline(task_id,at,kind,summary) VALUES(?,?,?,?)`,
		taskID, evidence.RecordedAt, "gate", evidence.Gate+": "+evidence.Classification)
	if err != nil {
		return err
	}
	return tx.Commit()
}
```

`SetChecklist`、`AddChecklistItem`、`Get`、`PutArtifact`、`GetArtifact`、`ArtifactBytes`、`DeleteArtifactsBefore` 也必须使用 context-aware SQL，并逐个在 `store_test.go` 覆盖。`AddChecklistItem` 在 transaction 内以 `COALESCE(MAX(item_id),0)+1` 分配 ID；`DeleteArtifactsBefore` 先完整读取并关闭 `rows`，再执行 DELETE，不能在 `MaxOpenConns(1)` 下嵌套查询。

Manager 委托的两个剩余 Store 原子 API（`Create` 在 dispatch 失败时调 `AppendTimeline`，成功时调 `AttachBrokerTask`）签名与 SQL 骨架：

```go
// AppendTimeline 追加单条 timeline 并 bump updated_at（dispatch_failed 与后续
// broker-result 回写 hook 都走它）。不与 Create/Transition 共用事务——这两条
// 都是 Manager 在 Store 调用之后、针对已存在 task 的补记。
func (s *Store) AppendTimeline(ctx context.Context, taskID string, entry TimelineEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task_work_timeline(task_id,at,kind,summary,detail_artifact_id) VALUES(?,?,?,?,?)`,
		taskID, entry.At.Unix(), entry.Kind, entry.Summary, entry.DetailArtifactID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_work SET updated_at=? WHERE id=?`,
		entry.At.Unix(), taskID); err != nil {
		return err
	}
	return tx.Commit()
}

// AttachBrokerTask 把 broker task id 盖到 durable task 上；guarded 到恰好一行。
func (s *Store) AttachBrokerTask(ctx context.Context, taskID, brokerTaskID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE task_work SET broker_task_id=?,updated_at=? WHERE id=?`,
		brokerTaskID, time.Now().Unix(), taskID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("work: task not found")
	}
	return nil
}
```

`store_test.go` 增加 `TestStoreAppendTimelineAndAttachBroker`：`AppendTimeline` 后 `Get` 可见新行且 `updated_at` 前移；`AttachBrokerTask` 对不存在 ID 返回 error；对已存在 ID 重读 `BrokerTaskID` 一致。

- [ ] **Step 7: 运行测试**

Run: `go test ./internal/task/work -run 'TestStore' -v`
Expected: PASS，且 deadlock 测试在 deadline 前完成。

---

## Task 3: Manager、broker adapter 与 deterministic Fake

**Files:**
- Create: `internal/task/work/manager.go`
- Create: `internal/task/work/fake.go`
- Modify: `internal/store/task.go`
- Modify: `internal/task/broker.go`
- Create: `internal/task/work/manager_test.go`

- [ ] **Step 1: 先写测试**

覆盖：创建后重启恢复；`Finish/Cancel` 的状态与 timeline 同时出现；`dispatch=true` 记录 broker ID；dispatch 失败保留 durable task 并追加 `dispatch_failed`（且 `AppendTimeline` 错误向上传播，不被吞掉）；Fake `List` 按 `CreatedAt DESC, ID DESC`；编译期接口断言（`var _ ManagerLike = (*Manager)(nil)` 与 `(*FakeManager)(nil)`）。

另在 `internal/store` 补 `TestStoreCancelTask_GuardedUpdate`：`store.CancelTask` 对 pending/running 行命中（`RowsAffected==1`，无 error）、对 completed 行不命中（`RowsAffected==0` → error），且 `broker.Cancel` 只委托该 API、不自行 `UPDATE tasks`。

- [ ] **Step 2: 用可后绑定 port 保持 bootstrap 装配顺序**

```go
type DispatcherRef struct {
	mu sync.RWMutex
	d  Dispatcher
}

func (r *DispatcherRef) Bind(d Dispatcher) {
	r.mu.Lock()
	r.d = d
	r.mu.Unlock()
}

func (r *DispatcherRef) Submit(typ, input, parent string) (string, error) {
	r.mu.RLock()
	d := r.d
	r.mu.RUnlock()
	if d == nil {
		return "", errors.New("work: dispatcher unavailable")
	}
	return d.Submit(typ, input, parent)
}

func (r *DispatcherRef) Cancel(id string) error {
	r.mu.RLock()
	d := r.d
	r.mu.RUnlock()
	if d == nil {
		return errors.New("work: dispatcher unavailable")
	}
	return d.Cancel(id)
}

type BrokerAdapter struct {
	Broker interface {
		Submit(string, string, string) (string, error)
		Cancel(string) error
	}
}

func (a BrokerAdapter) Submit(typ, input, parent string) (string, error) {
	return a.Broker.Submit(typ, input, parent)
}

func (a BrokerAdapter) Cancel(id string) error {
	return a.Broker.Cancel(id)
}
```

- [ ] **Step 3: 为现有 broker 增加最小 cancel surface**

`internal/store/task.go` 增加 guarded update；`internal/task/broker.go` 只委托该 API，并复用已有 worktree 回收逻辑。禁止在 work.Manager 中直接 UPDATE `tasks`。

```go
func (s *Store) CancelTask(id string) error {
	result, err := s.DB.Exec(`UPDATE tasks SET status='cancelled',updated_at=?
		WHERE id=? AND status IN ('pending','running')`, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errors.New("task not cancellable or not found")
	}
	return nil
}

func (b *Broker) Cancel(id string) error {
	if err := b.store.CancelTask(id); err != nil {
		return err
	}
	b.createdWTMu.Lock()
	worktreeID, created := b.createdWT[id]
	delete(b.createdWT, id)
	b.createdWTMu.Unlock()
	if created && b.VCS != nil {
		_ = b.VCS.RemoveWorktree(worktreeID)
	}
	return nil
}
```

- [ ] **Step 4: Manager 使用 Store 原子 API**

`manager.go` 先定义 ID 生成器（`wt-`/`art-` 前缀；6 字节 crypto/rand，base32 无 padding，小写）。`work` 是唯一持有该 kernel 的包；`tools.newEvidenceID` 经导出的 `work.NewID("ev")` 复用，不另写一份 rand。

```go
// newID 返回 prefix + "-" + 6 字节 crypto/rand 的 base32（NoPadding，小写）。
// panic 只在 crypto/rand 不可用时触发（属于运行环境致命错误，不静默退化）。
func newID(prefix string) string {
	var b [6]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		panic("work: crypto/rand unavailable: " + err.Error())
	}
	return prefix + "-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// NewID 是 newID 的导出形式，供其它包（tools 的 evidence id）复用同一 kernel。
func NewID(prefix string) string { return newID(prefix) }
```

```go
type Manager struct {
	store      *Store
	dispatcher Dispatcher
	policy     ArtifactPolicy
}

func NewManager(store *Store, dispatcher Dispatcher, policy ArtifactPolicy) *Manager {
	return &Manager{store: store, dispatcher: dispatcher, policy: policy.withDefaults()}
}

func (m *Manager) Create(ctx context.Context, req CreateReq) (*WorkTask, error) {
	if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("work: title and prompt are required")
	}
	now := time.Now()
	task := &WorkTask{
		ID: newID("wt"), Title: req.Title, Prompt: req.Prompt, Status: StatusPending,
		ThreadID: req.ThreadID, TurnID: req.TurnID, CreatedAt: now, UpdatedAt: now,
		Timeline: []TimelineEntry{{At: now, Kind: "created", Summary: truncate(req.Title, 160)}},
	}
	if err := m.store.Create(ctx, task); err != nil {
		return nil, err
	}
	if req.Dispatch {
		brokerID, err := m.dispatcher.Submit("work_task", req.Prompt, task.ID)
		if err != nil {
			// dispatch 失败：durable task 已建好，必须把失败写进 timeline，且
			// AppendTimeline 自身的错误不能再被 `_ =` 吞掉（与约束 9 一致）。
			tlErr := m.store.AppendTimeline(ctx, task.ID, TimelineEntry{
				At: time.Now(), Kind: "dispatch_failed", Summary: truncate(err.Error(), 240),
			})
			if tlErr != nil {
				return task, fmt.Errorf("work: durable task %s created but dispatch failed: %w (timeline log also failed: %v)", task.ID, err, tlErr)
			}
			return task, fmt.Errorf("work: durable task %s created but dispatch failed: %w", task.ID, err)
		}
		if err := m.store.AttachBrokerTask(ctx, task.ID, brokerID); err != nil {
			return task, err
		}
		task.BrokerTaskID = brokerID
	}
	return m.store.Get(ctx, task.ID)
}

func (m *Manager) Start(ctx context.Context, id string) error {
	return m.store.Transition(ctx, id, StatusRunning, "started", "task started")
}

func (m *Manager) Finish(ctx context.Context, id string, status Status, note string) error {
	if status != StatusCompleted && status != StatusFailed {
		return errors.New("work: finish status must be completed or failed")
	}
	return m.store.Transition(ctx, id, status, "finished", truncate(note, 240))
}

func (m *Manager) Cancel(ctx context.Context, id, by string) (*WorkTask, error) {
	task, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if task.BrokerTaskID != "" {
		if err := m.dispatcher.Cancel(task.BrokerTaskID); err != nil {
			return nil, fmt.Errorf("work: cancel broker task: %w", err)
		}
	}
	if err := m.store.Transition(ctx, id, StatusCancelled, "cancelled", "cancelled by "+by); err != nil {
		return nil, err
	}
	return m.store.Get(ctx, id)
}

var _ ManagerLike = (*Manager)(nil)
```

`Finish` 和 `Cancel` 不得再调用 `UpdateStatus` 后另调 `AppendTimeline`。完整实现 `List/Read/SetChecklist/AddChecklistItem/PatchChecklistItem/RecordGate`，每个只委托一个对应 Store 原子 API，返回更新后的 task。

- [ ] **Step 5: Fake 按真实 Store 顺序返回**

`fake.go` 先给出 struct 与构造器（`NewFakeManager` 必须预初始化 `tasks` map，避免 nil-map 写 panic；不持有任何 `*sql.DB`，纯内存）：

```go
// FakeManager 是 work.ManagerLike 的确定性内存实现，用于 tools/orchestrator/bootstrap
// 测试，无需 SQLite。Create 时间由 Fake 自增，保证 List 顺序稳定。
type FakeManager struct {
	mu    sync.Mutex
	next  int64
	tasks map[string]*WorkTask
}

// NewFakeManager 返回一个空的 FakeManager（map 预初始化）。
func NewFakeManager() *FakeManager {
	return &FakeManager{tasks: make(map[string]*WorkTask)}
}

func (f *FakeManager) Create(_ context.Context, req CreateReq) (*WorkTask, error) {
	if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("work: title and prompt are required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	now := time.Unix(f.next, 0)
	task := &WorkTask{
		ID: newID("wt"), Title: req.Title, Prompt: req.Prompt, Status: StatusPending,
		ThreadID: req.ThreadID, TurnID: req.TurnID, CreatedAt: now, UpdatedAt: now,
		Timeline: []TimelineEntry{{At: now, Kind: "created", Summary: truncate(req.Title, 160)}},
	}
	if req.Dispatch {
		task.BrokerTaskID = newID("bk") // Fake 不真正投递，只模拟 id 占位
	}
	f.tasks[task.ID] = task
	// 返回拷贝，避免调用方修改内部状态。
	cp := *task
	return &cp, nil
}

func (f *FakeManager) List(_ context.Context, limit int, threadID string) ([]Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Summary, 0, len(f.tasks))
	for _, task := range f.tasks {
		if threadID != "" && task.ThreadID != threadID {
			continue
		}
		out = append(out, summaryOf(task))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	if limit <= 0 || limit > len(out) {
		limit = len(out)
	}
	return append([]Summary(nil), out[:limit]...), nil
}

var _ ManagerLike = (*FakeManager)(nil)
```

- [ ] **Step 6: 运行测试**

Run: `go test ./internal/task/work ./internal/task ./internal/store -run 'TestManager|TestFakeManager|TestBroker_Cancel|TestCancelTask' -v`
Expected: PASS。

---

## Task 4: Artifact quota、TTL、清理与安全路径

**Files:**
- Modify: `internal/task/work/manager.go`
- Create: `internal/task/work/artifact_test.go`
- Create: `internal/pathjail/pathjail.go`
- Create: `internal/pathjail/pathjail_test.go`
- Create: `internal/tools/securepath.go`
- Create: `internal/task/work/securepath.go`

- [ ] **Step 1: 写失败测试**

覆盖：超过 per-task quota 拒绝且不留文件；TTL 清理同时删 metadata 与 `.yanshi/artifacts` 文件；symlink 从 root 内指向 root 外被拒绝；Windows 路径比较使用 volume + case-insensitive 语义。

- [ ] **Step 2: 固化 policy**

```go
const (
	DefaultArtifactQuota   int64 = 64 << 20
	DefaultArtifactTTL           = 7 * 24 * time.Hour
	DefaultArtifactReadSize      = 64 << 10
	MaxArtifactReadSize          = 1 << 20
)

type ArtifactPolicy struct {
	QuotaBytes int64
	TTL        time.Duration
}

func (p ArtifactPolicy) withDefaults() ArtifactPolicy {
	if p.QuotaBytes <= 0 {
		p.QuotaBytes = DefaultArtifactQuota
	}
	if p.TTL <= 0 {
		p.TTL = DefaultArtifactTTL
	}
	return p
}
```

- [ ] **Step 3: artifact 写入先 quota，再原子落盘，再 metadata；失败回滚文件**

```go
func (m *Manager) WriteArtifact(ctx context.Context, taskID, label string, content []byte, root string) (Artifact, error) {
	used, err := m.store.ArtifactBytes(ctx, taskID)
	if err != nil {
		return Artifact{}, err
	}
	if used+int64(len(content)) > m.policy.QuotaBytes {
		return Artifact{}, fmt.Errorf("work: artifact quota exceeded: %d > %d", used+int64(len(content)), m.policy.QuotaBytes)
	}
	id := newID("art")
	dir := filepath.Join(root, ".yanshi", "artifacts", taskID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Artifact{}, err
	}
	path := filepath.Join(dir, id+".txt")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return Artifact{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return Artifact{}, err
	}
	artifact := Artifact{ID: id, TaskID: taskID, Label: label, Summary: summarizeArtifact(content),
		ContentRef: filepath.ToSlash(filepath.Join(".yanshi", "artifacts", taskID, id+".txt")),
		Size: int64(len(content)), CreatedAt: time.Now().Unix()}
	if err := m.store.PutArtifact(ctx, artifact); err != nil {
		_ = os.Remove(path)
		return Artifact{}, err
	}
	return artifact, nil
}
```

- [ ] **Step 4: TTL janitor 返回被删 ref，再安全删文件**

```go
func SweepArtifacts(ctx context.Context, store *Store, root string, before time.Time) error {
	refs, err := store.DeleteArtifactsBefore(ctx, before.Unix())
	if err != nil {
		return err
	}
	for _, ref := range refs {
		// SecureArtifactPath 是 work 包的薄封装（见 Step 5），委托 pathjail.WithinRootAbs，
		// 让 janitor 既能 canonical 化 ref 又不反向依赖 internal/tools。
		path, err := SecureArtifactPath(root, ref)
		if err != nil {
			continue
		}
		_ = os.Remove(path)
	}
	return nil
}

func StartArtifactJanitor(ctx context.Context, store *Store, root string, interval, ttl time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = SweepArtifacts(ctx, store, root, time.Now().Add(-ttl))
			}
		}
	}()
}
```

- [ ] **Step 5: root jail 规范化 symlink、Windows volume 与大小写**

canonical kernel 落在**新建** `internal/pathjail` 包（不依赖 `tools`/`work`/`proto`），导出 `WithinRootAbs`。`work.SecureArtifactPath` 与 `tools.withinRootAbs` 各自一行委托它，禁止任何复制实现。依赖方向：`tools → pathjail`、`work → pathjail`，**绝无 `work → tools`**。

```go
package pathjail

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

// WithinRootAbs 返回 candidate 解析（含 symlink eval）后的绝对路径，并保证它
// 严格位于 root（同样解析后）之内；否则返回 error。Windows 下额外比较 volume
// （EqualFold）并按大小写不敏感复核拼接结果，堵住盘符/大小写绕过。
func WithinRootAbs(root, candidate string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", err
	}
	candidateReal, err := filepath.EvalSymlinks(candidateAbs)
	if err != nil {
		return "", err
	}
	rootVolume := filepath.VolumeName(rootReal)
	candidateVolume := filepath.VolumeName(candidateReal)
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(rootVolume, candidateVolume) {
			return "", errors.New("pathjail: path is on a different volume")
		}
	} else if rootVolume != candidateVolume {
		return "", errors.New("pathjail: path is on a different volume")
	}
	rel, err := filepath.Rel(rootReal, candidateReal)
	if err != nil {
		return "", err
	}
	if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("pathjail: path escapes work root")
	}
	if runtime.GOOS == "windows" {
		joined := filepath.Clean(filepath.Join(rootReal, rel))
		if !strings.EqualFold(joined, filepath.Clean(candidateReal)) {
			return "", errors.New("pathjail: path escapes work root")
		}
	}
	return candidateReal, nil
}
```

两个薄封装（一行委托，禁止复制算法）：

```go
// internal/task/work/securepath.go
package work

import "github.com/x6nux/yanshi/internal/pathjail"

// SecureArtifactPath 把 artifact 的 content_ref（相对 root）解析并 jail 到 root 内。
func SecureArtifactPath(root, ref string) (string, error) {
	return pathjail.WithinRootAbs(root, filepath.Join(root, filepath.FromSlash(ref)))
}
```

```go
// internal/tools/securepath.go
package tools

import "github.com/x6nux/yanshi/internal/pathjail"

// withinRootAbs 是 tools 包内的薄封装，供 gate cwd、artifact_read 复用，
// 避免这些调用点直接 import pathjail 散落各处。
func withinRootAbs(root, candidate string) (string, error) {
	return pathjail.WithinRootAbs(root, candidate)
}
```

`pathjail_test.go` 覆盖：symlink 从 root 内指向 root 外被拒；Windows volume 不同被拒（带 `runtime.GOOS=="windows"` guard）；`rel==".."` 与 `rel="../x"` 都被拒；合法子路径返回 canonical 绝对路径。最终只保留这一个 canonical kernel，由 artifact janitor、`artifact_read`、gate cwd 复用。

- [ ] **Step 6: 运行测试**

Run: `go test ./internal/pathjail ./internal/task/work ./internal/tools -run 'TestArtifact|TestWithinRootAbs|TestSecureArtifactPath' -v`
Expected: PASS。

---

## Task 5: per-turn context 与 typed work-event callback

**Files:**
- Create: `internal/tools/taskctx.go`
- Create: `internal/tools/workevent.go`
- Create: `internal/tools/taskctx_test.go`

- [ ] **Step 1: 写失败测试**

测试两个 sibling context 的 thread/plan 值互不影响；nil manager 不覆盖 parent manager；event callback 收到一条 `work.Event`。

- [ ] **Step 2: 实现 context API；保留 `TaskManagerCtx` alias**

```go
package tools

import (
	"context"

	"github.com/x6nux/yanshi/internal/task/work"
)

type taskManagerKey struct{}
type planModeKey struct{}
type threadLinkKey struct{}

type ThreadLink struct {
	ThreadID string
	TurnID   string
}

type TaskManagerCtx = work.ManagerLike

func WithTaskManager(ctx context.Context, manager work.ManagerLike) context.Context {
	if manager == nil {
		return ctx
	}
	return context.WithValue(ctx, taskManagerKey{}, manager)
}

func TaskManagerFromContext(ctx context.Context) (TaskManagerCtx, bool) {
	manager, ok := ctx.Value(taskManagerKey{}).(work.ManagerLike)
	return manager, ok && manager != nil
}

func WithPlanMode(ctx context.Context, active bool) context.Context {
	return context.WithValue(ctx, planModeKey{}, active)
}

func PlanModeActive(ctx context.Context) bool {
	active, _ := ctx.Value(planModeKey{}).(bool)
	return active
}

func WithThreadLink(ctx context.Context, threadID, turnID string) context.Context {
	return context.WithValue(ctx, threadLinkKey{}, ThreadLink{ThreadID: threadID, TurnID: turnID})
}

func ThreadLinkFromContext(ctx context.Context) (ThreadLink, bool) {
	link, ok := ctx.Value(threadLinkKey{}).(ThreadLink)
	return link, ok
}
```

```go
package tools

import (
	"context"

	"github.com/x6nux/yanshi/internal/task/work"
)

type workEventCallbackKey struct{}

type WorkEventCallback func(work.Event)

func WithWorkEventCallback(ctx context.Context, callback WorkEventCallback) context.Context {
	if callback == nil {
		return ctx
	}
	return context.WithValue(ctx, workEventCallbackKey{}, callback)
}

func WorkEventCallbackFromContext(ctx context.Context) WorkEventCallback {
	callback, _ := ctx.Value(workEventCallbackKey{}).(WorkEventCallback)
	return callback
}

func EmitWorkEvent(ctx context.Context, event work.Event) {
	if callback := WorkEventCallbackFromContext(ctx); callback != nil {
		callback(event)
	}
}
```

- [ ] **Step 3: 运行测试**

Run: `go test ./internal/tools -run 'TestTaskContext|TestWorkEventCallback' -v`
Expected: PASS；可配合 `go test -race` 验证 sibling context 无共享写。

---

## Task 6: durable task 工具与强制审批取消

**Files:**
- Create: `internal/tools/task.go`
- Create: `internal/tools/task_test.go`
- Modify: `internal/tools/permctx.go`
- Modify: `internal/tools/guard_test.go`

- [ ] **Step 1: 测试 `task_create/list/read/cancel`**

使用 `work.FakeManager`；`task_create` 不传 thread/turn 时读取 context；H3 cancel 测试必须 `json.Unmarshal` 输出并断言 `task.status == cancelled`，不能只做字符串包含。

```go
func TestTaskCancelReturnsStructuredTask(t *testing.T) {
	manager := work.NewFakeManager()
	task, err := manager.Create(context.Background(), work.CreateReq{Title: "x", Prompt: "p"})
	require.NoError(t, err)
	ctx := WithPermissionCallback(WithProfile(WithTaskManager(context.Background(), manager), guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
	}), func(req PermissionRequest) PermissionDecision {
		require.True(t, req.ForcePrompt)
		return PermissionAllow
	})
	out, err := runTool(ctx, NewTaskTools().Cancel, `{"id":"`+task.ID+`"}`)
	require.NoError(t, err)
	var payload struct {
		Task work.WorkTask `json:"task"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &payload))
	assert.Equal(t, work.StatusCancelled, payload.Task.Status)
}
```

`task_test.go` 必须新增 `runTool` helper（仓库当前无此 helper；禁止引用不存在的工具）。它接收一个 `StreamFunc`（即 `func(ctx, argsJSON) (string, error)`）直接调用，省去构造完整 `GuardedTool`/ADK 节点：

```go
// runTool 调用 StreamFunc kernel 并返回其 (string, error)，供 task/plan/gate 工具
// 单测在真实 context 上直接驱动 kernel，无需经 ADK ToolsNode。
func runTool(ctx context.Context, kernel StreamFunc, argsJSON string) (string, error) {
	return kernel(ctx, argsJSON)
}
```

`StreamFunc` 是 `internal/tools` 既有类型（`func(ctx context.Context, argsJSON string) (string, error)`）；若 kernel 以 `SyncStream` 包装存储，测试直接传未包装的函数即可。`task_create` 不传 thread/turn 时读取 context 的断言也用它驱动。

- [ ] **Step 2: 工具实现统一 manager/context helper**

```go
func requireTaskManager(ctx context.Context) (work.ManagerLike, error) {
	manager, ok := TaskManagerFromContext(ctx)
	if !ok {
		return nil, errors.New("tools: task manager unavailable")
	}
	return manager, nil
}

func taskCreate(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Title    string `json:"title"`
		Prompt   string `json:"prompt"`
		Dispatch bool   `json:"dispatch"`
	}
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	link, _ := ThreadLinkFromContext(ctx)
	task, err := manager.Create(ctx, work.CreateReq{Title: args.Title, Prompt: args.Prompt,
		ThreadID: link.ThreadID, TurnID: link.TurnID, Dispatch: args.Dispatch})
	if err != nil {
		return "", err
	}
	EmitWorkEvent(ctx, work.Event{Kind: work.EventTaskUpdate, Task: task})
	return toJSON(struct {
		Task *work.WorkTask `json:"task"`
	}{Task: task}), nil
}

func taskCancel(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	task, err := manager.Cancel(ctx, args.ID, "agent")
	if err != nil {
		return "", err
	}
	EmitWorkEvent(ctx, work.Event{Kind: work.EventTaskUpdate, Task: task})
	return toJSON(struct {
		Task *work.WorkTask `json:"task"`
	}{Task: task}), nil
}
```

`task_list` 默认用当前 thread filter；显式 `all=true` 才传空 filter。`task_read` 只按 ID 返回完整 task。四个工具全部通过 `NewGuardedTool + SyncStream` 构造。

- [ ] **Step 3: `forcePromptTools` 位于 static allow 之前**

**整体替换 `internal/tools/permctx.go` 当前 `Authorize` 函数体（行 153–196）。** 不是打补丁、不是在旧函数外再包一层——旧实现（allowlist → guard.Check → callback 三段式，无 Plan 检查、无 force-prompt 概念）整段删除，换成下面这段。同时给 `PermissionRequest`（permctx.go:14–18）加 `ForcePrompt bool` 字段，并新增 `forcePromptTools` map；复用 `PlanToolAllowed`（Task 10 Step 3 导出）、`ProfileFromContext`、`permissionCallback`/`allowlistFrom`/`allowKey` 等已有 helper，不重写。

```go
type PermissionRequest struct {
	Tool        string
	Args        string
	Reason      string
	ForcePrompt bool
}

var forcePromptTools = map[string]struct{}{
	"task_cancel": {},
}

func Authorize(ctx context.Context, action guard.Action, argsJSON string) error {
	profile, ok := ProfileFromContext(ctx)
	if !ok {
		return &DenyErr{Reason: "no permission profile in context"}
	}
	if PlanModeActive(ctx) && !PlanToolAllowed(action.Tool) {
		return &DenyErr{Reason: "tool not available in plan mode"}
	}
	if _, forced := forcePromptTools[action.Tool]; forced {
		// force-prompt 工具永远走 callback：不查 session allowlist、不受
		// Tools.Allow=["*"] 影响，且 always_allow 只对"本次调用"放行、不 record
		// 进 allowlist——下次同一 task_cancel 仍必须 prompt（约束 5）。
		ask, hasCallback := permissionCallback(ctx)
		if !hasCallback {
			return &DenyErr{Reason: "tool requires explicit approval"}
		}
		request := PermissionRequest{Tool: action.Tool, Args: argsJSON,
			Reason: "tool requires explicit approval", ForcePrompt: true}
		switch ask(request) {
		case PermissionAllow, PermissionAlwaysAllow:
			// 故意不调 allowlist.record：force-prompt 工具每次都要问。
			return nil
		default:
			return &DenyErr{Reason: request.Reason}
		}
	}
	if allowlist := allowlistFrom(ctx); allowlist != nil && allowlist.allows(allowKey(action)) {
		return nil
	}
	decision := guard.New().Check(profile, action)
	if decision.Allowed {
		return nil
	}
	ask, hasCallback := permissionCallback(ctx)
	if !hasCallback {
		return &DenyErr{Reason: decision.Reason}
	}
	switch ask(PermissionRequest{Tool: action.Tool, Args: argsJSON, Reason: decision.Reason}) {
	case PermissionAllow:
		return nil
	case PermissionAlwaysAllow:
		if allowlist := allowlistFrom(ctx); allowlist != nil {
			allowlist.record(allowKey(action))
		}
		return nil
	default:
		return &DenyErr{Reason: decision.Reason}
	}
}
```

- [ ] **Step 4: 运行测试**

Run: `go test ./internal/tools -run 'TestTask|TestAuthorize_ForcePrompt' -v`
Expected: wildcard profile 下仍触发 callback；无 callback 时 cancel 被拒绝；连续两次以 `PermissionAlwaysAllow` 应答，第二次仍触发 callback（allowlist 不短路 force-prompt）。

---

## Task 7: update_plan、checklist 与 todo aliases

**Files:**
- Create: `internal/tools/plan.go`
- Create: `internal/tools/plan_test.go`

- [ ] **Step 1: 写失败测试**

覆盖 `update_plan` 整体替换、add、patch、list；`todo_write/add/update/list` 的 `Info().Name` 分别是 alias 名，但执行委托同一 kernel；空 rows 发出非 nil 的 empty checklist event。

- [ ] **Step 2: 实现一个 kernel，多名称包装**

```go
func updatePlan(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		TaskID string               `json:"task_id"`
		Rows   []work.ChecklistItem `json:"rows"`
	}
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	task, err := manager.SetChecklist(ctx, args.TaskID, work.Checklist{Items: append([]work.ChecklistItem(nil), args.Rows...)})
	if err != nil {
		return "", err
	}
	EmitWorkEvent(ctx, work.Event{Kind: work.EventPlanUpdate, TaskID: task.ID, Checklist: task.Checklist})
	return toJSON(struct {
		TaskID   string         `json:"task_id"`
		Checklist work.Checklist `json:"checklist"`
	}{TaskID: task.ID, Checklist: task.Checklist}), nil
}

func aliasGuardedTool(name, display, desc string, timeout time.Duration, p *schema.ParamsOneOf, kernel StreamFunc) *GuardedTool {
	return NewGuardedTool(name, display, desc, timeout, p, kernel)
}
```

`checklist_write` 与 `todo_write` 复用 `updatePlan` kernel；`checklist_add/todo_add` 复用 add kernel；`checklist_update/todo_update` 复用 patch kernel；`checklist_list/todo_list` 复用 read kernel。Alias 不能把 `*GuardedTool` 当作 `StreamFunc`；传入的是 `SyncStream(kernelFunc)`。

- [ ] **Step 3: 运行测试**

Run: `go test ./internal/tools -run 'TestUpdatePlan|TestChecklist|TestTodoAlias' -v`
Expected: PASS。

---

## Task 8: task_gate_run 与 Evidence

**Files:**
- Create: `internal/tools/gate.go`
- Create: `internal/tools/gate_test.go`

- [ ] **Step 1: 写失败测试**

使用不含管道/重定向的命令：Windows `cmd /c exit 0` 由 tool 的 `env:"cmd"` 选择；Unix `sh -c true` 由 `env:"sh"`。不要使用 `yes | head`，因为 `|` 必须被 guard 拒绝。覆盖 cwd 越界、输出 spill 到 Artifact、非零 exit 仍返回 Evidence、`RecordGate` error 向上传播。

- [ ] **Step 2: cwd 先经共享 secure root jail，再执行**

```go
func runTaskGate(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		TaskID string `json:"task_id"`
		Gate   string `json:"gate"`
		Command string `json:"command"`
		Cwd    string `json:"cwd"`
		Timeout int   `json:"timeout"`
		Env    string `json:"env"`
	}
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	root := WorkRootFromContext(ctx)
	if root == "" {
		root = "."
	}
	cwd := args.Cwd
	if cwd == "" {
		cwd = root
	} else if !filepath.IsAbs(cwd) {
		cwd = filepath.Join(root, cwd)
	}
	cwd, err := withinRootAbs(root, cwd)
	if err != nil {
		return "", err
	}
	if err := Authorize(ctx, guard.Action{Tool: "task_gate_run", Shell: args.Command,
		FS: guard.FSWant{Op: "read", Paths: []string{cwd}}}, argsJSON); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	timeout := 120 * time.Second
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := shellCommand(runCtx, args.Env, args.Command)
	command.Dir = cwd
	started := time.Now()
	output, runErr := command.CombinedOutput()
	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	evidence := work.Evidence{ID: work.NewID("ev"), Gate: args.Gate, Command: args.Command, Cwd: cwd,
		ExitCode: exitCode, DurationMs: time.Since(started).Milliseconds(),
		Classification: work.ClassificationFromExitCode(exitCode), RecordedAt: time.Now().Unix()}
	if len(output) > SpillThreshold {
		artifact, err := manager.WriteArtifact(ctx, args.TaskID, "gate-log", output, root)
		if err != nil {
			return "", err
		}
		evidence.LogArtifactID = artifact.ID
		evidence.Summary = artifact.Summary
	} else {
		evidence.Summary = summarizeGateOutput(output)
	}
	if err := manager.RecordGate(ctx, args.TaskID, evidence); err != nil {
		return "", err
	}
	task, err := manager.Read(ctx, args.TaskID)
	if err != nil {
		return "", err
	}
	EmitWorkEvent(ctx, work.Event{Kind: work.EventTaskUpdate, Task: task})
	return toJSON(struct {
		Evidence work.Evidence `json:"evidence"`
	}{Evidence: evidence}), nil
}
```

注意 `shellCommand` 当前为 `tools` 包 package-private（`shell_run` 工具使用），`task_gate_run` 同在 `tools` 包故直接复用——这是**有意的范围收窄**：gate 复用同一 cmd 构造逻辑（env 选择、argv 拆分），但 gate 的命令面更窄（不接受管道/重定向，由 guard 的 shell metachar 规则拒绝），因此不把 `shellCommand` 导出、也不为 gate 单独复制一份。shell metachar 校验由 `Authorize(... Shell: command)` 的 guard 规则执行。`RecordGate` 禁止 `_ = manager.RecordGate(...)`。

- [ ] **Step 3: 运行测试**

Run: `go test ./internal/tools -run TestTaskGateRun -v`
Expected: PASS。

---

## Task 9: artifact_read 的 authorize-before-I/O

**Files:**
- Create: `internal/tools/artifact.go`
- Create: `internal/tools/artifact_test.go`

- [ ] **Step 1: 写失败测试**

覆盖：无 FS read 权限时不调用 `os.Open` test hook；symlink escape 被拒绝；offset/limit 分页；limit 超过 1 MiB clamp；返回 summary/ref/page，不把大文件整段塞回模型。

- [ ] **Step 2: 明确权限顺序**

```go
func readArtifact(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		ID     string `json:"id"`
		Offset int64  `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := ParseArgs(argsJSON, &args); err != nil {
		return "", err
	}
	manager, err := requireTaskManager(ctx)
	if err != nil {
		return "", err
	}
	artifact, err := manager.ReadArtifact(ctx, args.ID)
	if err != nil {
		return "", err
	}
	root := WorkRootFromContext(ctx)
	if root == "" {
		root = "."
	}
	lexical := filepath.Join(root, filepath.FromSlash(artifact.ContentRef))
	if err := Authorize(ctx, guard.Action{Tool: "artifact_read",
		FS: guard.FSWant{Op: "read", Paths: []string{lexical}}}, argsJSON); err != nil {
		return "", err
	}
	canonical, err := withinRootAbs(root, lexical)
	if err != nil {
		return "", err
	}
	if err := Authorize(ctx, guard.Action{Tool: "artifact_read",
		FS: guard.FSWant{Op: "read", Paths: []string{canonical}}}, argsJSON); err != nil {
		return "", err
	}
	limit := args.Limit
	if limit <= 0 {
		limit = work.DefaultArtifactReadSize
	}
	if limit > work.MaxArtifactReadSize {
		limit = work.MaxArtifactReadSize
	}
	file, err := os.Open(canonical)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if args.Offset < 0 {
		return "", errors.New("tools: offset must be non-negative")
	}
	if _, err := file.Seek(args.Offset, io.SeekStart); err != nil {
		return "", err
	}
	buffer := make([]byte, limit)
	n, readErr := file.Read(buffer)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return "", readErr
	}
	return toJSON(struct {
		Artifact work.Artifact `json:"artifact"`
		Offset   int64         `json:"offset"`
		Next     int64         `json:"next_offset"`
		EOF      bool          `json:"eof"`
		Content  string        `json:"content"`
	}{Artifact: artifact, Offset: args.Offset, Next: args.Offset + int64(n),
		EOF: args.Offset+int64(n) >= artifact.Size, Content: string(buffer[:n])}), nil
}
```

第一次 `Authorize` 发生在 `withinRootAbs` 的 `EvalSymlinks` 和 `os.Open` 之前；第二次针对 canonical path，防止 symlink 将已批准 lexical path 指向 root 外。绝不先 `os.ReadFile` 再授权。

- [ ] **Step 3: 运行测试**

Run: `go test ./internal/tools -run TestArtifactRead -v`
Expected: PASS。

---

## Task 10: Plan mode 枚举、只读白名单与 permission resolution

**Files:**
- Modify: `internal/guard/mode.go`
- Modify: `internal/guard/mode_test.go`
- Modify: `internal/tools/permctx.go`
- Modify: `internal/api/http/ws.go`

- [ ] **Step 1: 写失败测试**

断言：`Modes()` 包含 plan；`CycleMode` 永不返回 plan；从 plan cycle 返回 default；Plan context 下 `fs_write/shell_run/task_cancel` 拒绝；`task_create/update_plan/fs_read/artifact_read` 允许继续静态 guard；`resolvePermissionMode` 在 `ModePlan` 返回 deny+resolved；force prompt 不走 YOLO/Auto auto-resolve。

- [ ] **Step 2: Modes 与 cycle 使用不同列表**

```go
const (
	ModeDefault    PermissionMode = "default"
	ModeAllowEdits PermissionMode = "allow-edits"
	ModeYOLO       PermissionMode = "yolo"
	ModeAuto       PermissionMode = "auto"
	ModePlan       PermissionMode = "plan"
)

var allModes = []PermissionMode{ModeDefault, ModeAllowEdits, ModeAuto, ModeYOLO, ModePlan}
var cycleOrder = []PermissionMode{ModeDefault, ModeAllowEdits, ModeAuto, ModeYOLO}

func Modes() []PermissionMode {
	return append([]PermissionMode(nil), allModes...)
}

func CycleMode(current PermissionMode) PermissionMode {
	if current == "" || current == ModePlan {
		return ModeDefault
	}
	for index, mode := range cycleOrder {
		if mode == current {
			return cycleOrder[(index+1)%len(cycleOrder)]
		}
	}
	return ModeDefault
}
```

`NormalizeMode` 增加 `"plan"`；`ModeLabel(ModePlan)` 返回 `"plan"`。

- [ ] **Step 3: Plan runtime 白名单是第二层防线**

```go
var planAllowedTools = map[string]struct{}{
	"fs_read": {}, "fs_list": {},
	"memory_search": {}, "memory_recall": {},
	"vcs_log": {}, "vcs_diff": {},
	"time_now": {}, "summarize": {}, "analysis": {},
	"task_create": {}, "task_list": {}, "task_read": {},
	"update_plan": {}, "checklist_write": {}, "checklist_add": {},
	"checklist_update": {}, "checklist_list": {},
	"todo_write": {}, "todo_add": {}, "todo_update": {}, "todo_list": {},
	"artifact_read": {},
}

// PlanToolAllowed 是 Plan mode 工具白名单的唯一真值。runtime Authorize（本包）
// 与 orchestrator 的 filterPlanTools（跨包 tools.PlanToolAllowed）共用它，
// 杜绝两份白名单漂移。
func PlanToolAllowed(name string) bool {
	_, ok := planAllowedTools[name]
	return ok
}
```

- [ ] **Step 4: `resolvePermissionMode` 明确处理 Plan 与 force prompt**

```go
func resolvePermissionMode(ctx context.Context, cs connSession,
	models map[string]model.BaseChatModel, req tools.PermissionRequest) (tools.PermissionDecision, bool) {
	mode, threshold := cs.perm.get()
	if req.ForcePrompt {
		return tools.PermissionDeny, false
	}
	switch mode {
	// Plan mode 是只读：任何需回调的写操作（fs_write/shell_run/task_cancel/…）
	// 一律 deny 且 resolved=true（不再 prompt）。force-prompt 工具已在上面
	// req.ForcePrompt 分支拦截（同样不 resolved，回 Authorize 的 DenyErr）。
	// 这条与 Task 6 Authorize 的 PlanToolAllowed 是两层独立防线，互不依赖。
	case guard.ModePlan:
		return tools.PermissionDeny, true
	case guard.ModeYOLO:
		return tools.PermissionAllow, true
	case guard.ModeAllowEdits:
		if guard.IsEditTool(req.Tool) {
			return tools.PermissionAllow, true
		}
		return tools.PermissionDeny, false
	case guard.ModeAuto:
		if threshold <= 0 {
			threshold = guard.DefaultAutoThreshold
		}
		score, ok := assessRisk(ctx, models, cs, req)
		if ok && score <= threshold {
			return tools.PermissionAllow, true
		}
		return tools.PermissionDeny, false
	default:
		return tools.PermissionDeny, false
	}
}
```

WS permission callback 在调用 `resolvePermissionMode` 前也显式检查 `req.ForcePrompt`，force-prompt 时直接创建 `permission_request`，避免未来 resolver 改动绕过审批。

- [ ] **Step 5: 运行测试**

Run: `go test ./internal/guard ./internal/tools ./internal/api/http -run 'Test.*Plan|Test.*ForcePrompt' -v`
Expected: PASS。

---

## Task 11: mode-aware Runner cache 与 per-turn 注入

**Files:**
- Modify: `internal/agent/orchestrator/orchestrator.go`
- Create: `internal/agent/orchestrator/workevents.go`
- Modify: `internal/agent/orchestrator/orchestrator_test.go`

- [ ] **Step 1: 写失败测试**

用 `einollm.NewFakeModelWithMessages` 与两个具名 `GuardedTool` 构造 Orchestrator。测试：agent turn runner 可见全工具；plan runner 只见 plan-safe 工具；cache 中 `{sameModel,agent}` 与 `{sameModel,plan}` 是不同 runner；`FlushRunners` 后下一次返回新 runner；两个并发 `TurnOpts` 注入不同 thread/turn，不串值。测试不要调用不存在的 `o.Model()`；直接保留传给 `Config.Model` 的变量。

- [ ] **Step 2: Config/TurnOpts 增加稳定字段；Orchestrator 不加状态 setter**

```go
type Config struct {
	Model           model.BaseChatModel
	Tools           []BaseTool
	Instruction     string
	SkillMetaPrompt string
	MaxIters        int
	Profile         guard.PermissionProfile
	VCSScope        tools.VCSScope
	WorkRoot        string
	Compaction      CompactionConfig
	TaskManager     work.ManagerLike
}

type TurnOpts struct {
	Model          model.BaseChatModel
	ThinkingEffort string
	OutputSchema   json.RawMessage
	PlanMode       bool
	ThreadID       string
	TurnID         string
	EmitWorkFrame  func(proto.ServerFrame)
}

type runnerToolMode uint8

const (
	runnerModeAgent runnerToolMode = iota
	runnerModePlan
)

type runnerCacheKey struct {
	model model.BaseChatModel
	mode  runnerToolMode
}
```

`Orchestrator` 增加不可变 `rawModel model.BaseChatModel`、`taskManager work.ManagerLike`；删除计划旧稿中的 `planActive`、`threadLink`、`SetPlanActive`、`SetThreadLink`。`runners sync.Map` 只存 `runnerCacheKey → *adk.Runner`。**同时删除 `o.runner` 字段**（约束 14）：`New()` 不再预建 default runner，所有 turn 入口（见 Step 4）经 `runnerFor(o.rawModel, false)` 懒取。`New()` 里原先构造 `o.runner` 的那段代码删掉，`Query/Events/EventsWithHistory` 里对 `o.runner` 的引用全部换成 `runnerFor` 调用。

- [ ] **Step 3: runnerFor 根据 mode 过滤 ADK 注册工具**

```go
func (o *Orchestrator) runnerFor(chatModel model.BaseChatModel, plan bool) *adk.Runner {
	mode := runnerModeAgent
	if plan {
		mode = runnerModePlan
	}
	key := runnerCacheKey{model: chatModel, mode: mode}
	if cached, ok := o.runners.Load(key); ok {
		return cached.(*adk.Runner)
	}
	registered := o.agentTools
	if plan {
		registered = filterPlanTools(o.agentTools)
	}
	names := collectToolNames(registered)
	agent, err := adk.NewChatModelAgent(context.Background(), &adk.ChatModelAgentConfig{
		Model: wrapCompaction(chatModel, o.compaction), Instruction: o.instruction, MaxIterations: o.maxIters,
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: registered, UnknownToolsHandler: unknownToolHandler(names),
		}},
		Handlers: []adk.ChatModelAgentMiddleware{newMessageRecorder()},
	})
	if err != nil {
		// 不再回退到 o.runner（该字段已删除，约束 14）。agent 构建失败属于
		// 配置级致命错误（New 时已用 rawModel 验证过一次可构建），运行期
		// 不应发生；返回 nil 且不缓存，让下一 turn 重试构建而非吞掉错误。
		// 调用方拿到 nil 会在 .Run 处 panic，这比静默用错工具集的 runner
		// 更早暴露问题。
		return nil
	}
	runner := adk.NewRunner(context.Background(), adk.RunnerConfig{Agent: agent, EnableStreaming: true})
	actual, _ := o.runners.LoadOrStore(key, runner)
	return actual.(*adk.Runner)
}

func filterPlanTools(all []tool.BaseTool) []tool.BaseTool {
	out := make([]tool.BaseTool, 0, len(all))
	for _, candidate := range all {
		info, err := candidate.Info(context.Background())
		if err == nil && info != nil && tools.PlanToolAllowed(info.Name) {
			out = append(out, candidate)
		}
	}
	return out
}

func (o *Orchestrator) FlushRunners() {
	o.runners.Range(func(key, value any) bool {
		o.runners.Delete(key)
		return true
	})
}
```

`PlanToolAllowed` 已在 Task 10 Step 3 导出（`tools.PlanToolAllowed`），本步 `filterPlanTools` 直接复用，runtime Authorize 也用同一个，避免两份白名单漂移。

`runnerFor` 签名变为双参 `(chatModel model.BaseChatModel, plan bool)`，因此 `orchestrator_test.go` 里现有调用 `TestEventsWithHistoryOpts_RunnerForIsMemoized`（当前 `o.runnerFor(extra)` 单参）必须同步改为 `o.runnerFor(extra, false)`；并新增一条断言：`o.runnerFor(extra, true)` 与 `o.runnerFor(extra, false)` 是**不同** runner（plan/agent 两套工具集，cache key 不同）。

- [ ] **Step 4: 所有 turn 注入由一个 helper 完成**

```go
func (o *Orchestrator) withTurnContext(ctx context.Context, opts TurnOpts) context.Context {
	ctx = tools.WithProfile(ctx, o.profile)
	ctx = tools.WithWorkRoot(ctx, o.workRoot)
	ctx = tools.WithTaskManager(ctx, o.taskManager)
	ctx = tools.WithPlanMode(ctx, opts.PlanMode)
	ctx = tools.WithThreadLink(ctx, opts.ThreadID, opts.TurnID)
	if opts.EmitWorkFrame != nil {
		ctx = tools.WithWorkEventCallback(ctx, func(event work.Event) {
			opts.EmitWorkFrame(workEventFrame(event))
		})
	}
	if o.vcsScope.VCS != nil {
		ctx = tools.WithVCS(ctx, o.vcsScope)
	}
	return o.bindSubAgentRunner(ctx)
}
```

`withTurnContext` 是唯一注入点；orchestrator 里**恰好 4 处** turn 入口必须改写为"先 `withTurnContext`、再 `runnerFor(selectedModel, plan)`"，逐一对号入座（不得遗漏，也不得新增第五处）：

1. **`Query(ctx, userMessage)`** — `selectedModel = o.rawModel`、`plan = false`：`runner := o.runnerFor(o.rawModel, false); return runner.Query(ctx, ...)`。原 `o.runner.Query(...)` 删除。
2. **`Events(ctx, query)`** — 同上 `o.runnerFor(o.rawModel, false)`；原 `o.runner.Query(...)` 删除。
3. **`EventsWithHistory(ctx, messages)`** — 同上 `o.runnerFor(o.rawModel, false)`；原 `o.runner.Run(...)` 删除。
4. **`EventsWithHistoryOpts(ctx, messages, opts)`** — `selectedModel = opts.Model`，nil 时回退 `o.rawModel`；`plan = opts.PlanMode`：`runner := o.runnerFor(selectedModel, opts.PlanMode); return runner.Run(ctx, messages, runOpts...)`。原 `runner := o.runner; if opts.Model != nil { runner = o.runnerFor(opts.Model) }` 整段删除。

`o.rawModel` 由 `Config.Model` 在 `New()` 时存入；`o.runner` 字段不存在，故任何"回退 default runner"都无从引用。`runSubAgentTurn`（Step 5）是第 5 个调用 `EventsWithHistoryOpts` 的点，但它通过 sub-orchestrator 走，不直接碰 `runnerFor`。

- [ ] **Step 5: sub-agent 继承 plan/thread，不扩大工具面**

`runSubAgentTurn` 中真实调用改为：

```go
	link, _ := tools.ThreadLinkFromContext(ctx)
	iter := sub.EventsWithHistoryOpts(subCtx, []*schema.Message{schema.UserMessage(prompt)}, TurnOpts{
		// Model 留空（nil）：sub-agent 仍用它自身配置的 model（sub.rawModel，
		// 即构造 nested orchestrator 时传入的 o.model），不从父 turn 继承
		// per-turn model。/model 切换只影响父 orchestrator 的 runner cache，
		// 不向下传递一个不同的 BaseChatModel 指针。
		PlanMode: tools.PlanModeActive(ctx),
		ThreadID: link.ThreadID,
		TurnID:   link.TurnID,
	})
```

父 context 已携带 `TaskManager` 与 work-event callback；nested orchestrator 的 nil manager 不覆盖 parent value。Plan sub-agent 同样构建过滤后的 Plan Runner（`runnerFor(sub.rawModel, true)`）。注释明确：sub-agent event 流仍走现有 `SubAgentEmit`；durable work event 走独立 typed callback，二者不可混用。

- [ ] **Step 6: 运行测试**

Run: `go test ./internal/agent/orchestrator -run 'Test.*Runner|Test.*TurnContext|Test.*SubAgent.*Plan' -v`
Expected: PASS；`FlushRunners` 后按 mode 重建不同工具列表。

---

## Task 12: 统一 proto 帧与 orchestrator typed-event mapping

**Files:**
- Modify: `internal/proto/frame.go`
- Modify: `internal/proto/frame_test.go`
- Create: `internal/agent/orchestrator/workevents.go`
- Modify: `internal/cli/backend.go`
- Modify: `internal/cli/wsbackend.go`

- [ ] **Step 1: 写 round-trip 与 nil rows 测试**

```go
func TestNewPlanUpdateNilRowsIsNonNilEmptyChecklist(t *testing.T) {
	frame := NewPlanUpdate("wt-1", nil)
	require.NotNil(t, frame.Checklist)
	assert.Empty(t, frame.Checklist.Items)
	_, data := frame.SSEEvent()
	assert.Contains(t, string(data), `"type":"plan_update"`)
}
```

- [ ] **Step 2: ServerFrame 增加共享字段和构造器**

**依赖方向约束（防环）：** 本步让 `proto.ServerFrame` 嵌入 `*work.WorkTask` / `*work.Checklist` / `[]work.ChecklistItem`，即 `proto → work` 单向依赖。`work` 包**严禁**反向 import `internal/proto`——否则成环。`work` 需要参与帧语义时，只产出 `work.Event`（纯领域类型），由 orchestrator 的 `workEventFrame`（Step 3）单向映射成 `proto.ServerFrame`。同理 `proto` 不得 import `tools`/`orchestrator`。实现期若发现 `work` 里有 `import ".../internal/proto"`，视为破坏约束，必须改走 mapper。

```go
// 加到 proto.ServerFrame：
Task      *work.WorkTask `json:"task,omitempty"`
TaskID    string         `json:"task_id,omitempty"`
Checklist *work.Checklist `json:"checklist,omitempty"`

func NewTaskUpdate(task *work.WorkTask) ServerFrame {
	return ServerFrame{Type: "task_update", Task: task}
}

func NewPlanUpdate(taskID string, rows []work.ChecklistItem) ServerFrame {
	checklist := &work.Checklist{Items: append([]work.ChecklistItem(nil), rows...)}
	return ServerFrame{Type: "plan_update", TaskID: taskID, Checklist: checklist}
}

func NewChecklistUpdate(taskID string, rows []work.ChecklistItem) ServerFrame {
	checklist := &work.Checklist{Items: append([]work.ChecklistItem(nil), rows...)}
	return ServerFrame{Type: "checklist_update", TaskID: taskID, Checklist: checklist}
}
```

`ServerFrame` 文档与 `SSEEvent` 注释把三个新类型列入统一词表。

- [ ] **Step 3: orchestrator 是唯一 event→frame mapper**

```go
package orchestrator

import (
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/task/work"
)

func workEventFrame(event work.Event) proto.ServerFrame {
	switch event.Kind {
	case work.EventPlanUpdate:
		return proto.NewPlanUpdate(event.TaskID, event.Checklist.Items)
	case work.EventChecklistUpdate:
		return proto.NewChecklistUpdate(event.TaskID, event.Checklist.Items)
	default:
		return proto.NewTaskUpdate(event.Task)
	}
}
```

- [ ] **Step 4: CLI 映射保持 typed data**

`StreamEvent` 增加 `Task *work.WorkTask`、`TaskID string`、`Checklist *work.Checklist`；`toStreamEvent` 直接复制：

```go
Task: f.Task,
TaskID: f.TaskID,
Checklist: f.Checklist,
```

不要在 TUI 收到 plan frame 后自行发 `task_read`；frame 已携带完整渲染数据。

- [ ] **Step 5: 运行测试**

Run: `go test ./internal/proto ./internal/agent/orchestrator ./internal/cli -run 'Test.*Update|TestToStreamEvent' -v`
Expected: PASS。

---

## Task 13: WS 与 SSE 使用同一个 per-turn emit contract

**Files:**
- Modify: `internal/api/http/ws.go`
- Create: `internal/api/http/ws_plan_test.go`
- Modify: `internal/api/http/chat.go`
- Modify: `internal/api/http/chat_test.go`
- Modify: `internal/cli/ssebackend.go`
- Modify: `internal/cli/ssebackend_test.go`

- [ ] **Step 1: 写真实 `httptest.NewServer` 测试**

WS test 直接使用当前包的 `Server`、`httptest.NewServer(s.Handler())` 和 websocket dial；不要引用不存在的 `startTestServer`。SSE test POST 一次触发 `update_plan` 的 FakeModel tool call，断言 body 同时出现 `event: plan_update` 与 `event: tool_result`。

- [ ] **Step 2: WS 模式状态仍 per connection，mode 边界 cancel+flush**

`connSession` 增加 `transientThreadID string`，初始化为随机 transport ID。`applySetMode` 签名从无返回值改为返回 `(oldMode, newMode guard.PermissionMode)`，完整实现：

```go
// applySetMode normalizes a set_mode frame and writes it to the live permission
// state, returning the mode BEFORE and AFTER so callers can detect Plan-boundary
// crossings. Called from BOTH the reader goroutine (switch lands mid-turn) and
// the main loop (canonical re-apply + status echo). Mode update is conditional on
// a recognizable mode string (unknown value leaves mode unchanged); the threshold
// is sticky — a prior value is kept unless the frame overrides, defaulting when 0.
func (cs *connSession) applySetMode(cf proto.ClientFrame) (oldMode, newMode guard.PermissionMode) {
	oldMode, auto := cs.perm.get()
	mode := oldMode
	if m, ok := guard.NormalizeMode(cf.Mode); ok {
		mode = m
	}
	if cf.AutoThreshold > 0 {
		auto = cf.AutoThreshold
	} else if auto == 0 {
		auto = guard.DefaultAutoThreshold
	}
	cs.perm.set(mode, auto)
	return oldMode, mode
}
```

`ws.go` 现有两处 `cs.applySetMode(cf)` 调用方（reader goroutine 与 main-loop 重放）必须同步改写。reader 跨 Plan 边界检测：

```go
oldMode, newMode := cs.applySetMode(cf)
if (oldMode == guard.ModePlan) != (newMode == guard.ModePlan) {
	cancelTurn()
	o.FlushRunners()
}
```

main-loop 的那处（canonical re-apply + status echo）不需要边界动作，丢弃返回即可：`cs.applySetMode(cf)` 仍合法（多返回值在 statement context 自动丢弃），但为可读性写 `_, _ = cs.applySetMode(cf)`。

这不是共享 PlanActive setter。切换发生在连接状态；正在运行的旧工具集 turn 被取消，下一 user turn 才按新 `TurnOpts.PlanMode` 启动。history 不清空。

- [ ] **Step 3: WS user turn 构造完整 TurnOpts**

```go
mode, _ := cs.perm.get()
threadID := cs.sessionID
if threadID == "" {
	threadID = cs.transientThreadID
}
opts := orchestrator.TurnOpts{
	Model:          cs.selectModel(models),
	ThinkingEffort: cs.thinking,
	OutputSchema:   cf.OutputSchema,
	PlanMode:       mode == guard.ModePlan,
	ThreadID:       threadID,
	TurnID:         fmt.Sprintf("%s:%d", threadID, cs.turns+1),
	EmitWorkFrame:  conn.write,
}
```

保留原 `ClassifyEventsWithUsage(... conn.write(f))`。typed work frames不是解析 tool_result 生成，而是工具调用时经 `EmitWorkFrame` 直接写入同一连接。

- [ ] **Step 4: SSE request 携带 client thread/turn，并接同形 callback**

`chat.go` request body 增加：

```go
ThreadID string `json:"thread_id,omitempty"`
TurnID   string `json:"turn_id,omitempty"`
```

构造 opts 时：

```go
opts.PlanMode = false
opts.ThreadID = req.ThreadID
opts.TurnID = req.TurnID
opts.EmitWorkFrame = func(frame proto.ServerFrame) {
	// fl 在 ResponseWriter 不实现 http.Flusher 时为 nil；回调也可能在 SSE
	// 已写完 NewDone 之后被触发（tool 在收尾后才同步 emit）。两情形都 nil-guard，
	// 避免向已关闭/无 flusher 的响应再写。顺序：本回调在 tool 执行的同 goroutine
	// 内同步调用，与 ClassifyEventsWithUsage 的逐 chunk write 共用同一 writer/flusher，
	// 因此帧的相对顺序由 emit 调用点天然保证（先工具 emit work frame，再 ADK
	// 流出 tool_result）。
	if fl != nil {
		writeSSEFrame(w, fl, frame)
	}
}
```

这使 WS 与 SSE 共用 orchestrator typed callback；删除任何 `maybeEmitWorkFrame` 或仅在 WS 解析 JSON 的分支。

- [ ] **Step 5: sseBackend 持有稳定 thread ID 与 turn counter**

```go
type sseBackend struct {
	baseURL string
	client *http.Client
	histMu sync.Mutex
	history []schema.Message
	threadID string
	turns uint64
	cancelMu sync.Mutex
	cancelCurrent context.CancelFunc
}

func newSSEBackend(baseURL string) *sseBackend {
	return &sseBackend{baseURL: baseURL, client: &http.Client{}, threadID: newClientThreadID()}
}
```

`Send` 在 `histMu` 下递增 turns，并把 `thread_id`、`turn_id=fmt.Sprintf("%s:%d", threadID, turns)` 放入 POST body。`SendFrame(set_mode)` 仍返回 `ErrSSEControlUnsupported`；不伪造服务端 Plan 状态。

- [ ] **Step 6: 运行测试**

Run: `go test ./internal/api/http ./internal/cli -run 'Test.*Plan|Test.*TaskUpdate|Test.*SSE.*Update|Test.*Thread' -v`
Expected: WS/SSE 都收到 typed frame；两个 WS 连接 mode/thread 不串线。

---

## Task 14: TUI `/plan`、`/plan-off` 与完整 applyEvent cases

**Files:**
- Modify: `internal/cli/tui/commands.go`
- Modify: `internal/cli/tui/permissions.go`
- Modify: `internal/cli/tui/model.go`
- Modify: `internal/cli/tui/events.go`
- Modify: `internal/cli/tui/entries.go`
- Modify: `internal/cli/tui/commands_test.go`
- Modify: `internal/cli/tui/model_test.go`

- [ ] **Step 1: 写失败测试**

使用真实 `newTestModel(t)`（位于 `view_test.go`）。覆盖：`cmdPlan` 清 threshold；`cmdPlanOff` 恢复前 mode/threshold；SSE 下两个命令追加 local error 且 fake session 没收到 frame；`applyEvent` 三个 update case 直接渲染，不调用 `SendFrame(task_read)`；nil/empty checklist 不 panic。

- [ ] **Step 2: model 保存进入 Plan 前状态**

```go
prePlanMode      guard.PermissionMode
prePlanThreshold int
```

不要增加独立 `planActive bool`；`m.permMode == guard.ModePlan` 是唯一客户端真值。

- [ ] **Step 3: 添加命令；SSE 本地 fail**

```go
func cmdPlan(m model, _ []string) (tea.Model, tea.Cmd) {
	if m.sess.Mode() == "sse" {
		m.entries = append(m.entries, errorEntry{text: "plan mode requires the WebSocket transport; SSE is stateless"})
		m.refresh()
		return m, nil
	}
	if m.permMode != guard.ModePlan {
		m.prePlanMode = m.permMode
		if m.prePlanMode == "" {
			m.prePlanMode = guard.ModeDefault
		}
		m.prePlanThreshold = m.autoThreshold
	}
	m.permMode = guard.ModePlan
	m.autoThreshold = 0
	return m.sendMode()
}

func cmdPlanOff(m model, _ []string) (tea.Model, tea.Cmd) {
	if m.sess.Mode() == "sse" {
		m.entries = append(m.entries, errorEntry{text: "plan mode requires the WebSocket transport; SSE is stateless"})
		m.refresh()
		return m, nil
	}
	mode := m.prePlanMode
	if mode == "" || mode == guard.ModePlan {
		mode = guard.ModeDefault
	}
	m.permMode = mode
	m.autoThreshold = m.prePlanThreshold
	m.prePlanMode = ""
	m.prePlanThreshold = 0
	return m.sendMode()
}
```

`commandTable` 添加 `/plan` 与 `/plan-off`。`cmdMode` 需要新增 Plan 路由（现有 `commands.go:273` 的 `cmdMode` 不认识 plan），完整改写要点：

```go
func cmdMode(m model, args []string) (tea.Model, tea.Cmd) {
	// /mode plan == /plan（复用入口，保证两条路径行为一致）
	if len(args) > 0 && args[0] == "plan" {
		return cmdPlan(m, args[1:])
	}
	// 从 Plan 经 /mode <other> 离开：先清 pre-plan 快照，避免后续 /plan-off
	// 把 mode 恢复成一个已不再有效的旧值。
	if m.permMode == guard.ModePlan && len(args) > 0 && args[0] != "plan" {
		m.prePlanMode = ""
		m.prePlanThreshold = 0
	}
	if len(args) == 0 {
		// ... 现有 picker 分支（Modes() 已含 plan）保持不变 ...
	}
	// ... 现有 NormalizeMode / threshold / m.permMode=pm / sendMode 分支保持不变 ...
}
```

`cmdPlan` 在设置 `prePlanMode` 时不覆盖已是 Plan 的情况（见 Step 3 的 `cmdPlan`）。`sendMode` 在 Plan 下发送 threshold 0，不把旧 Auto threshold 带到 server。

- [ ] **Step 4: 完整 update entries 与 applyEvent cases**

```go
type taskUpdateEntry struct {
	task work.WorkTask
}

func (e taskUpdateEntry) render(_ int, _ spinner.Model) string {
	return fmt.Sprintf("  task %s  %s  %d%%\n    %s\n\n", e.task.ID, e.task.Status,
		e.task.Checklist.CompletionPct(), e.task.Title)
}

type planUpdateEntry struct {
	taskID string
	rows   []work.ChecklistItem
}

func (e planUpdateEntry) render(_ int, _ spinner.Model) string {
	var builder strings.Builder
	builder.WriteString("  plan " + e.taskID + "\n")
	if len(e.rows) == 0 {
		builder.WriteString("    (empty plan)\n\n")
		return builder.String()
	}
	for _, row := range e.rows {
		marker := "[ ]"
		if row.Status == work.ChecklistDone {
			marker = "[x]"
		} else if row.Status == work.ChecklistInProgress {
			marker = "[~]"
		}
		builder.WriteString(fmt.Sprintf("    %s %d. %s\n", marker, row.ID, row.Content))
	}
	builder.WriteString("\n")
	return builder.String()
}
```

在 `applyEvent` switch 中加入完整 case：

```go
case "task_update":
	m.flushAssistant()
	if ev.Task != nil {
		m.entries = append(m.entries, taskUpdateEntry{task: *ev.Task})
	}
case "plan_update", "checklist_update":
	m.flushAssistant()
	rows := []work.ChecklistItem(nil)
	if ev.Checklist != nil {
		rows = append(rows, ev.Checklist.Items...)
	}
	m.entries = append(m.entries, planUpdateEntry{taskID: ev.TaskID, rows: rows})
```

这三个 case 不发送请求、不调用 `task_read`、不依赖后续 tool_result。

- [ ] **Step 5: footer/picker**

`permModeText`（`permissions.go:64`）的 switch 必须新增 Plan 分支，否则 Plan 模式下 footer 会落到 `default` 显示 "manual mode"，与实际不符：

```go
func (m model) permModeText() string {
	switch m.permMode {
	case guard.ModeDefault, "":
		return "manual mode"
	case guard.ModeAllowEdits:
		return "edit mode"
	case guard.ModeAuto:
		t := m.autoThreshold
		if t == 0 {
			t = guard.DefaultAutoThreshold
		}
		return fmt.Sprintf("auto ≤%d", t)
	case guard.ModeYOLO:
		return "bypass permissions"
	case guard.ModePlan:          // 新增：Plan 模式独立标签
		return "plan mode"
	default:
		return "manual mode"
	}
}
```

mode picker 因 `guard.Modes()` 包含 plan 而显示它；Shift+Tab 使用 `CycleMode`（`cycleOrder` 不含 plan），因此不会进入 plan。

- [ ] **Step 6: 运行测试**

Run: `go test ./internal/cli/tui -run 'TestCmdPlan|TestApplyEvent.*Update|TestModePicker' -v`
Expected: PASS。

---

## Task 15: bootstrap 装配、artifact janitor 与全链路验收

**Files:**
- Modify: `internal/bootstrap/bootstrap.go`
- Modify: `internal/bootstrap/bootstrap_test.go`
- Modify: `internal/api/http/server.go` only if test injection needs existing Config field extension

- [ ] **Step 1: 写失败 bootstrap 测试**

使用现有 `bootstrap.Build(Options{FakeModel:true})`、`httptest.NewServer(app.Server.Handler)`；断言 `app.WorkTasks != nil`，work tables 可读，artifact startup sweep 移除过期项，现有 broker route 仍工作。不要引用不存在的 `startTestServer`。

- [ ] **Step 2: 在 store 后构造 work store/manager；store 包不反向依赖 work**

```go
workStore, err := work.FromDB(st.DB)
if err != nil {
	st.Close()
	return nil, fmt.Errorf("bootstrap: migrate work tasks: %w", err)
}
dispatcherRef := &work.DispatcherRef{}
workManager := work.NewManager(workStore, dispatcherRef, work.ArtifactPolicy{})
```

在构造工具时追加：

```go
for _, taskTool := range tools.NewTaskTools().Tools() {
	allTools = append(allTools, taskTool)
}
for _, planTool := range tools.NewPlanTools().Tools() {
	allTools = append(allTools, planTool)
}
allTools = append(allTools, tools.NewTaskGateTool(), tools.NewArtifactReadTool())
```

`orchConfig.TaskManager = workManager`。`App` 增加 `WorkTasks work.ManagerLike` 与 package-private `workStore *work.Store`（供 janitor shutdown 生命周期）。**关键生命周期声明：`workStore` 不持有独立 `*sql.DB`**——它由 `work.FromDB(st.DB)` 构造，只是复用 shared `st.DB` 的句柄；`App.Close()` 只对 shared DB 调一次 `st.Close()`，`workStore` 没有独立 `Close`，也不在 shutdown 时再关一次 DB。janitor 通过 `ctx.Done()` 停止（app lifecycle context cancel），无需显式 Close workStore。

- [ ] **Step 3: broker 最后构造后绑定 adapter**

沿用真实顺序：HTTP 后创建 `broker := task.NewBroker(...)`，然后：

```go
dispatcherRef.Bind(work.BrokerAdapter{Broker: broker})
```

Manager 不直接依赖 `*task.Broker`；broker 仍负责 transport。Task API 注册和 sweeper 保持原位置。

- [ ] **Step 4: `.yanshi/artifacts` TTL cleanup 与现有 spillover sweep 并存**

创建 app lifecycle context 后：

```go
if err := work.SweepArtifacts(ctx, workStore, workRoot, time.Now().Add(-work.DefaultArtifactTTL)); err != nil {
	fmt.Fprintf(os.Stderr, "yanshi: artifact sweep failed: %v\n", err)
}
work.StartArtifactJanitor(ctx, workStore, workRoot, 6*time.Hour, work.DefaultArtifactTTL)
```

保留 `tools.Sweep(workRoot)` 清 `.yanshi/tmp/spillover`。两者职责不同：spillover 启动全清；durable artifacts 只清超过 TTL 的 metadata+文件。Artifact 清理错误软降级到 stderr，不阻止应用启动。

- [ ] **Step 5: 全链路测试矩阵**

按顺序运行：

1. `go test ./internal/task/work -v`
2. `go test ./internal/tools -run 'TestTask|TestPlan|TestChecklist|TestGate|TestArtifact' -v`
3. `go test ./internal/guard ./internal/agent/orchestrator -run 'Test.*Plan|Test.*Runner|Test.*Turn' -v`
4. `go test ./internal/proto ./internal/api/http ./internal/cli -run 'Test.*Update|Test.*Plan|Test.*SSE|Test.*WS' -v`
5. `go test ./internal/cli/tui -run 'TestCmdPlan|TestApplyEvent.*Update' -v`
6. `go test ./internal/bootstrap -run TestBuild -v`
7. `go test ./...`
8. `go vet ./...`

实现阶段才运行这些命令；编写本计划时不运行。

---

## Acceptance Traceability

| Review/需求 | 落点 |
|---|---|
| 1 per-turn ThreadLink/PlanActive | Task 5 `taskctx.go`；Task 11 `TurnOpts/withTurnContext`；明确删除 setters |
| 2 Store.List 单连接死锁 | Task 2 单条 aggregate JOIN；无 rows-open N+1 |
| 3 Flush 对工具集有效 | Task 11 `{model,mode}` cache + `filterPlanTools`；Task 13 mode boundary flush |
| 4 cancel 强制审批 | Task 6 `forcePromptTools` 在 static allow 前；SSE 无 callback fail-closed |
| 5 WS/SSE typed rich frames | Task 12 mapper；Task 13 两 transport 同一 `EmitWorkFrame` |
| 6 SSE 无 set_mode | Task 13 保持 unsupported；Task 14 本地错误 |
| 7 artifact authorize 顺序 | Task 9 lexical Authorize → EvalSymlinks → canonical Authorize → Open |
| 8 symlink/Windows jail | Task 4 `EvalSymlinks` + volume + `EqualFold` |
| 9 gate cwd/error/metachar test | Task 8 jail；RecordGate error return；测试不用 pipe |
| 10 initial Timeline 持久化 | Task 2 `Create` transaction 插 timeline |
| 11 Finish/Cancel 原子 | Task 2 `Transition`；Task 3 Manager 单调用 |
| 12 checklist 并发丢更新 | Task 2 单条 UPDATE WHERE |
| 13 Fake deterministic | Task 3 CreatedAt DESC + ID DESC |
| 14 interface assertion | Task 3 real/Fake 两个 `var _ ManagerLike` |
| 15 store 不依赖 work | File Structure/Task 15 由 bootstrap 调 `work.FromDB` |
| 16 cmdPlan threshold/off mode | Task 14 threshold=0；恢复 pre-plan mode/threshold |
| 17 Modes/cycle + resolver | Task 10 独立 allModes/cycleOrder；Plan resolver deny |
| 18 假 helper 修正 | Task 11 保留 model 变量；Task 13/15 `httptest.NewServer` |
| 19 applyEvent 三 case | Task 14 给出完整 switch cases |
| 20 path/proto/sub-agent/cleanup/M7/R7 | Tasks 4/5/8/11/12/15 |
| DT1 broker 协作 | Task 3 Dispatcher port + BrokerAdapter；dispatch opt-in |
| DT2 Evidence 全字段 | Task 1 类型；Task 8 command/cwd/exit/duration/classification/summary/artifact |
| DT3 quota/TTL | Task 4 policy/write/janitor；Task 9 paged read |
| Plan history 连续 | 已决策约束 3；Task 13 mode switch 不清 history |

### v2 三评审合并必修（18 项）

| # | v2 必修 | 落点 |
|---|---|---|
| 1 | pathjail 包提取（work 不反向依赖 tools） | 已决策约束 11；职责边界；File Structure；Task 4 Step 5 `WithinRootAbs` + 两薄封装 |
| 2 | 7 helper 全部定义 | Task 3 Step 4 `newID`/`NewID`；末尾"Shared helpers"列其余 6 个签名+契约 |
| 3 | Store API 显式清单 | Task 2 Step 6 `AppendTimeline`/`AttachBrokerTask`；Task 3 Step 1 `TestStoreCancelTask_GuardedUpdate` |
| 4 | `runTool` helper 声明 | Task 6 Step 1 在 `task_test.go` 新增 |
| 5 | Authorize 整体替换声明 | Task 6 Step 3 顶部"整体替换 permctx.go:153–196"+ ForcePrompt 字段 |
| 6 | force-prompt allowlist 不短路 | Task 6 Step 3 删 allowlist 短路；每次必 prompt；Step 4 测试断言 |
| 7 | runnerFor 双参 + Query/Events 改写 | 已决策约束 14；Task 11 Step 2 删 `o.runner`；Step 3 nil 回退；Step 4 列 4 入口；测试同步双参 |
| 8 | applySetMode 签名变更 | Task 13 Step 2 给 `(oldMode,newMode)` 实现 + 两处调用方改写 |
| 9 | cmdMode plan 路由 | Task 14 Step 3 `cmdMode` 改写（plan→cmdPlan；离 plan 清字段） |
| 10 | permModeText + resolver 注释 | Task 14 Step 5 switch 加 `ModePlan`；Task 10 Step 4 Plan 分支注释 |
| 11 | proto→work anti-cycle | 职责边界；Task 12 Step 2 前约束（work 严禁 import proto） |
| 12 | EmitWorkFrame SSE nil check + 顺序 | Task 13 Step 4 `if fl != nil` + 同 goroutine 顺序保证 |
| 13 | dispatch_failed AppendTimeline 检查错误 | Task 3 Step 4 改为检查 `tlErr`；已决策约束 9 |
| 14 | workStore 生命周期声明 | Task 15 Step 2 "不持有独立 *sql.DB" |
| 15 | sub-agent model 选择 | Task 11 Step 5 Model 留空，用 sub.rawModel |
| 16 | NewFakeManager 构造器源码 | Task 3 Step 5 完整 struct + `NewFakeManager()` |
| 17 | C1 跨批 drift 对齐 | 末尾"C1 adapter 落地指引"小节 |
| 18 | task_gate_run 文档 | Task 8 "复用 package-private shellCommand，有意范围收窄" |

---

## 仍保留的待决策点

以下不阻塞 A2，执行时必须记录最终选择，不得静默扩 scope：

1. **broker terminal result 回写 WorkTask：**A2 只实现 submit/cancel 协作。worker `RecordResult` 自动映射到 WorkTask timeline/status 可作为后续 adapter hook；当前不改 broker result 语义。
2. **Artifact policy 是否配置化：**A2 使用代码默认值 64 MiB/7 天/6 小时 janitor；后续可加入 `config.yaml` 字段，但本批不扩大 config schema。
3. **SSE Plan mode：**A2 明确不支持，TUI 本地报错。若未来需要，应在每个 POST body 增加显式 `mode`，不能模拟双向 control frame。
4. **`task_create{dispatch:true}` 的失败恢复：**A2 保留 durable task 并记 `dispatch_failed`；后续可增加 `task_dispatch_retry`，本批不自动重试以避免重复 Submit。

已不再待决策：`/plan-off` 恢复进入前 mode；Plan 不加入 Shift+Tab cycle；Plan 允许 `task_create`；cancel 永远 force prompt；WS/SSE rich frame 必须对称。

---

## Shared helpers（除 `newID`/`NewID` 外的 6 个）

`newID(prefix)` 已在 Task 3 Step 4 给出（`work` 包，crypto/rand 6 字节 base32）。以下 6 个 helper 在本计划内首次出现，均需在实现期落地于各自自然包，签名与行为契约固定，不得改名或换语义：

1. **`func truncate(s string, max int) string`** — 包 `work`（manager.go）。按 **rune** 计数（非字节）：`utf8.RuneCountInString(s) <= max` 时原样返回；否则取前 `max` 个 rune 并追加 `"…"`。`max <= 0` 视为 0（返回 `"…"` 或空，实现择一并写测试）。绝不截断半个 rune。用于 timeline summary（160/240）。

2. **`func summaryOf(t *WorkTask) Summary`** — 包 `work`（fake.go / manager.go 共用）。把 `*WorkTask` 投影成 `Summary`：`ID/Title/Status/ThreadID/TurnID` 直接复制；`Pct = t.Checklist.CompletionPct()`；`GateCount = len(t.Gates)`；`CreatedAt/UpdatedAt` 原样。`t == nil` 不允许（调用方保证非 nil）。Fake 与真实 Manager.List 都用它，保证两路径 Summary 字段一致。

3. **`func summarizeArtifact(content []byte) string`** — 包 `work`（manager.go WriteArtifact 用）。取 `content` 第一行（`\n` 前）trim 空白；空 → `"(empty)"`；含 NUL 字节 → `"(binary)"`；否则截到 120 rune（复用 `truncate` 逻辑但前缀无 `"…"` 时直接截）。绝不把整段大文件塞进 summary。

4. **`func summarizeGateOutput(output []byte) string`** — 包 `tools`（gate.go 用，output 已判定 `< SpillThreshold`）。取尾部最后 N 行（实现取最后 ~512 字节内的完整行），trim；空输出 → `"(no output)"`；非空截到 160 rune。与 `summarizeArtifact` 不同：gate 关心命令末尾输出（错误通常在尾部）。

5. **`func (work) NewID(prefix string) string`** — 包 `work`（已决策，Task 3 Step 4）。`tools` 的 evidence id 经 `work.NewID("ev")` 复用，不在 `tools` 重写 rand。注意：Task 8 gate.go 调用点是 `work.NewID("ev")`，不是 `newEvidenceID()`。

6. **`func newClientThreadID() string`** — 包 `cli`（ssebackend.go 构造器用）。返回 `"sse-" + 6 字节 crypto/rand 的 hex`，作为 SSE client 在缺失服务端 session 时的稳定 thread 标识。与 `work.NewID` 算法同源但独立（cli 不 import work 仅为 id），属可接受的最小重复；若后续去重，提取到 `internal/idgen`，不在本批做。

---

## C1 adapter 落地指引（跨批 drift 对齐）

A2 定义了 `work.Dispatcher` port 与 `work.BrokerAdapter`，并新增 `store.CancelTask`。C1（broker/ACP 相关批次）若同期改动下列任一接触面，必须与本计划对齐，否则 adapter 编译失败或语义漂移：

- **broker 公开面：** `BrokerAdapter` 只依赖 2 个方法 `Submit(typ, input, parent string) (string, error)` 与 `Cancel(id string) error`。C1 若重命名 `task.Broker.Submit`、改其签名、或把 Cancel 移到别的 receiver，必须同步更新 `work.BrokerAdapter`（Task 3 Step 2）与 Task 3 Step 3 的 `b.Cancel`。
- **`tasks` 表 schema：** A2 的 `store.CancelTask` 是一条 guarded `UPDATE tasks SET status='cancelled' WHERE id=? AND status IN ('pending','running')`。C1 若给 `tasks` 加非空列或改 `status` 取值集合，须保证这条 UPDATE 仍命中（或改 CancelTask 适配），并重跑 `TestStoreCancelTask_GuardedUpdate`。
- **Dispatcher port 收窄原则：** A2 故意只暴露 Submit/Cancel 两方法，C1 若要让 worker `RecordResult` 回写 WorkTask，应新增**独立**的 adapter hook（见"仍保留的待决策点 1"），不要扩 `Dispatcher` 接口、也不要让 Manager 读 broker store——保持"领域不碰传输"的边界。
- **装配顺序：** `bootstrap.Build` 里 `dispatcherRef.Bind(work.BrokerAdapter{Broker: broker})` 必须在 broker 构造之后（Task 15 Step 3）。C1 若移动 broker 构造点，相应移动 Bind 调用。

冲突时以"port 最窄、Manager 不读 broker store、装配顺序 store→work→tools→orchestrator→http→broker→Bind"为准。

---

## Self-Review

- [x] 唯一组合根仍是 `bootstrap.Build`；`internal/store` 不导入 `work`。
- [x] durable WorkTask 与 broker transport 明确分层，并通过 port 复用而非重写。
- [x] `ThreadID/TurnID/PlanMode` 只在 `TurnOpts`/context；不存在共享 Orchestrator setter 或 data race 字段。
- [x] Plan Runner 的 ADK 工具注册列表真正缩减；runtime guard 是第二层防线；flush 后按新 mode 重建。
- [x] `task_cancel` 在 wildcard profile、YOLO、Auto 下都不能绕过显式审批；SSE fail-closed。
- [x] SQLite `List` 无 rows-open N+1；timeline/status 同事务；checklist patch 是 guarded single UPDATE；gate 是 `INSERT OR REPLACE`。
- [x] initial timeline 在 Create transaction 中持久化；Fake 顺序与 Store 一致；real/Fake 有编译期接口断言。
- [x] artifact read 严格 authorize-before-content-I/O；root jail 处理 symlink、Windows volume 与大小写。
- [x] quota、TTL、启动 sweep、周期 janitor 和 `.yanshi/artifacts` 文件清理均有施工步骤。
- [x] gate cwd 经过 root jail；metachar 仍由 guard 拒绝；RecordGate error 不忽略。
- [x] work event mapping 位于 orchestrator，WS/SSE 使用同一 per-turn typed callback；proto 词表统一。
- [x] SSE 不伪造 `set_mode`；TUI `/plan`/`/plan-off` 在 SSE 下本地报错。
- [x] `NewPlanUpdate(nil)` 生成非 nil empty checklist；TUI 三种 update case 完整且不自发 `task_read`。
- [x] sub-agent 继承 Plan/thread context，不扩大 Plan 工具面；durable event 与现有 sub-agent stream 分离。
- [x] 测试代码使用仓库真实 helper/构造方式：`newTestModel(t)`、`httptest.NewServer`、保存原 model 变量；未引用 `o.Model()`/`startTestServer`。
- [x] **v2-1 pathjail：** `WithinRootAbs` 落 `internal/pathjail`，`work`/`tools` 各一行薄封装；无 `work → tools` 反向依赖。
- [x] **v2-2 helpers：** 7 个 helper 全部有定义（`newID`/`NewID` 在 Step 4，其余 6 个在"Shared helpers"）。
- [x] **v2-3 Store API：** `AppendTimeline`/`AttachBrokerTask` 给签名+SQL+测试；`TestStoreCancelTask_GuardedUpdate` 在 Step 1 列出。
- [x] **v2-4 `runTool`：** Task 6 Step 1 在 `task_test.go` 声明，不引用不存在的 helper。
- [x] **v2-5 Authorize 整体替换：** Task 6 Step 3 明确替换 permctx.go:153–196 整段 + 加 `ForcePrompt` 字段。
- [x] **v2-6 force-prompt：** 删 allowlist 短路；`always_allow` 不 record；每次必 prompt（测试断言）。
- [x] **v2-7 runnerFor：** 删 `o.runner` 字段与 nil 回退；4 处 turn 入口逐一列出；测试同步双参。
- [x] **v2-8 applySetMode：** 给 `(oldMode, newMode)` 签名+实现；两处调用方改写。
- [x] **v2-9 cmdMode：** Plan 路由（→cmdPlan；离 plan 清字段）给出改写代码。
- [x] **v2-10 permModeText/resolver：** switch 加 `ModePlan`；Plan 分支注释解释 deny 原因。
- [x] **v2-11 proto→work：** 单向依赖约束写明，`work` 严禁 import proto。
- [x] **v2-12 EmitWorkFrame SSE：** `if fl != nil` 守卫 + 同 goroutine 顺序说明。
- [x] **v2-13 dispatch_failed：** 检查 `AppendTimeline` 错误，不 `_ =`。
- [x] **v2-14 workStore：** 明确不持有独立 `*sql.DB`，只 Close shared DB 一次。
- [x] **v2-15 sub-agent model：** TurnOpts.Model 留空，用 sub.rawModel。
- [x] **v2-16 NewFakeManager：** 给完整 struct + 构造器源码（map 预初始化）。
- [x] **v2-17 C1 drift：** 末尾"C1 adapter 落地指引"列出 4 个接触面与对齐原则。
- [x] **v2-18 gate 文档：** 注明复用 package-private `shellCommand`、有意范围收窄、不导出不复制。
- [x] 本计划共 **15 个 Task**，覆盖 DT1/G05/DT2/DT3、20 项初版 review 必修项与 18 项 v2 三评审合并必修项。
- [x] 本次只重写本 Markdown；未修改 `.go`、未运行 build/test、未提交 git。
