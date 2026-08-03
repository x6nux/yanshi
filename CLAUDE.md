# CLAUDE.md

本文件为 Claude Code（claude.ai/code）在本仓库中工作时提供指引。

## 交互语言

与用户的所有交互（解释、总结、提问、进度汇报等）一律使用**中文**。代码、命令、标识符、文件路径、技术术语保持原样（英文）。

## 这是什么

`yanshi`（模块 `github.com/x6nux/yanshi`）是一个 Go LLM agent 服务端：以 Eino ADK 编排器为核心，接入 guard 包裹的工具、基于 SQLite 的 memory/VCS 存储、标准 SKILL.md 技能系统、自驱动目标循环，以及一个 Bubble Tea TUI。单个 `yanshi` 二进制同时充当客户端（TUI）与服务端。使用 Go 1.26.4；在 Windows 上开发，但通过构建标签（`internal/lockfile/alive_*.go`、`third_party/bubbletea/*_windows.go`）做到跨平台。

## 命令

```sh
go build -o yanshi ./cmd/yanshi              # 构建 CLI
go run ./cmd/testchanged [flags]             # 仅测有变更的包（见下方说明）
go test ./...                                # 全量测试套件（缓存生效）
go test ./internal/tools -run TestName       # 跑单个测试（或 -run /TestSub/）
go test -tags e2e_real ./internal/acp/...    # 真实 CLI 的端到端测试（构建标签）
go vet ./...                                 # vet（仓库内不存在 golangci-lint 配置）
go test ./internal/archtest ./internal/bootstrap  # 架构治理测试（见下方"治理是机器强制的"）
./build.sh [release]                         # 带版本注入的构建（ldflags → internal/version）
```

**测试缓存与增量测试。** Go 的 build/test cache 默认已生效 —— 包未变更时第二次 `go test` 会输出 `(cached)` 且不重跑。但 `go test ./...` 仍会*遍历*所有包（即使大部分只是检查缓存）。如果只想跑有变更的包，使用 `go run ./cmd/testchanged [flags]`：

- 它通过 `git diff --name-only HEAD` 找出变更的 `.go` 文件（含未跟踪文件），提取所在目录，然后用 `go list` 过滤掉非包目录，只对实际有变包的包执行 `go test`。
- 支持透传所有 `go test` 参数：`go run ./cmd/testchanged -v -run TestFoo`。
- 没有变更时提示"未找到变更文件"，引导使用 `go test ./...` 做全量。

**测试门禁。** 两个带 `//go:build e2e_real` 的测试（`internal/acp/e2e_real_test.go`、`internal/vcs/e2e_acp_test.go`）在未设置 `YANSHI_E2E=1` 或 PATH 上没有 `codex`/`claudecode` CLI 时还会再跳过。少量 `internal/llm/eino` 与 `internal/bootstrap` 的测试在所锁定的 eino 版本中 `eino-ext` openai provider 不可用时会 `t.Skip` —— 这些跳过是预期行为，不是失败。

**运行。** `./yanshi` 启动自包含的 TUI（为当前项目发现后端，或在进程内嵌入一个）。`--fake-model` 无需任何 API key 即可启动一个确定性 fake model（`llm.providers` 为空时也会自动选择）。alt-screen TUI 无法通过管道驱动；启动自检可用 `./yanshi -h`（打印用法并退出 0）或 `timeout 5 ./yanshi --fake-model -inprocess`。

**配置。** `config.yaml` 已被 gitignore —— 从被跟踪的 `config.example.yaml` 复制而来。YAML 由 `internal/config` 加载；`${VAR}` 环境变量在反序列化前展开。

**治理是机器强制的（`internal/archtest` + `internal/bootstrap`）。** 下方「约定」里的规则不是荣誉制，而是由测试执行的 —— 违反时 `go test ./internal/archtest ./internal/bootstrap` 会红。**GOV1–GOV4、GOV6、GOV8 住在 `internal/archtest`；GOV5 与 GOV7 住在 `internal/bootstrap/wiring_test.go`** —— 后两条要拿真实装配出来的 `App.ToolNames` 跟 profile 对账，只有在组合根内部才拿得到，所以别去 archtest 里找它们。**债务型豁免表**（记录「有人打算修的违规」）遵循同一套语义：**只能删不能加，且死条目（豁免项已经合规或已消失）也判失败**。这套语义覆盖全部 8 张债务表：`lineExceptions`、`docExceptionPkgs`、`docExceptionSymbols`、`portExceptions`、`assemblyExceptions`、`ctxInjectExceptions`、`toolWiringExceptions`、`d2HistoricalDocs`。

⚠️ 两个例外，**不适用**上述语义，别按债务表去读：
- `fanOutExempt`（deps_test.go，R4(b) 的 25 fan-out 上限）记录的是**永久架构角色**而非债务 —— `bootstrap` 是组合根、`tools` 是工具枢纽，本来就该是 hub。**故意不做死条目检测**：某次依赖数偶然掉到 25 以下就删条目，等它长回来时门禁会反过来指控组合根是「第二个组合根」。
- `acceptancePins`（acceptance_pin_test.go，GOV8）不是豁免表而是**台账镜像**：它必须与 `docs/feature-status.yaml` 一一对应，缺行和多行都判失败，因此它随台账增减而不是单调收缩。

GOV7 与 GOV8 的对账部分**故意不设任何豁免表**。

- `deps_test.go`（GOV1，`TestR1_NoImportCycle`/`TestR2_PortAllowlist`/`TestR3_W2ConfigMustNotDependOnGuard`/`TestR4_SingleServerCompositionRoot`/`TestR5_PortsMustNotDependOnServiceLayer`）：六边形分层。`portAllowlists` 规定每个 port 包允许的 internal 依赖，已知的临时违规登记在 `portExceptions`（附整改工作包，`TestR2_PortAllowlist` 会把「port 已不再 import 该依赖但条目还在」判为死条目而失败）；`bootstrap` 是唯一组合根。新增跨包依赖前先看这里，否则 CI 直接红。
- `lines_test.go`（GOV2，`TestPureCodeLineGate`）：非测试 `.go` 文件 ≤ 1000 纯代码行。豁免写在 `lineExceptions` map 里。用 `go run ./cmd/codelines` 做即时检查。
- `docs_test.go`（GOV3，`TestExportedDocs`）：`internal/` 与 `cmd/` 下所有导出符号必须有 doc 注释；豁免为 `docExceptionPkgs`（整包）与 `docExceptionSymbols`（单符号）。两张表都做死条目检测：被豁免的包里已经没有缺注释的符号（或包已不存在）、被豁免的符号已经补上注释（或已不存在），都判失败。
- `assembly_test.go`（GOV4，`TestGOV4BuildFunctionsReachable`）：`internal/bootstrap` 里每个导出的 `Build*` 必须能从 `Build` 经同包调用图到达。写完、测绿、却没接进组合根 = 运行时死代码 —— 审计里 53% 的「部分实现」是这个形状。豁免表 `assemblyExceptions` 的条目被当作**额外的 BFS 根**（而非跳过的节点），这样一次接线能让整条链同时转绿。
- `internal/bootstrap/wiring_test.go`（GOV5，`TestGOV5ProfileAllowMatchesToolRegistry`/`TestGOV5ProductionProfileHasNoPhantomNames`/`TestGOV5ConditionalToolAuthorizedWhenRegistered`/`TestGOV5OperatorProfileIsNotWidened`）：默认 orchestrator profile 里 allow 的每个工具名都必须真的被注册。幻影名字让 profile 读起来比实际权限宽；两个方向都测（fake 形状与生产形状），豁免表是 `toolWiringExceptions`。
- `ctxinject_test.go`（GOV6，`TestGOV6ContextInjectorsHaveCallSites`）：每个导出的 `With<X>(ctx, …) context.Context` 注入器都必须有生产调用点，否则整条消费链静默读零值（`registry.WithRole` 曾这样空跑）。豁免表 `ctxInjectExceptions`。
- `internal/bootstrap/wiring_test.go`（GOV7，`TestGOV7EditToolsAreRegistered`）：guard 的 allow-edits 免提示自动批准集（`guard.EditToolNames()`）里的每个名字必须是已注册工具 —— 这是 GOV5 的消费侧孪生。该集合带**授权语义**，幻影名会白占一个「不弹窗」的槽位（`fs_mkdir` 就这样残留过）。**故意不设豁免表**：往这个集合里加名字是授权变更，该走工作包而不是治理逃生门。
- `status_test.go` + `status_evidence_test.go` + `acceptance_pin_test.go`（GOV8，`TestFeatureStatusLedgerIntegrity`/`TestLedgerEvidenceIsClauseComplete`/`TestLedgerMarkersAreLive`/`TestLedgerAcceptanceIsPinned`）：`docs/feature-status.yaml` 的终态条目（`done`/`removed`）必须逐句对账 —— evidence 是**子句号 → 测试引用**的映射，key 恰好等于 acceptance 切出的子句数，且只接受测试引用；被引的测试还要在**自己的 doc 注释**里回写 `ledger: <ID>#<n> <子句原文>`（逐字一致），反向扫描则拒绝陈旧标记。**分母也被钉住**：`acceptancePins` 给全部 63 条 acceptance 各存一行「子句数 + SHA-256 前 16 位」，任何改动 acceptance 的编辑都会红，必须显式改写这一行才能转绿 —— 否则「删掉 4 条子句 + 删掉对应 evidence key + 删掉随之陈旧的 marker」这套纯机械三步就能整条绕过 GOV8。看当前台账用 `go run ./cmd/featurestatus`（`-open` 只列未结项）。理由与边界见 [ADR-0011](docs/adr/0011-ledger-clause-level-evidence-handshake.md)。
- `removal_test.go`（不带 GOV 编号，`TestVSCodeExtensionRemoved`/`TestVSCodeExtensionNotAdvertisedInDocs`）：以**删除**结项的审计项（D2/O12）必须保持删除状态 —— 路径不得回归，文档也不得再把它当作在售能力宣传。仍然提到它的历史文档（审计、计划、spec 这类有日期的档案）登记在 `d2HistoricalDocs` 并须带 `D2/O12 已作废` 墓碑。识别用 `d2Mentions` 正则组，**中英文都认** —— 产品名紧跟「扩展 / 插件 / extension / plugin」的各种拼法，以及 `<那个词> for <产品名>` 的倒装写法。本仓多份文档是英文正文，只认中文等于对最可能复发的那批文档失效。正则刻意要求产品名与那几个词**相邻**，好让 `.vscode` 忽略项与 `ide-vscode` 提交 scope 这两处合法用法不误伤。**顺带一提：这段话本身就被这道门禁改写过一次** —— 初稿把几个正则示例原样写在这里，测试立刻变红。**这条门禁会扫描 `CLAUDE.md` 本身**：在这里描述那个被删的交付物会直接让测试变红，本条目就是这么被抓到过的。

**生成的文档会被 CI diff-gate。** 改动 `internal/config.Config`、`internal/api` schema 或任何子命令的 `-h` 文本后，必须重跑生成器并提交结果，否则 `.github/workflows/docs.yml` 的 `git diff --exit-code` 会失败：

```sh
go run ./cmd/api-schema -markdown docs/api/schema.md
go run ./cmd/api-schema -markdown docs/api/resources.md
go run ./cmd/gendocs -config docs/user-guide/configuration.md
go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md
```

**其余 dev 工具（不参与运行时）：** `cmd/depsanalyze` 打印 internal 包的 fan-in/fan-out、分层与风险标记；`cmd/agent-worker` 是连接 Task API 的独立远程 worker；`cmd/featurestatus` 读 `docs/feature-status.yaml` 打印 S0 功能状态统计（`-open` 只列未结项）。

**CI 硬门禁（`.github/workflows/ci.yml`）：** `go test ./...`（ubuntu/windows/macos）、`go vet`、`go test -race`（逐包、最多 3 次重试 —— 真实 race 会 3/3 全挂，时序 flake 通常重试即过）、以及 `CGO_ENABLED=0` 的构建矩阵（含 `-tags=nokeyring`）加 `yanshi -h` 冒烟。`governance`（跑 `go test ./internal/archtest`）与 `fuzz-seed` 在 W0 已从 `continue-on-error` 收紧为**硬门禁**。注意 governance job 只覆盖 archtest 包 —— 住在 `internal/bootstrap` 的 GOV5/GOV7 由主 `go test ./...` job 承担。

## 架构

### 组合根：`internal/bootstrap`

`bootstrap.Build` 是**唯一**一个知晓所有 internal 包的包 —— 这是有意的六边形/端口与适配器布局，因此请保持依赖图始终向内流动。装配顺序是固定且有意义的：`config → store → vcs → model → tools → orchestrator → http server → task broker`。新增组件时，在此处装配。非致命的启动失败（VCS 初始化、插件发现）会打到 stderr 并以该子系统禁用的方式继续，而不是拒绝整个启动 —— 对下游的使用要依据相应 `App` 字段做门禁判断（例如 `VCSRepoID != ""`）。

### 编排器（`internal/agent/orchestrator`）

将 Eino 的 `adk.ChatModelAgent` 包裹在 ReAct 循环中（`adk.Runner`，`EnableStreaming: true`）。不那么显而易见的设计：
- **`UnknownToolsHandler`** 把模型幻觉出的工具名作为工具的*结果*（而非 Go error）返回，以便 ADK 把它回喂给模型让其重试 —— 返回 `NodeRunError` 会中断整个 turn。改动工具分发时请保留这一行为。
- **上下文压缩（compaction）** 有两条路径——mid-turn（`einollm.CompactingModel` 在 ReAct 迭代之间触发）与 pre-turn（`ctxcompact.MaybeCompact` 在 user_message 之前触发）——都委托统一核心 `internal/ctxcompact.Run`：`Plan` 决定 pin 哪些消息原文（尾部 + user 原文 + working-set 路径 + 错误/diff 标记），`EnforceToolCallPairs` fixpoint 保证 tool_call/result 配对不被切断，`RunSummary` 在 summary 输入 ≤ 0.9×窗口时走 cache-aligned 单次、否则走携带式分块（每次调用严格不超窗口），`Assemble` 把 summary 作为 user+sentinel 消息放历史末尾（避免与编排器 system prompt 的双 system 冲突）。上下文窗口按模型配置（`provider.context_window`），`/model` 切换自动用新窗口。压缩状态只走 TUI 的 activity line，不进 transcript。`KeepRecent` 在 `CompactingModel` 里是消息数、在 `ctxcompact.PlanOpts` 里是对数，桥接是 `/2`。详见 `docs/compaction.md`。
- **按 turn 切换 model** 以 `model.BaseChatModel` 指针为键缓存在 `runners sync.Map` 中 —— 这正是 `/model` 在会话中途切换 provider 的实现方式。
- **子代理委派**（`agent_start`/`workflow_start`/`analysis` 工具）会构建一个带深度上限（`tools.MaxSubAgentDepth`）的嵌套编排器；该 runner 由 `bindSubAgentRunner` 绑定进 turn 的 context，在每个入口点都被调用。

### 上下文注入是横切模式

工具通过 **context value（而非参数）** 获取鉴权/追踪/scope 状态。每个编排器 turn 按如下顺序注入：`tools.WithProfile`（权限 profile）、`tools.WithSubAgentRunner`、以及 `tools.WithVCS`（仅在已配置时）。新增需要鉴权、自动追踪编辑或得知当前 acting agent 的工具时，请从 context 读取这些值（`internal/tools/permctx.go`、`vcsctx.go`）—— 不要把它们塞进工具参数。注意：当 VCS 已配置时，其 scope 注入会*覆盖*调用方传入的 scope；只有当 VCS 为 nil 时，调用方传入的 scope 才会保留。

### Guard（`internal/guard`）—— 安全关键、fail-closed

权限检查器，维度顺序为：**destructive**（破坏性删除，最先跑、与 profile 无关）→ **tools**（glob 白名单）→ **fs**（读/写路径 glob）→ **shell**（策略 + 白名单 pattern + execpolicy rules）→ **net**（host 白名单）→ **mcp**（动态 MCP 工具的 fail-closed opt-in）。`Check` 在第一个非 Allow 维度短路。Profile 来自 `profiles:` 配置 map（见 `config.example.yaml` 中的 `coding` profile）。`shell_run` 还会额外拒绝元字符（`&&`、`||`、`;`、`|`、反引号、`$()`、`>`、`<`、换行）—— 请改为顺序执行多条命令。交互式权限模式（`default`/`allow-edits`/`yolo`/`auto`）叠加在其之上，并通过 WebSocket 询问用户（`internal/guard/mode.go`）。

**HardDeny 分两档（`Decision.Overridable`）**。**结构性 HardDeny**（`Overridable=false`，任何模式都不可越过）只有三类：shell 元字符（注入防线）、execpolicy parse-error（畸形语法）、未知 policy（配置错误）。**可覆盖 HardDeny**（`Overridable=true`）涵盖 profile 能说"不"的一切：空的 tools/fs allowlist、`shell.policy: "deny"`、`net.allow: false`、denylist 命中、execpolicy hard_deny 规则、空 MCP allowlist——这些是"profile 策略"，`yolo` 直接越过、`auto` 交给 AI 评判（详见下方模式语义）。换言之：**yolo/auto 不受 profiles 限制**（含 MCP），只受结构性语法防线 + 破坏性删除门限制。

**破坏性删除门（`checkDestructive` / `ClassifyDestruction`，profile 无关，最先短路）**：`rm -rf` 类批量删除（`/`、`~`、`$HOME`、`*`、`/etc`、`/usr`、`/home`、`C:\`、workdir 自身或祖先、裸 `rm -rf`）= **Catastrophic** → 结构性 HardDeny，**所有模式都拦**（包括 yolo/auto）；删除工作目录之外的路径 = **OutOfScope** → Prompt。判定只做词法分析（`lexShellLite`，容忍 `*`/`$`/`\`，这些恰是 execpolicy lexer 会拒掉的灾难形式）；含控制算子（`&&`/`;`/`|`/`>`/`$(`…）的命令返回 None，让 checkShell 的元字符 HardDeny 去拦。workdir 由 shell 工具注入（`Action.Workdir = s.root`），未知时绝对路径按越界处理（fail-safe）。

**模式语义（`resolvePermissionMode`，仅 WS 有 callback 时生效；SSE 无 callback 一律 fail-closed）**：
- **yolo**：越过全部 profile 策略；**只**拦 Catastrophic 与 OutOfScope 删除。工作目录内的 `rm -rf build/` 等仍放行。
- **auto**：Catastrophic 直接拦（结构性）；其余一切（含 profile 拒绝、越界删除、allowlist 未命中）交给 AI 风险评分（`assessRisk`），低风险放行、高风险弹窗。
- **default / allow-edits**：普通拒绝弹窗询问；profile 策略拒绝（`ProfileHardDeny`）**静默拒绝**（`policy: "deny"` = 不问，直接拦）。
- **plan**：只读，写操作一律拒绝。

**子进程发射：`secproc` 是**不受信程序**的强制入口，不是唯一的 `exec.Command*` 调用点。** 非测试代码里 `exec.Command*` 有 27 处 —— `internal/lsp/manager.go`、`internal/mcp/manager.go`、`internal/acp/spawn.go`、`internal/skills/install.go`、`cmd/yanshi/pr.go`（直接起 `gh`）等。约束以 `internal/secproc/secproc.go` 的包头为准，且**只覆盖不受信程序**：`shell_run`、ACP agent 这类必须走 `tools.LaunchSecureProcess` → `secproc.Launch` 以统一过 Authorize 防火墙（Authorizer 是 `tools` 包 `init` 填充的函数变量，`secproc` 因此保持叶子包）。现状与该约束仍有差距，**收敛归 W6**：`shell_run` 只在 context 绑了 factory 时走 `secproc`，否则回落到直接 pipe（`tools/shell.go:171`）；ACP agent 完全不经 `secproc`（`acp/spawn.go:148`）。shell v2 则是**有意的另一条**路径：`shell.Manager` 用 `shell.Config.Factory`（`SecureLaunchFactory`，`internal/shell/procfactory.go`）—— 接口、spec、返回值都与 `secproc.Factory` 不同，一个类型无法同时实现两者，鉴权改由九个工具各自在工具层 `Authorize(guard.Action{...})` 完成。**新增 shell v2 工具时务必自己带上 `Authorize`**，那里没有 `secproc` 兜底。

### 两种传输、共享的只有 `ServerFrame`（`internal/proto/frame.go`）

WebSocket（`/api/v1/chat/ws`，主）与 SSE（`/api/v1/chat`，备）共用的是**同一套 `ServerFrame` 词表** —— **只有服务端→客户端方向共享**。新增一种*事件*帧 → 在 `frame.go` 加，并同时更新 `ws.go` 与 `ssebackend.go`，以保持两种传输同步。SSE handler 通过 `ServerFrame.SSEEvent()` 输出 `event:`/`data:` 行。

**请求方向不共享 —— `ClientFrame` 只有 WS 在用。** SSE 用的是 `chat.go` handler 内自己的匿名请求结构体，v1（`internal/api/v1/types.go`）是第三套，两者都**从不** unmarshal `proto.ClientFrame`。所以给 `ClientFrame` 加一个请求字段对 SSE/v1 **完全无效**，必须在各自的请求结构体里再声明一次（`Images` 就是这么加的三处）。`json.Decode` 静默忽略未知键，漏加**不报任何错**，字段只是无声消失 —— 这正是图像附件 POST 给 SSE 时曾经发生的事。

关键不对称点：**WS 在服务端持有历史**（单一持久连接、双向 —— 取消、控制帧、交互式权限、流式压缩）；**SSE 每次请求回放客户端持有的历史**，且始终使用静态权限 profile。

### 后端发现（`internal/cli/session.go`、`internal/lockfile`）

TUI 始终是一个轻量的本地客户端。一个 **session resolver** 通过位于 OS cache 目录下、按项目划分的 lockfile 加一次 `/healthz` 探测来为当前项目寻找后端；若找不到，则在进程内于 `127.0.0.1:0` 引导一个并认领该 lockfile。多窗口自愈：owner 退出时，断开的客户端会重新发现，第一个发现没有存活后端的客户端引导一个新的（带 PID 存活回收的原子 lockfile 选举）。`cli.NewSession` → `tui.NewProgram` 的接线放在 `package main`（而非 `cli`），因为 `tui` 依赖 `cli.StreamEvent`，所以 cli→tui 的连接不能放在 `cli` 内。

### 自驱动目标循环（`internal/agent/goalloop`）

`yanshi goal` 按 plan → implement → evaluate → judge 运行，直到耗尽预算（`MaxIterations`）。`LLMPlanner` 负责规划；`ACPImplementer` 拉起外部 agent CLI（`codex`/`claudecode`），并在 VCS 可用时让它在一条会合并回 main 的新 worktree 分支上运行；评估器（`Test`/`Intent`/`Quality`）+ `AggregateJudge` 判定是否完成。分层开发技能 T0–T4 位于 `skills/` 下；`RuleTierer` 依据目标文本挑选层级（`auto`），`t0`..`t4` 则强制指定。`--fake-model` 接入 `FakePlanner` + `FakeImplementer` + `counterEvaluator`，提供一个零依赖的两轮演示。

### autoVCS（`internal/vcs`）

基于 SQLite、类 git 的 VCS，会在 agent 编辑流经 fs 工具时**自动追踪每一次编辑**（通过被注入的 VCS scope）—— agent 无需额外配合，只需通过 Yanshi 的工具编辑即可。`main` 是规范主干（仓库根是它的工作副本）；worktree 从 `main_head` 分出，位于 `~/.yanshi/worktrees/` 下，并通过树级三方合并合并回去。聊天/编排器的编辑追踪到 `main`；task-agent 与 ACP-agent 的编辑追踪到当前活动的 worktree。VCS 工具还作为 MCP server 暴露（`yanshi vcs-mcp`，由环境变量驱动），交付给被拉起的 ACP agent。详见 `docs/vcs.md`。

### ACP —— 外部 agent（`internal/acp`）

Agent Client Protocol 适配器，以子进程方式拉起外部 agent CLI，并把 VCS MCP server 与权限策略交付给它们。`e2e_real_test.go` 覆盖真实路径（门禁方式同上）。

## 本地 fork 说明

`go.mod` 中有一条 `replace` 指令，把 `github.com/charmbracelet/bubbletea` 钉到 `./third_party/bubbletea`。该 fork 在 Windows 上能**区分 Ctrl+Enter 与 Enter**（上游无论修饰键如何都会把 `VK_RETURN` 收敛为 `KeyEnter`），从而让 TUI 可以绑定 Enter=发送、Ctrl+Enter=换行。若要改动 bubbletea 行为，请改这个 fork —— 不要去掉 `replace`。

## 约定

- **单文件不超过 1000 行** —— 这里指**纯代码行**（不含注释行和空行）。任何 `.go` 文件的纯代码行超过 1000 时，先按职责拆分（拆到同包的新文件，或独立的子包）再继续写新代码；不要在超长文件里继续堆叠。
- **重复逻辑必须抽成公共函数** —— 发现重复实现的函数或反复出现的相同逻辑片段时，提取为公共函数/辅助函数（同包内，或放进合适的小包）复用；禁止复制粘贴。
- **注释是承重文档** —— 包和导出符号都带有多段 doc 注释来解释*为什么*（尤其在 ADK、guard、VCS 周围）。在这些区域增改时，请保持同样的注释密度。
- **Fake 优先于 mock** —— `einollm.FakeModel`、`goalloop.FakePlanner`/`FakeImplementer`、`cli.FakeBackend`、`acp.FakeAgent` 驱动确定性测试，无需 API key 或子进程。优先新增一个 fake，而非引入 mock 框架。
- **承重架构决策走 ADR** —— `docs/adr/` 是单决策的演进档案（ADR-0001..0010 已覆盖 UnknownToolsHandler、guard fail-closed、压缩、WS/SSE、autoVCS scope 覆盖等）。新增或修改上述架构章节里的约束时，从 `docs/adr/0000-template.md` 复制一条新 ADR（编号取当前最大 +1），把不可违反的约束落进 Consequences。CLAUDE.md 写全景当前态，ADR 写单条决策的来龙去脉 —— 交叉引用，不要互相复制。
- **对外契约在 `sdk/`** —— `sdk/schema/` 存放版本化的 API 契约（v1、v1.1），`sdk/python` 与 `sdk/ts` 是从中生成/校验的客户端。改动 `internal/api` 的 wire 格式时同步这里。
- **提交信息用 conventional commit** —— `feat(scope):` / `fix:` / `docs:` / `refactor:` / `test:` / `chore:` / `ci:`，CHANGELOG 由 `cliff.toml` 自动生成。**（重要：用户没主动要求时，绝对不要执行 git 提交/分支操作）**
- **被忽略的产物**：`config.yaml`、`*.db`（运行时 SQLite 存储，含 `yanshi.db`）以及构建出的二进制都被 gitignore。构建产物（`yanshi.exe`、`yanshi.exe~`）可能出现在工作树中 —— 不要提交它们。
