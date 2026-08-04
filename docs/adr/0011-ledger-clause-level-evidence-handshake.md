# ADR-0011: 台账终态证据必须逐句对账，并与测试双向握手

- 状态：accepted
- 日期：2026-08-03

## 背景（Context）

`docs/feature-status.yaml` 是 S0 的唯一进度真相。W0 为它建了第一版门禁（`internal/archtest/status_test.go`）：终态条目（`done`/`removed`）必须带非空 evidence，且 `checkEvidence` 会校验这条引用**解析得开** —— 包目录存在、测试函数存在、或文件路径存在。

问题是它**从不数子句**。台账的 `acceptance` 字段是用分号连接的**合取式**：「可创建计划任务；按时触发入队；生命周期可控；持久化；approval 门禁」是五个彼此独立的承诺。而 evidence 只是一条字符串。于是一条能解析的引用就能把五条承诺全撑住 —— 实现只满足第 1 条、evidence 只引证明第 1 条的那个测试，条目照样能被翻成 `done`，门禁全绿。

这不是假想。S0/W1 完成后经三轮评审共提出 12 条阻塞，其中 **9 条属于同一个 bug 类**：条目的 acceptance 有 N 条子句，实现与证据只覆盖第 1 条。W1 的台账数字因此一路回退：**9（计划）→ 7 → 6 → 3 → 2 → 1**，每一次回退的原因都一样。

9 次同一个洞不是 9 个失误，是**缺一道门**。更糟的是，当时那道门的形状**在鼓励虚报**：它让「贴一条能解析的引用」看起来等于「证明了验收」，把翻牌成本压到了近乎为零，而回退成本要靠人肉三轮评审去付。

## 决策（Decision）

把终态证据从「一条字符串」改成**子句级的双向握手**，由 `internal/archtest/status_evidence_test.go` 强制（GOV8）：

**正向（`TestLedgerEvidenceIsClauseComplete`）** —— 台账逐条子句点名证据。`evidence` 对终态条目必须是**子句号 → 测试引用**的 YAML 映射（`evidenceField.UnmarshalYAML` 同时接受非终态条目的标量形态，并拒绝其余一切节点类型）。`splitClauses` 按 `；` 与 `;` 切出子句，`checkTerminalEvidence` 要求 key 恰好是 `1..N`：缺 key = 未被证明的承诺，多 key = 引用了已不存在的子句。每条引用还必须是**测试引用**（含 `::`），文件引用一律拒绝。

**反向（`TestLedgerMarkersAreLive`）** —— 被点名的测试要认领这条子句。每个被引测试必须在**自己的 doc 注释**里写一行 `ledger: <ID>#<n> <子句原文>`，且子句文本与台账**逐字一致**（`markerRe` 解析，文本不符时报「一个是错的」）。反向扫描遍历 `internal/`、`cmd/` 下所有 `_test.go`，任何台账不再引用的 `ledger:` 标记都判为陈旧标记而失败。

为什么必须是双向：单向的正向计数挡不住凑数（见下方 Alternatives）。加上反向握手后，要给一条子句虚报，你就不能只改 `docs/feature-status.yaml` —— 你必须打开那个不相干的测试文件，在**测试体正上方**写下「本测试证明 `<子句原文>`」。这句话被钉在它最容易被拆穿的位置：它必然出现在 diff 里、紧挨着与它矛盾的代码、并且是任何人读这个测试时看到的第一句话。

合法的一对多覆盖不受影响：一个测试真的证明两条子句，就带两行标记、被两处引用。

**分母（`TestLedgerAcceptanceIsPinned`）** —— 子句数由 acceptance 文本算出，而 acceptance 文本由台账作者控制。首版评审实测到一条整条绕过路径：把 `C1/AU1` 的 acceptance 从 5 子句删成 1 子句、删掉 evidence key 2–5、删掉 4 条随之陈旧的 marker，**三步纯机械编辑、零语义判断**，`go test ./internal/archtest` 全绿而条目仍是 `done`。这正是 GOV8 声称要堵的形状，却能从它审计的同一个文件里够到。原因是 acceptance 在全仓**没有任何 pin**：`docs/feature-status-audit.md` 是只记判定、不记逐项验收的日期快照，无从交叉校验。

`internal/archtest/acceptance_pin_test.go` 的 `acceptancePins` 给**全部 63 条**条目（不只终态 —— 条目可以在 `partial` 期间被悄悄削短、之后再翻牌）各存一行「子句数 + acceptance 文本的 SHA-256 前 16 位」。任何对 acceptance 的编辑都让它红，唯一转绿方式是把新的子句数与摘要写进这张表。

它**不**让 acceptance 不可改，也不该：W5 计划给 `A1/S09` 加作用域限定，不能被修正的验收标准会烂掉。它禁止的是**无声的**改动 —— 改 pin 是 diff 里一行 `Clauses: 5` → `Clauses: 1`，任何人一眼看懂，紧挨着引发它的台账编辑，且不可能作为别的工作的副产物出现。

## 后果（Consequences）

> 逐句对账是台账可信度的承重墙。以下约束一旦被绕过，台账立刻退化为自证。

- 翻牌成本从「贴一条能解析的引用」升到「逐子句给出可执行断言 + 在测试侧署名」，与回退成本重新对齐。
- **不可违反的约束**：**终态条目（`done`/`removed`）的 evidence 必须是子句 → 测试的映射，key 数恰好等于 acceptance 切出的子句数** —— 不允许缺（未证明的承诺），也不允许多（引用了不存在的子句）。
- **不可违反的约束**：**终态证据只接受测试引用（`pkg/path::TestName`），不接受文件引用** —— 终态断言的是**行为**，只有可执行的断言能承载这个声明；文件路径只能证明文件存在。（非终态条目仍可用 `checkEvidence` 支持的文件引用形态做线索；线索是**可选**的，但一旦写了就必须解析得开 —— 悬空的线索比没有线索更坏，它读起来像佐证。）
  - **勘误（首版在这一点上也虚报）**：这条约束**声称**的是「只有可执行的断言能承载声明」，首版**实现**的却是「某个 `_test.go` 里存在同名顶层函数」。两者差得很远，评审用三条探针径直穿了过去：一个建库 helper（`newTestStore`，没有 `Test` 前缀）、一个 `Test` 后接小写字母的名字（`go test` 根本不跑）、以及一个 `//go:build e2e_real` 挡住、CI 从不编译的测试 —— 三条全绿。**已收紧**（`internal/archtest/status_test.go` 的 `resolveTestRef`）：名字必须满足 `go test` 的命名规则（`Test` + 非小写字母开头），签名必须是 `func(*testing.T)`，包路径必须落在 `internal/`/`cmd/` 之内。一道门实现的谓词弱于它宣称的谓词，比没有这道门更坏 —— 它把弱检查当强检查洗白了。
  - **勘误其二（收紧之后仍剩三条，同一个形状）**：第二轮评审又造了三条探针，全部 `Problem="" Constrained=false` 通过。三条的共同病根是**这道门在问文件系统，而它宣称问的是工具链**：
    1. **`testdata/` 下的测试** —— `go list ./internal/archtest/...` 不列 `testdata`（工具链按约定跳过），CI 的 `go test ./...` 永远不编译它；而 `filepath.Glob(pkgDir/*_test.go)` 照收不误。
    2. **文件名式 GOOS 约束** —— `plat_windows_test.go` 在 darwin 上不编译，实测 `go test ./... -run TestZZProbe -v` 里它从不出现；而 `buildConstraintOf` 只扫 `//go:build` 注释，报 `Constrained=false`。当时仓库里每一个 GOOS 后缀测试文件都恰好还额外带了一行冗余的 `//go:build`，所以这是**潜在**洞而非已被利用的洞 —— 但文件名后缀单独即可生效是 Go 的常规写法，第一个不写那行注释的人就会打开它。（评审给这批文件报的数是 10，实测是 8：`_unix` 不是文件名层面的 GOOS 记号——`go tool dist list` 里没有 `unix`，它只能写进 `//go:build`——而 `..._windows_cov_test.go` 的末位元素是 `cov` 也不构成约束。**连清点这个洞的那次清点本身都数错了**，这正是本 ADR 反复要求「别在文档里存别处文件的计数」的理由。）**这一条尤其该记下来**：上一版 ADR 在下方边界里写的是「build 约束一律按未执行处理，**包括 GOOS/GOARCH 约束**」，而实现根本看不见文件名后缀 —— **文档比实现强了一档**，正是本 ADR 通篇在追的那类失败，只不过这次发生在本 ADR 自己身上。
    3. **无条件 `t.Skip`** —— 编译了、跑了、报 `--- SKIP` 也就是「非失败」，零断言。规则 4 的立论（「断言从未被执行过」）对它同样成立。
    **已收紧**：判据统一改成「**默认的 `go test ./...` 会不会编译并执行它**」。包集合改由 `go list -e ./...` 给出（`defaultTestPackages`）而不再自己猜目录约定 —— 工具链跳过什么，这道门就跳过什么，包括以后新加的规则；文件名约束由 `filenameConstraintOf` 按 `go/build` 的 `goodOSArchFile` 规则解析，GOOS/GOARCH 名单来自 `go tool dist list` 而非硬编码；无条件 skip 由 `unconditionalSkip` 检出（边界见下）。三条探针改后分别得到 `Problem`、`Constrained=true [file name suffix _windows]`、`Problem`。
- **不可违反的约束**：**没有任何子句可以只靠带 build 约束的测试成立**。`//go:build e2e_real` 这类测试**任何 CI job 都不提供对应 tag**，所以「它证明了这条子句」这句话在仓库里从未被执行过一次。它们可以作为**补充**证据与一条无约束测试并列（`internal/acp`、`internal/vcs` 的真实 CLI 覆盖是本仓最深的一层，一刀切禁掉只会把那些子句推向更弱的证据），但不能独自撑起一条终态子句。
- **不可违反的约束**：**证据的包路径必须在反向扫描的根之内**（`evidenceScanRoots = {internal, cmd}`，正反两侧共用同一个变量）。首版正向用 `filepath.Join(root, pkg)` 接受任意路径、反向只遍历 `internal`/`cmd`：证据指向 `sdk/` 的话，marker 进门时被校验一次，此后**永远**不会被陈旧检测看到 —— 台账撤回引用，那句认领就烂在原地，而它本是这道门的另一半。
- **不可违反的约束**：**被引用的测试必须在自己的 doc 注释里回写 `ledger: <ID>#<n> <子句原文>` 标记，且与台账逐字一致**。声明必须落在可被证伪的位置；文本漂移视为「两者必有一错」而非可容忍差异。
- **不可违反的约束**：**acceptance 文本必须被 `acceptancePins` 钉住，且 pin 表与台账一一对应** —— 缺行（新条目没 pin）与多行（pin 指向已不存在的条目）都判失败。改 acceptance 必须在同一次提交里显式改 pin 并说明理由；**削减验收是范围决策，不是清理**。
- **不可违反的约束**：**债务型豁免表的死条目必须失败**，且「死」有**两种**形状：**豁免项已经合规**，以及**豁免的主体已经消失**（文件被删/改名、函数被删或不再导出、注入器不存在、名字已不在任何 profile allow list）。这套语义现在**确实**覆盖全部 8 张债务表：`lineExceptions`、`docExceptionPkgs`、`docExceptionSymbols`、`portExceptions`、`assemblyExceptions`、`ctxInjectExceptions`、`toolWiringExceptions`、`d2HistoricalDocs`。GOV8 的陈旧标记检查是它在证据侧的等价物：台账撤回引用后，测试里留下的旧标记必须被删，不能悄悄烂在那里当作还生效的承诺。GOV8 的对账部分**不设**豁免表（`acceptancePins` 是台账镜像，不是豁免表）。
  - **勘误（本 ADR 首版在这一点上虚报）**：首版把 `docExceptionPkgs`/`docExceptionSymbols`/`portExceptions` 一并列入「同一套语义」，实际上当时这三张表**根本没有死条目检测** —— `docs_test.go` 全文件没有任何 stale 检查，`portExceptions` 的检测只覆盖 `TestR3_W2ConfigMustNotDependOnGuard` 里 config→guard 这一对。一份「防虚报」的文档自己虚报，是本 ADR 关心的同一类失败。三张表的检测已补齐（`TestExportedDocs` 的 dead-exemption 段、`deadPortExceptions`），补齐时立刻抓出一条死条目：`docExceptionPkgs["cmd/yanshi"]` 已无豁免对象（该包每个导出符号都有 doc 注释），已删除。
  - **勘误其二（同一处第二次虚报）**：补齐之后，剩下四张表（`lineExceptions`、`assemblyExceptions`、`ctxInjectExceptions`、`toolWiringExceptions`）仍**只查「已合规」这一半**。它们的失败条件一律形如「主体存在**且**已合规」，于是主体消失时条件**恒假** —— 指向已删文件、已删函数、不存在的注入器、已不在 allow list 的工具名的条目，全部静默长存。评审四条探针全绿，对照组 `docExceptionPkgs["internal/nonexistent"]` 正确变红。这类条目不是无害的垃圾，而是**永久预授权**：同名主体一旦回来，它带着一条从未被评审过的豁免直接落地。四张表的「主体已消失」检测已补齐。
  - **只是约定、机器拦不住的那一半**：「**只能删不能加**」没有任何测试强制。新增一条违规、同时新增一行豁免，全仓全绿 —— 门禁只能判条目是否还活着，判不了它是什么时候进来的。这条靠 diff 评审守。文档里把它和上面那条并列成「同一套语义」而不点明差别，本身就是一次范围虚报。
  - **不属于**这套语义的两张表，别按债务表读：`fanOutExempt` 记录的是永久架构角色（组合根 / 工具枢纽本来就该是 hub），**故意不做**死条目检测；`acceptancePins` 是随台账增减的镜像，不单调收缩。
- **不可违反的约束（这道门的边界，必须写明）**：**它判不了语义覆盖** —— 任何门禁都判不了。它无法确认某个测试是否真的证明了某条子句；一个内容为空的测试配一行正确的标记依然能过。它做的**不是消除虚报，而是提高虚报成本**：把「这条测试覆盖了这条验收」这个断言强制写在最容易被拆穿的位置（测试体正上方、必然出现在 diff 里、评审者一眼可见）。**任何人不得据此认为「GOV8 绿 = 验收已被证明」**；GOV8 绿只意味着「每条子句都有人具名认领，且验收文本自上次评审以来没被动过」。语义判定仍然是评审的职责，这道门只保证评审有明确的对账目标。
- **边界（修复后的新边界，同样必须写明）**：
  - **摘要判不出「改弱」与「改清楚」的区别**。pin 只能说「变了，并且有人写下了新形状」。把一条子句改写成同样字数但更容易满足的措辞，pin 会红一次、改一行就绿 —— 拦不住，只保证它出现在 diff 里。子句数下降会额外打印 `THIS DROPS N PROMISE(S).`，等距改写则没有这种信号。
  - **只钉 `acceptance`，不钉 `title`**。title 是散文，改它不影响任何门禁。
  - **pin 是本地的、无历史的**。它记「上次评审时是什么样」，不记「历来最严的版本是什么样」；连续几次各自看似合理的削减，每一步都能通过。防线是 diff 评审，不是这道门。
  - **「可执行」判到「`go test` 会不会跑它」为止，判不到「它跑的时候断言了什么」**。空壳测试仍然能过（见上一条边界）；`resolveTestRef` 只保证被引的东西是一个会被执行的 `func TestX(*testing.T)`，不保证它跑起来有意义。
  - **build 约束一律按「未执行」处理，包括 GOOS/GOARCH 约束，也包括只写在文件名里的那种**。`//go:build windows` 与 `foo_windows_test.go` 其实都会在 CI 的 windows leg 上跑，但这道门不区分 —— 判定标准是「默认 `go test ./...` 编不编译它」，好让规则只有一句话。**这条一致性是有代价换来的**：判定必须与运行门禁的机器无关，否则同一份台账在 ubuntu/windows/macos 三条 leg 上会给出三种结论，「GOV8 绿」就不再是一句能被引用的话。代价是这类测试也必须配一条无约束测试才能撑起子句；因为它只是**补充**限制而非禁用，代价可接受。
  - **无条件 `t.Skip` 的检测是窄的，而且只能是窄的**。`unconditionalSkip` 只在 skip **可证明支配整个函数体**时报警：函数体的每条顶层语句都是对本测试 `*testing.T` 的方法调用，skip 之前的那些取自一张不可能失败的白名单（`Helper`/`Parallel`/`Log`/`Logf`），出现任何别的东西（一个 `if`、一次赋值、一个 helper 调用）就放弃分析并返回「没有」。**藏在条件里、子测试闭包里、或 helper 里的 skip 检不出来**，要检出就得做可达性分析，那不是这道门该背的东西。这是**故意选的偏向**：漏检留一个洞，误检会让诚实的平台门控测试无故变红，而无故变红的门禁会被删掉 —— 那是更大的洞。
  - **前置条件由被测对象自己计算的属性测试是恒真的，GOV8 不检测这一类。**

    `unconditionalSkip` 只覆盖「skip 支配整个函数体」这一种形状。存在第二种：属性测试把 `t.Skip` 放在子测试闭包里，而 skip 条件是**用被测函数的产物**算出来的。被测函数一旦被掏空，每个 trial 都在守卫处 skip，`go test` 对 0 个执行 trial 报 PASS —— 与真正通过在退出码、在输出、在 CI 日志里都不可区分。`E2/PROP1#3` 的三条证据曾整体处于这个状态。

    GOV8 不检测它，是判断而非疏漏：
    - **静态检测不可行。** 判据是「某个 `t.Skip` 的条件传递依赖于被测包返回的值」，需要跨函数数据流分析。合法的条件 skip（`os.Getenv`、`runtime.GOOS`、由被测包构造的配置对象）与病态 skip 在 AST 上同形，误报会红掉诚实的平台门测试；按 `unconditionalSkip` 已经写下的权衡，会被删掉的门禁是更大的洞。
    - **动态检测不可行。** 「跑被引测试、量 skip 率」要编译执行全部被引包，突破 ADR-0011「同一个问题，不编译」的预算；且台账引用了 `internal/archtest` 自身的测试，GOV8 会自我递归。

    因此责任落在**属性测试作者**身上，约定是：*凡带前置条件守卫的属性测试，必须断言实际执行的 trial 数下限，且这个下限统一在一个入口上*。`internal/ctxcompact/plan_property_test.go::runGeneratedProperty` / `::minExecutedTrials` / `::requireTrialFloor` 是参考实现 —— 守卫（`::skipAlreadyCompacted`）只读生成的输入、绝不读被测函数的输出，执行率低于阈值直接 fail。

    **参考实现的关键是作用域，不只是那个下限。** 第一版只把这套守卫接进三条配对属性，作用域本身就是缺陷：被漏在外面的两条属性（pin 集合子集、token 缩减）继续拿被测函数自己的产物当守卫，掏空后依旧全绿。正确形态是**包内所有生成型属性一律走同一个入口**，包括跨文件的（`internal/ctxcompact/run_property_test.go` 也调它）。照第一版那个只接配对属性的形状实现，等于把已经判定过的缺陷复制回来。

    这条约定不是机器强制的，评审时要看 —— 展开成可执行动作的是 [`docs/superpowers/review-checklist.md`](../superpowers/review-checklist.md) 的 A 段。
  - **`go list` 是包集合的权威，但它答的是「是不是包」，不是「健不健康」**。`defaultTestPackages` 用 `-e`，编译坏掉的包仍在集合里：那是 CI 自己该报的红，让它顺带把台账门禁也打黑只会把一处失败读成两处不相干的失败。
  - **非终态 evidence 只校验「解析得开」，不校验「够不够」**。它是线索不是证据；`partial` 条目写 5 条引用也不代表覆盖了 5 条子句。台账文件头（`docs/feature-status.yaml` 开头的注释）已在 `6a1a2f0` 改成如实描述这条规则，与门禁一致。

## 关联

- 来源：S0/W1 三轮评审的 12 条阻塞（其中 9 条同类）；`CLAUDE.md`「治理是机器强制的」段 GOV8 条目。
- 评审侧对应物：[`docs/superpowers/review-checklist.md`](../superpowers/review-checklist.md) —— 本 ADR 逐条写明「这道门判不了什么」，那份清单把判不了的那些变成可执行的评审动作（变异测试、门禁正反探针、空壳测试识别）。上面「属性测试前置条件」这条边界所依赖的作者约定，机器强制不了，由它在评审时看。
- 代码落点：`internal/archtest/status_evidence_test.go`（GOV8 双向握手：`checkTerminalEvidence`、`TestLedgerEvidenceIsClauseComplete`、`TestLedgerMarkersAreLive`）；`internal/archtest/acceptance_pin_test.go`（分母 pin：`acceptancePins`、`TestLedgerAcceptanceIsPinned`）；`internal/archtest/status_test.go`（台账完整性、`checkEvidence` 引用解析，以及「这条引用会不会被执行」的三个谓词：`defaultTestPackages`、`filenameConstraintOf`、`unconditionalSkip`，由 `TestResolveTestRefExecutabilityPredicates` 单测钉住 —— 台账自己只走通过路径，谓词若退化成「一律放行」整套门禁会静默全绿）；台账本体 `docs/feature-status.yaml`；统计工具 `cmd/featurestatus`。
- 被否决的替代方案：
  - **单纯的条数比较（evidence 条数 ≥ 子句数）** —— 一行代码就能写出来，但也一行代码就能被满足：贴五个不相关的测试名即可凑数。它把「哪条测试证明哪条子句」这个关键映射留在台账作者的脑子里，等于把洞挪了个位置而没有堵上。逐句映射 + 测试侧回写才让「哪条证明哪条」变成可被 diff 审视的公开断言。
  - **人工评审 checklist（不上门禁）** —— 这正是发现 9 条问题的方式，代价是三轮评审；它能发现问题但不能防止问题重现，且成本随台账规模线性增长。
  - **把 acceptance 全文复制到第二处（如审计文档）做交叉校验** —— 也能挡住悄悄削短，但制造了两份必须同步的真相源；两份一旦漂移，没人知道该信哪份，而台账的全部价值就在于它是**唯一**真相源。摘要只存指纹，不存第二份内容，所以不会漂移。
