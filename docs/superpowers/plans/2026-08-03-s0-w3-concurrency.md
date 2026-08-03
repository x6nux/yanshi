# S0/W3 并发与事务 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修好 registry 的并发槽位泄漏（连带修好「进程内终态 agent 永不能 Resume」这个审计漏记的缺陷），把 `work` 包 12 个绕过写事务的写点接回进程级写锁，补齐 task 与 thread/turn 的关联，并把 gate 的破坏性删除判定收敛到与 `shell_run` 同等强度。

**Architecture:** 四条链路：① `registry.Manager` 的运行时生命周期（新写 `detachRuntime` + `parked` 计数）；② `work.Store` 的写事务接线；③ 三个 turn 入口填 `ThreadID`/`TurnID`；④ gate 的 `guard.Action` 补 `Workdir`。①③ 各有一个**已实证的阻塞点**，见裁定。

**Tech Stack:** Go 1.26.4，`modernc.org/sqlite`，既有 fake 体系。

---

## 本计划的写法

**这是意图级计划，不是代码清单。** 每步说清楚：改哪个文件的哪个函数、断言什么行为、怎么观察、为什么重要、预期看到什么。**具体代码由实现者写** —— 你手里有编译器，本文档没有。

「已核对事实」是**实测结果**，可直接依赖；其余标识符请先 grep。

---

## ⚠️ 裁定 0：**W1 必须先落地**

W1 要在 `ws.go:644`、`v1/service.go:313`、`chat.go:132` 的 `TurnOpts` 字面量加 `PlanMode` 与图像字段；W3 要在**同样三处**加 `ThreadID`/`TurnID`。**同一个 struct literal，必然冲突。**

> spec §4.4 声称 W1–W5「互不重叠代码区」—— **对 W1/W3 不成立**。这是 spec 的事实错误，本计划以实测为准。

**执行顺序：W1 的 Task 11/12/13 合并后，再开始 W3 的 Task 8。** W3 的其余任务与 W1 无重叠，可先行。

---

## 已核对事实（实测，不要重新推导）

| 事实 | 位置 |
|---|---|
| **`detachRuntime` 在生产、测试、git 历史里都不存在** —— 要新写 | `grep` 零命中；`git log -S` 只有 3 条 docs 提交 |
| 唯一「实现」在 B1 计划文档里（从未落地），且**锁名写的是 `m.mu`，当前实现叫 `m.mtx`** | `manager.go:37` |
| `finishTerminal` 从不 `delete(m.runtime,...)`，也从不 `rt.cancel()` | `manager.go:722-755` |
| 全文件 `delete(m.runtime,...)` 只有 3 处，**全在错误回滚路径** | `:202`、`:210`、`:486` |
| `runningLocked()` 是 `max(runtime条目数, records中Running数)` | `manager.go:571-589`，判断在 `:585-587` |
| cap 闸门两处：`Spawn:154`、`Resume:417` | `manager.go` |
| 💡 `:569-570` 注释**已预授权**「用 runtime map 作唯一权威计数，records 计数仅供参考」 | `manager.go` |
| `Resume` 在 `:412` 检查 runtime 条目存在即报错 | `manager.go` |
| `work/store.go` 的 `wt()` 定义在 `:49`，**生产代码零调用**；包头注释 `:2-3` 声称「所有写路径经 WriteTxer」**是假陈述** | `internal/task/work/store.go` |
| 裸写点 **12 处 / 11 个方法**（含被审计漏记的 `migrate`） | 见 Task 5 表 |
| `store.WriteTx` 的 doc 明写 **NOT reentrant** | `internal/store/store.go:177` |
| `TurnOpts.ThreadID/TurnID` 在**三个**生产入口全不填 → `WithThreadLink` 恒绑空串 | `ws.go:644`、`chat.go:132`、`v1/service.go:313`；消费点 `orchestrator.go:306` |
| `gate.go:88-92` 的 `guard.Action` **没填 `Workdir`**；对照 `tools/shell.go:128` 有 | `internal/task/gate.go` |
| `workdir==""` 时 `isCatastrophicTarget` **跳过**「删 workdir 自身/祖先」两条判定 | `internal/guard/destructive.go:274-295`，短路在 `:284` |
| `rt.turnCancel` **生产路径无人赋值**（只有白盒测试写过） | 读点 `manager.go:291,294-295`；写点仅 `manager_coverage_test.go:1143` |
| `SendInput` 已是「先塞 mailbox（`:290`）再 turnCancel（`:295`）」的正确顺序 | `manager.go` |
| `tools.UsageSinkFrom`（`subagent.go:261`）**生产零调用**；`orchestrator.go:672-678` 的 `subUsage` 累加完从不被读 | sink 绑定在 `orchestrator.go:713` |
| `runSubAgent` 生产调用点 **5 处覆盖 4 个工具面**（spec 说「三入口」不准确） | 见 Task 3 表 |
| `ManagedSubAgentRun`（`subagent.go:274`）与 `spawnWithRetry`（`:315`）**零生产调用点** | 唯一走 Manager 的是 `agent_spawn`（`agent_lifecycle.go:66`） |
| ctx 侧接线**是齐的** —— `runSubAgent` 在生产 ctx 里拿得到 Manager 与 factory，只是没用 | `orchestrator.go:342`、`:356`（都在 `bindManagedRunner`，`:337`） |
| `TurnUsage` 是 `int`，`registry.Usage` 是 `int64` —— 转发需转型 | — |

### 两处已实测复现的 bug

**槽位泄漏**（MaxConcurrent=2 → 连开两个瞬时完成的 agent 并 `Wait` 到终态 → `List(false)`）：
```
List(): Running=0 Limit=2 agents=2
SLOT LEAK: third Spawn rejected with cap=2 while List reports Running=0
```

**进程内终态 agent 无法 Resume**（审计漏记）：
```
RESUME BROKEN: subagent ag-... runtime already active
```
runtime 永不删除 ⇒ **任何在本进程内跑到终态的 agent 都永远不能 Resume**。既有 `manager_resume_test.go` 之所以绿，是因为它 Resume 的是从磁盘 load 进来、本进程从未有过 runtime 条目的记录。

> 同步一律用 `m.Wait(ctx, id, WaitOpts{Timeout: ...})` 这类**可观测信号**，**不用 `time.Sleep`**。

---

## 四条裁定

**裁定 1 — `runningLocked()` 改用 runtime map 作唯一权威计数。**
`manager.go:569-570` 的注释原文已经预授权了这条路径（「若 RAC1 报告可测量的竞态，解法：以 runtime map 为唯一权威计数，records 计数仅供参考」）—— **不需要新论证**。这同时消除 max() 的隐患，并且是裁定 2 的前提。

**裁定 2 — legacy 入口接 cap 必须同时引入 `parked`，否则活锁。**

原型化「`runSubAgent` 有 Manager+factory 时改走 `ManagedSubAgentRun`」后实证发现：`ManagedSubAgentRun` 是**阻塞**的（`mgr.Wait`），而 `spawnWithRetry` 无限重试直到有槽。于是**父 agent 持槽等子 agent**。默认 cap=10 时，10 个 depth-1 的 agent 各自再调 `agent_start` → 10 个槽全被阻塞的父占住，孙 agent 永远排不进来 → **活锁**。（depth-0 不会，编排器 turn 本身不占槽。）

**解法**：`runtimeAgent` 加 `parked bool`；`ManagedSubAgentRun` 在 `mgr.Wait` 前后翻转当前 agent 的 parked 状态；`runningLocked()` 只数**未 parked** 的条目。

**代价**：`List().Running`（按 record 数）可能短暂大于 cap —— **必须写进文档**。

> ⚠️ 原型只做到「能编译 + 死锁已实证识别」，**未跑完整回归**。本计划报的是「设计可行」，不是「已验证」。实现时若发现新的锁序问题，**停下来汇报**。

**裁定 3 — `A2/DT2` 的两条偏离分别处理：偏离 1 收敛，偏离 2 接受并改验收标准。**

| 偏离 | 处置 | 理由 |
|---|---|---|
| `gate.go` 的 `guard.Action` 缺 `Workdir` | **收敛到设计** | 纯安全降级；修法是加一个字段、零行为面扩张、可端到端断言。没有理由不改 |
| gate 用 `exec.CommandContext` 直跑，不走 shell session runtime | **接受偏离，改验收标准** | shell v2 runtime 至今全仓零注册（那正是 W1 的 9 条豁免）；把一次性 argv 命令挂到会话生命周期上，是为满足一句路线图措辞而引入耦合。gate 已经过同一个 `guard.Authorize`，**安全面不因绕开 session 而变**；DT2 的四条正式验收（证据结构 / 大输出成 artifact / 挂对 task / 退出码与 duration）**与执行载体无关，已全部满足** |

⚠️ **必须落一条新 ADR** 把「gate 不走 shell session」写进 Consequences，否则下一轮审计会再判一次 divergent。

**裁定 4 — `B1/M04b` 由本工作包收尾。**
W1 只完成了它四条验收里的「父可消费 EVIDENCE」，其余三条（重启后可 list/resume、并发上限生效、输出 5 段可解析）都落在 W3 的工作面上 —— 其中「可 resume」正是上面那条审计漏记的缺陷。**台账 `package` 字段仍写 W1（它的归属没变），但 verdict 由本工作包翻。**

---

## 文件结构

**修改**

| 文件 | 改什么 |
|---|---|
| `internal/agent/registry/manager.go` | `detachRuntime`（新写）、`finishTerminal`、`runningLocked`、`parked`、`turnCancel` 赋值 |
| `internal/agent/registry/manager_coverage_test.go` | `TestResumeRejectsRuntimeStillActive` 改写为白盒 |
| `internal/agent/batch/runner_test.go` | `TestRunnerSpawnCapExhausted` 改写 |
| `internal/tools/{subagent,agent,agent_analysis,agent_workflow,agent_dag}.go` | legacy 入口接 Manager；`UsageSinkFrom` 转发 |
| `internal/task/work/store.go` | 12 个写点接 `wt()` |
| `internal/api/http/{ws,chat}.go`、`internal/api/v1/service.go` | `TurnOpts` 填 `ThreadID`/`TurnID` |
| `internal/task/gate.go` | `guard.Action` 补 `Workdir` |
| `config.example.yaml` | `subagents:` 段（若 W1 未加） |
| `docs/adr/0011-*.md` 或后续编号 | gate 不走 shell session |

---

## Task 1: 新写 `detachRuntime` 并在 `finishTerminal` 释放槽位

**Files:** Modify `internal/agent/registry/manager.go`；Test `internal/agent/registry/manager_spawn_test.go`

- [ ] **Step 1: 写失败测试（两条）**

**断言什么**：
1. **槽位归还** —— MaxConcurrent=2，连开两个瞬时完成的 agent 并 `Wait` 到终态后，**第三次 `Spawn` 成功**
2. **进程内终态 agent 可 Resume** —— 一个在本进程内跑到终态的 agent，`Resume` **不报 "runtime already active"**

**怎么观察**：用 `m.Wait(ctx, id, WaitOpts{Timeout:...})` 同步，**不要 `time.Sleep`**。

**为什么重要**：第 2 条**审计完全没记**。`Resume` 在 `manager.go:412` 检查 runtime 条目存在即报错，而 runtime 永不删除 —— 所以这不是边缘情况，是**所有本进程 agent 的 Resume 全挂**。它直接打在 `B1/M04`/`M04b` 验收的「resume 跨重启可尝试」上。

**预期**：两条都 FAIL，错误消息与上面「已实测复现」段一致。

- [ ] **Step 2: 实现**

**怎么改**：新写一个方法，从 runtime map 摘除并返回该 agent 的运行时条目；在 `finishTerminal` 末尾调用它，拿到非 nil 且带 cancel 时执行 cancel。

⚠️ **锁名是 `m.mtx`**（`manager.go:37`），不是 B1 计划文档里写的 `m.mu` —— **照抄那份文档不编译**。

⚠️ **注意锁序**：`finishTerminal` 现有顺序是 `persistMu.Lock` → `mtx.Lock` → 写字段 → `mtx.Unlock` → `writeAtomic` → emit → `persistMu.Unlock`。摘除动作放在哪一段里要想清楚 —— **在持 `mtx` 时摘除、在释放 `mtx` 之后再 cancel**，避免在持锁状态下调用外部 cancel 回调。

**预期**：两条测试 PASS，`go test -race` 也 PASS。

- [ ] **Step 3: 提交**

---

## Task 2: 修好两条把 bug 当作预期行为的既有测试

> 依赖 Task 1 —— 它们会因 Task 1 的修复而变红。**这是预期的**。

**Files:** Modify `internal/agent/registry/manager_coverage_test.go`（`:1195-1236`）、`internal/agent/batch/runner_test.go`（`:253-279`）

**背景：** 两条既有测试正在断言 Task 1 修掉的 bug。

**`TestResumeRejectsRuntimeStillActive`** 的注释原文自陈「Agent is now terminal (Completed) but still in runtime map」—— **它在断言这个 bug**。修复后 `require.Error` 会拿到 nil。

**`TestRunnerSpawnCapExhausted`** 用 MaxConcurrent=1 + 2 行，原本靠「第一个 agent 跑完不还槽」让第二行耗尽重试。修复后两行都会成功。

- [ ] **Step 1: 改写 `TestResumeRejectsRuntimeStillActive` 为白盒**

**要保留的价值**：该分支修复后**仍然可达** —— `finishTerminal` 写完终态 record 到摘除 runtime 之间有一个窗口。但它**不能靠自然路径触发**了。

**怎么改**：先断言「终态后 runtime 条目已消失」（这是 Task 1 的新契约），再**手工重造那个窗口**（直接往 runtime map 塞一个条目），然后断言 `Resume` 仍然拒绝。

> ✅ 该改写已实测 `go test -race` PASS。

- [ ] **Step 2: 改写 `TestRunnerSpawnCapExhausted`**

**要保留的价值**：cap 耗尽时 runner 的重试行为。

**怎么改**：先用一个**阻塞的** runner 直接 `Spawn` 占住唯一槽位（用 channel 做 park/release 同步，**不用 sleep**），再跑两行 → 两行都应 cap 耗尽。

> ✅ 该改写已实测 `go test -race` PASS。

⚠️ **不要为了让测试变绿而放宽断言** —— 这两条测试保护的行为是真实的，只是触发方式必须换。

- [ ] **Step 3: 全量与提交**

Run: `go test -race ./internal/agent/... ./internal/task/...`

---

## Task 3: legacy 入口接 Manager cap（含 `parked` 防活锁）

> **裁定 2 的实现。这是本工作包风险最高的任务。**

**Files:** Modify `internal/agent/registry/manager.go`（`parked`、`runningLocked`）、`internal/tools/subagent.go`、五个调用点所在文件；Test 两个包

**背景：** `runSubAgent`（`internal/tools/agent.go:381`）先看 `SubAgentRunnerFromContext`，没有则退回裸 `chatModel.Generate` —— **全程不碰 `registry.Manager`**，所以进程级并发上限对它完全无效。

**生产调用点 5 处，覆盖 4 个工具面**（spec 说「legacy 三入口」不准确）：

| 位置 | 工具面 |
|---|---|
| `agent.go:336` | `agent_start` |
| `agent_analysis.go:60` | `analysis` |
| `agent_workflow.go:264` | `workflow_start`（plan 阶段） |
| `agent_workflow.go:372` | `workflow_start`（并行任务体） |
| `agent_dag.go:202` | DAG 步骤 |

`agent_dag.go:112` 与 `agent_workflow.go:323` 的 `NumCPU` 信号量是**每次调用各自一份的局部信号量**，与进程级 cap 无关。

- [ ] **Step 1: 先写活锁的复现测试**

**断言什么**：cap=N 时，N 个 depth-1 的 agent 各自再派生一个子 agent，**整体能在合理超时内完成**。

**为什么先写这条**：裁定 2 的活锁是**已实证**的。先有复现测试，才能证明 `parked` 真的解决了它，而不是碰巧没触发。

**预期**：接线后（未加 `parked`）FAIL / 超时。

- [ ] **Step 2: 实现 `parked` 与权威计数**

**怎么改**：`runtimeAgent` 加 parked 标志；提供一个设置它的方法；`runningLocked()` 改为**只数未 parked 的 runtime 条目**，去掉与 records 的交叉核对。

**为什么可以去掉交叉核对**：`manager.go:569-570` 的注释已预授权（裁定 1）。

⚠️ **`List().Running`（按 record 数）可能短暂大于 cap** —— 这是 `parked` 的固有代价，**必须写进该方法的 doc 注释与用户文档**，否则会被当成新 bug 上报。

- [ ] **Step 3: 接线 5 个调用点**

**怎么改**：`runSubAgent` 在 ctx 里拿得到 Manager 与 factory 时走受管路径；`ManagedSubAgentRun` 在阻塞等待前后翻转 parked。

⚠️ **五处都要接** —— 漏一处就等于 cap 有个后门。

⚠️ 若实现中发现新的锁序问题或 `parked` 无法覆盖某条路径，**停下来汇报**，不要自行扩大设计。

- [ ] **Step 4: 全量与提交**

Run: `go test -race ./...` —— **本任务必须跑 `-race`**。

---

## Task 4: `turnCancel` 赋值 + `UsageSinkFrom` 转发（M04 剩余两条）

**Files:** Modify `internal/agent/registry/manager.go`（`runAgentLoop`）、`internal/agent/orchestrator/orchestrator.go`（`:672-678`）；Test 两个包

**缺口 A —— `rt.turnCancel` 生产路径无人赋值。** 只有 `manager.go:291,294-295` 在读，写点仅存在于白盒测试（`manager_coverage_test.go:1143`）。后果：`SendInput` 的 `interrupt=true` 与 `false` **行为完全相同**。

**缺口 B —— `tools.UsageSinkFrom`（`subagent.go:261`）生产零调用。** `orchestrator.go:713` 绑了 sink，但消费点本该在的地方（`orchestrator.go:672-678` 的 `subUsage`）**累加完从不被读**。

- [ ] **Step 1: 写失败测试（两条）**

**断言什么**：
1. `SendInput(interrupt=true)` **真的中断**当前 turn，且与 `interrupt=false` 行为**可区分**
2. 子 agent 的用量**流进** sink

**为什么第 1 条重要**：中断后 mailbox 必有内容 —— `SendInput` 已是「先塞 mailbox 再 turnCancel」的正确顺序，所以中断语义本身是设计好的，**只差没人赋值**。

**预期**：两条 FAIL。

- [ ] **Step 2: 实现**

**A**：`runAgentLoop`（`:648` 的 for 循环）**每轮派生 turn ctx** 并设置 turnCancel。

**B**：在 `orchestrator.go:672-678` 把累加好的 `subUsage` 转发给从 ctx 取到的 sink。

⚠️ **`TurnUsage` 是 `int`，`registry.Usage` 是 `int64`** —— 需要显式转型。

- [ ] **Step 3: 全量与提交**

---

## Task 5: `work` 包 12 个写点接回写事务（WAL1）

**Files:** Modify `internal/task/work/store.go`；Test 新建 `internal/task/work/` 下的外部测试包文件

**背景：** `wt()` 定义在 `store.go:49`，**生产代码零调用**。包头注释 `:2-3` 声称「All write paths route through the injected WriteTxer」—— **是假陈述**。后果：`MaxOpenConns=4` 下这些写与进程级 `writeMu` 脱钩，实测能撞出 `SQLITE_BUSY`。

**12 个裸写点 / 11 个方法**（审计的「11 个写方法」口径不准，且**漏记 `migrate`**）：

| 行 | 方法 |
|---|---|
| **131** | `migrate`（未导出，**审计漏了**） |
| 138 | `Create` |
| 312 | `Transition` |
| 348 | `AppendTimeline` |
| 367 | `AttachBrokerTask` |
| 385 | `SetChecklist` |
| 408 | `AddChecklistItem` |
| 434 / 448 | `PatchChecklistItem`（**同一方法两处**） |
| 455 | `RecordGate` |
| 477 | `PutArtifact` |
| 533 | `DeleteArtifactsBefore` |

- [ ] **Step 1: 写能真正证伪的失败测试**

**文件**：`package work_test`（**外部测试包**，不触 GOV1）

**断言什么**：进程级写锁被持有期间，`work` 的写方法**不能**完成。

**怎么观察**：用 `store.OpenWith(..., OpenOptions{MaxOpenConns:4, BusyTimeoutMs:1})` 开**真实文件库**并注入作 WriteTxer；一个 goroutine 在 `WriteTx` 里做完 INSERT 后 park 住不 commit；另一个调 `AppendTimeline`。

⚠️ **一个已实测踩到的坑必须避开**：`t.Fatal` 会跳过 `close(release)`，导致 `t.Cleanup(st.Close)` 卡死在 writeMu（实测撞了 2 分钟超时）。**必须用 `sync.Once` 包住 release 并用 `t.Cleanup` 注册，且注册顺序在 `st.Close` 的 cleanup 之后**（LIFO 保证它先跑）。

> ✅ 双向实测：接线前 FAIL（`database is locked (5) (SQLITE_BUSY)`，0.02s）；接线后 `go test -race` PASS（0.57s）。

**预期**：FAIL，报 SQLITE_BUSY。

- [ ] **Step 2: 接线 12 个写点**

**怎么改**：全部改为经 `wt().WriteTx(...)`。

> ✅ 已实测可行：含 `migrate` 的多语句 DDL，modernc 在事务内正常执行；`go build ./...` OK，相关包测试全绿。

⚠️ **`store.WriteTx` 的 doc 明写 NOT reentrant** —— 这是必须守住的不变量。已核实**当前无重入风险**：`work.Manager` 每个方法只顺序调用一个 Store 写方法，Store 写方法之间不互调。**新增写方法时必须保持这条**，写进 store.go 的包头注释。

- [ ] **Step 3: 修正包头的假陈述**

`store.go:2-3` 那句「All write paths route through the injected WriteTxer」在接线后才**第一次成为真话**。顺手确认措辞与现实一致。

- [ ] **Step 4: 全量与提交**

Run: `go test -race ./internal/task/... ./internal/tools ./internal/store/...`

---

## Task 6: gate 的 `guard.Action` 补 `Workdir`（DT2 偏离 1）

**Files:** Modify `internal/task/gate.go`（`:88-92`）；Test `internal/task/` 或 tools 层

**背景：** `gate.go:88-92` 构造的 `guard.Action` **没填 `Workdir`**，而对照的 `tools/shell.go:128` 明确写了，注释原文说明它「feeds the destructive-deletion dimension」。

**实测后果**（`internal/guard/destructive.go`）：`workdir==""` 时 `isCatastrophicTarget`（`:274-295`）**跳过**「删 workdir 自身 / 祖先」两条判定（短路在 `:284`）；`resolvesOutsideWorkdir`（`:301`）对相对路径判为域内。即 `task_gate_run` 里的 `rm -rf ../sibling`、`rm -rf <项目根绝对路径>` 相比 `shell_run` **安全等级降级**。

> 字面 root（`/`、`~`、`*`、`..`、`/etc`…）仍被 `catastrophicRoots` 拦住 —— 所以这不是完全无防护，是**部分降级**。

- [ ] **Step 1: 写失败测试**

**断言什么**：经 gate 执行的破坏性命令，与经 `shell_run` 执行的**同一条命令**得到**相同的 guard 判定**。至少覆盖：删 workdir 自身、删 workdir 祖先、删相对路径的兄弟目录。

**怎么观察**：走 tools 层真实的 gate 执行路径做端到端断言。

**为什么重要**：这是纯安全降级，两条路径对同一命令给出不同答案本身就是缺陷。

**预期**：FAIL —— gate 放行了 `shell_run` 会拦的命令。

- [ ] **Step 2: 实现**

**怎么改**：`guard.Action` 加 `Workdir`，值取 gate 的执行根目录。

**零行为面扩张** —— 这个改动只让判定更严，不会放行任何原本被拦的东西。

- [ ] **Step 3: 全量与提交**

---

## Task 7: 为「gate 不走 shell session」落 ADR（DT2 偏离 2）

**Files:** Create `docs/adr/00NN-*.md`（编号取当前最大 +1，从 `docs/adr/0000-template.md` 复制）

**为什么需要**：裁定 3 决定**接受**这条偏离。但不写下来，下一轮审计会再判一次 `divergent`，然后再花一轮重新论证。

- [ ] **Step 1: 写 ADR**

**Context 要写**：路线图措辞要求 gate 走 shell session runtime；实际用 `exec.CommandContext` 直跑（`gate.go:105`）。

**Decision**：接受偏离。

**Consequences 必须固化**（这些是不可违反的约束）：
1. gate 执行的是**一次性 argv 命令**，不具备会话生命周期语义 —— 不得因此推论 gate 可以跳过 guard
2. gate **仍然经过同一个 `guard.Authorize`**，安全面不因绕开 session 而变（Task 6 补齐 `Workdir` 后与 `shell_run` 等强）
3. `A2/DT2` 的四条正式验收（证据结构 / 大输出成 artifact / 挂对 task / 退出码与 duration）**与执行载体无关**
4. 若将来 gate 需要长驻会话语义，**必须重开 ADR**，不得直接接线

- [ ] **Step 2: 交叉引用**

在 `docs/feature-status.yaml` 的 `A2/DT2` evidence 里指向这条 ADR。

- [ ] **Step 3: 提交**

---

## Task 8: 三个 turn 入口填 `ThreadID` / `TurnID`（DT1）

> ⚠️ **裁定 0：本任务必须在 W1 的 Task 11/12/13 合并之后再开始** —— 改的是同一批 `TurnOpts` 字面量。

**Files:** Modify `internal/api/http/ws.go`（`:644`）、`internal/api/http/chat.go`（`:132`）、`internal/api/v1/service.go`（`:313`）；Test 三个包

**背景：** 三个入口都不填 `ThreadID`/`TurnID` → `orchestrator.go:306` 的 `WithThreadLink` 恒绑空串 → `task_work.thread_id/turn_id` **恒为 `''`**。

> 审计的 `orchestrator.go:294` 是漂移行号，实际 `:306`。

- [ ] **Step 1: 三条路径各写一条失败测试**

**断言什么**：一次 turn 产生的 task work 记录，其 thread/turn 关联**非空且正确**。

**预期**：三条 FAIL（关联为空串）。

- [ ] **Step 2: 实现**

**v1**：`st.thread.ID` / `ts.turn.ID` **就在同一函数里现成可用**，直接填。

**WS**：建议用 `obslog.IDsFromContext(turnCtx)` 取（`ws.go:510-515` 已有 `turnIDs`），**避免依赖块作用域**。

**SSE**：⚠️ **SSE 没有线程概念** —— 只能每请求合成一对。**这个限制必须写进台账 evidence 与用户文档**，否则「task 挂对 thread」在 SSE 上是一句空话。

- [ ] **Step 3: 全量与提交**

---

## Task 9: 台账翻牌 + 文档同步 + W3 收尾验证

**Files:** Modify `docs/feature-status.yaml`、`config.example.yaml`、`CLAUDE.md`、用户文档

- [ ] **Step 1: 补 `config.example.yaml` 的 `subagents:` 段（若 W1 未加）**

`SubagentsConfig`（`config.go:274-278`，校验 `:621-622`）只有 `limit` 与 `persistence_path` 两个字段。目前只有 `docs/user-guide/configuration.md:51,205-210` 提到，**示例文件里完全没有**。

> 若 W1 的 Task 10 已加，本步跳过并确认。

- [ ] **Step 2: 翻牌**

| 条目 id | 现 verdict | 证据来自 |
|---|---|---|
| `F2/LEAK2` | partial | Task 1（槽位）+ Task 3（cap 覆盖 legacy） |
| `F1/WAL1` | partial | Task 5 的 SQLITE_BUSY 测试 |
| `A2/DT1` | partial | Task 8 |
| `B1/M04` | partial | Task 1/3/4 |
| `A2/DT2` | **divergent** | Task 6 + Task 7 的 ADR |
| `B1/M04b` | partial | Task 1（可 resume）+ Task 3（并发上限）+ 输出 5 段 |

⚠️ **`A2/DT1` 的 evidence 必须写明 SSE 的限制**（每请求合成、无真实 thread 语义），否则是谎报。

⚠️ **`A2/DT2` 从 `divergent` 翻 `done` 依赖 ADR 已落地** —— ADR 没写完就不能翻。

⚠️ **`B1/M04b` 若「输出 5 段可解析」未实现，保持 `partial`** —— 与 W1 对它的处理用同一把尺子。**先确认这条验收的现状再决定翻不翻。**

- [ ] **Step 3: 台账门与计数**

Run: `go test ./internal/archtest -run TestFeatureStatus` → PASS（总数仍为 63）
Run: `go run ./cmd/featurestatus` → 确认这几条已终态

- [ ] **Step 4: 顺手清两处**

1. `internal/tools/subagent.go:422` 的 `var _ = filepath.Base`（注释写着 "keep unused-import satisfaction"）—— `filepath` 在该文件已无真实用途
2. `manager_race_test.go:49` 用 `Path: t.TempDir()`（**目录**而非文件路径）给 Manager 做持久化路径，`writeAtomic` 必然失败；测试仍绿是因为它不断言持久化。**动 `finishTerminal` 时会碰到它**

- [ ] **Step 5: 全量验证**

```bash
go build ./... && go vet ./... && go test ./...
go test -race ./internal/agent/... ./internal/task/...
go run ./cmd/codelines
go run ./cmd/gendocs -config docs/user-guide/configuration.md
go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md
go run ./cmd/api-schema -markdown docs/api/schema.md
go run ./cmd/api-schema -markdown docs/api/resources.md
git diff --exit-code docs/
```

- [ ] **Step 6: 提交**

---

## W3 验收清单

- [ ] `go test -race ./...` 全绿（**本工作包并发改动多，`-race` 是硬要求**）
- [ ] 槽位泄漏复现测试通过：终态后第三次 `Spawn` 成功
- [ ] **进程内终态 agent 可以 Resume**（审计漏记的那条）
- [ ] 活锁复现测试通过：cap=N 时 N 个 depth-1 agent 各派生子 agent 能完成
- [ ] `work` 包 12 个写点全部经 `wt()`；持写锁时 `AppendTimeline` 被正确阻塞
- [ ] gate 与 `shell_run` 对同一条破坏性命令给出**相同**判定
- [ ] 新 ADR 落地并被 `A2/DT2` 的 evidence 引用
- [ ] 台账条目终态，`A2/DT1` 的 evidence 写明 SSE 限制

## 移交与依赖

| 事项 | 关系 |
|---|---|
| **W1 必须先于 Task 8** | 同一批 `TurnOpts` 字面量 |
| `EmitWorkEvent`（`gate.go:146`）受 G05 的 `EmitWorkFrame` 断链影响 | **属 W1**，不在 W3 |
| `List().Running` 可能短暂大于 cap | `parked` 的固有代价，需写进文档 |

## 审计 / spec 中已被证伪的论断（不要照着做）

1. ⚠️ **审计自相矛盾且其中一句是错的**：`:264` 写「`detachRuntime` 函数本身存在但无调用者」，`:538` 写「根本不存在」。**`:538` 正确，`:264` 错误** —— 要新写，不是加调用者。
2. **审计漏记了「进程内终态 agent 无法 Resume」** —— 只记了「取消不泄漏」。
3. **审计的「11 个写方法」口径不准** —— 实为 12 个写点 / 11 个方法，且漏记 `migrate`（`store.go:131`）。
4. ⚠️ **spec §4.4 说 W1–W5「互不重叠代码区」，对 W1/W3 不成立**（裁定 0）。
5. ⚠️ **spec §4.3 W3 缺了一个已实证的阻塞点**：legacy 入口接 cap 在阻塞式 `ManagedSubAgentRun` 下会活锁（裁定 2）。
6. **spec 说「legacy 三入口」不准确** —— 实际 5 个生产调用点覆盖 4 个工具面。
7. **行号漂移**：审计 DT1 的 `orchestrator.go:294` 实际 `:306`；审计 M04 的 `manager.go:570` 实际 `:571`。
