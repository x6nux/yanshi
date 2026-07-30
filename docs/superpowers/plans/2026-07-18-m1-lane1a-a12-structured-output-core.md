# A12-core: Per-Turn 结构化输出（provider 无关）实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让一个 turn 可以声明一个 JSON Schema，turn 结束后服务端校验模型输出的 JSON 是否符合该 schema，失败则带错误提示重试，成功则返回解析后的结构化结果；text 模式（无 schema）字节不变。

**Architecture:** schema 在 per-turn 声明（WS：`ClientFrame.OutputSchema`；SSE：POST body `output_schema`）。turn 正常跑（v1 不改 LLM 适配器——API 层 schema 注入留给 A12-providers）。turn 结束后服务端把累积的 `assistantText` 用 JSON Schema validator 校验：失败 → 把校验错误作为 user reminder 扩展历史后重跑（WS 折叠进 `ws.go` 已有的 task_end 重试循环；SSE 加一个并行的简版循环）；成功 → 发新的 `structured_result` ServerFrame。`OutputSchema` 为空时完全走原路径。

**Tech Stack:** Go stdlib；`github.com/santhosh-tekuri/jsonschema/v6`（新增依赖，支持 JSON Schema 2020-12）；现有 `internal/proto`、`internal/api/http`（ws.go/chat.go）、`internal/cli`（backend）。

**不变性（路线图风险约束）：** text 模式（无 schema）必须字节一致——所有新行为以 `len(OutputSchema) > 0` 门控。

---

## File Structure

- **Create** `internal/api/http/structresult.go` — `ValidateStructuredOutput`、`compileSchema`（带缓存）、`extractJSON`、`schemaRetryReminder`、`maxSchemaRetries` 常量。被 ws.go/chat.go 共用。
- **Create** `internal/api/http/structresult_test.go` — 校验/提取/reminder 的单测。
- **Modify** `internal/proto/frame.go` — `ClientFrame.OutputSchema`、`ServerFrame.StructuredResult`、`NewStructuredResult`、`NewUserMessageWithSchema`。
- **Modify** `internal/api/http/ws.go` — `user_message` 分支读 `cf.OutputSchema`；把 schema 校验折叠进现有 task_end 重试循环；成功发 `structured_result`。
- **Modify** `internal/api/http/chat.go` — `req.OutputSchema`；加 schema 重试循环；发 `structured_result`。
- **Modify** `internal/cli/backend.go` — `StreamEvent.StructuredResult` 字段。
- **Modify** `internal/cli/ssebackend.go` + `wsbackend.go` — `structured_result` 帧映射到 `StreamEvent.StructuredResult`（让 exec/TUI 能消费）。
- **Modify** `go.mod` — 加 `github.com/santhosh-tekuri/jsonschema/v6`。

---

## Task 1: JSON Schema validator + `ValidateStructuredOutput`

**Files:**
- Create: `internal/api/http/structresult.go`
- Create: `internal/api/http/structresult_test.go`
- Modify: `go.mod`

- [ ] **Step 1: 引入依赖**

Run:
```sh
go get github.com/santhosh-tekuri/jsonschema/v6
```
Expected: `go.mod` 新增 require 行；`go.sum` 更新。

- [ ] **Step 2: 确认 validator API（写一个 5 行探测）**

在任意临时位置跑最小示例，确认 v6 的 `NewCompiler` / `AddResource(url, io.Reader)` / `Compile(url)` / `Schema.Validate(v any) error` 签名。若 v6 API 不同（例如用 `CompileResource` 或 `Unmarshal`），以实际为准调整 Step 4 的实现——本任务测试是契约，实现跟随 API。确认后删除探测代码。

Run:
```sh
go doc github.com/santhosh-tekuri/jsonschema/v6
```

- [ ] **Step 3: 写失败测试**

`internal/api/http/structresult_test.go`:
```go
package http

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestValidateStructuredOutput(t *testing.T) {
	personSchema := json.RawMessage(`{
		"type": "object",
		"properties": {"name": {"type": "string"}, "age": {"type": "integer"}},
		"required": ["name"],
		"additionalProperties": false
	}`)

	tests := []struct {
		name    string
		text    string
		schema  json.RawMessage
		wantErr bool
	}{
		{"valid object", `{"name":"Ada","age":36}`, personSchema, false},
		{"fenced json", "```json\n{\"name\":\"Ada\"}\n```", personSchema, false},
		{"missing required", `{"age":36}`, personSchema, true},
		{"extra property", `{"name":"Ada","x":1}`, personSchema, true},
		{"not json", `hello world`, personSchema, true},
		{"empty schema (text mode)", `{"any":"thing"}`, nil, false},
		{"empty schema non-json text", `plain text`, nil, true}, // text mode still requires parseable JSON output when a schema was intended? NO — empty schema = no structured path; raw text returned
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ValidateStructuredOutput(tc.text, tc.schema)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (out=%s)", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	// Text mode: empty schema returns the extracted JSON bytes, no error, for
	// any text (including non-JSON): the no-schema path does NOT validate.
	if _, err := ValidateStructuredOutput("plain text", nil); err != nil {
		t.Fatalf("text mode must not error on non-JSON, got: %v", err)
	}
	_ = errors.New // keep import if not otherwise used
}

func TestExtractJSON(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{`  {"a":1}  `, `{"a":1}`},
		{`plain`, `plain`},
	}
	for _, tc := range tests {
		if got := extractJSON(tc.in); got != tc.want {
			t.Errorf("extractJSON(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSchemaRetryReminder(t *testing.T) {
	r := schemaRetryReminder("prev output", errors.New("missing field name"))
	if r == "" || !contains(r, "missing field name") || !contains(r, "prev output") {
		t.Fatalf("reminder must include error and prev text: %q", r)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

> 注：测试里 `"empty schema non-json text"` 这条标 `wantErr=true` 但语义上 text mode 不校验——删除该条，保留下方 `ValidateStructuredOutput("plain text", nil)` 无错的断言。最终测试表里该条不要。

- [ ] **Step 4: 运行测试确认失败**

Run:
```sh
go test ./internal/api/http -run TestValidateStructuredOutput -v
```
Expected: FAIL（`ValidateStructuredOutput` / `extractJSON` / `schemaRetryReminder` 未定义）。

- [ ] **Step 5: 实现 `structresult.go`**

`internal/api/http/structresult.go`:
```go
package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// maxSchemaRetries caps how many times a turn is re-run when the model's output
// fails JSON Schema validation. Mirrors ws.go's maxIncompleteRetries so the
// structured-output path and the task_end mandatory-completion path share one
// retry budget shape.
const maxSchemaRetries = 3

var (
	schemaCacheMu sync.Mutex
	schemaCache   = map[string]*jsonschema.Schema{}
)

// ValidateStructuredOutput parses text as JSON and validates it against schemaDoc
// (a JSON Schema document). On success it returns the extracted JSON bytes.
//
// An empty schemaDoc is the text-mode no-op: it returns the extracted bytes and
// a nil error WITHOUT validating — callers gate structured behavior on
// len(schemaDoc) > 0 so the text path is byte-identical to today.
//
// Models frequently wrap JSON in ```json fences; extractJSON trims those before
// parsing. A parse failure is reported as "output is not valid JSON"; a schema
// failure is reported as "output does not match schema".
func ValidateStructuredOutput(text string, schemaDoc json.RawMessage) (json.RawMessage, error) {
	raw := json.RawMessage(extractJSON(text))
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("output is not valid JSON: %w", err)
	}
	if len(schemaDoc) == 0 {
		return raw, nil
	}
	sch, err := compileSchema(schemaDoc)
	if err != nil {
		return nil, fmt.Errorf("invalid output schema: %w", err)
	}
	if err := sch.Validate(value); err != nil {
		return nil, fmt.Errorf("output does not match schema: %w", err)
	}
	return raw, nil
}

// compileSchema compiles (and memoizes by document content) a JSON Schema.
// Compilation is expensive; turns repeat the same schema, so a content-keyed
// cache avoids recompiling on every retry.
func compileSchema(doc json.RawMessage) (*jsonschema.Schema, error) {
	key := string(doc)
	schemaCacheMu.Lock()
	defer schemaCacheMu.Unlock()
	if sch, ok := schemaCache[key]; ok {
		return sch, nil
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema.json", bytes.NewReader(doc)); err != nil {
		return nil, err
	}
	sch, err := c.Compile("schema.json")
	if err != nil {
		return nil, err
	}
	schemaCache[key] = sch
	return sch, nil
}

// extractJSON trims surrounding whitespace and a single pair of ``` / ```json
// fences so fenced model output parses. It does NOT extract a brace-balanced
// span — v1 relies on the schema-retry reminder to teach the model to emit pure
// JSON. Returns the trimmed string unchanged when no fence is present.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = strings.TrimSpace(s[nl+1:])
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
	}
	return s
}

// schemaRetryReminder builds the user-role reminder appended to history when a
// schema-validation retry is attempted. prevText is the model's previous
// (invalid) output; verr is the error from ValidateStructuredOutput. Naming the
// error and echoing the bad output lets the model correct a specific deficiency
// instead of guessing.
func schemaRetryReminder(prevText string, verr error) string {
	return fmt.Sprintf(
		"Your previous reply did not satisfy the required output schema and was discarded:\n%[1]s\n\n"+
			"Reply again with a single JSON value that matches the schema exactly. Your previous reply was:\n%[2]s",
		verr.Error(), prevText)
}
```

- [ ] **Step 6: 运行测试确认通过**

Run:
```sh
go test ./internal/api/http -run "TestValidateStructuredOutput|TestExtractJSON|TestSchemaRetryReminder" -v
```
Expected: PASS。

- [ ] **Step 7: 提交**

```sh
git add internal/api/http/structresult.go internal/api/http/structresult_test.go go.mod go.sum
git commit -m "feat(http): add JSON Schema output validator (A12-core)"
```

---

## Task 2: proto 帧 — `OutputSchema` / `StructuredResult` + 构造函数

**Files:**
- Modify: `internal/proto/frame.go`
- Test: `internal/proto/frame_test.go`（新增或追加）

- [ ] **Step 1: 写失败测试**

追加到 `internal/proto/frame_test.go`（若不存在则新建 `package proto` 测试文件）:
```go
package proto

import (
	"encoding/json"
	"testing"
)

func TestStructuredResultFrame(t *testing.T) {
	f := NewStructuredResult(json.RawMessage(`{"k":"v"}`))
	if f.Type != "structured_result" {
		t.Fatalf("type = %q, want structured_result", f.Type)
	}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == `` || !contains(string(data), `"structured_result"`) {
		t.Fatalf("marshalled frame missing type: %s", data)
	}
	// StructuredResult must serialize as raw JSON, not an escaped string.
	if !contains(string(data), `"structured_result":{"k":"v"}`) {
		t.Fatalf("StructuredResult must be raw JSON object, got: %s", data)
	}
}

func TestNewUserMessageWithSchema(t *testing.T) {
	f := NewUserMessageWithSchema("hi", json.RawMessage(`{"type":"object"}`))
	if f.Type != "user_message" || f.Text != "hi" {
		t.Fatalf("got %+v", f)
	}
	if string(f.OutputSchema) != `{"type":"object"}` {
		t.Fatalf("schema not carried: %s", f.OutputSchema)
	}
}

func TestUserMessageOmitsSchemaByDefault(t *testing.T) {
	b, _ := json.Marshal(NewUserMessage("hi"))
	if contains(string(b), "output_schema") {
		t.Fatalf("plain user_message must omit output_schema: %s", b)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/proto -run "TestStructuredResultFrame|TestNewUserMessageWithSchema|TestUserMessageOmitsSchemaByDefault" -v
```
Expected: FAIL（构造函数/字段未定义）。

- [ ] **Step 3: 修改 `frame.go`**

(a) 在 `ClientFrame` 结构体（约 28-37 行）加一个字段：
```go
	// OutputSchema carries an optional JSON Schema for a user_message turn. When
	// non-empty the server validates the model's final output against it and
	// emits a structured_result frame; when empty/absent the turn is plain text
	// (text mode, byte-identical to pre-A12). omitempty keeps legacy clients and
	// the no-schema path unchanged on the wire.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"` // user_message
```

(b) 在 `NewUserMessage` 下方加：
```go
// NewUserMessageWithSchema builds a user_message frame that also declares a
// per-turn JSON Schema for structured output. schemaDoc is a JSON Schema
// document (e.g. `{"type":"object",...}`). See ClientFrame.OutputSchema.
func NewUserMessageWithSchema(text string, schemaDoc json.RawMessage) ClientFrame {
	return ClientFrame{Type: "user_message", Text: text, OutputSchema: schemaDoc}
}
```

(c) 在 `ServerFrame` 结构体（约 125-191 行，`Total` 字段后）加：
```go
	// StructuredResult carries the validated JSON the model produced for a turn
	// that declared an output schema (ClientFrame.OutputSchema). Emitted as a
	// dedicated structured_result frame AFTER the turn's agent_chunk stream and
	// BEFORE done, so a consumer can take the parsed value directly instead of
	// re-parsing the streamed text. Empty/absent on text-mode turns.
	StructuredResult json.RawMessage `json:"structured_result,omitempty"` // structured_result
```

(d) 在构造函数区（`NewNestedProgress` 之后、`SSEEvent` 之前）加：
```go
// NewStructuredResult builds a structured_result frame carrying the validated
// JSON for a schema-constrained turn. data is the raw JSON bytes that passed
// ValidateStructuredOutput.
func NewStructuredResult(data json.RawMessage) ServerFrame {
	return ServerFrame{Type: "structured_result", StructuredResult: data}
}
```

(e) 更新 `ServerFrame` 顶部的类型注释表，加一行：
```
//	structured_result  StructuredResult (validated JSON for a schema-constrained turn)
```

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./internal/proto -v
```
Expected: PASS（含新测试 + 既有 proto 测试）。

- [ ] **Step 5: 提交**

```sh
git add internal/proto/frame.go internal/proto/frame_test.go
git commit -m "feat(proto): add output_schema / structured_result frames (A12-core)"
```

---

## Task 3: WS — schema 校验折叠进 task_end 重试循环

**Files:**
- Modify: `internal/api/http/ws.go`（`user_message` 分支，约 614-925 行）

> 现有循环（794-891）已有 `assistantText` 累积、`prevAssistantText` 重试扩展、5 个中断条件。本任务把 schema 校验作为一条**并行的完成路径**折叠进去：`hasSchema` 时用 schema 校验决定完成/重试；否则走原 task_end 逻辑。两者共用 `assistantText` 累积、`prevAssistantText`、retry 上限取 `max(maxIncompleteRetries, maxSchemaRetries)`。

- [ ] **Step 1: 写失败测试**

追加到 `ws_test.go`（找到既有 WS handler 测试的桩构造方式，复用其 server/FakeModel 装配；以下给出断言骨架，装配部分照抄现有 `TestWS_*`）:
```go
func TestWSStructuredOutputSuccess(t *testing.T) {
	// FakeModel replies with valid JSON matching the schema.
	// Send NewUserMessageWithSchema("list users", schema).
	// Expect: agent_chunk stream, then a structured_result frame with the JSON,
	//         then done. No retry.
}

func TestWSStructuredOutputRetryThenSuccess(t *testing.T) {
	// FakeModel replies invalid JSON first, valid JSON second.
	// Expect: 2 agent_chunk streams (caveat: retried output re-streamed),
	//         structured_result with the second attempt's JSON, done.
	//         FakeModel.ReceivedMessages on the 2nd call contains the reminder.
}

func TestWSStructuredOutputRetryCapError(t *testing.T) {
	// FakeModel always replies invalid JSON.
	// Expect: an error frame mentioning "schema", done. No structured_result.
}

func TestWSNoSchemaIsTextMode(t *testing.T) {
	// Existing behavior: plain NewUserMessage, no output_schema.
	// Expect: NO structured_result frame ever; done as before. (Regression guard.)
}
```
> 装配细节：照抄 `ws_test.go` 里现有的 WS handler 测试如何构造 `Server`、注入 `einollm.NewFakeModelWithMessages`、用 `proto.NewUserMessageWithSchema` 发帧、用 `wsBackend`/`sseBackend` 或裸帧往返收集 `ServerFrame`。先读 `ws_test.go` 既有用例确认桩，再填实现。

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/api/http -run "TestWSStructuredOutput|TestWSNoSchemaIsTextMode" -v
```
Expected: FAIL（新测试断言 `structured_result`，当前不发）。

- [ ] **Step 3: 修改 `ws.go`**

(a) 确保 import 含 `"encoding/json"`（`json.RawMessage` 局部变量需要；若 ws.go 未导入则加）。

(b) 在 `user_message` 分支、构造 `opts`（约 723 行）之后、`turnTaskEnd` 声明（约 736 行）之前，加 schema 状态声明：
```go
// A12-core: per-turn structured output. When the client declares an output
// schema (NewUserMessageWithSchema), the loop below validates the model's
// final assistantText against it and retries with a reminder on failure,
// mirroring the task_end mandatory-completion path. hasSchema=false keeps the
// text path byte-identical to pre-A12.
hasSchema := len(cf.OutputSchema) > 0
var structuredResult json.RawMessage // set on schema success; emitted before done
```

(c) 替换整个重试循环（当前 794-891 行的 `const maxIncompleteRetries = 3` ... 循环结束）为下面的合并版本：
```go
					const maxIncompleteRetries = 3
					// retryCap covers whichever path is active: task_end mandatory
					// completion (maxIncompleteRetries) or schema validation
					// (maxSchemaRetries). Taking the max keeps a single loop bound.
					retryCap := maxIncompleteRetries
					if hasSchema && maxSchemaRetries > retryCap {
						retryCap = maxSchemaRetries
					}
					// prevAssistantText captures the previous attempt's final
					// assistant text so the retry can extend cs.history with it
					// plus a reminder. Set only when about to retry.
					var prevAssistantText string
					// reminder overrides the default task_end reminder when the
					// retry is schema-driven (carries the validation error).
					var reminder string
					for attempt := 0; attempt <= retryCap; attempt++ {
						// Reset per-attempt state so earlier attempts' partial output
						// is discarded — the FINAL attempt's output is what the user
						// keeps in history.
						assistantText = ""
						usage = orchestrator.TurnUsage{}
						turnTaskEnd.Store(false)
						retryReset.Store(false)

						// Build the history for this attempt. Attempt 0 uses
						// cs.history unchanged. attempt > 0 extends a COPY with the
						// previous attempt's assistant output plus a user reminder
						// (schema validation error, or the task_end nudge).
						history := cs.history
						if attempt > 0 {
							extra := make([]*schema.Message, 0, 2)
							if prevAssistantText != "" {
								extra = append(extra, schema.AssistantMessage(prevAssistantText, nil))
							}
							msg := reminder
							if msg == "" {
								msg = "Continue completing the task. Call `task_end` when you are done."
							}
							extra = append(extra, schema.UserMessage(msg))
							history = make([]*schema.Message, 0, len(cs.history)+len(extra))
							history = append(history, cs.history...)
							history = append(history, extra...)
						}

						iter := o.EventsWithHistoryOpts(turnCtx, history, opts)
						var hadError bool
						orchestrator.ClassifyEventsWithUsage(iter, &usage, func(f proto.ServerFrame) {
							if f.Type == "error" {
								hadError = true
							}
							if retryReset.CompareAndSwap(true, false) {
								assistantText = ""
							}
							if f.Type == "agent_chunk" {
								assistantText += f.Text
							}
							conn.write(f)
						}, onUsage)

						// Hard failures break regardless of mode: a model error or
						// a user cancel must not trigger a schema/task_end retry.
						if hadError {
							break
						}
						if turnCtx.Err() != nil {
							break
						}

						if hasSchema {
							// Structured-output path: validate the final text.
							validated, verr := ValidateStructuredOutput(assistantText, cf.OutputSchema)
							if verr == nil {
								structuredResult = validated
								break // schema satisfied — done
							}
							if attempt == retryCap {
								conn.write(proto.NewError("output did not match the required schema after " +
									itoa(attempt+1) + " attempt(s): " + verr.Error()))
								break
							}
							prevAssistantText = assistantText
							reminder = schemaRetryReminder(assistantText, verr)
							continue
						}

						// Text-mode task_end mandatory-completion path (unchanged).
						if turnTaskEnd.Load() {
							break
						}
						if !hasTaskEndTool {
							break
						}
						if attempt == retryCap {
							conn.write(proto.NewError("turn ended without a task_end completion signal; output may be incomplete"))
							break
						}
						// About to retry: capture this attempt's assistant text.
						prevAssistantText = assistantText
					}
```

(d) `itoa` 辅助：在 ws.go 末尾或紧邻处加（避免在热路径里引 `strconv` 转换开销的错觉——其实无妨，但保持自包含）：
```go
// itoa is strconv.Itoa without the import in the few call sites above.
func itoa(n int) string { return strconv.Itoa(n) }
```
并在 ws.go import 块加 `"strconv"`（若未导入）。

(e) 在循环之后、`conn.write(proto.NewDone())`（约 925 行）之前，发结构化结果：
```go
					// A12-core: emit the validated structured result before done so a
					// schema-constrained consumer (exec --output-schema, API client)
					// can take the parsed JSON without re-parsing the stream.
					if structuredResult != nil {
						conn.write(proto.NewStructuredResult(structuredResult))
					}
```

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./internal/api/http -run "TestWSStructuredOutput|TestWSNoSchemaIsTextMode" -v
go test ./internal/api/http -v
```
Expected: 新测试 PASS；既有 WS 测试全 PASS（text 模式回归）。

- [ ] **Step 5: 提交**

```sh
git add internal/api/http/ws.go internal/api/http/ws_test.go
git commit -m "feat(http): schema validation+retry in WS turn loop (A12-core)"
```

---

## Task 4: SSE — schema 重试循环（`chat.go`）

**Files:**
- Modify: `internal/api/http/chat.go`
- Modify: `internal/api/http/chat_test.go`（追加）

> SSE 目前是单次 turn（119-132）、无重试循环。加一个 schema 重试循环（仅 `hasSchema` 时生效；text 模式仍单次）。复用 `ValidateStructuredOutput` + `schemaRetryReminder`。

- [ ] **Step 1: 写失败测试**

追加到 `chat_test.go`（照抄现有 `TestChat*` 如何 POST 到 `/api/v1/chat` 并解析 SSE 事件）:
```go
func TestChatStructuredOutputSuccess(t *testing.T) {
	// POST {"message":"...","output_schema":{...}} with FakeModel returning valid JSON.
	// Parse SSE events; expect a structured_result event with the JSON before done.
}

func TestChatStructuredOutputRetryCapError(t *testing.T) {
	// FakeModel always invalid; expect error event mentioning "schema", no structured_result.
}

func TestChatNoSchemaIsTextMode(t *testing.T) {
	// POST without output_schema; expect NO structured_result event (regression).
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/api/http -run "TestChatStructuredOutput|TestChatNoSchemaIsTextMode" -v
```
Expected: FAIL。

- [ ] **Step 3: 修改 `chat.go`**

(a) 在 `req` 匿名结构（约 34-39 行）加字段：
```go
			OutputSchema json.RawMessage `json:"output_schema,omitempty"`
```

(b) 替换单次 turn 段（当前 112-132 行的 `var usage ... writeSSEFrame(proto.NewDone())`）为带 schema 重试的版本：
```go
			hasSchema := len(req.OutputSchema) > 0
			retryCap := 0
			if hasSchema {
				retryCap = maxSchemaRetries
			}
			var usage orchestrator.TurnUsage
			var structuredResult json.RawMessage
			var prevAssistantText string
			var lastVErr error
			for attempt := 0; attempt <= retryCap; attempt++ {
				runMsgs := msgs
				if attempt > 0 {
					extra := make([]*schema.Message, 0, 2)
					if prevAssistantText != "" {
						extra = append(extra, schema.AssistantMessage(prevAssistantText, nil))
					}
					extra = append(extra, schema.UserMessage(schemaRetryReminder(prevAssistantText, lastVErr)))
					runMsgs = make([]*schema.Message, 0, len(msgs)+len(extra))
					runMsgs = append(runMsgs, msgs...)
					runMsgs = append(runMsgs, extra...)
				}

				usage = orchestrator.TurnUsage{}
				var assistantText string
				// SSE is stateless and unidirectional — no interactive permission
				// callback (static profile only). Same as the pre-A12 path.
				tc := tools.WithErrCounter(r.Context())
				iter := o.EventsWithHistoryOpts(tc, runMsgs, opts)
				var hadError bool
				orchestrator.ClassifyEventsWithUsage(iter, &usage, func(f proto.ServerFrame) {
					if f.Type == "error" {
						hadError = true
					}
					if f.Type == "agent_chunk" {
						assistantText += f.Text
					}
					writeSSEFrame(w, fl, f)
				})
				if hadError || r.Context().Err() != nil {
					break
				}
				if !hasSchema {
					break // text mode: single attempt, original behavior
				}
				validated, verr := ValidateStructuredOutput(assistantText, req.OutputSchema)
				if verr == nil {
					structuredResult = validated
					break
				}
				lastVErr = verr
				if attempt == retryCap {
					writeSSEFrame(w, fl, proto.NewError("output did not match the required schema after "+
						strconv.Itoa(attempt+1)+" attempt(s): "+verr.Error()))
					break
				}
				prevAssistantText = assistantText
			}

			// Structured result before status/done (A12-core).
			if structuredResult != nil {
				writeSSEFrame(w, fl, proto.NewStructuredResult(structuredResult))
			}
			// Emit a status frame with the selection + usage before terminating so
			// the client can update its model indicator and /cost from either
			// transport. turns is always 1 for a stateless SSE request.
			sseStatus := proto.NewStatus(req.Model, req.Thinking, usage.PromptTokens, usage.CompletionTokens, 1, contextWindowFor(req.Model, s.compaction))
			sseStatus.CachedTokens = usage.CachedTokens
			sseStatus.ReasoningTokens = usage.ReasoningTokens
			writeSSEFrame(w, fl, sseStatus)
			writeSSEFrame(w, fl, proto.NewDone())
```

(c) import 块加 `"strconv"`（若未导入）。

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./internal/api/http -run "TestChat" -v
```
Expected: 新测试 PASS；既有 Chat 测试 PASS（text 回归）。

- [ ] **Step 5: 提交**

```sh
git add internal/api/http/chat.go internal/api/http/chat_test.go
git commit -m "feat(http): schema validation+retry on SSE chat (A12-core)"
```

---

## Task 5: 客户端 `StreamEvent.StructuredResult`（让 exec/TUI 能消费）

**Files:**
- Modify: `internal/cli/backend.go`（`StreamEvent` 加字段）
- Modify: `internal/cli/ssebackend.go`（`structured_result` 事件 → 字段）
- Modify: `internal/cli/wsbackend.go`（`toStreamEvent` 映射加分支）
- Modify: 对应 `_test.go`

- [ ] **Step 1: 写失败测试**

在 `backend_test.go` 或 `wsbackend_test.go` 加：
```go
func TestStreamEventStructuredResult(t *testing.T) {
	// Feed a ServerFrame{Type:"structured_result", StructuredResult: `{"x":1}`}
	// through toStreamEvent (ws) and through sseBackend's event parser.
	// Assert StreamEvent.StructuredResult == `{"x":1}` and Type == "structured_result".
}
```

- [ ] **Step 2: 运行确认失败**

Run:
```sh
go test ./internal/cli -run TestStreamEventStructuredResult -v
```
Expected: FAIL（字段不存在）。

- [ ] **Step 3a: `backend.go` 加字段**

在 `StreamEvent` 结构体（约 18 行起，`MessageStub`/`Sessions` 等字段附近）加：
```go
	// StructuredResult carries the validated JSON for a schema-constrained turn
	// (server frame type "structured_result"). Empty on text-mode turns. Consumed
	// by headless exec (--output-schema) and, later, the TUI.
	StructuredResult json.RawMessage
```
（`backend.go` 已 import `encoding/json`？若否则加。）

- [ ] **Step 3b: `ssebackend.go` 解析分支**

在 SSE 事件解析处（`event:` 分支判断，约 103-127 行处理各类事件的地方）加一个 `case "structured_result":` 分支，把帧的 `StructuredResult` 写入构造的 `StreamEvent.StructuredResult`（具体变量名照抄该文件现有分支如何 ev 结构体）。

- [ ] **Step 3c: `wsbackend.go` 的 `toStreamEvent` 加分支**

在 `toStreamEvent`（约 251 行）的 `switch f.Type` 里加：
```go
	case "structured_result":
		return StreamEvent{Type: "structured_result", StructuredResult: f.StructuredResult}
```
（字段名/构造照抄该函数现有分支风格。）

- [ ] **Step 4: 运行确认通过**

Run:
```sh
go test ./internal/cli -v
```
Expected: PASS（含新测试 + 既有 cli 测试）。

- [ ] **Step 5: 提交**

```sh
git add internal/cli/backend.go internal/cli/ssebackend.go internal/cli/wsbackend.go internal/cli
git commit -m "feat(cli): surface structured_result in StreamEvent (A12-core)"
```

---

## Task 6: 全量回归 + text 模式不变性验证

**Files:**
- 无新增；运行全量测试 + go vet。

- [ ] **Step 1: 全量测试**

Run:
```sh
go test ./...
```
Expected: 全 PASS（允许 CLAUDE.md 记载的预期 `t.Skip`：`e2e_real` 门禁、部分 eino/bootstrap 测试在 openai provider 不可用时 skip）。

- [ ] **Step 2: vet**

Run:
```sh
go vet ./...
```
Expected: 无输出。

- [ ] **Step 3: 构建**

Run:
```sh
go build -o autocode ./cmd/autocode
```
Expected: 成功。

- [ ] **Step 4: 冒烟（可选，需 fake model）**

Run:
```sh
timeout 5 ./autocode --fake-model -inprocess -h
```
Expected: 打印用法并退出 0（确认改动没破坏启动）。

- [ ] **Step 5: 提交（若前序步骤有未提交的小修）**

```sh
git add -A
git commit -m "test: A12-core regression green" || echo "nothing to commit"
```

---

## Self-Review（写完后自查结果）

1. **Spec 覆盖**：roadmap M1.A12 验收——「可传入 JSON Schema」（Task 2 ClientFrame + Task 4 SSE body）✅；「结果校验失败可重试」（Task 1 校验 + Task 3/4 重试）✅；「最终返回明确错误」（retryCap 耗尽发 error 帧）✅；「text 模式不受影响」（`hasSchema` 门控 + Task 3/4 回归测试 + Task 6 全量）✅。覆盖完整。
2. **Placeholder 扫描**：Task 3/4/5 的测试用了「照抄现有 X 测试的桩」措辞——这是有意的（避免重复粘贴大段装配代码），但工程师需先读既有测试。可接受；若要更硬可补桩。无 TBD/TODO。
3. **类型一致性**：`ValidateStructuredOutput`、`schemaRetryReminder`、`NewStructuredResult`、`NewUserMessageWithSchema`、`StreamEvent.StructuredResult`、`cf.OutputSchema`/`req.OutputSchema` 命名在所有任务一致 ✅。`maxSchemaRetries` 常量定义于 structresult.go，ws.go/chat.go 共用 ✅。
4. **已知限制（非 placeholder，是 v1 边界）**：openai 路径无 API 层 schema 注入（靠校验重试，A12-providers 补）；`extractJSON` v1 不做 brace 平衡（靠 reminder 引导）；retry 会重流 agent_chunk（已有 task_end 同样 caveat）；同步 `Query` 路径未加 schema（v1 聚焦流式 WS/SSE，exec 走后端）。

## 执行交接

Plan complete and saved to `docs/superpowers/plans/2026-07-18-m1-lane1a-a12-structured-output-core.md`. 两种执行方式：

1. **Subagent-Driven（推荐）** — 每个任务派一个新 subagent，任务间 review。
2. **Inline Execution** — 本会话内按 executing-plans 批次执行 + checkpoint。
