# 上下文压缩（Context Compaction）

yanshi 的对话历史会在接近模型上下文窗口时被自动压缩：旧消息折叠进一段 summary，尾部与关键消息保留原文。本文档解释压缩的架构、保真策略、分块机制，以及它修复的 8 个原始 bug。改 `internal/ctxcompact/` 或两条接入路径（`einollm.CompactingModel` / `ctxcompact.MaybeCompact`）前先读本文。

## 两条路径，一个核心

压缩有两条触发路径，都委托同一个核心：

- **mid-turn**（`internal/llm/eino/compacting.go` 的 `CompactingModel`）：在 ReAct 循环的*迭代之间*触发。ADK 每次迭代都把完整历史传给 `Generate`/`Stream`，`CompactingModel` 拦截它——所以这里的压缩发生在迭代之间，不只是 turn 边界。
- **pre-turn**（`internal/ctxcompact/compact.go` 的 `MaybeCompact`）：WS/SSE handler 在 `user_message` turn 之前触发。

两者都调用 `ctxcompact.Run`——这是唯一的压缩执行点。核心是一个纯流水线：

```
msgs ──► Plan() ──► {pinned, summarize, workingSetPaths}
                       │           │
                       │           └─► SerializeForSummary() ──► transcript
                       │                                          │
                       │            ┌─────────────────────────────┘
                       │            ▼
                       │       RunSummary()
                       │         ├─ 总 token ≤ 0.9×窗口 → cache-aligned 单次
                       │         └─ 否则 → 携带式分块（chunk1→s1→chunk2→s2→…→sN）
                       │            │
                       ▼            ▼
                  EnforceToolCallPairs()  fixpoint 修正 pinned
                       │
                       ▼
                  Assemble() ──► [pinned 原文..., summary 作 user 末尾]
```

`Plan` 是纯函数（无 IO）；`RunSummary` 是唯一调用模型的地方；`Assemble` 是纯拼装。这让两条路径只在「模型从哪来」分叉（mid-turn 用 `CompactingModel.Inner`，pre-turn 用配置的 summary 模型或当前 session 模型）。

## 保真 pin（什么保留原文）

`Plan` 把消息分成 `pinned`（原文保留）和 `summarize`（折进 summary）。一条消息满足**任一**即进 pinned：

1. **尾部 `KeepRecent` 对**——当前即时上下文。
2. **所有 `Role==User` 的非 tool-result 消息**（codex 风格）——用户意图永不丢。
3. **提到 working-set 路径**的消息——working-set 是从最近 12 条消息 + tool 参数提取的文件路径；正在编辑的文件上下文不丢。
4. **错误标记**（`error:`/`failed`/`panic`/`traceback`/`test failed`）——调试现场不丢。
5. **diff/patch 标记**（`diff --git`/`+++ b/`/`apply_patch`）——编辑变更不丢。

只有过程性的 assistant 工具交互会进 summarize——而这些的关键事实（路径、命令、错误）要么被 pin 兜底，要么 summary 模型被明确要求保留。

## 配对修复（防 API 400）

`EnforceToolCallPairs` 是一个 fixpoint 循环：对 pinned 集合里的每个 tool_call，确保它对应的 tool_result 也在 pinned（反之同理）。孤立的（对面不存在）从 pinned 移除。这防止压缩后的历史交给 API 一个没有配对 tool_call 的 tool_result——OpenAI/Anthropic 都会拒绝（400）。算法用 `permanently_removed` 集合防止级联移除时的振荡。

这一步同时保证 summarize 集合内部配对完整——summarize = 全集 − pinned，pinned 配对完整意味着切走的 tool 交互要么成对留在 pinned，要么成对进 summarize。

## 结构化序列化

`SerializeForSummary` 把 summarize 集合展平成给 summary 模型看的文本 transcript。它**不**只写 `Role: Content`（那会丢失工具交互的全部结构），而是保留：

- `ToolCalls`：`[tool_call: <name> id=<id>] <args_json>`
- tool_result：`[tool_result: <id>] <content 截断到 1200 字符>`
- `ReasoningContent`：`[thinking] ...`

没有这些，summary 模型根本看不到工具做了什么（原始 bug①）。截断按 rune 边界（多字节安全，yanshi 中文环境必要）。

## 携带式分块（把每次调用压回窗口量级）

`RunSummary` 的核心承诺，以及它的**例外**：

> 每次调用 summary 模型的输入**按窗口预算切分**，多数情况下 ≤ 模型窗口；
> 但超出量**对窗口无界** —— 真实上界是
> **窗口 + 「历史中最大不可分割段」**，后者是输入的属性，不是窗口的倍数。

「不可分割段」有两个来源，它们**都不是**「配对完整性」这一个：

1. **单条超大消息，完全不涉及配对。** `takeChunk` 的判据是
   `if i > 0 && tok+mt > budget && splitIsSafe(msgs, i)` —— `i == 0` 不做预算检查。这不是疏忽：
   chunk 为空会让 `RunSummary` 的 carry 循环永远推进不了（doc 自己写着 *"never returns an empty
   chunk"*）。所以一条比窗口还大的消息**自己一个人**就能超窗。
2. **并行 tool_call 组。** `splitIsSafe(msgs, i)` 的右侧判据扫 `j` 从 0 到 `i-1`（**整个左半边**），
   于是 `[call(id1..idN), r1..rN]` 的**每一个内部切点都 unsafe**，整串必进同一 chunk。上界因此是
   「一个 tool_call 消息 + 它全部 result 之和」，**随并行工具数线性增长**。
   并行工具调用是本仓的常规形状 —— `classify.go::emitAssistant` 就是 `for _, tc := range msg.ToolCalls`。

实测（budget=1000）由下面这条命令**现场打印**，本文不写死数字 —— 这里原先摆着一张四行的表，
和它下面那个 24.15× 不同（那个给了复现命令），四个数没有任何复核或防腐手段：

```sh
go test ./internal/ctxcompact -run TestTakeChunk_OvershootShapesAreMeasured -v
```

输出逐行给出 n=1 / n=5 / n=20 并行 result 与「单条超大消息（无配对）」四种形状的 chunk tokens 与比值。
测试**断言的是形状不是数字**（比值随并行工具数单调增长；超大消息在完全没有配对的情况下自己超窗），
数字随 fixture 变化，正是原来那张表腐烂的方式。

**为什么不给 `takeChunk` 加硬上限（取舍）**：最小合法 chunk 就是「剩余消息头部的那个不可分割段」，
它的大小是**输入**的属性。想压到它以下，只能二选一 —— 切断配对（provider 直接 400，而整条 carry
循环存在的理由正是避免这个），或者在 chunk 中途截断消息正文（静默丢信息，且要再引入一套截断预算）。
两者都比「一次偏大但格式合法的调用」更糟。所以这个界**被如实记录，而不是被强制**。

`TestProperty_EachSummaryCallWithinWindow` 钉的是上面那个真实上界：断言
`tok ≤ ModelWindow + maxAtomicGroupTokens(msgs)`，其中 `maxAtomicGroupTokens` 在测试里**独立于
`splitIsSafe` 重写**（共用 helper 的 oracle 会跟被测代码同步退化）。它还要求整轮扫描中**至少有一次
调用超过 2× 窗口** —— 那正是旧断言的阈值，用它当下限就等于持续证明旧界是假的。生成器
`genAdversarialHistory` 负责产出上面两种形状；`genHistory` 单独跑时最多到 ~1.01×，这正是
「< 2×」能在无人察觉中活这么久的原因。跑
`go test ./internal/ctxcompact -run TestProperty_EachSummaryCallWithinWindow -v`
能看到实测超出（如 `call[0] tok=4805 exceeds window=800 by 6.01x`，最高观测到 24.15×）。

- 当 summarize 集合总 token ≤ `ChunkThreshold × ModelWindow`（默认 0.9）时，走**单次 cache-aligned 调用**：`[原消息 verbatim..., 末尾指令]`。前缀和原对话逐字节一致，命中之前累积的前缀缓存。
- 否则走**携带式分块**（rolling summary）：按预算把 summarize 集合切成 chunk1, chunk2, …；串行压缩 `chunk1 → s1`，`[s1 作前缀, chunk2] → s2`，`[s2, chunk3] → s3` ……每块的预算 = `ModelWindow − carry(当前) − ack − instruction`。carry 每轮增长，预算跟着缩——这是动态的，不是固定 overhead（固定 overhead 会让大 carry 把后续块推过窗口边缘）。

chunk1 的前缀 = 原对话开头，命中前缀缓存；chunk2+ 的前缀变了不命中——**丢缓存**才是分块的代价（优于截断丢信息，也优于单次爆窗口）。

切分点回退到「安全边界」（不在 tool_call↔result 配对中间切）——注意因果方向：**安全边界回退不是「不超窗口」的代价，它正是超窗的机制之一**。预算说该切，配对说不能切，配对赢，于是这个 chunk 超预算。另一个机制是 `i == 0` 不做预算检查（见上文），它连配对都不需要。

## summary 载体（修双 system 冲突）

summary 作为 `Role: User` + `SummarySentinel`（`"[yanshi:conversation-summary]\n"`）前缀的消息放在历史**末尾**。

- **不用 System 角色**——编排器已有 system prompt（含 guard profile、技能、VCS scope），双 system 会被 OpenAI 拒绝或挤掉原 system。
- **放末尾**——pinned 消息成为稳定前缀，后续 turn 新消息追加在 summary 之后，前缀不变，持续命中前缀缓存。
- **sentinel**——`Plan` 检测历史已以 summary 结尾时短路（防 summary 的 summary）；TUI 渲染时跳过 summary 消息（它是模型上下文，不是对话内容）。

## 前缀缓存优化

两层：

1. **summary 调用本身命中缓存**（单次模式）：summary 请求的前缀 = 原对话开头，逐字节稳定。OpenAI 自动缓存；Anthropic 在最后一条 summarize 消息打 `cache_control: ephemeral`。
2. **压缩后历史前缀稳定**：产物 = `[pinned 原文..., summary 末尾]`，后续 turn 追加在 summary 之后，pinned 前缀不变。

## 按模型窗口配置

上下文窗口是模型属性，不是全局值。`ProviderConfig.ContextWindow`（`config.yaml` 的 `llm.providers[].context_window`）按模型配置。查询走 `BuildProviders` 返回的 `windows` map——键是模型注册表键（`chooseKey`，优先 `p.Model` 如 "gpt-4o"），所以 `cs.model`/`req.Model` 能命中。`compaction.context_window` 是回退（provider 没配时用）。`/model` 切换自动用新窗口，但**两条路径的机制不同，别把其中一条的解释套到另一条上**：

- **pre-turn / WS / `/compact`**：handler 自己拿 `cs.model`/`req.Model` 去查 `windows` map。
- **mid-turn**：`runnerFor` 拿本轮 `TurnOpts.ModelID` 查 `CompactionConfig.ProviderWindows`
  （bootstrap 传入的同一张表），把解析出的窗口交给 `wrapCompaction`，三个门由它算出。

「因为 `CompactingModel` 按 model 指针缓存」曾被写在这里当作 mid-turn 的解释，**那是错的**：
按指针缓存确实会为新模型建一个新实例，但在 W4 之前 `wrapCompaction` 给新实例填的
`ContextWindow` 仍是同一个全局回退值 —— **换了实例，没换窗口**。128K 的 provider 会拿到按
256K 算的 threshold，即自身容量的 1.9 倍，门永不触发，压缩对它等于不存在。修复见 ADR-0013
所属的 W4 工作包（`internal/agent/orchestrator::CompactionConfig.windowFor`）。

## 失败行为

summary 调用失败（重试耗尽）→ `Run` 返回 error，**绝不产出空 summary**（原始 bug⑥）。两条路径各自处理：

- **mid-turn**（`CompactingModel`）：回退原始 msgs（best-effort），真实调用可能仍能成；若已超窗口，真实错误浮出来给用户。
- **pre-turn**（`MaybeCompact`）：返回 `(原 msgs, before, before, false)`，WS handler 保留完整历史 + 不发 compacted 状态。

transient 错误（网络/429/超时）最多 **3 次尝试 / 2 次重试**，退避 1s、2s（`internal/ctxcompact/summarize.go::summaryRetryBackoffs`，由 `summaryRetryMax`/`summaryRetryBaseMs` 推出，`internal/ctxcompact/summarize_internal_test.go::TestSummaryRetryBackoffSequence` 钉住取值，`internal/ctxcompact/summarize_internal_test.go::TestCallWithRetrySleepsTheDerivedBackoffs` 钉住 `callWithRetry` 确实按它睡 —— 前者只证明函数**算得对**，若有人把公式内联回循环、这个指针就指向一个不再驱动行为的值）——一次耗尽最多睡 3s。此前这里写的是「重试 3 次（1s/2s/4s）」，两处都错：3 是**尝试数**不是重试数，而 4s 那一档因为末次尝试后不再 sleep 而**任何路径都到不了**，照它估算最坏耗时会高估一倍。permanent（401/400/解析）立即返回。`isTransient` 是第二道防线——生产环境 summary 模型通常是 `ResilientChatModel`，已过滤 4xx。

## TUI 呈现

压缩是元操作，状态只走 TUI 的 **activity line**（`m.activity`，即 `"✢ Running…"` 那一行，渲染在 transcript 之外），不进 transcript（原始 bug⑧）：

- 进行中：`m.activity = "Compacting context…"`
- 完成：activity 切回 `"Thinking…"`，token 缩减通过 header 的 ctx 计数体现
- summary 消息（user+sentinel）在 `session_restored` 渲染时跳过——它是模型上下文，不是对话气泡

## 配置参考

```yaml
llm:
  providers:
    - name: "openai"
      model: "gpt-4o"
      context_window: 128000      # 该模型的 token 窗口; compaction 按此判定阈值与分块
compaction:
  threshold: 0.8        # 对话总 token / 窗口 >= 此值时触发压缩
  keep_recent: 4        # 尾部保留的 user/assistant 对数
  context_window: 256000 # 回退窗口: provider 未配 context_window 时用
  model: ""             # 专用 summary 模型; empty = 当前 session model
  chunk_threshold: 0.9  # summary 输入 / 窗口 >= 此值时走携带式分块
```

## 文件清单

`internal/ctxcompact/`（核心包，两条路径共用）：

| 文件 | 职责 |
|---|---|
| `tokens.go` | `EstimateTokens`（计 Content + ReasoningContent + ToolCalls） |
| `sentinel.go` | `SummarySentinel` + `IsSummaryMessage` |
| `serialize.go` | `SerializeForSummary`（结构化 transcript） |
| `preserve.go` | working-set/error/diff pin 维度 |
| `pairs.go` | `EnforceToolCallPairs`（fixpoint） |
| `options.go` | `PlanOpts`/`RunOpts`/`ModelSummarizer`/`Result` 类型 |
| `plan.go` | `Plan`（pin 五类 + pairs + sentinel 短路） |
| `summarize.go` | `RunSummary`（cache-aligned + 携带式分块 + 重试） |
| `assemble.go` | `Assemble`（pinned + summary 末尾 user+sentinel） |
| `run.go` | `Run`（统一入口；失败返回 error） |
| `compact.go` | `MaybeCompact`（pre-turn 入口）+ `ForceCompact`（手动 /compact） |

接入点：`internal/llm/eino/compacting.go`（mid-turn）、`internal/api/http/ws.go` + `chat.go`（pre-turn 调用）、`internal/cli/tui/`（activity line 呈现）。


## 三个门与它们的量纲

> ⚠️ **这三个门只存在于 mid-turn 路径。** `apihttp.CompactionConfig` 里没有
> `Cooldown*` / `HardForce*` 任何字段，`ctxcompact.MaybeCompact` 的入参只有
> threshold / window / keepRecent —— **pre-turn 每次越过阈值就压，没有 cooldown 兜着，
> 也没有 hard-force 可言（它本就不受 cooldown 阻挡）**。下面这张表描述的是 mid-turn。

`config.yaml` 的 `compaction` 一节暴露了三个独立的门，它们对**同一个未压缩历史**求值，
但回答的是不同问题：

| 门 | 键 | 判据 | 作用 |
|---|---|---|---|
| threshold | `threshold` | `tokens >= threshold × window` | 常规触发线。**达到即触发**，不是「严格超过」 |
| hard force | `hard_force_fraction` | `tokens >= fraction × window` | 逼近窗口边缘时**越过 cooldown** 强制压缩 |
| cooldown | `cooldown_fraction` / `cooldown_duration` | 增长量 < `fraction × window`，或距上次不足时长 | 阻止「历史没怎么变却反复压缩」 |

三条要点，每一条都曾经出过错：

1. **cooldown 的「增长量」是未压缩量纲的差值** —— 记的是「上次触发压缩时历史有多大」，
   不是「压缩后剩多大」。理由与不可违反的约束见 [ADR-0013](adr/0013-mid-turn-compaction-token-dimension.md)。
2. **hard force 必须能越过 cooldown**。否则 cooldown 会在最该压缩的时刻赢：压缩被推迟、
   历史继续涨、下一次调用拿到一个装不进窗口的历史。
3. **`cooldown_fraction` 配 0 = 关闭 token 维**，不是「阈值为 0 所以永远满足」。
   实现里那个 `> 0` 判断是承重的：去掉它，历史一旦缩小（压缩之后正是如此）
   差值为负，会把一个已被运维关闭的 cooldown 重新武装起来。

`window` 是**本轮模型**的窗口（见上一节），不是全局回退值。


## ⚠️ `keep_recent` 的单位在两条路径上不同

`compaction.keep_recent` 被两条路径共用，**但它们对这个数的解释不一样**：

| 路径 | 接收方 | 单位 | `keep_recent: 4` 实际钉住 |
|---|---|---|---|
| pre-turn（WS / `/compact` / SSE） | `ctxcompact.PlanOpts.KeepRecent` | **对数** | 8 条消息 |
| mid-turn（`CompactingModel`） | `CompactingModel.KeepRecent` | **消息数**，内部 `/2` 转对数 | 4 条消息 |

同一个配置值在一条路上保留的量是另一条的两倍。这不是设计意图，是两条路径各自演进的结果：
`CompactingModel` 以消息数为单位、由 `/2` 桥接到 `Plan`（见 ADR-0006），而 pre-turn 的
handler 直接把配置值当对数交给 `MaybeCompact`。

单位分歧有**两个可观察后果**，不止一个：

1. **保留量**：pre-turn 钉 `2 × keep_recent` 条，mid-turn 钉 `keep_recent` 条。
2. **最短历史门**：pre-turn 在 `len(msgs) <= keep_recent*2+1` 时整个跳过压缩，
   mid-turn 在 `len(msgs) <= keep_recent` 时跳过。`keep_recent: 4` 下，一段 6 条消息的
   历史 mid-turn 会压、pre-turn 不会 —— 两条路径对「这段历史值不值得压」的判断本身就分岔。

`threshold` 的判据本身两条路**完全一致**（`tokens < int(threshold*window)` 逐字相同），
所以分岔全部来自 `keep_recent` 这一个键。

**目前只做记录，未统一。** 统一需要选一个单位并改另一条路，那会改变现网行为
（其中一条路径的保留量会翻倍或减半），应当单独立项并配 ADR，而不是夹在一次测试补全里。
发现于 W4 review 第 7 轮，第二个后果补记于第 14 轮。


## ⚠️ `compaction.model` 只在 pre-turn 生效

`compaction.model` 让操作员指定一个便宜的快速模型专做摘要。它**只被 pre-turn 路径消费**：
bootstrap 把它传给 `apihttp.CompactionConfig`，handler 经 `compactionModel(...)` 解析。

**mid-turn 拿不到它。** `orchestrator.CompactionConfig` 里没有这个字段，
`CompactingModel.maybeCompact` 把 `c.Inner`（当前会话模型）直接当摘要器用。所以配了这个键
之后，pre-turn 的摘要走廉价模型，mid-turn 的摘要仍然烧主模型 —— 没有任何提示。

与 `keep_recent` 的单位分歧是同一类问题：**一个配置键被两条路径共用，而只有一条真的读它**。
两条路径各自的测试都看不见这种缺陷，它只存在于「同一个键」这个连接点上。

**目前只做记录，未修。** 让 mid-turn 也支持它需要把摘要模型解析后经 `CompactionConfig`
传进 `wrapCompaction`（与 W4 处理 `ProviderWindows` 的路子相同），属功能变更，应单独立项。
发现于 W4 review 第 8 轮。


## ⚠️ `chunk_threshold` 目前是死配置

`compaction.chunk_threshold` 有 yaml 标签、有默认值（`applyDefaults` 填 0.9）、有一条断言
默认值的测试 —— 但**没有任何生产代码读它**。三个消费点全部硬编码：

- `internal/llm/eino/compacting.go` 的 `maybeCompact`：`RunOpts{… ChunkThreshold: 0.9}`
- `internal/ctxcompact/compact.go` 的 `MaybeCompact` 与 `ForceCompact`：同样写死 0.9

配 `chunk_threshold: 0.5` 得到的仍是 0.9。那条测试断言的是**默认值**，所以有没有人读它
都会绿 —— 它反而让这个死键看起来是活的。

[ADR-0006](adr/0006-compaction-two-paths.md) 里「由 `chunk_threshold`（默认 0.9）控制」
这句因此不成立。

**修法是接线而非删键**：把它经两个 `CompactionConfig` 传到三个调用点，路子与 W4 处理
`ProviderWindows` 完全相同。这个改动是安全的 —— 当前所有部署实际都在用 0.9，接线后未设该键
的仍得 0.9，只有显式设过别的值的人会看到变化，而他们本来就以为自己设过了。
发现于 W4 review 第 10 轮。
