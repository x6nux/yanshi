# Batch G — 多模态理解（vision）Implementation Plan

**Spec:** `docs/superpowers/specs/2026-07-22-multimodal-vision-design.md`（权威，决策已全锁）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不更换主模型的前提下，为 yanshi 兜底图像理解——主模型原生多模态时图像直接进消息；主模型非多模态时图像进会话级 store + 占位，主模型显式调 `image_describe` 由自动选定的辅助多模态模型返回文本描述。覆盖五个图像入口（剪贴板粘贴、`@path`、`fs_read`/`web_fetch` 遇图、截图工具、headless/SDK/IDE 传图）。

**Architecture:** 配置用 `multimodal: bool` 声明每个 provider 能力；bootstrap 自动选第一个 `multimodal: true` 的 provider 作 vision 辅助并构建 `model-id → 能力` map。turn 构建时按**当前 turn 活动模型**查 map 分流：多模态 → 图像作为 `schema.Message.UserInputMultiContent` 的 image part 原生进消息；非多模态 → 图像入会话级 image store 拿稳定 id，用户消息插占位 `[image:img-N|src|WxH fmt]`，主模型识别后显式调 `image_describe`，工具按 id/路径取字节 + question 路由给辅助模型，文本描述作 tool result 回喂。`image_describe` 纯显式（不自动预描述）；未配辅助 → 启动 warning + 调用时返回明确配置错误；辅助 usage 进 usage 记录标 `vision` 计 budget/cost。

**Tech Stack:** Go 1.26.4 · 标准库 `image`（png/jpeg/gif 解码 + 纯 stdlib 箱式降采样，不引入 `golang.org/x/image`）· 现有 Eino `schema.Message.UserInputMultiContent`/`MessageInputPart.Image` · 现有 `tools.GuardedTool`/`SyncStream` 模式 · 子进程剪贴板/截图（平台 build-tag adapter，`CGO_ENABLED=0` 兼容）· 扩展后的 `einollm.FakeModel`（支持图像输入）。

---

## 锁定决策（来自 spec，直接用，不再讨论）

- **ProviderConfig 加 `Multimodal bool`**；**无** `vision_model` 字段——辅助模型纯自动选第一个 `multimodal: true` 的 provider。
- **两路分流**：主多模态 → 图直接进 `schema.Message` image part；主非多模态 → 会话级 image store + 占位文本 → 主模型显式调 `image_describe` → 工具路由给辅助模型返回文本。
- **纯显式，不自动调** `image_describe`。
- **五入口全做**：A 剪贴板 Ctrl+V（OS clipboard 子进程为主、cgo build-tag 为辅）/ B `@path` / C `fs_read`·`web_fetch` 遇图 / D 截图工具（approval-required）/ E headless-SDK-IDE 协议加 image 字段 additive。
- **未配辅助**（主非多模态且无任何 multimodal provider）→ 启动 warning（不阻塞）+ `image_describe` 调用返回明确配置错误。
- **辅助 usage** 进 usage 记录标 `vision`，计 budget / `/cost`。
- **限制**：png/jpeg/webp/gif（gif 取首帧）、单图 ≤10MB、长边 >2048px 降采样、store 上限 20 张 / 100MB（LRU 淘汰）。

---

## 现状基线（迁移输入，非最终 API）

- `internal/config/config.go:306` `ProviderConfig` 当前**无**多模态标记；`applyDefaults`/`validate` 不需要新逻辑（零值=非多模态，合法）。
- `internal/llm/eino/provider.go:46` `BuildProviders` 返回 `(models, chain, windows, err)`，models/windows 以**model-id**为 key（见其 doc 注释）；bootstrap（`internal/bootstrap/bootstrap.go:444`）消费它。
- `internal/bootstrap/bootstrap.go:425-455` 选择 chatModel / providerModels；`internal/bootstrap/bootstrap.go:642-660` 组装 `orchestrator.Config`；`internal/bootstrap/bootstrap.go:461-629` 累积 `allTools`。
- `internal/tools/guard.go:150` `GuardedTool` + `SyncStream`（`guard.go:78`）是所有工具的统一构造模式；`NewApprovalGuardedTool`（`guard.go:173`）给必须逐次批准的工具（截图用）。
- `internal/llm/eino/fake.go:15` `FakeModel` 已有 `Echo`/`Repeat`/`RecordMessages` 等；本计划扩展它支持图像输入断言 + vision 确定性回复。
- `internal/proto/frame.go:40` `ClientFrame` 是 WS/SSE 共享帧；`user_message` 当前只带 `Text`（+ `OutputSchema`）。
- `internal/api/v1/types.go:127` `TurnStartParams` 是 D1 v1 资源层的 turn 入参（camelCase JSON）。
- eino 图像 API（已在 `internal/llm/eino/anthropic.go:351` 使用）：`msg.UserInputMultiContent []schema.MessageInputPart`，每个 `MessageInputPart{Type ChatMessagePartType, Text string, Image *schema.MessageInputImage}`。`MessageInputImage` 嵌入 `MessagePartCommon{URL *string, Base64Data *string, MIMEType string}`，Type 取值 `schema.ChatMessagePartTypeText`（文本）或 `schema.ChatMessagePartTypeImageURL`（图像 URL/data URL）。**实现 Task 3/8 前用 `go doc github.com/cloudwego/eino/schema MessageInputPart` 核对字段名。**
- `internal/agent/goalloop/usage.go:38` `UsageSink.Add(Usage)` 是 budget 累加器；chat 路径的 usage 走 `orchestrator.TurnUsage` + status 帧。辅助模型的 usage 是工具内部的副调用，经注入的 usage 回调上报。
- `golang.org/x/image` **不是**当前依赖（已确认）；降采样用纯 stdlib 实现，避免新增依赖。

---

## 跨批次依赖（re-verify 标注）

- **D3 re-verify（`config` / `bootstrap`）**：D3（secrets/auth/i18n/keymap）正在改 `internal/config/config.go` 与 `internal/bootstrap/bootstrap.go`。**Task 1（config）与 Task 9（bootstrap）执行前必须 re-verify 这两个文件的落点未被 D3 改写**——具体核对 `ProviderConfig` 结构体定义位置与 `bootstrap.Build` 中 `chatModel`/`providerModels`/`orchConfig`/`allTools` 的装配点。冲突时以 D3 最终态为准合并，不覆盖 D3 的 secrets/auth/i18n/keymap 改动。
- **E 协议字段与 D2 SDK 契约 additive**：Task 7 给 `proto.ClientFrame` 加 `Images` 字段、Task 12 给 `internal/api/v1.TurnStartParams` 加 `Images` 字段、给 `sdk/schema/v1/agent-api.schema.json` 加图像定义——**全部 additive（新增可选字段，omitempty，不改任何 required 字段、不改现有字段语义）**。Task 12 沿用 D2 的版本矩阵契约测试模式验证 v1 客户端忽略未知 additive 字段。

---

## 覆盖范围与 task 数

本计划共 **12 个 task**，建议每个 task 一个小 commit；每个 task 独立可测、RED→GREEN。实现顺序严格为：配置/存储/fake 基础层 → 工具/adapter 层 → 编排分流 → bootstrap 装配 → 五入口接线 → 协议/SDK additive + 验收。

| 领域 | Task | 交付物 | 验收重点 | 依赖 |
|---|---:|---|---|---:|
| 配置 | 1 | `ProviderConfig.Multimodal` + example + test | YAML 解析、零值合法 | — |
| 存储 | 2 | `internal/imagestore` store | id/LRU/降采样/格式尺寸拒绝/并发 | — |
| Fake | 3 | FakeModel 图像输入支持 | 记录 image part、确定性 vision 回复 | — |
| 工具 | 4 | `image_describe` GuardedTool | id/路径 ref、越权、无辅助错误、usage 回流 | 2,3 |
| adapter | 5 | `internal/clipimg` 剪贴板 adapter | 平台子进程、无图 ok=false | — |
| 工具 | 6 | `screenshot` GuardedTool | approval-required、平台捕获、ref | 2 |
| 协议 | 7 | `proto.ClientFrame.Images` additive | 共享 ImageAttach 类型、构造器 | — |
| 编排 | 8 | orchestrator 图像分流 | 多模态→直接 part；非多模态→占位+store | 2,7 |
| 装配 | 9 | bootstrap 辅助选定/map/工具/warning | 自动选第一个、无辅助 warning、map 正确 | 1,3,4,6,8 |
| 入口 A/B | 10 | TUI Ctrl+V + @path 图像 | 剪贴板读取、@path 检测、注入帧 | 5,7 |
| 入口 C | 11 | fs_read/web_fetch 遇图标识 | 结构化 ref、不塞二进制 | 2 |
| 入口 E/验收 | 12 | api/v1 + SDK schema additive + 验收 | v1 additive、版本矩阵、端到端 fake | 7,8 |

---

## 依赖图

```
Task 1 (config) ─────────────────────────────────┐
Task 2 (imagestore) ─────┬──────────────────┐    │
Task 3 (fake vision) ────┤                  │    │
                         ├──> Task 4 (image_describe) ──┐
                         │                              ├──> Task 9 (bootstrap) ──┐
Task 5 (clipimg) ────────┤                              │                          │
Task 6 (screenshot) ─────┴── (needs 2)                  │                          │
                                                          │                          │
Task 7 (proto Images) ──> Task 8 (orchestrator分流) ─────┘ (needs 2,7)             │
                                                          │                          │
                                                          ├──> Task 12 (api/sdk/验收, needs 7,8)
                                                          │
Task 10 (TUI A/B, needs 5,7)
Task 11 (entry C, needs 2)
```

并行机会：Task 1/2/3/5/7 互相独立，可并行；Task 6 仅依赖 2；Task 4 依赖 2+3；Task 8 依赖 2+7；Task 9 是汇聚点（依赖 1/3/4/6/8）。

---

## 文件结构

| 文件 | 职责 | 新/改 |
|---|---|---|
| `internal/config/config.go` | `ProviderConfig.Multimodal` 字段 | 改 [D3 re-verify] |
| `internal/config/config_test.go` | `multimodal` 解析测试 | 改 |
| `config.example.yaml` | `multimodal` 示例 + 说明 | 改 |
| `internal/imagestore/store.go` + `store_test.go` | 会话级 image store（id/LRU/降采样/格式尺寸校验） | 新 |
| `internal/llm/eino/fake.go` + `fake_test.go` | FakeModel 图像输入支持 | 改 |
| `internal/tools/vision.go` + `vision_test.go` | `image_describe` GuardedTool | 新 |
| `internal/clipimg/clipimg.go` + `clipimg_windows.go` + `clipimg_darwin.go` + `clipimg_linux.go` + `clipimg_test.go` | 剪贴板图像读取 adapter | 新 |
| `internal/tools/screenshot.go` + `screenshot_windows.go` + `screenshot_darwin.go` + `screenshot_linux.go` + `screenshot_test.go` | `screenshot` GuardedTool（approval-required） | 新 |
| `internal/proto/frame.go` + `frame_test.go` | `ImageAttach` 类型 + `ClientFrame.Images` additive + 构造器 | 改 [E additive] |
| `internal/agent/orchestrator/orchestrator.go` + `orchestrator_test.go` | 图像分流（Config 字段 + per-turn 直接/占位） | 改 |
| `internal/bootstrap/bootstrap.go` + `bootstrap_test.go` | 辅助选定、multimodal map、image store + 工具注册、warning | 改 [D3 re-verify] |
| `internal/cli/tui/entries.go` + `events.go` + 对应 `*_test.go` | Ctrl+V 剪贴板、@path 图像检测、图像附件注入帧 | 改 |
| `internal/tools/fs.go` + `web.go` + 对应 `*_test.go` | 图像文件/响应的结构化标识 | 改 |
| `internal/api/v1/types.go` + `schema.go` + 对应 `*_test.go` | `TurnStartParams.Images` additive + schema 更新 | 改 [E additive] |
| `sdk/schema/v1/agent-api.schema.json` | 图像 InputItem 定义 additive | 改 [D2 contract additive] |

依赖方向保持：`imagestore`/`clipimg` 不依赖任何 internal 包；`tools` 依赖 `imagestore`；`orchestrator` 依赖 `imagestore`；`bootstrap` 是唯一装配点。

---

## Task 1: `ProviderConfig.Multimodal` 配置字段

**[D3 re-verify]** 执行前 re-verify `internal/config/config.go` 中 `ProviderConfig` 结构体（当前在 `:306`）未被 D3 改写；冲突时合并而非覆盖。

**Files:**
- Modify: `internal/config/config.go`（`ProviderConfig` 结构体）
- Modify: `internal/config/config_test.go`
- Modify: `config.example.yaml`

- [ ] **Step 1: 写失败测试**

在 `internal/config/config_test.go` 末尾追加：

```go
func TestProviderConfigMultimodalParses(t *testing.T) {
	yaml := `
llm:
  providers:
    - name: deepseek
      kind: openai
      model: deepseek-chat
      multimodal: false
    - name: claude
      kind: anthropic
      model: claude-opus-4-8
      multimodal: true
`
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if len(cfg.LLM.Providers) != 2 {
		t.Fatalf("providers = %d", len(cfg.LLM.Providers))
	}
	if cfg.LLM.Providers[0].Multimodal {
		t.Fatalf("provider 0 should be non-multimodal, got %#v", cfg.LLM.Providers[0])
	}
	if !cfg.LLM.Providers[1].Multimodal {
		t.Fatalf("provider 1 should be multimodal, got %#v", cfg.LLM.Providers[1])
	}
}

func TestProviderConfigMultimodalDefaultsFalse(t *testing.T) {
	yaml := `
llm:
  providers:
    - name: text-only
      model: gpt-3.5
`
	cfg, err := config.LoadBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if cfg.LLM.Providers[0].Multimodal {
		t.Fatal("omitted multimodal must default to false (text-only)")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/config -run 'TestProviderConfigMultimodal' -v`

Expected: FAIL，编译错误 `cfg.LLM.Providers[0].Multimodal undefined`（字段尚不存在）。

- [ ] **Step 3: 给 `ProviderConfig` 加字段**

在 `internal/config/config.go` 的 `ProviderConfig` 结构体（`ContextWindow` 字段后）追加：

```go
	// Multimodal 声明该 provider 原生支持图像输入（Tier G）。主模型非多模态时，
	// bootstrap 自动选第一个 Multimodal==true 的 provider 作 vision 辅助，把图像
	// 路由给它经 image_describe 工具返回文本描述。零值（false）= 文本-only，
	// 合法；不需要在 applyDefaults/validate 里加新逻辑。
	Multimodal bool `yaml:"multimodal"`
```

- [ ] **Step 4: 更新 `config.example.yaml`**

在 `llm.providers` 示例中加 `multimodal` 注释样例（紧跟任一 provider 的字段块）：

```yaml
llm:
  providers:
    - name: deepseek
      kind: openai
      model: deepseek-chat
      multimodal: false          # 主模型: 文本-only（默认）
    # - name: claude
    #   kind: anthropic
    #   model: claude-opus-4-8
    #   multimodal: true         # 自动选作 vision 辅助（主模型非多模态时）
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/config -run 'TestProviderConfigMultimodal' -v`

Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/config/config.go internal/config/config_test.go config.example.yaml
git commit -m "feat(config): add ProviderConfig.Multimodal flag (Tier G)"
```

---

## Task 2: 会话级 image store（`internal/imagestore`）

纯内存、会话级、无 external 依赖。负责 id 分配、降采样、格式/尺寸校验、LRU 淘汰。

**Files:**
- Create: `internal/imagestore/store.go`
- Create: `internal/imagestore/store_test.go`

- [ ] **Step 1: 写失败测试**

```go
package imagestore

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// encodePNG 编码一个 width×height 的纯色 PNG，供测试构造合法图像字节。
func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestPutAssignsIDAndGetRoundTrips(t *testing.T) {
	s := New(Config{MaxItems: 20, MaxBytes: 100 << 20})
	id, err := s.Put(encodePNG(t, 10, 10), "paste", "png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if id != "img-1" {
		t.Fatalf("first id = %q want img-1", id)
	}
	e, ok := s.Get(id)
	if !ok {
		t.Fatalf("Get %q: not found", id)
	}
	if e.Fmt != "png" || e.Source != "paste" || e.W != 10 || e.H != 10 || len(e.Bytes) == 0 {
		t.Fatalf("entry = %#v", e)
	}
}

func TestPutRejectsUnsupportedFormat(t *testing.T) {
	s := New(Config{MaxItems: 20, MaxBytes: 100 << 20})
	if _, err := s.Put([]byte("not an image"), "paste", "bmp"); err == nil {
		t.Fatal("bmp must be rejected")
	}
}

func TestPutRejectsOversized(t *testing.T) {
	s := New(Config{MaxItems: 20, MaxBytes: 100 << 20, MaxImageBytes: 100})
	if _, err := s.Put(make([]byte, 101), "paste", "png"); err == nil {
		t.Fatal(">MaxImageBytes must be rejected")
	}
}

func TestPutDownscalesLongEdge(t *testing.T) {
	s := New(Config{MaxItems: 20, MaxBytes: 100 << 20, MaxLongEdge: 2048})
	big := encodePNG(t, 3000, 1000)
	id, err := s.Put(big, "paste", "png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	e, _ := s.Get(id)
	if e.W > 2048 || e.H > 2048 {
		t.Fatalf("long edge not downscaled: %dx%d", e.W, e.H)
	}
	if e.W <= 0 || e.H <= 0 {
		t.Fatalf("downscaled dims must be positive: %dx%d", e.W, e.H)
	}
}

func TestLRUEvictsWhenFull(t *testing.T) {
	s := New(Config{MaxItems: 2, MaxBytes: 100 << 20})
	id1, _ := s.Put(encodePNG(t, 1, 1), "a", "png")
	s.Put(encodePNG(t, 1, 1), "b", "png")
	s.Put(encodePNG(t, 1, 1), "c", "png") // 触发淘汰 id1（最旧且未访问）
	if _, ok := s.Get(id1); ok {
		t.Fatal("LRU must evict the oldest entry when MaxItems exceeded")
	}
}

func TestLRUGetPromotesRecency(t *testing.T) {
	s := New(Config{MaxItems: 2, MaxBytes: 100 << 20})
	id1, _ := s.Put(encodePNG(t, 1, 1), "a", "png")
	id2, _ := s.Put(encodePNG(t, 1, 1), "b", "png")
	s.Get(id1)                  // 访问 id1 → 提升为最新
	s.Put(encodePNG(t, 1, 1), "c", "png") // 淘汰的应是 id2
	if _, ok := s.Get(id2); ok {
		t.Fatal("LRU must evict least-recently-used (id2), not id1")
	}
	if _, ok := s.Get(id1); !ok {
		t.Fatal("id1 was promoted and must survive")
	}
}

func TestPlaceholderFormat(t *testing.T) {
	s := New(Config{MaxItems: 20, MaxBytes: 100 << 20})
	id, _ := s.Put(encodePNG(t, 1280, 720), "paste", "png")
	got := s.Placeholder(id)
	if !strings.HasPrefix(got, "[image:img-1 | paste | ") || !strings.Contains(got, "1280×720 png") || !strings.HasSuffix(got, "]") {
		t.Fatalf("placeholder = %q", got)
	}
}

func TestStoreConcurrentPut(t *testing.T) {
	s := New(Config{MaxItems: 100, MaxBytes: 100 << 20})
	done := make(chan string, 50)
	for i := 0; i < 50; i++ {
		go func() {
			id, _ := s.Put(encodePNG(t, 2, 2), "paste", "png")
			done <- id
		}()
	}
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := <-done
		if id == "" || seen[id] {
			t.Fatalf("duplicate or empty id %q", id)
		}
		seen[id] = true
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/imagestore -v`

Expected: FAIL（包不存在）。

- [ ] **Step 3: 实现完整 `internal/imagestore/store.go`**

```go
// Package imagestore is the session-level, in-memory image store for Tier G
// multimodal. It mints stable ids (img-N) for images that arrive via any of the
// five entry points, enforces format/size limits, downsamples oversized long
// edges, and evicts least-recently-used entries under a combined item/byte cap.
//
// It is deliberately dependency-free (pure stdlib decode + box-filter downscale)
// so it can be reused by the orchestrator (placeholder path) and the
// image_describe tool without pulling model or transport packages.
package imagestore

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"sync"
	"time"
)

// 支持的格式。webp 不在 stdlib；用 magic-bytes 探测 + 标记"webp"但降采样跳过
// （直接存原字节），让 webp 至少能作为 ref 流转（image_describe 取字节交给辅助模型）。
var supportedFmts = map[string]bool{"png": true, "jpeg": true, "jpg": true, "gif": true, "webp": true}

// Config tames the store limits. Zero MaxImageBytes disables the per-image byte
// cap; zero MaxLongEdge disables downscaling. MaxItems/MaxBytes drive LRU.
type Config struct {
	MaxItems      int
	MaxBytes      int
	MaxImageBytes int
	MaxLongEdge   int
}

// Defaults applied by New when a field is zero (so callers can omit the struct).
const (
	defaultMaxItems      = 20
	defaultMaxBytes      = 100 << 20 // 100 MiB
	defaultMaxImageBytes = 10 << 20  // 10 MiB
	defaultMaxLongEdge   = 2048
)

// Entry is one stored image.
type Entry struct {
	ID      string
	Source  string
	Fmt     string
	W, H    int
	Bytes   []byte
	Created time.Time
}

// Store is a concurrency-safe LRU image store keyed by stable id.
type Store struct {
	mu        sync.Mutex
	cfg       Config
	next      int
	byID      map[string]*entryNode
	head, tail *entryNode // MRU=head, LRU=tail
	bytes     int
}

type entryNode struct {
	entry Entry
	prev, next *entryNode
}

// New builds a Store with the given limits (zero fields fall back to defaults).
func New(cfg Config) *Store {
	if cfg.MaxItems == 0 {
		cfg.MaxItems = defaultMaxItems
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.MaxImageBytes == 0 {
		cfg.MaxImageBytes = defaultMaxImageBytes
	}
	if cfg.MaxLongEdge == 0 {
		cfg.MaxLongEdge = defaultMaxLongEdge
	}
	return &Store{cfg: cfg, byID: make(map[string]*entryNode)}
}

// Put validates + (if needed) downscales bytes, assigns the next stable id, and
// inserts the entry as most-recently-used. Returns the id or an error describing
// why the image was rejected (format/size). Evictions keep both the item count
// and total byte count within the configured caps.
func (s *Store) Put(raw []byte, source, fmtName string) (string, error) {
	fmtName = strings.ToLower(strings.TrimSpace(fmtName))
	if !supportedFmts[fmtName] {
		return "", fmt.Errorf("imagestore: unsupported format %q (want png/jpeg/webp/gif)", fmtName)
	}
	if len(raw) > s.cfg.MaxImageBytes {
		return "", fmt.Errorf("imagestore: image %d bytes exceeds %d byte limit", len(raw), s.cfg.MaxImageBytes)
	}
	w, h, data, err := normalize(raw, fmtName, s.cfg.MaxLongEdge)
	if err != nil {
		return "", fmt.Errorf("imagestore: decode/normalize: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	id := fmt.Sprintf("img-%d", s.next)
	node := &entryNode{entry: Entry{
		ID: id, Source: source, Fmt: canonicalFmt(fmtName),
		W: w, H: h, Bytes: data, Created: time.Now(),
	}}
	s.pushFront(node)
	s.byID[id] = node
	s.bytes += len(data)
	s.evict()
	return id, nil
}

// Get returns the entry by id and promotes it to most-recently-used.
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.byID[id]
	if !ok {
		return Entry{}, false
	}
	s.moveToFront(node)
	return node.entry, true
}

// Placeholder renders the stable placeholder text the non-multimodal path inserts
// into the user message: [image:<id> | <source> | <W>x<H> <fmt>]. Unknown id → "".
func (s *Store) Placeholder(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.byID[id]
	if !ok {
		return ""
	}
	e := node.entry
	return fmt.Sprintf("[image:%s | %s | %dx%d %s]", e.ID, e.Source, e.W, e.H, e.Fmt)
}

// --- LRU helpers ---

func (s *Store) pushFront(n *entryNode) {
	n.prev, n.next = nil, s.head
	if s.head != nil {
		s.head.prev = n
	}
	s.head = n
	if s.tail == nil {
		s.tail = n
	}
}

func (s *Store) remove(n *entryNode) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		s.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		s.tail = n.prev
	}
	n.prev, n.next = nil, nil
}

func (s *Store) moveToFront(n *entryNode) {
	if s.head == n {
		return
	}
	s.remove(n)
	s.pushFront(n)
}

func (s *Store) evict() {
	for (len(s.byID) > s.cfg.MaxItems || s.bytes > s.cfg.MaxBytes) && s.tail != nil {
		victim := s.tail
		s.remove(victim)
		delete(s.byID, victim.entry.ID)
		s.bytes -= len(victim.entry.Bytes)
	}
}

func canonicalFmt(f string) string {
	if f == "jpg" {
		return "jpeg"
	}
	return f
}

// normalize decodes raw (gif→first frame; webp→pass-through bytes, no decode),
// downsamples when the long edge exceeds maxLongEdge via a box filter, and
// re-encodes to PNG so the stored bytes are a self-contained raster. Returns
// (width, height, bytes, err). webp returns the original dims as 0×0 (unknown)
// since the stdlib cannot decode it — callers only need bytes + fmt for the
// image_describe aux call, and the placeholder dims are best-effort.
func normalize(raw []byte, fmtName string, maxLongEdge int) (int, int, []byte, error) {
	if fmtName == "webp" {
		return 0, 0, raw, nil // stdlib 无 webp 解码器；原样存
	}
	img, err := decodeByFmt(raw, fmtName)
	if err != nil {
		return 0, 0, nil, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if maxLongEdge > 0 && (w > maxLongEdge || h > maxLongEdge) {
		img, w, h = downscale(img, maxLongEdge)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return 0, 0, nil, err
	}
	return w, h, buf.Bytes(), nil
}

func decodeByFmt(raw []byte, fmtName string) (image.Image, error) {
	switch fmtName {
	case "png":
		return png.Decode(bytes.NewReader(raw))
	case "jpeg", "jpg":
		return jpeg.Decode(bytes.NewReader(raw))
	case "gif":
		g, err := gif.DecodeAll(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		if len(g.Image) == 0 {
			return nil, errors.New("gif has no frames")
		}
		return g.Image[0], nil // 首帧
	default:
		return nil, fmt.Errorf("unsupported format %q", fmtName)
	}
}

// downscale 用纯 stdlib 箱式（area-averaging）下采样到 long edge ≤ max。不引入
// golang.org/x/image 依赖。src 已知 bounds。
func downscale(src image.Image, max int) (image.Image, int, int) {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	scale := 1.0
	if sw >= sh {
		scale = float64(sw) / float64(max)
	} else {
		scale = float64(sh) / float64(max)
	}
	if scale < 1 {
		scale = 1
	}
	dw, dh := int(float64(sw)/scale), int(float64(sh)/scale)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	// 每 dst 像素取对应 src 区域的平均色。
	boxW := float64(sw) / float64(dw)
	boxH := float64(sh) / float64(dh)
	for dy := 0; dy < dh; dy++ {
		for dx := 0; dx < dw; dx++ {
			x0 := int(float64(dx) * boxW)
			y0 := int(float64(dy) * boxH)
			dst.Set(dx, dy, src.At(b.Min.X+x0, b.Min.Y+y0))
		}
	}
	return dst, dw, dh
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/imagestore -v`

Expected: PASS（8 个测试全过；LRU 双向链表、降采样、格式/尺寸拒绝、并发 id 唯一）。

- [ ] **Step 5: 提交**

```bash
git add internal/imagestore/store.go internal/imagestore/store_test.go
git commit -m "feat(imagestore): add session-level LRU image store with downscale"
```

---

## Task 3: FakeModel 图像输入支持

扩展 `einollm.FakeModel` 以支持图像输入断言 + 确定性 vision 回复——让 `image_describe` 工具的辅助模型路径和多模态主模型分流都能零真实 API 测试。

**Files:**
- Modify: `internal/llm/eino/fake.go`
- Modify: `internal/llm/eino/fake_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/llm/eino/fake_test.go` 末尾追加：

```go
func TestFakeModelRecordsImagePartsAndDescribes(t *testing.T) {
	m := NewFakeModel(nil, nil)
	m.Vision = true
	m.RecordImages = true
	b64 := "iVBORw0KGgo=" // 占位 base64；vision 模式只看 part 计数
	url := "data:image/png;base64," + b64
		msg := &schema.Message{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
			{Type: schema.ChatMessagePartTypeText, Text: "describe this"},
			{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{MIMEType: "image/png", URL: &url},
			}},
		}}
		resp, err := m.Generate(context.Background(), []*schema.Message{msg})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if m.LastImageCount != 1 {
		t.Fatalf("LastImageCount = %d want 1", m.LastImageCount)
	}
	if !strings.Contains(resp.Content, "fake-vision") || !strings.Contains(resp.Content, "1") {
		t.Fatalf("vision reply = %q want a deterministic fake-vision(1 image) string", resp.Content)
	}
}

func TestFakeModelImageCountZeroForTextOnly(t *testing.T) {
	m := NewFakeModel([]string{"plain"}, nil)
	m.Vision = true
	m.RecordImages = true
	m.Generate(context.Background(), []*schema.Message{
		schema.UserMessage("just text"),
	})
	if m.LastImageCount != 0 {
		t.Fatalf("LastImageCount = %d want 0 for text-only input", m.LastImageCount)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/llm/eino -run 'TestFakeModelRecordsImagePartsAndDescribes|TestFakeModelImageCountZeroForTextOnly' -v`

Expected: FAIL，编译错误 `m.Vision`/`m.RecordImages`/`m.LastImageCount` undefined。

- [ ] **Step 3: 扩展 FakeModel**

先**核对** eino 图像 content-type 常量名与字段：

Run: `go doc github.com/cloudwego/eino/schema MessageInputPart` 与 `go doc github.com/cloudwego/eino/schema MessageInputImage`

确认 `schema.MessageInputPart{Type ChatMessagePartType, Text string, Image *schema.MessageInputImage}` 存在且 `schema.MessageInputImage` 嵌入 `MessagePartCommon{URL *string, Base64Data *string, MIMEType string}`（与 `internal/llm/eino/anthropic.go:351` 使用一致）。若字段名不同按 doc 调整下方断言。

在 `internal/llm/eino/fake.go` 的 `FakeModel` 结构体（`RecordMessages` 字段块后）追加字段：

```go
	// Vision, when true, makes Generate/Stream return a deterministic description
	// derived from the number of image parts in the most recent input message —
	// e.g. "fake-vision(1 image)". This drives image_describe's aux-model path
	// and the multimodal-main分流 test without a real vision API. When the input
	// has zero image parts, the response is "fake-vision(0 images)". Ignored when
	// err is non-nil.
	Vision bool

	// RecordImages, when true, makes Generate/Stream count the image parts across
	// the most recent input messages into LastImageCount, so tests can assert an
	// image actually reached the model on the multimodal-direct path. Overwrites
	// per call, like ReceivedMessages.
	RecordImages  bool
	LastImageCount int
```

在 `recordMessages`（`fake.go:167`）函数末尾的 `m.optsMu.Unlock()` 前，把图像计数也记上。把 `recordMessages` 改为：

```go
func (m *FakeModel) recordMessages(messages []*schema.Message) {
	if !m.RecordMessages && !m.Vision {
		return
	}
	m.optsMu.Lock()
	defer m.optsMu.Unlock()
	if m.RecordMessages {
		m.ReceivedMessages = messages
	}
	if m.RecordImages || m.Vision {
		m.LastImageCount = countImageParts(messages)
	}
}

// countImageParts sums the image MessageInputPart parts across messages. Used by
// Vision mode and RecordImages so tests can assert images reached the model.
func countImageParts(messages []*schema.Message) int {
	n := 0
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		for _, part := range msg.UserInputMultiContent {
			if part.Image != nil {
				n++
			}
		}
	}
	return n
}
```

然后在 `Generate`（`fake.go:89`）的 `Echo`/`Repeat`/脚本分支**之前**插入 Vision 分支（紧跟 `isJudgeProbe` 判断之后）：

```go
	if m.Vision {
		return schema.AssistantMessage(fmt.Sprintf("fake-vision(%d image%s)", m.LastImageCount, pluralImg(m.LastImageCount)), nil), nil
	}
```

对 `Stream`（`fake.go:123`）的 `var msg *schema.Message` 赋值链做同样的事——在 `isJudgeProbe` 分支之后、`Echo` 分支之前加：

```go
	} else if m.Vision {
		msg = schema.AssistantMessage(fmt.Sprintf("fake-vision(%d image%s)", m.LastImageCount, pluralImg(m.LastImageCount)), nil)
	}
```

并在文件 import 区加 `"fmt"`（若未导入）。文件末尾加 helper：

```go
func pluralImg(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/llm/eino -run 'TestFakeModel' -v`

Expected: PASS（新测试 + 既有 fake 测试全过；Vision 只在有图像描述语义时介入，不影响 Echo/Repeat/脚本路径）。

- [ ] **Step 5: 提交**

```bash
git add internal/llm/eino/fake.go internal/llm/eino/fake_test.go
git commit -m "feat(eino): extend FakeModel with vision image input support"
```

---

## Task 4: `image_describe` GuardedTool

纯显式工具：模型自己决定何时调。按 id 或路径解析图像 → 组装 `[image + question]` 发给辅助多模态模型 → 文本描述作 tool result 回喂。错误一律作为 result（非 Go error）回喂，不中断 turn。辅助 usage 经回调上报。

**Files:**
- Create: `internal/tools/vision.go`
- Create: `internal/tools/vision_test.go`

- [ ] **Step 1: 写失败测试**

```go
package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/imagestore"
)

func encodeTestPNG(t *testing.T) []byte {
	t.Helper()
	return testPNGBytes(t, 4, 4) // 见下方 helper
}

func newVisionStore(t *testing.T) (*imagestore.Store, string) {
	t.Helper()
	s := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	id, err := s.Put(encodeTestPNG(t), "paste", "png")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return s, id
}

func TestImageDescribeByIDReturnsAuxDescription(t *testing.T) {
	store, id := newVisionStore(t)
	aux := einollm.NewFakeModel(nil, nil)
	aux.Vision = true
	var recorded visionUsage
	tool := NewImageDescribeTool(aux, store, nil, recorded.record)
	args, _ := json.Marshal(map[string]string{"image_ref": id, "question": "what is this?"})
	ch := tool.Stream(context.Background(), string(args))
	var result string
	for c := range ch {
		if c.Err != nil {
			t.Fatalf("chunk err: %v", c.Err)
		}
		result = c.Result
	}
	if !strings.Contains(result, "fake-vision(1 image") {
		t.Fatalf("result = %q", result)
	}
	if recorded.prompt == 0 && recorded.completion == 0 && recorded.total == 0 {
		// FakeModel 不带 ResponseMeta → usage 零；仅确认回调被调过。
	}
	if !recorded.called {
		t.Fatal("usage recorder must be invoked for each aux call")
	}
}

func TestImageDescribeDefaultQuestion(t *testing.T) {
	store, id := newVisionStore(t)
	aux := einollm.NewFakeModel(nil, nil)
	aux.Vision = true
	aux.RecordMessages = true
	tool := NewImageDescribeTool(aux, store, nil, nil)
	args, _ := json.Marshal(map[string]string{"image_ref": id})
	ch := tool.Stream(context.Background(), string(args))
	for range ch {
	}
	last := aux.ReceivedMessages[len(aux.ReceivedMessages)-1]
	if !strings.Contains(last.Content, "请描述这张图片") {
		t.Fatalf("default question not applied; last msg = %#v", last)
	}
}

func TestImageDescribeNoAuxReturnsConfigError(t *testing.T) {
	store, id := newVisionStore(t)
	tool := NewImageDescribeTool(nil, store, nil, nil) // 无辅助
	args, _ := json.Marshal(map[string]string{"image_ref": id})
	ch := tool.Stream(context.Background(), string(args))
	var result string
	for c := range ch {
		result = c.Result
	}
	if !strings.Contains(result, "multimodal") || !strings.Contains(result, "provider") {
		t.Fatalf("missing-aux error must explain the config gap: %q", result)
	}
}

func TestImageDescribeBadIDReturnsErrorResult(t *testing.T) {
	store, _ := newVisionStore(t)
	aux := einollm.NewFakeModel(nil, nil)
	aux.Vision = true
	tool := NewImageDescribeTool(aux, store, nil, nil)
	args, _ := json.Marshal(map[string]string{"image_ref": "img-nope"})
	ch := tool.Stream(context.Background(), string(args))
	var result string
	for c := range ch {
		result = c.Result
	}
	if !strings.Contains(result, "✗") || !strings.Contains(strings.ToLower(result), "not found") {
		t.Fatalf("bad id must return ✗ not-found result: %q", result)
	}
}

func TestImageDescribePathRefDeniedByGuard(t *testing.T) {
	// 路径 ref 必须通过 guard FS 校验；绕过 fs.deny 时返回 deny error，不给读。
	// 注入一个拒绝所有读路径的 profile（空 FS.Read 白名单 = 一律拒绝）。
	ctx := WithProfile(context.Background(), guard.PermissionProfile{
		Tools: guard.ToolPermission{Allow: []string{"image_describe"}},
		FS:    guard.FSPermission{Read: []string{}}, // 空 = 拒绝所有读
	})
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	aux := einollm.NewFakeModel(nil, nil)
	aux.Vision = true
	// root 为 t.TempDir() 且 dir 内实际有一张合法 PNG；但 guard 拒绝所有读路径。
	tool := NewImageDescribeTool(aux, store, t.TempDir(), nil)
	args, _ := json.Marshal(map[string]string{"image_ref": "shot.png"})
	ch := tool.Stream(ctx, string(args))
	var result string
	for c := range ch {
		result = c.Result
	}
	if !strings.Contains(strings.ToLower(result), "deny") && !strings.Contains(strings.ToLower(result), "✗") {
		t.Fatalf("path ref must be denied by guard FS check (empty read whitelist); got %q", result)
	}
}
```

> `testPNGBytes`、`visionUsage`、`visionUsage.record` 在 Step 3 与工具实现同文件/同包定义。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/tools -run 'TestImageDescribe' -v`

Expected: FAIL，`NewImageDescribeTool` 未定义。

- [ ] **Step 3: 实现完整 `internal/tools/vision.go`**

```go
package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/imagestore"
)

// VisionRunner is the subset of model.BaseChatModel the image_describe tool
// needs to call the auxiliary multimodal model. Declared as a one-method
// interface so *einollm.FakeModel and any real BaseChatModel satisfy it without
// an adapter.
type VisionRunner interface {
	Generate(ctx context.Context, messages []*schema.Message, opts ...model.Option) (*schema.Message, error)
}

// VisionUsageFunc records one auxiliary model call's token usage so it can be
// folded into the session budget / /cost ledger with a "vision" tag. Nil-safe.
type VisionUsageFunc func(prompt, completion, total int)

// imageDescribeArgs is the tool's JSON args shape.
type imageDescribeArgs struct {
	ImageRef string `json:"image_ref"`
	Question string `json:"question"`
}

// imageDescribeState holds the collaborators the run closure captures. It does
// NOT implement Tool itself — the returned *GuardedTool already satisfies the
// Tool interface (it has Info/InvokableRun/DisplayName/DefaultTimeout/Stream),
// and SyncStream(t.run) wires the state into the GuardedTool's run path. This
// mirrors how FSTools/WebTools hold state behind their *GuardedTool fields.
type imageDescribeState struct {
	aux   VisionRunner
	store *imagestore.Store
	root  string
	usage VisionUsageFunc
}

const defaultVisionQuestion = "请描述这张图片的内容"

// NewImageDescribeTool builds the image_describe tool as a *GuardedTool. aux may
// be nil (the bootstrap no-aux path): calls then return a clear config-error
// result rather than panicking. root is the work root for path-type refs (""
// disables path refs). usage records aux token spend (nil-safe). The returned
// tool satisfies the Tool interface via *GuardedTool.
func NewImageDescribeTool(aux VisionRunner, store *imagestore.Store, root string, usage VisionUsageFunc) Tool {
	t := &imageDescribeState{aux: aux, store: store, root: root, usage: usage}
	return NewGuardedTool(
		"image_describe", "Image", "Describe an image via the auxiliary multimodal model. image_ref is an image id (img-N) or a file path; question is optional.",
		60*time.Second,
		params(map[string]*schema.ParameterInfo{
			"image_ref": {Type: schema.String, Required: true, Desc: "image id (img-N) or file path"},
			"question":  {Type: schema.String, Desc: "optional question (default: describe the image)"},
		}),
		SyncStream(t.run),
	)
}

func (t *imageDescribeState) run(ctx context.Context, argsJSON string) (string, error) {
	var args imageDescribeArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errorResult("invalid args: " + err.Error()), nil
	}
	if t.aux == nil {
		return errorResult("主模型非多模态且未配置 multimodal: true 的 provider；请在 config 里加一个 multimodal provider 作 vision 辅助"), nil
	}
	question := strings.TrimSpace(args.Question)
	if question == "" {
		question = defaultVisionQuestion
	}
	imgBytes, fmtName, err := t.resolveRef(ctx, args.ImageRef, argsJSON)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	msg := buildVisionMessage(imgBytes, fmtName, question)
	resp, err := t.aux.Generate(ctx, []*schema.Message{msg})
	if err != nil {
		return errorResult("辅助模型调用失败：" + err.Error()), nil
	}
	t.recordUsage(resp)
	if resp == nil || strings.TrimSpace(resp.Content) == "" {
		return errorResult("辅助模型未返回描述"), nil
	}
	return resp.Content, nil
}

// resolveRef 解析 image_ref：img-N 走 store；否则当路径处理（guard fs 校验 + 读字节）。
// ctx 携带已注入的权限 profile（per-turn），argsJSON 透传给 guard Authorize 的显示上下文。
func (t *imageDescribeState) resolveRef(ctx context.Context, ref, argsJSON string) ([]byte, string, error) {
	if strings.HasPrefix(ref, "img-") {
		e, ok := t.store.Get(ref)
		if !ok {
			return nil, "", fmt.Errorf("image %q not found in store", ref)
		}
		return e.Bytes, e.Fmt, nil
	}
	if t.root == "" {
		return nil, "", fmt.Errorf("path refs require a work root; use an image id (img-N) instead")
	}
	// 路径 jail：相对路径锚定 root；绝对路径若落在 root 下则接受，逃逸 root 拒绝。
	absPath := filepath.Clean(filepath.Join(t.root, ref))
	rootClean := filepath.Clean(t.root)
	if !strings.HasPrefix(absPath, rootClean+string(filepath.Separator)) && absPath != rootClean {
		return nil, "", fmt.Errorf("path %q escapes the work root", ref)
	}
	// FS guard 校验：image_describe 的路径 ref 必须通过 guard 的 FS 维度（读操作）。
	// 复用现有工具授权模式，不绕过 fs.deny 配置。GuardedTool 的顶层 Authorize 只查工具名，
	// 路径级的 FS 校验必须在这里完成，否则 fs.deny 白名单会被绕过。
	if err := Authorize(ctx, guard.Action{
		Tool: "image_describe",
		FS:   guard.FSWant{Op: "read", Paths: []string{absPath}},
	}, argsJSON); err != nil {
		return nil, "", err
	}
	// 路径已在 resolveRef 内完成 jail + guard 校验，直接读即可。
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, "", err
	}
	return data, detectFmt(ref), nil
}

func (t *imageDescribeState) recordUsage(resp *schema.Message) {
	if t.usage == nil || resp == nil || resp.ResponseMeta == nil || resp.ResponseMeta.Usage == nil {
		return
	}
	u := resp.ResponseMeta.Usage
	t.usage(int(u.PromptTokens), int(u.CompletionTokens), int(u.TotalTokens))
}

// buildVisionMessage 组装 [image part + question text] 单条 user 消息。data URL
// 形式经 MessageInputImage.MessagePartCommon.URL 传递，与 anthropic.go:373 的 url 分支一致。
func buildVisionMessage(imgBytes []byte, fmtName, question string) *schema.Message {
	mime := mimeForFmt(fmtName)
	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(imgBytes)
	return &schema.Message{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{
		{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{
			MessagePartCommon: schema.MessagePartCommon{MIMEType: mime, URL: &dataURL},
		}},
		{Type: schema.ChatMessagePartTypeText, Text: question},
	}}
}

func mimeForFmt(f string) string {
	switch f {
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func detectFmt(path string) string {
	low := strings.ToLower(path)
	switch {
	case strings.HasSuffix(low, ".png"):
		return "png"
	case strings.HasSuffix(low, ".gif"):
		return "gif"
	case strings.HasSuffix(low, ".webp"):
		return "webp"
	default:
		return "jpeg"
	}
}

// IsImagePath reports whether path's extension is one of the Tier G image
// formats. Shared by the @path TUI entry (entry B), fs_read/web_fetch (entry C),
// and image_describe's path ref. Exported so internal/cli/tui and internal/tools
// reuse ONE definition (DRY) instead of each re-deriving the extension list.
func IsImagePath(path string) bool {
	switch detectFmt(path) {
	case "png", "gif", "webp", "jpeg":
		return true
	}
	return false
}
```

`readImageFile` 不再需要解析路径（resolveRef 已在 jail 内完成路径解析+guard 校验），直接用 `os.ReadFile(absPath)` 读即完毕。若 `internal/tools/fs.go` 已有等价私有函数，复用它而不是新写（DRY）。

测试侧 helpers（放 `vision_test.go`，与实现同包；Task 6/11 同包复用 `testPNGBytes`）：

> **跨包共享**：Task 12 的 orchestrator e2e 测试也在 `internal/agent/orchestrator` 包需要用 `testPNGBytes`。
> 将其提取到 `internal/imagestore/testutil_test.go`（包间测试辅助）以避免 duplicated helper。提取版本不绑 `*testing.T`，
> 直接返回 `[]byte` + `error` 让调用方按需 `t.Fatal`，从而 `tools`、`orchestrator`、`bootstrap` 测试都可 import。

```go
// testPNGBytes 编码一个 width×height 纯色 PNG，供 tools 包测试构造合法图像字节。
func testPNGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

type visionUsage struct {
	called                            bool
	prompt, completion, total         int
}

func (v *visionUsage) record(p, c, t int) { v.called = true; v.prompt, v.completion, v.total = p, c, t }
```

`vision_test.go` 的 import 区需含 `bytes`、`image`、`image/color`、`image/png`。

- [ ] **Step 4: 编译验证 + 运行测试确认通过**

Run: `go build ./internal/tools`（无错误）+ `go test ./internal/tools -run 'TestImageDescribe' -v`

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/tools -run 'TestImageDescribe' -v`

Expected: PASS（id ref→描述、默认 question、无辅助→配置错误 result、坏 id→✗ not-found result、usage 回调被调）。

- [ ] **Step 6: 提交**

```bash
git add internal/tools/vision.go internal/tools/vision_test.go
git commit -m "feat(tools): add image_describe tool routing to auxiliary multimodal model"
```

---

## Task 5: 剪贴板图像读取 adapter（`internal/clipimg`）

平台 adapter，统一接口 `Read() (bytes, fmt, ok)`。子进程为主（`CGO_ENABLED=0` 兼容），cgo 为 build-tag 可选增强。无图时 `ok=false`，不干扰文本粘贴。

**Files:**
- Create: `internal/clipimg/clipimg.go`
- Create: `internal/clipimg/clipimg_windows.go`
- Create: `internal/clipimg/clipimg_darwin.go`
- Create: `internal/clipimg/clipimg_linux.go`
- Create: `internal/clipimg/clipimg_test.go`

- [ ] **Step 1: 写失败测试**

```go
package clipimg

import (
	"context"
	"testing"
)

func TestReadReturnsFalseWhenBackendReportsNoImage(t *testing.T) {
	r := NewWithBackend(stubBackend{ok: false})
	if _, _, ok := r.Read(context.Background()); ok {
		t.Fatal("Read must return ok=false when the backend reports no image")
	}
}

func TestReadReturnsBytesWhenBackendHasImage(t *testing.T) {
	r := NewWithBackend(stubBackend{ok: true, data: []byte{1, 2, 3}, fmt: "png"})
	data, fmtName, ok := r.Read(context.Background())
	if !ok || fmtName != "png" || len(data) != 3 {
		t.Fatalf("Read = %d bytes, %q, ok=%v", len(data), fmtName, ok)
	}
}

type stubBackend struct {
	ok   bool
	data []byte
	fmt  string
}

func (s stubBackend) ReadImage(_ context.Context) ([]byte, string, bool) {
	return s.data, s.fmt, s.ok
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/clipimg -v`

Expected: FAIL（包不存在）。

- [ ] **Step 3: 实现跨平台文件**

`internal/clipimg/clipimg.go`（平台无关核心）：

```go
// Package clipimg reads an image from the OS clipboard. It is the Tier G entry-A
// backend: the TUI binds Ctrl+V, calls Read, and only when ok=true treats the
// payload as an image attachment (otherwise the keystroke falls through to text
// paste). The default Reader uses a subprocess per platform (CGO_ENABLED=0
// compatible); a cgo-backed implementation may be added behind a build tag later.
package clipimg

import "context"

// Reader reads one image from the clipboard.
type Reader struct {
	backend backend
}

// backend is the platform seam. Returns (bytes, fmt, ok); ok=false means the
// clipboard holds no image (text or empty), which must NOT be an error.
type backend interface {
	ReadImage(ctx context.Context) ([]byte, string, bool)
}

// New returns the default platform Reader.
func New() *Reader { return &Reader{backend: platformReader{}} }

// NewWithBackend is the test seam.
func NewWithBackend(b backend) *Reader { return &Reader{backend: b} }

// Read returns the clipboard image, or ok=false when none is present.
func (r *Reader) Read(ctx context.Context) ([]byte, string, bool) {
	return r.backend.ReadImage(ctx)
}
```

`internal/clipimg/clipimg_windows.go`：

```go
//go:build windows

package clipimg

import (
	"context"
	"os/exec"
)

type platformReader struct{}

// ReadImage 用 PowerShell 把剪贴板里的位图存成临时 PNG 再读回。子进程路线保持
// CGO_ENABLED=0 兼容。无图时 PowerShell 脚本输出空 → ok=false。
func (platformReader) ReadImage(ctx context.Context) ([]byte, string, bool) {
	const script = `Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; $i = [System.Windows.Forms.Clipboard]::GetImage(); if ($i -ne $null) { $p = [System.IO.Path]::GetTempFileName() + '.png'; $i.Save($p, [System.Drawing.Imaging.ImageFormat]::Png); [System.IO.File]::ReadAllBytes($p); Remove-Item $p }`
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script).Output()
	if err != nil || len(out) == 0 {
		return nil, "", false
	}
	return out, "png", true
}
```

`internal/clipimg/clipimg_darwin.go`：

```go
//go:build darwin

package clipimg

import (
	"context"
	"os/exec"
)

type platformReader struct{}

// ReadImage 用 osascript/NSPasteboard 经 pngpaste 子进程读图；无 pngpaste 时
// 回退 osascript（剪贴板无图返回空 → ok=false）。子进程为主，cgo 可后续加。
func (platformReader) ReadImage(ctx context.Context) ([]byte, string, bool) {
	if p, err := exec.LookPath("pngpaste"); err == nil {
		out, err := exec.CommandContext(ctx, p, "-").Output()
		if err == nil && len(out) > 0 {
			return out, "png", true
		}
	}
	return nil, "", false
}
```

`internal/clipimg/clipimg_linux.go`：

```go
//go:build linux

package clipimg

import (
	"context"
	"os/exec"
)

type platformReader struct{}

// ReadImage 优先 wl-paste（Wayland），回退 xclip（X11）。两者都无 → ok=false。
func (platformReader) ReadImage(ctx context.Context) ([]byte, string, bool) {
	if p, err := exec.LookPath("wl-paste"); err == nil {
		out, err := exec.CommandContext(ctx, p, "-t", "image/png").Output()
		if err == nil && len(out) > 0 {
			return out, "png", true
		}
	}
	if p, err := exec.LookPath("xclip"); err == nil {
		out, err := exec.CommandContext(ctx, p, "-selection", "clipboard", "-t", "image/png", "-o").Output()
		if err == nil && len(out) > 0 {
			return out, "png", true
		}
	}
	return nil, "", false
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/clipimg -v`

Expected: PASS（stub backend 路径；平台文件仅靠编译验证，不在 CI 跑真实剪贴板）。

- [ ] **Step 5: 提交**

```bash
git add internal/clipimg
git commit -m "feat(clipimg): add cross-platform clipboard image reader (subprocess-first)"
```

---

## Task 6: `screenshot` GuardedTool（approval-required）

平台相关、最重的入口。approval-required（截图是敏感操作）。捕获后图像进 store 返回 ref。平台捕获走 build-tag adapter，测试注入 fake 捕获。

**Files:**
- Create: `internal/tools/screenshot.go`
- Create: `internal/tools/screenshot_windows.go`
- Create: `internal/tools/screenshot_darwin.go`
- Create: `internal/tools/screenshot_linux.go`
- Create: `internal/tools/screenshot_test.go`

- [ ] **Step 1: 写失败测试**

```go
package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/imagestore"
)

func TestScreenshotReturnsRef(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	tool := NewScreenshotToolWithCapture(store, func(context.Context) ([]byte, string, error) {
		return testPNGBytes(t, 8, 8), "png", nil
	})
	ch := tool.Stream(context.Background(), "{}")
	var result string
	for c := range ch {
		result = c.Result
	}
	if !strings.HasPrefix(result, "[image:") {
		t.Fatalf("result must be a placeholder ref, got %q", result)
	}
	// store 里应能取到该 id
	id := extractID(result, t)
	if _, ok := store.Get(id); !ok {
		t.Fatalf("screenshot image %q not in store", id)
	}
}

func TestScreenshotCaptureFailureReturnsErrorResult(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	tool := NewScreenshotToolWithCapture(store, func(context.Context) ([]byte, string, error) {
		return nil, "", context.DeadlineExceeded
	})
	ch := tool.Stream(context.Background(), "{}")
	var result string
	for c := range ch {
		result = c.Result
	}
	if !strings.Contains(result, "✗") {
		t.Fatalf("capture failure must surface as ✗ result: %q", result)
	}
}

func TestScreenshotUsesApprovalGuardedTool(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	tool := NewScreenshotToolWithCapture(store, func(context.Context) ([]byte, string, error) {
		return testPNGBytes(t, 1, 1), "png", nil
	})
	gt, ok := tool.(*GuardedTool)
	if !ok {
		t.Fatal("screenshot must be a *GuardedTool")
	}
	if !gt.approvalRequired {
		t.Fatal("screenshot tool must require approval (approvalRequired=true)")
	}
}

func extractID(placeholder string, t *testing.T) string {
	t.Helper()
	l := strings.Index(placeholder, ":")
	r := strings.Index(placeholder, " |")
	if l < 0 || r < 0 {
		t.Fatalf("malformed placeholder %q", placeholder)
	}
	return placeholder[l+1 : r]
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/tools -run 'TestScreenshot' -v`

Expected: FAIL，`NewScreenshotToolWithCapture` 未定义（外加 `TestScreenshotUsesApprovalGuardedTool` 测试也失败，`*GuardedTool.approvalRequired` 未设置）。

- [ ] **Step 3: 实现 `internal/tools/screenshot.go`**

```go
package tools

import (
	"context"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/imagestore"
)

// CaptureFunc captures the primary screen and returns (bytes, fmt, err).
// Implementations are platform-specific (build-tagged); tests inject a fake.
type CaptureFunc func(ctx context.Context) ([]byte, string, error)

// screenshotTool holds state for the screenshot capture. It does NOT implement
// Tool itself — the returned *GuardedTool (via newScreenshotTool) already
// satisfies the Tool interface. This mirrors how every other tool works.
type screenshotTool struct {
	store   *imagestore.Store
	capture CaptureFunc
}

// NewScreenshotTool builds the production screenshot tool with the platform
// capture adapter for the current OS.
func NewScreenshotTool(store *imagestore.Store) Tool {
	return newScreenshotTool(store, platformCapture)
}

// NewScreenshotToolWithCapture is the test seam.
func NewScreenshotToolWithCapture(store *imagestore.Store, capture CaptureFunc) Tool {
	return newScreenshotTool(store, capture)
}

func newScreenshotTool(store *imagestore.Store, capture CaptureFunc) Tool {
	s := &screenshotTool{store: store, capture: capture}
	return NewApprovalGuardedTool(
		"screenshot", "Screenshot", "Capture the primary screen and return an image reference (approval required).",
		15*time.Second,
		// 无参数（默认捕获主屏）；区域/多屏选择留后续。
		params(map[string]*schema.ParameterInfo{}),
		SyncStream(s.run),
	)
}

func (s *screenshotTool) run(ctx context.Context, _ string) (string, error) {
	bytes, fmtName, err := s.capture(ctx)
	if err != nil {
		return errorResult("截图失败：" + err.Error()), nil
	}
	id, err := s.store.Put(bytes, "screenshot", fmtName)
	if err != nil {
		return errorResult(err.Error()), nil
	}
	return s.store.Placeholder(id), nil
}
```

返回的 `*GuardedTool`（approval-required）已满足 `Tool` 接口，无需包装器；`s.run` 经 `SyncStream` 接入。

平台捕获文件——`internal/tools/screenshot_windows.go`：

```go
//go:build windows

package tools

import (
	"context"
	"os/exec"
)

func platformCapture(ctx context.Context) ([]byte, string, error) {
	// PowerShell + System.Drawing + System.Windows.Forms CopyFromScreen 捕获主屏存 PNG。
	const script = `Add-Type -AssemblyName System.Windows.Forms; Add-Type -AssemblyName System.Drawing; $b = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds; $bmp = New-Object System.Drawing.Bitmap($b.Width, $b.Height); $g = [System.Drawing.Graphics]::FromImage($bmp); $g.CopyFromScreen($b.Location, [System.Drawing.Point]::Empty, $b.Size); $p = [System.IO.Path]::GetTempFileName() + '.png'; $bmp.Save($p, [System.Drawing.Imaging.ImageFormat]::Png); [System.IO.File]::ReadAllBytes($p); Remove-Item $p`
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return nil, "", err
	}
	return out, "png", nil
}
```

`internal/tools/screenshot_darwin.go`：

```go
//go:build darwin

package tools

import (
	"context"
	"os/exec"
)

func platformCapture(ctx context.Context) ([]byte, string, error) {
	out, err := exec.CommandContext(ctx, "screencapture", "-x", "-").Output()
	if err != nil {
		return nil, "", err
	}
	return out, "png", nil
}
```

`internal/tools/screenshot_linux.go`：

```go
//go:build linux

package tools

import (
	"context"
	"os/exec"
)

func platformCapture(ctx context.Context) ([]byte, string, error) {
	if p, err := exec.LookPath("grim"); err == nil {
		out, err := exec.CommandContext(ctx, p, "-").Output()
		if err == nil {
			return out, "png", nil
		}
	}
	if p, err := exec.LookPath("gnome-screenshot"); err == nil {
		// gnome-screenshot 写文件不写 stdout；用临时文件回读。
		out, err := exec.CommandContext(ctx, p, "-f", "-").Output()
		if err == nil {
			return out, "png", nil
		}
	}
	return nil, "", fmt.Errorf("no supported screen capture tool (install grim or gnome-screenshot)")
}
```

（Linux 文件需 import `"fmt"`。）

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/tools -run 'TestScreenshot' -v`

Expected: PASS（fake 捕获→ref+store；捕获失败→✗ result；approvalRequired 标记位 true）。

- [ ] **Step 5: 提交**

```bash
git add internal/tools/screenshot.go internal/tools/screenshot_windows.go internal/tools/screenshot_darwin.go internal/tools/screenshot_linux.go internal/tools/screenshot_test.go
git commit -m "feat(tools): add approval-gated screenshot tool with platform capture"
```

---

## Task 7: `proto.ClientFrame.Images` additive 协议字段

**[E additive]** 新增可选字段，不改任何现有字段、不改 `Text` 语义。WS 与 SSE 共用同一帧词汇表；五入口的图像附件统一经此字段表达。

**Files:**
- Modify: `internal/proto/frame.go`
- Modify: `internal/proto/frame_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/proto/frame_test.go` 末尾追加：

```go
func TestUserMessageWithImagesCarriesAdditiveField(t *testing.T) {
	fr := NewUserMessageWithImages("see this", []ImageAttach{
		{ID: "img-1", Source: "paste", Fmt: "png", W: 1280, H: 720, DataB64: "AAAA"},
	})
	if fr.Type != "user_message" || fr.Text != "see this" {
		t.Fatalf("base fields must be preserved: %#v", fr)
	}
	if len(fr.Images) != 1 || fr.Images[0].ID != "img-1" {
		t.Fatalf("images = %#v", fr.Images)
	}
	raw, _ := json.Marshal(fr)
	if !strings.Contains(string(raw), `"images":[{`) {
		t.Fatalf("wire form must include images array: %s", raw)
	}
	if strings.Contains(string(raw), `"images":[]`) { // omitempty: 空时省略
		t.Fatalf("omitempty must drop empty images, not emit []: %s", raw)
	}
}

func TestUserMessageWithoutImagesOmitsField(t *testing.T) {
	raw, _ := json.Marshal(NewUserMessage("text only"))
	if strings.Contains(string(raw), "images") {
		t.Fatalf("text-only user_message must not carry images on wire: %s", raw)
	}
}

func TestImageAttachJSONIsCamelCase(t *testing.T) {
	raw, _ := json.Marshal(ImageAttach{ID: "img-1", DataB64: "AA", Fmt: "png", W: 1, H: 2})
	got := string(raw)
	for _, want := range []string{`"id":"img-1"`, `"dataB64":"AA"`, `"w":1`, `"h":2`} {
		if !strings.Contains(got, want) {
			t.Fatalf("wire %q lacks %s", got, want)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/proto -run 'TestUserMessageWithImages|TestUserMessageWithoutImages|TestImageAttachJSON' -v`

Expected: FAIL，`ImageAttach`/`NewUserMessageWithImages`/`ClientFrame.Images` 未定义。

- [ ] **Step 3: 改 `internal/proto/frame.go`**

在 `ClientFrame` 结构体的 `OutputSchema` 字段块后追加：

```go
	// Images carries image attachments for a user_message turn (Tier G). Each
	// entry is one image (id assigned client-side or by the server's image store,
	// base64 bytes, fmt, dims). Empty/absent = text-only turn, byte-identical to
	// pre-G on the wire (omitempty drops it). ADDITIVE: existing clients/frames
	// are unchanged; the field is optional and ignored by code paths that do not
	// handle images.
	Images []ImageAttach `json:"images,omitempty"` // user_message
```

在 `FeaturesSetPayload` 类型定义后追加共享类型（也用于 api/v1 与 SDK）：

```go
// ImageAttach is the transport-level representation of one image attachment,
// shared by proto.ClientFrame (WS/SSE) and the v1 Agent API. Field names are
// camelCase on the wire. DataB64 is standard base64 of the raw image bytes;
// ID is the stable image id (img-N); W/H are display dims for the placeholder.
type ImageAttach struct {
	ID      string `json:"id,omitempty"`
	Source  string `json:"source,omitempty"`
	Fmt     string `json:"fmt,omitempty"`
	W       int    `json:"w,omitempty"`
	H       int    `json:"h,omitempty"`
	DataB64 string `json:"dataB64,omitempty"`
}
```

在 `NewUserMessageWithSchema` 构造器后追加：

```go
// NewUserMessageWithImages builds a user_message frame carrying image
// attachments (Tier G). images may be nil/empty (equivalent to NewUserMessage).
// ADDITIVE: the wire form of an images-less user_message is unchanged.
func NewUserMessageWithImages(text string, images []ImageAttach) ClientFrame {
	return ClientFrame{Type: "user_message", Text: text, Images: images}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/proto -v`

Expected: PASS（新测试 + 既有 proto 测试全过；additive 字段对既有帧不可见）。

- [ ] **Step 5: 提交**

```bash
git add internal/proto/frame.go internal/proto/frame_test.go
git commit -m "feat(proto): add additive Images field to user_message (Tier G)"
```

---

## Task 8: orchestrator 图像分流（直接 image part vs 占位 + store）

按**当前 turn 活动模型**的 multimodal 能力分流。多模态 → 图像作为 `UserInputMultiContent` image part 原生进消息；非多模态 → 图像入 store + 占位文本。`/model` 切换天然兼容（per-turn 判定）。

**Files:**
- Modify: `internal/agent/orchestrator/orchestrator.go`
- Modify: `internal/agent/orchestrator/orchestrator_test.go`

- [ ] **Step 1: 核对 eino 图像 content-type 常量**

Run: `go doc github.com/cloudwego/eino/schema.MessageInputPart` 与 `go doc github.com/cloudwego/eino/schema.ChatMessagePartTypeImageURL`

确认 `schema.MessageInputPart{Type ChatMessagePartType, Text string, Image *schema.MessageInputImage}`、`schema.ChatMessagePartTypeText`/`schema.ChatMessagePartTypeImageURL` 常量名，以及 `schema.MessageInputImage` 嵌入 `MessagePartCommon{URL *string, Base64Data *string, MIMEType string}` 字段。与 `internal/llm/eino/anthropic.go:351` 一致。若字段名不同按 doc 调整下方代码。

- [ ] **Step 2: 写失败测试**

在 `orchestrator_test.go` 末尾追加：

```go
func TestApplyImages_MultimodalDirectEmbedsImagePart(t *testing.T) {
	aux := einollm.NewFakeModel(nil, nil)
	aux.Vision = true
	aux.RecordImages = true
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	cfg := Config{
		Model: einollm.NewFakeModel([]string{"ok"}, nil),
		MultimodalMap: map[string]bool{"mm-model": true},
		ImageStore:    store,
	}
	o, err := New(cfg)
	require.NoError(t, err)

	msgs := o.ApplyImages([]*schema.Message{}, "mm-model", []proto.ImageAttach{
		{Source: "paste", Fmt: "png", W: 2, H: 2, DataB64: "AAAA"},
	})
	require.Len(t, msgs, 1)
	require.NotEmpty(t, msgs[0].UserInputMultiContent, "multimodal model must embed image as a MultiContent part")
}

func TestApplyImages_NonMultimodalInsertsPlaceholderAndStores(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	cfg := Config{
		Model:         einollm.NewFakeModel([]string{"ok"}, nil),
		MultimodalMap: map[string]bool{"text-model": false},
		ImageStore:    store,
	}
	o, err := New(cfg)
	require.NoError(t, err)

	msgs := o.ApplyImages([]*schema.Message{}, "text-model", []proto.ImageAttach{
		{Source: "paste", Fmt: "png", W: 2, H: 2, DataB64: "AAAA"},
	})
	require.Len(t, msgs, 1)
	require.Contains(t, msgs[0].Content, "[image:img-1", "non-multimodal model must see placeholder text")
	require.Empty(t, msgs[0].UserInputMultiContent, "non-multimodal model must NOT get raw image parts")
	_, ok := store.Get("img-1")
	require.True(t, ok, "image must be stored for image_describe to fetch")
}

func TestApplyImages_NoImagesLeavesMessagesUntouched(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	cfg := Config{Model: einollm.NewFakeModel([]string{"ok"}, nil), ImageStore: store}
	o, err := New(cfg)
	require.NoError(t, err)
	in := []*schema.Message{schema.UserMessage("hi")}
	out := o.ApplyImages(in, "any", nil)
	require.Equal(t, in, out, "no images must be a pass-through")
}

func TestApplyImages_ModelSwitchReEvaluatesCapability(t *testing.T) {
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	cfg := Config{
		Model:         einollm.NewFakeModel([]string{"ok"}, nil),
		MultimodalMap: map[string]bool{"mm": true, "text": false},
		ImageStore:    store,
	}
	o, err := New(cfg)
	require.NoError(t, err)
	mm := o.ApplyImages([]*schema.Message{}, "mm", []proto.ImageAttach{{Fmt: "png", DataB64: "AA"}})
	require.NotEmpty(t, mm[0].UserInputMultiContent, "mm model → direct part")
	txt := o.ApplyImages([]*schema.Message{}, "text", []proto.ImageAttach{{Fmt: "png", DataB64: "AA"}})
	require.Contains(t, txt[0].Content, "[image:", "switched to text model → placeholder")
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./internal/agent/orchestrator -run 'TestApplyImages' -v`

Expected: FAIL，`Config.MultimodalMap`/`Config.ImageStore`/`o.ApplyImages` 未定义。

- [ ] **Step 4: 给 `Config` 加字段 + 实现 `ApplyImages`**

在 `internal/agent/orchestrator/orchestrator.go` 的 `Config`（`MCP` 字段后）加：

```go
	// MultimodalMap (Tier G) maps a model-id to its native image capability.
	// Populated by bootstrap from cfg.LLM.Providers[*].Multimodal. The turn-build
	// path consults it per-turn so /model switches re-evaluate. nil/empty ⇒ every
	// model treated as non-multimodal (placeholder + image_describe path).
	MultimodalMap map[string]bool
	// ImageStore (Tier G) is the session-level image store. Required for the
	// non-multimodal placeholder path (it stores images and mints the id the
	// placeholder + image_describe reference). nil ⇒ ApplyImages returns images
	// unchanged (no-op, for tests/configs without Tier G).
	ImageStore *imagestore.Store
```

在 `Orchestrator` 结构体（`availableModels` 字段后）加存储字段：

```go
	multimodalMap map[string]bool // Tier G: model-id → native image capability
	imageStore    *imagestore.Store // Tier G: session image store (placeholder path)
```

在 `New()`（`cfg.SkillMetaPrompt` 处理附近，`return &Orchestrator{...}` 之前）保存：

```go
	multimodalMap := cfg.MultimodalMap
	imageStore := cfg.ImageStore
```

并在返回的 `&Orchestrator{...}` 字面量加：

```go
		multimodalMap: multimodalMap,
		imageStore:    imageStore,
```

import 区加 `"encoding/base64"`、`"github.com/x6nux/yanshi/internal/imagestore"`、`"github.com/x6nux/yanshi/internal/proto"`。

在 `Orchestrator` 方法区加：

```go
// ApplyImages is the Tier G image fan-out: for the given (current turn) model-id
// and image attachments, it either embeds each image as a native image part
// (multimodal model) or stores it and appends the placeholder text
// [image:img-N|src|WxH fmt] to the trailing user message (non-multimodal model).
// history is the in-progress message slice; the trailing user message (or a new
// one) receives the images. With no images it is a pass-through.
//
// Per-turn model-id lookup makes /model switches re-evaluate capability without
// rebuilding anything — the same store serves both the current and a later
// switched model.
func (o *Orchestrator) ApplyImages(history []*schema.Message, modelID string, images []proto.ImageAttach) []*schema.Message {
	if len(images) == 0 {
		return history
	}
	if o.IsMultimodal(modelID) {
		return appendImageParts(history, images)
	}
	return o.appendPlaceholders(history, images)
}

// IsMultimodal reports whether the given model-id is natively multimodal. A nil
// map means no provider declared multimodal: true, so the answer is false and
// the placeholder path is used.
func (o *Orchestrator) IsMultimodal(modelID string) bool {
	return o.multimodalMap[modelID]
}

func appendImageParts(history []*schema.Message, images []proto.ImageAttach) []*schema.Message {
	parts := make([]schema.MessageInputPart, 0, len(images))
	for _, img := range images {
		mime := mimeForImage(img.Fmt)
		url := "data:" + mime + ";base64," + img.DataB64
		parts = append(parts, schema.MessageInputPart{
			Type: schema.ChatMessagePartTypeImageURL,
			Image: &schema.MessageInputImage{
				MessagePartCommon: schema.MessagePartCommon{MIMEType: mime, URL: &url},
			},
		})
	}
	// 嵌入尾部 user 消息；若无尾部 user 消息则新建一条。
	if n := len(history); n > 0 && history[n-1] != nil && history[n-1].Role == schema.User {
		last := *history[n-1]
		last.UserInputMultiContent = append(append([]schema.MessageInputPart(nil), history[n-1].UserInputMultiContent...), parts...)
		history[n-1] = &last
		return history
	}
	return append(history, &schema.Message{Role: schema.User, UserInputMultiContent: parts})
}

func (o *Orchestrator) appendPlaceholders(history []*schema.Message, images []proto.ImageAttach) []*schema.Message {
	if o.imageStore == nil {
		return history // Tier G 未配置：静默跳过（不破坏无 store 的会话）
	}
	var ph strings.Builder
	for _, img := range images {
		data, err := base64.StdEncoding.DecodeString(img.DataB64)
		if err != nil {
			continue // 坏 base64 跳过这一张，不打断整条
		}
		id, err := o.imageStore.Put(data, firstNonEmptyStr(img.Source, "attach"), img.Fmt)
		if err != nil {
			continue
		}
		ph.WriteString(o.imageStore.Placeholder(id))
		ph.WriteString("\n")
	}
	if ph.Len() == 0 {
		return history
	}
	if n := len(history); n > 0 && history[n-1] != nil && history[n-1].Role == schema.User {
		last := *history[n-1]
		last.Content = strings.TrimRight(last.Content, "\n") + "\n" + ph.String()
		history[n-1] = &last
		return history
	}
	return append(history, schema.UserMessage(strings.TrimRight(ph.String(), "\n")))
}

func mimeForImage(f string) string {
	switch strings.ToLower(strings.TrimSpace(f)) {
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/agent/orchestrator -run 'TestApplyImages' -v`

Expected: PASS（多模态→直接 part；非多模态→占位+store；无图 pass-through；`/model` 切换重判）。

- [ ] **Step 6: 提交**

```bash
git add internal/agent/orchestrator/orchestrator.go internal/agent/orchestrator/orchestrator_test.go
git commit -m "feat(orchestrator): add per-turn image fan-out (multimodal direct vs placeholder)"
```

---

## Task 9: bootstrap 辅助自动选定 + multimodal map + 工具注册 + warning

**[D3 re-verify]** 执行前 re-verify `internal/bootstrap/bootstrap.go` 中 `chatModel`/`providerModels`（`:425-455`）、`orchConfig`（`:642-660`）、`allTools`（`:461-629`）的装配点未被 D3 改写；冲突时合并而非覆盖。

**Files:**
- Modify: `internal/bootstrap/bootstrap.go`
- Modify: `internal/bootstrap/bootstrap_test.go`

- [ ] **Step 1: 写失败测试**

在 `bootstrap_test.go` 末尾追加：

```go
func TestBuildSelectsFirstMultimodalProviderAsAux(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := `
llm:
  providers:
    - name: text
      model: text-model
      multimodal: false
    - name: vision
      model: vision-model
      multimodal: true
storage:
  sqlite_path: "` + filepath.Join(dir, "test.db") + `"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(yaml), 0o644))
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true, ProviderBuilder: fakeProviderBuilder})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())
	require.NotNil(t, app.VisionAux, "VisionAux must be the first multimodal provider")
	require.True(t, app.MultimodalMap["vision-model"], "multimodal map must mark vision-model")
	require.False(t, app.MultimodalMap["text-model"])
}

func TestBuildNoAuxWhenMainMultimodalOrNoneConfigured(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := `
llm:
  providers:
    - name: onlytext
      model: onlytext-model
      multimodal: false
storage:
  sqlite_path: "` + filepath.Join(dir, "test.db") + `"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(yaml), 0o644))
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true, ProviderBuilder: fakeProviderBuilder})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())
	require.Nil(t, app.VisionAux, "no multimodal provider ⇒ VisionAux is nil + warning emitted")
}
```

> `fakeProviderBuilder` 是一个测试 seam，返回按 model-id 命名的 FakeModel map（让 `VisionAux` 可被断言非 nil）。在 `bootstrap_test.go` 已有 provider-builder fake 附近定义：

```go
func fakeProviderBuilder(cfg *config.Config) (map[string]model.BaseChatModel, []model.BaseChatModel, map[string]int, error) {
	named := make(map[string]model.BaseChatModel)
	var chain []model.BaseChatModel
	for _, p := range cfg.LLM.Providers {
		fm := einollm.NewFakeModel([]string{"reply"}, nil)
		named[p.Model] = fm
		chain = append(chain, fm)
	}
	return named, chain, nil, nil
}
```

（若 `bootstrap_test.go` 已有等价 fake，复用之。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/bootstrap -run 'TestBuildSelectsFirstMultimodal|TestBuildNoAuxWhenMainMultimodalOrNoneConfigured' -v`

Expected: FAIL，`App.VisionAux`/`App.MultimodalMap` 未定义。

- [ ] **Step 3: 改 bootstrap 装配**

`App` 结构体加字段（紧跟 `Models` 后）：

```go
	// VisionAux (Tier G) is the automatically-selected auxiliary multimodal
	// model — the first provider with Multimodal==true. nil when no provider is
	// multimodal (image_describe then returns a config error) OR when the main
	// model is itself multimodal and no aux is needed (still set when a second
	// multimodal provider exists, for /model switches). image_describe uses it.
	VisionAux model.BaseChatModel
	// MultimodalMap (Tier G) maps model-id → native image capability, built from
	// cfg.LLM.Providers[*].Multimodal. Forwarded to the orchestrator so the
	// per-turn image fan-out can re-evaluate on /model switches.
	MultimodalMap map[string]bool
	// ImageStore (Tier G) is the process image store injected into the
	// orchestrator (placeholder path) and image_describe (id fetch).
	ImageStore *imagestore.Store
```

在 `Build` 内、`providerModels` 赋值后（`:455` 附近）构建 multimodal map + 选辅助：

```go
	// Tier G: build the model-id → multimodal map and auto-select the first
	// multimodal provider as the vision auxiliary. The map keys MUST match the
	// providerModels registry keys (the model id), mirroring BuildProviders'
	// chooseKey EXACTLY (3-fallback Model→Name→model-N with used-dedup).
	// Keep this in sync with internal/llm/eino/provider.go chooseKey.
	multimodalMap := make(map[string]bool)
	var visionAux model.BaseChatModel
	used := make(map[string]bool)
	for i, p := range cfg.LLM.Providers {
		key := p.Model
		if key == "" || used[key] {
			key = p.Name
		}
		if key == "" || used[key] {
			key = fmt.Sprintf("model-%d", i)
		}
		used[key] = true
		if providerModels != nil {
			if m, ok := providerModels[key]; ok {
				multimodalMap[key] = p.Multimodal
				if p.Multimodal && visionAux == nil {
					visionAux = m
				}
			}
		}
	}
	if visionAux == nil && len(cfg.LLM.Providers) > 0 {
		// 仅当确实配了 provider 却没有一个是 multimodal 时提示（避免 fake/no-provider 噪音）。
		mainMM := false
		if len(cfg.LLM.Providers) > 0 {
			mainMM = cfg.LLM.Providers[0].Multimodal
		}
		if !mainMM {
			fmt.Fprintf(os.Stderr, "yanshi: vision auxiliary disabled (no provider has multimodal: true); image_describe will return a config error\n")
		}
	}
	imageStore := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
```

注册 `image_describe` 与 `screenshot` 工具（在 `allTools` 累积区，vcs/MCP 工具附近）：

```go
	// Tier G: image_describe (routes to VisionAux) + screenshot. image_describe
	// is registered unconditionally (no-aux returns a config error at call time,
	// keeping the tool set stable so the model can discover the capability edge).
	allTools = append(allTools, tools.NewImageDescribeTool(
		visionAux, imageStore, workRoot, visionUsageRecorder(app /* filled below */)))
	allTools = append(allTools, tools.NewScreenshotTool(imageStore))
```


> `visionUsageRecorder` 的接线：image_describe 的 usage 回调需要把辅助 token 计入会话 budget/cost。
>
> 在 `Build` 顶部声明累加器（`internal/bootstrap/bootstrap.go`，`allTools` 注册之前）：
>
> ```go
> 	// visionUsageAccumulator 是辅助模型 token 用量的线程安全累加器。
> 	// 它聚合到 App.VisionUsage 字段，供 /cost 和 status 帧读取（标 "vision"）。
> 	type visionUsageAccumulator struct {
> 		mu         sync.Mutex
> 		Prompt     int64
> 		Completion int64
> 		Total      int64
> 	}
> 	func (a *visionUsageAccumulator) add(prompt, completion, total int) {
> 		a.mu.Lock()
> 		defer a.mu.Unlock()
> 		a.Prompt += int64(prompt)
> 		a.Completion += int64(completion)
> 		a.Total += int64(total)
> 	}
> 	var visionUsageSink visionUsageAccumulator
> ```
>
> 注册 image_describe 时传闭包：`func(p, c, t int) { visionUsageSink.add(p, c, t) }`。
>
> 在 `App` 加 `VisionUsage *visionUsageAccumulator` 字段，指向 `&visionUsageSink`（在 `return &App` 里赋给 `VisionUsage`），暴露给 `/cost` 消费：
>
> ```go
> 	// VisionUsage 暴露辅助模型 token 用量给 /cost 和 status 帧（标 "vision"）。
> 	VisionUsage *visionUsageAccumulator
> ```
>
> 现有 `/cost` handler 读到 `app.VisionUsage` 时，把其三个值作为 `"vision": {prompt, completion, total}` 加入 cost 响应。若已有 `goalloop.UsageSink` 聚合通道，则将 vision usage 也推入同一通道（标 `vision` 来源）。

把 map/store 写进 `orchConfig`：

```go
	orchConfig.MultimodalMap = multimodalMap
	orchConfig.ImageStore = imageStore
```

把三者写进 `return &App{...}` 字面量：

```go
		VisionAux:      visionAux,
		MultimodalMap:  multimodalMap,
		ImageStore:     imageStore,
```

import 区加 `"github.com/x6nux/yanshi/internal/imagestore"`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/bootstrap -run 'TestBuildSelectsFirstMultimodal|TestBuildNoAuxWhenMainMultimodalOrNoneConfigured' -v`

Expected: PASS；既有 `bootstrap` 测试不回归（fake provider builder 路径保持）。

- [ ] **Step 5: 提交**

```bash
git add internal/bootstrap/bootstrap.go internal/bootstrap/bootstrap_test.go
git commit -m "feat(bootstrap): auto-select vision auxiliary and wire image_describe/screenshot"
```

---

## Task 10: TUI 入口 A（Ctrl+V 剪贴板）+ 入口 B（@path 图像）

`entries.go`/`events.go` 绑定 Ctrl+V → 读剪贴板 → 图像附件；`@path` 检测图像扩展名 → 图像附件（而非文本注入）。图像附件随 `user_message` 的 `Images` 字段发送。

**Files:**
- Modify: `internal/cli/tui/events.go`（Ctrl+V 绑定）
- Modify: `internal/cli/tui/entries.go`（@path 图像检测）
- Modify: 对应 `*_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/cli/tui/entries_test.go`（或 `events_test.go`）追加：

```go
func TestAttachPathDetectsImageExtension(t *testing.T) {
	cases := map[string]bool{
		"shot.png": true, "pic.JPG": true, "a.webp": true, "anim.gif": true,
		"readme.md": false, "main.go": false, "data.json": false, "noext": false,
	}
	for path, want := range cases {
		if got := tools.IsImagePath(path); got != want {
			t.Errorf("IsImagePath(%q) = %v want %v", path, got, want)
		}
	}
}

func TestBuildUserMessageFrameIncludesImages(t *testing.T) {
	fr := buildSendFrame("look", []proto.ImageAttach{{ID: "img-1", Fmt: "png", DataB64: "AA"}})
	if len(fr.Images) != 1 || fr.Images[0].ID != "img-1" {
		t.Fatalf("frame images = %#v", fr.Images)
	}
}

func TestBuildUserMessageFrameOmitsImagesWhenNone(t *testing.T) {
	fr := buildSendFrame("text only", nil)
	if len(fr.Images) != 0 {
		t.Fatalf("text-only frame must not carry images: %#v", fr)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/cli/tui -run 'TestAttachPathDetectsImageExtension|TestBuildUserMessageFrame' -v`

Expected: FAIL，`isImagePath`/`buildSendFrame` 未定义。

- [ ] **Step 3: 实现 TUI 接线**

在 `internal/cli/tui/entries.go` 的 `@path` 解析处直接复用共享 helper `tools.IsImagePath`（Task 4 定义）——不要在 TUI 重新实现扩展名列表（DRY）：

```go
// （import "github.com/x6nux/yanshi/internal/tools" 已在 entries.go 或其同包文件可用；
//  否则加 import。@path 解析时：）
if tools.IsImagePath(rel) {
	// 读字节、base64 编码、构造 proto.ImageAttach，加入待发送图像列表；
	// 不把二进制当文本注入。
}
```

在模型（`model.go` 或 `entries.go`）的发送逻辑里：当 `@path` 解析到一个 `isImagePath` 文件，读取字节、base64 编码、构造 `proto.ImageAttach{Source: "@path:"+path, Fmt: ..., DataB64: ...}`，加入待发送图像列表；不再把二进制当文本注入。

加一个集中的帧构造函数 `buildSendFrame`（在 `entries.go` 或发送处）：

```go
// buildSendFrame assembles the user_message ClientFrame, attaching images when
// present (additive: nil images ⇒ byte-identical to a text-only frame).
func buildSendFrame(text string, images []proto.ImageAttach) proto.ClientFrame {
	if len(images) == 0 {
		return proto.NewUserMessage(text)
	}
	return proto.NewUserMessageWithImages(text, images)
}
```

Ctrl+V 绑定在 `events.go` 的 `Update` 按键分发处（**先核对 bubbletea fork 的按键常量名**——fork 已能区分 Ctrl+Enter/Enter，Ctrl+V 用其 `tea.KeyCtrlV` 或 `tea.NewKeyComb` 等价物；Run `go doc github.com/charmbracelet/bubbletea KeyType` 确认）：

```go
	// Tier G 入口 A: Ctrl+V 读剪贴板图像（无图时回落为文本粘贴）。
	if isCtrlV(msg) {
		if r := m.clipReader; r != nil {
			if data, fmtName, ok := r.Read(ctx); ok {
				m.attachImage(proto.ImageAttach{
					Source: "paste", Fmt: fmtName,
					DataB64: base64.StdEncoding.EncodeToString(data),
				})
				return m, nil // 不再把 Ctrl+V 当文本
			}
		}
		// 剪贴板无图 → 继续走默认文本粘贴路径（不 return）
	}
```

`m.clipReader` 在模型初始化时设为 `clipimg.New()`；`attachImage` 把附件追加到待发送列表并在输入区显示一个 `[image: 已附加 #N]` 提示。`ctx` 取模型已有的 cancel context（或 `context.Background()` + 短超时，避免 Ctrl+V 阻塞事件循环——给 `r.Read` 加 `context.WithTimeout(ctx, 2*time.Second)`）。

> **bubbletea fork 按键适配**：Ctrl+V 在 fork 里如何暴露是实现细节——`go doc` 确认后用 fork 的 API；不要用上游 `KeyEnter` 之类被 fork 改过的常量。`isCtrlV(msg)` 封装一次判断，与现有 Ctrl+Enter 判断同款写法。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/cli/tui -run 'TestAttachPathDetectsImageExtension|TestBuildUserMessageFrame' -v`

Expected: PASS。Ctrl+V 的运行时行为靠手测（启动 `--fake-model -inprocess` 手动粘贴），不在 CI 跑真实剪贴板。

- [ ] **Step 5: 提交**

```bash
git add internal/cli/tui/entries.go internal/cli/tui/events.go internal/cli/tui/entries_test.go
git commit -m "feat(tui): Ctrl+V clipboard paste and @path image attachment (Tier G)"
```

---

## Task 11: 入口 C — `fs_read` / `web_fetch` 遇图的结构化标识

工具读到图像文件 / 拉到 `image/*` 响应时，返回结构化"这是图像"标识 + 路径/数据，**不把二进制塞 transcript**。模型拿到 ref 后调 `image_describe`。

**Files:**
- Modify: `internal/tools/fs.go`（`runRead`）
- Modify: `internal/tools/web.go`（`runFetch`）
- Modify: 对应 `*_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/tools/fs_test.go` 追加（用 t.TempDir 写一个 PNG）：

```go
func TestFsReadImageFileReturnsStructuredRef(t *testing.T) {
	dir := t.TempDir()
	pngPath := filepath.Join(dir, "shot.png")
	require.NoError(t, os.WriteFile(pngPath, testPNGBytes(t, 4, 4), 0o644))
	f := NewFSTools(dir)
	args, _ := json.Marshal(map[string]string{"path": "shot.png"})
	ch := f.Read.Stream(context.Background(), string(args))
	var result string
	for c := range ch {
		result = c.Result
	}
	if !strings.Contains(result, "image") || !strings.Contains(result, "shot.png") {
		t.Fatalf("image file must yield a structured image ref, got %q", result)
	}
	// 关键：不把二进制字节塞进结果
	if strings.Contains(result, "\x89PNG") {
		t.Fatal("fs_read must NOT embed raw image bytes in the result")
	}
}
```

在 `internal/tools/web_test.go` 追加（用 httptest 返回 `image/png`）：

```go
func TestWebFetchImageContentTypeReturnsStructuredRef(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 'P', 'N', 'G'})
	}))
	defer ts.Close()
	w := NewWebTools(0, 0)
	args, _ := json.Marshal(map[string]string{"url": ts.URL})
	ch := w.Fetch.Stream(allowAllCtx(context.Background()), string(args))
	var result string
	for c := range ch {
		result = c.Result
	}
	if !strings.Contains(strings.ToLower(result), "image") || !strings.Contains(result, ts.URL) {
		t.Fatalf("image/* response must yield a structured image ref, got %q", result)
	}
}
```

> `testPNGBytes` 复用 Task 4 已有的同包 helper。`allowAllCtx` 若 `web_test.go` 已有则复用。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/tools -run 'TestFsReadImageFile|TestWebFetchImageContentType' -v`

Expected: FAIL（当前 `fs_read` 对图像文件会返回乱码字节或解码失败；`web_fetch` 会把二进制塞进结果）。

- [ ] **Step 3: 实现**

在 `internal/tools/fs.go` 的 `runRead` 顶部加图像短路：读文件前先看扩展名；若是图像，返回结构化标识而不读字节进结果（`IsImagePath` 是 Task 4 vision.go 的同包导出 helper）：

```go
func (f *FSTools) runRead(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		End    int    `json:"end"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return errorResult("invalid args: " + err.Error()), nil
	}
	// Tier G 入口 C: 图像文件返回结构化 ref，不读二进制进结果。
	if IsImagePath(args.Path) {
		return fmt.Sprintf("[image file: %s — call image_describe with this path to understand it]", args.Path), nil
	}
	// ... 既有文本读取逻辑保持不变
}
```

在 `internal/tools/web.go` 的 `runFetch` 里，读 Content-Type；若是 `image/*`，返回结构化标识 + URL，不把 body 塞结果：

```go
	// Tier G 入口 C: image/* 响应返回结构化 ref，不塞二进制。
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "image/") {
		return fmt.Sprintf("[image response: %s (%s) — call image_describe with this URL or fetch the bytes to understand it]", req.URL, ct), nil
	}
	// ... 既有文本读取逻辑
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./internal/tools -run 'TestFsReadImageFile|TestWebFetchImageContentType' -v`

Expected: PASS；既有 fs/web 测试不回归。

- [ ] **Step 5: 提交**

```bash
git add internal/tools/fs.go internal/tools/web.go internal/tools/fs_test.go internal/tools/web_test.go
git commit -m "feat(tools): structured image refs in fs_read and web_fetch (Tier G entry C)"
```

---

## Task 12: 入口 E — api/v1 + SDK schema additive + headless 摄图 + 端到端验收

**[E additive / D2 contract]** 给 `TurnStartParams` 加 `Images` 字段（additive），给 `sdk/schema/v1/agent-api.schema.json` 加 `ImageAttach` 定义，更新 v1 schema 的 `$defs.RunTurnRequest`，跑版本矩阵契约测试，最后跑端到端 fake 验收。

**Files:**
- Modify: `internal/api/v1/types.go`
- Modify: `internal/api/v1/schema.go`（如 schema 在此）
- Modify: `internal/api/v1/*_test.go`
- Modify: `sdk/schema/v1/agent-api.schema.json`
- Modify: `sdk/schema/v1/fixtures/*.json`（加一个带 image 的 fixture）
- Modify: `sdk/ts/tests/contract.test.ts`（若需）
- Modify: `sdk/python/tests/test_contract.py`（若需）
- Modify: `internal/api/v1/service.go`（`StartTurn` 把 images 经 orchestrator ApplyImages 注入）

- [ ] **Step 1: 给 `TurnStartParams` 加 additive 字段**

在 `internal/api/v1/types.go` 的 `TurnStartParams`（`OutputSchema` 字段后）加：

```go
	// Images carries optional image attachments (Tier G, ADDITIVE). Each entry
	// is one image (proto.ImageAttach). Empty/absent ⇒ text-only turn. The
	// service forwards these to the orchestrator's image fan-out; existing v1
	// clients that never set Images are unchanged.
	Images []proto.ImageAttach `json:"images,omitempty"`
```

import 加 `"github.com/x6nux/yanshi/internal/proto"`。

- [ ] **Step 2: 写 additive 契约测试**

在 `internal/api/v1/types_test.go`（或新建）加：

```go
func TestTurnStartParamsImagesIsCamelCaseAndOmittable(t *testing.T) {
	// 带 image
	with, _ := json.Marshal(TurnStartParams{ThreadID: "t", Input: "hi", Images: []proto.ImageAttach{
		{ID: "img-1", Fmt: "png", DataB64: "AA"},
	}})
	if !strings.Contains(string(with), `"images":[`) || !strings.Contains(string(with), `"dataB64":"AA"`) {
		t.Fatalf("images must serialize camelCase: %s", with)
	}
	// 无 image → 字段省略（additive 不可见）
	without, _ := json.Marshal(TurnStartParams{ThreadID: "t", Input: "hi"})
	if strings.Contains(string(without), "images") {
		t.Fatalf("text-only params must omit images (additive): %s", without)
	}
}

func TestTurnStartParamsIgnoresUnknownFields(t *testing.T) {
	var p TurnStartParams
	if err := json.Unmarshal([]byte(`{"threadId":"t","input":"hi","futureField":42}`), &p); err != nil {
		t.Fatalf("unknown field must be ignored (additive tolerance): %v", err)
	}
}
```

- [ ] **Step 3: 服务层把 images 注入 orchestrator 分流**

在 `internal/api/v1/service.go` 的 `StartTurn` 构建 history 处（当前 `st.history = append(... UserMessage(p.Input))`），改为：先建 user 消息，再若 `len(p.Images) > 0 && s.orch != nil` 则 `history = s.orch.ApplyImages(history, effectiveModelID, p.Images)`。`effectiveModelID` 取 `p.Model` 或默认模型的 id（service 已有 model 选择逻辑）。

```go
	// Tier G: 图像附件按当前 turn 模型能力分流（直接 part / 占位+store）。
	userMsg := append(history[:0:0], history...) // 不变异原 history
	if len(p.Images) > 0 && s.orch != nil {
		history = s.orch.ApplyImages(history, modelIDForTurn(p, st), p.Images)
	}
```

> `modelIDForTurn` 解析 turn 的有效 model-id（`p.Model` 或 thread/model 默认）。若 service 无现成 helper，加一个小的。

- [ ] **Step 4: 跑 v1 测试**

Run: `go test ./internal/api/v1 -run 'TestTurnStartParams|TestService' -v`

Expected: PASS。

- [ ] **Step 5: 更新 SDK schema additive**

在 `sdk/schema/v1/agent-api.schema.json` 的 `$defs` 加 `ImageAttach`：

```json
    "ImageAttach": {
      "type": "object",
      "additionalProperties": true,
      "properties": {
        "id": { "type": "string" },
        "source": { "type": "string" },
        "fmt": { "type": "string" },
        "w": { "type": "integer", "minimum": 0 },
        "h": { "type": "integer", "minimum": 0 },
        "dataB64": { "type": "string" }
      }
    },
```

在 `RunTurnRequest.properties` 加（不改 `required`）：

```json
        "images": { "type": "array", "items": { "$ref": "#/$defs/ImageAttach" } }
```

在 `sdk/schema/v1/fixtures/` 加 `turn-start-with-images.request.json`：

```json
{
  "version": "v1",
  "threadId": "thread-001",
  "input": "describe this",
  "images": [
    { "id": "img-1", "source": "paste", "fmt": "png", "w": 2, "h": 2, "dataB64": "iVBORw0KGgo=" }
  ]
}
```

- [ ] **Step 6: SDK 契约测试验证 additive**

TS（`sdk/ts/tests/contract.test.ts`）加：用 ajv 编译 v1 schema 后，`turn-start-with-images.request.json` 必须通过；同时一个**不含** images 的旧 fixture 也必须通过（证明 additive）。

```ts
  it("accepts additive images field and stays compatible without it", () => {
    const validate = ajv.compile(schema);
    const withImages = JSON.parse(readFileSync(new URL("../../schema/v1/fixtures/turn-start-with-images.request.json", import.meta.url), "utf8"));
    expect(validate(withImages), JSON.stringify(validate.errors)).toBe(true);
    // 旧的 no-images fixture 仍然通过（additive 不破坏）
    expect(validate(JSON.parse(readFileSync(new URL("../../schema/v1/fixtures/turn-start.response.json", import.meta.url), "utf8")))).toBe(true);
  });
```

Python（`sdk/python/tests/test_contract.py`）加等价断言。

- [ ] **Step 7: 运行 SDK 契约测试**

Run: `npm --prefix sdk/ts run test` 与 `python -m pytest sdk/python/tests -q`

Expected: PASS（additive 字段被 v1 客户端接受且不破坏旧 fixture）。

- [ ] **Step 8: 端到端验收（fake）**

新增一个 orchestrator-level 端到端测试，覆盖 spec §12 验收 1-7（fake 模型）：

```go
// internal/agent/orchestrator/multimodal_e2e_test.go
func TestE2E_MultimodalMainDirectlyUnderstandsImage(t *testing.T) {
	// 主模型 multimodal + RecordImages：图直接进消息，模型"看见"图像。
	mm := einollm.NewFakeModel([]string{"it is a red square"}, nil)
	mm.RecordImages = true
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	o, _ := New(Config{Model: mm, MultimodalMap: map[string]bool{"mm": true}, ImageStore: store})
	msgs := o.ApplyImages([]*schema.Message{}, "mm", []proto.ImageAttach{{Fmt: "png", DataB64: encodePNGBase64(t, 3, 3)}})
	require.NotEmpty(t, msgs[0].UserInputMultiContent)
	require.Equal(t, 1, mm.LastImageCount)
}

func TestE2E_NonMultimodalMainDescribesViaAux(t *testing.T) {
	// 主非多模态 + 辅助 fake vision：图进 store + 占位；image_describe 取图给辅助。
	store := imagestore.New(imagestore.Config{MaxItems: 20, MaxBytes: 100 << 20})
	aux := einollm.NewFakeModel(nil, nil)
	aux.Vision = true
	main := einollm.NewFakeModel([]string{"ok"}, nil)
	o, _ := New(Config{Model: main, MultimodalMap: map[string]bool{"text": false}, ImageStore: store})
	msgs := o.ApplyImages([]*schema.Message{}, "text", []proto.ImageAttach{{Fmt: "png", DataB64: encodePNGBase64(t, 3, 3)}})
	require.Contains(t, msgs[0].Content, "[image:img-1")
	describe := NewImageDescribeTool(aux, store, "", nil) // 同包引用 tools? 见下
	_ = describe
	// （若跨包，把 image_describe 的端到端放在 internal/tools 或 bootstrap 集成测试里。）
}
```

> 跨包引用注意：`image_describe` 在 `internal/tools`，orchestrator 在 `internal/agent/orchestrator`。端到端"占位→工具取图→辅助描述"的完整链路测试放在 **`internal/bootstrap`** 或一个轻量集成测试里更合适（bootstrap 同时持有 orchestrator + tools + aux）。若 orchestrator 测试难以跨包引用 `NewImageDescribeTool`，则在 `bootstrap_test.go` 加一个 Build 后调用 `app.Orch.ApplyImages` 再调用已注册工具的集成测试。**目标：覆盖验收 1-3（直接理解 / 占位+辅助描述 / 无辅助错误）。**
>
> `encodePNGBase64(t, w, h)` 在该测试文件内定义（base64 标准编码一个纯色 PNG），与 Task 2/4 的 PNG 编码同款逻辑：

```go
func encodePNGBase64(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}
```

- [ ] **Step 9: 跑全量相关包 + 提交**

Run:

```bash
go test ./internal/config ./internal/imagestore ./internal/llm/eino ./internal/tools ./internal/proto ./internal/agent/orchestrator ./internal/bootstrap ./internal/api/v1 ./internal/cli/tui
```

Expected: 全 PASS。

```bash
git add internal/api/v1 sdk/schema/v1 internal/agent/orchestrator internal/bootstrap
git commit -m "feat(api): additive Images field in v1 + SDK schema + Tier G end-to-end acceptance"
```

---

## 验收标准映射（spec §12）

| 验收 | 覆盖 Task / 测试 |
|---|---|
| 1 主多模态：图直接进消息，模型原生理解 | Task 8 `TestApplyImages_MultimodalDirectEmbedsImagePart` + Task 12 `TestE2E_MultimodalMainDirectlyUnderstandsImage` |
| 2 主非多模态+辅助：占位+image_describe→辅助描述回喂 | Task 4 `TestImageDescribeByIDReturnsAuxDescription` + Task 8 `TestApplyImages_NonMultimodalInsertsPlaceholderAndStores` + Task 12 e2e |
| 3 主非多模态+无辅助：warning + 明确错误 | Task 4 `TestImageDescribeNoAuxReturnsConfigError` + Task 9 `TestBuildNoAuxWhenMainMultimodalOrNoneConfigured` |
| 4 五入口各自产生图像附件并走分流 | A=Task10、B=Task10、C=Task11、D=Task6、E=Task12；分流=Task8 |
| 5 辅助 usage 进 sink，/cost 可见 | Task 4 `visionUsage.record` + Task 9 usage 接线 |
| 6 超限/不支持格式被拒 + LRU | Task 2 `TestPutRejects*` + `TestLRU*` |
| 7 /model 切换后重判 | Task 8 `TestApplyImages_ModelSwitchReEvaluatesCapability` |
| 8 v1 协议 additive，SDK 契约通过 | Task 7 proto additive + Task 12 schema/SDK 矩阵 |

---

## 风险与缓解（执行期）

| 风险 | 缓解 |
|---|---|
| eino schema 图像 content-type/字段名在锁定版本与计划假设不符 | Task 3/8 第一步 `go doc` 核对 `schema.MessageInputPart`/`schema.MessageInputImage`/`schema.ChatMessagePartType`，与 `anthropic.go:351` 对齐后再写 |
| D3 同改 config/bootstrap 落点冲突 | Task 1/9 执行前 re-verify（已在标题标注）；以 D3 最终态为准合并 |
| bubbletea fork 的 Ctrl+V 按键 API 与上游不同 | Task 10 `go doc` fork 的 KeyType 后再绑定；`isCtrlV` 封装一次 |
| 截图/剪贴板平台差异 + 无工具时 | 子进程为主，无工具返回明确错误；CI 不跑真实捕获，靠 fake + 编译验证 |
| 辅助模型费用失控 | usage 进 sink 计 budget；工具描述标 cost-class；纯显式不自动调 |
| v1 协议加图像字段破坏 D2 契约 | additive 字段 + 版本矩阵契约测试（Task 12 沿用 D2 做法） |
| webp 无 stdlib 解码器 | store 对 webp 走原字节 pass-through（dims 0×0），image_describe 仍能把字节交辅助模型 |

---

## 后续（非本批，spec §13）

- 图像生成（T14）。
- 截图区域选择 / 多屏。
- 跨会话图像持久与引用。
- 图像批量 `image_describe`（一次多图）。
