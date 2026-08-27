// Package work 中 Manager 的领域实现。
//
// Manager 负责：
//   - 创建/列表/读取/取消 WorkTask（状态转移、timeline、checklist、gate）
//   - 经 Dispatcher port（可选）把 WorkTask 投递到 task.Broker
//   - artifact 文件生命周期（quota/TTL/安全路径）
//
// Manager 不直接读写 internal/task.Broker 的 store、也不实现 claim/retry。
// 与 broker 的所有交互仅经过 Dispatcher 接口，保持"领域不碰传输"的边界。
package work

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// newID 返回 prefix + "-" + 6 字节 crypto/rand 的 base32（NoPadding，小写）。
// panic 只在 crypto/rand 不可用时触发（属于运行环境致命错误，不静默退化）。
func newID(prefix string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("work: crypto/rand unavailable: " + err.Error())
	}
	return prefix + "-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// NewID 是 newID 的导出形式，供其它包（tools 的 evidence id）复用同一 kernel。
func NewID(prefix string) string { return newID(prefix) }

// truncate 按 rune 计数截断 s：若 utf8.RuneCountInString(s) <= max 则原样返回；
// 否则取前 max 个 rune 并追加 "…"。绝不截断半个 rune。max <= 0 时返回 "…"。
func truncate(s string, max int) string {
	if max <= 0 {
		return "…"
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	// 用 rune 切片保证不破坏多字节字符。
	rs := []rune(s)
	return string(rs[:max]) + "…"
}

// summaryOf 把 *WorkTask 投影成 Summary；Fake 与真实 Manager 都用它，
// 保证两路径 Summary 字段一致。t 必须非 nil（调用方保证）。
func summaryOf(t *WorkTask) Summary {
	return Summary{
		ID:        t.ID,
		Title:     t.Title,
		Status:    t.Status,
		ThreadID:  t.ThreadID,
		TurnID:    t.TurnID,
		Pct:       t.Checklist.CompletionPct(),
		GateCount: len(t.Gates),
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}

// Manager 是 work 包提供的领域 Manager；它持有 Store、Dispatcher 与 ArtifactPolicy。
// 所有 Store 调用都经过 Manager，让工具层只看到一个统一入口。
type Manager struct {
	store      *Store
	dispatcher Dispatcher
	policy     ArtifactPolicy
	// verifyRoot is the directory a checklist item's file_exists condition
	// resolves against (L7). Empty makes every such condition UNSATISFIED
	// rather than skipped — see VerifyChecklist for why an unverifiable claim
	// must not read as a verified one. Set it with WithVerifyRoot.
	verifyRoot string
}

// NewManager 构造一个 Manager。dispatcher 可以为 nil（local-only 模式，
// Dispatch=true 的 Create 会回错）。policy 经 withDefaults 补齐默认值。
func NewManager(store *Store, dispatcher Dispatcher, policy ArtifactPolicy) *Manager {
	return &Manager{store: store, dispatcher: dispatcher, policy: policy.withDefaults()}
}

// WithVerifyRoot sets the project root that L7 file_exists checklist
// conditions resolve against, and returns m for chaining.
//
// A separate setter rather than a NewManager parameter because every existing
// call site would otherwise have to be edited to pass a value most of them do
// not have — and the ones that skipped it would silently get "" , which reads
// as "no file condition can ever be satisfied". A setter makes supplying the
// root a visible act at the one place (the composition root) that knows it.
func (m *Manager) WithVerifyRoot(root string) *Manager {
	m.verifyRoot = root
	return m
}

// Create 创建一条 WorkTask 并可选地经 Dispatcher 投递 broker。
//
// dispatch=true 时失败语义：
//   - durable task 已经 commit（Store.Create 已经成功）；
//   - 必须把 dispatcher.Submit 的错误追加到 timeline（kind=dispatch_failed），
//     且 AppendTimeline 自身的错误不能再被 `_ =` 吞掉；
//   - 返回错误时仍把已建的 task 返回，让调用方知道 durable ID。
//
// dispatch 成功时把 broker id 经 AttachBrokerTask 写回，避免 task 表与 broker 表脱节。
func (m *Manager) Create(ctx context.Context, req CreateReq) (*WorkTask, error) {
	if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("work: title and prompt are required")
	}
	now := time.Now()
	task := &WorkTask{
		ID:        newID("wt"),
		Title:     req.Title,
		Prompt:    req.Prompt,
		Status:    StatusPending,
		ThreadID:  req.ThreadID,
		TurnID:    req.TurnID,
		CreatedAt: now,
		UpdatedAt: now,
		Timeline:  []TimelineEntry{{At: now, Kind: "created", Summary: truncate(req.Title, 160)}},
	}
	if err := m.store.Create(ctx, task); err != nil {
		return nil, err
	}
	if req.Dispatch {
		if m.dispatcher == nil {
			// nil dispatcher 是装配错误，而非运行时可恢复错误：直接回错并不再投递。
			tlErr := m.store.AppendTimeline(ctx, task.ID, TimelineEntry{
				At: time.Now(), Kind: "dispatch_failed", Summary: truncate("dispatcher unavailable", 240),
			})
			if tlErr != nil {
				return task, fmt.Errorf("work: durable task %s created but dispatch failed: dispatcher unavailable (timeline log also failed: %v)", task.ID, tlErr)
			}
			return task, errors.New("work: durable task created but dispatch failed: dispatcher unavailable")
		}
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

// List 委托 Store.List，签名与返回值都直接透传。
func (m *Manager) List(ctx context.Context, limit int, threadID string) ([]Summary, error) {
	return m.store.List(ctx, limit, threadID)
}

// Read 读取单 task；不存在返回 sql.ErrNoRows（透传 Store）。
func (m *Manager) Read(ctx context.Context, id string) (*WorkTask, error) {
	return m.store.Get(ctx, id)
}

// Start 把 task 从 pending 转移到 running 并追加 timeline。
func (m *Manager) Start(ctx context.Context, id string) error {
	return m.store.Transition(ctx, id, StatusRunning, "started", "task started")
}

// Finish 把 task 转移到 completed 或 failed 并追加 timeline。
// 其它 status 一律拒绝（pending/cancelled 不是合法的 finish 目标）。
//
// L7: a completion is GATED on the checklist. Before this, Checklist was a
// data structure with no consumer but CompletionPct — a task could be finished
// with every item still pending and nothing anywhere noticed, which made the
// checklist a decoration rather than a plan. Now:
//
//   - each item carrying a Verify condition has its status RE-DERIVED from the
//     world (file present / recorded gate exited zero) rather than from what
//     the model last claimed, and the re-derived checklist is persisted so the
//     caller can see which items blocked them;
//   - if anything is still not done, the transition is REFUSED with
//     ErrChecklistIncomplete naming the unmet items, and the refusal is
//     recorded on the timeline.
//
// Only completed is gated. A FAILED task is allowed to have unfinished items —
// that is what failing means, and gating it would leave a broken task stuck at
// running forever with no legal exit.
//
// An empty checklist gates nothing, so every task that never used one behaves
// exactly as before.
func (m *Manager) Finish(ctx context.Context, id string, status Status, note string) error {
	if status != StatusCompleted && status != StatusFailed {
		return errors.New("work: finish status must be completed or failed")
	}
	if status == StatusCompleted {
		if err := m.gateCompletion(ctx, id); err != nil {
			return err
		}
	}
	return m.store.Transition(ctx, id, status, "finished", truncate(note, 240))
}

// gateCompletion runs the L7 checklist gate for one task: verify, persist the
// verified checklist, and refuse if anything is unmet.
//
// The verified checklist is written back BEFORE the refusal so the caller who
// then reads the task sees the machine's verdict on each item, not the stale
// self-reported one. Without that write, "why was I blocked" would require
// re-running the verification by hand.
//
// A SetChecklist failure is returned rather than swallowed: it means the
// verdict was not recorded, and completing on the strength of a verdict nobody
// stored would be the same trust-the-model failure this gate exists to remove.
func (m *Manager) gateCompletion(ctx context.Context, id string) error {
	task, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if len(task.Checklist.Items) == 0 {
		return nil
	}
	verified, unmet := checklistGate(task.Checklist, m.verifyRoot, task.Gates)
	if err := m.store.SetChecklist(ctx, id, verified); err != nil {
		return err
	}
	if unmet == "" {
		return nil
	}
	if tlErr := m.store.AppendTimeline(ctx, id, TimelineEntry{
		At: time.Now(), Kind: "completion_blocked", Summary: truncate(incompleteNote(unmet), 240),
	}); tlErr != nil {
		return fmt.Errorf("%w: %s (timeline log also failed: %v)", ErrChecklistIncomplete, unmet, tlErr)
	}
	return fmt.Errorf("%w: %s", ErrChecklistIncomplete, unmet)
}

// Cancel 先（若已投递 broker）通过 Dispatcher.Cancel 取消 broker 行，
// 再做状态转移。broker 取消失败会阻止后续状态转移，避免出现"durable cancelled
// 但 broker 仍在跑"的状态。
func (m *Manager) Cancel(ctx context.Context, id, by string) (*WorkTask, error) {
	task, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if task.BrokerTaskID != "" && m.dispatcher != nil {
		if err := m.dispatcher.Cancel(task.BrokerTaskID); err != nil {
			return nil, fmt.Errorf("work: cancel broker task: %w", err)
		}
	}
	if err := m.store.Transition(ctx, id, StatusCancelled, "cancelled", "cancelled by "+by); err != nil {
		return nil, err
	}
	return m.store.Get(ctx, id)
}

// SetChecklist 委托 Store.SetChecklist 并返回更新后的 task。
func (m *Manager) SetChecklist(ctx context.Context, id string, c Checklist) (*WorkTask, error) {
	if err := m.store.SetChecklist(ctx, id, c); err != nil {
		return nil, err
	}
	return m.store.Get(ctx, id)
}

// AddChecklistItem 在事务内由 Store 分配下一个 item id 并返回更新后的 task。
func (m *Manager) AddChecklistItem(ctx context.Context, id, content string) (*WorkTask, error) {
	if _, err := m.store.AddChecklistItem(ctx, id, content); err != nil {
		return nil, err
	}
	return m.store.Get(ctx, id)
}

// PatchChecklistItem 委托 Store.PatchChecklistItem（单条 guarded UPDATE）。
// content/status 为零值时表示"不改这一列"（见 Store.PatchChecklistItem）。
func (m *Manager) PatchChecklistItem(ctx context.Context, id string, itemID int, content string, status ChecklistItemStatus) (*WorkTask, error) {
	if err := m.store.PatchChecklistItem(ctx, id, itemID, content, status); err != nil {
		return nil, err
	}
	return m.store.Get(ctx, id)
}

// RecordGate 委托 Store.RecordGate（INSERT OR REPLACE + timeline 追加）。
// 调用方必须在 work.Manager 这一层完成 Evidence.ID 的赋值（经 work.NewID("ev")），
// 否则 Store 主键 (task_id, gate) 会把相同 gate 的旧记录覆盖但 ID 列变空。
func (m *Manager) RecordGate(ctx context.Context, id string, ev Evidence) error {
	return m.store.RecordGate(ctx, id, ev)
}

// summarizeArtifact 取 content 第一行（"\n" 前）trim 空白；空 → "(empty)"；
// 含 NUL 字节 → "(binary)"；否则截到 120 rune。绝不把整段大文件塞进 summary。
func summarizeArtifact(content []byte) string {
	if bytes.IndexByte(content, 0) >= 0 {
		return "(binary)"
	}
	// 取第一行
	idx := bytes.IndexByte(content, '\n')
	first := content
	if idx >= 0 {
		first = content[:idx]
	}
	trimmed := strings.TrimSpace(string(first))
	if trimmed == "" {
		return "(empty)"
	}
	// 按 rune 截到 120（不加 "…"，与 truncate 区分：summary 已经是衍生字符串）
	if utf8.RuneCountInString(trimmed) <= 120 {
		return trimmed
	}
	rs := []rune(trimmed)
	return string(rs[:120])
}

// WriteArtifact 是 ManagerLike.WriteArtifact 的真实实现：
//  1. 先查 quota（已用 + 新增 ≤ QuotaBytes）；超过 → 拒绝，不留文件。
//  2. 在 .yanshi/artifacts/<taskID>/ 下用 tmp+rename 原子落盘。
//  3. 写 metadata；若 metadata 失败回滚已落的文件。
//
// 文件权限 0o600，目录 0o700；root 由调用方传入（一般是项目根目录）。
// 失败时返回的 Artifact 是零值。
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
	artifact := Artifact{
		ID:         id,
		TaskID:     taskID,
		Label:      label,
		Summary:    summarizeArtifact(content),
		ContentRef: filepath.ToSlash(filepath.Join(".yanshi", "artifacts", taskID, id+".txt")),
		Size:       int64(len(content)),
		CreatedAt:  time.Now().Unix(),
	}
	if err := m.store.PutArtifact(ctx, artifact); err != nil {
		_ = os.Remove(path)
		return Artifact{}, err
	}
	return artifact, nil
}

// ReadArtifact 是 ManagerLike.ReadArtifact：经 SecureArtifactPath 把 content_ref
// 解析到 root 内的绝对路径，再读取（不超过 MaxReadSize 字节）。
//
// 调用顺序是 authorize-before-content-I/O：先 jail 校验，再 stat 文件，
// 最后读字节。任何一步失败都不读取任何字节。
func (m *Manager) ReadArtifact(ctx context.Context, artifactID string) (Artifact, error) {
	art, err := m.store.GetArtifact(ctx, artifactID)
	if err != nil {
		return Artifact{}, err
	}
	// 注：真实 root 由调用方（artifact_read 工具）通过 ReadArtifactAt 注入；
	// Manager 的 ReadArtifact 在没有 root 时只返回 metadata（让测试可以验证
	// metadata 流）。完整路径解析与文件读取发生在 Task 9 的 artifact_read。
	return art, nil
}

// SweepArtifacts 删除 created_at < before 的 artifact metadata 与对应的
// .yanshi/artifacts 文件。janitor 周期调用（默认 6 小时）。
//
// 失败模式：
//   - store.DeleteArtifactsBefore 失败 → 直接返回 err，不删任何文件。
//   - SecureArtifactPath 失败（ref 逃逸、文件不存在）→ skip 该 ref，
//     不影响其它清理。生产中文件被并发删除是合法情况。
//   - os.Remove 失败（权限等）→ 吞掉错误，让 metadata 一致性优先。
func SweepArtifacts(ctx context.Context, store *Store, root string, before time.Time) error {
	refs, err := store.DeleteArtifactsBefore(ctx, before.Unix())
	if err != nil {
		return err
	}
	for _, ref := range refs {
		path, err := SecureArtifactPath(root, ref)
		if err != nil {
			continue
		}
		_ = os.Remove(path)
	}
	return nil
}

// StartArtifactJanitor 启动周期性 artifact 清理 goroutine。interval 是清理间隔
// （默认 6 小时），ttl 是保留时长（默认 7 天）。ctx 取消时 goroutine 退出。
//
// bootstrap 在 store/mgr 构造之后调用一次；不接受 Close 钩子（goroutine 自行退出）。
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

// 编译期 ManagerLike 断言（真实 Manager 实现了 port 全部方法）。
var _ ManagerLike = (*Manager)(nil)

// DispatcherRef 是可后绑定的 Dispatcher 持有者，解决 bootstrap 装配顺序：
// Manager 在 broker 之前构造（store → work.Manager → ... → broker → Bind），
// 所以 dispatcher 通过 Bind 在 broker 构造之后注入。Bind 前的 Submit/Cancel
// 返回 "work: dispatcher unavailable"，让 Create(dispatch=true) 仍能生成
// durable task 并把失败写进 timeline。
type DispatcherRef struct {
	mu sync.RWMutex
	d  Dispatcher
}

// Bind 注入真实 Dispatcher（一般是 work.BrokerAdapter{Broker: broker}）。
func (r *DispatcherRef) Bind(d Dispatcher) {
	r.mu.Lock()
	r.d = d
	r.mu.Unlock()
}

// Submit 在 Dispatcher 未绑定时返回错误而非 panic；调用方（Manager）
// 会把这条错误写进 timeline 当作 dispatch_failed。
func (r *DispatcherRef) Submit(typ, input, parent string) (string, error) {
	r.mu.RLock()
	d := r.d
	r.mu.RUnlock()
	if d == nil {
		return "", errors.New("work: dispatcher unavailable")
	}
	return d.Submit(typ, input, parent)
}

// Cancel 与 Submit 同语义：未绑定即 fail。
func (r *DispatcherRef) Cancel(id string) error {
	r.mu.RLock()
	d := r.d
	r.mu.RUnlock()
	if d == nil {
		return errors.New("work: dispatcher unavailable")
	}
	return d.Cancel(id)
}

// Compile-time: DispatcherRef 实现 Dispatcher 接口。
var _ Dispatcher = (*DispatcherRef)(nil)

// BrokerAdapter 把 internal/task.Broker（或任何实现了 Submit/Cancel 的类型）
// 适配到 work.Dispatcher。Broker 实现的具体接口由 BrokerAdapter.Broker 字段
// 直接声明（不导入 internal/task），让 work 包不必反向依赖 internal/task。
type BrokerAdapter struct {
	Broker interface {
		Submit(string, string, string) (string, error)
		Cancel(string) error
	}
}

// Submit 委托底层 Broker.Submit。
func (a BrokerAdapter) Submit(typ, input, parent string) (string, error) {
	return a.Broker.Submit(typ, input, parent)
}

// Cancel 委托底层 Broker.Cancel。
func (a BrokerAdapter) Cancel(id string) error {
	return a.Broker.Cancel(id)
}

// Compile-time: BrokerAdapter 实现 Dispatcher。
var _ Dispatcher = BrokerAdapter{}
