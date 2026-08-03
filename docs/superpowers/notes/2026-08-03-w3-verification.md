# W3 并发与事务 — 核验报告

> 2026-08-03 实测。含两处**已实证复现**的 bug 与一处**已实证识别**的死锁风险。

## 1. `detachRuntime`：生产、测试、git 历史里都没有 Go 实现

```
$ grep -rn "detachRuntime" . --include="*.go" | grep -v '/reference/'
（零命中）

$ git log --oneline -S'detachRuntime' --all
85c152e / da0b60a / fd0210d   ← 三条全是 docs 提交
```

唯一的「实现」在 `docs/superpowers/plans/2026-07-21-b1-subastro.md:413`（B1 计划里的设计代码，调用点 :402）——**从未落地**。

**要新写。** 签名照 B1 计划：`func (m *Manager) detachRuntime(agentID string) *runtimeAgent`，在 `finishTerminal` 末尾以 `if rt := m.detachRuntime(agentID); rt != nil && rt.cancel != nil { rt.cancel() }` 调用。

⚠️ B1 计划里锁名是 `m.mu`，当前实现叫 **`m.mtx`**（`manager.go:37`），照抄不编译。

## 2. `finishTerminal`：`manager.go:722-755`

doc 在 `:720-721`，函数体 `:722` 起，`m.persistMu.Unlock()` 在 `:754`，右花括号 `:755`。

做的：`persistMu.Lock` → `mtx.Lock` → 写终态字段 → `m.records[agentID] = cloneRecord(rec)`（`:735`）→ `snapshotLocked()`（`:736`）→ `sinkLocked()`（`:737`）→ `mtx.Unlock` → `writeAtomic` → emit → `persistMu.Unlock`。

**不做**：任何 `delete(m.runtime, ...)`，任何 `rt.cancel()`。

全文件 `delete(m.runtime, ...)` 只有 3 处，全在错误回滚路径：`:202`、`:210`（Spawn 回滚）、`:486`（`restoreRecord`）。正常路径 `runAgentLoop`（`:626-682`）→ `finishTerminal`（`:681`）确实从不删。

## 3. `runningLocked()`：`manager.go:571-589`，确为 max()

```go
func (m *Manager) runningLocked() int {
    n := 0
    for _, rt := range m.runtime { if rt != nil { n++ } }
    count := 0
    for _, rec := range m.records { if rec.Status == StatusRunning { count++ } }
    if n > count { return n }      // :585-587
    return count
}
```

调用点：`Spawn` 的 cap 闸门 `:154`，`Resume` 的 cap 闸门 `:417`。

💡 **一条已预授权的路径**：`:569-570` 的注释原文写着「If RAC1 reports a measurable race here, resolution: use the runtime map as the sole authoritative count (the records count is only advisory)」——修复时把 records 交叉核对拿掉，**不需要新论证**。

## 4. 槽位泄漏：已实测复现

探针 `zz_leak_probe_test.go`（已删）：MaxConcurrent=2 → 连续 Spawn 两个瞬时完成的 agent 并 `Wait` 到终态 → `List(false)` → 第三次 Spawn。**修复前**：

```
List(): Running=0 Limit=2 agents=2
SLOT LEAK: third Spawn rejected with cap=2 while List reports Running=0
--- FAIL: TestProbeSlotLeak (0.04s)
```

与审计描述逐字吻合。同步用 `m.Wait(ctx, id, WaitOpts{Timeout: 2s})` 这个可观测信号，**不用 `time.Sleep`**。

### 4b. 同一泄漏引出审计没记的第二缺陷：进程内终态 agent 无法 Resume

```
RESUME BROKEN: subagent ag-... runtime already active
--- FAIL: TestProbeResumeAfterTerminal (0.03s)
```

原因：`Resume` 在 `manager.go:412` 检查 `if _, exists := m.runtime[agentID]; exists` 就报错。runtime 永不删除 ⇒ **任何在本进程内跑到终态的 agent 都永远不能 Resume**。

既有 `manager_resume_test.go` 之所以绿，是因为它 Resume 的是从磁盘 load 进来、本进程从未有过 runtime 条目的记录。**这直接打在 M04 验收标准的「resume 跨重启可尝试」上**，审计只记了「取消不泄漏」那一条。

### 4c. 修复会打红两条既有测试（它们把 bug 写成了预期行为）

1. **`manager_coverage_test.go:1195-1236 TestResumeRejectsRuntimeStillActive`** —— 注释原文自陈「Agent is now terminal (Completed) but still in runtime map」，即**测试正在断言这个 bug**。修复后 `require.Error` 拿到 nil。
   → 处置：该分支修复后仍可达（`finishTerminal` 写完终态 record 到 `detachRuntime` 之间有窗口），但不能靠自然路径触发。改写成白盒：先断言终态后 runtime 条目已消失，再手工 `m.runtime[id] = m.newRuntimeAgent(...)` 重造窗口。**已实测该改写 `go test -race` PASS。**

2. **`internal/agent/batch/runner_test.go:253-279 TestRunnerSpawnCapExhausted`** —— MaxConcurrent=1 + 2 行，原本靠「第一个 agent 跑完不还槽」让第二行耗尽重试。修复后两行都成功。
   → 处置：先用阻塞 runner 直接 `mgr.Spawn` 占住唯一槽位（`parked`/`release` channel 同步，不用 sleep），再跑两行 → 两行都 cap 耗尽。**已实测 `go test -race` PASS。**

## 5. legacy 入口确实绕过 Manager cap

`runSubAgent` 在 `internal/tools/agent.go:381`。先看 `SubAgentRunnerFromContext`，没有则退回裸 `chatModel.Generate`——**全程不碰 `registry.Manager`**。

**生产调用点 5 处，覆盖 4 个工具面**（spec 说「legacy 三入口」不准确）：
- `agent.go:336` — `agent_start`
- `agent_analysis.go:60` — `analysis`
- `agent_workflow.go:264` — `workflow_start`（plan 阶段）
- `agent_workflow.go:372` — `workflow_start`（并行任务体）
- `agent_dag.go:202` — DAG 步骤

`NumCPU` 信号量两处：`agent_dag.go:112`、`agent_workflow.go:323`——都是**每次调用各自一份的局部信号量**，与进程级 cap 无关。

`ManagedSubAgentRun`（`subagent.go:274`）与 `spawnWithRetry`（`subagent.go:315`）**零生产调用点**。唯一走 Manager 的是 `agent_spawn`（`agent_lifecycle.go:66`）。

ctx 侧接线是齐的：`orchestrator.go:342 tools.WithManager`、`:356 tools.WithManagedRunnerFactory`（都在 `bindManagedRunner`，`:337`）。即 `runSubAgent` 在生产 ctx 里**拿得到** Manager 和 factory，只是没用。

### 5b. ⚠️ 接进去有一个必须先解的死锁（审计/spec 都没提）

原型化「`runSubAgent` 有 Manager+factory 时改走 `ManagedSubAgentRun`」后发现：`ManagedSubAgentRun` 是**阻塞**的（`mgr.Wait`），而 `spawnWithRetry` 无限重试直到有槽。于是**父 agent 持槽等子 agent**。默认 cap=10 时：10 个 depth-1 的 agent 各自再调 `agent_start` → 10 个槽全被阻塞的父占住，孙 agent 永远排不进来 → **活锁**。depth-0 不会（编排器 turn 本身不占槽）。

**可行解**：给 `runtimeAgent` 加 `parked bool`，`ManagedSubAgentRun` 在 `mgr.Wait` 前后 `mgr.SetParked(registry.CurrentAgentID(ctx), true/false)`，`runningLocked()` 只数未 parked 的条目。这同时把 §3 那条注释预授权的「runtime map 作为唯一权威计数」落实了，max() 隐患一并消失。**代价**：`List().Running`（按 record 数）可能短暂大于 cap，需写进文档。

原型已做到能编译，**但未跑完整回归**——只报「设计可行 + 死锁已实证识别」，不报「已验证」。

## 6. `work/store.go` 写方法：**11 个裸写点 / 10 个导出方法**（审计的「11 个写方法」口径不准）

`wt()` 定义在 `store.go:49`。全仓引用：定义 1 处 + `manager_extra_test.go` 的 4 处（2 注释 2 测试调用）。**生产代码零调用**。包头注释 `:2-3` 声称「All write paths route through the injected WriteTxer」**是假陈述**。

裸 `s.db.Exec / BeginTx / ExecContext` 行号 → 方法：

| 行 | 方法 |
|---|---|
| **131** | `migrate`（未导出，**审计漏了**） |
| 138 | `Create` |
| 312 | `Transition` |
| 348 | `AppendTimeline` |
| 367 | `AttachBrokerTask` |
| 385 | `SetChecklist` |
| 408 | `AddChecklistItem` |
| 434 | `PatchChecklistItem` |
| 448 | `PatchChecklistItem`（同方法第二处） |
| 455 | `RecordGate` |
| 477 | `PutArtifact` |
| 533 | `DeleteArtifactsBefore` |

审计给的 11 行逐条命中，但 434/448 是同一方法，且漏记 `migrate`。准确说法：**12 个写点 / 11 个方法**（含 migrate）。

### 6b. 全量接线已实测可行

把全部 12 个写点改成 `s.wt().WriteTx(ctx, func(tx *sql.Tx) error { ... })`（含 `migrate` 的多语句 DDL，modernc 在事务内正常执行），`go build ./...` OK，`go test ./internal/task/work ./internal/tools ./internal/task/...` 全绿。

**无重入风险**：`work.Manager` 每个方法只顺序调用一个 Store 写方法（`manager.go:109,115,127,135,155,164,180,188,196,205,215,280`），Store 写方法之间不互调。`store.WriteTx`（`internal/store/store.go:177`）的 doc 明写「NOT reentrant」——**需在计划里作为不变量写死**。

### 6c. 已为 WAL1 写出真正能证伪的测试，双向实测

`package work_test`（外部测试包，不触 GOV1）。用 `store.OpenWith(..., OpenOptions{MaxOpenConns:4, BusyTimeoutMs:1})` 开真实文件库并注入 `st` 作 WriteTxer；一个 goroutine 在 `st.WriteTx` 里做完 INSERT 后 park 住不 commit；另一个调 `ws.AppendTimeline`。

- **接线前**：`work write completed while the process-wide write lock was held (err=database is locked (5) (SQLITE_BUSY))` → FAIL（0.02s）
- **接线后**：`go test -race` PASS（0.57s）

⚠️ **一个坑必须写进计划**：`t.Fatal` 会跳过 `close(release)`，导致 `t.Cleanup(st.Close)` 卡死在 writeMu（实测撞了 2 分钟超时）。必须用 `sync.Once` 包住 release 并 `t.Cleanup` 注册，且**注册顺序在 `st.Close` 的 cleanup 之后**（LIFO 保证它先跑）。

WAL 的 `:memory:` 豁免不受影响：`buildDSN`（`store.go:150-152`）与 `applyConnectionPragmas`（`store.go:163-166`）两处短路都在 `store` 包内。

## 7. `A2/DT2` divergent 的处置

审计记的三条偏离，逐条核实：

1. **`gate.go:88-92` 的 `guard.Action` 没填 `Workdir`**（真）。对照 `tools/shell.go:128` 明确写了 `Workdir: s.root`，注释原文「Workdir = s.root feeds the destructive-deletion dimension」。后果实测于 `internal/guard/destructive.go`：`isCatastrophicTarget`（`:274-295`）在 `workdir==""` 时**跳过**「删 workdir 自身 / 祖先」两条判定（`:284`）；`resolvesOutsideWorkdir`（`:301`）对相对路径 `resolveTarget` 返回 ok=false → 判为域内。即 `task_gate_run` 里的 `rm -rf ../sibling`、`rm -rf <项目根绝对路径>` 相比 `shell_run` **降级**。字面 root（`/`、`~`、`*`、`..`、`/etc`…）仍被 `catastrophicRoots` 拦住。
2. **gate 用 `exec.CommandContext` 直跑**（`gate.go:105 shellCommand`），未经 shell session runtime（真）。
3. **`EmitWorkEvent`（`gate.go:146`）受 G05 的 `EmitWorkFrame` 断链影响**（属 W1，不在 W3）。

**处置：偏离 1 收敛到设计，偏离 2 接受偏离并改验收标准。**

理由：偏离 1 是纯安全降级，修法是 `guard.Action` 加 `Workdir: root`、零行为面扩张、可用 tools 层真实 `runGate` 端到端断言，没有理由不改。偏离 2 相反——shell v2 runtime 至今全仓零注册（那正是 W1 的 9 条豁免），把一次性 argv 命令挂到会话生命周期上是为满足一句路线图措辞而引入耦合，且 gate 已经过同一个 `guard.Authorize`，安全面不因绕开 session 而变；DT2 的四条正式验收标准（证据结构 / 大输出成 artifact / 挂对 task / 退出码与 duration）与执行载体无关，已全部满足。**应落一条新 ADR** 把「gate 不走 shell session」写进 Consequences，否则下一轮审计会再判一次 divergent。

## 8. 五项 delta

- **`F2/LEAK2`** — cap 闸门本身真实（`manager.go:154`、clamp `:66-73`）；缺口 = `finishTerminal` 不释放槽（已复现）+ cap 不覆盖 legacy 入口 + `config.example.yaml` **完全没有 `subagents:` 段**（`SubagentsConfig` 在 `config.go:274-278`，校验 `:621-622`，只有 `docs/user-guide/configuration.md:51,205-210` 提到）
- **`F1/WAL1`** — store 侧 WAL/池/busy_timeout 全真；缺口 = work 包 12 个写点全绕过 `wt()`，`MaxOpenConns=4` 下与 `writeMu` 脱钩（已实测 SQLITE_BUSY）
- **`A2/DT1`** — create/list/read/cancel、状态机、持久恢复全真；缺口 = `TurnOpts.ThreadID/TurnID` 在**三个**生产入口（`ws.go:644`、`chat.go:132`、`v1/service.go:313`）全不填 → `orchestrator.go:306 tools.WithThreadLink` 恒绑空串 → `task_work.thread_id/turn_id` 恒为 `''`。v1 侧 `st.thread.ID`/`ts.turn.ID` 就在同一函数里现成可用；WS 侧建议在 `:644` 用 `obslog.IDsFromContext(turnCtx)` 取（避免依赖块作用域，`ws.go:510-515` 已有 `turnIDs`）；SSE 无线程概念，只能每请求合成一对，需在验收里写明
- **`B1/M04`** — 十个生命周期方法都存在（`Spawn:104 Result:232 List:249 SendInput:278 Assign:314 Cancel:365 Resume:387 Wait:499 Close:540 AddUsage:796`）；三个缺口：① 槽位泄漏（同 LEAK2，另牵出 §4b 的 Resume 全挂）；② **`rt.turnCancel` 生产路径无人赋值**（只有 `manager.go:291,294-295` 读、`manager_coverage_test.go:1143` 白盒写），`interrupt=true/false` 行为相同——修法是 `runAgentLoop`（`:648` 的 for 循环）每轮派生 turn ctx 并 `SetTurnCancel`；注意 `SendInput` 已是「先塞 mailbox（`:290`）再 turnCancel（`:295`）」的正确顺序，所以中断后 mailbox 必有内容；③ **`tools.UsageSinkFrom`（`subagent.go:261`）生产零调用**——`orchestrator.go:713` 绑了 sink，消费点本该在 `runSubAgentTurn` 的 usage 回调（`orchestrator.go:672-678`，那里 `subUsage` 累加完从不被读），补一行转发即可（`TurnUsage` 是 int，`registry.Usage` 是 int64，需转型）
- **`A2/DT2`** — 见 §7

## 9. 审计 / spec 的过时与错误

1. ⚠️ **审计自相矛盾且其中一句是错的**：`docs/feature-status-audit.md:264` 写「`detachRuntime` 函数本身存在但无调用者」，`:538` 写「根本不存在」。**:538 正确，:264 错误**。spec §4.3 W3（`:171`）跟的是正确的那句
2. **审计的「11 个写方法」口径含糊**：实为 11 个裸写点 / 10 个导出方法，且漏记 `migrate`（`store.go:131`）
3. **审计 DT1 行号漂移**：`:503` 说 `orchestrator.go:294 WithThreadLink`，实际 **`:306`**
4. **审计 M04 行号漂移**：`:538` 说 `runningLocked()（manager.go:570）`，声明实际在 **`:571`**
5. ⚠️ **spec §4.4 说 W1–W5「互不重叠代码区」，对 W1/W3 不成立**：W1 要在 `ws.go:644` 与 `v1/service.go:313` 的 `TurnOpts` 字面量加 `PlanMode` 与图像字段，W3 要在同两处加 `ThreadID/TurnID`。同一个 struct literal，必冲突。**需在两份计划里互相点名，或约定 W1 先落地**
6. ⚠️ **spec §4.3 W3 缺了一个已实证的阻塞点**：legacy 入口接 cap 在阻塞式 `ManagedSubAgentRun` 下会造成活锁（§5b）。另：spec 说「legacy 三入口」，实际是 **5 个生产调用点覆盖 4 个工具面**
7. `manager_race_test.go:49` 用 `Path: t.TempDir()`（目录而非文件路径）给 Manager 做持久化路径，`writeAtomic` 必然失败——测试仍绿是因为它不断言持久化。不属五项，但动 `finishTerminal` 时会碰到
8. `internal/tools/subagent.go:422` 有一行 `var _ = filepath.Base`，注释写着 "keep unused-import satisfaction"——`filepath` 在该文件已无真实用途，可顺手清
