# 工具调用输出管理设计（Tool Output Management）

日期：2026-07-19
状态：设计已确认，待写实现计划

## 背景

yanshi 的工具结果经 `GuardedTool.InvokableRun` 直接以 string 返回，ADK ToolsNode 原样喂回模型，**中间没有任何统一的截断/spillover 层**。各工具的体积控制参差不齐：

| 工具 | 现状 |
|---|---|
| `shell_run` | **无任何输出上限**——最大的超长来源（`go test`、大日志、`find` 全量） |
| `fs_read` | 256 KiB 字节上限，且有 bug（见下） |
| `web_fetch` | 1 MiB 上限 |
| `fs_search`(content) / `fs_diff` | 无显式输出上限 |
| `agent_start` / `analysis` 等子代理 | 子代理结果可能很长，无上限 |

### 现有 `fs_read` bug

`internal/tools/fs.go:runRead` 的截断发生在应用 `offset`/`limit` **之前**：

```go
data, _ := os.ReadFile(paths[0])
if origLen > fsReadMaxBytes { data = data[:fsReadMaxBytes] }   // 先截断
lines := strings.Split(...)
// 然后才在（已截断的）lines 上应用 offset/limit
```

大文件指定 `offset` 读到的会是空内容或错位——模型分页读取失效。

### 用户需求（4 项）

1. 工具返回内容过长 → 写入临时文件，返回路径让模型自己读。
2. `fs_read` 限制读取文件大小。
3. `fs_read` 增加读取范围（起始 + 结束行）。
4. 新增一个工具调用 Agent 来总结文件内容。

## 决策摘要（brainstorming 已确认）

| # | 决策点 | 选择 |
|---|---|---|
| Q1 | temp 文件位置 | **A**：项目根下 `.yanshi/tmp/spillover/`，被 `fs_list`/`fs_search`/`fs_glob` 跳过 |
| Q2 | spillover 作用范围 | **A**：统一在 `GuardedTool.InvokableRun`，所有工具自动覆盖 |
| Q3 | 阈值 | **64 KiB**（≈ 16k tokens，约占 128k 窗口 12%） |
| Q4 | `fs_read` 参数 | **B**：`offset` + `end`（`end` 替换原 `limit`） |
| Q5 | summarize 形态 | **A**：镜像 `analysis`——预定义 agent + 专属 GuardedTool |

## 目标

- **一个统一的 64 KiB 输出上限**横切所有 `GuardedTool`，超过即落盘 temp 文件并返回 head+tail 预览 + 路径。**例外**：`fs_read` 自检并直接报错（不落盘），避免"读取 spill 文件再 spill"的循环（§4）。
- 修掉 `fs_read` 的截断 bug；参数升级为 `offset`+`end`；流式扫描防 OOM。
- 新增 `summarize` 工具，用子代理（仅持 `fs_read`）分页读取并产出结构化总结。
- temp 文件可被 `fs_read` 读回（位于 jail 内）。
- 不破坏现有工具契约：工具仍返回 string，spill 只是 string→string 的后处理。

## 非目标（YAGNI）

- 不做 per-tool 阈值配置（统一 64 KiB 足够；如需可后续提升为 `Config` 字段）。
- 不做 spillover 文件的引用计数/LRU 淘汰（启动时 sweep 已足够，单进程 TUI 语义）。
- `summarize` 不做 map-reduce 多级压缩（单子代理 + `fs_read` 分页足以覆盖典型大文件）。
- 不改 `web_fetch` 的 1 MiB body 上限（它是 *读取* 上限，不是输出上限；超过 64 KiB 的部分由统一 spillover 处理）。
- 不为 spillover 目录加 `.gitignore`（`.yanshi/` 整体已 gitignore——实现时确认）。

## 架构总览

```
模型调用工具 → GuardedTool.InvokableRun → g.run(ctx, args)
                                          ↓ result (string)
                                   spillIfTooLong(ctx, name, result)   ← 新增统一收口
                                          ↓
                          len ≤ 64 KiB → 原样返回
                          len > 64 KiB → 写 .yanshi/tmp/spillover/<id>.txt
                                         返回 [预览 head+tail] + 路径 + 指引
```

## 设计

### 1. 统一 Spillover 层（新文件 `internal/tools/spillover.go`）

包级常量：

```go
const spillThreshold = 64 * 1024 // 64 KiB
```

核心函数：

```go
// spillIfTooLong returns result unchanged when len(result) <= spillThreshold.
// Otherwise it writes result to a temp file under <workRoot>/.yanshi/tmp/spillover/
// and returns a head+tail preview plus the path and usage guidance. workRoot is
// read from ctx (WithWorkRoot); absent ctx it falls back to "." so the layer
// still functions in bare tests.
func spillIfTooLong(ctx context.Context, toolName, result string) string
```

**`fs_read` 豁免**：`fs_read` 自检窗口大小并自行报错，永远不返回超长 string，因此 `spillIfTooLong` 对它天然不触发——避免"`fs_read` 读取 spill 文件再 spill"的循环（详见 §4）。spillover 只服务于无法分页索要的整块输出（`shell_run` / `web_fetch` / 子代理结果 / `fs_diff`）。

**temp 文件位置**：`<workRoot>/.yanshi/tmp/spillover/<tool>-<unixms>-<rand>.txt`
- `<unixms>` = Unix 毫秒时间戳；`<rand>` = 短随机串，避免同毫秒并发工具调用（如 workflow 并行子代理）碰撞。
- 写入用 `os.MkdirAll` + `os.WriteFile`（0600）。写入失败时**降级**：返回截断到阈值的结果 + footer 提示"落盘失败，结果已截断"（绝不让落盘失败拖垮工具调用）。

**预览格式**（head+tail，让尾部信息如 shell exit code 也可读）：

```
[spilled: 1345 lines / 280 KiB → .yanshi/tmp/spillover/shell_run-....txt]
<head ~15 行>
[... 1315 lines omitted ...]
<tail ~10 行>
Use summarize(path) to condense, or fs_read(path, offset, end) to page.
```

行数统计 = `strings.Count(result, "\n") + 1`；head/tail 通过按 `\n` 切片取首/尾。

**清理**：进程启动时（`bootstrap` 末尾，或 `spillover` 包的 `init`/显式 `Sweep(root)`）清空 spillover 目录内全部文件。运行期不主动删（模型可能正在分页读上一个 turn 的 temp 文件）。

### 2. workRoot 注入（`WithWorkRoot`）

新增 ctx 键（与 `WithProfile`/`WithSubAgentRunner` 同模式），文件 `internal/tools/permctx.go` 或新 `workroot.go`：

```go
func WithWorkRoot(ctx context.Context, root string) context.Context
func WorkRootFromContext(ctx context.Context) string  // "" 当未绑定
```

- `orchestrator.Config` 新增 `WorkRoot string` 字段。
- `orchestrator` 在注入 `WithProfile` 的同一批 ctx 装配点（`orchestrator.go` 4 处）追加 `ctx = tools.WithWorkRoot(ctx, o.workRoot)`。
- `bootstrap` 把已有的 `workRoot` 传入 `orchestrator.Config.WorkRoot`。
- `spillIfTooLong` 缺省回退 `.`，保证未绑定 ctx 的裸测试不崩。

### 3. `GuardedTool.InvokableRun` 改动（`guard.go`）

在 `g.run(ctx, argsJSON)` 返回成功路径上，把

```go
return out, nil
```

改为

```go
return spillIfTooLong(ctx, g.name, out), nil
```

`errorResult` 分支不经 spill（体积小，天然漏过阈值，且错误信息应原样可见）。`errcnt` 重置逻辑保持不变。

### 4. `fs_read` 重新设计（`fs.go`）

**参数**（`NewGuardedTool` 的 params map）：

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `path` | string | — 必填 | 文件路径（相对 work root） |
| `offset` | integer | 1 | 1-based 起始行 |
| `end` | integer | EOF | 1-based **包含**结束行（替换原 `limit`） |

**流程**（修 bug + 防 OOM，删除 `fsReadMaxBytes` 与旧预截断）：

1. `os.Stat` 拿总字节数（footer 的 "X MiB"）。
2. 一遍流式扫描（`bufio.Scanner` 按行）：维护行号计数器，落在 `[offset, end]` 区间则收集进缓冲，EOF 时 `totalLines = 计数器`。**内存占用 ≤ 窗口**（只持有区间内的行）；I/O 扫整文件一次以得到 `totalLines` 供 footer 分页参考——大文件 + 小窗口会多读一些字节但不爆内存。
3. 输出：每行 `fmt.Sprintf("%d\t%s", lineNo, text)`，**不加 footer**——保持与现有 `fs_read` 契约一致（无范围=整文件或超长报错；有范围=模型自知是窗口，无需多余提示），也避免破坏现有窗口测试的精确等值断言。
4. 窗口输出 > 64 KiB → **直接 `errorResult`**，消息含窗口字节数 + 总行数 + 总字节数（人类可读）+ "narrow offset/end, or summarize(path)"；**不走 spillover**（见下方"为何 fs_read 豁免"）。
5. `withInstructions`（AGENTS.md hint）照旧 prepend 在最外层。

边界：
- `offset > totalLines` → 返回空字符串 `""`（无 footer）。
- `end < offset` → 视为参数错误，返回 `errorResult`。
- `offset < 1` → 归一化为 1。
- `end <= 0` 或未给 → 取 `totalLines`。

**为何 `fs_read` 豁免 spillover（避免循环读取）**：若 `fs_read` 把超长结果落盘到 `.yanshi/tmp/spillover/fs_read-*.txt`，模型为看内容会再 `fs_read` 该 temp 文件——同样的超长内容又触发 spill、写一个新 temp 文件，模型再读……**temp 文件无限增殖、永不收敛**。`fs_read` 本质是分页工具，超长的正确响应是报错让模型收窄 `offset`/`end`，而非制造中间文件。

实现：`fs_read` 在拼装完窗口文本后、返回前自检 `len(output) > spillThreshold`，超限即返回 `errorResult`（消息含窗口实际大小 + "narrow offset/end, or summarize(path)"），**永远不返回超长 string**。于是 `InvokableRun` 里的统一 `spillIfTooLong` 对 `fs_read` 天然不触发（它只看到短 error 字符串）。无需给 `GuardedTool` 加 opt-out 开关。

### 5. `summarize` 工具（`agent.go` + `predefined.go`）

**`predefined.go` 注册**：

```go
"summarize": {
    Name:        "summarize",
    Description: "读取并结构化总结文件内容（支持大文件分页）",
    PromptTmpl: `你是一个内容总结专家。请阅读目标文件并产出结构化总结。

目标文件: {{target}}
{{focus_line}}
要求:
- 用 fs_read 分页读取（offset/end），不要假设一次能读完。
- 产出: ① 核心要点 ② 结构/章节 ③ 关键片段（必要时引用行号）。
- 总结不超过 {{max_lines}} 行。
- 若是日志/输出，提取异常、错误、关键时间线。
- 若是代码，概述职责、公开符号、依赖。`,
},
```

**`AgentTools` 新增**：

```go
Summarize *GuardedTool  // summarize(path, focus?, max_lines?)
```

参数：
- `path`（必填）——项目文件或 spillover 的 `.yanshi/tmp/spillover/*.txt`。
- `focus`（可选）——关注点，如 "只看错误处理"。
- `max_lines`（可选，默认 50）——总结长度上限。

实现（镜像 `runAnalysisAgent`）：

```go
func (t *AgentTools) runSummarize(ctx context.Context, argsJSON string) (string, error) {
    // parse path/focus/max_lines
    focusLine := ""
    if focus != "" { focusLine = "重点关注: " + focus }
    vars := map[string]string{"target": path, "max_lines": ..., "focus_line": focusLine}
    prompt := FillPrompt(def.PromptTmpl, vars)
    result, err := t.runSubAgent(WithLeafSubAgentTools(ctx), prompt, []string{"fs_read"}, "")
    // 返回 {"result": result, "target": path}
}
```

`AgentTools.Tools()` 与 `bootstrap` 注册新工具。子代理工具限定 `["fs_read"]`——只能读，不能改。summarize 自身结果也走 64 KiB 统一 spillover。

### 6. 横切：排除 spillover 目录

- `fsSearchIgnore`（`fs.go`）新增 `.yanshi`（同 `.git` 处理），`fs_search` 跳过。
- `fs_list` / `fs_glob`：在遍历时跳过根级 `.yanshi` 目录条目（与 search 对齐，避免模型把 spillover 文件当项目产物）。
- VCS：spillover 文件不经 `fs_write`/`fs_edit`，不被 `trackEdit` 追踪 ✓（autoVCS 只追踪 agent 编辑流，不扫盘，确认无副作用）。
- `.gitignore`：确认 `.yanshi/` 已整体忽略（若否，补一条 `.yanshi/tmp/`）。

## 数据流示例

**大日志读取**：
```
shell_run("go test ./... 2>&1")
  → run() 返回 2 MiB output
  → spillIfTooLong: > 64 KiB → 写 .yanshi/tmp/spillover/shell_run-<id>.txt
  → 返回 head+tail 预览 + 路径
模型 → fs_read(".yanshi/tmp/spillover/shell_run-<id>.txt", offset=400, end=600)
  → 分页看具体失败
或  → summarize(".yanshi/tmp/spillover/shell_run-<id>.txt", focus="失败用例")
  → 子代理分页读 + 产出结构化总结
```

**大源码文件**：
```
fs_read("big.go")
  → stat 1.2 MiB，offset=1/end=EOF
  → 流式收 [1, totalLines]，输出 > 64 KiB → errorResult
    "fs_read: result window 1.2 MiB exceeds 64 KiB limit; narrow offset/end, or summarize(path)"
模型 → fs_read("big.go", offset=100, end=200)  // 精准小窗口，原样返回
```

## 错误处理

| 场景 | 行为 |
|---|---|
| temp 文件写入失败（磁盘满/权限） | 降级：返回截断到阈值的结果 + footer 提示落盘失败 |
| `fs_read` 大文件但给了 offset/end 小窗口 | 正常返回小窗口，不 spill |
| `summarize` 子代理失败 | 沿用 `runSubAgent` 错误传播；`chatModel == nil` 时返回 `errorResult` |
| `WorkRootFromContext` 为空 | `spillIfTooLong` 回退 `.`，仍落盘到 `./.yanshi/tmp/spillover/` |
| `end < offset` | `errorResult("end must be >= offset")` |

## 测试（Fake 优先）

- **`spillover_test.go`**（新）：
  - `len == threshold` 原样返回；`len == threshold+1` 落盘。
  - 预览含 head+tail+路径+指引；行数/字节数正确。
  - temp 文件内容 == 原始 result；可被 `os.ReadFile` 读回。
  - `WithWorkRoot` 未绑定时回退 `.`。
  - 写入失败降级路径（用坏权限目录模拟）。
- **`fs_test.go`**（改）：
  - `offset`/`end` 窗口正确；`end` 超 EOF 截到 `totalLines`；`offset` 超 EOF 返回空+footer。
  - 大文件流式（fake 一个 5 MiB 文件）只读指定窗口，内存不爆（用 `testing.AllocsPerRun` 或窗口大小断言）。
  - 窗口输出 > 64 KiB → 返回 `errorResult`（**不**落盘、**不** spill），消息含窗口大小 + 总行数 + 总字节 + "narrow offset/end, or summarize"；小窗口原样返回（无 footer）。
  - `withInstructions` 仍 prepend。
- **`agent_test.go`**（改）：
  - `summarize` 用 fake model 验证 prompt 拼装（`{{target}}`/`{{focus_line}}`/`{{max_lines}}` 替换）。
  - 子代理工具限定为 `["fs_read"]`（mock runner 断言 allowedTools）。
  - `chatModel == nil` 返回 `errorResult`。
- **`guard_test.go`**（改）：
  - 端到端：一个 fake 工具返回 > 64 KiB → `InvokableRun` 返回预览 + temp 路径；`fs_read` 能读回该 temp。
- **orchestrator/bootstrap**：`WorkRoot` 透传到 `Config` 并注入 ctx（已有测试模式扩展）。

## 受影响文件

| 文件 | 改动 |
|---|---|
| `internal/tools/spillover.go` | **新**——阈值、`spillIfTooLong`、`Sweep`、预览格式 |
| `internal/tools/workroot.go` 或 `permctx.go` | **新/改**——`WithWorkRoot`/`WorkRootFromContext` |
| `internal/tools/guard.go` | `InvokableRun` 成功路径插入 `spillIfTooLong` |
| `internal/tools/fs.go` | `fs_read` 重写（`offset`+`end`、流式、删 `fsReadMaxBytes`）；`.yanshi` 加入忽略集合；`fs_list`/`fs_glob` 跳过 |
| `internal/tools/predefined.go` | 注册 `summarize` 预定义 agent |
| `internal/tools/agent.go` | 新增 `Summarize` 工具 + `runSummarize` |
| `internal/tools/agent_test.go` / `fs_test.go` / `guard_test.go` / `spillover_test.go`（新） | 测试 |
| `internal/agent/orchestrator/orchestrator.go` | `Config.WorkRoot` + ctx 注入（4 处） |
| `internal/bootstrap/bootstrap.go` | 透传 `WorkRoot` 到 orchestrator；启动时 `spillover.Sweep` |

## 风险与权衡

- **shell exit code 可见性**：spill 后 exit code 在 temp 文件末尾。预览含 tail ~10 行通常能盖到 JSON 末尾的 `"exit": N`；若不够，模型 fs_read 尾部几行即可。接受这个权衡以保持统一性（不为 shell_run 特判）。
- **spillover 目录膨胀**：单进程 + 启动 sweep 控制；多窗口（多进程）共享同一 project root 时各自 sweep 不冲突（只清自己的 spillover 目录内容，文件名带时间戳不互相覆盖）。可接受。
- **统一阈值 vs 子代理结果**：`analysis`/`agent_start` 的长结果也会被 spill——这是期望行为（子代理报告超长也应落盘），且 summarize 可压缩它。
- **`fs_read` 单遍全扫**：为在 footer 报告 `totalLines`，即使小窗口也扫整文件一次（内存仍 ≤ 窗口）。大文件 + 小窗口会多读字节但不爆内存；若日后成为瓶颈，可改为遇 `end` 早停、footer 省略总数。可接受。
