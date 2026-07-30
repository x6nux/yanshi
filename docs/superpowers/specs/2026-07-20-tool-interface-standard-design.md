# 工具接口标准化设计（Tool Interface Standard）

日期：2026-07-20
状态：设计已确认，待写实现计划

## 背景

yanshi 的所有工具目前是同一个具体类型 `*GuardedTool`（`internal/tools/guard.go`），通过 `NewGuardedTool(name, desc, params, RunFunc)` 构造，行为由 `RunFunc func(ctx, argsJSON) (string, error)` 闭包注入。围绕它的工具开发存在四个长期问题：

1. **无展示名**：`GuardedTool` 只有 `name`（给模型的标识，如 `shell_run`）和 `desc`（给模型的长描述）。TUI 渲染工具调用块时只能用 `name`，对终端用户不友好（显示 `shell_run` 而非 `Bash`）。
2. **输出格式不统一**：`fs_read`/`fs_edit` 等返回纯文本，但 `shell_run` 返回 JSON `{"output","exit","duration_ms"}`，`agent_start`/`analysis`/`summarize` 返回 `{"result":...}`，`workflow_start` 返回 `{"results":[...]}`，错误一律 `{"error":...}`。模型与 TUI 都被迫处理混合格式。
3. **超时缺失**：仅 `shell_run` 有 `timeout`（默认 120s）。`agent_start`/`analysis`/`summarize`/`workflow_start` 均无超时——它们跑嵌套 ReAct 循环，子代理 LLM 调用一旦 hang 住，整个 turn 卡死。
4. **运行中状态不统一**：`shell_run` 用 `lineProgressWriter` + `ToolProgressCallback` 流式 stdout；`workflow_start` 用 `nested_progress` transport 帧；`agent_start`（单子代理）两者皆无。TUI 要为不同工具写不同渲染逻辑。

本设计用**一个统一的 `Tool` 接口**把以上四点从"文档约定"升级为"编译器强制的契约"。

## 目标

- **统一 `Tool` 接口**：所有工具（不分长短任务）一律实现同一接口，编排层与 TUI 只有一条处理路径，无类型断言分叉。
- **① 展示名**：每个工具声明面向 TUI 的 `DisplayName`（如 `Bash`、`Agent`）。
- **② 输出非 JSON**：工具产出人类可读纯文本，废弃 `{"output":…}`/`{"result":…}`/`{"error":…}` 等 JSON 包装。
- **③ 超时**：每个工具声明 `DefaultTimeout`，编排层据此给 ctx 加 deadline，兜底卡死。
- **④ 运行中状态**：工具通过统一 channel 实时推送状态；TUI 一套固定模板渲染；channel 的 liveness + ctx 超时共同保证"异常结束后底层不再空转"。

## 核心设计原则：字段单一归属（No Field Sharing）

每个 `ToolChunk` 字段有且只有一个消费者，**绝不共享**——避免"一个字段服务两类消费者"导致的逻辑混乱：

| 字段 | 唯一消费者 |
|---|---|
| `Text` | TUI 下方正文区（按 `Overwrite` 追加/覆盖） |
| `Status` | TUI 右侧状态区（标题行右侧，**恒覆盖**；直接型=简短指示 / subagent=统计摘要） |
| `Result` | 模型（拼接成结果） |
| `Overwrite` | TUI 渲染模式标记（追加 / 覆盖，**仅作用于 `Text`**） |
| `Err` | 内部标记（`errcnt` 熔断 + TUI 标红） |

**模型永远只读 `Result`；TUI 永远只读 `Text` + `Status`。** 由此推出两条关键语义：

- **上下文洁净**（subagent 类：`agent_start`/`workflow_start`/`analysis`/`summarize`）：`Text` 推活动详情（工具调用/子Agent 状态，人看），`Status` 推总体统计摘要（人看），`Result` 推最终结论（模型看）。活动详情与统计都不进 `Result`，避免污染模型上下文。`workflow_start` 的 `Result` 只含**最终合成步结论**（C1），中间步结论不进模型。
- **直接型工具**（`shell_run`/`fs_*`/`web_fetch`/…）：输出人和模型都需要，因此**同时推 `Text`（给 TUI）和 `Result`（给模型）**——内容通常相同，但字段分开，保持单一归属。宁可冗余，不要共享字段。

## 非目标（YAGNI）

- 不为不同工具做 per-tool 渲染特判（TUI 只认固定字段）。
- 不保留 `RunFunc` 适配捷径——所有工具一律 `StreamFunc`，短任务在 Stream 里推一片再 close。
- 不改 Eino/ADK 的 `tool.InvokableTool` 契约（`InvokableRun` 仍是模型入口，由基类实现为 Stream 的收集器）。
- 不引入新的 transport 帧——subagent 进度从 `nested_usage`/`nested_progress` 改向喂给 Stream 的 `Status` 字段。

## 设计

### 1. 统一 `Tool` 接口

```go
// ToolChunk 是工具通过 Stream channel 推送的固定结构。所有工具一律产出此结构；
// TUI 与编排层各取固定字段，零 per-tool 特判。channel close = 工具结束。
type ToolChunk struct {
    Text      string  // 下方正文区内容：仅 TUI
    Status    string  // 右侧状态区内容（标题行右侧）：仅 TUI
    Result    string  // 最终结果：仅模型（拼接成模型结果）
    Overwrite bool    // 渲染模式：true=覆盖下方正文区，false=追加（默认）。仅作用于 Text（Status 恒覆盖，不走此标记）。
    Err       error   // 失败标记（内部用）：触发 errcnt 熔断 + TUI 标红块；可读错误文本由工具同时经 Text（TUI）和 Result（模型）推送
}

// Tool 是所有 yanshi 工具的统一契约。所有工具一律实现，无长短任务分叉。
type Tool interface {
    tool.InvokableTool   // Info + InvokableRun（基类提供，=Stream 收集器，给 Eino/ADK）
    DisplayName() string                                            // ① TUI 块标题
    DefaultTimeout() time.Duration                                  // ③ 默认超时
    Stream(ctx context.Context, argsJSON string) <-chan ToolChunk   // ②④ 唯一执行入口
}
```

**字段可见性**（核心设计决策——分离"给人看"与"给模型看"）：

**TUI 布局**：标题行 = `DisplayName()` + `Status`（右侧——直接型如 `Bash() 运行中·3s`，subagent 类如 `Agent() 1 tools 10k 1m9s` / `WorkFlow() 3/8 agents 10k 1m9s`）；`Text` 在标题下方正文区。

| 字段 | TUI | 模型结果 |
|---|---|---|
| `Text` | 下方正文区（按 `Overwrite` 追加/覆盖） | **不计入** |
| `Status` | 右侧状态区（**恒覆盖**，不走 `Overwrite`） | **不计入** |
| `Result` | **不显示** | 计入（拼接） |
| `Overwrite` | 渲染模式标记（false=追加 / true=覆盖），**仅作用于 `Text`** | **不计入** |
| `Err` | 标红块（仅视觉标记） | 触发 `errcnt` 熔断（错误文本经 `Result` 进模型） |

- 模型结果 = 所有 `Result` 拼接（`Text`/`Status` 均不计入；错误文本由工具经 `Result` 推送）。
- TUI = 按 `Overwrite` 把 `Text` 写入下方正文区 / 把 `Status` **覆盖**写入右侧状态区 / 忽略 `Result` / `Err` 标红块 / `close` 定格。
- `Status` 恒覆盖：表达"总体统计/状态摘要"——直接型是简短运行指示（如 `运行中·3s`，可空），subagent 类是累计统计（如 `1 tools 10k 1m9s` / `3/8 agents 10k 1m9s`）。追加无意义。详细活动（输出、工具调用、子Agent 状态）走 `Text`，由 `Overwrite` 区分追加（日志/调用历史累积）与覆盖（面板整体重画）。

### 2. 各工具的字段用法

| 工具 | `Text`（仅 TUI，下方） | `Status`（仅 TUI，右侧·恒覆盖） | `Result`（仅模型） |
|---|---|---|---|
| `shell_run` | stdout 逐行（追加） | 可选 `运行中·Xs` | 完整 stdout + `── exit N · Xs ──` footer |
| `fs_read`/`fs_edit`/`fs_patch`/`fs_diff`/`fs_search`/`fs_list`/`fs_glob` | 内容（追加，同 Result） | — | 内容 |
| `web_fetch` | 页面正文（追加，同 Result） | — | 页面正文 |
| `memory`/`skill`/`time`/`vcs` | 操作结果（追加，同 Result） | — | 操作结果 |
| `agent_start` | 工具调用记录，每行 `Read(args) <该工具status>`（**追加**） | `<N> tools <X>k <Y>s`（累计统计） | 子代理最终结论 |
| `workflow_start` | 子Agent 面板，每行 `Agent(Read) <该子Agent status>`（**覆盖刷新**） | `<done>/<total> agents <X>k <Y>s` | **仅最终合成步结论**（C1） |
| `analysis` | 工具调用记录（追加） | `<N> tools <X>k <Y>s` | 分析报告正文 |
| `summarize` | 工具调用记录（追加） | `<N> tools <X>k <Y>s` | 总结正文 |

关键点：**所有工具都给 TUI 推 `Text` + `Status`**。直接型：`Text`=输出（追加），`Status`=简短运行指示（可空）。subagent 类：`Text`=活动详情（`agent_start` 追加工具调用记录 / `workflow_start` 覆盖刷新子Agent 面板），`Status`=总体统计摘要（累计 tools/token/时长）。模型只读 `Result`：直接型=输出内容，subagent 类=最终结论（活动与统计都不进模型，保上下文洁净）。`Overwrite` 仅作用于 `Text`。

### 3. TUI 渲染模板（固定，零特判）

```
布局：
  ┌─ <DisplayName>() <Status 右侧> ──────────┐
  │ <Text 下方正文区>                          │
  └────────────────────────────────────────────┘

对每个工具块：
  标题行 = tool.DisplayName() + "()"        # ①
  loop:
    case chunk := <-ch:
      if chunk.Text != "" {
        if chunk.Overwrite { 正文区 = chunk.Text }          # 覆盖
        else { 正文区.append(chunk.Text); 滚到底 }           # 追加（默认）
      }
      if chunk.Status != "" { 标题行右 = chunk.Status }      # 恒覆盖（状态只刷新，不追加）
      if chunk.Err != nil { 块.标红() }                     # 仅视觉标记；错误文本已在 Text 正文区
      # Result 字段 TUI 完全忽略
    case ch 关闭:
      块.定格()
      break
```

TUI 永远不知道"这是哪个工具"——它只看字段。两种渲染方式（追加 / 覆盖）由字段决定，对应需求里"添加模式"与"更新状态模式"。

**错误显示规范**：

- 错误的**可读文本**同时经 `Text`（给 TUI，追加正文区）和 `Result`（给模型）推送——内容相同，字段分开（遵循单一归属）。TUI **不另设"错误区"**，错误信息与正常输出在同一区域，靠 `Err` 触发的块标红做视觉区分。
- 错误文本**不暴露内部工具名**（`shell_run`/`agent_start`/`fs_read` 等标识符）。块标题已显示 `DisplayName`，正文不重复工具名——错误描述只讲问题本身。
  - 旧：`shell_run: safe command must not contain shell metacharacters`
  - 新：`命令不得包含 shell 元字符`
- 错误文本统一前缀（如 `✗`，实现时定样式），便于 TUI/模型识别这是错误。
- `Err` 字段仅作**内部标记**：触发 `errcnt` 连续失败熔断、TUI 块标红；**不**承载可读文本。

### 4. `GuardedTool` 职责（基类）

```go
type StreamFunc func(ctx context.Context, argsJSON string) <-chan ToolChunk

func NewGuardedTool(name, display, desc string, timeout time.Duration,
    params *schema.ParamsOneOf, stream StreamFunc) *GuardedTool
```

- `Info(_)` → `schema.ToolInfo{Name, Desc, ParamsOneOf}`（Eino 用，不变）。
- `DisplayName()` / `DefaultTimeout()` → 返回构造值。
- `Stream(ctx, args)`：
  1. `Authorize(ctx, Action{Tool: name}, args)`（权限，fail-closed，与现状一致）。
  2. `ctx, cancel := context.WithTimeout(ctx, g.timeout)`（③ 超时）。
  3. 返回 `g.streamFunc(ctx, args)`（channel）；goroutine 在 ctx.Done 或 stream 结束时 cancel。
- `InvokableRun(ctx, args)`（Eino/ADK 入口，模型路径）：
  1. `ch := g.Stream(ctx, args)`。
  2. 收集：`var result strings.Builder`；`for c := range ch { result.WriteString(c.Result); if c.Err != nil { runErr = c.Err } }`（模型只读 `Result`；`Text`/`Status` 均不计入）。
  3. `runErr != nil` → `errcnt` 熔断逻辑（保留现状，连续 5 次中断 turn）→ 返回 `text.String()`（错误友好文本已由工具经 `Text` 推送，不再单独 `errorResult`）。
  4. 否则 `return spillIfTooLong(ctx, name, result.String()), nil`。

权限只在 `Stream` 检查一次；TUI 路径与模型路径都过 `Stream`。

### 5. subagent/workflow 的显示（`Status` 统计 + `Text` 活动详情）

固定格式（`Status` 右侧统计 / `Text` 下方活动）：

```
Status:  <N> tools <X>k <Y>m<Z>s                          # agent_start/analysis/summarize
Status:  <done>/<total> agents <X>k <Y>m<Z>s              # workflow_start
Text:    <ToolName>(<args>) <该工具调用status>             # agent_start/analysis/summarize（追加）
Text:    Agent(<当前工具或状态>) <该子Agentstatus>          # workflow_start（覆盖刷新）
```

`<当前工具或状态>` 枚举：
- `Agent(Read)`/`Agent(List)`/`Agent(Edit)`…—— 正在调用某工具（括号内为该工具的 `DisplayName`，不带工具输出）。
- `Agent(Thinking...)`—— 模型生成中。
- `Agent(Running...)`—— 兜底运行态。

- **`agent_start`/`analysis`/`summarize`**：`Status` = `N tools Xk Ys`（累计），`Text` = 子代理调用的工具，每行 `<ToolName>(<args>) <该工具调用 status>`，**追加**——每次调一个工具追加一行，形成调用历史。
- **`workflow_start`**：`Status` = `<done>/<total> agents Xk Ys`（进度+累计），`Text` = 执行中的子 Agent 面板，每行 `Agent(<当前工具或状态>) <该子Agent status>`（带 step ID 区分，如 `B1: Agent(Read) …`），**覆盖刷新**——整体重画，完成的子 Agent 移除（或定格），只剩执行中的。

**数据来源**：子代理是嵌套编排器，本身在 ReAct 循环里统计 {当前工具, 调用次数, token, 时长}，且每次工具调用有起止/参数。新增一条**进度回调**，runner 在每次工具调用起止、每次模型 token 计费、每秒计时时回调父工具；父工具据此重算 `Status`（统计摘要）与 `Text`（活动面板）并推送。现有 `SubAgentEmit`/`nested_usage`/`nested_progress` 的职能从"transport 帧"改向"喂 Stream 的 `Status`+`Text`"——它们只服务 TUI，绝不进模型结果（`Result` 仍只含最终结论）。

### 6. 超时与 liveness（④ 的兜底）

- 每个工具的 `DefaultTimeout` 由编排层转成 ctx deadline（`Stream` 内部）。
- 工具的 `StreamFunc` 必须响应 `ctx.Done()`：及时停止、清理子进程/goroutine（`shell_run` 的 `exec.Cmd` 已用 `CommandContext`；subagent runner 透传 ctx）、close channel。
- channel 的 liveness 即工具活性：channel 开着 = 在跑；TUI 据此显示"运行中"；ctx 到期强制 cancel → 工具收尾 → 推 `Err{ctx.Err()}` → close。这覆盖"异常结束后底层仍在空转"——ctx cancel 会 kill 子进程、回收 goroutine。

### 7. 各工具默认值

| 工具 | DisplayName | DefaultTimeout |
|---|---|---|
| `shell_run` | `Bash` | 120s |
| `fs_read`/`fs_edit`/`fs_patch`/`fs_diff` | `Read`/`Edit`/`Patch`/`Diff` | 30s |
| `fs_search`/`fs_list`/`fs_glob` | `Search`/`List`/`Glob` | 30s |
| `web_fetch` | `Fetch` | 60s |
| `agent_start` | `Agent` | 600s（10 min） |
| `workflow_start` | `Workflow` | 1800s（30 min） |
| `analysis` | `Analysis` | 600s |
| `summarize` | `Summarize` | 300s |
| `memory`/`skill`/`time`/`vcs` | `Memory`/`Skill`/`Time`/`VCS` | 30s/30s/5s/60s |

（具体值实现时可调；表给出合理起点。）

## 迁移清单

| 现有 | 处置 |
|---|---|
| 所有工具的 `RunFunc func(ctx, args) (string, error)` | 改写成 `StreamFunc`：`return out, nil` → `ch <- ToolChunk{Text: out}; close(ch)`；`return "", err` → `ch <- ToolChunk{Err: err}; close(ch)`。全量机械改。 |
| `NewGuardedTool` 签名 | 加 `display string`、`timeout time.Duration`，run 参数从 `RunFunc` 改 `StreamFunc`。所有调用点补 display/timeout。 |
| `ToolProgressCallback`（`progress.go`） | 废弃，由 Stream channel 取代。 |
| `lineProgressWriter`（`shell.go`） | 废弃；`shell_run` 改 StreamFunc，逐行推 `Text`、周期推 `Status`。 |
| `SubAgentEmit`/`nested_usage`/`nested_progress` | 改向：喂父工具的 `Status`（统计摘要）+ `Text`（活动面板）chunk，不再走 transport 帧（transport 层相应清理）。 |
| `errorResult`（`guard.go`） | 废弃 `{"error":…}` 机制；错误可读文本由工具经 `Text` 推送，`Err` 仅作内部熔断标记。 |
| 各工具错误信息 | 去掉内部工具名前缀（`shell_run: …` → `…`）；块标题已用 `DisplayName`，正文不重复工具名。 |
| `spillIfTooLong`（`spillover.go`） | 保留，接在 `InvokableRun` 的 `text+result` 拼装之后。 |
| `agent_start`/`analysis`/`summarize` 的 `toJSON({"result":…})` | 改成推 `ToolChunk{Result: result}`（仅模型最终结论）；TUI 看到的只是 `Status` 统计。 |
| `workflow_start` 的 `toJSON({"results":[…]})` | 改成推 `ToolChunk{Result: <仅合成步结论>}`；各中间步结论不进模型（上下文洁净）。 |

## 受影响文件

| 文件 | 改动 |
|---|---|
| `internal/tools/guard.go` | `Tool` 接口、`ToolChunk`、`StreamFunc`、`NewGuardedTool` 新签名、`Stream`/`InvokableRun` 重写、`errorResult` 改纯文本 |
| `internal/tools/shell.go` | `shell_run` 改 StreamFunc（逐行 Text + Status + exit footer），删 `lineProgressWriter` |
| `internal/tools/agent.go` | `agent_start`/`analysis`/`summarize`/`workflow_start` 改 StreamFunc，Status 统计行 + Result 最终结论（workflow 仅合成步） |
| `internal/tools/fs*.go`/`web.go`/`memory.go`/`skill.go`/`time.go`/`vcs.go` | 各 `RunFunc` 机械改 `StreamFunc`，补 DisplayName/Timeout |
| `internal/tools/progress.go` | 废弃 `ToolProgressCallback`（或保留过渡期） |
| `internal/agent/orchestrator/*` | subagent runner 的进度回调接线（喂父工具 `Status` 统计 + `Text` 活动面板） |
| `internal/cli/tui/*` | 工具块渲染改为消费 `Stream` channel 的固定模板；用 `DisplayName` 作标题 |
| `internal/bootstrap/*` | 工具构造调用点补 display/timeout |

## 测试策略（Fake 优先）

- `guard_test.go`：`Tool` 接口契约——`DisplayName`/`DefaultTimeout` 返回值；`Stream` 权限拒绝路径；`InvokableRun` **只收集 `Result`**（`Text`/`Status` 不计入模型）、`Err` 触发 errcnt 熔断且错误文本经 `Result` 进模型；ctx 超时触发 `Err{ctx.Err()}`；错误文本不含内部工具名。
- 各工具 `*_test.go`：改写后用 fake model/fake runner 验证 Stream 推送的 chunk 序列（`shell_run` 逐行 Text + footer；`agent_start` Text 工具调用记录 + Status 统计 + Result 最终结论；`fs_read` 单片 Text）。
- TUI 渲染测试：给定一个 mock Stream（推固定 chunk 序列），断言渲染输出（标题=`DisplayName`、正文=`Text` 拼接、状态区=最后 `Status`、错误标红、`Result` 不显示）。
- subagent 显示：fake runner 回调 {工具, 次数, token, 时长}，断言 `Status` 推送统计摘要（`1 tools 10k 1m9s` / `3/8 agents 10k 1m9s`），`Text` 推送活动面板（agent 追加 `Read(args) …`；workflow 覆盖 `Agent(Read) …`）；两者都不计入模型 `Result`。
- 上下文洁净断言：`agent_start`/`workflow_start` 的 `InvokableRun` 返回值（模型所见）只含最终结论，不含统计行/中间步结论。

## 风险与权衡

- **全量改签名的工作量**：10+ 工具的 `RunFunc`→`StreamFunc` 是机械但全量的改动。缓解：改写模式固定（两行替换），现有测试覆盖行为，改动低风险。
- **`Result` 字段不进 TUI**：subagent 的结论对用户不可见（TUI 显示活动详情 + 统计摘要，不显示最终结论文本）。这是期望行为（人看过程、模型看结论）；若用户要看结论，可经 spill 文件或专门工具。可接受。
- **直接型工具 `Text`+`Result` 冗余**：`shell_run`/`fs_read` 等同时推 `Text`（TUI）和 `Result`（模型），内容通常相同。这是"字段单一归属"的代价——宁可冗余（字符串拷贝廉价）也不要一个字段服务两类消费者。可接受。
- **workflow 中间步结论丢弃**：为保上下文洁净，A1/B1… 的结论不进模型，只有合成步 C1 进。若合成步质量不足，模型拿到的结论会偏弱。缓解：合成步 prompt 明确要求综合全部中间步。可接受（这是"委派"的应有语义）。
- **TUI 渲染从 `ToolProgressCallback` 迁移到 Stream channel**：是 TUI 侧的实质改动。缓解：固定模板简单，且 `shell_run` 的流式行为等价保留（逐行 Text）。
- **subagent 统计的实时性**：依赖 runner 回调频率。token 计费在模型返回时才有，故 `Agent(Thinking...)` 期间 token 不增——可接受（思考期间显示 Thinking 即可）。
- **transport 帧改向**：`nested_usage`/`nested_progress` 从 transport 帧改成 Stream 的 `Status`+`Text`，涉及 WS handler 清理。缓解：两者只给 TUI，不进 transcript，语义清晰。
