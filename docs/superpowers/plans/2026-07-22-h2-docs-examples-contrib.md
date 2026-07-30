# Batch H2 — 文档 / examples / 贡献 Implementation Plan

**Spec:** docs/superpowers/specs/2026-07-22-h2-docs-examples-contrib-design.md（权威）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 A–G 行为面冻结、D1/D2 协议与 SDK 落地后，补齐 yanshi "内部分析文档已 9/10、对外'怎么用'缺失" 的短板：交付面向终端用户的用户指南（UDOC1）、v1 Agent API 统一参考（APIREF1）、架构决策记录（ADR1）、可运行示例目录（EX1）、贡献指南 + docs 归档（CONTRIB1）。H2 **不新增功能面、不改运行时行为**；唯一引入代码是两个开发期生成器（`cmd/api-schema -markdown`、新 `cmd/gendocs`）与一个 CI 守门 workflow。

**Architecture:** 文档最大的风险是**与代码漂移**。H2 一以贯之的策略是 **手写 prose + 生成驱动的事实片段**，并在 CI 上对生成片段做 `git diff --exit-code` 守门。三类事实片段各有真相源与生成器：(1) API 资源/事件/Schema 表 ← `internal/api/v1/schemaDocument` 经 `cmd/api-schema -markdown` 渲染；(2) 配置骨架表 ← `internal/config.Config` struct 的 yaml tag 经新 `cmd/gendocs` 反射；(3) CLI 帮助快照 ← `yanshi -h` / 各子命令 `-h` 经 `cmd/gendocs` 捕获。生成器沿用 D1-DEC-2 的"Go 内建、无外部依赖"原则（不引入 Node/Python 文档工具链）。**防重复**靠受众切分：README=精简入口、UDOC1=对外可见、CLAUDE.md=权威内档、CONTRIBUTING=贡献者子集+指针、ADR=单决策演进档案；五者交叉引用而非复制。

**Tech Stack:** Go 1.26.4（生成器 + custom-tool 示例）· Markdown + `<!-- BEGIN GENERATED: ... -->` 区块约定 · GitHub Actions（守门 workflow）· TypeScript / Python（仅 SDK 示例复用 `sdk/{ts,python}` 已有 toolchain，不新增依赖）。

---

## 施工前约束与现状基线

- 本计划只覆盖新建 `docs/{user-guide,api,adr,archive}/`、`examples/`、`CONTRIBUTING.md`、两个开发期生成器、一个 CI workflow；**不改运行时 `internal/` 行为**（生成器只**读** `schemaDocument`/`Config` struct/FlagSet，不碰逻辑）。
- 本计划当前只写到本文件，实施阶段才执行下方命令；本次不运行 `go build`、`go test` 或创建任何 docs 内容文件。
- **现状确认（spec §2 已核对，均为"不存在→新建"）**：`examples/`、`CONTRIBUTING.md`、`docs/user-guide/`、`docs/api/`、`docs/adr/`、`docs/archive/` 均不存在。
- **已存在的真相源（生成器输入，不复制）**：
  - `internal/api/v1/schema.go` 的 `schemaDocument` 字面量 + `SchemaBytes()`；`types.go` 的 Thread/Turn/Item + 常量；`events.go` 的 `ItemFromServerFrame`；`internal/appserver/rpc.go` 的 RPCRequest/RPCResponse/标准错误码。
  - `internal/config/config.go` 的 `Config` struct（yaml tag）+ `config.example.yaml`（注释最全的配置真相源）。
  - `cmd/yanshi/main.go` 的 dispatch（bare/serve/chat/exec/app/goal/vcs-mcp/pr/auth/doctor）。
  - `cmd/api-schema/main.go` 已存在：读 `v1.SchemaBytes()`、手写 TS interface、`-out` 写 `sdk/ts/v1.ts`、并对 schema 做 smoke-check。
  - `CLAUDE.md`（权威内档，UDOC/CONTRIB 的**提炼源**）、`docs/synthesis-final.md` §9（ADR 源）、`docs/vcs.md`/`docs/compaction.md`/`docs/skills-authoring.md`（活技术参考，UDOC 交叉引用）。
- **已锁定决策（直接施工，不再讨论）**：
  1. 文档形态：**仓库 Markdown + 索引**，目录命名中性（**不**预留 `mkdocs.yml`/`docusaurus.config`，便于后续迁移但 v1 不建站）。
  2. 配置骨架 + CLI 帮助生成器：**新 `cmd/gendocs`**（保 `cmd/api-schema` 职责单一 = "API schema → {TS, markdown}"）。API 资源/Schema 表仍由 `cmd/api-schema` 新增 `-markdown` 模式产出（同属 "schema → markdown" 单一职责）。
  3. custom-tool 示例遇 `internal/` 未导出符号：**标 API gap 反馈后续 batch，不在 examples 里 hack、不临时导出**。
  4. 受众切分防重复（见 Architecture）；生成片段统一 `<!-- BEGIN GENERATED: <id> --> … <!-- END GENERATED: <id> -->` 区块，生成器原地重写区块、保留外围 prose。
- **衔接依赖（外部 batch，非本计划施工）**：
  - **G（agent SDK image/多模态字段）**：APIREF1 的**生成式资源表**（`resources.md`）依赖 G 把 `image` 字段加入 `schemaDocument`。G 未落地时该字段缺失；G 落地后 CI 守门自动捕获 diff 并触发重生成。见 Task 8 的 COORD 标注。
  - **CIG1（CI 门禁矩阵）**：本计划 Task 15 的 docs 守门 job 优先挂入 CIG1 矩阵；CIG1 未就绪时 H2 自带最小 `.github/workflows/docs.yml` 兜底（不阻塞）。
  - D1/D2（v1 资源层 + app-server + TS/Python SDK）——已落地，是 UDOC/APIREF/EX 的事实基础。

---

## 覆盖范围与任务数

本计划共 **15 个 task**，建议每个 task 一个小 commit；实现顺序为：生成器（1–2）→ ADR（3）→ UDOC1（4–7）→ APIREF1（8–9）→ EX1（10–12）→ CONTRIB1（13–14）→ CI 守门（15）。每个 task 独立可测、单独提交；文档 task 的"测试" = 生成-片段一致性检查（`git diff --exit-code`）+ 交叉引用可达性 + EX1 可编译/可冒烟。

| 领域 | Task | 交付物 | 验收重点 |
|---|---:|---|---|
| 生成器 | 1 | `cmd/api-schema -markdown` + `docs/api/schema.md` | 重生成 → `git diff --exit-code docs/api/` |
| 生成器 | 2 | `cmd/gendocs`（config 骨架 + CLI 帮助）+ golden 测试 | `go test ./cmd/gendocs` golden 一致 |
| ADR1 | 3 | `docs/adr/` 模板 + 索引 + ADR-0001..0010 | 关联代码路径 grep 可达 |
| UDOC1 | 4 | `docs/user-guide/` 骨架 + 索引 + getting-started | `--fake-model` 零依赖冒烟非空 |
| UDOC1 | 5 | `configuration.md`（全块 prose + config 骨架表） | `gendocs -config` 重生成 diff 守门 |
| UDOC1 | 6 | `tui.md` + `entrypoints.md`（+ CLI 帮助快照） | `gendocs -help` 重生成 diff 守门 |
| UDOC1 | 7 | `skills/autovcs/goalloop/guard.md`（prose 专页） | 无占位、交叉引用可达 |
| APIREF1 | 8 | `docs/api/` 索引 + resources + events（生成表）**[COORD: G image]** | `api-schema -markdown` diff 守门；含 image |
| APIREF1 | 9 | `docs/api/` jsonrpc + sdk-ts + sdk-python | SDK 最小 e2e 对 fake-model 可跑 |
| EX1 | 10 | `examples/` 索引 + headless-exec + headless-batch | headless 冒烟非空 |
| EX1 | 11 | `examples/` sdk-typescript + sdk-python | TS `tsc --noEmit`、Python 可解析 |
| EX1 | 12 | `examples/` custom-tool + custom-skill + goalloop-config | `go build` custom-tool；goalloop 两轮演示 |
| CONTRIB1 | 13 | `CONTRIBUTING.md`（子集 + 指针） | 覆盖全部必选约定段 |
| CONTRIB1 | 14 | `docs/archive/` 归档迁移 + README + 路径映射 | grep 无指向旧路径死链 |
| CI | 15 | docs 守门 workflow（或挂 CIG1） | 生成片段 diff + examples 检查全绿 |

---

## 依赖图

```
已落地（外部）                          本计划 task
─────────────                          ───────────
D1/D2 ──────────────────┬─────────────→ 1 (api-schema -markdown) ──→ 8 (APIREF 生成表)
                        ├─────────────→ 4,5,6,7 (UDOC1)
                        ├─────────────→ 9 (APIREF prose/sdk)
                        └─────────────→ 10,11,12 (EX1)

internal/config ───────────────────────→ 2 (gendocs) ──→ 5 (config 骨架), 6 (help 快照)

synthesis §9 + CLAUDE.md ───────────────→ 3 (ADR1) ──→ 13 (CONTRIB)
                                                        13 ──→ 14 (archive)

所有生成器/docs/examples ───────────────────────────────→ 15 (CI 守门)

外部衔接（非施工，标注点）：
  G (image 字段) ──(COORD)──→ 8        # resources.md 生成表须含 image；G 落地后 CI 重生成
  CIG1 (CI 矩阵) ──(COORD)──→ 15       # 优先挂 CIG1；未就绪则自带最小 workflow
```

关键路径：`1 → 8`（生成器先于其首个真实落点）、`2 → 5/6`、`3 → 13`。ADR（3）与 EX1（10–12）与生成器（1–2）三者互不依赖，可并行。

---

## 文件结构（新/改）

| 文件/目录 | 职责 | 新/改 | 条目 |
|---|---|---|---|
| `cmd/api-schema/main.go` | 增加 `-markdown` 模式：`schemaDocument` → 资源字段表 + Schema pretty-print | 改 | APIREF1 |
| `cmd/gendocs/`（`main.go` + 子文件 + `_test.go`） | Config struct → 骨架表；`yanshi -h`/子命令 → 帮助快照 | 新 | UDOC1 |
| `docs/api/schema.md` | 嵌入 pretty-printed JSON Schema（生成区块） | 新 | APIREF1 |
| `docs/api/README.md` | 索引 + 版本契约总述 | 新 | APIREF1 |
| `docs/api/resources.md` | Thread/Turn/Item + params/responses 字段表（生成） | 新 | APIREF1 |
| `docs/api/events.md` | item.type 枚举 + ItemFromServerFrame 映射 | 新 | APIREF1 |
| `docs/api/jsonrpc.md` | app-server JSON-RPC 方法/错误码/通知 | 新 | APIREF1 |
| `docs/api/sdk-ts.md` / `sdk-python.md` | SDK 最小端到端用法 | 新 | APIREF1 |
| `docs/adr/README.md` | 索引（编号表 + 状态列） | 新 | ADR1 |
| `docs/adr/0000-template.md` | ADR 模板 | 新 | ADR1 |
| `docs/adr/0001..0010-*.md` | 首批 10 条 ADR（synthesis §9 提炼） | 新 | ADR1 |
| `docs/user-guide/README.md` | 导航索引 + getting started 摘要 | 新 | UDOC1 |
| `docs/user-guide/getting-started.md` | 零依赖 `--fake-model` 可跟做步骤 | 新 | UDOC1 |
| `docs/user-guide/configuration.md` | config.example 全块 prose + 生成骨架表 | 新 | UDOC1 |
| `docs/user-guide/tui.md` | `/` 命令/键位/交互式权限/多窗口自愈 + 帮助快照 | 新 | UDOC1 |
| `docs/user-guide/entrypoints.md` | headless/serve/app/SDK/IDE 各入口 + 帮助快照 | 新 | UDOC1 |
| `docs/user-guide/{skills,autovcs,goalloop,guard}.md` | 各专页 prose | 新 | UDOC1 |
| `examples/README.md` | 索引（每示例一行 + 怎么跑） | 新 | EX1 |
| `examples/headless-exec/` · `headless-batch/` | headless 脚本类示例 | 新 | EX1 |
| `examples/sdk-typescript/` · `sdk-python/` | SDK 端到端示例 | 新 | EX1 |
| `examples/custom-tool/` · `custom-skill/` · `goalloop-config/` | Go 工具/技能/goalloop 示例 | 新 | EX1 |
| `CONTRIBUTING.md` | 贡献者指南（子集 + 指针） | 新 | CONTRIB1 |
| `docs/archive/README.md` | 归档说明 + 原路径→新路径映射 | 新 | CONTRIB1 |
| `docs/{synthesis-final,synthesis-report,synthesis-report-v2,analysis-report,feature-comparison-with-codex,feature-roadmap-codex-deepseek}.md`、`deps_analysis.md`、`deps_raw.txt` | 历史报告 `git mv` 入 `docs/archive/` | 移 | CONTRIB1 |
| `.github/workflows/docs.yml`（或挂 CIG1） | 生成片段 diff 守门 + examples 检查 + ADR 可达 + 归档无断链 | 新/改 | 全部 |

> 生成器原则：`cmd/api-schema` 与 `cmd/gendocs` 是开发期工具（`go run ./...`），不进 release 二进制产物；沿用 D1-DEC-2 的 Go 内建、无外部依赖路线。

---

## Task 1: `cmd/api-schema` 增加 `-markdown` 模式 + `docs/api/schema.md` 落地

**Files:**
- Modify: `cmd/api-schema/main.go`
- Create: `docs/api/schema.md`

> 生成器先于其首个真实落点。本 task 既交付 `-markdown` 代码，也交付它的第一个目标文件 `docs/api/schema.md`（纯生成区块 + 极少 prose），从而让"重生成 → diff"守门立刻可跑。

- [ ] **Step 0 (RED): 写失败测试——`-markdown` 渲染 + `rewriteBlock` 单测**

先写测试 `cmd/api-schema/markdown_test.go`（引用尚未实现的渲染函数，`go test` **编译失败 = 红**）。为实现可测，把 `-markdown` 渲染与区块替换抽成包级函数（`RenderMarkdown(schema []byte) string`、`RewriteBlock(path, id, content string) error`），测试直接调用：
- **`RewriteBlock` 三例**（Advisory 1——本函数此前零测试）：
  ① 文件已含目标 id 区块 → **替换**区块内容、外围行原样保留；
  ② 文件不含该 id 区块（含空文件）→ **追加**到末尾；
  ③ **幂等**——同一 (path, id, content) 调用两次，文件字节完全相等。
- **`RenderMarkdown`**：对 `v1.SchemaBytes()` 渲染，断言含 `json` 代码围栏 + `<!-- BEGIN GENERATED: api-schema-full -->` / `<!-- END GENERATED: api-schema-full -->`；遍历 `$defs`，每个定义各产一张 `<!-- BEGIN GENERATED: api-defs:<Name> -->` 区块表。

```bash
go test ./cmd/api-schema   # 期望：编译失败 / 红（RenderMarkdown、RewriteBlock 未实现）
```

- [ ] **Step 1 (GREEN): 实现 `-markdown` 模式**

在 `cmd/api-schema/main.go` 增加一个 `-markdown <path>` flag（与现有 `-out`（TS 输出）并列，互斥校验）。当 `-markdown` 指定时：

- 读 `v1.SchemaBytes()`（或直接引用 `schemaDocument` 字面量），`json.Unmarshal` 成结构化对象。
- 渲染两类产物，按 flag 控制输出哪一类（或一个 `-markdown` 同时产两段，用 id 区分写入目标）：
  - **schema 全文**：把 JSON Schema 以 2 空格缩进 pretty-print，包进 ```` ```json … ```` 围栏，外裹 `<!-- BEGIN GENERATED: api-schema-full -->` / `<!-- END GENERATED: api-schema-full -->`。
  - **资源字段表**（供 Task 8 `resources.md` 用）：分两类。**核心资源**（Thread/Turn/Item）从 `$defs` 遍历渲染；**params/responses**（ThreadStartParams/ThreadResumeParams/ThreadInterruptParams/TurnStartParams 及 response）不在 `$defs` 内——在 `cmd/api-schema/main.go` 维护一组与 hand-written TS 接口对齐的 Go 类型映射（字段名 / 类型 / required），`-markdown` 一并渲染为相同格式的 markdown 表。两类区块统一 `<!-- BEGIN GENERATED: api-defs:<Name> -->` / `<!-- END GENERATED: api-defs:<Name> -->`。
- 输出策略：读目标文件 → 用正则定位 `BEGIN/END GENERATED` 区块 → 原地替换区块内容 → 写回；若目标文件不存在或无区块，则按"纯区块文件"创建（schema.md 即此情形）。外围 prose 与其他非生成行原样保留。
- 复用现有对 `v1.SchemaBytes()` 的 smoke-check（断言非空、`$schema` 字段存在、`$id` 符合预期），失败 `log.Fatal`，防漂移口径与现有 TS 产物一致。

实现注意：区块替换函数 `rewriteBlock(path, id, content string)`（即 Step 0 的 `RewriteBlock`）要幂等（对同一输入两次调用结果一致），且当区块 id 不存在时 append 到文件末尾（schema.md 首次创建走这条）；其 replace/append/幂等三行为由 Step 0 的单测锁死，实现满足即转绿。

- [ ] **Step 2: 创建 `docs/api/schema.md`（生成区块 + 极少 prose）**

运行：

```bash
go run ./cmd/api-schema -markdown docs/api/schema.md
```

`docs/api/schema.md` 结构（prose 极少，主体是生成区块）：

```
# v1 JSON Schema

> 以下为 `sdk/schema/v1/agent-api.schema.json` 的完整 JSON Schema，由
> `go run ./cmd/api-schema -markdown` 从 `internal/api/v1/schemaDocument` 生成。
> 修改 schema 后重生成；不要手改本区块。

<!-- BEGIN GENERATED: api-schema-full -->
```json
{ ...pretty-printed schema... }
```
<!-- END GENERATED: api-schema-full -->
```

- [ ] **Step 3: 幂等性与守门验证**

运行：

```bash
go run ./cmd/api-schema -markdown docs/api/schema.md
git diff --exit-code docs/api/schema.md   # 期望：无 diff，退出 0
```

预期：第二次生成不产生 diff（幂等）；`docs/api/schema.md` 的 JSON 区块与 `sdk/schema/v1/agent-api.schema.json` 内容一致（同源于 `schemaDocument`）。

```bash
git add cmd/api-schema/main.go docs/api/schema.md
git commit -m "feat(api-schema): add -markdown generator and bootstrap docs/api/schema.md"
```

**验收：** `-markdown` 产 schema 全文（Task 1）与资源字段表（Task 8 复用）；区块替换幂等；`git diff --exit-code docs/api/schema.md` 通过；不破坏现有 `-out`（TS）路径（运行 `go run ./cmd/api-schema -out sdk/ts/v1.ts` 仍正常）。

---

## Task 2: `cmd/gendocs` 生成器（config 骨架表 + CLI 帮助快照）

**Files:**
- Create: `cmd/gendocs/main.go`
- Create: `cmd/gendocs/config.go`
- Create: `cmd/gendocs/help.go`
- Create: `cmd/gendocs/gendocs_test.go`

> 新 `cmd/gendocs` 保 `cmd/api-schema` 职责单一。本 task 交付生成器 + golden 单测；真实 docs 落点（`configuration.md`/`tui.md`/`entrypoints.md`）在 Task 5/6 接入。

- [ ] **Step 0 (RED): 写失败的 golden 测试（`gendocs_test.go`）**

先写测试，引用尚未实现的渲染函数（`go test` **编译失败 = 红**）：
- **config 骨架**：调用 `RenderConfigSkeleton()`（不经过 `rewriteBlock`），断言输出含全部顶层块的表头、至少 N 个字段行、幂等（两次调用字节相等）；断言 `config.example.yaml` 里出现的每个顶层 key 都能在生成的骨架表里找到对应分组。
- **help 快照**：用一个返回固定 stdout 的 fake `runYanshiHelp` 函数（注入，避免测试里 `go run`），断言 `RenderHelp(<id>, stdout)` 区块包裹正确、围栏完整、幂等。
- **help dispatch 对齐**：assert 子命令清单（含 `pr`、`auth` 等，见 `cmd/yanshi/main.go` dispatch）与 `cmd/yanshi/main.go` 的实际 `case` 分支一致——测试读取 main.go 的 dispatch 源码或维护一个清单常量 + 一个测试断言二者一致。

```bash
go test ./cmd/gendocs   # 期望：编译失败 / 红（RenderConfigSkeleton、RenderHelp 未实现）
```

- [ ] **Step 1 (GREEN): 实现 config 骨架表生成（`config.go`）**

`-config <path>` 模式：`reflect` 遍历 `internal/config.Config`（及递归嵌套 struct），读每个字段的 `yaml` tag 作为 key、Go 类型作为 type，输出一张 markdown 骨架表（列：key / type / 说明留空供 prose 填）。按顶层字段分组（`server`/`storage`/`llm`/`agents`/`subagents`/`skills`/`vcs`/`compaction`/`profiles`/`security`/`batch`/`lsp`/`mcp`/`memory`/`observability`/`features`/`pricing`/`secrets`/`token`/`auth`/`i18n`/`tui`，以 struct 实际字段为准，不硬编码列表）。包进 `<!-- BEGIN GENERATED: config-skeleton -->` / `<!-- END GENERATED: config-skeleton -->`，复用 Task 1 的 `rewriteBlock` 幂等替换（把 `rewriteBlock` 提到一个共享的小 helper 包或复制最小实现——本 task 内复制，避免引入新包耦合；若 Task 1 已抽出公共 helper 则复用）。

- [ ] **Step 2 (GREEN): 实现 CLI 帮助快照生成（`help.go`）**

`-help <subcmd>` 模式：用 `exec.Command("go", "run", "./cmd/yanshi", <subcmd>, "-h")`（`subcmd` 为空表 bare `yanshi -h`）捕获 stdout，包进 ```` ```text … ```` 围栏，外裹 `<!-- BEGIN GENERATED: help:<id> -->` / `<!-- END GENERATED: help:<id> -->`（`<id>` ∈ `yanshi`/`serve`/`goal`/`exec`/`chat`/`app`/`vcs-mcp`/`pr`/`auth`/`doctor`，对应 `cmd/yanshi/main.go` 的 dispatch）。支持 `-help-all` 一次遍历全部子命令。输出经 `rewriteBlock` 写入指定目标文件的对应区块。

子命令清单从 `cmd/yanshi/main.go` 的 dispatch 提取，**不硬编码**（扫源码或维护一个与 dispatch 对齐的清单常量 + 一个测试断言二者一致）。

- [ ] **Step 3: GREEN 确认**

Step 0 写好的 golden 测试在 Step 1/2 实现后转绿；本步不再新增测试，只重跑确认：

```bash
go test ./cmd/gendocs   # 全绿
```

```bash
git add cmd/gendocs
git commit -m "feat(gendocs): add config skeleton and CLI help snapshot generator"
```

**验收：** `go test ./cmd/gendocs` 绿；config 骨架覆盖 `Config` 全部顶层字段；help 子命令清单与 `cmd/yanshi/main.go` dispatch 对齐（测试断言）；生成幂等；不依赖外部工具链（仅 `go run`/`reflect`）。

---

## Task 3: `docs/adr/` 模板 + 索引 + ADR-0001..ADR-0010

**Files:**
- Create: `docs/adr/README.md`
- Create: `docs/adr/0000-template.md`
- Create: `docs/adr/0001-unknown-tools-handler-result-not-error.md`
- Create: `docs/adr/0002-runners-cache-key-model-pointer.md`
- Create: `docs/adr/0003-guard-fail-closed-empty-allow.md`
- Create: `docs/adr/0004-guard-stateless-and-shell-metachar-hardblock.md`
- Create: `docs/adr/0005-compaction-summary-user-role.md`
- Create: `docs/adr/0006-compaction-unified-core-strict-window.md`
- Create: `docs/adr/0007-ws-holds-history-sse-replays-shared-proto.md`
- Create: `docs/adr/0008-autovcs-context-injection-overrides-scope.md`
- Create: `docs/adr/0009-sqlite-pseudogit-tree-merge.md`
- Create: `docs/adr/0010-sse-static-profile-no-interactive-perm.md`

> 纯文档，无代码依赖，可与 Task 1/2/10 并行。ADR **引用 CLAUDE.md / synthesis §9 不复制**；独有价值 = 带 `superseded` 状态的演进档案。

- [ ] **Step 0 (RED): 确认基线——ADR 目录与文件不存在**

在本 task 之前 `docs/adr/` 不存在（模板、索引、ADR-0001..0010 均缺），末尾 Step 3 的关联路径可达性 grep（GREEN）无目标可检：

```bash
[ ! -e docs/adr/README.md ] || { echo "baseline dirty: docs/adr exists"; exit 1; }
```

- [ ] **Step 1: 写模板与索引**

`docs/adr/0000-template.md`（spec §6 模板）：

```
# ADR-NNNN: 标题
- 状态：proposed | accepted | deprecated | superseded by ADR-MMMM
- 日期：YYYY-MM-DD
## 背景（Context）
## 决策（Decision）
## 后果（Consequences）—— 含不可违反的约束
## 关联（CLAUDE.md / synthesis §9.x / 相关代码落点）
```

`docs/adr/README.md`：ADR 编号表（编号 / 标题 / 状态 / 来源节），开头一句"ADR 是单决策演进档案；CLAUDE.md 是全景当前态；新架构决策先写/更新 ADR（见 CONTRIBUTING.md）"。

- [ ] **Step 2: 写 ADR-0001..ADR-0010**

逐条对应 spec §6 的首批表（来源 synthesis §9.1–§9.5）。每条"决策要点"即"不可违反的约束"原文落进 Consequences；"关联"段指向真实代码落点（见下），且引用 synthesis §9.x 与 CLAUDE.md 对应段。

| ADR | 来源 | 关联代码落点（关联段须指向） |
|---|---|---|
| 0001 | §9.1 | `internal/agent/orchestrator/`（UnknownToolsHandler） |
| 0002 | §9.1 | `internal/agent/orchestrator/`（runners sync.Map） |
| 0003 | §9.2 | `internal/guard/`（checkTools） |
| 0004 | §9.2 | `internal/guard/`（checkShell 元字符） |
| 0005 | §9.3 | `internal/ctxcompact/`（Assemble User+sentinel） |
| 0006 | §9.3 | `internal/ctxcompact/`（Run + 携带式分块） |
| 0007 | §9.4 | `internal/proto/frame.go`、`internal/api/http/ws.go`、`internal/api/http/chat.go`（`handleSSEInternal`） |
| 0008 | §9.5 | `internal/tools/vcsctx.go`、`internal/vcs/` |
| 0009 | §9.5 | `internal/vcs/`（树级三方合并） |
| 0010 | §9.2 | `internal/api/http/chat.go`（`handleSSEInternal` 静态 profile） |

- [ ] **Step 3 (GREEN): 可达性自检**

```bash
# 断言每条 ADR 的"关联"段指向的代码路径真实存在
grep -rhoE 'internal/[a-z0-9/_-]+(/[a-z0-9_-]+\.go)?' docs/adr/ | sort -u | while read p; do
  [ -e "$p" ] || [ -n "$(ls "${p%/}"* 2>/dev/null)" ] || { echo "MISSING: $p"; exit 1; }
done
```

预期：退出 0（所有关联路径可达）。此检查后续由 Task 15 的 CI job 固化。

```bash
git add docs/adr
git commit -m "docs(adr): add ADR template index and ADR-0001..0010 from synthesis §9"
```

**验收：** ≥10 条 ADR 覆盖 synthesis §9 全部 5 个子节；模板与索引存在；每条"关联"段路径 grep 可达（无指向已删文件）。

---

## Task 4: `docs/user-guide/` 骨架 + 索引 + `getting-started.md`

**Files:**
- Create: `docs/user-guide/README.md`
- Create: `docs/user-guide/getting-started.md`

- [ ] **Step 0 (RED): 确认基线——user-guide 目录不存在**

在本 task 之前 `docs/user-guide/` 不存在（README、getting-started 均缺）；末尾 Step 3 的 `--fake-model` 零依赖冒烟非空（GREEN）为完成断言。

- [ ] **Step 1: 写导航索引（`README.md`）**

`docs/user-guide/README.md`：一段定位（"面向终端用户的怎么用 yanshi；贡献者见 CONTRIBUTING.md，架构决策见 docs/adr/"）+ 各专页一行摘要 + 指向 getting-started 的醒目入口。列出全部专页（getting-started/configuration/tui/skills/autovcs/goalloop/guard/entrypoints），未完成的页在本 task 可暂以"见 Task X"占位，但 **Task 4 只交付 README + getting-started 两份完整文件**，其余页占位行由各自 task 完成时改为实链。

- [ ] **Step 2: 写 getting-started（零依赖可跟做）**

完全沿用 README 的 `--fake-model` 路径，展开成可跟做步骤：
1. `go build -o yanshi ./cmd/yanshi`
2. `cp config.example.yaml config.yaml`（说明 `config.yaml` 已 gitignore、`${VAR}` 展开）
3. `./yanshi --fake-model -inprocess`（或 `timeout 5 ./yanshi --fake-model -inprocess` 用于 CI/管道；说明 alt-screen TUI 无法管道驱动）
4. 描述"看到什么输出、按什么键发送第一个 turn、第一个工具调用长什么样"。
5. 一个 headless 替代：`./yanshi exec --fake-model -p "hello"`（用于无法起 TUI 的环境）。

强调"无需任何 API key"（`llm.providers` 为空时自动选 fake model）。

- [ ] **Step 3 (GREEN): 零依赖冒烟验证**

```bash
go build -o yanshi ./cmd/yanshi
out=$(./yanshi exec --fake-model -p "hello" 2>&1 || true)
[ -n "$out" ] || { echo "empty output"; exit 1; }
```

预期：`yanshi exec --fake-model` 产出非空确定性输出（证明 getting-started 的零依赖路径成立）。

```bash
git add docs/user-guide/README.md docs/user-guide/getting-started.md
git commit -m "docs(user-guide): add index and zero-dependency getting-started"
```

**验收：** README 索引列出全部专页；getting-started 全程 `--fake-model`、零 API key；`yanshi exec --fake-model` 冒烟非空。

---

## Task 5: `configuration.md`（全块 prose + gendocs 骨架表）

**Files:**
- Create: `docs/user-guide/configuration.md`

- [ ] **Step 0 (RED): 确认基线——配置页与生成区块不存在或与生成器不一致**

在本 task 之前 `docs/user-guide/configuration.md` 不存在，其中 `<!-- BEGIN GENERATED: config-skeleton -->` 区块为空 / 与 `cmd/gendocs -config` 当前输出不一致；本 task 产出规范版本，末尾 Step 3 的重生成 `git diff --exit-code`（GREEN）为完成断言。

- [ ] **Step 1: 写 prose（围绕 `config.example.yaml` 全顶层块）**

对每个顶层块（`server`/`storage`/`llm`/`agents`/`subagents`/`skills`/`vcs`/`compaction`/`profiles`/`security`/`batch`/`lsp`/`mcp`/`memory`/`observability`/`features`/`pricing`/`secrets`/`token`/`auth`/`i18n`/`tui`）写一段 prose：语义、默认值、与其他块关系（例：`compaction.context_window` 与 provider `context_window` 的回退关系；`security` 与 `profiles` 的叠加）。prose 解释"为什么"，**不**逐字抄 `config.example.yaml` 注释。

- [ ] **Step 2: 嵌入生成的骨架表**

在文件合适位置放区块标记，运行：

```bash
go run ./cmd/gendocs -config docs/user-guide/configuration.md
git diff --exit-code docs/user-guide/configuration.md
```

`<!-- BEGIN GENERATED: config-skeleton -->` 区块由 Task 2 的 gendocs 填充；prose 在区块外。

- [ ] **Step 3 (GREEN): 配置一致性弱断言**

```bash
go run ./cmd/gendocs -config docs/user-guide/configuration.md
git diff --exit-code docs/user-guide/configuration.md   # 幂等
```

预期：重生成无 diff；骨架表分组覆盖 `Config` 全部顶层字段。

```bash
git add docs/user-guide/configuration.md
git commit -m "docs(user-guide): add configuration reference with generated skeleton table"
```

**验收：** 全顶层块有 prose；生成骨架表与 struct 一致（diff 守门）；prose 与区块分离、重生成不破坏 prose。

---

## Task 6: `tui.md` + `entrypoints.md`（prose + CLI 帮助快照）

**Files:**
- Create: `docs/user-guide/tui.md`
- Create: `docs/user-guide/entrypoints.md`

- [ ] **Step 0 (RED): 确认基线——tui/entrypoints 页与 help 快照区块不存在或与生成器不一致**

在本 task 之前 `docs/user-guide/tui.md`、`docs/user-guide/entrypoints.md` 不存在，其中 `<!-- BEGIN GENERATED: help:<id> -->` 区块为空 / 与 `cmd/gendocs -help-all` 当前输出不一致；本 task 产出规范版本，末尾 Step 3 的重生成 `git diff --exit-code`（GREEN）为完成断言。

- [ ] **Step 1: 写 `tui.md`**

prose 覆盖：`/` 前缀命令（`/model`、`/skill` 等的语义，提炼自 CLAUDE.md 对外可见部分）、键位（Enter=发送、Ctrl+Enter=换行——来自 bubbletea fork）、交互式权限模式（`default`/`allow-edits`/`yolo`/`auto`）、多窗口自愈（lockfile 选举 + PID 存活回收）。**不**写贡献者向的实现细节（装配、runners 缓存）。

在子命令帮助处放区块标记：

```
<!-- BEGIN GENERATED: help:yanshi -->
```text
...yanshi -h 输出...
```
<!-- END GENERATED: help:yanshi -->
```

- [ ] **Step 2: 写 `entrypoints.md`**

对各入口（bare TUI / `serve` / `chat --no-tui` / `exec` / `app`（JSON-RPC）/ SDK / IDE）给"适用场景 + 一行启动命令"。每个有 `-h` 的子命令嵌帮助快照区块（`help:serve`/`help:goal`/`help:exec`/`help:chat`/`help:app`/`help:vcs-mcp`/`help:doctor`）。

- [ ] **Step 3 (GREEN): 生成与守门**

```bash
go run ./cmd/gendocs -help-all -out docs/user-guide/tui.md -out docs/user-guide/entrypoints.md
# 或按 gendocs 设计：逐文件写入对应区块
git diff --exit-code docs/user-guide/tui.md docs/user-guide/entrypoints.md
```

预期：所有 `help:<id>` 区块填充且幂等；帮助文本与实际 `yanshi <sub> -h` 一致。

```bash
git add docs/user-guide/tui.md docs/user-guide/entrypoints.md
git commit -m "docs(user-guide): add tui and entrypoints pages with generated help snapshots"
```

**验收：** tui/entrypoints prose 完整；全部子命令帮助为生成区块（diff 守门）；不含贡献者向实现细节（受众边界）。

---

## Task 7: `skills.md` + `autovcs.md` + `goalloop.md` + `guard.md`（prose 专页）

**Files:**
- Create: `docs/user-guide/skills.md`
- Create: `docs/user-guide/autovcs.md`
- Create: `docs/user-guide/goalloop.md`
- Create: `docs/user-guide/guard.md`

> 纯 prose 专页，交叉引用活技术参考文档而非复制。

- [ ] **Step 0 (RED): 确认基线——四页 prose 不存在 / 含占位或断链**

在本 task 之前 `docs/user-guide/{skills,autovcs,goalloop,guard}.md` 不存在或含 TODO/TBD/占位、交叉引用断链；末尾 Step 2 的"无占位 + 交叉引用可达"grep（GREEN）为完成断言。

- [ ] **Step 1: 写四页 prose**

- `skills.md`：技能放哪（`skills/`）、怎么写 SKILL.md、渐进披露机制；**引用 `docs/skills-authoring.md`** 获取权威细节，不复制。
- `autovcs.md`：编辑自动追踪（经 context 注入）、worktree、对外可见视角（"agent 编辑流经 fs 工具即被追踪"）；**提炼 `docs/vcs.md`** 的用户面，不复制内部合并算法细节。
- `goalloop.md`：`yanshi goal` 的 plan→implement→evaluate→judge；`--fake-model` 两轮演示命令；`MaxIterations` 预算；T0–T4 层级（`auto`/`t0`..`t4`）。
- `guard.md`：四维权限（tools/fs/shell/net）、profile（`profiles:` map）、交互式 mode、fail-closed（空 Allow 拒绝一切）、shell 元字符硬拦截 → 顺序执行多条命令。

- [ ] **Step 2 (GREEN): 占位与交叉引用自检**

```bash
# 无 "TODO/TBD/待补/占位" 残留
! grep -rniE 'TODO|TBD|待补|占位|placeholder' docs/user-guide/{skills,autovcs,goalloop,guard}.md
# 交叉引用的目标文件存在
for f in docs/skills-authoring.md docs/vcs.md; do [ -f "$f" ] || { echo "missing $f"; exit 1; }; done
```

```bash
git add docs/user-guide/skills.md docs/user-guide/autovcs.md docs/user-guide/goalloop.md docs/user-guide/guard.md
git commit -m "docs(user-guide): add skills autovcs goalloop and guard topic pages"
```

**验收：** 四页 prose 完整无占位；交叉引用 `docs/skills-authoring.md`/`docs/vcs.md` 可达；不复制源文档（引用）。

---

## Task 8: `docs/api/` 索引 + `resources.md` + `events.md`（生成式表） **[COORD: G image 字段]**

**Files:**
- Create: `docs/api/README.md`
- Create: `docs/api/resources.md`
- Create: `docs/api/events.md`

> **COORD（G 衔接）**：`resources.md` 的生成式资源字段表依赖 G 把 `image` 字段加入 `internal/api/v1/schemaDocument`。**执行顺序**：若 G 已落地 → 本 task 直接生成含 image 的表；若 G 未落地 → 本 task 生成当前态表，并在 `resources.md` 顶部留一行 `> 待 G 落地 image 字段后由 CI 重生成`，G 落地后 Task 15 的 CI 守门捕获 diff 并触发重生成（不需本 task 重开）。

- [ ] **Step 0 (RED): 确认基线——api 索引/resources/events 不存在或生成表不一致**

在本 task 之前 `docs/api/README.md`、`docs/api/resources.md`、`docs/api/events.md` 不存在，其中 `<!-- BEGIN GENERATED: api-defs:<Name> -->` 区块为空 / 与 `cmd/api-schema -markdown` 当前输出不一致；本 task 产出规范版本，末尾 Step 4 的重生成 `git diff --exit-code` + events 枚举一致性（GREEN）为完成断言。

- [ ] **Step 1: 写索引（`README.md`）**

版本契约总述：`version: "v1"`、unknown 字段策略（客户端忽略、服务端保留为 `event.<legacyType>`）、item 类型枚举、camelCase、`additionalProperties: true` 的容忍语义。指向各页 + 指向 `sdk/schema/CONTRACT_HANDOFF.md`。

- [ ] **Step 2: 写 `resources.md`（生成表 + 极少 prose）**

对 Thread/Turn/Item 资源 + params/responses（`ThreadStartParams`/`ThreadResumeParams`/`ThreadInterruptParams`/`TurnStartParams` 及 response）每类给一段 ≤3 行语义说明（来自 spec §5 资源页要点），紧接生成字段表。区块标记：

```
<!-- BEGIN GENERATED: api-defs:Thread -->
| 字段 | 类型 | required | 说明 |
|---|---|---|---|
| ... |
<!-- END GENERATED: api-defs:Thread -->
```

每个资源项（Thread/Turn/Item + params/responses）一个区块，由 Task 1 的 `cmd/api-schema -markdown` 统一渲染。运行：

```bash
go run ./cmd/api-schema -markdown docs/api/resources.md
git diff --exit-code docs/api/resources.md
```

- [ ] **Step 3: 写 `events.md`**

列 `item.type` 枚举（`turn.started`/`message.delta`/`reasoning.delta`/`tool.call`/`tool.result`/`tool.progress`/`structured.result`/`turn.error`/`turn.completed`，来自 `types.go` 常量）为一张表（prose，枚举本身是稳定契约，**不**走生成区块——除非后续 batch 提供常量→markdown 生成器，本 task 手列并与 `types.go` 常量在 Step 4 做一致性断言）。说明 legacy frame → item 的映射（`events.go` `ItemFromServerFrame`）与"未知帧保留不丢"策略。

- [ ] **Step 4 (GREEN): 一致性守门**

```bash
go run ./cmd/api-schema -markdown docs/api/resources.md
git diff --exit-code docs/api/                                    # 生成表幂等
# events 枚举与 types.go 常量一致性（弱断言：每个枚举值在 types.go 出现）
for t in turn.started message.delta reasoning.delta tool.call tool.result tool.progress structured.result turn.error turn.completed; do
  grep -rq "\"$t\"" internal/api/v1/ || { echo "missing const $t"; exit 1; }
done
```

预期：resources 生成表幂等且（G 落地后）含 image 字段；events 枚举与 `types.go` 一致。

```bash
git add docs/api/README.md docs/api/resources.md docs/api/events.md
git commit -m "docs(api): add index resources and events reference with generated tables"
```

**验收：** 索引给版本契约总述；resources 生成表覆盖全部资源/params/responses（CI 守门）；events 枚举与 `types.go` 一致；COORD 行就位（G 未落地时）或 image 已在表（G 落地时）。

---

## Task 9: `docs/api/jsonrpc.md` + `sdk-ts.md` + `sdk-python.md`

**Files:**
- Create: `docs/api/jsonrpc.md`
- Create: `docs/api/sdk-ts.md`
- Create: `docs/api/sdk-python.md`

- [ ] **Step 0 (RED): 确认基线——jsonrpc/sdk 三页不存在**

在本 task 之前 `docs/api/jsonrpc.md`、`docs/api/sdk-ts.md`、`docs/api/sdk-python.md` 不存在；末尾 Step 4 的错误码可达 + TS typecheck（GREEN）为完成断言。

- [ ] **Step 1: 写 `jsonrpc.md`（基于 `internal/appserver/rpc.go`）**

方法表（`thread/start`/`thread/resume`/`turn/start`/`thread/interrupt`/`capabilities` 等）、JSON-RPC 2.0 标准错误码（-32700/-32600/-32601/-32602/-32603）、`item/updated` 通知、`ID` 为 RawMessage（string/number/null 原样回显）、notification（缺 ID）不回响应。显式标注"与 HTTP/SSE 共用同一 `*v1.Service`，语义不漂移"，并说明不对称处（SSE 静态 profile vs WS 交互式权限）。错误码与方法名在 Step 4 做与 `rpc.go` 的一致性断言。

- [ ] **Step 2: 写 `sdk-ts.md`**

最小端到端：建 thread → 起 turn → 消费 item 流 → interrupt。引 `sdk/ts/v1.ts` 类型（由 `cmd/api-schema` 生成）。给"如何对 fake-model 后端跑通"段（呼应 EX1 的 `examples/sdk-typescript/`，交叉引用而非复制脚本）。

- [ ] **Step 3: 写 `sdk-python.md`**

同上，引 `yanshi_sdk` 包（`sdk/python/`），最小端到端 + fake-model 后端说明，交叉引用 `examples/sdk-python/`。

- [ ] **Step 4 (GREEN): jsonrpc 一致性弱断言 + SDK e2e 可跑**

```bash
# 错误码在 rpc.go 出现
for c in 32700 32600 32601 32602 32603; do grep -rq "$c" internal/appserver/ || { echo "missing code -$c"; exit 1; }; done
# TS SDK 最小 e2e 对 fake-model 后端（起后端 → 跑最小 client 调用）
# （详细脚本见 examples/sdk-typescript，本 step 只断言文档里的代码片段可被 sdk/ts 类型检查通过）
npm --prefix sdk/ts run typecheck
```

预期：错误码可达；TS SDK typecheck 绿（证明 `sdk-ts.md` 引用的类型存在）。

```bash
git add docs/api/jsonrpc.md docs/api/sdk-ts.md docs/api/sdk-python.md
git commit -m "docs(api): add jsonrpc and sdk usage reference"
```

**验收：** jsonrpc 方法/错误码与 `rpc.go` 一致；SDK 两页各给可跑的最小端到端；与 EX1 交叉引用不复制。

---

## Task 10: `examples/` 索引 + `headless-exec` + `headless-batch`

**Files:**
- Create: `examples/README.md`
- Create: `examples/headless-exec/run.sh`（或 `.md` 含可复制命令）
- Create: `examples/headless-batch/run.sh` + `sample.jsonl`

- [ ] **Step 0 (RED): 确认基线——headless 示例不存在 / 冒烟为空**

在本 task 之前 `examples/headless-exec/`、`examples/headless-batch/` 不存在，`--fake-model` 单 turn / batch 冒烟无输出；末尾 Step 4 的冒烟非空（GREEN）为完成断言。

- [ ] **Step 1: 写索引**

`examples/README.md`：每个示例一行说明 + "复制即跑"命令（全部 `--fake-model`、零 API key）。顶部一句"示例对 fake-model 友好；CI 验证可编译/可冒烟（见 `.github/workflows`）"。

- [ ] **Step 2: headless-exec**

最小：`yanshi exec --fake-model -p "hello"` 单 turn 文本进/出。`run.sh` 构建并跑，断言非空输出。

- [ ] **Step 3: headless-batch**

`yanshi chat --no-tui --fake-model --input sample.jsonl` 批处理；`sample.jsonl` 给 2–3 行样例。`run.sh` 跑并断言产出行数与输入相关。

- [ ] **Step 4 (GREEN): 冒烟**

```bash
go build -o yanshi ./cmd/yanshi
out=$(./yanshi exec --fake-model -p "hello"); [ -n "$out" ] || exit 1
```

```bash
git add examples/README.md examples/headless-exec examples/headless-batch
git commit -m "examples: add headless exec and batch fake-model samples"
```

**验收：** 两脚本 `--fake-model` 零 key 跑通非空；README 有"怎么跑"。

---

## Task 11: `examples/sdk-typescript` + `examples/sdk-python`

**Files:**
- Create: `examples/sdk-typescript/index.ts` + `README.md`
- Create: `examples/sdk-python/main.py` + `README.md`

- [ ] **Step 0 (RED): 确认基线——TS/Python SDK 示例不存在 / 不可编译或不可解析**

在本 task 之前 `examples/sdk-typescript/`、`examples/sdk-python/` 不存在；TS `tsc --noEmit` 无目标、Python `py_compile` 无目标（红）；末尾 Step 3 的编译/解析通过（GREEN）为完成断言。

- [ ] **Step 1: TS 示例**

`index.ts`：用 `sdk/ts` 的 `AgentClient` 对本地 `yanshi serve --fake-model`（`127.0.0.1` 固定端口 + token 或 loopback 免 token）做 thread/start → turn/start → 消费 item 流 → cancel。README 给起后端 + 跑示例的两步命令。依赖 `sdk/ts`（`npm install` 本地 file 引用，不重复实现 transport）。

- [ ] **Step 2: Python 示例**

`main.py`：同上，用 `yanshi_sdk.AgentClient`。README 两步命令。

- [ ] **Step 3 (GREEN): 可编译/可解析**

```bash
# TS
npm --prefix sdk/ts run typecheck
npx tsc --noEmit --project examples/sdk-typescript/tsconfig.json 2>/dev/null || \
  npx tsc --noEmit --strict --module NodeNext --moduleResolution NodeNext --target ES2022 \
    --paths '{"@x6nux/yanshi-sdk":["./sdk/ts/src/index.ts"]}' examples/sdk-typescript/index.ts
# Python（不强求装运行时，但要能解析）
python -m py_compile examples/sdk-python/main.py
```

```bash
git add examples/sdk-typescript examples/sdk-python
git commit -m "examples: add TypeScript and Python SDK end-to-end samples"
```

**验收：** TS `tsc --noEmit` 通过；Python `py_compile` 通过；两示例指向本地 fake-model 后端、零 API key。

---

## Task 12: `examples/custom-tool` + `examples/custom-skill` + `examples/goalloop-config`

**Files:**
- Create: `examples/custom-tool/main.go` + `README.md`
- Create: `examples/custom-skill/`（SKILL.md 目录结构样例）+ `README.md`
- Create: `examples/goalloop-config/`（config 片段 + run.sh）+ `README.md`

> **API gap 政策（锁定决策）**：custom-tool 示例只依赖**公开面**（`tools.GuardedTool` 接口、config profile）。若实现所需符号未导出，**标注为"示例驱动的外部 API gap"**（在 `examples/custom-tool/README.md` 顶部列清单 + 在 `examples/README.md` 汇总），反馈给后续 batch；**不在 examples 里 hack、不临时导出 internal 符号**。

- [ ] **Step 0 (RED): 确认基线——custom-tool/skill/goalloop 示例不存在 / 不可编译或不可冒烟**

在本 task 之前 `examples/custom-tool/`、`examples/custom-skill/`、`examples/goalloop-config/` 不存在；`go build ./examples/custom-tool` 无目标（红）、goalloop 两轮演示无产出；末尾 Step 4 的可编译/可冒烟（GREEN）为完成断言。

- [ ] **Step 1: custom-tool（Go，编译型）**

`main.go`：实现一个最小 `tools.GuardedTool`，挂进 `bootstrap.Build` 风格的装配（展示接口与 guard profile 配法）。可连 fake model，不强求连真实模型。`go build ./examples/custom-tool` 必须过；若卡在未导出符号，按 API gap 政策处理（README 标注 + 降级为"接口形状示例 + 注释说明 gap"），保持可编译。

- [ ] **Step 2: custom-skill**

一个自定义 SKILL.md 目录结构样例（`name`/`description`/触发/instructions 字段），加一段"如何让 yanshi 发现并调用它"的说明（放 `skills/` 下或 `config.yaml` 指定路径）。无代码编译。

- [ ] **Step 3: goalloop-config**

`yanshi goal --fake-model` 的两轮演示命令 + goalloop 配置片段（`MaxIterations`、tier 选择 `auto`/`t0`..`t4`）。`run.sh` 跑两轮并断言产出。

- [ ] **Step 4 (GREEN): 可编译/可冒烟**

```bash
go build ./examples/custom-tool                 # Go 示例可 build（含 API gap 降级）
./yanshi goal --fake-model --max-iterations 2 ... # goalloop 两轮冒烟（按 run.sh）
# custom-skill：断言 SKILL.md 结构字段齐全
```

```bash
git add examples/custom-tool examples/custom-skill examples/goalloop-config
git commit -m "examples: add custom-tool custom-skill and goalloop-config samples"
```

**验收：** `go build ./examples/custom-tool` 过（API gap 按政策降级）；custom-skill 结构完整；goalloop 两轮演示可跑；API gap 清单就位（若有）。

---

## Task 13: `CONTRIBUTING.md`（贡献者子集 + 指针）

**Files:**
- Create: `CONTRIBUTING.md`

> 依赖 Task 3（ADR）：CONTRIB 引用 `docs/adr/` 的"新决策走 ADR"流程。

- [ ] **Step 0 (RED): 确认基线——CONTRIBUTING.md 不存在 / 未覆盖必选约定段**

在本 task 之前 `CONTRIBUTING.md` 不存在或未覆盖全部必选约定段（build/test、六边形、context 注入、guard fail-closed、Fake 优先、1000 行、注释密度、单 binary、传输协议、fork、ADR 流程、提交规范、被忽略产物）；末尾 Step 2 的覆盖度 grep（GREEN）为完成断言。

- [ ] **Step 1: 写 CONTRIBUTING（提炼自 CLAUDE.md，面向贡献者）**

覆盖（每条后写"详见 CLAUDE.md 对应段"，**不逐字重复**）：
- **怎么开始**：`go build`/`go test ./...`/`go run ./cmd/testchanged`、`--fake-model` 零依赖开发、`cp config.example.yaml config.yaml`。
- **架构约定（承重）**：六边形依赖向内流；**唯一组合根 `internal/bootstrap/Build`**；装配顺序固定（config→store→vcs→model→tools→orchestrator→http→task broker）。
- **context 注入横切**：`tools.WithProfile`/`WithSubAgentRunner`/`WithVCS`，不塞工具参数。
- **guard fail-closed**：空 Allow 拒绝一切；无状态；shell 元字符硬拦截；新增工具必须显式配权限。
- **Fake 优先于 mock**：优先新增 fake（`einollm.FakeModel` 等）。
- **单文件 ≤1000 纯代码行**（不含注释/空行）；超了先拆。
- **重复逻辑必须抽公共函数**。
- **注释是承重文档**：包/导出符号带多段 doc 注释解释"为什么"，ADK/guard/VCS 周围保持密度。
- **单 binary 客户端+服务端**：TUI 是本地轻客户端。
- **两种传输一套协议**：WS/SSE 共用 `internal/proto/frame.go`；新帧同步 `ws.go`+`ssebackend.go`。
- **本地 fork**：`go.mod` replace 钉 `bubbletea` 到 `./third_party/bubbletea`；改 bubbletea 行为改 fork、不去 replace。
- **新架构决策走 ADR**（引 `docs/adr/`，ADR-0001 起）。
- **提交/PR 约定**：conventional commit prefix（与 VER1 CHANGELOG 自动生成对齐）。
- **被忽略的产物**：`config.yaml`/`*.db`/构建二进制不提交。

边界：CONTRIBUTING 比 CLAUDE.md **更短、更"第一步导向"**；是子集 + 指针，非复制。

- [ ] **Step 2 (GREEN): 覆盖度自检**

```bash
for kw in bootstrap.Build WithProfile fail-closed FakeModel 1000 bubbletea "internal/proto/frame.go" ADR conventional; do
  grep -q "$kw" CONTRIBUTING.md || { echo "missing topic: $kw"; exit 1; }
done
```

```bash
git add CONTRIBUTING.md
git commit -m "docs: add CONTRIBUTING guide as contributor subset with CLAUDE.md and ADR pointers"
```

**验收：** 全部必选约定段在（build/test、六边形、context 注入、guard fail-closed、Fake 优先、1000 行、注释密度、单 binary、传输协议、fork、ADR 流程、提交规范、被忽略产物）；每条指向 CLAUDE.md 段落；不逐字重复 CLAUDE.md。

---

## Task 14: `docs/archive/` 归档迁移 + README + 路径映射

**Files:**
- Create: `docs/archive/README.md`
- Move (`git mv`): 见 Step 1 清单

> 依赖 Task 3（ADR-00xx 的"关联"引用归档路径时用新路径）+ Task 13（CONTRIB 指向 CLAUDE.md 为权威当前态）。

- [ ] **Step 0 (RED): 确认基线——archive 目录不存在 / 存在指向旧路径的死链**

在本 task 之前 `docs/archive/` 不存在，历史报告仍散落在 `docs/` 根与仓库根，且引用它们处使用旧路径（即 grep 命中旧裸路径 = 红）；本 task `git mv` 入归档 + 修引用，末尾 Step 3 的"无指向旧路径死链"grep（GREEN）为完成断言。

- [ ] **Step 1: 移动前 grep 引用，再 `git mv`**

先 grep 待移文件被哪些 `.md` 引用（记录断链风险）：

```bash
for f in synthesis-final synthesis-report synthesis-report-v2 analysis-report feature-comparison-with-codex feature-roadmap-codex-deepseek; do
  echo "== $f =="; grep -rl "$f" --include='*.md' . | grep -v '^docs/archive/'
done
```

`git mv`（保留历史）：

```bash
mkdir -p docs/archive
git mv docs/synthesis-final.md            docs/archive/
git mv docs/synthesis-report.md           docs/archive/
git mv docs/synthesis-report-v2.md        docs/archive/
git mv docs/analysis-report.md            docs/archive/
git mv docs/feature-comparison-with-codex.md docs/archive/
git mv docs/feature-roadmap-codex-deepseek.md docs/archive/
git mv deps_analysis.md                   docs/archive/
git mv deps_raw.txt                       docs/archive/
```

> `docs/synthesis-final.md` 是本计划的 ADR 源；§9 已被 ADR-0001..0010 结构化提炼（Task 3），归档的是"分析快照"，决策活在 ADR。ADR-00xx 的"关联"段若引用了 synthesis §9.x，用 `docs/archive/synthesis-final.md` 新路径。

- [ ] **Step 2: 写 `docs/archive/README.md`**

一句定位 + "原路径→新路径"映射表 + 指针："权威当前态见 CLAUDE.md；决策演进见 docs/adr/"。

- [ ] **Step 3 (GREEN): 修引用断链 + 无断链自检**

把 Step 1 grep 命中的非归档引用改为 `docs/archive/...` 新路径（特别是 ADR 的"关联"段、CONTRIBUTING、docs 根的活档互引）。

```bash
# 无指向归档前旧路径的死链（docs/synthesis-final.md 等作为裸路径不应再出现在非归档 .md）
! grep -rE 'docs/(synthesis-final|synthesis-report|synthesis-report-v2|analysis-report|feature-comparison-with-codex)\.md' \
  --include='*.md' . | grep -v '^docs/archive/'
```

```bash
git add -A docs/archive docs/ CONTRIBUTING.md docs/adr
git commit -m "docs: archive historical analysis reports under docs/archive with path mapping"
```

**验收：** `docs/archive/` 存在且历史报告已 `git mv` 入内（历史保留）；`docs/` 根只剩活档/入口；无指向旧路径的死链（grep 验证）；映射表就位。

---

## Task 15: docs 守门 CI workflow（生成片段 diff + examples 检查 + ADR 可达 + 归档无断链）

**Files:**
- Create: `.github/workflows/docs.yml`（或挂入 CIG1 矩阵——见 COORD）

> **COORD（CIG1 衔接）**：优先把本 job 挂入 CIG1 的 CI 门禁矩阵。CIG1 未就绪时，本 task 自带最小 `.github/workflows/docs.yml` 兜底（独立、不阻塞）。两条路径择一，由实施时 CIG1 状态决定。

- [ ] **Step 1: 写 workflow**

job（`docs-gate`）步骤：
1. checkout + setup-go（1.26.x）+ setup-node（20，用于 sdk/ts typecheck）+ setup-python（3.11，用于 py_compile）。
2. `go build -o yanshi ./cmd/yanshi`。
3. **生成片段守门**（UDOC/APIREF）：
   - `go run ./cmd/api-schema -markdown docs/api/schema.md`
   - `go run ./cmd/api-schema -markdown docs/api/resources.md`
   - `go run ./cmd/gendocs -config docs/user-guide/configuration.md`
   - `go run ./cmd/gendocs -help-all ...`（写入 tui.md/entrypoints.md 区块）
   - `git diff --exit-code docs/api/ docs/user-guide/`（任一 diff → 失败，证明有人手改了生成区块或生成器/源不同步）。
4. **examples 检查**：
   - `go build ./examples/custom-tool`
   - TS：`npm --prefix sdk/ts ci && npx tsc --noEmit`（复用 `sdk/ts` toolchain 校验 examples/sdk-typescript）。
   - Python：`python -m py_compile examples/sdk-python/main.py` + `PYTHONPATH=sdk/python python -c "import yanshi_sdk"`（断言 SDK 包可 import，Advisory 2）。
   - headless 冒烟：`./yanshi exec --fake-model -p "hi"` 断言非空。
5. **ADR 可达**：复用 Task 3 Step 3 的 grep 脚本，断言每条 ADR 关联路径存在。
6. **归档无断链**：复用 Task 14 Step 3 的 grep，断言无指向旧路径的死链。
7. **配置一致性**（Advisory 4）：断言 `internal/config.Config`（含递归嵌套 struct）每个带 `yaml` tag 的导出字段，在生成的 `config-skeleton` 骨架表里**恰好一行**。实现为一个小 Go 测试（`cmd/gendocs` 或 `internal/config`）：`reflect` 遍历 `Config` 收集所有 `yaml:"<key>"` tag 为集合 A；解析 `docs/user-guide/configuration.md` 中 `<!-- BEGIN GENERATED: config-skeleton -->` 区块内每行的 key 列为集合 B；断言 `A == B`（双向：防"struct 有字段、骨架缺行"与"骨架有行、struct 已删字段"）。
8. **跨文档相对链接可达**（Advisory 3）：遍历 `docs/`（不含 `docs/archive/`）与 `CONTRIBUTING.md`，对每个 `](relative/path)` 形式的相对 .md 链接按所在文件目录解析，断言目标存在（broken relative paths → CI 红）。

- [ ] **Step 2: 本地全量演练（单脚本 = CI 全部检查）**

下面这个 bash 块**逐项对应** Step 1 的 CI 检查 1–8，在干净工作树上跑通即可证明 CI 全绿；任一步失败即对应 CI 红。复制即跑：

```bash
set -e

# --- 0. 构建 ---
go build -o yanshi ./cmd/yanshi
go build ./examples/custom-tool

# --- 1. 生成片段守门（Step 1.3）---
go run ./cmd/api-schema -markdown docs/api/schema.md
go run ./cmd/api-schema -markdown docs/api/resources.md
go run ./cmd/gendocs -config docs/user-guide/configuration.md
go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md
git diff --exit-code docs/api/ docs/user-guide/

# --- 2. ADR 可达性（Step 1.5）---
grep -rhoE 'internal/[a-z0-9/_-]+(/[a-z0-9_-]+\.go)?' docs/adr/ | sort -u | while read p; do
  [ -e "$p" ] || [ -n "$(ls "${p%/}"* 2>/dev/null)" ] || { echo "MISSING ADR link: $p"; exit 1; }
done

# --- 3. 归档无断链（Step 1.6）---
! grep -rE 'docs/(synthesis-final|synthesis-report|synthesis-report-v2|analysis-report|feature-comparison-with-codex)\.md' \
  --include='*.md' . | grep -v '^docs/archive/'

# --- 4. 跨文档相对链接可达（Step 1.8 / Advisory 3）---
find . -name '*.md' -not -path './docs/archive/*' | while read md; do
  dir=$(dirname "$md")
  grep -oE '\]\(([^)#]+\.md)(#[^)]*)?\)' "$md" | \
    sed -E 's/^\]\(//; s/\)$//' | sed -E 's/#.*$//' | sort -u | while read ref; do
      [ -z "$ref" ] && continue
      [ -f "$dir/$ref" ] || { echo "broken link in $md: $ref"; exit 1; }
  done
done

# --- 5. 配置一致性（Step 1.7 / Advisory 4）---
go test ./cmd/gendocs -run TestConfigSkeletonFieldsMatchStruct

# --- 6. headless 冒烟（Step 1.4）---
out=$(./yanshi exec --fake-model -p "hi"); [ -n "$out" ] || { echo "exec empty"; exit 1; }
./yanshi chat --no-tui --fake-model --input examples/headless-batch/sample.jsonl | grep -q . || { echo "batch empty"; exit 1; }

# --- 7. TS typecheck（Step 1.4）---
npm --prefix sdk/ts ci
npx tsc --noEmit
npx tsc --noEmit --project examples/sdk-typescript/tsconfig.json 2>/dev/null || \
  npx tsc --noEmit --strict --module NodeNext --moduleResolution NodeNext --target ES2022 \
    --paths '{"@x6nux/yanshi-sdk":["./sdk/ts/src/index.ts"]}' examples/sdk-typescript/index.ts

# --- 8. Python 解析 + import（Step 1.4 / Advisory 2）---
python -m py_compile examples/sdk-python/main.py
PYTHONPATH=sdk/python python -c "import yanshi_sdk"

echo "ALL DOCS-GATE CHECKS GREEN"
```

```bash
git add .github/workflows/docs.yml
git commit -m "ci(docs): gate generated snippets examples and adr reachability"
```

**验收：** workflow 在干净 checkout 上全绿；Step 2 的本地单脚本块逐项对应 CI、复制即跑即验证。故意改动生成区块 → CI 红（`git diff --exit-code`）；故意删 ADR 关联的代码路径 → CI 红；examples 不可编译 → CI 红；故意引入跨文档断链 → CI 红（Step 1.8 / Advisory 3）；config struct 加字段未重生成骨架 → CI 红（Step 1.7 / Advisory 4）；SDK 包不可 import → CI 红（Step 1.4 / Advisory 2）。CIG1 已就绪时本 job 挂入其矩阵，否则独立 workflow 兜底。

---

## 整批验收（对齐 spec §12）

1. `docs/user-guide/` 存在，getting-started 可零依赖（`--fake-model`）跑通；配置/命令为生成守门片段（Task 4–7、Task 15）。
2. `docs/api/` 存在，资源/事件/schema/jsonrpc/SDK(TS/Python) 各页与 `types.go`/`schema.go`/`rpc.go` 一致（Task 8–9、CI 生成守门）。
3. `docs/adr/` 存在，≥10 条 ADR 覆盖 synthesis §9 全部子节；模板与索引在（Task 3）。
4. `examples/` 存在，≥5（实际 7）示例 fake-model 友好可跑；CI 可编译/可冒烟通过（Task 10–12、Task 15）。
5. `CONTRIBUTING.md` 存在，覆盖架构约定与提交流程，指向 CLAUDE.md/ADR（Task 13）。
6. `docs/archive/` 存在，历史报告已 `git mv` 入内；`docs/` 根无断链（Task 14）。
7. CI 上所有生成片段 `git diff --exit-code` 通过（Task 15）。
8. **COORD 闭环**：G 落地 image 字段后，`resources.md` 由 CI 重生成（Task 8/15）；CIG1 就绪后 docs-gate 挂入其矩阵（Task 15）。

---

## 风险与缓解（汇总，对齐 spec §11）

| 风险 | 影响 | 缓解 |
|---|---|---|
| 文档漂移（最大风险） | 文档与 CLI/配置/协议脱节 | 生成驱动 + CI `git diff --exit-code`（Task 15） |
| 三处重复（README/UDOC1/CLAUDE.md） | 维护负担、不一致 | 受众切分 + 交叉引用不复制（Task 4–7/13） |
| ADR 与 CLAUDE.md 重复 | 双处维护 | ADR 引用不复制；独有价值 = 历史/superseded（Task 3） |
| 示例漂移到跑不通 | 用户照抄失败 | CI 可编译/可冒烟阻断（Task 10–12/15） |
| 归档移动断链 | 文档死链 | `git mv` + grep 验证 + 映射表（Task 14/15） |
| `cmd/api-schema -markdown` 与 TS 产物分叉 | 两份"真相" | 同源 `schemaDocument`/`SchemaBytes()`；CI 同时校验（Task 1/15） |
| G 未就绪 → APIREF 缺 image | 资源表不全 | COORD 行 + G 落地后 CI 重生成（Task 8/15） |
| CIG1 未就绪 → 守门无处挂 | 守门缺失 | H2 自带最小 workflow 兜底（Task 15） |
| custom-tool 暴露 internal API gap | 示例 hack 或不可编译 | 标 API gap 反馈后续，不临时导出（Task 12） |
