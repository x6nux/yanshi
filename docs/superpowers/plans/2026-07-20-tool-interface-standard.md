# Tool Interface Standard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 yanshi 所有工具统一到 `Tool` 接口（`DisplayName` + `DefaultTimeout` + `Stream`），输出走固定字段 `ToolChunk{Text, Status, Result, Overwrite, Err}`，废弃 JSON 包装与 `ToolProgressCallback`/`lineProgressWriter`，subagent 进度改向喂 `Status`+`Text`，TUI 用一套固定模板渲染。

**Architecture:** `GuardedTool` 实现新 `Tool` 接口；`Stream` 是唯一执行入口（Authorize + ctx 超时 + 调 `StreamFunc`）；`InvokableRun` 是 `Stream` 的收集器（只拼接 `Result` 喂模型）。工具实现者写 `StreamFunc`——同步型用 `SyncStream` helper 包成单 chunk 流，流式型（shell_run）与活动型（subagent）手写 channel 推多 chunk。TUI 消费同一个 channel，按固定字段渲染（标题=`DisplayName()`+`Status` / 正文=`Text` / 标红=`Err`）。

**Tech Stack:** Go 1.26.4，Eino ADK（`tool.InvokableTool`/`schema.ToolInfo`），Bubble Tea TUI。Fake 优先测试（`einollm.FakeModel`、fake subagent runner）。

**Spec:** `docs/superpowers/specs/2026-07-20-tool-interface-standard-design.md`（commit `d7a364c`，分支 `tool-interface-standard`）。

**迁移模式（贯穿全 plan）：**
- 直接型工具的 `RunFunc func(ctx, argsJSON) (string, error)` → 用 `SyncStream` 包成 `StreamFunc`，返回值 `out` 同时推 `Text` 和 `Result`（内容相同，单一归属）；错误推 `Text`+`Result`+`Err`。
- 每个 `NewGuardedTool(...)` 调用点补 `display string`、`timeout time.Duration` 两个参数（紧挨 `name`/`desc`）。
- `Stream` 内部统一做 `Authorize` + `context.WithTimeout(DefaultTimeout())`，工具不再自己加超时。

---

## File Structure

| 文件 | 职责 | 改动 |
|---|---|---|
| `internal/tools/guard.go` | `ToolChunk`/`Tool`/`StreamFunc`/`SyncStream`/`GuardedTool` | 新增类型 + 改造基类 |
| `internal/tools/shell.go` | `shell_run` | 改流式 `StreamFunc`，删 `lineProgressWriter` |
| `internal/tools/fs.go`/`fs_edit.go`/`fs_patch.go`/`fs_diff.go` | fs 工具 | `SyncStream` 迁移 |
| `internal/tools/web.go` | `web_fetch` | `SyncStream` 迁移 |
| `internal/tools/memory.go`/`skill.go`/`time.go`/`vcs.go` | 小工具 | `SyncStream` 迁移 |
| `internal/tools/agent.go` | `agent_start`/`analysis`/`summarize`/`workflow_start` | 改活动型 `StreamFunc`（Status 统计 + Text 活动 + Result 结论） |
| `internal/tools/progress.go` | `ToolProgressCallback` | 废弃 |
| `internal/tools/subagent.go` | `SubAgentRunner`/`SubAgentEmit` | 进度回调改向喂父工具 |
| `internal/agent/orchestrator/orchestrator.go` | `runSubAgentTurn` | 进度回调接线（喂 `Status`+`Text`） |
| `internal/cli/tui/entries.go` | `toolEntry` 渲染 | 消费 `Stream` channel 固定模板 |
| `internal/cli/tui/*.go` | progress 接线 | 去 `ToolProgressCallback` |
| `internal/bootstrap/*.go` | 工具构造 | 补 display/timeout |

---

## Lane 1：接口与基类（`internal/tools/guard.go`）

### Task 1.1：定义 `ToolChunk` / `Tool` / `StreamFunc`

**Files:**
- Modify: `internal/tools/guard.go`（在 `RunFunc` 定义附近新增类型）

- [ ] **Step 1: 在 `guard_test.go` 写接口契约测试（先失败）**

```go
func TestToolChunkFields(t *testing.T) {
	c := ToolChunk{Text: "t", Status: "s", Result: "r", Overwrite: true, Err: nil}
	assert.Equal(t, "t", c.Text)
	assert.Equal(t, "s", c.Status)
	assert.Equal(t, "r", c.Result)
	assert.True(t, c.Overwrite)
}

func TestGuardedToolSatisfiesToolInterface(t *testing.T) {
	var _ Tool = (*GuardedTool)(nil)
}
```

- [ ] **Step 2: 运行测试，确认编译失败（`Tool`/`ToolChunk` 未定义）**

Run: `go test ./internal/tools -run 'TestToolChunkFields|TestGuardedToolSatisfiesToolInterface'`
Expected: 编译错误 `undefined: ToolChunk` / `undefined: Tool`

- [ ] **Step 3: 在 `guard.go` 加类型定义**

在 `RunFunc` 类型定义之前插入：

```go
// ToolChunk 是工具通过 Stream channel 推送的固定结构。所有工具一律产出此结构；
// TUI 与编排层各取固定字段，零 per-tool 特判。channel close = 工具结束。
//
// 字段单一归属（No Field Sharing）：每个字段只有一个消费者，绝不共享。
//   - Text      → TUI 下方正文区（按 Overwrite 追加/覆盖）
//   - Status    → TUI 右侧状态区（恒覆盖；直接型=简短指示 / subagent=统计摘要）
//   - Result    → 模型（拼接成模型结果）
//   - Overwrite → TUI 渲染模式标记（追加/覆盖），仅作用于 Text
//   - Err       → 内部标记（errcnt 熔断 + TUI 标红）；可读错误文本由工具经 Text+Result 推送
type ToolChunk struct {
	Text      string
	Status    string
	Result    string
	Overwrite bool
	Err       error
}

// StreamFunc 是工具的执行体：返回一个 channel，工具在 goroutine 里推 ToolChunk，
//结束时 close。模型路径（InvokableRun）和 TUI 路径都消费同一个 channel。
type StreamFunc func(ctx context.Context, argsJSON string) <-chan ToolChunk

// Tool 是所有 yanshi 工具的统一契约。所有工具一律实现，无长短任务分叉。
type Tool interface {
	tool.InvokableTool                                              // Info + InvokableRun（兼容 Eino/ADK）
	DisplayName() string                                            // ① TUI 块标题
	DefaultTimeout() time.Duration                                  // ③ 默认超时
	Stream(ctx context.Context, argsJSON string) <-chan ToolChunk   // ②④ 唯一执行入口
}
```

在 import 块加 `"time"`（若未有）。

- [ ] **Step 4: 运行测试，确认 `ToolChunk` 通过，但 `Tool` 接口断言仍失败（`GuardedTool` 还没实现 `DisplayName`/`DefaultTimeout`/`Stream`）**

Run: `go test ./internal/tools -run 'TestToolChunkFields|TestGuardedToolSatisfiesToolInterface'`
Expected: `TestToolChunkFields` PASS；`TestGuardedToolSatisfiesToolInterface` 编译失败（缺方法）。

- [ ] **Step 5: Commit**

```bash
git add internal/tools/guard.go internal/tools/guard_test.go
git commit -m "feat(tools): add ToolChunk/Tool/StreamFunc types"
```

---

### Task 1.2：`SyncStream` helper（同步逻辑 → `StreamFunc`）

**Files:**
- Modify: `internal/tools/guard.go`

- [ ] **Step 1: 写失败测试**

```go
func TestSyncStream_Success(t *testing.T) {
	sf := SyncStream(func(ctx context.Context, args string) (string, error) {
		return "hello", nil
	})
	ch := sf(context.Background(), "")
	var got []ToolChunk
	for c := range ch {
		got = append(got, c)
	}
	require.Len(t, got, 1)
	assert.Equal(t, "hello", got[0].Text)    // TUI
	assert.Equal(t, "hello", got[0].Result)  // 模型（单一归属：内容相同，字段分开）
	assert.Nil(t, got[0].Err)
}

func TestSyncStream_Error(t *testing.T) {
	sf := SyncStream(func(ctx context.Context, args string) (string, error) {
		return "", fmt.Errorf("boom")
	})
	ch := sf(context.Background(), "")
	var got []ToolChunk
	for c := range ch {
		got = append(got, c)
	}
	require.Len(t, got, 1)
	assert.Equal(t, "✗ boom", got[0].Text)   // 错误文本走 Text 给 TUI
	assert.Equal(t, "✗ boom", got[0].Result) // 同时走 Result 给模型
	assert.NotNil(t, got[0].Err)
}
```

- [ ] **Step 2: 运行，确认失败（`SyncStream` 未定义）**

Run: `go test ./internal/tools -run TestSyncStream`
Expected: 编译错误 `undefined: SyncStream`

- [ ] **Step 3: 实现 `SyncStream`**

在 `guard.go`（`StreamFunc` 定义之后）：

```go
// SyncStream 把一个同步函数 fn 包成 StreamFunc：在 goroutine 里跑 fn，把返回值同时
// 推进 Text（给 TUI）和 Result（给模型）——内容相同，字段分开（字段单一归属）。
// 错误推进 Text+Result（前缀 "✗ "）并置 Err。channel 在推完一片后 close。
//
// 这不是第二种工具构造——所有工具仍是 StreamFunc；SyncStream 只是消除同步型工具
// 手写 goroutine/channel 样板的 helper。需要流式（shell_run）或活动面板（subagent）
// 的工具直接手写 StreamFunc，不用 SyncStream。
func SyncStream(fn func(ctx context.Context, argsJSON string) (string, error)) StreamFunc {
	return func(ctx context.Context, argsJSON string) <-chan ToolChunk {
		ch := make(chan ToolChunk, 1)
		go func() {
			defer close(ch)
			out, err := fn(ctx, argsJSON)
			if err != nil {
				msg := "✗ " + err.Error()
				ch <- ToolChunk{Text: msg, Result: msg, Err: err}
				return
			}
			ch <- ToolChunk{Text: out, Result: out}
		}()
		return ch
	}
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/tools -run TestSyncStream`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tools/guard.go internal/tools/guard_test.go
git commit -m "feat(tools): add SyncStream helper (sync fn -> StreamFunc)"
```

---

### Task 1.3：`GuardedTool` 字段 + `NewGuardedTool` 新签名 + `DisplayName`/`DefaultTimeout`

**Files:**
- Modify: `internal/tools/guard.go`（`GuardedTool` 结构体 + `NewGuardedTool` + 新方法）

- [ ] **Step 1: 写失败测试**

```go
func TestGuardedToolDisplayNameAndTimeout(t *testing.T) {
	g := NewGuardedTool("shell_run", "Bash", "desc", 120*time.Second,
		params(map[string]*schema.ParameterInfo{}),
		SyncStream(func(ctx context.Context, a string) (string, error) { return "", nil }))
	assert.Equal(t, "Bash", g.DisplayName())
	assert.Equal(t, 120*time.Second, g.DefaultTimeout())
	// Info 仍用 name（给模型的标识），不是 display。
	info, err := g.Info(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "shell_run", info.Name)
}
```

- [ ] **Step 2: 运行，确认失败（签名不匹配）**

Run: `go test ./internal/tools -run TestGuardedToolDisplayNameAndTimeout`
Expected: 编译错误（`NewGuardedTool` 参数数量不符）

- [ ] **Step 3: 改 `GuardedTool` 结构体与构造函数**

把现有：
```go
type GuardedTool struct {
	name   string
	desc   string
	params *schema.ParamsOneOf
	run    RunFunc
}

func NewGuardedTool(name, desc string, params *schema.ParamsOneOf, run RunFunc) *GuardedTool {
	return &GuardedTool{name: name, desc: desc, params: params, run: run}
}
```

改成：
```go
type GuardedTool struct {
	name    string
	display string
	desc    string
	timeout time.Duration
	params  *schema.ParamsOneOf
	stream  StreamFunc
}

// NewGuardedTool builds a GuardedTool. display 是 TUI 块标题（如 "Bash"）；
// timeout 是默认执行超时（编排层据此给 ctx 加 deadline）；stream 是执行体。
func NewGuardedTool(name, display, desc string, timeout time.Duration,
	params *schema.ParamsOneOf, stream StreamFunc) *GuardedTool {
	return &GuardedTool{name: name, display: display, desc: desc, timeout: timeout, params: params, stream: stream}
}

// DisplayName returns the TUI-facing tool title.
func (g *GuardedTool) DisplayName() string { return g.display }

// DefaultTimeout returns the tool's default execution timeout.
func (g *GuardedTool) DefaultTimeout() time.Duration { return g.timeout }
```

保留 `Info(_)` 不变（仍返回 `Name: g.name`）。

> ⚠️ 此改动会让所有 `NewGuardedTool(...)` 调用点编译失败（缺 display/timeout，且第 4 参从 `RunFunc` 变 `StreamFunc`）。**这是预期的**——Lane 2/3 逐个修复。本 task 先让 `guard.go` 自身编译通过（用 Task 1.1 的 `_ = run` 删掉 `RunFunc` 类型，见下）。

- [ ] **Step 4: 删除旧的 `RunFunc` 类型（已被 `StreamFunc` 取代）**

在 `guard.go` 删除：
```go
// RunFunc is the body of a guarded invokable tool.
type RunFunc func(ctx context.Context, argsJSON string) (string, error)
```

- [ ] **Step 5: 运行 `guard_test.go` 的目标测试，确认通过；整个包暂时编译失败（其他工具未迁移）**

Run: `go test ./internal/tools -run TestGuardedToolDisplayNameAndTimeout`
Expected: PASS

Run: `go build ./internal/tools/...`
Expected: 编译失败（`shell.go`/`fs.go`/... 的 `NewGuardedTool` 调用 + `RunFunc` 引用）——这是预期的，Lane 2 修复。

- [ ] **Step 6: Commit（`guard.go` 自身正确，其余文件 Lane 2/3 修）**

```bash
git add internal/tools/guard.go internal/tools/guard_test.go
git commit -m "feat(tools): GuardedTool implements Tool (display/timeout/stream fields)"
```

---

### Task 1.4：`Stream` 方法（Authorize + ctx 超时 + 调 streamFunc）

**Files:**
- Modify: `internal/tools/guard.go`

- [ ] **Step 1: 写失败测试（权限拒绝 + 超时）**

```go
func TestGuardedToolStream_PermissionDenied(t *testing.T) {
	g := NewGuardedTool("shell_run", "Bash", "", 10*time.Second, nil,
		SyncStream(func(ctx context.Context, a string) (string, error) { return "ok", nil }))
	// 无 profile 绑定 → fail-closed。
	ch := g.Stream(context.Background(), "")
	c := <-ch
	assert.NotNil(t, c.Err)
	assert.Contains(t, c.Text, "permission denied")
}

func TestGuardedToolStream_AllowedPassesThrough(t *testing.T) {
	g := NewGuardedTool("shell_run", "Bash", "", 10*time.Second, nil,
		SyncStream(func(ctx context.Context, a string) (string, error) { return "ok", nil }))
	ctx := WithProfile(context.Background(), allowAllProfile())
	ch := g.Stream(ctx, "")
	var got []ToolChunk
	for c := range ch {
		got = append(got, c)
	}
	require.Len(t, got, 1)
	assert.Equal(t, "ok", got[0].Text)
}
```

> `allowAllProfile()` 是测试 helper，返回一个 tools 全允许、shell/fs/net 全允许的 `guard.PermissionProfile`。若 `helpers_test.go` 已有类似 helper 就复用；否则在该 task 里加：
> ```go
> func allowAllProfile() guard.PermissionProfile {
>     return guard.PermissionProfile{
>         Tools: guard.ToolsPolicy{Allow: []string{"*"}},
>         FS:    guard.FSPolicy{Read: []string{"**"}, Write: []string{"**"}},
>         Shell: guard.ShellPolicy{Allow: []string{"*"}},
>         Net:   guard.NetPolicy{Allow: []string{"*"}},
>     }
> }
> ```
> （字段名以 `internal/guard` 现有 `PermissionProfile` 定义为准；执行时核对。）

- [ ] **Step 2: 运行，确认失败（`Stream` 方法未定义）**

Run: `go test ./internal/tools -run 'TestGuardedToolStream_'`
Expected: 编译错误 `g.Stream undefined`

- [ ] **Step 3: 实现 `Stream`**

在 `guard.go`（`InvokableRun` 之前）加：

```go
// Stream 是工具的唯一执行入口：权限检查 → ctx 超时 → 调 streamFunc。TUI 路径与模型
// 路径（InvokableRun）都过这里，因此权限只检查一次。streamFunc 必须响应 ctx.Done
// （超时/取消）及时收尾并 close channel。
func (g *GuardedTool) Stream(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ch := make(chan ToolChunk, 1)
	if err := Authorize(ctx, guard.Action{Tool: g.name}, argsJSON); err != nil {
		msg := "✗ permission denied: " + denyReason(err)
		ch <- ToolChunk{Text: msg, Result: msg, Err: err}
		close(ch)
		return ch
	}
	runCtx, cancel := context.WithTimeout(ctx, g.timeout)
	out := g.stream(runCtx, argsJSON)
	// 包装一层：runCtx 到期时 cancel（streamFunc 内部应已响应，cancel 是兜底防泄漏）。
	wrapped := make(chan ToolChunk, 16)
	go func() {
		defer cancel()
		defer close(wrapped)
		for c := range out {
			wrapped <- c
		}
	}()
	return wrapped
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/tools -run 'TestGuardedToolStream_'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tools/guard.go internal/tools/guard_test.go
git commit -m "feat(tools): GuardedTool.Stream (authorize + ctx timeout + streamFunc)"
```

---

### Task 1.5：`InvokableRun` 重写（收集 `Result` + errcnt + spill）

**Files:**
- Modify: `internal/tools/guard.go`（替换现有 `InvokableRun`）

- [ ] **Step 1: 写失败测试（模型只收 Result，Text/Status 不计入）**

```go
func TestInvokableRun_OnlyResultReachesModel(t *testing.T) {
	g := NewGuardedTool("x", "X", "", 10*time.Second, nil,
		func(ctx context.Context, args string) <-chan ToolChunk {
			ch := make(chan ToolChunk, 3)
			go func() {
				defer close(ch)
				ch <- ToolChunk{Text: "tui-only", Status: "running"}  // 不计模型
				ch <- ToolChunk{Result: "model-sees-this"}
			}()
			return ch
		})
	ctx := WithProfile(context.Background(), allowAllProfile())
	out, err := g.InvokableRun(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, "model-sees-this", out)  // 只 Result，不含 Text/Status
}

func TestInvokableRun_ErrorBecomesResultText(t *testing.T) {
	g := NewGuardedTool("x", "X", "", 10*time.Second, nil,
		func(ctx context.Context, args string) <-chan ToolChunk {
			ch := make(chan ToolChunk, 1)
			go func() { defer close(ch); ch <- ToolChunk{Text: "✗ boom", Result: "✗ boom", Err: fmt.Errorf("boom")} }()
			return ch
		})
	ctx := WithProfile(context.Background(), allowAllProfile())
	out, err := g.InvokableRun(ctx, "")
	require.NoError(t, err)         // 不返回 Go error（回喂模型）
	assert.Equal(t, "✗ boom", out)
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `go test ./internal/tools -run TestInvokableRun_`
Expected: FAIL（旧 `InvokableRun` 调 `g.run`，已不存在）

- [ ] **Step 3: 重写 `InvokableRun`**

把现有整个 `InvokableRun` 方法替换成：

```go
// InvokableRun 是 Eino/ADK 的模型入口：驱动 Stream，只收集 Result 字段拼成模型结果。
// Text/Status 均不计入（字段单一归属）。遇 Err 触发 errcnt 连续失败熔断（保留现状
// 语义，连续 5 次中断 turn）；错误文本已由工具经 Result 推送，故这里不再单独
// errorResult。spillIfTooLong 对最终拼装结果收口。
func (g *GuardedTool) InvokableRun(ctx context.Context, argsJSON string, _ ...tool.Option) (string, error) {
	ch := g.Stream(ctx, argsJSON)
	var result strings.Builder
	var runErr error
	for c := range ch {
		if c.Result != "" {
			result.WriteString(c.Result)
		}
		if c.Err != nil {
			runErr = c.Err
		}
	}
	if runErr != nil {
		var d *DenyErr
		if errors.As(runErr, &d) {
			// 权限拒绝已在 Stream 里写成 "✗ permission denied: ..." 进 Result；
			// 这里仍走 errcnt 让连续失败熔断生效。
		}
		if c := getErrCounter(ctx); c != nil {
			*c++
			if *c >= 5 {
				return "", fmt.Errorf("tool %q failed %d consecutive times; aborting turn", g.name, *c)
			}
		}
		return result.String(), nil
	}
	if c := getErrCounter(ctx); c != nil {
		*c = 0
	}
	return spillIfTooLong(ctx, g.name, result.String()), nil
}
```

import 加 `"strings"`（若未有）。删除旧 `errorResult` 函数（错误文本现由 `Stream`/`SyncStream`/工具直接写进 chunk，不再需要基类包装）。

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/tools -run TestInvokableRun_`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tools/guard.go internal/tools/guard_test.go
git commit -m "feat(tools): InvokableRun collects only Result (field single-owner)"
```

---

### Task 1.6：Lane 1 收尾验证

- [ ] **Step 1: 确认 `guard.go` + `guard_test.go` 编译通过，其余工具文件仍预期失败**

Run: `go vet ./internal/tools/guard.go`
Expected: `guard.go` 自身无错。

Run: `go build ./internal/tools/...`
Expected: 失败（`shell.go`/`fs.go`/`agent.go` 等仍用旧 `RunFunc`/旧 `NewGuardedTool`）——Lane 2/3 修复。

- [ ] **Step 2: 不 commit（无文件改动）**

---

## Lane 2：直接型工具迁移

> **通用迁移模式**（每个工具都一样）：
> 1. 把 `func (x *XTools) run(ctx, args) (string, error)` 的**调用**从 `NewGuardedTool(name, desc, params, x.run)` 改成 `NewGuardedTool(name, DISPLAY, desc, TIMEOUT, params, SyncStream(x.run))`——`run` 函数体**完全不动**（它仍是同步 `(string, error)`，由 `SyncStream` 包成 `StreamFunc`）。
> 2. 给每个工具定 `DISPLAY` 与 `TIMEOUT`（见下表）。
> 3. `run` 内部的 `return "", fmt.Errorf(...)` 保持——`SyncStream` 会把它转成 `✗ ...` 的 `Text`+`Result`+`Err`。
> 4. `run` 内部若原本 `return toJSON(map[string]any{...}), nil`（如 shell_run 的 `{output,exit,duration_ms}`），改成返回**纯文本**（去 JSON 包装）——见各工具的"去 JSON"说明。
> 5. 删除工具内部自己加的 `context.WithTimeout`（`Stream` 已统一加超时）。
>
> 每个 task 都先改构造点（让编译过），再改 `run` 内部的 JSON→纯文本，跑现有测试，更新断言（输出格式变了），commit。

| 工具 | DisplayName | DefaultTimeout | 去 JSON 改动 |
|---|---|---|---|
| `fs_read`/`fs_write` | `Read`/`Write` | 30s | 无（已是纯文本） |
| `fs_edit`/`fs_patch`/`fs_diff` | `Edit`/`Patch`/`Diff` | 30s | 无 |
| `fs_list`/`fs_glob`/`fs_search` | `List`/`Glob`/`Search` | 30s | 无 |
| `web_fetch` | `Fetch` | 60s | 若返回 JSON 包装，改纯文本 |
| `memory`/`skill`/`time`/`vcs` | `Memory`/`Skill`/`Time`/`VCS` | 30s/30s/5s/60s | 视现有实现 |

### Task 2.1：迁移 fs 工具（`fs.go` / `fs_edit.go` / `fs_patch.go` / `fs_diff.go`）

**Files:**
- Modify: `internal/tools/fs.go:33-100`（`NewFSTools` 里 7 个 `NewGuardedTool` 调用）
- Modify: `internal/tools/fs_edit.go` / `fs_patch.go` / `fs_diff.go`（各自的构造点）

- [ ] **Step 1: 改 `fs.go` 的 `NewFSTools` 构造点**

把每个 `NewGuardedTool(name, desc, params(...), f.runX)` 改成 `NewGuardedTool(name, DISPLAY, desc, 30*time.Second, params(...), SyncStream(f.runX))`。例如 `f.Read`：

```go
f.Read = NewGuardedTool(
	"fs_read", "Read",
	"Read a file ...（保持原 desc）",
	30*time.Second,
	params(map[string]*schema.ParameterInfo{
		"path":   {Type: schema.String, Desc: "...", Required: true},
		"offset": {Type: schema.Integer, Desc: "..."},
		"end":    {Type: schema.Integer, Desc: "..."},
	}),
	SyncStream(f.runRead),
)
```

对其余 6 个（`fs_write`→`Write`、`fs_edit`→`Edit`、`fs_list`→`List`、`fs_glob`→`Glob`、`fs_search`→`Search`、`fs_patch`→`Patch`）做同样改动。`fs_edit.go`/`fs_patch.go`/`fs_diff.go` 里的构造点同理。

- [ ] **Step 2: 确认 `runRead`/`runWrite`/... 函数体无需改（它们已返回纯文本，`SyncStream` 包裹即可）**

Run: `go build ./internal/tools/...`
Expected: `fs*.go` 编译通过（`shell.go`/`agent.go` 仍失败，预期）。

- [ ] **Step 3: 跑 fs 测试，确认行为不变（输出仍是纯文本）**

Run: `go test ./internal/tools -run 'TestFS|TestRead|TestEdit|TestPatch|TestDiff'`
Expected: PASS（fs 工具原本就返回纯文本，`SyncStream` 透传）。

> 若有测试断言 `InvokableRun` 返回值含 `{"error":...}` JSON，改成断言 `✗ ...` 纯文本（fs 工具内部错误经 `SyncStream` 转 `✗ ` 前缀）。

- [ ] **Step 4: Commit**

```bash
git add internal/tools/fs.go internal/tools/fs_edit.go internal/tools/fs_patch.go internal/tools/fs_diff.go internal/tools/fs_test.go internal/tools/fs_edit_test.go internal/tools/fs_patch_test.go internal/tools/fs_diff_test.go
git commit -m "feat(tools): migrate fs tools to Stream (SyncStream, display/timeout)"
```

---

### Task 2.2：`shell_run` 改流式 `StreamFunc`（逐行 `Text` + `Status` + exit footer）

**Files:**
- Modify: `internal/tools/shell.go`（`run` → `stream`，删 `lineProgressWriter`）

- [ ] **Step 1: 改 `shell_test.go` 断言（输出格式从 JSON 改纯文本）**

现有测试断言 `run()` 返回 `{"output":...,"exit":N,"duration_ms":...}` JSON。改成断言新行为：`stream()` 返回的 channel 逐片推 `Text`（stdout 行）+ 末片含 exit footer。示例：

```go
func TestShellRun_StreamsLinesAndExitFooter(t *testing.T) {
	s := NewShellTools(".")
	ctx := WithProfile(context.Background(), allowAllProfile())
	ch := s.Run.Stream(ctx, `{"command":"echo hello"}`)
	var text, result string
	for c := range ch {
		text += c.Text
		result += c.Result
	}
	assert.Contains(t, text, "hello")               // stdout 进 Text（TUI）
	assert.Contains(t, text, "exit 0")              // exit footer 在 Text 末尾
	assert.Contains(t, result, "hello")             // 同时进 Result（模型）
	assert.Contains(t, result, "exit 0")
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `go test ./internal/tools -run TestShellRun_StreamsLinesAndExitFooter`
Expected: FAIL（`s.Run.Stream` 存在但 `run` 仍返回 JSON）

- [ ] **Step 3: 重写 `shell.go` 的 `run` 为 `stream`（StreamFunc）**

把 `func (s *ShellTools) run(ctx, argsJSON) (string, error)` 改成 `func (s *ShellTools) stream(ctx, argsJSON) <-chan ToolChunk`，核心逻辑（参数解析、safe 命令检查、Authorize、`shellCommand`、`cmd.Run`）保留，但：
- 删除 `context.WithTimeout`（`Stream` 已加）。
- 删除 `lineProgressWriter` 及其用法。
- 用一个 `bufio.Scanner`（或逐行读 `cmd.StdoutPipe`）扫 stdout+stderr，每行推 `ToolChunk{Text: line, Result: line}`。
- 周期性（每秒）推 `ToolChunk{Status: "运行中·Xs"}`（用 `time.Tick` 在单独 goroutine，直到命令结束）。
- 命令结束：根据 exit code / err 推末片 `ToolChunk{Text: footer, Result: footer}`，footer = `── exit N · Xs ──`；出错置 `Err`。

骨架：
```go
func (s *ShellTools) stream(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ch := make(chan ToolChunk, 32)
	go func() {
		defer close(ch)
		var a shellRunArgs
		if err := ParseArgs(argsJSON, &a); err != nil {
			pushErr(ch, err); return
		}
		// ... safe command / metachar / Authorize 检查（保留现状）...
		// 失败时 pushErr(ch, fmt.Errorf("..."))

		cmd := shellCommand(ctx, a.Env, a.Command)
		cmd.Dir = wd(a.Workdir, s.root)
		cmd.WaitDelay = 5 * time.Second
		stdout, _ := cmd.StdoutPipe()
		cmd.Stderr = cmd.Stdout // 合并 stderr 到 stdout
		start := time.Now()
		if err := cmd.Start(); err != nil { pushErr(ch, err); return }

		// 状态 tick（每秒推 Status）
		ticker := time.NewTicker(time.Second)
		go func() {
			for {
				select {
				case <-ctx.Done(): ticker.Stop(); return
				case <-ticker.C:
					select {
					case ch <- ToolChunk{Status: "运行中·" + formatDur(time.Since(start))}:
					default: // 非阻塞，避免拖慢 stdout
					}
				}
			}
		}()

		// 逐行读 stdout
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			ch <- ToolChunk{Text: line + "\n", Result: line + "\n"}
		}
		err := cmd.Wait()
		// footer
		exit := exitCodeOf(err, ctx)
		footer := fmt.Sprintf("── exit %d · %s ──\n", exit, formatDur(time.Since(start)))
		if err != nil && exit == 0 {
			pushErr(ch, err); return
		}
		ch <- ToolChunk{Text: footer, Result: footer}
	}()
	return ch
}
```

> `pushErr(ch, err)` = `ch <- ToolChunk{Text: "✗ "+err.Error(), Result: "✗ "+err.Error(), Err: err}`（抽成 helper，复用）。`formatDur`/`exitCodeOf` 是小 helper，在本文件加。

构造点改成：
```go
s.Run = NewGuardedTool("shell_run", "Bash", DESC, 120*time.Second, params(...), s.stream)
```

删除 `lineProgressWriter` 类型与 `flush` 方法。

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/tools -run TestShellRun`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tools/shell.go internal/tools/shell_test.go
git commit -m "feat(tools): shell_run streams Text lines + Status + exit footer (no JSON)"
```

---

### Task 2.3：迁移 `web_fetch` / `memory` / `skill` / `time` / `vcs`

**Files:**
- Modify: `internal/tools/web.go` / `memory.go` / `skill.go` / `time.go` / `vcs.go`

- [ ] **Step 1: 对每个文件，把构造点 `NewGuardedTool(name, desc, params, x.run)` 改成 `NewGuardedTool(name, DISPLAY, desc, TIMEOUT, params, SyncStream(x.run))`**

DisplayName/Timeout 见 File Structure 表。`run` 函数体若返回 JSON 包装（如 `web_fetch` 返回 `{"url":...,"body":...}`），改成纯文本（body 前加一行 url 或直接 body）。

- [ ] **Step 2: 跑各工具测试，更新断言（JSON→纯文本）**

Run: `go test ./internal/tools -run 'TestWeb|TestMemory|TestSkill|TestTime|TestVCS'`
Expected: PASS（更新断言后）

- [ ] **Step 3: 确认整个 `internal/tools` 包编译通过（除 agent.go 的 subagent 类）**

Run: `go build ./internal/tools/...`
Expected: 失败仅剩 `agent.go`（Lane 3 修）。

- [ ] **Step 4: Commit**

```bash
git add internal/tools/web.go internal/tools/memory.go internal/tools/skill.go internal/tools/time.go internal/tools/vcs.go internal/tools/web_test.go internal/tools/memory_test.go internal/tools/skill_test.go internal/tools/time_test.go internal/tools/vcs_test.go
git commit -m "feat(tools): migrate web/memory/skill/time/vcs to Stream (SyncStream)"
```

---

### Task 2.4：删除 `lineProgressWriter` 残留 + `progress.go` 标记废弃

**Files:**
- Modify: `internal/tools/shell.go`（确认 `lineProgressWriter` 已删）
- Modify: `internal/tools/progress.go`（标记 `ToolProgressCallback` 废弃）

- [ ] **Step 1: 确认 `shell.go` 无 `lineProgressWriter` 引用**

Run: `go build ./internal/tools/...`
Expected: 成功（除 agent.go）。

- [ ] **Step 2: 在 `progress.go` 给 `ToolProgressCallback` 加废弃注释（Lane 4 彻底删）**

```go
// Deprecated: 工具输出统一走 Tool.Stream channel。ToolProgressCallback 保留至 Lane 4
// TUI 渲染迁移完成后再删。新代码不要绑定它。
type ToolProgressCallback func(toolName, text string)
```

- [ ] **Step 3: Commit**

```bash
git add internal/tools/progress.go
git commit -m "chore(tools): deprecate ToolProgressCallback (replaced by Tool.Stream)"
```

---

### Task 2.5：错误消息去掉内部工具名前缀（需求 ②"不暴露内部工具名"）

**Files:**
- Modify: `internal/tools/shell.go`/`fs.go`/`fs_edit.go`/`fs_patch.go`/`web.go`/...（所有 `run`/`stream` 函数体的 `fmt.Errorf`）

- [ ] **Step 1: grep 定位带工具名前缀的错误消息**

Run: `grep -rn 'fmt:Errorf("[a-z_]\+:' internal/tools/` （匹配 `shell_run: ` / `fs_read: ` 等前缀；正则按 ripgrep 语法调）

- [ ] **Step 2: 逐条去掉工具名前缀，描述只讲问题本身**

例如 `shell.go`：
- `fmt.Errorf("shell_run: safe command must not contain shell metacharacters (| ; > < && ||)")` → `fmt.Errorf("命令不得包含 shell 元字符 (| ; > < && ||)")`
- `fmt.Errorf("shell_run: '..' path traversal is not allowed; use paths relative to the work root")` → `fmt.Errorf("'..' 路径穿越不允许；请用相对 work root 的路径")`

对 grep 出的每条做同样处理（去 `<toolname>:` 前缀，块标题已用 `DisplayName` 昭示）。

- [ ] **Step 3: 更新测试断言（错误文本不含工具名）**

Run: `go test ./internal/tools/...`
更新任何断言错误消息含 `shell_run:`/`fs_read:` 等前缀的测试。

- [ ] **Step 4: Commit**

```bash
git add internal/tools/
git commit -m "feat(tools): strip internal tool names from error messages"
```

---

## Lane 3：subagent 类 + 进度回调

### Task 3.1：`agent_start` 改活动型 `StreamFunc`

**Files:**
- Modify: `internal/tools/agent.go`（`runStartAgent` → `streamStartAgent`）

- [ ] **Step 1: 写失败测试（agent_start 推 Status 统计 + Text 工具调用 + Result 结论）**

```go
func TestAgentStart_StreamsStatusActivityAndResult(t *testing.T) {
	// fake runner：模拟子代理调了 fs_read、fs_list 两个工具，每调一个回调一次进度。
	at := NewAgentTools(fakeModel())
	ctx := WithProfile(context.Background(), allowAllProfile())
	ctx = WithSubAgentRunner(ctx, fakeRunnerThatCallsTools("fs_read", "fs_list", "done-result"))
	ch := at.StartAgent.Stream(ctx, `{"prompt":"do it"}`)
	var text, result, lastStatus string
	for c := range ch {
		text += c.Text
		result += c.Result
		if c.Status != "" { lastStatus = c.Status }
	}
	assert.Contains(t, lastStatus, "2 tools")         // Status 统计（累计 2 次调用）
	assert.Contains(t, text, "Read(")                  // Text 活动详情（工具调用记录）
	assert.Contains(t, text, "List(")
	assert.Equal(t, "done-result", result)             // Result 最终结论
}
```

> `fakeRunnerThatCallsTools(...)` 是测试 fake：返回一个 `SubAgentRunner`，它模拟子代理按顺序调用给定工具（每调一个经进度回调通知父），最后返回给定结论文本。在 `agent_test.go` 加。

- [ ] **Step 2: 运行，确认失败**

Run: `go test ./internal/tools -run TestAgentStart_StreamsStatusActivityAndResult`
Expected: FAIL（`streamStartAgent` 不存在 / 旧 `runStartAgent` 返回 JSON）

- [ ] **Step 3: 实现 `streamStartAgent`**

把 `runStartAgent(ctx, argsJSON) (string, error)` 改成 `streamStartAgent(ctx, argsJSON) <-chan ToolChunk`。骨架：

```go
func (t *AgentTools) streamStartAgent(ctx context.Context, argsJSON string) <-chan ToolChunk {
	ch := make(chan ToolChunk, 16)
	go func() {
		defer close(ch)
		var a agentStartArgs
		if err := ParseArgs(argsJSON, &a); err != nil { pushErr(ch, err); return }
		if a.Prompt == "" { pushErr(ch, fmt.Errorf("prompt must not be empty")); return }
		if t.chatModel == nil { pushErr(ch, fmt.Errorf("no chat model configured")); return }
		allowed, perr := parseToolList(a.Tools)
		if perr != nil { pushErr(ch, perr); return }

		// 进度回调：子代理每调一个工具 / 每秒计时，重算 Status（统计）+ Text（活动）。
		start := time.Now()
		var toolCalls int
		var tokens int
		progress := func(ev SubAgentEvent) {
			switch ev.Kind {
			case SubAgentToolStart:
				toolCalls++
				ch <- ToolChunk{Text: fmt.Sprintf("%s(%s) …\n", ev.ToolDisplay, ev.ToolArgs)} // 追加活动行
				ch <- ToolChunk{Status: fmt.Sprintf("%d tools %dk %s", toolCalls, tokens/1000, formatDur(time.Since(start)))}
			case SubAgentTokens:
				tokens += ev.Tokens
				ch <- ToolChunk{Status: fmt.Sprintf("%d tools %dk %s", toolCalls, tokens/1000, formatDur(time.Since(start)))}
			}
		}
		ctx = WithSubAgentProgress(ctx, progress)

		result, err := t.runSubAgent(WithLeafSubAgentTools(ctx), a.Prompt, allowed, a.Instruction)
		if err != nil { pushErr(ch, err); return }
		ch <- ToolChunk{Result: result} // 最终结论仅给模型
	}()
	return ch
}
```

> `SubAgentEvent`/`SubAgentToolStart`/`SubAgentTokens`/`WithSubAgentProgress`/`SubAgentProgressFromContext` 是 Lane 3 新增的进度回调类型（Task 3.4 定义）。`formatDur`、`pushErr` 复用。

构造点改成：
```go
t.StartAgent = NewGuardedTool("agent_start", "Agent", DESC, 600*time.Second, params(...), t.streamStartAgent)
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/tools -run TestAgentStart_StreamsStatusActivityAndResult`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tools/agent.go internal/tools/agent_test.go
git commit -m "feat(tools): agent_start streams Status(stats)+Text(activity)+Result(conclusion)"
```

---

### Task 3.2：`analysis` / `summarize` 改活动型（同 `agent_start` 模式）

**Files:**
- Modify: `internal/tools/agent.go`（`runAnalysis`/`runAnalysisAgent`/`runSummarize` → stream 版）

- [ ] **Step 1: 改 `runAnalysisAgent` / `runSummarize` 为 stream 版**

与 Task 3.1 同模式：`WithSubAgentProgress` 绑定进度回调，Status 推 `<N> tools <X>k <Y>s`，Text 追加工具调用行，Result 推报告文本。`runAnalysis`（mode 分发）的 stream 版只是把 `runAnalysisAgent`/`runAnalysisWorkflow` 的 stream 透传。

构造点：`analysis`→`Analysis`/600s，`summarize`→`Summarize`/300s。

- [ ] **Step 2: 跑测试，更新断言**

Run: `go test ./internal/tools -run 'TestAnalysis|TestSummarize'`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/tools/agent.go internal/tools/agent_test.go
git commit -m "feat(tools): analysis/summarize stream Status+Text+Result"
```

---

### Task 3.3：`workflow_start` 改活动型（多 Agent 面板）

**Files:**
- Modify: `internal/tools/agent.go`（`runFlatWorkflow`/`runDAGWorkflow` → stream 版）

- [ ] **Step 1: 写失败测试（workflow Status 推 done/total 统计 + Text 覆盖子Agent 面板）**

```go
func TestWorkflow_StreamsProgressPanelAndSynthesisResult(t *testing.T) {
	// fake：3 个 step，每 step 跑一个子代理，各回调若干工具调用。
	ch := wf.Stream(ctx, `{"workflow":"{\"steps\":[...]}"}`)
	var text, result, lastStatus string
	for c := range ch {
		if c.Text != "" { text = c.Text }      // workflow Text 是覆盖（面板），取最后
		result += c.Result
		if c.Status != "" { lastStatus = c.Status }
	}
	assert.Contains(t, lastStatus, "/3 agents")   // done/total
	assert.Contains(t, text, "Agent(")            // 子Agent 面板行
	assert.Contains(t, result, "synthesis")       // 仅合成步结论
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `go test ./internal/tools -run TestWorkflow_StreamsProgressPanelAndSynthesisResult`
Expected: FAIL

- [ ] **Step 3: 改 `runFlatWorkflow`/`runDAGWorkflow` 为 stream 版**

核心：保留 DAG 调度逻辑，但：
- 每个子 Agent 用 `WithSubAgentProgress` 绑一个**带 step ID 的**进度回调，回调时更新该 step 的 `{当前工具, 调用次数, token, 时长}` 状态表。
- 状态表变化时，重算 Text 面板（每个执行中 step 一行 `Agent(<当前工具>) <status>`，`Overwrite=true`）+ Status（`<done>/<total> agents <X>k <Y>s`）推送。
- 用现有的 `reportProgress`（makeNestedProgressEmitter 的逻辑）累计 done。
- 结束时：Result 只推**合成步**（C1 或最后汇总步）的结论；无合成步则 Result 推空或概要（依 workflow 结构）。

> 关键：现有 `reportProgress := makeNestedProgressEmitter(ctx, total)` 改成更新内部状态表 + 推 chunk，而不是发 transport 帧。

构造点：`workflow_start`→`Workflow`/1800s。

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/tools -run 'TestWorkflow|TestDAG'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tools/agent.go internal/tools/agent_test.go
git commit -m "feat(tools): workflow_start streams Status(progress)+Text(agent panel)+Result(synthesis)"
```

---

### Task 3.4：进度回调类型（`SubAgentEvent` / `WithSubAgentProgress`）

**Files:**
- Modify: `internal/tools/subagent.go`（新增进度回调 ctx 注入）

- [ ] **Step 1: 写失败测试**

```go
func TestSubAgentProgressFromContext_RoundTrip(t *testing.T) {
	var got []SubAgentEvent
	ctx := WithSubAgentProgress(context.Background(), func(ev SubAgentEvent) { got = append(got, ev) })
	cb := SubAgentProgressFromContext(ctx)
	require.NotNil(t, cb)
	cb(SubAgentEvent{Kind: SubAgentToolStart, ToolDisplay: "Read", ToolArgs: "x.go"})
	cb(SubAgentEvent{Kind: SubAgentTokens, Tokens: 500})
	require.Len(t, got, 2)
	assert.Equal(t, "Read", got[0].ToolDisplay)
	assert.Equal(t, 500, got[1].Tokens)
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `go test ./internal/tools -run TestSubAgentProgressFromContext_RoundTrip`
Expected: 编译错误（类型未定义）

- [ ] **Step 3: 在 `subagent.go` 加类型**

```go
// SubAgentEventKind 枚举子代理进度事件种类。
type SubAgentEventKind int

const (
	SubAgentToolStart SubAgentEventKind = iota  // 子代理开始调用某工具
	SubAgentToolEnd                              // 调用结束
	SubAgentTokens                               // 模型返回，token 计费
)

// SubAgentEvent 是子代理执行中向父工具上报的进度事件。父工具据此重算 Status/Text。
type SubAgentEvent struct {
	Kind        SubAgentEventKind
	ToolDisplay string  // 当前工具的 DisplayName（SubAgentToolStart/End）
	ToolArgs    string  // 工具调用参数摘要（SubAgentToolStart）
	Tokens      int     // 本次模型调用消耗（SubAgentTokens）
}

type subAgentProgressKey struct{}

// WithSubAgentProgress 绑定进度回调。nil cb 则不改 ctx（子代理不上报，父工具静默）。
func WithSubAgentProgress(ctx context.Context, cb func(SubAgentEvent)) context.Context {
	if cb == nil {
		return ctx
	}
	return context.WithValue(ctx, subAgentProgressKey{}, cb)
}

// SubAgentProgressFromContext 返回绑定的回调，或 nil。
func SubAgentProgressFromContext(ctx context.Context) func(SubAgentEvent) {
	cb, _ := ctx.Value(subAgentProgressKey{}).(func(SubAgentEvent))
	return cb
}
```

- [ ] **Step 4: 运行测试，确认通过**

Run: `go test ./internal/tools -run TestSubAgentProgressFromContext_RoundTrip`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tools/subagent.go internal/tools/subagent_test.go
git commit -m "feat(tools): SubAgentProgress callback ctx injection"
```

---

### Task 3.5：orchestrator `runSubAgentTurn` 接线进度回调（喂父工具）

**Files:**
- Modify: `internal/agent/orchestrator/orchestrator.go`（`runSubAgentTurn`）
- Modify: `internal/agent/orchestrator/orchestrator_test.go`（更新 `TestSubAgentRunner_ForwardsEventsToEmit` 等）

- [ ] **Step 1: 读 `runSubAgentTurn` 当前实现，定位工具调用事件源**

Run: `git grep -n 'runSubAgentTurn' internal/agent/orchestrator`
读该函数，找到子代理 ReAct 循环里"工具调用开始/结束"与"模型 token 计费"的事件来源（现有 `nested_usage`/`nested_progress` 的发射点就是这些事件源）。

- [ ] **Step 2: 改 `runSubAgentTurn`：在工具调用起止与 token 计费处，调 `SubAgentProgressFromContext(ctx)` 上报**

```go
// 工具调用开始处
if cb := tools.SubAgentProgressFromContext(ctx); cb != nil {
	cb(tools.SubAgentEvent{Kind: tools.SubAgentToolStart, ToolDisplay: toolDisplayName(call), ToolArgs: argSummary(call)})
}
// ... 执行工具 ...
// 工具调用结束处
if cb := tools.SubAgentProgressFromContext(ctx); cb != nil {
	cb(tools.SubAgentEvent{Kind: tools.SubAgentToolEnd})
}
// 模型返回后
if cb := tools.SubAgentProgressFromContext(ctx); cb != nil {
	cb(tools.SubAgentEvent{Kind: tools.SubAgentTokens, Tokens: tokensFromUsage(usage)})
}
```

- [ ] **Step 3: 更新 orchestrator 测试**

`TestSubAgentRunner_ForwardsEventsToEmit` / `TestSubAgentRunner_EmitsNestedUsage` 改成断言 `SubAgentProgress` 回调被触发（而非 transport 帧）。保留 `TestSubAgentRunner_NoEmitDegradesGracefully`（无回调时静默）。

Run: `go test ./internal/agent/orchestrator -run TestSubAgentRunner`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/agent/orchestrator/orchestrator.go internal/agent/orchestrator/orchestrator_test.go
git commit -m "feat(orchestrator): runSubAgentTurn reports progress via SubAgentProgress"
```

---

### Task 3.6：废弃 `nested_usage` / `nested_progress` transport 帧

**Files:**
- Modify: `internal/proto/frame.go`（标记帧废弃，或删除）
- Modify: `internal/agent/orchestrator/orchestrator.go`（删 `makeNestedProgressEmitter`/`SubAgentEmit` 的 nested 帧发射）
- Modify: `internal/tools/agent.go`（`makeNestedProgressEmitter` 改成更新状态表，已 在 Task 3.3 部分完成；此处清理残留）

- [ ] **Step 1: 确认 transport 帧已无生产者（grep）**

Run: `git grep -n 'nested_usage\|nested_progress\|NewNestedProgress' internal/`
Expected: 仅剩定义与测试引用。

- [ ] **Step 2: 删除 `NewNestedProgress`/`nested_usage` 帧的生产代码，保留类型定义（过渡）或一并删（若 TUI 已不消费）**

> 决策：若 Lane 4 后 TUI 不再消费这些帧，从 `proto/frame.go` 删除；否则保留类型但标 deprecated。执行时确认 TUI 渲染路径。

- [ ] **Step 3: 跑全量 orchestrator + proto 测试**

Run: `go test ./internal/agent/orchestrator/... ./internal/proto/...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/proto/frame.go internal/agent/orchestrator/orchestrator.go internal/tools/agent.go
git commit -m "refactor(proto): drop nested_usage/nested_progress frames (replaced by SubAgentProgress)"
```

---

## Lane 4：TUI 渲染重写

### Task 4.1：`toolEntry` 消费 `Stream` channel（固定模板）

**Files:**
- Modify: `internal/cli/tui/entries.go`（`toolEntry` 结构 + `renderToolHeader` + 各 render 方法）
- Modify: `internal/cli/tui/events.go`（工具 chunk 事件处理）

- [ ] **Step 1: 读 `entries.go` 当前 `toolEntry` 结构与 `toolDisplayName`**

Run: 读 `internal/cli/tui/entries.go:1-130`（`toolEntry` 定义、`toolDisplayName`、`renderToolHeader`）。
确认 `toolEntry` 字段（`name`/`progress`/`args`/...）与渲染方法（`renderRunning`/`renderDone`/...）。

- [ ] **Step 2: 改 `toolEntry` 字段以承载 chunk 累积状态**

```go
type toolEntry struct {
	name     string        // 工具内部名（如 shell_run）
	display  string        // DisplayName（如 Bash）—— 从 Tool.DisplayName() 取
	textBuf  strings.Builder  // Text 累积（追加 or 覆盖，按 Overwrite）
	status   string        // 最新 Status（覆盖）
	result   strings.Builder  // 仅用于最终摘要（可选）
	err      error
	start    time.Time
	done     bool
	// ... 保留现有必要的样式/args 字段 ...
}
```

加方法：
```go
// applyChunk 按 ToolChunk 固定字段更新 entry（零 per-tool 特判）。
func (e *toolEntry) applyChunk(c tools.ToolChunk) {
	if c.Text != "" {
		if c.Overwrite {
			e.textBuf.Reset()
		}
		e.textBuf.WriteString(c.Text)
	}
	if c.Status != "" {
		e.status = c.status // 恒覆盖
	}
	if c.Err != nil {
		e.err = c.Err
	}
}
```

> 注：`tools.ToolChunk` 跨包引用——TUI 已 import `tools`（确认；若未 import 则加）。或把 `ToolChunk` 的渲染需要的字段通过 `cli.StreamEvent` 透传（见 Step 3）。

- [ ] **Step 3: 决定 chunk 如何从 WS 传到 TUI**

两条路：
- (a) WS handler 把 `ToolChunk` 转成新的 `cli.StreamEvent{Kind:"tool_chunk", Text, Status, Result, Overwrite, Err}` 帧发给 TUI；TUI `applyEvent` 调 `applyChunk`。
- (b) 直接传 `ToolChunk`。

推荐 (a)（保持 TUI 与 transport 解耦）。在 `internal/cli/stream.go`（或等价文件）加 `StreamEvent` 字段，WS handler（`internal/agent/orchestrator` 或 `internal/server`）消费 `Tool.Stream` channel 并逐 chunk 发 `tool_chunk` 帧。

- [ ] **Step 4: 改 `renderToolHeader` 用 `display` 而非 `toolDisplayName(name)`**

```go
func (e *toolEntry) renderToolHeader(sp spinner.Model) string {
	name := glyphStyle.Render(e.display + "()")
	status := ""
	if e.status != "" {
		status = " " + statusStyle.Render(e.status)  // 右侧
	}
	// ... 拼标题行：name + status ...
}
```

- [ ] **Step 5: 改各 render 方法：正文区显示 `e.textBuf.String()`（不再用 `e.progress`）**

把 `renderRunning`/`renderDone` 等里的 `strings.Join(e.progress, "\n")` 换成 `e.textBuf.String()`。删除 `e.progress` 字段（被 `textBuf` 取代）。

- [ ] **Step 6: 跑 TUI 渲染测试，更新断言**

Run: `go test ./internal/cli/tui -run 'TestRender|TestEntry|TestApply'`
Expected: PASS（更新断言后）

- [ ] **Step 7: Commit**

```bash
git add internal/cli/tui/entries.go internal/cli/tui/events.go internal/cli/stream.go internal/cli/tui/entries_test.go
git commit -m "feat(tui): tool blocks consume Stream chunks (fixed-template render)"
```

---

### Task 4.2：WS handler 消费 `Tool.Stream` 发 `tool_chunk` 帧

**Files:**
- Modify: WS handler（`internal/server/` 或 `internal/agent/orchestrator` 里绑定 `ToolProgressCallback` 的地方——grep 定位）

- [ ] **Step 1: grep 定位现有 `WithToolProgress` 绑定点**

Run: `git grep -n 'WithToolProgress' internal/`
找到 WS handler 里把 stdout 流推给 TUI 的位置。

- [ ] **Step 2: 改成消费 `Tool.Stream` channel，逐 chunk 发 `tool_chunk` `StreamEvent`**

```go
// 对每个工具调用：
ch := tool.Stream(ctx, argsJSON)
for c := range ch {
	emit(cli.StreamEvent{
		Kind: "tool_chunk",
		Text: c.Text, Status: c.Status, Result: c.Result,
		Overwrite: c.Overwrite, Err: c.Err,
		ToolName: tool.DisplayName(),  // TUI 用 display
	})
}
```

> 具体接线点取决于现有架构（WS handler 怎么驱动工具）。执行时读 `ws.go`/orchestrator 的工具调用点，把 `InvokableRun` 的调用拆成"先 Stream 给 TUI，同时收集 Result 给 ADK"。可能需要一个适配器：同时消费 channel（发 TUI 帧）+ 拼装 Result（喂 ADK）。

- [ ] **Step 3: 端到端验证（fake-model + inprocess）**

Run: `timeout 10 ./yanshi --fake-model -inprocess` （手动观察 TUI 工具块渲染：标题行 `Bash() 运行中·Xs` / `Agent() 2 tools 10k 1m9s`，正文区 stdout/活动面板）。

> alt-screen TUI 无法管道驱动，这步需人工目检；或写一个消费 `tool_chunk` 帧的 headless 测试。

- [ ] **Step 4: Commit**

```bash
git add internal/server/ws.go internal/cli/stream.go
git commit -m "feat(server): WS handler streams tool_chunk frames from Tool.Stream"
```

---

### Task 4.3：删除 `ToolProgressCallback` 及其接线

**Files:**
- Modify: `internal/tools/progress.go`（删 `ToolProgressCallback`/`WithToolProgress`/`ToolProgressFromContext`）
- Modify: 所有 `WithToolProgress` 引用点

- [ ] **Step 1: grep 确认无生产引用**

Run: `git grep -n 'ToolProgressCallback\|WithToolProgress\|ToolProgressFromContext' internal/`
Expected: 仅 `progress.go` 定义 + 可能的测试。

- [ ] **Step 2: 删除 `progress.go` 内容（或整个文件）**

```bash
git rm internal/tools/progress.go
```
（若 `progress_test.go` 存在也删。）

- [ ] **Step 3: 跑全量测试**

Run: `go test ./internal/tools/...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/tools/progress.go
git commit -m "refactor(tools): remove ToolProgressCallback (superseded by Tool.Stream)"
```

---

### Task 4.4：bootstrap 工具构造补 display/timeout（收尾）

**Files:**
- Modify: `internal/bootstrap/*.go`（若有直接构造工具的点）

- [ ] **Step 1: grep 确认所有 `NewGuardedTool` 调用都已带 display/timeout**

Run: `git grep -n 'NewGuardedTool' internal/`
逐个确认 6 参数齐全。bootstrap 若有装配点（如 `NewShellTools`/`NewFSTools`/`NewAgentTools` 的调用），确认它们间接已通过（这些构造函数内部已改）。

- [ ] **Step 2: 跑全量测试 + vet**

Run: `go vet ./... && go test ./...`
Expected: 全绿（除已知 e2e_real skip）。

- [ ] **Step 3: Commit（若有改动）**

```bash
git add internal/bootstrap/
git commit -m "chore(bootstrap): confirm tool constructors carry display/timeout"
```

---

## Self-Review

**1. Spec coverage：**
- ① 展示名 → Task 1.3（`DisplayName`）+ 全量迁移带 display。✓
- ② 输出非 JSON → Task 1.5（`InvokableRun` 收 Result）+ Task 2.1-2.3（去 JSON）+ Task 2.2（shell footer）。✓
- ③ 超时 → Task 1.4（`Stream` 加 ctx 超时）+ 各工具 DefaultTimeout。✓
- ④ 运行中状态 → Task 1.4（Stream channel）+ Task 3.4-3.5（进度回调）+ Task 4.1（TUI 固定模板）。✓
- 字段单一归属 → Task 1.1/1.5（模型只读 Result）。✓
- 上下文洁净 → Task 3.1-3.3（subagent 活动进 Text，结论进 Result）。✓
- 错误走 Text+Result+Err、不暴露内部名 → Task 1.2（`SyncStream` 的 `✗` 前缀）+ Task 2.x（各工具去 `shell_run:` 前缀）。⚠️ **注意**：Task 2.x 需在迁移时顺手去掉 `run` 函数体里错误消息的工具名前缀（如 `shell_run: safe command must not...` → `命令不得包含...`）。**补一个 task。**
- 废弃 `ToolProgressCallback`/`lineProgressWriter` → Task 2.2/2.4/4.3。✓
- `nested_usage`/`nested_progress` 改向 → Task 3.5/3.6。✓
- TUI 固定模板 → Task 4.1。✓

**补 Task 2.5：去内部工具名前缀（错误消息洁净）**

在 Lane 2 末尾加：
- 逐工具检查 `run`/`stream` 函数体的 `fmt.Errorf("shell_run: ...")`/`fmt.Errorf("fs_read: ...")` 等，去掉工具名前缀，描述只讲问题本身。grep `grep -rn 'fmt.Errorf("[a-z_]*:' internal/tools/` 定位。
- 跑测试更新断言（错误文本不含工具名）。

**2. Placeholder scan：** 无 TBD/TODO。"执行时核对"/"执行时确认"是合理的实现期指引（如 `PermissionProfile` 字段名核对），不是 placeholder。

**3. Type consistency：**
- `ToolChunk` 五字段（Text/Status/Result/Overwrite/Err）贯穿全 plan。✓
- `SyncStream` 签名一致。✓
- `SubAgentEvent`/`WithSubAgentProgress` 在 Task 3.4 定义，3.1/3.5 使用。✓
- `pushErr`/`formatDur`/`exitCodeOf` 是 lane 内 helper，定义点（shell.go/agent.go）明确。✓
- `NewGuardedTool` 新签名（name, display, desc, timeout, params, stream）贯穿。✓

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-20-tool-interface-standard.md`. Two execution options:

**1. Subagent-Driven (recommended)** - 每个 task 派一个 fresh subagent，task 间 review，快速迭代。

**2. Inline Execution** - 在当前会话用 executing-plans 批量执行，带 checkpoint review。

哪种？
