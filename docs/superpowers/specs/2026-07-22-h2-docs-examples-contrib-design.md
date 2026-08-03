# Tier H2 — 文档 / examples / 贡献 设计

> **日期**：2026-07-22
> **归属**：E-H roadmap 的文档与 onboarding tier（`docs/feature-roadmap-e-h.md` 的 Tier H）
> **命题**：把"内部强、对外弱"的文档现状翻面——内部分析文档已 9/10，但面向用户的"怎么用 yanshi"+ 可运行示例 + 贡献入口缺失。H2 补齐这块，**放在行为稳定后**（A–G 落地、协议/SDK 冻结）。
> **范围**：只写面向外部的文档、示例、贡献指南与 docs 治理；**不新增功能面**，不改运行时行为。
> **状态**：设计稿，待用户审阅 → writing-plans。

> ⚠️ **D2/O12 已作废** —— VS Code 扩展（`ide/vscode/`）与 `scripts/check-d2.sh` 已于 2026-08
> 以移除方式结案（spec `docs/superpowers/specs/2026-08-03-yanshi-roadmap-design.md` §3.2 ④），
> 由 `internal/archtest::TestVSCodeExtensionRemoved` 守住。本文中一切把它当作交付物/待办的
> 描述均已失效，**不要照做**。

> **命名说明（重要）**：team 内已把 roadmap 的 H tier 重组为 hybrid 结构——**H1 = 发布工程**（roadmap 原 Tier G：VER/CIG/PKG/UPG），**H2 = 文档 / examples / 贡献**（roadmap 原 Tier H 全部：UDOC1 + APIREF1 + ADR1 + EX1 + CONTRIB1）。本 spec 即 team-H2，覆盖 5 个条目，以本定义为权威；roadmap 文件里"Batch H1=文档 / Batch H2=examples"的旧切分是 stale 的，需在 roadmap 自检（task #2）里对齐（见 open question OQ-1）。

---

## 1. 目标与非目标

### 目标

- **UDOC1**：面向用户的"怎么用 yanshi"指南（`docs/user-guide/`），getting started 可零依赖（`--fake-model`）跑通；覆盖配置、TUI、技能、autoVCS、goalloop、guard、headless/SDK/IDE 入口。
- **APIREF1**：v1 Agent API 统一参考（`docs/api/`）：thread/turn/item 资源、SSE 事件、JSON Schema、TS/Python SDK 用法、app-server JSON-RPC。
- **ADR1**：架构决策记录（`docs/adr/`），把散在 CLAUDE.md / synthesis §9 的关键决策结构化、可检索。
- **EX1**：可运行示例目录（`examples/`），每个 fake-model 友好可跑；CI 验证可编译。
- **CONTRIB1**：`CONTRIBUTING.md` + docs 归档（`docs/archive/`），保留 CLAUDE.md 为权威内文档。

### 非目标（H2 不做）

- 新增功能面、工具、协议帧（A–G 已冻结功能面）。
- 国际化文档翻译（i18n 是 D3-I18N1 的运行时能力，文档本身先出中文+英文双语标题即可，不做全量多语言文档站）。
- 自建文档网站/静态站点生成器（v1 用 Markdown + 仓库内导航；是否上 mkdocs/Docusaurus 是后续 OQ）。
- 把 CLAUDE.md 拆分或移走——它仍是权威内文档，H2 只从中**提炼对外可见部分**，不复制。
- 视频/截图脚本化（TUI 截图手工配，不纳入 CI）。

---

## 2. 背景

- **现有文档盘点**（`docs/`）：全是架构/分析文档——`synthesis-final.md`、`synthesis-report*.md`、`analysis-report.md`、`feature-comparison-with-codex.md`、`vcs.md`、`compaction.md`、`skills-authoring.md`、`feature-roadmap-*.md`，以及 `docs/superpowers/{specs,plans,notes}/` 的设计与计划。**没有任何面向终端用户的"怎么用"文档**。
- **README.md**（仓库根）：已有 Quick start（build → cp config → `./yanshi` / `--fake-model` / `serve`）与简要 TUI 按键说明。UDOC1 是 README 的**深化展开**，不替代它；README 保持精简入口，细节进 `docs/user-guide/`。
- **CLAUDE.md**：权威内文档（六边形布局、context 注入、guard、约定）。是 UDOC1/CONTRIB1 的**提炼源**，不是复制源。
- **D1/D2 已落地的对外面**（H2 的依赖前置）：
  - v1 资源层 `internal/api/v1/`（`types.go` Thread/Turn/Item + params/responses + `Version`；`schema.go` 的 `schemaDocument` 字面量 + `SchemaBytes()`；`events.go` 的 `ItemFromServerFrame` 单一映射）。
  - JSON Schema 产物 `sdk/schema/v1/agent-api.schema.json` + `sdk/schema/v1.1/` + `sdk/schema/v1/fixtures/`，以及 `sdk/schema/CONTRACT_HANDOFF.md`。
  - HTTP/SSE 传输 `internal/api/http/agent_v1.go`；JSON-RPC app-server `internal/appserver/`（`rpc.go` RPCRequest/RPCResponse/标准错误码、`server.go`、`config.go`）——两者共用同一 `*v1.Service`，语义不漂移。
  - TS SDK `sdk/ts/`（`v1.ts` 由 `cmd/api-schema` 生成；`src/generated.ts`；vitest 契约测试）。
  - Python SDK `sdk/python/`（`yanshi_sdk` 包：`client.py`/`transport.py`/`contract.py`/`generated.py`；pytest + `test_version_matrix.py`）。
  - IDE 扩展 `ide/vscode/`。
- **生成器已存在**：`cmd/api-schema/main.go` 读 `v1.SchemaBytes()`、手写 TS interface、`-out` 写 `sdk/ts/v1.ts`，并对 schema 做 smoke-check 防漂移（D1-DEC-2 option A：Go 内建生成器，无 Node/npm 依赖）。H2 的"生成驱动文档"沿用这条路线。
- **现状确认（落点核对）**：
  - `examples/` —— **不存在**（新建）。
  - `CONTRIBUTING.md`（仓库根）—— **不存在**（新建；`reference/` 下的都是第三方仓库，不算）。
  - `docs/user-guide/` / `docs/api/` / `docs/adr/` / `docs/archive/` —— **均不存在**（新建）。

---

## 3. 总体策略：生成驱动，防漂移

文档最大的风险是**与代码漂移**。H2 一以贯之的策略是：**手写 prose + 生成驱动的事实片段**，并在 CI 上对生成片段做 `git diff --exit-code` 守门。

三类事实片段及其生成方式：

| 片段 | 真相源 | 生成/守门方式 |
|---|---|---|
| CLI 命令表/帮助 | `cmd/yanshi/*.go` 的 FlagSet | 运行 `yanshi -h`/`yanshi <sub> -h` 快照进 docs；CI 重新生成并 diff |
| 配置参考 | `config.example.yaml`（已是注释最全的真相源）+ `internal/config/config.go` Config struct 的 yaml tag | Config struct → 字段骨架表（生成）；prose 手写；CI 断言 struct 每个字段在 example 中出现或被显式标记可选 |
| API 资源/Schema | `internal/api/v1/types.go` + `schema.go` | 扩展 `cmd/api-schema` 增加 `-markdown` 模式，把 `schemaDocument` 渲染为资源字段表；嵌入 docs 的 `<!-- BEGIN GENERATED -->` 区块；CI 重生成并 diff |

生成器统一走 `cmd/api-schema`（已存在、已 smoke-check schema），新增一个 `-markdown` 子能力，**不引入新二进制**（沿用 D1-DEC-2 的"Go 内建、无外部依赖"原则）。配置骨架表与 CLI 帮助快照也由一个小生成器/脚本产出，挂在同一个 CI job。

---

## 4. [UDOC1] 用户指南

### 缺口
无面向终端用户的"怎么用 yanshi"指南；README 只覆盖最精简的 Quick start。

### 落点
`docs/user-guide/`（新建目录），首屏 `docs/user-guide/README.md` 作为导航索引。

### 设计

**目录结构**：

```
docs/user-guide/
  README.md            # 导航索引 + getting started（零依赖起步）
  getting-started.md   # build → cp config → --fake-model → 跑通第一个 turn
  configuration.md     # config.yaml 全字段（config.example 详解 + 生成骨架表）
  tui.md               # TUI 命令（/ 前缀）/ 键位 / 交互式权限 / 多窗口自愈
  skills.md            # 技能系统：放哪、怎么写、渐进披露（引用 docs/skills-authoring.md，不复制）
  autovcs.md           # autoVCS：编辑自动追踪、worktree、对外视角（提炼 docs/vcs.md）
  goalloop.md          # yanshi goal：plan→implement→evaluate→judge，--fake-model 两轮演示
  guard.md             # 四维权限模型（tools/fs/shell/net）、profile、交互式 mode、fail-closed
  entrypoints.md       # headless(exec/chat --no-tui) / serve / app(JSON-RPC) / SDK / IDE 各入口与适用场景
```

**getting-started（零依赖）**：完全沿用 README 的 `--fake-model` 路径，但展开成"看到什么输出、按什么键、第一个工具调用长什么样"的可跟做步骤；不要求任何 API key。

**配置页（configuration.md）**：
- prose 围绕 `config.example.yaml` 的每个顶层块（`server`/`storage`/`llm`/`agents`/`skills`/`vcs`/`compaction`/`profiles`/`security`/`batch`/`lsp`/`mcp`/`memory`/`observability`/`features`/`pricing`/`secrets`/`auth`/`i18n`/`tui`）展开，说明语义、默认值、与其他块的关系（例如 `compaction.context_window` 与 provider `context_window` 的回退关系）。
- 嵌入一张**生成的字段骨架表**（key/type），标 `<!-- BEGIN GENERATED: config -->`。prose 是手写的（解释"为什么"），骨架表是生成的（保证"有什么"不漂移）。

**命令表（tui.md / entrypoints.md）**：`/` 命令与子命令帮助用**快照**方式嵌入（`<!-- BEGIN GENERATED: help:yanshi -->` 等），CI 重新跑 `yanshi -h`、`yanshi serve -h`、`yanshi goal -h`、`yanshi exec -h`、`yanshi chat -h`、`yanshi app -h`、`yanshi vcs-mcp -h`、`yanshi doctor -h` 并 diff。子命令清单来自 `cmd/yanshi/main.go` 的 dispatch（bare/serve/chat/exec/app/goal/vcs-mcp/pr/auth/doctor）。

**对外可见性的提炼边界**：从 CLAUDE.md 提炼"用户需要知道的"（配置怎么写、权限怎么配、技能放哪、VCS 自动追踪了什么、压缩何时触发），**不**提炼"贡献者需要知道的"（六边形装配顺序、context 注入实现、runners 缓存键）——后者进 CONTRIBUTING1。两者通过"受众"切分，避免重复。

### 依赖
- 协议/SDK 面：D1/D2（已落地）。headless/SDK/IDE 入口的准确性依赖这些面冻结。
- 生成器：扩展 `cmd/api-schema`（-markdown）+ 一个 config-struct 骨架生成器。

### 风险与缓解

| 风险 | 缓解 |
|---|---|
| 文档与 CLI/配置漂移 | 生成片段 + CI `git diff --exit-code` 守门（见 §3） |
| 与 CLAUDE.md/README 内容重复 → 三处维护 | UDOC1 只放"对外可见"；CLAUDE.md 权威内文档；README 精简入口；交叉引用而非复制 |
| 子命令随 A–G 演进增减 | 快照生成自动跟上；help 文本即契约 |
| 配置块跨 batch 多（C1/C4/D3/LSP 都加了块） | 以 `config.example.yaml` 为单一真相源展开，它是各 batch 更新的汇聚点 |

### 验收
- getting-started 可在无 API key、无后端的情况下用 `--fake-model` 跑通并看到确定性输出。
- 配置页覆盖 `config.example.yaml` 全部顶层块；生成的骨架表与 struct 一致（CI 守门）。
- 命令/帮助快照与实际 `yanshi -h` 一致（CI 守门）。
- guard/autoVCS/goalloop/skills/entrypoints 各有专页，无"待补"占位。

### 预估
2–3d。

---

## 5. [APIREF1] v1 API/协议参考

### 缺口
D1 落地 v1 资源模型 + JSON Schema，D2 落地 TS/Python SDK，但**无统一对外 API 参考**；真相分散在 `types.go`/`schema.go`/`sdk/schema/`/`sdk/{ts,python}/`/`internal/appserver/`。

### 落点
`docs/api/`（新建目录），`docs/api/README.md` 为索引。

### 设计

**目录结构**：

```
docs/api/
  README.md           # 索引 + 版本契约总述（version: "v1"、unknown 字段策略、item 类型枚举）
  resources.md        # Thread/Turn/Item 资源 + params/responses（types.go 生成字段表）
  events.md           # SSE 事件（turn/item）、ItemFromServerFrame 映射、未知类型保留策略
  schema.md           # 完整 JSON Schema（嵌入 sdk/schema/v1/agent-api.schema.json，生成）
  jsonrpc.md          # app-server JSON-RPC 2.0：方法表、错误码、item/updated 通知
  sdk-ts.md           # TS SDK 用法（thread/start → turn/start → 消费 item 流）
  sdk-python.md       # Python SDK 用法（同上）
```

**生成方式（具体）**：
- 扩展 `cmd/api-schema` 增加 `-markdown`：遍历 `schemaDocument`（`internal/api/v1/schema.go`，已是 Go 字面量、可 diff）→ 渲染每个 `$defs` 为一张"字段 / 类型 / required / const"表 → 包进 `<!-- BEGIN GENERATED: api-resources -->`。
- JSON Schema 全文：`docs/api/schema.md` 嵌入 `sdk/schema/v1/agent-api.schema.json` 的 pretty-printed 内容（生成区块），避免手抄。
- **CI 守门**：`go run ./cmd/api-schema -markdown` 重生成 → `git diff --exit-code docs/api/`；与现有 `cmd/api-schema` 对 `v1.SchemaBytes()` 的 smoke-check 一致地防漂移。

**资源页（resources.md）**：对每个资源说明——
- `Thread`：会话资源；id == session id（SQLite 行）；status（active/archived）。
- `Turn`：一次用户输入的生命周期；同 thread 至多一个 in-progress turn；`completedAt` 在终态前省略。
- `Item`：最小流式事件；monotonic `sequence` 从 1 起；未知 `type` 客户端忽略、服务端保留为 `event.<legacyType>`（契约）。
- params/responses：`ThreadStartParams`/`ThreadResumeParams`/`ThreadInterruptParams`/`TurnStartParams` 及对应 response；`TurnStartParams.OutputSchema` 镜像 legacy structured-turn 能力。

**事件页（events.md）**：列 `item.type` 枚举（`turn.started`/`message.delta`/`reasoning.delta`/`tool.call`/`tool.result`/`tool.progress`/`structured.result`/`turn.error`/`turn.completed`，来自 `types.go` 常量），并说明 legacy frame → item 的映射（`events.go` `ItemFromServerFrame`）与"未知帧保留不丢"策略。

**JSON-RPC 页（jsonrpc.md）**：基于 `internal/appserver/rpc.go`——方法表（`thread/start`/`thread/resume`/`turn/start`/`thread/interrupt`/`capabilities` 等）、JSON-RPC 2.0 标准错误码（-32700/-32600/-32601/-32602/-32603）、`item/updated` 通知、`ID` 为 RawMessage（string/number/null 原样回显）、notification（缺 ID）不回响应。强调"与 HTTP/SSE 共用同一 `*v1.Service`，语义不漂移"。

**SDK 用法页**：每个 SDK 给一个最小端到端：建 thread → 起 turn → 消费 item 流 → interrupt。TS 引 `sdk/ts/v1.ts` 类型；Python 引 `yanshi_sdk` 包。两页都给"如何对 fake-model 后端跑通"的说明（呼应 EX1 的 SDK 示例）。

### 依赖
- D1（v1 资源层 + app-server）、D2（TS/Python SDK）——均已落地。
- 生成器：`cmd/api-schema -markdown`（新）。

### 风险与缓解

| 风险 | 缓解 |
|---|---|
| 协议演进 → 文档漂移 | 生成驱动（schema 字面量 → markdown）；CI diff 守门 |
| `cmd/api-schema` 现 TS 是手写 interface + smoke-check，加 -markdown 后两份产物同源 | 都读同一 `schemaDocument`/`SchemaBytes()`；TS 手写保可读、markdown 表保完整；任一变更 CI 同时校验 |
| app-server 方法集与 HTTP 不完全对称 | jsonrpc.md 显式标注"共用 Service"；不对称处（如 SSE 静态 profile vs WS 交互式权限）在页内说明 |
| SDK 用法示例与 EX1 重复 | APIREF1 给最小调用序列（聚焦契约）；EX1 给场景化完整脚本（聚焦集成）；交叉引用 |

### 验收
- resources/events/schema/jsonrpc 四页的字段/事件/错误码与 `types.go`/`schema.go`/`rpc.go` 一致（CI 生成守门）。
- TS/Python SDK 各有可跑的最小端到端说明。
- 索引页给出版本契约总述（version 字段、unknown 字段策略、item 类型枚举）。

### 预估
2d。

---

## 6. [ADR1] 架构决策记录

### 缺口
关键决策散落 CLAUDE.md / synthesis §9，**无独立、可检索、带状态演进**的 ADR。

### 落点
`docs/adr/`（新建目录），`docs/adr/README.md` 为索引（ADR 编号表 + 状态）。

### 设计

**模板**（每条 ADR 一个文件 `docs/adr/NNNN-kebab-title.md`）：

```
# ADR-NNNN: 标题
- 状态：proposed | accepted | deprecated | superseded by ADR-MMMM
- 日期：YYYY-MM-DD
## 背景（Context）
## 决策（Decision）
## 后果（Consequences）—— 含不可违反的约束
## 关联（CLAUDE.md / synthesis §9.x / 相关代码落点）
```

**首批 ADR（从 synthesis §9 提炼，每条对应一节）**：

| ADR | 来源 | 决策要点（不可违反的约束） |
|---|---|---|
| ADR-0001 | §9.1 | UnknownToolsHandler 返回结果而非 error（永不改 NodeRunError，否则中断 turn） |
| ADR-0002 | §9.1 | runners sync.Map 以 model 指针为缓存键（按 turn 切 provider） |
| ADR-0003 | §9.2 | Guard fail-closed：空 Allow 拒绝一切（永不默认允许） |
| ADR-0004 | §9.2 | Guard 无状态 + shell 元字符硬拦截（兜底） |
| ADR-0005 | §9.3 | 压缩 summary 用 User 角色而非 System（避免双 system 冲突） |
| ADR-0006 | §9.3 | 双路径共享统一核心 ctxcompact.Run + 携带式分块严格不超窗口 |
| ADR-0007 | §9.4 | WS 服务端持有历史 vs SSE 客户端回放；共享 proto 帧词表（新帧同步 ws.go+ssebackend.go） |
| ADR-0008 | §9.5 | autoVCS 经 context 注入自动追踪（VCS 非 nil 覆盖调用方 scope） |
| ADR-0009 | §9.5 | SQLite 类 git（树级三方合并，非真实 git） |
| ADR-0010 | §9.4 | SSE 路径永久静态 profile（无持久连接，不支持交互式权限） |

**关系原则**：ADR **引用 CLAUDE.md 不复制**。CLAUDE.md 是"贡献者必读的全景"，ADR 是"单个决策的演进档案"。当一个决策被后续 batch 改变（如某 batch 放宽了某约束），写新 ADR 并把旧 ADR 标 `superseded`——这是 ADR 相对 CLAUDE.md 的独有价值（CLAUDE.md 只反映当前态，ADR 保留历史）。

**新决策走 ADR**：CONTRIB1 里写明"引入新架构决策时先写/更新 ADR"，让 ADR 成为活档。

### 依赖
- 无（纯文档）。引用 synthesis §9 与 CLAUDE.md。

### 风险与缓解

| 风险 | 缓解 |
|---|---|
| 与 CLAUDE.md 重复 → 双处维护漂移 | ADR 引用不复制；CLAUDE.md 是全景当前态，ADR 是单决策演进档案；ADR 多出的"历史/superseded"是独有信息 |
| 写完即冻结，不再更新 | CONTRIBUTING1 写明"新决策走 ADR"；索引页状态列可见 stale |
| 编号冲突 | 顺序四位编号；superseded 不复用编号 |

### 验收
- ≥10 条 ADR 覆盖 synthesis §9 全部 5 个子节。
- 模板存在；索引页含状态列。
- 每条 ADR 的"关联"指向真实代码落点（如 `internal/agent/orchestrator/`、`internal/guard/`、`internal/ctxcompact/`）。

### 预估
1–2d。

---

## 7. [EX1] examples 目录

### 缺口
**无可运行示例**；SDK/工具/技能/goalloop 的集成方式只能读代码猜。

### 落点
`examples/`（新建目录），`examples/README.md` 为索引（每个示例一行说明 + 怎么跑）。

### 设计

**示例清单（≥5，每个 fake-model 友好可跑）**：

```
examples/
  README.md
  headless-exec/        # yanshi exec --fake-model：单 turn 文本进/出
  headless-batch/       # yanshi chat --no-tui --fake-model --input jsonl：批处理 jsonl
  sdk-typescript/       # TS SDK：thread/start → turn/start → 消费 item 流（对 --fake-model 后端）
  sdk-python/           # Python SDK：同上
  custom-tool/          # Go：实现一个 GuardedTool，挂进 bootstrap（编译型示例）
  custom-skill/         # 一个自定义 SKILL.md 目录结构 + 调用
  goalloop-config/      # yanshi goal --fake-model 的两轮演示 + goalloop 配置说明
```

**可跑性约束**：
- 脚本类（headless/SDK/goalloop）必须在 `--fake-model` 下零 API key 跑通；`examples/README.md` 给每个的"复制即跑"命令。
- 编译型（custom-tool）是 `go build` 能过的最小 main，展示 `tools.GuardedTool` 接口与 guard profile 配法，不强求连真实模型（可连 fake）。
- SDK 示例指向本地构建的 `yanshi serve --fake-model` 后端（`127.0.0.1:0` 或固定端口 + token）。

**CI 验证（可编译/可跑检查）**：
- 新增 CI job（挂到 CIG1 的矩阵，或 H2 自带一个轻量 job）：
  - `go build` 每个 Go 示例（`go build ./examples/custom-tool`）。
  - TS 示例 `tsc --noEmit`（复用 `sdk/ts/` 的 toolchain）。
  - Python 示例 `python -c "import ..."` 或 `ruff`/语法检查（不强求装运行时，但要能解析）。
  - 脚本类：跑一次 `yanshi exec --fake-model -p "hi"` 并断言非空输出（最快的可跑冒烟）。
- **不**把 examples 纳入 `go test ./...`（它们不是包内测试），单独 build 检查。

### 依赖
- D1/D2（SDK、headless、app-server）——已落地。
- `--fake-model` 路径——已存在（CLAUDE.md 确认 fake model 在 `llm.providers` 为空时自动选用）。

### 风险与缓解

| 风险 | 缓解 |
|---|---|
| 示例与 SDK/CLI 演进漂移 → 跑不通 | CI 可编译/可跑冒烟（见上）；examples 失败即阻断 |
| SDK 示例需起后端，CI 环境复杂 | 用 `--fake-model` + `--inprocess` 或固定 loopback；复用 `cmd/yanshi/headless_test.go` 已验证的起后端模式 |
| custom-tool 示例引入对 internal 包的耦合 | 示例只依赖公开面（`tools.GuardedTool` 接口、config profile）；若所需符号未导出，标注为"示例驱动的外部 API gap"反馈给后续 batch，不在 examples 里 hack |
| 示例太多变维护负担 | v1 只做上面 7 个最小集；覆盖主要集成点即停 |

### 验收
- ≥5 个示例（实际 7 个），每个 `examples/README.md` 有"怎么跑"。
- CI：Go 示例可 build、TS 可 typecheck、Python 可解析、脚本类可冒烟跑通。
- 覆盖集成点：headless ×2、SDK ×2（TS/Python）、custom-tool、custom-skill、goalloop。

### 预估
2d。

---

## 8. [CONTRIB1] 贡献指南 + docs 归档

### 缺口
无 `CONTRIBUTING.md`（仓库根确认不存在）；多份 synthesis/analysis 报告散在 `docs/` 根，与面向用户文档混在一起。

### 落点
- `CONTRIBUTING.md`（仓库根，新建）。
- `docs/archive/`（新建），存放历史分析报告。

### 设计

**CONTRIBUTING.md 内容（提炼自 CLAUDE.md，面向贡献者）**：
- **怎么开始**：build/test 命令（`go build`/`go test ./...`/`go run ./cmd/testchanged`）、`--fake-model` 零依赖开发、配置从 `config.example.yaml` 复制。
- **架构约定（承重）**：
  - 六边形/端口适配：依赖向内流；**唯一组合根 `internal/bootstrap/Build`** 知晓所有 internal 包；新增组件在此装配，装配顺序固定（config→store→vcs→model→tools→orchestrator→http→task broker）。
  - **context 注入是横切模式**：鉴权/scope/acting agent 走 context value（`tools.WithProfile`/`WithSubAgentRunner`/`WithVCS`），不塞工具参数。
  - **guard fail-closed**：空 Allow 拒绝一切；无状态；shell 元字符硬拦截；新增工具必须显式配权限。
  - **Fake 优先于 mock**：优先新增 fake（`einollm.FakeModel` 等）而非引入 mock 框架。
  - **单文件 ≤1000 纯代码行**（不含注释/空行）；超了先拆再写。
  - **重复逻辑必须抽公共函数**。
  - **注释是承重文档**：包/导出符号带多段 doc 注释解释"为什么"，尤其在 ADK/guard/VCS 周围保持密度。
  - **单 binary 客户端+服务端**：TUI 是本地轻客户端，发现或内嵌后端。
- **两种传输一套协议**：WS/SSE 共用 `internal/proto/frame.go` 词表；新帧同步 `ws.go`+`ssebackend.go`。
- **本地 fork 说明**：`go.mod` replace 钉 `bubbletea` 到 `./third_party/bubbletea`（Ctrl+Enter 区分），改 bubbletea 行为改 fork、不去 replace。
- **新架构决策走 ADR**（引用 `docs/adr/`，ADR-0001 起）。
- **提交/PR 约定**：conventional commit prefix（与 VER1 的 CHANGELOG 自动生成对齐）；引用 CLAUDE.md 为权威详细版。
- **被忽略的产物**：`config.yaml`/`*.db`/构建二进制不提交。

**边界**：CONTRIBUTING.md 提炼"贡献者必知"的**子集 + 指针**，每个约定后写"详见 CLAUDE.md 对应段"。CLAUDE.md 保持权威、完整；CONTRIBUTING.md 是 onboarding 友好的入口。两者**不**逐字重复——CONTRIBUTING.md 比 CLAUDE.md 更短、更"第一步导向"。

**docs 归档（`docs/archive/`）**：
- 移入：`docs/synthesis-final.md`、`docs/synthesis-report.md`、`docs/synthesis-report-v2.md`、`docs/analysis-report.md`、`docs/feature-comparison-with-codex.md`、`deps_analysis.md`（仓库根的）、`deps_raw.txt`、`docs/feature-roadmap-codex-deepseek.md`。
- **保留在原位**：`CLAUDE.md`（权威）、`README.md`（入口）、`docs/vcs.md`/`docs/compaction.md`/`docs/skills-authoring.md`（活的技术参考，UDOC1 引用它们）、`docs/feature-roadmap-e-h.md`（当前 roadmap，活档）、`docs/superpowers/`（specs/plans/notes，活的工作产物）。
- 归档目录加 `docs/archive/README.md` 说明"这些是历史分析快照，权威当前态见 CLAUDE.md，决策演进见 docs/adr/"。
- **移文件用 `git mv`** 保留历史；ADR-00xx 的"关联"里引用归档路径时用新路径。

### 依赖
- ADR1（CONTRIB1 引用 `docs/adr/` 的"新决策走 ADR"流程）。
- VER1（提交规范与 CHANGELOG 对齐）——软依赖，CONTRIB1 可先写"遵循 conventional commit"，VER1 落地后补 CHANGELOG 细节。

### 风险与缓解

| 风险 | 缓解 |
|---|---|
| CONTRIBUTING 与 CLAUDE.md 重复 → 双处维护漂移 | CONTRIBUTING 是子集+指针，每条指向 CLAUDE.md 段落；CLAUDE.md 改时 CONTRIBUTING 的"指针"不变、只子集表述可能需跟进（低频） |
| 归档移动破坏现有链接（其他文档引用了 synthesis-final.md 等） | 移动前 grep 引用；`git mv` 保留历史；在归档 README 列出"原路径→新路径"映射；ADR 引用用新路径 |
| 归档后 synthesis §9 的决策"失踪" | §9 已被 ADR1 结构化提炼（ADR-0001..0010）；归档的是"分析快照"，决策活在 ADR |

### 验收
- `CONTRIBUTING.md` 存在；覆盖 build/test、六边形、context 注入、guard fail-closed、Fake 优先、1000 行、注释密度、单 binary、传输协议、fork、ADR 流程、提交规范。
- `docs/archive/` 存在且历史报告已移入；`docs/` 根只剩活档/入口。
- 无断链（grep 验证归档移动后的引用）。

### 预估
1d（CONTRIB1 半天 + 归档半天）。

---

## 9. 文件结构（新/改）

| 文件/目录 | 职责 | 新/改 | 条目 |
|---|---|---|---|
| `docs/user-guide/` (+ 9 md) | 用户指南 | 新 | UDOC1 |
| `docs/api/` (+ 7 md) | v1 API 参考 | 新 | APIREF1 |
| `docs/adr/` (+ README + ADR-0001..0010) | 架构决策记录 | 新 | ADR1 |
| `examples/` (+ README + 7 子目录) | 可运行示例 | 新 | EX1 |
| `CONTRIBUTING.md` | 贡献指南 | 新 | CONTRIB1 |
| `docs/archive/` (+ README) | 历史分析报告归档 | 新 | CONTRIB1 |
| `cmd/api-schema/main.go` | 增加 `-markdown` 模式：schemaDocument → 资源字段表 | 改 | APIREF1/UDOC1 |
| `internal/config/config.go` 或新 `cmd/gendocs` | Config struct → 配置骨架表生成器 | 新（生成器） | UDOC1 |
| `.github/workflows/`（CIG1 的或 H2 自带 job） | 生成片段 `git diff --exit-code` 守门 + examples 可编译/可跑冒烟 | 改/新 | 全部 |
| `docs/feature-roadmap-e-h.md` | Tier H 切分对齐 hybrid 命名（H1=发布工程/H2=文档） | 改 | OQ-1 |
| `docs/` 根 → `docs/archive/` | `synthesis-final.md` 等历史报告 `git mv` | 移 | CONTRIB1 |

> 生成器原则：不新增独立二进制产物到 release；`cmd/api-schema -markdown` 与 config 骨架生成器是开发期工具（`go run ./...`），沿用 D1-DEC-2 的 Go 内建、无外部依赖路线。

---

## 10. 测试策略

H2 主要是文档，"测试"= **防漂移守门 + 示例可跑**，不是单测。

- **生成片段守门**（UDOC1/APIREF1）：
  - CI 跑 `go run ./cmd/api-schema -markdown` → `git diff --exit-code docs/api/ docs/user-guide/configuration.md`；有 diff 即失败。
  - CI 跑 config 骨架生成器 → diff 配置骨架表区块。
  - CI 跑 `yanshi -h`/各子命令 `-h` → diff 帮助快照区块。
- **配置一致性**（UDOC1 附带）：一个轻量断言——`internal/config` Config struct 的每个导出字段要么出现在 `config.example.yaml`、要么在一个显式可选清单里（防止"代码有配置项、example 与文档都没有"）。
- **ADR 可达性**（ADR1）：CI/grep 断言每条 ADR 的"关联"指向的代码路径真实存在（防止指向已删除文件）。
- **examples 可编译/可跑**（EX1）：见 §7——Go build、TS typecheck、Python 解析、headless 冒烟。
- **归档无断链**（CONTRIB1）：grep 断言仓库内 `.md` 无指向归档前旧路径的死链。

> 这些守门与 CIG1（CI 门禁矩阵）协同：CIG1 提供 CI 骨架，H2 把"文档 diff 守门 + examples 检查"作为额外 job 挂入。若 CIG1 尚未落地，H2 自带一个最小 workflow（不阻塞）。

---

## 11. 风险与缓解（汇总）

| 风险 | 影响 | 缓解 |
|---|---|---|
| 文档漂移（最大风险） | 文档与 CLI/配置/协议脱节，误导用户 | 生成驱动 + CI `git diff --exit-code` 守门（§3/§10） |
| 三处重复（README/UDOC1/CLAUDE.md） | 维护负担、不一致 | 按受众切分：README=入口、UDOC1=对外可见、CLAUDE.md=权威内档、CONTRIBUTING1=贡献者子集+指针；交叉引用不复制 |
| ADR 与 CLAUDE.md 重复 | 双处维护 | ADR 引用不复制；ADR 独有价值=历史/superseded |
| 示例漂移到跑不通 | 用户照抄失败 | CI 可编译/可跑冒烟阻断 |
| 归档移动断链 | 文档死链 | `git mv` + grep 验证 + 归档 README 路径映射 |
| `cmd/api-schema -markdown` 与现有 TS 产物分叉 | 两份"真相" | 同源 `schemaDocument`/`SchemaBytes()`；CI 同时校验 |
| H2 依赖 CIG1 的 CI 骨架未就绪 | 守门无处挂 | H2 自带最小 workflow兜底（不阻塞） |
| custom-tool 示例暴露 internal API gap | 示例 hack 或不可编译 | 标注为"外部 API gap"反馈后续 batch；不在 examples 里 hack |

---

## 12. 验收标准（整批）

1. `docs/user-guide/` 存在，getting-started 可零依赖（`--fake-model`）跑通；配置/命令为生成守门片段。
2. `docs/api/` 存在，资源/事件/schema/jsonrpc/SDK(TS/Python) 各页与 `types.go`/`schema.go`/`rpc.go` 一致（CI 生成守门）。
3. `docs/adr/` 存在，≥10 条 ADR 覆盖 synthesis §9 全部子节；模板与索引在。
4. `examples/` 存在，≥5 个示例（实际 7）fake-model 友好可跑；CI 可编译/可跑冒烟通过。
5. `CONTRIBUTING.md` 存在，覆盖架构约定与提交流程，指向 CLAUDE.md/ADR。
6. `docs/archive/` 存在，历史报告已 `git mv` 入内；`docs/` 根无断链。
7. CI 上所有生成片段 `git diff --exit-code` 通过（无漂移）。

---

## 13. Out-of-scope / 后续

- 文档静态站点（mkdocs/Docusaurus/多语言文档站）—— v1 用仓库内 Markdown + 索引；是否建站是后续 OQ。
- TUI 截图/视频脚本化 —— 手工配图，不纳入 CI。
- 用户文档全量多语言翻译 —— i18n 是运行时能力（D3-I18N1），文档先中英双语标题。
- 把 `docs/superpowers/`（specs/plans/notes）纳入归档 —— 它是活的工作产物，保留原位。
- 用户文档的搜索/反馈机制 —— 后续。

---

## 14. Open questions（需人决策）

- **OQ-1（命名对齐）**：roadmap `docs/feature-roadmap-e-h.md` 里 Tier H 仍是"Batch H1=文档体系 / Batch H2=examples"的旧切分；team hybrid 重组为"H1=发布工程 / H2=文档+examples+贡献"。**需决策**：是否在本批顺带把 roadmap 的 Tier H 章节改写对齐 hybrid 命名（避免 team 内 H1/H2 与 roadmap H1/H2 指代不同东西）？建议改，并在 roadmap 自检（task #2）一并处理。
- **OQ-2（文档站）**：v1 是否就停在"仓库 Markdown + 索引"，还是预留上 mkdocs/Docusaurus 的结构（如 `docs/user-guide/` 用站点友好的 mkdocs.yml nav）？影响目录命名约定。建议 v1 停在 Markdown，目录命名保持中性以便后续迁移。
- **OQ-3（config 骨架生成器落点）**：配置骨架表生成器放 `cmd/api-schema`（扩成通用 docs 生成器，改名风险）还是新 `cmd/gendocs`？建议新 `cmd/gendocs`，保持 `cmd/api-schema` 职责单一（API schema → TS/markdown）。
- **OQ-4（examples 的 custom-tool 依赖深度）**：custom-tool 示例若需要的符号（如 GuardedTool 的构造辅助）未导出，是"标注 API gap 反馈后续"还是"为示例临时导出一小撮 API"？建议前者，保持 examples 不驱动 internal API 变更。
