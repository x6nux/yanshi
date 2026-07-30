# Tier G — 多模态理解（vision）设计

> **日期**：2026-07-22
> **归属**：E-H roadmap 的 Tier G（`docs/feature-roadmap-e-h.md`）
> **命题**：给非原生多模态的主模型兜底图像理解——主模型不换，图像理解按需走"辅助多模态模型 + `image_describe` 工具"代理。
> **范围**：只做**理解/识别**，不做图像生成（生成留后续）。
> **状态**：设计稿（brainstorm 产出），待用户审阅 → writing-plans。

---

## 1. 目标与非目标

### 目标

- 配置里用 `multimodal: bool` 声明每个 provider 是否原生支持图像。
- 主模型多模态 → 图像直接进消息（原生 image content）。
- 主模型**非**多模态 → 图像以占位引用进上下文，主模型显式调 `image_describe` 工具；工具把图+问题路由给**自动选定的辅助多模态模型**，返回文本描述回喂。
- 支持五个图像入口：剪贴板粘贴、`@path` 文件、工具遇图（`fs_read`/`web_fetch`）、截图工具、headless/SDK/IDE 传图。
- 未配辅助 + 主非多模态 → 清晰错误，不静默失败。

### 非目标（v1 不做）

- 图像**生成**。
- 视频理解。
- 图像编辑（inpainting/outpainting）。
- 跨会话图像持久（store 是会话级，进程内）。

---

## 2. 背景

- `ProviderConfig`（`internal/config/config.go:305`）当前无多模态标记；`BuildProviders`（`internal/llm/eino/provider.go:46`）按 model-id 建 `providerModels` map。
- `bootstrap.Build`（`internal/bootstrap/bootstrap.go`）装配所有 `GuardedTool`（`internal/tools/guard.go:150`，按 set 分组，如 `NewGitTools()`）；`providerModels` 是 name/model-id → `model.BaseChatModel`。
- TUI `entries.go` 目前只处理**文本粘贴**，无图像附件；`@path` 文件附加（UX3）未确认支持图像。
- 辅助模型与现有 `UsageSink`（goalloop）、pricing（COST1）、OTel（OBS2）复用。
- **D3 正在改 `config`/`bootstrap`**（secrets/auth/i18n/keymap）；本 spec 落地前须 re-verify 这两个文件的落点未被改写。

---

## 3. 架构：按主模型能力分两路

图像在 turn 构建时按**当前 turn 活动模型**的多模态能力分流（支持 `/model` 中途切换）：

```
图像附件(任意入口 A-E)
   │
   ├─ 主模型 multimodal=true ──→ 作为 schema.Message 的 image part 直接进消息
   │                             (原生; 不经工具, 不进 image store)
   │
   └─ 主模型 multimodal=false ──→ 进会话级 image store (稳定 id)
                                 → 用户消息插占位 [image:img-N | src | WxH fmt]
                                 → 主模型识别占位 → 显式调 image_describe(img-N, 问?)
                                 → 工具按 id 取图字节 + question
                                 → 发给辅助多模态模型 → 文本描述作 tool result 回喂
```

**辅助模型自动选定**：bootstrap 扫 `cfg.LLM.Providers`，取第一个 `Multimodal==true` 的 provider 作为 vision 辅助（从 `providerModels` 里按其 model-id 拿 `BaseChatModel`）。多个时取第一个 + 打 warning；主模型自己多模态时仍可选定辅助（供 `/model` 切到非多模态模型后使用），但非必需。

**能力判定来源**：bootstrap 构建 `providerMultimodal map[model-id]bool`（与 `providerModels` 同 key），传给 orchestrator/api，供 turn 构建时按当前 model-id 查询。

---

## 4. 配置

### 4.1 schema（改 `internal/config/config.go`）

`ProviderConfig` 增一字段：

```go
type ProviderConfig struct {
    Name          string `yaml:"name"`
    Kind          string `yaml:"kind"`
    Model         string `yaml:"model"`
    APIKey        string `yaml:"api_key"`
    BaseURL       string `yaml:"base_url"`
    CostClass     string `yaml:"cost_class"`
    ContextWindow int    `yaml:"context_window"`
    // Multimodal 声明该 provider 原生支持图像输入(Tier G)。主模型非多模态时,
    // bootstrap 自动选第一个 Multimodal==true 的 provider 作 vision 辅助。
    Multimodal bool `yaml:"multimodal"`
}
```

不加 `vision_model` 字段（用户决定纯自动选择）。`applyDefaults`/`validate` 不需要新逻辑（零值 = 非多模态，合法）。

### 4.2 示例（`config.example.yaml`）

```yaml
llm:
  providers:
    - name: deepseek
      kind: openai
      model: deepseek-chat
      multimodal: false        # 主模型: 文本-only
    - name: claude
      kind: anthropic
      model: claude-opus-4-8
      multimodal: true         # 自动选作 vision 辅助
```

### 4.3 启动校验（`bootstrap.Build`）

- 主模型 `multimodal==true` → 无需辅助（图直接进消息）；仍构建 `providerMultimodal` map 供 `/model` 切换判定。
- 主模型 `multimodal==false` 且**存在** `multimodal==true` provider → 选定辅助，注册 `image_describe` 工具。
- 主模型 `multimodal==false` 且**无**任何 `multimodal==true` provider → 打 warning（不阻塞启动），`image_describe` 仍注册但调用时返回配置错误。

---

## 5. 图像入口（A-E，全做）

### A. 剪贴板粘贴（Ctrl+V → 读 OS clipboard）— 主入口

- TUI 绑定 Ctrl+V（bubbletea fork 已能精细区分按键）。
- 触发时读系统剪贴板：新建 `internal/clipimg/`（平台 adapter）。
  - Windows：syscall 读 `GetClipboardData(CF_DIB/CF_BITMAP)` → 编码 png。
  - macOS：`pngpaste` 子进程 或 NSPasteboard（cgo）。
  - Linux：`wl-paste -t image/png` / `xclip -selection clipboard -t image/png -o` 子进程。
  - 统一接口 `Read() (image bytes, format, ok)`；剪贴板无图 → `ok=false`，不干扰文本粘贴。
- **cgo 策略**：优先子进程路线（与 `CGO_ENABLED=0` 打包 PKG1 兼容）；cgo 库（如 `golang.design/x/clipboard`）作为可选增强，受 build-tag 门控。spec 阶段定子进程为主、cgo 为辅。

### B. `@path` 图像文件

- `entries.go` 的 `@path` 附加扩展：检测扩展名（png/jpg/webp/gif）→ 作为图像附件（而非文本注入）。
- 图像附件统一进 image store 拿 id（占位一致）；`image_describe` 额外接受裸路径 ref（模型独立引用路径时用），经 guard fs 校验、按需读字节。

### C. 工具遇图（`fs_read` / `web_fetch`）

- `fs_read` 读到图像文件 → 返回结构化"这是图像"标识 + 路径（不把二进制塞 transcript）。
- `web_fetch` 拉到 image/* 内容 → 同理。
- 模型拿到图像 ref 后调 `image_describe`。

### D. 截图工具（平台相关，最重）

- 新 `internal/tools/screenshot.go`：`screenshot` 工具，按平台捕获（Windows GDI BitBlt / macOS `screencapture` / Linux `grim` 或 xdg-desktop-portal）→ 返回图像 ref（进 store）。
- approval-required（截图是敏感操作）；默认放整个主屏，多屏/区域选择留后续。

### E. headless / SDK / IDE 传图

- D1 v1 协议 `InputItem` 增加图像字段（additive，不破坏现有契约）。
- `internal/api/v1/types.go` + `sdk/schema/v1/agent-api.schema.json` 同步；TS/Python SDK 生成类型更新。
- TUI/SDK 收到图像 InputItem → 进 image store → 走占位/直接两路。

---

## 6. 会话级 image store（新 `internal/imagestore/`）

```go
type Store struct { /* mutex + map[id]Entry + LRU */ }
type Entry struct {
    ID string; Source string; Fmt string; W, H int; Bytes []byte; Created time.Time }
func (s *Store) Put(bytes []byte, source, fmt string) (id string, err error)  // 降采样+入量检查
func (s *Store) Get(id string) (Entry, bool)
func (s *Store) ByPath(path string) (Entry, bool)  // 可选: @path/fs_read 缓存
```

- 会话级（随 WS 连接/turn 上下文生命周期）；进程内内存。
- 上限：≤20 张 或 ≤100MB，LRU 淘汰；超限淘汰最旧并打 activity 提示。
- 入量时：格式校验（png/jpeg/webp/gif，gif 取首帧）、单图 ≤10MB、长边 >2048px 降采样。
- 占位格式：`[image:img-3 | paste | 1280×720 png]`（id | source | WxH fmt）。

---

## 7. `image_describe` 工具（新 `internal/tools/vision.go`）

```
image_describe(image_ref: string, question?: string) -> string
```

- `image_ref`：image id（`img-3`，从 store 取）或路径（`shot.png`，fs 读）。
- `question`：可选；空 = "请描述这张图片的内容"。
- 流程：解析 ref → 取字节 → 组装 `[image bytes + question]` 发给**辅助多模态模型**（非流式单轮）→ 返回文本描述。
- **纯显式**：主模型自己决定何时调（不自动预描述）。
- guard：路径型 ref 经 fs 校验（复用 guard fs 维度）；工具描述标注 cost-class（走辅助模型，有费用）。
- 错误（作为 tool result 回喂，不中断 turn）：
  - 无辅助模型 → "主模型非多模态且未配置 multimodal: true 的 provider；请在 config 里加一个"。
  - ref 解析失败/文件越权 → 对应错误。
  - 辅助调用失败（网络/4xx）→ 上报原因，模型可重试。
- 用量：辅助模型响应的 usage → `UsageSink`（标 `vision`），计入 goalloop budget 与 `/cost`。

### 注册时机

- 存在辅助模型时注册（主多模态时也注册，供 `/model` 切到非多模态后可用）。
- 无辅助时仍注册（调用返回配置错误），保证工具集稳定、模型可发现能力边界。

---

## 8. orchestrator 集成

- `orchestrator.Config` 增 `MultimodalMap map[string]bool`（model-id → 能力）与 `ImageStore`、`VisionAux model.BaseChatModel`（或经 context 注入）。
- turn 构建：拿到用户消息的图像附件后，按**当前 turn model-id** 查 `MultimodalMap`：
  - `true` → 图像作为 `schema.Message.MultiContent` 的 image part（eino 支持 image_url；字节经 data URL 或适配器支持的 bytes 形式——实现时按锁定 eino 版本核对字段）。
  - `false` → 图像入 `ImageStore`（若未在），消息插占位文本。
- `/model` 切换天然兼容（判定是 per-turn 的）。

---

## 9. 文件结构

| 文件 | 职责 | 新/改 |
|---|---|---|
| `internal/config/config.go` | `ProviderConfig.Multimodal` | 改 |
| `internal/config/config_test.go` | 新字段解析 | 改 |
| `config.example.yaml` | `multimodal` 示例 + 说明 | 改 |
| `internal/llm/eino/provider.go` | `BuildProviders` 附带返回/暴露 multimodal 标记（或 bootstrap 自建 map） | 改 |
| `internal/bootstrap/bootstrap.go` | 选辅助、建 `providerMultimodal` map、装 image store + `image_describe` + screenshot、注入 orchestrator | 改 |
| `internal/imagestore/store.go` + `_test.go` | 会话级 image store（id/LRU/降采样/格式校验） | 新 |
| `internal/tools/vision.go` + `vision_test.go` | `image_describe` GuardedTool | 新 |
| `internal/tools/screenshot.go` + 平台文件 + `_test.go` | `screenshot` 工具（Windows/macOS/Linux） | 新 |
| `internal/clipimg/clipimg.go` + `_*_windows.go`/`_*_unix.go` + `_test.go` | 剪贴板图像读取 adapter | 新 |
| `internal/cli/tui/entries.go`/`events.go` | Ctrl+V 绑定、`@path` 图像检测、图像附件注入 | 改 |
| `internal/tools/fs.go`/`web.go` | 图像文件/响应的结构化标识（不塞二进制） | 改 |
| `internal/agent/orchestrator/orchestrator.go` | turn 构建的图像分流（直接 vs 占位）、`MultimodalMap` | 改 |
| `internal/proto/frame.go` | 用户消息图像附件帧（A-E 统一表示） | 改 |
| `internal/api/v1/types.go` + `sdk/schema/v1/*.json` | `InputItem` 图像字段（additive） + SDK 类型 | 改 |
| `cmd/api-schema` | 重生成 schema/类型 | 改 |

---

## 10. 测试策略（Fake 优先）

- **Fake 辅助模型**：扩展 `einollm.FakeModel` 支持图像输入 → 返回确定性描述（如 `"fake-vision(img-N)"`），无需真实多模态 API。
- imagestore：Put/Get、LRU 淘汰、降采样、格式/尺寸拒绝、并发。
- `image_describe`：id ref、路径 ref、越权路径、无辅助错误、辅助失败错误、question 透传、usage 回流。
- clipimg：每平台 adapter（注入 fake 子进程/cgo stub）；无图时 ok=false 不影响文本。
- screenshot：平台 adapter（fake 捕获）。
- bootstrap：辅助自动选定（第一个 multimodal）、无辅助 warning、`providerMultimodal` map 正确、主多模态时不强制辅助。
- orchestrator：分流正确性（多模态主→直接；非多模态→占位+工具）、`/model` 切换后重判。
- 协议/SDK：`InputItem` 图像字段 additive 契约测试（版本矩阵）。

---

## 11. 风险与缓解

| 风险 | 缓解 |
|---|---|
| eino schema 图像字段在不同 adapter(openai/anthropic/responses) 不一致 | 实现时按锁定 eino 版本核对；三种 kind 各跑端到端 fake |
| 剪贴板 cgo 与 PKG1 `CGO_ENABLED=0` 打包冲突 | 子进程路线为主、cgo build-tag 为辅；doctor 检测 clipboard 能力 |
| 截图工具平台差异大、权限弹窗 | D 入口最重，可拆为 G 的后续子任务；approval-required |
| 辅助模型费用失控 | usage 进 sink 计 budget；工具描述标 cost-class；纯显式不自动调 |
| D3 同时改 config/bootstrap 落点冲突 | 执行前 re-verify；spec 任务里标注依赖 D3 的 config/bootstrap 最终态 |
| 协议加图像字段破坏 v1 契约 | additive 字段 + 版本矩阵契约测试（沿用 D2 做法） |

---

## 12. 验收标准

1. 主模型多模态：粘贴/`@path` 图像 → 图直接进消息，模型原生理解（fake 端到端）。
2. 主模型非多模态 + 有辅助：图像进 store + 占位；调 `image_describe` → 辅助返回描述回喂。
3. 主模型非多模态 + 无辅助：启动 warning；`image_describe` 返回明确配置错误。
4. 五个入口（A-E）各自可产生图像附件并走分流。
5. 辅助 usage 进 `UsageSink`，`/cost` 可见。
6. 图像超限（>10MB / 不支持格式）被拒并给可读错误；store LRU 生效。
7. `/model` 切换后图像分流按新模型能力重判。
8. v1 协议 additive，TS/Python SDK 契约测试通过。

---

## 13. 后续（非本批）

- 图像生成（T14）。
- 截图区域选择 / 多屏。
- 跨会话图像持久与引用。
- 图像批量 `image_describe`（一次多图）。
