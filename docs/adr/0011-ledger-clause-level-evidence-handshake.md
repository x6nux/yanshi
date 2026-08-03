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
- **不可违反的约束**：**终态证据只接受测试引用（`pkg/path::TestName`），不接受文件引用** —— 终态断言的是**行为**，只有可执行的断言能承载这个声明；文件路径只能证明文件存在。（非终态条目仍可用 `checkEvidence` 支持的文件引用形态做线索。）
- **不可违反的约束**：**被引用的测试必须在自己的 doc 注释里回写 `ledger: <ID>#<n> <子句原文>` 标记，且与台账逐字一致**。声明必须落在可被证伪的位置；文本漂移视为「两者必有一错」而非可容忍差异。
- **不可违反的约束**：**acceptance 文本必须被 `acceptancePins` 钉住，且 pin 表与台账一一对应** —— 缺行（新条目没 pin）与多行（pin 指向已不存在的条目）都判失败。改 acceptance 必须在同一次提交里显式改 pin 并说明理由；**削减验收是范围决策，不是清理**。
- **不可违反的约束**：**债务型豁免表只减不增，死条目必须失败**。这套语义现在**确实**覆盖全部 8 张债务表：`lineExceptions`、`docExceptionPkgs`、`docExceptionSymbols`、`portExceptions`、`assemblyExceptions`、`ctxInjectExceptions`、`toolWiringExceptions`、`d2HistoricalDocs`。GOV8 的陈旧标记检查是它在证据侧的等价物：台账撤回引用后，测试里留下的旧标记必须被删，不能悄悄烂在那里当作还生效的承诺。GOV8 的对账部分**不设**豁免表（`acceptancePins` 是台账镜像，不是豁免表）。
  - **勘误（本 ADR 首版在这一点上虚报）**：首版把 `docExceptionPkgs`/`docExceptionSymbols`/`portExceptions` 一并列入「同一套语义」，实际上当时这三张表**根本没有死条目检测** —— `docs_test.go` 全文件没有任何 stale 检查，`portExceptions` 的检测只覆盖 `TestR3_W2ConfigMustNotDependOnGuard` 里 config→guard 这一对。一份「防虚报」的文档自己虚报，是本 ADR 关心的同一类失败。三张表的检测已补齐（`TestExportedDocs` 的 dead-exemption 段、`deadPortExceptions`），补齐时立刻抓出一条死条目：`docExceptionPkgs["cmd/yanshi"]` 已无豁免对象（该包每个导出符号都有 doc 注释），已删除。
  - **不属于**这套语义的两张表，别按债务表读：`fanOutExempt` 记录的是永久架构角色（组合根 / 工具枢纽本来就该是 hub），**故意不做**死条目检测；`acceptancePins` 是随台账增减的镜像，不单调收缩。
- **不可违反的约束（这道门的边界，必须写明）**：**它判不了语义覆盖** —— 任何门禁都判不了。它无法确认某个测试是否真的证明了某条子句；一个内容为空的测试配一行正确的标记依然能过。它做的**不是消除虚报，而是提高虚报成本**：把「这条测试覆盖了这条验收」这个断言强制写在最容易被拆穿的位置（测试体正上方、必然出现在 diff 里、评审者一眼可见）。**任何人不得据此认为「GOV8 绿 = 验收已被证明」**；GOV8 绿只意味着「每条子句都有人具名认领，且验收文本自上次评审以来没被动过」。语义判定仍然是评审的职责，这道门只保证评审有明确的对账目标。
- **边界（修复后的新边界，同样必须写明）**：
  - **摘要判不出「改弱」与「改清楚」的区别**。pin 只能说「变了，并且有人写下了新形状」。把一条子句改写成同样字数但更容易满足的措辞，pin 会红一次、改一行就绿 —— 拦不住，只保证它出现在 diff 里。子句数下降会额外打印 `THIS DROPS N PROMISE(S).`，等距改写则没有这种信号。
  - **只钉 `acceptance`，不钉 `title`**。title 是散文，改它不影响任何门禁。
  - **pin 是本地的、无历史的**。它记「上次评审时是什么样」，不记「历来最严的版本是什么样」；连续几次各自看似合理的削减，每一步都能通过。防线是 diff 评审，不是这道门。

## 关联

- 来源：S0/W1 三轮评审的 12 条阻塞（其中 9 条同类）；`CLAUDE.md`「治理是机器强制的」段 GOV8 条目。
- 代码落点：`internal/archtest/status_evidence_test.go`（GOV8 双向握手：`checkTerminalEvidence`、`TestLedgerEvidenceIsClauseComplete`、`TestLedgerMarkersAreLive`）；`internal/archtest/acceptance_pin_test.go`（分母 pin：`acceptancePins`、`TestLedgerAcceptanceIsPinned`）；`internal/archtest/status_test.go`（台账完整性与 `checkEvidence` 引用解析）；台账本体 `docs/feature-status.yaml`；统计工具 `cmd/featurestatus`。
- 被否决的替代方案：
  - **单纯的条数比较（evidence 条数 ≥ 子句数）** —— 一行代码就能写出来，但也一行代码就能被满足：贴五个不相关的测试名即可凑数。它把「哪条测试证明哪条子句」这个关键映射留在台账作者的脑子里，等于把洞挪了个位置而没有堵上。逐句映射 + 测试侧回写才让「哪条证明哪条」变成可被 diff 审视的公开断言。
  - **人工评审 checklist（不上门禁）** —— 这正是发现 9 条问题的方式，代价是三轮评审；它能发现问题但不能防止问题重现，且成本随台账规模线性增长。
  - **把 acceptance 全文复制到第二处（如审计文档）做交叉校验** —— 也能挡住悄悄削短，但制造了两份必须同步的真相源；两份一旦漂移，没人知道该信哪份，而台账的全部价值就在于它是**唯一**真相源。摘要只存指纹，不存第二份内容，所以不会漂移。
