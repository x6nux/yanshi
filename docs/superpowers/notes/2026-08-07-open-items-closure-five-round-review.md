# 四个未闭合项收口后五轮评审 · 2026-08-07

本轮修的是 Stop hook 点名的四项（出厂权限梯度、`PRAGMA foreign_keys`、
`LLMTierer` 零调用点、`LifecycleMirror` 不推 TUI），对应 `2dffc20`..`5ecbc74`
六个提交。台账在这轮结束时仍是 **63/63**（本轮不新增台账条目 —— 修的都不是
acceptance 子句，详见第 3 轮）。

**独立评审仍为零。** 本轮开工前**在本会话内实际调用了一次 Agent 工具**，返回
`Subagent spawn limit reached (200 of 200 agents spawned)`。以下五轮全部是主循环
自评；按清单的定义，「自评零阻塞」不等于「独立评审零阻塞」。

---

## 第 1 轮 · 变异测试

| 探针 | 变异 | 预期 | 实测 |
|---|---|---|---|
| R1-A | DSN 去掉 fk，改为在 `*sql.DB` 上单条 Exec | 红 | 红（点名 connection 1） |
| R1-B | `deleteBypassingForeignKeys` 只关 pragma 不执行 query | 红 | 四条全红 |
| R1-C-1 | 调用点搬到 `if !forced` 外面 | 红 | 红 |
| R1-C-2 | 调用点保留，返回值改成 `_ =` | 红 | **绿 → 阻塞** |
| R1-D | mirror 把 running 也报成 completed | 红 | 红 |
| R1-E | 广播注销不删 map 条目 | 红 | 红（但见下） |

**阻塞 R1-1（已修）：`go/ast` 调用点断言只证明了「调用发生」。**
`_ = refineTierWithModel(...)` —— 分类器跑了、花了一次模型调用、答案被丢弃 ——
仍然绿。`-tier auto` 于是悄悄退回 RuleTierer，而门禁是绿的。这正是清单 D4
「spy 只证明调用发生」的形态，出现在**我自己为了防这个形态而写的那条断言里**。
改成要求 `resolvedTier = refineTierWithModel(...)` 的赋值形态，复验红。

**阻塞 R1-2（已修）：`broadcast_test` 的注释描述了一个走不到的机制。**
注释声称 nil `*websocket.Conn` 是「注销坏了就 panic」的兜底；实测 `require.Equal`
在计数那一步就中止了测试，`Broadcast` 根本没跑到。代码是对的、注释是错的 ——
与 W3 那轮抓到的三条同形态（`writeMu` / `Close` checkpoint / DualOpen）。
注释改成实测结论。

## 第 2 轮 · 幻影名与出厂可达性

按本轮新加的清单 **B2**（探针输入必须来自注册表，不能手写）做：遍历
`app.ToolNames` 反查档位，而不是手写工具名。

```
registered=78  registered-but-NOT-allowed(3)=[revert_turn rlm_query screenshot]
phantom-in-allow(0)=[]
```

三个未授权的正是 `profile.go` 里逐条写了理由的故意排除项；allow 列表零幻影。
零阻塞。

## 第 3 轮 · GOV8 / GOV9 正反探针

- evidence 指向不存在的测试名 → 红（GOV8 与 GOV9 同时红）
- 给本轮新测试挂一条不属于它的 `ledger:` marker（`A2/DT1#2 状态机正确`）→ 红
- 活文档里造一个死符号引用 → 红
- 干净树 → 全绿

**本轮不给任何新测试挂 `ledger:` marker，这是刻意的。** 广播测试证明的是
「状态变化对用户可见」，而 `A2/DT1#2` 说的是「状态机正确」；把它挂上去会让
子句级握手读起来像是「状态机正确性由一条广播断言证明」。同理，权限梯度测试
关掉的是评审发现，不是 acceptance 子句。零阻塞。

## 第 4 轮 · 零消费者 / 供给端为 nil

本轮把「写了但零读者」的**第二个子形态**做成了可复用扫描：结构体里 func 类型的
导出字段，反查**谁给它赋值**（而不是谁读它）。`EmitWorkFrame` 就是这么漏掉的 ——
它带 `if != nil` 门禁，读起来像可选特性。

全仓 336 个非测试文件、10 个这类字段，逐个查生产赋值点，唯一为 0 的是
`internal/lsp/manager.go` 的 `Config.Dial`，而它的 doc 注释明写「生产留 nil（走 exec）；
测试注入」—— 声明过的测试 seam，不是同形态。零阻塞。

本轮新增的导出符号全部有生产消费者。

## 第 5 轮 · 端到端（证据证零件 vs 成品）

**阻塞 R5-1（已修）：`task_update` 帧到得了客户端，TUI 不认。**
服务端广播每一次 durable 状态变化，WS backend 也已经把 `ServerFrame.Task` 透传成
`StreamEvent.Task` —— 而 `applyEvent` 的 switch 里没有 `task_update` 分支，帧到了
就被丢弃。**我把广播接通，却停在了同一堵墙前一格**：这正是 A2 时期 `plan_update`
卡住的那堵墙，往下游挪了一跳。

**阻塞 R5-2（已修）：三个渲染器写好、有测试、`applyEvent` 一个都没用。**
`taskUpdateEntry` / `planUpdateEntry` / `checklistUpdateEntry` 的**唯一**引用是
`var _ entry = X{}` —— 一条编译期接口断言。这是本会话第七次「写了但不可达」，
也是第一次伪装成**接口断言**而不是 nil 字段：`var _ entry` 会让死渲染器一直编译，
读起来像有人在用。

修法分两种：`taskUpdateEntry` 是为这个帧写的，接进新分支用活；
`planUpdateEntry`/`checklistUpdateEntry` 已被 `checklistEntry` 取代（后者多两条性质：
去掉样式后仍可读的字形、以及拒绝把未知状态渲染成 done），**删掉**而不是接线。

---

## 结论

五轮共 4 条阻塞，全部已修并各配变异探针复验：

1. **R1-1** 调用点断言只证明调用发生，不证明结果被采纳
2. **R1-2** 注释描述了一个走不到的机制
3. **R5-1** `task_update` 到了 TUI 被丢弃
4. **R5-2** 三个渲染器被 `var _ entry` 伪装成活的

**本轮最值得记的一件事：清单里的实例段落会被当成历史故事读，不会被当成待办读。**
`EmitWorkFrame` 零生产赋值点这件事，**第 22 轮的评审清单里就白纸黑字写着**，
`docs/superpowers/acceptance-breakdown.md` 里也写着。两跳里的第二跳（TUI 分支）在
A2 收尾时修了，第一跳没修，而它就躺在一份每轮必读的文件里。四个月后另一轮评审
把它当作**新发现**重新挖了一遍。

教训不是「要更仔细读清单」——那不可执行。教训是：**清单实例在描述一个未修复的缺陷时，
必须同时在台账或工作包里留一个可跟踪的条目**。已写进清单 J 段那条实例。

**未闭合：**

- 独立评审为零 → 需要 `CLAUDE_CODE_MAX_SUBAGENTS_PER_SESSION` 提额。本轮已在会话内
  实际调用 Agent 工具验证过，仍是 `200 of 200`
