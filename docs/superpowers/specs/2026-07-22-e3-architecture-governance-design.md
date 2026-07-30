# Batch E3 — 架构治理测试（CI 门禁）设计

> **日期**：2026-07-22
> **归属**：E-H roadmap 的 Tier E（`docs/feature-roadmap-e-h.md` §5）
> **命题**：把 CLAUDE.md 与 `synthesis-final.md` 里"靠人工每 PR 检查"的架构承诺——依赖向内流动、单文件 1000 纯代码行、导出符号文档化——变成**可执行的 Go 测试**，让违规在本地 `go test` 与未来 CI（G1/CIG1）里被自动拦下。**建议最先做**：它保护 E/F/G/H 后续所有改动不再引入同类债。
> **范围**：只做**治理门禁**（测试 + 现有超长文件拆分），不加新功能面；不触碰 guard/store/proto/vcs 的运行时行为。
> **状态**：设计稿（基于 2026-07-22 代码实测），待用户审阅 → writing-plans。

---

## 1. 目标与非目标

### 目标

- **[GOV1]** 一个 Go 测试（`internal/archtest/`）解析 `go list -deps` 导入图，断言六边形分层：端口包（guard/store/proto/vcs）内部依赖在白名单内、无循环、`config` 不依赖 `guard`（W2）、bootstrap 是服务端唯一组合根；失败给**可读报告**（列出违反的边）。
- **[GOV2]** 一个零依赖纯代码行统计（去注释/空行，口径同 CLAUDE.md）入 `go test`；>1000 失败；并把当前超长的 `internal/tools/agent.go`、`internal/cli/tui/model.go`（实测均已超 1000）拆到合规，行为不变。
- **[GOV3]** 一个零依赖导出符号文档检查（基于 `go/parser`）；补齐 `tools`/`proto`/`config`/`mcp`/`skills` 等包缺失的 doc 注释。
- 三类门禁都是**普通 Go 测试**，随 `go test ./...` 跑；无新第三方依赖；G1/CIG1 只需把它们纳入矩阵。

### 非目标（本批不做）

- **不改运行时行为**：guard 的 fail-closed、orchestrator、VCS、压缩逻辑一行不动。
- **不引入 lint 框架**：仓库现无 `golangci-lint` 配置（实测无 `.golangci.*`、无 `Makefile`），本批用 `go/parser` + `go list` 自写零依赖检查，不引入 lint 依赖（与"Fake/零依赖优先"一致）。
- **不搭 CI workflow**：`.github/workflows/` 是 G1/CIG1 的活；本批只产出**可被 CI 调用**的测试与脚本，不写 YAML。
- **不修 W2**：`config → guard` 的修复（挪 `PermissionProfile` 类型）是行为相关的重构，本批只**用例外清单登记并持续拦截新增违规**，修复留作 GOV1 的 fast-follow（见 §11 open question 4）。

---

## 2. 背景（2026-07-22 代码实测）

> 以下数字均为本设计稿撰写当天的**直接测量值**（`go list -deps` + 两种独立纯代码行计数法，互证一致），非引用 07-20 `synthesis-final.md` 的快照。A-D 之后多个文件明显增长，旧快照已过期（见 §4 的 ws.go 例子）。

### 2.1 当前导入关系（`go list -deps`，按端口包聚合）

| 端口/底层包 | 直接内部依赖（实测） | 备注 |
|---|---|---|
| `internal/guard` | `execpolicy` | execpolicy 是零内部依赖的命令解析器，逻辑上在 guard 之下 |
| `internal/store` | `auth` | auth → secrets；store 持久化 auth_metadata |
| `internal/proto` | `pathjail`, `task/work` | task/work → pathjail |
| `internal/vcs` | `auth`, `execpolicy`, `guard`, `secrets`, `store` | 扇入最广的端口包 |
| `internal/config` | **`guard`** | **W2 违规**：`config.go:48` 用 `guard.PermissionProfile` 作 map 值类型 |
| `internal/execpolicy` | （无） | 真正的底层 |
| `internal/pathjail` | （无） | 真正的底层 |
| `internal/secrets` | （无） | 真正的底层 |
| `internal/approval` | （无） | 真正的底层 |

**无循环依赖**：`go list -deps ./...` 成功返回（Go 工具链在存在 import 环时直接报错）。

### 2.2 两个组合根（非"bootstrap 唯一全知"的字面解）

`go list -deps ./internal/bootstrap` 的内部依赖闭包为 **37 个**；仓库共 **50 个** internal 包。差出的 13 个是**客户端/子命令**侧，从 `cmd/yanshi/` 装配，不经 bootstrap：

| 不在 bootstrap 闭包的包 | 由谁装配（实测 grep） |
|---|---|
| `internal/cli`、`internal/cli/tui`、`internal/lockfile`、`internal/i18n`、`internal/keymap` | `cmd/yanshi/main.go`（TUI 客户端） |
| `internal/agent/goalloop` | `cmd/yanshi/main.go`（`yanshi goal` 子命令） |
| `internal/vcs/mcp` | `cmd/yanshi/main.go`（`yanshi vcs-mcp` 子命令） |
| `internal/appserver` | `cmd/yanshi/app.go`（`yanshi app` 子命令） |
| `internal/version` | `cmd/yanshi/version.go`、`internal/cli/tui/startup.go` |
| `internal/acp`、`internal/agent/worker`、`internal/plugin` | 经 goalloop/appserver 间接装配 |
| `internal/llm`（顶层，非 `llm/eino`） | 实测无人 import，疑似孤儿（见 §11） |

> **含义**：CLAUDE.md 称 bootstrap 为"唯一知晓所有 internal 包的包"是**服务端视角的理想化**。治理规则不能断言"bootstrap 扇出含全部 50 个 internal 包"（事实为假），而应断言"bootstrap 扇出含**定义好的服务端集合**，且不存在第三个组合根"（见 §3 设计 R4）。

### 2.3 纯代码行实测（两种独立方法互证）

口径 = CLAUDE.md"纯代码行（不含注释行和空行）"。实现用 `go/parser` 的 comment group 精确剔除（含 `//` 与 `/* */`）。设计稿阶段用两种 shell 启发式（awk 状态机 + 逐行分类）互证，**两者差 ≤1 行**：

| 文件 | 总行 | 纯代码行（实测） | 超限？ | 本批处理 |
|---|---|---|---|---|
| `internal/api/http/ws.go` | 2274 | **1385** | **是** | **见 open question 1** |
| `internal/tools/agent.go` | 1436 | **1134** | 是 | GOV2 拆分 |
| `internal/cli/tui/model.go` | 1631 | **1030** | 是 | GOV2 拆分 |
| `internal/vcs/vcs.go` | 1304 | 971 | 否（临界） | 监控，不拆 |
| `internal/cli/tui/commands.go` | 1257 | 925 | 否（临界） | 监控，不拆 |
| `internal/bootstrap/bootstrap.go` | 1131 | 724 | 否 | — |

> **关键偏差**：roadmap §2.2 / B0 称 `ws.go`"拆为 857 纯代码行不违规"。那是 **B0 时（07-21，1480 总行）的快照**；A-D 在 ws.go 上叠加了 cost/features/memory/fork/side/skills/mcp 等帧与权限交互，**现已 1385 纯代码行，明确超限**。但任务书指示"只动 agent.go 与 model.go"。这是本 spec 的头号 open question（§11-1）。

### 2.4 现有构建/CI 设施（实测）

- 无 `.github/workflows/`；无 `Makefile`；无 `.golangci.*`。
- `build.sh` 存在（ldflags 注入 `version.BuildStamp`，G2/PKG1 会扩展）。
- `cmd/testchanged` 存在（增量测试）。

---

## 3. [GOV1] 依赖分层治理测试  (P0 | 缺失 | synthesis S2/A23)

- **缺口**：六边形分层（端口包近零内部依赖、依赖向内流动、无循环、bootstrap 服务端唯一组合根、config 不依赖 guard）全靠人工每 PR 审查，无机器门禁。synthesis A23 把"架构治理测试"列为待办。
- **落点**：新 `internal/archtest/`（**选 test 包而非 `cmd/archtest` 二进制**——随 `go test ./...` 跑、零部署、CIG1 直接纳入矩阵；理由见风险①）。
  - `internal/archtest/deps_test.go`（分层规则）
  - `internal/archtest/helpers.go`（`go list -json -deps` 解析、找模块根、导入图构建）

### 设计

**数据源**：测试用 `os/exec` 调 `go list -json -deps ./...`（从模块根，即向上找 `go.mod` 的目录），解析每个包的 `Imports`，构建 `map[pkgPath][]internalDep`。**零第三方依赖**（`go list` 是工具链自带，JSON 输出稳定）。

**分层模型**（用实测数据定义，非凭空）：

```
L0 底层（零内部依赖）: execpolicy, pathjail, secrets, approval
L1 端口（近零内部依赖，受白名单约束）:
    guard   -> {execpolicy}
    store   -> {}              （当前 {auth} 为已知违规，登记为 tracked exception）
    proto   -> {pathjail, task/work}
    config  -> {}              （当前 {guard} 为 W2 已知违规，tracked exception）
    vcs     -> {auth, execpolicy, guard, secrets, store}
L2 领域服务: auth(->secrets), task/work(->pathjail), ctxcompact, llm/eino ...
L3 编排/工具: tools, agent/*, api/*, lsp, mcp, shell, sandbox ...
L4 服务端组合根: bootstrap
（独立）客户端/子命令根: cmd/yanshi（cli/tui/goalloop/vcs-mcp/appserver/version）
```

**规则（每条一条子测试，失败各自报可读原因）**：

| 规则 | 断言 | 当前状态 | 违反时的报告 |
|---|---|---|---|
| **R1 无环** | `go list -deps ./...` 退出码 0 | ✅ 通过 | "import cycle: a -> b -> a"（附 `go list` 原始 stderr） |
| **R2 端口包白名单** | 对每个端口包 P，`directInternalDeps(P) ⊆ allowList[P]` | guard✅ proto✅ vcs✅；store⚠(auth)、config⚠(guard) 为 tracked | "config imports guard (not in allowList=[]; tracked exception W2)" |
| **R3 W2：config 不依赖 guard** | `guard ∉ directDeps(config)` | ⚠ 违规（在 `exceptions` 内） | 列出 config.go:48 的 `guard.PermissionProfile` 引用 |
| **R4 服务端组合根唯一** | (a) bootstrap 的内部依赖闭包 ⊇ `serverSet`（定义见下）；(b) 任一 internal 包的内部扇出 > 阈值（如 18）⇒ 该包 ∈ {bootstrap}（即不允许 internal 内出现"第三个组合根"） | ✅（bootstrap 闭包 37 = serverSet；tools 扇出虽大但 < 阈值或单列白名单） | "package X fans out to N internal packages (third composition root?)" |
| **R5 方向** | 端口包不得 import L2+ 包（guard/store/proto/vcs 不得依赖 tools/agent/api/orchestrator/bootstrap） | ✅ | "guard -> tools (port must not depend on service layer)" |

**`serverSet` 定义**：bootstrap 当前闭包的 37 个包（实测），作为代码里的显式集合。新增服务端包时若忘了接进 bootstrap，R4(a) 会失败并提示"package X is used server-side but not in bootstrap's closure; wire it in `bootstrap.Build` or move to client set"。

**`exceptions`（tracked，必须收缩）**：一个显式 `map[string]string{edge: reason/ticket}`，初始含两条：
- `config -> guard`：W2，`config.go:48 PermissionProfile`；fast-follow 修复（§11-4）。
- `store -> auth`：待评估是否能下沉 auth_metadata 类型。

> **门禁语义**：规则对**目标态**断言；当前未达标项放进 `exceptions` 并标注 reason。任何**新增**违规（不在 exceptions 里）直接失败；exceptions 里的项被修复后必须从 map 删除（删了若仍违规则失败，防止"修了又退化"）。这样既不阻塞本批合并，又锁死债不再增长。

**判定逻辑要点**（writing-plans 阶段细化）：
- `directInternalDeps(P)` 只取 `Imports` 里以本模块路径为前缀的项（不含 `DepErrors`/间接）。
- 测试在 `TestMain` 里一次性构建导入图，各子测试共享（避免 N 次 `go list`）。
- 找模块根：从 `os.Getwd()` 向上找首个含 `go.mod` 的目录，作为 `go list` 的 `Dir`。

- **依赖**：- （独立；不依赖 E1/E2）。
- **风险**：
  1. `cmd/archtest` vs `internal/archtest`：选后者（test），随 `go test ./...` 免部署、CIG1 零成本纳入；代价是它在 `go test` 进程内 `exec` `go list`（CI 须有 Go 工具链——CIG1 本就跑 `go test`，满足）。
  2. `go list` 慢（全仓 ~1-2s）：`TestMain` 构建一次图、缓存；CI 上可接受。
  3. exceptions 沦为永久豁免：要求每条附 reason + 关联 ticket/issue；spec 验收要求 exceptions 数量**只降不升**。
  4. `serverSet` 漂移：把 37 个包列成代码常量会随重构过期；缓解——R4(a) 的真正断言是"bootstrap 闭包 ⊇ 一个手写最小服务端核心集合"，全集漂移靠 R4(b)（扇出阈值）兜底。
- **验收**：
  - `go test ./internal/archtest` 本地通过（含当前两条 tracked exceptions）。
  - 人为给 `config` 加一条 `import "internal/tools"` 的违规 → 测试失败并打印可读边。
  - `guard` 零非白名单内部依赖被锁定（从 `tools` 反向 import guard 会被 R5 拦）。
  - exceptions 初始清单 = {config→guard, store→auth}，每条有 reason。
- **预估**：2d（含 helpers + 5 条规则 + 可读报告）。

---

## 4. [GOV2] 文件纯代码行门禁 + 超长文件拆分  (P1 | 部分 | synthesis S6/W4/W5/A7/A17)

- **缺口**：CLAUDE.md"单文件 ≤1000 纯代码行"靠人工；`tools/agent.go`（实测 1134）、`cli/tui/model.go`（实测 1030）已超限（旧 synthesis 数字 ~900/~850 是 07-20 快照，过期）。
- **落点**：
  - `internal/archtest/lines_test.go`（门禁测试，`go/parser` 精确计纯代码行）。
  - 拆分（行为不变）：`internal/tools/agent.go` → 同包新文件；`internal/cli/tui/model.go` → 同包新文件。

### 4.1 纯代码行统计口径（与 CLAUDE.md 一致）

**定义**：纯代码行 = 含**至少一个非空白、非注释 token** 的行数。用 `go/parser` 解析每个 `.go`（排除 `_test.go`），取 `file.Comments` 的所有 comment group，把每个 comment 覆盖的行号区间标记为"注释行"；空行（仅空白）也排除；其余行计数。这精确处理 `//` 与 `/* */`、行尾注释、块注释跨行——比 shell 启发式更准，且零依赖（标准库 `go/parser`）。

**门禁**：对 `internal/`、`cmd/` 下所有非测试 `.go`，纯代码行 ≤ 1000。超过则测试失败，报告形如 `internal/tools/agent.go: 1134 pure code lines (limit 1000) — split required`。

**`exceptions`（grandfather，tracked）**：初始**建议为空**（见 open question 1）；若决定本批不拆 ws.go，则初始含 `internal/api/http/ws.go: 1385 (tracked, split deferred)`。与 GOV1 同语义：只降不升。

### 4.2 拆分映射（行为不变；同包新文件，不改 import path / 不改签名）

**`internal/tools/agent.go`（1134 → 拆 4 文件，均 < 1000）**——天然沿顶层声明切，零风险：

| 新文件 | 迁入的顶层声明（行号区间为现 agent.go） | 预估纯行 |
|---|---|---|
| `agent.go`（保留） | `AgentTools`/`NewAgentTools`/`Tools`/`streamReviewTool`/`agentStartArgs`/`streamStartAgent`/`runSubAgent` + 小工具（`bindSubAgentProgress`/`naturalIDLess`/`digitsStart`/`formatDur`/`formatTokens`/`parseToolList`） | ~450 |
| `agent_analysis.go` | `analysisArgs`/`streamAnalysis`/`runAnalysisWorkflow`/`fillWorkflowTarget`/`generateAnalysisWorkflow`（379–450, 692–810） | ~200 |
| `agent_workflow.go` | `makeWorkflowProgress`/`streamStartWorkflow`/`summarizeArgs`/`streamSummarize`/`runStartWorkflow`/`runFlatWorkflow`/`workflowStartArgs`/`WorkflowProgress` 相关（451–691, 786–948） | ~350 |
| `agent_dag.go` | DAG 引擎：`WorkflowDef`/`WorkflowStepDef`/`ExpandedStep`/`stepState`/`dagResult`/`workflowTaskResult`/`runDAGWorkflow`/`executeLevel` + range 拓扑（`rangeRegex`/`expandStepID`/`expandSteps`/`expandDeps`/`resolveDeps`/`topoSortLevels`/`interpolatePrompt`，949–1420） | ~480 |

> 这些全是**顶层 `func`/`type`/`var`**，在**同一 package `tools`** 内移到新文件——零签名变化、零 import path 变化、私有字段仍同包可见。回归保障：拆分**前后**各跑一次 `go test ./internal/tools ./internal/agent/...`，全绿即行为不变。

**`internal/cli/tui/model.go`（1030 → 拆，均 < 1000）**——**比 agent.go 风险高**：其大头不是顶层声明，而是 `Update` 方法本身（行 567–1142，**单个方法 ~575 行的巨型 switch**，含 ~60 个 `case`）。不能简单"移声明"，需 **extract method**：

| 新文件 | 内容 | 手法 |
|---|---|---|
| `model.go`（保留） | `model` 结构体、`tuiSession`、`newModel`、`NewProgram`、`Init`、`Update`（瘦身为**调度器**：每个顶层 `case` 调一个 handler 方法） | 重构 Update switch 体 |
| `handlers.go` | 从 `Update` 抽出的各 `case` 分支 → `(m model) handleKeyMsg(...) / handleStreamMsg(...) / handleWindowSizeMsg(...)` 等方法 + `applyEvent`/`submit`/`waitForEvent`/`streamMsg` | extract method（行为等价） |
| `state.go` | `QueueMode` 及方法、`pickerItem`/`pendingSeamRestoreState`/`pickerConfirm`、`defaultBundle`/`dirName`/`detectGitBranch`/`parseGitHead`/`fetchInitialStatus`/`syncSavedMode` | 整声明迁移 |

> **extract method 必须严格行为等价**：每个 `case` 原地逻辑原样搬到新方法，`m` 与所需局部变量作参数传入，返回 `(model, tea.Cmd)` 与原 switch 臂一致。回归保障更强：除 `go test ./internal/cli/tui` 外，**拆分前后对同一组 fake 事件序列断言渲染/状态快照一致**（tui 已有 `model_test.go`/`view_test.go`，必要时加 golden）。这是本批**唯一有回归风险**的步骤，writing-plans 应单独成一个 Task 并强制 GREEN。

- **依赖**：- （独立）。
- **风险**：
  1. model.go 的 extract method 引入微妙行为差异（return 早退、Cmd 合并顺序）→ 强制 golden/事件序列对比 + 拆分前后测试全绿；不确定的臂**不拆**（保留在 model.go，反正总量已降）。
  2. 纯代码行计数与开发者手感不一致（块注释边界）→ 用 `go/parser` 权威口径，并在 `archtest` 里暴露一个 `go test -run TestLineCount -v` 的可读逐文件报告，供人核对。
  3. ws.go 已超限但任务书说不拆 → open question 1；门禁须 grandfather 它，否则一上线就红。
  4. vcs.go(971)/commands.go(925) 临界 → 不拆，但在报告里标"approaching limit"提醒。
- **验收**：
  - `go test ./internal/archtest -run Lines` 通过；agent.go/model.go 拆后纯代码行均 < 1000。
  - 拆分前后 `go test ./...` 全绿（重点 `internal/tools`、`internal/cli/tui`、`internal/agent/...`）。
  - 人为给某文件堆到 1001 行 → 门禁失败并指名文件。
  - exceptions 清单（ws.go 是否在内）与 open question 1 的决策一致。
- **预估**：门禁 0.5d + agent.go 拆 0.5d + model.go 拆 1d（extract method 较重）= **2d**。

---

## 5. [GOV3] exported symbol 文档覆盖  (P2 | 缺失 | synthesis A26)

- **缺口**：CLAUDE.md"注释是承重文档"约定无机器保证；synthesis A26 称约 6 个包有未文档化导出符号。本设计稿用 shell 启发式（`^(func|type|var|const) [A-Z]` 前一行非注释）**实测**缺口集中在：

  | 包 | 启发式未文档化导出数（含方法会更多） |
  |---|---|
  | `internal/tools` | 29 |
  | `internal/proto` | 13 |
  | `internal/config` | 7 |
  | `internal/mcp` | 4 |
  | `internal/skills` | 3 |
  | `internal/cli/tui` | 2 |
  | `internal/secrets` / `internal/guard` | 各 1 |

  > 启发式只扫顶层、漏方法，真实缺口（Go 语义下"导出标识符 = 首字母大写的 func/type/var/const/方法/字段"）由 `go/parser` 权威扫描产出清单。
- **落点**：
  - `internal/archtest/docs_test.go`（`go/parser` 扫描）。
  - 补注释：上述包的缺失导出符号（实现期由扫描输出驱动精确清单）。

### 设计

**检查规则**：用 `go/parser` 解析每个非测试 `.go`，遍历 AST：
- 导出标识符 = `ast.FuncDecl`/`ast.GenDecl`(`VAR`/`CONST`/`TYPE`)/`ast.Field`（导出结构体字段）且名字首字母大写。
- 每个导出标识符必须有**直接前置 doc 注释**（`ast.CommentGroup` 关联，即 Go vet `doc` 系列 / golint `exported` 规则的同一口径）。
- 包级：每个包要有 package doc 注释（`ast.File.Doc` 且位于 `package` 子句上方）。

**例外（`exceptions`，避免噪声）**：
- `*_test.go` 全部排除。
- `main` 包（`cmd/yanshi`）的导出符号不要求（程序入口）。
- 实现/生成的文件（如 `internal/version` 的生成字段、`cmd/api-schema` 产物）按需白名单。
- 测试 helper 包的导出若仅包内用，可列入 exceptions。

**输出**：失败时按包列出未文档化导出符号，形如 `internal/proto/frame.go:42: exported func SSEEvent lacks doc comment`。

**补注释原则**：与 CLAUDE.md"承重文档、解释 why"一致——尤其 guard/adk/proto/vcs 周围保持现有注释密度；新注释至少一句"这个符号做什么、为什么存在"，不写无信息量的"`// Foo is a Foo`"。

- **依赖**：- （独立）。
- **风险**：
  1. 选型：`golangci-lint` 的 `revive.exported` 能做，但仓库无 lint 配置、引入框架违反"零依赖优先"→ 用 `go/parser` 自写（~120 行），与 GOV1/GOV2 同栈。
  2. 一次性补注释量大（tools/proto 主力）→ 分包提交，每包一个 Task；例外清单兜底噪声。
  3. 字段/方法误报 → 精确按 `ast` 关联 doc 判定，与 `go vet` 口径对齐。
- **验收**：
  - `go test ./internal/archtest -run Docs` 通过（exceptions 之外的导出符号全有 doc）。
  - 关键包（guard/proto/config/tools）导出符号文档化；package doc 齐全。
  - 人为加一个无注释导出函数 → 测试失败指名。
- **预估**：检查器 0.5d + 补注释（tools/proto/config/mcp/skills 等）1–1.5d = **1.5–2d**。

---

## 6. 文件结构

| 文件 | 职责 | 新/改 |
|---|---|---|
| `internal/archtest/helpers.go` | 找模块根、`go list -json -deps` 解析、构建导入图、`go/parser` 纯代码行计数、doc 扫描公共工具 | 新 |
| `internal/archtest/deps_test.go` | [GOV1] R1–R5 分层规则 + `exceptions`（config→guard / store→auth） | 新 |
| `internal/archtest/lines_test.go` | [GOV2] 纯代码行 ≤1000 门禁 + `exceptions`（ws.go 视决策） | 新 |
| `internal/archtest/docs_test.go` | [GOV3] 导出符号 doc 覆盖 + `exceptions` | 新 |
| `internal/tools/agent.go` | 保留 agent 核心与生命周期；删去迁出声明 | 改（瘦身后 ~450 纯行） |
| `internal/tools/agent_analysis.go` | analysis 工具与模板生成 | 新 |
| `internal/tools/agent_workflow.go` | workflow 启动/扁平执行/summarize | 新 |
| `internal/tools/agent_dag.go` | DAG 引擎 + range 拓扑展开 | 新 |
| `internal/cli/tui/model.go` | 保留结构体/构造/Init/Update 调度器；瘦身 | 改（瘦身后 < 1000） |
| `internal/cli/tui/handlers.go` | 从 `Update` 抽出的各 handler 方法 + applyEvent/submit | 新 |
| `internal/cli/tui/state.go` | QueueMode/picker/pending/git 检测等状态与辅助 | 新 |
| 上述包若干 `.go` | 补导出符号 doc 注释（tools/proto/config/mcp/skills 等） | 改 |

> `internal/archtest/` 是纯测试包（`package archtest`，文件皆 `_test.go` + 一个非测试 `helpers.go` 也可放测试里），不进任何运行时依赖图，对 bootstrap 扇出零影响。

---

## 7. 测试策略

- **archtest 自测**：用 `t.TempDir()` 构造**最小合成模块**（几个含已知 import 边/超长行/无注释导出的 `.go`），跑分层/行数/doc 三组检查，断言通过/失败与可读报告各按预期。这是"测测试"——保证门禁本身正确。
- **GOV1**：合成模块含一条违规边（如 fake `config` import fake `tools`）→ 失败并报边；含一条在白名单的边 → 通过。
- **GOV2**：合成模块含一个 1001 纯行文件 → 失败；含一个 999 行 → 通过；块注释/行尾注释计数正确。
- **GOV3**：合成模块含无 doc 导出函数 → 失败指名；有 doc → 通过。
- **拆分回归（GOV2 的核心保障）**：
  - agent.go：拆分前后 `go test ./internal/tools ./internal/agent/...` 全绿（这些包已有 agent/workflow/DAG 的测试）。
  - model.go：拆分前后 `go test ./internal/cli/tui` 全绿；新增/复用 golden 对一组 fake `tea.Msg` 序列的渲染与状态快照，断言 extract method 前后逐像素/逐状态一致。
- **全量**：`go test ./...` 与 `go vet ./...` 在拆分与补注释后均通过。

---

## 8. 风险与缓解

| 风险 | 缓解 |
|---|---|
| ws.go 已超限但任务书指示不拆 → 门禁一上线即红 | open question 1 决策：要么本批一并拆 ws.go（推荐，见 §11），要么 grandfather 进 exceptions（只降不升） |
| model.go 的 extract method 引入行为差异 | 强制 golden/事件序列对比 + 拆分前后测试全绿；不确定的 case 臂不拆；单独 Task + GREEN 门 |
| `go/parser` 行数/doc 口径与开发者直觉不符 | archtest 暴露 `-v` 逐文件报告供核对；口径写进 CLAUDE.md 与 CONTRIBUTING |
| `serverSet`/exceptions 随重构漂移成永久豁免 | exceptions 每条带 reason + ticket；验收要求数量只降不升；R4(b) 扇出阈值兜底"第三组合根" |
| 补注释量大致冲突/延后 | 分包提交、每包一 Task；exceptions 兜底噪声；优先 guard/proto/tools 等承重包 |
| G1/CIG1 尚未搭 CI，门禁暂只本地跑 | 本批产出即"CI-ready 测试"；CIG1 纳入矩阵即可（不阻塞本批验收） |
| `internal/llm` 顶层包疑似无人 import（孤儿） | GOV1 R4 报告里标"unreachable from both roots"；清理留作 GOV1 fast-follow（非本批硬要求） |

---

## 9. 验收标准

1. `go test ./internal/archtest` 通过（含 GOV1 的 2 条 tracked exceptions、GOV2 的 ws.go 视决策、GOV3 的有限 exceptions）。
2. **GOV1**：5 条规则可执行；人为加 `config → tools` 违规边 → 失败并打印可读边；`guard` 零非白名单内部依赖被锁定；无循环被验证。
3. **GOV2**：`agent.go`/`model.go` 拆后纯代码行均 < 1000；拆分前后 `go test ./...` 全绿；人为堆 1001 行 → 门禁失败指名文件。
4. **GOV3**：exceptions 之外导出符号全有 doc；package doc 齐全；关键包（guard/proto/config/tools）文档化。
5. 三类门禁零第三方依赖（仅标准库 + `go list`/`go/parser`）。
6. exceptions 清单初始明确、每条带 reason；数量只降不升写入验收。
7. `go vet ./...` 通过。

---

## 10. out-of-scope（非本批）

- **修 W2**（`config → guard`）：挪 `PermissionProfile` 类型是行为相关重构，本批只登记、不修（fast-follow）。
- **拆 ws.go**：除非 open question 1 决策为"本批拆"，否则只 grandfather。
- **CI workflow YAML**：归 G1/CIG1。
- **lint 框架引入**：明确不做。
- **vcs.go(971)/commands.go(925) 拆分**：未超限，不拆；报告里提示监控。
- **清理 `internal/llm` 孤儿包**：登记、非硬要求。
- **doc 注释的 i18n**：注释保持与现有代码一致（多为英文，与代码同语言）。

---

## 11. 需人决策的 open question

> **已决策（2026-07-22，实施计划锁定）：**
> 1. ws.go → **本批一并拆（option a）** — 拆为 ws.go（保留 connSession + ChatWS）、ws_perm.go、ws_handlers.go、ws_compaction.go。GOV2 初始 exceptions 为空。
> 2. model.go → **接受本批 extract-method + golden 守门** — 先移 state.go 降行数至 <1000，再抽 handlers.go 并用 golden 快照守门行为不变。
> 3. serverSet → **手写最小核心集合 ⊆ bootstrap 闭包（option b）+ R4(b) 扇出阈值兜底** — serverCoreSet 列 16 包（store/vcs/guard/proto/config/secrets/execpolicy/pathjail/approval/auth/tools/agent/orchestrator/ctxcompact/skills/mcp），不在 bootstrap 闭包内的包（cli/tui/lockfile/goalloop/acp/version 等客户端包）明确排除。R4(b) 阈值 25，tools 豁免。
> 4. W2 → **不纳入本批 fast-follow**，留 P3；GOV1 仅登记 exception（config→guard）。
> 5. GOV3 → **统一"至少一句"**（一行起），承重包严、工具包宽松。最终 docExceptionPkgs = {cmd/yanshi}，docExceptionSymbols = {}。
>

1. **ws.go 已 1385 纯代码行（实测），超限，但任务书指示"只动 agent.go 与 model.go"。** 三选一：
   - (a) **本批一并拆 ws.go**（推荐）：按职责再拆 `api/http/`（已拆 7 文件，再把 ws.go 的帧处理/权限交互/管理帧/流式压缩分到 `ws_frames.go`/`ws_perm.go`/`ws_admin.go` 等），让 GOV2 门禁初始 exceptions 为空、最干净。
   - (b) **grandfather**：`exceptions = {ws.go: 1385, split deferred to follow-up}`，门禁初始即带一条，后续单独拆。
   - (c) **放宽阈值**（不推荐）：与 CLAUDE.md 白纸黑字冲突。
   → **需决策**：本批是否拆 ws.go？若不拆，确认走 (b)。

2. **model.go 拆分的回归风险**：`Update` 是 ~575 行单方法 switch，extract method 比 agent.go 的"移声明"风险高。是否接受本批对 model.go 做 extract-method 重构？还是本批只拆 agent.go + 把 model.go 也 grandfather（推迟到独立 PR）？

3. **`serverSet` 的定义方式**：R4(a) 用"bootstrap 当前闭包的 37 包"作显式常量会随重构过期。是否改为"手写最小服务端核心集合 ⊆ bootstrap 闭包"（更稳但需人维护核心集合）？或只保留 R4(b) 扇出阈值、放弃 R4(a)？

4. **W2 修复是否纳入本批 fast-follow**：`config.go:48` 把 `guard.PermissionProfile` 改为 config 本地类型 + bootstrap 转换，是小幅行为相关改动。是否在 E3 内带一个独立 Task 修掉（让 exceptions 当场归零），还是留给后续？

5. **GOV3 补注释的深度**：是否要求每个导出符号都满足"解释 why"（承重包如 guard/proto 严，工具包可宽松一句），还是统一"至少一句"？影响工作量与 exceptions 规模。
