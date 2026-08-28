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

**治理是机器强制的（`internal/archtest` + `internal/bootstrap`）。** 下方「约定」里的规则不是荣誉制，而是由测试执行的 —— 违反时 `go test ./internal/archtest ./internal/bootstrap` 会红。**GOV1–GOV4、GOV6、GOV8、GOV9 住在 `internal/archtest`；GOV5 与 GOV7 住在 `internal/bootstrap/wiring_test.go`** —— 后两条要拿真实装配出来的 `App.ToolNames` 跟 profile 对账，只有在组合根内部才拿得到，所以别去 archtest 里找它们。

**债务型豁免表**（记录「有人打算修的违规」）遵循同一套语义，但这套语义里**只有一半是机器强制的**，读的时候别把两半混为一谈：

- **机器强制：死条目判失败。** 豁免项**已经合规**或**主体已消失**（文件被删/改名、函数被删、符号已不存在、名字已不在 allow list）都会让门禁变红。这条覆盖全部 8 张债务表：`lineExceptions`、`docExceptionPkgs`、`docExceptionSymbols`、`portExceptions`、`assemblyExceptions`、`ctxInjectExceptions`、`toolWiringExceptions`、`d2HistoricalDocs`。（「主体已消失」这半是后补的：`lineExceptions`/`assemblyExceptions`/`ctxInjectExceptions`/`toolWiringExceptions` 四张表原先只查「已合规」，而主体消失时那个条件恒假 —— 指向已删文件的条目就成了**永久预授权**，同名主体一回来就自带豁免。）
- **只是约定，机器拦不住：「只能删不能加」。** 没有任何测试能区分「新增一条违规 + 同时新增一行豁免」与「本来就在表里」—— 实测这么干全绿。这条靠 code review 守，写进表里的每条都必须附整改工作包。

⚠️ 三个例外，**不适用**上述语义，别按债务表去读：
- `fanOutExempt`（deps_test.go，R4(b) 的 25 fan-out 上限）记录的是**永久架构角色**而非债务 —— `bootstrap` 是组合根、`tools` 是工具枢纽，本来就该是 hub。**故意不做死条目检测**：某次依赖数偶然掉到 25 以下就删条目，等它长回来时门禁会反过来指控组合根是「第二个组合根」。
- `acceptancePins`（acceptance_pin_test.go，GOV8）不是豁免表而是**台账镜像**：它必须与 `docs/feature-status.yaml` 一一对应，缺行和多行都判失败，因此它随台账增减而不是单调收缩。
- `docsymbols_ablation_test.go::gov9Ablations`（GOV9 的收口条款，见下方 GOV9 条目）既不是豁免表也不是镜像，而是**测量记录**：每行存一层排版防线在当期活语料上的边际召回，全 0 是实测结果不是目标。测出非 0 时**不要改数字**，那是收口条款的触发条件。

GOV7、GOV9 与 GOV8 的对账部分**故意不设任何豁免表**。

- `deps_test.go`（GOV1，`TestR1_NoImportCycle`/`TestR2_PortAllowlist`/`TestR3_W2ConfigMustNotDependOnGuard`/`TestR4_SingleServerCompositionRoot`/`TestR5_PortsMustNotDependOnServiceLayer`）：六边形分层。`portAllowlists` 规定每个 port 包允许的 internal 依赖，已知的临时违规登记在 `portExceptions`（附整改工作包，`TestR2_PortAllowlist` 会把「port 已不再 import 该依赖但条目还在」判为死条目而失败）；`bootstrap` 是唯一组合根。新增跨包依赖前先看这里，否则 CI 直接红。
- `lines_test.go`（GOV2，`TestPureCodeLineGate`）：`internal/` 与 `cmd/` 下的非测试 `.go` 文件 ≤ **5000** 纯代码行（门禁只扫这两个目录，`third_party/`、`sdk/` 与所有 `_test.go` 都不在范围内）。豁免写在 `lineExceptions` map 里，key 是绝对路径（用 `abs("internal/…")`），当前为空。用 `go run ./cmd/codelines` 做即时检查 —— 它扫的目录与门禁一致，阈值由 `internal/archtest::TestCodelinesLimitMatchesGate` 钉住两处不漂（那个常量在 `_test.go` 里，`cmd/codelines` 没法 import，只能手抄一份）。**上限原为 1000，S0 之后按项目决定放宽到 5000**：1000 那档已经不再筛选内聚性而是在筛选**碎片化** —— `bootstrap.go` 卡在 984，每加一行接线都被逼进新文件，而 CLAUDE.md 自己称为承重的「组合根装配顺序」正被计数器打散到 `w3wiring.go`/`adaptive.go`/`c1.go`。按行数而不是按职责拆出来的文件更难读。5000 仍然能抓住这道门禁真正要抓的东西（某个文件悄悄变成第二个组合根或上帝对象），日常提醒改由 90% 预警带承担（预警阈值从 `pureLineLimit` 推导，不再是写死的 900 —— 写死的 900 配 5000 会在 18% 处报警，等于对每个文件都报，真正快撞线的那个反而分辨不出来）。
- `docs_test.go`（GOV3，`TestExportedDocs`）：`internal/` 与 `cmd/` 下所有导出符号必须有 doc 注释；豁免为 `docExceptionPkgs`（整包）与 `docExceptionSymbols`（单符号）。两张表都做死条目检测：被豁免的包里已经没有缺注释的符号（或包已不存在）、被豁免的符号已经补上注释（或已不存在），都判失败。
- `assembly_test.go`（GOV4，`TestGOV4BuildFunctionsReachable`）：`internal/bootstrap` 里每个导出的 `Build*` 必须能从 `Build` 经同包调用图到达。写完、测绿、却没接进组合根 = 运行时死代码 —— 审计把它定性为**主导失效模式**（「零件造好了，总装线没接上」），但**没给过任何百分比**（`docs/feature-status-audit.md` 里的 53% 是另一回事：94 项里 50 项判为「部分实现」，即 **53% 的条目是部分实现**，不是「53% 的部分实现是装配断裂」）。豁免表 `assemblyExceptions` 的条目被当作**额外的 BFS 根**（而非跳过的节点），这样一次接线能让整条链同时转绿。
- `internal/bootstrap/wiring_test.go`（GOV5，`TestGOV5ProfileAllowMatchesToolRegistry`/`TestGOV5ProductionProfileHasNoPhantomNames`/`TestGOV5ConditionalToolAuthorizedWhenRegistered`/`TestGOV5OperatorProfileIsNotWidened`）：默认 orchestrator profile 里 allow 的每个工具名都必须真的被注册。幻影名字让 profile 读起来比实际权限宽；两个方向都测（fake 形状与生产形状），豁免表是 `toolWiringExceptions`。
- `ctxinject_test.go`（GOV6，`TestGOV6ContextInjectorsHaveCallSites`）：每个导出的 `With<X>(ctx, …) context.Context` 注入器都必须有生产调用点，否则整条消费链静默读零值（`registry.WithRole` 曾这样空跑）。豁免表 `ctxInjectExceptions`。
- `internal/bootstrap/wiring_test.go`（GOV7，`TestGOV7EditToolsAreRegistered`）：guard 的 allow-edits 免提示自动批准集（`guard.EditToolNames()`）里的每个名字必须是已注册工具 —— 这是 GOV5 的消费侧孪生。该集合带**授权语义**，幻影名会白占一个「不弹窗」的槽位（`fs_mkdir` 就这样残留过）。**故意不设豁免表**：往这个集合里加名字是授权变更，该走工作包而不是治理逃生门。
- `status_test.go` + `status_evidence_test.go` + `acceptance_pin_test.go`（GOV8，`TestFeatureStatusLedgerIntegrity`/`TestLedgerEvidenceIsClauseComplete`/`TestLedgerMarkersAreLive`/`TestLedgerAcceptanceIsPinned`）：`docs/feature-status.yaml` 的终态条目（`done`/`removed`）必须逐句对账 —— evidence 是**子句号 → 测试引用**的映射，key 恰好等于 acceptance 切出的子句数，且只接受测试引用；被引的测试还要在**自己的 doc 注释**里回写 `ledger: <ID>#<n> <子句原文>`（逐字一致），反向扫描则拒绝陈旧标记。**「测试引用」按 `go test` 的口径解析**（`resolveTestRef`），判据统一是「**默认的 `go test ./...` 会不会编译并执行它**」：名字必须是 `Test` + 非小写字母开头、签名必须是 `func(*testing.T)`、包路径必须在 `internal/`/`cmd/` 之内（反向陈旧扫描只走这两个根，指向别处的 marker 永远不会被复查）、**包必须出现在 `go list ./...` 里**（`testdata/`、`_`/`.` 前缀目录是工具链按约定跳过的，`filepath.Glob` 却照收）、**函数体不能以无条件 `t.Skip` 开头**（编译了、跑了、报 pass，却零断言）。**带 build 约束的测试**可以作为**补充**证据，但**不能是某条子句的唯一证据**；「约束」两种形态都算：显式 `//go:build`（如 `e2e_real`，没有任何 CI job 提供那些 tag），以及**文件名后缀**（`foo_windows_test.go` 等价于 `//go:build windows`，却一行注释都没有）。后两条判定**与运行门禁的机器无关**，否则同一份台账会在 CI 矩阵的不同 leg 上给出不同结论。非终态条目的 evidence 是**可选**的，但一旦写了就必须解析得开。**分母也被钉住**：`acceptancePins` 给全部 63 条 acceptance 各存一行「子句数 + SHA-256 前 16 位」，任何改动 acceptance 的编辑都会红，必须显式改写这一行才能转绿 —— 否则「删掉 4 条子句 + 删掉对应 evidence key + 删掉随之陈旧的 marker」这套纯机械三步就能整条绕过 GOV8。看当前台账用 `go run ./cmd/featurestatus`（`-open` 只列未结项）。理由与边界见 [ADR-0011](docs/adr/0011-ledger-clause-level-evidence-handshake.md)。
- `docsymbols_test.go`（GOV9，`TestGOV9DocSymbolReferencesResolve`）：活文档里每个 `路径::符号` 引用，只要**路径解析得出**，符号就必须真实存在。这道门禁补的是评审清单 F3 规则的另一半 —— 「用符号引用替代行号，因为符号改名时 `grep` 找得回来」只在**有人真去 grep** 时成立，而清单自己的参考实现指针就这样在一次改名后无声作废了 6 个提交。**路径解析不出的引用被故意放过**，这一条同时承担三件事：给必须写幻影名的文档（记录幻影、模板占位、举反例）留自指逃生门、让别的语言的 `::`（Rust 路径、pytest node id）不误伤、让带日期的归档文档不必追改名。因此**故意不设豁免表**（同 GOV7/GOV8 的对账半）：逃生门本身是有原则的，且只要一次编辑。查找是**先文件后同包回退** —— 大文件上限会逼出文件拆分，符号挪到同包兄弟文件不该判红。扫描范围是全部活 `.md` **外加台账 `docs/feature-status.yaml`**（`docsymbols_test.go::ledgerDoc`，`docsymbols_test.go::TestGOV9ScansTheLedger` 钉住它不被悄悄摘出去）——台账注释里承载着每个未结项的判断依据，引用形态与拆解文档完全一样，把它留在 `.md only` 的过滤器外面等于那批引用一条都没保护，这是评审探针实测出来的。排除 `docs/superpowers/` 下的 plans / notes / specs（有日期的档案，理由同 `d2HistoricalDocs`）与 `reference/`（外部素材）。**这道门禁也扫 `CLAUDE.md` 与评审清单本身。**

  **收口条款 —— 排版形态漏检不再构成阻塞。** GOV9 的引用识别有三层排版防线（`docsymbols_test.go::stripMarkdownEmphasis` 的词法剥离、`docsymbols_test.go::docSymbolRefRe` 的首字母锚、`docsymbols_test.go::deglued` 的下划线重锚）。**只有当活文档里真实存在一条因某种排版形态而逃脱的死引用时才修** —— 判据是拿探针在**真实语料**上跑、测该层的**边际召回是否 > 0**，而不是能否在合成模块上造出一个反例。理由：能构造出一种绕过，与「这道已覆盖绝大多数引用、逃生门是设计一部分的门禁存在缺陷」是两件事，前者恒真、后者才需要工作包。

  **这条判据是机器算的，不是辩论出来的**：`docsymbols_ablation_test.go::TestGOV9MarginalRecallOfEachDefenceLayer` 逐层关掉防线、在活语料上重算覆盖，把每层的边际召回打印出来并钉住（`docsymbols_ablation_test.go::gov9Ablations`）。它同时打印门禁的绝对覆盖率，跑一次就能拿到当期数字，**别把数字抄进文档**。配套两条：`docsymbols_ablation_test.go::TestGOV9AblationBaselineMatchesProductionPipeline` 保证消融测的就是在跑的那道门禁（消融管线是第二份拷贝，拷贝会悄悄停止描述被测物）；`docsymbols_ablation_test.go::TestGOV9EachLayerCoversAShapeTheOthersDoNot` 给每层各钉一条**只有它能救**的形态。**边际召回为 0 不是删层请求** —— 冗余是当期语料的性质而非代码的性质，删掉任何一层都用一次已经写完的成本换一个未来无声的洞，而且删掉之后这层的边际召回再也算不出来，「没人需要它」就变成不可证伪。那张表里的 0 是**测量值**，测出非 0 时要去看失败信息点名的那条引用，别去改数字。**什么样的编辑会让它变红是钉死的**：只有两种排版会 —— 下划线强调整条引用（`_路径::符号_`）、以及只强调路径那一半（`**路径**::符号`）。这两种都是**真信号**（那条引用的判决从此只靠一层），代价是一次编辑：换个排版，或把新数字连同那条引用一起记下。`**整条引用**` 这种本仓主流写法**不会**动任何数字，`docsymbols_ablation_test.go::TestGOV9CommonEmphasisDoesNotMoveMarginalRecall` 两个方向都钉住了。
- `slashcmd_test.go`（不带 GOV 编号，`TestPhantomSlashCommandsNotAdvertised`）：**从未在 `commandTable` 注册过的斜杠命令，任何文本载体都不得把它当作可键入的能力宣传。** 载体集合是 `.md` + `.yaml/.yml` + `.go` + `.json`（`slashCarrierExts`），**多载体是这道门禁存在的全部理由**：`/keymap` 幻影连续逃了三次，`5eb5869` 只扫 markdown、`cf088f7` 只扫 Go 注释，两次都把 `config.example.yaml`（操作员照抄它建 `config.yaml`，离用户最近的一层）留在扫描面之外；GOV5/GOV7/GOV9 全都只推理 Go 符号，看不见 YAML 注释里的斜杠命令。本次接上门禁后它当场又抓出**第四种载体**：`internal/i18n/catalog/{en,zh-Hans}.json` 里成套的 `usage:` 字符串（已随本次删除，零消费者）。live 命令集从 `internal/cli/tui/commands.go` 的 `commandTable` **现场解析**而非写死。**这是永久 denylist 不是债务表**，不做「无人提及即死条目」检测（零提及正是目标态）；唯一的死条目方向是**毕业**——名字真被实现了就必须从表里删掉。**W8 把原有四条全部毕业了**（`/keymap` `/vim` `/contrast` `/locale` 都已注册、偏好级联已接通、原先宣传它们的文档现在说的是真话），所以 `phantomSlashCommands` 当前是**空 map**：机制留着，下一个幻影往里加，而不是再花三次提交跨四种载体重新发现它。需要谈论幻影的文件要**在那一行的邻近几行内**明说该命令不存在，`phantomDenialMarkers` 收了中英文各几种写法，作用域是 `phantomDenialWindow` 行而**不是整文件** —— 文件级豁免会让「写下否认」这个修复动作把整份文档永久移出扫描面，`tui.md` 与 `configuration.md`（幻影当初被宣传的地方）就这样各免疫过一次。**这条门禁同样扫描 `CLAUDE.md` 本身**。
- `overlay_test.go`（不带 GOV 编号，`TestGateFilesReadFromDiskAtRuntime`）：把「`go test -overlay` 对 `internal/archtest` 全体门禁无效」做成机器判据。`-overlay` 只改**构建期**看到的源文件，不改测试**运行时**用 `os.ReadFile`/`parser.ParseFile`/`WalkDir` 读到的字节，也不传播进 `go list` 子进程——而本包门禁全是后者，于是用 overlay 做变异探针会拿到**静默的假绿**（某轮评审据此开出过两条并不存在的阻塞）。该测试解析本包自己的 `*_test.go`，标记直接读盘的函数，沿包内调用图做**反向不动点**传播，再要求「声明了 Test 且能到达读盘」的文件集合与 `overlayImmuneGateFiles` **完全相等**（两个方向都失败）。新增读盘门禁文件时它会当场点名——`slashcmd_test.go` 就是这么被要求登记的。判别式与「哪些包 overlay 有效／无效」的实测表见 `docs/superpowers/review-checklist.md` A 段。
- `removal_test.go`（不带 GOV 编号，`TestVSCodeExtensionRemoved`/`TestVSCodeExtensionNotAdvertisedInDocs`）：以**删除**结项的审计项（D2/O12）必须保持删除状态 —— 路径不得回归，文档也不得再把它当作在售能力宣传。仍然提到它的历史文档（审计、计划、spec 这类有日期的档案）登记在 `d2HistoricalDocs` 并须带 `D2/O12 已作废` 墓碑。识别用 `d2Mentions` 正则组，**中英文都认** —— 产品名（缩写与官方全称都收在 `d2Product` 里）紧跟「扩展 / 插件 / extension / plugin」的各种拼法、`<那个词> for <产品名>` 的倒装写法、把产品名放进尾部括号当限定语的倒装词序（中英文括号都认，这正是本条目自己 title 的形状），以及打包制品的那个 token。本仓多份文档是英文正文，只认中文等于对最可能复发的那批文档失效。正则刻意要求产品名与那几个词**相邻**，好让 `.vscode` 忽略项与 `ide-vscode` 提交 scope 这两处合法用法不误伤。**顺带一提：这段话本身就被这道门禁改写过一次** —— 初稿把几个正则示例原样写在这里，测试立刻变红。**这条门禁会扫描 `CLAUDE.md` 本身**：在这里描述那个被删的交付物会直接让测试变红，本条目就是这么被抓到过的。

**机器判不了的那半，靠 [`docs/superpowers/review-checklist.md`](docs/superpowers/review-checklist.md)。** 上面 GOV1–GOV9 拦得住的都是结构性问题；「这条测试跑起来有没有意义」任何门禁都判不了（见 [ADR-0011](docs/adr/0011-ledger-clause-level-evidence-handshake.md) 的边界）。那份清单是**评审的固定尺子**：变异测试、门禁正反探针、空壳测试识别、幻影名与文档虚报扫描等手法，每条都写明做什么、怎么做、什么输出算发现问题，并附本仓真实抓到过的实例。它存在的理由是 S0/W1 的教训 —— 评审技术每轮升级会让「干净」变成移动靶，「连续 N 轮干净」因此永远不可达。**规则：每轮评审跑完整份清单，不挑不减；清单只能加不能删；新手法先把本轮跑完再补进去，从下一轮生效。** 一轮「干净」的定义是「按清单全部手法找过、零阻塞」，而不是「没发现问题」。

**生成的文档会被 CI diff-gate。** 改动 `internal/config.Config`、`internal/api` schema 或任何子命令的 `-h` 文本后，必须重跑生成器并提交结果，否则 `.github/workflows/docs.yml` 的 `git diff --exit-code` 会失败：

```sh
go run ./cmd/api-schema -markdown docs/api/schema.md
go run ./cmd/api-schema -markdown docs/api/resources.md
go run ./cmd/gendocs -config docs/user-guide/configuration.md
go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md
```

**其余 dev 工具（不参与运行时）：** `cmd/depsanalyze` 打印 internal 包的 fan-in/fan-out、分层与风险标记；`cmd/agent-worker` 是连接 Task API 的独立远程 worker；`cmd/featurestatus` 读 `docs/feature-status.yaml` 打印 S0 功能状态统计（`-open` 只列未结项）；`cmd/covercheck` 按包检查语句覆盖率下限（阈值表在 `cmd/covercheck/main.go::thresholds`，`-v` 打印每个包的实测值）；`cmd/tuidbg` 用 tmux 当终端模拟器驱动 alt-screen TUI（起会话、发按键、把渲染后的屏幕读回成文本，`-png` 可光栅化成图片），用法见 `skills/tui-debug/SKILL.md` —— **这是唯一能真的看见 TUI 渲染结果的手段**，`internal/cli/tui` 的单测断言的是 `Model.Update`/`View` 的返回值，启动崩溃与布局错位在它们全绿时照样复现。

**覆盖率门禁不在 archtest 里，在 `ci.yml` 的 `coverage` job。** 理由是类别不同：GOV1–GOV9 全是对**源码结构**的静态断言（AST / `go list`），覆盖率是测试二进制的**运行产物**。放进 archtest 意味着在 `go test` 里再起 `go test`（范围写 `./...` 直接自我递归，写死包名又是另一张要维护的表），并且会在 `-race` job 下变成嵌套的非 race 运行 —— 而 `bootstrap` 的测试会真起 sqlite 与 `127.0.0.1:0` 监听。**CI job 同样是机器强制的，并不比 archtest 弱。** 阈值取 `max(spec 验收值, 实测 − 3pp)`：spec 的下限（proto 80 / store 75 / bootstrap 50）当前分别有 18/21/44 个百分点的余量，只守它们等于把那么多退化空间免费送出去。

**CI 硬门禁（`.github/workflows/ci.yml`）：** `go test ./...`（ubuntu/windows/macos）、`go vet`、`go test -race`（逐包、最多 3 次重试 —— 真实 race 会 3/3 全挂，时序 flake 通常重试即过）、以及 `CGO_ENABLED=0` 的构建矩阵（含 `-tags=nokeyring`）加 `yanshi -h` 冒烟。`governance`（跑 `go test ./internal/archtest`）与 `fuzz-seed` 在 W0 已从 `continue-on-error` 收紧为**硬门禁**。注意 governance job 只覆盖 archtest 包 —— 住在 `internal/bootstrap` 的 GOV5/GOV7 由主 `go test ./...` job 承担。

## 架构

### 组合根：`internal/bootstrap`

`bootstrap.Build` 是**唯一**一个知晓所有 internal 包的包 —— 这是有意的六边形/端口与适配器布局，因此请保持依赖图始终向内流动。装配顺序是固定且有意义的：`config → store → vcs → model → tools → orchestrator → http server → task broker`。新增组件时，在此处装配。非致命的启动失败（VCS 初始化、插件发现）会打到 stderr 并以该子系统禁用的方式继续，而不是拒绝整个启动 —— 对下游的使用要依据相应 `App` 字段做门禁判断（例如 `VCSRepoID != ""`）。

### 编排器（`internal/agent/orchestrator`）

将 Eino 的 `adk.ChatModelAgent` 包裹在 ReAct 循环中（`adk.Runner`，`EnableStreaming: true`）。不那么显而易见的设计：
- **`UnknownToolsHandler`** 把模型幻觉出的工具名作为工具的*结果*（而非 Go error）返回，以便 ADK 把它回喂给模型让其重试 —— 返回 `NodeRunError` 会中断整个 turn。改动工具分发时请保留这一行为。
- **上下文压缩（compaction）** 有两条路径——mid-turn（`einollm.CompactingModel` 在 ReAct 迭代之间触发）与 pre-turn（`ctxcompact.MaybeCompact` 在 user_message 之前触发）——都委托统一核心 `internal/ctxcompact.Run`：`Plan` 决定 pin 哪些消息原文（尾部 + user 原文 + working-set 路径 + 错误/diff 标记），`EnforceToolCallPairs` fixpoint 保证 tool_call/result 配对不被切断，`RunSummary` 在 summary 输入 ≤ 0.9×窗口时走 cache-aligned 单次、否则走携带式分块（每次调用按窗口预算切分，**但超窗量对窗口无界**：`takeChunk` 的预算判据是 `i > 0 && tok+mt > budget && splitIsSafe(…)`，`i == 0` 不检查预算——否则会返回空 chunk 让 `RunSummary` 空转——所以①**单条超大消息在完全无配对时**就能超窗，②`splitIsSafe` 扫整个左半边找配对，`[call(id1..idN), r1..rN]` 这种并行工具组**每个内部切点都不安全**，整组必进同一 chunk。真实上界是 **窗口 + 历史中最大不可分割段**，与窗口无关，随并行工具数线性增长。**这里刻意不写死任何比值**——实测数字由 `internal/ctxcompact::TestTakeChunk_OvershootShapesAreMeasured` 现场打印（`go test ./internal/ctxcompact -run TestTakeChunk_OvershootShapesAreMeasured -v`），它断言的是形状而不是数字（比值随并行工具数单调增长；超大消息无配对时自己超窗），数字随 fixture 变化。此处曾写死过三个比值，fixture 一次重写就全部腐烂，而更早还写过「< 2×」——那个更听起来精确、实际两个分句都不成立，因为当时的属性测试生成器从不产出上述两种形状，于是把一个未经证明的不变量当成了结论），`Assemble` 把 summary 作为 user+sentinel 消息放历史末尾（避免与编排器 system prompt 的双 system 冲突）。上下文窗口按模型配置（`provider.context_window`），`/model` 切换自动用新窗口 —— 两条路径机制不同：pre-turn 由 handler 查 `windows` map，mid-turn 由 `runnerFor` 拿 `TurnOpts.ModelID` 查 `CompactionConfig.ProviderWindows` 再交给 `wrapCompaction`。**W4 之前 mid-turn 这条是假的**：`wrapCompaction` 给每个新实例填的都是全局回退窗口，128K 的 provider 拿到按 256K 算的 threshold（自身容量的 1.9 倍，门永不触发）。量纲约束见 ADR-0013。压缩状态只走 TUI 的 activity line，不进 transcript。`KeepRecent` 在 `CompactingModel` 里是消息数、在 `ctxcompact.PlanOpts` 里是对数，桥接是 `/2`。详见 `docs/compaction.md`。
- **按 turn 切换 model** 以 `model.BaseChatModel` 指针为键缓存在 `runners sync.Map` 中 —— 这正是 `/model` 在会话中途切换 provider 的实现方式。
- **子代理委派**（`agent_start`/`workflow_start`/`analysis` 工具）会构建一个带深度上限（`tools.MaxSubAgentDepth`）的嵌套编排器；该 runner 由 `bindSubAgentRunner` 绑定进 turn 的 context，在每个入口点都被调用。

### 上下文注入是横切模式

工具通过 **context value（而非参数）** 获取鉴权/追踪/scope 状态。**注入的权威清单是 `orchestrator.withTurnContext` 的函数体**（`internal/agent/orchestrator/orchestrator.go`），别照抄下面这段摘要 —— 它会漂。当前它分三层：

- `bindExecutionContext`（每 turn 必调）：`WithProfile`、`WithWorkRoot`、`WithTaskManager`，以及在对应字段非 nil 时的 `WithApprovalManager`、`WithSandbox`、`WithNetworkPolicy`、`WithSecureProcessFactory`、`WithShellManager`、`WithVCS`、`WithLSP`。
- `withTurnContext` 自己再加：`WithPlanMode`、`WithThreadLink`、`WithTurnImages`、`WithWorkEventCallback`、`WithMCP`。
- `bindManagedRunner` → `bindSubAgentRunner` 最后绑 `WithSubAgentRunner`（**不在** `bindExecutionContext` 里，虽然同样是每 turn 都绑）。

**"nil 就不注入"是新工具最容易踩的坑**：`WithSandbox`/`WithShellManager`/`WithSecureProcessFactory` 这几个都带 nil 门禁，所以从 context 读它们必须走 `…FromContext(ctx)` 的双返回值形态并处理 `ok=false`，不能假设一定有值。新增需要鉴权、自动追踪编辑或得知当前 acting agent 的工具时，请从 context 读这些值（`internal/tools/permctx.go`、`vcsctx.go`）—— 不要把它们塞进工具参数。注意：当 VCS 已配置时，其 scope 注入会*覆盖*调用方传入的 scope；只有当 VCS 为 nil 时，调用方传入的 scope 才会保留。

### Guard（`internal/guard`）—— 安全关键、fail-closed

权限检查器，维度顺序为：**destructive**（破坏性删除，最先跑、与 profile 无关）→ **mcp**（动态 MCP 工具的 fail-closed opt-in，只对 `mcp_` 前缀的名字生效）→ **tools**（glob 白名单）→ **fs**（读/写路径 glob）→ **shell**（策略 + 白名单 pattern + execpolicy rules）→ **net**（host 白名单）。

**这个顺序是承重的**，因为 `Check` 在第一个非 Allow 维度短路，而不同维度对同一个动作给的是不同**档位**的否定。以 `Tools.Allow=["fs_*"]`、`MCP.Allow` 为空、动作是 `mcp_foo` 为例：mcp 维度先跑 → 空 allowlist → 可覆盖 HardDeny（default 模式静默拒绝，SSE 无 callback 一律 fail-closed）；若 tools 排在前面则是 glob 未命中 → Prompt（可交互批准）。两条完全不同的下游路径。`checkMCPTools` 排在 `checkTools` 之前是**刻意**的，理由写在它自己的 doc 注释里（宽泛的 `Tools.Allow`（尤其历史遗留的 `"*"`）不得静默授权新配置的 MCP server）—— 动这个顺序前先读那段。

Profile 来自 `profiles:` 配置 map（见 `config.example.yaml` 中的 `coding` profile）。`shell_run` 的命令先被 `execpolicy.ParseCommandList` **拆成段**（`&&` / `||` / `;` / `|` 为界），每段各自过完整的 shell 判定，整条取**最严的一段**（`guard.moreSevere`）；重定向目标送进 fs 维度，read/write 的判据是「去掉前导 fd 数字后是不是 `<` 开头」（`guard.Guard.checkRedirectTargets`），**不是一张操作符清单** —— `>&文件` 也是写（bash/sh/zsh 实测一致，只有 `>&数字` 与 `>&-` 不指向文件），把它读成描述符复制曾让目标路径整条绕过 fs 维度，实测无提示写进 `~/.ssh/authorized_keys`。**重定向也不是命令边界**：`>/dev/null rm -rf /` 是一条命令，破坏性删除门必须连着读（`guard.skipRedirect`），把 `>` 当分隔符曾让 `rm -rf /` 的 argv 走到真实进程。**拆不出段的形态仍然一律结构性 HardDeny**：命令替换 `$(…)`/反引号、进程替换 `<(…)`、子 shell 括号、here-document `<<`、后台 `&`、裸换行回车、未闭合引号、结尾反斜杠 —— 权威清单是 `execpolicy.ParseCommandList` 的 doc 注释与它返回 error 的那些分支，别在这里数。**命令替换在双引号里也一样拒，只有单引号才是数据**：POSIX shell 在 `"…"` 里照样做替换，而 `execpolicy.listScanner.scanQuoted` 曾把 `$(` 与反引号逐字节抄进 word，于是 `run()` 里那两条拒绝永远看不到它们 —— `rm -rf "$(echo /)"`、`eval "$(echo rm) -rf /"` 等六种拼法实测走到 Allow 而 `/bin/sh` 真跑了 `rm -rf /`。这句话在防线存在之前就写在这里了，是「文档承诺了一条不存在的防线」的实例。理由、代价与新不变量见 [ADR-0004](docs/adr/0004-guard-stateless-and-shell-metachar-hardblock.md) 的「补充后果」。**`Guard.Check` 评的是两种读法**：字面文本，加上「把值就在这个字符串里的参数展开解析掉」之后的文本（`guard.expandKnownParameters`），`moreSevere` 折叠。值来自字符串之外的展开**原样保留** —— 抹成空串会让 `rm -rf $BUILD_DIR` 变成裸 `rm -rf`，落进任何模式都不可申诉的 catastrophic 档。凭据维度在这一点上取**相反**决定（`guard.elideExpansions`，抹空，因为 denylist 匹配的是字面目录段，`~/.s${x}sh` 就是 `~/.ssh` 插了个空展开），这个不对称是刻意的，理由与两条不可违反的约束见 [ADR-0017](docs/adr/0017-expansion-is-a-second-reading-not-a-rewrite.md)。**wrapper payload 的重定向也进 fs 维度**：`bash -c "echo k > ~/.ssh/authorized_keys"` 的 payload 对外层 reader 是一个引号词，`guard.nestedPayloads` 把它挖出来只交给 `guard.Guard.checkRedirectTargets`（**不过 profile 的命令策略** —— 那会让 `patterns: ["sh -c 'npm test'"]` 拒绝自己那一条），读不出来的 payload 跳过而不是升级成结构性拒绝。**额外一道收口不在 guard 里**：落到 Prompt 的链走不到交互批准，`internal/tools::Authorize` 的审批 scope 构造（`scopeFromAction`）拒绝多于一个可执行段的命令，所以链要么每段都被静态放行、要么被拒。**reader 的选择只有一处**：`execpolicy.ParseCommandListFor`，guard 与审批 scope 都走它 —— 两边各选各的时候，guard 知道 `C:\temp` 与 `C:temp` 是两个目录而审批缓存认为它们是一条，批准前者就静默放行了后者；反方向同一处缺陷让所有以 `\` 结尾的 PowerShell 命令（`Get-ChildItem C:\`）在 scope 构造时报 `trailing escape`，guard 说 Prompt 而用户**永远看不到弹窗**。语言本身也进 scope（`approval.Scope.Interpreter`），因为共用 reader 只能解决**同一种语言内**两种拼法撞车，同一串文本交给两种语言仍是两条命令。POSIX 是空字符串这个取值是刻意的：既有的持久化批准都带着空值，改成 `"posix"` 会让它们全部重新弹窗。交互式权限模式（`default`/`allow-edits`/`yolo`/`auto`）叠加在其之上，并通过 WebSocket 询问用户。**模式词表**在 `internal/guard/mode.go`（`guard.Modes`/`guard.NormalizeMode`）、**auto 模式的判据**在 `internal/guard/autoapproval.go`（`guard.AutoApprovalPrompt` 的提示词 + `guard.ParseAutoApproval` 的回答解析）；**询问逻辑本身**（`resolvePermissionMode`）在 `internal/api/http/ws_perm.go` —— guard 包里没有这个函数。

**HardDeny 分两档（`Decision.Overridable`）**。**结构性 HardDeny**（`Overridable=false`，任何模式都不可越过）当前是 **6 类**：

1. 灾难性批量删除（`checkDestructive`，见下一段）
2. **命令嵌套超出解包预算**（`checkDestructive` 的 `DestructionUnreadable`，W-B-03）—— 与第 1 条同出一个维度但**不是同一档否定**：那条说「这是场灾难」，这条说「预算用完了、我们没能读到底下是什么」。两者的 reason 文案必须不同，理由与第 1 条那个括号一样（拿灾难文案去描述一条可能根本不含删除的命令，会把读者送去找错误的命令）。**「层数耗尽按最严」是 fail-closed 原则，审计只说了上限是 8 层、没说到底该怎么办** —— 若改成「按已看到的部分判」，这道上限自己就是绕过：`nohup` 重复 9 次再接 `rm -rf /`，最外层的词在任何删除表里都不在，整条判 None。
3. shell 结构读不出来（`execpolicy.ParseCommandList` 返回 error —— 注入防线；INF1 之前这一条是「含元字符」，集合大小没变、判据从子串扫描换成了「拆得出段吗」）
4. execpolicy parse-error（畸形语法）
5. 未知 shell policy（配置错误）—— 合法值只有 `""`（= `allowlist`）/ `allowlist` / `deny` / `denylist`，**没有 `allow`**。这一档从 config 侧走不到了：`guard.ValidateShellPolicy` 是权威目录，`internal/config::Config.validateProfiles` 逐个 profile 校验。**校验只覆盖 `shell.rules` 为空的 profile** —— `rules` 非空时 `checkShellPolicy` 在 execpolicy 分支里就 return 了，policy switch 不可达、写错的值是惰性的，为它拒绝一次启动等于给一个 guard 从不读的字段制造行为回归（理由与边界写在那个函数的 doc 注释上）。两条路合起来仍然闭合：`rules` 清空后同一个值变成活的，下一次加载会被拒。对账分两条测试、覆盖的方向不同：`internal/guard::TestShellPolicyCatalogMatchesCheckShell` 用真实 `Check` 钉住每个目录值观察到的档位，`internal/guard::TestShellPolicyCatalogEqualsCheckShellSwitch` 用 `go/ast` 解析 `checkShellPolicy` 的 switch 与目录做**集合相等**（`checkShellPolicy` 多认一个值同样判红——那会让 guard 能执行的配置加载失败）。运行时这一档仍留着，因为 profile 也可以由代码直接构造。
6. **未知 execpolicy verdict**（`checkShellPolicy` 里 `switch result.Verdict` 的 `default` 分支）—— **防御性分支，当前从任何配置都到不了**。`execpolicy.Evaluate` 的出口集合是 `allow` / `prompt` / `hard_deny`，全被前面的 `case` 接住。**规则文件里把 `decision` 写错不走这里**：`Evaluate` 自己的 `default` 先把它转成 `hard_deny`，于是落进 `case "hard_deny", "deny":` —— 那是**可覆盖**的一档，`yolo` 能越过。这条曾被写成「规则文件给出了本代码不认识的判决」，因果正好是反的。出口集合 ⊆ `checkShellPolicy` 已处理集合由 `internal/guard::TestExecPolicyVerdictsAreHandledByCheckShell` 钉住（静态推导 `Evaluate` 能产出的 verdict + 行为矩阵自证），谁给 `Evaluate` 加一个直通新 verdict 就会在那里变红。**这也是 `docs/user-guide/guard.md` 那份枚举比这里少一项的原因**：那份面向配置操作者，不数不可达的源码分支；本条面向改 guard 源码的人，数它。**两边都不要去写对方当前有几条** —— 那是「描述另一个文件当前内容」的裸计数，改任一侧就打穿另一侧，而没人会回来改。写「差在哪一项」即可，差集本身是稳定的。

**这个"6"是描述而非契约**：权威枚举在 `internal/guard/guard.go` 的 `overridableDeny` doc 注释上，现场清点用 `grep -n 'hardDeny(' internal/guard/guard.go` 加 `checkShellPolicy` 里那两处内联 `Decision`（它们不带 `Overridable` 字段，因此是结构性）。这段话此前写"只有三类"，漏了第 1 和第 5 条——而**同一份 CLAUDE.md 的下一段又把 Catastrophic 称作结构性 HardDeny**，同一个术语在相邻两段指了两个不同的集合。根因是 `checkShell` 头上那段陈旧注释（在那次一并改掉了）：权威那段在 `a30eb80` 修过，消费侧那段没跟上，CLAUDE.md 抄的正是没跟上的那份。**闭合枚举（"只有"）在安全边界上必须与源码同改**，读者据此判断 yolo 能越过什么。

**可覆盖 HardDeny**（`Overridable=true`）涵盖 profile 能说"不"的一切：空的 tools/fs allowlist、`shell.policy: "deny"`、`net.allow: false`、denylist 命中、execpolicy hard_deny 规则、空 MCP allowlist——这些是"profile 策略"，`yolo` 直接越过、`auto` 交给 AI 评判（详见下方模式语义）。换言之：**yolo/auto 不受 profiles 限制**（含 MCP），只受上面那 6 条结构性防线限制 —— **破坏性删除门就是其中第 1、2 两条，不是额外的一条**（这里此前把它并列写成「N 条 + 破坏性删除门」，读者会多数出一条）。此外越界删除（`OutOfScope`）与读不懂的 payload（`Opaque`）都不是 HardDeny 而是 Prompt，因此两者都不在这 6 条里；`yolo` 会为前者弹窗、为后者放行（`resolvePermissionMode` 的破坏性开关只列 Catastrophic 与 Unreadable）。⚠️ **「Opaque ⇒ yolo 放行」只在 payload 读不出灾难性读法时成立** —— payload 若能被读成 shell 命令且判 Catastrophic，档位就是 Catastrophic（第 1 条，yolo 也拦），与哪个程序接收它无关。`fish -c "rm -rf /"` 因此与 `bash -c "rm -rf /"` 同判；理由见 [ADR-0019](docs/adr/0019-the-tier-follows-the-payload-not-the-program-name.md)。

**破坏性删除门（`checkDestructive` / `ClassifyDestruction`，profile 无关，最先短路）**：`rm -rf` 类批量删除（`/`、`~`、`$HOME`、`*`、`/etc`、`/usr`、`/home`、`C:\`、workdir 自身或祖先、裸 `rm -rf`）= **Catastrophic** → 结构性 HardDeny，**所有模式都拦**（包括 yolo/auto）；嵌套层数超过 `guard.maxUnwrapDepth`（8，与审计报告里 codex 的数字一致）而底下还藏着命令 = **Unreadable** → 同样是结构性 HardDeny；删除工作目录之外的路径 = **OutOfScope** → Prompt；**没有任何读法认领、而命令仍然带着一个本包读不懂的 payload** = **Opaque** → Prompt（`guard.opaquePayload`）；**尾部 argv 的某个后缀本身读得出一条破坏性命令** = 已知 runner 判全档、未知程序封顶 Opaque（`guard.classifyTrailingArgv`）。

**Opaque 这一档是「读不懂就拒」的落点，它的存在理由是问题的形状而不是某几个形态**：逐段判定要求 guard 正确解析 shell，而那是无界的，用「发现一条补一条」追它等于把未知拼法的默认判决设成放行。它同时承担两件事 ——「本包不认识的程序 + 一个 code flag（`-c`/`-e`/`--command`/`--eval`/`--execute`）+ 一个长得像程序而不像选项值的操作数」（`python3 -c …`、`perl -e …`、`powershell -EncodedCommand …`），以及 **wrapper 表的兜底**（`bash +o posix -c "rm -rf /"` 没有任何 unwrapper 认领，从静默 Allow 变成弹窗）。**它是 Prompt 而不是结构性地板**，所以 profile 之外 yolo 仍能放行、default 会弹窗 —— 代价是过严会变多（`psql -c "SELECT 1"` 现在弹窗），这个代价被明确接受，因为判错方向可观测而反方向不可观测。

**但档位是从 payload 读出来的，不是从程序名读出来的（`guard.gradeUnreadPayload`，[ADR-0019](docs/adr/0019-the-tier-follows-the-payload-not-the-program-name.md)）**：`opaquePayload` 交回的那段操作数会被再读一次，读出灾难就判灾难档。上一版只写「Opaque = Prompt」，于是 `fish -c "rm -rf /"` 与 `bash -c "rm -rf /"` 判决不同 —— 唯一差别是 `fish` 不在 `guard.posixShellPrograms` 里，「换一个 guard 没听说过的 shell」因此是一条通用 yolo 绕过。

**flag 之外还有一半：尾部 argv（`guard.classifyTrailingArgv`）。** `opaquePayload` 只对「flag 标成 code 的操作数」发声，而 `pkexec rm -rf /` / `firejail` / `bwrap` / `strace` / 一个**编造的程序名** 全部走的是无 flag 的尾部 argv，实测全 Allow 且真 shell 真跑。判据是结构性的 —— **argv 的任一后缀读得出一条破坏性命令就报**，与前面那个程序叫什么无关；`prefixRunners`/`remoteShellRunners` 里的程序按定义会跑它的 argv，判全档，其余程序**封顶 Opaque**（「它会不会执行 argv」正是未知的那一件事，`echo rm -rf /` 只是打印六个词），`scriptEmitters` 整个豁免。**这一半无论 `read` 标志如何都跑**：`taskset -c 0 rm -rf /` 曾被 prefix stripper 误读（`-c` 同时算 value flag 与 mask positional，吃掉了 `rm`），而「被认领」本身就把兜底关掉了 —— **「我读错了」拿到的判决比「我读不动」还弱**。配套的 `guard.nonInterpreterPrograms` 是过严缓解表，**漏一条 = 多一次弹窗，错放一条 = 静默放行**。理由、被否决的四个替代方案与四条不可违反的约束见 [ADR-0018](docs/adr/0018-an-unread-payload-is-a-refusal-not-a-pass.md)。判定只做词法分析（`lexShellLite`，容忍 `*`/`$`/`\`，这些恰是 execpolicy lexer 会拒掉的灾难形式）；**程序词的拼法一律看穿而不是拒绝** —— 反斜杠转义、`FOO=1` 赋值前缀、`{`/`!`/`then`/`do` 这类保留字、`eval`、以及 `sudo`/`nohup`/`timeout` 这类前缀执行器，权威清单在 `ClassifyDestruction` 自己的 doc 注释上（每一条都是实测过的 Allow，别在这里数）；含控制算子的命令**也拆段逐个判、取最严** —— 这一条是 INF1 的承重配套：它原先返回 None 把链交给 checkShell 那道整条 HardDeny，而那道兜底一细化，`ls && rm -rf /` 就会两头落空。workdir 由 shell 工具注入（`Action.Workdir = s.root`），未知时绝对路径按越界处理（fail-safe）。

**`checkDestructive` 的 Prompt 不再短路 `Check`**：它是唯一一个既能给结构性 HardDeny（Catastrophic）又能给 Prompt（OutOfScope）的维度，而 Prompt 一旦短路，一条「越界删除 + 另一段被 shell 维度结构性拒绝」的链就只会返回那个较轻的 Prompt。现在 Catastrophic 仍然短路，OutOfScope 改为当作**下限**折进后续维度（`moreSevere`）—— 折叠只会更严：`moreSevere(Prompt, Allow)` 逐字节等于原来短路返回的那个 Prompt。

**模式语义（`internal/api/http::resolvePermissionMode`，仅 WS 有 callback 时生效；SSE 无 callback 一律 fail-closed）**：
- **yolo**：越过全部 profile 策略；拦 Catastrophic 与 OutOfScope 删除。工作目录内的 `rm -rf build/` 等仍放行。
- **所有模式（含 yolo/auto）之上还有两道 auto-resolve 豁免**：`req.ForcePrompt`（如 `task_cancel`）与 `req.ApprovalRequired`（`NewApprovalGuardedTool` 包的工具，如 GitHub 变更）在 `resolvePermissionMode` 的**最开头**就返回 `(deny, false)`，即"不自动放行、交回 callback 显式审批"。所以"yolo 只拦破坏性删除"是不对的 —— 这两类工具在 yolo 下**每次**都弹窗，无 callback 时 fail-closed。
- **auto**：Catastrophic 直接拦（结构性）、越界删除弹窗；**其余一切交给 AI 判断，Go 侧没有任何静态白/黑名单**。模型连同会话上下文（最近一条 user message 作为意图、workdir、profile 的拒绝理由、完整命令原文）拿到问题，答 `ALLOW`/`ASK`。

  **风险类别写在提示词里，不写成代码**（`internal/guard/autoapproval.go::AutoApprovalPrompt`），分四组：**伸出项目之外**（提权、关机/服务、磁盘、系统账户与属主、防火墙/内核、系统包管理器、定时任务、远程执行、Windows 注册表）、**不可逆**（force-push/filter-branch 改写共享历史、删除 VCS 从未记录的东西、容器逃逸、跨会话杀进程）、**执行没人读过的代码**（下载即执行的各种形态、从 `/tmp` `~/Downloads` `~/.cache` 跑脚本、执行本会话抓来但没读回的东西 —— 远程脚本必须先落盘、被读过再执行）、**数据外泄**（把项目内容/凭据/环境变量发给外部服务、`env`/`printenv` 把 API key 打进 transcript 进而随下一轮请求发给 provider）。**这么放比写成 Go 黑名单强**，理由不是"AI 更聪明"而是很具体的一条：`bash -c "sudo rm -rf /"` 被 `lexShellLite` 切成程序 `bash` + 一个引号参数，静态表只能匹配到 `bash`（于是要么全拒每个 `bash -c`，要么永远看不见那个 sudo）；模型读的是原始整串，`sudo` 就在眼前。`env FOO=1 sudo x`、`nohup sudo x`、`timeout 5 sudo x` 同理。**代价是真的**：黑名单没法被说服，提示词可以 —— `Args` 里混着攻击者可控文本（路径、commit message、抓回的文档），所以调用能给自己辩护。fence 与「视为数据」的标注减轻它但不消除它。

  **提示词里的类别有测试保护**：`internal/guard::TestAutoApprovalPrompt_CoversEveryRiskCategory` 逐类断言关键词还在。搬进提示词等于搬出编译器视野 —— 删掉一整段，代码照样编译、照样返回判决，只有这个测试会红。

  **没有等级也没有阈值**（此前是 LLM 打 1-10 分比阈值，中间还短暂做过静态黑白名单）。**错误策略是单向的**：无模型、超时、API 报错、回复读不懂，全部 → 弹窗。**auto 退化成 manual，永远不退化成放行**。回复解析（`guard.ParseAutoApproval`）只认 ≤3 词的短回答且拒绝两个判决词并存 —— 散文没法解析只能拒绝，`I would not allow this without asking` 里 "asking" 不是 "ask"，纯词扫描会把一句拒绝读成批准（这是测试逼出来的，不是设计出来的）。
- **default / allow-edits**：普通拒绝弹窗询问；profile 策略拒绝（`ProfileHardDeny`）**静默拒绝**（`policy: "deny"` = 不问，直接拦）。
- **plan**：只读，写操作一律拒绝。

**子进程发射：`secproc` 是**不受信程序**的强制入口，不是唯一的 `exec.Command*` 调用点。** 非测试代码里 `exec.Command*` 的调用点散落在二十来个文件里 —— `internal/lsp/manager.go`、`internal/mcp/manager.go`、`internal/skills/install.go`、`internal/tools/gate.go`（`task_gate_run` 的 argv，理由见 ADR-0012）、`cmd/yanshi/pr.go`（直接起 `gh`）等。**这里刻意不写具体条数**，要当前数字自己跑：

```sh
# 调用点（排掉纯注释行里的提及）
grep -rn 'exec\.Command' --include='*.go' internal cmd | grep -v _test.go \
  | grep -vE ':[0-9]+:[[:space:]]*(//|\*)' | wc -l
# 文件数（同一口径：只算有真实调用点的文件）
grep -rn 'exec\.Command' --include='*.go' internal cmd | grep -v _test.go \
  | grep -vE ':[0-9]+:[[:space:]]*(//|\*)' | cut -d: -f1 | sort -u | wc -l
# 文件数（不排注释行的口径 —— 它与上一条的差就是本段说的那个分歧）
grep -rl 'exec\.Command' --include='*.go' internal cmd | grep -v _test.go | wc -l
```

**那条 `grep -vE` 不是可选的，它就是本条的教训。** 这段话原先写"27 处"，后来一次自我更正说"写下它的那次提交里实际就已经是 32 处"，据此断言数字腐烂了。实测：32 是**不排注释行**的口径，排掉 5 行纯注释里的提及后恰好是 27，而本段开头用的词就是**调用点** —— 原来那个 27 在它自己的口径下**一直是对的**，"腐烂"是换了口径量出来的。所以真正的结论不是"数字会腐烂"，而是**不写口径的数字无法被复核**：同一句话里"文件数"也有同样的分歧（写本段时实测：不排注释 29 个，只算调用点 22 个 —— 上面那个块的第二、三条命令就是这两个口径，差值即那些只在注释里提到它的文件）。要写数字就把命令一起写上，否则别写 —— **本段自己曾只给了"调用点"那一条命令，把这两个文件数裸着留在正文里**。

约束以 `internal/secproc/secproc.go` 的包头为准，且**只覆盖不受信程序**：`shell_run`、ACP agent 这类必须走 `tools.LaunchSecureProcess` → `secproc.Launch` 以统一过 Authorize 防火墙（Authorizer 是 `tools` 包 `init` 填充的函数变量，`secproc` 因此保持叶子包）。**W-B-02 把这两条都收敛完了**：

- `shell_run` 现在**只有一条发射路径**。那条「context 没绑 factory 就回落到直连 pipe」的分支已**删除**（不是旁路 —— 它是第二份 spawn 实现：没有凭据清洗、没有沙箱缝、没有托管代理 env，而走哪条由「有没有人记得绑 factory」决定）。`orchestrator.bindExecutionContext` 现在**无条件**绑 factory，nil 时替换成 `shell.UnsandboxedSecureFactory()`；没绑 factory 于是变成接线 bug，`secproc.Launch` 如实 fail-closed 而不是偷偷发射。配套地，`shell_run` **自己不再调 `Authorize`** —— `Launch` 的第一步就是同一次 Authorize，两处都留会为一条命令弹两次窗；原先本地那次多带的两个字段改从 spec 下传（`Workdir` 是破坏性维度的边界、`ArgsJSON` 是弹窗要显示的东西）。`Workdir` 传的是**工具构造时的 root 而不是调用方给的 `workdir` 参数**，否则模型写 `{"workdir":"/"}` 就能把边界挪走。
- **ACP agent 走 `acp.SpawnSecure`**，`internal/acp` 里那个 exec 版 `Spawn` 与 `buildCmd` 已删除，goal loop 是最后一个调用方。**代价是真的且是有意的**：exec 版把 `os.Environ()` 原样交给外部 agent CLI，凭据清洗后不再如此，靠 env 变量提供 API key 的机器上 `yanshi goal` 的 agent 拿不到 key（agent CLI 自己的磁盘登录仍然有效；掉了哪些变量由 `netpolicy credential scrub` 那条日志按名字列出）。要给它开口子等于改授权，得走工作包 —— `internal/acp::TestSpawnSecureHandsTheAgentNoCredentials` 就是拦这个的。

**这里同样只给符号名不给行号** —— 上一版写的 `tools/shell.go:171` 早已漂进 factory 分支内部，指向了它想描述的那条路径的反面。shell v2 则是**有意的另一条**路径：`shell.Manager` 用 `shell.Config.Factory`（`SecureLaunchFactory`，`internal/shell/procfactory.go`）—— 接口、spec、返回值都与 `secproc.Factory` 不同，一个类型无法同时实现两者，鉴权改由 `internal/tools/shell_v2.go` 里注册的**每一个** v2 工具各自在工具层 `Authorize(guard.Action{...})` 完成（现场清点：`grep -c 'NewGuardedTool(' internal/tools/shell_v2.go` 与 `grep -c 'Authorize(' internal/tools/shell_v2.go`，**两个数必须相等** —— 相等本身才是这句话要说的事，具体是几不重要；同段其余数字都已改成现算 + 命令，这个中文数字是最后一处漏网的）。**新增 shell v2 工具时务必自己带上 `Authorize`**，那里没有 `secproc` 兜底。

### 两种传输、共享的只有 `ServerFrame`（`internal/proto/frame.go`）

WebSocket（`/api/v1/chat/ws`，主）与 SSE（`/api/v1/chat`，备）共用的是**同一套 `ServerFrame` 词表** —— **只有服务端→客户端方向共享**。新增一种*事件*帧要动**三个不同层**的文件，这句话此前把它们并排写成好像都在一处，实测害人给错过指令：

- `internal/proto/frame.go` —— 词表本身。
- `internal/api/http/ws.go` —— **服务端**的 WS 写出侧。
- `internal/cli/ssebackend.go` —— **客户端**的 SSE 解析侧（注意它在 `internal/cli/` 不在 `internal/api/http/`）。

SSE 的**服务端** handler 是 `internal/api/http/chat.go`，它通过 `ServerFrame.SSEEvent()` 泛化输出 `event:`/`data:` 行，所以通常不必为新帧改动它 —— 要改的是客户端那一半，否则新帧到了客户端边界就停住。**给已有帧加字段同理**：漏掉客户端解析侧时，字段在 wire 上有、在 UI 里没有，两端都不报错。

**请求方向不共享 —— `ClientFrame` 只有 WS 在用。** SSE 用的是 `chat.go` handler 内自己的匿名请求结构体，v1（`internal/api/v1/types.go`）是第三套，两者都**从不** unmarshal `proto.ClientFrame`。所以给 `ClientFrame` 加一个请求字段对 SSE/v1 **完全无效**，必须在各自的请求结构体里再声明一次（`Images` 就是这么加的三处）。`json.Decode` 静默忽略未知键，漏加**不报任何错**，字段只是无声消失 —— 这正是图像附件 POST 给 SSE 时曾经发生的事。

关键不对称点：**WS 在服务端持有历史**（单一持久连接、双向 —— 取消、控制帧、交互式权限、流式压缩）；**SSE 每次请求回放客户端持有的历史**，且始终使用静态权限 profile。

### 后端发现（`internal/cli/session.go`、`internal/lockfile`）

TUI 始终是一个轻量的本地客户端。一个 **session resolver** 通过位于 OS cache 目录下、按项目划分的 lockfile 加一次 `/healthz` 探测来为当前项目寻找后端；若找不到，则在进程内于 `127.0.0.1:0` 引导一个并认领该 lockfile。多窗口自愈：owner 退出时，断开的客户端会重新发现，第一个发现没有存活后端的客户端引导一个新的（带 PID 存活回收的原子 lockfile 选举）。`cli.NewSession` → `tui.NewProgram` 的接线放在 `package main`（而非 `cli`），因为 `tui` 依赖 `cli.StreamEvent`，所以 cli→tui 的连接不能放在 `cli` 内。

### 自驱动目标循环（`internal/agent/goalloop`）

`yanshi goal` 按 plan → implement → evaluate → judge 运行，直到耗尽预算（`MaxIterations`）。`LLMPlanner` 负责规划；`ACPImplementer` 拉起外部 agent CLI（`codex`/`claudecode`），并在 VCS 可用时让它在一条会合并回 main 的新 worktree 分支上运行；评估器（`Test`/`Intent`/`Quality`）+ `AggregateJudge` 判定是否完成。分层开发技能 T0–T4 位于 `skills/` 下；`RuleTierer` 依据目标文本挑选层级（`auto`），`t0`..`t4` 则强制指定。`--fake-model` 接入 `FakePlanner` + `FakeImplementer` + `counterEvaluator`，提供一个零依赖的两轮演示。

### autoVCS（`internal/vcs`）

基于 SQLite、类 git 的 VCS，会在 agent 编辑流经 fs 工具时**自动追踪每一次编辑**（通过被注入的 VCS scope）—— agent 无需额外配合，只需通过 Yanshi 的工具编辑即可。`main` 是规范主干（仓库根是它的工作副本）；worktree 从 `main_head` 分出，位于 `~/.yanshi/worktrees/` 下，并通过树级三方合并合并回去。聊天/编排器的编辑追踪到 `main`；task-agent 与 ACP-agent 的编辑追踪到当前活动的 worktree。VCS 工具还作为 MCP server 暴露（`yanshi vcs-mcp`，由环境变量驱动），交付给被拉起的 ACP agent。详见 `docs/vcs.md`。

### ACP —— 外部 agent（`internal/acp`）

Agent Client Protocol 适配器，以子进程方式拉起外部 agent CLI，并把 VCS MCP server 与权限策略交付给它们。`e2e_real_test.go` 覆盖真实路径（门禁方式同上）。反方向的 server 端（把 yanshi 自己暴露给 Zed 这类宿主）在 `internal/acpserver`，走 stdio、复用 `internal/appserver` 的传输与会话管理约定（诊断走 stderr，stdout 只放协议帧）。

### 运行期工具名 fail-closed（`internal/toolreg`）

GOV5/GOV7 只在**编译期**推理工具名，运行期一个陌生名字仍然能走到「点一下批准」的对话框 —— 实测 `Tools.Allow=["*"]` 且绑了 callback 时，`Authorize` 对一个不存在的工具名会返回 nil 并**真的问用户一次**。`internal/toolreg` 是这条缝的运行期一半：`toolreg.WithRegistered` 由 orchestrator 的 `bindExecutionContext` 绑定「本执行作用域里真实存在的名字」，`toolreg.Check` 在 `internal/tools::Authorize` 与 `internal/tools::AuthorizeApprovalRequired` 的**第一句**执行，未注册即拒且不弹窗。

两点容易踩错：**未绑定集合时它是 no-op**（子代理与大量测试不绑，fail-closed 只在生产装配下生效）；**新增给模型用的工具必须在组合根注册**，否则运行期被这道检查拒掉 —— 症状是工具在 schema 里、模型能调、每次都被拒，而 GOV5 看不见这个方向（GOV5 只校验「allow 的名字必须已注册」，反过来不管）。`internal/bootstrap/w3wiring_test.go` 对**真实装配的 App** 同时断言注册与授权，补的就是这个盲区。

### 循环护栏（`internal/loopguard`）

per-turn 的可插拔停止条件框架：`loopguard.Gate` 观察每次迭代（工具名 + 参数哈希、已用 token、已用 wall-clock、迭代序号），返回 continue / modify_prompt（注入一条纠偏消息）/ stop。`loopguard.NewHandler` 组装成一条链，由 orchestrator 作为 ADK middleware 装进 `runnerFor`。现有闸：重复调用检测（滑窗 + 分级：先纠偏后停止）、per-turn 工具调用预算、wall-clock deadline、turn token 预算。

三条承重设计：**状态在 context 里按 turn 存，不在 middleware 上** —— `runnerFor` 会 memoise runner，一个 middleware 实例服务同一 model 的所有会话，计数器放在它身上就是进程级的，第一个把预算用完的用户会替所有人用完。**token 累加的是 prompt 增量而非 prompt 原值** —— provider 按调用累计上报，直接求和会在长 turn 里高估数倍、让预算在名义值的一个零头处就触发。**零值配置 = 全部关闭**，行为与引入前逐字节一致；一个自作主张打开的停止条件会静默截断 turn，看起来像模型自己放弃了。

## 本地 fork 说明

`go.mod` 中有一条 `replace` 指令，把 `github.com/charmbracelet/bubbletea` 钉到 `./third_party/bubbletea`。该 fork 在 Windows 上能**区分 Ctrl+Enter 与 Enter**（上游无论修饰键如何都会把 `VK_RETURN` 收敛为 `KeyEnter`），从而让 TUI 可以绑定 Enter=发送、Ctrl+Enter=换行。若要改动 bubbletea 行为，请改这个 fork —— 不要去掉 `replace`。

## 约定

- **单文件不超过 5000 行** —— 这里指**纯代码行**（不含注释行和空行）。这是**全仓约定**，但机器强制的范围更窄：GOV2 只扫 `internal/` 与 `cmd/` 下的**非测试**文件，`_test.go`、`third_party/`、`sdk/` 超标不会让门禁变红 —— 那部分靠自觉。
  **拆分的判据是职责，不是行数。** 5000 是「这个文件已经不是一个东西了」的兜底信号，不是目标值；一个 3000 行的内聚单元不需要拆，两个 200 行但职责无关的东西合在一个文件里就已经该拆了。反过来也成立：为了压住计数器而把一段本该连着读的逻辑（组合根的装配顺序是最典型的例子）切到别的文件，是拿可读性换一个数字。
- **重复逻辑必须抽成公共函数** —— 发现重复实现的函数或反复出现的相同逻辑片段时，提取为公共函数/辅助函数（同包内，或放进合适的小包）复用；禁止复制粘贴。
- **注释是承重文档** —— 包和导出符号都带有多段 doc 注释来解释*为什么*（尤其在 ADK、guard、VCS 周围）。在这些区域增改时，请保持同样的注释密度。
- **Fake 优先于 mock** —— `einollm.FakeModel`、`goalloop.FakePlanner`/`FakeImplementer`、`cli.FakeBackend`、`acp.FakeAgent` 驱动确定性测试，无需 API key 或子进程。优先新增一个 fake，而非引入 mock 框架。
- **承重架构决策走 ADR** —— `docs/adr/` 是单决策的演进档案（ADR-0001..0011 已覆盖 UnknownToolsHandler、guard fail-closed、压缩、WS/SSE、autoVCS scope 覆盖、台账逐句对账等）。新增或修改上述架构章节里的约束时，从 `docs/adr/0000-template.md` 复制一条新 ADR（编号取当前最大 +1），把不可违反的约束落进 Consequences。CLAUDE.md 写全景当前态，ADR 写单条决策的来龙去脉 —— 交叉引用，不要互相复制。
- **对外契约在 `sdk/`** —— `sdk/schema/` 存放版本化的 API 契约（v1、v1.1），**它就是真相源**：`sdk/schema/schema.go` 用 embed 暴露那两个物理文件，`internal/api/v1::SchemaBytes` 原样返回 v1，`GET /api/v1/schema/agent-v1.json` 吐的就是这串字节。此前运行时另有一份 3-`$defs` 的 Go 字面量、`$id` 与 SDK 那份 21-`$defs` 的不同，客户端 fetch 到的文档描述的不是它自己 SDK 强制的契约。
  `sdk/python` 与 `sdk/ts` 是**手工维护**的镜像（都不是生成的 —— `cmd/api-schema` 曾自称 TS 生成器而实为手抄字面量，那一半已删，命令现在只有 `-markdown` 一个职责）。四路一致性由 `internal/api/v1/parity_test.go::TestContractParityAcrossFourSources` 对账：Go struct / JSON Schema / TS / pydantic 的字段集合必须一致，差异逐条具名带理由，死条目判失败。落点在 Go 侧是因为 `go test ./...` 无条件跑，而 Node/Python 工具链在 CI 里是可选步骤。改动 `internal/api` 的 wire 格式时四处同改，漏一处那条测试会红。
- **提交信息用 conventional commit** —— `feat(scope):` / `fix:` / `docs:` / `refactor:` / `test:` / `chore:` / `ci:`，CHANGELOG 由 `cliff.toml` 自动生成。**（重要：用户没主动要求时，绝对不要执行 git 提交/分支操作）**
- **被忽略的产物**：`config.yaml`、`*.db`（运行时 SQLite 存储，含 `yanshi.db`）以及构建出的二进制都被 gitignore。构建产物（`yanshi.exe`、`yanshi.exe~`）可能出现在工作树中 —— 不要提交它们。
