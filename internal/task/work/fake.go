// FakeManager：work.ManagerLike 的 deterministic in-memory 实现。
//
// 用途：tools/orchestrator/bootstrap 测试驱动，无需 SQLite 或 crypto/rand 之外的
// 外部依赖。Fake.Create 的时间戳由内部计数器自增，保证 List 顺序稳定
// （CreatedAt DESC，相同时按 ID DESC）。
//
// 行为契约与真实 Manager 保持一致：
//   - title/prompt 非空校验；
//   - Create 返回拷贝，避免调用方修改 Fake 内部状态；
//   - dispatch=true 时 Fake 生成一个 "bk-…" 占位 ID（不真正投递）；
//   - Start/Finish/Cancel 走 CanTransitionTo 校验，失败返回 error；
//   - List 按 CreatedAt DESC, ID DESC。
package work

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// FakeManager 是 ManagerLike 的内存实现。
type FakeManager struct {
	mu        sync.Mutex
	next      int64
	tasks     map[string]*WorkTask
	artifacts map[string]Artifact
	artBytes  map[string]int64 // taskID → total artifact bytes
}

// NewFakeManager 返回一个空的 FakeManager（map 预初始化）。
func NewFakeManager() *FakeManager {
	return &FakeManager{
		tasks:     make(map[string]*WorkTask),
		artifacts: make(map[string]Artifact),
		artBytes:  make(map[string]int64),
	}
}

// Create 复刻 Manager.Create 的语义：校验、生成 ID、追加 created timeline。
// dispatch=true 时生成占位 broker id（不真正投递）。
func (f *FakeManager) Create(_ context.Context, req CreateReq) (*WorkTask, error) {
	if strings.TrimSpace(req.Title) == "" || strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("work: title and prompt are required")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	now := time.Unix(f.next, 0)
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
	if req.Dispatch {
		task.BrokerTaskID = newID("bk") // Fake 不真正投递，只模拟 id 占位
	}
	f.tasks[task.ID] = task
	cp := *task
	return &cp, nil
}

// List 按 CreatedAt DESC, ID DESC 排序；limit <= 0 时不截断。
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
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// Read 返回 task 的拷贝；不存在返回 error。
func (f *FakeManager) Read(_ context.Context, id string) (*WorkTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return nil, errors.New("work: task not found")
	}
	cp := *t
	return &cp, nil
}

// Start 校验 pending → running。
func (f *FakeManager) Start(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return errors.New("work: task not found")
	}
	if err := t.Status.CanTransitionTo(StatusRunning); err != nil {
		return err
	}
	t.Status = StatusRunning
	t.UpdatedAt = time.Now()
	t.Timeline = append(t.Timeline, TimelineEntry{At: t.UpdatedAt, Kind: "started", Summary: "task started"})
	return nil
}

// Finish 校验 status ∈ {completed, failed} 并从 running 转移。
func (f *FakeManager) Finish(_ context.Context, id string, status Status, note string) error {
	if status != StatusCompleted && status != StatusFailed {
		return errors.New("work: finish status must be completed or failed")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return errors.New("work: task not found")
	}
	if err := t.Status.CanTransitionTo(status); err != nil {
		return err
	}
	t.Status = status
	t.UpdatedAt = time.Now()
	t.Timeline = append(t.Timeline, TimelineEntry{At: t.UpdatedAt, Kind: "finished", Summary: truncate(note, 240)})
	return nil
}

// Cancel 校验转移并清空 broker id（如果存在）。Fake 不模拟外部 broker。
func (f *FakeManager) Cancel(_ context.Context, id, by string) (*WorkTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return nil, errors.New("work: task not found")
	}
	if err := t.Status.CanTransitionTo(StatusCancelled); err != nil {
		return nil, err
	}
	t.Status = StatusCancelled
	t.BrokerTaskID = ""
	t.UpdatedAt = time.Now()
	t.Timeline = append(t.Timeline, TimelineEntry{At: t.UpdatedAt, Kind: "cancelled", Summary: "cancelled by " + by})
	cp := *t
	return &cp, nil
}

// SetChecklist 整组替换。
func (f *FakeManager) SetChecklist(_ context.Context, id string, c Checklist) (*WorkTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return nil, errors.New("work: task not found")
	}
	t.Checklist = c
	t.UpdatedAt = time.Now()
	cp := *t
	return &cp, nil
}

// AddChecklistItem 用 COALESCE(MAX(item_id),0)+1 分配新 id（与真实 Store 一致）。
func (f *FakeManager) AddChecklistItem(_ context.Context, id, content string) (*WorkTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return nil, errors.New("work: task not found")
	}
	next := 1
	for _, item := range t.Checklist.Items {
		if item.ID >= next {
			next = item.ID + 1
		}
	}
	t.Checklist.Items = append(t.Checklist.Items, ChecklistItem{ID: next, Content: content, Status: ChecklistPending})
	t.UpdatedAt = time.Now()
	cp := *t
	return &cp, nil
}

// PatchChecklistItem 用单点 find-and-update（Fake 内部不需要 SQL guarded UPDATE）。
func (f *FakeManager) PatchChecklistItem(_ context.Context, id string, itemID int, content string, status ChecklistItemStatus) (*WorkTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return nil, errors.New("work: task not found")
	}
	for i := range t.Checklist.Items {
		if t.Checklist.Items[i].ID == itemID {
			if content != "" {
				t.Checklist.Items[i].Content = content
			}
			if status != "" {
				t.Checklist.Items[i].Status = status
			}
			t.UpdatedAt = time.Now()
			cp := *t
			return &cp, nil
		}
	}
	return nil, errors.New("work: checklist item not found")
}

// RecordGate 把 Evidence 加入 task.Gates（INSERT OR REPLACE 的 Fake 等价：
// 同 gate 名替换），并追加 timeline。
func (f *FakeManager) RecordGate(_ context.Context, id string, ev Evidence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return errors.New("work: task not found")
	}
	replaced := false
	for i, g := range t.Gates {
		if g.Gate == ev.Gate {
			t.Gates[i] = ev
			replaced = true
			break
		}
	}
	if !replaced {
		t.Gates = append(t.Gates, ev)
	}
	t.UpdatedAt = time.Now()
	t.Timeline = append(t.Timeline, TimelineEntry{At: time.Now(), Kind: "gate", Summary: ev.Gate + ": " + ev.Classification})
	return nil
}

// WriteArtifact stores an artifact in memory. Unlike the real Manager, it does
// not enforce quota or persist to filesystem — the in-memory artifact is
// sufficient for test assertions on taskID/label/size round-trip.
func (f *FakeManager) WriteArtifact(_ context.Context, taskID string, label string, content []byte, _ string) (Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := newID("art")
	used := f.artBytes[taskID]
	_ = used // FakeManager does not enforce quota
	artifact := Artifact{
		ID:        id,
		TaskID:    taskID,
		Label:     label,
		Summary:   summarizeArtifact(content),
		Size:      int64(len(content)),
		CreatedAt: time.Now().Unix(),
	}
	f.artifacts[id] = artifact
	f.artBytes[taskID] += artifact.Size
	return artifact, nil
}

// ReadArtifact reads an artifact from the in-memory map.
func (f *FakeManager) ReadArtifact(_ context.Context, artifactID string) (Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	art, ok := f.artifacts[artifactID]
	if !ok {
		return Artifact{}, errors.New("work: artifact not found")
	}
	return art, nil
}

// ListArtifacts returns all artifacts for the given taskID. Test-only helper
// (not on the ManagerLike interface) used to assert that writeArtifactOrSpill
// stored an artifact for the expected task.
func (f *FakeManager) ListArtifacts(_ context.Context, taskID string) ([]Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Artifact
	for _, art := range f.artifacts {
		if art.TaskID == taskID {
			out = append(out, art)
		}
	}
	return out, nil
}

// Compile-time: FakeManager 实现 ManagerLike。
var _ ManagerLike = (*FakeManager)(nil)
