# W1（装配线）完成后五轮评审 · 2026-08-07

W1 九条全部翻成 `done` 后，按 `docs/superpowers/review-checklist.md` 跑五轮。台账
在这轮结束时 53/63（84%）。

**独立评审仍为零。** 本会话 subagent 配额（200/200）自 W6 起始终耗尽，本轮开工前
又探过一次，仍是 `Subagent spawn limit reached`。以下五轮全部是主循环自评 ——
按清单的定义，「自评零阻塞」不等于「独立评审零阻塞」，这一条按 S0/W1 的教训必须
写在结论前面而不是脚注里。

---

## 第 1 轮 · 空壳测试与变异探针

| 探针 | 变异 | 预期 | 实测 |
|---|---|---|---|
| R1-A | `rlm.go` 上界判据 → `len(prompts) >= 1`（拒绝一切非空批） | 红 | **绿 → 阻塞** |
| R1-B | `imagestore.Put` 恒拒 | 红 | 红 |
| R1-D | `permctx.go` 两处 plan 门禁掏空 | 红 | 工具层红、WS 层绿 |
| R1-E | `runnerFor(model, false, …)`（plan 工具过滤失效） | 红 | 红 |

**阻塞 R1-1（已修）：** `C1/RLM1#1「1-16 并发」` 的唯一 evidence 是
`TestRLMQueryRejectsMoreThanSixteen` —— 一条只走拒绝方向的测试。把工具改成
「拒绝一切 n>=1」（即 `rlm_query` 永远不可用）后它仍然 PASS。子句命名的是一个
**区间**，两端都得走。

修复：新增 `internal/tools::TestRLMQueryAcceptsBothEndsOfTheRange`，对 n=1 与
n=16 各断言「不被拒 + 模型调用数 == n + 结果里 index 0..n-1 齐全」。后两条是防
静默截断的：一个把批量截到 1 条的实现同样会「成功返回」。两个变异探针复验均红。

**R1-D 是我的误判，撤销。** `TestChatWS_ModePlan_ProducesReadOnlyTurn` 在
permctx 门禁被掏空后仍绿，看起来像空壳；实际上它测的是**上游那道防线**（plan
runner 把 `fs_write` 从工具集里过滤掉），它甚至显式断言「不该出现 permission
denied」。R1-E 对准那道防线后当场变红。教训：探针的红绿要对着**测试自称覆盖
什么**去判，不是对着子句名。

## 第 2 轮 · 幻影名与文档虚报

实测 `DefaultOrchestratorProfile` 对 W1 涉及的每个工具的档位（探针文件用完即删）：

- **Allow：** `shell_start` `shell_read` `shell_wait` `shell_cancel`
  `shell_write_stdin` `agent_start` `agent_batch` `automation_*`（八个）
- **Prompt：** `update_plan` `image_describe` `shell_list` `rlm_query`

零阻塞。两点澄清：

1. `rlm_query` 的 Prompt 是**设计**——它是 `ConditionalProfileTools`，只在
   `batch.rlm_model` 指向 cheap provider 时注册，硬编进静态 allow 列表反而是幻影。
2. 拆解文档第 142 行把「出厂 profile 可达性天花板」列为影响 `A2/G05#2·#4`、
   `G/VISION#2`、`G/VISION-TOOL#2`。**复核结论：不否定这些子句。** Prompt 不是
   拒绝，用户批准即可用；`B1/M05` 当初被回退是因为 `agent_spawn` 实测
   `not permitted`（allowlist 未命中的**静默**档），与这里不是一回事。

**留给 W5 的观察（不是 W1 阻塞）：** 权限梯度是倒置的 —— `shell_start`（起进程）
免提示，而 `shell_list`（列出会话）、`update_plan`（改自己的待办）、
`image_describe`（读一张图）每次弹窗。一个每更新一次清单就要用户点一次批准的
agent，实用性上等于关掉了这个功能。改出厂 allow 列表是授权面变更，按 CLAUDE.md
的规矩要走工作包，不在本轮动。

## 第 3 轮 · GOV8 正反探针

六个方向逐个试，全部正确变红：

| 探针 | 变异 | 实测 |
|---|---|---|
| R3-A | evidence 指向不存在的测试名 | 红 |
| R3-B | 删掉一条子句的 evidence key | 红 |
| R3-C | 改 acceptance 不动 `acceptancePins` | 红 |
| R3-D | 删掉测试上的 `ledger:` marker | 红 |
| R3-E | 挂一个不存在条目的 marker | 红（点名 `B1/NOSUCH#2`） |
| R3-F | marker 里的子句原文改一个字 | 红（两条测试同时红） |

零阻塞。

## 第 4 轮 · 零消费者 / 能力位撒谎

**阻塞 R4-1（已修，且是我自己刚引入的）：** `ParseResultSections` 生产调用点为
零。本条目要修的缺陷正是「`knownResultSections` 只有一个消费者、四段无人读」——
结果我加的新解析器**自己也没有读者**，同一形态复制了一层。

修复：`ParentWorkingSetHint` 改走 `ParseResultSections`，并在结果**部分遵守契约**
时把缺了哪几段回报给父代理。三条既有 hint 测试全绿（含逐字透传那条），新增
`TestParentHintReportsAPartialContract` 钉住三种形态：部分 → 报缺段、完整 →
不报、无段（自由格式）→ 逐字透传。变异探针（退回只抽 EVIDENCE）红。

**能力位对账（R4-C/D）：零阻塞。** `CanKillTreeOnPlatform()` 三份实现的构建标签
与 `setProcessGroup` 完全对齐：`unix` → true 且真 `Setpgid`；`windows` 与
`!unix && !windows` → false，`killtree_other.go`（`!unix`）覆盖这两者。

## 第 5 轮 · 端到端可用性 / 证据证零件

**阻塞 R5-1（已修）：** `C1/AU1#2「按时触发入队」` 的唯一 evidence 是
`TestManagerTickEnqueuesDueSlotOnce` —— 它**手工调 Tick**。把 `internal/bootstrap`
的 `go scheduler.Start(parent)` 换成 no-op（整个定时循环消失）后它仍绿：子句里
「入队」有证据，「按时」一条都没有。

修复：新增 `internal/agent/automation::TestSchedulerEnqueuesWithoutAnyoneCallingTick`
—— 起 Scheduler 后**什么都不调**，等队列自己动；再断言 cancel 后 goroutine 退出
（「周期」的另一半是它得能停）。变异探针（`Start` 里不调 `Tick`）红，带 `-race` 绿。

**顺带修掉一条空壳（不在任何 evidence 里）：**
`TestBuildAutomationSchedulerGoroutineExitsOnCancel` 里那个
`for atomic.LoadInt32(&ticks) < 1` 循环计的是 **Create 的成功次数**，与 tick 无关，
第一次 Create 成功就退出 —— 配套那句 `t.Fatal("no tick observed")` 永远打不出来。
该测试的真实主体（cancel → goroutine 退出）是好的，删掉误导的循环并把去向写进
注释。清单只能加不能删，这条按清单的精神一并修了。

**A1/T07/T08#1 复核为强证据：** `TestShellV2EndToEndSpawnsRealProcess` 真起进程、
断言真 PID 非零、跨两个 ReAct 迭代（start → wait → read），并把两种装配失败
（`ShellManager` 没进 context、`shell.Config.Factory` 为 nil）写成了各自的失败信息。

---

## 结论

五轮共 3 条阻塞，全部已修并各配变异探针复验：

1. **R1-1** `C1/RLM1#1` 只有拒绝方向 → 补接受方向（区间两端 + 防截断）
2. **R4-1** `ParseResultSections` 零消费者 → 接进 `ParentWorkingSetHint`
3. **R5-1** `C1/AU1#2` 只覆盖「入队」不覆盖「按时」 → 补 Scheduler 自驱测试

三条是同一个形态的三种伪装：**证据证明了子句的一半，而那一半恰好是不会坏的那半。**
拒绝方向对一个恒拒的实现成立，解析器对一个没人调用的解析器成立，Tick 对一个从不
自己转的循环成立。

**未闭合（不阻塞 W1，各有归属）：**

- 出厂权限梯度倒置（`shell_start` 免提示 / `shell_list` 弹窗）→ W5 授权面
- 独立评审为零 → 需要 `CLAUDE_CODE_MAX_SUBAGENTS_PER_SESSION` 提额
