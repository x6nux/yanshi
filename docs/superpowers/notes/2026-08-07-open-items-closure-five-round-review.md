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

---

## 追加：第 5 轮的 J 清扫（同日，配额再次确认耗尽后）

R5-1 的教训是「修一跳只让缺陷往下游挪一格」。把它做成系统对账：**服务端能构造的
每一种 `ServerFrame` 类型 vs `applyEvent` 认得的每一个 case。**

33 种可构造类型，3 种无分支：

| 类型 | 判定 |
|---|---|
| `history_replaced` | **不是缺口** —— 只有 SSE 构造它，且在 `internal/cli/ssebackend.go` 里消费掉了 |
| `structured_result` | **不是缺口** —— 需要 `TurnOpts.OutputSchema`，TUI 从不设置（headless/SDK 路径） |
| `subagent_event` | **阻塞（已修）** |

**阻塞 J-1（已修）：`subagent_event` 在整个客户端零处理。** `StreamEvent` 连字段都没有，
`toStreamEvent` 全丢，`applyEvent` 无分支。两个传输各有一条**服务端**测试证明帧发出去了
（`internal/api/http::TestChatWS_ForwardsTypedSubagentEvent`、
`internal/api/http::TestChatSSE_ForwardsTypedSubagentEventWithSingleWriter`），没有一条
检查有人收到。用户 `agent_spawn` 之后到父 turn 结束之间**什么都看不到**。

**这一条最值得记的不是缺陷，是门禁为什么没拦住它。**
`internal/cli::TestToStreamEventCarriesEveryServerFrameField` 一直存在，而且是一道好门禁 ——
它在我改动的当场就红了。但那四个字段躺在它的 `streamEventNotCarried` 表里，理由写的是
「TUI 从 tool block 渲染 subagent 生命周期，不用这些 id」。

这条理由**对生命周期从来就不成立**：tool block 显示的是 `agent_start` 的最终结果，
而 relay 存在的理由正是 `agent_spawn` —— 它 fire-and-forget，返回时什么都还没发生。
它只是**恰好无害**，因为 `agent_spawn` 不在出厂 allow 列表里，几乎没人能产出那些被丢弃的帧。
**而这个会话把它加进去了。**

门禁验证「声明不透传的字段确实没透传」，它**验证不了理由**，而理由正是让这张表成为
「决策」而非「清单」的那部分。于是门禁忠实地执行了一条假前提，前提的失效日
（profile 放宽那天）静默地过去了。

修法：四个字段转为 carried，`streamEventNotCarried` 清空并留下这段实例；新增
类型级门禁 `internal/cli/tui::TestEveryServerFrameTypeIsRenderedOrDeclared`
（四正一反探针全部正确）。它的两条豁免**都写成可达性主张**（帧到不了这个 switch），
这是唯一一种不会因为「有人放宽了 profile」而腐烂的理由形态。

**探针纪律又被打了一次（同日第四次，新变体）：** 探针对象是**新增文件**，
`git add` 过了、`git status` 看起来正常，而 `git checkout HEAD -- <新文件>` 报
`pathspec did not match`，**还原静默失败**，紧接着的反向探针于是红了、读起来像门禁误伤。
已写进清单 0-ter。

---

## 追加二：请求方向的对账（同日）

J 清扫做完响应方向后，按同一形态查 CLAUDE.md 点名的另一条陷阱：**请求方向不共享词表** ——
`proto.ClientFrame`（仅 WS）、`chat.go` 里的匿名结构体（SSE）、`v1.TurnStartParams` 三套，
`json.Decode` 静默忽略未知键，所以往一套里加字段对另两套等于**沉默**。图像附件 POST 给 SSE
就是这么消失的，CLAUDE.md 写着这条陷阱，**而没有任何机器判据**。

**实测唯一分歧：v1 缺 `Attachments`。判为不阻塞** —— `resolveAttachments` 需要 workRoot 与
permission profile 才能校验路径，而 `internal/api/v1.Service` 两者都没有；`sdk/` 与 `docs/api/`
也都没有宣传过它。按清单 J 的分流规则，这类流向后续工作包而不是本轮阻塞。

新增门禁 `internal/api/http::TestEveryClientFrameTurnInputFieldReachesEveryTransport`：
每个 `ClientFrame` 字段必须被分类为 turn-input（并给出另两套结构体各自的目标字段名）
或 control-only；缺席必须附**结构性理由**。

**门禁自己的两个探针没红，都是真洞：**

- **P1 无效探针（我自己犯 0-ter 次级规则）：** 第一版探针改名了 `chat.go` 引用着的字段，
  包直接编译不过。红是红了，红的是编译器不是断言。改成「把声明的目标指向一个结构体里
  不存在的名字」才拿到真正的断言失败。
- **P5 真洞（已修）：** 把 `Attachments` 真的加进 `v1.TurnStartParams`、而表里仍写着「缺席」，
  门禁**保持绿**，那段断言「这个字段在 v1 上不可能工作」的理由继续活着并且是错的。
  **这正是这道门禁被写出来要防的失效模式，在门禁自己身上复现了一次。**
  已加断言：声明缺席的字段必须在目标结构体里确实不存在。

同一天第三次撞上同一个道理：**豁免/缺席条目的理由没有任何东西在验，除非你专门写一条断言去验。**

**未闭合：**

- 独立评审为零 → 需要 `CLAUDE_CODE_MAX_SUBAGENTS_PER_SESSION` 提额。本轮已在会话内
  实际调用 Agent 工具验证过，仍是 `200 of 200`
