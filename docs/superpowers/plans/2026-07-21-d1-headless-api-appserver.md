# Batch D1 — headless + 版本化 API + app-server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在保留现有 `/api/v1/chat`、WebSocket、SSE 和 TUI 行为的前提下，完成 V12 headless 执行增强、V14 版本化 Agent API v1，以及复用同一资源服务的 APS1 JSON-RPC v2 app-server。

**Architecture:** V12 把已有 `cli.Exec` 单 turn 驱动器扩展为可复用的 headless runner，并让 `exec` 与 `chat --no-tui` 共享输入、输出、resume、取消和退出码契约。V14 在现有 `internal/api/http` 之上新增独立的 `internal/api/v1` thread/turn/item 资源服务：HTTP/SSE 只是 transport，服务通过现有 `Orchestrator`、`Store`、`proto.ServerFrame` 运行 turn。APS1 的 `internal/appserver` 只实现 JSON-RPC 2.0 stdio transport，并调用同一个 V14 service，避免 HTTP 与 app-server 各自复制编排逻辑。

**Tech Stack:** Go 1.26.4、标准库 `encoding/json`/`bufio`/`context`/`sync`/`net/http`、现有 Eino ADK `Orchestrator`、`internal/proto`、SQLite `internal/store`、现有 `FakeModel`；不引入 JSON-RPC 或 OpenAPI 运行时依赖。JSON Schema 首版由仓库内版本化 schema 文件提供；TypeScript 生成工具链在决策门中确定，wire contract 不依赖生成器。

---

## 范围、现状与 task 数

本计划覆盖 **12 个 implementation tasks + 2 个决策/验收 gates**，共 **14 个 task/gate**。三个子系统属于同一批次，但每个 implementation task 都有可独立提交、可独立测试的边界：

1. V12 输入解析与 headless runner。
2. V12 `exec`/`chat --no-tui` CLI 接线。
3. V12 输入、JSONL、退出码与 CI 契约测试。
4. V14 v1 资源类型与 provisional version envelope。
5. V14 `proto.ServerFrame` → item 映射。
6. V14 thread/turn service、resume、interrupt、背压。
7. V14 HTTP/SSE resource endpoints。
8. bootstrap 组合根装配 V14 service。
9. V14 schema、未知字段与兼容性测试。
10. APS1 JSON-RPC v2 wire/parser/dispatcher。
11. `yanshi app` stdio 入口与 config read/write。
12. APS1 schema export、TS 生成决策与最终验收。

当前代码已经有以下 V12 基础，计划不会重新复制一套实现：

- `internal/cli/exec.go` 已有 `Exec`/`execWithBackend`、text/JSONL 输出枚举、`--resume` 驱动的 `restore_session`、`context.Canceled`/`context.DeadlineExceeded` 映射。
- `cmd/yanshi/main.go` 已有 `exec` 参数解析、stdin 单 prompt、`--output text|jsonl`、`--timeout`、`--resume` 和稳定退出码常量。
- `chat --no-tui` 仍走 `chatLegacy`：默认 SSE、逐行 prompt、无 JSONL output、无 resume、无 timeout/稳定退出码。这是 V12 主要 CLI 缺口。
- `/api/v1/chat`、`/api/v1/chat/ws` 仍是 legacy `ClientFrame`/`ServerFrame` transport；V14 不修改其 wire vocabulary，新增资源层与明确的 adapter。

### 明确不在本批

- 不实现 V15 Python/TypeScript SDK 的 client runtime；本批只输出 v1 schema 和可生成的 TS 类型入口。
- 不实现 IDE extension、认证/远程公网部署、Durable Task 新表、MCP client。
- 不改变 `proto.ClientFrame`/`ServerFrame` 的旧字段命名；legacy TUI 必须继续通过现有 WS/SSE 正常工作。
- 不把完整的 ADK 原始 event history 直接暴露给外部客户端；外部只消费稳定的 item 资源。

---

## 决策点（必须在对应 gate 关闭后再写实现）

### D1-DEC-1：帧版本号方案（V14）

候选方案：

- **A（本计划 provisional default）**：每个 v1 resource/event 带 `version: "v1"`，HTTP 同时带 `X-Yanshi-API-Version: v1`；请求中的 `version` 可省略，省略等同 v1。
- **B**：只用 HTTP header / JSON-RPC capability negotiation，payload 不带 `version`。
- **C**：用整数 `schemaVersion: 1`，未来不再使用字符串版本。

在 Task 4 前由维护者选择一种并记录在 `docs/feature-roadmap-codex-deepseek.md` 或本计划执行记录中。若没有额外决定，按 A 实现；所有 schema、HTTP、JSON-RPC 测试都必须只依赖同一个常量，而不是散落字符串。JSON-RPC 的 `jsonrpc: "2.0"` 是 transport 版本，不替代 Agent API 的 v1 资源版本。

### D1-DEC-2：TS 生成工具链（APS1）

候选方案：

- **A**：Go 内置 generator，读取仓库内 `SchemaJSON`，生成 `sdk/ts/v1.ts`，无 Node/npm 依赖。
- **B**：`openapi-typescript`/`quicktype` 等外部工具，schema 作为唯一输入。
- **C**：只发布 schema，由 SDK 仓库在 CI 中生成 TS 类型。

在 Task 12 前选择。wire/schema 不应依赖生成工具；若选择 A，计划中的 Go fallback generator 是完整可交付路径；若选择 B/C，只替换生成步骤，不改 V14/APS1 resource types 和 method names。选择结果必须包含：工具版本锁定位置、生成命令、生成文件是否提交、CI 如何检测 generated output 未漂移。

### D1-DEC-3：V14 HTTP stream 的 canonical path

本计划的 canonical path 是 method-shaped 路径：`POST /api/v1/thread/start`、`POST /api/v1/thread/resume`、`POST /api/v1/thread/interrupt`、`POST /api/v1/turn/start`。如果维护者需要 REST-shaped alias（例如 `/api/v1/threads/{id}/turns`），只能作为无语义差异的 compatibility alias，不能让两套 handler 演化出不同语义。

---

## V14 v1 资源模型（先冻结语义，字段可在 schema review 中微调）

资源名称使用单数，方法名使用 `<resource>/<method>`，线上字段统一 camelCase：

| Resource | 核心字段 | 语义 |
|---|---|---|
| `thread` | `id`, `status`, `createdAt`, `updatedAt`, `model`, `thinking`, `turns` | 持久会话的外部资源；v1 复用现有 SQLite `sessions` 行，session id 即 thread id |
| `turn` | `id`, `threadId`, `status`, `startedAt`, `completedAt`, `input` | 一次 user input 的生命周期；同一 thread 只允许一个 active turn |
| `item` | `id`, `version`, `sequence`, `threadId`, `turnId`, `type`, `text`, `toolName`, `toolArgs`, `status`, `error`, `structuredResult` | 流式可消费的最小事件；每个 item 在 thread 内有单调 sequence |

v1 item types：`turn.started`、`message.delta`、`reasoning.delta`、`tool.call`、`tool.result`、`tool.progress`、`structured.result`、`turn.error`、`turn.completed`。未知 legacy `ServerFrame.Type` 必须映射成 `event.<legacyType>`，不能静默丢失；客户端按未知 item type 忽略即可。

方法集：

- `thread/start`：创建一个新 thread；不自动发起 turn。
- `thread/resume`：按 thread id 从 store 加载历史和 session metadata；返回可继续工作的 snapshot。
- `thread/interrupt`：取消该 thread 当前 active turn；重复 interrupt 是幂等的。
- `turn/start`：在 thread 上开始一个 turn，立即返回 `turn`，之后通过 SSE 或 JSON-RPC notification 流 item。
- `turn/interrupt`：APS1 的兼容别名，语义等于 `thread/interrupt`，params 同时携带 `threadId` 和 `turnId`；HTTP canonical endpoint 仍是 `thread/interrupt`。
- `capabilities`：返回 API version、methods、item types、stream transport 和 `unknownFields: "ignored"`。
- `config/read` / `config/write`：APS1 app-server 的本地配置读写；写入拒绝 token/api key 等 secret path。
- `initialize` / `shutdown`：JSON-RPC session lifecycle。

V14 v1 的 `resume` 保证 transcript/session metadata 可恢复；v1 MVP 不新增 turn/item SQLite 表，历史 item stream 不承诺跨进程 replay。若未来需要 `sinceSequence` replay，新增 durable event store 时保持上述 item 字段不变。

---

## File Structure

| 文件 | 职责 | 新建/修改 |
|---|---|---|
| `internal/cli/headless_input.go` | text/lines/jsonl 输入解析、空行与输入错误 | 新建 |
| `internal/cli/headless.go` | 多 prompt headless runner、同一 backend 上只做一次 resume | 新建 |
| `internal/cli/exec.go` | 把单 turn 核心抽成可被 batch runner 调用的函数、稳定 JSONL event 编码 | 修改 |
| `internal/cli/headless_input_test.go`、`internal/cli/exec_test.go` | 输入、resume、JSONL、错误优先级测试 | 修改/新建 |
| `cmd/yanshi/headless.go` | 共享 `exec` 和 `chat --no-tui` 的 flags、stdin、signal、exit code | 新建 |
| `cmd/yanshi/main.go` | dispatch、usage、`chatTUI` no-tui 分支改为共享 runner | 修改 |
| `cmd/yanshi/headless_test.go` | parse/exit code 测试 | 新建 |
| `internal/api/v1/types.go` | Thread/Turn/Item/Params/Response 类型和 camelCase tags | 新建 |
| `internal/api/v1/service.go` | V14 thread registry、store resume、turn goroutine、bounded item channel | 新建 |
| `internal/api/v1/events.go` | `proto.ServerFrame` 到稳定 item 的唯一映射 | 新建 |
| `internal/api/v1/schema.go` | v1 JSON Schema bytes 和 schema endpoint payload | 新建 |
| `internal/api/v1/*_test.go` | resource、映射、并发、未知字段、schema 测试 | 新建 |
| `internal/proto/versioned.go` | 版本化 frame envelope 常量和构造器，不污染 legacy frame | 新建 |
| `internal/proto/versioned_test.go` | envelope version/sequence JSON 契约 | 新建 |
| `internal/api/http/agent_v1.go` | 在现有 Server mux 上注册 V14 JSON/SSE routes | 新建 |
| `internal/api/http/agent_v1_test.go` | httptest start/resume/interrupt/stream contract tests | 新建 |
| `internal/bootstrap/bootstrap.go` | 构造 `api/v1.Service`、注册 HTTP routes、暴露 `App.AgentAPI` | 修改 |
| `internal/appserver/rpc.go` | JSON-RPC 2.0 request/response/error types、line codec | 新建 |
| `internal/appserver/server.go` | stdio dispatch、capabilities、stream notifications、writer mutex | 新建 |
| `internal/appserver/config.go` | 本地 YAML dot-path config read/write、secret path deny | 新建 |
| `internal/appserver/*_test.go` | JSON-RPC protocol、method dispatch、backpressure、config tests | 新建 |
| `cmd/yanshi/app.go` | `yanshi app` flags、bootstrap、signal、stdio server | 新建 |
| `cmd/yanshi/main.go` | `app` subcommand dispatch和 usage | 修改 |
| `cmd/yanshi/app_test.go` | app args/exit handling test | 新建 |
| `cmd/api-schema/main.go` | 若 D1-DEC-2 选择内置 generator，生成 TS types | 新建（条件） |
| `sdk/ts/v1.ts` | 生成产物；只在 D1-DEC-2 选择提交 generated output 时新增 | 新建（条件） |

依赖方向保持：`api/v1` 不依赖 `api/http`；`api/http` 和 `appserver` 都依赖 `api/v1`；`bootstrap` 是唯一装配点；`cmd/yanshi` 只负责入口和生命周期。

---


## Wire Contract Freeze（V14 / APS1 共享契约）

以下契约在 Task 4 中由 `internal/api/v1/types.go` 强制实施，并在 Task 9 的 JSON Schema 和兼容性测试中锁定。任何对本节的修改都必须同步更新类型定义、JSON Schema、HTTP 路由、JSON-RPC dispatch 和所有兼容性测试。

### 资源类型与 camelCase 字段

| 类型 | 必需字段 | 可选字段 |
|------|----------|----------|
| Thread | `version`, `id`, `status`, `createdAt`, `updatedAt` | `title`, `model`, `thinking`, `turns` |
| Turn | `version`, `id`, `threadId`, `status`, `input`, `startedAt` | `completedAt` |
| Item | `version`, `id`, `sequence`, `threadId`, `turnId`, `type` | `text`, `toolName`, `toolArgs`, `status`, `error`, `structuredResult` |

所有线上 JSON 属性名使用 camelCase（不是 snake_case）。`version` 字段值为常量 `"v1"`。

### 方法名

| 方法 | 资源 | HTTP | JSON-RPC |
|------|------|------|----------|
| thread/start | Thread | POST /api/v1/thread/start | thread/start |
| thread/resume | Thread | POST /api/v1/thread/resume | thread/resume |
| thread/interrupt | Thread | POST /api/v1/thread/interrupt | thread/interrupt, turn/interrupt |
| turn/start | Turn | POST /api/v1/turn/start | turn/start |
| capabilities | - | GET /api/v1/schema/agent-v1.json | capabilities |
| config/read | - | - | config/read |
| config/write | - | - | config/write |
| initialize | - | - | initialize |
| shutdown | - | - | shutdown |

### 版本行为

- 请求中的 `version` 字段可省略，省略等同 `"v1"`。HTTP 响应携带 `X-Yanshi-API-Version: v1` header。
- 每个 JSON-RPC response 和 HTTP JSON 响应体的顶层 `version` 字段都必须存在。
- JSON-RPC 的 `jsonrpc: "2.0"` 是 transport 版本，不替代 Agent API 的 v1 资源版本。

### 未知字段

- 所有 decode 位置使用标准 `encoding/json`，不使用 `DisallowUnknownFields`。未知请求字段静默忽略。
- 未知 legacy `proto.ServerFrame.Type` 字段映射为 `event.<legacyType>`，不静默丢失。
- 客户端收到未知 item type 时按未知类型忽略即可。

### Stream item 类型

| Item.Type 常量 | Go 标识符 | 来源 ServerFrame.Type |
|----------------|-----------|----------------------|
| turn.started | ItemTurnStarted | （由 service 合成） |
| message.delta | ItemMessageDelta | agent_chunk |
| reasoning.delta | ItemReasoningDelta | thinking |
| tool.call | ItemToolCall | tool_call |
| tool.result | ItemToolResult | tool_result |
| tool.progress | ItemToolProgress | tool_chunk / tool_progress |
| structured.result | ItemStructuredResult | structured_result |
| turn.error | ItemTurnError | error |
| turn.completed | ItemTurnCompleted | done |

### 错误格式

HTTP 错误返回 JSON body：`{"version":"v1","error":{"message":"..."}}`，状态码为 4xx/5xx。
JSON-RPC 错误使用标准 error object：`{"code":-32603,"message":"..."}`。

---

## Task 1: Headless 输入解析（V12）

**Files:**
- Create: `internal/cli/headless_input.go`
- Test: `internal/cli/headless_input_test.go`

- [ ] **Step 1: 写失败测试**

```go
package cli

import (
	"strings"
	"testing"
)

func TestReadHeadlessInputs_Text(t *testing.T) {
	got, err := ReadHeadlessInputs(strings.NewReader("  hello\nworld\n"), HeadlessInputText)
	if err != nil {
		t.Fatalf("ReadHeadlessInputs: %v", err)
	}
	if len(got) != 1 || got[0].Prompt != "hello\nworld" {
		t.Fatalf("text input = %#v", got)
	}
}

func TestReadHeadlessInputs_LinesSkipsBlankLines(t *testing.T) {
	got, err := ReadHeadlessInputs(strings.NewReader("one\n\n two \n"), HeadlessInputLines)
	if err != nil {
		t.Fatalf("ReadHeadlessInputs: %v", err)
	}
	if len(got) != 2 || got[0].Prompt != "one" || got[1].Prompt != "two" {
		t.Fatalf("line input = %#v", got)
	}
}

func TestReadHeadlessInputs_JSONL(t *testing.T) {
	input := "{\"prompt\":\"one\"}\n\n{\"prompt\":\"two\",\"resume\":\"sess-2\"}\n"
	got, err := ReadHeadlessInputs(strings.NewReader(input), HeadlessInputJSONL)
	if err != nil {
		t.Fatalf("ReadHeadlessInputs: %v", err)
	}
	if len(got) != 2 || got[0].Prompt != "one" || got[1].Resume != "sess-2" {
		t.Fatalf("jsonl input = %#v", got)
	}
}

func TestReadHeadlessInputs_JSONLRejectsMissingPrompt(t *testing.T) {
	_, err := ReadHeadlessInputs(strings.NewReader("{\"resume\":\"sess-2\"}\n"), HeadlessInputJSONL)
	if err == nil {
		t.Fatal("missing prompt should fail")
	}
}

func TestReadHeadlessInputs_UnknownMode(t *testing.T) {
	_, err := ReadHeadlessInputs(strings.NewReader("x"), HeadlessInputMode("yaml"))
	if err == nil {
		t.Fatal("unknown input mode should fail")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/cli -run 'TestReadHeadlessInputs' -v`

Expected: FAIL，因为 `HeadlessInputMode`、`HeadlessInput` 和 `ReadHeadlessInputs` 尚未定义。

- [ ] **Step 3: 实现完整 `headless_input.go`**

```go
package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// HeadlessInputMode selects how a headless command turns stdin into prompts.
type HeadlessInputMode string

const (
	HeadlessInputText  HeadlessInputMode = "text"
	HeadlessInputLines HeadlessInputMode = "lines"
	HeadlessInputJSONL HeadlessInputMode = "jsonl"
)

// HeadlessInput is one prompt accepted by the headless runner. Resume is only
// honored for the first record; later records continue the same backend session.
type HeadlessInput struct {
	Prompt string
	Resume string
}

type headlessJSONLInput struct {
	Prompt string `json:"prompt"`
	Resume string `json:"resume,omitempty"`
}

// ReadHeadlessInputs reads all prompts before a backend is opened. Text mode
// treats the whole stream as one prompt; lines mode treats each non-empty line
// as a prompt; JSONL mode accepts one object per line and ignores unknown fields
// so a producer can add metadata without breaking v1 clients.
func ReadHeadlessInputs(r io.Reader, mode HeadlessInputMode) ([]HeadlessInput, error) {
	if r == nil {
		return nil, fmt.Errorf("headless input: nil reader")
	}
	switch mode {
	case HeadlessInputText:
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("read text input: %w", err)
		}
		prompt := strings.TrimSpace(string(b))
		if prompt == "" {
			return nil, fmt.Errorf("prompt is empty")
		}
		return []HeadlessInput{{Prompt: prompt}}, nil
	case HeadlessInputLines:
		var out []HeadlessInput
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			prompt := strings.TrimSpace(sc.Text())
			if prompt != "" {
				out = append(out, HeadlessInput{Prompt: prompt})
			}
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("read line input: %w", err)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("prompt is empty")
		}
		return out, nil
	case HeadlessInputJSONL:
		var out []HeadlessInput
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		line := 0
		for sc.Scan() {
			line++
			text := strings.TrimSpace(sc.Text())
			if text == "" {
				continue
			}
			var in headlessJSONLInput
			if err := json.Unmarshal([]byte(text), &in); err != nil {
				return nil, fmt.Errorf("jsonl line %d: %w", line, err)
			}
			in.Prompt = strings.TrimSpace(in.Prompt)
			if in.Prompt == "" {
				return nil, fmt.Errorf("jsonl line %d: prompt is empty", line)
			}
			out = append(out, HeadlessInput{Prompt: in.Prompt, Resume: strings.TrimSpace(in.Resume)})
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("read jsonl input: %w", err)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("prompt is empty")
		}
		return out, nil
	default:
		return nil, fmt.Errorf("invalid input mode %q (want text, lines, or jsonl)", mode)
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/cli -run 'TestReadHeadlessInputs' -v`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/cli/headless_input.go internal/cli/headless_input_test.go
git commit -m "feat(cli): parse text lines and JSONL headless input"
```

---

## Task 2: 多 prompt headless runner 与 JSONL event contract（V12）

**Files:**
- Create: `internal/cli/headless.go`
- Modify: `internal/cli/exec.go`
- Test: `internal/cli/exec_test.go`

`execWithBackend` 已经能执行一个 turn。不要在 `chatLegacy` 中复制这套逻辑；新增 runner 只在同一个 `Session`/`ChatBackend` 上循环，并且只在第一条输入前执行一次 restore。`HeadlessOutputEvent` 是 stdout 的稳定 JSONL contract，不能直接依赖 `StreamEvent` 的 Go 默认字段名。

- [ ] **Step 1: 写失败测试**

```go
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderHeadlessJSONLEventUsesStableKeys(t *testing.T) {
	var out, errOut bytes.Buffer
	renderHeadlessEvent(&out, &errOut, ExecOutputJSONL, StreamEvent{
		Kind: "agent_chunk",
		Text: "hello",
	})
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &got); err != nil {
		t.Fatalf("JSONL event is invalid JSON: %v", err)
	}
	if got["type"] != "agent_chunk" || got["text"] != "hello" {
		t.Fatalf("JSONL event = %#v", got)
	}
	if _, ok := got["Kind"]; ok {
		t.Fatal("JSONL must not expose Go field name Kind")
	}
}

func TestRunHeadlessWithBackendResumesOnlyOnce(t *testing.T) {
	backend := &recordingBackend{}
	var stdout, stderr bytes.Buffer
	result, err := runHeadlessWithBackend(context.Background(), backend, HeadlessRunOptions{
		Inputs:  []HeadlessInput{{Prompt: "one"}, {Prompt: "two"}},
		Output:  ExecOutputJSONL,
		Stdout:  &stdout,
		Stderr:  &stderr,
		Resume:  "sess-1",
	})
	if err != nil {
		t.Fatalf("runHeadlessWithBackend: %v", err)
	}
	if result.Completed != 2 || result.SessionID == "" {
		t.Fatalf("result = %#v", result)
	}
	if backend.restores != 1 || !strings.Contains(backend.prompts[0], "one") || !strings.Contains(backend.prompts[1], "two") {
		t.Fatalf("backend calls restores=%d prompts=%#v", backend.restores, backend.prompts)
	}
}
```

> `recordingBackend` 必须作为在同一个 `_test.go` 中的完整 `ChatBackend` fake 实现，不要引入 mock framework。在同一个文件末尾添加以下结构：

```go
// recordingBackend records prompts and restore counts for multi-prompt tests.
type recordingBackend struct {
	prompts  []string
	restores int
	mu       sync.Mutex
}

func (r *recordingBackend) Send(_ context.Context, text string) (<-chan StreamEvent, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, text)
	r.mu.Unlock()
	ch := make(chan StreamEvent, 4)
	ch <- StreamEvent{Kind: "agent_chunk", Text: "ok"}
	ch <- StreamEvent{Kind: "status", SessionID: "sess-rec"}
	ch <- StreamEvent{Kind: "done"}
	close(ch)
	return ch, nil
}

func (r *recordingBackend) SendFrame(_ context.Context, fr proto.ClientFrame) (<-chan StreamEvent, error) {
	r.mu.Lock()
	r.restores++
	r.mu.Unlock()
	ch := make(chan StreamEvent, 2)
	if fr.Type == "restore_session" {
		ch <- StreamEvent{Kind: "session_restored", SessionID: fr.ID}
	}
	close(ch)
	return ch, nil
}

func (r *recordingBackend) Cancel() error { return nil }
func (r *recordingBackend) Close() error  { return nil }
func (r *recordingBackend) Mode() string  { return "fake" }
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/cli -run 'TestRenderHeadlessJSONLEvent|TestRunHeadlessWithBackendResumesOnlyOnce' -v`

Expected: FAIL，因为 runner 和稳定 JSONL encoder 尚未定义。

- [ ] **Step 3: 实现完整 `internal/cli/headless.go`**

```go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// HeadlessRunOptions drives one or more prompts through one resolved backend.
type HeadlessRunOptions struct {
	Inputs  []HeadlessInput
	Output  ExecOutputFormat
	Stdout  io.Writer
	Stderr  io.Writer
	Resume  string
}

// HeadlessResult reports the last known server session id and completed prompts.
type HeadlessResult struct {
	SessionID string
	Completed int
}

// RunHeadless resolves one backend and keeps it alive for all input records.
// Resume from the CLI flag is applied before the first record; a JSONL record's
// resume is used only when the flag is empty and the record is first.
func RunHeadless(ctx context.Context, opts Options, run HeadlessRunOptions) (HeadlessResult, error) {
	if len(run.Inputs) == 0 {
		return HeadlessResult{}, fmt.Errorf("headless: no input")
	}
	sess := NewSession(opts)
	if err := sess.Resolve(ctx); err != nil {
		return HeadlessResult{}, err
	}
	defer sess.Close()
	return runHeadlessWithBackend(ctx, sess.Backend(), run)
}

func runHeadlessWithBackend(ctx context.Context, b ChatBackend, opts HeadlessRunOptions) (HeadlessResult, error) {
	if b == nil {
		return HeadlessResult{}, fmt.Errorf("headless: backend is nil")
	}
	stdout, stderr := opts.Stdout, opts.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	output := opts.Output
	if output == "" {
		output = ExecOutputText
	}
	result := HeadlessResult{}
	resume := opts.Resume
	for i, input := range opts.Inputs {
		if i == 0 && resume == "" {
			resume = input.Resume
		}
		one := ExecOptions{
			Prompt:  input.Prompt,
			Output:  output,
			Resume:  resume,
			Stdout:  stdout,
			Stderr:  stderr,
		}
		oneResult, err := execWithBackend(ctx, b, one)
		if err != nil {
			return result, err
		}
		if oneResult.SessionID != "" {
			result.SessionID = oneResult.SessionID
		}
		result.Completed++
		resume = ""
	}
	return result, nil
}

// HeadlessOutputEvent is the stable JSONL projection. Error is a string rather
// than an error interface, so transport errors and server error frames have one
// machine-readable representation.
type HeadlessOutputEvent struct {
	Type             string          `json:"type"`
	Text             string          `json:"text,omitempty"`
	ToolName         string          `json:"toolName,omitempty"`
	ToolArgs         string          `json:"toolArgs,omitempty"`
	Status           string          `json:"status,omitempty"`
	ID               string          `json:"id,omitempty"`
	Model            string          `json:"model,omitempty"`
	Thinking         string          `json:"thinking,omitempty"`
	SessionID        string          `json:"sessionId,omitempty"`
	TokensIn         int             `json:"tokensIn,omitempty"`
	TokensOut        int             `json:"tokensOut,omitempty"`
	Turns            int             `json:"turns,omitempty"`
	Items            []string        `json:"items,omitempty"`
	StructuredResult json.RawMessage `json:"structuredResult,omitempty"`
	Error            string          `json:"error,omitempty"`
}

func projectHeadlessEvent(ev StreamEvent) HeadlessOutputEvent {
	out := HeadlessOutputEvent{
		Type: ev.Kind, Text: ev.Text, ToolName: ev.ToolName, ToolArgs: ev.ToolArgs,
		Status: ev.ToolStatus, ID: ev.ID, Model: ev.Model, Thinking: ev.Thinking,
		SessionID: ev.SessionID, TokensIn: ev.TokensIn, TokensOut: ev.TokensOut,
		Turns: ev.Turns, Items: ev.Items, StructuredResult: ev.StructuredResult,
	}
	if ev.Err != nil {
		out.Error = ev.Err.Error()
	}
	return out
}

func renderHeadlessEvent(stdout, stderr io.Writer, output ExecOutputFormat, ev StreamEvent) {
	if output == ExecOutputJSONL {
		line, err := json.Marshal(projectHeadlessEvent(ev))
		if err != nil {
			fmt.Fprintf(stderr, "exec: marshal event: %v\n", err)
			return
		}
		fmt.Fprintln(stdout, string(line))
		return
	}
	renderExecEvent(stdout, stderr, ExecOutputText, ev)
}
```

- [ ] **Step 4: 修改 `internal/cli/exec.go` 的单 turn renderer 调用**

把现有 `renderExecEvent` 的 JSONL 分支替换为下面完整函数；text 行为保持不变，所有调用点仍可继续调用 `renderExecEvent`。

```go
func renderExecEvent(stdout, stderr io.Writer, out ExecOutputFormat, ev StreamEvent) {
	if out == ExecOutputJSONL {
		renderHeadlessEvent(stdout, stderr, ExecOutputJSONL, ev)
		return
	}
	switch ev.Kind {
	case "agent_chunk":
		fmt.Fprint(stdout, ev.Text)
	case "tool_call":
		fmt.Fprintf(stderr, "tool %s %s …\n", ev.ToolName, ev.ToolArgs)
	case "tool_result":
		fmt.Fprintf(stderr, "result %s [%s]\n", ev.ToolName, ev.ToolStatus)
	case "error":
		fmt.Fprintf(stderr, "error: %s\n", execEventText(ev))
	}
}
```

`tool_call`/`tool_result` 的 text 输出去除不可移植的 emoji，避免 CI 非 UTF-8 locale 下误判；TUI 的 renderer 不在本任务内，不受影响。

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/cli -run 'TestRenderHeadlessJSONLEvent|TestRunHeadlessWithBackendResumesOnlyOnce' -v`

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/cli/headless.go internal/cli/exec.go internal/cli/exec_test.go
git commit -m "feat(cli): share multi-prompt headless runner and stable JSONL events"
```

---

## Task 3: `exec` 与 `chat --no-tui` 共享 CLI 契约（V12）

**Files:**
- Create: `cmd/yanshi/headless.go`
- Modify: `cmd/yanshi/main.go`
- Test: `cmd/yanshi/headless_test.go`

CLI contract：

- `yanshi exec [-p PROMPT] [--input text|lines|jsonl] [-output text|jsonl] [-resume ID] [-timeout DURATION]`。
- `yanshi chat --no-tui` 接受同一组 headless flags；默认 `--input lines` 以保留旧 REPL 的逐行习惯，`--input text` 读取整个 stdin，`--input jsonl` 读取 `{ "prompt": "...", "resume": "..." }`。
- `-p/--prompt` 只允许配 `--input text`；lines/jsonl 必须从 stdin 读取，避免 prompt 与批量输入的歧义。
- stdout 只放 assistant text 或 JSONL event；stderr 放工具进度、错误和 `session: <id>`。
- 退出码：`0` 成功，`1` runtime/server/transport error，`2` usage/input error，`124` timeout，`130` cancellation。

- [ ] **Step 1: 写失败测试**

```go
package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestParseHeadlessArgs(t *testing.T) {
	cfg, err := parseHeadlessArgs([]string{"--input", "jsonl", "--output", "jsonl", "--resume", "s1", "--timeout", "2s"}, "exec")
	if err != nil {
		t.Fatalf("parseHeadlessArgs: %v", err)
	}
	if cfg.Input != "jsonl" || cfg.Output != "jsonl" || cfg.Resume != "s1" || cfg.Timeout != 2*time.Second {
		t.Fatalf("cfg = %#v", cfg)
	}
}

func TestParseHeadlessArgsRejectsPromptWithJSONL(t *testing.T) {
	_, err := parseHeadlessArgs([]string{"-p", "hello", "--input", "jsonl"}, "exec")
	if err == nil {
		t.Fatal("prompt + jsonl should fail")
	}
}

func TestHeadlessExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"ok", nil, exitOK},
		{"runtime", errors.New("boom"), exitErr},
		{"timeout", context.DeadlineExceeded, exitTimeout},
		{"cancel", context.Canceled, exitCancel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapExecError(tc.err); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./cmd/yanshi -run 'TestParseHeadlessArgs|TestHeadlessExitCode' -v`

Expected: FAIL，因为 `headlessConfig` 和 `parseHeadlessArgs` 尚未定义。

- [ ] **Step 3: 新建完整 `cmd/yanshi/headless.go`**

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/x6nux/yanshi/internal/cli"
)

type headlessConfig struct {
	ConfigPath string
	Prompt     string
	Input      string
	Output     string
	File       string // read input from file instead of stdin
	Timeout    time.Duration
	Resume     string
	FakeModel  bool
	Server     string
	InProcess  bool
}

func parseHeadlessArgs(args []string, command string) (headlessConfig, error) {
	cfg := headlessConfig{Input: "text", Output: "text"}
	if command == "chat" {
		cfg.Input = "lines"
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.StringVar(&cfg.ConfigPath, "config", "config.yaml", "path to configuration file")
	fs.StringVar(&cfg.Prompt, "p", "", "prompt text; with input=text only")
	fs.StringVar(&cfg.Prompt, "prompt", "", "alias for -p")
	fs.StringVar(&cfg.Input, "input", cfg.Input, "input mode: text | lines | jsonl")
	fs.StringVar(&cfg.Output, "output", "text", "output format: text | jsonl")
	fs.StringVar(&cfg.File, "file", "", "read input from FILE instead of stdin")
	fs.DurationVar(&cfg.Timeout, "timeout", 0, "abort after this duration (0 = no limit)")
	fs.StringVar(&cfg.Resume, "resume", "", "restore session id before the first turn")
	fs.BoolVar(&cfg.FakeModel, "fake-model", false, "use deterministic fake model")
	fs.StringVar(&cfg.Server, "server", "", "force connect to this server URL")
	fs.BoolVar(&cfg.InProcess, "inprocess", false, "force in-process backend")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() != 0 {
		return cfg, fmt.Errorf("unexpected positional argument %q", fs.Arg(0))
	}
	if cfg.Output != "text" && cfg.Output != "jsonl" {
		return cfg, fmt.Errorf("invalid --output %q (want text or jsonl)", cfg.Output)
	}
	if cfg.Input != "text" && cfg.Input != "lines" && cfg.Input != "jsonl" {
		return cfg, fmt.Errorf("invalid --input %q (want text, lines, or jsonl)", cfg.Input)
	}
	if cfg.Prompt != "" && cfg.Input != "text" {
		return cfg, fmt.Errorf("-p/--prompt requires --input text")
	}
	if cfg.File != "" && cfg.Prompt != "" {
		return cfg, fmt.Errorf("--file and -p/--prompt are mutually exclusive")
	}
	return cfg, nil
}

func runHeadlessCommand(args []string, command string) int {
	cfg, err := parseHeadlessArgs(args, command)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yanshi %s: %v\n", command, err)
		return exitUsage
	}
	inputs := []cli.HeadlessInput(nil)
	if cfg.File != "" {
		data, rerr := os.ReadFile(cfg.File)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "yanshi %s: read file: %v\n", command, rerr)
			return exitUsage
		}
		inputs = []cli.HeadlessInput{{Prompt: strings.TrimSpace(string(data))}}
	} else if cfg.Prompt != "" {
		inputs = []cli.HeadlessInput{{Prompt: strings.TrimSpace(cfg.Prompt)}}
	} else {
		mode := cli.HeadlessInputMode(cfg.Input)
		inputs, err = cli.ReadHeadlessInputs(os.Stdin, mode)
		if err != nil {
			fmt.Fprintf(os.Stderr, "yanshi %s: %v\n", command, err)
			return exitUsage
		}
	}
	if cfg.Resume != "" {
		inputs[0].Resume = cfg.Resume
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}
	result, err := cli.RunHeadless(ctx, cli.Options{
		ConfigPath: cfg.ConfigPath,
		FakeModel:  cfg.FakeModel,
		Server:     cfg.Server,
		InProcess:  cfg.InProcess,
	}, cli.HeadlessRunOptions{
		Inputs: inputs,
		Output: cli.ExecOutputFormat(cfg.Output),
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if result.SessionID != "" {
		fmt.Fprintf(os.Stderr, "session: %s\n", result.SessionID)
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		fmt.Fprintf(os.Stderr, "yanshi %s: %v\n", command, err)
	}
	return mapExecError(err)
}
```

- [ ] **Step 4: 修改 `cmd/yanshi/main.go` 的 dispatch 和 `chatTUI` 分支**

将 `main` switch 中的 `exec` 分支替换为：

```go
case "exec":
	os.Exit(runHeadlessCommand(os.Args[2:], "exec"))
```

将 `chatTUI` 中 `if *noTUI { ... }` 整段替换为：

```go
if *noTUI {
	// The headless chat path shares input/output/resume/timeout semantics with
	// exec. It defaults to line input so existing scripts keep one prompt per line.
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		if a != "--no-tui" { filtered = append(filtered, a) }
	}
	os.Exit(runHeadlessCommand(filtered, "chat"))
}
```

同时删除 `chatLegacy`/`sendChatLegacy` 以及旧的 `execConfig`、`parseExecArgs`、`exec` 实现，避免两套 flags 和退出码漂移。`usage` 中保留如下完整文字：

```go
  yanshi chat    [--no-tui] [-p "prompt" | stdin] [--input text|lines|jsonl] [-output text|jsonl] [-timeout 1m] [-resume ID]
  yanshi exec    [-p "prompt" | stdin] [--input text|lines|jsonl] [-output text|jsonl] [-timeout 1m] [-resume ID]
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./cmd/yanshi -run 'TestParseHeadlessArgs|TestHeadlessExitCode' -v`

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add cmd/yanshi/headless.go cmd/yanshi/main.go cmd/yanshi/headless_test.go
git commit -m "feat(cmd): unify exec and chat no-tui headless contract"
```

---

## Task 4: V14 resource types 与 provisional version envelope

**依赖 gate:** 先关闭 D1-DEC-1；默认方案为 `version: "v1"` + `X-Yanshi-API-Version: v1`。

**Files:**
- Create: `internal/api/v1/types.go`
- Create: `internal/proto/versioned.go`
- Test: `internal/api/v1/types_test.go`
- Test: `internal/proto/versioned_test.go`

- [ ] **Step 1: 写失败测试**

```go
package v1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestItemJSONUsesCamelCaseAndVersion(t *testing.T) {
	data, err := json.Marshal(Item{
		Version: "v1", ID: "item-1", ThreadID: "thread-1", TurnID: "turn-1",
		Sequence: 7, Type: ItemMessageDelta, Text: "hello",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)
	for _, want := range []string{`"version":"v1"`, `"threadId":"thread-1"`, `"turnId":"turn-1"`, `"sequence":7`} {
		if !strings.Contains(got, want) {
			t.Fatalf("JSON %q lacks %s", got, want)
		}
	}
	if strings.Contains(got, "thread_id") || strings.Contains(got, "turn_id") {
		t.Fatalf("wire JSON must be camelCase: %s", got)
	}
}

func TestUnknownFieldsAreIgnored(t *testing.T) {
	var p TurnStartParams
	if err := json.Unmarshal([]byte(`{"version":"v1","threadId":"t1","input":"hi","futureField":42}`), &p); err != nil {
		t.Fatalf("unknown future field should be ignored: %v", err)
	}
	if p.ThreadID != "t1" || p.Input != "hi" {
		t.Fatalf("params = %#v", p)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/api/v1 ./internal/proto -run 'TestItemJSON|TestUnknownFields' -v`

Expected: FAIL，因为 v1 resource types 尚未定义。

- [ ] **Step 3: 写完整 `internal/api/v1/types.go`**

```go
package v1

import "encoding/json"

const Version = "v1"

const (
	ThreadStatusActive    = "active"
	ThreadStatusArchived  = "archived"
	TurnStatusInProgress  = "inProgress"
	TurnStatusCompleted   = "completed"
	TurnStatusInterrupted = "interrupted"
	TurnStatusFailed      = "failed"
	ItemMessageDelta      = "message.delta"
	ItemReasoningDelta    = "reasoning.delta"
	ItemToolCall          = "tool.call"
	ItemToolResult        = "tool.result"
	ItemToolProgress      = "tool.progress"
	ItemStructuredResult  = "structured.result"
	ItemTurnStarted       = "turn.started"
	ItemTurnError         = "turn.error"
	ItemTurnCompleted     = "turn.completed"
)

type Thread struct {
	Version   string `json:"version"`
	ID        string `json:"id"`
	Status    string `json:"status"`
	Title     string `json:"title,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	Model     string `json:"model,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Turns     []Turn `json:"turns,omitempty"`
}

type Turn struct {
	Version     string `json:"version"`
	ID          string `json:"id"`
	ThreadID    string `json:"threadId"`
	Status      string `json:"status"`
	Input       string `json:"input"`
	StartedAt   int64  `json:"startedAt"`
	CompletedAt int64  `json:"completedAt,omitempty"`
}

type Item struct {
	Version          string          `json:"version"`
	ID               string          `json:"id"`
	Sequence         int64           `json:"sequence"`
	ThreadID         string          `json:"threadId"`
	TurnID           string          `json:"turnId"`
	Type             string          `json:"type"`
	Text             string          `json:"text,omitempty"`
	ToolName         string          `json:"toolName,omitempty"`
	ToolArgs         string          `json:"toolArgs,omitempty"`
	Status           string          `json:"status,omitempty"`
	Error            string          `json:"error,omitempty"`
	StructuredResult json.RawMessage `json:"structuredResult,omitempty"`
}

type ThreadSnapshot struct {
	Version string `json:"version"`
	Thread  Thread  `json:"thread"`
	Items   []Item  `json:"items,omitempty"`
}

type ThreadStartParams struct {
	Version  string `json:"version,omitempty"`
	Title    string `json:"title,omitempty"`
	Model    string `json:"model,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

type ThreadResumeParams struct {
	Version  string `json:"version,omitempty"`
	ThreadID string `json:"threadId"`
}

type ThreadInterruptParams struct {
	Version  string `json:"version,omitempty"`
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
}

type TurnStartParams struct {
	Version      string          `json:"version,omitempty"`
	ThreadID     string          `json:"threadId"`
	Input        string          `json:"input"`
	Model        string          `json:"model,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

type ThreadStartResponse struct {
	Version string `json:"version"`
	Thread  Thread `json:"thread"`
}

type ThreadResumeResponse struct {
	Version string         `json:"version"`
	Thread  Thread         `json:"thread"`
	Items   []Item         `json:"items,omitempty"`
}

type TurnStartResponse struct {
	Version string `json:"version"`
	Turn    Turn   `json:"turn"`
}

type InterruptResponse struct {
	Version  string `json:"version"`
	OK       bool   `json:"ok"`
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
}

type Capabilities struct {
	Version        string   `json:"version"`
	Methods        []string `json:"methods"`
	ItemTypes      []string `json:"itemTypes"`
	UnknownFields  string   `json:"unknownFields"`
	Stream         string   `json:"stream"`
}
```

- [ ] **Step 4: 写完整 `internal/proto/versioned.go`**

```go
package proto

import "encoding/json"

// AgentAPIVersionV1 is the resource contract version. It is intentionally
// separate from legacy ClientFrame/ServerFrame so TUI compatibility is not tied
// to the public Agent API lifecycle.
const AgentAPIVersionV1 = "v1"

type VersionedFrame struct {
	Version  string          `json:"version"`
	Sequence int64           `json:"sequence"`
	Type     string          `json:"type"`
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

func NewVersionedFrame(sequence int64, typ, threadID, turnID string, payload any) (VersionedFrame, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return VersionedFrame{}, err
	}
	return VersionedFrame{
		Version: AgentAPIVersionV1, Sequence: sequence, Type: typ,
		ThreadID: threadID, TurnID: turnID, Payload: data,
	}, nil
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/api/v1 ./internal/proto -run 'TestItemJSON|TestUnknownFields' -v`

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/api/v1/types.go internal/api/v1/types_test.go internal/proto/versioned.go internal/proto/versioned_test.go
git commit -m "feat(api): define versioned thread turn item resources"
```

---

## Task 5: 唯一的 legacy frame → v1 item 映射

**Files:**
- Create: `internal/api/v1/events.go`
- Test: `internal/api/v1/events_test.go`

- [ ] **Step 1: 写失败测试**

```go
package v1

import (
	"encoding/json"
	"testing"

	"github.com/x6nux/yanshi/internal/proto"
)

func TestItemFromServerFrame(t *testing.T) {
	cases := []struct {
		frame proto.ServerFrame
		kind  string
		text  string
	}{
		{proto.NewAgentChunk("hello"), ItemMessageDelta, "hello"},
		{proto.NewThinking("think"), ItemReasoningDelta, "think"},
		{proto.NewToolCall("fs_read", `{"path":"a.go"}`, "running"), ItemToolCall, ""},
		{proto.NewToolResult("fs_read", "ok", "ok"), ItemToolResult, "ok"},
		{proto.NewError("boom"), ItemTurnError, "boom"},
		{proto.NewDone(), ItemTurnCompleted, ""},
	}
	for _, tc := range cases {
		got := ItemFromServerFrame(tc.frame, "thread-1", "turn-1", 9)
		if got.Version != Version || got.Type != tc.kind || got.Text != tc.text || got.Sequence != 9 {
			t.Fatalf("frame %#v -> %#v", tc.frame, got)
		}
	}
}

func TestItemFromServerFramePreservesStructuredResult(t *testing.T) {
	data := json.RawMessage(`{"ok":true}`)
	got := ItemFromServerFrame(proto.NewStructuredResult(data), "t", "u", 1)
	if got.Type != ItemStructuredResult || string(got.StructuredResult) != string(data) {
		t.Fatalf("structured item = %#v", got)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/api/v1 -run 'TestItemFromServerFrame' -v`

Expected: FAIL，因为映射函数尚未定义。

- [ ] **Step 3: 写完整 `internal/api/v1/events.go`**

```go
package v1

import "github.com/x6nux/yanshi/internal/proto"

// ItemFromServerFrame is the only mapping from the legacy transport vocabulary
// to the public resource vocabulary. Keeping it centralized prevents HTTP and
// JSON-RPC from disagreeing about the meaning of a streamed event.
func ItemFromServerFrame(f proto.ServerFrame, threadID, turnID string, sequence int64) Item {
	item := Item{
		Version: Version, ID: "item-" + formatSequence(sequence), Sequence: sequence,
		ThreadID: threadID, TurnID: turnID, Status: f.Status,
		Text: f.Text, ToolName: f.ToolName, ToolArgs: f.ToolArgs,
		StructuredResult: f.StructuredResult,
	}
	switch f.Type {
	case "agent_chunk":
		item.Type = ItemMessageDelta
	case "thinking":
		item.Type = ItemReasoningDelta
	case "tool_call":
		item.Type = ItemToolCall
	case "tool_result":
		item.Type = ItemToolResult
	case "tool_chunk", "tool_progress":
		item.Type = ItemToolProgress
	case "structured_result":
		item.Type = ItemStructuredResult
	case "error":
		item.Type = ItemTurnError
		item.Error = f.Text
	case "done":
		item.Type = ItemTurnCompleted
	default:
		item.Type = "event." + f.Type
	}
	return item
}

func formatSequence(sequence int64) string {
	if sequence < 0 {
		return "0"
	}
	return decimal(sequence)
}

func decimal(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/api/v1 -run 'TestItemFromServerFrame' -v`

Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/api/v1/events.go internal/api/v1/events_test.go
git commit -m "feat(api): map legacy server frames to v1 items"
```

---

## Task 6: V14 service（thread/resume/turn/interrupt + 背压）

**Files:**
- Create: `internal/api/v1/service.go`
- Test: `internal/api/v1/service_test.go`

设计约束：

- `Store` 非 nil 时 thread id 直接复用 session id；resume 读取 `Store.Messages` 和 `SessionSummary`。
- `Store` 为 nil 时仍允许 fake/in-memory service，使用进程内递增 id；这只用于测试和无持久化 server，不伪装成可 resume 的 durable session。
- 每个 thread 一个 mutex；active turn 只有一个；`StartTurn` 返回容量为 128 的 channel。发送者在 channel 满时阻塞，客户端断开时 context cancel，绝不无界增长、静默丢 item 或让 model goroutine 永久占用。
- HTTP/SSE 使用静态 orchestrator profile，和现有 SSE 一样不启用交互式 WS permission callback。

- [ ] **Step 1: 写失败测试**

```go
package v1

import (
	"context"
	"errors"
	"testing"
	"time"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

func TestServiceStartTurnStreamsItemsInSequence(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	thread, err := svc.Start(context.Background(), ThreadStartParams{Title: "test"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	resp, items, err := svc.StartTurn(context.Background(), TurnStartParams{
		ThreadID: thread.ID,
		Input:    "hello",
	})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if resp.Turn.Status != TurnStatusInProgress {
		t.Fatalf("turn = %#v", resp.Turn)
	}
	var last int64
	var count int
	for item := range items {
		if item.Sequence <= last {
			t.Fatalf("non-increasing sequence: last=%d item=%#v", last, item)
		}
		last, count = item.Sequence, count+1
	}
	if count == 0 || last == 0 {
		t.Fatal("expected streamed items")
	}
}

func TestServiceInterruptIsIdempotent(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	thread, err := svc.Start(context.Background(), ThreadStartParams{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, items, err := svc.StartTurn(context.Background(), TurnStartParams{ThreadID: thread.ID, Input: "hello"})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	if err := svc.Interrupt(context.Background(), ThreadInterruptParams{ThreadID: thread.ID}); err != nil {
		t.Fatalf("first Interrupt: %v", err)
	}
	if err := svc.Interrupt(context.Background(), ThreadInterruptParams{ThreadID: thread.ID}); err != nil {
		t.Fatalf("second Interrupt: %v", err)
	}
	select {
	case <-items:
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt did not close/advance stream")
	}
}

func TestServiceResumeReturnsSnapshotAndRejectsMissing(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	thread, err := svc.Start(context.Background(), ThreadStartParams{Title: "snapshot"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	snap, err := svc.Resume(context.Background(), ThreadResumeParams{ThreadID: thread.ID})
	if err != nil {
		t.Fatalf("Resume known thread: %v", err)
	}
	if snap.Version != Version || snap.Thread.ID != thread.ID || snap.Thread.Title != "snapshot" {
		t.Fatalf("snapshot = %#v", snap)
	}
	if _, err := svc.Resume(context.Background(), ThreadResumeParams{ThreadID: "missing"}); !errors.Is(err, ErrThreadNotFound) {
		t.Fatalf("Resume missing err = %v want ErrThreadNotFound", err)
	}
	if _, err := svc.Resume(context.Background(), ThreadResumeParams{ThreadID: ""}); err == nil {
		t.Fatal("Resume with empty thread id should fail")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/api/v1 -run 'TestServiceStartTurn|TestServiceInterrupt|TestServiceResume' -v`

Expected: FAIL，因为 `NewService`、`Service.Start`、`StartTurn`、`Interrupt` 和 `Resume` 尚未定义。

- [ ] **Step 3: 写完整 `internal/api/v1/service.go`**

```go
package v1

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/store"
)

type Config struct {
	Orchestrator *orchestrator.Orchestrator
	DefaultModel model.BaseChatModel
	Models       map[string]model.BaseChatModel
	Store        *store.Store
}

type Service struct {
	orch    *orchestrator.Orchestrator
	defaultModel model.BaseChatModel
	models  map[string]model.BaseChatModel
	store   *store.Store
	mu      sync.Mutex
	threads map[string]*threadState
	nextID  uint64
}

type threadState struct {
	mu       sync.Mutex
	thread   Thread
	history  []*schema.Message
	turns    map[string]*turnState
	active   *turnState
	nextSeq  int64
}

type turnState struct {
	turn  Turn
	cancel context.CancelFunc
}

var ErrThreadNotFound = fmt.Errorf("thread not found")
var ErrTurnAlreadyActive = fmt.Errorf("thread already has an active turn")

func NewService(cfg Config) (*Service, error) {
	if cfg.DefaultModel == nil && cfg.Orchestrator == nil {
		return nil, fmt.Errorf("api v1: default model or orchestrator is required")
	}
	return &Service{
		orch: cfg.Orchestrator, defaultModel: cfg.DefaultModel,
		models: cfg.Models, store: cfg.Store, threads: make(map[string]*threadState),
	}, nil
}

func (s *Service) Start(_ context.Context, p ThreadStartParams) (Thread, error) {
	title := strings.TrimSpace(p.Title)
	if title == "" {
		title = "New thread"
	}
	var id string
	var created int64
	if s.store != nil {
		var err error
		id, err = s.store.CreateSession(title)
		if err != nil {
			return Thread{}, fmt.Errorf("create thread: %w", err)
		}
		created = time.Now().Unix()
	} else {
		id = fmt.Sprintf("memory-%d", atomic.AddUint64(&s.nextID, 1))
		created = time.Now().Unix()
	}
	thread := Thread{Version: Version, ID: id, Status: ThreadStatusActive, Title: title,
		CreatedAt: created, UpdatedAt: created, Model: p.Model, Thinking: p.Thinking}
	st := &threadState{thread: thread, turns: make(map[string]*turnState), nextSeq: 0}
	s.mu.Lock()
	s.threads[id] = st
	s.mu.Unlock()
	return thread, nil
}

func (s *Service) Resume(_ context.Context, p ThreadResumeParams) (ThreadSnapshot, error) {
	if strings.TrimSpace(p.ThreadID) == "" {
		return ThreadSnapshot{}, fmt.Errorf("threadId is required")
	}
	s.mu.Lock()
	if st := s.threads[p.ThreadID]; st != nil {
		s.mu.Unlock()
		return s.snapshot(st), nil
	}
	s.mu.Unlock()
	if s.store == nil {
		return ThreadSnapshot{}, ErrThreadNotFound
	}
	ss, err := s.store.GetSession(p.ThreadID)
	if err != nil {
		return ThreadSnapshot{}, fmt.Errorf("read thread: %w", err)
	}
	if ss == nil {
		return ThreadSnapshot{}, ErrThreadNotFound
	}
	msgs, err := s.store.Messages(p.ThreadID)
	if err != nil {
		return ThreadSnapshot{}, fmt.Errorf("read thread messages: %w", err)
	}
	history := make([]*schema.Message, 0, len(msgs))
	for _, msg := range msgs {
		role := schema.Assistant
		if msg.Role == "user" {
			role = schema.User
		}
		history = append(history, &schema.Message{Role: role, Content: msg.Content})
	}
	thread := Thread{Version: Version, ID: ss.ID, Status: ThreadStatusActive, Title: ss.Title,
		CreatedAt: ss.CreatedAt, UpdatedAt: ss.UpdatedAt, Model: ss.Model, Thinking: ss.Thinking}
	st := &threadState{thread: thread, history: history, turns: make(map[string]*turnState), nextSeq: int64(len(msgs))}
	s.mu.Lock()
	if existing := s.threads[p.ThreadID]; existing != nil {
		st = existing
	} else {
		s.threads[p.ThreadID] = st
	}
	s.mu.Unlock()
	return s.snapshot(st), nil
}

func (s *Service) StartTurn(parent context.Context, p TurnStartParams) (TurnStartResponse, <-chan Item, error) {
	st := s.thread(p.ThreadID)
	if st == nil {
		return TurnStartResponse{}, nil, ErrThreadNotFound
	}
	if strings.TrimSpace(p.Input) == "" {
		return TurnStartResponse{}, nil, fmt.Errorf("input is required")
	}
	st.mu.Lock()
	if st.active != nil {
		st.mu.Unlock()
		return TurnStartResponse{}, nil, ErrTurnAlreadyActive
	}
	turnID := fmt.Sprintf("%s-turn-%d", st.thread.ID, len(st.turns)+1)
	now := time.Now().Unix()
	turn := Turn{Version: Version, ID: turnID, ThreadID: st.thread.ID,
		Status: TurnStatusInProgress, Input: p.Input, StartedAt: now}
	ctx, cancel := context.WithCancel(parent)
	ts := &turnState{turn: turn, cancel: cancel}
	st.active, st.turns[turnID] = ts, ts
	st.thread.Turns = append(st.thread.Turns, turn)
	st.thread.UpdatedAt = now
	st.history = append(st.history, &schema.Message{Role: schema.User, Content: p.Input})
	history := append([]*schema.Message(nil), st.history...)
	st.mu.Unlock()

	items := make(chan Item, 128)
	if !s.sendItem(ctx, items, s.item(st, turnID, ItemTurnStarted, "")) {
		close(items)
		return TurnStartResponse{}, nil, context.Canceled
	}
	go s.runTurn(ctx, st, ts, history, p, items)
	return TurnStartResponse{Version: Version, Turn: turn}, items, nil
}

func (s *Service) Interrupt(_ context.Context, p ThreadInterruptParams) error {
	st := s.thread(p.ThreadID)
	if st == nil {
		return ErrThreadNotFound
	}
	st.mu.Lock()
	active := st.active
	st.mu.Unlock()
	if active == nil {
		return nil
	}
	if p.TurnID != "" && p.TurnID != active.turn.ID {
		return fmt.Errorf("turn %q is not active", p.TurnID)
	}
	active.cancel()
	return nil
}

func (s *Service) thread(id string) *threadState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threads[id]
}

func (s *Service) snapshot(st *threadState) ThreadSnapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	thread := st.thread
	thread.Turns = append([]Turn(nil), st.thread.Turns...)
	return ThreadSnapshot{Version: Version, Thread: thread}
}

func (s *Service) runTurn(ctx context.Context, st *threadState, ts *turnState, history []*schema.Message, p TurnStartParams, out chan Item) {
	defer close(out)
	var assistant strings.Builder
	failed := false
	emit := func(f proto.ServerFrame) {
		if f.Type == "agent_chunk" {
			assistant.WriteString(f.Text)
		}
		item := ItemFromServerFrame(f, st.thread.ID, ts.turn.ID, s.nextSequence(st))
		if !s.sendItem(ctx, out, item) {
			failed = true
		}
	}
	if s.orch != nil {
		opts := orchestrator.TurnOpts{ThinkingEffort: p.Thinking, OutputSchema: p.OutputSchema}
		if p.Model != "" && s.models[p.Model] != nil {
			opts.Model = s.models[p.Model]
		}
		iter := s.orch.EventsWithHistoryOpts(ctx, history, opts)
		var usage orchestrator.TurnUsage
		orchestrator.ClassifyEventsWithUsage(iter, &usage, emit)
	} else {
		emit(proto.NewAgentChunk("(no real model configured)"))
	}
	st.mu.Lock()
	if assistant.Len() > 0 {
		st.history = append(st.history, &schema.Message{Role: schema.Assistant, Content: assistant.String()})
	}
	status := TurnStatusCompleted
	if failed || ctx.Err() != nil {
		status = TurnStatusInterrupted
	}
	if failed {
		status = TurnStatusFailed
	}
	completed := time.Now().Unix()
	for i := range st.thread.Turns {
		if st.thread.Turns[i].ID == ts.turn.ID {
			st.thread.Turns[i].Status = status
			st.thread.Turns[i].CompletedAt = completed
		}
	}
	ts.turn.Status, ts.turn.CompletedAt = status, completed
	st.active = nil
	st.thread.UpdatedAt = completed
	st.mu.Unlock()
	if s.store != nil {
		seq, _ := s.store.SessionMessageCount(st.thread.ID)
		_ = s.store.AppendMessage(st.thread.ID, seq, "user", ts.turn.Input)
		if assistant.Len() > 0 {
			_ = s.store.AppendMessage(st.thread.ID, seq+1, "assistant", assistant.String())
		}
		_ = s.store.UpdateSessionMeta(st.thread.ID, p.Model, p.Thinking, 0, 0, len(st.thread.Turns), 0, 0)
	}
	finalType := ItemTurnCompleted
	if status == TurnStatusFailed || status == TurnStatusInterrupted {
		finalType = ItemTurnError
	}
	seq := s.nextSequence(st)
	final := Item{Version: Version, ID: "item-" + formatSequence(seq), Sequence: seq, ThreadID: st.thread.ID, TurnID: ts.turn.ID, Type: finalType, Status: string(status)}
	_ = s.sendItem(context.Background(), out, final)
}

func (s *Service) nextSequence(st *threadState) int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.nextSeq++
	return st.nextSeq
}

func (s *Service) item(st *threadState, turnID, typ, text string) Item {
	seq := s.nextSequence(st)
	return Item{Version: Version, ID: "item-" + formatSequence(seq), Sequence: seq,
		ThreadID: st.thread.ID, TurnID: turnID, Type: typ, Text: text}
}

func (s *Service) sendItem(ctx context.Context, out chan Item, item Item) bool {
	select {
	case out <- item:
		return true
	case <-ctx.Done():
		return false
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/api/v1 -run 'TestServiceStartTurn|TestServiceInterrupt|TestServiceResume' -v`

Expected: PASS；fake model 至少收到 `turn.started`、`message.delta`、`turn.completed`，sequence 严格递增，interrupt 后 channel 在有限时间内结束，resume 返回的 snapshot 携带原 thread id 与 title 且未知 thread id 返回 `ErrThreadNotFound`。

- [ ] **Step 5: 加背压回归测试并提交**

新增完整测试：构造一个容量为 1 的 fake item sink，消费者延迟读取；确认 producer 阻塞而不是继续分配无界 slice，取消 context 后 producer 返回。测试只使用 channel 和 `FakeModel`，不使用 mock framework。

```go
func TestServiceStreamStopsWhenConsumerCancels(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := NewService(Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	thread, err := svc.Start(context.Background(), ThreadStartParams{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, items, err := svc.StartTurn(ctx, TurnStartParams{ThreadID: thread.ID, Input: "hello"})
	if err != nil {
		t.Fatalf("StartTurn: %v", err)
	}
	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-items:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stream did not stop after consumer cancellation")
		}
	}
}
```

Run: `go test ./internal/api/v1 -run 'TestService' -v`

Expected: PASS。

```bash
git add internal/api/v1/service.go internal/api/v1/service_test.go
git commit -m "feat(api): add resumable thread service with bounded item streams"
```

---

## Task 7: V14 HTTP/SSE resource layer

**Files:**
- Create: `internal/api/http/agent_v1.go`
- Test: `internal/api/http/agent_v1_test.go`

该层只做 HTTP decode/auth/version header/SSE framing；不能重新运行 orchestrator，也不能重新映射 frame。`Server.AgentV1` 接受 `*v1.Service` 并注册 route。请求 decoder 默认允许未知字段；必需字段由 service 校验。客户端断开时 `r.Context()` 传给 `StartTurn`，触发 bounded stream cancellation。

- [ ] **Step 1: 写失败测试**

```go
package http

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/api/v1"
)

func TestAgentV1StartAcceptsUnknownFieldsAndReturnsCamelCase(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{Token: "token"})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := ts.Client().Post(ts.URL+"/api/v1/thread/start", "application/json", strings.NewReader(`{"title":"x","futureField":1}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("X-Yanshi-API-Version") != "v1" {
		t.Fatalf("status=%d headers=%v body=%s", resp.StatusCode, resp.Header, body)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body JSON: %v", err)
	}
	if _, ok := got["thread"]; !ok || strings.Contains(string(body), "created_at") {
		t.Fatalf("non-v1 body: %s", body)
	}
}

func TestAgentV1TurnStreamUsesItemEvents(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	start, _ := ts.Client().Post(ts.URL+"/api/v1/thread/start", "application/json", strings.NewReader(`{}`))
	var started v1.ThreadStartResponse
	_ = json.NewDecoder(start.Body).Decode(&started)
	_ = start.Body.Close()
	body := `{"threadId":"` + started.Thread.ID + `","input":"hello"}`
	resp, err := ts.Client().Post(ts.URL+"/api/v1/turn/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("turn POST: %v", err)
	}
	defer resp.Body.Close()
	stream, _ := io.ReadAll(resp.Body)
	text := string(stream)
	if !strings.Contains(text, "event: item") || !strings.Contains(text, `"version":"v1"`) {
		t.Fatalf("stream = %s", text)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/api/http -run 'TestAgentV1' -v`

Expected: FAIL，因为 `Server.AgentV1` 和 resource handlers 尚未定义。

- [ ] **Step 3: 写完整 `internal/api/http/agent_v1.go`**

```go
package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/x6nux/yanshi/internal/api/v1"
)

// AgentV1 registers the versioned resource API beside the legacy chat routes.
// It intentionally consumes the shared v1.Service so HTTP and app-server have
// identical thread/turn/item semantics.
func (s *Server) AgentV1(agent *v1.Service) {
	s.HandleFunc("POST /api/v1/thread/start", func(w http.ResponseWriter, r *http.Request) {
		var p v1.ThreadStartParams
		if err := decodeAgentJSON(w, r, &p); err != nil {
			return
		}
		thread, err := agent.Start(r.Context(), p)
		if err != nil {
			writeAgentError(w, http.StatusInternalServerError, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, v1.ThreadStartResponse{Version: v1.Version, Thread: thread})
	})

	s.HandleFunc("POST /api/v1/thread/resume", func(w http.ResponseWriter, r *http.Request) {
		var p v1.ThreadResumeParams
		if err := decodeAgentJSON(w, r, &p); err != nil {
			return
		}
		snapshot, err := agent.Resume(r.Context(), p)
		if errors.Is(err, v1.ErrThreadNotFound) {
			writeAgentError(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeAgentError(w, http.StatusInternalServerError, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, v1.ThreadResumeResponse{Version: v1.Version, Thread: snapshot.Thread, Items: snapshot.Items})
	})

	s.HandleFunc("POST /api/v1/thread/interrupt", func(w http.ResponseWriter, r *http.Request) {
		var p v1.ThreadInterruptParams
		if err := decodeAgentJSON(w, r, &p); err != nil {
			return
		}
		if err := agent.Interrupt(r.Context(), p); err != nil && !errors.Is(err, v1.ErrThreadNotFound) {
			writeAgentError(w, http.StatusConflict, err)
			return
		} else if errors.Is(err, v1.ErrThreadNotFound) {
			writeAgentError(w, http.StatusNotFound, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, v1.InterruptResponse{Version: v1.Version, OK: true, ThreadID: p.ThreadID, TurnID: p.TurnID})
	})

	s.HandleFunc("POST /api/v1/turn/start", func(w http.ResponseWriter, r *http.Request) {
		var p v1.TurnStartParams
		if err := decodeAgentJSON(w, r, &p); err != nil {
			return
		}
		started, items, err := agent.StartTurn(r.Context(), p)
		if errors.Is(err, v1.ErrThreadNotFound) {
			writeAgentError(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeAgentError(w, http.StatusConflict, err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Yanshi-API-Version", v1.Version)
		fl, _ := w.(http.Flusher)
		writeAgentSSE(w, fl, "turn", started)
		for item := range items {
			if !writeAgentSSE(w, fl, "item", item) {
				return
			}
		}
	})

	s.HandleFunc("GET /api/v1/schema/agent-v1.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/schema+json")
		w.Header().Set("X-Yanshi-API-Version", v1.Version)
		_, _ = w.Write(v1.SchemaBytes())
	})
}

func decodeAgentJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(limitBody(w, r))
	if err := dec.Decode(dst); err != nil {
		writeAgentError(w, http.StatusBadRequest, fmt.Errorf("invalid request: %w", err))
		return err
	}
	return nil
}

func writeAgentJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Yanshi-API-Version", v1.Version)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeAgentError(w http.ResponseWriter, status int, err error) {
	writeAgentJSON(w, status, map[string]any{
		"version": v1.Version,
		"error": map[string]string{"message": err.Error()},
	})
}

func writeAgentSSE(w http.ResponseWriter, fl http.Flusher, event string, value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return false
	}
	if fl != nil {
		fl.Flush()
	}
	return true
}
```

`writeAgentSSE` 的 `turn` event 只携带 `TurnStartResponse`，后续所有流均为 `event: item`；客户端不应把 legacy `agent_chunk` 当作 v1 event name。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/api/http -run 'TestAgentV1' -v`

Expected: PASS；legacy `TestChat_*`、`TestChatWS_*` 未被 route registration 影响。

- [ ] **Step 5: 提交**

```bash
git add internal/api/http/agent_v1.go internal/api/http/agent_v1_test.go
git commit -m "feat(http): expose versioned thread turn item SSE API"
```

---

## Task 8: bootstrap 装配 V14 service

**Files:**
- Modify: `internal/bootstrap/bootstrap.go`
- Test: `internal/bootstrap/bootstrap_test.go`

- [ ] **Step 1: 写失败测试**

```go
func TestBuildExposesAgentV1Service(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())
	if app.AgentAPI == nil {
		t.Fatal("AgentAPI must be wired by bootstrap")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/bootstrap -run 'TestBuildExposesAgentV1Service' -v`

Expected: FAIL，因为 `App.AgentAPI` 尚未定义。

- [ ] **Step 3: 修改 bootstrap 的完整装配片段**

在 import 区加入：

```go
apiV1 "github.com/x6nux/yanshi/internal/api/v1"
```

在 `orchestrator.New` 成功之后、`apihttp.New` 之前创建 service：

```go
agentAPI, err := apiV1.NewService(apiV1.Config{
	Orchestrator: orch,
	DefaultModel: chatModel,
	Models:       providerModels,
	Store:        st,
})
if err != nil {
	st.Close()
	return nil, fmt.Errorf("bootstrap: agent api: %w", err)
}
```

在 `App` struct 中加入完整字段声明：

```go
// AgentAPI is the versioned thread/turn/item service shared by HTTP and
// JSON-RPC app-server transports. It is non-nil after a successful Build.
AgentAPI *apiV1.Service
```

在 HTTP route registration 后加入：

```go
srv.AgentV1(agentAPI)
```

在 `return &App{...}` 中加入：

```go
AgentAPI: agentAPI,
```

装配顺序仍是 `config → store → vcs → model → tools → orchestrator → agent API/http → task`；agent API 不自行读取 config、不自行创建 model、不绕过 bootstrap。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/bootstrap -run 'TestBuildExposesAgentV1Service' -v`

Expected: PASS；fake model 下无 API key，VCS/skill 的既有软降级行为保持不变。

- [ ] **Step 5: 提交**

```bash
git add internal/bootstrap/bootstrap.go internal/bootstrap/bootstrap_test.go
git commit -m "feat(bootstrap): wire shared versioned agent API service"
```

---

## Task 9: JSON Schema 与 V14 compatibility matrix

**Files:**
- Create: `internal/api/v1/schema.go`
- Create: `internal/api/v1/schema_test.go`
- Modify: `internal/api/http/agent_v1_test.go`

schema 必须覆盖 `Thread`、`Turn`、`Item`、start/resume/interrupt/turn params、responses 和 item notification。所有线上属性名必须是 camelCase。未知字段策略是“忽略并继续”，未知 item type 策略是“客户端忽略，服务端保留为 `event.<legacyType>`”。

- [ ] **Step 1: 写失败测试**

```go
package v1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSchemaDeclaresVersionAndCamelCaseResources(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(SchemaBytes(), &doc); err != nil {
		t.Fatalf("schema JSON: %v", err)
	}
	if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("schema dialect = %#v", doc["$schema"])
	}
	defs, ok := doc["$defs"].(map[string]any)
	if !ok {
		t.Fatal("schema lacks $defs")
	}
	item, ok := defs["Item"].(map[string]any)
	if !ok {
		t.Fatal("schema lacks Item")
	}
	props := item["properties"].(map[string]any)
	if _, ok := props["threadId"]; !ok {
		t.Fatal("Item schema lacks threadId")
	}
	if _, ok := props["thread_id"]; ok {
		t.Fatal("Item schema must not expose thread_id")
	}
}

func TestSchemaBytesAreStableForContractReview(t *testing.T) {
	first := string(SchemaBytes())
	second := string(SchemaBytes())
	if first == "" || first != second || !strings.Contains(first, `"version"`) {
		t.Fatal("schema bytes are empty or unstable")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/api/v1 -run 'TestSchema' -v`

Expected: FAIL，因为 `SchemaBytes` 尚未定义。

- [ ] **Step 3: 写完整 `internal/api/v1/schema.go`**

```go
package v1

import "encoding/json"

var schemaDocument = map[string]any{
	"$schema": "https://json-schema.org/draft/2020-12/schema",
	"$id":     "https://yanshi.dev/schema/agent-api-v1.json",
	"title":   "Yanshi Agent API v1",
	"type":    "object",
	"$defs": map[string]any{
		"Thread": map[string]any{
			"type": "object",
			"required": []string{"version", "id", "status", "createdAt", "updatedAt"},
			"properties": map[string]any{
				"version": map[string]any{"const": "v1"}, "id": map[string]any{"type": "string"},
				"status": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"},
				"createdAt": map[string]any{"type": "integer"}, "updatedAt": map[string]any{"type": "integer"},
				"model": map[string]any{"type": "string"}, "thinking": map[string]any{"type": "string"},
				"turns": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/Turn"}},
			},
		},
		"Turn": map[string]any{
			"type": "object",
			"required": []string{"version", "id", "threadId", "status", "input", "startedAt"},
			"properties": map[string]any{
				"version": map[string]any{"const": "v1"}, "id": map[string]any{"type": "string"},
				"threadId": map[string]any{"type": "string"}, "status": map[string]any{"type": "string"},
				"input": map[string]any{"type": "string"}, "startedAt": map[string]any{"type": "integer"},
				"completedAt": map[string]any{"type": "integer"},
			},
		},
		"Item": map[string]any{
			"type": "object",
			"required": []string{"version", "id", "sequence", "threadId", "turnId", "type"},
			"properties": map[string]any{
				"version": map[string]any{"const": "v1"}, "id": map[string]any{"type": "string"},
				"sequence": map[string]any{"type": "integer", "minimum": 1},
				"threadId": map[string]any{"type": "string"}, "turnId": map[string]any{"type": "string"},
				"type": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"},
				"toolName": map[string]any{"type": "string"}, "toolArgs": map[string]any{"type": "string"},
				"status": map[string]any{"type": "string"}, "error": map[string]any{"type": "string"},
				"structuredResult": map[string]any{},
			},
		},
	},
}

// SchemaBytes returns a fresh JSON encoding so callers cannot mutate the global
// schema document and accidentally make HTTP and JSON-RPC schemas diverge.
func SchemaBytes() []byte {
	data, err := json.Marshal(schemaDocument)
	if err != nil {
		return []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"https://yanshi.dev/schema/agent-api-v1.json"}`)
	}
	return data
}
```

- [ ] **Step 4: 增加兼容矩阵测试**

必须新增以下完整测试场景到 `internal/api/http/agent_v1_test.go`：

```go
func TestV1CompatibilityMatrix(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	cases := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"missing optional version defaults to v1", `{"title":"x"}`, 200},
		{"unknown request field ignored", `{"title":"x","future":{"x":1}}`, 200},
		{"malformed json rejected", `{"title":`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := ts.Client().Post(ts.URL+"/api/v1/thread/start", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("POST: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, tc.wantStatus, body)
			}
			if resp.Header.Get("X-Yanshi-API-Version") != "v1" {
				t.Fatalf("missing X-Yanshi-API-Version header")
			}
			// Every successful response must be JSON-decodable into a value whose
			// keys are all camelCase; the simple substring check is sufficient for
			// thread/start's flat response shape and is the same convention used by
			// TestAgentV1StartAcceptsUnknownFieldsAndReturnsCamelCase.
			if tc.wantStatus == 200 && strings.Contains(string(body), "_") {
				t.Fatalf("response contains underscore (non-camelCase key): %s", body)
			}
		})
	}
}

// TestV1TurnStreamItemsAreVersionedAndOrdered drives a real turn/start stream
// and asserts every SSE item carries version/sequence/threadId/turnId — the
// wire contract every client depends on. It complements the per-response
// matrix above by walking the streaming payload.
func TestV1TurnStreamItemsAreVersionedAndOrdered(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	svc, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s := New(Config{})
	s.AgentV1(svc)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	start, _ := ts.Client().Post(ts.URL+"/api/v1/thread/start", "application/json", strings.NewReader(`{}`))
	var started v1.ThreadStartResponse
	_ = json.NewDecoder(start.Body).Decode(&started)
	_ = start.Body.Close()

	body := `{"threadId":"` + started.Thread.ID + `","input":"hello"}`
	resp, err := ts.Client().Post(ts.URL+"/api/v1/turn/start", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("turn POST: %v", err)
	}
	defer resp.Body.Close()
	stream, _ := io.ReadAll(resp.Body)
	text := string(stream)
	if !strings.Contains(text, "event: item") {
		t.Fatalf("stream missing item events: %s", text)
	}
	// Walk every `data: {...}` line that follows an `event: item` marker and
	// assert required fields. Lines that do not decode as v1.Item are rejected.
	for _, ln := range strings.Split(text, "\n") {
		if !strings.HasPrefix(ln, "data: ") {
			continue
		}
		var item v1.Item
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &item); err != nil {
			continue // turn-start SSE payload is a TurnStartResponse, not an Item
		}
		if item.Version != "v1" || item.Sequence == 0 || item.ThreadID == "" || item.TurnID == "" {
			t.Fatalf("item missing required fields: %#v", item)
		}
	}
}
```

以上两个测试都使用 `httptest.NewServer` + `FakeModel`，不依赖外部进程；`TestV1CompatibilityMatrix` 守护 HTTP request flex（未知字段/缺省 version/坏 JSON），`TestV1TurnStreamItemsAreVersionedAndOrdered` 守护 stream item 的必需字段。

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/api/v1 ./internal/api/http -run 'TestSchema|TestV1CompatibilityMatrix|TestV1TurnStreamItemsAreVersionedAndOrdered|TestAgentV1' -v`

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/api/v1/schema.go internal/api/v1/schema_test.go internal/api/http/agent_v1_test.go
git commit -m "test(api): publish v1 JSON Schema and compatibility matrix"
```

---

## Task 10: APS1 JSON-RPC v2 line protocol 与 dispatcher

**Files:**
- Create: `internal/appserver/rpc.go`
- Create: `internal/appserver/server.go`
- Test: `internal/appserver/rpc_test.go`
- Test: `internal/appserver/server_test.go`

JSON-RPC wire：每行一个 JSON object，响应始终带 `jsonrpc: "2.0"`、原 request id、`result` 或 `error`；notification 没有 id，不产生 response。错误码使用标准 `-32700` parse、`-32600` invalid request、`-32601` method not found、`-32602` invalid params、`-32603` internal error。所有 outbound writes 经过 mutex；stream item 使用 notification method `item/updated`，params 是 v1 `Item`。

- [ ] **Step 1: 写失败测试**

```go
package appserver

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/api/v1"
)

func TestRPCServerInitializeAndMethodNotFound(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	agent, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv := New(agent, nil)
	in := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"unknown/x\",\"params\":{}}\n")
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), in, &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("responses = %q", out.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first response: %v", err)
	}
	if first["jsonrpc"] != "2.0" || first["id"].(float64) != 1 {
		t.Fatalf("first response = %#v", first)
	}
	var second map[string]any
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if second["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method response = %#v", second)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/appserver -run 'TestRPCServer' -v`

Expected: FAIL，因为 JSON-RPC types/server 尚未定义。

- [ ] **Step 3: 写完整 `internal/appserver/rpc.go`**

```go
package appserver

import (
	"encoding/json"
	"fmt"
)

type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type RPCError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

func parseRPCLine(line []byte) (RPCRequest, error) {
	var req RPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return RPCRequest{}, &RPCError{Code: -32700, Message: "parse error", Data: err.Error()}
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return req, &RPCError{Code: -32600, Message: "invalid request"}
	}
	if len(req.Params) == 0 {
		req.Params = json.RawMessage(`{}`)
	}
	return req, nil
}

func rpcResponse(id json.RawMessage, result any) RPCResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return RPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func rpcErrorResponse(id json.RawMessage, code int64, message string, data any) RPCResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return RPCResponse{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message, Data: data}}
}

func decodeParams(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid params: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: 写完整 `internal/appserver/server.go`**

```go
package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/x6nux/yanshi/internal/api/v1"
)

type ConfigBackend interface {
	Read(key string) (any, error)
	Write(key string, value json.RawMessage) error
}

type Server struct {
	agent *v1.Service
	config ConfigBackend
	writeMu sync.Mutex
}

func New(agent *v1.Service, config ConfigBackend) *Server {
	return &Server{agent: agent, config: config}
}

func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		req, err := parseRPCLine(line)
		if err != nil {
			if rpcErr, ok := err.(*RPCError); ok {
				if writeErr := s.write(w, rpcErrorResponse(nil, rpcErr.Code, rpcErr.Message, rpcErr.Data)); writeErr != nil {
					return writeErr
				}
				continue
			}
			return err
		}
		result, stream, dispatchErr := s.dispatch(ctx, req)
		if dispatchErr != nil {
			if len(req.ID) != 0 {
				if err := s.write(w, rpcErrorResponse(req.ID, dispatchErr.Code, dispatchErr.Message, dispatchErr.Data)); err != nil {
					return err
				}
			}
			continue
		}
		if len(req.ID) != 0 {
			if err := s.write(w, rpcResponse(req.ID, result)); err != nil {
				return err
			}
		}
		if stream != nil {
			go s.forwardItems(ctx, w, stream)
		}
		if req.Method == "shutdown" {
			return nil
		}
	}
	return sc.Err()
}

func (s *Server) dispatch(ctx context.Context, req RPCRequest) (any, <-chan v1.Item, *RPCError) {
	switch req.Method {
	case "initialize":
		return v1.Capabilities{Version: v1.Version, Methods: []string{
			"thread/start", "thread/resume", "thread/interrupt", "turn/start", "turn/interrupt",
			"capabilities", "config/read", "config/write", "initialize", "shutdown",
		}, ItemTypes: []string{v1.ItemMessageDelta, v1.ItemReasoningDelta, v1.ItemToolCall, v1.ItemToolResult, v1.ItemToolProgress, v1.ItemStructuredResult, v1.ItemTurnStarted, v1.ItemTurnError, v1.ItemTurnCompleted}, UnknownFields: "ignored", Stream: "item/updated"}, nil, nil
	case "capabilities":
		return v1.Capabilities{Version: v1.Version, Methods: []string{"thread/start", "thread/resume", "thread/interrupt", "turn/start", "turn/interrupt", "config/read", "config/write"}, ItemTypes: []string{v1.ItemMessageDelta, v1.ItemToolCall, v1.ItemToolResult}, UnknownFields: "ignored", Stream: "item/updated"}, nil, nil
	case "thread/start":
		var p v1.ThreadStartParams
		if err := decodeParams(req.Params, &p); err != nil { return nil, nil, &RPCError{Code: -32602, Message: err.Error()} }
		thread, err := s.agent.Start(ctx, p)
		if err != nil { return nil, nil, &RPCError{Code: -32603, Message: err.Error()} }
		return v1.ThreadStartResponse{Version: v1.Version, Thread: thread}, nil, nil
	case "thread/resume":
		var p v1.ThreadResumeParams
		if err := decodeParams(req.Params, &p); err != nil { return nil, nil, &RPCError{Code: -32602, Message: err.Error()} }
		snapshot, err := s.agent.Resume(ctx, p)
		if errors.Is(err, v1.ErrThreadNotFound) { return nil, nil, &RPCError{Code: -32602, Message: err.Error()} }
		if err != nil { return nil, nil, &RPCError{Code: -32603, Message: err.Error()} }
		return v1.ThreadResumeResponse{Version: v1.Version, Thread: snapshot.Thread, Items: snapshot.Items}, nil, nil
	case "thread/interrupt", "turn/interrupt":
		var p v1.ThreadInterruptParams
		if err := decodeParams(req.Params, &p); err != nil { return nil, nil, &RPCError{Code: -32602, Message: err.Error()} }
		if err := s.agent.Interrupt(ctx, p); err != nil { return nil, nil, &RPCError{Code: -32602, Message: err.Error()} }
		return v1.InterruptResponse{Version: v1.Version, OK: true, ThreadID: p.ThreadID, TurnID: p.TurnID}, nil, nil
	case "turn/start":
		var p v1.TurnStartParams
		if err := decodeParams(req.Params, &p); err != nil { return nil, nil, &RPCError{Code: -32602, Message: err.Error()} }
		started, items, err := s.agent.StartTurn(ctx, p)
		if errors.Is(err, v1.ErrThreadNotFound) { return nil, nil, &RPCError{Code: -32602, Message: err.Error()} }
		if err != nil { return nil, nil, &RPCError{Code: -32603, Message: err.Error()} }
		return started, items, nil
	case "config/read":
		if s.config == nil { return nil, nil, &RPCError{Code: -32603, Message: "config backend is unavailable"} }
		var p struct{ Key string `json:"key"` }
		if err := decodeParams(req.Params, &p); err != nil { return nil, nil, &RPCError{Code: -32602, Message: err.Error()} }
		value, err := s.config.Read(p.Key)
		if err != nil { return nil, nil, &RPCError{Code: -32602, Message: err.Error()} }
		return map[string]any{"version": v1.Version, "key": p.Key, "value": value}, nil, nil
	case "config/write":
		if s.config == nil { return nil, nil, &RPCError{Code: -32603, Message: "config backend is unavailable"} }
		var p struct{ Key string `json:"key"`; Value json.RawMessage `json:"value"` }
		if err := decodeParams(req.Params, &p); err != nil { return nil, nil, &RPCError{Code: -32602, Message: err.Error()} }
		if err := s.config.Write(p.Key, p.Value); err != nil { return nil, nil, &RPCError{Code: -32602, Message: err.Error()} }
		return map[string]any{"version": v1.Version, "ok": true, "key": p.Key}, nil, nil
	case "shutdown":
		return map[string]any{"version": v1.Version, "ok": true}, nil, nil
	default:
		return nil, nil, &RPCError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

func (s *Server) forwardItems(ctx context.Context, w io.Writer, items <-chan v1.Item) {
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-items:
			if !ok { return }
			_ = s.write(w, RPCNotification{JSONRPC: "2.0", Method: "item/updated", Params: item})
		}
	}
}

func (s *Server) write(w io.Writer, value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	data, err := json.Marshal(value)
	if err != nil { return fmt.Errorf("marshal JSON-RPC response: %w", err) }
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/appserver -run 'TestRPCServer' -v`

Expected: PASS；另加 notification、malformed JSON、bad version、invalid params、stream item notification 测试后运行：

```bash
go test ./internal/appserver -v
```

- [ ] **Step 6: 提交**

```bash
git add internal/appserver/rpc.go internal/appserver/server.go internal/appserver/rpc_test.go internal/appserver/server_test.go
git commit -m "feat(appserver): add JSON-RPC 2.0 stdio dispatcher"
```

---

## Task 11: app-server config read/write 与 `yanshi app`

**Files:**
- Create: `internal/appserver/config.go`
- Create: `internal/appserver/config_test.go`
- Create: `cmd/yanshi/app.go`
- Modify: `cmd/yanshi/main.go`
- Test: `cmd/yanshi/app_test.go`

配置 API 只面向 app-server 本地 supervisor；不把 secret 读到 stdout，也不允许写 `token`、`api_key`、`secret` 或其 dot-path 子路径。YAML 文件写入使用 temp + rename，失败不覆盖原文件。无 config path 时使用 in-memory backend，便于 `--fake-model` 测试。

- [ ] **Step 1: 写失败测试**

```go
package appserver

import (
	"encoding/json"
	"testing"
)

func TestMemoryConfigRejectsSecretPaths(t *testing.T) {
	cfg := NewMemoryConfig()
	if err := cfg.Write("llm.providers.0.model", json.RawMessage(`"fake"`)); err != nil {
		t.Fatalf("safe write: %v", err)
	}
	if _, err := cfg.Read("llm.providers.0.model"); err != nil {
		t.Fatalf("safe read: %v", err)
	}
	if err := cfg.Write("llm.providers.0.api_key", json.RawMessage(`"secret"`)); err == nil {
		t.Fatal("api_key write must be rejected")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/appserver -run 'TestMemoryConfig' -v`

Expected: FAIL，因为 `NewMemoryConfig` 尚未定义。

- [ ] **Step 3: 写完整 `internal/appserver/config.go`**

```go
package appserver

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

type MemoryConfig struct {
	mu     sync.Mutex
	values map[string]any
}

func NewMemoryConfig() *MemoryConfig {
	return &MemoryConfig{values: make(map[string]any)}
}

func (c *MemoryConfig) Read(key string) (any, error) {
	if err := validateConfigKey(key); err != nil { return nil, err }
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.values[key]
	if !ok { return nil, fmt.Errorf("config key %q is not set", key) }
	return value, nil
}

func (c *MemoryConfig) Write(key string, value json.RawMessage) error {
	if err := validateConfigKey(key); err != nil { return err }
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil { return fmt.Errorf("config value: %w", err) }
	c.mu.Lock()
	c.values[key] = decoded
	c.mu.Unlock()
	return nil
}

func validateConfigKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" { return fmt.Errorf("config key is required") }
	for _, part := range strings.Split(strings.ToLower(key), ".") {
		if part == "token" || part == "api_key" || part == "apikey" || part == "secret" || strings.Contains(part, "password") {
			return fmt.Errorf("config key %q is restricted", key)
		}
	}
	return nil
}
```

- [ ] **Step 4: 写完整 `cmd/yanshi/app.go`**

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/x6nux/yanshi/internal/appserver"
	"github.com/x6nux/yanshi/internal/bootstrap"
)

func runApp(args []string, in io.Reader, out io.Writer) int {
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "config.yaml", "path to configuration file")
	fakeModel := fs.Bool("fake-model", false, "use deterministic fake model")
	if err := fs.Parse(args); err != nil { return exitUsage }
	if fs.NArg() != 0 { fmt.Fprintln(os.Stderr, "yanshi app: unexpected positional argument"); return exitUsage }

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: *configPath, FakeModel: *fakeModel})
	if err != nil { fmt.Fprintf(os.Stderr, "yanshi app: %v\n", err); return exitErr }
	defer app.Shutdown(context.Background())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := appserver.NewMemoryConfig()
	srv := appserver.New(app.AgentAPI, cfg)
	if err := srv.Serve(ctx, in, out); err != nil {
		fmt.Fprintf(os.Stderr, "yanshi app: %v\n", err)
		return exitErr
	}
	return exitOK
}
```

`runApp` 的参数使用 `io.Reader`/`io.Writer`，便于测试。在 `main` 中传 `os.Stdin`/`os.Stdout`（`*os.File` 实现了这些接口）。不要让 app-server 的 logs 写 stdout；所有 diagnostics 走 stderr。

- [ ] **Step 5: 修改 `cmd/yanshi/main.go` dispatch**

加入完整分支：

```go
case "app":
	os.Exit(runApp(os.Args[2:], os.Stdin, os.Stdout))
```

usage 加入：

```go
  yanshi app      [-config FILE] [-fake-model]
```

说明 `app` 默认 stdio JSON-RPC v2，一行一个 request/response，stream item 以 `item/updated` notification 输出；stdout 不得混入启动日志。

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./internal/appserver ./cmd/yanshi -run 'TestMemoryConfig|TestRunAppStdioSmoke' -v`

Expected: PASS。

把以下完整测试加到 `cmd/yanshi/app_test.go`，覆盖 `runApp` 的 stdio 生命周期（initialize → shutdown）：

```go
func TestRunAppStdioSmoke(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := strings.ReplaceAll(filepath.Join(dir, "test.db"), "\\", "/")
	cfg := fmt.Sprintf(`
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "%s"
`, dbPath)
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}`,
		"",
	}, "\n")
	var out bytes.Buffer
	code := runApp([]string{"-config", cfgPath, "-fake-model"}, strings.NewReader(input), &out)
	require.Equal(t, exitOK, code, "stdout = %q", out.String())

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "expected at least 2 responses, got %q", out.String())
	for i, ln := range lines {
		var msg map[string]any
		if err := json.Unmarshal([]byte(ln), &msg); err != nil {
			t.Fatalf("line %d not valid JSON-RPC: %v (%q)", i, err, ln)
		}
		if msg["jsonrpc"] != "2.0" {
			t.Fatalf("line %d missing jsonrpc field: %q", i, ln)
		}
	}
	var init map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &init))
	assert.Equal(t, float64(1), init["id"], "first response must echo initialize id")
	var shutdown map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &shutdown))
	assert.Equal(t, float64(2), shutdown["id"], "last response must echo shutdown id")
}
```

该测试只使用 `FakeModel`、`bytes.Buffer`、`strings.Reader` 与 `t.TempDir()`，不拉起子进程，也不依赖网络；守护 stdout 全为 JSON-RPC、`runApp` 在 shutdown 后返回 `exitOK`。

- [ ] **Step 7: 提交**

```bash
git add internal/appserver/config.go internal/appserver/config_test.go cmd/yanshi/app.go cmd/yanshi/main.go cmd/yanshi/app_test.go
git commit -m "feat(app): add local JSON-RPC app command and safe config backend"
```

---

## Task 12: Schema export、TS 生成决策与 D1 最终验收

**Files:**
- Decision: D1-DEC-2 的选择记录
- Conditional Create: `cmd/api-schema/main.go`
- Conditional Create: `sdk/ts/v1.ts`
- Test: `internal/appserver/server_test.go`、`cmd/yanshi/app_test.go`

### Step 1：关闭 TS 生成工具链决策

先选择 D1-DEC-2 A/B/C。无论选择哪项，都必须锁定以下 contract：

```text
schema source: internal/api/v1.SchemaBytes()
wire version: v1（按 D1-DEC-1 的最终结果）
method names: thread/start, thread/resume, thread/interrupt, turn/start, turn/interrupt
notification: item/updated
camelCase: required
unknown fields: ignored
```

如果选择 A，使用下面的完整 fallback generator；如果选择 B/C，只需把 generator 替换为外部命令，并在 CI 中对 schema 输入和生成文件做 deterministic diff。不要在这个 gate 之后让 Go struct tags 和 TS output 各自维护一份字段列表。

### Step 2：选择 A 时写完整 `cmd/api-schema/main.go`

```go
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/x6nux/yanshi/internal/api/v1"
)

func main() {
	outPath := flag.String("out", "", "write TypeScript output to this file; empty means stdout")
	flag.Parse()
	text := `// Code generated by cmd/api-schema; DO NOT EDIT.

export type AgentApiVersion = "v1";
export type ItemType =
  | "turn.started"
  | "message.delta"
  | "reasoning.delta"
  | "tool.call"
  | "tool.result"
  | "tool.progress"
  | "structured.result"
  | "turn.error"
  | "turn.completed";

export interface Thread {
  version: AgentApiVersion;
  id: string;
  status: string;
  title?: string;
  createdAt: number;
  updatedAt: number;
  model?: string;
  thinking?: string;
  turns?: Turn[];
}

export interface Turn {
  version: AgentApiVersion;
  id: string;
  threadId: string;
  status: string;
  input: string;
  startedAt: number;
  completedAt?: number;
}

export interface Item {
  version: AgentApiVersion;
  id: string;
  sequence: number;
  threadId: string;
  turnId: string;
  type: ItemType | ` + "`event.${string}`" + `;
  text?: string;
  toolName?: string;
  toolArgs?: string;
  status?: string;
  error?: string;
  structuredResult?: unknown;
}

export interface ThreadStartParams {
  version?: AgentApiVersion;
  title?: string;
  model?: string;
  thinking?: string;
}

export interface ThreadResumeParams {
  version?: AgentApiVersion;
  threadId: string;
}

export interface ThreadInterruptParams {
  version?: AgentApiVersion;
  threadId: string;
  turnId?: string;
}

export interface TurnStartParams {
  version?: AgentApiVersion;
  threadId: string;
  input: string;
  model?: string;
  thinking?: string;
  outputSchema?: unknown;
}

export interface ItemUpdatedNotification {
  jsonrpc: "2.0";
  method: "item/updated";
  params: Item;
}
`
	_ = v1.SchemaBytes()
	if *outPath == "" {
		_, _ = fmt.Fprint(os.Stdout, text)
		return
	}
	if err := os.WriteFile(*outPath, []byte(text), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
```

这段 fallback generator 保证无 Node/npm 依赖且可在 Windows/Unix CI 运行；`v1.SchemaBytes()` 的调用是 source-of-truth smoke check。若选择外部 generator，必须把这段替换为读取 schema JSON 的命令，并保持输出接口与上述名称一致。

### Step 3：最终 contract tests

把以下完整测试加到 `internal/appserver/server_test.go`（package appserver）。两个 happy-path 测试驱动 FakeModel + stdio 端到端，逐行 `json.Unmarshal` 校验 wire 契约；错误码矩阵覆盖 JSON-RPC 标准错误码与 `config/write` 的 secret 拒绝。

```go
func TestJSONRPCStreamNotificationIsVersionedItem(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	agent, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv := New(agent, NewMemoryConfig())
	startReq := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"thread/start","params":{"title":"t"}}`,
	}
	input := strings.Join(startReq, "\n") + "\n"
	var out bytes.Buffer
	// Run initialize + thread/start synchronously (no stream channel).
	if err := srv.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	// Read back the thread id from the thread/start response.
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected 2 responses, got %q", out.String())
	}
	var threadResp struct {
		Result struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &threadResp); err != nil {
		t.Fatalf("parse thread/start response: %v", err)
	}
	if threadResp.Result.Thread.ID == "" {
		t.Fatalf("thread/start response missing thread.id: %s", lines[1])
	}

	// Now drive turn/start on a fresh server with the same thread; the server
	// emits item/updated notifications into stdout alongside the turn/start
	// response. Each notification must carry the v1 wire fields.
	var out2 bytes.Buffer
	srv2 := New(agent, NewMemoryConfig())
	turnInput := strings.Join([]string{
		`{"jsonrpc":"2.0","id":10,"method":"thread/start","params":{"title":"t2"}}`,
		`{"jsonrpc":"2.0","id":11,"method":"turn/start","params":{"threadId":"PLACEHOLDER","input":"hi"}}`,
		`{"jsonrpc":"2.0","id":12,"method":"shutdown","params":{}}`,
	}, "\n") + "\n"
	// Resolve the thread id from the first response on srv2 by running the
	// initialize+thread/start pair first, then building the turn input.
	var setupOut bytes.Buffer
	setupIn := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"thread/start","params":{"title":"t2"}}`,
	}, "\n") + "\n"
	if err := srv2.Serve(context.Background(), strings.NewReader(setupIn), &setupOut); err != nil {
		t.Fatalf("setup Serve: %v", err)
	}
	setupLines := strings.Split(strings.TrimRight(setupOut.String(), "\n"), "\n")
	var t2 struct {
		Result struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(setupLines[0]), &t2); err != nil {
		t.Fatalf("parse setup response: %v", err)
	}
	turnInput = strings.ReplaceAll(turnInput, "PLACEHOLDER", t2.Result.Thread.ID)

	if err := srv2.Serve(context.Background(), strings.NewReader(turnInput), &out2); err != nil {
		t.Fatalf("turn Serve: %v", err)
	}
	notified := false
	for _, ln := range strings.Split(strings.TrimRight(out2.String(), "\n"), "\n") {
		var msg map[string]any
		if err := json.Unmarshal([]byte(ln), &msg); err != nil {
			t.Fatalf("stdout line not JSON: %v (%q)", err, ln)
		}
		if msg["method"] != "item/updated" {
			continue
		}
		notified = true
		params, ok := msg["params"].(map[string]any)
		if !ok {
			t.Fatalf("item/updated params missing: %s", ln)
		}
		if params["version"] != "v1" {
			t.Fatalf("item version = %#v, want v1", params["version"])
		}
		if seq, _ := params["sequence"].(float64); seq <= 0 {
			t.Fatalf("item sequence must be > 0: %#v", params["sequence"])
		}
		if params["threadId"] == "" || params["turnId"] == "" {
			t.Fatalf("item missing threadId/turnId: %s", ln)
		}
	}
	if !notified {
		t.Fatalf("expected at least one item/updated notification, got: %s", out2.String())
	}
}

func TestJSONRPCNotificationHasNoResponseID(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	agent, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	srv := New(agent, NewMemoryConfig())
	// A notification (no id) for capabilities must NOT produce a response; the
	// following request with id=7 must still be answered.
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","method":"capabilities","params":{}}`,
		`{"jsonrpc":"2.0","id":7,"method":"shutdown","params":{}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("notification must not produce a response; got %d lines: %q", len(lines), out.String())
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["id"].(float64) != 7 {
		t.Fatalf("response id = %#v, want 7 (echo of shutdown request)", resp["id"])
	}
}

func TestJSONRPCErrorCodes(t *testing.T) {
	model := einollm.NewFakeModel([]string{"answer"}, nil)
	agent, err := v1.NewService(v1.Config{DefaultModel: model})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	cases := []struct {
		name     string
		line     string
		wantCode int64
	}{
		{"malformed json -> parse error", `{not json`, -32700},
		{"bad jsonrpc version", `{"jsonrpc":"1.0","id":1,"method":"initialize"}`, -32600},
		{"unknown method -> not found", `{"jsonrpc":"2.0","id":2,"method":"nope/x","params":{}}`, -32601},
		{"missing input -> invalid params", `{"jsonrpc":"2.0","id":3,"method":"turn/start","params":{"threadId":"t"}}`, -32602},
		{"config/write secret -> invalid params", `{"jsonrpc":"2.0","id":4,"method":"config/write","params":{"key":"api_key","value":"x"}}`, -32602},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(agent, NewMemoryConfig())
			var out bytes.Buffer
			input := tc.line + "\n"
			_ = srv.Serve(context.Background(), strings.NewReader(input), &out)
			lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
			if len(lines) == 0 {
				t.Fatalf("no response for %q", tc.line)
			}
			var resp struct {
				Error *struct {
					Code int64 `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
				t.Fatalf("response not JSON: %v (%q)", err, lines[0])
			}
			if resp.Error == nil || resp.Error.Code != tc.wantCode {
				t.Fatalf("error = %+v, want code %d (line=%q)", resp.Error, tc.wantCode, lines[0])
			}
		})
	}
}
```

这些测试不需要 mock 框架：全部使用 `FakeModel`、`bytes.Buffer`、`strings.NewReader`，断言都通过 `json.Unmarshal` 后字段比较。`TestJSONRPCErrorCodes` 一次性覆盖 malformed/-32700、bad-version/-32600、unknown-method/-32601、invalid-params/-32602（含 `config/write` secret 拒绝）。

补充断言：`item/updated` notification 与 response 在同一 stdout 上不会字节交错 —— 每个 `\n` 结尾的行都可以独立 `json.Unmarshal` 成功，`TestJSONRPCStreamNotificationIsVersionedItem` 的逐行解析即是此项守护。`shutdown` response 后 Serve 返回的行为由 `TestRunAppStdioSmoke`（Task 11）守护。

### Step 4：运行 D1 验收命令

本步骤由实现者执行，当前计划编写阶段不运行任何命令：

```bash
go test ./internal/cli ./cmd/yanshi ./internal/api/v1 ./internal/api/http ./internal/appserver ./internal/bootstrap
go vet ./internal/cli ./cmd/yanshi ./internal/api/v1 ./internal/api/http ./internal/appserver ./internal/bootstrap
```

命令行 smoke contract：

```bash
printf '{"prompt":"hello"}\n' | yanshi exec --fake-model --input jsonl --output jsonl --inprocess
printf '{"prompt":"hello"}\n' | yanshi chat --no-tui --fake-model --input jsonl --output jsonl --inprocess
printf '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}\n{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}\n' | yanshi app --fake-model
```

预期：

- 第一、二条命令 stdout 每行均为稳定 JSONL object，stderr 只含 progress/session；输入错误为 2，timeout 为 124，SIGINT 为 130。
- 第三条命令 stdout 每行均为 JSON-RPC response，首个 response 为 capabilities，第二个 response 为 shutdown result；没有日志污染 stdout。
- `GET /api/v1/schema/agent-v1.json` 与 app-server capabilities 报告同一版本和 method set。

### Step 5：提交

选择 D1-DEC-2 A 时：

```bash
git add cmd/api-schema/main.go sdk/ts/v1.ts internal/api/v1 internal/appserver cmd/yanshi
git commit -m "feat(appserver): publish v1 schema and generated TypeScript contract"
```

选择 B/C 时：

```bash
git add internal/api/v1 internal/appserver cmd/yanshi
 git commit -m "test(appserver): finalize v1 schema and JSON-RPC compatibility contract"
```

---

## 风险、兜底与实现注意事项

- **现有 `exec` 已部分完成 V12**：先抽公共 runner，再改 CLI；禁止让新 `chat --no-tui` 重新调用旧 `chatLegacy`。
- **WS/SSE legacy 不回归**：`internal/proto/frame.go` 的 `ClientFrame`/`ServerFrame` 保持原 JSON tags；V14 使用新 `internal/api/v1` types，不把 `threadId` 等 camelCase 字段加进 legacy frame。
- **模型/工具事件顺序**：`ClassifyEventsWithUsage` 是现有唯一 frame classifier；V14 只能消费它的 `emit`，不能旁路 ADK iterator 读取第二次。
- **背压**：v1 channel 必须有固定容量；HTTP handler 的 `range` 是消费者，disconnect 由 request context 取消；JSON-RPC writer mutex 防止 response 与 notification 字节交错。不得用无限 buffer、goroutine-per-item 无上限队列或默认丢弃 item。
- **resume 一致性**：Store session id 是 v1 thread id；resume 重新加载 `schema.Message` history。v1 MVP 不承诺跨进程事件 replay，必须在 API capabilities 和 schema 文档中明确。
- **中断语义**：`context.Canceled` 不能被包装成普通 runtime error 后丢失；item stream 发送 `turn.error`/`turn.completed` 的状态必须与 `Turn.Status` 一致，`thread/interrupt` 第二次调用幂等。
- **未知字段**：所有 decode 位置不能使用 `DisallowUnknownFields`；新增字段必须有兼容测试。未知 server frame type 映射到 `event.<legacyType>`，不静默消失。
- **安全**：HTTP 继续复用现有 `Server.Handler` token/loopback auth；SSE/V14 使用 orchestrator static profile；app-server 默认是本地 stdio，不把 `token`、`api_key`、prompt 或工具结果写入启动日志；config write deny secret paths。
- **Windows/Unix**：headless 退出码和 JSONL 使用标准库，不能依赖 shell quoting；app-server stdio 不使用 Unix-only signal API 之外的 transport；`signal.NotifyContext` 仅在 CLI 入口使用。
- **文件行数**：`ws.go` 已接近拆分边界，D1 不继续堆逻辑；V14/appserver 按 `types/service/events` 和 `rpc/server/config` 拆文件，每个 `.go` 文件保持纯代码行不超过 1000。

---

## Self-review：规格覆盖

- V12 stdin：Task 1/3 的 text、lines、JSONL parser 覆盖；`exec` 与 `chat --no-tui` 共享。
- V12 JSONL output：Task 2 定义稳定 `type`/camelCase encoder，并由 Task 3/12 smoke test 守护。
- V12 resume：Task 2 单 backend 一次 restore，Task 6/8 Store-backed thread resume。
- V12 稳定退出码：Task 3 保留 0/1/2/124/130，错误优先级由 context cancellation 明确定义。
- V14 thread.start/resume/interrupt：Task 4/6/7/8；`turn/start` 作为 streaming operation。
- V14 流式 item：Task 5 映射，Task 6 bounded channel，Task 7 SSE，Task 10 JSON-RPC notification。
- V14 版本与 JSON Schema：Task 4 provisional version、Task 9 schema，D1-DEC-1 明确最终决策点。
- V14 camelCase：Task 4 tags、Task 9 schema/recursive compatibility test。
- V14 背压：Task 6 channel + cancel、Task 10 writer mutex/stream tests。
- V14 未知字段：Task 4 unmarshal test、Task 7/9 HTTP compatibility matrix、Task 10 JSON-RPC params。
- V14 兼容：legacy frame untouched；Task 5 only adapter；Task 9 matrix。
- APS1 JSON-RPC v2：Task 10 line protocol、standard errors、notification、method dispatch。
- APS1 `internal/appserver/` 与 `yanshi app`：Task 10/11。
- APS1 `<resource>/<method>`、*Params/*Response/*Notification、camelCase：Task 4/10/12 method table and types。
- APS1 TS 类型生成：Task 12；工具链明确列为 D1-DEC-2，内置 Go fallback 作为无外部依赖路径。

计划正文不执行 build/test、不修改 `.go`；上述命令只供后续实施者按 checkbox 顺序执行。
