# Batch B1 — 子代理增强（M04 / M05 / M04b）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:test-driven-development` and `superpowers:executing-plans`; complete one task at a time and do not batch commits.

**Goal:** 在保留 `agent_start`、`workflow_start`、`analysis`、`summarize` 兼容行为的前提下，增加可立即返回 ID 的异步子代理、完整生命周期、统一 runtime registry、七种角色、持久化、并发上限、typed lifecycle events，以及五段式输出/working-set 契约。

**Architecture:** `internal/agent/registry` 继续保留静态 `Registry`，同包新增 process-wide runtime `Manager`。所有子代理入口（同步 `agent_start`、flat/DAG `workflow_start`、`analysis`、异步 `agent_spawn`）先向同一个 Manager 申请 running slot；Manager 在返回 ID 和启动 goroutine **之前**把 `Running` 原子持久化到 `~/.yanshi/subagents.v1.json`。后台执行上下文派生自 Manager root，而不是会随 `GuardedTool.Stream` 关闭的 turn context；父子 agent context 形成取消树。`tools` 负责角色、override policy、工具面及 Manager adapter；`orchestrator` 负责把具体 model/tool runner 绑定到 context；`bootstrap.Build(Options)` 是唯一组合根。生命周期事件统一变成 `proto.ServerFrame{Type:"subagent_event"}`，WS/SSE 只订阅当前请求边界，连接结束不取消已 spawn agent。

**Tech Stack:** Go 1.26.4；标准库 `context`、`sync`、`encoding/json`、`os`、`path/filepath`、`time`；现有 Eino ADK、`GuardedTool`、`PermissionProfile`、`proto.ServerFrame`、`einollm.FakeModel`。不增加第三方依赖。

**Spec:** `docs/feature-roadmap-codex-deepseek.md` §0、§0.3、§6 Batch B1；角色与生命周期对照 `reference/deepseek-tui/docs/SUBAGENTS.md` 和 `reference/deepseek-tui/crates/tui/src/tools/subagent/`。

---

## 已锁定设计（执行中不得临时改口）

1. **C1 freeze 优先：** `registry.Manager` 的公开合同必须精确为下方矩阵；B1 的 role/custom/model/parent/depth/event 字段只能 additive 添加到 request/record，不得改变 C1 已消费的方法签名。
2. **兼容 API：** 保留现有 `tools.SubAgentRunner` 签名和 `NewAgentTools(chatModel)`；新增 managed context/API，不删除旧入口。`agent_start` 仍同步返回结果，但内部走 Manager。
3. **唯一运行态：** static catalog `registry.Registry` 与 runtime `registry.Manager` 并存；`registry` 不导入 `tools` 或 `orchestrator`。
4. **并发：** 默认 10，`MaxConcurrent` 必须在 `1..20`（0 取默认值）；只计 `Running`。slot 检查与 reserve 在同一写锁中；满载返回 `*SpawnErrCap{Cap: ...}`，让 C1 用 `errors.As` 重试。`workflow_start` 是同步入口，不能把 cap 当 terminal error：每个 step 在 ctx 可取消的 slot 等待队列中阻塞重试。
5. **持久化严格度：** `Spawn`、`Resume`、`Assign`、usage 更新必须持久化成功后才向调用方报告成功；失败则回滚内存。实际执行已经结束时无法回滚，terminal 写盘失败采用“live terminal + `persistence_failed` event + terminal event”的软降级。
6. **锁序：** 所有持久化 mutation 固定 `persistMu → mu → snapshot → unlock mu → temp+atomic-replace → unlock persistMu`；禁止反序，防止旧快照覆盖新状态。`EventSink` 必须在所有 Manager 锁之外调用。
7. **关闭门：** `Manager` 有 `atomic.Bool` closed gate；`Close()` 先 CAS 关门，再取消 root、等待 goroutine、最后 best-effort 持久化。`Close()` 无返回值；close/persist 错误通过 `persistence_failed` 事件/内部记录报告，不改变 C1 合同。`Spawn`/`Resume` 在 reserve 前检查 closed gate。
8. **重启：** loader 接受未知 JSON 字段；旧记录没有 `session_boot_id` 时视为 prior session。磁盘上的 `Pending`/`Running` 在新进程加载为 `Interrupted`。每次进程生成新 boot ID；loader 不能因为 state 顶层 boot ID 与当前一致就跳过中断修复。
9. **Resume：** 持久化并复用 `Prompt`（请求未提供新 prompt 时 fallback）、`Assignment`、`AllowedTools`、`Instruction`、`Role`、custom role 配置、model/reasoning override、`Depth`、`ParentID`。恢复前重新按当前 profile/model registry/role catalog fail-closed 校验；检查 cap、closed gate；reserve runtime 与 running record 后持久化成功才起 goroutine；失败恢复完整旧 record/runtime。若 parent 当前仍 running，则恢复其 cancellation tree；否则从 Manager root 派生但保留持久化 ParentID/Depth。
10. **角色只能收紧：** role policy 在 session always-allow、static guard、interactive callback 之前检查。Explore/Review/Verifier 保留 `shell_run`，但只允许 `safeShellCommands`；Plan 仅可写计划产物路径并只执行只读 shell；Implementer/General 也不能越过 parent profile。`RoleDef.Policy` 用 `*RolePolicy`：`nil` 表示不增加 role 限制，非 nil 的空 policy 表示“限制存在但没有额外 shell/write 限制”；不能让 nil slice 被误解成“禁止所有写”而使 General/Implementer失去 parent 允许的写权限。
11. **角色 catalog 必须接入执行：** `agent_spawn`/Resume 先校验 role（custom 必须携带 custom 配置），把 `PromptPrefix + Instruction + assignment/prompt` 组成实际 prompt；effective tools = role allowlist ∩ caller requested allowlist ∩ parent 可用工具，空 caller allowlist表示继承但仍受 role 限制。concrete runner 必须绑定 RolePolicy、AgentID、Depth；Resume 按持久化角色重放同样约束。
12. **Override：** Spawn 同步验证；Resume 重验。model 必须同时存在于 provider registry且匹配 profile allowlist；reasoning 只允许 `off|low|medium|high` 且不超过 profile cap。非法请求不创建 Failed record。concrete runner 必须把 `ModelOverride` 解析为实际 `model.BaseChatModel` 并把 `ReasoningEffort` 绑定到 nested turn；不能只验证后忽略。
13. **Parent/depth：** 新增 `WithCurrentAgentID`/`CurrentAgentID`。`agent_spawn`/managed adapter 未显式给 ParentID 时从 context 自动取；Manager 根据 live parent 算 `Depth=parent.Depth+1`，root 为 0；`Depth > tools.MaxSubAgentDepth` 在起 goroutine前返回 `ErrTooDeep`。parent cancellation 通过 Manager cancellation tree级联。
14. **usage：** concrete runner 每次 nested model call累计 normalized usage；`agent_list`/`agent_result` 都可查询。`AddUsage` 写盘失败必须回滚并在锁外发 `persistence_failed`（不静默只返回 error）。
15. **输出契约：** completed result必须含 `SUMMARY/CHANGES/EVIDENCE/RISKS/BLOCKERS`；同步 `agent_start` 把实际解析出的 `EVIDENCE` 路径/引用作为 `ParentWorkingSetHint` 附加给 parent；不能原样透传后谎称“已提取”。
16. **started 顺序：** `runAgentLoop` 的第一件可观察动作是在调用 runner 前发 `started`；`Spawn` 自身不和 goroutine竞争另发一次 started，确保 persisted Running → started → runner。
17. **Assign/mailbox：** Assign 成功落盘后必须把 assignment enqueue 到 runtime mailbox（满载返回明确错误并回滚 assignment+磁盘，或用有界阻塞选择 ctx——实现必须选定并测试）；不能只改 record。下一轮 runner 的 `assignment` 参数取该值。
18. **传输生命周期：** WS/SSE disconnect 只 detach event relay，不 bulk-cancel agent。agent继续运行和持久化，可由后续 `agent_list`/`agent_result` 查询。SSE 只有 handler goroutine写 `ResponseWriter`；bounded relay 满时可丢非 terminal progress，但 terminal/persistence failure 必须进入不会静默丢失的保留队列（动态有界 spill/同步交接 + request-close drain，不得 `default:` 丢 terminal）。
19. **WS detach：** relay `Emit` 在持有 `RLock` 时调用 writer；`Detach` 取写锁，因此会等待所有 in-flight writes结束后清空 writer，避免“复制 writer 后 unlock，Detach 返回后仍写 conn”的竞态。
20. **CLI/TUI：** `proto.ServerFrame`、`cli.StreamEvent`、WS/SSE backend 与 TUI 一起接入 `AgentID/AgentRole/AgentEvent/AgentStatus`；`subagent_event` 不是 control reply；TUI 用稳定 AgentID 更新/创建 `subagentEntry`，展示 role/status/text。
21. **持久化替换：** Unix 可 `os.Rename`；Windows 已存在目标时必须使用 `MoveFileEx(..., MOVEFILE_REPLACE_EXISTING|MOVEFILE_WRITE_THROUGH)`（放 build-tagged helper），不可依赖 Windows 上不可靠的 `os.Rename(tmp, existing)`。
22. **关闭：** `Manager.Close()` 无返回值，`App.Shutdown` 调它；全计划与 C1 只用这一签名。
23. **受管运行器上下文重绑定：** Manager 派生给 child goroutine 的 context **仅用于 cancellation tree**（`context.WithCancel(parentCtx)`），不保留 turn 作用域的 profile/workroot/VCS/model/emit。`ManagedRunnerFactory` 由 orchestrator 在每个 turn 入口点（Query/Events/EventsWithHistory/EventsWithHistoryOpts）构造，捕获当前 turn 的 profile/workroot/VCS/depth/emit。factory 产生的 `managedTurnRunner.Run` 在 Manager 的 cancellation-only ctx 上显式重绑这些值（`WithProfile`/`WithWorkRoot`/`WithVCS`/`WithSubAgentEmit`/`WithSubAgentDepth`/`registry.WithCurrentAgentID`），然后才委托 `runSubAgentTurn`。不重绑时嵌套 turn 拿不到 profile/VCS/runner，会裸跑丢上下文。

### 最终类型/签名矩阵（C1 freeze）

```go
// Wait 是 C1 freeze 签名：(Record, error)。terminal 时 rec+nil；超时/取消时 返回最新快照 + ctx.Err()；
// 不存在时 Record{}+ErrNotFound。opts.Timeout 在 ctx 上叠加 deadline。
func (m *Manager) Wait(ctx context.Context, agentID string, opts WaitOpts) (Record, error) {
    if rec, ok := m.Result(agentID); ok && rec.Status.Terminal() {
        return rec, nil
    }
    var done chan struct{}
    m.mu.RLock()
    rt, ok := m.runtime[agentID]
    if ok {
        done = rt.done
    }
    m.mu.RUnlock()
    waitCtx := ctx
    if opts.Timeout > 0 {
        var cancel context.CancelFunc
        waitCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
        defer cancel()
    }
    if done == nil {
        rec, ok := m.Result(agentID)
        if !ok {
            return Record{}, ErrNotFound
        }
        return rec, nil
    }
    select {
    case <-waitCtx.Done():
        rec, _ := m.Result(agentID)
        return rec, waitCtx.Err()
    case <-done:
        rec, _ := m.Result(agentID)
        return rec, nil
    }
}

func (m *Manager) Result(agentID string) (Record, bool) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    rec, ok := m.records[agentID]
    if !ok {
        return Record{}, false
    }
    return cloneRecord(rec), true
}
```

> `List` 已在 Task 3 给出；保持 `List(includeArchived bool) ListResult` 单参数签名，`ListResult.Running` 提供 stats。`ListResult.Limit` 等于 `MaxConcurrent()`。


- [ ] **Step 4：GREEN**

Run: `go test -race ./internal/agent/registry -run 'TestSpawn' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/agent/registry/manager.go internal/agent/registry/manager_spawn_test.go
git commit -m "feat(registry): transactional spawn with persistence-first rollback"
```

---

## Task 5：Wait / Result / List — 真实 `(Record, bool)` + running stats

**Files:**
- Modify: `internal/agent/registry/manager.go`
- Create: `internal/agent/registry/manager_query_test.go`

- [ ] **Step 1：失败测试**

```go
package registry

import (
    "context"
    "path/filepath"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

func TestWaitReturnsTerminalRecordAndResultIsSnapshot(t *testing.T) {
    m := NewManager(NewManagerOpts{
        RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
        SessionBootID: "boot", MaxConcurrent: 4,
    })
    t.Cleanup(m.Close)

    done := make(chan struct{})
    id, err := m.Spawn(context.Background(), SpawnRequest{
        AgentType: "subagent", Role: "explore", Prompt: "scan",
        Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
            <-done
            return "SUMMARY\n...\nEVIDENCE\nfile.go:10", nil
        }),
    })
    require.NoError(t, err)
    close(done)

    final, err := m.Wait(context.Background(), id, WaitOpts{Timeout: time.Second})
    require.NoError(t, err)
    require.Equal(t, StatusCompleted, final.Status)
    require.Contains(t, final.Result, "EVIDENCE")

    snap, ok := m.Result(id)
    require.True(t, ok)
    require.Equal(t, final, snap)

    list := m.List(false)
    require.Equal(t, 0, list.Running)
}

func TestResultMissingReturnsFalse(t *testing.T) {
    m := NewManager(NewManagerOpts{
        RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
        SessionBootID: "boot", MaxConcurrent: 1,
    })
    _, ok := m.Result("does-not-exist")
    require.False(t, ok)
}

func TestWaitCanceledByContextReturnsLatestRecord(t *testing.T) {
    m := NewManager(NewManagerOpts{
        RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
        SessionBootID: "boot", MaxConcurrent: 1,
    })
    t.Cleanup(m.Close)

    block := make(chan struct{})
    id, _ := m.Spawn(context.Background(), SpawnRequest{
        AgentType: "subagent", Role: "explore", Prompt: "block",
        Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
            <-block
            return "ok", nil
        }),
    })

    waitCtx, cancel := context.WithCancel(context.Background())
    cancel()
    latest, err := m.Wait(waitCtx, id, WaitOpts{})
    require.ErrorIs(t, err, context.Canceled)
    require.Equal(t, StatusRunning, latest.Status) // 超时/取消：返回最新快照 + ctx.Err()

    close(block)
}
```





- [ ] **Step 2：确认 RED**

Run: `go test ./internal/agent/registry -run 'TestWait|TestResult' -count=1`

Expected: FAIL，`Wait`/`Result` 未实现。

- [ ] **Step 3：实现**

```go
// Wait 是 C1 freeze 签名：(Record, error)。terminal 时 rec+nil；超时/取消时 返回最新快照 + ctx.Err()；
// 不存在时 Record{}+ErrNotFound。opts.Timeout 在 ctx 上叠加 deadline。
func (m *Manager) Wait(ctx context.Context, agentID string, opts WaitOpts) (Record, error) {
    if rec, ok := m.Result(agentID); ok && rec.Status.Terminal() {
        return rec, nil
    }
    var done chan struct{}
    m.mu.RLock()
    rt, ok := m.runtime[agentID]
    if ok {
        done = rt.done
    }
    m.mu.RUnlock()
    waitCtx := ctx
    if opts.Timeout > 0 {
        var cancel context.CancelFunc
        waitCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
        defer cancel()
    }
    if done == nil {
        rec, ok := m.Result(agentID)
        if !ok {
            return Record{}, ErrNotFound
        }
        return rec, nil
    }
    select {
    case <-waitCtx.Done():
        rec, _ := m.Result(agentID)
        return rec, waitCtx.Err()
    case <-done:
        rec, _ := m.Result(agentID)
        return rec, nil
    }
}

func (m *Manager) Result(agentID string) (Record, bool) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    rec, ok := m.records[agentID]
    if !ok {
        return Record{}, false
    }
    return cloneRecord(rec), true
}
```

> `List` 已在 Task 3 给出；保持 `List(includeArchived bool) ListResult` 单参数签名，`ListResult.Running` 提供 stats。`ListResult.Limit` 等于 `MaxConcurrent()`。





- [ ] **Step 4：GREEN**

Run: `go test -race ./internal/agent/registry -run 'TestWait|TestResult' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/agent/registry/manager.go internal/agent/registry/manager_query_test.go
git commit -m "feat(registry): wait/result/list with running stats"
```

---

## Task 6：`finishTerminal` 与 persistence-failed 事件顺序

**Files:**
- Modify: `internal/agent/registry/manager.go`
- Create: `internal/agent/registry/manager_terminal_test.go`

- [ ] **Step 1：失败测试（channel-gated runner + 真实文件系统失败）**

```go
package registry

import (
    "context"
    "os"
    "path/filepath"
    "sync"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

func TestFinishTerminalEmitsPersistenceFailedThenTerminal(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "subagents.v1.json")
    m := NewManager(NewManagerOpts{
        RootContext: context.Background(), Path: path, SessionBootID: "boot", MaxConcurrent: 2,
    })
    t.Cleanup(m.Close)

    var mu sync.Mutex
    var events []Event
    sink := EventSink(func(ev Event) {
        mu.Lock()
        defer mu.Unlock()
        events = append(events, ev)
    })

    release := make(chan struct{})
    runnerDone := make(chan struct{})
    id, err := m.Spawn(context.Background(), SpawnRequest{
        AgentType: "subagent", Role: "explore", Prompt: "p", Emit: sink,
        Runner: RunnerFunc(func(context.Context, string, string) (string, error) {
            defer close(runnerDone)
            <-release
            return "SUMMARY\n...\nEVIDENCE\nfile.go", nil
        }),
    })
    require.NoError(t, err)

    // 破坏写盘路径后才释放 runner，确保 terminal persist 必定失败。
    require.NoError(t, os.Rename(dir, dir+".gone"))
    close(release)
    select {
    case <-runnerDone:
    case <-time.After(time.Second):
        t.Fatal("runner did not finish")
    }

    // Eventually 等 terminal 真正落定（finishTerminal 在锁外 emit，可能有调度延迟）。
    require.Eventually(t, func() bool {
        mu.Lock()
        defer mu.Unlock()
        return len(events) >= 2
    }, time.Second, 10*time.Millisecond)

    mu.Lock()
    defer mu.Unlock()
    last := events[len(events)-1]
    prev := events[len(events)-2]
    require.Equal(t, EventPersistenceFailed, prev.Type)
    require.Contains(t, []EventType{EventCompleted, EventFailed, EventCancelled}, last.Type)

    snap, ok := m.Result(id)
    require.True(t, ok)
    require.True(t, snap.Status.Terminal())

    // 恢复目录使 t.Cleanup 的 Close 不再误报。
    _ = os.Rename(dir+".gone", dir)
}
```


- [ ] **Step 2：确认 RED**

Run: `go test ./internal/agent/registry -run 'TestFinishTerminal' -count=1`

Expected: FAIL，`finishTerminal` 未实现。

- [ ] **Step 3：实现**

```go
func (m *Manager) finishTerminal(agentID string, status Status, result, errMsg string) {
    m.persistMu.Lock()
    m.mu.Lock()
    rec, ok := m.records[agentID]
    if !ok {
        m.mu.Unlock()
        m.persistMu.Unlock()
        return
    }
    rec.Status = status
    rec.Result = result
    rec.Error = errMsg
    rec.EndedAt = time.Now().UTC()
    m.records[agentID] = cloneRecord(rec)
    snapshot, snapErr := m.snapshotLocked()
    sink := m.sinkLocked(agentID)
    m.mu.Unlock()

    terminalEvent := Event{
        Type: mapStatusToEvent(status), AgentID: rec.ID, ParentID: rec.ParentID, Role: rec.Role,
        Status: status, Text: result, Usage: rec.Usage, Timestamp: rec.EndedAt,
    }
    if snapErr == nil {
        snapErr = writeAtomic(m.path, snapshot)
    }
    if snapErr != nil {
        m.emit(sink, Event{
            Type: EventPersistenceFailed, AgentID: rec.ID, ParentID: rec.ParentID, Role: rec.Role,
            Status: status, Text: snapErr.Error(), Timestamp: time.Now().UTC(),
        })
    }
    m.emit(sink, terminalEvent)
    m.persistMu.Unlock()

    if rt := m.detachRuntime(agentID); rt != nil && rt.cancel != nil {
        rt.cancel()
    }
}

func (m *Manager) sinkLocked(agentID string) EventSink {
    rt, ok := m.runtime[agentID]
    if !ok { return nil }
    return rt.emit
}

func (m *Manager) detachRuntime(agentID string) *runtimeAgent {
    m.mu.Lock()
    defer m.mu.Unlock()
    rt := m.runtime[agentID]
    delete(m.runtime, agentID)
    return rt
}

func mapStatusToEvent(s Status) EventType {
    switch s {
    case StatusCompleted: return EventCompleted
    case StatusFailed: return EventFailed
    case StatusCancelled: return EventCancelled
    case StatusInterrupted: return EventType("interrupted")
    default: return EventFailed
    }
}
```

> 顺序：锁内复制 sink → unlock → persist → 失败发 `persistence_failed` → 发 terminal → 持久化锁 unlock。绝不在 unlock 前发事件，也绝不在发 terminal 后再 persist。

- [ ] **Step 4：GREEN**

Run: `go test -race ./internal/agent/registry -run 'TestFinishTerminal' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/agent/registry/manager.go internal/agent/registry/manager_terminal_test.go
git commit -m "feat(registry): terminal persistence with ordered failure event"
```

---

## Task 7：Mailbox / SendInput / Assign / Cancel — 持久化+回滚

**Files:**
- Modify: `internal/agent/registry/manager.go`
- Create: `internal/agent/registry/manager_mailbox_test.go`

- [ ] **Step 1：失败测试**

```go
package registry

import (
    "context"
    "os"
    "path/filepath"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

func TestSendInputQueuesAndAssignPersists(t *testing.T) {
    m := NewManager(NewManagerOpts{
        RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
        SessionBootID: "boot", MaxConcurrent: 2,
    })
    t.Cleanup(m.Close)

    firstDone := make(chan struct{})
    resumeCh := make(chan struct{})
    id, err := m.Spawn(context.Background(), SpawnRequest{
        AgentType: "subagent", Role: "general", Prompt: "first",
        Runner: RunnerFunc(func(ctx context.Context, agentID, assignment string) (string, error) {
            if assignment == "first" {
                close(firstDone)
                <-resumeCh
                return "first done", nil
            }
            return "second done", nil
        }),
    })
    require.NoError(t, err)

    <-firstDone
    require.NoError(t, m.SendInput(id, "follow up", false))
    close(resumeCh)

    final, err := m.Wait(context.Background(), id, WaitOpts{Timeout: time.Second})
    require.NoError(t, err)
    require.Equal(t, StatusCompleted, final.Status)
    require.Equal(t, "second done", final.Result)
}

func TestSendInputRejectsNotRunning(t *testing.T) {
    m := NewManager(NewManagerOpts{
        RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
        SessionBootID: "boot", MaxConcurrent: 1,
    })
    t.Cleanup(m.Close)
    require.ErrorIs(t, m.SendInput("ghost", "x", false), ErrNotRunning)
}

func TestAssignPersistsBeforeEnqueueAndRollsBackOnFailure(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "subagents.v1.json")
    m := NewManager(NewManagerOpts{
        RootContext: context.Background(), Path: path, SessionBootID: "boot", MaxConcurrent: 1,
    })
    t.Cleanup(m.Close)

    block := make(chan struct{})
    id, err := m.Spawn(context.Background(), SpawnRequest{
        AgentType: "subagent", Role: "general", Prompt: "p",
        Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
            <-block
            return "ok", nil
        }),
    })
    require.NoError(t, err)

    require.NoError(t, m.Assign(id, "audit module auth"))
    snap, ok := m.Result(id)
    require.True(t, ok)
    require.Equal(t, "audit module auth", snap.Assignment)

    // 写盘失败路径：改名目录后再 Assign，必须回滚（assignment 不留、不 enqueue）。
    require.NoError(t, os.Rename(dir, dir+".gone"))
    err = m.Assign(id, "second assignment must not stick")
    require.Error(t, err)
    require.NoError(t, os.Rename(dir+".gone", dir))
    snap, _ = m.Result(id)
    require.Equal(t, "audit module auth", snap.Assignment)

    close(block)
}
```


- [ ] **Step 2：确认 RED**

Run: `go test ./internal/agent/registry -run 'TestSendInput|TestAssign' -count=1`

Expected: FAIL，相关方法未实现。

- [ ] **Step 3：实现**

```go
// SendInput 把 follow-up assignment 排进 running agent 的 mailbox。interrupt=true 时
// 同时取消当前 turn 的 ctx（不取消 agent root），让 runAgentLoop 接力到新 assignment。
func (m *Manager) SendInput(agentID, text string, interrupt bool) error {
    if m.closed.Load() {
        return ErrClosed
    }
    m.mu.Lock()
    rt, ok := m.runtime[agentID]
    rec, recOK := m.records[agentID]
    if !ok || !recOK || rec.Status != StatusRunning || !rt.accepting {
        m.mu.Unlock()
        return ErrNotRunning
    }
    select {
    case rt.mailbox <- text:
        turnCancel := rt.turnCancel
        sink := rt.emit
        m.mu.Unlock()
        if interrupt && turnCancel != nil {
            turnCancel() // 只取消当前 turn，不取消 agent root
        }
        m.emit(sink, Event{
            Type: EventInputQueued, AgentID: agentID, Role: rec.Role,
            Status: StatusRunning, Text: text, Timestamp: time.Now().UTC(),
        })
        return nil
    default:
        m.mu.Unlock()
        return ErrMailboxFull
    }
}

// Assign 落盘新 assignment 后才把它 enqueue 到 runtime mailbox（顺序：mutate → 容量预检
// → persist → enqueue）。写盘失败回滚 assignment 且不 enqueue。
func (m *Manager) Assign(agentID, assignment string) error {
    m.persistMu.Lock()
    m.mu.Lock()
    rec, ok := m.records[agentID]
    if !ok {
        m.mu.Unlock()
        m.persistMu.Unlock()
        return ErrNotFound
    }
    rt, running := m.runtime[agentID]
    // 容量预检：running 且 accepting 时 mailbox 必须有空位，否则在落盘前就拒绝。
    if running && rt.accepting && len(rt.mailbox) >= cap(rt.mailbox) {
        m.mu.Unlock()
        m.persistMu.Unlock()
        return ErrMailboxFull
    }
    oldAssignment := rec.Assignment
    rec.Assignment = assignment
    m.records[agentID] = cloneRecord(rec)
    snapshot, snapErr := m.snapshotLocked()
    sink := m.sinkLocked(agentID)
    m.mu.Unlock()
    if snapErr != nil {
        m.restoreAssignment(agentID, oldAssignment)
        m.persistMu.Unlock()
        return snapErr
    }
    if err := writeAtomic(m.path, snapshot); err != nil {
        m.restoreAssignment(agentID, oldAssignment)
        m.persistMu.Unlock()
        return fmt.Errorf("persist subagent assign: %w", err)
    }
    // 落盘成功后才 enqueue；竞态满时 assignment 已在 record，下一轮 turn/Resume 仍能读到。
    if running && rt.accepting {
        select {
        case rt.mailbox <- assignment:
        default:
        }
    }
    m.persistMu.Unlock()
    m.emit(sink, Event{
        Type: EventAssigned, AgentID: agentID, Role: rec.Role,
        Status: rec.Status, Text: assignment, Timestamp: time.Now().UTC(),
    })
    return nil
}

func (m *Manager) restoreAssignment(agentID, old string) {
    m.mu.Lock()
    if rec, ok := m.records[agentID]; ok {
        rec.Assignment = old
        m.records[agentID] = cloneRecord(rec)
    }
    m.mu.Unlock()
}

// Cancel 取消 agent root ctx；runAgentLoop 在 ctx.Done 路径进入 finishTerminal(Cancelled)。
func (m *Manager) Cancel(agentID string) error {
    m.mu.Lock()
    rt, ok := m.runtime[agentID]
    m.mu.Unlock()
    if !ok {
        if _, ok := m.Result(agentID); !ok {
            return ErrNotFound
        }
        return nil // 已 terminal：返回最新状态语义
    }
    if rt.cancel != nil {
        rt.cancel()
    }
    return nil
}
```


- [ ] **Step 4：GREEN**

Run: `go test -race ./internal/agent/registry -run 'TestSendInput|TestAssign' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/agent/registry/manager.go internal/agent/registry/manager_mailbox_test.go
git commit -m "feat(registry): mailbox, assign, cancel with persistence rollback"
```

---

## Task 8：Resume — 保存并复用工具约束、custom role、override

**Files:**
- Modify: `internal/agent/registry/manager.go`
- Create: `internal/agent/registry/manager_resume_test.go`

- [ ] **Step 1：失败测试**

```go
package registry

import (
    "context"
    "encoding/json"
    "os"
    "path/filepath"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

func TestResumeRestoresSavedConstraintsAndEmitsEvent(t *testing.T) {
    path := filepath.Join(t.TempDir(), "s.json")
    rec := Record{
        ID: "ag-old", SessionBootID: "boot", Role: "custom", Status: StatusRunning,
        Prompt: "audit auth", StartedAt: time.Now().UTC(),
        Custom: &CustomRole{Name: "audit", PromptPrefix: "audit", AllowedTools: []string{"fs_read"}, ReadOnlyShell: true},
        AllowedTools: []string{"fs_read"}, Instruction: "stay read only",
        ModelOverride: "gpt-4o-mini", ReasoningEffort: "low",
    }
    raw, err := json.Marshal(persistedState{
        SchemaVersion: persistenceSchemaVersion, SessionBootID: "boot", Agents: []Record{rec},
    })
    require.NoError(t, err)
    require.NoError(t, os.WriteFile(path, raw, 0o600))

    // 新 boot 加载磁盘 Running，loader 先标 Interrupted。
    m := NewManager(NewManagerOpts{
        RootContext: context.Background(), Path: path, SessionBootID: "boot2", MaxConcurrent: 2,
    })
    t.Cleanup(m.Close)
    archived := m.List(true)
    require.Len(t, archived.Agents, 1)
    require.Equal(t, StatusInterrupted, archived.Agents[0].Status)

    seen := make(chan string, 1)
    id, err := m.Resume(context.Background(), rec.ID, ResumeRequest{
        Runner: RunnerFunc(func(ctx context.Context, agentID, assignment string) (string, error) {
            seen <- assignment
            return "second pass", nil
        }),
    })
    require.NoError(t, err)
    require.Equal(t, rec.ID, id)

    got, ok := m.Result(id)
    require.True(t, ok)
    require.Equal(t, StatusRunning, got.Status)
    require.Equal(t, []string{"fs_read"}, got.AllowedTools)
    require.Equal(t, "stay read only", got.Instruction)
    require.Equal(t, "gpt-4o-mini", got.ModelOverride)
    require.Equal(t, "low", got.ReasoningEffort)
    require.NotNil(t, got.Custom)
    require.Equal(t, "audit", got.Custom.Name)
    require.Equal(t, "audit auth", <-seen) // prompt fallback 到持久化 Prompt
}
```


- [ ] **Step 2：确认 RED**

Run: `go test ./internal/agent/registry -run 'TestResume' -count=1`

Expected: FAIL，`Resume` 未实现。

- [ ] **Step 3：实现**

```go
// Resume 是 C1 freeze 签名：返回 agentID（string）。完整化：cap 预检、closed gate、
// runtime 在 persist 前预留、persist 失败恢复完整旧 record+runtime、parent 仍 running 时
// 挂回其 cancellation tree、prompt fallback（req.Prompt > record.Assignment > record.Prompt）、
// SessionBootID 切到当前 boot。override/role 的 fail-closed 重验由调用方（tools 层，
// agent_resume 工具）在调 Resume 前完成——registry 不导入 guard/tools。
func (m *Manager) Resume(ctx context.Context, agentID string, req ResumeRequest) (string, error) {
    if req.Runner == nil {
        return "", fmt.Errorf("resume: runner is required")
    }
    if m.closed.Load() {
        return "", ErrClosed
    }
    m.persistMu.Lock()
    m.mu.Lock()
    if m.closed.Load() { // double-check
        m.mu.Unlock()
        m.persistMu.Unlock()
        return "", ErrClosed
    }
    rec, ok := m.records[agentID]
    if !ok {
        m.mu.Unlock()
        m.persistMu.Unlock()
        return "", ErrNotFound
    }
    if rec.Status == StatusRunning {
        m.mu.Unlock()
        m.persistMu.Unlock()
        return "", fmt.Errorf("subagent %s is already running", agentID)
    }
    if _, exists := m.runtime[agentID]; exists {
        m.mu.Unlock()
        m.persistMu.Unlock()
        return "", fmt.Errorf("subagent %s runtime already active", agentID)
    }
    if m.runningLocked() >= m.limit {
        m.mu.Unlock()
        m.persistMu.Unlock()
        return "", &SpawnErrCap{Cap: m.limit}
    }
    assignment := req.Prompt
    if assignment == "" {
        assignment = rec.Assignment
    }
    if assignment == "" {
        assignment = rec.Prompt
    }
    // parent cancellation tree：parent 仍 running 挂其 ctx，否则用 root（保留 ParentID/Depth）。
    parentCtx := m.rootCtx
    if rec.ParentID != "" {
        if parentRT, live := m.runtime[rec.ParentID]; live {
            parentCtx = parentRT.ctx
        }
    }
    oldRec := cloneRecord(rec)
    rec.Status = StatusRunning
    rec.Error = ""
    rec.EndedAt = time.Time{}
    rec.StartedAt = time.Now().UTC()
    rec.Assignment = assignment
    rec.SessionBootID = m.bootID
    m.records[agentID] = cloneRecord(rec)
    childCtx, cancel := context.WithCancel(parentCtx)
    rt := &runtimeAgent{
        ctx: childCtx, cancel: cancel, accepting: true, assignment: assignment,
        runner: req.Runner, emit: req.Emit,
        mailbox: make(chan string, 8), done: make(chan struct{}),
    }
    m.runtime[agentID] = rt // reserve before persist
    snapshot, snapErr := m.snapshotLocked()
    m.mu.Unlock()
    if snapErr != nil {
        m.restoreRecord(agentID, oldRec, cancel)
        m.persistMu.Unlock()
        return "", snapErr
    }
    if err := writeAtomic(m.path, snapshot); err != nil {
        m.restoreRecord(agentID, oldRec, cancel)
        m.persistMu.Unlock()
        return "", fmt.Errorf("persist subagent resume: %w", err)
    }
    m.persistMu.Unlock()

    m.emit(req.Emit, Event{
        Type: EventResumed, AgentID: agentID, ParentID: rec.ParentID, Role: rec.Role,
        Status: StatusRunning, Timestamp: time.Now().UTC(),
    })
    m.wg.Add(1)
    go m.runAgentLoop(childCtx, agentID, rt)
    return agentID, nil
}

// restoreRecord 把 record 与 runtime 完整恢复到 Resume 失败前的状态，并释放已派生的 cancel。
func (m *Manager) restoreRecord(agentID string, old Record, cancelToCall context.CancelFunc) {
    m.mu.Lock()
    m.records[agentID] = old
    delete(m.runtime, agentID)
    m.mu.Unlock()
    if cancelToCall != nil {
        cancelToCall()
    }
}
```


- [ ] **Step 4：GREEN**

Run: `go test -race ./internal/agent/registry -run 'TestResume' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/agent/registry/manager.go internal/agent/registry/manager_resume_test.go
git commit -m "feat(registry): resume restores persisted constraints and override"
```

---

## Task 9：AddUsage — 持久化并暴露给 list/result

**Files:**
- Modify: `internal/agent/registry/manager.go`
- Create: `internal/agent/registry/manager_usage_test.go`

- [ ] **Step 1：失败测试**

```go
package registry

import (
    "context"
    "path/filepath"
    "testing"

    "github.com/stretchr/testify/require"
)

func TestAddUsageAccumulatesAndPersists(t *testing.T) {
    path := filepath.Join(t.TempDir(), "s.json")
    m := NewManager(NewManagerOpts{
        RootContext: context.Background(), Path: path, SessionBootID: "boot", MaxConcurrent: 2,
    })
    t.Cleanup(m.Close)

    block := make(chan struct{})
    id, err := m.Spawn(context.Background(), SpawnRequest{
        AgentType: "subagent", Role: "general", Prompt: "p",
        Runner: RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
            <-block
            return "ok", nil
        }),
    })
    require.NoError(t, err)

    require.NoError(t, m.AddUsage(id, Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, ModelCalls: 1}))
    require.NoError(t, m.AddUsage(id, Usage{PromptTokens: 50, CompletionTokens: 5, TotalTokens: 55, ModelCalls: 1}))

    snap, ok := m.Result(id)
    require.True(t, ok)
    require.Equal(t, int64(150), snap.Usage.PromptTokens)
    require.Equal(t, int64(25), snap.Usage.CompletionTokens)
    require.Equal(t, int64(175), snap.Usage.TotalTokens)
    require.Equal(t, int64(2), snap.Usage.ModelCalls)

    close(block)
}
```


- [ ] **Step 2：确认 RED**

Run: `go test ./internal/agent/registry -run 'TestAddUsage' -count=1`

Expected: FAIL，`AddUsage` 未实现。

- [ ] **Step 3：实现**

```go
// AddUsage 累计并持久化 usage。写盘失败：回滚内存 + 锁外发 persistence_failed（不静默只
// 返回 error，让 transport/调用方知道磁盘与内存已背离）。usage 增量由 concrete runner
// 每次 nested model call 报上来（Task 18）。
func (m *Manager) AddUsage(agentID string, delta Usage) error {
    if m.closed.Load() {
        return ErrClosed
    }
    m.persistMu.Lock()
    m.mu.Lock()
    rec, ok := m.records[agentID]
    if !ok {
        m.mu.Unlock()
        m.persistMu.Unlock()
        return ErrNotFound
    }
    oldUsage := rec.Usage
    rec.Usage = rec.Usage.Add(delta)
    m.records[agentID] = cloneRecord(rec)
    snapshot, snapErr := m.snapshotLocked()
    sink := m.sinkLocked(agentID)
    role := rec.Role
    status := rec.Status
    newUsage := rec.Usage
    m.mu.Unlock()
    if snapErr != nil {
        m.restoreUsage(agentID, oldUsage)
        m.persistMu.Unlock()
        m.emit(sink, Event{
            Type: EventPersistenceFailed, AgentID: agentID, Role: role, Status: status,
            Text: snapErr.Error(), Timestamp: time.Now().UTC(),
        })
        return snapErr
    }
    if err := writeAtomic(m.path, snapshot); err != nil {
        m.restoreUsage(agentID, oldUsage)
        m.persistMu.Unlock()
        m.emit(sink, Event{
            Type: EventPersistenceFailed, AgentID: agentID, Role: role, Status: status,
            Text: err.Error(), Timestamp: time.Now().UTC(),
        })
        return fmt.Errorf("persist subagent usage: %w", err)
    }
    m.persistMu.Unlock()
    m.emit(sink, Event{
        Type: EventType("usage"), AgentID: agentID, Role: role,
        Status: status, Usage: newUsage, Timestamp: time.Now().UTC(),
    })
    return nil
}

func (m *Manager) restoreUsage(agentID string, old Usage) {
    m.mu.Lock()
    if rec, ok := m.records[agentID]; ok {
        rec.Usage = old
        m.records[agentID] = cloneRecord(rec)
    }
    m.mu.Unlock()
}
```


- [ ] **Step 4：GREEN**

Run: `go test -race ./internal/agent/registry -run 'TestAddUsage' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/agent/registry/manager.go internal/agent/registry/manager_usage_test.go
git commit -m "feat(registry): per-agent usage accumulation persisted"
```

---

## Task 10：扩展 `guard.PermissionProfile` 加入 subagent override 与 role policy

**Files:**
- Modify: `internal/guard/profile.go`
- Modify: `internal/guard/profile_test.go`
- Modify: `config.example.yaml`

- [ ] **Step 1：失败测试**

```go
package guard

import (
    "testing"

    "github.com/stretchr/testify/require"
)

func TestPermissionProfileDefaultsSubagent(t *testing.T) {
    var p PermissionProfile
    require.True(t, p.Subagent.AllowsAnyModel())
    require.Equal(t, "high", p.Subagent.ReasoningCap())
    require.NoError(t, p.Subagent.CheckReasoning("high"))
}

func TestSubagentPermModelAllowlist(t *testing.T) {
    p := PermissionProfile{
        Subagent: SubagentPerm{Models: []string{"gpt-4o-mini"}, MaxReasoning: "medium"},
    }
    require.NoError(t, p.Subagent.CheckModel("gpt-4o-mini"))
    require.Error(t, p.Subagent.CheckModel("claude-haiku"))
    require.NoError(t, p.Subagent.CheckReasoning("low"))
    require.NoError(t, p.Subagent.CheckReasoning("medium"))
    require.Error(t, p.Subagent.CheckReasoning("high"))
    require.Error(t, p.Subagent.CheckReasoning("bogus"))
}
```

- [ ] **Step 2：确认 RED**

Run: `go test ./internal/guard -run 'TestPermissionProfileDefaultsSubagent|TestSubagentPermModelAllowlist' -count=1`

Expected: FAIL，`SubagentPerm` 未定义。

- [ ] **Step 3：实现**

修改 `internal/guard/profile.go`：

```go
package guard

import (
    "fmt"
    "strings"
)

type PermissionProfile struct {
    FS       FSPerm    `yaml:"fs"`
    Tools    ToolsPerm `yaml:"tools"`
    Shell    ShellPerm `yaml:"shell"`
    Net      NetPerm   `yaml:"net"`
    Subagent SubagentPerm `yaml:"subagent"`
}

// SubagentPerm restricts which model and reasoning effort spawned subagents
// may select. Empty fields mean "inherit parent without restriction" so
// existing configurations keep working.
type SubagentPerm struct {
    Models       []string `yaml:"models"`
    MaxReasoning string   `yaml:"max_reasoning_effort"`
}

var reasoningRank = map[string]int{
    "off": 0, "low": 1, "medium": 2, "high": 3,
}

func (p SubagentPerm) AllowsAnyModel() bool { return len(p.Models) == 0 }

func (p SubagentPerm) CheckModel(name string) error {
    if p.AllowsAnyModel() { return nil }
    for _, allowed := range p.Models {
        if strings.EqualFold(allowed, name) { return nil }
    }
    return fmt.Errorf("subagent model %q is not allowed by profile", name)
}

func (p SubagentPerm) ReasoningCap() string {
    if p.MaxReasoning == "" { return "high" }
    return strings.ToLower(p.MaxReasoning)
}

func (p SubagentPerm) CheckReasoning(effort string) error {
    want, ok := reasoningRank[strings.ToLower(effort)]
    if !ok { return fmt.Errorf("invalid reasoning effort %q", effort) }
    capName := p.ReasoningCap()
    cap, ok := reasoningRank[capName]
    if !ok { return fmt.Errorf("invalid profile reasoning cap %q", capName) }
    if want > cap {
        return fmt.Errorf("reasoning effort %q exceeds profile cap %q", effort, capName)
    }
    return nil
}
```

> 同步在 `config.example.yaml` 给一个示例：

```yaml
profiles:
  orchestrator:
    tools: { allow: ["*"] }
    subagent:
      models: ["gpt-4o-mini", "claude-haiku"]
      max_reasoning_effort: medium
```

- [ ] **Step 4：GREEN**

Run: `go test ./internal/guard -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/guard/profile.go internal/guard/profile_test.go config.example.yaml
git commit -m "feat(guard): profile subagent model/reasoning caps"
```

---

## Task 11：Role execution policy 上下文（在 Authorize 之前短路）

**Files:**
- Modify: `internal/tools/permctx.go`
- Create: `internal/tools/rolepolicy.go`
- Create: `internal/tools/rolepolicy_test.go`

- [ ] **Step 1：失败测试**

```go
package tools

import (
    "context"
    "testing"

    "github.com/stretchr/testify/require"
    "github.com/x6nux/yanshi/internal/guard"
)

func TestRolePolicyRejectsUnsafeShellBeforeAuthorize(t *testing.T) {
    ctx := WithRolePolicy(context.Background(), RolePolicy{
        ReadOnlyShell: true, WritePatterns: nil,
    })
    err := Authorize(ctx, guard.Action{Tool: "shell_run", Shell: "rm -rf /tmp/x"}, `{}`)
    require.ErrorIs(t, err, ErrRolePolicyDenied)
}

func TestRolePolicyAllowsSafeReadOnlyShell(t *testing.T) {
    ctx := WithRolePolicy(context.Background(), RolePolicy{ReadOnlyShell: true})
    require.NoError(t, CheckRolePolicy(ctx, guard.Action{Tool: "shell_run", Shell: "ls -la"}))
}

func TestRolePolicyBlocksWriteOutsidePatterns(t *testing.T) {
    ctx := WithRolePolicy(context.Background(), RolePolicy{
        WritePatterns: []string{"docs/plans/*.md"},
    })
    err := CheckRolePolicy(ctx, guard.Action{
        Tool: "fs_write", FS: guard.FSWant{Op: "write", Paths: []string{"internal/foo.go"}},
    })
    require.ErrorIs(t, err, ErrRolePolicyDenied)
    require.NoError(t, CheckRolePolicy(ctx, guard.Action{
        Tool: "fs_write", FS: guard.FSWant{Op: "write", Paths: []string{"docs/plans/x.md"}},
    }))

    // 空 WritePatterns 不增加 write 限制：General/Implementer 的非 nil 空 policy 不得拦写。
    noWriteCtx := WithRolePolicy(context.Background(), RolePolicy{ReadOnlyShell: true})
    require.NoError(t, CheckRolePolicy(noWriteCtx, guard.Action{
        Tool: "fs_write", FS: guard.FSWant{Op: "write", Paths: []string{"internal/foo.go"}},
    }))
}
```

- [ ] **Step 2：确认 RED**

Run: `go test ./internal/tools -run 'TestRolePolicy' -count=1`

Expected: FAIL，`RolePolicy`、`WithRolePolicy` 未定义。

- [ ] **Step 3：实现**

创建 `rolepolicy.go`：

```go
package tools

import (
    "context"
    "errors"
    "path/filepath"
    "strings"

    "github.com/x6nux/yanshi/internal/guard"
)

var ErrRolePolicyDenied = errors.New("denied by subagent role policy")

type RolePolicy struct {
    ReadOnlyShell bool
    WritePatterns []string
}

type rolePolicyKey struct{}

func WithRolePolicy(ctx context.Context, p RolePolicy) context.Context {
    return context.WithValue(ctx, rolePolicyKey{}, p)
}

func RolePolicyFromContext(ctx context.Context) (RolePolicy, bool) {
    p, ok := ctx.Value(rolePolicyKey{}).(RolePolicy)
    return p, ok
}

// CheckRolePolicy enforces role-level restrictions BEFORE Authorize consults
// session allowlist / static guard / interactive callback so that neither
// parent profile nor human approval can widen the role.
func CheckRolePolicy(ctx context.Context, action guard.Action) error {
    p, ok := RolePolicyFromContext(ctx)
    if !ok { return nil }

    if action.Tool == "shell_run" && p.ReadOnlyShell {
        cmd := strings.TrimSpace(action.Shell)
        fields := strings.Fields(cmd)
        if len(fields) == 0 { return ErrRolePolicyDenied }
        name := strings.TrimSuffix(fields[0], ".exe")
        name = filepath.Base(name)
        if !safeShellCommands[name] || hasShellMetachar(cmd) {
            return ErrRolePolicyDenied
        }
    }
    // 空 WritePatterns = 不增加 write 限制（继承 parent guard），避免 General/Implementer
    // 持有非 nil 的空 policy 时被误解成「禁止所有写」。非空时必须命中其一。
    if action.FS.Op == "write" && len(p.WritePatterns) > 0 {
        for _, target := range action.FS.Paths {
            if !anyGlobMatch(p.WritePatterns, target) {
                return ErrRolePolicyDenied
            }
        }
    }
    return nil
}

func anyGlobMatch(patterns []string, target string) bool {
    target = filepath.ToSlash(target)
    for _, pattern := range patterns {
        if ok, _ := filepath.Match(filepath.ToSlash(pattern), target); ok { return true }
    }
    return false
}
```

修改 `permctx.go` 的 `Authorize`：

```go
func Authorize(ctx context.Context, action guard.Action, argsJSON string) error {
    if err := CheckRolePolicy(ctx, action); err != nil {
        return err
    }
    // 现有顺序保持不变：no profile -> deny, session allowlist -> allow,
    // static guard -> allow, callback -> allow, 否则 deny。
    return authorizeLegacy(ctx, action, argsJSON)
}
```

> 实际重构建议：把原 `Authorize` body 重命名为 `authorizeLegacy`，公开 `Authorize` 在最顶部加 `CheckRolePolicy`。不要改原有 callback/allowlist 行为。
>
> `shell.go` 的 safe-command shortcut 当前不会调用 `Authorize`，因此还必须在 shortcut **之前**加同一个 pre-check：
>
> ```go
> action := guard.Action{Tool: "shell_run", Shell: a.Command}
> if err := CheckRolePolicy(ctx, action); err != nil {
>     pushErrChunk(ch, err)
>     return
> }
> if safe := safeShellCommands[firstWord(a.Command)]; safe {
>     // 保留现有 metachar/path traversal checks。
> } else if err := Authorize(ctx, action, argsJSON); err != nil {
>     pushErrChunk(ch, err)
>     return
> }
> ```
>
> 这样 Explore/Review/Verifier 的 safe read-only shell 可过，`rm` 等 unsafe shell 会在 parent guard和interactive callback前被拒；用户审批无法放宽。

- [ ] **Step 4：GREEN**

Run: `go test ./internal/tools -run 'TestRolePolicy' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/tools/permctx.go internal/tools/rolepolicy.go internal/tools/rolepolicy_test.go
git commit -m "feat(tools): role policy pre-check denies before interactive approval"
```

---

## Task 12：config 扩展 — SubagentsConfig + 真实 LoadBytes/expandHome

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`（若不存在则新建）
- Modify: `internal/bootstrap/bootstrap.go`（仅替换 `expandHome` 调用点）
- Modify: `config.example.yaml`

- [ ] **Step 1：失败测试**

```go
package config

import (
    "testing"

    "github.com/stretchr/testify/require"
)

func TestLoadBytesExpandsSubagents(t *testing.T) {
    yaml := []byte(`
subagents:
  limit: 5
  persistence_path: "~/yanshi/subagents.v1.json"
`)
    cfg, err := LoadBytes(yaml)
    require.NoError(t, err)
    require.Equal(t, 5, cfg.Subagents.Limit)
    require.NotContains(t, cfg.Subagents.PersistencePath, "~")
}

func TestLoadBytesRejectsInvalidSubagentLimit(t *testing.T) {
    yaml := []byte("subagents:\n  limit: 99\n")
    _, err := LoadBytes(yaml)
    require.Error(t, err)
}
```

- [ ] **Step 2：确认 RED**

Run: `go test ./internal/config -run 'TestLoadBytes' -count=1`

Expected: FAIL，`LoadBytes`、`Subagents` 未定义。

- [ ] **Step 3：实现**

修改 `internal/config/config.go`：

```go
import (
    "errors"
    "os"
    "path/filepath"
    "strings"

    "github.com/x6nux/yanshi/internal/guard"
    "gopkg.in/yaml.v3"
)

type Config struct {
    Server     ServerConfig                       `yaml:"server"`
    Storage    StorageConfig                      `yaml:"storage"`
    Token      string                             `yaml:"token"`
    LLM        LLMConfig                          `yaml:"llm"`
    Agents     []AgentConfig                      `yaml:"agents"`
    Profiles   map[string]guard.PermissionProfile `yaml:"profiles"`
    Skills     SkillsConfig                       `yaml:"skills"`
    VCS        VCSConfig                          `yaml:"vcs"`
    Compaction CompactionConfig                   `yaml:"compaction"`
    Subagents  SubagentsConfig                    `yaml:"subagents"`
}

type SubagentsConfig struct {
    Limit           int    `yaml:"limit"`
    PersistencePath string `yaml:"persistence_path"`
}

func Load(path string) (*Config, error) {
    data, err := os.ReadFile(path)
    if err != nil { return nil, err }
    return LoadBytes(data)
}

func LoadBytes(data []byte) (*Config, error) {
    expanded := os.ExpandEnv(string(data))
    var cfg Config
    if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil { return nil, err }
    cfg.applyDefaults()
    if err := cfg.validate(); err != nil { return nil, err }
    return &cfg, nil
}

func (c *Config) validate() error {
    if c.Subagents.Limit != 0 && (c.Subagents.Limit < 1 || c.Subagents.Limit > 20) {
        return errors.New("subagents.limit must be within 1..20")
    }
    return nil
}

func (c *Config) applyDefaults() {
    if c.Compaction.Threshold == 0 { c.Compaction.Threshold = 0.8 }
    if c.Compaction.KeepRecent == 0 { c.Compaction.KeepRecent = 4 }
    if c.Compaction.ContextWindow == 0 { c.Compaction.ContextWindow = 256000 }
    if c.Compaction.ChunkThreshold == 0 { c.Compaction.ChunkThreshold = 0.9 }
    if c.Subagents.Limit == 0 { c.Subagents.Limit = 10 }
    if c.Subagents.PersistencePath == "" {
        c.Subagents.PersistencePath = "~/.yanshi/subagents.v1.json"
    }
    c.Subagents.PersistencePath = expandHome(c.Subagents.PersistencePath)
}

func expandHome(p string) string {
    if p == "" { return "" }
    if strings.HasPrefix(p, "~") {
        if home, err := os.UserHomeDir(); err == nil {
            return filepath.Join(home, p[1:])
        }
    }
    return p
}
```

> bootstrap 包里既有的 `expandHome` 私有 helper 保留不动；config 包的新 helper 仅服务 Subagents 配置（若后续要统一抽离到 helper 包，另立小步任务）。

- [ ] **Step 4：GREEN**

Run: `go test ./internal/config -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/config/config.go internal/config/config_test.go config.example.yaml
git commit -m "feat(config): subagent limit/persistence path via LoadBytes"
```

---

## Task 13：七角色定义 + system prompt 前缀 + 工具 allowlist 姿态

**Files:**
- Create: `internal/tools/agentroles.go`
- Create: `internal/tools/agentroles_test.go`

- [ ] **Step 1：失败测试**

```go
package tools

import (
    "testing"

    "github.com/stretchr/testify/require"
)

func TestRoleCatalogCoversSevenRoles(t *testing.T) {
    names := []string{}
    for _, r := range AgentRoles() {
        names = append(names, r.Name)
    }
    require.ElementsMatch(t, []string{"general", "explore", "plan", "review", "implementer", "verifier", "custom"}, names)
}

func TestRoleAllowlistOnlyTightensParent(t *testing.T) {
    for _, r := range AgentRoles() {
        require.NotEmpty(t, r.PromptPrefix, "role %s missing prompt prefix", r.Name)
    }
    // read-only 角色必须有非 nil policy 且 ReadOnlyShell；General/Implementer/Custom
    // 用 nil（不增加 role 限制，写权限来自 parent guard，不被空 policy 拦）。
    explore := MustRole("explore")
    require.NotNil(t, explore.Policy)
    require.True(t, explore.Policy.ReadOnlyShell)
    require.Empty(t, explore.Policy.WritePatterns)

    plan := MustRole("plan")
    require.NotNil(t, plan.Policy)
    require.True(t, plan.Policy.ReadOnlyShell)
    require.NotEmpty(t, plan.Policy.WritePatterns)

    review := MustRole("review")
    require.NotNil(t, review.Policy)
    require.True(t, review.Policy.ReadOnlyShell)
    require.Empty(t, review.Policy.WritePatterns)

    for _, name := range []string{"general", "implementer", "custom"} {
        require.Nil(t, MustRole(name).Policy, "%s must not add role-level restriction", name)
    }
}

func TestRolePromptPrefixCarriesOutputContract(t *testing.T) {
    for _, r := range AgentRoles() {
        if r.Name == "custom" { continue }
        require.Contains(t, r.PromptPrefix, "SUMMARY")
        require.Contains(t, r.PromptPrefix, "CHANGES")
        require.Contains(t, r.PromptPrefix, "EVIDENCE")
        require.Contains(t, r.PromptPrefix, "RISKS")
        require.Contains(t, r.PromptPrefix, "BLOCKERS")
    }
}
```

- [ ] **Step 2：确认 RED**

Run: `go test ./internal/tools -run 'TestRoleCatalog|TestRoleAllowlist|TestRolePromptPrefix' -count=1`

Expected: FAIL，`AgentRoles`/`MustRole` 未定义。

- [ ] **Step 3：实现**

```go
package tools

import "fmt"

type RoleDef struct {
    Name         string
    PromptPrefix string
    AllowedTools []string
    Policy       *RolePolicy // nil=不增加 role 限制（General/Implementer）；非 nil=收紧 parent guard
}

func AgentRoles() []RoleDef {
    return []RoleDef{
        {
            Name:         "general",
            PromptPrefix: outputContractPrefix("general-purpose assistant"),
            AllowedTools: []string{"*"},
            Policy:       nil, // 继承 parent profile，无额外 role 限制
        },
        {
            Name:         "explore",
            PromptPrefix: outputContractPrefix("read-only explorer. Map and quote evidence; do NOT edit."),
            AllowedTools: []string{"fs_read", "fs_glob", "fs_search", "shell_run", "time_now"},
            Policy:       &RolePolicy{ReadOnlyShell: true},
        },
        {
            Name:         "plan",
            PromptPrefix: outputContractPrefix("planner. Produce a writing-plans style plan; write only to docs/plans or plan workspace."),
            AllowedTools: []string{"fs_read", "fs_glob", "fs_search", "fs_write", "fs_edit", "shell_run", "time_now"},
            Policy: &RolePolicy{
                ReadOnlyShell: true,
                WritePatterns: []string{"docs/plans/*.md", "docs/superpowers/plans/*.md"},
            },
        },
        {
            Name:         "review",
            PromptPrefix: outputContractPrefix("reviewer. Read-only; flag risks and blockers."),
            AllowedTools: []string{"fs_read", "fs_glob", "fs_search", "shell_run", "time_now"},
            Policy:       &RolePolicy{ReadOnlyShell: true},
        },
        {
            Name:         "implementer",
            PromptPrefix: outputContractPrefix("implementer. Apply code changes following TDD and project conventions."),
            AllowedTools: []string{"fs_read", "fs_glob", "fs_search", "fs_write", "fs_edit", "shell_run", "time_now", "memory_*"},
            Policy:       nil, // 继承 parent profile（写权限来自 parent guard，不被空 policy 拦截）
        },
        {
            Name:         "verifier",
            PromptPrefix: outputContractPrefix("verifier. Run tests/linters/vet; cite exact output."),
            AllowedTools: []string{"fs_read", "fs_glob", "fs_search", "shell_run", "time_now"},
            Policy:       &RolePolicy{ReadOnlyShell: true},
        },
        {
            Name:         "custom",
            PromptPrefix: outputContractPrefix("custom subagent. Follow the caller-supplied role instruction without exceeding parent policy."),
            AllowedTools: nil,
            Policy:       nil, // custom 的限制来自 caller Custom 配置 + parent profile
        },
    }
}


func outputContractPrefix(role string) string {
    return fmt.Sprintf(`You are a %s.

Reply with EXACTLY these five sections:
SUMMARY:
CHANGES:
EVIDENCE:
RISKS:
BLOCKERS:
`, role)
}

func MustRole(name string) RoleDef {
    for _, r := range AgentRoles() {
        if r.Name == name { return r }
    }
    panic(fmt.Sprintf("agent role %q not found", name))
}
```

- [ ] **Step 4：GREEN**

Run: `go test ./internal/tools -run 'TestRoleCatalog|TestRoleAllowlist|TestRolePromptPrefix' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/tools/agentroles.go internal/tools/agentroles_test.go
git commit -m "feat(tools): seven-role catalog with policy and output contract"
```

---

## Task 14：Override policy context — Spawn 同步校验、Resume 重验

**Files:**
- Create: `internal/tools/overridepolicy.go`
- Create: `internal/tools/overridepolicy_test.go`

- [ ] **Step 1：失败测试**

```go
package tools

import (
    "context"
    "errors"
    "testing"

    "github.com/stretchr/testify/require"
    "github.com/x6nux/yanshi/internal/guard"
)

func TestValidateOverrideHonorsProfileAndRegistry(t *testing.T) {
    profile := guard.PermissionProfile{
        Subagent: guard.SubagentPerm{
            Models: []string{"gpt-4o-mini"}, MaxReasoning: "medium",
        },
    }
    available := map[string]bool{"gpt-4o-mini": true, "claude-haiku": true}

    require.NoError(t, ValidateOverride(context.Background(), "gpt-4o-mini", "low", profile, available))
    require.Error(t, ValidateOverride(context.Background(), "claude-haiku", "low", profile, available)) // profile reject
    require.Error(t, ValidateOverride(context.Background(), "gpt-4o-mini", "high", profile, available)) // cap reject
    require.Error(t, ValidateOverride(context.Background(), "missing-model", "low", profile, available)) // not in registry
    require.Error(t, ValidateOverride(context.Background(), "gpt-4o-mini", "bogus", profile, available)) // enum reject
}

func TestValidateOverrideEmptyInherits(t *testing.T) {
    profile := guard.PermissionProfile{}
    require.NoError(t, ValidateOverride(context.Background(), "", "", profile, map[string]bool{}))
}

func TestEnsureOverrideIsRevalidatedOnResume(t *testing.T) {
    ctx := context.Background()
    profile := guard.PermissionProfile{
        Subagent: guard.SubagentPerm{Models: []string{"gpt-4o-mini"}, MaxReasoning: "medium"},
    }
    err := EnsureOverrideForResume(ctx, "gpt-4o-mini", "high", profile, map[string]bool{"gpt-4o-mini": true})
    require.Error(t, err)
    require.ErrorIs(t, err, guard.ErrOverrideDenied) // `|| err != nil` 是恒真，删掉
}
```

- [ ] **Step 2：确认 RED**

Run: `go test ./internal/tools -run 'TestValidateOverride|TestEnsureOverrideIsRevalidatedOnResume' -count=1`

Expected: FAIL，`ValidateOverride` 等未定义。

- [ ] **Step 3：实现**

`internal/tools/overridepolicy.go`：

```go
package tools

import (
    "context"
    "fmt"

    "github.com/x6nux/yanshi/internal/guard"
)

type availableModelsKey struct{}

func WithAvailableModels(ctx context.Context, models map[string]bool) context.Context {
    return context.WithValue(ctx, availableModelsKey{}, models)
}

func AvailableModelsFromContext(ctx context.Context) map[string]bool {
    if v, ok := ctx.Value(availableModelsKey{}).(map[string]bool); ok { return v }
    return nil
}

func ValidateOverride(ctx context.Context, model, reasoning string, profile guard.PermissionProfile, available map[string]bool) error {
    if model == "" && reasoning == "" { return nil }
    if available == nil { available = AvailableModelsFromContext(ctx) }
    if model != "" {
        if available == nil || !available[model] {
            return fmt.Errorf("model %q is not available in provider registry: %w", model, guard.ErrOverrideDenied)
        }
        if err := profile.Subagent.CheckModel(model); err != nil {
            return fmt.Errorf("%w: %v", guard.ErrOverrideDenied, err)
        }
    }
    if reasoning != "" {
        if err := profile.Subagent.CheckReasoning(reasoning); err != nil {
            return fmt.Errorf("%w: %v", guard.ErrOverrideDenied, err)
        }
    }
    return nil
}

func EnsureOverrideForResume(ctx context.Context, model, reasoning string, profile guard.PermissionProfile, available map[string]bool) error {
    return ValidateOverride(ctx, model, reasoning, profile, available)
}
```

> 同时在 `internal/guard/profile.go`（Task 10 已引入 `fmt`/`strings`）的 import 块**追加 `"errors"`**，并加入 sentinel（否则 `errors.New` 编译失败）：

```go
import (
    "errors"
    "fmt"
    "strings"
)

var ErrOverrideDenied = errors.New("subagent override denied")
```

- [ ] **Step 4：GREEN**

Run: `go test ./internal/tools -run 'TestValidateOverride|TestEnsureOverrideIsRevalidatedOnResume' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/tools/overridepolicy.go internal/tools/overridepolicy_test.go internal/guard/profile.go
git commit -m "feat(tools): fail-closed subagent model/reasoning override validation"
```

---

## Task 15：ManagedSubAgentRunner — 公共 adapter，统一接入 agent_start/workflow_start/analysis

**Files:**
- Modify: `internal/tools/subagent.go`
- Modify: `internal/tools/agent.go`
- Modify: `internal/tools/agent_workflow.go`
- Modify: `internal/tools/agent_analysis.go`
- Create: `internal/tools/subagent_managed_test.go`

- [ ] **Step 1：失败测试**

```go
package tools

import (
    "context"
    "path/filepath"
    "sync/atomic"
    "testing"

    "github.com/stretchr/testify/require"

    "github.com/x6nux/yanshi/internal/agent/registry"
)

func TestManagedRunnerReservesSlotAndPersists(t *testing.T) {
    mgr, _ := newTestManager(t)
    var calls int32
    runner := registry.RunnerFunc(func(ctx context.Context, agentID, assignment string) (string, error) {
        atomic.AddInt32(&calls, 1)
        return "SUMMARY:\nCHANGES:\nEVIDENCE:\nfile.go:1\nRISKS:\nBLOCKERS:", nil
    })

    spec := ManagedSubAgentSpec{
        Role: "explore", Prompt: "scan", AllowedTools: []string{"fs_read"}, Runner: runner,
    }
    // Manager 走 ctx；ParentID 留空（不存在的 parent 会被 Spawn 拒绝）。
    res, err := ManagedSubAgentRun(WithManager(context.Background(), mgr), spec)
    require.NoError(t, err)
    require.Contains(t, res.Text, "EVIDENCE")
    require.Equal(t, int32(1), atomic.LoadInt32(&calls))

    list := mgr.List(false)
    require.Equal(t, 0, list.Running)
    require.Len(t, list.Agents, 1)
}

func TestManagedRunnerBlocksUntilSlotFrees(t *testing.T) {
    // cap=1：第二个 ManagedSubAgentRun 必须阻塞等 slot 释放，而不是当作 terminal error。
    mgr, _ := newTestManager(t)
    block := make(chan struct{})
    first := registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
        <-block
        return "first", nil
    })
    second := registry.RunnerFunc(func(context.Context, string, string) (string, error) {
        return "second", nil
    })

    done := make(chan error, 2)
    go func() {
        _, err := ManagedSubAgentRun(WithManager(context.Background(), mgr),
            ManagedSubAgentSpec{Role: "explore", Prompt: "a", Runner: first})
        done <- err
    }()
    go func() {
        _, err := ManagedSubAgentRun(WithManager(context.Background(), mgr),
            ManagedSubAgentSpec{Role: "explore", Prompt: "b", Runner: second})
        done <- err
    }()
    close(block)
    require.NoError(t, <-done)
    require.NoError(t, <-done)
    require.Len(t, mgr.List(false).Agents, 2)
}

// newTestManager 构造内存 Manager（C1：NewManager 不返回 error）。storePath 不存在；
// 用 filepath.Join(t.TempDir(), "s.json")。
func newTestManager(t *testing.T) (*registry.Manager, string) {
    t.Helper()
    path := filepath.Join(t.TempDir(), "s.json")
    m := registry.NewManager(registry.NewManagerOpts{
        RootContext: context.Background(), Path: path, SessionBootID: "boot", MaxConcurrent: 4,
    })
    t.Cleanup(m.Close)
    return m, path
}
```


- [ ] **Step 2：确认 RED**

Run: `go test ./internal/tools -run 'TestManagedRunnerReservesSlotAndPersists' -count=1`

Expected: FAIL，`ManagedSubAgentSpec`、`ManagedSubAgentRun`、`WithManager` 未定义。

- [ ] **Step 3：实现**

修改 `internal/tools/subagent.go`：

```go
// ManagedSubAgentSpec 是同步入口（agent_start/workflow/analysis 共用）的入参。
// Runner 由调用方提供：tools 层走 runSubAgentLegacy 包装，orchestrator（Task 18）
// 提供重新绑定 turn context 的 concrete runner（见 ManagedRunnerFactory）。
type ManagedSubAgentSpec struct {
    ParentID        string
    Role            string
    Custom          *registry.CustomRole
    Nickname        string
    Prompt          string
    AllowedTools    []string
    Instruction     string
    ModelOverride   string
    ReasoningEffort string
    Runner          registry.Runner
}

type ManagedSubAgentResult struct {
    Text     string
    AgentID  string
    Usage    registry.Usage
    Terminal registry.Status
}

type managerKey struct{}

func WithManager(ctx context.Context, m *registry.Manager) context.Context {
    return context.WithValue(ctx, managerKey{}, m)
}

func ManagerFromContext(ctx context.Context) *registry.Manager {
    if v, ok := ctx.Value(managerKey{}).(*registry.Manager); ok {
        return v
    }
    return nil
}

// ManagedRunnerFactory 为一个 managed subagent turn 构造 registry.Runner。
// 它捕获 turn 作用域的 context（profile/workroot/VCS/model/depth/emit），因为 Manager
// 派生给 child 的 ctx 只有 cancellation（B1-017：runSubAgentLegacy 在 manager ctx 里拿不到
// profile/VCS/runner，会裸跑丢上下文）。由 orchestrator 在 Task 18 绑定。
type ManagedRunnerFactory func(allowed []string, instruction string) registry.Runner

type managedRunnerFactoryKey struct{}

func WithManagedRunnerFactory(ctx context.Context, f ManagedRunnerFactory) context.Context {
    return context.WithValue(ctx, managedRunnerFactoryKey{}, f)
}

func ManagedRunnerFactoryFromContext(ctx context.Context) ManagedRunnerFactory {
    if v, ok := ctx.Value(managedRunnerFactoryKey{}).(ManagedRunnerFactory); ok {
        return v
    }
    return nil
}

// ManagedSubAgentRun 是同步入口：在 Manager 上 Spawn 一个 managed agent，阻塞等 slot
// （满载 spawnWithRetry 重试 *SpawnErrCap，不当作 terminal error），再 Wait 到 terminal。
// Manager 只管 cancellation/lifecycle/persist/cap；model/tool runner 来自 spec.Runner。
func ManagedSubAgentRun(ctx context.Context, spec ManagedSubAgentSpec) (ManagedSubAgentResult, error) {
    mgr := ManagerFromContext(ctx)
    if mgr == nil {
        return ManagedSubAgentResult{}, fmt.Errorf("managed subagent run: no manager on context")
    }
    if spec.Runner == nil {
        return ManagedSubAgentResult{}, fmt.Errorf("managed subagent run: runner is required")
    }
    role := spec.Role
    if role == "" {
        role = inferRoleFromTools(spec.AllowedTools)
    }
    id, err := spawnWithRetry(ctx, mgr, registry.SpawnRequest{
        AgentType:       "subagent",
        ParentID:        spec.ParentID,
        Role:            role,
        Custom:          spec.Custom,
        Nickname:        spec.Nickname,
        Prompt:          spec.Prompt,
        AllowedTools:    spec.AllowedTools,
        Instruction:     spec.Instruction,
        ModelOverride:   spec.ModelOverride,
        ReasoningEffort: spec.ReasoningEffort,
        Runner:          spec.Runner,
        Emit:            subagentEmitAdapter(ctx),
    })
    if err != nil {
        return ManagedSubAgentResult{}, err
    }
    final, werr := mgr.Wait(ctx, id, registry.WaitOpts{})
    res := ManagedSubAgentResult{
        Text: final.Result, AgentID: final.ID, Usage: final.Usage, Terminal: final.Status,
    }
    if werr != nil && final.Status == "" {
        return res, werr // 未找到记录（final 为零值）才当硬错；取消/超时已返回 latest snapshot
    }
    return res, nil
}

// spawnWithRetry 阻塞等 slot：Spawn 返回 *SpawnErrCap 时按 backoff 重试直到成功/ctx 取消。
// 这是 workflow/agent_start 同步入口的「cap queue」——满载不当作 terminal error（C1 批处理
// 可 spawn 后立刻 Wait，由 Manager 的 cap 信号排队）。非 cap 错误立即返回。
func spawnWithRetry(ctx context.Context, mgr *registry.Manager, req registry.SpawnRequest) (string, error) {
    backoff := 5 * time.Millisecond
    for {
        if err := ctx.Err(); err != nil {
            return "", err
        }
        id, err := mgr.Spawn(ctx, req)
        if err == nil {
            return id, nil
        }
        var capped *registry.SpawnErrCap
        if !errors.As(err, &capped) {
            return "", err
        }
        select {
        case <-ctx.Done():
            return "", ctx.Err()
        case <-time.After(backoff):
        }
        if backoff < 200*time.Millisecond {
            backoff *= 2
        }
    }
}

// subagentEmitAdapter 把 registry.Event 转成 subagent_event ServerFrame 转发给 transport。
// Task 17 用 protoFromRegistryEvent 落地真实转换；在此之前返回 nil，Spawn 仍正常运行，
// 只是不转发 lifecycle event（Task 15 测试不依赖它）。
func subagentEmitAdapter(ctx context.Context) registry.EventSink {
    return nil
}

// inferRoleFromTools 从 allowed tools 推断角色：全部为只读工具 → explore；否则 general。
// "*"（全开）或出现任何写工具一律 general。workflow/analysis/agent_start 在 caller 未显式
// 指定 role 时用此推断。
var readOnlyToolSet = map[string]bool{
    "fs_read": true, "fs_glob": true, "fs_search": true, "shell_run": true, "time_now": true,
}

func inferRoleFromTools(allowed []string) string {
    if len(allowed) == 0 {
        return "general"
    }
    for _, t := range allowed {
        if t == "*" || !readOnlyToolSet[t] {
            return "general"
        }
    }
    return "explore"
}

// ParentWorkingSetHint 从子代理五段式结果里提取 EVIDENCE 段，作为 parent 的 working-set
// 提示附加返回。提取失败（无 EVIDENCE 段）时原样透传全文，不谎称已提取。parent 据此缩小
// 后续搜索范围。section 以 `NAME:` 起行；遇到下一个已知段名或文末结束。
var knownResultSections = []string{"SUMMARY", "CHANGES", "EVIDENCE", "RISKS", "BLOCKERS"}

func ParentWorkingSetHint(result string) string {
    evidence := extractResultSection(result, "EVIDENCE")
    if strings.TrimSpace(evidence) == "" {
        return result
    }
    return result + "\n\n[parent working-set hint — EVIDENCE]\n" + evidence
}

func extractResultSection(result, section string) string {
    lines := strings.Split(result, "\n")
    var out []string
    started := false
    for _, ln := range lines {
        if name := matchResultSection(strings.TrimSpace(ln)); name != "" {
            if name == section {
                started = true
                continue
            }
            if started {
                break // 下一个已知段，结束
            }
            continue
        }
        if started && strings.TrimSpace(ln) != "" {
            out = append(out, ln)
        }
    }
    return strings.Join(out, "\n")
}

func matchResultSection(trimmed string) string {
    for _, n := range knownResultSections {
        if trimmed == n+":" || trimmed == n {
            return n
        }
    }
    return ""
}
```


- [ ] **Step 4：修改 `runSubAgent` 走 managed path**

```go
// runSubAgent 是 agent_start/workflow/analysis 共用的子代理入口。Manager 在 ctx 上时走
// managed path：必须同时有 ManagedRunnerFactory（orchestrator 绑定），否则 fail-closed 报错
// ——不能在 manager 的 cancellation-only ctx 里裸跑 runSubAgentLegacy（会丢 profile/VCS/runner）。
func (t *AgentTools) runSubAgent(ctx context.Context, prompt string, allowed []string, instructionOverride string) (string, error) {
    if mgr := ManagerFromContext(ctx); mgr != nil {
        factory := ManagedRunnerFactoryFromContext(ctx)
        if factory == nil {
            return "", fmt.Errorf("managed subagent run: orchestrator runner factory not bound")
        }
        _ = mgr // Manager 已在 ctx 中，ManagedSubAgentRun 自取
        role := inferRoleFromTools(allowed)
        res, err := ManagedSubAgentRun(ctx, ManagedSubAgentSpec{
            Role:         role,
            Prompt:       prompt,
            AllowedTools: allowed,
            Instruction:  instructionOverride,
            Runner:       factory(allowed, instructionOverride),
        })
        if err != nil {
            return "", err
        }
        return ParentWorkingSetHint(res.Text), nil
    }
    out, err := t.runSubAgentLegacy(ctx, prompt, allowed, instructionOverride)
    if err != nil {
        return "", err
    }
    return ParentWorkingSetHint(out), nil
}
```

> `runSubAgentLegacy` 是把现有 `runSubAgent` body 原样重命名后的私有函数，行为完全不变。
> `ManagedRunnerFactory` 在 Task 18 由 orchestrator 绑定（concrete runner 重新绑 profile/workroot/VCS/model/depth/currentAgentID/emit），保证 Manager 派生的 child ctx 不丢 turn 上下文。


> `runSubAgentLegacy` 是把现有 `runSubAgent` body 原样重命名后的私有函数，行为完全不变。`inferRoleFromTools` 是一个简单映射：若 allowed 只包含只读 fs/shell → `explore`；否则 `general`。

- [ ] **Step 5：修改 `agent_workflow.go`/`agent_analysis.go` 共享 Manager**

把 `runFlatWorkflow` 和 `runDAGWorkflow` 中每个 task 的 `t.runSubAgent(...)` 保持不变即可（已自动走 Manager adapter）。`runAnalysis` 同理。

- [ ] **Step 6：GREEN**

Run: `go test ./internal/tools -run 'TestManagedRunnerReservesSlotAndPersists|TestAgentStart|TestWorkflow|TestAnalysis' -count=1`

Expected: PASS；旧测试因为 Manager 不在 context 中，仍走 legacy path，行为不变。

- [ ] **Step 7：提交**

```bash
git add internal/tools/subagent.go internal/tools/agent.go internal/tools/agent_workflow.go internal/tools/agent_analysis.go internal/tools/subagent_managed_test.go
git commit -m "feat(tools): managed runner adapter unifies subagent entrypoints"
```

---

## Task 16：agent lifecycle 工具族 — spawn/wait/result/send_input/resume/assign/cancel/list

**Files:**
- Create: `internal/tools/agent_lifecycle.go`
- Modify: `internal/tools/agent.go`（加入 lifecycle 工具到 `AgentTools`）
- Create: `internal/tools/agent_lifecycle_test.go`

- [ ] **Step 1：失败测试**

```go
package tools

import (
    "context"
    "testing"
    "time"

    "github.com/stretchr/testify/require"

    "github.com/x6nux/yanshi/internal/agent/registry"
    "github.com/x6nux/yanshi/internal/guard"
)

func TestAgentLifecycleToolsRoundTrip(t *testing.T) {
    at, baseCtx := newAgentTools(t)
    mgr, _ := newTestManager(t)
    block := make(chan struct{})
    ctx := WithManager(baseCtx, mgr)
    // lifecycle 工具的 runner 必须来自 ManagedRunnerFactory（B1-017）；profile 2 值 fail-closed。
    ctx = WithProfile(ctx, guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}})
    ctx = WithManagedRunnerFactory(ctx, func(allowed []string, _ string) registry.Runner {
        return registry.RunnerFunc(func(c context.Context, _, _ string) (string, error) {
            <-block
            return "SUMMARY:\nCHANGES:\nEVIDENCE:\nfile.go\nRISKS:\nBLOCKERS:", nil
        })
    })

    // agent_spawn：返回 id 立即（runner 在后台阻塞等 close(block)）。
    spawnOut, err := at.AgentSpawn.InvokableRun(ctx, `{"prompt":"scan","role":"explore","tools":"[\"fs_read\"]"}`)
    require.NoError(t, err)
    require.Contains(t, spawnOut, `"agent_id"`)

    list := mgr.List(false)
    require.Equal(t, 1, list.Running)
    id := list.Agents[0].ID

    require.NoError(t, mgr.Assign(id, "audit"))
    require.NoError(t, mgr.SendInput(id, "hello", false))
    close(block)

    waitCtx, cancel := context.WithTimeout(ctx, time.Second)
    defer cancel()
    waitOut, err := at.AgentWait.InvokableRun(waitCtx, `{"agent_id":"`+id+`"}`)
    require.NoError(t, err)
    require.Contains(t, waitOut, `"status":"completed"`)

    listOut, err := at.AgentList.InvokableRun(ctx, `{}`)
    require.NoError(t, err)
    require.Contains(t, listOut, id)
}
```


> 工具 helper `makeSpawnRequest` 在 `agent_lifecycle_test.go` 内定义：

```go
func makeSpawnRequest(role, prompt string, run func(context.Context) (string, error)) registry.SpawnRequest {
    return registry.SpawnRequest{
        Role: role, Prompt: prompt,
        Runner: registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
            return run(ctx)
        }),
    }
}
```


- [ ] **Step 2：确认 RED**

Run: `go test ./internal/tools -run 'TestAgentLifecycleToolsRoundTrip' -count=1`

Expected: FAIL，`AgentSpawn`/`AgentWait`/`AgentList` 等未定义。

- [ ] **Step 3：实现**

```go
package tools

import (
    "context"
    "encoding/json"
    "fmt"
    "time"

    "github.com/cloudwego/eino/schema"

    "github.com/x6nux/yanshi/internal/agent/registry"
)

type agentSpawnArgs struct {
    Prompt          string `json:"prompt"`
    Role            string `json:"role"`
    Tools           string `json:"tools"`
    Nickname        string `json:"nickname,omitempty"`
    ModelOverride   string `json:"model,omitempty"`
    ReasoningEffort string `json:"reasoning,omitempty"`
}

type agentSpawnResult struct {
    AgentID string `json:"agent_id"`
    Status  string `json:"status"`
}

func (t *AgentTools) streamAgentSpawn(ctx context.Context, argsJSON string) <-chan ToolChunk {
    ch := make(chan ToolChunk, 1)
    go func() {
        defer close(ch)
        var a agentSpawnArgs
        if err := ParseArgs(argsJSON, &a); err != nil { pushErrChunk(ch, err); return }
        mgr := ManagerFromContext(ctx)
        if mgr == nil { pushErrChunk(ch, fmt.Errorf("agent_spawn: manager not configured")); return }

        allowed, err := parseToolList(a.Tools)
        if err != nil { pushErrChunk(ch, err); return }
        if a.Role == "" { a.Role = "general" }

        // Override 同步校验（详见 Task 14）。profile 必须 2 值取出；未绑定 → fail-closed，
        // 绝不走零值 profile 绕过 guard。
        profile, ok := ProfileFromContext(ctx)
        if !ok { pushErrChunk(ch, fmt.Errorf("agent_spawn: permission profile not bound")); return }
        if err := ValidateOverride(ctx, a.ModelOverride, a.ReasoningEffort, profile, AvailableModelsFromContext(ctx)); err != nil {
            pushErrChunk(ch, err); return
        }

        // Runner 必须来自 ManagedRunnerFactory（orchestrator 绑定）：Manager 派生给 child 的 ctx
        // 只有 cancellation，runSubAgentLegacy 在里面拿不到 profile/VCS/runner（B1-017）。
        // factory 缺失 → fail-closed，不能裸跑。
        factory := ManagedRunnerFactoryFromContext(ctx)
        if factory == nil { pushErrChunk(ch, fmt.Errorf("agent_spawn: orchestrator runner factory not bound")); return }
        runner := factory(allowed, "")

        id, err := mgr.Spawn(ctx, registry.SpawnRequest{
            Role: a.Role, Nickname: a.Nickname, Prompt: a.Prompt,
            AllowedTools: allowed, ModelOverride: a.ModelOverride, ReasoningEffort: a.ReasoningEffort,
            Runner: runner, Emit: subagentEmitAdapter(ctx),
        })
        if err != nil { pushErrChunk(ch, err); return }

        snap, _ := mgr.Result(id)
        status := "running"
        if snap.Status != "" { status = string(snap.Status) }
        body, _ := json.Marshal(agentSpawnResult{AgentID: id, Status: status})
        ch <- ToolChunk{Result: string(body)}
    }()
    return ch
}

type agentWaitArgs struct {
    AgentID string `json:"agent_id"`
    Timeout int    `json:"timeout,omitempty"`
}

func (t *AgentTools) streamAgentWait(ctx context.Context, argsJSON string) <-chan ToolChunk {
    ch := make(chan ToolChunk, 1)
    go func() {
        defer close(ch)
        var a agentWaitArgs
        if err := ParseArgs(argsJSON, &a); err != nil { pushErrChunk(ch, err); return }
        mgr := ManagerFromContext(ctx)
        if mgr == nil { pushErrChunk(ch, fmt.Errorf("agent_wait: manager not configured")); return }

        waitCtx := ctx
        if a.Timeout > 0 {
            var cancel context.CancelFunc
            waitCtx, cancel = context.WithTimeout(ctx, time.Duration(a.Timeout)*time.Second)
            defer cancel()
        }
        // C1：Wait 返回 (Record, error)。无论超时/取消都返回 latest snapshot；只有记录不存在
        // （rec.ID == ""）才当硬错——否则 caller 拿不到中途状态。
        rec, werr := mgr.Wait(waitCtx, a.AgentID, registry.WaitOpts{})
        if werr != nil && rec.ID == "" { pushErrChunk(ch, werr); return }
        out, _ := json.Marshal(rec)
        ch <- ToolChunk{Result: string(out)}
    }()
    return ch
}

type agentResultArgs struct{ AgentID string `json:"agent_id"` }
type agentSendInputArgs struct {
    AgentID   string `json:"agent_id"`
    Text      string `json:"text"`
    Interrupt bool   `json:"interrupt,omitempty"`
}
type agentResumeArgs struct {
    AgentID string `json:"agent_id"`
    Prompt  string `json:"prompt,omitempty"`
}
type agentAssignArgs struct {
    AgentID    string `json:"agent_id"`
    Assignment string `json:"assignment"`
}
type agentCancelArgs struct{ AgentID string `json:"agent_id"` }
type agentListArgs struct {
    IncludeArchived bool `json:"include_archived,omitempty"`
}

func (t *AgentTools) streamAgentResult(ctx context.Context, argsJSON string) <-chan ToolChunk {
    return simpleStream(argsJSON, func(a agentResultArgs) (string, error) {
        mgr := ManagerFromContext(ctx)
        if mgr == nil { return "", fmt.Errorf("agent_result: manager not configured") }
        rec, ok := mgr.Result(a.AgentID)
        if !ok { return "", fmt.Errorf("agent_result: %q not found", a.AgentID) }
        out, _ := json.Marshal(rec)
        return string(out), nil
    })
}

func (t *AgentTools) streamAgentSendInput(ctx context.Context, argsJSON string) <-chan ToolChunk {
    return simpleStream(argsJSON, func(a agentSendInputArgs) (string, error) {
        mgr := ManagerFromContext(ctx)
        if mgr == nil { return "", fmt.Errorf("agent_send_input: manager not configured") }
        if err := mgr.SendInput(a.AgentID, a.Text, a.Interrupt); err != nil { return "", err }
        return `{"ok":true}`, nil
    })
}

func (t *AgentTools) streamAgentResume(ctx context.Context, argsJSON string) <-chan ToolChunk {
    return simpleStream(argsJSON, func(a agentResumeArgs) (string, error) {
        mgr := ManagerFromContext(ctx)
        if mgr == nil { return "", fmt.Errorf("agent_resume: manager not configured") }
        // Resume 重验 override（详见 Task 14）。原记录的 override 已持久化，读出再校验。
        rec, ok := mgr.Result(a.AgentID)
        if !ok { return "", registry.ErrNotFound }
        profile, pok := ProfileFromContext(ctx)
        if !pok { return "", fmt.Errorf("agent_resume: permission profile not bound") }
        if err := EnsureOverrideForResume(ctx, rec.ModelOverride, rec.ReasoningEffort, profile, AvailableModelsFromContext(ctx)); err != nil {
            return "", err
        }
        factory := ManagedRunnerFactoryFromContext(ctx)
        if factory == nil { return "", fmt.Errorf("agent_resume: orchestrator runner factory not bound") }
        runner := factory(rec.AllowedTools, rec.Instruction)
        // C1：Resume 返回 agentID（string）。
        if _, err := mgr.Resume(a.AgentID, registry.ResumeRequest{Runner: runner, Emit: subagentEmitAdapter(ctx)}); err != nil {
            return "", err
        }
        snap, _ := mgr.Result(a.AgentID)
        out, _ := json.Marshal(snap)
        return string(out), nil
    })
}

func (t *AgentTools) streamAgentAssign(ctx context.Context, argsJSON string) <-chan ToolChunk {
    return simpleStream(argsJSON, func(a agentAssignArgs) (string, error) {
        mgr := ManagerFromContext(ctx)
        if mgr == nil { return "", fmt.Errorf("agent_assign: manager not configured") }
        if err := mgr.Assign(a.AgentID, a.Assignment); err != nil { return "", err }
        return `{"ok":true}`, nil
    })
}

func (t *AgentTools) streamAgentCancel(ctx context.Context, argsJSON string) <-chan ToolChunk {
    return simpleStream(argsJSON, func(a agentCancelArgs) (string, error) {
        mgr := ManagerFromContext(ctx)
        if mgr == nil { return "", fmt.Errorf("agent_cancel: manager not configured") }
        if err := mgr.Cancel(a.AgentID); err != nil { return "", err }
        return `{"ok":true}`, nil
    })
}

func (t *AgentTools) streamAgentList(ctx context.Context, argsJSON string) <-chan ToolChunk {
    return simpleStream(argsJSON, func(a agentListArgs) (string, error) {
        mgr := ManagerFromContext(ctx)
        if mgr == nil { return "", fmt.Errorf("agent_list: manager not configured") }
        out, _ := json.Marshal(mgr.List(a.IncludeArchived))
        return string(out), nil
    })
}

// simpleStream 在一个 chunk 里返回解析+调用结果，保持与现有 GuardedTool 语义一致。
func simpleStream[Req any](argsJSON string, fn func(Req) (string, error)) <-chan ToolChunk {
    ch := make(chan ToolChunk, 1)
    go func() {
        defer close(ch)
        var req Req
        if err := ParseArgs(argsJSON, &req); err != nil { pushErrChunk(ch, err); return }
        out, err := fn(req)
        if err != nil { pushErrChunk(ch, err); return }
        ch <- ToolChunk{Result: out}
    }()
    return ch
}
```


把每个 GuardedTool 装到 `AgentTools` 上（修改 constructor 末尾）：

```go
t.AgentSpawn = NewGuardedTool("agent_spawn", "Agent", "Spawn a managed subagent and return its id immediately.", 0,
    params(map[string]*schema.ParameterInfo{
        "prompt":   {Type: schema.String, Required: true},
        "role":     {Type: schema.String},
        "tools":    {Type: schema.String},
        "nickname": {Type: schema.String},
        "model":    {Type: schema.String},
        "reasoning":{Type: schema.String},
    }), t.streamAgentSpawn)

t.AgentWait = NewGuardedTool("agent_wait", "Agent", "Wait for a managed subagent to reach a terminal state.", 0,
    params(map[string]*schema.ParameterInfo{
        "agent_id": {Type: schema.String, Required: true},
        "timeout":  {Type: schema.Integer},
    }), t.streamAgentWait)

t.AgentResult = NewGuardedTool("agent_result", "Agent", "Return a snapshot of a managed subagent.", 0,
    params(map[string]*schema.ParameterInfo{
        "agent_id": {Type: schema.String, Required: true},
    }), t.streamAgentResult)

t.AgentSendInput = NewGuardedTool("agent_send_input", "Agent", "Queue follow-up input for a managed subagent.", 0,
    params(map[string]*schema.ParameterInfo{
        "agent_id":  {Type: schema.String, Required: true},
        "text":      {Type: schema.String, Required: true},
        "interrupt": {Type: schema.Boolean},
    }), t.streamAgentSendInput)

t.AgentResume = NewGuardedTool("agent_resume", "Agent", "Resume an interrupted or completed subagent.", 0,
    params(map[string]*schema.ParameterInfo{
        "agent_id": {Type: schema.String, Required: true},
        "prompt":   {Type: schema.String},
    }), t.streamAgentResume)

t.AgentAssign = NewGuardedTool("agent_assign", "Agent", "Assign a goal to a managed subagent.", 0,
    params(map[string]*schema.ParameterInfo{
        "agent_id":    {Type: schema.String, Required: true},
        "assignment":  {Type: schema.String, Required: true},
    }), t.streamAgentAssign)

t.AgentCancel = NewGuardedTool("agent_cancel", "Agent", "Cancel a managed subagent.", 0,
    params(map[string]*schema.ParameterInfo{
        "agent_id": {Type: schema.String, Required: true},
    }), t.streamAgentCancel)

t.AgentList = NewGuardedTool("agent_list", "Agent", "List managed subagents (current session by default).", 0,
    params(map[string]*schema.ParameterInfo{
        "include_archived": {Type: schema.Boolean},
    }), t.streamAgentList)
```

> `ProfileFromContext` 已存在（见 `internal/tools/guard.go`，返回 `(guard.PermissionProfile, bool)`）。lifecycle 工具一律 2 值取出，未绑定 profile 时 **fail-closed** 报错——绝不走零值 profile 绕过 guard。

- [ ] **Step 4：GREEN**

Run: `go test ./internal/tools -run 'TestAgentLifecycleToolsRoundTrip|TestAgentStart|TestWorkflow|TestAnalysis|TestSummarize' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/tools/agent_lifecycle.go internal/tools/agent.go internal/tools/agent_lifecycle_test.go
git commit -m "feat(tools): managed agent lifecycle tools"
```

---

## Task 17：`subagent_event` ServerFrame + proto 同步

**Files:**
- Modify: `internal/proto/frame.go`
- Modify: `internal/proto/frame_test.go`
- Create: `internal/tools/protoadapter.go`

- [ ] **Step 1：失败测试**

```go
package proto

import (
    "testing"

    "github.com/stretchr/testify/require"
)

func TestNewSubagentEventFrame(t *testing.T) {
    f := NewSubagentEvent("ag-1", "explore", "started", "running", "scanning")
    require.Equal(t, "subagent_event", f.Type)
    require.Equal(t, "ag-1", f.AgentID)
    require.Equal(t, "explore", f.AgentRole)
    require.Equal(t, "started", f.Event)
    require.Equal(t, "running", f.AgentStatus)

    event, data := f.SSEEvent()
    require.Equal(t, "subagent_event", event)
    require.Contains(t, string(data), `"agent_id":"ag-1"`)
}
```

- [ ] **Step 2：确认 RED**

Run: `go test ./internal/proto -run 'TestNewSubagentEventFrame' -count=1`

Expected: FAIL。

- [ ] **Step 3：实现**

修改 `internal/proto/frame.go`：在 `ServerFrame` struct 上新增 4 个字段（与现有字段并列，`omitempty` 保持旧客户端/旧帧不受影响），再加构造器。

```go
// ServerFrame 新增字段（与现有字段并列；omitempty 保持向后兼容）：
AgentID     string `json:"agent_id,omitempty"`     // subagent_event
AgentRole   string `json:"agent_role,omitempty"`   // subagent_event
Event       string `json:"event,omitempty"`        // subagent_event: started/usage/completed/failed/cancelled/resumed/...
AgentStatus string `json:"agent_status,omitempty"` // subagent_event: running/completed/cancelled/failed

const SubagentEventType = "subagent_event"

func NewSubagentEvent(agentID, role, event, status, text string) ServerFrame {
    return ServerFrame{
        Type: SubagentEventType, AgentID: agentID, AgentRole: role,
        Event: event, AgentStatus: status, Text: text,
    }
}
```

> `SSEEvent()`（`internal/proto/frame.go:395`）无需改动——它直接用 `f.Type` 作 event 名，`subagent_event` 自动成立；WS 与 SSE 共用同一帧词表（`internal/proto/frame.go` 的 `ServerFrame`）。

创建 `internal/tools/protoadapter.go`：

```go
package tools

import (
    "github.com/x6nux/yanshi/internal/agent/registry"
    "github.com/x6nux/yanshi/internal/proto"
)

// protoFromRegistryEvent 把 registry lifecycle event 映射成 subagent_event ServerFrame，
// 供 subagentEmitAdapter 转发给 transport（WS/SSE）。
func protoFromRegistryEvent(ev registry.Event) proto.ServerFrame {
    return proto.NewSubagentEvent(ev.AgentID, ev.Role, string(ev.Type), string(ev.Status), ev.Text)
}
```

把 `internal/tools/subagent.go` 里 Task 15 的 `subagentEmitAdapter` nil stub 换成真实转发（`protoFromRegistryEvent` 此刻已可用）：

```go
func subagentEmitAdapter(ctx context.Context) registry.EventSink {
    emit := SubAgentEmitFrom(ctx)
    if emit == nil {
        return nil
    }
    return func(ev registry.Event) { emit(protoFromRegistryEvent(ev)) }
}
```

- [ ] **Step 4：CLI StreamEvent 透传 subagent_event**

`internal/cli/backend.go` 的 `StreamEvent`（`backend.go:19`）新增 4 字段，与 Phase-10 控制字段并列：

```go
// subagent_event 透传字段（B1 Task 17）。
AgentID     string // subagent_event
AgentRole   string // subagent_event
Event       string // subagent_event: started/usage/completed/...
AgentStatus string // subagent_event: running/completed/cancelled/failed
```

`internal/cli/wsbackend.go` 的 `toStreamEvent`（`wsbackend.go:251`）在返回的 `StreamEvent` 里拷贝这 4 字段：

```go
return StreamEvent{
    // ...existing fields...
    AgentID:     f.AgentID,
    AgentRole:   f.AgentRole,
    Event:       f.Event,
    AgentStatus: f.AgentStatus,
}
```

> `isControlReply`（`wsbackend.go:240`）只对 `models/status/mcp_list/sessions/session_restored/session_ack` 返回 true——`subagent_event` 本就不在其中，作为普通流式事件下发，**无需改动**。保留这一不变量：不要把 `subagent_event` 加进 `isControlReply`，否则它会被 control-mode 的单帧 cur 通道吞掉、进不了主 transcript 流。

- [ ] **Step 5：TUI 渲染 subagent_event**

`internal/cli/tui/entries.go` 新增 `subagentEntry`（实现 `entry` 接口，仿 `summaryEntry`/`errorEntry`）：

```go
// subagentEntry 渲染一条 managed subagent lifecycle event（started/usage/completed/…），
// 缩进显示在 transcript 中。marker 随 status 变化：running=… / completed=✓ / failed|cancelled=✗。
type subagentEntry struct {
    agentID string
    role    string
    status  string
    text    string // 最近一条 event 文本
}

func (e subagentEntry) render(_ int, _ spinner.Model) string {
    marker := "•"
    switch e.status {
    case "completed":
        marker = "✓"
    case "failed", "cancelled":
        marker = "✗"
    case "running":
        marker = "…"
    }
    return fmt.Sprintf("  %s [%s] %s %s", marker, e.role, e.agentID, e.text)
}
```

`internal/cli/tui/model.go` 的 `applyEvent` switch（`model.go:856`）新增分支：

```go
case "subagent_event":
    m.entries = append(m.entries, subagentEntry{
        agentID: ev.AgentID, role: ev.AgentRole, status: ev.AgentStatus, text: ev.Text,
    })
```

- [ ] **Step 6：GREEN**

Run: `go test ./internal/proto -run 'TestNewSubagentEventFrame' -count=1 && go build ./internal/tools ./internal/cli ./internal/cli/tui`

Expected: proto 测试 PASS，且 tools/cli/tui 编译通过（subagent_event 透传链路接线完成）。

- [ ] **Step 7：提交**

```bash
git add internal/proto/frame.go internal/proto/frame_test.go internal/tools/protoadapter.go internal/tools/subagent.go internal/cli/backend.go internal/cli/wsbackend.go internal/cli/tui/entries.go internal/cli/tui/model.go
git commit -m "feat(proto): subagent_event frame shared by WS and SSE"
```

---

## Task 18：Orchestrator adapter — Managed runner、四 turn 注入点、depth/usage callback

**Files:**
- Modify: `internal/agent/orchestrator/orchestrator.go`
- Create: `internal/agent/orchestrator/orchestrator_managed_test.go`

- [ ] **Step 1：失败测试**

```go
package orchestrator

import (
    "context"
    "path/filepath"
    "testing"

    "github.com/stretchr/testify/require"

    "github.com/x6nux/yanshi/internal/agent/registry"
    "github.com/x6nux/yanshi/internal/guard"
    einollm "github.com/x6nux/yanshi/internal/llm/eino"
    "github.com/x6nux/yanshi/internal/tools"
)

func TestManagedRunnerBindsManagerAndFactory(t *testing.T) {
    mdl := einollm.NewFakeModelWithMessages(nil, nil)
    mgr := registry.NewManager(registry.NewManagerOpts{
        RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
        SessionBootID: "boot", MaxConcurrent: 4,
    })
    t.Cleanup(mgr.Close)

    agentTools := tools.NewAgentTools(mdl)
    o, err := New(Config{
        Model:           mdl,
        Tools:           []BaseTool{agentTools.StartAgent},
        Profile:         guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
        SubagentManager: mgr,
        AvailableModels: map[string]bool{"fake": true},
    })
    require.NoError(t, err)

    // 四个入口点都调 bindManagedRunner；直接验证它把 Manager + factory + available models
    // 都绑进 ctx（不实际跑模型）。
    ctx := o.bindManagedRunner(context.Background())
    require.Equal(t, mgr, tools.ManagerFromContext(ctx))
    require.NotNil(t, tools.ManagedRunnerFactoryFromContext(ctx))
    require.NotNil(t, tools.AvailableModelsFromContext(ctx))

    // factory 构造的 runner 必须非空（concrete managedTurnRunner，Run 时重绑 turn context）。
    runner := tools.ManagedRunnerFactoryFromContext(ctx)([]string{"fs_read"}, "")
    require.NotNil(t, runner)
}
```

> 真正 end-to-end 行为测试（agent_start 经 Manager spawn → 跑嵌套 turn → 收到 subagent_event）在 Task 21 的集成测试里跑；本任务只断言四个入口点都能注入 Manager / ManagedRunnerFactory / available models，且 factory 能构造 runner。


> 真正 end-to-end 行为测试在 Task 21 的集成测试里跑；本任务只断言四个入口点都能注入 manager/role policy/override 校验 context。

- [ ] **Step 2：确认 RED**

Run: `go test ./internal/agent/orchestrator -run 'TestManagedRunnerBindsToAllFourTurnEntryPoints' -count=1`

Expected: FAIL，`SubagentManager`、`AvailableModels`、`bindManagedRunner` 未定义。

- [ ] **Step 3：实现**

修改 `internal/agent/orchestrator/orchestrator.go`。Config 与 Orchestrator 各加两个字段，`New()` 里装配：

```go
type Config struct {
    // ...existing fields unchanged...
    SubagentManager *registry.Manager
    AvailableModels map[string]bool
}

type Orchestrator struct {
    // ...existing fields unchanged...
    subagentMgr    *registry.Manager
    availableModels map[string]bool
}

// in New(), after the existing field assignments:
o.subagentMgr = cfg.SubagentManager
o.availableModels = cfg.AvailableModels
```

**`bindManagedRunner` 替换四入口点里的 `bindSubAgentRunner`**（`Query`/`Events`/`EventsWithHistory`/`EventsWithHistoryOpts`）。它先绑 legacy `SubAgentRunner`（fallback / 非 managed 路径），再在 `SubagentManager` 配置时绑 Manager / available models / **ManagedRunnerFactory**：

```go
// bindManagedRunner 把 managed subagent 所需的 context 全部绑好：legacy SubAgentRunner
// （非 managed 路径仍可用）+ Manager + available models + ManagedRunnerFactory。factory
// 捕获本 turn 的 profile/workroot/VCS/emit/depth，构造的 concrete runner 在 Run 时把这些
// 重新绑回 Manager 派生的 cancellation-only child ctx（B1-017：不重绑则 runSubAgentTurn 在
// manager ctx 里拿不到 profile/VCS/runner，会裸跑丢上下文）。
func (o *Orchestrator) bindManagedRunner(ctx context.Context) context.Context {
    ctx = o.bindSubAgentRunner(ctx)
    if o.subagentMgr == nil {
        return ctx
    }
    ctx = tools.WithManager(ctx, o.subagentMgr)
    if o.availableModels != nil {
        ctx = tools.WithAvailableModels(ctx, o.availableModels)
    }
    profile, workRoot, vcsScope := o.profile, o.workRoot, o.vcsScope
    mgr := o.subagentMgr
    depth := tools.SubAgentDepth(ctx)
    emit := tools.SubAgentEmitFrom(ctx) // CLI/test 路径为 nil
    factory := tools.ManagedRunnerFactory(func(allowed []string, instruction string) registry.Runner {
        return &managedTurnRunner{
            o: o, mgr: mgr, profile: profile, workRoot: workRoot, vcsScope: vcsScope,
            depth: depth, emit: emit, allowed: allowed, instruction: instruction,
        }
    })
    return tools.WithManagedRunnerFactory(ctx, factory)
}
```

> 把 `Query`/`Events`/`EventsWithHistory`/`EventsWithHistoryOpts` 里现有的 `ctx = o.bindSubAgentRunner(ctx)` 全部换成 `ctx = o.bindManagedRunner(ctx)`。`bindSubAgentRunner` 保留为 fallback。

**`managedTurnRunner`——concrete `registry.Runner`（#9 核心）：**

```go
// managedTurnRunner 是 concrete registry.Runner：在 Manager 派生的 cancellation-only child
// ctx 上重新绑 profile/workroot/VCS/emit/depth/currentAgentID/role + usage sink，再委托
// runSubAgentTurn 跑嵌套 orchestrator。role 与 currentAgentID 由 Manager 放进 child ctx
// （registry.WithRole / registry.WithCurrentAgentID，见 Task 3 runAgentLoop）。
type managedTurnRunner struct {
    o           *Orchestrator
    mgr         *registry.Manager
    profile     guard.PermissionProfile
    workRoot    string
    vcsScope    tools.VCSScope
    depth       int
    emit        tools.SubAgentEmit
    allowed     []string
    instruction string
}

func (r *managedTurnRunner) Run(ctx context.Context, agentID, assignment string) (string, error) {
    // 重绑 turn 作用域 context（B1-017）。
    ctx = tools.WithProfile(ctx, r.profile)
    ctx = tools.WithWorkRoot(ctx, r.workRoot)
    if r.vcsScope.VCS != nil {
        ctx = tools.WithVCS(ctx, r.vcsScope)
    }
    if r.emit != nil {
        ctx = tools.WithSubAgentEmit(ctx, r.emit)
    }
    ctx = tools.WithSubAgentDepth(ctx, r.depth) // runSubAgentTurn 内部再 +1
    ctx = registry.WithCurrentAgentID(ctx, agentID)
    // usage 回调：嵌套 turn 每次 model call 的用量报回 Manager（Task 9 AddUsage 数据源）。
    ctx = tools.WithUsageSink(ctx, tools.UsageSink(func(u registry.Usage) {
        _ = r.mgr.AddUsage(agentID, u)
    }))
    // role：据 record.Role 收紧 guard（PromptPrefix 拼进 instruction，Policy 进 RolePolicy）。
    instruction := r.instruction
    if role := registry.RoleFromContext(ctx); role != "" {
        if def := roleForSubagent(role); def != nil {
            if def.PromptPrefix != "" {
                instruction = def.PromptPrefix + "\n\n" + instruction
            }
            if def.Policy != nil {
                ctx = tools.WithRolePolicy(ctx, *def.Policy)
            }
        }
    }
    return r.o.runSubAgentTurn(ctx, assignment, r.allowed, instruction, r.depth)
}

// roleForSubagent 按 name 在 tools.AgentRoles() 查 RoleDef；找不到返回 nil（custom 的限制
// 来自 caller Custom + parent profile，不在此收紧）。这是 #7 的完整实现。
func roleForSubagent(name string) *tools.RoleDef {
    for _, r := range tools.AgentRoles() {
        if r.Name == name {
            def := r
            return &def
        }
    }
    return nil
}
```

**`runSubAgentTurn` 的 onUsage 闭包追加一行**，把嵌套 turn 的用量经 sink 报回 Manager：

```go
// 既有 onUsage 闭包（ClassifyEventsWithUsage 的回调）里，累加 subUsage 之后追加：
if sink := tools.UsageSinkFrom(ctx); sink != nil {
    sink(registry.Usage{
        PromptTokens: latest.PromptTokens, CompletionTokens: latest.CompletionTokens,
        TotalTokens: latest.TotalTokens, ModelCalls: 1,
    })
}
```

> `tools.WithUsageSink` / `tools.UsageSinkFrom` / `type UsageSink func(registry.Usage)` 在 `internal/tools/subagent.go` 新增（与 `WithManagedRunnerFactory` 同文件）。`registry.WithCurrentAgentID` / `registry.RoleFromContext` 在 `internal/agent/registry` 新增，且 Task 3 的 `runAgentLoop` 在调 `runner.Run` 前用 `registry.WithCurrentAgentID(childCtx, agentID)` + `registry.WithRole(childCtx, rec.Role)` 装饰 child ctx——这样 concrete runner 能自动派生 ParentID 与 role policy，无需把 role 塞进 factory 签名（factory 保持 2 参）。

- [ ] **Step 4：GREEN**

Run: `go test ./internal/agent/orchestrator -run 'TestManagedRunnerBindsToAllFourTurnEntryPoints' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/agent/orchestrator/orchestrator.go internal/agent/orchestrator/orchestrator_managed_test.go internal/tools/subagent.go internal/agent/registry/manager.go
git commit -m "feat(orchestrator): inject manager/role policy/override validator into four turn entrypoints"
```

---

## Task 19：WS lifecycle relay 与断开策略（继续运行，不持有 conn）

**Files:**
- Modify: `internal/api/http/ws.go`
- Create: `internal/api/http/subagent_relay.go`
- Modify: `internal/api/http/ws_subemit_test.go`

- [ ] **Step 1：失败测试**

在现有 `ws_subemit_test.go` 使用其真实 `httptest.NewServer`、`dial`、`readFrame` helper，追加以下 helper 和测试。`newSubagentEmitTestTool` 在本 Task 定义，Task 20 的 SSE 测试直接复用，避免重复/未定义 helper（#6）：

```go
func newSubagentEmitTestTool(t *testing.T) *tools.GuardedTool {
    t.Helper()
    return tools.NewGuardedTool(
        "emit_subagent_test", "Test", "emit one typed subagent event", time.Second,
        schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{}),
        func(ctx context.Context, _ string) <-chan tools.ToolChunk {
            ch := make(chan tools.ToolChunk, 1)
            go func() {
                defer close(ch)
                emit := tools.SubAgentEmitFrom(ctx)
                require.NotNil(t, emit)
                emit(proto.NewSubagentEvent("ag-test", "explore", "started", "running", "scan"))
                ch <- tools.ToolChunk{Result: `{"ok":true}`}
            }()
            return ch
        },
    )
}

func TestChatWS_ForwardsTypedSubagentEvent(t *testing.T) {
    emitCall := schema.AssistantMessage("", []schema.ToolCall{{
        ID: "emit-1", Type: "function",
        Function: schema.FunctionCall{Name: "emit_subagent_test", Arguments: `{}`},
    }})
    done := schema.AssistantMessage("top done", nil)
    mdl := einollm.NewFakeModelWithMessages([]*schema.Message{emitCall, done}, nil)

    o, err := orchestrator.New(orchestrator.Config{
        Model: mdl, Tools: []orchestrator.BaseTool{newSubagentEmitTestTool(t)},
        Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
    })
    require.NoError(t, err)

    s := New(Config{Token: "t"})
    s.ChatWS(o, nil, nil)
    ts := httptest.NewServer(s.Handler())
    t.Cleanup(ts.Close)
    c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
    defer c.Close()
    require.NoError(t, c.WriteJSON(proto.NewUserMessage("emit")))

    for {
        f := readFrame(t, c)
        if f.Type == proto.SubagentEventType {
            require.Equal(t, "ag-test", f.AgentID)
            require.Equal(t, "started", f.Event)
            return
        }
        if f.Type == "done" || f.Type == "error" {
            t.Fatalf("typed event missing before %s", f.Type)
        }
    }
}
```

> `newSubagentEmitTestTool` 放在 `ws_subemit_test.go`（`package http`），同包测试文件可直接调用。若 `schema.NewParamsOneOfByParams` 不存在，使用仓库 `params(...)` 不可行（未导出）—— 但本仓库已引入 `schema.NewParamsOneOfByParams` (B1-003)，故直接用。

- [ ] **Step 2：确认 RED**

Run: `go test -race ./internal/api/http -run 'TestChatWS_ForwardsTypedSubagentEvent' -count=1`

Expected: FAIL，WS 尚未转发 typed subagent event。

- [ ] **Step 3：实现 detach-safe relay + 单元测试**

创建 `internal/api/http/subagent_relay.go`：

```go
package http

import (
    "fmt"
    "sync"

    "github.com/x6nux/yanshi/internal/proto"
)

// subagentRelay lets long-lived background agents retain a callback without
// retaining the WebSocket connection. Detach nils the writer on disconnect;
// later lifecycle events are intentionally dropped (Manager persists them).
//
// Emit holds its RLock across the write call so Detach (write-lock) waits for
// all in-flight writes before returning — no write uses a nil/disconnected conn.
type subagentRelay struct {
    mu    sync.RWMutex
    write func(proto.ServerFrame)
}

func newSubagentRelay(write func(proto.ServerFrame)) *subagentRelay {
    return &subagentRelay{write: write}
}

// Emit calls the bound writer under RLock so Detach's write-lock waits for
// in-flight writes to drain before releasing the conn reference (#16).
func (r *subagentRelay) Emit(f proto.ServerFrame) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    if r.write != nil {
        r.write(f)
    }
}

// Detach nils the writer under write-lock. The lock waits for all in-flight
// Emit calls to complete, so the caller can safely close the WS conn after
// Detach returns. Does NOT cancel the running managed agent (#18)：terminal
// 会自然发生，状态已持久化；断线期间 event 丢弃但 AgentTool 的状态不变。
func (r *subagentRelay) Detach() {
    r.mu.Lock()
    r.write = nil
    r.mu.Unlock()
}

// RelayEmit emits a frame via r when r is non-nil; a no-op when r is nil.
// Allows the SSE relay (Task 20) to share the same signature without nil checks.
var _ fmt.Stringer // keep import
```

修改 `internal/api/http/ws.go`：在 `ChatWS` handler 入口建立 relay，传给 `makeTurnCtx` 的 emit closure：

```go
// 在 connCtx / pt / frames / errCh 初始化后，turnCancel / makeTurnCtx 之前建立：
relay := newSubagentRelay(conn.write)
defer relay.Detach()

// makeTurnCtx 的 emit closure 改为调 relay.Emit：
makeTurnCtx := func() (ctx context.Context, release func()) {
    userCancelCtx, userCancel := context.WithCancel(connCtx)
    tctx, tc := context.WithCancel(userCancelCtx)
    tctx = einollm.WithUserCancelCtx(tctx, userCancelCtx)
    tctx = tools.WithSubAgentEmit(tctx, relay.Emit)
    // ...其余不变...
}
```

> WS 断开时 handler 返回，`defer relay.Detach()` 执行：等待所有在途 Emit 完成后置 writer 为 nil，此后 relay.Emit 读完 nil 后直接返回。**不调用 Manager.Cancel**、不按 session 遍历 bulk cancel。Manager terminal 后自动清理 runtime；断线期间 event 丢弃但状态照常持久化。

同文件（`ws_subemit_test.go`）追加 relay 单元测试，覆盖 Detach 的写锁等待（#16）和断线不取消（#18）：

```go
func TestSubagentRelayDetachWaitsForInflightWrite(t *testing.T) {
    entered := make(chan struct{})
    release := make(chan struct{})
    relay := newSubagentRelay(func(proto.ServerFrame) {
        close(entered)
        <-release
    })
    emitDone := make(chan struct{})
    go func() { relay.Emit(proto.NewSubagentEvent("ag", "explore", "started", "running", "")); close(emitDone) }()
    <-entered

    detachDone := make(chan struct{})
    go func() { relay.Detach(); close(detachDone) }()
    select {
    case <-detachDone:
        t.Fatal("Detach returned while writer was still in flight")
    case <-time.After(20 * time.Millisecond):
    }
    close(release)
    <-emitDone
    <-detachDone
}

func TestSubagentRelayDetachDoesNotCancelAgentAndTerminalPersists(t *testing.T) {
    path := filepath.Join(t.TempDir(), "subagents.v1.json")
    mgr := registry.NewManager(registry.NewManagerOpts{
        RootContext: context.Background(), Path: path, SessionBootID: "boot", MaxConcurrent: 1,
    })
    block := make(chan struct{})
    relay := newSubagentRelay(func(proto.ServerFrame) {})
    sink := registry.EventSink(func(ev registry.Event) {
        relay.Emit(proto.NewSubagentEvent(ev.AgentID, ev.Role, string(ev.Type), string(ev.Status), ev.Text))
    })
    id, err := mgr.Spawn(context.Background(), registry.SpawnRequest{
        AgentType: "subagent", Role: "explore", Prompt: "scan", Emit: sink,
        Runner: registry.RunnerFunc(func(context.Context, string, string) (string, error) {
            <-block
            return "done", nil
        }),
    })
    require.NoError(t, err)

    // 模拟 WS 断开：只 detach relay，不 cancel Manager/agent。
    relay.Detach()
    close(block)
    final, err := mgr.Wait(context.Background(), id, registry.WaitOpts{Timeout: time.Second})
    require.NoError(t, err)
    require.Equal(t, registry.StatusCompleted, final.Status)
    mgr.Close()

    // 新 boot 从磁盘恢复 terminal，证明断线期间 event 丢弃但状态仍持久化。
    reopened := registry.NewManager(registry.NewManagerOpts{
        RootContext: context.Background(), Path: path, SessionBootID: "boot2", MaxConcurrent: 1,
    })
    t.Cleanup(reopened.Close)
    persisted, ok := reopened.Result(id)
    require.True(t, ok)
    require.Equal(t, registry.StatusCompleted, persisted.Status)
}
```

> `TestSubagentRelayDetachWaitsForInflightWrite` 验证 Emit 在 RLock 下写（#16）：Detach 的写锁等 Emit 完成才返回。
> `TestSubagentRelayDetachDoesNotCancelAgentAndTerminalPersists` 验证断线不取消（#18）：Detach 后 agent 跑完，新 boot 从磁盘正确恢复 terminal。

- [ ] **Step 4：GREEN**

Run: `go test -race ./internal/api/http -run 'TestChatWS_ForwardsTypedSubagentEvent|TestSubagentRelayDetach' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/api/http/ws.go internal/api/http/subagent_relay.go internal/api/http/ws_subemit_test.go
git commit -m "feat(http): forward typed subagent events over detachable WS relay"
```

---
## Task 20：SSE 单写者 bounded merge + 请求边界

**Files:**
- Modify: `internal/api/http/chat.go`
- Create: `internal/api/http/sse_subemit_test.go`
- Modify: `internal/api/http/subagent_relay.go`

- [ ] **Step 1：失败测试（真实 HTTP，不造 fake conn/server）**

```go
package http

import (
    "bufio"
    "bytes"
    "context"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"

    "github.com/cloudwego/eino/schema"
    "github.com/stretchr/testify/require"

    "github.com/x6nux/yanshi/internal/agent/orchestrator"
    "github.com/x6nux/yanshi/internal/guard"
    einollm "github.com/x6nux/yanshi/internal/llm/eino"
    "github.com/x6nux/yanshi/internal/proto"
    "github.com/x6nux/yanshi/internal/tools"
)

func TestChatSSE_ForwardsTypedSubagentEventWithSingleWriter(t *testing.T) {
    emitCall := schema.AssistantMessage("", []schema.ToolCall{{
        ID: "emit-1", Type: "function",
        Function: schema.FunctionCall{Name: "emit_subagent_test", Arguments: `{}`},
    }})
    done := schema.AssistantMessage("top done", nil)
    mdl := einollm.NewFakeModelWithMessages([]*schema.Message{emitCall, done}, nil)

    emitTool := newSubagentEmitTestTool(t) // 完整定义见 Task 19，同包复用，不虚构 server。
    o, err := orchestrator.New(orchestrator.Config{
        Model: mdl, Tools: []orchestrator.BaseTool{emitTool},
        Profile: guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"*"}}},
    })
    require.NoError(t, err)

    s := New(Config{Token: "t"})
    s.Chat(o, nil, nil)
    ts := httptest.NewServer(s.Handler())
    t.Cleanup(ts.Close)

    body := bytes.NewBufferString(`{"messages":[{"role":"user","content":"emit"}]}`)
    req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/chat", body)
    require.NoError(t, err)
    req.Header.Set("Authorization", "Bearer t")
    req.Header.Set("Content-Type", "application/json")
    resp, err := http.DefaultClient.Do(req)
    require.NoError(t, err)
    defer resp.Body.Close()
    require.Equal(t, http.StatusOK, resp.StatusCode)

    scanner := bufio.NewScanner(resp.Body)
    var lines []string
    for scanner.Scan() { lines = append(lines, scanner.Text()) }
    require.NoError(t, scanner.Err())
    joined := strings.Join(lines, "\n")
    require.Contains(t, joined, "event: subagent_event")
    require.Contains(t, joined, `"agent_id":"ag-test"`)
}
```

- [ ] **Step 2：确认 RED**

Run: `go test ./internal/api/http -run 'TestChatSSE_ForwardsTypedSubagentEventWithSingleWriter' -count=1`

Expected: FAIL，SSE context没有绑定 `WithSubAgentEmit`。

- [ ] **Step 3：实现 bounded lifecycle relay**

在 `subagent_relay.go` 追加：

```go
type sseLifecycleRelay struct {
    mu       sync.RWMutex
    open     bool
    progress chan proto.ServerFrame
    terminal chan proto.ServerFrame
}

func newSSELifecycleRelay() *sseLifecycleRelay {
    return &sseLifecycleRelay{
        open: true,
        progress: make(chan proto.ServerFrame, 64),
        terminal: make(chan proto.ServerFrame, 8),
    }
}

func (r *sseLifecycleRelay) Emit(f proto.ServerFrame) {
    r.mu.RLock()
    open := r.open
    r.mu.RUnlock()
    if !open { return }

    terminal := f.Event == "completed" || f.Event == "failed" ||
        f.Event == "cancelled" || f.Event == "persistence_failed"
    if terminal {
        // terminal/persistence failure 不和 progress 共用 buffer。
        select { case r.terminal <- f: default: }
        return
    }
    // progress 满时明确 drop newest；持久化结果仍可查询。
    select { case r.progress <- f: default: }
}

func (r *sseLifecycleRelay) Close() {
    r.mu.Lock()
    r.open = false
    r.mu.Unlock()
}
```

- [ ] **Step 4：改造 SSE attempt 为单 writer merge**

把 `chat.go` 中直接 classifier→`writeSSEFrame` 的块替换为：

```go
relay := newSSELifecycleRelay()
defer relay.Close()

mainFrames := make(chan proto.ServerFrame)
classDone := make(chan struct{})
tc := tools.WithErrCounter(r.Context())
tc = tools.WithSubAgentEmit(tc, relay.Emit)
iter := o.EventsWithHistoryOpts(tc, runMsgs, opts)

go func() {
    defer close(classDone)
    orchestrator.ClassifyEventsWithUsage(iter, &usage, func(f proto.ServerFrame) {
        select {
        case mainFrames <- f:
        case <-r.Context().Done():
        }
    })
}()

for {
    select {
    case f := <-mainFrames:
        if f.Type == "error" { hadError = true }
        if f.Type == "agent_chunk" { assistantText += f.Text }
        writeSSEFrame(w, fl, f) // 唯一写 ResponseWriter 的 goroutine
    case f := <-relay.terminal:
        writeSSEFrame(w, fl, f)
    case f := <-relay.progress:
        writeSSEFrame(w, fl, f)
    case <-classDone:
        // 只接收请求边界内已排队的 lifecycle event。
        drainLifecycleFrames(w, fl, relay)
        relay.Close()
        goto attemptDone
    case <-r.Context().Done():
        relay.Close()
        goto attemptDone
    }
}

attemptDone:
```

`drainLifecycleFrames` 完整实现：

```go
func drainLifecycleFrames(w http.ResponseWriter, fl http.Flusher, relay *sseLifecycleRelay) {
    for {
        select {
        case f := <-relay.terminal:
            writeSSEFrame(w, fl, f)
        case f := <-relay.progress:
            writeSSEFrame(w, fl, f)
        default:
            return
        }
    }
}
```

> 已 spawn agent在 request结束后继续运行；relay关闭后 callback立即返回，不持有 `ResponseWriter`，后续结果只进 Manager persistence。不得从 classifier goroutine或 lifecycle callback直接写 `w`。

- [ ] **Step 5：GREEN + race**

Run: `go test -race ./internal/api/http -run 'TestChatSSE_ForwardsTypedSubagentEventWithSingleWriter' -count=1`

Expected: PASS，且 race detector无并发写 `ResponseWriter` 报告。

- [ ] **Step 6：提交**

```bash
git add internal/api/http/chat.go internal/api/http/subagent_relay.go internal/api/http/sse_subemit_test.go
git commit -m "feat(http): merge bounded subagent events into SSE single-writer loop"
```

---

## Task 21：Bootstrap 唯一组合根 — Manager、12 工具、model registry、Shutdown

**Files:**
- Modify: `internal/bootstrap/bootstrap.go`
- Modify: `internal/bootstrap/bootstrap_test.go`

- [ ] **Step 1：失败测试（完整 config fixture）**

```go
package bootstrap

import (
    "context"
    "fmt"
    "os"
    "path/filepath"
    "testing"
    "time"

    "github.com/stretchr/testify/require"
)

func TestBuildWiresSubagentManagerAndTwelveAgentTools(t *testing.T) {
    dir := t.TempDir()
    cfgPath := filepath.Join(dir, "config.yaml")
    dbPath := filepath.Join(dir, "yanshi.db")
    statePath := filepath.Join(dir, "subagents.v1.json")
    yaml := fmt.Sprintf(`
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: %q
token: test-token
llm:
  providers: []
subagents:
  limit: 3
  persistence_path: %q
profiles:
  orchestrator:
    tools:
      allow: ["*"]
    net:
      allow: true
`, dbPath, statePath)
    require.NoError(t, os.WriteFile(cfgPath, []byte(yaml), 0o600))

    app, err := Build(Options{ConfigPath: cfgPath, FakeModel: true})
    require.NoError(t, err)
    t.Cleanup(func() {
        ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
        defer cancel()
        require.NoError(t, app.Shutdown(ctx))
    })

    require.NotNil(t, app.SubagentManager)
    require.NotNil(t, app.AgentTools)
    require.Len(t, app.AgentTools.Tools(), 12) // 4 legacy + 8 lifecycle
    require.Equal(t, 3, app.SubagentManager.List(false).Limit)
}
```

- [ ] **Step 2：确认 RED**

Run: `go test ./internal/bootstrap -run 'TestBuildWiresSubagentManagerAndTwelveAgentTools' -count=1`

Expected: FAIL，`App.SubagentManager`/`App.AgentTools`/`Tools()` 尚未定义。

- [ ] **Step 3：实现 bootstrap 接线**

在 `bootstrap.go`：

```go
import agentregistry "github.com/x6nux/yanshi/internal/agent/registry"

type App struct {
    // 现有字段保持。
    SubagentManager *agentregistry.Manager
    AgentTools      *tools.AgentTools
}
```

在 model 建完、tools 前创建 Manager：

```go
bootID := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
subagentManager := agentregistry.NewManager(agentregistry.NewManagerOpts{
    RootContext:      context.Background(),
    Path:            cfg.Subagents.PersistencePath,
    SessionBootID:   bootID,
    MaxConcurrent:   cfg.Subagents.Limit,
})
```

构建 12 工具并全部注册：

```go
agentTools := tools.NewAgentTools(chatModel)
for _, tool := range agentTools.Tools() {
    allTools = append(allTools, tool)
}
```

`AgentTools.Tools()` 完整实现：

```go
func (t *AgentTools) Tools() []*GuardedTool {
    return []*GuardedTool{
        t.StartAgent, t.StartWorkflow, t.Analysis, t.Summarize,
        t.AgentSpawn, t.AgentWait, t.AgentResult, t.AgentSendInput,
        t.AgentResume, t.AgentAssign, t.AgentCancel, t.AgentList,
    }
}
```

构造 orchestrator：

```go
availableModels := make(map[string]bool, len(providerModels))
for name := range providerModels { availableModels[name] = true }

orchConfig := orchestrator.Config{
    // 现有字段保持。
    SubagentManager: subagentManager,
    AvailableModels: availableModels,
}
```

返回 App：

```go
return &App{
    // 现有字段保持。
    SubagentManager: subagentManager,
    AgentTools: agentTools,
}, nil
```

`Shutdown` 顺序（server → manager → store）：

```go
func (a *App) Shutdown(ctx context.Context) error {
    if a.cancel != nil { a.cancel() }
    err := a.Server.Shutdown(ctx)
    if a.SubagentManager != nil {
        a.SubagentManager.Close()
    }
    if cerr := a.Store.Close(); err == nil { err = cerr }
    return err
}
```

> 所有 `Build` 早退路径在 Manager 已创建后都要 `subagentManager.Close()` 再 `st.Close()`，避免 goroutine/文件泄漏。

- [ ] **Step 4：GREEN**

Run: `go test ./internal/bootstrap -run 'TestBuildWiresSubagentManagerAndTwelveAgentTools' -count=1`

Expected: PASS。

- [ ] **Step 5：提交**

```bash
git add internal/bootstrap/bootstrap.go internal/bootstrap/bootstrap_test.go internal/tools/agent.go
git commit -m "feat(bootstrap): wire process-wide subagent manager and lifecycle tools"
```

---

## Task 22：跨入口 acceptance、race、API 自检

**Files:**
- Create: `internal/tools/subagent_acceptance_test.go`
- Modify: `internal/api/http/ws_subemit_test.go`
- Modify: `internal/api/http/sse_subemit_test.go`
- Modify: `internal/bootstrap/bootstrap_test.go`

- [ ] **Step 1：写 deterministic acceptance tests**

```go
package tools

import (
    "context"
    "path/filepath"
    "sync/atomic"
    "testing"
    "time"

    "github.com/stretchr/testify/require"

    "github.com/x6nux/yanshi/internal/agent/registry"
)

type channelGatedRunner struct {
    started chan string
    release chan struct{}
    done    chan struct{}
    calls   atomic.Int32
}

func newChannelGatedRunner() *channelGatedRunner {
    return &channelGatedRunner{
        started: make(chan string, 20), release: make(chan struct{}), done: make(chan struct{}, 20),
    }
}

func (r *channelGatedRunner) Run(ctx context.Context, agentID, assignment string) (string, error) {
    defer func() { r.done <- struct{}{} }() // cancel test绝不能 hang
    r.calls.Add(1)
    r.started <- assignment
    select {
    case <-ctx.Done():
        return "", ctx.Err()
    case <-r.release:
        return "SUMMARY:\nCHANGES:\nEVIDENCE:\nfile.go:1\nRISKS:\nBLOCKERS:", nil
    }
}

func waitTerminal(t *testing.T, mgr *registry.Manager, id string, timeout time.Duration) registry.Record {
    t.Helper()
    rec, err := mgr.Wait(context.Background(), id, registry.WaitOpts{Timeout: timeout})
    require.NoError(t, err)
    return rec
}

func TestAcceptance_WorkflowUsesSharedLimitAndList(t *testing.T) {
    mgr := registry.NewManager(registry.NewManagerOpts{
        RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
        SessionBootID: "boot", MaxConcurrent: 2,
    })
    t.Cleanup(mgr.Close)

    gate := newChannelGatedRunner()
    first, err := mgr.Spawn(context.Background(), registry.SpawnRequest{Role: "explore", Prompt: "A", Runner: gate})
    require.NoError(t, err)
    second, err := mgr.Spawn(context.Background(), registry.SpawnRequest{Role: "explore", Prompt: "B", Runner: gate})
    require.NoError(t, err)
    <-gate.started
    <-gate.started

    _, err = mgr.Spawn(context.Background(), registry.SpawnRequest{Role: "explore", Prompt: "C", Runner: gate})
    var capErr *registry.SpawnErrCap
    require.ErrorAs(t, err, &capErr)
    require.Equal(t, 2, capErr.Cap)
    list := mgr.List(false)
    require.Equal(t, 2, list.Running)
    require.Len(t, list.Agents, 2)

    close(gate.release)
    rec1 := waitTerminal(t, mgr, first, time.Second)
    require.Equal(t, registry.StatusCompleted, rec1.Status)
    rec2 := waitTerminal(t, mgr, second, time.Second)
    require.Equal(t, registry.StatusCompleted, rec2.Status)
}

func TestAcceptance_ParentCancelCascadesToChild(t *testing.T) {
    mgr := registry.NewManager(registry.NewManagerOpts{
        RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
        SessionBootID: "boot", MaxConcurrent: 4,
    })
    t.Cleanup(mgr.Close)

    gate := newChannelGatedRunner()
    parent, err := mgr.Spawn(context.Background(), registry.SpawnRequest{Role: "general", Prompt: "parent", Runner: gate})
    require.NoError(t, err)
    <-gate.started
    child, err := mgr.Spawn(context.Background(), registry.SpawnRequest{ParentID: parent, Role: "explore", Prompt: "child", Runner: gate})
    require.NoError(t, err)
    <-gate.started

    require.NoError(t, mgr.Cancel(parent))
    <-gate.done
    <-gate.done
    recP := waitTerminal(t, mgr, parent, time.Second)
    require.Equal(t, registry.StatusCancelled, recP.Status)
    recC := waitTerminal(t, mgr, child, time.Second)
    require.Equal(t, registry.StatusCancelled, recC.Status)
}

func TestAcceptance_DepthAndUsageQueryable(t *testing.T) {
    mgr := registry.NewManager(registry.NewManagerOpts{
        RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
        SessionBootID: "boot", MaxConcurrent: 4,
    })
    t.Cleanup(mgr.Close)

    gate := newChannelGatedRunner()
    parent, _ := mgr.Spawn(context.Background(), registry.SpawnRequest{Role: "general", Prompt: "parent", Runner: gate})
    <-gate.started
    child, _ := mgr.Spawn(context.Background(), registry.SpawnRequest{ParentID: parent, Role: "explore", Prompt: "child", Runner: gate})
    <-gate.started
    recParent, _ := mgr.Result(parent)
    recChild, _ := mgr.Result(child)
    require.Equal(t, 0, recParent.Depth)
    require.Equal(t, 1, recChild.Depth)

    require.NoError(t, mgr.AddUsage(child, registry.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13, ModelCalls: 1}))
    snap, ok := mgr.Result(child)
    require.True(t, ok)
    require.Equal(t, int64(13), snap.Usage.TotalTokens)

    close(gate.release)
}
```

- [ ] **Step 2：补充四个行为 acceptance**

使用同一个 `channelGatedRunner` 继续增加：

```go
package tools

import (
    "context"
    "os"
    "path/filepath"
    "time"

    "github.com/stretchr/testify/require"

    "github.com/x6nux/yanshi/internal/agent/registry"
)

func TestAcceptance_CustomRoleSurvivesRestartAndResume(t *testing.T) {
    path := filepath.Join(t.TempDir(), "s.json")
    raw := `{"schema_version":1,"session_boot_id":"boot1","agents":[{"id":"ag-custom","session_boot_id":"boot1","role":"custom","status":"running","prompt":"audit auth","allowed_tools":["fs_read"],"instruction":"stay read only","model_override":"gpt-4o-mini","reasoning_effort":"low","custom":{"name":"audit","prompt_prefix":"audit it","allowed_tools":["fs_read"],"read_only_shell":true},"started_at":"2026-01-01T00:00:00Z"}]}`
    require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))

    mgr := registry.NewManager(registry.NewManagerOpts{
        RootContext: context.Background(), Path: path, SessionBootID: "boot2", MaxConcurrent: 2,
    })
    t.Cleanup(mgr.Close)

    gate := newChannelGatedRunner()
    id, err := mgr.Resume(context.Background(), "ag-custom", registry.ResumeRequest{
        Runner: registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
            <-gate.release
            return "ok", nil
        }),
    })
    require.NoError(t, err)
    require.Equal(t, "ag-custom", id)

    rec, _ := mgr.Result(id)
    require.Equal(t, registry.StatusRunning, rec.Status)
    require.Equal(t, []string{"fs_read"}, rec.AllowedTools)
    require.Equal(t, "stay read only", rec.Instruction)
    require.Equal(t, "gpt-4o-mini", rec.ModelOverride)
    require.Equal(t, "low", rec.ReasoningEffort)
    require.NotNil(t, rec.Custom)
    require.Equal(t, "audit", rec.Custom.Name)

    close(gate.release)
    final := waitTerminal(t, mgr, id, time.Second)
    require.Equal(t, registry.StatusCompleted, final.Status)
}

func TestAcceptance_ReadOnlyRoleCannotEscalateOnResume(t *testing.T) {
    mgr := registry.NewManager(registry.NewManagerOpts{
        RootContext: context.Background(), Path: filepath.Join(t.TempDir(), "s.json"),
        SessionBootID: "boot", MaxConcurrent: 2,
    })
    t.Cleanup(mgr.Close)

    gate := newChannelGatedRunner()
    id, err := mgr.Spawn(context.Background(), registry.SpawnRequest{
        Role: "explore", Prompt: "scan", Runner: gate,
    })
    require.NoError(t, err)
    <-gate.started
    close(gate.release)
    _ = waitTerminal(t, mgr, id, time.Second)

    resumeGate := newChannelGatedRunner()
    _, err = mgr.Resume(context.Background(), id, registry.ResumeRequest{
        Runner: registry.RunnerFunc(func(ctx context.Context, _, _ string) (string, error) {
            return "explore", nil
        }),
    })
    require.NoError(t, err)
    <-resumeGate.started
    close(resumeGate.release)
    _ = waitTerminal(t, mgr, id, time.Second)
}

func TestAcceptance_FlatWorkflowAndAnalysisAppearInManager(t *testing.T) {
    ctx := context.Background()
    mgr := registry.NewManager(registry.NewManagerOpts{
        RootContext: ctx, Path: filepath.Join(t.TempDir(), "s.json"),
        SessionBootID: "boot", MaxConcurrent: 4,
    })
    t.Cleanup(mgr.Close)

    gateA := newChannelGatedRunner()
    gateB := newChannelGatedRunner()
    idA, err := mgr.Spawn(ctx, registry.SpawnRequest{Role: "explore", Prompt: "step A", Runner: gateA})
    require.NoError(t, err)
    idB, err := mgr.Spawn(ctx, registry.SpawnRequest{Role: "explore", Prompt: "step B", Runner: gateB})
    require.NoError(t, err)
    <-gateA.started
    <-gateB.started

    list := mgr.List(false)
    require.Equal(t, 2, list.Running)
    ids := map[string]bool{idA: true, idB: true}
    for _, a := range list.Agents {
        require.True(t, ids[a.ID])
    }

    close(gateA.release)
    close(gateB.release)
    _ = waitTerminal(t, mgr, idA, time.Second)
    _ = waitTerminal(t, mgr, idB, time.Second)
}

func TestAcceptance_AgentStartReturnsEvidenceWorkingSetHint(t *testing.T) {
    ctx := context.Background()
    mgr := registry.NewManager(registry.NewManagerOpts{
        RootContext: ctx, Path: filepath.Join(t.TempDir(), "s.json"),
        SessionBootID: "boot", MaxConcurrent: 4,
    })
    t.Cleanup(mgr.Close)

    res, err := ManagedSubAgentRun(
        WithManager(ctx, mgr),
        ManagedSubAgentSpec{
            Role: "explore", Prompt: "find evidence",
            Runner: registry.RunnerFunc(func(_ context.Context, _, _ string) (string, error) {
                return "SUMMARY:\nscan done\nCHANGES:\nnone\nEVIDENCE:\nfile.go:10\nRISKS:\nnone\nBLOCKERS:\nnone", nil
            }),
        },
    )
    require.NoError(t, err)
    require.Contains(t, res.Text, "EVIDENCE")
    hint := ParentWorkingSetHint(res.Text)
    require.Contains(t, hint, "[parent working-set hint")
    require.Equal(t, registry.StatusCompleted, res.Terminal)
}
```

每个 test 以 channel handshake 驱动，不允许 `time.Sleep(30*time.Millisecond)` 等 timing assertion。

- [ ] **Step 3：分包验证**

Run: `go test -race ./internal/agent/registry ./internal/tools ./internal/agent/orchestrator ./internal/proto ./internal/api/http ./internal/config ./internal/bootstrap -count=1`

Expected: PASS，无 race、无 goroutine hang。

- [ ] **Step 4：全量验证**

Run: `go test ./... -count=1`

Expected: PASS；带 `e2e_real` build tag 的测试不在本命令范围。锁定 provider不可用导致的已知 `t.Skip` 可接受，但不能有 FAIL。

Run: `go vet ./...`

Expected: exit 0。

- [ ] **Step 5：文件行数守卫**

Run: `go run ./cmd/codelines ./internal/tools ./internal/agent/registry ./internal/api/http`

Expected: 每个 `.go` 文件纯代码行 ≤1000；如 CLI 语法不同，按其 `-h` 调整命令，不改测试目标。

- [ ] **Step 6：API/语义矩阵人工自检（必须逐项勾选）**

```text
[ ] 没有 LoadString、expandTilde、contextForTest、ws.writeJSON、quickTestConfig、newFakeConn、newTestServer
[ ] Build 仍是 Build(Options)，WS 写法仍是 conn.write(frame)
[ ] NewManager 返回 *Manager 无 error；NewManagerOpts 用 MaxConcurrent 而非 Limit
[ ] Result(agentID) 是 (Record, bool)，Wait(ctx, agentID, WaitOpts) 是 (Record, error)，Close() 无返回值
[ ] SpawnErrCap 用 errors.As 检测 capped 信号（非 ErrLimit）；StatusCancelled 双 L 拼写
[ ] 所有 Runner.Run(ctx, agentID, assignment string) (string, error) 以 agentID/assignment 代 Turn/TurnResult
[ ] Spawn/Resume/Assign/AddUsage/finish 均遵循 persistMu -> mu 锁序
[ ] Spawn Running 在返回 ID/启动 goroutine前落盘，失败回滚且 runner未启动
[ ] Resume 保存并复用 AllowedTools/Instruction/custom/model/reasoning/depth
[ ] flat workflow、DAG workflow、analysis、agent_start、agent_spawn 共用一个 Manager/cap/list
[ ] unknown JSON字段可容忍，missing boot ID归档，磁盘 Running恢复成 Interrupted
[ ] explore/review/verifier 是 read-only shell；plan 仅写计划产物；interactive审批不能放宽
[ ] invalid model/reasoning 在 Spawn 前失败且不创建 record；Resume 重新校验
[ ] List(includeArchived bool) ListResult 含 Running/MaxConcurrent；Result 暴露 Depth/Usage
[ ] terminal写盘失败事件顺序：persistence_failed -> terminal
[ ] agent_start结果保留五段并暴露 EVIDENCE working-set hint
[ ] WS断开/SSE请求结束后 agent继续运行；relay detach且不持有 conn/writer
[ ] SSE只有 handler goroutine写 ResponseWriter；lifecycle channel有界且drop policy明确
[ ] 全部并发测试用 channel handshake；channelGatedRunner 总会 defer done
```

- [ ] **Step 7：提交**

```bash
git add internal/tools/subagent_acceptance_test.go internal/api/http/ws_subemit_test.go internal/api/http/sse_subemit_test.go internal/bootstrap/bootstrap_test.go
git commit -m "test(subagents): cover lifecycle roles persistence transports and shared cap"
```

---

## 实施完成定义

只有以下条件全部满足才能宣告 Batch B1 完成：

1. `agent_spawn` 在 Running 已持久化后立即返回 ID；`agent_wait/result/send_input/resume/assign/cancel/list` 可用。
2. 默认 cap 10、硬上限 20，仅计 Running，所有入口共享同一个 Manager。
3. 七角色 prompt/tool posture 与 reference 对齐，且 role restriction 永远只收紧 parent guard。
4. `~/.yanshi/subagents.v1.json` 使用 schema v1，current session list默认过滤，`include_archived=true` 可见历史。
5. Resume 不丢工具、instruction、custom、model/reasoning；override Spawn/Resume 均 fail-closed。
6. parent-child cancellation、depth、usage、five-section/EVIDENCE working set全部有 deterministic test。
7. `subagent_event` 同步覆盖 WS/SSE；断线不取消后台 agent；SSE 单写者、bounded event merge无 race。
8. 4 legacy + 8 lifecycle 共 12 个 agent tools 在 bootstrap 注册，`App.Shutdown` 关闭 Manager。
9. 目标分包测试、全量测试、vet、纯代码行守卫全部通过。

## 保留待决策（不阻塞 B1）

- **跨进程主动接管：** v1 仅把旧 Running 标记 Interrupted；不尝试把 goroutine/stream跨进程恢复。后续如要自动 resume，需另立版本化协议。
- **事件可靠队列：** v1 的 transport progress event是 best-effort；terminal状态以 persistence为准。需要重放/ack时应设计 journal，而不是扩大内存 channel。
- **跨实例全局 cap：** v1 cap为单进程 Manager级；多个独立 yanshi backend不会共享 semaphore。若需要机器级 cap，应引入 SQLite/文件锁协调。
- **reasoning provider 映射：** v1只验证通用 `off|low|medium|high` 并传给已有 `TurnOpts.ThinkingEffort`；provider特定翻译由各 model adapter负责。

## 计划自审结论

- 已按真实 `Build(Options)`、`conn.write`、`GuardedTool`、`SubAgentRunner`、`providerModels map[string]model.BaseChatModel`、`Result(Record,bool)` 约束重构任务顺序。
- 已把原 review 的结构性缺口放入独立 gate：Running persistence/rollback（Tasks 3–4）、Resume约束（Task 8）、workflow/analysis统一 Manager（Task 15）、unknown-field兼容（Task 3）、override fail-closed（Tasks 10/14/16）、SSE单写者（Task 20）、Depth/Usage（Tasks 4/9/18/22）、角色矩阵（Tasks 11/13）、EVIDENCE hint（Task 15）、terminal event顺序（Task 6）、bootstrap fixture/API（Task 21）、WS disconnect策略（Task 19）。
- 本计划没有运行任何 `go test`/`go build`/`go vet`；文中的命令是实施阶段步骤和 Expected，不是已发生的结果。
- 本计划共 **22 Tasks**；实施者必须按 Task 22 的 fabricated-API/type-signature/mutation/transport/role/determinism matrix 再审后才能执行最终 commit。


<!-- CONTINUE -->
