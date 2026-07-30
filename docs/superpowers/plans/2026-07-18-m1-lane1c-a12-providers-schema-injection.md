# A12-providers: 把 per-turn output schema 注入 anthropic / responses 适配器请求体 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让一个声明了 JSON Schema 的 turn，把该 schema 作为 provider 原生的结构化输出参数注入到 **anthropic** 与 **responses** 两个自定义 HTTP 适配器的 API 请求体里，从而提高一次成功率；openai（chat completions，eino-ext）路径 v1 不注入，靠 A12-core 的校验重试兜底；text 模式（无 schema）请求体字节不变。

**Architecture:** 新增一个**为 `internal/llm/eino` 包所拥有**的 per-call `model.Option`（`einollm.OutputSchemaOption`），承载 schema。orchestrator 的 `TurnOpts.OutputSchema` 在非空时经 `adk.WithChatModelOptions` 转发它（照搬 `ReasoningEffortOption` 的转发处）。anthropic.go 与 responses.go 在 `Generate`/`Stream` 里用 `model.GetImplSpecificOptions` 解码该 option，并在 `buildRequest` 里把它映射到各自 provider 的结构化输出请求字段。openai ext 路径不解码这个 eino 私有 option 类型，因此自然忽略、不受影响。所有新行为以 `len(schema) > 0` 门控，text 模式保持字节一致。

> **首要研究结论（决定本 plan 的关键事实）。** 调研起点要求"先彻底搞清 anthropic/responses 这两个自定义适配器如何接收并消耗 per-call `model.Option`"。结论：
> 1. **`reasoning_effort` 现在并不到达这两个自定义适配器。** `ReasoningEffortOption` 返回的 option 由 `eino-ext/libs/acl/openai` 的 `openai.WithReasoningEffort` 产生，本质是 `model.WrapImplSpecificOptFn(func(o *openaiOptions){...})`——其闭包形参类型 `*openaiOptions` 是 acl 包的**未导出**类型。两个自定义适配器只调用 `model.GetCommonOptions`（提取 `Temperature`/`Tools`/`MaxTokens` 等公共字段，见 `eino@v0.9.12/components/model/option.go` 的 `Options` 结构体——它**没有** `ReasoningEffort` 字段），从不调用 `GetImplSpecificOptions[*openaiOptions]`，即便调用也无法实例化该未导出类型。所以 reasoning_effort 对这两个适配器是静默 no-op（orchestrator_test.go:734-738 的注释也明说"can't be decoded from outside the acl package"）。
> 2. **因此 `OutputSchemaOption` 不能照搬 `openai.WithReasoningEffort`**。要让它能被本包自己的适配器解码，必须在**本包内**定义一个新的 impl-specific option 类型（`outputSchemaOptions`），用 `model.WrapImplSpecificOptFn(func(o *outputSchemaOptions){...})` 包装，适配器侧用 `model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...)` 解码。`GetImplSpecificOptions[T]` 内部做类型断言 `opt.implSpecificOptFn.(func(*T))`（option.go:247），不同 `T` 的 option **互不串扰**——这正是 openai 路径自动忽略本 option 的保证。

**Tech Stack:** Go stdlib；`github.com/cloudwego/eino@v0.9.12/components/model`（`WrapImplSpecificOptFn` / `GetImplSpecificOptions` / `Option`）；现有 `internal/llm/eino`（anthropic.go、responses.go、fake.go）、`internal/agent/orchestrator`、`internal/api/http`（ws.go、chat.go）。

> **DESIGN NOTE — Anthropic 字段选择（与 brief 的出入，需执行者/lead 确认）。** brief 写的是"anthropic 适配器请求带 `output_schema`（Anthropic API 顶层字段）"。但 claude-api skill（模型缓存 2026-06-24）明确：当前 Anthropic Messages API 的结构化输出字段是 **`output_config.format`**（`{"type":"json_schema","schema":{...}}`），旧的顶层 `output_format` 已弃用，**不存在**顶层 `output_schema` 字段。OpenAI Responses 的 `text.format.json_schema` brief 写对了。
>
> 本 plan 因此实现 `output_config.format`（更可能正确、与 skill 一致），而**不是**顶层 `output_schema`。代码结构使得切换到顶层 `output_schema`（若你的目标网关，如 ai.lfree.org / OpenRouter-like 代理，确实接受该字段）只需改 Task 2 里的结构体标签与字段名（约 2 行）。**执行前请确认目标 Anthropic 端点接受哪个字段**；若 lead 确认是顶层 `output_schema`，按 Task 2 末尾的"备选"注释改。本偏差在 Self-Review 与返回摘要里再次标注。

---

## File Structure

- **Create** `internal/llm/eino/outputschema.go` — `outputSchemaOptions`（impl-specific option 类型）+ `OutputSchemaOption(json.RawMessage) *model.Option`。照 `reasoning.go` 的风格：包内私有 option 类型 + 一个导出的构造函数。
- **Create** `internal/llm/eino/outputschema_test.go` — option 构造/类型隔离/FakeModel 记录的单测。
- **Modify** `internal/llm/eino/fake.go` — `FakeModel` 加 `ReceivedOutputSchema json.RawMessage` 字段，`recordOpts` 用 `GetImplSpecificOptions` 解码（仅 `RecordOpts=true` 时），让 orchestrator/transport 测试无需触碰未导出 option 类型即可断言 schema 到达模型。
- **Modify** `internal/llm/eino/anthropic.go` — `anthropicRequest` 加 `OutputConfig` 字段 + 配套 wire 类型；`Generate`/`Stream` 解码 impl-specific option 并传入 `buildRequest`；`buildRequest` 在 schema 非空时填充 `output_config.format`。
- **Modify** `internal/llm/eino/anthropic_test.go` — `TestAnthropicGenerate_OutputSchema`（断言请求体含 `output_config.format.json_schema`）+ `TestAnthropicGenerate_NoSchemaOmitsOutputConfig`（text 模式回归）。
- **Modify** `internal/llm/eino/responses.go` — `responsesRequest` 加 `Text` 字段 + 配套 wire 类型；`Generate`/`Stream` 解码并传入；`buildRequest` 在 schema 非空时填充 `text.format.json_schema`。
- **Modify** `internal/llm/eino/responses_test.go` — `TestResponsesGenerate_OutputSchema` + `TestResponsesGenerate_NoSchemaOmitsText`。
- **Modify** `internal/agent/orchestrator/orchestrator.go` — `TurnOpts` 加 `OutputSchema json.RawMessage`；`EventsWithHistoryOpts` 在非空时转发 option（照搬 line 647-649 的 `ReasoningEffortOption` 转发）；import 加 `"encoding/json"`。
- **Modify** `internal/agent/orchestrator/orchestrator_test.go` — `TestEventsWithHistoryOpts_OutputSchemaPassesOption`（经 FakeModel.ReceivedOutputSchema 断言端到端）。
- **Modify** `internal/api/http/ws.go` — `TurnOpts{...}` 构造处（约 723 行）加 `OutputSchema: cf.OutputSchema`。**依赖 A12-core 合并**（`cf.OutputSchema` 由 A12-core 引入）。
- **Modify** `internal/api/http/chat.go` — `TurnOpts{...}` 构造处（约 107 行）加 `OutputSchema: req.OutputSchema`。**依赖 A12-core 合并**。

> **openai（eino-ext chat completions）路径不改任何文件。** 它不解码 `outputSchemaOptions`（类型不匹配），option 静默忽略——这是 Task 1 的类型隔离测试所保证的。v1 靠 A12-core 的校验重试兜底。

---

## Task 1: `OutputSchemaOption` + impl-specific option 类型 + FakeModel 记录

**Files:**
- Create: `internal/llm/eino/outputschema.go`
- Create: `internal/llm/eino/outputschema_test.go`
- Modify: `internal/llm/eino/fake.go:44-58`（字段块）、`internal/llm/eino/fake.go:129-137`（`recordOpts`）、import 块加 `"encoding/json"`

- [ ] **Step 1: 写失败测试**

`internal/llm/eino/outputschema_test.go`:
```go
package eino

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloudwego/eino/schema"
)

// TestOutputSchemaOption_Dispatch proves the helper returns a non-nil option
// for a non-empty schema and nil for an empty schema (so the no-schema / text
// path forwards nothing and stays byte-identical to pre-A12).
func TestOutputSchemaOption_Dispatch(t *testing.T) {
	opt := OutputSchemaOption(json.RawMessage(`{"type":"object"}`))
	require.NotNil(t, opt, "non-empty schema must yield a non-nil option")

	assert.Nil(t, OutputSchemaOption(nil), "nil schema must yield nil")
	assert.Nil(t, OutputSchemaOption(json.RawMessage(``)), "empty schema must yield nil")
}

// TestOutputSchemaOption_TypeIsolation proves the eino-owned output-schema
// option and the openai-owned reasoning-effort option do NOT cross-contaminate:
// GetImplSpecificOptions[T] applies only the setters whose func type matches
// func(*T). This is the structural guarantee that the openai (chat completions,
// eino-ext) path — which decodes its own *openaiOptions — silently ignores our
// output-schema option ("openai 路径不受影响").
func TestOutputSchemaOption_TypeIsolation(t *testing.T) {
	schemaOpt := *OutputSchemaOption(json.RawMessage(`{"type":"object"}`))
	reasonOpt := ReasoningEffortOption("high") // openai WithReasoningEffort
	require.NotNil(t, reasonOpt)

	// Decoding as outputSchemaOptions picks up the schema, drops the effort.
	got := model.GetImplSpecificOptions(&outputSchemaOptions{}, schemaOpt, *reasonOpt)
	assert.Equal(t, `{"type":"object"}`, string(got.Schema),
		"output-schema decode must see the schema and ignore the openai option")

	// Decoding as outputSchemaOptions with ONLY the openai option sees nothing.
	got2 := model.GetImplSpecificOptions(&outputSchemaOptions{}, *reasonOpt)
	assert.Empty(t, got2.Schema, "openai option must not leak into output-schema decode")
}

// TestFakeModel_RecordsOutputSchema proves FakeModel captures the decoded
// schema from the per-call options when RecordOpts is set. The orchestrator
// forwarding test (Task 4) relies on this.
func TestFakeModel_RecordsOutputSchema(t *testing.T) {
	m := NewFakeModel([]string{"ok"}, nil)
	m.RecordOpts = true

	optPtr := OutputSchemaOption(json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}}}`))
	require.NotNil(t, optPtr)
	_, err := m.Generate(context.Background(),
		[]*schema.Message{schema.UserMessage("hi")}, *optPtr)
	require.NoError(t, err)

	assert.Equal(t, `{"type":"object","properties":{"x":{"type":"integer"}}}`,
		string(m.ReceivedOutputSchema), "FakeModel must record the decoded output schema")

	// No option -> nothing recorded.
	m.ReceivedOutputSchema = nil
	_, _ = m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")})
	assert.Empty(t, m.ReceivedOutputSchema, "a call without the option must not record a schema")
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```sh
go test ./internal/llm/eino -run "TestOutputSchemaOption_Dispatch|TestOutputSchemaOption_TypeIsolation|TestFakeModel_RecordsOutputSchema" -v
```
Expected: FAIL（`OutputSchemaOption` / `outputSchemaOptions` 未定义；`FakeModel.ReceivedOutputSchema` 字段不存在）。

- [ ] **Step 3: 实现 `outputschema.go`**

`internal/llm/eino/outputschema.go`:
```go
package eino

import (
	"encoding/json"

	"github.com/cloudwego/eino/components/model"
)

// outputSchemaOptions is the eino-package-owned impl-specific option struct
// that carries a per-turn JSON Schema for structured output. It is consumed
// ONLY by this package's own adapters (anthropic.go, responses.go) via
// model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...).
//
// Why an eino-owned type (and not openai.WithReasoningEffort-style)? The
// reasoning_effort option produced by eino-ext/libs/acl/openai wraps a setter
// over the acl package's UNEXPORTED *openaiOptions, so no caller outside that
// package can decode it — and indeed reasoning_effort never reaches the custom
// anthropic/responses adapters today (they only call model.GetCommonOptions).
// By owning the option type HERE, the package's own adapters can decode it,
// while the openai (eino-ext chat completions) path — which decodes its own
// *openaiOptions — silently ignores it (model.GetImplSpecificOptions[T] type-
// asserts each option's setter against func(*T) and skips non-matching ones;
// see eino@v0.9.12/components/model/option.go:239-255). That type isolation is
// exactly what makes "openai 路径不受影响" structural rather than convention-based.
type outputSchemaOptions struct {
	Schema json.RawMessage
}

// OutputSchemaOption returns a per-call model.Option that carries a JSON Schema
// (schemaDoc) for structured output on a single Generate/Stream invocation, or
// nil when schemaDoc is empty so the text-mode path forwards nothing and stays
// byte-identical to pre-A12.
//
// Pass the returned option through the ADK agent via adk.WithChatModelOptions
// (same forwarding shape as ReasoningEffortOption). The anthropic and responses
// adapters decode it in Generate/Stream and map it onto the provider's native
// structured-output request field; the openai (eino-ext) path does not decode
// it and is unaffected.
//
// model.Option is a struct (not an interface); a nil *model.Option signals
// "no option" so callers skip wiring it when nil.
func OutputSchemaOption(schemaDoc json.RawMessage) *model.Option {
	if len(schemaDoc) == 0 {
		return nil
	}
	opt := model.WrapImplSpecificOptFn(func(o *outputSchemaOptions) {
		o.Schema = schemaDoc
	})
	return &opt
}
```

- [ ] **Step 4: 改 `fake.go` 记录解码后的 schema**

(a) 在 import 块（`fake.go:3-10`）加 `"encoding/json"`：
```go
import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)
```

(b) 在 `ReceivedOpts` 字段下方（`fake.go:44-45` 之后）加：
```go
	// ReceivedOutputSchema is the output schema decoded (via
	// model.GetImplSpecificOptions) from the per-call options of the most recent
	// Generate/Stream call, when RecordOpts is true. It lets orchestrator and
	// transport tests assert the schema survived the option-forwarding path
	// (TurnOpts.OutputSchema -> adk.WithChatModelOptions -> adapter) WITHOUT
	// reaching into the unexported outputSchemaOptions type. Overwrite per call,
	// like ReceivedOpts. nil when no schema option was passed.
	ReceivedOutputSchema json.RawMessage
```

(c) 替换 `recordOpts`（`fake.go:129-137`）为：
```go
// recordOpts captures the call's model.Options when RecordOpts is set, and
// decodes the per-turn output schema (if any) into ReceivedOutputSchema so
// higher-layer tests can assert schema forwarding without importing the
// unexported option type.
func (m *FakeModel) recordOpts(opts []model.Option) {
	if !m.RecordOpts {
		return
	}
	m.optsMu.Lock()
	m.ReceivedOpts = opts
	implOpts := model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...)
	m.ReceivedOutputSchema = implOpts.Schema
	m.optsMu.Unlock()
}
```

- [ ] **Step 5: 运行测试确认通过**

Run:
```sh
go test ./internal/llm/eino -run "TestOutputSchemaOption_Dispatch|TestOutputSchemaOption_TypeIsolation|TestFakeModel_RecordsOutputSchema" -v
```
Expected: PASS。

- [ ] **Step 6: 提交**

```sh
git add internal/llm/eino/outputschema.go internal/llm/eino/outputschema_test.go internal/llm/eino/fake.go
git commit -m "feat(llm/eino): add OutputSchemaOption per-call option (A12-providers)"
```

---

## Task 2: anthropic 适配器 — 注入 `output_config.format`

**Files:**
- Modify: `internal/llm/eino/anthropic.go:123-130`（`anthropicRequest`）、`anthropic.go:188-227`（`Generate`）、`anthropic.go:231-262`（`Stream`）、`anthropic.go:275-391`（`buildRequest`）
- Modify: `internal/llm/eino/anthropic_test.go`（追加两个测试）

- [ ] **Step 1: 写失败测试**

追加到 `internal/llm/eino/anthropic_test.go`（文件已 import `context`/`encoding/json`/`io`/`net/http`/`net/http/httptest`/`strings`/`testing`/`schema`）：
```go
// TestAnthropicGenerate_OutputSchema proves a per-turn OutputSchemaOption makes
// the adapter emit output_config.format = {type:"json_schema", schema:{...}} on
// the POST /v1/messages body (the Anthropic Messages API structured-output
// field). See the plan's DESIGN NOTE for why output_config.format is used over
// a top-level output_schema.
func TestAnthropicGenerate_OutputSchema(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "message", "role": "assistant", "stop_reason": "end_turn",
			"content": []any{map[string]any{"type": "text", "text": "{}"}},
		})
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{APIKey: "k", Model: "claude", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	optPtr := OutputSchemaOption(json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"],"additionalProperties":false}`))
	if optPtr == nil {
		t.Fatal("OutputSchemaOption must be non-nil for a non-empty schema")
	}
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}, *optPtr); err != nil {
		t.Fatal(err)
	}

	oc, ok := captured["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("期望请求体含 output_config，实际 %v", captured)
	}
	fmtField, ok := oc["format"].(map[string]any)
	if !ok || fmtField["type"] != "json_schema" {
		t.Fatalf("期望 output_config.format.type=json_schema，实际 %v", oc["format"])
	}
	sch, _ := fmtField["schema"].(map[string]any)
	if sch == nil || sch["type"] != "object" {
		t.Fatalf("期望 output_config.format.schema.type=object，实际 %v", fmtField["schema"])
	}
}

// TestAnthropicGenerate_NoSchemaOmitsOutputConfig is the text-mode regression
// guard: with no OutputSchemaOption the request body must NOT contain
// output_config (byte-identical to pre-A12 on the wire).
func TestAnthropicGenerate_NoSchemaOmitsOutputConfig(t *testing.T) {
	var sawOutputConfig bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if _, ok := m["output_config"]; ok {
			sawOutputConfig = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "message", "role": "assistant", "stop_reason": "end_turn",
			"content": []any{map[string]any{"type": "text", "text": "hi"}},
		})
	}))
	defer srv.Close()

	m, err := NewAnthropicModel(context.Background(), &AnthropicModelConfig{APIKey: "k", Model: "claude", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatal(err)
	}
	if sawOutputConfig {
		t.Fatal("text 模式请求体不得含 output_config")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```sh
go test ./internal/llm/eino -run "TestAnthropicGenerate_OutputSchema|TestAnthropicGenerate_NoSchemaOmitsOutputConfig" -v
```
Expected: `TestAnthropicGenerate_OutputSchema` FAIL（请求体无 `output_config`）；回归测试 PASS（当前本就不发）。

- [ ] **Step 3: 改 `anthropic.go`**

(a) 在 `anthropicRequest`（`anthropic.go:123-130`）末尾加 `OutputConfig` 字段，并在其上方加配套 wire 类型：
```go
// anthropicOutputConfig maps onto the Anthropic Messages API top-level
// output_config field for structured outputs ({"format":{"type":"json_schema",
// "schema":{...}}}). Pointer-optional so omitempty drops it entirely on
// text-mode turns. See plan DESIGN NOTE for field choice.
type anthropicOutputConfig struct {
	Format anthropicOutputFormat `json:"format"`
}

// anthropicOutputFormat is the output_config.format payload. Type is always
// "json_schema"; Schema is the raw JSON Schema document.
type anthropicOutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream"`

	// OutputConfig, when non-nil, requests provider-enforced structured output
	// for this turn (Anthropic output_config.format json_schema). nil on
	// text-mode turns so the field is omitted from the wire body entirely.
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}
```

(b) `Generate`（`anthropic.go:188-194`）在取公共 options 后取 impl-specific options，并传入 buildRequest。把：
```go
func (m *AnthropicModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	options := model.GetCommonOptions(&model.Options{}, opts...)
	req, err := m.buildRequest(in, options, false)
```
改为：
```go
func (m *AnthropicModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	options := model.GetCommonOptions(&model.Options{}, opts...)
	implOpts := model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...)
	req, err := m.buildRequest(in, options, implOpts, false)
```

(c) `Stream`（`anthropic.go:231-236`）同样改。把：
```go
func (m *AnthropicModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	options := model.GetCommonOptions(&model.Options{}, opts...)
	req, err := m.buildRequest(in, options, true)
```
改为：
```go
func (m *AnthropicModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	options := model.GetCommonOptions(&model.Options{}, opts...)
	implOpts := model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...)
	req, err := m.buildRequest(in, options, implOpts, true)
```

(d) `buildRequest` 签名加 `implOpts *outputSchemaOptions`，并在函数体开头（`anthropic.go:275-280` 构造 `req` 之后）填充 `OutputConfig`。把：
```go
func (m *AnthropicModel) buildRequest(in []*schema.Message, options *model.Options, stream bool) (*anthropicRequest, error) {
	req := &anthropicRequest{
		Model:     m.config.Model,
		MaxTokens: m.config.MaxTokens,
		Stream:    stream,
	}
```
改为：
```go
func (m *AnthropicModel) buildRequest(in []*schema.Message, options *model.Options, implOpts *outputSchemaOptions, stream bool) (*anthropicRequest, error) {
	req := &anthropicRequest{
		Model:     m.config.Model,
		MaxTokens: m.config.MaxTokens,
		Stream:    stream,
	}
	// A12-providers: when the turn declared an output schema (per-call
	// OutputSchemaOption), request provider-enforced structured output. Empty
	// schema = text mode, OutputConfig stays nil and is omitted from the wire
	// body entirely (byte-identical to pre-A12).
	if len(implOpts.Schema) > 0 {
		req.OutputConfig = &anthropicOutputConfig{
			Format: anthropicOutputFormat{
				Type:   "json_schema",
				Schema: implOpts.Schema,
			},
		}
	}
```

> **备选（若 lead 确认目标网关接受顶层 `output_schema`）。** 把 wire 类型与字段换成：
> ```go
> type anthropicRequest struct {
>     ...
>     OutputSchema json.RawMessage `json:"output_schema,omitempty"`
> }
> ```
> buildRequest 里：`if len(implOpts.Schema) > 0 { req.OutputSchema = implOpts.Schema }`。测试相应断言 `captured["output_schema"]`。其余步骤不变。

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/llm/eino -run "TestAnthropicGenerate_OutputSchema|TestAnthropicGenerate_NoSchemaOmitsOutputConfig" -v
go test ./internal/llm/eino -v
```
Expected: 新测试 PASS；既有 anthropic 测试全 PASS（text 模式回归）。

- [ ] **Step 5: 提交**

```sh
git add internal/llm/eino/anthropic.go internal/llm/eino/anthropic_test.go
git commit -m "feat(llm/eino): inject output_config.format in anthropic adapter (A12-providers)"
```

---

## Task 3: responses 适配器 — 注入 `text.format.json_schema`

**Files:**
- Modify: `internal/llm/eino/responses.go:179-186`（`responsesRequest`）、`responses.go:275-281`（`Generate`）、`responses.go:318-324`（`Stream`）、`responses.go:377-382`（`buildRequest`）
- Modify: `internal/llm/eino/responses_test.go`（追加两个测试）

- [ ] **Step 1: 写失败测试**

追加到 `internal/llm/eino/responses_test.go`（文件已 import `context`/`encoding/json`/`fmt`/`io`/`net/http`/`net/http/httptest`/`strings`/`testing`/`time`/`schema`）：
```go
// TestResponsesGenerate_OutputSchema proves a per-turn OutputSchemaOption makes
// the adapter emit text.format = {type:"json_schema", name, schema, strict} on
// the POST /v1/responses body (the OpenAI Responses API structured-output
// field).
func TestResponsesGenerate_OutputSchema(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_1", "object": "response", "status": "completed",
			"output": []any{map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "{}"}},
			}},
		})
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	optPtr := OutputSchemaOption(json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"],"additionalProperties":false}`))
	if optPtr == nil {
		t.Fatal("OutputSchemaOption must be non-nil for a non-empty schema")
	}
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}, *optPtr); err != nil {
		t.Fatal(err)
	}

	txt, ok := captured["text"].(map[string]any)
	if !ok {
		t.Fatalf("期望请求体含 text，实际 %v", captured)
	}
	fmtField, ok := txt["format"].(map[string]any)
	if !ok || fmtField["type"] != "json_schema" {
		t.Fatalf("期望 text.format.type=json_schema，实际 %v", txt["format"])
	}
	if fmtField["name"] == nil || fmtField["name"] == "" {
		t.Fatalf("期望 text.format.name 非空（OpenAI 要求），实际 %v", fmtField["name"])
	}
	sch, _ := fmtField["schema"].(map[string]any)
	if sch == nil || sch["type"] != "object" {
		t.Fatalf("期望 text.format.schema.type=object，实际 %v", fmtField["schema"])
	}
}

// TestResponsesGenerate_NoSchemaOmitsText is the text-mode regression guard:
// with no OutputSchemaOption the request body must NOT contain a text field.
func TestResponsesGenerate_NoSchemaOmitsText(t *testing.T) {
	var sawText bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if _, ok := m["text"]; ok {
			sawText = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_1", "object": "response", "status": "completed",
			"output": []any{map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "hi"}},
			}},
		})
	}))
	defer srv.Close()

	m, err := NewOpenAIResponsesModel(context.Background(), &ResponsesConfig{APIKey: "k", Model: "gpt-4o", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("hi")}); err != nil {
		t.Fatal(err)
	}
	if sawText {
		t.Fatal("text 模式请求体不得含 text 字段")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```sh
go test ./internal/llm/eino -run "TestResponsesGenerate_OutputSchema|TestResponsesGenerate_NoSchemaOmitsText" -v
```
Expected: `TestResponsesGenerate_OutputSchema` FAIL（请求体无 `text`）；回归测试 PASS。

- [ ] **Step 3: 改 `responses.go`**

(a) 在 `responsesRequest`（`responses.go:179-186`）末尾加 `Text` 字段，并在其上方加配套 wire 类型：
```go
// responsesTextConfig maps onto the OpenAI Responses API top-level text field
// for structured outputs ({"format":{"type":"json_schema","name":...,
// "schema":{...},"strict":true}}). Pointer-optional so omitempty drops it on
// text-mode turns.
type responsesTextConfig struct {
	Format responsesTextFormat `json:"format"`
}

// responsesTextFormat is the text.format payload. Name is required by the
// Responses API (arbitrary identifier); Strict enforces the schema.
type responsesTextFormat struct {
	Type   string          `json:"type"`   // "json_schema"
	Name   string          `json:"name"`   // required by OpenAI Responses
	Schema json.RawMessage `json:"schema"` // the JSON Schema document
	Strict bool            `json:"strict"`
}

// responsesRequest is the POST /v1/responses body.
type responsesRequest struct {
	Model           string               `json:"model"`
	Input           []responsesInputItem `json:"input"`
	Instructions    string               `json:"instructions,omitempty"`
	Stream          bool                 `json:"stream"`
	MaxOutputTokens int                  `json:"max_output_tokens,omitempty"`
	Tools           []responsesTool      `json:"tools,omitempty"`

	// Text, when non-nil, requests provider-enforced structured output for this
	// turn (Responses API text.format json_schema). nil on text-mode turns so
	// the field is omitted from the wire body entirely.
	Text *responsesTextConfig `json:"text,omitempty"`
}
```
> 注：该结构体原有字段的对齐已在此调整（`MaxOutputTokens` 与 `Tools` 之间去掉多余空格使 gofmt 干净）；若 gofmt 报错，以 gofmt 为准。

(b) `Generate`（`responses.go:275-281`）取 impl-specific options 并传入 buildRequest。把：
```go
func (m *openaiResponsesModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	options := model.GetCommonOptions(&model.Options{}, opts...)
	req, err := m.buildRequest(in, options, false)
```
改为：
```go
func (m *openaiResponsesModel) Generate(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	options := model.GetCommonOptions(&model.Options{}, opts...)
	implOpts := model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...)
	req, err := m.buildRequest(in, options, implOpts, false)
```

(c) `Stream`（`responses.go:318-324`）同样改。把：
```go
func (m *openaiResponsesModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	options := model.GetCommonOptions(&model.Options{}, opts...)
	req, err := m.buildRequest(in, options, true)
```
改为：
```go
func (m *openaiResponsesModel) Stream(ctx context.Context, in []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	options := model.GetCommonOptions(&model.Options{}, opts...)
	implOpts := model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...)
	req, err := m.buildRequest(in, options, implOpts, true)
```

(d) `buildRequest` 签名加 `implOpts *outputSchemaOptions`，并在构造 `req` 后填充 `Text`。把：
```go
func (m *openaiResponsesModel) buildRequest(in []*schema.Message, options *model.Options, stream bool) (*responsesRequest, error) {
	req := &responsesRequest{
		Model:           m.cfg.Model,
		Stream:          stream,
		MaxOutputTokens: m.cfg.MaxOutputTokens,
	}
```
改为：
```go
func (m *openaiResponsesModel) buildRequest(in []*schema.Message, options *model.Options, implOpts *outputSchemaOptions, stream bool) (*responsesRequest, error) {
	req := &responsesRequest{
		Model:           m.cfg.Model,
		Stream:          stream,
		MaxOutputTokens: m.cfg.MaxOutputTokens,
	}
	// A12-providers: when the turn declared an output schema (per-call
	// OutputSchemaOption), request provider-enforced structured output. Empty
	// schema = text mode, Text stays nil and is omitted from the wire body
	// entirely (byte-identical to pre-A12). Name is required by the Responses
	// API but otherwise arbitrary; "output" is the conventional default.
	if len(implOpts.Schema) > 0 {
		req.Text = &responsesTextConfig{
			Format: responsesTextFormat{
				Type:   "json_schema",
				Name:   "output",
				Schema: implOpts.Schema,
				Strict: true,
			},
		}
	}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/llm/eino -run "TestResponsesGenerate_OutputSchema|TestResponsesGenerate_NoSchemaOmitsText" -v
go test ./internal/llm/eino -v
```
Expected: 新测试 PASS；既有 responses 测试全 PASS（text 模式回归）。

- [ ] **Step 5: 提交**

```sh
git add internal/llm/eino/responses.go internal/llm/eino/responses_test.go
git commit -m "feat(llm/eino): inject text.format json_schema in responses adapter (A12-providers)"
```

---

## Task 4: orchestrator `TurnOpts.OutputSchema` 转发

**Files:**
- Modify: `internal/agent/orchestrator/orchestrator.go:4-20`（import）、`orchestrator.go:616-629`（`TurnOpts`）、`orchestrator.go:646-649`（`EventsWithHistoryOpts` 的 option 转发处）
- Modify: `internal/agent/orchestrator/orchestrator_test.go`（追加测试）

- [ ] **Step 1: 写失败测试**

追加到 `internal/agent/orchestrator/orchestrator_test.go`（顶部 import 块加 `"encoding/json"` 若未导入）：
```go
// TestEventsWithHistoryOpts_OutputSchemaPassesOption proves a non-empty
// TurnOpts.OutputSchema is forwarded to the model as the per-call
// OutputSchemaOption (the schema reaches the adapter via
// adk.WithChatModelOptions), and an empty OutputSchema forwards nothing.
// FakeModel.ReceivedOutputSchema (populated by recordOpts decoding the
// impl-specific option) makes this an end-to-end-through-orchestrator
// assertion, not just an option-count delta.
func TestEventsWithHistoryOpts_OutputSchemaPassesOption(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"ok", "ok"}, nil)
	fm.RecordOpts = true
	o, err := New(Config{Model: fm})
	require.NoError(t, err)

	msgs := []*schema.Message{{Role: schema.User, Content: "hi"}}
	schemaDoc := json.RawMessage(`{"type":"object","properties":{"x":{"type":"integer"}}}`)

	drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{OutputSchema: schemaDoc}))
	assert.Equal(t, string(schemaDoc), string(fm.ReceivedOutputSchema),
		"TurnOpts.OutputSchema must reach the model as the decoded schema")

	// Empty OutputSchema -> nothing forwarded.
	fm.ReceivedOutputSchema = nil
	drainAgentChunks(t, o.EventsWithHistoryOpts(context.Background(), msgs, TurnOpts{}))
	assert.Empty(t, fm.ReceivedOutputSchema, "empty OutputSchema must not forward a schema option")
}
```
> 若 `orchestrator_test.go` 未 import `"encoding/json"`，在 import 块加入。

- [ ] **Step 2: 运行测试确认失败**

Run:
```sh
go test ./internal/agent/orchestrator -run TestEventsWithHistoryOpts_OutputSchemaPassesOption -v
```
Expected: 编译失败（`TurnOpts.OutputSchema` 未定义）或 FAIL。

- [ ] **Step 3: 改 `orchestrator.go`**

(a) import 块（`orchestrator.go:4-20`）加 `"encoding/json"`：
```go
import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/autocode/internal/guard"
	einollm "github.com/x6nux/autocode/internal/llm/eino"
	"github.com/x6nux/autocode/internal/proto"
	"github.com/x6nux/autocode/internal/tools"
)
```

(b) `TurnOpts`（`orchestrator.go:616-629`）加 `OutputSchema` 字段。把：
```go
type TurnOpts struct {
	Model          model.BaseChatModel
	ThinkingEffort string
}
```
改为：
```go
type TurnOpts struct {
	Model          model.BaseChatModel
	ThinkingEffort string

	// OutputSchema, when non-empty, is a JSON Schema document that the turn's
	// model should enforce as structured output. It is forwarded to the model
	// as a per-call OutputSchemaOption via adk.WithChatModelOptions (same
	// forwarding shape as ThinkingEffort). The anthropic and responses adapters
	// decode it and map it onto output_config.format / text.format json_schema;
	// the openai (eino-ext) path silently ignores it (v1 relies on A12-core's
	// validate-and-retry). Empty = text mode, nothing forwarded.
	OutputSchema json.RawMessage
}
```
（同时把 `TurnOpts` 的 doc 注释里补一行说明 OutputSchema 的语义，保持注释承重密度。）

(c) `EventsWithHistoryOpts`（`orchestrator.go:646-649`）的 option 转发处，照搬 `ReasoningEffortOption` 的写法加 OutputSchema 转发。把：
```go
	var runOpts []adk.AgentRunOption
	if optPtr := einollm.ReasoningEffortOption(opts.ThinkingEffort); optPtr != nil {
		runOpts = append(runOpts, adk.WithChatModelOptions([]model.Option{*optPtr}))
	}
```
改为：
```go
	var runOpts []adk.AgentRunOption
	if optPtr := einollm.ReasoningEffortOption(opts.ThinkingEffort); optPtr != nil {
		runOpts = append(runOpts, adk.WithChatModelOptions([]model.Option{*optPtr}))
	}
	if optPtr := einollm.OutputSchemaOption(opts.OutputSchema); optPtr != nil {
		runOpts = append(runOpts, adk.WithChatModelOptions([]model.Option{*optPtr}))
	}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```sh
go test ./internal/agent/orchestrator -run TestEventsWithHistoryOpts_OutputSchemaPassesOption -v
go test ./internal/agent/orchestrator -v
```
Expected: 新测试 PASS；既有 orchestrator 测试全 PASS（含 ThinkingEffort、per-model runner、streaming 回归）。

- [ ] **Step 5: 提交**

```sh
git add internal/agent/orchestrator/orchestrator.go internal/agent/orchestrator/orchestrator_test.go
git commit -m "feat(orchestrator): forward TurnOpts.OutputSchema as per-call option (A12-providers)"
```

---

## Task 5: 传输层接线 — WS / SSE 把声明 schema 填入 `TurnOpts`

> **PREREQUISITE：A12-core（lane1a）必须已合并。** 本任务读 `cf.OutputSchema`（WS）与 `req.OutputSchema`（SSE）——这两个字段由 A12-core 在 `proto.ClientFrame` 与 SSE 请求体里引入。若 A12-core 尚未合并，**先跳过本任务**，执行 Task 1-4（已覆盖全部验收标准），待 A12-core 合并后再做本任务。没有本任务，schema 能从 orchestrator 到达适配器（Task 4 已证），但传输层不会把客户端声明的 schema 灌进 TurnOpts，feature 在生产里是"断的"——故本任务是把整条链路接通的必要一环。

**Files:**
- Modify: `internal/api/http/ws.go:723-726`（`TurnOpts` 构造）
- Modify: `internal/api/http/chat.go:107-110`（`TurnOpts` 构造）

- [ ] **Step 1: 写失败测试（端到端经传输层）**

在 `internal/api/http/ws_test.go` 追加（照抄现有 WS handler 测试如何装配 Server + 注入 FakeModel + 用 proto 帧往返；以下给出断言骨架，装配照抄最近的 `TestWS_*` 用例）：
```go
// TestWS_OutputSchemaReachesModel proves the WS user_message branch fills
// TurnOpts.OutputSchema from cf.OutputSchema so the schema reaches the model
// (end-to-end through the transport -> orchestrator -> adapter path).
func TestWS_OutputSchemaReachesModel(t *testing.T) {
	// 装配：照抄现有 TestWS_* —— 构造 Server、注入 einollm.NewFakeModel（RecordOpts=true）、
	// 建立 WS 连接、发送 proto.NewUserMessageWithSchema("hi", json.RawMessage(`{"type":"object"}`))。
	// 抽干 ServerFrame 流至 done。
	//
	// 断言：fm.ReceivedOutputSchema == `{"type":"object"}`（schema 经 cf.OutputSchema ->
	// TurnOpts.OutputSchema -> option -> FakeModel 到达）。
}

func TestWS_NoSchemaLeavesModelEmpty(t *testing.T) {
	// 装配同上，但发送普通 proto.NewUserMessage("hi")（无 schema）。
	// 断言：fm.ReceivedOutputSchema 为空（text 模式不转发）。
}
```
> 装配细节：照抄 `ws_test.go` 里既有 WS handler 测试如何构造 `Server`、注入 fake model、发帧、收帧。先读 `ws_test.go` 既有用例确认桩，再填实现。`proto.NewUserMessageWithSchema` 由 A12-core 提供。

SSE 侧在 `internal/api/http/chat_test.go` 追加（照抄现有 `TestChat*` 如何 POST 到 `/api/v1/chat`）：
```go
func TestChat_OutputSchemaReachesModel(t *testing.T) {
	// POST {"message":"hi","output_schema":{"type":"object"}} 到 /api/v1/chat，
	// 装配含 RecordOpts=true 的 FakeModel。
	// 断言：fm.ReceivedOutputSchema == `{"type":"object"}`。
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```sh
go test ./internal/api/http -run "TestWS_OutputSchemaReachesModel|TestWS_NoSchemaLeavesModelEmpty|TestChat_OutputSchemaReachesModel" -v
```
Expected: FAIL（当前 ws.go/chat.go 不填 `TurnOpts.OutputSchema`，故 `fm.ReceivedOutputSchema` 为空）。

- [ ] **Step 3: 改 `ws.go`**

把 `ws.go:723-726` 的 TurnOpts 构造：
```go
					opts := orchestrator.TurnOpts{
						Model:          cs.selectModel(models),
						ThinkingEffort: cs.thinking,
					}
```
改为：
```go
					opts := orchestrator.TurnOpts{
						Model:          cs.selectModel(models),
						ThinkingEffort: cs.thinking,
						OutputSchema:   cf.OutputSchema,
					}
```
> `cf` 是 user_message 分支的 ClientFrame，在作用域内（A12-core 引入 `cf.OutputSchema`）。

- [ ] **Step 4: 改 `chat.go`**

把 `chat.go:107-110` 的 TurnOpts 构造：
```go
		opts := orchestrator.TurnOpts{ThinkingEffort: req.Thinking}
		if req.Model != "" && models[req.Model] != nil {
			opts.Model = models[req.Model]
		}
```
改为：
```go
		opts := orchestrator.TurnOpts{
			ThinkingEffort: req.Thinking,
			OutputSchema:   req.OutputSchema,
		}
		if req.Model != "" && models[req.Model] != nil {
			opts.Model = models[req.Model]
		}
```
> `req.OutputSchema` 由 A12-core 在 SSE 请求体结构里引入。

- [ ] **Step 5: 运行测试确认通过**

Run:
```sh
go test ./internal/api/http -run "TestWS_OutputSchemaReachesModel|TestWS_NoSchemaLeavesModelEmpty|TestChat_OutputSchemaReachesModel" -v
go test ./internal/api/http -v
```
Expected: 新测试 PASS；既有 WS/SSE 测试全 PASS（text 模式回归）。

- [ ] **Step 6: 提交**

```sh
git add internal/api/http/ws.go internal/api/http/chat.go internal/api/http/ws_test.go internal/api/http/chat_test.go
git commit -m "feat(http): wire cf/req OutputSchema into TurnOpts (A12-providers)"
```

---

## Task 6: 全量回归

**Files:** 无新增；运行全量测试 + vet + build。

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

- [ ] **Step 4: 冒烟（可选，fake model）**

Run:
```sh
timeout 5 ./autocode --fake-model -inprocess -h
```
Expected: 打印用法并退出 0（确认改动没破坏启动）。

- [ ] **Step 5: 提交（若前序步骤有未提交的小修）**

```sh
git add -A
git commit -m "test: A12-providers regression green" || echo "nothing to commit"
```

---

## Self-Review（写完后自查结果）

1. **Spec 覆盖**（对照 brief 验收）：
   - 「anthropic 适配器请求带结构化输出字段」→ Task 2 注入 `output_config.format.json_schema`，`TestAnthropicGenerate_OutputSchema` 断言请求体含该字段 ✅。（**字段名偏差见下条**。）
   - 「responses 适配器请求带 `text.format json_schema`」→ Task 3，`TestResponsesGenerate_OutputSchema` 断言 ✅。
   - 「单测证明 schema 到达请求体」→ Task 2/3 的 httptest 捕获请求体断言；Task 4 的 FakeModel 端到端断言；Task 5 传输层端到端断言 ✅。
   - 「openai 路径不受影响」→ 不改任何 openai 路径文件；`TestOutputSchemaOption_TypeIsolation` 证明 option 类型隔离（openai 的 `GetImplSpecificOptions[openaiOptions]` 跳过我们的 setter）✅。
   - 「text 模式（无 schema）请求体不变」→ Task 2/3 各有 `...NoSchemaOmits...` 回归测试；option 在空 schema 时返回 nil 不转发（Task 1）✅。
2. **Placeholder 扫描**：无 TBD/TODO；Task 5 的 WS/SSE 测试用了「照抄现有 X 测试的桩」措辞（与 A12-core 参考计划同一惯例，避免粘贴大段装配代码），工程师需先读既有 `ws_test.go`/`chat_test.go`。可接受；若要更硬可补桩。所有 Go 代码步骤完整。
3. **类型一致性**：`outputSchemaOptions`（Task 1 定义）→ `OutputSchemaOption`（Task 1）→ anthropic/responses 的 `model.GetImplSpecificOptions(&outputSchemaOptions{}, opts...)`（Task 2/3）→ orchestrator `TurnOpts.OutputSchema` + `einollm.OutputSchemaOption`（Task 4）命名一致 ✅。`FakeModel.ReceivedOutputSchema`（Task 1）被 Task 4/5 测试消费 ✅。buildRequest 新签名 `(in, options, implOpts, stream)` 在 Generate/Stream 两处一致 ✅。
4. **关键设计决策（再次标注）**：
   - option 机制用 **eino 包自有**的 impl-specific 类型（`outputSchemaOptions` + `WrapImplSpecificOptFn`），而非照搬 `openai.WithReasoningEffort`——因为后者依赖 acl 包未导出类型，自定义适配器无法解码（见 Architecture 的研究结论）。
   - **Anthropic 字段**用 `output_config.format`（claude-api skill 2026-06-24 所载当前字段），而非 brief 字面的顶层 `output_schema`。切换是 Task 2 末"备选"注释的约 2 行改动。**执行前请 lead 确认目标端点接受哪个字段。**
   - Task 5（传输接线）依赖 A12-core 合并；Task 1-4 独立可交付，已覆盖全部验收标准。

## 执行交接

Plan complete and saved to `docs/superpowers/plans/2026-07-18-m1-lane1c-a12-providers-schema-injection.md`。两种执行方式：

1. **Subagent-Driven（推荐）** — 每个任务派一个新 subagent，任务间 review。建议 Task 1-4 先行（独立可交付、覆盖全部验收），Task 5 等 A12-core 合并后再做。
2. **Inline Execution** — 本会话内按 executing-plans 批次执行 + checkpoint。
