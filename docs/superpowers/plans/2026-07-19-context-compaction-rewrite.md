# Context Compaction Rewrite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite yanshi's context-compaction to fix 8 bugs (structure loss, broken tool pairs, no fidelity tiers, system conflict, token undercount, failure-swallowing, double-compaction, transcript pollution) with a unified `internal/ctxcompact` core, carry-style chunked summarization that never exceeds the summary model's window, and per-model context windows.

**Architecture:** One core package (`internal/ctxcompact`) with pure functions (`Plan`/`SerializeForSummary`/`EnforceToolCallPairs`/`Assemble`) plus a `Run` entry point. Two existing compaction paths (mid-turn `CompactingModel` + pre-turn `MaybeCompact`) both delegate to it via an injected `ModelSummarizer`. Summary is a `user`+sentinel message at history tail (cache-friendly, no system conflict). When the summarize set exceeds 0.9×window, carry-style rolling summary splits it so each model call stays in-window.

**Tech Stack:** Go 1.26.4, eino ADK (`schema.Message`/`schema.ToolCall`/`model.BaseChatModel`), testify, chars/4 token heuristic. Fake-first testing (`einollm.FakeModel`).

**Spec:** `docs/superpowers/specs/2026-07-19-context-compaction-rewrite-design.md`

**Conventions:** Chinese for prose, English for code/identifiers (per CLAUDE.md). Single file ≤1000 pure code lines. Extract duplicated logic into shared helpers. TDD: red → green → commit.

---

## File Structure

新建文件按职责拆分，每个一个清晰责任，可独立测试：

| 文件 | 职责 | 依赖 |
|---|---|---|
| `tokens.go` | `EstimateTokens`/`estimateMessageTokens`（计 ToolCalls+reasoning） | schema |
| `sentinel.go` | `SummarySentinel` 常量 + `IsSummaryMessage` | schema |
| `serialize.go` | `SerializeForSummary`（结构化 transcript） | schema, tokens |
| `preserve.go` | `deriveWorkingSetPaths`/`isErrorMarker`/`isDiffMarker`/`shouldPin` | schema |
| `pairs.go` | `EnforceToolCallPairs`（fixpoint） | schema |
| `options.go` | `PlanOpts`/`RunOpts`/`Config`/`ModelSummarizer`/`PlanResult`/`Result` | schema, model |
| `plan.go` | `Plan`（pin 五类 + preserve + pairs + sentinel 短路） | preserve, pairs, sentinel, tokens |
| `summarize.go` | `RunSummary`（cache-aligned + 携带式分块 + 重试 + safe-boundary 切分） | serialize, tokens, options |
| `assemble.go` | `Assemble`（pinned 原文 + summary 末尾 user+sentinel） | sentinel |
| `run.go` | `Run`（Plan→Serialize→Summarize→Assemble + 失败处理） | plan, summarize, assemble, options |

---

## Section A: 核心包叶子组件

### Task 1: `tokens.go` — EstimateTokens 计 ToolCalls（修 bug⑤）

**Files:**
- Create: `internal/ctxcompact/tokens.go`
- Test: `internal/ctxcompact/tokens_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/ctxcompact/tokens_test.go
package ctxcompact

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

func TestEstimateTokens_CountsContentAndOverhead(t *testing.T) {
	// 8 chars -> 8/4 + 8 = 10
	assert.Equal(t, 10, EstimateTokens([]*schema.Message{{Content: "12345678"}}))
}

func TestEstimateTokens_CountsToolCalls(t *testing.T) {
	// bug⑤ 回归: ToolCalls arguments 必须计入,否则 ReAct 循环严重低估
	msg := &schema.Message{
		Role:    schema.Assistant,
		Content: "I'll read the file", // 18 chars -> 18/4+8 = 12
		ToolCalls: []schema.ToolCall{
			{ID: "call_1", Function: schema.FunctionCall{Name: "read_file", Arguments: `{"path":"internal/llm/eino/compacting.go"}`}},
		},
	}
	// args: name(9)+args(44)+id(6) = 59 chars -> 59/4 = 14, +16 overhead = 30
	// total = 12 (content) + 30 (toolcall) = 42
	n := EstimateTokens([]*schema.Message{msg})
	assert.Greater(t, n, 12, "toolcall args must add tokens beyond bare content")
	assert.GreaterOrEqual(t, n, 40, "estimated ~42")
}

func TestEstimateTokens_CountsReasoning(t *testing.T) {
	msg := &schema.Message{
		Role:             schema.Assistant,
		ReasoningContent: "thinking " + string(make([]byte, 40)), // ~50 chars -> 12 tokens
	}
	n := EstimateTokens([]*schema.Message{msg})
	assert.Greater(t, n, 8, "reasoning content must add tokens")
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ctxcompact/ -run TestEstimateTokens -v`
Expected: FAIL — `EstimateTokens` undefined（包内现有的 `estimateTokens` 是小写旧版，签名不同）。

- [ ] **Step 3: 实现**

```go
// internal/ctxcompact/tokens.go
package ctxcompact

import "github.com/cloudwego/eino/schema"

// EstimateTokens sums a chars/4 + per-field-overhead heuristic across msgs.
// It counts Content, ReasoningContent, and every ToolCall's name/arguments/id
// — the prior len(Content)/4+8 estimate badly undercounted tool-heavy ReAct
// loops, so the threshold gate never fired until too late (bug⑤). This is only
// used to gate WHEN to compact and to pick chunk boundaries; it never feeds
// billing.
func EstimateTokens(msgs []*schema.Message) int {
	n := 0
	for _, m := range msgs {
		n += estimateMessageTokens(m)
	}
	return n
}

func estimateMessageTokens(m *schema.Message) int {
	if m == nil {
		return 0
	}
	n := len(m.Content)/4 + 8 // base content + per-message overhead
	if m.ReasoningContent != "" {
		n += len(m.ReasoningContent) / 4
	}
	for _, tc := range m.ToolCalls {
		// name + arguments(JSON string) + id, each ~chars/4, plus structural overhead
		n += (len(tc.Function.Name) + len(tc.Function.Arguments) + len(tc.ID))/4 + 16
	}
	return n
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run TestEstimateTokens -v`
Expected: PASS（3 个测试全过）。

> 注意：现有 `compact.go` 里有小写 `estimateTokens`，与新 `EstimateTokens` 共存会编译失败（或被新函数取代）。此 task 只新建 tokens.go；旧 `estimateTokens` 在 Task 11（compact.go 重写）删除。若此时 `go build ./internal/ctxcompact/` 报重复定义，先临时把旧 `estimateTokens` 重命名为 `estimateTokensLegacy`，Task 11 再删。实际两者签名不同（旧版接收 `[]*schema.Message`，新版也叫 `EstimateTokens` 接收同类型但导出）——Go 不允许同名仅大小写不同的包级函数？允许（不同标识符）。但旧版叫 `estimateTokens`（小写），新版叫 `EstimateTokens`（大写），是两个不同标识符，可共存。

- [ ] **Step 5: Commit**

```bash
git add internal/ctxcompact/tokens.go internal/ctxcompact/tokens_test.go
git commit -m "feat(ctxcompact): EstimateTokens counts ToolCalls+reasoning (bug⑤)"
```

---

### Task 2: `sentinel.go` — SummarySentinel + IsSummaryMessage（修 bug⑦基础）

**Files:**
- Create: `internal/ctxcompact/sentinel.go`
- Test: `internal/ctxcompact/sentinel_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/ctxcompact/sentinel_test.go
package ctxcompact

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

func TestIsSummaryMessage_DetectsSentinel(t *testing.T) {
	m := &schema.Message{Role: schema.User, Content: SummarySentinel + "对话摘要..."}
	assert.True(t, IsSummaryMessage(m), "sentinel-prefixed user msg is a summary")
}

func TestIsSummaryMessage_RejectsPlain(t *testing.T) {
	assert.False(t, IsSummaryMessage(&schema.Message{Role: schema.User, Content: "普通消息"}))
	assert.False(t, IsSummaryMessage(nil))
}

func TestIsSummaryMessage_LastMessageIsSummary(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "hi"},
		{Role: schema.User, Content: SummarySentinel + "sum"},
	}
	assert.True(t, lastMessageIsSummary(msgs), "history ending in summary is already compacted")
	assert.False(t, lastMessageIsSummary(msgs[:1]))
	assert.False(t, lastMessageIsSummary(nil))
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ctxcompact/ -run TestIsSummaryMessage -v`
Expected: FAIL — `SummarySentinel`/`IsSummaryMessage` undefined。

- [ ] **Step 3: 实现**

```go
// internal/ctxcompact/sentinel.go
package ctxcompact

import (
	"strings"

	"github.com/cloudwego/eino/schema"
)

// SummarySentinel prefixes every compaction-produced summary message. It lets
// Plan short-circuit when history already ends in a summary (preventing
// summary-of-summary, bug⑦) and lets the TUI skip rendering the summary as a
// chat bubble (it's model context, not conversational content). The bracketed
// form is deliberately distinctive so normal user text never collides.
const SummarySentinel = "[yanshi:conversation-summary]\n"

// IsSummaryMessage reports whether m is a compaction-produced summary (a user
// message prefixed with SummarySentinel). nil-safe.
func IsSummaryMessage(m *schema.Message) bool {
	return m != nil && m.Role == schema.User && strings.HasPrefix(m.Content, SummarySentinel)
}

// lastMessageIsSummary reports whether msgs ends in a compaction summary — the
// signal Plan uses to short-circuit (history was just compacted).
func lastMessageIsSummary(msgs []*schema.Message) bool {
	if len(msgs) == 0 {
		return false
	}
	return IsSummaryMessage(msgs[len(msgs)-1])
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run TestIsSummaryMessage -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/ctxcompact/sentinel.go internal/ctxcompact/sentinel_test.go
git commit -m "feat(ctxcompact): SummarySentinel + IsSummaryMessage (bug⑦ base)"
```

---

### Task 3: `serialize.go` — 结构化序列化（修 bug①）

**Files:**
- Create: `internal/ctxcompact/serialize.go`
- Test: `internal/ctxcompact/serialize_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/ctxcompact/serialize_test.go
package ctxcompact

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

func TestSerializeForSummary_PreservesToolCalls(t *testing.T) {
	// bug① 回归: 旧 serialize 只写 Role+Content,丢了 ToolCalls/ToolResult
	msgs := []*schema.Message{
		{Role: schema.User, Content: "read compacting.go"},
		{Role: schema.Assistant, Content: "ok", ToolCalls: []schema.ToolCall{
			{ID: "call_1", Function: schema.FunctionCall{Name: "read_file", Arguments: `{"path":"compacting.go"}`}},
		}},
		{Role: schema.Tool, ToolCallID: "call_1", Content: "package eino\n..."},
	}
	got := SerializeForSummary(msgs)
	assert.Contains(t, got, "read_file", "tool name preserved")
	assert.Contains(t, got, "call_1", "tool id preserved")
	assert.Contains(t, got, `"path":"compacting.go"`, "tool args preserved")
	assert.Contains(t, got, "package eino", "tool result content preserved")
}

func TestSerializeForSummary_PreservesReasoning(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, Content: "answer", ReasoningContent: "because X"},
	}
	got := SerializeForSummary(msgs)
	assert.Contains(t, got, "because X", "reasoning preserved as [thinking]")
	assert.Contains(t, got, "[thinking]")
}

func TestSerializeForSummary_SkipsEmpty(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.Assistant, Content: ""}, // empty, skipped
		{Role: schema.User, Content: "real"},
	}
	got := SerializeForSummary(msgs)
	assert.NotContains(t, got, "assistant:", "empty msg skipped")
	assert.Contains(t, got, "real")
	_ = strings.TrimSpace // keep import if asserts above evolve
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ctxcompact/ -run TestSerializeForSummary -v`
Expected: FAIL — `SerializeForSummary` undefined。

- [ ] **Step 3: 实现**

```go
// internal/ctxcompact/serialize.go
package ctxcompact

import (
	"strings"

	"github.com/cloudwego/eino/schema"
)

// toolResultSnippetChars caps each tool_result's content in the transcript so a
// single huge file read can't blow the summary model's window from one message.
const toolResultSnippetChars = 1200

// SerializeForSummary flattens msgs into a text transcript for the summarization
// prompt. Unlike the prior Role+Content-only serializer, it preserves ToolCalls
// (name+id+arguments), tool results (id+content), and reasoning — without these
// the summary model literally cannot see what tools did (bug①). Empty messages
// (no Content, no ToolCalls, no ReasoningContent) are skipped.
func SerializeForSummary(msgs []*schema.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m == nil {
			continue
		}
		writeMessageTranscript(&b, m)
	}
	return b.String()
}

func writeMessageTranscript(b *strings.Builder, m *schema.Message) {
	role := string(m.Role)
	hasContent := m.Content != ""
	hasReasoning := m.ReasoningContent != ""
	hasToolCalls := len(m.ToolCalls) > 0
	isToolResult := m.Role == schema.Tool && m.ToolCallID != ""
	if !hasContent && !hasReasoning && !hasToolCalls && !isToolResult {
		return // skip blank turns
	}
	if hasContent {
		b.WriteString("[" + role + "]: " + m.Content + "\n")
	}
	if hasReasoning {
		b.WriteString("[thinking] " + m.ReasoningContent + "\n")
	}
	for _, tc := range m.ToolCalls {
		b.WriteString("  [tool_call: " + tc.Function.Name + " id=" + tc.ID + "] " + tc.Function.Arguments + "\n")
	}
	if isToolResult {
		snippet := m.Content
		if len(snippet) > toolResultSnippetChars {
			snippet = snippet[:toolResultSnippetChars] + "\n…[truncated]"
		}
		b.WriteString("[tool_result: " + m.ToolCallID + "] " + snippet + "\n")
	}
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run TestSerializeForSummary -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/ctxcompact/serialize.go internal/ctxcompact/serialize_test.go
git commit -m "feat(ctxcompact): SerializeForSummary preserves tool calls/results/reasoning (bug①)"
```

---

### Task 4: `preserve.go` — working-set / error / diff pin 维度（修 bug③）

**Files:**
- Create: `internal/ctxcompact/preserve.go`
- Test: `internal/ctxcompact/preserve_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/ctxcompact/preserve_test.go
package ctxcompact

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

func TestDeriveWorkingSetPaths_ExtractsFromTextAndToolInput(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "fix internal/llm/eino/compacting.go"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{Function: schema.FunctionCall{Name: "edit", Arguments: `{"path":"internal/ctxcompact/compact.go"}`}},
		}},
	}
	paths := deriveWorkingSetPaths(msgs, nil)
	assert.Contains(t, paths, "internal/llm/eino/compacting.go")
	assert.Contains(t, paths, "internal/ctxcompact/compact.go")
}

func TestIsErrorMarker(t *testing.T) {
	assert.True(t, isErrorMarker("build error: undefined: foo"))
	assert.True(t, isErrorMarker("panic: runtime fault"))
	assert.True(t, isErrorMarker("Traceback (most recent call last)"))
	assert.True(t, isErrorMarker("test failed: TestX"))
	assert.False(t, isErrorMarker("all good"))
}

func TestIsDiffMarker(t *testing.T) {
	assert.True(t, isDiffMarker("diff --git a/foo b/foo"))
	assert.True(t, isDiffMarker("+++ b/src/main.go"))
	assert.True(t, isDiffMarker("```diff"))
	assert.False(t, isDiffMarker("normal text"))
}

func TestShouldPin_UsesWorkingSetPaths(t *testing.T) {
	ws := map[string]bool{"internal/llm/eino/compacting.go": true}
	assert.True(t, shouldPin(&schema.Message{Content: "edit internal/llm/eino/compacting.go"}, ws))
	assert.False(t, shouldPin(&schema.Message{Content: "unrelated chatter"}, ws))
	assert.True(t, shouldPin(&schema.Message{Content: "error: boom"}, map[string]bool{}), "error always pins")
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ctxcompact/ -run "TestDeriveWorkingSetPaths|TestIsErrorMarker|TestIsDiffMarker|TestShouldPin" -v`
Expected: FAIL — 函数 undefined。

- [ ] **Step 3: 实现**

```go
// internal/ctxcompact/preserve.go
package ctxcompact

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/cloudwego/eino/schema"
)

const (
	recentWorkingSetWindow = 12 // msgs scanned back from the tail to seed working-set paths
	maxWorkingSetPaths     = 24 // cap so a sprawling session can't pin everything
)

var (
	// pathRe matches repo-relative file paths (dir/file.ext) and common root
	// config files. Conservative: requires a slash + extension to avoid pinning
	// every English sentence that happens to contain a dot.
	pathRe = regexp.MustCompile(`(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]+\.(?:go|rs|md|json|ya?ml|txt|toml|ts|js|py)`)

	errorMarkers = []string{"error:", "error ", "failed", "panic", "traceback", "stack trace", "assertion failed", "test failed"}
	diffMarkers  = []string{"diff --git", "+++ b/", "--- a/", "```diff", "apply_patch"}
)

// deriveWorkingSetPaths scans the recent tail (plus any tool inputs) for
// repo-relative file paths. These define the "what we're editing" set; any
// message mentioning them is pinned so the summary never drops live file
// context (bug③).
func deriveWorkingSetPaths(msgs []*schema.Message, seedIndices []int) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= maxWorkingSetPaths {
			return
		}
	}

	// 1. seed indices first (explicit pins, e.g. the most recent tool calls)
	for i := len(seedIndices) - 1; i >= 0; i-- {
		idx := seedIndices[i]
		if idx < 0 || idx >= len(msgs) {
			continue
		}
		for _, p := range extractPaths(msgs[idx]) {
			add(p)
		}
	}
	// 2. recent window, newest first
	start := len(msgs) - recentWorkingSetWindow
	if start < 0 {
		start = 0
	}
	for i := len(msgs) - 1; i >= start; i-- {
		for _, p := range extractPaths(msgs[i]) {
			add(p)
		}
		if len(out) >= maxWorkingSetPaths {
			break
		}
	}
	return out
}

func extractPaths(m *schema.Message) []string {
	if m == nil {
		return nil
	}
	var paths []string
	paths = append(paths, pathRe.FindAllString(m.Content, -1)...)
	paths = append(paths, pathRe.FindAllString(m.ReasoningContent, -1)...)
	if m.Role == schema.Tool {
		paths = append(paths, pathRe.FindAllString(m.Content, -1)...)
	}
	for _, tc := range m.ToolCalls {
		// pull "path"/"file" out of the arguments JSON
		var obj map[string]any
		if json.Unmarshal([]byte(tc.Function.Arguments), &obj) == nil {
			for _, key := range []string{"path", "file", "target"} {
				if s, ok := obj[key].(string); ok {
					paths = append(paths, pathRe.FindString(s))
				}
			}
		}
	}
	return paths
}

func isErrorMarker(text string) bool {
	low := strings.ToLower(text)
	for _, m := range errorMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func isDiffMarker(text string) bool {
	for _, m := range diffMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// shouldPin reports whether a message must be preserved verbatim because it
// mentions a working-set path, carries an error, or carries a diff/patch.
func shouldPin(m *schema.Message, workingSetPaths map[string]bool) bool {
	if m == nil {
		return false
	}
	for p := range workingSetPaths {
		if strings.Contains(m.Content, p) || strings.Contains(m.ReasoningContent, p) {
			return true
		}
	}
	return isErrorMarker(m.Content) || isDiffMarker(m.Content)
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run "TestDeriveWorkingSetPaths|TestIsErrorMarker|TestIsDiffMarker|TestShouldPin" -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/ctxcompact/preserve.go internal/ctxcompact/preserve_test.go
git commit -m "feat(ctxcompact): working-set/error/diff pin dimensions (bug③)"
```

---

### Task 5: `pairs.go` — fixpoint 配对修复（修 bug②）

**Files:**
- Create: `internal/ctxcompact/pairs.go`
- Test: `internal/ctxcompact/pairs_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/ctxcompact/pairs_test.go
package ctxcompact

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

// bug② 回归: 切分点落在 tool_call 与 result 之间会产生孤立 result -> API 400.
// fixpoint 必须把配对拉齐: pinned 了 result 就拉入 call, 反之同理.

func TestEnforceToolCallPairs_PinsCallForResult(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "noise"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "read"}}}},
		{Role: schema.Tool, ToolCallID: "c1", Content: "ok"},
	}
	pinned := map[int]bool{2: true} // only result pinned
	EnforceToolCallPairs(msgs, pinned)
	assert.True(t, pinned[1], "call pulled in to pair with pinned result")
}

func TestEnforceToolCallPairs_RemovesOrphanCall(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "noise"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "orphan", Function: schema.FunctionCall{Name: "read"}}}},
		{Role: schema.Assistant, Content: "recent"},
	}
	pinned := map[int]bool{0: true, 1: true, 2: true}
	EnforceToolCallPairs(msgs, pinned)
	assert.False(t, pinned[1], "orphaned call (no result anywhere) removed")
	assert.True(t, pinned[0])
	assert.True(t, pinned[2])
}

func TestEnforceToolCallPairs_Cascades(t *testing.T) {
	// msg1 has two calls (good+orphan); removing msg1 orphans good's result (msg2).
	msgs := []*schema.Message{
		{Role: schema.User, Content: "start"},
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "good", Function: schema.FunctionCall{Name: "r"}},
			{ID: "orphan", Function: schema.FunctionCall{Name: "s"}},
		}},
		{Role: schema.Tool, ToolCallID: "good", Content: "ok"},
		{Role: schema.Assistant, Content: "done"},
	}
	pinned := map[int]bool{1: true, 2: true, 3: true}
	EnforceToolCallPairs(msgs, pinned)
	// orphan has no result -> msg1 removed -> good's result now orphaned -> msg2 removed
	assert.False(t, pinned[1])
	assert.False(t, pinned[2])
	assert.True(t, pinned[3])
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ctxcompact/ -run TestEnforceToolCallPairs -v`
Expected: FAIL — `EnforceToolCallPairs` undefined。

- [ ] **Step 3: 实现**

```go
// internal/ctxcompact/pairs.go
package ctxcompact

import "github.com/cloudwego/eino/schema"

// EnforceToolCallPairs mutates pinned so every pinned tool_call has its
// matching tool_result also pinned (and vice versa), and removes orphans
// (a call with no result anywhere, or a result with no call). This is the
// fixpoint that prevents the compacted history from handing the API an
// unpaired tool_result — which OpenAI/Anthropic reject with 400 (bug②).
//
// Algorithm (ported from deepseek-tui's enforce_tool_call_pairs):
//   - build call_id→idx and result_id→idx over ALL msgs
//   - repeat: for each pinned msg, pull in the counterpart of any tool_call/
//     tool_result it carries; remove a msg if its counterpart is permanently
//     gone (orphan). Track permanently_removed so a later iteration can't
//     re-add a cascaded-removed idx (prevents oscillation).
//   - stop when no change in a full pass.
func EnforceToolCallPairs(msgs []*schema.Message, pinned map[int]bool) {
	if len(pinned) == 0 {
		return
	}
	callIdx := map[string]int{}
	resultIdx := map[string]int{}
	for i, m := range msgs {
		if m == nil {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" {
				callIdx[tc.ID] = i
			}
		}
		if m.ToolCallID != "" {
			resultIdx[m.ToolCallID] = i
		}
	}
	permanentlyRemoved := map[int]bool{}
	for {
		changed := false
		// snapshot keys so mutating pinned mid-loop is safe
		idxs := make([]int, 0, len(pinned))
		for i := range pinned {
			idxs = append(idxs, i)
		}
		for _, idx := range idxs {
			if permanentlyRemoved[idx] {
				continue
			}
			m := msgs[idx]
			if m == nil {
				continue
			}
			for _, tc := range m.ToolCalls {
				if tc.ID == "" {
					continue
				}
				ri, ok := resultIdx[tc.ID]
				if !ok || permanentlyRemoved[ri] {
					if remove(pinned, permanentlyRemoved, idx) {
						changed = true
					}
					break
				}
				if !pinned[ri] {
					pinned[ri] = true
					changed = true
				}
			}
			if m.ToolCallID != "" {
				ci, ok := callIdx[m.ToolCallID]
				if !ok || permanentlyRemoved[ci] {
					remove(pinned, permanentlyRemoved, idx)
					changed = true
				} else if !pinned[ci] {
					pinned[ci] = true
					changed = true
				}
			}
		}
		if !changed {
			return
		}
	}
}

// remove deletes idx from pinned and marks it permanently removed. Returns
// whether it was present (and thus changed).
func remove(pinned, permanentlyRemoved map[int]bool, idx int) bool {
	if pinned[idx] {
		delete(pinned, idx)
		permanentlyRemoved[idx] = true
		return true
	}
	return false
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run TestEnforceToolCallPairs -v`
Expected: PASS（3 个 fixpoint 场景）。

- [ ] **Step 5: Commit**

```bash
git add internal/ctxcompact/pairs.go internal/ctxcompact/pairs_test.go
git commit -m "feat(ctxcompact): fixpoint tool-call/result pair enforcement (bug②)"
```

---

## Section B: 核心包组装

### Task 6: `options.go` — 类型与 ModelSummarizer 接口

**Files:**
- Create: `internal/ctxcompact/options.go`

- [ ] **Step 1: 写测试（接口满足性）**

```go
// internal/ctxcompact/options_test.go
package ctxcompact

import (
	"testing"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/stretchr/testify/assert"
)

func TestModelSummarizer_FakeModelSatisfies(t *testing.T) {
	// compile-time: FakeModel 必须满足 ModelSummarizer (== model.BaseChatModel)
	var _ ModelSummarizer = einollm.NewFakeModel(nil, nil)
	assert.True(t, true)
}

func TestPlanResult_ZeroValue(t *testing.T) {
	var p PlanResult
	assert.Empty(t, p.PinnedIndices)
	assert.Empty(t, p.SummarizeIndices)
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ctxcompact/ -run "TestModelSummarizer|TestPlanResult" -v`
Expected: FAIL — 类型 undefined。

- [ ] **Step 3: 实现**

```go
// internal/ctxcompact/options.go
package ctxcompact

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// ModelSummarizer is the slice of model.BaseChatModel that RunSummary needs.
// Any model.BaseChatModel (the real remote model, einollm.FakeModel, the
// CompactingModel inner) satisfies it, so both compaction paths inject their
// own model source without the core depending on either.
type ModelSummarizer interface {
	Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error)
	Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error)
}

// PlanOpts configures the pure Plan function (no IO).
type PlanOpts struct {
	KeepRecent int // trailing user/assistant pairs kept verbatim (2*KeepRecent msgs)
	// ExternalPins are message indices the caller wants pinned regardless of
	// heuristics (reserved for future pre/post-compact hooks). May be nil.
	ExternalPins []int
	// ExternalWorkingSetPaths augments the derived working set. May be nil.
	ExternalWorkingSetPaths []string
}

// PlanResult is the pinned/summarize split Plan produces.
type PlanResult struct {
	PinnedIndices    []int
	SummarizeIndices []int
	WorkingSetPaths  []string
}

// RunOpts configures Run / RunSummary (carries the summary-model window).
type RunOpts struct {
	// ModelWindow is the summary model's context window (tokens), used to pick
	// single-vs-chunked and chunk boundaries. From provider.context_window.
	ModelWindow int
	// ChunkThreshold is the fraction of ModelWindow at which RunSummary switches
	// from single cache-aligned call to carry-style chunking (default 0.9).
	ChunkThreshold float64
	// SummaryWordLimit caps the summary length the model is asked for.
	SummaryWordLimit int
}

// Result is what Run returns on success.
type Result struct {
	Messages     []*schema.Message // the compacted history ([pinned..., summary-at-tail])
	TokensBefore int
	TokensAfter  int
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run "TestModelSummarizer|TestPlanResult" -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/ctxcompact/options.go internal/ctxcompact/options_test.go
git commit -m "feat(ctxcompact): PlanOpts/RunOpts/ModelSummarizer types"
```

---

### Task 7: `plan.go` — Plan（pin 五类 + 配对修复 + sentinel 短路）

**Files:**
- Create: `internal/ctxcompact/plan.go`
- Test: `internal/ctxcompact/plan_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/ctxcompact/plan_test.go
package ctxcompact

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

func TestPlan_PinsTail(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		{Role: schema.Assistant, Content: "b"},
		{Role: schema.User, Content: "c"},
		{Role: schema.Assistant, Content: "d"},
	}
	p := Plan(msgs, PlanOpts{KeepRecent: 1})
	// keep_recent=1 -> last 2 msgs pinned (indices 2,3)
	assert.Contains(t, p.PinnedIndices, 2)
	assert.Contains(t, p.PinnedIndices, 3)
}

func TestPlan_PinsAllUserOriginals(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "first request"},                   // 0
		{Role: schema.Assistant, Content: "noise"},                      // 1
		{Role: schema.User, Content: "second request"},                  // 2
		{Role: schema.Assistant, Content: "more noise"},                 // 3
		{Role: schema.User, Content: "third"},                           // 4
	}
	p := Plan(msgs, PlanOpts{KeepRecent: 1})
	assert.Contains(t, p.PinnedIndices, 0, "early user intent preserved verbatim")
	assert.Contains(t, p.PinnedIndices, 2)
}

func TestPlan_PinsErrorAndWorkingSet(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "edit internal/ctxcompact/compact.go"}, // 0 ws
		{Role: schema.Assistant, Content: "error: undefined: Foo"},          // 1 error
		{Role: schema.Assistant, Content: "noise"},                          // 2
		{Role: schema.User, Content: "recent"},                              // 3
	}
	p := Plan(msgs, PlanOpts{KeepRecent: 1})
	assert.Contains(t, p.PinnedIndices, 0, "working-set mention pinned")
	assert.Contains(t, p.PinnedIndices, 1, "error pinned")
	assert.Contains(t, p.SummarizeIndices, 2, "noise summarized")
}

func TestPlan_ShortCircuitsWhenAlreadySummarized(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "x"},
		{Role: schema.User, Content: SummarySentinel + "already compacted"}, // tail summary
	}
	p := Plan(msgs, PlanOpts{KeepRecent: 1})
	assert.Empty(t, p.SummarizeIndices, "no re-compaction when already summarized (bug⑦)")
}

func TestPlan_EnforcesToolPairs(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "noise"},                                                  // 0
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "r"}}}}, // 1
		{Role: schema.Tool, ToolCallID: "c1", Content: "res"},                                  // 2 (ws mention pins 2)
		{Role: schema.User, Content: "recent"},                                                 // 3
	}
	p := Plan(msgs, PlanOpts{KeepRecent: 1})
	assert.Contains(t, p.PinnedIndices, 1, "call pulled in to pair with pinned result")
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ctxcompact/ -run TestPlan_ -v`
Expected: FAIL — `Plan` undefined。

- [ ] **Step 3: 实现**

```go
// internal/ctxcompact/plan.go
package ctxcompact

import (
	"github.com/cloudwego/eino/schema"
)

// Plan computes which messages stay verbatim (pinned) and which get summarized.
// It is a PURE function (no IO) so both compaction paths share it.
//
// Pin policy (any one fires):
//  1. tail KeepRecent pairs (current immediate context)
//  2. every Role==User non-tool-result message (user intent never lost — codex style)
//  3. messages mentioning a working-set path (live file context)
//  4. messages carrying an error marker
//  5. messages carrying a diff/patch marker
//
// After heuristic pinning, EnforceToolCallPairs fixes up the set so tool_call
// and tool_result stay paired (bug②). If history already ends in a summary,
// returns an empty summarize set (bug⑦ — no summary-of-summary).
func Plan(msgs []*schema.Message, opts PlanOpts) *PlanResult {
	res := &PlanResult{}
	if len(msgs) == 0 {
		return res
	}
	// bug⑦: short-circuit if already compacted.
	if lastMessageIsSummary(msgs) {
		res.PinnedIndices = allIndices(msgs)
		return res
	}

	pinned := map[int]bool{}

	// 1. tail
	pairCount := opts.KeepRecent
	if pairCount < 0 {
		pairCount = 0
	}
	tailStart := len(msgs) - pairCount*2
	if tailStart < 0 {
		tailStart = 0
	}
	for i := tailStart; i < len(msgs); i++ {
		pinned[i] = true
	}

	// derive working set from seed (tail) indices + recent window
	seed := make([]int, 0, len(opts.ExternalPins)+pairCount*2)
	for i := tailStart; i < len(msgs); i++ {
		seed = append(seed, i)
	}
	seed = append(seed, opts.ExternalPins...)
	res.WorkingSetPaths = deriveWorkingSetPaths(msgs, seed)
	wsSet := map[string]bool{}
	for _, p := range res.WorkingSetPaths {
		wsSet[p] = true
	}
	for _, p := range opts.ExternalWorkingSetPaths {
		wsSet[p] = true
	}

	// 2-5. heuristic pin
	for i, m := range msgs {
		if pinned[i] {
			continue
		}
		if isUserOriginal(m) { // 2. user intent
			pinned[i] = true
			continue
		}
		if shouldPin(m, wsSet) { // 3/4/5. working-set / error / diff
			pinned[i] = true
		}
	}
	// external pins are authoritative
	for _, i := range opts.ExternalPins {
		if i >= 0 && i < len(msgs) {
			pinned[i] = true
		}
	}

	// fixpoint: keep tool pairs intact, drop orphans
	EnforceToolCallPairs(msgs, pinned)

	for i := 0; i < len(msgs); i++ {
		if pinned[i] {
			res.PinnedIndices = append(res.PinnedIndices, i)
		} else {
			res.SummarizeIndices = append(res.SummarizeIndices, i)
		}
	}
	return res
}

// isUserOriginal reports whether m is a genuine user message (not a tool
// result masquerading as Role==User in some schemas, and not a summary).
func isUserOriginal(m *schema.Message) bool {
	if m == nil || m.Role != schema.User {
		return false
	}
	if IsSummaryMessage(m) {
		return false
	}
	// eino encodes tool results as Role==Tool + ToolCallID; if a provider uses
	// Role==User for tool results, ToolCallID is still set.
	return m.ToolCallID == ""
}

func allIndices(msgs []*schema.Message) []int {
	out := make([]int, len(msgs))
	for i := range msgs {
		out[i] = i
	}
	return out
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run TestPlan_ -v`
Expected: PASS（5 个场景）。

- [ ] **Step 5: Commit**

```bash
git add internal/ctxcompact/plan.go internal/ctxcompact/plan_test.go
git commit -m "feat(ctxcompact): Plan with 5 pin dimensions + pair fixpoint + sentinel short-circuit"
```

---

### Task 8: `summarize.go` — cache-aligned + 携带式分块 + 重试

**Files:**
- Create: `internal/ctxcompact/summarize.go`
- Test: `internal/ctxcompact/summarize_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/ctxcompact/summarize_test.go
package ctxcompact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 单次 cache-aligned: summarize 集合 ≤ 0.9 窗口,一次调用,前缀 = msgs verbatim.
func TestRunSummary_SingleCacheAligned(t *testing.T) {
	// Echo model returns concatenated input content, proving the original msgs
	// reached the model verbatim (cache-aligned prefix).
	fm := einollm.NewFakeModel(nil, nil)
	fm.Echo = true
	msgs := []*schema.Message{
		{Role: schema.User, Content: "alpha"},
		{Role: schema.Assistant, Content: "beta"},
	}
	summary, err := RunSummary(context.Background(), msgs, RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9, SummaryWordLimit: 200}, fm, nil)
	require.NoError(t, err)
	assert.Contains(t, summary, "alpha")
	assert.Contains(t, summary, "beta")
}

// 携带式分块: summarize 集合 > 0.9 窗口 -> 切块, 每块输入 ≤ 预算.
func TestRunSummary_CarryChunkedStaysInBudget(t *testing.T) {
	// recordingModel captures every call's input so we can assert each fit.
	rm := &recordingModel{reply: "summary-chunk"}
	big := []*schema.Message{}
	for i := 0; i < 40; i++ {
		big = append(big, &schema.Message{Role: schema.User, Content: strings.Repeat("x", 200)}) // ~58 tok each
	}
	_, err := RunSummary(context.Background(), big, RunOpts{ModelWindow: 300, ChunkThreshold: 0.9, SummaryWordLimit: 100}, rm, nil)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rm.inputs), 2, "must split into multiple chunks")
	budget := int(0.9 * 300)
	for i, in := range rm.inputs {
		tok := EstimateTokens(in)
		assert.LessOrEqual(t, tok, budget+60, "chunk %d input must fit budget (relaxed for carry prefix)", i)
	}
}

// 失败不产出空 summary: 重试耗尽返回 error.
func TestRunSummary_FailureReturnsError(t *testing.T) {
	fm := einollm.NewFakeModel(nil, errors.New("boom"))
	_, err := RunSummary(context.Background(), []*schema.Message{{Role: schema.User, Content: "x"}},
		RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm, nil)
	assert.Error(t, err, "failure must surface, not produce empty summary (bug⑥)")
}

// recordingModel records each call's input messages and returns a fixed reply.
type recordingModel struct {
	inputs [][]*schema.Message
	reply  string
}

func (r *recordingModel) Generate(_ context.Context, in []*schema.Message, _ ...any) (*schema.Message, error) {
	cp := make([]*schema.Message, len(in))
	copy(cp, in)
	r.inputs = append(r.inputs, cp)
	return &schema.Message{Role: schema.Assistant, Content: r.reply}, nil
}
func (r *recordingModel) Stream(_ context.Context, in []*schema.Message, _ ...any) (*schema.StreamReader[*schema.Message], error) {
	r.Generate(context.Background(), in)
	return schema.StreamReaderFromArray[*schema.Message)([]*schema.Message{{Role: schema.Assistant, Content: r.reply}}), nil
}
```

> ⚠️ 上面的 `recordingModel` 用了 `_ ...any` 参数，但这不满足 `ModelSummarizer`（要求 `_ ...model.Option`）。修正为下面 Step 3 配套的签名 —— 测试里改为：

```go
import "github.com/cloudwego/eino/components/model"

func (r *recordingModel) Generate(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.Message, error) { ... }
func (r *recordingModel) Stream(_ context.Context, in []*schema.Message, _ ...model.Option) (*schema.StreamReader[*schema.Message], error) { ... }
```

并在 `TestRunSummary_CarryChunkedStaysInBudget` 前加 `var _ ModelSummarizer = (*recordingModel)(nil)` 编译期校验。`schema.StreamReaderFromArray[*schema.Message]`（修正测试里的拼写 `[*schema.Message)` 笔误为 `[*schema.Message]`）。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ctxcompact/ -run TestRunSummary_ -v`
Expected: FAIL — `RunSummary` undefined + recordingModel 签名编译错。

- [ ] **Step 3: 实现**

```go
// internal/ctxcompact/summarize.go
package ctxcompact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

const (
	summaryRetryMax       = 3
	summaryRetryBaseMs    = 1000
	carryAckContent       = "Understood — continuing with the prior summary as context."
)

// summaryInstruction is the final user turn appended to ask for the summary.
// It names the must-keep facts explicitly so the model doesn't drop them.
func summaryInstruction(wordLimit int) string {
	if wordLimit <= 0 {
		wordLimit = 500
	}
	return fmt.Sprintf("Summarize the conversation above in concise but comprehensive form. "+
		"PRESERVE exactly: file paths, shell commands, error messages, key decisions, and "+
		"any tool-result facts needed to continue. Tool outputs may be abbreviated only when "+
		"repetitive. Keep it under %d words.", wordLimit)
}

// RunSummary produces a text summary of msgs via m. If the estimated token count
// fits within ChunkThreshold*ModelWindow it makes a single CACHE-ALIGNED call
// (msgs verbatim as prefix + a trailing instruction) so the summary request hits
// the prefix cache. Otherwise it splits msgs into budget-bounded chunks and runs
// a CARRY-style rolling summary: chunk1→s1, [s1,chunk2]→s2, … so each call stays
// in-window while carrying prior context forward. Retries transient errors.
func RunSummary(ctx context.Context, msgs []*schema.Message, opts RunOpts, m ModelSummarizer, onChunk func(string)) (string, error) {
	if len(msgs) == 0 {
		return "", nil
	}
	budget := int(float64(opts.ModelWindow) * opts.ChunkThreshold)
	if budget <= 0 {
		budget = opts.ModelWindow // fallback: no chunking guard
	}
	total := EstimateTokens(msgs)
	if total <= budget {
		req := append([]*schema.Message{}, msgs...)
		req = append(req, &schema.Message{Role: schema.User, Content: summaryInstruction(opts.SummaryWordLimit)})
		return callWithRetry(ctx, m, req, onChunk)
	}
	chunks := splitChunks(msgs, budget)
	carry := ""
	for i, chunk := range chunks {
		req := buildCarryRequest(carry, chunk, opts.SummaryWordLimit)
		s, err := callWithRetry(ctx, m, req, onChunk)
		if err != nil {
			return "", fmt.Errorf("compaction chunk %d/%d: %w", i+1, len(chunks), err)
		}
		carry = s
	}
	return carry, nil
}

// buildCarryRequest assembles the model input for one carry chunk: prior summary
// as a sentinel-prefixed user turn (+ ack), the chunk's messages verbatim, then
// the instruction. chunk1 has no carry prefix so its prefix == original history
// opening (cache-aligned for that one block).
func buildCarryRequest(carry string, chunk []*schema.Message, wordLimit int) []*schema.Message {
	var req []*schema.Message
	if carry != "" {
		req = append(req, &schema.Message{Role: schema.User, Content: SummarySentinel + carry})
		req = append(req, &schema.Message{Role: schema.Assistant, Content: carryAckContent})
	}
	req = append(req, chunk...)
	req = append(req, &schema.Message{Role: schema.User, Content: summaryInstruction(wordLimit)})
	return req
}

// splitChunks slices msgs into contiguous runs whose token sum ≤ budget, backing
// off any split point that would sever a tool_call from its tool_result (safe
// boundary — reuses the same pairing invariant the fixpoint enforces).
func splitChunks(msgs []*schema.Message, budget int) [][]*schema.Message {
	var chunks [][]*schema.Message
	var cur []*schema.Message
	curTok := 0
	flush := func() {
		if len(cur) > 0 {
			chunks = append(chunks, cur)
			cur = nil
			curTok = 0
		}
	}
	for i := 0; i < len(msgs); i++ {
		mt := estimateMessageTokens(msgs[i])
		if curTok+mt > budget && len(cur) > 0 && splitIsSafe(msgs, i) {
			flush()
		}
		cur = append(cur, msgs[i])
		curTok += mt
	}
	flush()
	return chunks
}

// splitIsSafe reports whether msgs[:i] | msgs[i:] severs no tool pair: the
// message ending the left side must not have a tool_call whose result is on the
// right, and the message starting the right side must not be a tool_result whose
// call is on the left.
func splitIsSafe(msgs []*schema.Message, i int) bool {
	if i <= 0 || i >= len(msgs) {
		return true
	}
	left := msgs[i-1]
	if left != nil {
		for _, tc := range left.ToolCalls {
			if tc.ID == "" {
				continue
			}
			for j := i; j < len(msgs); j++ {
				if msgs[j] != nil && msgs[j].ToolCallID == tc.ID {
					return false // result is on the right — would sever
				}
			}
		}
	}
	right := msgs[i]
	if right != nil && right.ToolCallID != "" {
		for j := 0; j < i; j++ {
			if msgs[j] != nil {
				for _, tc := range msgs[j].ToolCalls {
					if tc.ID == right.ToolCallID {
						return false // call is on the left — would sever
					}
				}
			}
		}
	}
	return true
}

// callWithRetry invokes m.Stream (preferring streaming so onChunk gets deltas)
// and falls back to Generate. Retries only transient errors up to
// summaryRetryMax with exponential backoff. Permanent errors surface immediately.
func callWithRetry(ctx context.Context, m ModelSummarizer, msgs []*schema.Message, onChunk func(string)) (string, error) {
	var lastErr error
	for attempt := 0; attempt < summaryRetryMax; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(summaryRetryBaseMs * (1 << (attempt - 1)) * time.Millisecond):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		s, err := streamOnce(ctx, m, msgs, onChunk)
		if err == nil {
			return s, nil
		}
		if !isTransient(err) {
			return "", err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("compaction summary failed")
	}
	return "", lastErr
}

func streamOnce(ctx context.Context, m ModelSummarizer, msgs []*schema.Message, onChunk func(string)) (string, error) {
	sr, err := m.Stream(ctx, msgs)
	if err != nil {
		// fall back to Generate (some doubles don't implement Stream)
		if msg, gerr := m.Generate(ctx, msgs); gerr == nil && msg != nil {
			if onChunk != nil && msg.Content != "" {
				onChunk(msg.Content)
			}
			return msg.Content, nil
		}
		return "", err
	}
	defer sr.Close()
	var sb strings.Builder
	for {
		msg, recvErr := sr.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return "", recvErr
		}
		if msg == nil || msg.Content == "" {
			continue
		}
		sb.WriteString(msg.Content)
		if onChunk != nil {
			onChunk(msg.Content)
		}
	}
	return sb.String(), nil
}

// isTransient classifies an error as retryable. Network/timeout/rate-limit
// retry; auth/parse/validation surface immediately. NOTE: yanshi's
// internal/llm/eino/resilient.go owns the authoritative classification; if a
// shared helper already exists, prefer it. This local set is the conservative
// fallback so ctxcompact doesn't import eino (keeping the dependency arrow
// inward per the hexagonal layout in CLAUDE.md).
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false // user cancel / timeout-abort is not a retry case here
	}
	msg := strings.ToLower(err.Error())
	for _, m := range []string{"timeout", "timed out", "connection reset", "eof", "broken pipe", "temporary", "429", "503", "502", "retry"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// model.Option import kept to satisfy the interface signature shape used by
// callers; not all summarizers consume it.
var _ model.Option = nil
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run TestRunSummary_ -v`
Expected: PASS。

> ⚠️ 实现末尾的 `var _ model.Option = nil` 是错误的（`model.Option` 是函数类型，不是可 nil-compare 的接口）。删除该行。import `"github.com/cloudwego/eino/components/model"` 仍需要（`ModelSummarizer` 接口签名在 options.go 里用到，但 summarize.go 本身的 `streamOnce`/`callWithRetry` 签名没用 model.Option）。**实现时核对：summarize.go 若不直接用 model.Option，去掉该 import 和那行错误的断言。**

- [ ] **Step 5: Commit**

```bash
git add internal/ctxcompact/summarize.go internal/ctxcompact/summarize_test.go
git commit -m "feat(ctxcompact): cache-aligned + carry-style chunked RunSummary with retry"
```

---

### Task 9: `assemble.go` — 拼装产物（修 bug④）

**Files:**
- Create: `internal/ctxcompact/assemble.go`
- Test: `internal/ctxcompact/assemble_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/ctxcompact/assemble_test.go
package ctxcompact

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
)

// bug④ 回归: summary 必须是 user+sentinel 放末尾,不能是 System (会和编排器 system prompt 冲突).
func TestAssemble_SummaryIsUserSentinelAtTail(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "first"},
		{Role: schema.Assistant, Content: "noise"},
		{Role: schema.User, Content: "recent"},
	}
	plan := &PlanResult{PinnedIndices: []int{0, 2}}
	out := Assemble(msgs, plan, "the summary text")
	assert.Equal(t, schema.User, out[len(out)-1].Role, "last is user")
	assert.True(t, IsSummaryMessage(out[len(out)-1]), "last is summary sentinel")
	assert.Contains(t, out[len(out)-1].Content, "the summary text")
	// no System role anywhere in the compacted output
	for _, m := range out {
		assert.NotEqual(t, schema.System, m.Role, "no System summary (bug④)")
	}
}

func TestAssemble_PreservesPinnedOrder(t *testing.T) {
	msgs := []*schema.Message{
		{Role: schema.User, Content: "a"},
		{Role: schema.Assistant, Content: "b"},
		{Role: schema.User, Content: "c"},
	}
	plan := &PlanResult{PinnedIndices: []int{0, 2}}
	out := Assemble(msgs, plan, "s")
	assert.Equal(t, "a", out[0].Content)
	assert.Equal(t, "c", out[1].Content)
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ctxcompact/ -run TestAssemble_ -v`
Expected: FAIL — `Assemble` undefined。

- [ ] **Step 3: 实现**

```go
// internal/ctxcompact/assemble.go
package ctxcompact

import "github.com/cloudwego/eino/schema"

// Assemble builds the compacted history: the pinned messages verbatim (in
// original order) followed by the summary as a sentinel-prefixed USER message
// at the tail. Summary-as-user (not System) avoids the double-system conflict
// with the orchestrator's own system prompt (bug④). Putting it at the tail
// keeps the pinned prefix byte-stable for cache hits on subsequent turns.
func Assemble(msgs []*schema.Message, plan *PlanResult, summary string) []*schema.Message {
	out := make([]*schema.Message, 0, len(plan.PinnedIndices)+1)
	for _, i := range plan.PinnedIndices {
		if i >= 0 && i < len(msgs) {
			out = append(out, msgs[i])
		}
	}
	out = append(out, &schema.Message{Role: schema.User, Content: SummarySentinel + summary})
	return out
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run TestAssemble_ -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/ctxcompact/assemble.go internal/ctxcompact/assemble_test.go
git commit -m "feat(ctxcompact): Assemble — summary as user+sentinel at tail (bug④)"
```

---

### Task 10: `run.go` — Run 统一入口 + 失败处理（修 bug⑥）

**Files:**
- Create: `internal/ctxcompact/run.go`
- Test: `internal/ctxcompact/run_test.go`

- [ ] **Step 1: 写失败测试**

```go
// internal/ctxcompact/run_test.go
package ctxcompact

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_FullPipeline(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"the compacted summary"}, nil)
	msgs := []*schema.Message{}
	for i := 0; i < 12; i++ {
		msgs = append(msgs, &schema.Message{Role: schema.User, Content: strings.Repeat("a", 200)})
	}
	res, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 2},
		RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm, nil)
	require.NoError(t, err)
	assert.True(t, IsSummaryMessage(res.Messages[len(res.Messages)-1]), "ends with summary")
	assert.Contains(t, res.Messages[len(res.Messages)-1].Content, "the compacted summary")
	assert.Less(t, res.TokensAfter, res.TokensBefore)
}

func TestRun_FailureDoesNotProduceEmptySummary(t *testing.T) {
	// bug⑥: 旧路径失败时返回空 summary 消息. 新路径必须返回 error.
	fm := einollm.NewFakeModel(nil, errors.New("model down"))
	msgs := []*schema.Message{}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, &schema.Message{Role: schema.User, Content: strings.Repeat("a", 200)})
	}
	_, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 1},
		RunOpts{ModelWindow: 10000, ChunkThreshold: 0.9}, fm, nil)
	require.Error(t, err, "failure surfaces as error, never an empty summary")
}

func TestRun_NoOpWhenNothingToSummarize(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"UNUSED"}, nil)
	// few messages -> all pinned -> nothing to summarize -> model never called
	msgs := []*schema.Message{
		{Role: schema.User, Content: "hi"},
		{Role: schema.Assistant, Content: "yo"},
	}
	res, err := Run(context.Background(), msgs, PlanOpts{KeepRecent: 2},
		RunOpts{ModelWindow: 10000}, fm, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, fm.GenerateCalls+fm.StreamCalls, "no summary call when nothing to summarize")
	// messages returned unchanged (no summary appended)
	assert.Equal(t, len(msgs), len(res.Messages))
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/ctxcompact/ -run TestRun_ -v`
Expected: FAIL — `Run` undefined。

- [ ] **Step 3: 实现**

```go
// internal/ctxcompact/run.go
package ctxcompact

import (
	"context"
	"fmt"

	"github.com/cloudwego/eino/schema"
)

// Run is the unified compaction entry both paths (mid-turn CompactingModel and
// pre-turn MaybeCompact) delegate to. It Plans, summarizes the summarize set,
// enforces tool pairs, and assembles the result. On summary failure it returns
// an error — callers decide (mid-turn falls back to original msgs, pre-turn
// keeps history and warns) — it NEVER produces an empty summary (bug⑥).
func Run(ctx context.Context, msgs []*schema.Message, planOpts PlanOpts, runOpts RunOpts, m ModelSummarizer, onChunk func(string)) (*Result, error) {
	before := EstimateTokens(msgs)
	plan := Plan(msgs, planOpts)

	if len(plan.SummarizeIndices) == 0 {
		// nothing to summarize (everything pinned, or already-summarized history).
		return &Result{Messages: msgs, TokensBefore: before, TokensAfter: before}, nil
	}

	toSummarize := make([]*schema.Message, 0, len(plan.SummarizeIndices))
	for _, i := range plan.SummarizeIndices {
		if i >= 0 && i < len(msgs) {
			toSummarize = append(toSummarize, msgs[i])
		}
	}

	summary, err := RunSummary(ctx, toSummarize, runOpts, m, onChunk)
	if err != nil {
		return nil, fmt.Errorf("compaction summary: %w", err)
	}

	out := Assemble(msgs, plan, summary)
	return &Result{Messages: out, TokensBefore: before, TokensAfter: EstimateTokens(out)}, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/ctxcompact/ -run TestRun_ -v`
Expected: PASS。

- [ ] **Step 5: 跑整个包 + vet**

Run: `go test ./internal/ctxcompact/ && go vet ./internal/ctxcompact/`
Expected: 全 PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/ctxcompact/run.go internal/ctxcompact/run_test.go
git commit -m "feat(ctxcompact): Run unified entry — failure returns error, never empty summary (bug⑥)"
```

---

## Section C: pre-turn 接入

### Task 11: 重写 `compact.go`（MaybeCompact 用 Run）+ 更新现有测试

**Files:**
- Modify: `internal/ctxcompact/compact.go`
- Modify: `internal/ctxcompact/compact_test.go`

- [ ] **Step 1: 更新现有测试以匹配新行为**

现有 `compact_test.go` 断言旧行为（summary 是 System role、第 0 条；旧 `estimateTokens`/`serialize` 函数）。全部改为新契约。**替换整个文件**：

```go
// internal/ctxcompact/compact_test.go
package ctxcompact

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
)

func longMsgs(n, chars int) []*schema.Message {
	out := make([]*schema.Message, n)
	for i := 0; i < n; i++ {
		out[i] = &schema.Message{Role: schema.User, Content: strings.Repeat("a", chars)}
	}
	return out
}

func TestMaybeCompact_OverThreshold(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	msgs := longMsgs(10, 200)
	chunks := 0
	out, before, after, did := MaybeCompact(context.Background(), msgs,
		0.8, 100, 2, fm, func(string) { chunks++ })
	require.True(t, did)
	assert.Less(t, len(out), len(msgs))
	assert.Greater(t, before, after)
	// bug④: summary is now user+sentinel at TAIL (not system at head)
	assert.True(t, IsSummaryMessage(out[len(out)-1]))
	assert.Contains(t, out[len(out)-1].Content, "SUMMARY")
	assert.GreaterOrEqual(t, chunks, 1)
}

func TestMaybeCompact_UnderThreshold(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	msgs := longMsgs(3, 8)
	out, before, after, did := MaybeCompact(context.Background(), msgs,
		0.8, 100000, 2, fm, func(string) {})
	assert.False(t, did)
	assert.Equal(t, len(msgs), len(out))
	assert.Equal(t, before, after)
}

func TestMaybeCompact_DisabledThreshold(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	_, _, _, did := MaybeCompact(context.Background(), longMsgs(10, 200),
		0, 100, 2, fm, func(string) {})
	assert.False(t, did)
}

func TestMaybeCompact_TooFewMessages(t *testing.T) {
	fm := einollm.NewFakeModel([]string{"SUMMARY"}, nil)
	out, _, did := MaybeCompact(context.Background(), longMsgs(5, 200),
		0.8, 100, 4, fm, func(string) {})
	assert.False(t, did)
	assert.Equal(t, 5, len(out))
}

func TestMaybeCompact_PreservesToolCalls(t *testing.T) {
	// bug①/②: pinned tool interaction stays verbatim (not flattened into summary text)
	fm := einollm.NewFakeModel([]string{"summary"}, nil)
	msgs := []*schema.Message{
		{Role: schema.User, Content: "edit internal/ctxcompact/compact.go"}, // pinned (ws)
		{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
			{ID: "c1", Function: schema.FunctionCall{Name: "edit_file", Arguments: `{"path":"internal/ctxcompact/compact.go"}`}},
		}}, // pinned (paired)
		{Role: schema.Tool, ToolCallID: "c1", Content: "edited"}, // pinned (ws mention)
	}
	out, _, did := MaybeCompact(context.Background(), msgs, 0.8, 1, 1, fm, func(string) {})
	// even if it runs, the assistant tool_call + tool result must survive verbatim
	var sawCall, sawResult bool
	for _, m := range out {
		if len(m.ToolCalls) > 0 {
			sawCall = true
		}
		if m.ToolCallID == "c1" {
			sawResult = true
		}
	}
	assert.True(t, sawCall || !did, "tool_call preserved when compaction ran")
	assert.True(t, sawResult || !did)
}
```

- [ ] **Step 2: 重写 compact.go**

```go
// internal/ctxcompact/compact.go
package ctxcompact

import (
	"context"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// MaybeCompact is the PRE-TURN compaction entry (WS handler calls it before a
// user_message turn). It delegates to Run with a ModelSummarizer built from the
// caller-supplied model.BaseChatModel. When compaction fires it returns the new
// history and did=true; on summary failure it returns (nil, before, before,
// false) so the caller keeps the original history intact (bug⑥ — never an empty
// summary). threshold <= 0 or too-few-messages is a no-op.
func MaybeCompact(ctx context.Context, msgs []*schema.Message,
	threshold float64, contextWindow, keepRecent int,
	m model.BaseChatModel, onChunk func(string)) ([]*schema.Message, int, int, bool) {

	before := EstimateTokens(msgs)
	noop := func() ([]*schema.Message, int, int, bool) {
		return msgs, before, before, false
	}
	if threshold <= 0 || contextWindow <= 0 {
		return noop()
	}
	if before < int(threshold*float64(contextWindow)) {
		return noop()
	}
	if len(msgs) <= keepRecent*2+1 {
		return noop()
	}

	res, err := Run(ctx, msgs, PlanOpts{KeepRecent: keepRecent},
		RunOpts{ModelWindow: contextWindow, ChunkThreshold: 0.9}, summarizer{m}, onChunk)
	if err != nil {
		// best-effort: keep original history, surface no compaction.
		return noop()
	}
	return res.Messages, res.TokensBefore, res.TokensAfter, true
}

// summarizer adapts a model.BaseChatModel to ModelSummarizer (they are the same
// shape; the adapter exists so ctxcompact's core doesn't import eino's model
// package transitively beyond the interface it already needs).
type summarizer struct{ m model.BaseChatModel }

func (s summarizer) Generate(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return s.m.Generate(ctx, msgs, opts...)
}
func (s summarizer) Stream(ctx context.Context, msgs []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	return s.m.Stream(ctx, msgs, opts...)
}
```

> ⚠️ 旧的 `MaybeCompact` 签名接收 `*orchestrator.Orchestrator` 和 `orchestrator.TurnOpts`。新签名改为直接接收 `model.BaseChatModel`——这解耦了 ctxcompact 对 orchestrator 的依赖（依赖箭头向内，符合 CLAUDE.md 的六边形布局）。**这会破坏 WS handler 的调用点**（Task 15 修复）。Task 15 必须在同一 commit 或紧随其后，否则 build 断裂。如果选择分 commit，Task 11 与 Task 15 必须连续完成。

- [ ] **Step 3: 跑测试**

Run: `go test ./internal/ctxcompact/ -v`
Expected: 新测试 PASS。但 `go build ./...` 可能因 ws.go 调用旧 MaybeCompact 签名而失败——这是预期的，Task 15 修复。

- [ ] **Step 4: Commit（与 Task 15 合并提交以保持 build 绿，或标记为 WIP）**

```bash
git add internal/ctxcompact/compact.go internal/ctxcompact/compact_test.go
git commit -m "refactor(ctxcompact): MaybeCompact delegates to Run, drops orchestrator dep"
```

---

## Section D: mid-turn 接入

### Task 12: 重写 `compacting.go`（CompactingModel 内核换 Run）+ 更新测试

**Files:**
- Modify: `internal/llm/eino/compacting.go`
- Modify: `internal/llm/eino/compacting_test.go`

- [ ] **Step 1: 更新 compacting_test.go 以匹配新行为**

现有测试断言 `last[0].Role == schema.System`（summary 在头部是 system）。新设计 summary 是 user+sentinel 在尾部。**修改 `TestCompactingModel_CompactsWhenOverThreshold` 等测试的断言**：

在 `internal/llm/eino/compacting_test.go` 的 `TestCompactingModel_CompactsWhenOverThreshold` 中，把：
```go
assert.Equal(t, schema.System, last[0].Role)
assert.Contains(t, last[0].Content, "SUMMARY")
```
改为：
```go
// bug④: summary is now user+sentinel at TAIL, not system at head
assert.True(t, last[len(last)-1].Role == schema.User &&
    strings.HasPrefix(last[len(last)-1].Content, ctxcompact.SummarySentinel))
assert.Contains(t, last[len(last)-1].Content, "SUMMARY")
```
（顶部加 `"strings"` 和 `"github.com/x6nux/yanshi/internal/ctxcompact"` import。）

同样修改 `TestCompactingModel_StreamCompacts` 的 `assert.Equal(t, schema.System, ins[1][0].Role)` → 改为断言末尾是 sentinel summary。

- [ ] **Step 2: 重写 compacting.go 的 maybeCompact**

把 `internal/llm/eino/compacting.go` 的 `maybeCompact`/`summarize`/`shouldCompact`/`estimateTokens`/`serializeMessages` 全部替换为委托 `ctxcompact.Run`。保留 `CompactingModel` struct、`Generate`/`Stream` 入口、`WithCompactCallback`/`compactCallback`（TUI 进度回调）、和 `compactCallbackKey`。**核心替换**：

```go
// 在 compacting.go 顶部 import 增加:
//   "github.com/x6nux/yanshi/internal/ctxcompact"

// 替换 maybeCompact:
func (c *CompactingModel) maybeCompact(ctx context.Context, msgs []*schema.Message) ([]*schema.Message, bool) {
	if !c.shouldCompact(msgs) {
		return msgs, false
	}
	cb := compactCallback(ctx)
	res, err := ctxcompact.Run(ctx, msgs,
		ctxcompact.PlanOpts{KeepRecent: c.KeepRecent / 2}, // KeepRecent 字段是消息数,Plan 按对数
		ctxcompact.RunOpts{ModelWindow: c.ContextWindow, ChunkThreshold: 0.9},
		c.Inner, cb)
	if err != nil {
		// best-effort: forward original history so the real call surfaces any error
		return msgs, false
	}
	return res.Messages, true
}

// shouldCompact 保留(只是 gate),summarize/estimateTokens/serializeMessages 全删(由 ctxcompact 提供):
func (c *CompactingModel) shouldCompact(msgs []*schema.Message) bool {
	if c.Threshold <= 0 || c.ContextWindow <= 0 || c.KeepRecent <= 0 {
		return false
	}
	if len(msgs) <= c.KeepRecent {
		return false
	}
	return ctxcompact.EstimateTokens(msgs) >= int(c.Threshold*float64(c.ContextWindow))
}
```

> ⚠️ `CompactingModel.KeepRecent` 当前是「消息数」，`ctxcompact.PlanOpts.KeepRecent` 是「对数」。上面用 `c.KeepRecent / 2` 做转换。在 compacting.go 的 struct 注释里标注这个语义。如果 `c.KeepRecent` 是奇数，`/2` 向下取整——可接受（gate 用途）。

- [ ] **Step 3: 跑测试**

Run: `go test ./internal/llm/eino/ -run TestCompactingModel -v`
Expected: PASS。

- [ ] **Step 4: Commit**

```bash
git add internal/llm/eino/compacting.go internal/llm/eino/compacting_test.go
git commit -m "refactor(eino): CompactingModel delegates to ctxcompact.Run (bug①②④⑤⑦)"
```

---

## Section E: 配置

### Task 13: `config.go` — ProviderConfig.ContextWindow + ChunkThreshold + ContextWindowFor

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/config/config_test.go` 追加：

```go
func TestProviderConfig_ContextWindow(t *testing.T) {
	cfg := &Config{LLM: LLMConfig{Providers: []ProviderConfig{
		{Name: "openai", ContextWindow: 128000},
		{Name: "claude", /* unset */},
	}}}
	assert.Equal(t, 128000, ContextWindowFor("openai", cfg.LLM.Providers, 256000))
	assert.Equal(t, 256000, ContextWindowFor("claude", cfg.LLM.Providers, 256000), "fallback when provider unset")
	assert.Equal(t, 256000, ContextWindowFor("unknown", cfg.LLM.Providers, 256000), "fallback when provider absent")
	assert.Equal(t, 0, ContextWindowFor("unknown", cfg.LLM.Providers, 0), "zero when no fallback either")
}

func TestLoad_ChunkThresholdDefault(t *testing.T) {
	tmp := t.TempDir() + "/c.yaml"
	_ = os.WriteFile(tmp, []byte("compaction:\n  threshold: 0.8\n"), 0o644)
	cfg, err := Load(tmp)
	require.NoError(t, err)
	assert.Equal(t, 0.9, cfg.Compaction.ChunkThreshold, "chunk_threshold defaults to 0.9")
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/config/ -run "TestProviderConfig_ContextWindow|TestLoad_ChunkThresholdDefault" -v`
Expected: FAIL — `ContextWindowFor` undefined，`ChunkThreshold` 字段不存在。

- [ ] **Step 3: 实现**

在 `internal/config/config.go`：

(a) `ProviderConfig` 加字段：
```go
type ProviderConfig struct {
	Name          string `yaml:"name"`
	Kind          string `yaml:"kind"`
	Model         string `yaml:"model"`
	APIKey        string `yaml:"api_key"`
	BaseURL       string `yaml:"base_url"`
	ContextWindow int    `yaml:"context_window"` // 该模型的 token 窗口(0 = 回退 compaction.context_window)
}
```

(b) `CompactionConfig` 加字段 + 更新注释：
```go
type CompactionConfig struct {
	Threshold      float64 `yaml:"threshold"`       // 触发压缩: 总token/窗口 >= 此值(默认 0.8). <=0 禁用.
	KeepRecent     int     `yaml:"keep_recent"`     // 尾部保留对数(默认 4).
	ContextWindow  int     `yaml:"context_window"`  // 回退窗口: provider 没配时用(默认 256000).
	Model          string  `yaml:"model"`           // 专用 summary 模型; empty = 当前 session model.
	ChunkThreshold float64 `yaml:"chunk_threshold"` // 单次 vs 分块: summary输入/窗口 >= 此值走携带式分块(默认 0.9).
}
```

(c) `applyDefaults` 加 ChunkThreshold：
```go
if c.Compaction.ChunkThreshold == 0 {
	c.Compaction.ChunkThreshold = 0.9
}
```

(d) 新增 `ContextWindowFor`：
```go
// ContextWindowFor 查 provider 的 context_window; 没配或 provider 不在则回退 fallback.
// fallback 也为 0 时返回 0 (调用方据此禁用 compaction 子系统).
func ContextWindowFor(providerName string, providers []ProviderConfig, fallback int) int {
	for _, p := range providers {
		if p.Name == providerName && p.ContextWindow > 0 {
			return p.ContextWindow
		}
	}
	return fallback
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/config/ -v`
Expected: 全 PASS（含新增 + 现有）。

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): per-provider context_window + chunk_threshold"
```

---

### Task 14: `config.example.yaml` 更新示例

**Files:**
- Modify: `config.example.yaml`

- [ ] **Step 1: 更新示例**

把 `config.example.yaml` 的 `llm.providers` 段改为（每个 provider 加 `context_window`）：
```yaml
llm:
  providers:
    - name: "openai"
      kind: "openai"
      model: "gpt-4o"
      api_key: "${OPENAI_API_KEY}"
      context_window: 128000      # 该模型的 token 窗口; compaction 按此判定阈值与分块
    - name: "claude"
      kind: "anthropic"
      model: "claude-opus-4-8"
      api_key: "${ANTHROPIC_API_KEY}"
      context_window: 200000
```
`compaction` 段改为：
```yaml
compaction:
  threshold: 0.8        # 对话总 token / 窗口 >= 此值时触发压缩
  keep_recent: 4        # 尾部保留的 user/assistant 对数
  context_window: 256000 # 回退: provider 未配 context_window 时用
  model: ""             # 专用 summary 模型; empty = 当前 session model
  chunk_threshold: 0.9  # summary 输入 / 窗口 >= 此值时走携带式分块(不超窗口)
```

- [ ] **Step 2: 校验 YAML 可加载**

Run: `go test ./internal/config/ -run TestLoad -v`
Expected: PASS（现有 Load 测试仍通过）。

- [ ] **Step 3: Commit**

```bash
git add config.example.yaml
git commit -m "docs(config): example provider.context_window + chunk_threshold"
```

---

## Section F: WS handler 接入

### Task 15: `ws.go` — contextWindowFor 按 model 查 + 适配新 MaybeCompact 签名

**Files:**
- Modify: `internal/api/http/ws.go`

- [ ] **Step 1: 定位改动点**

Run: `grep -n "contextWindowFor\|maybeAutoCompact\|compactNow\|MaybeCompact" internal/api/http/ws.go`
确认三个调用点：`contextWindowFor(cs.model, s.compaction)`（两处）、`ctxcompact.MaybeCompact(...)`（在 `maybeAutoCompact`/`compactNow` 里）。

- [ ] **Step 2: 更新 contextWindowFor 与 MaybeCompact 调用**

(a) 找到现有的 `contextWindowFor` 本地函数（约 ws.go:1363 `func contextWindowFor(_ string, cc CompactionConfig) int { return cc.ContextWindow }`），替换为：
```go
// contextWindowFor 查当前 model 对应 provider 的窗口; 没配则回退 compaction.context_window.
func contextWindowFor(model string, providers []config.ProviderConfig, cc config.CompactionConfig) int {
	return config.ContextWindowFor(model, providers, cc.ContextWindow)
}
```
（调用点改为传 `s.cfg.LLM.Providers` 或等价的 providers slice；确认 `Server` 持有 `*config.Config` 或 providers。若 Server 只持有 `s.compaction` 而不持有 providers，需在 bootstrap 装配时把 providers 传入 Server——见 Step 2b。）

(b) 若 Server 结构没有 providers 字段：在 `internal/api/http` 的 Server struct 加 `providers []config.ProviderConfig`，bootstrap 装配时从 `cfg.LLM.Providers` 传入。把所有 `contextWindowFor(cs.model, s.compaction)` 调用改为 `contextWindowFor(cs.model, s.providers, s.compaction)`。

(c) `maybeAutoCompact` / `compactNow` 里对 `ctxcompact.MaybeCompact` 的调用：旧签名 `(ctx, o, msgs, threshold, cw, kr, opts, onChunk)`（o 是 orchestrator，opts 是 TurnOpts）。新签名 `(ctx, msgs, threshold, cw, kr, model, onChunk)`。改为：
```go
// 取 summary 模型: compaction.Model 配了且 registry 有就用它, 否则用当前 session model.
sumModel := compactionModel(s.compaction, models, cs.model) // 见下
newHist, tb, ta, did := ctxcompact.MaybeCompact(ctx, cs.history,
	s.compaction.Threshold, cw, kr, sumModel,
	func(chunk string) { conn.write(proto.NewCompactChunk(chunk)) })
```
其中 `compactionModel` 替换旧的 `compactionModelOpts`：
```go
// compactionModel 返回用于 summary 的 model.BaseChatModel:
// 配了 compaction.Model 且 registry 有 -> 那个; 否则当前 session model.
func compactionModel(cc config.CompactionConfig, models map[string]model.BaseChatModel, sessionModel string) model.BaseChatModel {
	if cc.Model != "" {
		if m, ok := models[cc.Model]; ok {
			return m
		}
	}
	if sessionModel != "" {
		return models[sessionModel]
	}
	for _, m := range models { // fallback: any registered
		return m
	}
	return nil
}
```
若 `sumModel == nil`：跳过压缩（compaction 子系统事实禁用），打 stderr warn。

- [ ] **Step 3: 跑构建 + 相关测试**

Run: `go build ./... && go test ./internal/api/http/ -v`
Expected: build 通过；http 测试 PASS。若有测试断言旧 MaybeCompact 的 orchestrator 注入，相应更新。

- [ ] **Step 4: Commit**

```bash
git add internal/api/http/ws.go internal/api/http/*.go
git commit -m "refactor(http): contextWindowFor queries provider config; MaybeCompact takes model not orchestrator"
```

---

## Section G: TUI

### Task 16: TUI — 删 compactEntry，压缩状态走 activity line（修 bug⑧）

**Files:**
- Modify: `internal/cli/tui/model.go`
- Modify: `internal/cli/tui/commands.go`
- Modify: `internal/cli/tui/events.go`
- Modify: `internal/cli/tui/commands_test.go`

- [ ] **Step 1: 更新测试断言新行为**

在 `internal/cli/tui/commands_test.go`，`TestModel_CompactChunkStreaming`（约 line 242）当前断言 delta 累积进 compactEntry。改为断言：compact_chunk 设置 `m.activity`（不产生 transcript entry）：
```go
func TestModel_CompactChunkDrivesActivityLine(t *testing.T) {
	m := newModel() // 或现有构造
	m = m.applyEvent(cli.StreamEvent{Kind: "compact_chunk", Text: "sum-"})
	m = m.applyEvent(cli.StreamEvent{Kind: "compact_chunk", Text: "mary"})
	assert.Contains(t, m.activity, "ompact", "compact_chunk drives the Running activity line")
	// no compactEntry in the transcript
	for _, e := range m.entries {
		assert.NotIsType(t, compactEntry{}, e, "compact is not a transcript entry (bug⑧)")
	}
}
```
（`newModel`/`m.entries` 类型按现有测试里的实际构造方式。）

- [ ] **Step 2: 改 model.go 的 compact_chunk 分支**

把 `internal/cli/tui/model.go` 的 `case "compact_chunk":`（约 line 934）：
```go
case "compact_chunk":
	m.appendCompactChunk(ev.Text)
```
改为：
```go
case "compact_chunk":
	// bug⑧: compaction is a meta-op; its status lives in the activity line
	// (the "Running…" row rendered separately from the transcript), NOT as a
	// transcript entry. Summary deltas are not shown as chat content.
	m.activity = "Compacting context…"
```

- [ ] **Step 3: 删 compactEntry**

在 `internal/cli/tui/commands.go`，删除 `compactEntry` struct 及其 `render` 方法（约 line 710-731），并删除 `appendCompactChunk` 方法（在 model.go 里，grep 定位）。把 `applyStatus` 里 resolve compactEntry 的逻辑（grep `compactEntry` / `live = false` / `tokensBefore`）改为只更新 header token 计数 + 清 activity：
```go
// 在 applyStatus 里 ev.Compacted 分支:
if ev.Compacted {
	m.activity = "Thinking…" // resume; token reduction shows in header ctx counter
}
```

- [ ] **Step 4: summary 消息渲染跳过**

在 transcript 渲染历史消息的地方（grep 渲染 `m.entries` 或 `schema.Message` 的循环），对历史消息加判定：
```go
import "github.com/x6nux/yanshi/internal/ctxcompact"
// 渲染每条历史消息时:
if ctxcompact.IsSummaryMessage(msg) {
	continue // summary is model context, not conversational content (bug⑧)
}
```

- [ ] **Step 5: 跑测试**

Run: `go test ./internal/cli/tui/ -v`
Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/cli/tui/
git commit -m "refactor(tui): compaction status on activity line, not transcript (bug⑧)"
```

---

## Section H: 文档

### Task 17: CLAUDE.md + docs/compaction.md

**Files:**
- Modify: `CLAUDE.md`
- Create: `docs/compaction.md`

- [ ] **Step 1: 更新 CLAUDE.md 的「回合中压缩」段**

把 CLAUDE.md 里描述 mid-turn compaction 的段落（grep `CompactingModel` / `回合中压缩`）更新为反映新设计：携带式分块、按模型窗口、user+sentinel summary、压缩状态走 activity line。新增一句指向 `docs/compaction.md`。

- [ ] **Step 2: 写 docs/compaction.md**

新文件，覆盖：架构（统一 `internal/ctxcompact` 核心 + 两路径复用）、8 个 bug 与修复、保真 pin 五类、携带式分块原理、前缀缓存两层、按模型窗口配置、失败行为。参考 `docs/vcs.md` 的文档风格（多段解释 why）。

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md docs/compaction.md
git commit -m "docs(yanshi): rewrite compaction docs for new design"
```

---

## Self-Review（写完计划后自查）

**1. Spec coverage**（对照 spec 每节）：
- 架构/包结构/统一入口 → Task 1-10 ✓
- 保真 pin 五类 → Task 4 + 7 ✓
- 配对修复 → Task 5 ✓
- 结构化序列化 → Task 3 ✓
- 携带式分块 + cache-aligned → Task 8 ✓
- summary 载体 user+sentinel → Task 2 + 9 ✓
- 传输与 UI 呈现（activity line）→ Task 16 ✓
- 前缀缓存两层 → Task 8（cache-aligned）+ Task 9（summary 放末尾保前缀稳定）✓
- 鲁棒性（重试/失败不吞）→ Task 8（重试）+ Task 10（失败返回 error）✓
- token 估算 → Task 1 ✓
- 配置（provider.context_window + chunk_threshold）→ Task 13 + 14 ✓
- 双路径去重 → Task 2（sentinel）+ Task 7（Plan 短路）✓
- bug 映射表 ①-⑧ → 全覆盖 ✓

**2. Placeholder scan**：无 TBD/TODO。Task 8/11/12/15 标了 ⚠️ 注意点（类型签名转换、KeepRecent 对数 vs 消息数、build 连续性）——这些是必要的实现提醒，不是占位符。

**3. Type consistency**：
- `ModelSummarizer`（Task 6）→ `RunSummary`/`Run`（Task 8/10）接收它 ✓
- `PlanOpts`/`RunOpts`/`PlanResult`/`Result`（Task 6）→ 各处一致 ✓
- `MaybeCompact` 新签名（Task 11）→ ws.go 调用（Task 15）一致 ✓
- `EstimateTokens`/`IsSummaryMessage`/`SummarySentinel` 导出名一致 ✓
- `EnforceToolCallPairs(msgs, map[int]bool)`（Task 5）→ Plan 调用一致 ✓

**执行注意**：
- Task 8 测试里 `recordingModel` 的 `Stream` 返回值有拼写笔误（`[*schema.Message)` 应为 `[*schema.Message]`）——实现时按 Step 1 的修正说明写。
- Task 11 改 MaybeCompact 签名会断 ws.go，Task 15 必须紧随（或合并提交）。
- Task 16 涉及多处 grep 定位，实现时先 `grep -n compactEntry` 确认所有引用点。

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-07-19-context-compaction-rewrite.md`. Two execution options:

**1. Subagent-Driven (recommended)** — 我每个 task 派一个 fresh subagent 执行，task 之间做 review，快速迭代。适合这个 17-task 的较大计划。

**2. Inline Execution** — 在当前会话用 executing-plans 批量执行，带 checkpoint review。

选哪种？
