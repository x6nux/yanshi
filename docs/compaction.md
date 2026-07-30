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

## 携带式分块（保证不超窗口）

`RunSummary` 的核心承诺：**每次调用 summary 模型的输入严格 ≤ 模型窗口**。

- 当 summarize 集合总 token ≤ `ChunkThreshold × ModelWindow`（默认 0.9）时，走**单次 cache-aligned 调用**：`[原消息 verbatim..., 末尾指令]`。前缀和原对话逐字节一致，命中之前累积的前缀缓存。
- 否则走**携带式分块**（rolling summary）：按预算把 summarize 集合切成 chunk1, chunk2, …；串行压缩 `chunk1 → s1`，`[s1 作前缀, chunk2] → s2`，`[s2, chunk3] → s3` ……每块的预算 = `ModelWindow − carry(当前) − ack − instruction`。carry 每轮增长，预算跟着缩——这是动态的，不是固定 overhead（固定 overhead 会让大 carry 把后续块推过窗口边缘）。

chunk1 的前缀 = 原对话开头，命中前缀缓存；chunk2+ 的前缀变了不命中——这是「不超窗口」的必要代价（优于截断丢信息，也优于单次爆窗口）。切分点回退到「安全边界」（不在 tool_call↔result 配对中间切）。

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

上下文窗口是模型属性，不是全局值。`ProviderConfig.ContextWindow`（`config.yaml` 的 `llm.providers[].context_window`）按模型配置。查询走 `BuildProviders` 返回的 `windows` map——键是模型注册表键（`chooseKey`，优先 `p.Model` 如 "gpt-4o"），所以 `cs.model`/`req.Model` 能命中。`compaction.context_window` 是回退（provider 没配时用）。`/model` 切换自动用新窗口——因为 `CompactingModel` 按 model 指针缓存。

## 失败行为

summary 调用失败（重试耗尽）→ `Run` 返回 error，**绝不产出空 summary**（原始 bug⑥）。两条路径各自处理：

- **mid-turn**（`CompactingModel`）：回退原始 msgs（best-effort），真实调用可能仍能成；若已超窗口，真实错误浮出来给用户。
- **pre-turn**（`MaybeCompact`）：返回 `(原 msgs, before, before, false)`，WS handler 保留完整历史 + 不发 compacted 状态。

transient 错误（网络/429/超时）重试 3 次指数退避（1s/2s/4s）；permanent（401/400/解析）立即返回。`isTransient` 是第二道防线——生产环境 summary 模型通常是 `ResilientChatModel`，已过滤 4xx。

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
