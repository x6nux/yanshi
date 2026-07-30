# 上下文压缩重写设计（Context Compaction Rewrite）

日期：2026-07-19
状态：设计已确认，待写实现计划

## 背景

yanshi 有两条上下文压缩路径，共享同一类设计缺陷，在 ReAct agent（工具密集）场景下严重丢失关键信息、甚至触发 API 拒绝：

- **mid-turn**：`internal/llm/eino/compacting.go` 的 `CompactingModel`（在 ReAct 迭代之间压缩）
- **pre-turn**：`internal/ctxcompact/compact.go` 的 `MaybeCompact`（在 user_message turn 之前压缩，被 WS handler 调用）

### 现有 bug 清单

**① 序列化丢失全部结构化信息（致命）**
两条路径的 `serializeMessages`/`serialize` 只读 `m.Content`：
```go
b.WriteString(string(m.Role)); b.WriteString(": "); b.WriteString(m.Content)
```
`ToolCalls`（工具名+参数+ID）、`ToolCallID`+`Role==Tool`（工具结果）、`ReasoningContent`（思考链）全部丢失。ReAct 循环里绝大部分 token 在工具调用/结果上——等于把对话真正的内容都丢了。

**② 切分点切断 tool_call ↔ tool_result 配对**
`split := len(msgs) - KeepRecent` 一刀切尾部。若切口落在「assistant 发起 tool_call」与「对应 tool 结果」之间，压缩后 `recent` 出现孤立 tool_result（无产生它的 tool_call）——OpenAI/Anthropic API 直接 400。

**③ 无保真层级**
一锤子把所有 older 压成自然语言 summary，关键决策/文件路径/错误/未完成任务只能"祈祷"模型提到。

**④ System 消息冲突**
压缩产物是 `Role: System` 的 summary。编排器已有 system prompt（含 guard profile、技能、VCS scope）——双 system（OpenAI 拒绝）或原 system 被挤掉。

**⑤ token 估算漏算工具**
`len(Content)/4 + 8` 不计 ToolCalls 的 arguments JSON、不计 reasoning。工具密集 ReAct 循环严重低估，threshold 永不触发或触发已太晚。

**⑥ 失败路径吞信息**
`ctxcompact` summarize 出错时返回空 summary，产物变成 `"Conversation summary so far:\n"`，信息全丢。

**⑦ 双路径无去重**
mid-turn 和 pre-turn 独立触发，可能同一 turn 双压缩，产生 summary 的 summary。

**⑧ 压缩状态污染对话流**
`compact_chunk` delta 累积到 `compactEntry`——一个 transcript 条目（消息流的一部分），渲染成 `↻ compacting…` block + summary 文本（截断 400 字符）。压缩这个元操作的产物/进度混进了对话内容，污染 transcript。

### 参考项目

- **codex**（`reference/codex/codex-rs/core/src/compact.rs`）：保留所有 user 消息原文；`SUMMARY_PREFIX` sentinel 避免重复压缩；窗口超限时逐条移除最老消息重试；mid-turn 与 pre-turn 的 initial context injection 策略不同。
- **deepseek-tui**（`reference/deepseek-tui/crates/tui/src/compaction.rs`）：三类 pin（尾部 + working-set 路径 + 错误/diff 标记）；`enforce_tool_call_pairs` fixpoint 修复配对；cache-aligned / fallback 双 summary 路径；head+tail 截断保护；transient 错误重试。

本设计综合两者，并按 yanshi 的实际场景（ReAct + VCS 自动追踪编辑 + 默认 256K 窗口 + 携带式分块）裁剪。Claude Code 的「拆开来压缩、保证不超窗口」思路体现为携带式分块（rolling summary）。

## 目标

- 修复 bug ①–⑧
- 统一两条压缩路径的核心逻辑（消除 `estimateTokens`/`serialize` 重复——CLAUDE.md「重复逻辑必须抽公共」）
- 上下文窗口按模型配置（`provider.context_window`）
- 前缀缓存作为一等目标（summary 调用命中缓存 + 压缩后历史前缀稳定）
- 分块携带式压缩，保证 summary 调用永不超自己的窗口
- 关键信息（用户意图、工作集路径、错误、diff）原文保留
- 压缩状态只在 activity line（"Running…"位置）显示，不进 transcript

## 非目标（YAGNI）

- 不引入内置模型窗口表（如 deepseek-tui 的 `context_window_for_model`）——provider 显式配 + 回退已足够
- 不引入真实 tokenizer——chars/4 启发式 thresholding 够用
- 不引入 pre/post compact hooks——yanshi 无 hook 基础设施
- 不引入 workflow context 结构化提取块（files_touched/tools_used/tasks）——summary 模型从 serialize 文本自行提取
- 不引入 analytics/telemetry 上报——本地日志足够
- 不引入 auto floor（低 token 不压缩）——yanshi 默认 256K 窗口，前缀缓存保护收益小；可后续按需加

## 架构

### 包结构

复用 `internal/ctxcompact` 包名，从「pre-turn 专用」升级为核心 compaction 包，两条路径共用。按职责拆分（CLAUDE.md 单文件 ≤1000 纯代码行 + 重复逻辑抽公共）：

```
internal/ctxcompact/
  plan.go        Plan(): 计算 pinned/summarize 索引集（纯函数，无 IO）
  preserve.go    保真维度判定: workingSetPaths / isErrorMarker / isDiffMarker
  pairs.go       EnforceToolCallPairs(): fixpoint 配对修复
  serialize.go   SerializeForSummary(): 结构化序列化（保留 ToolCalls/ToolResult/Reasoning）
  summarize.go   RunSummary(): cache-aligned + 携带式分块 + 重试
  tokens.go      EstimateTokens(): 计 ToolCalls arguments + reasoning
  assemble.go    Assemble(): 拼装最终消息（summary 作 user 放末尾）
  sentinel.go    SummarySentinel + IsSummaryMessage()
  run.go         Run(): 统一压缩入口（Plan→Serialize→Summarize→Assemble）
  options.go     PlanOpts / RunOpts / Config
  compact.go     MaybeCompact(): pre-turn 入口（保留，内部调 Run）
```

### 统一入口

```go
// Plan 纯函数，决定哪些消息保留原文、哪些进 summary
func Plan(msgs []*schema.Message, opts PlanOpts) *PlanResult

type PlanResult struct {
    PinnedIndices    []int
    SummarizeIndices []int
    WorkingSetPaths  []string
}

// Run 执行压缩。model 由调用方注入（两条路径模型源不同）
func Run(ctx context.Context, msgs []*schema.Message, plan *PlanResult,
    opts RunOpts, m ModelSummarizer, onChunk func(string)) (*Result, error)

// ModelSummarizer —— eino model.BaseChatModel 天然满足
type ModelSummarizer interface {
    Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error)
    Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error)
}
```

两条路径如何复用：
- **mid-turn**（`CompactingModel.maybeCompact`）：保留 wrapper 结构（满足 `model.BaseChatModel`），内部改为 `plan := ctxcompact.Plan(msgs, opts)` + `ctxcompact.Run(ctx, msgs, plan, opts, c.Inner, cb)`。模型源 = `c.Inner`（当前 turn 的模型）。
- **pre-turn**（`MaybeCompact`，被 WS handler 调用）：同样 `Plan` + `Run`。模型源 = `orchestrator` + 可配置的 `Compaction.Model`（专用快速模型）。

核心逻辑（plan/serialize/summarize/assemble）完全共用，只在「模型从哪来」分叉。

### 数据流

```
msgs ──► Plan() ──► {pinned, summarize, workingSetPaths}
                       │           │
                       │           └─► SerializeForSummary() ──► transcript
                       │                                    │
                       │            ┌───────────────────────┘
                       │            ▼
                       │       RunSummary()
                       │         ├─ 总 token ≤ chunk_threshold×窗口 → cache-aligned 单次
                       │         └─ 否则 → 携带式分块（chunk1→s1→chunk2→s2→…→sN）
                       │            │
                       ▼            ▼
                  EnforceToolCallPairs()  fixpoint 修正 pinned
                       │
                       ▼
                  Assemble() ──► [pinned 原文..., summary 作 user 末尾]
```

### 双路径去重（修 bug ⑦）

- **sentinel 标记**：summary 消息以 `SummarySentinel` 开头，`IsSummaryMessage()` 可识别。
- **Plan 阶段短路**：若 msgs 已以 summary 结尾（刚压过），返回空 summarize 集，Run no-op。
- **per-turn 互斥**：mid-turn wrapper 通过 context value 标记「本 turn 已压缩」；pre-turn 检测到则跳过，下一轮 user 消息后才允许再压。

## 保真策略（Plan 函数）

一条消息满足任一即进 `pinned`（原文保留，不压缩）：

| # | 维度 | 理由 |
|---|---|---|
| 1 | 尾部 `KeepRecent` 对（默认 4 对 = 8 条消息） | 当前即时上下文 |
| 2 | 所有 `Role==User` 的**非 tool-result** 消息（codex 风格） | 用户意图永不丢 |
| 3 | 文本提到 **working-set 路径**的消息 | working-set = 从最近 12 条提取的文件路径；正在编辑的文件上下文不丢 |
| 4 | **错误标记**：`error:`/`failed`/`panic`/`traceback`/`assertion failed`/`test failed` | 调试现场不丢 |
| 5 | **diff/patch 标记**：`diff --git`/`+++ b/`/`--- a/`/` ```diff `/`apply_patch` | 编辑变更不丢 |

tool_result 识别：`Role==Tool`（`ToolCallID != ""`）或 Anthropic 风格 user 带 tool_result block——两种都判。

## 配对修复（修 bug ②）

`EnforceToolCallPairs(msgs, &pinned)` —— fixpoint 循环（移植 deepseek-tui 算法）：

1. 扫描**全部** msgs 建 `call_id → idx` 和 `result_id → idx` 两个 map。
2. 对每个 pinned 消息：含 tool_call 但对应 result 不在 pinned → 拉入 result（或移除 call）；反之同理。
3. 重复直到稳定，`permanently_removed` 集合防振荡。
4. 孤立的 tool_call/result（对面根本不存在）→ 从 pinned 移除，避免 API 400。

这一步同时保证 pinned 和 summarize 集合内部配对都完整（summarize = 全集 − pinned）。

## 结构化序列化（修 bug ①）

`SerializeForSummary(msgs) -> string` —— 给 summary 模型看的文本化对话（**不是**给 API 的结构化消息；结构化消息由 `Assemble` 用原始 `schema.Message` 拼装）：

```
[user]: 帮我修复 compaction 的 bug
[assistant]: 我来分析...
  [tool_call: read_file id=call_1] {"path":"internal/llm/eino/compacting.go"}
[tool_result: call_1]: <文件内容 截断>
[assistant]: [thinking] serialize 只读 Content，丢了 ToolCalls
[assistant]: 发现 bug 在第 199 行...
```

- ToolCalls：`[tool_call: <name> id=<id>] <args_json>`
- tool_result：`[tool_result: <id>] <content 截断到 N 字符>`
- ReasoningContent：`[thinking] ...`
- 空 Content + 空 ToolCalls 消息跳过（复用 `resilient.go` 的同类判定）

## 分块压缩（保证不超窗口）

### 切换逻辑

```
summarize 集合总 token ≤ summary 模型窗口 × chunk_threshold(默认 0.9) ?
  ├─ 是 ──► 单次 cache-aligned 调用
  └─ 否 ──► 携带式分块
```

### 携带式 / rolling summary

1. 按 token 预算把 summarize 集合切成 `chunk1, chunk2, …, chunkN`
   - 切分点回退到最近的「安全边界」（不在 tool_call↔result 配对中间切，复用 `pairs.go` 配对检测）。
2. 串行压缩：
   - `chunk1 → s1`（独立压缩，前缀 = 原对话开头 → 命中前缀缓存）
   - `[s1 作前缀, chunk2] → s2`（s1 提供上文，s2 累积 chunk1+2 要点）
   - `[s2, chunk3] → s3` … `→ sN`
3. 最终 `summary = sN`

### 三个保证

- **不超窗口**：每次调用输入 = `[上一个 summary（已压缩、可控）+ 一个 chunk（≤ 预算）]`，严格 ≤ summary 模型预算。
- **关键信息不丢**：被 `Plan` pin 的消息根本不进 summarize 集合，原文保留；summarize 集合只是过程性工具交互，关键事实已被 pin 兜底。
- **块间关联不丢**：携带式让每个 chunk 都有上文 summary 作上下文。

### 失败处理

某块失败 → 重试该块（transient）。重试耗尽 → 整个 `Run` 失败（携带式 `sN` 依赖 `s(N-1)`，不能部分成功）。

## summary 载体（修 bug ④⑦）

不用 System 角色。改为：
```go
{Role: schema.User, Content: SummarySentinel + summary}
// SummarySentinel = "[yanshi:conversation-summary]\n"
```

- 放历史**末尾**（最后一条）→ pinned 消息成为稳定前缀（前缀缓存友好）。
- `IsSummaryMessage()` 检测 sentinel → Plan 短路（防重复压缩）。
- 模型被训练成「带标记的 user 消息 = 历史摘要」（codex 的 `SUMMARY_PREFIX` 同款做法）。

## 传输与 UI 呈现（修 bug ⑧）

压缩是**元操作**，状态不进入对话内容流（transcript），只在 TUI 的 **activity line**（`m.activity`，即 `"✢ Running …"` 那一行）显示。`styles.go:46` 已明确该行「rendered separately between the transcript」——正是「Running 那个位置」。

### 现状（要改的）

- `compact_chunk` delta → `appendCompactChunk` → 累积到 `compactEntry`（一个 **transcript 条目**）。
- `compactEntry.render` 输出 `↻ compacting…` block + summary 文本（截断 400 字符）——summary 内容**混进了消息流**。

### 目标

- **删除 `compactEntry` 作为 transcript 条目**。
- 压缩状态驱动 activity line：
  - 进行中：`m.activity = "Compacting context…"`（或遵循现有 `"Running <name>…"` 模式写 `"Running compact…"`——实现时定）。
  - summary delta 流可更新 activity 行的子状态（如显示已生成字数/块进度），但**不作为 transcript 内容**。
  - 完成后 activity line 切回正常（`"Thinking…"`）；token 缩减通过 header 的 ctx 计数体现（现有 `contextWindowFor` 已驱动该显示），activity 行不残留 compact 痕迹。
- **summary 消息的可见性**：summary 作为 user+sentinel 消息放历史末尾是给模型的历史结构（模型需要看到以延续上下文），但 transcript 渲染时**跳过**它（`IsSummaryMessage` 判定）——不渲染为对话气泡，避免误导用户以为是他们自己说的。

### 实现要点

- `internal/cli/tui/model.go`：`case "compact_chunk"` 不再 `appendCompactChunk` 到 transcript；改为设置 `m.activity = "Compacting context…"`。
- `internal/cli/tui/commands.go`：删除 `compactEntry` struct 及其 render（或改为非 transcript 的轻量状态字段，供 activity 行渲染）。
- `internal/cli/tui/model.go`：消息渲染时跳过 `IsSummaryMessage` 的消息（sentinel 判定，从 `ctxcompact` 包导入）。
- `internal/cli/tui/events.go`：`status{compacted}` 更新 header token 计数 + activity 行短暂提示。
- `proto` 层：`compact_chunk` / `status{compacted}` 帧类型不变，语义归类为状态事件。

## 前缀缓存优化（一等目标）

两层：

**① summary 调用本身命中缓存**（cache-aligned path，单次模式时）
- summary 请求 = `[原 summarize 集合 verbatim] + [末尾 user: "总结以上对话，保留文件路径/命令/错误/决策..."]`
- 前缀 = 原对话开头，逐字节稳定 → 命中之前对话累积的前缀缓存。
- OpenAI 自动缓存；Anthropic 在最后一条 summarize 消息打 `cache_control: ephemeral`。

**② 压缩后历史前缀稳定**
- 产物 = `[pinned 原文..., summary 作末尾 user]`。
- 后续 turn 新消息追加在 summary 之后 → pinned 前缀不变，持续命中缓存。

携带式分块时只有 `chunk1` 命中原对话前缀缓存，`chunk2+` 前缀变了不命中——这是「不超窗口」的必要代价（`chunk1` 仍命中，且优于截断丢信息）。

## 鲁棒性

### transient 错误重试

`RunSummary` 包装：
- 3 次尝试，指数退避 1s / 2s / 4s。
- 只重试 transient：网络错误、429、超时、503。
- 不重试 permanent：401、400 invalid request、JSON 解析错误。
- 错误分类**复用 `internal/llm/eino/resilient.go`**（必要时抽到公共位置，避免复制粘贴）。

### 失败不吞信息（修 bug ⑥）

- summary 调用失败且重试耗尽 → **返回 error，绝不产出空 summary**。
- `Run` 把 error 交给调用方：
  - **mid-turn**（`CompactingModel`）：回退原始 msgs（best-effort），warn 日志；若已超窗口，真实错误浮出来给用户。
  - **pre-turn**（`MaybeCompact`）：返回 error 给 WS handler，用原 history 继续 + 给用户「压缩失败，已保留完整历史」状态（走 activity line，非 transcript）。

## token 估算（修 bug ⑤）

```go
func estimateMessageTokens(m *schema.Message) int {
    n := len(m.Content)/4 + 8                 // 基础 + 每条开销
    n += len(m.ReasoningContent) / 4          // 思考链
    for _, tc := range m.ToolCalls {          // 工具调用参数
        n += (len(tc.Function.Name) + len(tc.Function.Arguments) + len(tc.ID))/4 + 16
    }
    // tool_result 的 content 已在 m.Content；MultiContent 按块加开销
    return n
}
```

覆盖所有内容字段。仍是 chars/4 启发式（thresholding 够用，不引入真实 tokenizer）。

## 配置

### `ProviderConfig` 新增 `ContextWindow`

```go
type ProviderConfig struct {
    Name          string `yaml:"name"`
    Kind          string `yaml:"kind"`
    Model         string `yaml:"model"`
    APIKey        string `yaml:"api_key"`
    BaseURL       string `yaml:"base_url"`
    ContextWindow int    `yaml:"context_window"` // 新增：该模型的 token 窗口
}
```

### `CompactionConfig` 新增 `ChunkThreshold`

```go
type CompactionConfig struct {
    Threshold      float64 `yaml:"threshold"`       // 触发压缩阈值，默认 0.8
    KeepRecent     int     `yaml:"keep_recent"`     // 尾部保留对数，默认 4
    ContextWindow  int     `yaml:"context_window"`  // 回退默认（provider 没配时用），默认 256000
    Model          string  `yaml:"model"`           // 专用 summary 模型，empty = 当前 session model
    ChunkThreshold float64 `yaml:"chunk_threshold"` // 单次 vs 分块切换，默认 0.9
}
```

### 查询优先级

`ContextWindowFor(providerName, providers, compactionFallback)`：
1. provider 显式配置（`provider.context_window > 0`）→ 用之
2. 回退 `compaction.context_window`（兼容旧配置，默认 256000）
3. 两者都 0 → compaction 子系统禁用 + stderr warn（CLAUDE.md「非致命启动失败以子系统禁用方式继续」惯例）

### 两个阈值

| 阈值 | 含义 | 默认 |
|---|---|---|
| `compaction.threshold` | 触发压缩：对话总 token / 窗口 ≥ 此值时压缩 | 0.8 |
| `compaction.chunk_threshold` | 单次 vs 分块：summary 输入 / 窗口 ≥ 此值时分块 | 0.9 |

### 落地

- `contextWindowFor(model, ...)` 真正按 model 查 provider（替换占位实现）。
- `CompactingModel.ContextWindow` 在 `runnerFor` 构建时从 provider 配置查（`/model` 切换自动用新窗口——model 指针为键缓存，天然 per-model）。
- summary 模型窗口：`compaction.model` 配了用那个 provider，empty 用当前 session model。

## 测试策略（Fake 优先，CLAUDE.md 约定）

| 文件 | 覆盖 |
|---|---|
| `plan_test.go` | 五类 pin 各一例 + working-set 路径提取 |
| `pairs_test.go` | fixpoint：孤立移除 / transitive 拉入 / 级联 / 长链收敛（移植 deepseek-tui 场景） |
| `serialize_test.go` | ToolCalls/ToolResult/Reasoning 都进 transcript |
| `tokens_test.go` | ToolCalls arguments 被计入 |
| `summarize_test.go` | 携带式分块：FakeModel 记录每块输入，断言 ≤ 预算 + `s1` 作 `s2` 前缀 + cache-aligned 路径选择 |
| `sentinel_test.go` | `IsSummaryMessage` + Plan 短路（防重复压缩） |
| `run_test.go` | 失败重试 + 失败不产出空 summary |

**回归测**（bug ①–⑧ 各一个，钉死契约）+ **集成测**（扩充 `compacting_test.go`：CompactingModel 经核心后保留 ToolCalls；新增 pre-turn 复用核心）+ **TUI 测**（`compact_chunk` 驱动 activity line 而非 transcript entry；summary 消息渲染时跳过）。

Fake 复用 `einollm.FakeModel`，必要时加 `ChunkAwareFakeModel` 记录携带式每块输入。

## Deliverables

- 新增 `internal/ctxcompact/{plan,preserve,pairs,serialize,summarize,tokens,assemble,sentinel,run,options}.go` + 各 `_test.go`
- 重写 `internal/llm/eino/compacting.go`（wrapper 保留，内核换 `ctxcompact.Run`）
- 删除 `internal/ctxcompact/compact.go` 的 `summarizeStream`/`serialize`/`estimateTokens`（迁入新文件）
- `internal/config/config.go`：`ProviderConfig` 加 `ContextWindow`；`CompactionConfig` 加 `ChunkThreshold`
- `internal/api/http/ws.go`：`contextWindowFor` 真正按 model 查 provider
- `internal/cli/tui/model.go` + `commands.go` + `events.go`：删除 `compactEntry` transcript 条目，压缩状态改走 activity line；消息渲染跳过 summary 消息
- `config.example.yaml`：加 `provider.context_window` + `compaction.chunk_threshold` 示例
- `CLAUDE.md`：更新「回合中压缩」段
- 新增 `docs/compaction.md`

## bug → 修复 映射

| bug | 修复 | 位置 |
|---|---|---|
| ① serialize 丢结构 | 结构化序列化 | `serialize.go` |
| ② 切分断配对 | fixpoint 配对修复 | `pairs.go` |
| ③ 无保真层级 | 五类 pin + 携带式分块 | `plan.go` + `summarize.go` |
| ④ System 冲突 | user + sentinel 放末尾 | `assemble.go` + `sentinel.go` |
| ⑤ token 漏算 | `EstimateTokens` 计 ToolCalls | `tokens.go` |
| ⑥ 失败吞信息 | 失败返回 error，不产出空 summary | `run.go` |
| ⑦ 双路径无去重 | sentinel 短路 + per-turn 互斥 | `sentinel.go` + `run.go` |
| ⑧ 压缩污染 transcript | 删 `compactEntry`，状态走 activity line | `tui/model.go` + `commands.go` |
