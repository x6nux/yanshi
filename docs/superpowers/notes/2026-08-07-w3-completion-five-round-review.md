# W3（并发与事务）完成后五轮评审 · 2026-08-07

W3 五条全部翻成 `done`（A2/DT1、A2/DT2、B1/M04、F1/WAL1、F2/LEAK2）后按
`docs/superpowers/review-checklist.md` 跑五轮。台账在这轮结束时 58/63（92%）。

**独立评审仍为零。** subagent 配额（200/200）本会话始终耗尽，本轮开工前又探过一次，
仍是 `Subagent spawn limit reached`。以下五轮全部是主循环自评。按清单的定义，
「自评零阻塞」不等于「独立评审零阻塞」。

---

## 本包最值得记的一件事：探针不红，两次都是注释错了

F1/WAL1 的两个变异探针**没有变红**。停下来查因，两次都发现代码是对的、**解释它的
注释是错的** —— 而且错的方向都是「把某一行代码说成了某个保证的成因」：

| 探针 | 预期 | 实测 | 真相 |
|---|---|---|---|
| 去掉 `writeMu` | 零 BUSY 断言应红 | 绿 | 零 BUSY 由 `busy_timeout` 保证。实测 16×50：只去 `writeMu` → 0 BUSY；去 `writeMu` **且** `busy_timeout(0)` → **717/800**。`writeMu` 买的是「等 Go mutex 而不是在 SQLite 重试循环里自旋」。 |
| `Close` 不做 `wal_checkpoint(TRUNCATE)` | -wal 有界断言应红 | 绿 | SQLite 在最后一个连接干净关闭时自己 checkpoint 并删 -wal。那句是**防御性**的（非干净退出时下一个 opener 会捡到 WAL），不是有界性的成因。 |

两处注释都已改成实测结论，并把命令与数字一起写进去。这是本次评审的最高价值产出 ——
**探针不红不等于测试没用，也可能是文档在撒谎**，而后者会在下一次改动时把人引到错误
的地方。

同一轮还抓到第三条同形态的：`TestCrossProcessBusyTimeout_DualOpen` 锁的是
`a.writeMu`（Store 实例 a **私有**的 mutex），而 `b` 是另一个 Store、有自己的
`writeMu`，`b.KVSet` 从不等任何东西 —— 50ms sleep 里早跑完了。它证明的是「两个 Store
实例能写同一个文件」，那在 `busy_timeout=0` 时也成立。已重写成真持有 SQLite 写事务
（文件级锁，同进程两个 `sql.DB` 与两个进程语义一致，不必起子进程）。

## 第 1 轮 · 零消费者与装配断言

扫本包新增的 9 个导出符号，全部有生产消费者。但发现**两处「接上了却没测」**：

- **R1-1（已修）** `broker.Work = mirror` 在 `Build` 里是内联赋值，GOV4 看不见（它只
  遍历 `Build*` 函数）。删掉那一行，`internal/task` 与 `internal/task/work` 两个包的
  测试**全绿** —— 而 durable 行会重新永远停在 pending。补
  `internal/bootstrap::TestBuildWiresTheDurableLifecycleMirror`，探针红。
- **R1-2（已修）** `RecoverInterrupted` 同理：方法有测试，`Build` 里的调用点没有。
  补 `internal/bootstrap::TestBuildRecoversInterruptedDurableTasks` —— 先用一个普通
  Store 在同一个文件里预置一条 running 行，再 `Build`，断言它变 pending。探针红。

这两条正是「写了但没接」的**镜像**：接上了，但接线本身没测。

## 第 2 轮 · 幻影名与出厂可达性

GOV5/GOV7 全绿。实测出厂 profile 对 W3 涉及工具的档位后，发现一条**梯度倒置**：

**R2-1（已修）** `task_gate_run` 与 `artifact_read` 已注册但不在 allow 列表 → Prompt，
而**更宽**的 `shell_run` 免提示。`task_gate_run` 的能力面是 `shell_run` 的严格子集
（拒元字符、cwd jail 到 work root、额外 Authorize 一次 FS read）；`artifact_read` 是
只读，且是 gate 大输出溢写后的唯一取回途径 —— 不给它，spill 就等于删除。两个都已加入
`DefaultOrchestratorProfile`，理由写在 profile.go 那段注释里。

同一形态在 W1 评审里记过一次（记录里的 `shell_list` 是我探针手写的幻影名，已在那份
记录里更正）。剩余部分已于同日闭合：10 个自管理工具加入出厂 allow 列表。

## 第 3 轮 · GOV8 正反探针

- evidence 指向不存在的测试名 → 红
- evidence 指向带 build 约束的测试作为**唯一**证据 → 红
- 新加的两条 bootstrap 测试没进 evidence → `TestLedgerMarkersAreLive` 当场点名（这是
  它抓到的，不是我记得的）

零阻塞。

## 第 4 轮 · 最后一跳

**R4-1（未修，记录归属）** `LifecycleMirror` 的状态变化**不发** `task_update` 帧。
事件只在工具层经 `EmitWorkEvent` 发出，需要 turn context 里的 callback；broker 的回调
跑在 worker goroutine 里，没有 turn，拿不到 callback。**后果**：用户在 TUI 上看不到
durable task 从 pending 变 running/completed，除非再调一次 `task_read`。

**不作为 A2/DT1 的阻塞**：acceptance 说的是「状态机正确」，不是「状态变化实时推送」。
推送需要一条 broker → WS 的跨 turn 事件通道，那是新设计而非接线。记在这里等排期。

## 第 5 轮 · 端到端（证据证零件 vs 成品）

**R5-1（已修）** broker 侧有 `TestBrokerReportsDurableTaskLifecycle`（证明 broker 调
sink），work 侧有 `TestBrokerMirrorMovesTheDurableRow`（证明 mirror 移动行），**没有
一条同时跑这两段代码**。中间任何不匹配 —— broker 不认这个 task type、parent id 放在
别的字段、adapter 用错 type 字符串 —— 两边都绿，而 `task_read` 会对一个已完成的任务
永远报 pending，**正是本工作包要修的那个缺陷**。

补 `internal/task::TestDurableTaskRunsEndToEnd`（一个 SQLite 文件、真 Broker、真
Manager 经 BrokerAdapter 投递、worker 做 Claim + RecordResult 两步），外加
`TestDurableTaskSurvivesAWorkerThatDies`（worker 不心跳，只有 sweeper 会动它）。

写第二条时顺带踩到一个**秒精度陷阱**并记进注释：`hbTimeout` 传 `-1`（1 纳秒）无效，
cutoff 与 `updated_at` 都是秒粒度 Unix 时间戳，亚秒偏移四舍五入成同一个值，
`ListStaleRunning` 的严格比较找不到任何行。必须至少 `-time.Second`。

---

## 结论

五轮共 4 条阻塞，全部已修并各配变异探针复验：

1. **R1-1** `broker.Work` 的接线没测（GOV4 盲区：内联赋值）
2. **R1-2** `RecoverInterrupted` 的启动调用点没测
3. **R2-1** `task_gate_run` / `artifact_read` 权限梯度倒置
4. **R5-1** broker 与 work 两半从未一起跑过

外加 3 处**注释与实测因果相反**（`writeMu` / `Close` checkpoint / DualOpen 测试），
已全部改正并把复核命令写进注释。

**未闭合（各有归属）：**

- ~~`LifecycleMirror` 的状态变化不推送到 TUI~~ → **同日已闭合**：Server 维护 WS 连接
  注册表，`LifecycleMirror.OnTransition` 经 bootstrap 接到 `srv.Broadcast`；端到端测试
  跑真 Build + 真 WS 客户端 + 真 Claim，三个探针各一向
- ~~`PRAGMA foreign_keys` 从未开启~~ → **同日已闭合**：DSN 里开（每条池连接都要，
  单条 Exec 只武装一条比不开更糟），`:memory:` 走 `applyConnectionPragmas`。
  实测**并不是**「10 条测试造孤儿数据」——开了之后暴露的是一个**真实的写序 bug**：
  `vcs.initNewRepoLocked` 先写初始 commit 再插 `vcs_repos` 行。四条 vcs 测试改走
  「一条 pragma 关掉的固定连接」来伪造腐蚀（外部工具的真实形态），
  `TestSession_AppendMessage_MissingSession` 反转为「孤儿消息不可存储」
- ~~出厂权限梯度倒置的另一半~~ → **同日已闭合**（W1 那份记录里有更正与闭合说明）
- 独立评审为零 → 需要 `CLAUDE_CODE_MAX_SUBAGENTS_PER_SESSION` 提额
