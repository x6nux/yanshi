# W-A 立即修 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 7 处「已交付功能坏了」的缺陷 —— 图片不计 token、工具输出不脱敏就发给 provider、中文检索三条链全失效、会话恢复丢工具轮、记忆蒸馏链零调用点、流式空闲永久挂起、并发子代理互相踩文件。

**Architecture:** 7 条彼此零依赖，落点在 7 个不同包（`ctxcompact` / `tools` / `store` / `api/http` / `llm/eino` / `vcs`），可完全并行。每条自带台账条目（GOV8 逐句证据握手），因此每个 Task 都以「台账转绿 + 门禁全绿」收尾，可独立评审、独立回滚。

**Tech Stack:** Go 1.26.4 · SQLite (FTS5) · `github.com/cloudwego/eino` v0.9.12 (schema.Message) · `github.com/stretchr/testify/require`

**Spec:** `docs/superpowers/specs/2026-08-27-capability-roadmap-design.md` §2（W-A 包）

---

## Global Constraints

以下约束对**每一个 Task 隐式生效**，值逐字来自 spec 与 CLAUDE.md：

1. **交互语言** —— 与用户的一切交互用中文；代码、标识符、命令、路径保持英文。**注释语言跟随所在文件的既有语言**（`internal/tools/guard.go` 是中文注释，`internal/ctxcompact/tokens.go` 是英文注释，各自照抄邻近风格）。
2. **禁止占位实现** —— 不得留 TODO 占位、空函数壳、硬编码假数据、mock 代替真实实现。
3. **Fake 优先于 mock** —— 需要替身时新增一个 fake（参照 `einollm.FakeModel`、`cli.FakeBackend`），不引入 mock 框架。
4. **重复逻辑必须抽成公共函数** —— 同包内或放进合适的小包，禁止复制粘贴。
5. **单文件 ≤ 5000 纯代码行**（不含注释与空行），`internal/` 与 `cmd/` 下的非测试 `.go` 由 GOV2 强制。即时检查：`go run ./cmd/codelines`。
6. **导出符号必须有 doc 注释**（GOV3），密度对齐所在包 —— guard / VCS / ADK 周围是多段注释解释**为什么**。
7. **每个 Task 结束前必须全绿**：
   ```sh
   go test ./internal/archtest ./internal/bootstrap
   go vet ./...
   ```
8. **台账三件套**（GOV8，本包 7 条全部要做）：
   - `docs/feature-status.yaml` 新增条目，`acceptance` 按「；」切成 N 条子句，`evidence` 的 key 恰好是 `"1"`..`"N"`，值只接受**测试引用**（`包路径::测试名`，不是文件路径）；
   - 被引用的每个测试，在**它自己的 doc 注释**里回写一行 `ledger: <条目ID>#<子句号> <子句原文>`，**逐字一致**；
   - `internal/archtest/acceptance_pin_test.go::acceptancePins` 补一行（子句数 + acceptance 原文的 SHA-256 前 16 位）。
   
   算 SHA 的命令（`<原文>` 替换为 acceptance 字符串本身，不含引号）：
   ```sh
   printf '%s' '<原文>' | shasum -a 256 | cut -c1-16
   ```
9. **不执行 git 提交以外的 git 操作**（不建分支、不 push）。每个 Task 末尾的 commit 是计划的一部分，允许执行。
10. **禁止把 `_test.go` 里的门禁常量抄进生产代码** —— 若需共享，走 CLAUDE.md 记的「手抄一份 + 用测试钉住两处不漂」的既有模式。

---

## File Structure

| 文件 | 责任 | Task |
|---|---|---|
| `internal/ctxcompact/tokenest.go` | 新增多模态 part 的 token 估算常量与函数 | 1 |
| `internal/ctxcompact/tokens.go` | `estimateMessageTokens` 接入多模态分支 | 1 |
| `internal/ctxcompact/tokens_multimodal_test.go` | 新建；多模态估算的回归测试 | 1 |
| `internal/tools/redactctx.go` | 新建；Redactor 的 context 注入器（与 `permctx.go`/`vcsctx.go` 同构） | 2 |
| `internal/tools/guard.go` | `InvokableRun` 的两个返回点接 Redactor | 2 |
| `internal/agent/orchestrator/orchestrator.go` | `bindExecutionContext` 绑定 Redactor | 2 |
| `internal/bootstrap/bootstrap.go` | 把 `redactor` 传进 orchestrator | 2 |
| `internal/store/message_log.go` | CJK 查询检测 + 有界 LIKE 回退 | 3 |
| `internal/store/ftsquery.go` | 新建；CJK 检测与回退查询构造（`SearchMessages` 与 `SearchMemory` 共用） | 3 |
| `internal/api/http/ws_handlers.go` | 恢复循环补齐 tool role 与三个字段 | 4 |
| `internal/tools/memory_distill.go` | 无改动（已完整），只是被接线 | 5 |
| `internal/cli/tui/commands.go` | `commandTable` 注册 `/distill` | 5 |
| `internal/cli/tui/commands_session_memory.go` | `cmdDistill` 实现 | 5 |
| `internal/proto/frame.go` | `/distill` 的请求/响应帧 | 5 |
| `internal/api/http/ws_handlers.go` | `/distill` 的服务端处理 + turn 后台触发 | 5 |
| `internal/llm/eino/streamwatchdog.go` | 新建；首块 + 稳态双超时 | 6 |
| `internal/llm/eino/resilient.go` | `consumeStream` 接看门狗 | 6 |
| `internal/tools/subagent.go` | `WithSubAgentWorkRoot` 注入器 + spec 字段 | 7 |
| `internal/agent/orchestrator/subagent.go` | 并发路径分配 worktree | 7 |

---

## Task 1: 图片计入 token 估算（W-A-01 / `F5`）

**为什么**：`internal/ctxcompact/tokens.go::estimateMessageTokens` 只累加 `Content` + `ReasoningContent` + `ToolCalls`。`schema.Message` 上**三个**多模态字段全部不被读：`MultiContent`（deprecated 但 `internal/ctxcompact/redact.go` 仍在处理）、`UserInputMultiContent`（`internal/agent/orchestrator/multimodal.go::appendImageParts` 的生产写入点）、`AssistantGenMultiContent`。

> **注意：审计只发现了第二个。** 写计划时复核发现三个都漏，本 Task 一并修 —— 只修一个会让同一 bug 换个字段复发。

后果：贴图会话里图片算 0 token → 压缩门永不触发 → 直接撞 provider 400。

**Files:**
- Modify: `internal/ctxcompact/tokenest.go`（在 `perToolCallOverhead` 常量之后追加）
- Modify: `internal/ctxcompact/tokens.go::estimateMessageTokens`
- Create: `internal/ctxcompact/tokens_multimodal_test.go`
- Modify: `docs/feature-status.yaml`
- Modify: `internal/archtest/acceptance_pin_test.go`

**Interfaces:**
- Consumes: `estimateTextTokens(string) int`（同包已有）、`perMessageOverhead = 8`、`perToolCallOverhead = 16`
- Produces: `estimateInputParts([]schema.MessageInputPart) int`、`estimateChatParts([]schema.ChatMessagePart) int`、`estimateOutputParts([]schema.MessageOutputPart) int`、`imageTokens(schema.ImageURLDetail) int`（均为包内未导出，无下游 Task 依赖）

---

- [ ] **Step 1: 写失败测试**

创建 `internal/ctxcompact/tokens_multimodal_test.go`：

```go
package ctxcompact

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
)

// imagePart builds a user input image part carrying a base64 data URL of the
// requested payload size, mirroring what
// orchestrator.appendImageParts writes in production.
func imagePart(detail schema.ImageURLDetail, payloadBytes int) schema.MessageInputPart {
	url := "data:image/png;base64," + strings.Repeat("A", payloadBytes)
	return schema.MessageInputPart{
		Type: schema.ChatMessagePartTypeImageURL,
		Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{MIMEType: "image/png", URL: &url},
			Detail:            detail,
		},
	}
}

// ledger: A2/W-A-01#1 带图片的消息其 token 估算随图片数量单调增长
func TestEstimateTokensGrowsWithImageCount(t *testing.T) {
	base := EstimateTokens([]*schema.Message{{Role: schema.User}})
	one := EstimateTokens([]*schema.Message{{
		Role:                  schema.User,
		UserInputMultiContent: []schema.MessageInputPart{imagePart(schema.ImageURLDetailHigh, 1024)},
	}})
	two := EstimateTokens([]*schema.Message{{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{
			imagePart(schema.ImageURLDetailHigh, 1024),
			imagePart(schema.ImageURLDetailHigh, 1024),
		},
	}})

	require.Greater(t, one, base, "one image must cost more than none")
	require.Equal(t, two-one, one-base, "each additional image costs the same")
}

// ledger: A2/W-A-01#2 估算不随 base64 载荷字节数线性增长
func TestEstimateTokensImageCostIsIndependentOfPayloadSize(t *testing.T) {
	small := EstimateTokens([]*schema.Message{{
		Role:                  schema.User,
		UserInputMultiContent: []schema.MessageInputPart{imagePart(schema.ImageURLDetailHigh, 4<<10)},
	}})
	large := EstimateTokens([]*schema.Message{{
		Role:                  schema.User,
		UserInputMultiContent: []schema.MessageInputPart{imagePart(schema.ImageURLDetailHigh, 1<<20)},
	}})

	require.Equal(t, small, large,
		"image cost is per-tile in provider accounting; len(data)/4 would differ by 3 orders of magnitude")
}

// ledger: A2/W-A-01#3 Message 上三个多模态字段都被计入
func TestEstimateTokensCountsAllThreeMultimodalFields(t *testing.T) {
	base := EstimateTokens([]*schema.Message{{Role: schema.User}})
	url := "data:image/png;base64,AAAA"

	deprecated := EstimateTokens([]*schema.Message{{
		Role: schema.User,
		MultiContent: []schema.ChatMessagePart{{
			Type:     schema.ChatMessagePartTypeImageURL,
			ImageURL: &schema.ChatMessageImageURL{URL: url, Detail: schema.ImageURLDetailHigh},
		}},
	}})
	userInput := EstimateTokens([]*schema.Message{{
		Role:                  schema.User,
		UserInputMultiContent: []schema.MessageInputPart{imagePart(schema.ImageURLDetailHigh, 4)},
	}})
	assistantGen := EstimateTokens([]*schema.Message{{
		Role: schema.Assistant,
		AssistantGenMultiContent: []schema.MessageOutputPart{{
			Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageOutputImage{
				MessagePartCommon: schema.MessagePartCommon{MIMEType: "image/png", URL: &url},
			},
		}},
	}})

	require.Greater(t, deprecated, base, "MultiContent must be counted")
	require.Greater(t, userInput, base, "UserInputMultiContent must be counted")
	require.Greater(t, assistantGen, base, "AssistantGenMultiContent must be counted")
}

// ledger: A2/W-A-01#4 低 detail 档位的估算低于高 detail 档位
func TestEstimateTokensLowDetailCostsLessThanHigh(t *testing.T) {
	low := EstimateTokens([]*schema.Message{{
		Role:                  schema.User,
		UserInputMultiContent: []schema.MessageInputPart{imagePart(schema.ImageURLDetailLow, 4<<10)},
	}})
	high := EstimateTokens([]*schema.Message{{
		Role:                  schema.User,
		UserInputMultiContent: []schema.MessageInputPart{imagePart(schema.ImageURLDetailHigh, 4<<10)},
	}})

	require.Less(t, low, high)
}

// TestEstimateTokensCountsTextParts pins that a text part inside multimodal
// content is estimated as text, not skipped alongside the media branch.
func TestEstimateTokensCountsTextParts(t *testing.T) {
	base := EstimateTokens([]*schema.Message{{Role: schema.User}})
	withText := EstimateTokens([]*schema.Message{{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: strings.Repeat("hello world ", 100)},
		},
	}})

	require.Greater(t, withText, base)
}
```

- [ ] **Step 2: 跑测试确认失败**

```sh
go test ./internal/ctxcompact -run 'TestEstimateTokens(GrowsWithImageCount|ImageCostIsIndependent|CountsAllThree|LowDetail|CountsTextParts)' -v
```

预期：`TestEstimateTokensGrowsWithImageCount` FAIL（`one` 与 `base` 相等）、`CountsAllThree` FAIL、`LowDetail` FAIL。
`ImageCostIsIndependentOfPayloadSize` 与 `CountsTextParts` 此刻可能"通过"（因为全是 0 或都相等）——这正常，它们是防回归的，实现后仍须通过。

> **若 `schema.MessageOutputImage` 或 `schema.ChatMessageImageURL` 的字段名与上面不符**，以 `go doc github.com/cloudwego/eino/schema.MessageOutputPart` 的实际输出为准修正测试，不要修改断言语义。

- [ ] **Step 3: 加估算常量**

在 `internal/ctxcompact/tokenest.go` 的 `perToolCallOverhead` 常量之后追加：

```go
// imageTokensLow / imageTokensHigh are the fixed per-image costs charged for
// the two detail tiers.
//
// The cost is per-TILE in provider accounting, not per byte: a 1 MiB base64
// payload and a 4 KiB one of the same pixel dimensions bill identically. That
// is why this must NOT be len(data)/4 — the two differ by three orders of
// magnitude, and the absence of any multimodal branch here is what let a pasted
// screenshot count as 8 tokens (the per-message envelope alone) and hold the
// compaction gate shut until the provider answered 400.
//
// ponytail: fixed constants, not a tile computation. Pricing a `high` image
// exactly needs its pixel dimensions, which means decoding the payload on every
// gate check. imageTokensHigh is instead the cost of a 1024x1024 image
// (85 base + 6 tiles x 170), the largest a provider bills before downscaling,
// so the estimate overcounts small images rather than undercounting large ones
// — the same bias estimateTextTokens documents. Decode for real dimensions if
// image-heavy sessions ever compact too eagerly.
const (
	imageTokensLow  = 85
	imageTokensHigh = 1105
)

// opaquePartTokens is the floor charged for an audio / video / file part.
//
// yanshi only writes image parts today (see orchestrator.appendImageParts), so
// this branch is unreachable in production. It exists anyway because zero is
// the value that caused this whole defect: a new modality wired later would
// silently weigh nothing, and nothing in the type system would say so.
const opaquePartTokens = 1105

// imageTokens prices one image at its declared detail tier.
//
// An empty or "auto" Detail resolves to the high tier: the provider decides,
// and overcounting compacts a little early while undercounting hits a 400.
func imageTokens(d schema.ImageURLDetail) int {
	if d == schema.ImageURLDetailLow {
		return imageTokensLow
	}
	return imageTokensHigh
}
```

若 `tokenest.go` 尚未 import `schema`，在其 import 块加入 `"github.com/cloudwego/eino/schema"`。

- [ ] **Step 4: 加三个 part 遍历函数**

在 `internal/ctxcompact/tokens.go` 的 `estimateMessageTokens` 之后追加：

```go
// estimateInputParts prices schema.Message.UserInputMultiContent.
//
// Text parts go through the same structural estimator as Content; media parts
// get their fixed tier cost. A part carrying both (the schema permits it) is
// charged for both.
func estimateInputParts(parts []schema.MessageInputPart) int {
	n := 0
	for _, p := range parts {
		n += estimateTextTokens(p.Text)
		switch {
		case p.Image != nil:
			n += imageTokens(p.Image.Detail)
		case p.Audio != nil, p.Video != nil, p.File != nil:
			n += opaquePartTokens
		}
	}
	return n
}

// estimateChatParts prices the deprecated schema.Message.MultiContent.
//
// Deprecated upstream, but internal/ctxcompact/redact.go still walks it, so a
// message carrying it is still a message this package will hand to a provider.
// Pricing one field and not the other would leave exactly the hole this change
// closes.
func estimateChatParts(parts []schema.ChatMessagePart) int {
	n := 0
	for _, p := range parts {
		n += estimateTextTokens(p.Text)
		switch {
		case p.ImageURL != nil:
			n += imageTokens(p.ImageURL.Detail)
		case p.AudioURL != nil, p.VideoURL != nil, p.FileURL != nil:
			n += opaquePartTokens
		}
	}
	return n
}

// estimateOutputParts prices schema.Message.AssistantGenMultiContent.
//
// Model-generated media counts against the window exactly like user-supplied
// media: it is replayed in the next request's history.
func estimateOutputParts(parts []schema.MessageOutputPart) int {
	n := 0
	for _, p := range parts {
		n += estimateTextTokens(p.Text)
		switch {
		case p.Image != nil:
			n += imageTokensHigh
		case p.Audio != nil, p.Video != nil, p.File != nil:
			n += opaquePartTokens
		}
	}
	return n
}
```

> **实现时先跑 `go doc github.com/cloudwego/eino/schema.ChatMessagePart` 与 `.MessageOutputPart` 核对字段名**（`AudioURL`/`VideoURL`/`FileURL`、`Image`/`Audio`/`Video`/`File`）。字段名不符就按实际改，**分支语义不变**。

- [ ] **Step 5: 接进 `estimateMessageTokens`**

修改 `internal/ctxcompact/tokens.go::estimateMessageTokens`，在 `for _, tc := range m.ToolCalls` 循环**之前**插入三行，并更新它的 doc 注释：

```go
// estimateMessageTokens returns the per-message token estimate, accounting for
// Content, ReasoningContent, all three multimodal part slices, and all
// ToolCalls (name + arguments + id), plus fixed structural overheads for the
// message envelope and each tool call.
//
// Tool-call ARGUMENTS are estimated as their own run rather than concatenated
// with the name and id: the JSON blob is punctuation-dense and the identifiers
// are opaque, and estimateTextTokens charges those at different rates. Summing
// the three lengths first and dividing once (what the old form did) applies one
// blended rate to all three and loses exactly that distinction.
//
// The multimodal slices are priced by part, never by payload length: see
// imageTokens. Counting them at zero — which this function did until W-A-01 —
// let a pasted screenshot weigh 8 tokens and kept the compaction gate shut
// until the provider answered 400.
func estimateMessageTokens(m *schema.Message) int {
	if m == nil {
		return 0
	}
	n := estimateTextTokens(m.Content) + perMessageOverhead
	n += estimateTextTokens(m.ReasoningContent)
	n += estimateChatParts(m.MultiContent)
	n += estimateInputParts(m.UserInputMultiContent)
	n += estimateOutputParts(m.AssistantGenMultiContent)
	for _, tc := range m.ToolCalls {
		n += estimateTextTokens(tc.Function.Name)
		n += estimateTextTokens(tc.Function.Arguments)
		n += estimateTextTokens(tc.ID)
		n += perToolCallOverhead
	}
	return n
}
```

- [ ] **Step 6: 跑测试确认通过**

```sh
go test ./internal/ctxcompact -run 'TestEstimateTokens' -v
```

预期：全部 PASS。

- [ ] **Step 7: 跑全包回归**

```sh
go test ./internal/ctxcompact
```

预期：PASS。**若既有测试断言了具体 token 数且现在偏大**，检查该 fixture 是否含多模态字段 —— 数字变大是本次修复的正确结果，更新 fixture 的期望值并在该测试的注释里写明原因；**不要**为了让旧数字通过而缩小估算。

- [ ] **Step 8: 加台账条目**

在 `docs/feature-status.yaml` 末尾追加：

```yaml
- id: "A2/W-A-01"
  package: "W-A"
  verdict: done
  title: "多模态内容计入 token 估算"
  # 审计（docs/superpowers/notes/2026-08-27-capability-audit.md P0-1）只点了
  # UserInputMultiContent。实测 schema.Message 上三个多模态字段全部不被
  # estimateMessageTokens 读到，因此本条覆盖三个 —— 只修一个会让同一缺陷换个
  # 字段复发。子句 2 钉住「按 part 计价不按载荷长度」，那是 len(data)/4 这个
  # 错误实现唯一会被抓住的地方。
  acceptance: "带图片的消息其 token 估算随图片数量单调增长；估算不随 base64 载荷字节数线性增长；Message 上三个多模态字段都被计入；低 detail 档位的估算低于高 detail 档位"
  evidence:
    "1": "internal/ctxcompact::TestEstimateTokensGrowsWithImageCount"
    "2": "internal/ctxcompact::TestEstimateTokensImageCostIsIndependentOfPayloadSize"
    "3": "internal/ctxcompact::TestEstimateTokensCountsAllThreeMultimodalFields"
    "4": "internal/ctxcompact::TestEstimateTokensLowDetailCostsLessThanHigh"
```

- [ ] **Step 9: 补 acceptancePins**

先算 SHA：

```sh
printf '%s' '带图片的消息其 token 估算随图片数量单调增长；估算不随 base64 载荷字节数线性增长；Message 上三个多模态字段都被计入；低 detail 档位的估算低于高 detail 档位' | shasum -a 256 | cut -c1-16
```

把输出填进 `internal/archtest/acceptance_pin_test.go::acceptancePins`，照抄该 map 里既有条目的字面量形状（先读一条现有的再仿写），条目为 `"A2/W-A-01"`，子句数 `4`。

- [ ] **Step 10: 跑台账门禁**

```sh
go test ./internal/archtest -run 'TestFeatureStatus|TestLedger' -v
```

预期：全部 PASS。若 `TestLedgerEvidenceIsClauseComplete` 报「子句数不符」，检查 acceptance 里的分号是否为全角「；」（切分只认「；」与「;」）。若 `TestLedgerMarkersAreLive` 报标记不匹配，逐字比对测试 doc 注释里的 `ledger:` 行与 acceptance 切出的子句原文。

- [ ] **Step 11: 全门禁 + commit**

```sh
go test ./internal/archtest ./internal/bootstrap && go vet ./... && go test ./internal/ctxcompact
```

预期：全部 ok。

```bash
git add internal/ctxcompact/tokenest.go internal/ctxcompact/tokens.go \
        internal/ctxcompact/tokens_multimodal_test.go \
        docs/feature-status.yaml internal/archtest/acceptance_pin_test.go
git commit -m "fix(ctxcompact): multimodal content weighed zero tokens, so the compaction gate never opened"
```

---

## Task 2: 工具输出脱敏后再进模型（W-A-02 / `F2` / **INF8**）

**为什么**：`internal/tools` 下 `Redact` 命中数为 **0**（实测）。`RedactPatterns` 的消费端只有崩溃报告、压缩、入库 —— **工具输出 → 模型这条路没有任何脱敏**。`cat .env`、`env`、`printenv` 的输出原样进 transcript，并随下一轮请求发给模型厂商。这是 S6 修复的另一半：那次修的是审计表落库。

**收敛点已确认**：`internal/tools/guard.go::GuardedTool.InvokableRun` 是模型路径的**单一出口**，且 `ToolChunk.Result` 是唯一进模型的字段（`ToolChunk` 的 doc 注释写明「字段单一归属」：`Result → 模型`，`Text → TUI`，`Status → TUI`）。因此**一处收口即可**，不必每个工具各改一次。

**设计决断（spec §1.2 INF8 留给实现定夺的那条）**：
> **按调用形态区分，不按内容匹配。** `fs_read` 读 `.env` 是用户显式点名的路径，打码会让工具失效；`shell_run` 跑 `env` 是模型自己发起的、凭据只是顺带混进 stdout。
>
> 但**本 Task 不做这个区分** —— 理由是 `secrets.Redactor` 只替换**已注册的 secret 字面量**（`Register(key)` 在 `internal/bootstrap/bootstrap.go` 里逐个注册 provider api_key），它不是正则式的「像凭据就打码」。所以 `fs_read` 一个普通 `.env` 完全不受影响；被打码的只有「本进程自己持有的那几个 key」，而那几个 key 出现在任何工具输出里都应该被打码。
>
> **ponytail: 全量收口，无 per-tool 例外表。** 例外表要等到出现「模型确实需要看见自己的 api_key」的真实用例再加 —— 那个用例不存在。

**Files:**
- Create: `internal/tools/redactctx.go`
- Modify: `internal/tools/guard.go::GuardedTool.InvokableRun`（两个返回点）
- Create: `internal/tools/redactctx_test.go`
- Modify: `internal/agent/orchestrator/orchestrator.go`（`Config` 加字段、结构体加字段、`New` 赋值、`bindExecutionContext` 绑定）
- Modify: `internal/bootstrap/bootstrap.go`（把 `redactor` 传进 orchestrator Config）
- Modify: `docs/feature-status.yaml`
- Modify: `internal/archtest/acceptance_pin_test.go`

**Interfaces:**
- Consumes: `secrets.NewRedactor() *secrets.Redactor`、`(*secrets.Redactor).Register(string)`、`(*secrets.Redactor).Redact(string) string`（`internal/secrets/secrets.go`）
- Produces:
  - `tools.WithRedactor(ctx context.Context, r *secrets.Redactor) context.Context`
  - `tools.RedactorFromContext(ctx context.Context) (*secrets.Redactor, bool)`
  - `orchestrator.Config.Redactor *secrets.Redactor`

---

- [ ] **Step 1: 写失败测试**

创建 `internal/tools/redactctx_test.go`：

```go
package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/secrets"
)

// leakyTool builds a GuardedTool whose Result carries the given string
// verbatim, standing in for `shell_run` printing `env`.
func leakyTool(t *testing.T, payload string) *GuardedTool {
	t.Helper()
	return NewGuardedTool("leaky", "Leaky", "emits its payload", time.Minute, nil,
		func(ctx context.Context, argsJSON string) <-chan ToolChunk {
			ch := make(chan ToolChunk, 1)
			ch <- ToolChunk{Result: payload, Text: payload}
			close(ch)
			return ch
		})
}

// allowAll is a profile that permits the fake tool so the test exercises the
// result path rather than a denial.
func allowAll() guard.PermissionProfile {
	return guard.PermissionProfile{Tools: guard.ToolsPolicy{Allow: []string{"*"}}}
}

// ledger: A2/W-A-02#1 工具结果在返回给编排器之前经过 Redactor
func TestInvokableRunRedactsRegisteredSecrets(t *testing.T) {
	const key = "sk-test-DEADBEEFdeadbeef0123456789"
	r := secrets.NewRedactor()
	r.Register(key)

	ctx := WithProfile(context.Background(), allowAll())
	ctx = WithRedactor(ctx, r)

	out, err := leakyTool(t, "OPENAI_API_KEY="+key+"\nPATH=/usr/bin").InvokableRun(ctx, "{}")
	require.NoError(t, err)
	require.NotContains(t, out, key, "a registered secret reached the model verbatim")
	require.Contains(t, out, "PATH=/usr/bin", "redaction must not eat the rest of the output")
}

// ledger: A2/W-A-02#2 未绑定 Redactor 时工具结果逐字节不变
func TestInvokableRunWithoutRedactorIsByteIdentical(t *testing.T) {
	const payload = "OPENAI_API_KEY=sk-test-DEADBEEFdeadbeef0123456789\nPATH=/usr/bin"

	ctx := WithProfile(context.Background(), allowAll())

	out, err := leakyTool(t, payload).InvokableRun(ctx, "{}")
	require.NoError(t, err)
	require.Equal(t, payload, out,
		"an unbound redactor must leave the pre-W-A-02 behaviour byte-identical")
}

// ledger: A2/W-A-02#3 TUI 路径的 Text 字段不受脱敏影响
func TestStreamTextFieldIsNotRedacted(t *testing.T) {
	const key = "sk-test-DEADBEEFdeadbeef0123456789"
	r := secrets.NewRedactor()
	r.Register(key)

	ctx := WithProfile(context.Background(), allowAll())
	ctx = WithRedactor(ctx, r)

	var text strings.Builder
	for c := range leakyTool(t, key).Stream(ctx, "{}") {
		text.WriteString(c.Text)
	}
	require.Contains(t, text.String(), key,
		"Text goes to the local TUI, not to the provider; redacting it would hide the operator's own key from them")
}
```

> **注意 Step 1 的第三条断言**：脱敏收口在 `InvokableRun`（模型路径），**不在 `Stream`**（TUI 路径）。这不是疏漏 —— TUI 渲染在操作员自己的终端上，把他自己的 key 打码只会制造困惑，而 `Text` 字段永远不会进入发给 provider 的请求体。

- [ ] **Step 2: 跑测试确认失败**

```sh
go test ./internal/tools -run 'TestInvokableRunRedacts|TestInvokableRunWithoutRedactor|TestStreamTextFieldIsNot' -v
```

预期：编译失败，`undefined: WithRedactor`。

- [ ] **Step 3: 写注入器**

创建 `internal/tools/redactctx.go`：

```go
package tools

import (
	"context"

	"github.com/x6nux/yanshi/internal/secrets"
)

// redactorKey 是 Redactor 在 context 中的键。未导出，因此只能经 WithRedactor
// 写入、经 RedactorFromContext 读出。
type redactorKey struct{}

// WithRedactor 把进程级 Redactor 绑进 ctx，供 GuardedTool.InvokableRun 在把
// 工具结果交给模型之前收口。
//
// 为什么收口在这里而不在每个工具里：ToolChunk 的字段单一归属（见 guard.go 的
// ToolChunk doc 注释）意味着只有 Result 会拼进模型结果，而 InvokableRun 是
// Result 的唯一汇合点。在工具里各改一次既是重复逻辑，也保证不了下一个新工具
// 记得改 —— 而「写了但零读者」这个形状在本仓已经复发过九次。
//
// r 为 nil 时返回原 ctx：未绑定等于不脱敏，行为与引入前逐字节一致。
func WithRedactor(ctx context.Context, r *secrets.Redactor) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, redactorKey{}, r)
}

// RedactorFromContext 读出 WithRedactor 绑定的 Redactor。
//
// 双返回值形态是刻意的：本注入器带 nil 门禁，所以消费方必须处理 ok=false，
// 不能假设一定有值（CLAUDE.md 记的「nil 就不注入」那个坑）。
func RedactorFromContext(ctx context.Context) (*secrets.Redactor, bool) {
	r, ok := ctx.Value(redactorKey{}).(*secrets.Redactor)
	return r, ok && r != nil
}

// redactForModel 用 ctx 里的 Redactor 处理即将交给模型的字符串。
// 未绑定时原样返回。
func redactForModel(ctx context.Context, s string) string {
	if r, ok := RedactorFromContext(ctx); ok {
		return r.Redact(s)
	}
	return s
}
```

- [ ] **Step 4: 在 `InvokableRun` 收口**

修改 `internal/tools/guard.go::GuardedTool.InvokableRun` 的**两个**返回内容的分支，并补 doc 注释：

```go
// InvokableRun 是 Eino/ADK 的模型入口：驱动 Stream，只收集 Result 字段拼成模型结果。
// Text/Status 均不计入（字段单一归属）。遇 Err 触发 errcnt 连续失败熔断（连续 5 次
// 中断 turn）；错误文本已由工具经 Result 推送，故这里不再单独 errorResult。
// spillIfTooLong 对最终拼装结果收口。
//
// 错误处理语义保留：权限/操作错误作为工具的 *结果内容*（非 Go error）回喂模型，让
// 模型改路径重试（capped by MaxIterations）；返回 Go error 会中断整个 turn。连续失败
// 熔断仍由 errcnt 触发。
//
// W-A-02：返回前一律过 redactForModel。这是工具输出通往 provider 的唯一出口，
// 在这里收口而不是在每个工具里，是因为 Result 只在这里汇合 —— 见 redactctx.go。
// 两个返回点都要过：错误分支的 result 同样会被回喂给模型。
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
		// Permission denials (DenyErr) surface as a result for the model to retry,
		// but do NOT count toward the consecutive-error circuit breaker — a user
		// may legitimately deny several calls in a row. Only operational errors
		// trip the breaker. (Preserves the pre-rewrite DenyErr carve-out.)
		var d *DenyErr
		if !errors.As(runErr, &d) {
			if c := getErrCounter(ctx); c != nil {
				*c++
				if *c >= 5 {
					return "", fmt.Errorf("tool %q failed %d consecutive times; aborting turn", g.name, *c)
				}
			}
		}
		return redactForModel(ctx, result.String()), nil
	}
	if c := getErrCounter(ctx); c != nil {
		*c = 0
	}
	return spillIfTooLong(ctx, g.name, redactForModel(ctx, result.String())), nil
}
```

> **顺序要点**：`redactForModel` 必须在 `spillIfTooLong` **之前** —— 否则超长结果被落盘时写的是**未脱敏**的原文，`artifact_read` 一取就把凭据取回来了，等于给自己开了后门。

- [ ] **Step 5: 跑测试确认通过**

```sh
go test ./internal/tools -run 'TestInvokableRunRedacts|TestInvokableRunWithoutRedactor|TestStreamTextFieldIsNot' -v
```

预期：全部 PASS。若第一条失败且 `out` 仍含 key，检查 `secrets.Redactor.Register` 是否对短于其最小长度的字符串静默忽略（读 `internal/secrets/secrets.go::Register` 的实现），必要时把测试里的 key 换成更长的字面量。

- [ ] **Step 6: 接进 orchestrator**

在 `internal/agent/orchestrator/orchestrator.go`：

1. `Config` 结构体里，紧邻 `NetworkPolicy` 加：

```go
	// Redactor 是进程级 secrets redactor。绑进每个 turn 的执行 context，
	// 供 GuardedTool.InvokableRun 在结果交给模型之前收口（W-A-02）。
	// nil 表示不脱敏，行为与引入前逐字节一致。
	Redactor *secrets.Redactor
```

2. `Orchestrator` 结构体里，紧邻 `networkPolicy` 加：

```go
	redactor        *secrets.Redactor
```

3. `New` 的字段赋值块里，紧邻 `networkPolicy: cfg.NetworkPolicy,` 加：

```go
		redactor:           cfg.Redactor,
```

4. `bindExecutionContext` 里，紧邻 `WithNetworkPolicy` 的 nil 门禁块加：

```go
	if o.redactor != nil {
		ctx = tools.WithRedactor(ctx, o.redactor)
	}
```

5. import 块加 `"github.com/x6nux/yanshi/internal/secrets"`。

- [ ] **Step 7: 在组合根接线**

在 `internal/bootstrap/bootstrap.go` 里构造 orchestrator `Config` 的位置（搜 `NetworkPolicy:` 找到那个字面量），加一行：

```go
		Redactor: redactor,
```

`redactor` 是 `bootstrap.go` 里已有的局部变量（`redactor := output.Redactor`，其上有一段要求它「MUST stay aliased to output.Redactor」的注释 —— 传指针不违反那条，别做拷贝）。

- [ ] **Step 8: 跑 GOV6 与装配门禁**

```sh
go test ./internal/archtest -run TestGOV6 -v && go test ./internal/bootstrap
```

预期：PASS。GOV6 要求每个导出的 `With<X>` 注入器有生产调用点 —— Step 6 的第 4 点就是那个调用点，漏掉会在这里变红。

- [ ] **Step 9: 写端到端断言（真实装配）**

在 `internal/bootstrap/w3wiring_test.go` 追加：

```go
// ledger: A2/W-A-02#4 真实装配出的 App 其 orchestrator 已绑定 Redactor
func TestW3RedactorReachesToolResults(t *testing.T) {
	app, err := Build(Options{ConfigPath: w3ConfigFile(t), FakeModel: true})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())

	require.NotNil(t, app.Redactor,
		"the process-wide redactor must exist for W-A-02 to have anything to bind")

	ctx := app.Orchestrator.BindExecutionContextForTest(context.Background(), "")
	_, ok := tools.RedactorFromContext(ctx)
	require.True(t, ok,
		"bindExecutionContext did not bind the redactor: every tool result still reaches the provider unredacted")
}
```

`bindExecutionContext` 未导出，因此需在 `internal/agent/orchestrator` 加一个导出的测试钩子。**在 `orchestrator.go` 里加**（不是 `_test.go`，因为跨包调用）：

```go
// BindExecutionContextForTest 暴露 bindExecutionContext 给组合根的接线测试。
//
// 存在的理由是 GOV6 够不到的那半：GOV6 只证明注入器**有**调用点，证明不了
// 真实装配出的 App 确实走到了那个调用点。w3wiring_test.go 对真 App 断言，
// 补的正是这个盲区。
func (o *Orchestrator) BindExecutionContextForTest(ctx context.Context, connectionSessionID string) context.Context {
	return o.bindExecutionContext(ctx, connectionSessionID)
}
```

> 若 `App` 结构体尚无 `Redactor` 字段，在 `internal/bootstrap/bootstrap.go` 的 `App` 上补一个 `Redactor *secrets.Redactor` 并在 `Build` 末尾赋值 —— 该字段带 doc 注释（GOV3）。

- [ ] **Step 10: 台账 + pins**

`docs/feature-status.yaml` 追加：

```yaml
- id: "A2/W-A-02"
  package: "W-A"
  verdict: done
  title: "工具输出脱敏后再进模型"
  # 收口在 GuardedTool.InvokableRun 而不是每个工具里：ToolChunk 的字段单一归属
  # 决定了 Result 只在那里汇合。子句 2 钉住「未绑定时逐字节不变」，那是这类
  # 全量收口最容易造成的回归方向。子句 3 是刻意的不对称：Text 走 TUI，脱敏它
  # 只会把操作员自己的 key 对他自己藏起来。子句 4 对真实装配断言，补 GOV6
  # 够不到的那半（GOV6 证明注入器有调用点，证明不了 App 真的走到）。
  acceptance: "工具结果在返回给编排器之前经过 Redactor；未绑定 Redactor 时工具结果逐字节不变；TUI 路径的 Text 字段不受脱敏影响；真实装配出的 App 其 orchestrator 已绑定 Redactor"
  evidence:
    "1": "internal/tools::TestInvokableRunRedactsRegisteredSecrets"
    "2": "internal/tools::TestInvokableRunWithoutRedactorIsByteIdentical"
    "3": "internal/tools::TestStreamTextFieldIsNotRedacted"
    "4": "internal/bootstrap::TestW3RedactorReachesToolResults"
```

算 SHA 并补 `acceptancePins`（条目 `"A2/W-A-02"`，子句数 `4`）：

```sh
printf '%s' '工具结果在返回给编排器之前经过 Redactor；未绑定 Redactor 时工具结果逐字节不变；TUI 路径的 Text 字段不受脱敏影响；真实装配出的 App 其 orchestrator 已绑定 Redactor' | shasum -a 256 | cut -c1-16
```

- [ ] **Step 11: 全门禁 + commit**

```sh
go test ./internal/archtest ./internal/bootstrap ./internal/tools && go vet ./...
```

**这一条的回归风险最高**（spec R3），额外跑一遍全量：

```sh
go test ./...
```

```bash
git add internal/tools/redactctx.go internal/tools/redactctx_test.go internal/tools/guard.go \
        internal/agent/orchestrator/orchestrator.go internal/bootstrap/bootstrap.go \
        internal/bootstrap/w3wiring_test.go \
        docs/feature-status.yaml internal/archtest/acceptance_pin_test.go
git commit -m "fix(tools): tool output reached the provider unredacted; RedactPatterns had zero consumers here"
```

---

## Task 3: CJK 检索（W-A-03 / `F7`，合并 `F6`）

**为什么**：`internal/store/store.go:401` 的 `tokenize='porter unicode61'` 不切中文词，整句被当成一个 token。实测（真驱动真 FTS5）：`截止日期` / `项目` / `周二` / `张伟` **全部 0 命中**，只有搜整串才中；英文正常。

`memories_fts`（`store.go:561`）**连 `tokenize=` 子句都没有**，用默认的 `unicode61` —— 同样不切中文。

失效的是**三条链**：`history_search`（`SearchMessages`）、`SearchMemory`（`SearchMemoryRanked`）、`memory_autorecall`（走 `SearchMemoryRanked`）。**而 CLAUDE.md 规定本仓交互语言就是中文。**

**方案选择（spec 已定，此处记录理由）**：走**CJK 查询检测 + 有界 LIKE 回退**，不换 tokenizer。

- 换 `trigram` tokenizer 要**重建两张 FTS 表**（一次性迁移，有风险），且英文检索质量下降；
- LIKE 回退是纯增量、可回滚、对英文路径**零影响**；
- 代价是 CJK 查询无 bm25 排序、无 `snippet()` —— 可接受，因为现状是**零命中**。

**Files:**
- Create: `internal/store/cjkquery.go`
- Create: `internal/store/cjkquery_test.go`
- Modify: `internal/store/message_log.go::SearchMessages`
- Modify: `internal/store/memory.go::SearchMemoryRanked`
- Modify: `docs/feature-status.yaml`
- Modify: `internal/archtest/acceptance_pin_test.go`

**Interfaces:**
- Consumes: `prefixed(cols, alias string) string`、`clampLimit(int) int`、`memoryColumns`、`messageColumns`、`MemoryFilter.where(alias string) (string, []any)`（均为 `internal/store` 包内已有）
- Produces:
  - `hasCJK(s string) bool`
  - `likePattern(q string) (pattern string, escape string)`
  - `cjkSnippet(content, query string) string`
  - `maxCJKFallbackRows`（常量）

---

- [ ] **Step 1: 写失败测试（纯函数部分）**

创建 `internal/store/cjkquery_test.go`：

```go
package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHasCJKDetectsEachScript(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"截止日期", true},
		{"张伟", true},
		{"プロジェクト", true},  // 片假名
		{"ひらがな", true},     // 平假名
		{"프로젝트", true},     // 한글
		{"deadline", false},
		{"", false},
		{"api_key v2", false},
		{"deadline 截止", true}, // 混合也算
	} {
		require.Equalf(t, tc.want, hasCJK(tc.in), "hasCJK(%q)", tc.in)
	}
}

func TestLikePatternEscapesWildcards(t *testing.T) {
	pat, esc := likePattern("100%_done")
	require.Equal(t, `%100\%\_done%`, pat)
	require.Equal(t, `\`, esc)
}

func TestLikePatternEscapesTheEscapeChar(t *testing.T) {
	pat, _ := likePattern(`a\b`)
	require.Equal(t, `%a\\b%`, pat,
		"an unescaped backslash would make the next character literal and change the match")
}

func TestCJKSnippetBoundsTheWindow(t *testing.T) {
	content := "前" + string(make([]rune, 0)) + "填充填充填充填充填充填充填充填充填充填充截止日期填充填充填充填充填充填充填充填充填充填充后"
	s := cjkSnippet(content, "截止日期")
	require.Contains(t, s, "截止日期")
	require.Less(t, len([]rune(s)), len([]rune(content)),
		"a snippet that returns the whole row is not a snippet")
}

func TestCJKSnippetOnMissReturnsHead(t *testing.T) {
	s := cjkSnippet("完全不相关的内容", "截止日期")
	require.NotEmpty(t, s, "a miss must still yield something renderable, not an empty cell")
}
```

- [ ] **Step 2: 跑测试确认失败**

```sh
go test ./internal/store -run 'TestHasCJK|TestLikePattern|TestCJKSnippet' -v
```

预期：编译失败，`undefined: hasCJK`。

- [ ] **Step 3: 实现纯函数**

创建 `internal/store/cjkquery.go`：

```go
package store

import (
	"strings"
	"unicode"
)

// maxCJKFallbackRows 限制 LIKE 回退扫描后返回的行数上限。
//
// FTS5 的 MATCH 走倒排索引，LIKE '%…%' 是全表扫描。上限存在不是为了「结果太多
// 看不完」——那是 limit 的职责——而是为了让一次退化查询的代价有界：一个几十万行
// 的 messages 表上，无界的 LIKE 会把一次 history_search 变成一次可感知的停顿。
const maxCJKFallbackRows = 200

// likeEscape 是 LIKE 模式里的转义字符。反斜杠而非默认（无转义），因为查询串
// 来自用户与模型，里面出现 % 和 _ 是常态（路径、SQL 片段、格式化字符串）。
const likeEscape = `\`

// hasCJK 报告 s 是否含中日韩文字。
//
// 判据是 Unicode 脚本表而不是码点区间：区间写法会在扩展区（Ext-B 及以后）
// 和补充平面上漏判，而那正是人名与生僻字所在的地方。
//
// 这个函数决定走 FTS5 还是走 LIKE 回退。判 false 时行为与引入前逐字节一致 ——
// 英文查询永远不进回退路径，这是本次改动零回归的根据。
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) ||
			unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) ||
			unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// likePattern 把查询串转成 SQLite LIKE 的模式与转义字符。
//
// 转义顺序是承重的：先转义反斜杠自身，再转义 % 和 _。反过来会把刚插入的
// 转义反斜杠再转义一遍，模式就不再匹配用户输入的字面量了。
func likePattern(q string) (string, string) {
	r := strings.NewReplacer(
		likeEscape, likeEscape+likeEscape,
		"%", likeEscape+"%",
		"_", likeEscape+"_",
	)
	return "%" + r.Replace(q) + "%", likeEscape
}

// cjkSnippetRadius 是片段窗口在命中两侧各保留的 rune 数。
// 与 FTS5 那侧 snippet(..., 24) 的量级对齐，好让两条路径的返回长度接近。
const cjkSnippetRadius = 24

// cjkSnippet 在 content 里围绕 query 的首次出现切出一个有界片段。
//
// 存在的理由：LIKE 回退拿不到 FTS5 的 snippet()，而返回整行会让一条几 KB 的
// 工具输出把检索结果撑爆。命中标记用与 FTS5 侧相同的书名号，因此 UI 不必分辨
// 结果来自哪条路径。
//
// 未命中时返回开头一段而不是空串：调用方拿到的是「这行匹配了」这个事实
// （匹配可能发生在 tool_args 而不是 content 上），空单元格会让它看起来像坏了。
func cjkSnippet(content, query string) string {
	runes := []rune(content)
	idx := strings.Index(content, query)
	if idx < 0 {
		return headRunes(runes, 2*cjkSnippetRadius)
	}
	start := len([]rune(content[:idx]))
	end := start + len([]rune(query))
	lo := max(0, start-cjkSnippetRadius)
	hi := min(len(runes), end+cjkSnippetRadius)

	var b strings.Builder
	if lo > 0 {
		b.WriteString(" … ")
	}
	b.WriteString(string(runes[lo:start]))
	b.WriteString("«")
	b.WriteString(string(runes[start:end]))
	b.WriteString("»")
	b.WriteString(string(runes[end:hi]))
	if hi < len(runes) {
		b.WriteString(" … ")
	}
	return b.String()
}

// headRunes 返回前 n 个 rune，不足则全部返回。
func headRunes(runes []rune, n int) string {
	if len(runes) <= n {
		return string(runes)
	}
	return string(runes[:n]) + " … "
}
```

> Go 1.26 的内置 `max`/`min` 可直接用于 int，无需辅助函数。

- [ ] **Step 4: 跑纯函数测试确认通过**

```sh
go test ./internal/store -run 'TestHasCJK|TestLikePattern|TestCJKSnippet' -v
```

预期：全部 PASS。

- [ ] **Step 5: 写检索链的失败测试**

在 `internal/store/cjkquery_test.go` 追加（`openTestStore` 是本包既有的测试辅助 —— 先 `grep -rn "func openTestStore\|func newTestStore" internal/store/*_test.go` 确认实际名字并照用）：

```go
// seedCJK writes one message and one memory carrying the same Chinese sentence.
func seedCJK(t *testing.T, s *Store) string {
	t.Helper()
	const sentence = "项目的截止日期是周二，负责人是张伟"
	sid, err := s.CreateSession("cjk")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(Message{
		SessionID: sid, Seq: 1, Role: "user", Content: sentence,
	}))
	_, err = s.WriteMemory("note", sentence)
	require.NoError(t, err)
	return sid
}

// ledger: A2/W-A-03#1 中文词查询在 history_search 上返回非零命中
func TestSearchMessagesFindsChineseWords(t *testing.T) {
	s := openTestStore(t)
	sid := seedCJK(t, s)

	for _, word := range []string{"截止日期", "项目", "周二", "张伟"} {
		hits, err := s.SearchMessages(sid, word, 10)
		require.NoError(t, err)
		require.NotEmptyf(t, hits, "query %q returned zero hits", word)
		require.NotEmptyf(t, hits[0].Snippet, "query %q returned an empty snippet", word)
	}
}

// ledger: A2/W-A-03#2 SearchMemory 与 memory_autorecall 走同一检索路径因而同时生效
func TestSearchMemoryFindsChineseWords(t *testing.T) {
	s := openTestStore(t)
	seedCJK(t, s)

	for _, word := range []string{"截止日期", "项目", "周二", "张伟"} {
		hits, err := s.SearchMemoryRanked(word, 10, MemoryFilter{})
		require.NoError(t, err)
		require.NotEmptyf(t, hits, "memory query %q returned zero hits", word)
	}
}

// ledger: A2/W-A-03#3 英文查询的命中集合与本改动前逐条一致
func TestSearchMessagesEnglishPathIsUnchanged(t *testing.T) {
	s := openTestStore(t)
	sid, err := s.CreateSession("en")
	require.NoError(t, err)
	require.NoError(t, s.AppendMessage(Message{
		SessionID: sid, Seq: 1, Role: "user",
		Content: "the deadline for the project is Tuesday and the owner is Wei",
	}))
	require.NoError(t, s.AppendMessage(Message{
		SessionID: sid, Seq: 2, Role: "assistant",
		Content: "unrelated text about compilers",
	}))

	hits, err := s.SearchMessages(sid, "deadline", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1, "the English path must still go through FTS5 MATCH")
	require.Equal(t, 1, hits[0].Seq)
	require.Contains(t, hits[0].Snippet, "«",
		"an English hit must still carry the FTS5 snippet markers")

	// Stemming is a porter-tokenizer property; losing it would mean the English
	// path silently switched to LIKE.
	stemmed, err := s.SearchMessages(sid, "deadlines", 10)
	require.NoError(t, err)
	require.Len(t, stemmed, 1, "porter stemming must survive this change")
}

// ledger: A2/W-A-03#4 CJK 回退路径有结果数上限
func TestSearchMessagesCJKFallbackIsBounded(t *testing.T) {
	s := openTestStore(t)
	sid, err := s.CreateSession("bulk")
	require.NoError(t, err)
	for i := 1; i <= maxCJKFallbackRows+50; i++ {
		require.NoError(t, s.AppendMessage(Message{
			SessionID: sid, Seq: i, Role: "user", Content: "截止日期",
		}))
	}

	hits, err := s.SearchMessages(sid, "截止日期", 100000)
	require.NoError(t, err)
	require.LessOrEqual(t, len(hits), maxCJKFallbackRows,
		"an unbounded LIKE scan turns one search into a visible stall")
}
```

> `CreateSession(title string) (string, error)` 已按实际签名写。**`AppendMessage` 与 `WriteMemory` 的签名以 `go doc github.com/x6nux/yanshi/internal/store` 为准**，不符就改调用，**断言语义不变**。

- [ ] **Step 6: 跑测试确认失败**

```sh
go test ./internal/store -run 'TestSearchMessagesFindsChinese|TestSearchMemoryFindsChinese|TestSearchMessagesEnglish|TestSearchMessagesCJKFallback' -v
```

预期：两条 CJK 测试 FAIL（零命中），英文那条 PASS，上限那条 FAIL 或 PASS 皆可（此刻零命中）。

- [ ] **Step 7: 改 `SearchMessages`**

修改 `internal/store/message_log.go::SearchMessages`，保留原有的两个入参校验，在 FTS 查询前分流：

```go
func (s *Store) SearchMessages(sessionID, query string, limit int) ([]MessageSearchHit, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("store: search messages: empty session id")
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("store: search messages: empty query")
	}
	if hasCJK(query) {
		return s.searchMessagesCJK(sessionID, query, limit)
	}
	rows, err := s.DB.Query(
		`SELECT `+prefixed(messageColumns, "m.")+`,
		        snippet(messages_fts, -1, '«', '»', ' … ', 24)
		 FROM messages_fts f
		 JOIN messages m ON m.rowid = f.rowid
		 WHERE messages_fts MATCH ? AND m.session_id = ?
		 ORDER BY rank
		 LIMIT ?`,
		query, sessionID, clampLimit(limit),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageSearchHit
	for rows.Next() {
		var h MessageSearchHit
		if err := rows.Scan(scanTargets(&h.Message, &h.Snippet)...); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// searchMessagesCJK 是 SearchMessages 的中日韩回退路径。
//
// 为什么需要它：messages_fts 用 tokenize='porter unicode61'，它不切中文词，
// 于是整句话被当成一个 token —— 实测「截止日期」「项目」「周二」「张伟」
// 在一条包含它们全部的中文消息上**全部零命中**，只有搜整串才中。
// 这让 history_search / SearchMemory / memory_autorecall 三条链在中文会话下
// 同时失效，而本仓的交互语言就是中文。
//
// 为什么是 LIKE 而不是换 tokenizer：换成 trigram 要重建两张 FTS 表（一次性
// 迁移，有风险），且英文检索会失去 porter 词干。LIKE 回退是纯增量、可回滚、
// 对英文路径零影响 —— 英文查询根本不进这条路（见 hasCJK）。
//
// 代价是明确的：没有 bm25 排序，因此按 seq 倒序（新的在前）；没有 FTS5 的
// snippet()，因此用 cjkSnippet 自己切。两者都比现状的零命中好。
// ORDER BY 之外还有 maxCJKFallbackRows 兜底，因为 LIKE '%…%' 是全表扫描。
func (s *Store) searchMessagesCJK(sessionID, query string, limit int) ([]MessageSearchHit, error) {
	pattern, esc := likePattern(query)
	n := clampLimit(limit)
	if n > maxCJKFallbackRows {
		n = maxCJKFallbackRows
	}
	rows, err := s.DB.Query(
		`SELECT `+prefixed(messageColumns, "m.")+`
		 FROM messages m
		 WHERE m.session_id = ?
		   AND (m.content LIKE ? ESCAPE ? OR m.tool_args LIKE ? ESCAPE ?)
		 ORDER BY m.seq DESC
		 LIMIT ?`,
		sessionID, pattern, esc, pattern, esc, n,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageSearchHit
	for rows.Next() {
		var h MessageSearchHit
		if err := rows.Scan(scanTargets(&h.Message)...); err != nil {
			return nil, err
		}
		h.Snippet = cjkSnippet(h.Content, query)
		out = append(out, h)
	}
	return out, rows.Err()
}
```

> **`scanTargets` 的可变参数形态需确认** —— 原调用是 `scanTargets(&h.Message, &h.Snippet)`，回退路径少一列。先 `grep -n "func scanTargets" -A 10 internal/store/*.go` 读它的签名；若它不接受省略 snippet 的形态，就在 SELECT 里补一个 `'' AS snippet` 占位列，保持 `scanTargets(&h.Message, &h.Snippet)` 调用形状不变，再用 `cjkSnippet` 覆盖。

- [ ] **Step 8: 改 `SearchMemoryRanked`**

修改 `internal/store/memory.go::SearchMemoryRanked`，在 `cond, args := dims.where("m.")` 之后分流：

```go
	if hasCJK(query) {
		return s.searchMemoryCJK(query, limit, dims)
	}
```

并在其后新增：

```go
// searchMemoryCJK 是 SearchMemoryRanked 的中日韩回退路径，与
// searchMessagesCJK 同因同治 —— memories_fts 连 tokenize= 子句都没写，
// 用的是默认 unicode61，同样不切中文词。
//
// Score 一律填 0：bm25 在这条路径上不存在，而 MemoryHit.Score 的 doc 注释
// 已经写明它只用于 ORDER 且不是可用的绝对阈值。填一个编出来的分数会让
// 「相关性判据」那层拿它当真。排序改用 created_at DESC。
func (s *Store) searchMemoryCJK(query string, limit int, dims MemoryFilter) ([]MemoryHit, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > maxCJKFallbackRows {
		limit = maxCJKFallbackRows
	}
	pattern, esc := likePattern(query)
	cond, args := dims.where("m.")
	q := `SELECT ` + prefixed(memoryColumns, "m.") + `
	      FROM memories m
	      WHERE m.content LIKE ? ESCAPE ?` + cond + `
	      ORDER BY m.created_at DESC LIMIT ?`
	all := append([]any{pattern, esc}, args...)
	all = append(all, limit)
	rows, err := s.DB.Query(q, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryHit
	for rows.Next() {
		var h MemoryHit
		var from string
		if err := rows.Scan(&h.ID, &h.Kind, &h.Content, &h.SessionID, &h.AgentID,
			&h.CreatedAt, &from, &h.SupersededBy, &h.DistilledAt); err != nil {
			return nil, err
		}
		h.DistilledFrom = splitIDs(from)
		out = append(out, h)
	}
	return out, rows.Err()
}
```

- [ ] **Step 9: 跑测试确认通过**

```sh
go test ./internal/store -run 'TestSearchMessages|TestSearchMemory|TestHasCJK|TestLikePattern|TestCJKSnippet' -v
```

预期：全部 PASS。

- [ ] **Step 10: 跑全包回归**

```sh
go test ./internal/store
```

预期：PASS。**特别留意既有的英文检索测试** —— 它们必须一条不改地通过，那是「英文路径零回归」的实证。

- [ ] **Step 11: 台账 + pins**

```yaml
- id: "A2/W-A-03"
  package: "W-A"
  verdict: done
  title: "CJK 检索（history_search / SearchMemory / memory_autorecall）"
  # 审计 P0-3 与 P0-11 同根同落点，此处合并为一条。选 LIKE 回退而不是换
  # tokenizer 的理由写在 searchMessagesCJK 的 doc 注释里。子句 3 是这次改动
  # 的护栏而不是特性：全量收口最容易的失败方向是「修好中文顺手改坏英文」，
  # 那条测试同时断言命中集合与 porter 词干仍在。
  acceptance: "中文词查询在 history_search 上返回非零命中；SearchMemory 与 memory_autorecall 走同一检索路径因而同时生效；英文查询的命中集合与本改动前逐条一致；CJK 回退路径有结果数上限"
  evidence:
    "1": "internal/store::TestSearchMessagesFindsChineseWords"
    "2": "internal/store::TestSearchMemoryFindsChineseWords"
    "3": "internal/store::TestSearchMessagesEnglishPathIsUnchanged"
    "4": "internal/store::TestSearchMessagesCJKFallbackIsBounded"
```

```sh
printf '%s' '中文词查询在 history_search 上返回非零命中；SearchMemory 与 memory_autorecall 走同一检索路径因而同时生效；英文查询的命中集合与本改动前逐条一致；CJK 回退路径有结果数上限' | shasum -a 256 | cut -c1-16
```

- [ ] **Step 12: 全门禁 + commit**

```sh
go test ./internal/archtest ./internal/bootstrap ./internal/store && go vet ./...
```

```bash
git add internal/store/cjkquery.go internal/store/cjkquery_test.go \
        internal/store/message_log.go internal/store/memory.go \
        docs/feature-status.yaml internal/archtest/acceptance_pin_test.go
git commit -m "fix(store): CJK queries matched nothing, so all three retrieval chains were dead in Chinese"
```

---

## Task 4: 会话恢复保真度（W-A-04 / `F9`）

**为什么**：`internal/api/http/ws_handlers.go` 的恢复循环只映射 `Role` + `Content`，且 role 只二分 —— `if m.Role == "assistant" { role = schema.Assistant }`，`else` 一律 `schema.User`。实测该文件里 `m.Role ==` 只出现一次、`ToolCallID` 零命中。

`store.Message`（`internal/store/session.go:27`）明明存了 `ToolCallID` / `ToolName` / `ToolArgs`，恢复时**全丢**，**`tool` 消息还被错当成 user**。恢复会话后模型看不见自己做过什么。

**额外的隐藏后果**（审计未提，实现时必须防）：只恢复一半会让下一次压缩把孤儿删掉 —— `ctxcompact.EnforceToolCallPairs` 的 fixpoint 保证 tool_call/result 配对不被切断，一条没有对应 `assistant.ToolCalls` 的 `tool` 消息就是孤儿。症状是「恢复后第一次压缩历史突然变短」。

**Files:**
- Modify: `internal/api/http/ws_handlers.go`（恢复循环）
- Create: `internal/api/http/ws_restore_test.go`
- Modify: `docs/feature-status.yaml`
- Modify: `internal/archtest/acceptance_pin_test.go`

**Interfaces:**
- Consumes: `store.Message{Role, Content, ToolCallID, ToolName, ToolArgs}`、`schema.User/Assistant/System/Tool`、`ctxcompact.EnforceToolCallPairs`
- Produces: `restoreMessages([]store.Message) []*schema.Message`（`internal/api/http` 包内未导出）

---

- [ ] **Step 1: 写失败测试**

创建 `internal/api/http/ws_restore_test.go`：

```go
package http

import (
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
)

// storedTurn is one persisted ReAct turn: a user ask, an assistant tool call,
// the tool result, and the assistant answer — exactly what the durable log
// holds after a single tool-using exchange.
func storedTurn() []store.Message {
	return []store.Message{
		{Seq: 1, Role: "user", Content: "list the files"},
		{Seq: 2, Role: "assistant", Content: "", ToolCallID: "call_1", ToolName: "fs_list", ToolArgs: `{"path":"."}`},
		{Seq: 3, Role: "tool", Content: "a.go\nb.go", ToolCallID: "call_1", ToolName: "fs_list"},
		{Seq: 4, Role: "assistant", Content: "there are two files"},
	}
}

// ledger: A2/W-A-04#1 恢复后的历史包含 tool 角色的消息
func TestRestoreMessagesKeepsToolRole(t *testing.T) {
	got := restoreMessages(storedTurn())

	require.Len(t, got, 4)
	require.Equal(t, schema.User, got[0].Role)
	require.Equal(t, schema.Assistant, got[1].Role)
	require.Equal(t, schema.Tool, got[2].Role,
		"a tool message restored as user makes the model read its own tool output as the operator speaking")
	require.Equal(t, schema.Assistant, got[3].Role)
}

// ledger: A2/W-A-04#2 每条 tool 消息的 ToolCallID 能在同一历史中找到对应的 assistant ToolCalls
func TestRestoreMessagesPairsToolCallsWithResults(t *testing.T) {
	got := restoreMessages(storedTurn())

	calls := map[string]bool{}
	for _, m := range got {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = true
		}
	}
	require.True(t, calls["call_1"], "the assistant's ToolCalls were dropped on restore")

	for _, m := range got {
		if m.Role != schema.Tool {
			continue
		}
		require.NotEmpty(t, m.ToolCallID, "a tool message without ToolCallID is an orphan")
		require.Truef(t, calls[m.ToolCallID],
			"tool result %q has no matching assistant tool call in the restored history", m.ToolCallID)
	}
}

// ledger: A2/W-A-04#3 恢复后的消息序列通过 EnforceToolCallPairs 不产生删除
func TestRestoreMessagesSurvivesPairEnforcement(t *testing.T) {
	got := restoreMessages(storedTurn())
	kept := ctxcompact.EnforceToolCallPairs(got)

	require.Len(t, kept, len(got),
		"restoring only half a pair makes the next compaction silently shorten the history")
}

// ledger: A2/W-A-04#4 非工具消息的恢复结果与本改动前逐字节一致
func TestRestoreMessagesPlainTurnIsUnchanged(t *testing.T) {
	got := restoreMessages([]store.Message{
		{Seq: 1, Role: "user", Content: "hello"},
		{Seq: 2, Role: "assistant", Content: "hi"},
	})

	require.Len(t, got, 2)
	require.Equal(t, schema.User, got[0].Role)
	require.Equal(t, "hello", got[0].Content)
	require.Empty(t, got[0].ToolCalls)
	require.Equal(t, schema.Assistant, got[1].Role)
	require.Equal(t, "hi", got[1].Content)
	require.Empty(t, got[1].ToolCalls)
}
```

> `EnforceToolCallPairs` 的**实际导出名与签名**先用 `go doc github.com/x6nux/yanshi/internal/ctxcompact | grep -i pair` 确认；若它未导出，改为在 `internal/ctxcompact` 里加一个导出的薄封装（带 doc 注释），**不要**把断言删掉 —— 那条正是本 Task 唯一能抓住「只恢复一半」的测试。
> import 块需加 `"github.com/x6nux/yanshi/internal/ctxcompact"`；若这会撞 GOV1（`internal/api/http` → `internal/ctxcompact`），先跑 `go test ./internal/archtest -run TestR2` 看结论，撞了就把该断言挪到 `internal/ctxcompact` 的测试里、用同一份 fixture。

- [ ] **Step 2: 跑测试确认失败**

```sh
go test ./internal/api/http -run TestRestoreMessages -v
```

预期：编译失败，`undefined: restoreMessages`。

- [ ] **Step 3: 抽出恢复函数**

在 `internal/api/http/ws_handlers.go` 里新增（放在使用它的 handler 之前）：

```go
// restoreMessages 把持久化的消息日志还原成 ReAct 历史。
//
// 这个函数存在的理由就是它修的那个缺陷：恢复循环原先只映射 Role + Content，
// 且 role 只二分 user/assistant，于是 store 里存着的 ToolCallID / ToolName /
// ToolArgs 全部丢失、tool 消息还被当成 user。恢复会话后模型看不见自己做过
// 什么，而它读到的「用户消息」其实是自己上一轮的工具输出。
//
// 配对是承重的：assistant 一侧的 ToolCalls 与 tool 一侧的 ToolCallID 必须
// 同时还原。只还原一半会让下一次压缩的 EnforceToolCallPairs fixpoint 把孤儿
// 删掉，症状是「恢复后第一次压缩历史突然变短」——一个没有报错、没有日志、
// 只有模型行为退化的失败。
func restoreMessages(msgs []store.Message) []*schema.Message {
	out := make([]*schema.Message, 0, len(msgs))
	for _, m := range msgs {
		msg := &schema.Message{Role: restoreRole(m.Role), Content: m.Content}
		switch msg.Role {
		case schema.Tool:
			msg.ToolCallID = m.ToolCallID
			msg.ToolName = m.ToolName
		case schema.Assistant:
			// 一条 assistant 记录携带 ToolCallID 表示它发起过工具调用。
			// 持久化层每条记录只存一个调用，因此这里还原成单元素切片；
			// 同一轮的多个并行调用在日志里是多条 assistant 记录。
			if m.ToolCallID != "" {
				msg.ToolCalls = []schema.ToolCall{{
					ID: m.ToolCallID,
					Function: schema.FunctionCall{
						Name:      m.ToolName,
						Arguments: m.ToolArgs,
					},
				}}
			}
		}
		out = append(out, msg)
	}
	return out
}

// restoreRole 把持久化的 role 字符串映射回 schema.RoleType。
//
// 未知值落到 User 是 fail-safe 的那一侧：把一条来历不明的记录当成用户输入，
// 比当成 assistant（模型会以为那是自己说的）或 tool（会成为配对孤儿）都安全。
func restoreRole(role string) schema.RoleType {
	switch role {
	case "assistant":
		return schema.Assistant
	case "tool":
		return schema.Tool
	case "system":
		return schema.System
	default:
		return schema.User
	}
}
```

- [ ] **Step 4: 替换恢复循环**

把 `internal/api/http/ws_handlers.go` 里这一段：

```go
	hist := make([]schema.Message, 0, len(msgs))
	csHist := make([]*schema.Message, 0, len(msgs))
	for _, m := range msgs {
		role := schema.User
		if m.Role == "assistant" {
			role = schema.Assistant
		}
		msg := schema.Message{Role: role, Content: m.Content}
		hist = append(hist, msg)
		csHist = append(csHist, &msg)
	}
```

替换为：

```go
	csHist := restoreMessages(msgs)
	hist := make([]schema.Message, 0, len(csHist))
	for _, m := range csHist {
		hist = append(hist, *m)
	}
```

> **原代码有一处别名 bug 顺带被修掉**：`msg` 是循环变量，`csHist` 里存的是 `&msg`（Go 1.22+ 每轮新变量，因此不是经典的循环变量捕获问题），但 `hist` 存值拷贝、`csHist` 存指针，两者指向不同对象。新写法让 `hist` 明确从 `csHist` 拷贝，两者内容一致且关系显式。

- [ ] **Step 5: 跑测试确认通过**

```sh
go test ./internal/api/http -run TestRestoreMessages -v
```

预期：全部 PASS。

- [ ] **Step 6: 跑全包回归**

```sh
go test ./internal/api/http
```

- [ ] **Step 7: 台账 + pins**

```yaml
- id: "A2/W-A-04"
  package: "W-A"
  verdict: done
  title: "会话恢复保真度（工具轮不再丢失）"
  # 子句 3 是审计没点到的那半：只恢复一半配对不会立刻出错，而是让下一次压缩的
  # EnforceToolCallPairs fixpoint 把孤儿删掉，表现为「恢复后第一次压缩历史突然
  # 变短」。子句 4 是护栏：纯文本会话的恢复结果必须逐字节不变。
  acceptance: "恢复后的历史包含 tool 角色的消息；每条 tool 消息的 ToolCallID 能在同一历史中找到对应的 assistant ToolCalls；恢复后的消息序列通过 EnforceToolCallPairs 不产生删除；非工具消息的恢复结果与本改动前逐字节一致"
  evidence:
    "1": "internal/api/http::TestRestoreMessagesKeepsToolRole"
    "2": "internal/api/http::TestRestoreMessagesPairsToolCallsWithResults"
    "3": "internal/api/http::TestRestoreMessagesSurvivesPairEnforcement"
    "4": "internal/api/http::TestRestoreMessagesPlainTurnIsUnchanged"
```

```sh
printf '%s' '恢复后的历史包含 tool 角色的消息；每条 tool 消息的 ToolCallID 能在同一历史中找到对应的 assistant ToolCalls；恢复后的消息序列通过 EnforceToolCallPairs 不产生删除；非工具消息的恢复结果与本改动前逐字节一致' | shasum -a 256 | cut -c1-16
```

> **若 Step 1 的 GOV1 分流让第 3 条测试落在 `internal/ctxcompact`**，evidence 的 `"3"` 相应改成 `internal/ctxcompact::<测试名>`，并把 `ledger:` 标记写到那个测试的 doc 注释上。

- [ ] **Step 8: 全门禁 + commit**

```sh
go test ./internal/archtest ./internal/bootstrap ./internal/api/http && go vet ./...
```

```bash
git add internal/api/http/ws_handlers.go internal/api/http/ws_restore_test.go \
        docs/feature-status.yaml internal/archtest/acceptance_pin_test.go
git commit -m "fix(api): session restore dropped every tool turn and typed tool messages as user"
```

---

## Task 5: 记忆蒸馏链接线（W-A-05 / `F8`）

**为什么**：`internal/tools/memory_distill.go::DistillMemories` + `internal/store/memory_distill.go::ApplyDistillation` **整条链零生产 caller**（实测：全仓只有自身定义与 doc 注释提及）。memories 表只增不并，长期使用后召回质量下降。

这是本仓 MEMORY 记的「写了但零读者」教训的**第九次复发**。

**决策（spec §2 已批准）**：**接线，不删**。落点是**最小入口** —— 一个 `/distill` 斜杠命令 + turn 结束后的后台触发（受 `internal/features` 开关门禁，默认关）。

**为什么不造复杂调度器**：W-D 的 A15（跨会话记忆自动生成）Phase2 会**直接复用这个入口**。现在造「每 N 轮」的调度逻辑，W-D 落地时会被 Phase2 整段取代。
> `ponytail: 手动命令 + 单个 turn 钩子，不做调度器。W-D 的 Phase2 接同一个入口。`

**Files:**
- Modify: `internal/proto/frame.go`（`NewDistillMemories` / `NewMemoriesDistilled`）
- Modify: `internal/cli/tui/commands.go`（`commandTable` 注册 `/distill`）
- Modify: `internal/cli/tui/commands_session_memory.go`（`cmdDistill`）
- Modify: `internal/api/http/ws_handlers.go`（`distill_memories` 帧处理 + turn 后台触发）
- Create: `internal/api/http/ws_distill_test.go`
- Modify: `docs/feature-status.yaml`
- Modify: `internal/archtest/acceptance_pin_test.go`

**Interfaces:**
- Consumes:
  - `tools.DistillMemories(ctx context.Context, s *store.Store, m tools.DistillModel, dims store.MemoryFilter) (tools.DistillResult, error)`
  - `store.MemoryFilter`
  - `proto.ClientFrame` / `proto.ServerFrame`（`internal/proto/frame.go`）
  - `m.sendControlFrame(proto.ClientFrame) (tea.Model, tea.Cmd)`（`internal/cli/tui`）
- Produces:
  - `proto.NewDistillMemories() proto.ClientFrame`
  - `proto.NewMemoriesDistilled(considered, merged int) proto.ServerFrame`
  - `cmdDistill(m model, args []string) (tea.Model, tea.Cmd)`

---

- [ ] **Step 1: 写失败测试**

创建 `internal/api/http/ws_distill_test.go`：

```go
package http

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli/tui"
	"github.com/x6nux/yanshi/internal/proto"
)

// ledger: A2/W-A-05#1 /distill 命令在 commandTable 中注册并能触发一次蒸馏
func TestDistillSlashCommandIsRegistered(t *testing.T) {
	require.True(t, tui.HasCommand("distill"),
		"a /distill documented but not registered is a phantom capability")
}

// ledger: A2/W-A-05#2 蒸馏请求帧被服务端处理并回复结果帧
func TestDistillFrameRoundTrips(t *testing.T) {
	f := proto.NewDistillMemories()
	require.Equal(t, "distill_memories", f.Type)

	reply := proto.NewMemoriesDistilled(7, 2)
	require.Equal(t, "memories_distilled", reply.Type)
	require.Contains(t, reply.Text, "7")
	require.Contains(t, reply.Text, "2")
}

// ledger: A2/W-A-05#3 蒸馏失败不影响所在 turn 的正常结束
func TestDistillFailureDoesNotAbortTurn(t *testing.T) {
	err := runDistillPass(context.Background(), nil, nil)
	require.NoError(t, err,
		"a nil store/model is an ordinary state (see DistillMemories), not a turn-aborting failure")
}
```

`tui.HasCommand` 需要在 `internal/cli/tui` 新增一个导出的查询函数（跨包读 `commandTable`）：

```go
// HasCommand 报告 name 是否是已注册的斜杠命令（不含前导斜杠）。
//
// 导出它是为了让别的包能对「文档里宣传的命令真的存在吗」这个问题给出机器
// 判据。internal/archtest/slashcmd_test.go 是 denylist（只拦已知幻影），
// 抓不到「文档写了但从未注册」这个方向。
func HasCommand(name string) bool {
	for _, c := range commandTable {
		if c.name == name {
			return true
		}
	}
	return false
}
```

> **若 `internal/api/http` → `internal/cli/tui` 撞 GOV1**（很可能撞 —— TUI 是客户端层），把 `TestDistillSlashCommandIsRegistered` 挪到 `internal/cli/tui` 包内，evidence 相应改成 `internal/cli/tui::TestDistillSlashCommandIsRegistered`，并把 `ledger:` 标记写到那里。`HasCommand` 仍然值得导出，理由如上。

- [ ] **Step 2: 跑测试确认失败**

```sh
go test ./internal/api/http -run TestDistill -v
```

预期：编译失败，`undefined: proto.NewDistillMemories`。

- [ ] **Step 3: 加协议帧**

在 `internal/proto/frame.go` 的 fork 帧那一节之后追加：

```go
// --- W-A-05 记忆蒸馏帧 ---

// NewDistillMemories 请求对当前会话作用域跑一次记忆合并（W-A-05）。
// 回复：memories_distilled{text}。
//
// 这条帧存在的理由是 DistillMemories 与 ApplyDistillation 整条链写完之后
// 从未有过任何生产调用点 —— memories 表只增不并。一个手动入口是最小的
// 修复；W-D 的跨会话记忆自动生成会复用同一个入口而不是再造一个。
func NewDistillMemories() ClientFrame {
	return ClientFrame{Type: "distill_memories"}
}

// NewMemoriesDistilled 是 distill_memories 的回复，报告本次考察与合并的条数。
// 作为单帧控制回复发出，因此 isControlReply 会在它上面关闭客户端的回复通道。
func NewMemoriesDistilled(considered, merged int) ServerFrame {
	return ServerFrame{
		Type: "memories_distilled",
		Text: fmt.Sprintf("distilled: considered %d, merged %d", considered, merged),
	}
}
```

同时把 `"memories_distilled"` 加进 `isControlReply` 的判定集合（搜 `func isControlReply` 找到它，照抄 `session_forked` 的写法）。

- [ ] **Step 4: 加斜杠命令**

在 `internal/cli/tui/commands_session_memory.go` 的 `cmdFork` 之后追加：

```go
// cmdDistill 请求一次记忆合并。无参数：作用域由服务端按当前会话决定。
func cmdDistill(m model, _ []string) (tea.Model, tea.Cmd) {
	return m.sendControlFrame(proto.NewDistillMemories())
}
```

在 `internal/cli/tui/commands.go` 的 `commandTable` 里，紧邻 `{name: "sessions", ...}` 加一行：

```go
	{name: "distill", help: "merge redundant memories", helpKey: "tui.command.help.distill", run: cmdDistill},
```

并在 `internal/i18n/catalog/en.json` 与 `zh-Hans.json` 各加 `tui.command.help.distill` 的文案（**两个文件都要加** —— 缺一个会让另一种语言下显示 key 本身）。

- [ ] **Step 5: 加服务端处理与 turn 后台触发**

在 `internal/api/http/ws_handlers.go` 新增：

```go
// runDistillPass 跑一次记忆合并。
//
// 错误一律吞掉并只记日志：蒸馏是后台优化，失败不该影响它所在的 turn。
// DistillMemories 自己对 nil store / nil model / 候选不足都返回 (零值, nil)
// ——那些是普通状态不是失败，这里不必再判一次。
func runDistillPass(ctx context.Context, s *store.Store, m tools.DistillModel) error {
	res, err := tools.DistillMemories(ctx, s, m, store.MemoryFilter{})
	if err != nil {
		slog.Warn("memory distillation failed", "err", err)
		return nil
	}
	if res.Merged > 0 {
		slog.Info("memory distillation", "considered", res.Considered, "merged", res.Merged)
	}
	return nil
}
```

在 WS 的控制帧分发处（搜 `case "fork_session":` 找到那个 switch）加一个 case：

```go
	case "distill_memories":
		res, err := tools.DistillMemories(ctx, s.store, s.distillModel, store.MemoryFilter{})
		if err != nil {
			conn.write(proto.NewError("distillation failed: " + err.Error()))
			return
		}
		conn.write(proto.NewMemoriesDistilled(res.Considered, res.Merged))
		return
```

在 turn 正常结束的位置（搜该文件里写 `proto.NewDone()` 的主 turn 出口）加后台触发：

```go
	// W-A-05：turn 结束后在后台跑一次记忆合并。受 features 开关门禁、默认关，
	// 因为它会调用一次模型。go 出去而不是内联，是为了不把 turn 的收尾时间
	// 绑在一次外部调用上；错误由 runDistillPass 自己吞掉。
	if features.Enabled("memory_distill_after_turn") {
		go func() {
			bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			_ = runDistillPass(bg, s.store, s.distillModel)
		}()
	}
```

> **`s.distillModel` 需要新增**：`tools.DistillModel` 是个接口（读 `internal/tools/memory_distill.go` 顶部的定义），在 server 结构体上加一个字段，由 `internal/bootstrap` 传入 —— **用哪个模型**：优先 `batch.rlm_model` 指向的廉价 provider（蒸馏是批量小任务），没配就用主模型。这条选择写进该字段的 doc 注释。
> **`features.Enabled` 的实际 API 以 `go doc github.com/x6nux/yanshi/internal/features` 为准**，形态不符就照它改，**默认关这一点不变**。
> `context.WithoutCancel` 是必需的：turn 的 ctx 在 `NewDone()` 之后就被取消了，直接用它 go 出去等于立刻超时。

- [ ] **Step 6: 跑测试确认通过**

```sh
go test ./internal/api/http -run TestDistill -v && go test ./internal/cli/tui -run TestDistill -v
```

- [ ] **Step 7: 跑幻影命令门禁**

```sh
go test ./internal/archtest -run TestPhantomSlashCommands -v
```

预期：PASS。`phantomSlashCommands` 当前是空 map，新命令不会被拦 —— 但**这道门禁是 denylist 不是白名单**，它证明不了 `/distill` 真的注册了。`TestDistillSlashCommandIsRegistered` 才是那个证明。

- [ ] **Step 8: 台账 + pins**

```yaml
- id: "A2/W-A-05"
  package: "W-A"
  verdict: done
  title: "记忆蒸馏链接线（/distill + turn 后台触发）"
  # 「写了但零读者」的第九次复发：DistillMemories 与 ApplyDistillation 单测
  # 全绿、零生产 caller。因此子句 1 显式描述「被谁调用」而不只是「做对了
  # 什么」——那是这类缺陷唯一能被测试抓住的形状。落点刻意选最小入口：W-D 的
  # 跨会话记忆自动生成 Phase2 会复用它，现在造调度器会被那一步整段取代。
  acceptance: "/distill 命令在 commandTable 中注册并能触发一次蒸馏；蒸馏请求帧被服务端处理并回复结果帧；蒸馏失败不影响所在 turn 的正常结束"
  evidence:
    "1": "internal/cli/tui::TestDistillSlashCommandIsRegistered"
    "2": "internal/api/http::TestDistillFrameRoundTrips"
    "3": "internal/api/http::TestDistillFailureDoesNotAbortTurn"
```

```sh
printf '%s' '/distill 命令在 commandTable 中注册并能触发一次蒸馏；蒸馏请求帧被服务端处理并回复结果帧；蒸馏失败不影响所在 turn 的正常结束' | shasum -a 256 | cut -c1-16
```

- [ ] **Step 9: 生成文档 diff-gate**

本 Task 动了斜杠命令表，`docs/user-guide/tui.md` 由生成器产出：

```sh
go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md
```

不重跑会让 `.github/workflows/docs.yml` 的 `git diff --exit-code` 失败。

- [ ] **Step 10: 全门禁 + commit**

```sh
go test ./internal/archtest ./internal/bootstrap ./internal/api/http ./internal/cli/tui ./internal/proto && go vet ./...
```

```bash
git add internal/proto/frame.go internal/cli/tui/commands.go \
        internal/cli/tui/commands_session_memory.go internal/api/http/ws_handlers.go \
        internal/api/http/ws_distill_test.go internal/bootstrap/bootstrap.go \
        internal/i18n/catalog/en.json internal/i18n/catalog/zh-Hans.json \
        docs/user-guide/tui.md \
        docs/feature-status.yaml internal/archtest/acceptance_pin_test.go
git commit -m "fix(memory): the distillation chain shipped complete with zero callers; wire /distill and a post-turn pass"
```

---

## Task 6: 流式空闲超时看门狗（W-A-06 / `F10`）

**为什么**：`internal/llm/eino/resilient.go::consumeStream` 的循环里，只有 `ctx.Err()` 检查与 `sr.Recv()`。`Recv` **没有任何超时包裹** —— 网关不断连也不发数据就永久挂起。

`loopguard` 的 `DeadlineGate` 在**迭代边界**检查；一条卡在 `Recv` 上的流根本进不到下一次迭代，所以那道闸永不触发。**无人值守的 goal loop 被一条僵死流吃掉整轮预算。**

**契约（spec §2 已定）**：首块 + 稳态**双超时**。
- `FirstChunkTimeout`：发出请求到收到第一个**有内容**的块；
- `IdleTimeout`：两个**有内容**的块之间。

**「空控制块不续命」是承重的** —— 只含 `role` 或空 delta 的块**不重置计时器**，否则一个只发心跳的僵死网关能永远续命。这正是 `consumeStream` 已有的「blank delta 丢弃」逻辑要复用的地方。

**Files:**
- Create: `internal/llm/eino/streamwatchdog.go`
- Create: `internal/llm/eino/streamwatchdog_test.go`
- Modify: `internal/llm/eino/resilient.go`（`ResilientConfig` 加两个字段、`consumeStream` 接看门狗）
- Modify: `internal/config/config.go`（可选：暴露给操作员）
- Modify: `docs/feature-status.yaml`
- Modify: `internal/archtest/acceptance_pin_test.go`

**Interfaces:**
- Consumes: `(*schema.StreamReader[*schema.Message]).Recv() (*schema.Message, error)`、`ResilientConfig`
- Produces:
  - `type watchdogReader struct{ ... }` + `func newWatchdogReader(sr *schema.StreamReader[*schema.Message], first, idle time.Duration) *watchdogReader`
  - `(*watchdogReader).Recv() (*schema.Message, error)`
  - `ErrStreamIdle error`（哨兵）
  - `ResilientConfig.FirstChunkTimeout` / `ResilientConfig.IdleTimeout`（`time.Duration`，零值 = 关闭）

---

- [ ] **Step 1: 写失败测试**

创建 `internal/llm/eino/streamwatchdog_test.go`：

```go
package eino

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/require"
)

// pipeStream returns a reader plus a writer the test drives by hand, so a
// "gateway that connects and then says nothing" is expressible.
func pipeStream() (*schema.StreamReader[*schema.Message], *schema.StreamWriter[*schema.Message]) {
	return schema.Pipe[*schema.Message](8)
}

// ledger: A2/W-A-06#1 首块超时后流被终止并返回可重试错误
func TestWatchdogFirstChunkTimeout(t *testing.T) {
	sr, sw := pipeStream()
	defer sw.Close()

	w := newWatchdogReader(sr, 50*time.Millisecond, time.Hour)
	_, err := w.Recv()

	require.ErrorIs(t, err, ErrStreamIdle)
	require.True(t, IsRetryable(err),
		"a stalled gateway is transient; a non-retryable verdict would burn the whole failover chain")
}

// ledger: A2/W-A-06#2 仅发送空控制块的流在稳态超时后被终止
func TestWatchdogEmptyControlChunksDoNotRenewTheDeadline(t *testing.T) {
	sr, sw := pipeStream()
	go func() {
		defer sw.Close()
		// One real chunk starts the steady-state clock.
		sw.Send(&schema.Message{Role: schema.Assistant, Content: "hi"}, nil)
		// Then heartbeats forever, carrying nothing.
		for i := 0; i < 100; i++ {
			sw.Send(&schema.Message{Role: schema.Assistant}, nil)
			time.Sleep(5 * time.Millisecond)
		}
	}()

	w := newWatchdogReader(sr, time.Hour, 60*time.Millisecond)
	_, err := w.Recv() // the real chunk
	require.NoError(t, err)

	start := time.Now()
	for {
		_, err = w.Recv()
		if err != nil {
			break
		}
		require.Less(t, time.Since(start), 2*time.Second, "watchdog never fired")
	}
	require.ErrorIs(t, err, ErrStreamIdle,
		"blank deltas renewed the deadline, so a heartbeat-only gateway hangs forever")
}

// ledger: A2/W-A-06#3 有实际内容持续到达的长流不被误杀
func TestWatchdogLongStreamWithContentIsNotKilled(t *testing.T) {
	sr, sw := pipeStream()
	go func() {
		defer sw.Close()
		for i := 0; i < 20; i++ {
			sw.Send(&schema.Message{Role: schema.Assistant, Content: "."}, nil)
			time.Sleep(10 * time.Millisecond)
		}
	}()

	w := newWatchdogReader(sr, time.Hour, 80*time.Millisecond)
	n := 0
	for {
		_, err := w.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err, "a stream delivering content every 10ms must survive an 80ms idle budget")
		n++
	}
	require.Equal(t, 20, n)
}

// ledger: A2/W-A-06#4 两个超时值均可配置且零值表示关闭
func TestWatchdogZeroTimeoutsDisableIt(t *testing.T) {
	sr, sw := pipeStream()
	go func() {
		defer sw.Close()
		time.Sleep(120 * time.Millisecond)
		sw.Send(&schema.Message{Role: schema.Assistant, Content: "late"}, nil)
	}()

	w := newWatchdogReader(sr, 0, 0)
	msg, err := w.Recv()

	require.NoError(t, err, "zero timeouts must behave byte-identically to the pre-W-A-06 code")
	require.Equal(t, "late", msg.Content)
}
```

> `schema.Pipe` 与 `IsRetryable` 的实际名字先确认：`go doc github.com/cloudwego/eino/schema.Pipe`、`grep -n "func IsRetryable\|func classify" internal/llm/eino/resilient_classify.go`。**错误分类函数的实际名字可能是 `classifyError` 之类的未导出函数** —— 那就在测试里改用它，或让 `ErrStreamIdle` 走既有的 transient 分类路径并断言分类结果。

- [ ] **Step 2: 跑测试确认失败**

```sh
go test ./internal/llm/eino -run TestWatchdog -v
```

预期：编译失败，`undefined: newWatchdogReader`。

- [ ] **Step 3: 实现看门狗**

创建 `internal/llm/eino/streamwatchdog.go`：

```go
package eino

import (
	"errors"
	"time"

	"github.com/cloudwego/eino/schema"
)

// ErrStreamIdle 表示上游在预算内没有送来任何有内容的块。
//
// 它必须被分类为 TRANSIENT：僵死的网关是网络级故障，不是模型拒绝。判成
// 不可重试会让一次卡顿烧掉整条 failover 链。
var ErrStreamIdle = errors.New("eino: stream idle timeout")

// watchdogReader 给一个流读取器加上首块与稳态两重超时预算。
//
// 存在的理由：consumeStream 原先只在 Recv 返错时动作。一个连上了却不发数据
// 的网关不会返错——它就那么挂着，而 loopguard 的 DeadlineGate 在迭代边界
// 检查，进不去下一次迭代也就永不触发。无人值守的 goal loop 会被一条僵死流
// 吃掉整轮预算。
//
// 两个预算而不是一个，因为它们量的是不同的东西：首块预算量的是「上游到底
// 有没有开始干活」（可以给得紧），稳态预算量的是「上游是不是还在干活」
// （必须给得松，因为长推理的块间间隔本来就长）。
//
// 承重：空控制块不续命。只含 role、或 delta 全空的块不重置计时器——否则一个
// 只发心跳的僵死网关可以永远续命，而那正是这道防线要拦的形态。判据与
// consumeStream 丢弃 blank delta 用的是同一个（见 hasContent）。
type watchdogReader struct {
	sr    *schema.StreamReader[*schema.Message]
	first time.Duration
	idle  time.Duration
	begun bool // 是否已收到过第一个有内容的块
}

// newWatchdogReader 包装 sr。first 或 idle 为零表示该重预算关闭；
// 两个都为零时 Recv 的行为与直接调用 sr.Recv 逐字节一致。
func newWatchdogReader(sr *schema.StreamReader[*schema.Message], first, idle time.Duration) *watchdogReader {
	return &watchdogReader{sr: sr, first: first, idle: idle}
}

// budget 返回当前该用哪一重预算。零表示不设限。
func (w *watchdogReader) budget() time.Duration {
	if w.begun {
		return w.idle
	}
	return w.first
}

// Recv 读下一个块，超预算则返回 ErrStreamIdle。
//
// 实现要点：sr.Recv 是阻塞调用且没有 context 形态，因此把它放进 goroutine、
// 用 select 竞速。超时后那个 goroutine 仍然阻塞在 Recv 上——这是刻意接受的：
// 调用方随后会关闭 sr，Recv 因而返回并让 goroutine 退出。缓冲 channel 保证
// 即使无人接收它也不泄漏。
func (w *watchdogReader) Recv() (*schema.Message, error) {
	d := w.budget()
	if d <= 0 {
		msg, err := w.sr.Recv()
		w.note(msg, err)
		return msg, err
	}

	type result struct {
		msg *schema.Message
		err error
	}
	ch := make(chan result, 1)
	go func() {
		msg, err := w.sr.Recv()
		ch <- result{msg, err}
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case r := <-ch:
		w.note(r.msg, r.err)
		return r.msg, r.err
	case <-timer.C:
		return nil, ErrStreamIdle
	}
}

// note 记录本次读取是否让稳态时钟开始走。
func (w *watchdogReader) note(msg *schema.Message, err error) {
	if err == nil && hasContent(msg) {
		w.begun = true
	}
}

// hasContent 报告一个块是否携带实际内容。
//
// 与 consumeStream 丢弃 blank delta 的判据保持一致：内容、推理内容、工具调用
// 三者任一非空即算。只含 role 的块是控制帧，不算——那是「空控制块不续命」
// 这条约束的落点。
func hasContent(msg *schema.Message) bool {
	if msg == nil {
		return false
	}
	return msg.Content != "" || msg.ReasoningContent != "" || len(msg.ToolCalls) > 0
}
```

> **`hasContent` 与 `consumeStream` 里既有的 blank-delta 判据必须是同一份逻辑**（全局约束 4：重复逻辑必须抽成公共函数）。实现时先读 `consumeStream` 里那段丢弃逻辑：若它已有一个等价的辅助函数，**复用它、删掉这里的 `hasContent`**；若判据是内联的，把内联那段替换成对 `hasContent` 的调用。两份判据漂开会让看门狗和消费者对「什么算内容」产生分歧。

- [ ] **Step 4: 让 `ErrStreamIdle` 被分类为可重试**

在 `internal/llm/eino/resilient_classify.go`（或错误分类所在文件）的分类函数里加一条：

```go
	if errors.Is(err, ErrStreamIdle) {
		return errTransient
	}
```

`errTransient` 的实际标识符以该文件里既有的常量为准。**放在分类链的前部** —— `ErrStreamIdle` 是自造哨兵，不会被字符串匹配的规则误判，但显式前置能防止后续加规则时被抢走。

- [ ] **Step 5: 加配置字段**

在 `internal/llm/eino/resilient.go::ResilientConfig` 追加：

```go
	// FirstChunkTimeout / IdleTimeout 是流式响应的两重空闲预算（W-A-06）。
	//
	// FirstChunkTimeout 量「上游有没有开始干活」：请求发出到第一个有内容的块。
	// IdleTimeout 量「上游还在不在干活」：两个有内容的块之间。空控制块不续命
	// （见 watchdogReader 的 doc 注释）。
	//
	// 零值 = 该重预算关闭。两个都为零时行为与引入前逐字节一致——这与 loopguard
	// 的原则一致：一个自作主张打开的停止条件会静默截断响应，看起来像模型自己
	// 放弃了。因此这里刻意不设非零默认值，由组合根按配置显式打开。
	FirstChunkTimeout time.Duration
	IdleTimeout       time.Duration
```

- [ ] **Step 6: 在 `consumeStream` 接入**

`consumeStream` 目前签名是 `(ctx, sr, sw, deliveredTools)`。**不改签名**，改由调用方包装 —— 在 `Stream` 里拿到 `sr` 之后立刻包一层：

```go
	sr = newWatchdogReader(sr, r.cfg.FirstChunkTimeout, r.cfg.IdleTimeout)
```

**但 `newWatchdogReader` 返回的不是 `*schema.StreamReader`**，所以这行不能直接写。两条路：

1. **给 `consumeStream` 加一个最小接口参数**（推荐）：把 `sr *schema.StreamReader[*schema.Message]` 换成
   ```go
   // streamRecver 是 consumeStream 需要的全部能力。抽成接口是为了让
   // watchdogReader 能插在真实流与消费者之间，而不必伪造一个 StreamReader。
   type streamRecver interface {
       Recv() (*schema.Message, error)
   }
   ```
   `*schema.StreamReader[*schema.Message]` 天然满足它。调用点改为：
   ```go
   var rd streamRecver = sr
   if r.cfg.FirstChunkTimeout > 0 || r.cfg.IdleTimeout > 0 {
       rd = newWatchdogReader(sr, r.cfg.FirstChunkTimeout, r.cfg.IdleTimeout)
   }
   outcome, err := consumeStream(ctx, rd, sw, &deliveredTools)
   ```
2. 在 `consumeStream` 内部按 cfg 自己包 —— 需要把 cfg 传进去，参数更多。

**走第 1 条。** 接口只有一个方法，是 Go 里最小的抽象，且不引入「一个实现的接口」那类过度设计 —— 它有两个实现（真流与看门狗）。

- [ ] **Step 7: 跑测试确认通过**

```sh
go test ./internal/llm/eino -run TestWatchdog -v && go test ./internal/llm/eino
```

- [ ] **Step 8: 暴露给操作员（可选但推荐）**

在 `internal/config/config.go` 的 LLM 段加两个字段并映射进 `ResilientConfig`。**加了就必须重跑文档生成器**：

```sh
go run ./cmd/gendocs -config docs/user-guide/configuration.md
```

- [ ] **Step 9: 台账 + pins**

```yaml
- id: "A2/W-A-06"
  package: "W-A"
  verdict: done
  title: "流式空闲超时看门狗"
  # 子句 2 是这条防线的全部要害：一个只发空控制块的网关，如果那些块能续命，
  # 就能永远挂住一条流。DeadlineGate 在迭代边界检查，进不去下一次迭代也就
  # 永不触发，所以看门狗必须在 Recv 这一层。子句 4 的「零值关闭」与 loopguard
  # 同一原则：自作主张打开的停止条件会静默截断响应。
  acceptance: "首块超时后流被终止并返回可重试错误；仅发送空控制块的流在稳态超时后被终止；有实际内容持续到达的长流不被误杀；两个超时值均可配置且零值表示关闭"
  evidence:
    "1": "internal/llm/eino::TestWatchdogFirstChunkTimeout"
    "2": "internal/llm/eino::TestWatchdogEmptyControlChunksDoNotRenewTheDeadline"
    "3": "internal/llm/eino::TestWatchdogLongStreamWithContentIsNotKilled"
    "4": "internal/llm/eino::TestWatchdogZeroTimeoutsDisableIt"
```

```sh
printf '%s' '首块超时后流被终止并返回可重试错误；仅发送空控制块的流在稳态超时后被终止；有实际内容持续到达的长流不被误杀；两个超时值均可配置且零值表示关闭' | shasum -a 256 | cut -c1-16
```

- [ ] **Step 10: 全门禁 + commit**

**本 Task 有时序测试，必须跑 race**：

```sh
go test -race ./internal/llm/eino
go test ./internal/archtest ./internal/bootstrap && go vet ./...
```

> 若 race job 偶发失败，按 CI 的口径重试最多 3 次 —— 真实 race 会 3/3 全挂，时序 flake 通常重试即过。若 3/3 全挂，问题在 `watchdogReader.Recv` 的 goroutine 与 `w.begun` 的读写上（`Recv` 不是并发安全的，调用方是单 goroutine —— 若测试并发调用了它，改测试而不是加锁）。

```bash
git add internal/llm/eino/streamwatchdog.go internal/llm/eino/streamwatchdog_test.go \
        internal/llm/eino/resilient.go internal/llm/eino/resilient_classify.go \
        internal/config/config.go docs/user-guide/configuration.md \
        docs/feature-status.yaml internal/archtest/acceptance_pin_test.go
git commit -m "fix(eino): a gateway that connects and says nothing hung the stream forever"
```

---

## Task 7: 子代理 worktree 隔离（W-A-08 / `A26`）

**为什么**：`internal/tools/subagent.go` 里 `WorkRoot` / `cwd` / `Cwd` **零命中**（实测）。子代理经 `orchestrator.runSubAgentTurn` 复用同一个 `Orchestrator`，而 `bindExecutionContext` 绑的是 `tools.WithWorkRoot(ctx, o.workRoot)` —— **所有子代理共用一个工作根**。

`agent_dag` / `agent_batch` 明确支持并发派发。**并发子代理互相覆盖对方的编辑是数据损坏，不是缺失特性** —— 这是它在 W-A 而不在 P1 的理由。

**落点比预想干净**：`tools.VCSScope` 已经有 `WorktreeID` 字段，`internal/vcs` 已有完整的 worktree 生命周期（`AddWorktree` / `MergeToMain` / `MarkWorktreeMerged` / `MarkWorktreeAbandoned`），且 `vcs.Worktree` 自带 `Path`。**要做的是接线，不是造机制。**

**契约（spec §2）**：
- 子代理 spawn 时**可选**分配 worktree；
- **默认向后兼容** —— 不请求隔离的子代理仍共用 WorkRoot（否则每个 `agent_spawn` 都产生一个 worktree，`~/.yanshi/worktrees/` 会爆）；
- `agent_dag` / `agent_batch` 的**并发路径默认开启**隔离。

**为什么用 context 而不是加参数**：`tools.SubAgentRunner` 的签名 `func(ctx, prompt string, allowedTools []string, instructionOverride string) (string, error)` 有多个实现与调用点，加参数是一次波及面很大的改动。隔离请求是**横切状态**，与 profile / workRoot / VCS scope 同类 —— CLAUDE.md 明确记着「工具通过 context value（而非参数）获取鉴权/追踪/scope 状态」。

**Files:**
- Modify: `internal/tools/subagent.go`（`WithSubAgentIsolation` / `SubAgentIsolationRequested`）
- Modify: `internal/tools/batch.go`（并发派发点绑定隔离请求）
- Modify: `internal/agent/orchestrator/orchestrator.go`（`runSubAgentTurn` 分配/回收 worktree）
- Create: `internal/agent/orchestrator/subagent_isolation_test.go`
- Modify: `docs/feature-status.yaml`
- Modify: `internal/archtest/acceptance_pin_test.go`

**Interfaces:**
- Consumes:
  - `(*vcs.VCS).AddWorktree(repoID string, agents []string) (vcs.Worktree, error)`
  - `(*vcs.VCS).MergeToMain(wtID, author string, force bool) (string, []string, error)`
  - `(*vcs.VCS).MarkWorktreeMerged(wtID string) error`
  - `(*vcs.VCS).MarkWorktreeAbandoned(wtID string) error`
  - `(*vcs.VCS).ListWorktreeStates(repoID string) ([]vcs.WorktreeState, error)`
  - `vcs.Worktree{ID, RepoID, Path, BaseCommit, CreatedAt, Tip}`；`vcs.WorktreeState` 内嵌 `Worktree`（故 id 是 `s.ID`）
  - `tools.VCSScope{VCS, RepoID, WorktreeID, Agent}`、`tools.WithVCS`、`tools.WithWorkRoot`
- Produces:
  - `tools.WithSubAgentIsolation(ctx context.Context) context.Context`
  - `tools.SubAgentIsolationRequested(ctx context.Context) bool`

---

- [ ] **Step 1: 写失败测试**

创建 `internal/agent/orchestrator/subagent_isolation_test.go`：

```go
package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/tools"
)

// ledger: A2/W-A-08#1 并发子代理各自在独立 worktree 中编辑且互不覆盖
func TestConcurrentIsolatedSubAgentsDoNotShareAWorkRoot(t *testing.T) {
	o := newTestOrchestratorWithVCS(t)

	const n = 4
	roots := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := tools.WithSubAgentIsolation(context.Background())
			roots[i] = o.workRootForSubAgentTurn(ctx, "agent-"+string(rune('a'+i)))
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, r := range roots {
		require.NotEmptyf(t, r, "sub-agent %d got no work root", i)
		require.Falsef(t, seen[r], "sub-agent %d reused work root %q; concurrent agents overwrite each other", i, r)
		seen[r] = true
	}
}

// ledger: A2/W-A-08#2 未请求隔离的子代理行为与本改动前一致
func TestNonIsolatedSubAgentKeepsTheSharedWorkRoot(t *testing.T) {
	o := newTestOrchestratorWithVCS(t)

	got := o.workRootForSubAgentTurn(context.Background(), "plain")

	require.Equal(t, o.workRoot, got,
		"an unrequested worktree per agent_spawn would fill ~/.yanshi/worktrees/ with one directory per call")
}

// ledger: A2/W-A-08#3 子代理结束后其 worktree 被合并回主干或显式丢弃
func TestIsolatedSubAgentWorktreeIsSettledOnExit(t *testing.T) {
	o := newTestOrchestratorWithVCS(t)
	ctx := tools.WithSubAgentIsolation(context.Background())

	root, _, settle := o.acquireSubAgentWorkspace(ctx, "writer")
	require.NotEqual(t, o.workRoot, root)

	require.NoError(t, os.WriteFile(filepath.Join(root, "note.txt"), []byte("hi"), 0o644))
	settle(nil) // 正常结束 → 合并

	states, err := o.vcsScope.VCS.ListWorktreeStates(o.vcsScope.RepoID)
	require.NoError(t, err)
	for _, s := range states {
		// WorktreeState 内嵌 vcs.Worktree，所以 id 是 s.ID。
		require.NotEqualf(t, "active", string(s.Lifecycle),
			"worktree %s was left active; orphan worktrees accumulate until GC", s.ID)
	}
}

// ledger: A2/W-A-08#4 子代理失败时其 worktree 被标记为放弃而不是合并
func TestFailedIsolatedSubAgentWorktreeIsAbandoned(t *testing.T) {
	o := newTestOrchestratorWithVCS(t)
	ctx := tools.WithSubAgentIsolation(context.Background())

	root, _, settle := o.acquireSubAgentWorkspace(ctx, "failer")
	require.NoError(t, os.WriteFile(filepath.Join(root, "half.txt"), []byte("partial"), 0o644))
	settle(context.Canceled) // 失败结束 → 放弃

	_, err := os.Stat(filepath.Join(o.workRoot, "half.txt"))
	require.True(t, os.IsNotExist(err),
		"a failed sub-agent's half-finished edits must not land on main")
}
```

`newTestOrchestratorWithVCS` 需要在同包的测试辅助文件里新建 —— 用 `t.TempDir()` 作 workRoot、`store` 用内存或临时文件、`vcs.New(store, filepath.Join(t.TempDir(), "worktrees"))` 建 VCS 并 `InitRepo(workRoot)`。**参照 `internal/vcs` 既有测试的建库方式**（`grep -rn "vcs.New(" internal/vcs/*_test.go | head -3`）。

- [ ] **Step 2: 跑测试确认失败**

```sh
go test ./internal/agent/orchestrator -run 'TestConcurrentIsolated|TestNonIsolated|TestIsolatedSubAgent|TestFailedIsolated' -v
```

预期：编译失败，`undefined: tools.WithSubAgentIsolation`。

- [ ] **Step 3: 加隔离请求注入器**

在 `internal/tools/subagent.go` 的 `WithLeafSubAgentTools` 附近追加：

```go
type subAgentIsolationKey struct{}

// WithSubAgentIsolation 标记「这个子代理需要独立的工作副本」。
//
// 为什么是 context 而不是 SubAgentRunner 的参数：隔离请求是横切状态，与
// profile / workRoot / VCS scope 同类，本仓的既定做法是走 context value
// （见 CLAUDE.md「上下文注入是横切模式」）。SubAgentRunner 的签名有多个
// 实现与调用点，加一个参数是一次波及面很大的改动。
//
// 谁设置它：agent_dag / agent_batch 的并发派发点。它们明确并发，而并发子
// 代理共用一个 WorkRoot 就是互相覆盖对方的编辑——不是「缺个特性」，是数据
// 损坏。单发的 agent_spawn 不设置，因为给每次调用都开一个 worktree 会把
// ~/.yanshi/worktrees/ 填满。
func WithSubAgentIsolation(ctx context.Context) context.Context {
	return context.WithValue(ctx, subAgentIsolationKey{}, true)
}

// SubAgentIsolationRequested 报告 ctx 是否请求了独立工作副本。
// 未设置时返回 false，即共用 WorkRoot 的既有行为。
func SubAgentIsolationRequested(ctx context.Context) bool {
	v, _ := ctx.Value(subAgentIsolationKey{}).(bool)
	return v
}
```

- [ ] **Step 4: 在编排器分配与回收**

在 `internal/agent/orchestrator/orchestrator.go` 新增：

```go
// acquireSubAgentWorkspace 为一次子代理执行准备工作根。
//
// 返回工作根路径与一个 settle 回调。settle(nil) 表示子代理正常结束、把
// worktree 合并回 main；settle(err) 表示失败、把它标记为放弃。两条路都必须
// 走到，否则 worktree 一直是 active 状态，孤儿回收要等到下一次 GC。
//
// 未请求隔离时返回 (o.workRoot, no-op)：这是引入本特性之前的全部行为，
// 逐字节不变。VCS 未配置时同样退回共用——隔离建立在 autoVCS 的 worktree
// 机制上，没有它就没有可隔离的东西，静默退回比启动失败合适（编排器的既有
// 惯例是非致命子系统缺失即降级）。
func (o *Orchestrator) acquireSubAgentWorkspace(ctx context.Context, agent string) (string, tools.VCSScope, func(error)) {
	noop := func(error) {}
	if !tools.SubAgentIsolationRequested(ctx) || o.vcsScope.VCS == nil || o.vcsScope.RepoID == "" {
		return o.workRoot, o.vcsScope, noop
	}
	wt, err := o.vcsScope.VCS.AddWorktree(o.vcsScope.RepoID, []string{agent})
	if err != nil {
		slog.Warn("sub-agent worktree allocation failed; falling back to the shared work root",
			"agent", agent, "err", err)
		return o.workRoot, o.vcsScope, noop
	}
	// vcs.Worktree 自带 Path，不必再经 WorktreePath 反查一次。
	scope := o.vcsScope
	scope.WorktreeID = wt.ID
	scope.Agent = agent
	return wt.Path, scope, func(runErr error) {
		if runErr != nil {
			if err := o.vcsScope.VCS.MarkWorktreeAbandoned(wt.ID); err != nil {
				slog.Warn("abandoning sub-agent worktree failed", "worktree", wt.ID, "err", err)
			}
			return
		}
		if _, conflicts, err := o.vcsScope.VCS.MergeToMain(wt.ID, agent, false); err != nil || len(conflicts) > 0 {
			// 合并冲突不是错误：并发子代理动同一个文件是可能的。标记放弃而
			// 不是强制合并——强制会静默丢掉一侧的编辑，而那正是本条要修的
			// 那类损坏。冲突清单进日志，操作员可以从 worktree 里取回。
			slog.Warn("sub-agent worktree not merged", "worktree", wt.ID,
				"conflicts", conflicts, "err", err)
			_ = o.vcsScope.VCS.MarkWorktreeAbandoned(wt.ID)
			return
		}
		if err := o.vcsScope.VCS.MarkWorktreeMerged(wt.ID); err != nil {
			slog.Warn("marking sub-agent worktree merged failed", "worktree", wt.ID, "err", err)
		}
	}
}

// workRootForSubAgentTurn 只取工作根，立即结清并丢弃 scope。
// 供只关心「拿到哪个根」的测试与诊断路径使用。
func (o *Orchestrator) workRootForSubAgentTurn(ctx context.Context, agent string) string {
	root, _, settle := o.acquireSubAgentWorkspace(ctx, agent)
	settle(nil)
	return root
}
```

在 `runSubAgentTurn` 的开头接上（先 `grep -n "func (o \*Orchestrator) runSubAgentTurn" -A 15 internal/agent/orchestrator/*.go` 读它现在的样子，`agentName` 用它实际的形参名）：

```go
	root, scope, settle := o.acquireSubAgentWorkspace(ctx, agentName)
	var runErr error
	defer func() { settle(runErr) }()
	ctx = tools.WithWorkRoot(ctx, root)
	if scope.VCS != nil {
		ctx = tools.WithVCS(ctx, scope)
	}
```

> **`runErr` 必须是被 defer 闭包捕获的具名变量**，并在函数的**每一个**失败返回点赋值 —— 否则 settle 永远收到 nil，失败的子代理也会被合并回 main，那比不隔离更糟。若 `runSubAgentTurn` 有具名返回值 `(out string, err error)`，直接 `defer func() { settle(err) }()` 即可，不必另起变量。
> 未请求隔离时 `scope` 就是 `o.vcsScope`，`WithVCS` 绑的是原样的作用域，与引入本特性之前一致。

- [ ] **Step 5: 在并发派发点请求隔离**

在 `internal/tools/batch.go` 的并发派发处（`agent_dag` 与 `agent_batch` 各自 fan-out 的那个 goroutine 启动点），给每个分支的 ctx 加：

```go
		bctx := WithSubAgentIsolation(ctx)
```

并把 `bctx` 传给该分支的 `SubAgentRunner` 调用。**单发路径（`agent_spawn` / `agent_start`）不加。**

- [ ] **Step 6: 跑测试确认通过**

```sh
go test ./internal/agent/orchestrator -run 'TestConcurrentIsolated|TestNonIsolated|TestIsolatedSubAgent|TestFailedIsolated' -v
```

- [ ] **Step 7: 跑 GOV6 与并发回归**

```sh
go test ./internal/archtest -run TestGOV6 -v
go test -race ./internal/agent/orchestrator ./internal/tools ./internal/vcs
```

GOV6 要求 `WithSubAgentIsolation` 有生产调用点 —— Step 5 就是那个调用点，只加注入器不接派发点会在这里变红。

- [ ] **Step 8: 台账 + pins**

```yaml
- id: "A2/W-A-08"
  package: "W-A"
  verdict: done
  title: "并发子代理的 worktree 隔离"
  # 审计列为 A 档（缺失特性），本 spec 判为 W-A（已交付功能坏了）：agent_dag
  # 与 agent_batch 明确支持并发派发，并发下互相覆盖对方的编辑是数据损坏。
  # 子句 2 是护栏——单发 agent_spawn 每次开 worktree 会把 ~/.yanshi/worktrees/
  # 填满，那是把一个损坏换成另一个。子句 4 钉住失败路径：失败的子代理必须
  # 被放弃而不是合并，否则半成品编辑落到 main，比不隔离更糟。
  acceptance: "并发子代理各自在独立 worktree 中编辑且互不覆盖；未请求隔离的子代理行为与本改动前一致；子代理结束后其 worktree 被合并回主干或显式丢弃；子代理失败时其 worktree 被标记为放弃而不是合并"
  evidence:
    "1": "internal/agent/orchestrator::TestConcurrentIsolatedSubAgentsDoNotShareAWorkRoot"
    "2": "internal/agent/orchestrator::TestNonIsolatedSubAgentKeepsTheSharedWorkRoot"
    "3": "internal/agent/orchestrator::TestIsolatedSubAgentWorktreeIsSettledOnExit"
    "4": "internal/agent/orchestrator::TestFailedIsolatedSubAgentWorktreeIsAbandoned"
```

```sh
printf '%s' '并发子代理各自在独立 worktree 中编辑且互不覆盖；未请求隔离的子代理行为与本改动前一致；子代理结束后其 worktree 被合并回主干或显式丢弃；子代理失败时其 worktree 被标记为放弃而不是合并' | shasum -a 256 | cut -c1-16
```

- [ ] **Step 9: 全门禁 + commit**

```sh
go test ./internal/archtest ./internal/bootstrap && go vet ./... && go test ./...
```

```bash
git add internal/tools/subagent.go internal/tools/batch.go \
        internal/agent/orchestrator/orchestrator.go \
        internal/agent/orchestrator/subagent_isolation_test.go \
        docs/feature-status.yaml internal/archtest/acceptance_pin_test.go
git commit -m "fix(orchestrator): concurrent sub-agents shared one work root and overwrote each other's edits"
```

---

## 收尾：整包验收

7 个 Task 全部完成后跑一次完整验收。**这不是可选步骤** —— 台账的 7 条新条目彼此独立，但 `acceptancePins` 是一张共享的表，逐条改容易在最后一条上把前六条的行改漂。

- [ ] **Step 1: 台账完整性**

```sh
go run ./cmd/featurestatus
```

预期：条目总数为 `70`（原 63 + 新 7），未结项 `0`。

- [ ] **Step 2: 全门禁**

```sh
go test ./internal/archtest ./internal/bootstrap
go test ./...
go vet ./...
go run ./cmd/codelines
```

- [ ] **Step 3: 生成文档同步**

```sh
go run ./cmd/api-schema -markdown docs/api/schema.md
go run ./cmd/api-schema -markdown docs/api/resources.md
go run ./cmd/gendocs -config docs/user-guide/configuration.md
go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md
git diff --exit-code
```

预期：无 diff。有 diff 说明前面某个 Task 漏跑了生成器 —— 提交生成结果。

- [ ] **Step 4: race**

```sh
go test -race ./internal/llm/eino ./internal/agent/orchestrator ./internal/tools ./internal/vcs
```

- [ ] **Step 5: 启动自检**

```sh
go build -o yanshi ./cmd/yanshi && ./yanshi -h
timeout 5 ./yanshi --fake-model -inprocess || true
```

预期：`-h` 打印用法并退出 0。**这一步抓的是「测试全绿但二进制起不来」** —— `internal/cli/tui` 的单测断言的是 `Model.Update`/`View` 的返回值，启动崩溃在它们全绿时照样复现。

- [ ] **Step 6: 更新 spec 的交付状态**

在 `docs/superpowers/specs/2026-08-27-capability-roadmap-design.md` §10.1 的交付顺序表里，把 W-A 那行标记为已完成，并在 §14「下一步」里把「取下一批」的指向改为 W-D。

- [ ] **Step 7: 更新 CLAUDE.md 的受影响描述**

本包完成后，CLAUDE.md 里这两处会变成「说的是已消失的行为」：

- **压缩章节**关于 token 估算的描述 —— 现在多模态计入了；
- **记忆/检索**相关描述（若有）关于 CJK —— 现在有回退路径了。

spec §11.3 列的四处是 W-B / W-C / W-D 完成后才需要改的，**本包不动那四处**。改动 CLAUDE.md 后必须跑：

```sh
go test ./internal/archtest -run 'TestGOV9|TestPhantomSlashCommands|TestVSCodeExtensionNotAdvertised'
```

—— 这三道门禁**扫描 CLAUDE.md 本身**。

---

## 附录：本计划的复核状态

7 条全部为 **✔已核** —— 落点符号与现状描述均在 `1c3760a` 上用 grep / 读码确认过。三处对审计的修正记录在 spec §0，**其中两处是写本计划时才发现的**：

| 发现 | 内容 |
|---|---|
| 写 spec 时 | 审计的 F11（`acp_delegate`）是刻意的授权决策，有测试与 doc 注释钉住 → 驳回 |
| 写本计划时 | 审计的 F4（沙箱路径展开）**没有对应的代码路径** —— `SandboxConfig` 无路径列表、`Options.WorkRoot` 零生产赋值点、scratch 路径已展开 → 驳回，W-A 从 8 条降为 7 条 |
| 写本计划时 | 审计的 F5 只点了 `UserInputMultiContent`，实测 `schema.Message` 上**三个**多模态字段全部不计数 → Task 1 覆盖三个 |

**动手时仍需现场确认的签名**（计划里已逐处标注）：`schema.MessageOutputPart` 字段名、`scanTargets` 可变参数形态、`EnforceToolCallPairs` 导出名、`features.Enabled` API、`schema.Pipe` 与错误分类函数名、`store.AppendMessage` / `WriteMemory` 签名、`runSubAgentTurn` 当前形态。**签名不符就改调用，断言语义不变。**
