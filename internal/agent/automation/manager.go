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
func (a Automation) ThreadID() string { return "" }

// newID 用 crypto/rand 生成 8 字节十六进制 id。失败时返回 prefix-fallback。
func newID(prefix string) string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return prefix + "-fallback"
	}
	return prefix + "-" + hex.EncodeToString(bytes[:])
}
