# W2（预算闸门）+ W4（压缩正确性）完成后五轮评审 · 2026-08-07

两个包都小（W2 四条、W4 两条），合并跑一轮五轮评审。台账在这轮结束时
**63/63（100%）** —— S0 的 63 项验收全部终态（62 done + 1 removed）。

**独立评审仍为零。** subagent 配额（200/200）本会话始终耗尽。以下五轮全部是主循环
自评；按清单的定义，「自评零阻塞」不等于「独立评审零阻塞」。

---

## 第 1 轮 · 零消费者与装配断言

扫本轮新增的导出符号，全部有生产消费者：`QueryWithUsage`（`runLightweightGoal`
调用）、`planKeepRecent`（`maybeCompact` 调用）、`KnownStatuses`（`isKnown` 调用）。

顺带验了一条容易漏的接线：bootstrap 的 fake model 现在报 usage(12/8/20)，删掉那行
`TestLightweightGoalReportsItsTokensToTheSink` 当场变红 —— 接线有断言守着。

零阻塞。

## 第 2 轮 · 文档与配置对账

`docs/compaction.md` 的 cooldown/hard-force 一节逐条对了配置字段名与语义：
`cooldown_fraction` / `cooldown_duration` / `hard_force_fraction` 三个键在
`internal/config::Config`、`config.example.yaml`、文档表格里完全一致，且文档明说
这三个**只对 mid-turn 生效**（pre-turn 的 `ctxcompact.MaybeCompact` 入参里根本没有
它们）。keepRecent 的两套量纲（pre-turn 对数 / mid-turn 消息数）文档也给了对照表。

零阻塞。

## 第 3 轮 · GOV8 正反探针

本轮两条条目都改写了 acceptance（`F2/CCL1` 与 `B0/TD1`），所以 `acceptancePins`
的探针格外重要：把 acceptance 改回旧文本、保留新 digest → `TestLedgerAcceptanceIsPinned`
变红。evidence 指向不存在的测试名 → 红。指向带 build 约束的测试作唯一证据 → 红。

零阻塞。

## 第 4 轮 · 最后一跳

**R4-1（已修）：`goalrun:<unix>` 记录零读取方。** `persistGoalRun` 自 G02 起就往 kv
里写 `RunRecord`，全仓**没有任何代码读回来过** —— 一条为运维写的记录，而运维没有
任何途径看到它。

acceptance 说的是「预算耗尽可靠停止并**把原因持久化**」，持久化本身是达成的，所以
这不推翻子句。但持久化的目的就是给人看，而 `StopReason` 恰恰是决定下一步的那个字段：
「跑完了」与「token 用光了」都叫「停了」，只有后者告诉你该不该加预算。

修复：`yanshi goal -history N` 列出最近 N 条，逐条打 tier / complete / stop_reason /
iterations / tokens。空库时明说「no goal runs recorded yet」——什么都不打印读起来像
命令坏了。变异探针（history 不读 `StopReason`）红。`-h` 文本变了，已重跑
`cmd/gendocs` 并提交结果。

## 第 5 轮 · 端到端

W2 的端到端在做的时候就已经补齐了，这轮复核确认覆盖闭合：

- **轻量路径的 token 真的进 sink**：`runLightweightGoal` → `QueryWithUsage` →
  `sink.Add`，探针（去掉 sink 调用 / `QueryWithUsage` 不收 usage）两个方向都红。
- **每个 LLM 组件各自上报**：不再只断言 loop 会停（那对「哪些组件上报」完全不敏感），
  而是逐个组件驱动 + 一条 `go/ast` 结构断言「任何持有 `model.BaseChatModel` 的类型
  必须同时持有 `*UsageSink`」。后者是给**还没写出来的组件**准备的。
- **T3/T4 真的产生结果**：`--fake-model` 那条自足路径断言 exitOK + `complete=true` +
  **跑到第 2 轮**；真路径断言无 agent CLI 时是 exitErr。

W4 的 hard-force 分支在做的时候补了正反双向（在冷却真被武装的前提下越过 / 在冷却
未到 hard-force 时不越过），探针四个方向全红。

零新增阻塞。

---

## 结论

五轮共 1 条阻塞（R4-1，已修并配探针）。W2 4/4、W4 2/2，**S0 台账 63/63**。

**本轮最值得记的两件事：**

1. **两条 acceptance 被改写了**（`F2/CCL1#3`「keepRecent 文档清晰」、`B0/TD1` 的
   5 条里有 2 条不是行为断言）。这类条目的共同点是：**代码可以是完美的，条目也永远
   出不了 partial** —— GOV8 的子句级握手对「文档清晰」「有测试守护」这类主张无从落笔。
   两次都按同一套动作处理：把主张翻译成同覆盖面的可断言形态、显式改写 `acceptancePins`、
   并用「改回旧文本 + 保留新 digest」的探针验证 pin 真的在把关。
2. **「写了但零读者」在本会话出现了第四次**（`ParseResultSections`、
   `visionUsageAccumulator`、`knownResultSections`，现在是 `goalrun:` 记录）。前三次
   都是内部符号，这次是**用户可见的产出**——记录写进了数据库，只是没人能看。形态
   完全一样：写入端完整、有测试、接线也在，缺的是消费端。

**未闭合（各有归属，已在台账逐条记名）：**

- `LLMTierer` 零生产调用点 → `-tier auto` 永远只走 `RuleTierer`，`LLMTierer.Sink`
  那条 G02 接线因此也不可达。接它需要 model，而 model 在 bootstrap 之后才有，
  `resolveGoalTier` 却要在 bootstrap 之前决定走哪条路 —— 设计问题，另开工作包
- `LifecycleMirror` 的状态变化不推送到 TUI → 需要 broker → WS 事件通道
- `PRAGMA foreign_keys` 从未开启，四张 `task_work_*` 表的 FK / CASCADE 全部不生效
- 出厂权限梯度倒置的剩余部分（`shell_start` 免提示 / `shell_list` 弹窗）→ W5
- 独立评审为零 → 需要 `CLAUDE_CODE_MAX_SUBAGENTS_PER_SESSION` 提额
