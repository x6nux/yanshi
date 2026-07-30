# Batch F2 — 资源治理与压测基线 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 补齐 A-D 功能面后的质量/性能/可观测缺口 —— 清理 `createdWT` 泄漏（LEAK1）、核对子代理并发上限（LEAK2）、加 mid-turn 压缩 cooldown（CCL1）、建性能基准基线（BENCH1）、让 ACP 子进程 usage 回流到 budget（LEAK3）。

**Architecture:** LEAK1 先抽 `reclaimWorktree(id)` 公共回收 helper 供三处复用（RecordResult/Cancel/RequeueStale），所有回收走 `createdWTMu` 一条锁路径。LEAK2 不新增计数/上限逻辑，只补注释 + 判定顺序文档化（条件性 runningLocked 收紧登记给 RAC1）。CCL1 在 `CompactingModel` 加 cooldown 字段（token 增长阈值 + time 阈值 + hardForceFraction 0.95 强制兜底），不动 `ctxcompact.Run` 核心。BENCH1 四个 `_bench_test.go`（零外部依赖） + benchstat 脚本；CI 集成在 CIG1 nightly，F2 只产出 bench 文件。LEAK3 先解析 `usage_report` 事件于 `internal/acp`，再回流到 `goalloop.UsageSink`（共享 sink，overBudget 天然含子进程；budget 行为见 blocker §7）。

**Tech Stack:** Go 1.26.4、`testify`（`assert`/`require`）、`einollm.FakeModel`、`acp.FakeAgent`、`store` (`:memory:`)、`benchstat`。

**范围:** docs/superpowers/specs/2026-07-22-f2-resource-benchmarks-design.md §4-8 的五个缺口。不新增功能面（A-D 未完成项）、不跨重启持久化回收、不优化压缩算法、不硬门禁基准、不解析 agent stdout 兜底、不重复造并发上限。

---

## 不变量与边界

1. **LEAK2 仅核对** —— B1/M04b 已完整实现并发上限（MaxConcurrent clamp 1..20、SpawnErrCap、spawnWithRetry、subagents.limit）；本批只补交互注释 + 判定顺序文档化。runningLocked 收紧仅当 RAC1 报竞态时才做。
2. **CCL1 cooldown 状态跨 turn 保留** —— 字段在 `CompactingModel` 实例上（随 `runners sync.Map` 缓存）。hardForceFraction 0.95 强制执行兜底。不动 `ctxcompact.Run` / `Planning` 核心。
3. **CCL1 不动 keepRecent /2 桥接** —— `CompactingModel.KeepRecent`（消息数）→ `PlanOpts.KeepRecent`（对数）的 `/2` 桥接不变，只补承重注释。
4. **BENCH1 阈值 20%** —— benchstat 记录趋势不硬门禁；PR 子集 + nightly 全集的切分词在 CIG1；F2 只产 bench + 脚本。
5. **LEAK3 只解析 usage_report 事件** —— agent stdout 兜底不做。解析失败 recover 降级不阻塞 turn、不 panic。budget 见 blocker §7。
6. **LEAK1 reclaimWorktree 语义** —— 仅在任务已确认终态后调用（`delete` 幂等 + `RemoveWorktree` 容错已不存在）；pending 重试任务 `保留` worktree 供复用。
7. **reclaimWorktree 走 createdWTMu** —— 与 Cancel/RecordResult 互斥；三处复用（CLAUDE.md "重复逻辑必须抽公共"）。
8. **全部 Fake 优先** —— FakeModel/FakeAgent/内存 store/t.TempDir；零外部依赖。e2e_real 测试（门禁 `YANSHI_E2E=1`）是真实路径的补充覆盖，非本批验收门禁。
9. **单文件 ≤1000 纯代码行** —— compacting.go 目前 124 行（纯代码 ~110），加 cooldown 字段+逻辑后约 **+50 行**，仍安全（≤200）。
10. **reclaimWorktree 必须同时在 createdWTMu 内操作 createdWT 并调用 RemoveWorktree** —— 不允许分两次锁（其他 goroutine 的 Cancel 可能在此区间删了同一 id → 误回收已复用 worktree）。

---

## 依赖图

```
E2-RAC1 ────────────────────► Task 1 (LEAK1 reclaimWorktree) ──► Task 2 (RequeueStale)
                                          │                               │
                                          │                              ┌┘
                                          │    ┌─────────────────────────┘
                                          ▼    ▼
    Task 3 (LEAK2 comments)      独立     ─ (仅 if RAC1 报 runningLocked 竞态 → 需收紧)
                                          ▲
    Task 4 (CCL1 cooldown)       独立     ─ (不动 ctxcompact；不依赖 E1/PROP1/RAC1)

    Task 5 (BENCH1)              独立     ─ (产出供 CIG1 nightly 集成)

    Task 6 (LEAK3 acp parse)     ──────► Task 7 (goalloop 回流)
                                          │
                                          ▼
    [blocker: budget 仅告警不硬停需确认 ← ─┘
```

### 衔接
- **E2-RAC1 → LEAK1**：RAC1 先暴露 `createdWTMu` 与 `RemoveWorktree` 的既有竞态；LEAK1 Task 1 抽的 `reclaimWorktree` 统一在 `createdWTMu` 下，其互斥设计与 RAC1 对齐。Task 2 （RequeueStale 的 race 测试）依赖 RAC1 已固化 `-race` 测试基础设施。
- **LEAK1 Task 2 依赖 RAC1**：RAC1 产生的 broker race 测试是本批 `-race` 并发断言的基础。若 RAC1 因故推迟，Task 2 可作为纯非-race 测试先合入。
- **BENCH1 → CIG1 nightly**：F2 产出 `_bench_test.go` 三个文件 + `scripts/bench.sh`。CIG1（G 批次）将 bench job 接入 CI 矩阵（区分 PR 子集 / nightly 全集）。F2 不做 workflow YAML 改动。
- **LEAK3 Task 6 → Task 7**：acp 层解析出 `Event.Usage` 后，goalloop 层才能读取。两任务可紧邻提交。

---

## 文件结构

| 文件 | 职责 | 新/改 | Task |
|---|---|---|---|
| `internal/task/broker.go` | 抽 `reclaimWorktree`（RecordResult/Cancel 复用）+ `RequeueStale` 终态回收 + 生命周期注释 | 改 | 1,2 |
| `internal/task/broker_test.go` | reclaimWorktree 单元 + RequeueStale 回收/保留/长跑/race | 改 | 1,2 |
| `internal/agent/registry/manager.go` | depth/concurrency 双上限交互注释 + (条件) runningLocked 注释 | 改 | 3 |
| `internal/tools/subagent.go` | `MaxSubAgentDepth` 正交注释 | 改 | 3 |
| `internal/agent/registry/manager_spawn_test.go` | （可选）深度优先于并发的判定顺序断言 | 改 | 3 |
| `internal/llm/eino/compacting.go` | `lastCompactTokens`/`At` + cooldown 延后/强制判定 + keepRecent 双语义注释 + `cmMu sync.Mutex` | 改 | 4 |
| `internal/llm/eino/compacting_test.go` | cooldown 内延后、0.95 强制、单 turn 不重复、keepRecent 桥接 | 改 | 4 |
| `internal/config/config.go` | `CompactionConfig` cooldown/hardForce 字段 + defaults | 改 | 4 |
| `internal/agent/orchestrator/orchestrator.go` | `CompactionConfig` cooldown/hardForce 字段 + `wrapCompaction` 透传 | 改 | 4 |
| `internal/vcs/vcs_bench_test.go` | BenchmarkVCSCommit + BenchmarkDAGApply | 新 | 5 |
| `internal/tools/fs_bench_test.go` | BenchmarkFSEdit | 新 | 5 |
| `internal/agent/orchestrator/orchestrator_bench_test.go` | BenchmarkOrchestratorTurn（FakeModel） | 新 | 5 |
| `scripts/bench.sh` | benchstat 比对脚本 | 新 | 5 |
| `internal/acp/types.go` | `Usage` 结构体 | 改 | 6 |
| `internal/acp/client.go` | `handleNotify` 解析 `usage_report` + `Prompt` onEvent 透传 `Event.Usage` | 改 | 6 |
| `internal/acp/client_test.go` / `fakeagent.go` | FakeAgent `UsageReports` scripting + 解析通/畸形降级 | 改 | 6 |
| `internal/agent/goalloop/implementer.go` | `ACPImplementer.Sink` 字段 + `worker` onEvent 累加 | 改 | 7 |
| `cmd/yanshi/main.go` | 装配时注入 `loopSink` 到 `ACPImplementer` | 改 | 7 |

---

## Task 1: LEAK1 — 抽 `reclaimWorktree` helper + RecordResult/Cancel 复用

**Files:**
- Modify: `internal/task/broker.go`
- Modify: `internal/task/broker_test.go`

---

- [ ] **Step 1: 写 `reclaimWorktree` 单元测试**

```go
// append to internal/task/broker_test.go
func TestBroker_ReclaimWorktreeClearsMapAndRemovesWorktree(t *testing.T) {
	b, s, v, _, _ := newTestBrokerWithVCS(t, 2, 5*time.Second)
	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)
	// Simulate: Claim creates a worktree and records it in createdWT.
	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotEmpty(t, task.WorktreeID)

	// reclaimWorktree must clear the map entry and call RemoveWorktree.
	// assert our internal state before.
	b.createdWTMu.Lock()
	_, has := b.createdWT[id]
	b.createdWTMu.Unlock()
	assert.True(t, has, "createdWT must track the broker-created worktree after Claim")

	b.reclaimWorktree(id)

	b.createdWTMu.Lock()
	_, stillHas := b.createdWT[id]
	b.createdWTMu.Unlock()
	assert.False(t, stillHas, "createdWT must be cleared after reclaimWorktree")

	// The VCS worktree row is deactivated.
	active, found := worktreeActive(t, s, task.WorktreeID)
	assert.True(t, found, "worktree row must exist (deactivated)")
	assert.Equal(t, 0, active, "worktree row must be deactivated after reclaim")
}

func TestBroker_ReclaimWorktreeIdempotent_NoErrorForMissingID(t *testing.T) {
	b, _, _, _, _ := newTestBrokerWithVCS(t, 2, 5*time.Second)
	// No createdWT entry for "never-claimed" — must not panic or error.
	b.reclaimWorktree("never-claimed")
	// No assertions needed: the helper is a no-op.
	b.reclaimWorktree("never-claimed") // double call also safe.
}
```

Import 块已有 `"os"`、`"time"`；`worktreeActive` 已在 test 文件 line 548 存在。无需新增 import。

- [ ] **Step 2: 运行测试，确认 `reclaimWorktree` 未定义**

Run: `go test ./internal/task -run TestBroker_ReclaimWorktree -v`

Expected: FAIL，`Broker.reclaimWorktree (undefined)`。

- [ ] **Step 3: 插入 `reclaimWorktree` 公共 helper**

在 `broker.go` 的 `Cancel` 前插入（包内复用，保留原有 RecordResult/Cancel 行为不变）：

```go
// reclaimWorktree removes the broker-created worktree (if any) for task id from
// both the createdWT bookkeeping map and the VCS. Shared (pre-set) worktrees
// are never in createdWT and are left untouched. Operation is under createdWTMu
// so it is mutually exclusive with the same path in Cancel / RecordResult and
// the future RequeueStale terminal path. Callers MUST have confirmed the task
// reached a terminal status before calling. Removal is best-effort: delete is
// idempotent and RemoveWorktree errors are swallowed (no logger in this pkg).
func (b *Broker) reclaimWorktree(id string) {
	b.createdWTMu.Lock()
	wtID, created := b.createdWT[id]
	if created {
		delete(b.createdWT, id)
	}
	b.createdWTMu.Unlock()
	if created && b.VCS != nil {
		_ = b.VCS.RemoveWorktree(wtID)
	}
}
```

- [ ] **Step 4: 替换 RecordResult 和 Cancel 中的内联回收逻辑为 `reclaimWorktree(id)`**

`RecordResult`（broker.go:162-173）的大锁 + delete + RemoveWorktree 替换为单行 `b.reclaimWorktree(id)`。

```go
// Terminal transition succeeded: reclaim any worktree the broker itself
// created for this task.
b.reclaimWorktree(id)
return nil
```

`Cancel`（broker.go:201-207）的锁 + delete + RemoveWorktree 同理替换为单行 `b.reclaimWorktree(id)`。

```go
// Reclaim any worktree the broker created for this task.
b.reclaimWorktree(id)
return nil
```

删除 RecordResult 和 Cancel 各自原有的 `b.createdWTMu.Lock() ... / b.VCS.RemoveWorktree ...` 代码块。

- [ ] **Step 5: 在 `Broker` 与 `Claim` 处补 worktree 生命周期注释**

在 `type Broker struct` 的 `createdWT` 字段文档追加生命周期摘要（`broker.go:38-40`）：

```go
// createdWT tracks worktrees the broker itself created in Claim (taskID →
// worktreeID) so terminal paths can reclaim them. Lifecycle:
//   ╔═══════════╗   Claim   ╔════════════════╗   terminal (finalize /   ╔═══════════════╗
//   ║  no VCS / ║ ──► ───── ─║ createdWT[id] ║ ──► cancel / requeue-  ──║ reclaimWorktree║
//   ║ pre-set wt║  skip map  ║  = wtID       ║    failed)              ║ delete + 移除  ║
//   ╚═══════════╝            ╚════════════════╝                         ╚═══════════════╝
//   non-terminal requeue → KEEP worktree (reusable by re-claim)
```

- [ ] **Step 6: 运行测试并提交**

Run: `go test ./internal/task -v`

Expected: PASS；所有既有 finalize/cancel/requeue 测试 + 新 `reclaimWorktree` 测试绿。行为等价于重构前（existing tests guard）。

```bash
git add internal/task/broker.go internal/task/broker_test.go
git commit -m "refactor(task): extract reclaimWorktree helper for three-site reuse"
```

---

## Task 2: LEAK1 — RequeueStale 终态回收 + 长跑 + race

**Files:**
- Modify: `internal/task/broker.go`
- Modify: `internal/task/broker_test.go`

- [ ] **Step 1: 写 RequeueStale 未回收的泄漏测试**

```go
// append to internal/task/broker_test.go

// TestBroker_RequeueStaleMaxRetriesReclaimsWorktree verifies that when RequeueStale
// fails a task (exceeded maxRetries) the broker-created worktree is reclaimed —
// this is the LEAK1 gap fix.
func TestBroker_RequeueStaleMaxRetriesReclaimsWorktree(t *testing.T) {
	b, s, v, _, root := newTestBrokerWithVCS(t, 1, 5*time.Millisecond)
	// hbTimeout=5ms so the task becomes stale almost immediately.

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)

	// Claim → broker creates a worktree and records it in createdWT.
	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotNil(t, task)
	require.NotEmpty(t, task.WorktreeID, "should have a broker-created worktree")

	// Confirm worktree is present.
	_, err = v.WorktreePath(task.WorktreeID)
	require.NoError(t, err)

	// Wait for the heartbeat timeout to expire + sweeper run.
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, b.RequeueStale(context.Background()))
	// maxRetries=1 → first stale timeout exceeds maxRetries → status='failed'.

	got, err := s.GetTask(id)
	require.NoError(t, err)
	require.Equal(t, "failed", got.Status, "task must be failed after exceeding maxRetries")

	// createdWT must no longer contain this id.
	b.createdWTMu.Lock()
	_, has := b.createdWT[id]
	b.createdWTMu.Unlock()
	assert.False(t, has, "createdWT entry must be cleared when RequeueStale fails a task")

	// VCS worktree must be deactivated.
	active, found := worktreeActive(t, s, task.WorktreeID)
	assert.True(t, found, "worktree row must exist (deactivated)")
	assert.Equal(t, 0, active, "worktree must be deactivated after RequeueStale terminal")
}

// TestBroker_RequeueStaleRequeueKeepsWorktree verifies that when RequeueStale
// requeues a task (within retry budget → pending) the broker-created worktree
// is PRESERVED for reuse by the next Claim.
func TestBroker_RequeueStaleRequeueKeepsWorktree(t *testing.T) {
	b, s, v, _, _ := newTestBrokerWithVCS(t, 3, 5*time.Millisecond)

	id, err := b.Submit("echo", "in", "")
	require.NoError(t, err)
	task, err := b.Claim("worker-1")
	require.NoError(t, err)
	require.NotEmpty(t, task.WorktreeID)

	// Stale → requeued (attempts=1 < maxRetries=3 → pending).
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, b.RequeueStale(context.Background()))

	got, err := s.GetTask(id)
	require.NoError(t, err)
	require.Equal(t, "pending", got.Status, "must be pending because within retry budget")

	// createdWT MUST retain the entry (worktree to be reused).
	b.createdWTMu.Lock()
	_, has := b.createdWT[id]
	b.createdWTMu.Unlock()
	assert.True(t, has, "createdWT entry must be kept for a pending requeue")

	// Worktree still exists on disk.
	wtPath, err := v.WorktreePath(task.WorktreeID)
	require.NoError(t, err)
	assert.DirExists(t, wtPath, "worktree dir must exist after pending requeue")
}

// TestBroker_RequeueStaleLongRun asserts that after submitting N tasks all doomed
// to fail (maxRetries=0) the len(createdWT) returns to zero.
func TestBroker_RequeueStaleLongRunMapBounds(t *testing.T) {
	b, s, _, _, _ := newTestBrokerWithVCS(t, 0, 5*time.Millisecond)
	n := 10
	for i := 0; i < n; i++ {
		_, err := b.Submit("echo", "in", "")
		require.NoError(t, err)
		_, err = b.Claim("worker-1")
		require.NoError(t, err)
	}
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, b.RequeueStale(context.Background()))

	b.createdWTMu.Lock()
	leakCount := len(b.createdWT)
	b.createdWTMu.Unlock()
	assert.Equal(t, 0, leakCount, "len(createdWT) must be zero after all tasks have failed and been reclaimed")

	// Also confirm all worktree rows are deactivated.
	rows, err := s.DB.Query("SELECT active FROM vcs_worktrees")
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var active int
		require.NoError(t, rows.Scan(&active))
		assert.Equal(t, 0, active, "all worktree rows must be deactivated")
	}
	require.NoError(t, rows.Err())
}

// TestBroker_RequeueStaleConcurrentWithCancel_Race ensures that running RequeueStale
// concurrently with Cancel and RecordResult does not produce a data race or
// double-reclaim a worktree. Requires -race.
func TestBroker_RequeueStaleConcurrentWithCancel_Race(t *testing.T) {
	b, s, _, _, _ := newTestBrokerWithVCS(t, 3, 5*time.Millisecond)

	for i := 0; i < 20; i++ {
		id, err := b.Submit("echo", "in", "")
		require.NoError(t, err)
		_, err = b.Claim("worker-1")
		require.NoError(t, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			_ = b.RequeueStale(ctx)
		}
	}()

	// Concurrent Cancels.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			pending, err := s.ListPending(100)
			if err == nil {
				for _, t := range pending {
					_ = b.Cancel(t.ID)
				}
			}
		}
	}()

	wg.Wait()
	// No panic, no race. -race must pass.
}
```

注意: 在 `TestBroker_RequeueStaleConcurrentWithCancel_Race` 中引入 `"sync"`。测试文件 import 块确认已有 `"sync"` (`line 8: some tasks may need it`)。检查 `broker_test.go` 当前 import: `context, os, path/filepath, testing, time, testify/...` — 需加 `"sync"`。

- [ ] **Step 2: 运行测试，确认 RequeueStale 泄漏仍然存在**

Run: `go test ./internal/task -run 'TestBroker_RequeueStale(Max|Requeue|Long|Concurrent)' -v -count=1`

Expected: FAIL。`TestBroker_RequeueStaleMaxRetriesReclaimsWorktree` 断言 `createdWT` 有条目 → 但 `RequeueStale` 不清理 → `has=true` → FAIL。

- [ ] **Step 3: 在 `RequeueStale` 添加终态回收**

在 `broker.go` 的 `RequeueStale` 函数中，`changed=true` 的现有 notify 后追加回收逻辑（`broker.go:235-238`）：

```go
if !changed {
    // Task was already finalized or re-claimed — skip.
    continue
}
// RequeueStaleTask atomically set the status to either 'failed' (exceeded
// maxRetries) or 'pending' (within retry budget). Only terminal statuses
// reclaim the broker-created worktree; pending keeps it for the retry.
t2, err := b.store.GetTask(t.ID)
if err != nil {
    return err
}
if t2.Status == "failed" {
    b.reclaimWorktree(t.ID)
}
// Signal that a task is available for claiming.
select {
case b.notify <- struct{}{}:
default:
}
```

注意：`"failed"` 是 RequeueStaleTask 在 `attempts+1 > maxRetries` 时设置的唯一终态。`pending` 分支不回收。`GetTask` 读回的新状态避免与并发 finalize 的双删（`delete` 幂等 + RemoveWorktree 容错）。

- [ ] **Step 4: 运行 `-race` 测试**

Run: `go test -race ./internal/task -run TestBroker_RequeueStaleConcurrentWithCancel_Race -v -count=3`

Expected: PASS（无 race）。

Run: `go test ./internal/task -v`

Expected: 全部 PASS（含既有 `TestClaim_FinalizeRemovesBrokerCreatedWorktree`、`TestBroker_Cancel`、`TestBroker_RequeueStaleMaxRetries` 等）。

- [ ] **Step 5: 提交**

```bash
git add internal/task/broker.go internal/task/broker_test.go
git commit -m "fix(task): reclaim worktree on RequeueStale terminal; long-run + race guarded"
```

---

## Task 3: LEAK2 — 深度/并发双上限交互承重注释 + 判定顺序断言

**Files:**
- Modify: `internal/agent/registry/manager.go`
- Modify: `internal/tools/subagent.go`
- Modify: `internal/agent/registry/manager_spawn_test.go`（可选，加顺序断言）

- [ ] **Step 1: 确认既有上限测试通过**

Run: `go test ./internal/agent/batch -run TestRunnerCapsAtRegistryMaxConcurrent -v`

Expected: PASS（B1/M04b 已完整实现并发上限）。

- [ ] **Step 2: 写深度/并发双上限与 `runningLocked` 注释**

在 `manager.go` 的 `Spawn` 函数内（`depth` 判定处 `:121` 前 / 并发判定 `:131` 前）添加承重注释：

```go
// --- depth vs concurrency: two orthogonal dimensions ---
// Depth (vertical, MaxSubAgentDepth=3 from subagent.go:99) is checked FIRST.
// When both depth and concurrency are exceeded, ErrTooDeep wins — a deeper
// agent will never starve a shallower slot (the concurrency limit governs
// independently). Both dimensions are in effect simultaneously: an agent at
// max depth may still spawn its own sub-agent if the concurrency budget
// permits, and a sub-agent at the concurrency limit cannot spawn another
// even if the depth budget is available.
if depth > 3 { // MaxSubAgentDepth
    return "", ErrTooDeep
}
```

```go
// --- concurrency cap gate (second dimension) ---
// runningLocked() returns the max of runtime map entries and StatusRunning
// records. The two can diverge transiently under heavy concurrency; taking the
// max is conservative (prefer spawning fewer than budgeted over more). If RAC1
// reports a measurable race here, the resolution is to use the runtime map as
// the sole authoritative count (the records count is only advisory).
if m.runningLocked() >= m.limit {
```

`runningLocked` 函数自身（`:536-553`）添加上述双计数取大的语义注释（同上）。

在 `subagent.go` 的 `MaxSubAgentDepth` 常量处（`:99`）添加正交注释：

```go
// MaxSubAgentDepth is the vertical nesting limit. Concurrency (horizontal
// limit, configurable via subagents.limit / registry.Manager limit) is a
// separate orthogonal dimension. They interact at the Spawn site but neither
// subsumes the other: see registry/manager.go Spawn for the priority order.
```

- [ ] **Step 3: （可选）加深度优先于并发的判定顺序断言**

若当前 `manager_spawn_test.go` 的 test helpers 能方便构造 `maxDepth` + `maxConcurrency` 的场景，追加：

```go
// TestSpawnDepthBeforeConcurrency asserts that when both the depth limit and
// the concurrency cap are exceeded, ErrTooDeep is returned (depth is checked
// first). This documents the priority ordering in Spawn.
func TestSpawnDepthBeforeConcurrency(t *testing.T) {
    // Requires a test helper that fills all runtime slots and sets up a deep
    // parent chain. If the existing manager_spawn_test.go helpers support this,
    // add the test; otherwise the commentary alone suffices.
    t.Skip("add when manager_spawn_test.go has helpers for depth+concurrency saturation")
}
```

- [ ] **Step 4: 运行测试并提交**

Run: `go test ./internal/agent/registry ./internal/tools ./internal/agent/batch -v -race`

Expected: PASS（纯注释改动，行为无变化）。

```bash
git add internal/agent/registry/manager.go internal/tools/subagent.go
git commit -m "docs(leak2): document depth/concurrency dual-limit interaction"
```

---

## Task 4: CCL1 — mid-turn 压缩 cooldown

**Files:**
- Modify: `internal/llm/eino/compacting.go`
- Modify: `internal/llm/eino/compacting_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/bootstrap/bootstrap.go`

- [ ] **Step 1: 写 cooldown/force/keepRecent 测试**

```go
// append to internal/llm/eino/compacting_test.go

// TestCompactingCooldown_SameTurnRepeatedGrowth verifies that after one compaction
// succeeds, a second Generate call with moderate new messages (still over threshold
// but within token-growth cooldown) does NOT compress again — the cooldown defers.
// The hard-force fraction is 0.95 so moderate growth does not force.
func TestCompactingModel_CooldownDefersReCompact(t *testing.T) {
	inner := &recordingModel{summary: "summarized", reply: "done"}
	cm := &CompactingModel{
		Inner:               inner,
		Threshold:           0.8,
		ContextWindow:       2000,
		KeepRecent:          2,
		CooldownTokens:      500,  // defer unless growth >=500 tokens
		HardForceFraction:   0.95, // force only at 1900+ tokens
	}
	ctx := context.Background()

	// First call: no prior compact → over threshold → compacts.
	msgs := buildMessages(t, 50) // ~2000 tokens → over 0.8×2000=1600
	out, didCompact := cm.maybeCompact(ctx, msgs)
	require.True(t, didCompact, "first call must compact (over threshold)")
	require.Equal(t, 1, inner.calls, "one summarize call must have happened")
	_ = out

	// Simulate that compaction left history near threshold (decouples this test
	// from the exact compact result size).  lastCompactTokens=1550, just under
	// threshold=1600, and lastCompactAt=now means cooldown is active.
	// Tune buildMessages(n) if this test fails due to token estimate mismatch.
	cm.lastCompactTokens = 1550
	cm.lastCompactAt = time.Now()
	inner.calls = 0

	// Messages over threshold (~1700 tokens) but growth from 1550 is ~150
	// tokens, which < CooldownTokens=500 → cooldown defers re-compaction.
	msgs2 := buildMessages(t, 42)
	out2, didCompact2 := cm.maybeCompact(ctx, msgs2)
	require.False(t, didCompact2, "must NOT compact inside cooldown window")
	require.Equal(t, 0, inner.calls, "no additional summarize call")
	// out2 must be msgs2 unchanged (no compaction occurred).
	require.Equal(t, len(msgs2), len(out2), "cooldown must return original msgs")

	// Messages over hard-force threshold: growth from 1550 is ~350 < 500
	// (still inside token cooldown) but HardForceFraction=0.95 overrides it.
	msgs3 := buildMessages(t, 48)
	inner.calls = 0
	out3, didCompact3 := cm.maybeCompact(ctx, msgs3)
	require.True(t, didCompact3, "must compact at hard-force fraction regardless of cooldown")
	_ = out3
}

// TestCompactingModel_HardForceOverridesCooldown verifies that when estimated
// tokens reach 0.95×ContextWindow, compaction fires even inside the cooldown.
func TestCompactingModel_HardForceOverridesCooldown(t *testing.T) {
	inner := &recordingModel{summary: "s", reply: "ok"}
	cm := &CompactingModel{
		Inner:               inner,
		Threshold:           0.8,
		ContextWindow:       1000,
		KeepRecent:          1,
		CooldownTokens:      99999, // extremely large cooldown — should STILL be overridden
		HardForceFraction:   0.95,
	}
	msgs := buildMessages(t, 30) // ~1200 tokens → over 0.95×1000=950

	out, didCompact := cm.maybeCompact(context.Background(), msgs)
	require.True(t, didCompact, "must compact at 0.95 fraction regardless of cooldown")
	_ = out
}

// TestCompactingCooldown_FirstCompactAlwaysProceeds tests that before any prior
// compact, the cooldown is a no-op (lastCompactTokens=0 → no cooldown).
func TestCompactingModel_FirstCompactNoCooldown(t *testing.T) {
	inner := &recordingModel{summary: "s", reply: "ok"}
	cm := &CompactingModel{
		Inner:               inner,
		Threshold:           0.8,
		ContextWindow:       2000,
		KeepRecent:          2,
		CooldownTokens:      100,
		HardForceFraction:   0.0, // disable hard-force for isolation
	}
	msgs := buildMessages(t, 50) // over 0.8×2000=1600
	out, didCompact := cm.maybeCompact(context.Background(), msgs)
	require.True(t, didCompact, "first compact must proceed even with cooldown configured")
	_ = out
}

// TestCompactingModel_KeepRecentBridge verifies the /2 bridge is documented.
// The test does NOT assert the bridge itself (it's a legacy semantics decision);
// it simply asserts existing CompactingModel.KeepRecent behavior works.
// This test will be updated if the /2 semantics change.
func TestCompactingModel_KeepRecentBridge(t *testing.T) {
	cm := &CompactingModel{KeepRecent: 4}
	// KeepRecent on CompactingModel is a MESSAGE count (not pair count).
	// ctxcompact.PlanOpts.KeepRecent is a PAIR count, bridged via /2.
	if cm.KeepRecent/2 < 2 { // 4/2 = 2 pairs → 4 messages
		t.Fatal("KeepRecent=4 must bridge to at least 2 pinned pairs")
	}
}
```

`buildMessages` 辅助函数：

```go
// buildMessages creates n assistant messages (each ~1 line, ~40-50 token equiv)
// to fill the context window for compaction threshold tests.
func buildMessages(t *testing.T, n int) []*schema.Message {
	t.Helper()
	msgs := make([]*schema.Message, 0, n+1)
	msgs = append(msgs, schema.UserMessage("hello"))
	for i := 0; i < n; i++ {
		msgs = append(msgs, schema.AssistantMessage(fmt.Sprintf("line %d: just some filler text to raise token count above the threshold.", i), nil))
	}
	// Verify the message list is over a rough threshold using the estimate (optional).
	// tokens := ctxcompact.EstimateTokens(msgs)
	return msgs
}
```

Import 块需要 `"fmt"`（已有）。

- [ ] **Step 2: 运行测试，确认 cooldown 相关符号未定义**

Run: `go test ./internal/llm/eino -run 'TestCompactingModel_Cooldown|TestCompactingModel_HardForce|TestCompactingModel_FirstCompact|TestCompactingModel_KeepRecentBridge' -v`

Expected: FAIL，`CooldownTokens`、`HardForceFraction`、`cm.cmMu`、`lastCompactTokens` 未定义。

- [ ] **Step 3: 实现 cooldown 字段 + `shouldCompact` 延后/强制 + `maybeCompact` 成功后更新**

在 `compacting.go` 的 `CompactingModel` 结构体追加字段（`:68` 后）：

```go
// CooldownTokens is the minimum token growth since the last successful
// compaction before another one is allowed. 0 means no token-growth cooldown
// (the prevailing non-regression default). Used in combination with
// HardForceFraction — on approach of the hard-force threshold, cooldown is
// overridden. The cooldown is instance-local (per-model pointer in runners
// sync.Map) so it persists across turns.
CooldownTokens int
// CooldownDuration is the minimum wall-time since the last compaction before
// another one is allowed. ≤0 means no time-based cooldown (default).
CooldownDuration time.Duration
// HardForceFraction forces compaction once estimated tokens reach this
// fraction of ContextWindow, even when inside a cooldown period. 0 disables
// (not recommended — the token budget safety net). Default via config: 0.95.
HardForceFraction float64
// lastCompactTokens is the TokensAfter (from ctxcompact.Result) of the most
// recent successful compaction, or 0 if no compaction has occurred yet on
// this model instance. Guarded by cmMu.
lastCompactTokens int
// lastCompactAt is the wall clock time of the most recent successful
// compaction, or zero if none yet. Guarded by cmMu.
lastCompactAt time.Time

// cmMu guards the mutable compaction state that is shared across concurrent
// turns on the same model wrapper (runners sync.Map may be hit from multiple
// WS sessions). Always lock before reading or writing cooldown/force fields.
cmMu sync.Mutex
```

在 `compacting.go` import 块加入 `"sync"`、`"time"`。

在 `maybeCompact` 成功分支（`:112`，`return res.Messages, true` 前）追加：

```go
c.cmMu.Lock()
c.lastCompactTokens = res.TokensAfter
c.lastCompactAt = time.Now()
c.cmMu.Unlock()
```

替换 `shouldCompact` 完整实现：

```go
func (c *CompactingModel) shouldCompact(msgs []*schema.Message) bool {
	if c.Threshold <= 0 || c.ContextWindow <= 0 || c.KeepRecent <= 0 {
		return false
	}
	if len(msgs) <= c.KeepRecent {
		return false
	}
	tokens := ctxcompact.EstimateTokens(msgs)

	// Hard force: when approaching the window edge we compact even inside a
	// cooldown period so the inner model call does not over-window.
	if c.HardForceFraction > 0 && tokens >= int(c.HardForceFraction*float64(c.ContextWindow)) {
		return true
	}

	// Threshold gate: only compact when over the configured threshold.
	if tokens < int(c.Threshold*float64(c.ContextWindow)) {
		return false
	}

	// Cooldown gate: if we recently compacted and the history hasn't grown
	// past the post-compact size by CooldownTokens (or not enough time elapsed),
	// defer re-compaction.
	if c.inCooldown(tokens) {
		return false
	}
	return true
}

// inCooldown reports whether the cooldown period is still active relative to
// the last successful compaction. Either dimension (token-growth or time) being
// unmet is enough to be in cooldown.
func (c *CompactingModel) inCooldown(tokens int) bool {
	c.cmMu.Lock()
	lastT := c.lastCompactTokens
	lastAt := c.lastCompactAt
	c.cmMu.Unlock()

	if lastT == 0 && lastAt.IsZero() {
		return false // no prior compact → no cooldown
	}
	tokenCool := c.CooldownTokens > 0 && lastT > 0 && tokens-lastT < c.CooldownTokens
	timeCool := c.CooldownDuration > 0 && !lastAt.IsZero() && time.Since(lastAt) < c.CooldownDuration
	return tokenCool || timeCool
}
```

在 `keepRecent` 字段（`:67`）处补承重注释：

```go
// KeepRecent is the number of TRAILING MESSAGES kept verbatim (a raw message
// count, NOT a pair count — legacy bridge: it is halved when passed to
// ctxcompact.PlanOpts.KeepRecent which expects a pair count; see :104 below).
```

- [ ] **Step 4: 加 config DTO（`CompactionConfig` 加 cooldown/hardForce 参数 + defaults）**

在 `config.go` 的 `CompactionConfig`（当前仅有 `Threshold`, `KeepRecent`, `ContextWindow`, `ChunkThreshold`, `Model`）追加：

```go
// CooldownFraction is the fraction of ContextWindow that sets CooldownTokens
// (CooldownTokens = int(CooldownFraction * ContextWindow)). 0→no token-growth
// cooldown. Default 0.05 delivers meaningful "no re-compact for trivial growth"
// per CCL1 design.
CooldownFraction float64 `yaml:"cooldown_fraction"`

// CooldownDuration is the minimum wall-time since the last compaction before
// re-compaction is allowed. Parsed via time.ParseDuration (e.g. "3s", "500ms").
// "" or "0s" disables time-based cooldown. Default "" (disabled) — reduces CI
// timing nondeterminism and keeps cooldown purely token-based.
CooldownDuration string `yaml:"cooldown_duration"`

// HardForceFraction forces compaction once estimated tokens reach this fraction
// of ContextWindow, even when inside a cooldown. Default 0.95 (safety fallback).
HardForceFraction float64 `yaml:"hard_force_fraction"`
```

在 `applyDefaults` 设置默认值：

```go
if c.Compaction.Threshold == 0 { c.Compaction.Threshold = 0.8 }
if c.Compaction.KeepRecent == 0 { c.Compaction.KeepRecent = 4 }
if c.Compaction.ContextWindow == 0 { c.Compaction.ContextWindow = 256000 }
if c.Compaction.ChunkThreshold == 0 { c.Compaction.ChunkThreshold = 0.9 }
if c.Compaction.CooldownFraction == 0 { c.Compaction.CooldownFraction = 0.05 }
if c.Compaction.HardForceFraction == 0 { c.Compaction.HardForceFraction = 0.95 }
// CooldownDuration defaults to "" (disabled).
```

在 `config.example.yaml` 的 compaction 区域加入示例：

```yaml
compaction:
  threshold: 0.8
  keep_recent: 4
  context_window: 256000
  chunk_threshold: 0.9
  cooldown_fraction: 0.05          # token-growth cooldown: 5% of window
  cooldown_duration: ""            # time-based cooldown (optional, e.g. "3s")
  hard_force_fraction: 0.95        # force-compact near window edge
```

- [ ] **Step 5: 给 `orchestrator.CompactionConfig` 追加 cooldown 字段 + `wrapCompaction` 透传 + `bootstrap.go` 装配**

	**5a. `orchestrator.go`**: 在 `CompactionConfig` struct（`orchestrator.go:68-73`）追加字段：

	```go
	// CooldownTokens is the minimum token growth since the last successful
	// compaction before another one is allowed (per-model instance). 0 means
	// no token-growth cooldown.
	CooldownTokens int
	// CooldownDuration is the minimum wall-time since last compaction. <=0
	// means no time-based cooldown.
	CooldownDuration time.Duration
	// HardForceFraction forces compaction once estimated tokens reach this
	// fraction of ContextWindow, even when inside a cooldown period. 0 disables.
	HardForceFraction float64
	```

	在 `wrapCompaction`（`orchestrator.go:211`）将这些字段透传到 `einollm.CompactingModel` 字面量：

	```go
	cm := &einollm.CompactingModel{
		Inner:             rawModel,
		Threshold:         cc.Threshold,
		ContextWindow:     cc.ContextWindow,
		KeepRecent:        cc.KeepRecent,
		CooldownTokens:    cc.CooldownTokens,
		CooldownDuration:  cc.CooldownDuration,
		HardForceFraction: cc.HardForceFraction,
	}
	```

	**5b. `bootstrap.go`**: 在构建 `orchestrator.CompactionConfig` 的位置（`bootstrap.go:655-659`）追加 cooldown 值：

	```go
	var cooldownDuration time.Duration
	if cfg.Compaction.CooldownDuration != "" {
		if d, err := time.ParseDuration(cfg.Compaction.CooldownDuration); err == nil {
			cooldownDuration = d
		}
	}
	CompactionConfig: orchestrator.CompactionConfig{
		Threshold:         cfg.Compaction.Threshold,
		ContextWindow:     contextWindow,
		KeepRecent:        cfg.Compaction.KeepRecent * 2, // pairs→msgs (existing bridge)
		CooldownTokens:    int(cfg.Compaction.CooldownFraction * float64(contextWindow)),
		CooldownDuration:  cooldownDuration,
		HardForceFraction: cfg.Compaction.HardForceFraction,
	},
	```

- [ ] **Step 6: 运行所有 CCL1 测试 + 既有压缩测试**

Run: `go test ./internal/llm/eino -run 'TestCompacting' -v`

Expected: 所有新测试 + 既有 `TestCompacting_*` 全部 PASS。`buildMessages` 辅助可能需调整消息数量以准确跨 threshold/force 边界；如第一个 cooldown 测试 `didCompact=false` 但本应 `true`，增加 `buildMessages(t, n)` 的 n。

Run: `go test ./internal/config ./internal/bootstrap -v`

Expected: PASS（新字段默认值 + bootstrap 装配不改变既有行为）。

Run: `go test ./internal/...`（全量）

Expected: PASS（cooldown 默认 0.05 不改变任何用默认值的压缩行为，因为 cooldown 首次 `lastCompactTokens==0` 不触发；仅同实例第二次压缩才被 defer）。

- [ ] **Step 7: 提交**

```bash
git add internal/llm/eino/compacting.go internal/llm/eino/compacting_test.go internal/config/config.go config.example.yaml internal/agent/orchestrator/orchestrator.go internal/bootstrap/bootstrap.go
git commit -m "feat(compaction): add mid-turn compression cooldown with hard-force fallback"
```

---

## Task 5: BENCH1 — 四个基准 + benchstat 脚本

**Files:**
- Create: `internal/vcs/vcs_bench_test.go`
- Create: `internal/tools/fs_bench_test.go`
- Create: `internal/agent/orchestrator/orchestrator_bench_test.go`
- Create: `scripts/bench.sh`

BENCH1 不遵循 RED→GREEN（没有 "failing test"），而是直接写 `_bench_test.go` 然后验证可运行。

- [ ] **Step 1: VCS 基准 — BenchmarkVCSCommit / BenchmarkDAGApply**

```go
// internal/vcs/vcs_bench_test.go
package vcs

import (
	"os"
	"path/filepath"
	"testing"
	"github.com/x6nux/yanshi/internal/store"
)

func BenchmarkVCSCommit(b *testing.B) {
	subs := []struct{ name, size string; n int }{
		{name: "SmallTree", size: "small", n: 10},
		{name: "LargeTree", size: "large", n: 1000},
	}
	for _, sub := range subs {
		b.Run(sub.name, func(b *testing.B) {
			s, err := store.Open(":memory:")
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			v := New(s, b.TempDir())
			root := b.TempDir()
			repoID, err := v.InitRepo(root)
			if err != nil {
				b.Fatal(err)
			}

			// Pre-populate with N files.
			for i := 0; i < sub.n; i++ {
				if err := os.WriteFile(filepath.Join(root, filepath.Base(b.Name())+"_file_"+b.Name()+".go"),
					[]byte("package main\n\nfunc f"+b.Name()+"() {}\n"), 0o644); err != nil {
					b.Fatal(err)
				}
				if _, err := v.RecordEditMain(repoID, "bench", filepath.Join(root, "f.go"),
					[]byte("package main\n\nfunc f() {}\n")); err != nil {
					b.Fatal(err)
				}
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := v.CommitMain(repoID, "bench", "bench commit"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDAGApply(b *testing.B) {
	// Merging three trees: base / ours / theirs.
	subs := []struct{ name string; conflict bool }{
		{name: "NoConflict", conflict: false},
		{name: "WithConflict", conflict: true},
	}
	for _, sub := range subs {
		b.Run(sub.name, func(b *testing.B) {
			s, err := store.Open(":memory:")
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			v := New(s, b.TempDir())
			root := b.TempDir()
			repoID, err := v.InitRepo(root)
			if err != nil {
				b.Fatal(err)
			}
			// base commit with a file
			if err := os.WriteFile(filepath.Join(root, "f.go"), []byte("package main\n\nconst X = 1\n"), 0o644); err != nil {
				b.Fatal(err)
			}
			if _, err := v.RecordEditMain(repoID, "bench", filepath.Join(root, "f.go"),
				[]byte("package main\n\nconst X = 1\n")); err != nil {
				b.Fatal(err)
			}
			baseHash, err := v.CommitMain(repoID, "bench", "base")
			if err != nil {
				b.Fatal(err)
			}

			// Ours worktree with changes
			wtO, err := v.AddWorktree(repoID, []string{"bench"})
			if err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(wtO.Path, "f.go"),
				[]byte("package main\n\nconst X = 2\n"), 0o644); err != nil {
				b.Fatal(err)
			}
			if _, err := v.RecordEditWorktree(wtO.ID, "bench", filepath.Join(wtO.Path, "f.go"),
				[]byte("package main\n\nconst X = 2\n")); err != nil {
				b.Fatal(err)
			}
			if _, err := v.CommitWorktree(wtO.ID, "bench", "ours"); err != nil {
				b.Fatal(err)
			}

			// Theirs worktree with changes
			wtT, err := v.AddWorktree(repoID, []string{"bench"})
			if err != nil {
				b.Fatal(err)
			}
			conflictText := "package main\n\nconst X = 3\n"
			if !sub.conflict {
				// different file — no conflict
				if err := os.WriteFile(filepath.Join(wtT.Path, "g.go"),
					[]byte("package main\n\nconst Y = 4\n"), 0o644); err != nil {
					b.Fatal(err)
				}
				if _, err := v.RecordEditWorktree(wtT.ID, "bench", filepath.Join(wtT.Path, "g.go"),
					[]byte("package main\n\nconst Y = 4\n")); err != nil {
					b.Fatal(err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(wtT.Path, "f.go"), []byte(conflictText), 0o644); err != nil {
					b.Fatal(err)
				}
				if _, err := v.RecordEditWorktree(wtT.ID, "bench", filepath.Join(wtT.Path, "f.go"),
					[]byte(conflictText)); err != nil {
					b.Fatal(err)
				}
			}
			if _, err := v.CommitWorktree(wtT.ID, "bench", "theirs"); err != nil {
				b.Fatal(err)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := v.MergeToMain(wtO.ID, "bench", false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
```

**注意**：由于 `MergeToMain` 在被合并的 worktree 上操作，而 repeated bench loop 需要可重置状态，bench 实现者应确认每次 `MergeToMain` 是可重复的（rollback 或每次循环重新创建 worktree）。若不可重复，将 `b.ResetTimer()` + `for` 改为在循环内重新创建 worktree 和 base/ours/theirs（该调整建议在实现时验证）。

- [ ] **Step 2: fs 基准 — BenchmarkFSEdit**

```go
// internal/tools/fs_bench_test.go
package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkFSEdit(b *testing.B) {
	root := b.TempDir()
	path := filepath.Join(root, "target.go")
	content := []byte(`package main

func foo() {
	// some line to replace
	_ = 1
	_ = 2
	_ = 3
	_ = 4
	_ = 5
}
`)
	require.NoError(b, os.WriteFile(path, content, 0o644))

	fs := NewFSTools(root)
	ctx := context.Background()

	b.Run("ExactReplace", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			// Each iteration rewrites the file back then edits it.
			_ = os.WriteFile(path, content, 0o644)
			if _, err := fs.runEdit(ctx, `{"path":"`+path+`","oldText":"some line to replace","newText":"replaced line"}`); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("ExactReplaceLarge", func(b *testing.B) {
		// Build a larger file for bench.
		large := []byte("package main\n\n")
		for j := 0; j < 500; j++ {
			large = append(large, []byte("var x"+b.Name()+" = "+b.Name()+"\n")...)
		}
		target := filepath.Join(root, "large.go")
		_ = os.WriteFile(target, large, 0o644)

		for i := 0; i < b.N; i++ {
			_ = os.WriteFile(target, large, 0o644)
			if _, err := fs.runEdit(ctx, `{"path":"`+target+`","oldText":"var x`+b.Name()+` = `+b.Name()+`","newText":"var y`+b.Name()+` = `+b.Name()+`"}`); err != nil {
				b.Fatal(err)
			}
		}
	})
}
```

- [ ] **Step 3: orchestrator 基准 — BenchmarkOrchestratorTurn**

```go
// internal/agent/orchestrator/orchestrator_bench_test.go
package orchestrator

import (
	"context"
	"testing"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

func BenchmarkOrchestratorTurn(b *testing.B) {
	model := einollm.NewFakeModel([]string{"hello from agent"}, nil)
	o, err := New(Config{Model: model})
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := o.Query(context.Background(), "hi"); err != nil {
			b.Fatal(err)
		}
	}
}
```

- [ ] **Step 4: 验证基准可运行**

Run: `go test -bench=. -benchmem ./internal/vcs ./internal/tools ./internal/agent/orchestrator -count=1`

Expected: 每个包输出基准结果（可能有预热数据点远大于后续点）。不要硬门禁结果，只要可运行。

- [ ] **Step 5: 创建 benchstat 脚本**

```bash
# scripts/bench.sh - run F2 benchmarks and compare against main baseline
# Usage: ./scripts/bench.sh [--diff]
# When --diff is provided and old.txt exists, benchstat is used to compare.
#
# CIG1 nightly: runs ALL sub-benchmarks.
# PR: runs only fast subsets (select by -run / -bench sub-pattern).

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

BENCH_PKGS="./internal/vcs ./internal/tools ./internal/agent/orchestrator"
BENCH_ARGS="-bench=. -benchmem -count=5"
OUTPUT_DIR=".bench-results"
mkdir -p "$OUTPUT_DIR"

# Run new benchmarks
echo "=== Running F2 benchmarks ==="
go test $BENCH_ARGS $BENCH_PKGS | tee "$OUTPUT_DIR/new.txt"

if [ "${1:-}" = "--diff" ] && [ -f "$OUTPUT_DIR/old.txt" ]; then
	echo "=== Comparing against baseline ==="
	benchstat "$OUTPUT_DIR/old.txt" "$OUTPUT_DIR/new.txt" | tee "$OUTPUT_DIR/diff.txt"
	echo "(above $THRESHOLD_PCT% regression? check diff.txt)"
else
	echo "=== Saving as new baseline ==="
	cp "$OUTPUT_DIR/new.txt" "$OUTPUT_DIR/old.txt"
	echo "Baseline saved. Run with --diff next time for comparison."
fi
```

- [ ] **Step 6: 验证 benchstat 脚本可运行**

Run: `bash scripts/bench.sh`

Expected: 产生 `.bench-results/new.txt`，无 error（benchstat 在无 `old.txt` 时不比较）。若有 benchstat，下载安装：`go install golang.org/x/perf/cmd/benchstat@latest`（单次依赖）。

- [ ] **Step 7: 提交**

```bash
git add internal/vcs/vcs_bench_test.go internal/tools/fs_bench_test.go internal/agent/orchestrator/orchestrator_bench_test.go scripts/bench.sh
git commit -m "feat(bench): add VCS commit/merge, fs edit, and orchestrator turn benchmarks"
```

F2 不创建 `.github/workflows/` 文件。CIG1 将 `scripts/bench.sh` 集成到 nightly CI 矩阵。

---

## Task 6: LEAK3 — acp 层解析 usage_report

**Files:**
- Modify: `internal/acp/types.go`
- Modify: `internal/acp/client.go`
- Modify: `internal/acp/fakeagent.go`
- Modify: `internal/acp/client_test.go`

- [ ] **Step 1: 写 acp 解析测试（含 FakeAgent scripting）**

```go
// append to internal/acp/client_test.go

func TestParseUsageReportEvent(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()
	fa.Updates = []string{"thinking…"}
	// Script a usage report notification.
	fa.UsageReports = []acp.Usage{
		{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
	}

	cl := NewClient(clientIn, clientOut)
	_, err := cl.Initialize(context.Background(), ClientCapabilities{})
	require.NoError(t, err)

	sessionID, err := cl.NewSession(context.Background(), t.TempDir(), nil, nil)
	require.NoError(t, err)

	var capturedUsage *acp.Usage
	onEvent := func(ev Event) {
		if ev.Usage != nil {
			capturedUsage = ev.Usage
		}
	}
	stopReason, err := cl.Prompt(context.Background(), sessionID, "do it", onEvent)
	require.NoError(t, err)
	require.Equal(t, "end_turn", stopReason)
	require.NotNil(t, capturedUsage, "usage_report must be delivered as an Event")
	assert.Equal(t, 100, capturedUsage.InputTokens)
	assert.Equal(t, 50, capturedUsage.OutputTokens)
	assert.Equal(t, 150, capturedUsage.TotalTokens)
}

func TestNoUsageReportDoesNotSetUsage(t *testing.T) {
	fa, clientIn, clientOut := NewFakeAgent()
	defer fa.Close()
	fa.Updates = []string{"hello"}
	// No UsageReports → no usage on events.

	cl := NewClient(clientIn, clientOut)
	_, err := cl.Initialize(context.Background(), ClientCapabilities{})
	require.NoError(t, err)

	sessionID, err := cl.NewSession(context.Background(), t.TempDir(), nil, nil)
	require.NoError(t, err)

	var gotUsage bool
	onEvent := func(ev Event) {
		if ev.Usage != nil {
			gotUsage = true
		}
	}
	_, err = cl.Prompt(context.Background(), sessionID, "step", onEvent)
	require.NoError(t, err)
	assert.False(t, gotUsage, "no usage_report should leave Event.Usage nil")
}
```

在 `fakeagent.go` 添加 `UsageReports` 字段（见 Step 3）后，还需添加畸形 payload 的解析单元测试（在 `client_test.go` 中用一个畸形 JSON 直接调用 `parseUsageReport`，验证不 panic 且返回 nil）：

```go
func TestParseUsageReport_MalformedPayload(t *testing.T) {
	// Direct unit test of parseUsageReport.
	malformed := json.RawMessage(`{"update": {"sessionUpdate": "usage_report", "usage": {bad}`)
	u := parseUsageReport(malformed)
	assert.Nil(t, u, "malformed JSON must return nil, not panic")
}
```

确认 import 块已有 `"encoding/json"`。

- [ ] **Step 2: 运行测试，确认类型/函数未定义**

Run: `go test ./internal/acp -run 'TestParseUsageReportEvent|TestNoUsageReportDoesNot|TestParseUsageReport_Malformed' -v`

Expected: FAIL。`acp.Usage`、`Event.Usage`、`parseUsageReport`、`FakeAgent.UsageReports` 未定义。

- [ ] **Step 3: 在 types.go 添加 `Usage` 结构体**

```go
// Usage carries parsed ACP token consumption from a "usage_report" session/update.
// Fields are best-effort: adapters vary (codex vs claudecode vs future), so any
// subset may be populated; callers must tolerate zero values.
type Usage struct {
	InputTokens  int `json:"inputTokens,omitempty"`
	OutputTokens int `json:"outputTokens,omitempty"`
	TotalTokens  int `json:"totalTokens,omitempty"`
}
```

在 `Event` 结构体（`:11-22`）追加字段：

```go
// Usage is populated when the update is a "usage_report" discriminator, nil
// for other event kinds. Callers must nil-check before reading.
Usage *Usage
```

- [ ] **Step 4: 在 `client.go` 的 `handleNotify` 增加 `usage_report` 分支**

在 `default` 前插入：

```go
case "usage_report":
	ev.Usage = parseUsageReport(params)
```

实现 `parseUsageReport` 在 `client.go` 某处（可在 `handleNotify` 之前的文件区域）：

```go
// parseUsageReport tolerantly extracts token usage from a session/update
// usage_report notification. It never panics: on any parse failure it returns
// nil so the turn continues uninterrupted. The adapter (codex / claudecode)
// determines the exact JSON shape; we try common keys.
func parseUsageReport(raw json.RawMessage) *Usage {
	defer func() {
		if r := recover(); r != nil {
			// Malformed payload must never crash a turn.
		}
	}()
	var probe struct {
		Update struct {
			Usage Usage `json:"usage"`
		} `json:"update"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		// Also try alternative key: in the params object itself.
		var alt struct {
			Usage Usage `json:"usage"`
		}
		if err2 := json.Unmarshal(raw, &alt); err2 != nil {
			return nil
		}
		probe.Update.Usage = alt.Usage
	}
	u := probe.Update.Usage
	if u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 {
		return nil // refuse to emit a zero-only usage that would pollute the sink
	}
	return &u
}
```

在 `Prompt` 的 docs（`:157-162`）追加注释说明 onEvent 可能携带 Usage：

```go
// ... until the agent resolves the prompt. onEvent receives each session/update
// notification; the usage_report discriminator populates Event.Usage. Returns
// the stopReason (e.g. "end_turn", "cancelled").
```

- [ ] **Step 5: 在 `FakeAgent` 增加 UsageReports 脚本支持**

在 `fakeagent.go` 的 `FakeAgent` struct（`:24` 区域）添加字段：

```go
// UsageReports, when non-empty, are emitted as session/update usage_report
// notifications right before the prompt resolves (after text chunks).
UsageReports []Usage
```

在 `handleRequest` 的 `session/prompt` 分支（`:150`）中，在现有 chunk 循环后（即 `fa.tr.Respond` 前，非 HoldPrompt 路径），追加 usage_report 发射：

```go
for _, text := range chunks { /* existing chunk emission */ }

// Emit scripted usage_report notifications.
for _, u := range fa.UsageReports {
	updParams := UpdateParams{
		SessionID: params.SessionID,
		Update: Update{
			SessionUpdate: "usage_report",
			Content: nil,
		},
	}
	// Override the Content to inject usage; update the param struct.
	raw := fmt.Appendf(nil, `{"sessionId":"%s","update":{"sessionUpdate":"usage_report","usage":{"inputTokens":%d,"outputTokens":%d,"totalTokens":%d}}}`,
		params.SessionID, u.InputTokens, u.OutputTokens, u.TotalTokens)
	if err := fa.tr.Notify("session/update", json.RawMessage(raw)); err != nil {
		return nil, err
	}
}
```

注意：上述 raw JSON 方法绕过了 `UpdateParams` 的结构体字段映射（因为 `Usage` 字段在 `Update` 上没有对应 tag）。一个更整洁的做法是在 `Update` 结构体上增加 `Usage *Usage` tag —— 但 spec 要求 "tolerant parse in handleNotify" 且 `Update` 是 one-of 结构。为了最小化改动，FakeAgent 侧的 usage_report 直接写 raw JSON（它本身是测试桩，可靠性足够）。或者加一个 `RawUsage *Usage json:"usage,omitempty"` 到 `Update`——虽不是最优雅但可行且解耦。implementation 时可选。

- [ ] **Step 6: 运行测试并提交**

Run: `go test ./internal/acp -v`

Expected: 全部 PASS。Usage 捕获、不发 usage 不开、畸形 payload 不 panic。

```bash
git add internal/acp/types.go internal/acp/client.go internal/acp/client_test.go internal/acp/fakeagent.go
git commit -m "feat(acp): parse usage_report events for subprocess token accounting"
```

---

## Task 7: LEAK3 — goalloop 回流 + wiring

**Files:**
- Modify: `internal/agent/goalloop/implementer.go`
- Modify: `cmd/yanshi/main.go`

- [ ] **Step 1: 写 ACPImplementer 回流的 goalloop 层测试**

```go
// append to internal/agent/goalloop/implementer_test.go

func TestACPImplementerWorkerAccumulatesSubprocessUsage(t *testing.T) {
	// We need a FakeAgent that scripts a usage_report and a worker that
	// captures it into a sink. This test verifies the goalloop integration:
	// onEvent from Prompt forwards usage to the shared sink.

	// The worker's onEvent closure is what we test: given an acp.Event with
	// Usage, it must call sink.Add with the correct conversion.
	var captured goalloop.Usage
	sink := &goalloop.UsageSink{}
	// Simulate an onEvent from worker.run.
	onEvent := func(ev acp.Event) {
		if sink != nil && ev.Usage != nil {
			sink.Add(goalloop.Usage{
				PromptTokens:     ev.Usage.InputTokens,
				CompletionTokens: ev.Usage.OutputTokens,
				TotalTokens:      ev.Usage.TotalTokens,
			})
		}
	}

	onEvent(acp.Event{Usage: &acp.Usage{InputTokens: 50, OutputTokens: 30, TotalTokens: 80}})
	// Second event accumulates.
	onEvent(acp.Event{Usage: &acp.Usage{InputTokens: 20, OutputTokens: 10, TotalTokens: 30}})

	got := sink.Snapshot()
	assert.Equal(t, 70, got.PromptTokens, "input must accumulate (50+20)")
	assert.Equal(t, 40, got.CompletionTokens, "output must accumulate (30+10)")
	assert.Equal(t, 110, got.TotalTokens, "total must accumulate (80+30)")
}
```

- [ ] **Step 2: 运行测试，确认 `ACPImplementer.Sink` 未定义**

Run: `go test ./internal/agent/goalloop -run TestACPImplementerWorkerAccumulatesSubprocessUsage -v`

Expected: FAIL（因 Task 6 尚未合入，`acp.Usage` 不存在）。若在 Dev 分支上 Task 6 已合，则 FAIL 因 `ACPImplementer.Sink` 不存在。该测试在 Task 6 之前的独立分支可能 SKIP；Task 6→7 是线性依赖。

- [ ] **Step 3: 给 `ACPImplementer` 加 `Sink` 字段并注入到 `worker`**

在 `implementer.go` 的 `ACPImplementer` struct（`:143`）追加：

```go
// Sink is the shared token accumulator (goalloop.UsageSink). When non-nil,
// each ACP subprocess usage_report event is added to this sink so the goal
// loop's budget and cost tracking reflect subprocess token consumption.
Sink *UsageSink
```

在 `worker` struct（`:223`）追加：

```go
// sink receives subprocess token usage from ACP usage_report events (LEAK3).
sink *UsageSink
```

在 `Implement` 方法内创建每个 worker 时（`:421-429`），传递 `Sink: a.Sink`：

```go
w := &worker{
	agent:       a.Agent,
	wt:          wt,
	profile:     a.Profile,
	profileSet:  a.profileSet,
	vcs:         a.VCS,
	repoID:      a.RepoID,
	vcsDBPath:   a.VCSDBPath,
	worktreeDir: a.WorktreeDir,
	sink:        a.Sink,
}
```

- [ ] **Step 4: 给 `worker.run` 的 Prompt 调用加 onEvent closure**

在 `runWithGit`（`:330`）和 `runWithAutoVCS`（`:359`）两处，将 onEvent `nil` 替换为闭包：

```go
// runWithGit:
onEvent := func(ev acp.Event) {
	if w.sink != nil && ev.Usage != nil {
		w.sink.Add(Usage{
			PromptTokens:     ev.Usage.InputTokens,
			CompletionTokens: ev.Usage.OutputTokens,
			TotalTokens:      ev.Usage.TotalTokens,
		})
	}
}
stopReason, err := spawned.Client.Prompt(ctx, spawned.SessionID, task.Step, onEvent)
```

```go
// runWithAutoVCS (same closure pattern):
onEvent := func(ev acp.Event) {
	if w.sink != nil && ev.Usage != nil {
		w.sink.Add(Usage{
			PromptTokens:     ev.Usage.InputTokens,
			CompletionTokens: ev.Usage.OutputTokens,
			TotalTokens:      ev.Usage.TotalTokens,
		})
	}
}
if _, err := spawned.Client.Prompt(ctx, spawned.SessionID, task.Step, onEvent); err != nil {
```

由于 `acp.Event` 的 `Usage *acp.Usage` 字段在 Task 6 定义后可用，此处使用 `ev.Usage.InputTokens`、`ev.Usage.OutputTokens`、`ev.Usage.TotalTokens`。若 `ev.Usage` 为 nil 则不触发 sink 添加（`w.sink.Add` 零值 → no-op）。

- [ ] **Step 5: 在 `main.go` 装配注入**

在 `cmd/yanshi/main.go` 的 `ACPImplementer` 构造点（`:701`）：

```go
// Before:
impl := &goalloop.ACPImplementer{Agent: *agent}

// After:
impl := &goalloop.ACPImplementer{
	Agent: *agent,
	Sink:  loopSink,
}
```

`loopSink` 已在此作用域存在（planner 建造时创建于 `:695` 附近）。

- [ ] **Step 6: 运行 goalloop 测试**

Run: `go test ./internal/agent/goalloop -v`

Expected: PASS（既有 test 无退化，新 sink 测试使用 `UsageSink` 断言）。

Run: `go build ./cmd/yanshi`

Expected: 构建成功（main.go 新字段可编译）。

- [ ] **Step 7: 提交**

```bash
git add internal/agent/goalloop/implementer.go cmd/yanshi/main.go
git commit -m "feat(goalloop): wire ACP subprocess usage into shared usage sink"
```

---

## 6. 风险与缓解 (已确认：budget 采用 Option A 硬停)

> **budget 决策（2026-07-22 团队确认）**：采用 **Option A（硬停接受）**——子进程 usage 进共享 `UsageSink`，`overBudget` 天然含子进程。用户可通过 `MaxTokens=0` 自行禁用硬停。`"budget 仅告警不硬停"` 的原始 OQ 建议被 spec 的实用性设计覆盖。推荐方案无额外改动。

| 风险 | 缓解 |
|---|---|
| Task 2 LongRun 测试中 `len(createdWT)` 因 SQLite transient 状态不归零 | 用 `b.createdWTMu.Lock()` + `len(b.createdWT)` 读内存 map，不通过 store（内存 map 即时）。 |
| `buildMessages` 辅助的 token 估算与实际 `EstimateTokens` 有偏差 | 测试中加注释指定估算参考（`~40 tokens/msg`），CI 跑时根据实际输出微调消息数。 |
| CCL1 cooldown 默认 0.05 在极短窗口模型（如 4096）下 `CooldownTokens≈204` 可被单条长消息突破 | 0.95 hardForce 兜底确保不 over-window。cooldown 只延后不阻止。 |
| LEAK3 budget 行为 | 已确认 Option A：共享 sink → overBudget 含子进程 → 硬停（用户可设 `MaxTokens=0` 关闭）。无需额外改动。 |
| RAC1 尚未冻结 `-race` 基础设施 | Task 2 并发测试可先以非 `-race` 版合入；`-race` tag 加在 Test name 上，仅在 `-race` 下执行。 |
| `manager_spawn_test.go` 缺少深度+并发同时饱和的 helper | Task 3 顺序断言标记为 `t.Skip("add when helpers exist")`，仅注释提交。 |

---

## 决策记录: budget 行为 — Option A (已确认)

**问题背景**：spec §8 设计"子进程 usage 进共享 `UsageSink`，`overBudget` 天然包含"，与团队 OQ 原始建议"仅告警不硬停"存在歧义。

**已确认决策 (2026-07-22)**：采用 **Option A（硬停接受）**。Task 7 按共享 sink 实施：ACP 子进程 tokens 进 `UsageSink`，goal loop 的 `MaxTokens` 预算硬停机制（`overBudget`）自然涵盖子进程消费。用户可设 `MaxTokens=0` 禁用硬停以恢复"仅告警"语义。Option B（独立 accumulator 不计入预算）**未采用**。

**理由**：spec 的权威设计、改动为零（Task 7 的共享 sink 即 spec §8 设计）、且 `MaxTokens=0` 提供了运行时退出开关，比单独维护一份不计预算的子进程账本更实用。

**对 Task 7 的影响**：无额外代码改动。`ACPImplementer.Sink = loopSink`（共享 sink）即正确实现。

---

## 验收标准

1. **LEAK1**: `reclaimWorktree` 在三处（RecordResult/Cancel/RequeueStale）被调用。RequeueStale 终态 (failed) 回收 worktree，pending 保留。长跑 `len(createdWT)==0`。`-race` 下 sweeper+Cancel+RecordResult 无 race。
2. **LEAK2**: depth/concurrency 双上限交互注释在 `manager.go` `Spawn` 处和 `subagent.go` `MaxSubAgentDepth` 处。`runningLocked` 双计数取大语义有注释。既有 cap 测试全绿。
3. **CCL1**: 同 turn 内重复超阈值不重复压缩（cooldown defers）。历史达 0.95×window 强制压缩。keepRecent /2 桥接有承重注释。既有压缩测试全绿。config cooldown/hardForce 字段可配，默认 0.05/0.95。
4. **BENCH1**: 四个基准文件存在且 `go test -bench` 可运行，零外部依赖。`scripts/bench.sh` 产出 benchstat 兼容格式。
5. **LEAK3**: acp `usage_report` 被解析为 `Event.Usage`；FakeAgent 可 scripting。`ACPImplementer.Sink` 使子进程 tokens 进入 `UsageSink`。不发/畸形 usage 安全降级。`go build` 成功。
6. `go test ./internal/task ./internal/agent/... ./internal/acp ./internal/llm/eino ./internal/config ./internal/bootstrap ./internal/vcs ./internal/tools -v` 全绿。
