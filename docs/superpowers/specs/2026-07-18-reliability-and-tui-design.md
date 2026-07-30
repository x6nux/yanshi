# autocode 可靠性与 TUI 体验增强设计

- **日期**：2026-07-18
- **状态**：草案 v3（待用户 review）
- **范围**：15 项需求，覆盖后端/编排/网络可靠性（4 项）与 TUI 体验（11 项）
- **模块**：`github.com/x6nux/autocode`

> v2 更新：新增 T15；T7 修正 `startedAt` 取值；3.1(b) 用户取消改用独立 `userCancelCtx`。
> v3 更新：T14 重做 —— 引入结束工具 `task_end`、基于确定状态判运行（不靠时间猜，避免误判流式等待）、异常结束 5s 重试、用 ADK 原生 hook；移除基于时间的 idle watchdog。
> v4 更新：T15 定为 24FPS（~42ms）+ 每 5s 全局重绘（三频率刷新）。

---

## 1. 背景与目标

两条主线：

1. **可靠性主线**：LLM 请求错误重试覆盖不全；瞬时网络问题把 turn context 一并取消，被误判为"用户取消"而不重试；偶发模型输出半截就卡死。
2. **体验主线**：TUI 工具调用/思考块/活动行按工具语义分化显示；思考块限时显示与配色；用量统计（含思考预算）；24FPS 流畅刷新（+ 5s 全局重绘）；以及输入/粘贴/选择/流式渲染的性能与正确性问题。

### 15 项需求索引

| # | 需求 | 归属 |
|---|------|------|
| T1 | 接口错误全部重试、10 次、指数退避（含"ctx 取消被误判为用户取消"） | Spec A |
| T2 | 多 provider：openai Chat Completions / openai Responses / anthropic，配置指定 Kind | Spec A |
| T8a | 用量统计采集（含思考预算） | Spec A |
| T14 | 模型输出半截就卡住的恢复机制 | Spec A |
| T3 | 工具调用显示优化（Read/List 静默、Edit/Write 显 diff、Bash/Agent 限高流式、Agent 不显思考块） | Spec B |
| T4 | 思考块限时显示（10 行滚动 + 展开） | Spec B |
| T5 | 思考中与思考结束后文本颜色不同 | Spec B |
| T6 | "Running" 活动行配色 | Spec B |
| T7 | 思考时间计算 bug（开始时间取值错误） | Spec B |
| T8b | 用量统计展示（含思考预算） | Spec B |
| T9 | 粘贴文本逐字显示（非一次性） | Spec B |
| T10 | 鼠标选择只能复制一整行不能复制一截 | Spec B |
| T11 | 折叠判断：仅粘贴折叠，手动输入不折叠 | Spec B |
| T12 | 删除文本过慢 | Spec B |
| T13 | 输出文本像被限速（与 T9 同因） | Spec B |
| T15 | TUI 刷新率 24FPS + 每 5s 全局重绘 | Spec B |

---

## 2. 设计原则

1. **单一重试权威**：所有重试集中在 `ResilientChatModel`，移除 HTTP transport 层独立重试，避免嵌套退避。
2. **用户取消与网络取消解耦**：用户取消用**独立 context**（`userCancelCtx`），与请求生命周期 ctx 分离；重试判定只看前者。
3. **复用现有基础设施**：`MaxRetries`/`MaxEmptyRetries`/`RetryCallback`/`retryableStreamMarkers` 已成熟，扩展而非重写。
4. **三频率刷新**：动画层 24FPS（~42ms，轻量重绘），布局层 debounced（输入/窗口变化时合并），全局重绘层每 5s 兜底。流畅的前提是 `renderBody` 够快，靠"历史 entry 渲染缓存 + pending 纯文本增量"。
5. **按工具语义分化显示**：`toolDisplayFor` 四分类已写好但未接线 —— 接线是 T3 核心。
6. **Fake 优先**：所有新增逻辑用 fake model 驱动确定性测试。

---

## 3. Spec A — 后端 / 编排 / 网络

### 3.1 统一重试策略与用户取消解耦（T1 + ctx 误判）

#### 现状

两层重试并存：

- **HTTP transport 层**（`internal/llm/eino/provider.go:22-105`，`retryTransport`）：独立重试 5xx/429/upstream-400，**`maxRetries: 3`**。
- **模型层**（`internal/llm/eino/resilient.go`，`ResilientChatModel`）：`MaxRetries: 10`，指数退避（base 200ms / max 5s），覆盖 mid-stream EOF / 5xx / 429 / 超时 / 空响应 / markers 命中。

**ctx 误判 bug**：`isRetryableStreamErr`（`resilient.go:342-364`）开头 `if ctx.Err() != nil { return false }`，把"turn ctx 已取消"当用户取消。但 turnCtx（`ws.go:537/563`，派生自 connCtx ← HTTP `r.Context()`）同时是 LLM 请求的生命周期 ctx —— 瞬时网络问题经 eino-ext adapter 传染取消 turnCtx 后，可重试错误被误判为用户取消、直接放弃。代码注释（`resilient.go:336-341`）声称"http.Client.Timeout 不会取消 turnCtx"，与实际现象矛盾。

#### 设计

**(a) 移除 transport 层重试，统一到模型层。** `retryTransport` 退化为普通 transport（保留连接复用），模型层 `MaxRetries=10` 默认即满足。双层重试会让一次失败最多触发 `3×10=30` 次上游请求、退避叠加失控；单一权威使重试次数/退避/回调全部可观测。

**(b) 用户取消用独立 context（用户原话："单独的 ctx 来控制"）。** 当前 turnCtx 身兼两职——既传"用户取消意图"又作请求生命周期 ctx。拆开：

```go
// internal/api/http/ws.go —— turn 建立时（替换现有 turnCtx, tc := context.WithCancel(connCtx)）
userCancelCtx, userCancel := context.WithCancel(connCtx)  // 只表达用户意图
turnCtx, tc              := context.WithCancel(userCancelCtx) // 请求生命周期，派生自 userCancelCtx
```

- `userCancelCtx` 派生自 `connCtx`（连接断开时取消，合理），**只**由用户 Ctrl-C / `/cancel` / `CancelCurrent` / 退出触发 `userCancel()`。
- `turnCtx` 派生自 `userCancelCtx`：用户取消能向下传染中断 in-flight 请求；但**反向不传染**——网络层超时/断连取消的是 turnCtx（或其派生的 callCtx），不会取消 userCancelCtx。
- 模型层每次调用再派生 `callCtx, _ := context.WithCancel(turnCtx)`；连接异常（3.4）/client 行为取消 callCtx 时同样不波及 userCancelCtx。

`userCancelCtx` 经 ctx value 注入到 resilient 层（供 `isRetryableStreamErr` 读取）：

```go
// internal/llm/eino/resilient.go
type userCancelCtxKey struct{}
func WithUserCancelCtx(ctx, cancelCtx context.Context) context.Context {
    return context.WithValue(ctx, userCancelCtxKey{}, cancelCtx)
}
func userCancelCtxFrom(ctx context.Context) context.Context {
    if c, ok := ctx.Value(userCancelCtxKey{}).(context.Context); ok { return c }
    return ctx // 未注入时退化回旧行为（向后兼容现有测试）
}
```

重试判定**只看 userCancelCtx**：

```go
func isRetryableStreamErr(ctx context.Context, err error) bool {
    if err == nil { return false }
    if userCancelCtxFrom(ctx).Err() != nil { return false } // 只有用户取消才不重试
    // …（其余 marker / net.Error / EOF 判定不变）
}
```

效果：网络/超时把 turnCtx 或 callCtx 取消了也没关系——只要用户没按 Ctrl-C（userCancelCtx alive），就重试。重试中若用户按 Ctrl-C，`userCancel()` 取消 userCancelCtx → 传染到 turnCtx → `sleepRetry` 的 `select <-ctx.Done()` 返回，用户随时能停。

**(c) HTTP client timeout 移除。** `provider.go:144` 的 `Timeout: 60s` 对长上下文/思考模型偏紧，且是 ctx 传染的可疑路径。改为移除 client 级 timeout，由 turn 寿命 + 连接异常检测（见 3.4）控制单次调用。

#### 错误边界（默认决策，请 review）

**重试**：5xx、429、超时、连接错误（reset/refused/broken pipe/no such host）、mid-stream EOF、upstream 400、空响应、**因网络导致的 ctx 取消**（现在能正确区分）。

**不重试**：真客户端错误 —— 401/403/404/422、非 upstream 的 400、content_filter。新增 `nonRetryableClientMarkers`（`invalid_api_key`、`model_not_found`、`invalid_request_error` 等）短路。

#### 接入点

- `internal/llm/eino/provider.go`：删 `retryTransport` 重试循环；`BuildProviders` 的 `HTTPClient` 去掉 `Timeout`。
- `internal/llm/eino/resilient.go`：`isRetryableStreamErr` 与 `retry`（Generate 路径）改用 `userCancelCtxFrom`；新增 `WithUserCancelCtx`/`userCancelCtxFrom`/`userCancelCtxKey`。
- `internal/api/http/ws.go:537/563`：turnCtx 改派生自 `userCancelCtx`；`WithUserCancelCtx` 注入；`CancelCurrent`/Ctrl-C 调 `userCancel()`。

---

### 3.2 多 Provider Kind 分发（T2）

#### 现状

`config.ProviderConfig.Kind`（`config.go:78`，`"openai"|"openai-responses"|"anthropic"`）已存在，但 `BuildProviders`（`provider.go:118-156`）**无视 Kind**，全部走 `openai.NewChatModel`。`anthropic.go`（588 行）已完整实现 `NewAnthropicModel` 却未被引用。`openai-responses`（`/v1/responses`）**无现成 eino-ext adapter**，需自写。

#### 设计

**(a) Kind 分发器。** `BuildProviders` 的单 provider 构建抽成 `buildOne`：

```go
func buildOne(ctx context.Context, p config.ProviderConfig) (model.BaseChatModel, error) {
    switch normalizeKind(p.Kind) { // "" 默认 "openai"
    case "anthropic":
        return NewAnthropicModel(ctx, &AnthropicModelConfig{APIKey: p.APIKey, Model: p.Model, BaseURL: p.BaseURL})
    case "openai-responses":
        return NewOpenAIResponsesModel(ctx, &ResponsesConfig{APIKey: p.APIKey, Model: p.Model, BaseURL: p.BaseURL})
    default: // "openai"
        return openai.NewChatModel(ctx, &openai.ChatModelConfig{...})
    }
}
```

三个 provider 共用同一 `http.Client`（无 client timeout），都实现 `model.BaseChatModel`，failover chain / `/model` 切换 / `CompactingModel` 包裹不受影响。

**(b) openai-responses 本期实现（用户决策：硬需求）。** Responses API 与 Chat Completions 在 `*schema.Message` 层兼容，差异在 HTTP 端点（`/v1/responses`）与 JSON 形状。仿 `anthropic.go` 范式手写 `internal/llm/eino/responses.go`（~400-600 行：`buildRequest` / `responseToMessage` / `readStream`），对接 Responses API。`output_text` / 内置工具等特性按需映射到 `*schema.Message`。

**(c) config.example.yaml 更新。** 三个 provider 标注 `kind`。

#### 接入点

- `internal/llm/eino/provider.go`：`BuildProviders` 改调 `buildOne`；`normalizeKind`。
- `internal/llm/eino/responses.go`（新）：`openai-responses` adapter（仿 `anthropic.go`：`buildRequest` / `responseToMessage` / `readStream`，对接 `/v1/responses`）。
- `config.example.yaml`：补 `kind`。

---

### 3.3 用量统计采集（T8a）

#### 现状

`ws.go` 已有 `onUsage`（`ws.go:672-705`）采集 `TurnUsage{PromptTokens, CompletionTokens}`。`schema.TokenUsage` 实际字段更丰富。

#### 设计

**扩展 `TurnUsage` 与 status 帧，采集全部细分**。`schema.TokenUsage` 字段（已核实）：

| 字段 | 含义 | 来源 |
|------|------|------|
| `PromptTokens` | 输入 token | openai / anthropic |
| `PromptTokenDetails.CachedTokens` | 缓存命中输入 token | openai / anthropic(cache_read) |
| `CompletionTokens` | 输出 token | openai / anthropic |
| `CompletionTokensDetails.ReasoningTokens` | **思考 token**（OpenAI/Gemini/ARK/Qwen 填充，其他 0） | openai |
| `TotalTokens` | 合计 | openai / anthropic |

"思考预算"= **`ReasoningTokens`（本 turn 实际消耗的思考 token）**，配合可选配置上限 `thinking.budget_tokens`（P2，跨 provider 语义不一）。本期采集实际消耗并展示。

Anthropic 路径：anthropic 不单独上报 thinking token（计入 `output_tokens`），`anthropic.go` 可在 `readStream` 近似统计或直接报 0。**默认：anthropic 路径 `ReasoningTokens = 0` 并在展示层标注"thinking 计入 output"**，避免不准确数字。

#### 数据流

```
model stream → msg.ResponseMeta.Usage → orchestrator.ClassifyEventsWithUsage
  → onUsage(TurnUsage{Prompt, Cached, Completion, Reasoning, Total})
  → cs.*（持久化）→ proto.StatusFrame → TUI footer / /cost
```

`proto/frame.go` 的 `StatusFrame`/`StreamEvent` 加 `CachedTokens`/`ReasoningTokens`；`store.UpdateSessionMeta` 扩列（SQLite migration，旧库默认 0）。

#### 接入点

- `internal/agent/orchestrator/`：`TurnUsage` 加字段；usage 提取从 `ResponseMeta.Usage` 取细分。
- `internal/proto/frame.go`：加字段。
- `internal/store/session*.go`：meta 列扩展。
- `internal/api/http/ws.go`：`onUsage`/status 传递。

---

### 3.4 完成判定与半截恢复（T14）

#### 用户诉求

- 能判断 agent 是否还在运行；
- 新增"结束工具"，agent 正常完成应调用它；
- 若 agent 在会话途中结束但没调用结束工具 → 等 5 秒自动重试；
- **务必区分"流式正常等待响应"与"真卡死"，别误判**。

#### 运行状态模型（基于确定状态，不靠时间猜测）

一个 turn 内 agent 在 ReAct 循环里多次调用 model，运行状态由**确定信号**判定：

| 状态 | 判据 | 处理 |
|------|------|------|
| 运行中 | stream 未关闭（Recv 阻塞），或产出 tool_call 且工具执行中 | 不干预。"流式等待响应"在此态——**不打断**。 |
| 正常结束 | stream 关闭、产出纯文本、**调了结束工具** 或 finish_reason=stop 且内容非空 | turn 完成。 |
| 异常结束（半截） | stream 关闭、产出纯文本，但**没调结束工具** 且 finish_reason ∈ {length,max_tokens} 或内容空 | 等 5s → 重试本次调用。 |

**不依赖"无 chunk 多久"判卡死**：stream 未关闭即运行中，合法的慢思考/长间隔都安全，不会误判流式等待（回应用户"别误判"）。极端情况连接挂着永不返回（罕见）由用户 Ctrl-C（userCancelCtx）终止，不靠时间猜测。

**同样不依赖"是否有在途 LLM 请求"判运行**：工具执行期间（如 `shell_run` 跑长任务、`fs_read` 读大文件）没有对 model 的请求在途——上一个 model stream 已关闭、下一个还没发起。此时判定基于"是否有未完成的 tool_call"（上表"运行中"第二判据），而非"有无在途请求"或"stream 是否开着"，因此**工具执行期间不会被误判为异常结束**。只有 model 产出纯文本（无 tool_call）且 stream 关闭时，才进入完成/异常判定。又因已移除基于时间的 idle 判定，工具执行多久都不会被超时误判。

#### (a) 结束工具（新增，用 ADK `ReturnDirectly` 原生终止）

注册 `task_end`（no-op，无副作用）+ 把 `"task_end"` 加入 `ChatModelAgentConfig.ReturnDirectly`（`adk` 包原生能力）。**框架自动**：model 一调 `task_end`，agent 即到达成功终止态（return-directly），不跑下一轮——**无需 orchestrator 自己拦截**。

- 作跨 provider 统一、可靠的"完成确认"信号（不依赖各家 finish_reason 语义差异）。
- **mandatory**（用户决策 B）：agent 正常完成**必须**调用 `task_end`；没调即视为异常 → 自动继续。系统提示指示 model 完成时调 `task_end`。重试耗尽（`MaxIncompleteRetries`）兜底：接受最后输出并标记"未正常结束"，避免无限卡死。

#### (b) 完成判定（ADK `AfterModelRewriteState` hook + `AfterAgent`）

用框架 hook 而非自解析事件流：注册一个 `TypedChatModelAgentMiddleware` Handler（经 `ChatModelAgentConfig.Handlers`），`AfterModelRewriteState`（每次 model 调用后，`state.Messages` 末条即本次响应）检查响应是否含 `task_end` tool_call；`AfterAgent`（仅成功终止态 = final answer 或 return-directly 触发）区分——return-directly（`task_end` 触发）= 完成，final answer 无 `task_end` = 异常。mandatory 下判据：① 调了 `task_end` → 完成；② **没调 `task_end`** → 异常（无论 finish_reason / 内容长短）。`finish_reason` 仍记录用于诊断（`length`/`max_tokens` = 截断等），但不影响 mandatory 判定。

#### (c) 5 秒缓冲 + 重试

**自动继续机制**（用户决策）：没调 `task_end`（异常结束）→ 自动重新发起本次 model 调用继续。重试可选"补续"（带"请继续"提示从断点续）或从头生成；已执行的工具副作用保留（不重启 agent）。独立计 `MaxIncompleteRetries`（默认 3）；**耗尽兜底**：接受最后输出并标记"未正常结束"提示用户，避免无限重试/卡死。

#### (d) 连接级异常（resilient 层，已有）

连接断开（TCP reset / mid-stream EOF）走 `retryableStreamMarkers` 重试（见 3.1）。

#### (e) 不重启 agent（坚持）

重启会重复执行有副作用的工具（Bash/Write），危险。恢复粒度是"重试本次 model 调用"，不是重启 agent。

#### 层次划分

- **resilient 层（单次调用）**：连接级重试（EOF / 网络 / 超时）—— 已有。
- **orchestrator 层（ReAct turn）**：语义级完成判定（结束工具 + finish_reason + 内容）+ 5s 重试 —— 新增。

#### 接入点（用 ADK 原生 hook，不自解析事件流）

- 新增 `internal/tools/end.go`：`task_end` no-op 工具 + 注册到工具集。
- `internal/agent/orchestrator/`：
  - `ChatModelAgentConfig.ReturnDirectly["task_end"] = true`（框架自动终止循环）。
  - 注册 `TypedChatModelAgentMiddleware` Handler：`AfterModelRewriteState` 读 `state.Messages` 末条 `ResponseMeta.FinishReason` + 内容做完成判定；`AfterAgent` 确认成功终止。
  - 事件流以 `ev.Err`（非 nil）结束 = 异常终止；正常 EOF 但判定不完整 = 异常结束 → 5s 缓冲 + 重试（`MaxIncompleteRetries`）。
- `internal/llm/eino/anthropic.go`：终止帧填 `FinishReason`（`end_turn→stop` / `max_tokens→length` / `tool_use→tool_calls`）。
- openai 路径：eino-ext 已填（核实）。

---

## 4. Spec B — TUI

### 4.1 工具调用显示（T3）

#### 现状

`toolDisplayFor`（`entries.go:191-201`）已分四类，但 `toolEntry.render`（`entries.go:105-150`）**完全没读它** —— 所有工具统一渲染。`progress []string`（shell 流式）字段存在但未用。

#### 设计

**`toolEntry.render` 按 `toolDisplayFor(name)` 分支**：

| 类 | 工具 | 渲染 |
|----|------|------|
| **Silent** | fs_read/fs_list/fs_glob/fs_search/fs_mkdir | 仅头部 `Read(foo.go) ✓`，**无** `⎿` 行（ctrl+o 展开）。sizeHint 保留。 |
| **Normal** | 默认 | 头部 + 折叠 `⎿`（现状）。 |
| **Tail** | shell_run | 头部 + 限高结果区：running 最后 10 行（progress 流式），结束后最后 3 行，ctrl+o 展开。 |
| **Agent** | agent_start/workflow_start/analysis/skill_use | **不显思考块**：只流式展示子 agent 工具调用与模型输出。头部 `Agent(任务) …` + 嵌套缩进子事件。 |

**Edit/Write 的 diff（新增 `toolDispDiff` 类）。**

- `fs_edit`：参数有 `old_string`/`new_string` → unified diff（`-` 红 / `+` 绿 / ` ` 灰 context），自写行级 LCS（~40 行），无依赖、无语法高亮。
- `fs_write`：新文件 → "wrote N lines"；覆盖 → VCS 有旧版则 diff，否则首行预览。

diff 结束后默认折叠到 "+N -M 行 (ctrl+o)"，展开看完整。

#### 接入点

- `internal/cli/tui/entries.go`：`toolEntry.render` 按 display 分支；新增 `renderDiff`/`renderTail`/`renderAgent`；`toolDisplayFor` 加 `toolDispDiff`。
- 新增 `internal/cli/tui/diff.go`：行级 LCS diff（~60 行）。
- Agent 不显思考：applyEvent 在 tool running 期间把 thinking 事件喂给该 toolEntry 缓冲（`toolEntry` 加 `nested bool`）而非新建 thinkingEntry。

---

### 4.2 思考块：限时显示、配色、计时（T4 + T5 + T7）

#### 现状

`thinkingEntry`（`commands.go:608`）三态：live（`time.Since(startedAt)` + 全量 markdown）/ collapsed / expanded。`thinkingStyle`（grey italic）统一。`appendThinkingDelta`（`events.go:137`）用 `time.Now()` 作 `startedAt`。

#### T7 计时 bug（用户确认：开始时间取错）

现状 `startedAt = time.Now()`（`appendThinkingDelta` 首次调用时）= **首个 reasoning delta 到达 TUI 的时刻**。但模型在吐第一个 chunk 前已经思考了一段时间（TTFT + 推理启动），这段被漏掉 → 显示偏短。

#### 设计

**修正 `startedAt` 取"上一非思考事件到达时刻"（`lastEventAt`）。** ReAct 循环里，上一事件（tool_result 返回 / user 发言 / turn 开始）之后模型立刻开始下一轮思考，所以"上一事件时刻 ≈ 本次思考真正开始"，包含 TTFT 与推理启动。

- `model` 加 `lastEventAt time.Time`，在 `applyEvent` 每个 `tool_result` / `agent_chunk` / turn 开始（`submit`）时更新为 `time.Now()`。
- `appendThinkingDelta` 新建 thinkingEntry 时 `startedAt = m.lastEventAt`（而非 `time.Now()`）。
- **不用 `turnStart`**（会把同 turn 内之前所有工具执行耗时算进去——events.go 注释的旧坑）；`lastEventAt` 只反映最近一段思考起点。
- 兜底：`discardLastThinking` 同时清理 attach 到最后一条 assistantEntry 的 `thought`，避免 retry 后旧计时残留。

live 显示 `time.Since(startedAt)`、finalized 显示 `endedAt.Sub(startedAt)` 不变——只是 startedAt 更准。

**T4 限时显示：**

- live：渲染最后 10 行（尾部滚动，`tailLines` 已有），头部 `✻ Thinking… (Xs · N 行)`，ctrl+o 展开。
- collapsed：现状。
- expanded：全文。

**T5 配色：**

- live（思考中）：`thinkingLiveStyle` —— 亮色 + italic（如 `117` 浅蓝），传达"进行中"。
- finalized（结束）：`thinkingDoneStyle` —— 暗色 + italic（如 `245` 灰），传达"已沉淀"。
- 头部 `✻ Thinking…/Thought for` 配色随态。

#### 接入点

- `internal/cli/tui/commands.go`：`thinkingEntry` render 按 live/collapsed/expanded + 限时 + 配色。
- `internal/cli/tui/styles.go`：`thinkingLiveStyle`/`thinkingDoneStyle`。
- `internal/cli/tui/events.go`：维护 `lastEventAt`；`appendThinkingDelta` 用它；`discardLastThinking` 清 attached thought。

---

### 4.3 Running / 活动行配色（T6）

#### 现状

`statusLine`（`view.go:144`）整行 `activityStyle`（白 `252`）。工具块工具名已按状态上色，但活动行文字全白。

#### 设计

活动行按语义上色关键 token：

- `Thinking…` → `footerThinkStyle`（粉 `213`），与思考块呼应。
- `Running <Tool>…` → "Running" 用 `toolMeta`（灰），`<Tool>` 用 `toolName`（亮青 `123`，与工具块头部一致）。
- `↻ retry N/M` → `warnStyle`（琥珀），已是警示语义。

实现：`statusLine` 拆 token 分别 `Render` 再拼接（footer 已是此模式）。

#### 接入点

- `internal/cli/tui/view.go:144`：`statusLine` 重写为分段渲染。

---

### 4.4 用量统计展示（T8b）

#### 现状

footer 有 `ctx: in/window`；`/cost` 的 `statusEntry`（`commands.go:464`）显示 `tokens: N in · N out`。

#### 设计

**footer 扩展**：`ctx: 12k/128k · 🧠1.2k · cache 8k`（思考 token + cache 命中）。`/cost` 详情：

```
tokens: 12,340 in (cache 8,000) · 3,210 out · think 1,200
turns: 5 · model: claude-opus-4-8 · thinking: high
```

字段来自 3.3 的 `CachedTokens`/`ReasoningTokens`。

#### 接入点

- `internal/cli/tui/view.go:311`：`statusHeader` 加思考/cache 段。
- `internal/cli/tui/commands.go:464`：`statusEntry.render` 细分。
- `internal/cli/tui/model.go`：加 `cachedTokens`/`reasoningTokens`，`applyStatus` 填充。

---

### 4.5 渲染节奏：三频率刷新 + 节流（T9 + T12 + T13 + T15）

#### 根因

`model.Update` 末尾（`model.go:514-523`）对每次 msg 都 `growInput + updatePalette + reflow`。`reflow`（`view.go:77`）调 `blockHeight` 七次（每次 `strings.Split` + 逐行 `lipgloss.Width`）；`renderBody` 重渲染**所有** entry + 对 `pending` 全量 `renderMarkdown`。粘贴 = textarea 逐 rune 发 KeyMsg → 每字符一次全量 reflow+markdown（T9）；删除 = 每次 backspace 同理（T12）；流式输出 = 每 chunk 一次（T13）。同时 `activityTick` 仅 500ms（2 FPS），动画卡顿（T15）。

#### 设计：三频率刷新

**① 动画帧（~42ms / 24FPS）。** `activityTick`（`startup.go:28`，现 500ms）改为 `~42ms`（24FPS，电影帧率，对 TUI 足够流畅且远省于 60FPS 的 CPU）。每帧只做轻量重绘：

- 推进 spinner / glyphFrame、重算 elapsed time、追加 `pending` 新 chunk（纯文本，不 markdown）。
- 只调 `m.viewport.SetContent(renderBody())` 重绘，**不调 `reflow`**。
- 前提：`renderBody` 在 ~42ms 内完成 → 靠下面的"历史 entry 缓存 + pending 纯文本"。

**② 布局帧（debounced）。** `reflow` 不再每 msg 触发，改为：

- 输入变更（textarea）排一个 debounce tick；期间新变更取消旧 tick。一次粘贴/快速删除只触发一次 reflow（治 T9/T12）。
- 窗口尺寸变化（WindowSizeMsg）/ entry 增减（新 tool/assistant 块出现）立即 reflow（这些低频，无碍）。

**③ 全局重绘（每 5s）。** 一个独立的 5s tick 触发**全量** `reflow` + `renderBody`（绕过 entry 缓存）+ `tea.Repaint`。兜底作用：捕获不被事件/动画驱动的时变渲染（相对时间戳等）、强制刷新陈旧缓存、修复 diff 渲染器可能留下的不同步。即使无任何输入/事件，屏幕每 5s 也保证完全一致一次。

**让 renderBody 够快（支撑 24FPS）：**

1. **历史 entry 渲染缓存**：已 finalize 的 entry 按 `(内容指纹, width)` 缓存其 markdown 渲染结果（`map[uint64]string` 或 `sync.Map`）。reflow/renderBody 对历史 entry 直接取缓存，不重复 `renderMarkdown`（历史不变，重算是纯浪费）。
2. **pending 纯文本增量**：流式 `agent_chunk` 累积到 `pending` 时，动画帧只追加纯文本（等宽 `pendingStyle`）；仅在 `flushAssistant`（turn 段落结束）时做一次 markdown 定稿并入缓存。
3. **输入路径分离**：textarea 的 KeyMsg 走 debounce，不阻塞动画帧。

> 三频率各司其职：动画帧高频轻绘（流畅）、布局帧按需重排（治卡）、全局重绘 5s 兜底（防不同步）。过去的问题是用同一个 `reflow` 同时承担所有刷新。

#### 接入点

- `internal/cli/tui/startup.go:28`：`activityTick` 间隔 500ms → ~42ms（24FPS）；新增 `repaintTick`（5s）触发全局重绘 + `tea.Repaint`。
- `internal/cli/tui/model.go:290-298`：动画帧 handler 只做轻量更新 + `SetContent`，不 reflow；输入变更排 debounce。
- 新增 `internal/cli/tui/debounce.go`：tick 合并辅助。
- `internal/cli/tui/view.go:359`：`renderBody` 区分流式中（纯文本）与定稿（markdown + 缓存）。
- `internal/cli/tui/styles.go`：entry 渲染缓存。

---

### 4.6 鼠标选择：行内部分选择 + 多行（T10）

#### 现状

`handleSelectMouse`（`view.go:194`）/ `selRange` / `selectedText`（`view.go:229`）以**屏幕整行**为单位：`splitStripANSI(renderScreen())` 切行，取 `[lo,hi]` 行 join。所以无论在行内拖选多小，结果都是**一整行**——无法复制"一截"。多行选择也因 release/行映射不稳定。

#### 根因

选择模型只有"屏幕行号"一维，没有字符列。要做行内部分选择，需二维（行 + 列），并把屏幕坐标映射回底层字符串偏移（处理 ANSI strip、自动换行 wrap、东亚宽字符占 2 列）。

#### 设计

- **选择模型升级为二维**：`selAnchor`/`selLine` 从"行号"扩为 `{row, col}`（col = 屏幕列，已扣除 ANSI）。press/motion 记二维坐标。
- **字符区间截取**：`selectedText` 不再"取整行 join"，而按起止 `{row,col}` 在纯文本上做**字符区间**截取（跨行：首行 col→尾、中间整行、末行头→col）。
- **release 加固**：press/motion 持续更新；release 未到达时下次 press/esc 兜底 finalize。
- **拖到边框自动滚动**：拖选到屏幕顶/底边缘时触发 viewport 自动滚动（配合 24FPS 动画帧），把选择扩展到屏幕外内容。实现：motion 时若 `selLine` 接近视口边界，发 `viewport.LineUp/Down` + 同步扩展选择范围。
- **宽字符对齐**：用 `lipgloss.Width`/runewidth 对齐屏幕列与字符偏移。

#### 接入点

- `internal/cli/tui/view.go:194-242` + `model.go` 选择字段：行号 → `{row,col}`；`selectedText` 改字符区间截取。
- 复现优先：fake model 构造多行 + 行内长文本，验证能选中一截与多行。

---

### 4.7 粘贴折叠语义修正（T11）

#### 现状

`userEntry.isLong()`（`entries.go:39`）对所有 >240 字符 / >4 行的消息折叠，不区分来源。

#### 设计

`userEntry` 加 `pasted bool`。**只有 `pasted && isLong()` 才折叠**；手动输入的长消息始终全文。

- 粘贴检测：textarea 收到 `tea.KeyPaste`（括号粘贴序列）或一次 Update 里 runes 数量超阈值（如 >50 字符的单次 KeyMsg ≈ 粘贴）时，标记接下来提交为 `pasted=true`。
- `submit` 据标记建 `&userEntry{text, pasted: m.inputPasted}`。

#### 接入点

- `internal/cli/tui/entries.go`：`userEntry` 加 `pasted`；`render`/`isLong` 分支。
- `internal/cli/tui/model.go`：输入路径检测粘贴、`submit` 传递。

---

## 5. 实现顺序与依赖

```
Spec A（后端，独立可测，需真实 key 验证）：
  3.1 重试统一 + 独立 userCancelCtx ──┐
  3.4 结束工具 + 完成判定 + 5s 重试 ──┤── resilient(连接级) + orchestrator(语义级)，一起做
  3.3 用量采集 ─────────────────────── 独立
  3.2 多 provider Kind 分发 ────────── 依赖 3.1 的 transport 调整

Spec B（TUI，fake-model 可测）：
  4.5 渲染节奏（三频率 + 缓存）── 先做（24FPS + 性能，后续渲染都受益）
  4.1 工具显示（含 diff.go）
  4.2 思考块（计时修正 + 限时 + 配色）
  4.3 活动行配色 ── 小
  4.4 用量展示 ── 依赖 Spec A 3.3 字段
  4.7 粘贴折叠 ── 小
  4.6 选择修复 ── 需复现，独立
```

Spec A 与 Spec B 文件零重叠，可并行；Spec B 内部按上序（渲染节奏先于显示细化）。

---

## 6. 测试策略

- **Fake 驱动**：`einollm.FakeModel` 扩展支持注入 `FinishReason`、`idle stall`、`ctx cancel`、`ReasoningTokens`、`reasoning chunk 时序` 等，覆盖 T1/T7/T8a/T14 确定性测试，无需 key。
- **重试/ctx 解耦**：构造会 cancel 传入 ctx（但不 cancel userCancelCtx）的 fake model，断言仍重试；再构造 cancel userCancelCtx 的，断言不重试。
- **完成判定**：fake 注入 `finish_reason=length` 或"不调 `task_end` 的纯文本" → 断言 5s 后重试且受 `MaxIncompleteRetries` 限；调了 `task_end` → 断言立即完成、不重试。
- **T7 计时**：fake 按时序发 tool_result → 延迟 → reasoning，断言 startedAt ≈ tool_result 时刻（含延迟）而非首 chunk 时刻。
- **TUI 快照**：工具显示/diff/思考块/三频率 render 做字符串断言（`model_test.go` 先例）。
- **门禁**：`go test ./...` 全绿；eino-ext openai provider 不可用时 `t.Skip`（既有约定）。

---

## 7. 默认决策清单（请 review 时逐条确认或挑战）

| 决策 | 我的默认 | 影响 |
|------|---------|------|
| transport 层 3 次重试 | **移除**，统一模型层 10 次 | T1 |
| 用户取消 | **独立 `userCancelCtx`**，重试只看它 | T1/ctx |
| HTTP client 60s timeout | **移除**，由 turn 寿命 + 连接异常检测（3.4）管 | T1/T14 |
| 真 4xx（401/403/404/422/真400） | **不重试** | T1 |
| `openai-responses` Kind | **本期实现** `responses.go`（用户决策） | T2 |
| 思考预算语义 | = `ReasoningTokens` 实际消耗；anthropic 报 0 并标注 | T8 |
| 结束工具 `task_end` | 新增，ADK `ReturnDirectly` 原生终止；**mandatory**（用户决策 B） | T14 |
| 异常结束重试 | **自动继续**（重试/补续），`MaxIncompleteRetries = 3`，耗尽兜底接受 | T14 |
| 自动重启 agent | **不引入**（副作用重复风险）；只重试本次 model 调用 | T14 |
| Edit/Write diff | 行级 LCS unified，无语法高亮，无依赖 | T3 |
| T7 startedAt | 取 `lastEventAt`（上一非思考事件时刻） | T7 |
| TUI 刷新 | **三频率**：动画 ~42ms(24FPS) / 布局 debounced / 全局重绘 5s | T15/T9/T12/T13 |
| pending 流式渲染 | 流式中纯文本增量，定稿后 markdown + 缓存 | T13 |
| 输入 reflow | ~16ms debounce 合并 | T9/T12 |
| 粘贴折叠 | 仅 `pasted && isLong` | T11 |

---

## 8. 决策记录（用户已拍板，2026-07-18）

1. 结束工具 `task_end` → **mandatory**（没调即异常 → 自动继续；耗尽兜底接受）。
2. `openai-responses` → **本期实现** `responses.go`。
3. T6 → 活动行文字上色（确认）。
4. T10 → 字符级任意起止选择 + 拖到边框自动滚动。
5. T14 恢复 → 自动继续机制（重试/补续，不重启 agent）。

（T7 思考计时、ctx 解耦已据前序反馈定稿。）
