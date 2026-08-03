# Yanshi 补全路线图设计：从 32% 可用率到「个人助手全家桶 + 开发者 coding agent」

> **日期**：2026-08-03 ｜ **状态**：设计已批准，待转 implementation plan
> **基线**：`docs/feature-status-audit.md`（2026-07-31 机器化审计，94 项，32% 端到端可用）
> **本文范围**：7 个子项目的分解与顺序 + **S0（断线修复）的完整设计**。S1–S4c 只定边界与依赖，各自的详细设计在其启动时另立 spec。

---

## 1. 目标与定位

把 yanshi 从「开发者向 coding agent 服务端」扩展为**个人助手全家桶 + 开发者 coding 专业 agent**，并作为**开源产品对外发布**。

对标参考 `reference/QwenPaw`（AgentScope 出品的 Python 个人助手）。yanshi 不抄它的形态，而是把自己已有但只露了一角的资产（autoVCS 编辑级追踪、六维 fail-closed guard、LSP 客户端、ACP 外部 agent 接入）放大成差异化能力。

### 1.1 前置事实

审计给出的核心结论是：**规划的 94 项里只有 12 项是真没写，主导失效模式是「零件造好了，总装线没接上」**——包写完、测试全绿、却没有任何生产调用点。

2026-08-03 的复核确认，审计第 8 章的 20 条优先修复项**只落地了 3 条**（P0-1 `shell/factory.go` 的 `Wait` 字段、P0-3 goreleaser `changelog` 段、P0-4 `WithChatModelOptions` 合并），其余 17 条原样。复核还发现一条审计未记录的新问题：`internal/bootstrap/bootstrap.go:629-630` 把 shell v2 的九个工具名写进了 profile allow 列表，**但这些工具从未注册**——授权了一批不存在的工具。

---

## 2. 贯穿约束（不可违反）

| # | 约束 | 理由 |
|---|---|---|
| 1 | **单二进制** | Web UI 走 `embed.FS`，浏览器走纯 Go CDP（`chromedp`/`go-rod`），IM 走 Go SDK。任何引入运行时外部依赖的方案直接否决——「下一个文件就能跑」是 yanshi 的身份 |
| 2 | **六边形分层不破** | 新子系统一律新建 `internal/<name>` 端口包，只在 `bootstrap.Build` 装配；跨包依赖前先改 `internal/archtest` 的 `portAllowlists`，改不动说明设计错了 |
| 3 | **一切经 v1 service 层** | Web UI、IM 渠道、TUI 一律通过 `internal/api/v1` 获得能力，**禁止直连 orchestrator**。这是双 UI（TUI + Web IDE）与多渠道不分叉的唯一保证 |
| 4 | **开源产品标准** | 每个子项目交付时必须同时具备：`-h` 可见、config 可配、`docs/` 有页、CI 有门禁、SDK/schema 同步 |

---

## 3. 子项目分解（7 个）

| ID | 名称 | 交付边界 | 依赖 |
|---|---|---|---|
| **S0** | 断线修复 | 审计 64 项中的 63 项推到「已实现」（S08 移出）+ W0 治理断言常绿 | — |
| **S1** | 真沙箱 | `internal/sandbox` 从 Phase 0 骨架升级为三平台 OS 级强制（macOS Seatbelt / Linux Landlock+seccomp / Windows AppContainer）；`CapabilityReport.Effective` 不再恒为 `DegradedHostGuard`；`doctor` 的 `checkSandbox` 硬编码占位替换为真实探测。**含审计项 `A1 S08`** | S0 的 W1、W3、W5 |
| **S2** | 浏览器自动化 | 新端口包 `internal/browser` + `browser_*` 工具；跑在 S1 沙箱内；页面内容进 prompt 前经提示注入防护 | S1 |
| **S3** | 渠道抽象 + Telegram | **先做 `Channel` 端口抽象并把 TUI/WS 重构成它的实现**，再接 Telegram bot 单渠道 | S1、S0 的 W1 |
| **S4a** | Web Console + Coding IDE | `embed.FS`；聊天/设置/技能/渠道管理 + Monaco IDE（文件树、多文件 diff、**autoVCS 编辑时间线**、LSP 桥接） | S3 的入口抽象 |
| **S4b** | 技能生态 | GitHub 安装 + 技能市场 + 安全扫描 + 内置技能包（文档处理等） | S4a（管理界面） |
| **S4c** | 记忆增强 | MEM1 升级为可搜索/可链接的 Markdown 知识库 + 对话自动提炼 | 无（可与 S4a/b 并行） |

### 3.1 顺序决定的理由

顺序是 **S0 → S1 → S2 → S3 → S4a/b/c**。

S1 排在 S2/S3 之前是**安全决定，不可调换**：

- IM 渠道让攻击面从「你自己」变成「互联网上任何人」。开源发布后，任何人给 bot 发一条含提示注入的消息就直接落到 agent 的工具执行上。当前 `internal/sandbox` 是 Phase 0——只有接口骨架、零 OS 级强制，各 adapter 诚实上报 `DegradedHostGuard`，挡在前面的只有 guard 的词法分析。
- 浏览器自动化同理：Chrome 子进程 + 页面注入脚本 + 从网页读回的内容进 prompt，是经典的提示注入通道。

S2 排在 S3 之前，是因为浏览器只面向使用者本人，风险低于 IM，可以当沙箱的第一个真实压力测试对象。

### 3.2 关键设计决定

**① S3 先做抽象再做渠道。** IM 是「第 N 个入口」。先定义 `Channel` 端口，把 TUI/WS 重构成它的实现，再往上加 Telegram。做对了每加一个渠道几百行；做错了每个渠道都是一次手术。

`Channel` 端口必须包含**权限询问的三档能力声明**：

| 档位 | 语义 | 示例 |
|---|---|---|
| `Interactive` | 能在渠道内完成一次授权往返 | TUI（Shift+Tab 面板）、**Telegram（inline keyboard 的 `[允许][拒绝][本会话都允许]`）** |
| `Preauthorized` | 无法交互，只能按静态 profile 判定 | 邮件、Webhook、SSE |
| `ReadOnly` | 只能读，任何写操作直接拒 | 广播型渠道 |

**先接 Telegram 而非弱渠道是有意的**：它能做交互式授权，会把抽象压到正确形状；如果首个渠道只能 fail-closed，抽象会被设计成「IM 一律无授权」，后面再改就是返工。

**② Web IDE 打差异化牌，不抄 QwenPaw。** 两个现成资产直接接：

| 资产 | 在 Web IDE 里变成什么 |
|---|---|
| `internal/vcs`（autoVCS，SQLite，**编辑级**自动追踪，非 git 提交粒度） | 编辑时间线：逐条回放 agent 改了什么、何时改、属于哪个 turn；配合 `revert_turn` 做可视化按 turn 回滚。QwenPaw 的影子 git checkpoint 只到快照粒度 |
| `internal/lsp`（已有 LSP 客户端，gopls/pyright） | Monaco 原生说 LSP 协议，桥接现有 client 即得跳转定义/查找引用/诊断，**不重写** |

**③ TUI 与 Web IDE 双主力对等。** 已知成本：S0 的 W8（TUI 体验 8 项）照做，且此后每个新功能要做两遍 UI。这是产品决策，作为已接受的成本记录在案。约束 3（一切经 v1 service 层）是防止双 UI 分叉的唯一机制。

**④ VS Code 扩展以移除方式结案。** 审计项 `D2 O12` 判「未实现」（`runWithRecovery` 从未被 `extension.ts` import，README 在描述不存在的能力）。既已确定 TUI + Web IDE 双主力，第三个门面受众重叠、收益不足。处置：删除 `ide/vscode/`、删除 `scripts/check-d2.sh`、清理文档中对它的描述。

### 3.3 明确的非目标（YAGNI）

- **不做**多渠道广覆盖——S3 只交付 Telegram；第二个渠道另立子项目，由 `Channel` 抽象是否经得住检验来决定
- **不做**内置 llama.cpp 本地推理——`openai` kind + 自定义 `base_url` 已覆盖 Ollama / LM Studio / DeepSeek / OpenRouter / SiliconFlow 等 OpenAI 兼容端点
- **不做**插件系统真实化——已定「全部树内 Go」，`internal/plugin` 的 `Connector` 骨架维持现状或删除
- **不做** Tauri 桌面端——Web Console 已覆盖
- **不做** A2A 协议——QwenPaw 那边也只是个残桩
- **不做** VS Code 扩展——见 3.2 ④

### 3.4 低成本可选项（不占主线，任何阶段顺手做）

- **Gemini 原生 provider kind**——唯一不兼容 OpenAI 协议的主流 provider，其余靠 `base_url` 覆盖
- Docker / Compose 打包
- cloudflared tunnel（公网暴露）

---

## 4. S0 详细设计

### 4.1 完成定义

审计共 **64 项**（50 部分实现 + 12 未实现 + 2 有差别）。其中 `A1 S08 — OS 级 Sandbox` 的内容等同于整个 S1 子项目，从 S0 移出，成为 S1 的验收项。

**S0 完成 = 63 项状态台账全部处于终态（`done` 或 `removed`）+ evidence 全部可验证 + W0 治理断言全绿。**

终态有两档：`done`（做完了）与 `removed`（以移除方式结案，见 §5.1）。O12 的终态必然是 `removed`，所以完成条件不能写成「全 `done`」——那样永远不可能成立。

### 4.2 W0：防复发治理断言（阻塞全部，最先做）

修完 63 项只是补洞，**失效模式本身没被消除**。本次审计花了 118 个子代理、1300 万 token、4.3 小时——这个成本不可持续，且下次一定会再发生。W0 把「重跑一次审计」变成 `go test ./internal/archtest`。

现有 `internal/archtest` 已有三条治理规则（GOV1 分层依赖 / GOV2 单文件 ≤1000 纯代码行 / GOV3 导出符号 doc 注释），全部仅用 stdlib（`go/ast`、`go/parser`、`go list` via `os/exec`）。W0 沿用同一套 helper，不引新依赖。

#### GOV4 — 装配可达性（静态）

- **落点**：`internal/archtest/assembly_test.go`
- **规则**：`internal/bootstrap` 包内每个导出的 `Build*` 函数，必须在从 `Build` 出发的**传递调用图**内可达
- **实现**：用现有 `helpers_test.go` 的 AST 解析构建包内调用图 → 从 `Build` 做可达性 BFS → 未命中的导出 `Build*` 报错
- **本次抓到**：`BuildC1`（审计 P0-2）。`BuildRLM` / `BuildAutomation` 经 `BuildC1` 传递可达，修好一处三个同时绿——这正是用「传递可达」而非「直接调用」的原因
- **豁免**：`assemblyExceptions` map

#### GOV5 — 工具注册一致性（运行时）

初稿设想静态分析，验证后**不可行**：`allTools` 是命令式 `append` 构造（`bootstrap.go:588-761`），工具名藏在 `tools.NewTestRunTool()` 这类构造函数内部，AST 拿不到。

- **落点**：`internal/bootstrap/wiring_test.go`（包内集成测试，非 archtest）
- **规则**：用 `--fake-model` 跑一次完整 `Build`，从 `App` 取出实际注册的工具名集合（经 `Tool.Info()`），与 `guard.ToolsPerm.Allow` 里的非通配名双向比对
  - **allow 有、注册表无** → 硬失败（授权了不存在的工具）
  - **注册表有、任何 profile 都未 allow** → 报 warning 并列出（收紧型 profile 是合法的，不硬失败）
- **本次抓到 9 条**（审计 P1-7）：`shell_start` / `shell_read` / `shell_write_stdin` / `shell_wait` / `shell_cancel`，**加上** `bootstrap.go:630` 的 `task_shell_start` / `task_shell_wait` / `task_shell_stdin` / `task_shell_cancel`——`NewShellV2Tools` 全仓非测试零调用，九个名字全部是「allow 有、注册表无」。**这是审计本身都没发现的问题**，由 2026-08-03 复核新查出。GOV5 首次运行的预期命中数是 **9**，W1 的注册范围也须覆盖这 9 个而非 5 个
- **附带收益**：直接推进审计项 `E1 COV3`（bootstrap 集成测试 23% → 50%+），一份代码销两个账
- **豁免**：`toolWiringExceptions` map（同款只减不增，与 `assemblyExceptions` / `ctxInjectExceptions` 并列的第三张表）
- **需要的生产代码改动**：`App` 上加一个导出的工具名访问器（或让 `Orch` 暴露只读工具列表）。这是 W0 唯一新增的生产 API

#### GOV6 — context 注入闭环（静态）

- **落点**：`internal/archtest/ctxinject_test.go`
- **规则**：任何签名形如 `func With<X>(ctx context.Context, ...) context.Context` 的导出函数，必须在**非测试代码**中至少有一个调用点
- **实现**：AST 扫全 `internal/` 找符合签名的声明 → 扫全部非 `_test.go` 文件找调用 → 零调用点报错
- **本次抓到 2 条**（实测：全仓 38 个符合签名的函数，零非测试调用点的有两个）：
  1. `registry.WithRole`（审计 P0-5）。消费侧（`orchestrator.go:717-724` 的 `RoleFromContext` → `PromptPrefix` + `WithRolePolicy`）代码齐全，但 role 恒为空串导致整条链空转——典型的「只有 getter 没有 setter」。归属 **W1**
  2. `orchestrator.WithTurnRecorder`（`completion.go:78`）。doc 注释写着「Callers (ws.go's turn loop)」，但 `ws.go` 实际用的是 `WithNewTurnRecorder`。**这一项不在 63 项台账内、也不属于任何工作包**——W0 落地时须判定：若确认是死代码则删除，若是预留 API 则进豁免表并写明理由。不允许沉默放过
- **豁免**：`ctxInjectExceptions` map。实测误报仅 2 条，远低于 §7 风险表里「>5 条则收窄规则」的阈值，**规则不需要收窄**

#### 三条断言的共同设计

| 属性 | 取值 | 理由 |
|---|---|---|
| 依赖 | 仅 stdlib | 与现有 archtest 一致，避免 import cycle |
| 豁免语义 | map 条目**只减不增** + 死条目失败 | 与 GOV2/GOV3 完全同款，维护者不用学新规则 |
| CI 位置 | 主 `test` job 的 `go test ./...` | archtest 在 `go list ./...` 范围内自动硬跑；**不放进 `governance` 软门禁 job** |
| 失败信息 | 必须给 `file:line` + 修复方向 | 治理测试红了要能自解释，否则会被 `// nolint` 掉 |

#### W0 顺带修掉

删除 `.github/workflows/ci.yml:116` 与 `:138` 的 `continue-on-error: true`（审计 P2-14）。注释写着「soft until E3/E2 lands」，但 E3/E2 资产早已落地全绿。

#### W0 的 CI 策略

W0 落地时把**已知违规写进豁免 map**（每条带 TODO + 对应工作包编号），每修一个工作包删掉相应条目。这样 CI 始终绿，**豁免表长度就是剩余债务的计数器**。

### 4.3 W1–W10：63 项的工作包映射

聚类依据：审计第 2 章的「跨批次系统性发现」已证明**根因是跨批次的**——`G02`/`TD1`/`LEAK3` 三个编号其实是同一个 token 预算问题；`VISION` 与 `VISION-TOOL` 是一对；`P0-2`/`P0-5`/`P1-6`/`P1-7`/`P1-8` 五条全是「bootstrap 没接线」。按审计编号逐条修会把同一片代码来回改 3–5 次。

| 包 | 项数 | 覆盖审计项 | 交付内容 |
|---|---:|---|---|
| **W1** 装配线 | 9 | AU1, M07, RLM1, T07/T08, M05, M04b, VISION, VISION-TOOL, G05 | `bootstrap.Build` 调用 `BuildC1`；shell v2 **九个**工具注册（含 `task_shell_*` 四个，见 §4.2 GOV5）+ `shell.Manager` 的 `Config.Factory` 填值；`runAgentLoop` 派生 ctx 时绑 `registry.WithRole`；`ApplyImages` 接进 WS `runUserTurn` 与 v1 `runTurn` 两条 turn 路径、`TurnOpts` 加图像字段；`ws.go` 填 `TurnOpts.PlanMode`。<br>⚠️ **双路径是已知的临时重复**：`ws.go:644` 与 `v1/service.go:313` 各自构造 `TurnOpts` 并直调 `EventsWithHistoryOpts`，两条都绕过 v1 service 层，与约束 3 表面冲突。**收敛点在 S3**（把 TUI/WS 重构成 `Channel` 的实现）。W1 只做「两边都接上」，**不得**在此顺手把 `ws.go` 重构到 v1 上——那是一次未预算的大手术 |
| **W2** 预算闸门 | 4 | G02, TD1, LEAK3, G03 | `yanshi goal` 加 `-max-tokens` flag + `internal/config` 与 `config.example.yaml` 对应项；ACP usage 回流在外部 CLI 不发 `usage_report` 时的兜底；`--fake-model` 路径的 `goalloop.Config` 补 `Tier` 字段 |
| **W3** 并发与事务 | 5 | LEAK2, WAL1, DT1, M04, DT2 | `registry.Manager.finishTerminal` 末尾调 `detachRuntime` + `cancel`（`detachRuntime` 当前在生产代码中不存在，需新建）；legacy 三入口（`agent_start`/`workflow_start`/`analysis`）接入 Manager 的并发 cap；`internal/task/work/store.go` 的 11 个写方法接上 `wt().WriteTx`（当前 `wt()` 是死代码，包头注释与实现相悖）；durable tasks 与验证门收口 |
| **W4** 压缩正确性 | 2 | CCL1, PROP1 | `internal/llm/eino/compacting.go` 的 cooldown 量纲统一（`:143` 存 `res.TokensAfter`（压缩后）vs `:192` 比较未压缩估算）；`bootstrap.go:793` 的 `CooldownTokens` 从全局 `cfg.Compaction.ContextWindow` 改用 per-provider `context_window`；补 `ctxcompact` 属性测试 |
| **W5** 安全底座收口 | 4 | S06, S07, S09, S10 | execpolicy / 持久审批规则 / 子进程网络隔离 / secrets+keyring 各自的验收缺口。**S08 已移出至 S1** |
| **W6** 工具面收口 | 11 | SPEC-TOOLIF, W07, DT4, DT5, GH1, T11, V13, LSP1, C13, MCP1, V16 | 8 个 `agent_*` 工具的 `DefaultTimeout()==0`；P0-1 已修的四组工具（git/run_tests/diagnostics/github）做端到端复验；GitHub 工具集、`web_search`、结构化 code review、LSP 诊断回喂、`/mcp` 管理界面、MCP palette、通用 MCP client 各自补齐 |
| **W7** 可观测 | 7 | COST1, OBS1, OBS2, OBS3, O07×2, BENCH1 | 成本估算 / slog / OTel / feature flags 的验收缺口；`internal/cli/doctor.go:515` 的 `checkSandbox` 硬编码占位清除（当前无条件返回「not implemented yet」，导致 exit 0 不可达）；性能基准基线 |
| **W8** TUI 体验 | 8 | UX1, UX2, UX3, UX4, UX8, C15, I18N1, E03 | Ctrl+K 命令面板 / F1 可搜索帮助 / `@path` 文件附加 / 文件 frecency / 思考流式展示 / `/keymap`+`/vim`+`/contrast` 三命令进 `commandTable`（当前文档宣称可用但表里没有）/ i18n / skill 从 GitHub 安装。**E03 的 Web 管理界面留给 S4b，此处只做 CLI + TUI 侧**。<br>⚠️ **UX3 有一条不可绕过的前提**：它当初被 plan 三轮 review 主动移出，理由是「附件读取必须在**服务端**做真实 profile + `guard.Check`，MVP 做不到」。W8 必须按服务端方案实现（附件 frame 字段 + 服务端 handler + `guard` fs 维度校验 + 硬大小上限 + 超阈值提示改用 `fs_read`），**纯客户端读文件塞进 prompt 的 MVP 方案已被否决过一次，不得重蹈** |
| **W9** 对外契约 | 5 | APS1, V12, V14, V15, APIREF1 | app-server / headless exec 增强 / API 版本化 / TS+Python SDK / schema parity 测试（v1 有 21 个 `$defs`，v1.1 现只剩 1 个）/ v1 API 参考文档 |
| **W10** 发布就绪 | 7 | PKG1, VER1, COV2, COV3, CONTRIB1, EX1, UDOC1 | 多平台打包分发 / 语义化版本 + CHANGELOG 自动化 / proto 与 bootstrap 覆盖率门禁入 CI / 贡献指南 / examples / 用户指南；含 `docs.yml` 的 `paths` 补 `cmd/yanshi/**`、`cmd/yanshi/main.go` 的 usage 补 `auth` 子命令、两份归档路线图头部加指向本审计的说明 |
| — | 1 | O12 | 以**移除**方式结案：删 `ide/vscode/`、删 `scripts/check-d2.sh`、清理文档描述 |

合计 9+4+5+2+4+11+7+8+5+7+1 = **63 项**。

### 4.4 执行顺序与并行性

```
W0（阻塞全部，先让豁免表满，再逐包清空）
 │
 ├─ W1 装配线 ────────┐   ← 最高优先：一包覆盖 9 项，含 5 条 P0/P1 头部条目
 ├─ W2 预算闸门       │
 ├─ W3 并发与事务     ├─ 可并行（互不重叠代码区）
 ├─ W4 压缩正确性     │
 └─ W5 安全底座 ──────┘
        ↓
 ├─ W6 工具面（依赖 W1 的工具注册）
 ├─ W7 可观测（依赖 W1/W2 的数据源）
 └─ W8 TUI 体验（独立）
        ↓
 ├─ W9 对外契约（依赖 W1–W8 的最终形态）
 └─ W10 发布就绪（最后，锁定门禁）
```

### 4.5 每包的统一验收标准

每个工作包合并前必须同时满足：

1. 该包覆盖的审计项，逐条对照**原验收标准**给出通过证据，并写入状态台账的 `evidence` 字段（格式见 §5.3）
2. `go test ./...` 全绿，含 W0 三条治理断言
3. **三张豁免表**（`assemblyExceptions`、`ctxInjectExceptions` 在 `internal/archtest`；GOV5 的豁免表在 `internal/bootstrap/wiring_test.go`）均无**新增**条目。<br>注意验收按**豁免表枚举**而非包路径——GOV5 不在 archtest 包内，只跑 `go test ./internal/archtest` 覆盖不到它的豁免表
4. 涉及 config / `-h` / schema 的改动，四个文档生成器重跑并提交（`cmd/api-schema` ×2、`cmd/gendocs` ×2），否则 `.github/workflows/docs.yml` 的 `git diff --exit-code` 会红
5. 该包新增/修改的能力，在 `docs/` 有对应页面

---

## 5. 进度度量机制

「32% → 100%」这个数字必须是**机器算出来的**，不能靠人重跑 118 个子代理的审计。

### 5.1 状态台账 `docs/feature-status.yaml`

63 项的机器可读单一真相源：

```yaml
- id: "C1/RLM1"
  package: W1
  verdict: partial          # partial | missing | divergent | done | removed
  acceptance: "rlm_query 进 allTools，模型可见并可调用"
  evidence: ""              # 完成时填，格式见 5.3

- id: "D2/O12"
  package: "-"
  verdict: removed          # 以移除方式结案，见 §3.2 ④
  acceptance: "ide/vscode/ 与 scripts/check-d2.sh 不存在；文档无对其描述"
  evidence: "internal/archtest::TestVSCodeExtensionRemoved"   # 包路径，非文件路径
```

**`removed` 是一档独立的终态**，与 `done` 同样计入「S0 完成」。它存在的理由：O12 的交付物是**删除**，不存在任何 `file:line` 可指——若强塞 `done`，§5.3 的 evidence 断言必红；若不给终态，63/63 永远收敛不了。`removed` 条目的 evidence 必须是一条**断言目标已不存在**的测试名。

### 5.2 `cmd/featurestatus`

读台账输出统计表（`已结项 N/63`，其中 **N = `done` 数 + `removed` 数**，两者同为终态）。可在 CI 打印，也可生成 README 徽章。与 `cmd/codelines`、`cmd/depsanalyze` 同属不参与运行时的 dev 工具。

### 5.3 `internal/archtest/status_test.go`

目的：**防止有人为了让数字好看直接改 verdict。**

**evidence 的合法形态只有两种**，校验语义在此定死：

| 形态 | 格式 | 校验方式 |
|---|---|---|
| 文件引用 | `path/to/file.go:123` | **只校验文件路径存在，不校验行号** |
| 测试引用 | `path/to/pkg::TestName` | 校验 `go test -list` 能在该包内命中 `TestName` |

**为什么不校验行号**：行号会随任何无关编辑漂移，一个按字面实现的「行号必须存在」校验会让 CI 频繁无理由变红，然后被 `// nolint` 掉——恰好是 §4.2「失败信息必须自解释，否则会被 nolint 掉」要避免的结局。行号在 evidence 里保留是给人读的定位信息，不作为断言对象。

**断言清单**：

1. `verdict` ∈ `{partial, missing, divergent, done, removed}`
2. `verdict: done` 或 `removed` 的条目，`evidence` 非空且符合上表两种形态之一
3. `verdict: removed` 的条目，evidence 必须是测试引用形态（因为它断言的是「不存在」）
4. 台账条目总数恒为 63，`id` 唯一
5. 每个 `package` 值 ∈ `{W1..W10, "-"}`

**依赖说明**：本测试需要 YAML 解析（`gopkg.in/yaml.v3`）。该依赖仓库已有（`internal/config` 在用），不引入新依赖，也不违反 GOV1（`portAllowlists` 只管 internal 依赖）。**§4.2 表格里「依赖：仅 stdlib」的作用域是 GOV4/5/6 三条断言，不含本测试。**

**连带改动**：`internal/archtest/helpers_test.go` 的包 doc 注释目前写着「The helpers rely **exclusively** on the standard library…」。本测试落进该包后这句会变成假陈述，而 GOV3 恰好是管 doc 注释的治理规则。落地时须同步把该表述限定到 GOV1–GOV4/GOV6 的 helper 上。

### 5.4 与审计文档的关系

台账是唯一真相源。`docs/feature-status-audit.md` 头部加指向说明，降级为历史快照（正如它自己对两份归档路线图所做的那样）。

---

## 6. 测试策略

| 层 | 手段 | 覆盖什么 |
|---|---|---|
| **治理层** | W0 三条治理断言（GOV4/GOV6 在 `internal/archtest`，**GOV5 在 `internal/bootstrap`**）+ 台账断言 | 结构性失效模式：没接线、授权不存在的工具、只有 getter 没 setter、虚报状态 |
| **装配层** | `internal/bootstrap` 集成测试（`--fake-model` 跑完整 `Build`） | W1 的全部 9 项——工具真进了 `allTools`、真能调、不 panic。同时销 COV3 |
| **单元层** | 各包既有测试 + 缺口补测 | W2–W8 各自的验收标准 |
| **端到端** | `timeout 5 ./yanshi --fake-model -inprocess` 冒烟 + `yanshi exec` headless 断言 | 工具在真实 turn 里可达 |
| **契约层** | schema parity 测试 | W9 |

### 6.1 新规则：生产实现必须有一条测试跑通

仓库既有约定是「Fake 优先于 mock」。但审计给了一个必须写进 spec 的反面教训：

> P0-1 的 nil `Cmd` 之所以测试全绿，是因为**测试用的 fake Factory 比生产 Factory 多填了一个字段**。fake 比真实现更完整，于是掩盖了生产路径的断裂。

**新规则**：任何生产 `Factory` / `Provider` 接口，必须有一条测试用**生产实现**跑通关键路径（模板见既有的 `TestRunSecureCaptureWithProductionFactory`）。W1 的每一处装配都要配一条这样的测试。

---

## 7. 风险与处置

| 风险 | 概率 | 处置 |
|---|---|---|
| **W1 一包改动过大，review 不动** | 高 | W1 内部按「每个接线点一个 commit」切；9 项对应 5 个接线点，可拆 5 个 PR |
| **GOV6 误报淹没信号** | 低（已消解） | **统计已完成**：全仓 38 个符合签名，命中 2 条 < 阈值 5，规则维持全 `internal/` 扫描，无需收窄（见 §4.2 GOV6）。仅当后续新增 `With*` 使误报升破 5 条时才收窄为「只检查 `internal/agent/**` 与 `internal/tools/**`」 |
| **W0 让 CI 长期红** | 中 | 已知违规先写进豁免 map（带 TODO + 工作包编号），每修一包删一条。CI 始终绿，豁免表长度 = 剩余债务计数 |
| **台账与审计文档漂移** | 中 | 台账是唯一真相源；审计文档头部加指向说明，降级为历史快照 |
| **S0 期间无新功能，动力衰减** | 中 | 每个工作包完成即发一个 `v0.x` pre-release + CHANGELOG（W10 的 VER1 提前部分落地） |
| **W8 TUI 8 项与 S4a Web IDE 重复投入** | 已接受 | 双主力对等是产品决策，作为已知成本记录在案 |
| **S0 期间已修项被新代码回退** | 中 | 这正是 W0 存在的理由——治理断言在 CI 硬跑，回退会立刻红 |

---

## 8. 待后续 spec 的开放问题

以下不在本 spec 范围，由各子项目启动时另立 spec 解决：

- **S1**：三平台沙箱后端的能力矩阵与降级语义；`WorkspaceWrite` 在各平台的确切边界；沙箱失败时是 fail-closed 还是降级告警
- **S2**：`chromedp` vs `go-rod` 的选型；页面内容进 prompt 的注入防护具体手段；浏览器会话与 agent turn 的生命周期绑定
- **S3**：`Channel` 端口的完整接口签名；TUI/WS 重构的迁移路径（能否不破坏现有 WS 帧词表）；Telegram 的身份到 yanshi session 的映射
- **S4a**：前端技术栈与构建产物的 CI 集成方式；Monaco ↔ `internal/lsp` 的桥接协议；autoVCS 时间线的 API 形态
- **S4b/S4c**：技能安全扫描的规则来源；记忆知识库的存储与检索方案

---

## 9. 参考

- `docs/feature-status-audit.md` — 2026-07-31 机器化审计（94 项，本 spec 的事实基线）
- `docs/adr/` — ADR-0001..0010，承重架构决策档案
- `CLAUDE.md` — 仓库全景当前态与约定
- `reference/QwenPaw/` — 功能对标参考（Python，AgentScope 2.0）
