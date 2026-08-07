# S0 验收子句级拆解

> ⚠️ **D2/O12 已作废** —— VS Code 扩展（`ide/vscode/`）与 `scripts/check-d2.sh` 已于 2026-08
> 以移除方式结案（spec `docs/superpowers/specs/2026-08-03-yanshi-roadmap-design.md` §3.2 ④），
> 由 `internal/archtest::TestVSCodeExtensionRemoved` 守住。本文「无工作包 — 已删除项」一节为
> 记录 `d2Mentions` 召回漏洞而**逐字抄录了重新宣传该交付物的若干写法**（产品官方全称、打包
> 制品名、括号倒装词序）。那些句子是门禁的测试语料，**不是**产品承诺，**不要照做**。

**用途**：`docs/feature-status.yaml` 的 63 条验收全部拆到子句级，每一子句显式标注「证据形状 / 当前状态 / 归属工作包」。

**为什么存在**：台账的 acceptance 是分号分隔的多子句，而 GOV8 只在**翻终态时**才按子句数对账。七轮评审下来主导错误始终是同一个 —— 实现满足了第 1 句，就把 verdict 翻成 `done`；已经回退过 7 条。根因不是评审不认真，而是**子句从未被显式拆开过**：每次翻牌都要临时重新切分、临时判断每句需要什么证据，于是每次都漏同一批。这份文档把切分与「每句要什么证据」固化下来，让翻牌变成查表而不是重新推理。

**和 GOV8 的关系**：这份文档不是门禁，是门禁的**输入**。GOV8（`internal/archtest/status_test.go` + `status_evidence_test.go` + `acceptance_pin_test.go`）负责机器强制；这里负责把「第 3 句需要什么」写成人能复核的形式。子句切分与 `splitClauses`（`internal/archtest/status_evidence_test.go`）完全一致：按 `；` / `;` 切、去空白、丢空串。**子句编号即 GOV8 evidence 的 key。**

**规模**：63 条 / 230 子句。

**状态栏是写作时快照，别当现状读。** 每条标题上的「台账 `partial`」、汇总表的
`1/1/2` 计数、以及每一子句的「已兑现 / 部分 / 未兑现」都是**写下那天**的判断。
翻掉一条牌就让其中几处失效，而这份文档一共 230 子句 —— 逐条追改不可持续，追不动
就会变成没人信的陈述。**现状唯一权威是 `docs/feature-status.yaml`，现算用
`go run ./cmd/featurestatus`。** 本文不追那一栏，它追的是**不随状态变化**的那半：
子句怎么切、每句要什么形状的证据、什么样的测试骗得过去。那半就是查表时要读的东西。
（这条说明本身也是被这个问题逼出来的：一次会话翻掉 6 条，本文对应的 6 处状态
当场全部作废。）

**引用约定（读之前先看）**：代码引用一律写成 `路径::符号`（或「路径 + 就近的反引号符号名」），**不带行号**。这份文档最初 386 处引用全部是 `file.go:NNN` 形式，写下 14 分钟后就有 4 处指错了符号 —— 紧接着的一次提交给 `internal/tools/git.go` 与 `internal/tools/testrun.go` 各加了几十行，行号原地漂移。**行号越界能被扫出来，范围内漂移是无声的**，而这份文档被 `docs/feature-status.yaml` 指定为翻牌前的唯一查表依据，无声错位比缺失更贵。符号名同时也是更好的定位手段（`grep` 一次即得）。**这条约定只覆盖 Go 引用**：本文与 `docs/feature-status.yaml` 的 Go 行号引用现在都是零，而且**这句话本身由机器算出来**（`internal/archtest/docsymbols_test.go::TestNoGoLineCitationsInLedgerInputs`）—— 因为它腐烂过一次：某个提交在宣布「刚修好台账里三处漂移的行号」的同时，重写了两个属性测试文件，把本文三处行号写漂了 6–10 行，而这一段仍旧写着「已清零」。**一句断言计数的话在有人去算之前只是愿望**，所以现在有人算。非 Go 文件（`*.yml` / `*.sh` / `*.toml` / 其他文档）没有符号可指，那些 `file:line` 仍在，也仍会漂移 —— 它们是**写作时快照**，读到时请按内容而非行号核对。Go 符号引用的活性由 GOV9（`internal/archtest/docsymbols_test.go::TestGOV9DocSymbolReferencesResolve`）守着：路径解析得出而符号解析不出即判红；扫描范围**含台账 yaml**（`internal/archtest/docsymbols_test.go::ledgerDoc`），所以本文与台账两侧的 Go 符号引用受同一道门禁保护。台账里的非 Go `file:line` 仍在门禁之外，只能人工核。

**否定式断言与计数的约定**：本文里的「某文件完全没有 X」「某 struct 有 N 个字段」这类断言**没有任何门禁**（GOV9 只管符号解析，`TestNoGoLineCitationsInLedgerInputs` 只管行号），而它们已经错过两次：一条说 `config.example.yaml` 没有 `subagents:` 段（实际一直都在，还有绿测试钉着），一条把 `Evidence` 的字段数写成 11（实际 10，且同条自己的枚举也等于 10）。为什么不给它们加门禁，理由与实测分母见 `docs/superpowers/review-checklist.md` 的 F1/F2。**写作要求**：否定式断言必须当场附上产生空输出的那条命令；字段/元素计数一律**不写数字**，改写枚举或「见 `路径::符号`」。撤回一条错断言时用删除线保留原文并写明「撤回」，不要静默改掉 —— 虚增的缺口已经进过工作包，读者需要知道它作废了。

---

## 怎么读「证据形状」这一栏

这一栏是本文档的核心价值。它不写「需要一个测试」，它写**什么样的断言才能证明这一句**，以及**什么样的测试骗得过去**。后半句同样重要 —— 本仓每一次错误翻牌都是被一个「看起来相关」的测试顶上去的。

### 假证据分类（每种都在本仓真实发生过）

| 代号 | 名称 | 判据 | 本仓案例 |
|---|---|---|---|
| **M** | 变异盲 | 把被测函数掏空，测试仍 PASS | 属性测试的守卫条件用被测函数自己的产物算 skip 条件 —— 函数坏掉时测试直接 skip 而不是 fail |
| **T** | 恒真空壳 | 断言的两边都是字面量或同一表达式 | `TestCompactingModel_KeepRecentBridge` 断言 `KeepRecent(4)/2 >= 2`，且自己注释就写明「不断言 bridge 本身」 |
| **S** | 吞错 | `t.Logf` 吃掉 error、`_ = out` 丢弃结果 | `TestRunTestsDefaultTimeout` —— 超时行为对错都绿 |
| **F** | fake 太宽 | fake 不看关键入参，硬编码返回正确值 | diagnostics 的 fake 不看 argv，把 `go --version` 的 bug 盖了三轮 |
| **P** | spy 只证明调用发生 | 返回 stub 对象，看不见环境/退出码/资源回收 | `shell_run` 的 spy 返回 `&StartedProcess{PID:1234}` |
| **X** | 不会被执行 | `testdata/` 下、文件名式 GOOS 约束、`//go:build` 约束、无条件 `t.Skip` | e2e_real 系列（这批是**有意**的门禁，不算假证据；无意的才算） |
| **N** | 测量不是断言 | 验收要求的是一次测量或一份文档，不是可执行的行为断言 | 「覆盖率 ≥80%」「CI 记录趋势」「docs 结构清晰」 |

**N 类要单独决策**：它不是「找不到测试」，是**这句话的形状撑不起测试引用**。GOV8 的不可违反约束是终态证据只收测试引用（ADR-0011），所以 N 类子句只有两条出路 ——
(a) 补一道**门禁测试**把测量变成断言（例如子进程跑 `go test -cover` 并断言下限、或对生成产物做 diff-gate）；
(b) 承认这句不可终态化，**改写 acceptance**（改 acceptance 必须同步改 `internal/archtest/acceptance_pin_test.go` 的 pin，是范围决策而非台账对账）。
在其中之一落地前，含 N 类子句的条目**不能翻终态** —— 拿任何功能测试顶上去都是凑数。

### 状态三档

- **已兑现**：实现存在，且有测试真正驱动它（非上述任一假证据形状）。
- **部分**：实现存在但测试不驱动关键分支；或实现只覆盖子句的一部分外延。
- **未兑现**：实现不存在，或实现是残桩（有函数、有注册，但输出契约本身不成立）。

⚠️ 「未兑现」不等于「文件不存在」。本仓最常见的形状是**残桩**：工具已注册、已装配、在出厂 profile 里，但输出字段恒为空。判 `missing` 时要写明是哪一种，否则下一轮评审会把它误读为低报。

⚠️ **「事实已满足但没有门禁看守」一律归 `部分`，不是第四档、更不是 `已兑现`。** 这类子句（覆盖率数值已达标、废弃物今天数出来是 0）的**实现侧已经完备，缺的只是把事实钉住的那条断言**，恰好落在 `部分` 的前半句定义上；`已兑现` 的第二个合取项「有测试真正驱动它」它不满足。**判它「未查证」尤其是错的** —— 未查证的含义是**本轮没去看**，一旦有人去看过并写下了可重跑的复核命令，这一格就必须离开「未查证」，否则汇总那句「翻牌前必须补查」会变成一条要求重做已完成工作的活指令，正是上面「否定式断言与计数的约定」里登记的「虚增的缺口会直接变成工作包」同形。已按此定档的实例：E1/COV2#1、E1/COV3#1（数值达标、无覆盖率门禁）与 M1/SPEC-TOOLIF#3（废弃物零命中、无防回流门禁）。**新写这种子句时不要发明第四个标记** —— 四桶交叉校验（各桶之和 = 子句数、`+N?` 跨行加总）对一个桶外标记完全无感，索引与汇总会同时读起来自洽而实际错位。

---

## 索引：63 条 × 230 子句

「状态」列格式为 `已兑现/部分/未兑现`（如有未查证另标 `+N?`）。

| ID | 包 | 台账 verdict | 子句 | 状态 |
|---|---|---|---:|---|
| [A1/T07/T08](#a1t07t08) | W1 | `partial` | 5 | 1/3/1 |
| [A2/G05](#a2g05) | W1 | `partial` | 4 | 1/1/2 |
| [B1/M04b](#b1m04b) | W1 | `partial` | 4 | 1/2/1 |
| [B1/M05](#b1m05) | W1 | `partial` | 5 | 2/3/0 |
| [C1/AU1](#c1au1) | W1 | `partial` | 5 | 3/2/0 |
| [C1/M07](#c1m07) | W1 | `partial` | 3 | 2/0/1 |
| [C1/RLM1](#c1rlm1) | W1 | `partial` | 4 | 3/0/1 |
| [G/VISION](#gvision) | W1 | `partial` | 3 | 1/1/1 |
| [G/VISION-TOOL](#gvisiontool) | W1 | `partial` | 4 | 0/2/1 +1? |
| [B0/TD1](#b0td1) | W2 | `partial` | 5 | 2/2/1 |
| [F2/LEAK3](#f2leak3) | W2 | `partial` | 3 | 1/1/1 |
| [M1/G02](#m1g02) | W2 | `partial` | 2 | 0/2/0 |
| [M1/G03](#m1g03) | W2 | `partial` | 3 | 2/1/0 |
| [A2/DT1](#a2dt1) | W3 | `partial` | 4 | 0/2/2 |
| [A2/DT2](#a2dt2) | W3 | `divergent` | 4 | 0/2/2 |
| [B1/M04](#b1m04) | W3 | `partial` | 4 | 0/3/1 |
| [F1/WAL1](#f1wal1) | W3 | `partial` | 5 | 1/3/1 |
| [F2/LEAK2](#f2leak2) | W3 | `partial` | 4 | 1/2/1 |
| [E2/PROP1](#e2prop1) | W4 | `done` | 3 | 3/0/0 |
| [F2/CCL1](#f2ccl1) | W4 | `partial` | 3 | 1/0/2 |
| [A1/S06](#a1s06) | W5 | `partial` | 3 | 0/3/0 |
| [A1/S07](#a1s07) | W5 | `partial` | 4 | 0/3/1 |
| [A1/S09](#a1s09) | W5 | `partial` | 4 | 0/1/3 |
| [D3/S10](#d3s10) | W5 | `partial` | 3 | 0/2/1 |
| [A3/C13](#a3c13) | W6 | `partial` | 3 | 0/1/2 |
| [A3/MCP1](#a3mcp1) | W6 | `partial` | 3 | 0/1/2 |
| [A3/V16](#a3v16) | W6 | `partial` | 4 | 0/2/2 |
| [B2/LSP1](#b2lsp1) | W6 | `partial` | 4 | 1/1/2 |
| [B3/DT4](#b3dt4) | W6 | `partial` | 4 | 0/2/2 |
| [B3/DT5](#b3dt5) | W6 | `partial` | 3 | 3/0/0 |
| [B3/GH1](#b3gh1) | W6 | `partial` | 5 | 1/2/2 |
| [B3/T11](#b3t11) | W6 | `missing` | 4 | 1/1/2 |
| [B3/V13](#b3v13) | W6 | `partial` | 4 | 3/0/1 |
| [B3/W07](#b3w07) | W6 | `partial` | 4 | 1/2/1 |
| [M1/SPEC-TOOLIF](#m1spectoolif) | W6 | `partial` | 5 | 0/3/0 +2? |
| [C4/COST1](#c4cost1) | W7 | `partial` | 4 | 1/3/0 |
| [C4/O07](#c4o07) | W7 | `partial` | 3 | 1/1/1 |
| [C4/OBS1](#c4obs1) | W7 | `partial` | 4 | 1/2/1 |
| [C4/OBS2](#c4obs2) | W7 | `partial` | 4 | 2/1/1 |
| [C4/OBS3](#c4obs3) | W7 | `partial` | 3 | 2/0/1 |
| [F2/BENCH1](#f2bench1) | W7 | `partial` | 3 | 0/1/2 |
| [M1/O07](#m1o07) | W7 | `partial` | 5 | 2/3/0 |
| [C2/UX1](#c2ux1) | W8 | `partial` | 4 | 1/3/0 |
| [C2/UX2](#c2ux2) | W8 | `partial` | 3 | 1/0/2 |
| [C2/UX3](#c2ux3) | W8 | `missing` | 4 | 0/2/2 |
| [C2/UX4](#c2ux4) | W8 | `partial` | 3 | 1/0/2 |
| [C2/UX8](#c2ux8) | W8 | `done` | 4 | 4/0/0 |
| [C3/E03](#c3e03) | W8 | `partial` | 4 | 2/2/0 |
| [D3/C15](#d3c15) | W8 | `partial` | 4 | 0/2/2 |
| [D3/I18N1](#d3i18n1) | W8 | `partial` | 3 | 0/2/1 |
| [D1/APS1](#d1aps1) | W9 | `partial` | 3 | 1/0/2 |
| [D1/V12](#d1v12) | W9 | `partial` | 4 | 0/4/0 |
| [D1/V14](#d1v14) | W9 | `divergent` | 3 | 1/2/0 |
| [D2/V15](#d2v15) | W9 | `partial` | 3 | 0/2/1 |
| [H2/APIREF1](#h2apiref1) | W9 | `partial` | 3 | 1/2/0 |
| [E1/COV2](#e1cov2) | W10 | `partial` | 4 | 1/2/1 |
| [E1/COV3](#e1cov3) | W10 | `partial` | 3 | 0/3/0 |
| [H1/PKG1](#h1pkg1) | W10 | `partial` | 4 | 0/2/2 |
| [H1/VER1](#h1ver1) | W10 | `partial` | 3 | 1/2/0 |
| [H2/CONTRIB1](#h2contrib1) | W10 | `partial` | 3 | 1/2/0 |
| [H2/EX1](#h2ex1) | W10 | `partial` | 3 | 1/2/0 |
| [H2/UDOC1](#h2udoc1) | W10 | `partial` | 3 | 1/2/0 |
| [D2/O12](#d2o12) | - | `removed` | 2 | 2/0/0 |
| **合计** | | | **230** | **63/103/61 +3?** |

---
## W1 — 装配断裂与工具接线

> **本包两个跨条目发现，先读这两条再读逐句：**
>
> 1. **槽位泄漏一处，砸两条验收。** `registry.finishTerminal`（`internal/agent/registry/manager.go`）只翻 `rec.Status`，从不 `delete(m.runtime, …)` —— 全仓 `delete(m.runtime,…)` 只有 3 处且全在错误回滚路径。于是 `runningLocked()` 单调不减，「并发上限」实际是**终身 spawn 预算**。B1/M04b#2 与 C1/M07#2 因此同时不成立，修一处转两条绿。⚠️ 修复时 `internal/agent/batch::TestRunnerSpawnCapExhausted` 会变红 —— 它目前把 bug 钉成了期望行为。根因条目是 F2/LEAK2（W3）。
> 2. **出厂 profile 的可达性天花板。** `DefaultOrchestratorProfile`（`internal/bootstrap/profile.go`）不含 `agent_spawn` / `agent_list` / `agent_resume`、`update_plan` / `checklist_*` / `todo_*`、`image_describe`。这些工具**已注册**（GOV5 只查「allow 里的名字必须已注册」，**不查反向**），所以模型看得到、调得动、然后被 guard 拒。影响 A2/G05#2·#4、B1/M04b#1、B1/M05#1、G/VISION#2、G/VISION-TOOL#2。
>    ⚠️ 别读歪：`config.example.yaml` 的 `profiles.orchestrator` 是 `tools: {allow: ["*"]}` —— 照示例部署的用户这些工具全都可达。所以是「**未配置 profiles.orchestrator 时的出厂默认**不可达」，不是「永远不可达」。加名字进出厂 allow 是**授权面变更**（同 `apply_patch`），走 W5。

### A1/T07/T08 — Shell runtime v2 + 后台 /jobs · 台账 `partial`

> acceptance：长进程返回 session id；可续读/stdin；yield/timeout/输出上限/显式关闭；进程树取消干净；session 关闭按策略回收

**1. 长进程返回 session id** — 已兑现
- 依据：`internal/tools/shell_v2.go::NewShellV2Tools`（`shell_start`）→ `internal/shell/manager.go` Start；九个工具都在出厂 allow 列表（`internal/bootstrap/profile.go::DefaultOrchestratorProfile`）。`internal/bootstrap::TestShellV2EndToEndSpawnsRealProcess` 走**真实 `SecureLaunchFactory`**，断言 `sess.ID` 非空 **且 `sess.PID != 0`**，并显式 `NotContains "no process factory configured"` / `"runtime unavailable"` —— 正好钉死历史上那两种装配断裂。
- 证据形状：返回的 id 能在后续调用中被再次解析成同一 session + PID 指向真实 OS 进程。**骗过去**：只断言返回串非空，或用替换过 `shell.Config.Factory` 的 fake —— 这正是本仓此前所有 shell 测试的形状，也是装配断裂长期没被发现的原因。

**2. 可续读/stdin** — 部分
- 依据：`shell_read`（`shell_v2.go::NewShellV2Tools`）**只有 `id` + `max_bytes`，无 cursor/offset 参数** → `internal/shell/manager.go` `ringBuffer.Read(max)` 返回缓冲区**尾部**，重复调用返回重叠数据，**不是增量游标**。stdin 有 `shell_write_stdin`（`shell_v2.go::NewShellV2Tools`），但未查到「写进去 + 从 read 看到子进程回显」的端到端测试。
- 证据形状：两次 `shell_read` 之间新产生的输出**不重复也不丢失**（第二次返回不包含第一次已返回的字节）；stdin 侧断言 `cat` 类子进程把写入内容回显到输出缓冲。**骗过去**：只调一次 read 断言 contains marker（当前形状）。

**3. yield/timeout/输出上限/显式关闭** — 部分
- 依据：输出上限有（`manager.go::ringBuffer.Write` ringBuffer 按 cap 丢头部）、显式关闭有（`shell_cancel` / `task_shell_cancel`）。**yield 全仓无实现**；`shell_start`/`shell_read`/`shell_wait` 的参数表里**没有任何 timeout 字段**，只有 `GuardedTool` 的 30s/130s 工具级 deadline。
- 证据形状：输出上限断言「写入 cap+N 字节后 `Read` 返回长度 == cap 且内容是尾部那段」；timeout 断言超时后 session 状态变为 timed-out **且进程真的不在了**。**骗过去**：`len(out) <= cap`（对空输出恒真）。

**4. 进程树取消干净** — 未兑现（**实现缺口**）
- 依据：`internal/shell/console_unix.go` `CanKillTreeOnPlatform() == true`，但 `internal/shell` / `internal/secproc` / `internal/tools` **没有任何 `Setpgid` / `SysProcAttr` / `CREATE_NEW_PROCESS_GROUP`** → 杀的是直接子进程，孙进程留孤儿。`StartPTYProcess` 在三个平台文件里一律返回 `ErrPTYUnavailable`。⚠️ 附带**能力谎报**：`CanKillTreeOnPlatform()=true` 让 `ProcessCapabilities.CanKillTree`（`process.go`）对上层撒谎。
- 证据形状：起 `sh -c 'sleep 300 & wait'`，记下孙进程 PID，cancel 后断言**孙进程不可 signal**（`syscall.Kill(pid,0)` == ESRCH）。**骗过去**：只断言 `cmd.Wait()` 返回，或断言 `CanKillTree` 这个 bool 字段的值（对着被测函数自己的产物断言 = 变异盲）。

**5. session 关闭按策略回收** — 部分
- 依据：`internal/shell/manager.go` 有 `Config.IdleTimeout`，但唯一使用点在 `manager.go::Manager.Wait` —— 它是 **Wait 调用内部的 `time.After` 超时分支**（「Wait 等太久就返回」），**不是后台 reaper**。没有 GC goroutine、没有 `MaxSessions`、没有闲置 session 从 map 摘除的路径。
- 证据形状：session 闲置超过 IdleTimeout 后，`shell_read` 返回 not-found（或 list 不含它）**且其 OS 进程不可 signal**。**骗过去**：断言 `Wait` 在 IdleTimeout 后返回 error —— 那只证明调用方超时，session 仍在 map 里活着。

---

### A2/G05 — Plan mode + update_plan/checklist 工具 · 台账 `partial`

> acceptance：plan 模式禁编辑类工具；计划可流式更新；确认后切执行且历史连续；checklist 状态持久

**1. plan 模式禁编辑类工具** — 已兑现
- 依据：三层防线 —— `orchestrator.go` `runnerFor(model, plan)` 用 `filterPlanTools` 把工具**从模型的工具表里摘掉**（按 `runnerCacheKey{model,mode}` 分开缓存）；`internal/tools/permctx.go::Authorize/AuthorizeApprovalRequired` 运行时 `Authorize` 再拦；`internal/api/http/ws.go::Server.ChatWS` WS turn 真的设置了 `PlanMode`。测试：`internal/guard::TestPlanToolAllowed`、`internal/tools::TestAuthorize_PlanMode_DeniesWriteTools`、`internal/api/http::TestChatWS_ModePlan_ProducesReadOnlyTurn`。
- 证据形状：断言 plan turn 的 `tool_result` 里出现「工具不存在/未授权」，且 `filterPlanTools` 后的工具名集合不含 `fs_write`。**骗过去**：断言「目标文件不存在」—— 修复前后文件都不会出现，这是变异盲。

**2. 计划可流式更新** — 未兑现（**实现断在两处**）
- 依据：工具端齐全（`internal/tools/plan.go::PlanTools.updatePlan/PlanTools.checklistAdd/PlanTools.checklistUpdate` 三处 `EmitWorkEvent`）、帧构造齐全（`internal/proto/frame.go::NewPlanUpdate/NewChecklistUpdate`）。但链路断两刀：(a) `TurnOpts.EmitWorkFrame`（`orchestrator.go`）**全仓零生产赋值点** —— 只命中定义、消费处（`::Orchestrator.withTurnContext`）和计划文档，WS/SSE 都没接；(b) `internal/cli/tui/model.go` 的 `applyEvent` switch 里**没有 `plan_update` / `checklist_update` 分支**，当时那两个渲染器只在测试里被构造过，是运行时死代码（名字已随 2026-08-07 的修复删除，故此处不给可解析路径）。
- 现有两个测试都是假证据：`internal/proto::TestNewPlanUpdateNilRowsIsNonNilEmptyChecklist` 与那条直接 new 一个 entry 调 `render` 的渲染测试 —— 典型的「每一跳有单测但接缝没接」，把死代码测绿了。
- 证据形状：端到端 —— WS 连接上发一个触发 `update_plan` 的 turn，**从连接上收到 type=`plan_update` 的帧**，且 TUI `applyEvent` 处理后 `m.entries` 里多出一个条目。**骗过去**：正是现有这两个测试。
- **2026-08-07 结局**：(b) 在 A2 收尾时修了；(a) 直到今天才修
  （`internal/api/http/ws.go` 与 `internal/api/http/chat.go` 各接一次 `EmitWorkFrame`，
  端到端测试 `internal/api/http::TestChatWS_ToolWorkEventReachesTheClient` /
  `internal/api/http::TestChatSSE_ToolWorkEventReachesTheClient`）。

**3. 确认后切执行且历史连续** — 未兑现
- 依据：模式切回有（`internal/cli/tui/commands.go::cmdPlan/cmdPlanOff` `prePlanMode` 保存/恢复），但**没有任何测试断言 history 连续**。`internal/cli/tui::TestCmdPlan_EnterAndExitWS` 只测 TUI 侧模式字段来回，不碰 history。
- 证据形状：同一 WS 连接上：plan turn（产生若干 assistant 消息）→ `/plan-off` → 执行 turn，断言第二个 turn 传给模型的 history **前缀逐条等于**第一个 turn 结束时的 history（不截断、不重复、tool_call/result 配对不断）。**骗过去**：只断言 `m.permMode` 从 plan 变回 default。

**4. checklist 状态持久** — 部分
- 依据：SQLite 落盘是真的（`internal/task/work/store.go`），`internal/task/work::TestManagerChecklistAPIs` 覆盖 store/manager 层往返。但 `internal/tools/plan.go` 的 kernel 从 **ctx** 取 `work.ManagerLike`（`taskctx.go::TaskManagerCtx/WithTaskManager`），**工具层测试全部走 `work.NewFakeManager()`**（内存 map），「update_plan 工具 → 真实 SQLite」这段接缝无测试。附：`update_plan`/`checklist_*`/`todo_*` 不在出厂 allow 列表。
- 证据形状：用真实 `work.Manager`（真 SQLite **文件**）经 ctx 绑进 `update_plan`，调用后**新开一个 Manager 打开同一个 DB 文件**断言 checklist 读得回来。**骗过去**：用 `FakeManager` 断言「写进去能读出来」（内存 map，恒真）。

---

### B1/M04b — 持久化 + 并发上限 + 输出契约 · 台账 `partial`

> acceptance：重启后可 list/resume；并发上限生效；输出 5 段可解析；父可消费 EVIDENCE

**1. 重启后可 list/resume** — 部分
- 依据：manager 层已兑现且测试很强 —— `internal/agent/registry::TestResumeRestoresSavedConstraintsAndEmitsEvent` 落一份 state.json 到盘、用**不同 bootID** 新建 Manager、断言 `List(true)` 里状态是 `Interrupted`、Resume 后逐字段断言 `AllowedTools`/`Instruction`/`ModelOverride`/`ReasoningEffort`/`Custom` 全部从盘还原，还断言 prompt 回落。弱点：那份 state.json 是测试**手写**的 `persistedState`，不是上一个 Manager 实例真正 snapshot 出来的；两个 Manager 串起来的耐久性测试没有。工具面（`agent_list`/`agent_resume`）不在出厂 allow 列表。
- 证据形状：Manager A spawn + 跑完 → `A.Close()` → Manager B 用同一 Path 打开 → 断言 `B.List(true)` 看得到 A 写的记录且 `B.Resume` 成功。**骗过去**：同一 Manager 实例内做 save/load 往返。

**2. 并发上限生效** — 未兑现（**实现缺口，见本包开头第 1 条**）
- 依据：gate 存在（`manager.go::Manager.Spawn/Manager.Resume`）但槽位永不释放 → 终身预算而非并发上限。生产后果：`internal/tools/batch.go` 的 `CappedRetries:50 / CappedBackoff:100ms` 意味着行数超过 registry cap（默认 10）的批任务，每行空转 5s 后必然失败。现有两条测试（`TestSpawnRespectsCapAndReturnsSpawnErrCap`、`internal/tools::TestAcceptance_WorkflowUsesSharedLimitAndList`）都**只测上界方向**（被阻塞的 agent 仍在跑时第 N+1 个被拒），**没有任何测试测释放方向**。
- 证据形状：cap=1，spawn A 并**等它 terminal**，然后断言 spawn B **成功**（而非 `SpawnErrCap`），并断言 `len(m.runtime)` 回到 0。**骗过去**：只测「跑满时拒绝」—— 上界方向对一个永不释放的计数器同样成立。

**3. 输出 5 段可解析** — 部分
- 依据：有 5 段词表（`internal/tools/agentroles.go` `outputContractPrefix` 生成 SUMMARY/CHANGES/EVIDENCE/RISKS/BLOCKERS）与边界识别（`subagent.go` `knownResultSections` + `extractResultSection`），但 **`knownResultSections` 的唯一消费者是 `matchResultSection`，唯一被抽取的段是 EVIDENCE** —— 没有 5 段解析器，另外四段无任何消费者。**接缝断裂**：`PromptPrefix` 只在 `orchestrator.go::managedTurnRunner.Run` 的 `managedTurnRunner` 里注入（依赖 `registry.RoleFromContext`，即 `agent_spawn` 托管路径），而出厂可达的 `agent_start`（`internal/tools/agent.go::AgentTools.streamStartAgent`）**完全不设 role、不加 prefix** → 子代理从没被要求输出 5 段。现有测试 `TestExtractResultSection`（只解析 EVIDENCE）与 `TestRolePromptPrefixCarriesOutputContract`（只断言提示词里有那 5 个字符串）都不是「输出可解析」的证据。
- 证据形状：给一份完整 5 段文本，断言**五段各自都能被取出且内容正确**，缺段/乱序时返回可判别的错误；再断言可达入口（`agent_start`）真的注入了契约提示。**骗过去**：`Contains(prefix, "BLOCKERS")`。

**4. 父可消费 EVIDENCE** — 已兑现
- 依据：`internal/tools/subagent.go` `ParentWorkingSetHint`，调用点在 `agent_start`（`agent.go::AgentTools.streamStartAgent`）与 analysis 两个终端路径。测试 `internal/tools::TestAgentStart_AppendsParentWorkingSetHint` / `TestAnalysis_AppendsParentWorkingSetHint` / `TestAgentStart_NoEvidenceIsPassedThroughVerbatim` —— marker 常量化后逐字断言、断言 evidence 里的具体路径出现在输出里、断言原始 result 逐字保留为前缀、并测了「无 EVIDENCE 必须原样透传」的反向。
- ⚠️ 实现没问题，但它的**输入**（5 段结果）在生产可达路径上不会出现（见 #3）→ 生产里多半是 no-op。
- 证据形状：正是现在这个（marker + 具体 evidence 内容 + 前缀保留 + 反向透传）。

---

### B1/M05 — 子代理 7 角色 · 台账 `partial`

> acceptance：7 角色可选；权限矩阵符合；越权拒绝；别名大小写不敏感；未知值返回可接受集

**1. 7 角色可选** — 部分
- 依据：七个 RoleDef 齐（`internal/tools/agentroles.go::AgentRoles`），`role` 参数只挂在 `agent_spawn` 上（`agent.go::NewAgentTools`），而 **`agent_spawn` 不在 `DefaultOrchestratorProfile` 的 allow 列表**（`profile.go` 只有 `agent_start`/`workflow_start`/`analysis`/`summarize`）。`internal/tools::TestRoleCatalogCoversSevenRoles` 用 `ElementsMatch` 断七个名字（改目录会红，真证据），但**没有任何测试断言这七个角色在出厂 profile 下可被调用**。
- 证据形状：`guard.Check(DefaultOrchestratorProfile(), Action{Tool:"agent_spawn"})` 断言 Allow；或一条经出厂 profile 的 turn 里成功用 role 起子代理。**骗过去**：`ElementsMatch` —— 它只证明 Go 里有七个 struct。

**2. 权限矩阵符合** — 部分
- 依据：`internal/tools::TestRoleAllowlistOnlyTightensParent` **只钉死矩阵的 Policy 一半**（explore/plan/review 的 `ReadOnlyShell`/`WritePatterns` + general/implementer/custom 的 `Policy == nil`）。**`AllowedTools` 的具体工具名列表没有任何逐字断言** —— 改写任一角色的工具清单不会让任何测试变红。且 **`verifier` 的 Policy 根本没进任何断言分支**（既不在具名三个里，也不在 nil 三个里）。
- 证据形状：对七个角色逐个 `require.Equal(expectedTools, role.AllowedTools)`（表驱动、期望值字面量写在测试里），外加 verifier 的 Policy 断言。**骗过去**：只断言 Policy 的三个字段（现状）。

**3. 越权拒绝** — 已兑现
- 依据：`internal/tools::TestSpawnIntersectsRoleWithCallerTools`（explore + 请求 fs_read/fs_write/time_now → 断言结果**逐字等于** `"fs_read,time_now"`，不是长度）、`TestSpawnRejectsFullyDisjointToolSets`、`TestSpawnEmptySideMeansNoExtraRestriction`、`TestSpawnCustomRoleRequiresExplicitTools`。`spawnCapture` 从 `ManagedRunnerFactory` 捕获**真正交给 runner 的 allowlist** —— 这是这条链上唯一的可观察量，不是 spy 计数。
- 证据形状：正是现在这个（捕获下发给 runner 的实际 allowlist 逐字比对，且两个方向都测：不能扩大、空交集必须报错而非「继承全部」）。

**4. 别名大小写不敏感** — 部分
- 依据：`agentroles.go` `LookupRole` 做 `ToLower(TrimSpace(...))`，行为对。`internal/tools::TestSpawnRoleNameIsCaseInsensitive` 前半段（`LookupRole("  Review ")` 名字相等）是真证据；**后半段是弱断言** —— `{"role":"ExPlOrE"}` 后只比 `len(allowed) != len(want)`，而 **explore 和 review 的 AllowedTools 恰好是同 5 个工具**，长度检查区分不出选中的是哪个角色。
- 证据形状：把长度比较换成 `require.Equal(MustRole("explore").AllowedTools, allowed)`；加一条「大小写混拼与全小写产出的 allowlist 完全一致」的对照。**骗过去**：长度比较（现状）。

**5. 未知值返回可接受集** — 已兑现
- 依据：`internal/tools::TestSpawnRejectsUnknownRoleAndListsValidOnes` —— 断言 err 非 nil、错误串回显坏名字 `reviewr`、并**遍历 `AgentRoleNames()` 逐个断言都出现在错误串里**（加角色但忘了进枚举 → 红）。
- 证据形状：正是现在这个。

---

### C1/AU1 — Automations（计划任务） · 台账 `partial`（本次拆解后由 `done` 退回）

> acceptance：可创建计划任务；按时触发入队；生命周期可控；持久化；approval 门禁

**1. 可创建计划任务** — 已兑现
- 依据：`internal/agent/automation/manager.go` Create + `schedule.go` NextRun；`internal/agent/automation::TestManagerCreateAssignsIDAndNextRun`。
- 证据形状：返回 ID 非空 **且 NextRun 是按 schedule 算出的具体时刻**（不是零值、不是 now）。**骗过去**：只断言 `err == nil`。

**2. 按时触发入队** — 已兑现
- 依据：`internal/agent/automation/scheduler.go` Tick；`internal/agent/automation::TestManagerTickEnqueuesDueSlotOnce`（台账记录它从 `TestManagerRunNowIdempotentPerKey` 改名 —— 原名调的是 Tick 不是 RunNow，覆盖成立但名字误导，改名是对的）。
- 证据形状：注入假时钟，断言到点后队列恰好多一项**且**同一 slot 重复 Tick 不重复入队。**骗过去**：不推进时钟，断言队列长度 >= 0。

**3. 生命周期可控** — 部分 ⚠️（**这是建议回退的第一条依据**）
- 依据：`internal/tools::TestAutomationCreateReadUpdatePauseResumeDeleteRun`（`automation_test.go`）走工具层全链，create/read/list/run 有实质断言（能 unmarshal、ID 非空、run 返回含 `"status":"queued"`）。但 **update / pause / resume / delete 四步只有 `require.NoError(err)` + 丢弃返回值**。而 `GuardedTool.InvokableRun` 把拒绝写进 **result 串**而不是 Go error（同文件 `TestApprovalGatedToolsDeniedWithoutCallback` 正是靠 `Contains(result,"permission denied")` 判定的）→ 这四步的 `NoError` **对「操作实际没生效」完全是盲的**，属吞错的近亲。
- 证据形状：pause 后 read 断言 `status == paused`、resume 后断言回 `active`、update 后断言 prompt 真变成 "do Y"、delete 后断言 read 返回 not-found。**骗过去**：正是那四步的裸 `NoError`。

**4. 持久化** — 部分 ⚠️（**建议回退的第二条依据**；台账已自记为「已知弱点」）
- 依据：`internal/agent/automation::TestRepositorySaveLoadRoundTrip` 在**同一个 live store** 上新建 Repository 做往返，没有重开进程/重开文件 DB 的跨重启断言。
- 证据形状：写入 → 关闭 DB 句柄 → 用同一文件路径重开 → 断言读得回来且 NextRun/status 未丢。**骗过去**：同一连接上 new 一个 Repository（那只证明 struct 无状态）。

**5. approval 门禁** — 已兑现
- 依据：`internal/tools::TestApprovalGatedToolsDeniedWithoutCallback` —— profile 明确 allow 全部八个工具（排除「被 profile 拦掉」这个混淆变量），断言 result 同时含 `"permission denied"` 与 `"requires explicit approval"`；配 `TestApprovalPromisedInDescriptionIsEnforced`（遍历全部 9 个 + `assert.Equal(len(all), checked)` 防漏）。
- 证据形状：正是现在这个（**先把 profile 放开**，再断言拒绝来自 approval 维度而非 tools 维度）+ 遍历式防漏计数。

> **已回退 `partial`**（2026-08-04）：按与 A1/T07/T08、B1/M04b、G/VISION-TOOL 相同的尺子，#3 与 #4 都未达「已兑现」。#3 的指控经变异测试证实 —— 把 `Manager.Update` 与 `Manager.Delete` 掏空成 `if true { return }` 后，`go test ./internal/tools -run '^TestAutomationCreateReadUpdatePauseResumeDeleteRun$' -v` 仍输出 `--- PASS`。四条 `ledger: C1/AU1#n` marker 已随之删除。

---

### C1/M07 — CSV 批量 agent jobs · 台账 `partial`

> acceptance：可提交批量任务；限并发；逐项结果+汇总可查

**1. 可提交批量任务** — 已兑现
- 依据：`internal/agent/batch/input.go` CSV/structured 解析（稳定 Index）、`runner.go` `Runner.Run`、`internal/tools/batch.go` `agent_batch`（在出厂 allow 列表里）。测试：`internal/agent/batch::TestRunnerSpawnsPerRowAndPreservesIndex`（3 行 → `Results[i].Index == i` 且 Success==3）、`TestRunnerPromptIncludesBasePromptAndRowJSON`、9 个 `TestParseCSV_*`、`internal/tools::TestAgentBatchCSVInputEndToEnd`。
- 证据形状：断言每行 prompt 含该行自己的 `row_index` 与 row JSON 且**不含**别行数据（现状即是）。

**2. 限并发** — 未兑现（同 B1/M04b#2 的同一根因）
- 依据：`runner.go` **自己不设并发上限**，完全依赖 registry cap + `spawnWithRetry` 重试，而 registry cap 因槽位泄漏是终身预算。两个候选证据都是假的：
  - `internal/agent/batch::TestRunnerCapsAtRegistryMaxConcurrent` —— **恒真空壳**，cap=4 / 3 行，注释自陈「we don't test overflow retry here」，唯一断言是 `Success == 3`。
  - `internal/agent/batch::TestRunnerSpawnCapExhausted` —— **是泄漏的目击者，不是限并发的证据**：cap=1 / 2 行，断言「第 0 行已跑完，第 1 行仍被判超限并耗尽重试」。它把 bug 钉成了期望行为，**修好泄漏后它会变红**。
- 证据形状：cap=2 / 5 行，用带门闩的 spawn 记录**同时在跑的峰值**，断言 `peak <= 2` **且** `report.Success == 5`（全部完成，没有一行因耗尽重试而失败）。**骗过去**：正是上面两个。

**3. 逐项结果+汇总可查** — 已兑现
- 依据：`Report{Results, Total, Success, Failed, Canceled}` + 按 index（而非完成顺序）排序。`internal/agent/batch::TestRunnerPerItemErrorRetention` 用 `failRow` 按 prompt 内容确定性地让第 1 行失败（避开并发下按调用序号映射的不确定性），断言 `Success==2 / Failed==1`、`Results[1].Output == ""`、`Results[1].Error` 含 `"row-1-boom"`；配 `TestRunnerCancellationPendingRowsMarkedCanceled`、`TestRunnerAgentCancelledStatus`。
- 证据形状：正是现在这个（单行失败不影响其他行、失败行 Output 必须被清空、汇总计数三分）。

---

### C1/RLM1 — rlm_query 批量并行 LLM · 台账 `partial`

> acceptance：1-16 并发；顺序对应；cap 生效；成本显著低于 sub-agent

**1. 1-16 并发** — 已兑现
- 依据：`internal/agent/rlm/runner.go`；`rlm_query` 是条件注册工具（`profile.go` `ConditionalProfileTools`，需 `batch.rlm_model` 配置，由 `extendProfileWithConditionalTools` 在真注册时才加进 allow）。测试 `internal/tools::TestRLMQueryRejectsMoreThanSixteen`。
- 证据形状：n=16 接受、n=17 报错且错误串点名上限。

**2. 顺序对应** — 已兑现
- 依据：`internal/agent/rlm::TestRunUsesGenerateAndPreservesOrder`。
- 证据形状：让 fake model 对不同输入返回**可区分**的输出，并**故意让完成顺序与提交顺序相反**，断言 `out[i]` 对应 `in[i]`。**骗过去**：fake 不看入参、硬编码同一个返回值 —— 那样任何排列都「顺序正确」。这是 fake-太宽 的经典位置，翻牌前建议实测复核该 fake 是否读入参。

**3. cap 生效** — 已兑现
- 依据：`internal/agent/rlm::TestRunCapsConcurrencyAtSixteen`。
- 证据形状：用原子计数记录同时在飞的调用峰值，断言 `peak <= 16` **且 `peak` 确实达到过 16**（否则可能只是串行）。**骗过去**：只断言全部返回。

**4. 成本显著低于 sub-agent** — 未兑现（**N 类：测量不是断言**）
- 依据：`SelectRLMModel` 强制 `cost_class=cheap` 是**声明**便宜，不是**实测**便宜。全仓无任何测试或基准比较 `rlm_query` 与 `agent_spawn` 的 token / 费用。
- 证据形状：这条本质是测量。可承载的行为化改写：断言 `rlm_query` 一次批量调用的 **LLM 调用次数 / prompt token 总量**严格小于同等任务经 `agent_spawn` 的量（用 fake model 计数），或断言 RLM 选中 provider 的 `cost_class` 严格低于主 model 的。**若要翻终态，先把这条子句改写成可断言形式**。

---

### G/VISION — 能力声明 + 辅助模型 + turn 分流 · 台账 `partial`

> acceptance：主多模态：图直接通过消息内容到达；主非多模态+有辅助：占位+image_describe 走通；无辅助：error 而非静默

**1. 主多模态：图直接通过消息内容到达** — 已兑现
- 依据：`internal/agent/orchestrator/multimodal.go` `ApplyImages` → `appendImageParts`，把每张图做成 `schema.ChatMessagePartTypeImageURL` 的 data-URL part 挂到尾部 user 消息；`IsMultimodal` 查 `multimodalMap`（nil map = 无 provider 声明多模态）。`appendImageParts` 是 **copy-on-write**（`last := *history[n-1]` 后重建切片元素），不污染调用方 history —— 这在 WS 重试路径上是必需的。测试：`TestE2E_MultimodalMainDirectlyUnderstandsImage` + WS/SSE/v1 三条传输各自的单测。
- 证据形状：断言 fake model 收到的消息里**真有 `ChatMessagePartTypeImageURL` part 且 MIME/base64 内容正确**，外加「同一 history 切片传两次不会出现两份图」的回归。**骗过去**：只断言 `err == nil` 或只断言 history 长度变了。

**2. 主非多模态+有辅助：占位+image_describe 走通** — 部分（**接缝无测试**）
- 依据：`multimodal.go` `appendPlaceholders` → `imageStore.Put` + `Placeholder(id)` 追加 `[image:img-N|src|WxH fmt]`；`image_describe` 在 `internal/tools/vision.go::NewImageDescribeTool` 按 id 从同一 store 取图交辅助模型。两端各有测试（`TestE2E_NonMultimodalMainUsesPlaceholderAndStore` / `TestImageDescribeByIDReturnsAuxDescription`），但**没有任何测试把 `ApplyImages` 产出的 `img-N` 喂给同一个 store 上的 `image_describe`**。附：`image_describe` 不在出厂 allow 列表 → 出厂默认下模型看得到占位符却调不动描述工具。
- 证据形状：一个测试内：真 store → `ApplyImages`（非多模态）→ 从产出的占位文本里正则解出 `img-N` → 喂给绑同一 store 的 `image_describe` → 断言拿回辅助模型的描述。**骗过去**：现在这两个独立测试（各自 new 自己的 store）。

**3. 无辅助：error 而非静默** — 未兑现（**实现缺口，不只是测试缺口**）
- 依据：`multimodal.go` `ApplyImages` **完全不看 visionAux 是否配置** —— 非多模态就直接进 `appendPlaceholders`；而 `appendPlaceholders` 第一行 `if o.imageStore == nil { return history }` 是**静默丢图**，base64 解码失败 `continue`、`Put` 失败 `continue`、`ph.Len()==0` 原样返回 —— **四条静默路径，零 error 出口**。turn 层与 API 层拿不到任何错误。唯一相关的 `internal/tools::TestImageDescribeNoAuxReturnsConfigError` 是**工具层**的，证明的是「模型恰好调了 image_describe 时会看到配置错误串」，不是 turn 分流层报错 —— 拿它顶这条属凑数。
- 证据形状：`ApplyImages` 返回 error（或 turn 层前置校验）：主模型非多模态 + visionAux 未配置 + 带图 → 断言**turn 直接以可读错误终止**（WS 上收到 `error` 帧），而不是把图默默扔掉后照常发问。**骗过去**：拿工具层的 config-error 顶（现状）。

---

### G/VISION-TOOL — image_describe + image store + 五入口 · 台账 `partial`

> acceptance：五入口各自可产生图像附件；image_describe/id-ref+path-ref 走通；超限/越权被拒；费用纳入 /cost

**1. 五入口各自可产生图像附件** — 部分
- 依据：服务端三条传输入口各自声明了 `Images` 字段（WS 的 `proto.ClientFrame`、SSE 在 `chat.go` handler 内的匿名结构体、v1 在 `internal/api/v1/types.go` —— 三处**各自**声明，`json.Decode` 对未知键静默忽略，正是 CLAUDE.md 记的那个坑）；path-ref 展开在 `internal/tools/pathref.go` + `orchestrator.expandPathRefs`。**TUI 触发面（粘贴/拖拽产生附件）未做**，台账已移交 W8。
- 证据形状：五个入口逐一断言「附件从入口进去后，出现在传给模型的消息（多模态）或 store（非多模态）里」。**骗过去**：只测 JSON 能反序列化出 `Images` 字段 —— 那证明不了它被用上。

**2. image_describe/id-ref+path-ref 走通** — 部分
- 依据：id-ref = `internal/tools/vision.go`；path-ref = `internal/tools/pathref.go`（⚠️ 这是**服务端只处理图片的** path-ref 展开，与 TUI 的 `@` 补全无关 —— 见 C2/UX3）。同 G/VISION#2，id-ref 的端到端接缝（占位→描述）无测试；`image_describe` 不在出厂 allow 列表。
- 证据形状：同 G/VISION#2；path-ref 侧断言「提示里写路径 → 展开成真实图像 part/占位」。

**3. 超限/越权被拒** — 未查证
- 依据：本轮未核到大小/数量上限与路径越权拒绝的实现与测试。**翻牌前必须补查。**
- 证据形状：断言超过字节上限的图返回**明确 error**（不是截断、不是静默跳过）；断言 workdir 之外的 path-ref 被 `fs_read` 鉴权拒绝（`pathRefGuardTool = "fs_read"` 是鉴权用的工具名，不是给用户看的提示）。**骗过去**：断言超限时「没崩」。

**4. 费用纳入 /cost** — 未兑现（**实现缺口：只写不读**）
- 依据：`/cost` 存在（`internal/cli/tui/commands.go::commandTable/cmdCost` → `requestStatusBlock` → `proto.NewGetStatus`），辅助模型 token 也确实被收集（`internal/tools/vision.go` `VisionUsageFunc` → `bootstrap.go::Build` `VisionUsage: &visionUsageSink`）。**但 `visionUsageAccumulator` 是只写的** —— `grep VisionUsage` 全仓只有类型定义、`App` 字段、装配点，以及 `internal/bootstrap::TestVisionUsageAccumulator_Add` / `_AddConcurrentSafe` 两个自测，**没有任何读取方** → 累加值进不了 status 帧，也就进不了 `/cost`。那两个自测是「累加器自证」，不碰「这个数字有没有到达 /cost」。
- 证据形状：调一次 `image_describe`（辅助模型返回已知 token 数）→ 发 `get_status` → 断言返回帧的 usage 字段**包含**那部分 token。**骗过去**：正是现在的 accumulator 单测。

---

## W2 — goal loop 预算与用量

> **本包的承重结论：token 预算在生产里依然完全失效。** `Budget.MaxTokens` 在整个非测试代码里**从未被赋值** —— `cmd/yanshi/main.go` 只设 `MaxIterations`，`runGoal` 的 flag 集（`main.go`）没有 `-max-tokens`，`internal/config` 也没有对应字段。而 `overBudget()` 是 `l.cfg.Budget.MaxTokens > 0 && …` → **在发布出去的 `yanshi goal` 里恒为 false**。累计器建好了，闸门没接线。全部现有单测都在测试里手动构造 `Budget{MaxTokens: 100}`，生产从不这么构造。

### B0/TD1 — SpentTokens 死代码 / goal loop token budget 失效 · 台账 `partial`

> ⚠️ **这条 acceptance 本身形状有问题**，见文末「acceptance 需要重写的条目」。它是一段**实测表引文**被分号切碎的产物：子句 1 前半是元陈述（「路线图未给验收标准」）不可断言，子句 4 **把具体测试名写进验收**（把证据当验收 —— 任何人只要保留同名测试壳就能"满足"它），引号在子句 1 开、子句 4 闭。

**1. `路线图未给 B0 独立验收标准，仅在实测表里给出判定依据：'goalloop/usage.go 完整 UsageSink+addUsage/usageFromMeta`** — 已兑现（就其中可断言的后半段而言）
- 依据：`internal/agent/goalloop/usage.go`（UsageSink）/`::UsageSink.Add`/`::UsageSink.Snapshot`/`::usageFromMeta`/`::addUsage`。测试 `TestUsageSink_AddAccumulates`、`TestUsage_SnapshotIsCopy`、`TestUsage_Total_FallsBackToInOutSum`、`TestUsageFromMeta`、`TestAddUsage_NilSafe`。**掏空实验**：`addUsage` 直接 return → `TestLLMTierer_RecordsUsage` / `TestQualityEvaluator_RecordsUsage` / `TestEvaluators_SharedSinkSumsAcrossBoth` FAIL，非变异盲。
- 证据形状：断言 sink 快照的三个字段等于 provider 上报值之和（现状即是）。**骗过去**：只断言 `Add` 不 panic，或只断言字段存在。

**2. `loop.go spent()→sink、overBudget() 在循环顶 & plan 后双检查`** — 部分（**双检查只守住了一半**）
- 依据：实现 `loop.go`（spent）/`::Loop.overBudget`/`::Loop.Run`（循环顶）/`::Loop.Run`（plan 后）。plan 后有真证据 `TestLoop_BudgetCheckMidIteration_AfterPlan`（删掉 `::Loop.Run` 即 FAIL，它断言 `impl.Calls()==0`）。**循环顶无有效证据** —— **掏空实验**：删除 `loop.go::Loop.Run` 整个循环顶检查 → `go test ./internal/agent/goalloop/` **全绿**。原因：`TestLoop_BudgetStopsOnAccumulatedUsage` 只断言 `StopReason==token_budget` 与 Summary 含 `"budget exceeded"`，而这两条被 plan 后的检查同样满足 —— 两个检查生成的 Decision 只差 `"before iteration"` / `"after plan"` 一个词，测试恰好不看这个词。附：该测试的注释声称「iter 2 的顶部触发」，实际 sink 预充值后是 iter 1 顶部触发，注释与行为已不符。
- 证据形状：**必须区分两个检查点** —— 循环顶要么断言 Summary 含 `"before iteration"`，要么用记数 planner 断言 `Plan` 被调用 **0** 次（预充值超预算时不应进 Plan）。**骗过去**：任何只看 `StopReason` / `"budget exceeded"` 子串的断言。

**3. `planner/evaluators/tier 全接 Sink`** — 已兑现
- 依据：`planner.go::LLMPlanner.Plan`、`evaluators.go::IntentEvaluator.Evaluate`（Intent）/`::QualityEvaluator.Evaluate`（Quality）、`tier.go::LLMTierer.Tier`（LLMTierer）、`tier.go`（`EvaluatorsForTier` 把 sink 盖章进两个 LLM 评估器）。五条 `*_RecordsUsage` 测试断言「sink 收到模型上报的 token 数」，掏空 `addUsage` 即 FAIL。`TestEvaluatorsForTier_StampSink` 单看是接线 spy，但有前五个兜底。
- 证据形状：断言 sink 总量 == 该组件 Generate 返回的 `ResponseMeta.Usage`。**骗过去**：只断言 `evals[1].(IntentEvaluator).Sink == sink`（字段接线 spy）。

**4. `TestLoop_BudgetStopsOnAccumulatedUsage 等测试守护'`** — 部分
- 依据：该测试存在且非空壳，但如 #2 所述它守不住「循环顶」那一半。
- ⚠️ 这条子句**把证据当验收**，本身不是行为主张。见文末。

**5. `隐含目标为原缺口'budget 控制完全失效'被修复`** — 未兑现（**本包虚报风险最高的一条**）
- 依据：见本包开头。另外两处：`Budget.SpentTokens`（当初被点名的死代码字段，`types.go`）仍在，**无任何生产调用点写它**，只被 `spent()` 的 pre-G02 回退分支读（`loop.go`）→ 生产上恒读 0；T0–T2 轻量路径（`main.go::runGoal`）根本不把 `loopSink` 交给 `runLightweightGoal`，那条路径持久化的 usage **恒为零值**。原缺口在**用户可见层面依然完全失效**，只是从「没有累计器」变成「有累计器但没有阈值入口」。
- 证据形状：端到端 —— 给 `yanshi goal` 传一个 token 上限（新增 flag 或 config），跑到超限，断言进程以 `stop_reason=token_budget` 结束。**骗过去**：现有全部单测（它们在测试里手动构造 `Budget{MaxTokens:100}`）。

---

### F2/LEAK3 — ACPImplementer usage 回流 · 台账 `partial`

> acceptance：ACP turn usage 进 sink；budget 含子进程；解析失败安全降级

**1. ACP turn usage 进 sink** — 部分（**实现在，测试是假证据**）
- 依据：`internal/agent/goalloop/implementer.go` `usageForwarder()`，接线于 `::worker.runWithGit`与 `::worker.runWithAutoVCS`；`ACPImplementer.Sink` → `worker.sink`；生产接线 `cmd/yanshi/main.go::runGoal`。唯一测试 `TestACPImplementerWorkerAccumulatesSubprocessUsage` 是**变异盲**：它在测试文件里**手抄了一份 onEvent 闭包**（注释自陈「Re-constructed here so the test is independent of the spawn path」）然后测自己抄的那份 —— `usageForwarder` 在**全仓测试中零引用**。**掏空实验**：`usageForwarder` 改成 `return nil` → `go test ./internal/agent/goalloop/` **全绿**。
- 证据形状：直接调 `w.usageForwarder()` 拿到闭包再喂事件；或（更强）用 `acp.FakeAgent` + `UsageReports` 走一遍 `worker.run`，断言 sink 收到子进程 token。**骗过去**：任何在测试里重写闭包的写法。

**2. budget 含子进程** — 未兑现
- 依据：链路在（forwarder → 共享 sink → `Loop.spent()`），但**没有任何测试把 ACP 事件一路驱动到 `overBudget()`**；叠加 B0/TD1#5 的 `MaxTokens` 恒 0，生产上子进程 token 再多也不会触发预算停止。
- 证据形状：用一个 sink，喂 ACP usage 事件把它推过 `MaxTokens`，断言 `Loop.Run` 返回 `StopReason=token_budget`。**骗过去**：只断言 sink 数值（不接 Loop）。

**3. 解析失败安全降级** — 已兑现
- 依据：`internal/acp/client.go` `parseUsageReport`（双 shape 探测 + recover + 全零拒绝），调用点 `::Client.handleNotify`。七条测试（`TestParseUsageReport_MalformedPayload`、`TestParseUsageReportZeroOnly`、`TestParseUsageReportAltFmtZeroOnly`、`TestParseUsageReportAltFormatFailsFirstUnmarshal`、`TestParseUsageReportPanicRecover`、`TestClientHandleNotifyUsageReportAltFormat`、`TestNoUsageReportDoesNotSetUsage`）。**掏空实验**：掏空全零守卫 → 两条 ZeroOnly 测试 FAIL，非变异盲。
- 证据形状：畸形/全零输入 → 返回 nil 且 turn 继续（现状即是）。

---

### M1/G02 — Goal Token 预算累计（UsageSink） · 台账 `partial`

> acceptance：SpentTokens 随 LLM 调用累计；预算耗尽可靠停止并把原因持久化

**1. SpentTokens 随 LLM 调用累计** — 部分
- 依据：planner / 两个 evaluator / LLMTierer 均已接（见 B0/TD1#3，五条真证据）。**两个漏点**：(a) ACP 子进程那条（F2/LEAK3#1，测试是假证据）；(b) **T0–T2 轻量路径完全不喂 sink**（`cmd/yanshi/main.go::runGoal`，`runLightweightGoal` 签名里没有 sink）→ 那条路径持久化的 usage 恒为零。
- 证据形状：对**每条会调模型的生产路径**断言其 usage 出现在共享 sink 里。**骗过去**：只覆盖 planner/evaluator 而不覆盖 ACP 与轻量路径（现状）。

**2. 预算耗尽可靠停止并把原因持久化** — 部分（「停止」生产不可达；「持久化」只有半个断言）
- 依据：停止 `loop.go::Loop.Run`（但 `MaxTokens` 生产恒 0）；持久化 `record.go` `NewRunRecord` + `cmd/yanshi/main.go` `persistGoalRun` → `kv` 表 `goalrun:<unix>`。`TestNewRunRecord_FieldsAndReason` / `TestRunRecord_RoundTripsJSON` 真断言 StopReason 进结构体与 wire；但 `cmd/yanshi::TestPersistGoalRun_WritesRecord` 是**弱证据** —— 只断言 `SELECT COUNT(*) … LIKE 'goalrun:%' >= 1`，且传的是 `Decision{Complete: true}`。**从来没有一个测试把 `StopReason=token_budget` 写进 store 再读回来断言**；把 `NewRunRecord` 里的 `StopReason: decision.StopReason` 改成 `""`，`persistGoalRun` 侧不会红。
- 证据形状：`persistGoalRun(st, tier, Decision{StopReason: StopReasonTokenBudget, …}, …)` 后从 kv 读回该行、`json.Unmarshal` 成 `RunRecord`、断言 `rec.StopReason == "token_budget"` 且 `rec.Usage.TotalTokens` 非零。**骗过去**：`COUNT(*) >= 1`。

---

### M1/G03 — T0–T4 难度路由接通 · 台账 `partial`

> acceptance：每个 tier（T0–T4）都产生真实结果；auto/强制 tier 可测；升级不再静默退出

**1. 每个 tier（T0–T4）都产生真实结果** — 部分（**T3/T4 无任何结果断言**）
- 依据：`tier.go::Tier.Path` `Path()` 分流；T0–T2 → `cmd/yanshi/main.go` `runLightweightGoal`（加载 `skills/dev-*` 的 SKILL.md → `app.Orch.Query`）；T3–T4 → `main.go::runGoal` 完整 loop；五个 tier 的 skill 目录都存在。T0 有 `TestRunLightweightGoalSkillFound`（断言 `dec.Complete`）与 `TestRunGoalRealPathLightweight`；T2 有 `TestRunLightweightGoal_NonTier4Hint`；T1 无专门测试（同一条代码路径，风险低）。**T3/T4 的 `TestRunGoalRealPathLoopSetup`（2026-08-07 已删除，故此处刻意不带包路径 —— GOV9 对解析不出的路径放行）是吞错** —— 它显式 `_ = code`（`coverage_test.go`）丢弃退出码，注释自陈「Any non-timeout result means the setup path ran cleanly」→ **只可能因 20s 超时而失败**，loop 返回什么都算过。这是覆盖率打卡，不是「产生真实结果」的证据。
- 证据形状：T3/T4 需要一条不依赖外部 CLI 的路径（Implementer 换成 `FakeImplementer`，或把 `ACPImplementer` 的 agent 指向 `acp.FakeAgent`），断言 Decision 的 Complete/StopReason 与迭代数。**骗过去**：任何丢弃退出码 / 只防超时的测试。

**2. auto/强制 tier 可测** — 已兑现
- 依据：`tier.go` `ResolveTierFlag`（auto → `RuleTierer`；t0..t4 强制；其余报错）、`cmd/yanshi/main.go` `resolveGoalTier`。测试 `TestResolveTierFlag`（三分支）、`TestRuleTierer`、`cmd/yanshi::TestResolveGoalTier`、`runGoal(-tier t9)` → `exitUsage`。
- ⚠️ 附带发现（不改判定，但影响「auto 路由」的完整性）：**`LLMTierer` 零生产调用点** —— `auto` 永远只走 `RuleTierer{}`（`tier.go::ResolveTierFlag`），`LLMTierer.Sink` 那条 G02 接线因此也是生产不可达的。它不是 `With<X>(ctx,…)` 形状，所以 **GOV6 抓不到**。
- 证据形状：现有表驱动断言已足够。若要覆盖 LLMTierer，先得给它一个生产调用点。

**3. 升级不再静默退出** — 已兑现
- 依据：`tier.go` `EscalationHint`；loop 侧 `loop.go::Loop.Run`（拼进 Summary + `StopReason=escalate`）；轻量侧 `main.go::runLightweightGoal`。测试 `TestLoop_MaxIterationsHasEscalationHint`（正向）+ `TestLoop_T4MaxIterationsNoEscalationHint`（**反向**：T4 不含 hint、`StopReason==max_iterations`）+ `TestEscalationHint` + `TestRunLightweightGoal_NonTier4Hint`。正反双向都测了。
- 证据形状：正向断言 hint 出现在**用户可见的 Summary** 里 + 反向断言顶层 tier 不出现（现状即是）。**骗过去**：只测 `EscalationHint()` 纯函数而不测它是否被拼进 Decision。

---

## W3 — 任务与并发

### A2/DT1 — Durable tasks (TaskManager) · 台账 `partial`

> acceptance：可创建/列出/读取/取消；状态机正确；thread/turn 关联准确；重启后持久恢复

**1. 可创建/列出/读取/取消** — 部分
- 依据：四个工具存在（`internal/tools/task.go::NewTaskTools`）、已装配（`internal/bootstrap/bootstrap.go::Build`）；`internal/tools::TestTaskCreateReadListCancel` 端到端跑通四个动作。但它用 `work.NewFakeManager()`（内存 map）+ `Tools.Allow:["*"]` 的 wildcard profile。⚠️ **装配级缺口**：`task_create` / `task_list` / `task_read` / `task_cancel` **都不在** `DefaultOrchestratorProfile` 的 allow 列表（`internal/bootstrap/profile.go`，那里只有 shell v2 的 `task_shell_*`）→ `guard.checkTools` 对未命中返回 Prompt，出厂形态每次调用弹窗，SSE 无 callback 时直接拒。
- 证据形状：file-backed（非 `:memory:`）Store 上 create→list→read→cancel 往返，逐字段比对；**外加**一条 `guard.Check(DefaultOrchestratorProfile(), Action{Tool:"task_create"})` 断言 `Allow`。**骗过去**：wildcard profile + FakeManager（现状）——它证明的是「工具代码能跑」，不是「出厂形态下模型能用」。

**2. 状态机正确** — 未兑现（**虚报**）
- 依据：`CanTransitionTo`（`internal/task/work/types.go`）+ guarded UPDATE（`store.go`）有真单测（`TestStoreTransitionIllegal` 等）。但 `work.Manager.Start` / `Manager.Finish`（`manager.go`）**零生产调用点**；`task.Broker.RecordResult` 只 finalize 自己的 `tasks` 行，从不回写 `task_work`。生产中每条 durable task 永远停在 `pending`，`running`/`completed`/`failed` 运行时不可达。另：`CanTransitionTo` 允许非终态转到**任意**已知状态（`running→pending` 合法），"状态机"三个字名不副实。
- 证据形状：`dispatch=true` 提交 → broker 跑完 → 断言 `task_read` 返回的 status **不再是 pending**；外加一张显式非法转移矩阵（含 `running→pending`）。**骗过去**：只断言 `completed→running` 报错（现状）——它绕开了「生产里状态根本不转」这个命题。

**3. thread/turn 关联准确** — 部分（台账**低报**）
- 依据：注入链完整且有断言 —— `orchestrator.go::Orchestrator.withTurnContext` `tools.WithThreadLink(ctx, opts.ThreadID, opts.TurnID)` → `task.go::TaskTools.runCreate` 读取 → 落库 `thread_id`/`turn_id`（`store.go::workSchema`）+ 索引；`internal/tools::TestTaskCreate_ReadsThreadLinkFromContext`、`TestTaskContext_SiblingIsolation`。缺的是上游填充点（WS/SSE handler 填 `TurnOpts.ThreadID/TurnID`）无断言，`task_list` 的 thread filter 无隔离断言。
- 证据形状：两条不同 thread 各建 task，`task_list(all=false)` 各自只看见自己那条；同一 WS 会话两次 turn 的 TurnID 不同而 ThreadID 相同。

**4. 重启后持久恢复** — 未兑现（**虚报**）
- 依据：表结构存在，但**没有任何重启恢复逻辑** —— 对比 `registry.NewManager`（把磁盘上的 Running 改写为 Interrupted，`manager.go`），`work` 侧没有对应物。唯一沾边的 `internal/task/work::TestManagerCreatePersistsAndRecovers` 是**恒真空壳 + 命名误导**：它在**同一个 `:memory:` DB、同一个 `*Store` 实例**上再 `Get` 一次，注释自己写着「用 Store.Get 模拟 Manager 重启后读回」。`:memory:` 物理上不可能跨 Open 存活 —— 把 SQLite 换成纯内存 map 它照样绿。整个 `internal/task/work` 包 10 处 `sql.Open` **全是 `:memory:`**。
- 证据形状：`t.TempDir()/x.db` 上建 task → `Close()` → **重新 Open** → `Get` 拿回同一条且字段全等；并断言重启后 `running` 的 task 被标成某个可恢复态。**骗过去**：同一实例上再 Get（现状）。

---

### A2/DT2 — 验证门 (task_gate_run + evidence) · 台账 `divergent`

> acceptance：gate 证据结构完整；大输出成 artifact；挂到正确 task；退出码/duration 准确

**1. gate 证据结构完整** — 部分
- 依据：`internal/task/work::Evidence` 的全部字段（`internal/task/work/types.go`）、填充（`internal/tools/gate.go::GateTools.runGate`）、落 `task_work_gates` + timeline（`store.go::Store.RecordGate`）。`internal/tools::TestTaskGateRun_PassEvidence` 只断言 Classification / ExitCode / Gate / Command，**ID、Cwd、DurationMs、RecordedAt、Summary、LogArtifactID 未断言**（字段全集见 `internal/task/work::Evidence`），且走 FakeManager 不经 SQLite 往返；`internal/task/work::TestStoreRecordGate` 走真实 Store 但只断言 `Gates[0].Classification`。
- 证据形状：真实 gate 跑完后从 SQLite **读回**，逐字段非零/相等（ID 非空、Cwd == 解析后的 cwd、RecordedAt 落在 [before, after] 区间）。**骗过去**：只断言输入里手填的那几个字段。

**2. 大输出成 artifact** — 未兑现（**虚报**）
- 依据：实现有（`gate.go::GateTools.runGate`，`SpillThreshold` = 64 KiB，`spillover.go`）。唯一沾边的 `TestTaskGateRun_SpillToArtifact`（2026-08-07 已删除，故此处刻意不带包路径 —— GOV9 对解析不出的路径放行，这正是记录幻影用的逃生门）是**恒真空壳且断言方向相反** —— 注释自陈「我们没法轻易产生 > SpillThreshold 的 stdout……所以这里只验证小输出时不 spill」，然后 `assert.Empty(payload.Evidence.LogArtifactID)`。一个名叫 SpillToArtifact 的测试断言的是**没有产生 artifact**；把 `>` 写成 `<`、或把 `WriteArtifact` 整段删掉，它都不会红。
- 证据形状：让 gate 跑一条产出 > 64 KiB 的命令（注意 `sh -c "yes | head -c"` 会被 metachar HardDeny 拦，需先写大文件再 `cat`），断言 `Evidence.LogArtifactID != ""` 且 `artifact_read` 能取回全部字节。**骗过去**：现状这条。

**3. 挂到正确 task** — 未兑现（**虚报 + 承重缺陷**）
- 依据：`gate.go::GateTools.runGate` 的 `args.TaskID` **完全未校验存在性**。schema 声明了 `FOREIGN KEY(task_id) REFERENCES task_work(id)`（`store.go::workSchema`），但 `internal/store/store.go` 的 `buildDSN`与 `applyConnectionPragmas`**从不设 `PRAGMA foreign_keys=ON`**，SQLite 默认关闭外键 → 往不存在的 task_id 写 gate 证据在真实 Store 里**静默成功**。更糟：`work.FakeManager.RecordGate` **会**对不存在的 task 报错（`manager_extra_test.go::TestFakeManagerRecordGateNotFound` 断言了这一点），而 `gate_test.go` 全用 FakeManager —— **fake 比真实实现更严格**，制造了「挂错 task 会被拒」的反向安全假象。
- 证据形状：真实 Store 上建 task A/B，对 A 跑 gate，断言 `Read(B).Gates` 为空且 `Read(A).Gates` 长度 1；再对不存在的 id 跑 gate 断言返回 error（需先开 `foreign_keys=ON` 或在 gate.go 显式 `Read` 校验）。**骗过去**：任何用 FakeManager 的测试。

**4. 退出码/duration 准确** — 部分
- 依据：退出码有 `TestTaskGateRun_PassEvidence`（== 0）与 `TestTaskGateRun_NonZeroExitRecordsFailClassification`（`assert.Greater(exitCode, 0)`，未钉死值）；`exitCode = -1` 分支（启动失败 → classification "error"）无测试。**duration 全仓零断言** —— `DurationMs` 在测试里只作为**构造 Evidence 字面量的输入**出现（`DurationMs: 10 / 1 / 5`），把 `gate.go::GateTools.runGate` 改成 `DurationMs: 0` 不会让任何测试变红（变异盲）。
- 证据形状：跑一条 sleep 200ms 的命令断言 `DurationMs >= 150`；跑一条不存在的程序断言 `ExitCode == -1 && Classification == "error"`。

---

### B1/M04 — 完整生命周期 (wait/result/send_input/resume/assign/list) · 台账 `partial`

> acceptance：全部生命周期操作可用；线程树/深度/并发/usage 可查；取消不泄漏；resume 跨重启可尝试

**1. 全部生命周期操作可用** — 部分
- 依据：8 个工具齐备（`internal/tools/agent.go::NewAgentTools`），底层 `registry.Manager` 全实现。但 **`internal/tools/agent_lifecycle_full_test.go` 是假证据密集区**：`TestAgentWaitWithManager` 只 drain channel 末尾 `_ = result`（吞错，零断言）；`TestAgentResultWithManager` 用 `t.Logf` 代替 `t.Fatal`（吞错，永不失败）；`TestAgentSendInputWithManager` / `TestAgentAssignWithManager` / `TestAgentCancelWithManager` 各自只有一个 `for range ch {}`（spy，只证明不 panic）；`TestAgentLifecycleSuccessPaths` 同形。真有断言的只有 `TestAgentSpawnWithManagerAndFactory`、`TestAgentListWithManager`、`TestAgentResumeModelMismatch`。⚠️ 同 DT1：这 8 个工具**没有一个在** `DefaultOrchestratorProfile` 的 allow 列表里。
- 证据形状：每个工具断言其**可观察后效**而非「没 panic」—— send_input 后 mailbox 收到该文本、assign 后 `Result().Assignment` 变了、cancel 后 `Result().Status == cancelled`。**骗过去**：`for range ch {}`（现状）。

**2. 线程树/深度/并发/usage 可查** — 部分（深度+并发**低报**；usage **虚报**）
- 依据：深度与并发有本次审查里质量最好的两条测试 —— `internal/tools::TestAcceptance_DepthAndUsageQueryable`（Depth 0/1）与 `TestAcceptance_WorkflowUsesSharedLimitAndList`（`list.Running==2`、`capErr.Cap==2`）。**usage 虚报**：`tools.UsageSinkFrom`（`internal/tools/subagent.go`）**零生产调用点**（全仓只有定义 + 往返测试）。`orchestrator.go::managedTurnRunner.Run` 确实 `WithUsageSink(...)→mgr.AddUsage`（所以 GOV6 的注入器检查会过），但**没有任何生产代码读这个 sink 并调它** → 生产中 `agent_list`/`agent_result` 的 Usage 恒为 0；测试里直接手调 `mgr.AddUsage(...)` 绕过了断掉的那一段。「线程树」只有扁平 list + ParentID，无树形装配/断言。
- 证据形状：跑一个真实（fake model）子代理 turn，断言结束后 `Result(agentID).Usage.TotalTokens > 0` —— 这条现在**必然失败**。**骗过去**：测试自己调 `AddUsage` 再查（现状）。
- ⚠️ 这是 **GOV6 的盲区形状**：注入器有生产调用点（GOV6 绿），消费者没有。值得单列。

**3. 取消不泄漏** — 未兑现（**虚报**，与 F2/LEAK2 同一处 bug）
- 依据：`finishTerminal`（`internal/agent/registry/manager.go`）只改 `records[id].Status`，**从不 `delete(m.runtime, id)`、从不 `rt.cancel()`**；全仓 `delete(m.runtime,…)` 只有 3 处（2 处在 `Manager.Spawn`、1 处在 `Manager.restoreRecord`），全在**错误回滚**路径。每个终止的 agent 永久留下 `runtimeAgent`（含 mailbox chan、EventSink、一个从未 cancel 的 child context）。唯一沾边的 `internal/agent/registry::TestManager_ConcurrentSpawnCancel` 是**不会真正执行**的空壳：它把 `Path: t.TempDir()`（一个**目录**）当持久化文件路径 → `writeAtomic` 每次失败 → Spawn 全部回滚 → `if err == nil` 过滤掉全部 id → 后面的 Cancel 循环**一次都不执行**；再加 `_ = m.Cancel(aID)` 与全程零断言。同文件 `TestManager_ConcurrentListAndResult` 同病。
- 证据形状：spawn N 个 → 全部 Wait 到终态 → 白盒断言 `len(m.runtime) == 0`；或黑盒断言 `List().Running == 0` **且**第 N+1 次 Spawn 成功。

**4. resume 跨重启可尝试** — 部分（台账**低报**）
- 依据：`Manager.Resume`（`manager.go`）+ 加载时把非终态改写为 `Interrupted`。`internal/tools::TestAgentResumeSuccess` 做了**真正的两阶段重启**（file path → mgr1 spawn+完成+Close → mgr2 重新加载 → resume），结构正确，但**结尾是 `_ = gotResult`，零断言**（吞错）。有断言的是负分支 `TestAgentResumeModelMismatch`（同样两阶段重启，断言 "not available"）—— 所以「跨重启能读回记录」这半边被**间接**证明了。`internal/agent/registry::TestResumeRestoresSavedConstraintsAndEmitsEvent` 断言扎实但是手写 JSON + 单 Manager，不是真重启。
- 证据形状：把 `_ = gotResult` 换成 `require.True(gotResult)`，并断言 mgr2 上 `Result(id).Status == running` + runner 真被调用一次。

---

### F1/WAL1 — WAL 模式 + 连接池 + busy_timeout · 台账 `partial`

> acceptance：WAL 启用；并发写不报 locked；性能不退化；旧 DB 平滑升级；WAL 文件有界（roadmap:295）。plan 另有 10 条细化验收：…

**1. WAL 启用** — 已兑现（有保留）
- 依据：`internal/store/store.go` `applyConnectionPragmas` → `PRAGMA journal_mode=WAL`，`:memory:` 显式跳过；`internal/store::TestOpenWith_DefaultsApplied` / `TestOpenWith_ZeroDefaultsInMemory`。
- 保留：`journal_mode` 是库级持久属性，走池里随机一条连接是对的；但 `synchronous`/`busy_timeout`/`wal_autocheckpoint` 是**每连接**属性，靠 DSN `_pragma`，而 `:memory:` 的 DSN **完全不带 `_pragma`**（`buildDSN` 对 `:memory:` 直接 return）。**全仓绝大多数测试用 `:memory:`**（`internal/task/work` 10/10）→ 这些测试对 WAL 路径零覆盖。
- 证据形状：file-backed DB 上并发占满 `MaxOpenConns` 后**逐连接**查 `PRAGMA journal_mode` / `busy_timeout`。

**2. 并发写不报 locked** — 部分
- 依据：`Store.writeMu` + `WriteTx`（`store.go::Store.applyConnectionPragmas/Store.WriteTx`）进程内串行化；跨进程靠 DSN `busy_timeout`。测试 `internal/store::TestConcurrentAppend_NoBusy` / `TestConcurrentMixedWrite_NoBusy` / `TestWriteTxSerializes` 存在。
- 证据形状：断言 N×M 并发写下 `SQLITE_BUSY` 计数**恰好为 0**（而非「没 panic」）。

**3. 性能不退化** — 未兑现（**N 类：测量不是断言**）
- 依据：`internal/store` 下**零个 `func Benchmark`**；全仓无性能基线，无阈值门禁。
- 证据形状：即使补了 Benchmark，没有基线比较也不构成验收证据。需要「上次的数 + 这次的数 + 差值判定」这条链，或改写 acceptance 为可断言形态（如对 `AllocsPerRun` 设上限）。与 F2/BENCH1#2/#3、E1/COV2#1、E1/COV3#1 同一形状。

**4. 旧 DB 平滑升级** — 部分
- 依据：`internal/store/wal_upgrade_test.go::TestWALUpgradeFromRollback` 存在。
- 证据形状：必须**真造**一个 `journal_mode=delete` 的旧库（先用 rollback 模式写入数据再 Open），断言升级后 journal_mode 变为 wal 且旧数据逐行可读、零丢失。**骗过去**：直接建新库再断言是 wal（那只是子句 1）。

**5. WAL 文件有界 + plan 的 10 条细化验收** — 部分
- 逐条：
  - **每条池连接 PRAGMA 生效** — 未见对应测试（见子句 1 的保留）。
  - **MaxOpenConns 按配置且 `:memory:` 强制 1** — 已兑现（`store.go`；`TestInMemoryForcedSingleConn` + `TestOpenWith_DefaultsApplied`）。
  - **16×50 零 BUSY** — 测试名存在（`TestConcurrentAppend_NoBusy`），规模未逐行核对。
  - **读不阻塞写** — 测试名存在（`TestConcurrentReadWrite_NoBlocking`）。
  - **双 Open 跨进程 busy_timeout** — ⚠️ `TestCrossProcessBusyTimeout_DualOpen` 名字是 DualOpen，很可能是**同进程两个 `sql.Open`**，那样测不到跨进程锁语义（需真起子进程）。
  - **rollback→WAL 幂等零丢失** — 见子句 4。
  - **Close 执行 `wal_checkpoint(TRUNCATE)`** — 实现已兑现（`store.go`），但错误被 `_, _ =` 吞掉（设计如此）；**没有测试断言 `-wal` 文件大小归零/有界** —— 这正是「WAL 文件有界」这半句的正主。
  - **work/vcs/auth/bootstrap 现有测试全绿** — 未核。
  - **Windows 相关** — acceptance 文本本身被截断（见下方「acceptance 本身有问题」一节）。
- **额外发现（应补进本条）**：`PRAGMA foreign_keys` 从未开启，四张 `task_work_*` 表声明的 FK / `ON DELETE CASCADE` **全部不生效**（见 A2/DT2#3）。
- 证据形状（WAL 文件有界）：写入足够多数据让 `-wal` 涨起来 → `Close()` → 断言 `os.Stat(db+"-wal")` 的 Size 归零或小于阈值。

---

### F2/LEAK2 — 子代理并发上限 · 台账 `partial`

> acceptance：并发上限生效；满则拒绝；计数准确；与深度上限交互文档化

**1. 并发上限生效** — 部分（门存在且会触发，但它守的不是「当前并发数」）
- 依据：`internal/agent/registry/manager.go`（`runningLocked() >= m.limit → &SpawnErrCap{}`）、配置链 `config.Subagents.Limit` → `bootstrap.go::Build`。两条测试（`TestSpawnRespectsCapAndReturnsSpawnErrCap`、`TestAcceptance_WorkflowUsesSharedLimitAndList`）**只覆盖「同时在跑」这一种情形**，从不让第一个 agent 先跑完再 spawn。因子句 3 的计数 bug，这个「上限」在第一波跑完后变成**永久锁死**而不是限流；叠加 `internal/tools/subagent.go` 的 `spawnWithRetry`（对 `SpawnErrCap` **无限重试 + 指数退避，只有 ctx 取消才退出**），一旦历史 spawn 数达到 limit，任何走 `ManagedSubAgentRun` 的调用会**永久挂起**直到 turn 超时。
- 证据形状：spawn limit 个 → **全部 Wait 到终态** → 第 limit+1 个 Spawn 必须成功。这条现在**必然失败**。**骗过去**：让所有 agent 都 park 住再 spawn（现状两条测试）。

**2. 满则拒绝** — 已兑现（台账**低报**）
- 依据：`TestSpawnRespectsCapAndReturnsSpawnErrCap`、`TestResumeRejectsConcurrencyCap`、`internal/tools::TestAcceptance_WorkflowUsesSharedLimitAndList` —— 都是 `ErrorAs(&SpawnErrCap)` + `capErr.Cap` 相等断言，真证据。四条里唯一干净兑现的一条。
- 证据形状：类型化错误 + Cap 值相等（现状即是）。

**3. 计数准确** — 未兑现（**承重 bug**）
- 依据：`runningLocked()`（`manager.go`）取「非 nil 的 `m.runtime` 长度」与「`StatusRunning` 记录数」的**最大值**；而 `finishTerminal` 从不 `delete(m.runtime, id)` → runtime 长度单调递增 → `runningLocked()` 实际返回「本进程历史累计 spawn 数」。可直接观测的自相矛盾：`List().Running` 从 records 数（准确），spawn gate 用 `runningLocked()`（不准）→ 出现 **`List().Running == 0` 但第 3 次 Spawn 直接 `SpawnErrCap{Cap:2}`**。唯一碰 `runningLocked` 的 `TestRunningLockedIgnoresNilRuntimeEntries` 只覆盖 nil 守卫，与泄漏正交（**变异盲**：补不补 delete 它都绿）。
- 证据形状：`spawn → Wait 终态 → require.Equal(0, len(m.runtime))`，或黑盒版 `List().Running == 0 且下一次 Spawn 成功`。
- 修复落点：`finishTerminal` 末尾摘除 runtime 并 cancel child ctx；注意 `Cancel`、`Wait`、`sinkLocked` 都读 runtime，顺序须先取 sink 再 detach。

**4. 与深度上限交互文档化** — 部分
- 依据：注释齐全（`manager.go::Manager.Spawn` 明写「ErrTooDeep 优先」「两个维度同时生效」+ `internal/tools/subagent.go::MaxSubAgentDepth` 对偶注释），docs/ 下无对应章节。两处缺口：
  - **doc drift**：`manager.go::Manager.Spawn` 硬编码 `if depth > 3 { // MaxSubAgentDepth }`（registry 不能 import tools，会成环）→ 把 `tools.MaxSubAgentDepth` 改成 5，registry 仍卡 3，而 `TestSpawnRejectsTooDeep` 自己硬编码 4 层链照样绿（变异盲）。深度检查还有**第三处** `orchestrator.go::Orchestrator.runSubAgentTurn`。
  - 注释里最关键的「两者同时超限时 ErrTooDeep 胜出」**零测试**（`TestSpawnRejectsTooDeep` 用 `MaxConcurrent: 10`，并发根本没超）。
  - ~~第三处缺口：`config.example.yaml` 完全没有 `subagents:` 段~~ —— **撤回，是错的**。该段一直都在，连合法范围 `1..20` 都写了，而且 `internal/config::TestExampleConfigDocumentsSubagents` 正是钉住它的绿测试。本条据此把缺口记成三处，实为两处。
- 证据形状：构造「深度已达上限 **且** 并发已满」的场景断言返回 `ErrTooDeep` 而非 `SpawnErrCap`；外加一条跨包常量对账测试（测试里断言 registry 的深度常量 == `tools.MaxSubAgentDepth`，用测试而非 import 破环）。

---

## W4 — 压缩与属性测试

### E2/PROP1 — ctxcompact 属性测试 · 台账 `done` ✅ **复核：done 站得住**

> acceptance：≥3 个属性；随机输入通过；工具对配对不变量成立

> **重点复核**：外部指控「第 3 句的三条证据全是变异盲的空壳」，**对当前树不成立**。掏空实验（`EnforceToolCallPairs` 直接返回入参，用 `go test -overlay` 在副本上跑，工作树零写入）：`TestProperty_ToolCallPairingFixpointHolds` / `TestProperty_ToolCallPairFixpointIsIdempotent` / `TestProperty_ToolCallPairFixpointRepairsCorruption` **三个全 FAIL**。测试文件自身注释（`plan_property_test.go`）记录了这个空壳形态**曾经存在并已被修复** —— `pinnedSetIsConsistent` 被降级为 oracle、skip 守卫换成只读生成输入的 `skipAlreadyCompacted`、并加了 60% 执行率下限。指控针对的是修复前的版本。

> **本段曾经记录了两条空心证据（`TestProperty_RunReducesTokens`、`TestProperty_EachSummaryCallWithinWindow`），那两条已在 `8dcc7d2` 修好**，本段随之改写。改写前的版本一度自相矛盾：§1/§2 与处置段说三处引用空心，§3 与本框却说全部变异敏感。留这行是因为「已修复的缺陷被文档继续记成未修复」本身是一种虚报 —— 它让下一个读者去修一个不存在的问题，或据此下调对台账的信任。

**1. ≥3 个属性** — 已兑现，三条引用**各自变异敏感**（实测）
- 依据：`internal/ctxcompact/` 共 8 个 `TestProperty_*`，互相独立的性质 ≥3。ledger 挂的三条逐条掏空实测（全部 `go test -overlay`，工作树零写入）：
  - `TestProperty_PinSetIsSubsetOfOutput` — **真**（见 #2）
  - `TestProperty_RunReducesTokens` — **真**（见 #2）
  - `TestProperty_EachSummaryCallWithinWindow` — **真**。它曾经是「吞错」形态（`if err != nil { t.Logf(...); return }`，一个永远失败的 `RunSummary` 也满足这条属性），现在整条属性的 `t.Fatal*` 不止一处，数法（**不写死条数**）：

      ```sh
      awk '/^func TestProperty_EachSummaryCallWithinWindow/,/^}/' \
        internal/ctxcompact/summarize_property_test.go | grep -c 't\.Fatal'
      ```

      逐条：
    - 掏空成「返回常量 summary，从不调 summarizer」→ 四个 window 子测试全 FAIL，落进 `summarize_property_test.go::TestProperty_EachSummaryCallWithinWindow` 里那句 `t.Fatal("RunSummary returned success but summarizer was never called")`
    - 掏空成「无条件返回 `ErrNoWindowRoom`」→ FAIL，落进同一测试末尾的跨 window 下限 `t.Fatalf("only %d of %d windows produced summarizer calls (want ≥%d)…")`，实测 `only 0 of 4`
    - 掏空成「返回任何别的 error」→ 落进 `t.Fatalf("RunSummary failed for a reason this property does not tolerate")`，只有 `ErrNoWindowRoom` 被容忍
    - **上界本身换过一次**：原先断言「`< 2×` 窗口」，**已被证伪并收回**（索引 0 从不做预算检查；`splitIsSafe` 扫描整个左侧，使 `[call(id1..idN), r1..rN]` 的每个内部切点都不安全，超出倍率跟并行工具数走而不跟窗口走）。现在断言的是 `ModelWindow + 最大不可分割段`（`::maxAtomicGroupTokens`）。两种证伪形状与实测倍率写在该测试的 doc 注释里，本文不复述数字
    - 末尾还有一道**反向下限**：整条 sweep 里若一次调用都没超过 2× 窗口就 `t.Fatal` —— 那说明证伪旧界的输入形状根本没到 `RunSummary`，上面那条容忍度就成了没人碰的摆设
    - 这条属性的 oracle（`::atomicBoundaryIsSafe`，全仓唯一一处明示豁免 CLAUDE.md「禁止复制粘贴」的复制）**不再只靠散文说明**：`::TestOracleIndependence_DelegationGoesVacuous` 把两个方向都钉住 —— 委托型 oracle 在 `splitIsSafe → return false` 下会把整条历史算成一个不可分割段、上界随之吞掉全部输入（属性变得不可证伪），而 `return true` 只会收紧上界、区分不出两种设计
- 证据形状：这是计数主张，应该挂「互不重复**且各自变异敏感**」的 3 个测试（现状即是）。**骗过去**：把 8 个测试名都列上而不管其中几个是空的。

**2. 随机输入通过** — 已兑现（`TestProperty_PinSetIsSubsetOfOutput` 与 `TestProperty_RunReducesTokens` 两条同时扛）
- 依据：`gen_test.go` `genHistory` 随机生成含孤儿 tool_call / 孤儿 tool_result / working-set / error / diff 标记 / summary 尾的历史，`planPropertyGen` 每属性 30–50 trial。
  - `TestProperty_PinSetIsSubsetOfOutput` — **真证据**：50 个随机历史，断言的是**指针恒等**（`out[i] != msgs[idx]` 即失败）+ 索引升序。掏空实验：把 `Assemble` 改成复制 message（内容相同、指针不同）→ FAIL。
  - `TestProperty_RunReducesTokens` — **真证据**。它曾经是「守卫用被测函数自己的产物」的坏形态（`if after >= before && calls > 0`，调用次数由被测的 `Run` 自己决定，`Run` 一掏空条件恒假、测试全绿）。现在调用次数是**跨 trial 的硬下限**而非每 trial 的条件：测试体走 `plan_property_test.go::runGeneratedProperty`，末尾用 `::requireTrialFloor` 断言「至少 60% 的 trial 真的 summarize 了东西」。掏空实验：`Run` 改成「原样返回入参、永不 summarize」→ FAIL，落进 `run_property_test.go::TestProperty_RunReducesTokens` 末尾那句 `requireTrialFloor(t, "summarized anything", …)`，实测 `only 0/30 trials summarized anything (need ≥18): the property is vacuous, not passing`
- 证据形状：随机输入 + 守卫只对**输入侧可算的前提**成立，且把「被测函数真的干活了」写成跨 trial 的硬下限而非 per-trial 条件（现状即是，`run_property_test.go` 与 `plan_property_test.go` 走同一个入口）。**骗过去**：任何把被测函数的调用计数当 skip 条件、又不数执行率的写法。

**3. 工具对配对不变量成立** — 已兑现（**指控不成立**）
- 依据：`internal/ctxcompact/pairs.go` `EnforceToolCallPairs`（call_id/result_id 双索引 + `permanentlyRemoved` 防振荡 + 不动点循环），调用点 `plan.go::Plan`。三个测试**全部变异敏感**（见上）。三道防空壳机制都在位：① `plan_property_test.go::skipAlreadyCompacted` 只读**生成输入**的 `lastMessageIsSummary(msgs)`，不读 `Plan` 输出；② `plan_property_test.go::runGeneratedProperty` 强制执行率 ≥60%（`::minExecutedTrials`，由 `::requireTrialFloor` 断言），把「全 skip 也算 PASS」变红，且它是**包内所有生成型属性的统一入口** —— 现在是 8 条属性对 8 个调用点，跨文件的 `run_property_test.go` 与 `summarize_property_test.go` 都走它（这句曾在只接了 5/8 时就被写成「全覆盖」，核对方法见 review-checklist A 段：数 `^func TestProperty_` 与数 `runGeneratedProperty(t,`，两个数相等才算数）；③ `RepairsCorruption` 末尾 `if injected == 0 { t.Fatal }`。
- 已知弱点（不足以推翻 done，与台账注释一致）：种子固定（`plan_property_test.go::propSeed` 一个常量，每个 trial 的种子由 `propSeed*1000+trial` 派生；此前这里列过三个种子常量，另外两个从不存在），是「确定性重放的属性测试」，覆盖来自 trial 数而非跨运行随机性。
- 证据形状：不动点成立 + 幂等 + 人为破坏可修复三角度；且**守卫只读生成输入 + 有执行率下限**。**骗过去**：用 `pinnedSetIsConsistent(msgs, pinned)` 当 skip 条件 —— 那正是历史上的坏形态。

> **处置**：`done` 保留，三句的 evidence 引用**全部有效**。曾经空心的两条（`TestProperty_RunReducesTokens` 在 #1、#2 各引一次，`TestProperty_EachSummaryCallWithinWindow` 在 #1 引一次，共 3 处）已在 `8dcc7d2` 由测试侧修复 —— 不是改挂别的引用，是把这两条测试本身改成变异敏感的：调用计数从「条件」升级为「跨 trial 硬下限」，错误分支从 `t.Logf` 升级为按错误类型分流的 `t.Fatalf`。本条无遗留动作。

---

### F2/CCL1 — mid-turn 压缩 cooldown · 台账 `partial`

> acceptance：同 turn 不重复压缩；逼近上限仍触发；keepRecent 文档清晰

**1. 同 turn 不重复压缩** — 已兑现
- 依据：`internal/llm/eino/compacting.go` `inCooldown`（token 增长维度 + 时间维度，任一未满足即在冷却）、`::CompactingModel.shouldCompact` 在 `shouldCompact` 里短路、`::CompactingModel.maybeCompact` 压缩成功后更新 `lastCompactTokens`/`lastCompactAt`。配置链完整：`config.go::CompactionConfig` → `bootstrap.go::Build` → `orchestrator.go::wrapCompaction` → `CompactingModel`。**掏空实验**：`inCooldown` 恒返回 false → `TestCompactingModel_CooldownDefersReCompact` **FAIL**。配套 `TestCompactingModel_FirstCompactNoCooldown` 覆盖反向。
- 轻微弱点：该测试在第一次真实 `maybeCompact` 之后**手工覆写** `cm.lastCompactTokens = 180` / `lastCompactAt = time.Now()`，即断言的冷却状态不是生产写入的那个 —— 「`maybeCompact` 成功后正确记录冷却状态」这一步没被独立守住。
- 证据形状：真实压缩一次 → **不手工改内部状态** → 第二次同规模输入 → 断言 `didCompact==false` 且 summarizer 调用数不增。

**2. 逼近上限仍触发** — 未兑现（**台账把它记为「已覆盖」是虚报**）
- 依据：实现在 `compacting.go::CompactingModel.shouldCompact`（`HardForceFraction` 分支，在阈值门与冷却门之前 `return true`）。`TestCompactingModel_HardForceOverridesCooldown` 是**恒真（前提从未成立）+ 变异盲**：它设了 `CooldownTokens: 99999` 想制造冷却，但 `lastCompactTokens==0 && lastCompactAt.IsZero()` → `inCooldown` 在 `::CompactingModel.inCooldown` 直接 `return false`（"无先前压缩 → 无冷却"）→ **冷却根本没开启**，普通阈值门（`0.8×1000=800` vs 约 1025 tok）就已放行，hard-force 分支是死路。
  **掏空实验**：把 `shouldCompact` 里整个 `HardForceFraction` 分支删掉 → `internal/llm/eino` + `internal/bootstrap` + `internal/agent/orchestrator` **三个包全绿**。`TestCompactingModel_CooldownDefersReCompact` 的第三段（`msgs3`，注释宣称测 hard-force）同样是死的：`432-180=252 ≥ CooldownTokens=100`，本来就不在冷却里。**全仓没有一个测试守住 hard-force。**
- 证据形状：必须让 `inCooldown(tokens)` **真的返回 true**（先跑一次成功压缩，或显式设 `lastCompactAt=now` 且 `CooldownDuration` 未过期 / 令 `tokens-lastCompactTokens < CooldownTokens`），**再**把输入推到 `≥ HardForceFraction×ContextWindow`，断言 `didCompact==true`；并配一个刚好低于该比例的**负向孪生**断言 `didCompact==false`。**骗过去**：任何 `lastCompactTokens==0` 的用例，以及任何 token 数已越过普通 Threshold 的用例。

**3. keepRecent 文档清晰** — 未兑现（台账判定正确）
- 依据：桥接在 `compacting.go::CompactingModel.maybeCompact`（`ctxcompact.PlanOpts{KeepRecent: c.KeepRecent / 2}`），语义差异在 `::CompactingModel` 的 doc 注释里说明。`TestCompactingModel_KeepRecentBridge`（**已于 2026-08-06 W4 review 第 23 轮删除**，故此处刻意不带路径前缀）曾是**恒真空壳**：函数体是 `cm := &CompactingModel{KeepRecent: 4}; if cm.KeepRecent/2 < 2 { t.Fatal }`，即对字面量做 `4/2 >= 2` 的算术断言，**从不调用 `maybeCompact`、不触碰 `/2` 桥接的任何生产代码**；测试自己的注释写着「The test does NOT assert the bridge itself」。桥接现由 `internal/llm/eino::TestCompactingModel_KeepRecentBridgesMessagesToPairs` 真实覆盖。
- 证据形状：「文档清晰」是文档质量主张（**N 类**），GOV8 只收测试引用。这条要么改写成行为主张 —— 例如断言 `maybeCompact` 传给 `ctxcompact.Plan` 的 `PlanOpts.KeepRecent` 恰为 `CompactingModel.KeepRecent/2`，从而两处语义漂移可被机器检出 —— 要么它永远撑不起终态。**骗过去**：现状这种字面量算术。

---

## W5 — 安全策略与凭据

### A1/S06 — 结构化 Shell 策略 (execpolicy) · 台账 `partial` ⚠️ **虚报**

> acceptance：能识别程序/参数/管道/重定向；规则结果可解释；已知绕过样例（IFS、$()、glob 注入）有回归测试

> **三条子句都受同一组前置事实支配，先读这个：**
> 1. `internal/guard/guard.go::Guard.checkShell` 的**元字符结构性 HardDeny 在 execpolicy 之前短路**，拦截列表为 `&& & || ; | ` $( \n \r > <`。
> 2. execpolicy 只在 `len(p.Shell.Rules) > 0` 时运行（`guard.go::Guard.checkShell`）。**全仓非测试代码零处构造 `ShellPerm.Rules`**；`config.example.yaml` 零 `rules:` 命中；`DefaultOrchestratorProfile()`（`profile.go`）连 `Shell` 字段都没有。→ **出厂配置下 `execpolicy.Evaluate` 完全不执行。**
> 3. 唯一无条件可达的生产入口是 `internal/tools/permctx.go::scopeFromAction` 的 `execpolicy.Parse`（生成 approval scope），且 `len(cmd.Segments) != 1` 直接 error。

**1. 能识别程序/参数/管道/重定向** — 部分
- 依据：实现真实（`lexer.go::Lex` Pipe/Redirect token、`parser.go::Parse` Segment 切分 + RedirectSpec、`::normalizeProgram` normalizeProgram）。程序/参数识别生产可达；**管道/重定向识别在生产路径上不可达** —— 含 `|`/`>`/`<` 的命令在 `guard.go::Guard.checkShell` 就被结构性 HardDeny，永远到不了 `guard.go::Guard.checkShell` 的 Parse。是**元字符层**拦的，不是 execpolicy。`TestParsePipelineAndRedirects` 等四条测试全部直接调 `Lex`/`Parse`，**没有任何测试经 `guard.Check` 驱动管道或重定向**（也不可能 —— 会被元字符拦）。假证据类别：**层级错位** —— 单测驱动的是一条生产永不进入的分支。
- 证据形状：经 `guard.New().Check(profileWithRules, Action{Shell:"printf ok | cat"})` 的测试，断言返回的 `Decision` 携带 execpolicy 的 `RuleID`（而非 `"shell metacharacter rejected: |"`）。**骗过去**：任何直接调 `execpolicy.Parse` 的包内单测。

**2. 规则结果可解释** — 部分（**实现存在，解释在工具边界被丢弃**）
- 依据：`policy.go`（`Result{RuleID, Justification, MatchedPrefix, Reason}`）→ `guard.go::Guard.checkShell` 映射进 `Decision`。**断链**：`Decision.RuleID` / `Decision.Justification` 在 `internal/guard` 之外**零个非测试读者**；`tools.Authorize` 三处出口（`permctx.go`）全部只写 `&DenyErr{Reason: dec.Reason}`，`Justification` 被静默丢弃。WS 的 `permission_rule_hit`（`ws.go::Server.ChatWS`）用的是 `approval.AuditEvent.RuleID`，是**另一个概念**。`internal/guard::TestGuardExecPolicyMapsRuleIDAndHardDeny` 是 **spy 只证明字段被填** —— 断言的是一个没有任何消费者的内部结构体字段。
- 证据形状：断言 execpolicy 的 `Justification` 出现在**用户可观察量**里（工具返回文本、`proto.ServerFrame`、或 slog 审计行）。

**3. 已知绕过样例（IFS、$()、glob 注入）有回归测试** — 部分
- 依据：`lexer.go` `forbiddenExpansionAt` 拒 `$ \` * ? [` 及 `%VAR%`；`TestLexRejectsExpansionAndGlobBypasses` 覆盖三类样例；`internal/guard::TestGuardExecPolicyParserFailureIsHardDeny` 用 `printf ${IFS}` 走完整 `guard.Check`。**按「是哪一层拦的」拆开**：
  - **`$()` / 反引号** —— 元字符层拦的（`guard.go::Guard.checkShell` 列表含 `"$("` 和 `` "`" ``），execpolicy 在生产里收不到这类输入。lexer 里那份拒绝只在包内单测被驱动。
  - **`$IFS` / `${IFS}`** —— 裸 `$` **不在**元字符表 → 确实穿过元字符层到达 execpolicy，是 lexer 拦的。**这是三类里唯一有 guard 层测试的。**
  - **glob 注入（`*` `?` `[`）** —— 同样不在元字符表 → 由 lexer 拦，但**只有 execpolicy 包内单测，无 guard 层测试**；且 `Rules` 为空的出厂配置下 `cat *.go` 走的是 legacy allowlist 的 `MatchGlob`，跟 execpolicy 无关。
- 证据形状：三类各需一条经 `guard.Check`（`Rules` 非空 profile）的测试，断言 `Verdict==HardDeny && RuleID=="parse-error"`。**骗过去**：当前的纯 `Lex()` 单测。

---

### A1/S07 — 持久审批规则 · 台账 `partial`（低报与虚报并存）

> acceptance：规则含来源/scope/过期；每次命中可审计；用户可查看撤销；前缀规则有绕过回归测试

**1. 规则含来源/scope/过期** — 部分
- 依据：`internal/approval/types.go`（`Rule{Source, Scope, TTL, CreatedAt, ExpiresAt, ProcessID}`）；过期消费在 `manager.go` `expireLocked`。**持久层是真的**（台账**低报**）：`manager.go::Manager.Record` 走 `persistLocked` → KV `security.approvals.v1`，`New()`重启后重载，`bootstrap.go::Build` 传入真 `*store.Store` —— **不是「会话内存里的 remember」**。**缺口**：生产代码**从不设置 `ExpiresAt`**（`permctx.go::Authorize` 构造 `Rule` 时只填 `ID/Action/Scope/TTL/Source`），`Source` 恒为 `SourceUser`，`SourceMode`（`types.go`）是死常量、零生产写入点 → 「过期」在生产中恒为零值（= 永不过期），「来源」恒为单值。四条过期测试都由测试自己塞 `ExpiresAt`（**fake 太宽**）—— 证明的是 Manager *能*过期，不是产品*会*过期。`TestNewLoadsPersistentRules` 对跨重启重载是真断言。
- ⚠️ 另有 `ws.go::Server.ChatWS` 的降级分支 `approval.New(nil, "ws-conn", nil)`（KV=nil）：一旦 `Server.Config.Approvals` 未注入，`allow_persistent` 会因 "persistent store unavailable" 而失败（生产 bootstrap 有注入，此分支只在测试触发）。
- 证据形状：一条从 `tools.Authorize` 出发（callback 返回 `allow_session`）、断言**落库** `Rule.ExpiresAt` 非零的测试；或明确把「过期」从验收里删掉。

**2. 每次命中可审计** — 部分
- 依据：`manager.go` `auditLocked` → `AuditBus.Publish`→ `ws.go::Server.ChatWS` `permission_rule_hit` 帧；另有 `permctx.go::auditPermission` 的 slog 审计。**实现缺口**：`AuditBus.Publish` 是 **drop-on-full**（`select { case ch <- e: default: }`，缓冲 64）—— 慢客户端下命中事件按设计丢弃，「每次」不成立。`TestMatchReturnsEmptyRuleOnMiss` 是唯一一条从真 `Match` 出发断言 emit 的（且只覆盖 **miss**）；所有 `Kind:"hit"` 出现的地方都是**手工构造 `AuditEvent` 字面量直接喂给 `bus.Publish`**（**恒真空壳**）—— 证明的是 bus 会转发，不是 Match 命中会 emit。slog 侧 `internal/tools::TestAuthorizeLogsDecisionWithoutArguments` / `TestAuthorizeLogsDenyDecision` 是真断言。
- 证据形状：`Record` 一条规则 → `Match` 命中 → 断言 emit 收到 `Kind=="hit" && RuleID==<那条规则的 ID>`。

**3. 用户可查看撤销** — 部分
- 依据：`internal/api/http/ws.go::Server.ChatWS`（`permissions_list`）、`::Server.ChatWS`（`permission_revoke`）；TUI `commands.go::commandTable/cmdPermissions` `/permissions [revoke <id>]`；渲染 `entries.go::permissionsEntry/permissionsEntry.render`。**实现缺陷**：`ws.go::Server.ChatWS` 每次 WS 连接铸造新的 `connectionSessionID = fmt.Sprintf("ws-%d", time.Now().UnixNano())`，`List`/`Revoke` 都用它 → **会话级规则在下次连接就查不到也撤销不了**（只有 persistent 规则跨连接可见）。现有测试都在**帧编解码 + TUI 渲染**层，不是端到端。
- 证据形状：一条 WS 端到端 —— `allow_persistent` → `permissions_list` 帧含该 rule ID → `permission_revoke` → 同一动作再次触发 prompt。

**4. 前缀规则有绕过回归测试** — 未兑现
- 依据：`internal/tools/permctx.go` `scopeFromAction`，`Scope.Prefix` = `cmd.Segments[0].Args`（**全部** args，不是前缀）；`manager.go::Manager.Match` 用 `reflect.DeepEqual(r.Scope, scope)` 全字段比对；多段命令 fail-closed（`::Manager.Revoke`）。结构上**不存在**前缀放宽绕过 —— 但这是「碰巧对」，**没有测试钉住**。最接近的 `TestAuthorize_AlwaysAllow_DifferentActionStillPrompts` 用的是 `ls` vs `rm`（不同 program），完全不触及「同 program、args 是已批准 args 的**超集**」这个绕过形状。
- 证据形状：批准 `go test ./...` 后，断言 `go test -tags=e2e_real ./...` 仍触发 callback（`asks==2`）；反向再断言 `go test` 本身不再触发。**骗过去**：任何换 program 的对照（现状即是）。

---

### A1/S09 — 子进程网络隔离 · 台账 `partial` ⚠️ **虚报（标题与实现范围错配）**

> acceptance：未授权连接失败；host/port 规则生效；DNS/重定向不能绕过；决策入审计

> **范围错配确认。** `internal/bootstrap/bootstrap.go::Build` 注释自陈属实：**不启 `netpolicy.Proxy`**；`internal/shell/childlaunch.go` `proxy()` 在 `Policy!=nil && ProxyURL==""` 时返回 `http://127.0.0.1:0` **死端口**，**不 consult 任何 `security.network` 字段**。`internal/shell/procfactory.go::SecureLaunchFactory` 的 `Policy *netpolicy.Policy` 字段**只被用作「是否发死端口」的布尔开关**（`childlaunch.go::childLaunchPosture.proxy`），其 Allow/Deny/Default/AllowPrivate 从不参与子进程决策。生产唯一真实施加点是 `internal/tools/web.go::WebTools.runFetch/WebTools.runSearch` 的**进程内 HTTP**。

**1. 未授权连接失败** — 未兑现（就「子进程」而言）；进程内已兑现
- 依据：进程内 `web.go::WebTools.runFetch` + `netpolicy.NewTransport` / `PolicyDialer`（`proxy.go::PolicyDialer.DialContext`）为真。子进程侧只有死端口环境变量，且**只覆盖 shell/secproc 两个 factory** —— ACP（`acp/spawn.go`）、MCP（`mcp/manager.go`）、LSP（`lsp/manager.go`）、`cmd/yanshi/pr.go` 的 `gh` 全部走 `os.Environ()`，**无任何管制**。`TestDefaultSecureFactoryDefaultProxyURL`（**已于 2026-08-06 W5 Task 5 改写为 `internal/shell::TestDefaultSecureFactoryPublishesNoPlaceholderProxy`**，故此处刻意不带路径前缀） 只断言 env 里出现 `HTTP_PROXY=http://127.0.0.1:0` —— 它断言的是「变量被设置」，不是「连接失败」。
- 证据形状：拉起真子进程访问一个未授权 host，断言非零退出 + 拒绝原因可辨识。

**2. host/port 规则生效** — 未兑现（**port 维度在数据结构上就不存在**）
- 依据：host 侧 `netpolicy/policy.go` `CheckHost` + `hostMatches`（精确 or `.` 后缀，**不支持 glob** —— 注意 `security.network.allow: ["*"]` 不会匹配任何主机；`profiles.*.net.hosts` 的 `"*"` 走的是另一条 `guard.MatchGlob` 路径）。**port 侧：`netpolicy.Policy` 没有任何 Port 字段**，`normalizeHost`（`policy.go`）主动剥掉 `:port`。host 侧测试为真；**`internal/netpolicy::TestCheckHost_StripsPort` 把「忽略端口」钉成期望行为** —— 端口规则不是漏测，是被测试**固化为不存在**。
- 证据形状：`Policy` 需要 Port 维度 + 一条 `allow host:443 / deny host:22` 的断言。**当前形状下这条子句无法兑现**（需要改实现或改 acceptance）。

**3. DNS/重定向不能绕过** — 部分（进程内已兑现；子进程未兑现 —— 无代理即无此语义）
- 依据：DNS rebinding 防线 `proxy.go::PolicyDialer.DialContext`（resolve → `CheckResolvedIPs` → **pin `ips[0]`**，不交给 `net.Dialer` 重解析）；重定向逐跳复检 `web.go::WebTools.runFetch`（`CheckRedirect`）与 `proxy.go::NewProxy`（`ErrUseLastResponse` 使每跳重进 `ServeHTTP`）。真证据：`TestDialContext_CheckResolvedIPsRejectsPrivate`、`TestCheckResolvedIPs_*`、`TestServeHTTP_DeniedHostReturns403`、`internal/tools::TestWebFetch_RedirectDeniedByHostPolicy`（端到端 302 → `169.254.169.254`，断言输出含 `redirect denied`）。
- ⚠️ 假证据：`internal/tools::TestWebFetch_RedirectEnforcesHostPolicy`（`web_test.go`）注释自陈 `// Simulate what CheckRedirect does`，只调 `policy.CheckHost`，**根本不驱动 web_fetch** —— **恒真空壳（重新实现被测逻辑）**。所幸同文件有真的那条兜底。
- 证据形状：端到端跟随重定向并断言拒绝理由（现状 `TestWebFetch_RedirectDeniedByHostPolicy` 即是）；子进程侧需先有代理。

**4. 决策入审计** — 未兑现
- 依据：`netpolicy.Decision{Rule, Reason}`（`policy.go`）字段齐备，但全仓非测试代码**没有任何一处把 netpolicy 决策写进 slog/审计**。`web.go` 只 `&DenyErr{Reason: d.Reason}` 返回给模型；`proxy.go::Proxy.ServeHTTP` 只 `http.Error`。`childlaunch.go` 注释自陈 "produces no decision record"。
- 证据形状：捕获 slog，断言拒绝一次 host 后出现含 `rule` 字段（如 `"deny:.evil.com"`）的审计行。

---

### D3/S10 — secrets / keyring · 台账 `partial` ⚠️ **虚报（测试执行层）**

> acceptance：secret 不入日志/DB 明文；keyring 读写删；无 keyring 安全降级

**1. secret 不入日志/DB 明文** — 部分
- 依据：`internal/secrets/secrets.go::NewRedactor/NewSafeOutput`（Redactor / SafeLogger / SafeOutput / RedactJSON）；接线为真 —— `bootstrap.go::Build` 里四处：MergeRedactors、注册已解析 provider key、`st.SetRedactor`、`httpCfg.Redactor`。**缺口**：`store.redact` 只覆盖 **3 条写路径** —— `CreateSession` / `AppendMessage` / `UpdateSessionTitle`（`store.go::Store/Store.redact` 注释即列此三条）。**`WriteMemory`（`internal/store/memory.go`）、tasks、VCS blob 内容都不脱敏** → 「不入 DB 明文」只对会话标题与消息成立。`internal/store::TestStore_RedactsAllWritePaths` 名叫 "All" 但只测那三条（名实不符，断言本身是真的）；`internal/secrets::TestSecurity_NoPlaintextInEncryptedFile`（真，读原始文件字节）、`TestSecurity_RawLiteralFailsClosedWithoutLegacyOptIn`、`internal/tools::TestAuthorizeLogsDecisionWithoutArguments`（真，断言 `sk-test` 不出现在 slog）。
- 证据形状：补 `WriteMemory` / task artifact 路径的脱敏断言；或把子句范围缩到会话表。

**2. keyring 读写删** — 未兑现（**测试维度；本包最关键的取证点**）
- 依据：实现真实（`keyring_enabled.go::osKeyringStore.Get/osKeyringStore.Delete` Get/Set/Delete，错误分类 `ErrSecretNotFound` vs `ErrKeyringUnavailable`）。但**唯一覆盖 Set/Get/Delete 的 `TestKeyring_RoundTripWhenAvailable` 实跑 SKIP**：
  ```
  SKIP TestKeyring_FailsGracefullyWhenUnavailable  (OS keyring IS available on this host)
  PASS TestKeyring_MissingEntryReturnsSecretNotFound
  SKIP TestKeyring_RoundTripWhenAvailable          (readable but not writable: set failed: exit status 36)
  SKIP TestKeyring_AvailableProbeHit               (同上)
  ```
  4 条里 3 条 SKIP，唯一 PASS 的只覆盖 **Get-miss**，读/写/删一个都没验证。假证据类别：**不会被执行（环境性 t.Skip）**。`requireKeyringWritable`（`secrets_extra_test.go`）有 `YANSHI_E2E=1` 逃生门把 skip 变 fail，但 `.github/workflows/ci.yml` **从不设置 `YANSHI_E2E`** → 该门在任何 CI 作业里都不生效。
- 证据形状：CI 里加一个设置 `YANSHI_E2E=1` 的 job（Linux + gnome-keyring / Windows wincred）；或接受「读写删由 fake Store 覆盖」并把子句改写为对 `Store` **接口契约**的断言。

**3. 无 keyring 安全降级** — 部分（两半：Manager 半真，build-tag 半**完全不执行**）
- 依据：
  - **(b) Manager 半 —— 真且无条件执行**：`manager.go::NewManager` 的 `"auto"` 分支（`Available()` 失败 → 有 passphrase 则 `fileStore`，否则 store=nil + warn，**从不 fatal**）。`internal/secrets::TestManager_NewManagerAllModes` 的四个子测试通过 `withFakeKeyring(t, &fakeStore{avail: ErrKeyringUnavailable})` 注入 `newKeyringStore` seam，断言 `*FileStore` 类型回退与两条 warn 文本。**这是本条的实质证据。**
  - **(a) build-tag 半 —— 零测试**：`keyring_disabled.go`（`//go:build nokeyring`）的 `noKeyringStore` 四个方法全返回 `ErrKeyringUnavailable`，但**没有任何 workflow 跑 `go test -tags=nokeyring`** —— `grep -rnE 'go test[^|&;]*nokeyring' .github/workflows/` 产出空输出（负向对照：同目录下 `grep -rnE 'go test' .github/workflows/` 命中 `ci.yml`/`nightly.yml`/`docs.yml` 多行，所以空不是模式写窄了）。**这句原先写的是「全仓 grep `nokeyring` 只命中实现文件与注释」，过松**：那条 grep 还命中 `internal/cli/doctor.go` 里的生产字符串与 `.goreleaser.yaml`/`ci.yml` 里真实的 tag 传递，承重结论靠的一直是「没有 workflow 拿这个 tag 跑测试」而不是「没人提过这个词」。`ci.yml` 的 `build` job（`tags: [default, nokeyring]` 矩阵）**只做 `go build` + `./yanshi -h` 冒烟，不跑 `go test`** → **`noKeyringStore` 的四个方法在任何环境下都从未被执行过**（假证据类别：**不会被执行（`//go:build` tag）**）。
- 证据形状：CI 增一个 `go test -tags=nokeyring ./internal/secrets/...` 作业；或把 (a) 半的断言写成不依赖 tag 的形式（当前 `Store` 接口已足以做到）。

---

## W6 — 工具生态

### A3/C13 — `/mcp` 实化管理界面 · 台账 `partial`

> acceptance：展示 server/tool/status/error；enable/disable 生效；状态与 client 实际连接一致

**1. 展示 server/tool/status/error** — 部分
- 依据：`internal/mcp/manager.go`（server 注册与工具枚举）+ TUI `/mcp` 渲染层。manager 侧枚举有单测；TUI 渲染侧断言是「字符串包含 label」形状 —— **恒真空壳**：断言的是模板字面量而非「数据来自真实 manager 状态」，把渲染函数换成硬编码常量仍绿。
- 证据形状：注入含 2 个 server（一个 connected、一个 error 带具体 reason）的 fake manager，断言渲染结果中 error 行**逐字包含该 reason**（一个不出现在模板里的随机 token），且 tool 计数等于 fake 提供的数量。**骗过去**：`Contains(out, "MCP")` 或 `Contains(out, "status")`。

**2. enable/disable 生效** — 未兑现
- 依据：未找到从 `/mcp` 界面写回 manager 启停状态的生产路径。无测试。
- 证据形状：disable 某 server 后断言其工具**从模型可见的工具集里消失**（对 `ToolNames`/registry 快照做前后 diff），再 enable 后恢复。**骗过去**：只断言 UI 上的复选框状态位翻转。

**3. 状态与 client 实际连接一致** — 未兑现
- 依据：无「界面状态由 client 实连状态派生」的对账逻辑。无测试。
- 证据形状：让 fake client 在测试中途转为断连，断言下一次渲染的 status 由 connected 变为 disconnected，且这个变化**不需要额外刷新动作**。

---

### A3/MCP1 — MCP palette 发现 · 台账 `partial`

> acceptance：palette 含 MCP 工具分组；disabled/failed 可见标灰；命名与模型可见一致

**1. palette 含 MCP 工具分组** — 部分
- 依据：TUI command palette 的工具列表构建处有实现，但**无针对「MCP 工具被单独分组」的断言**。
- 证据形状：注册两个 MCP 工具 + 若干内建工具，断言 palette 条目序列中 MCP 两项**相邻且带同一 group 标记**，且 group 名派生自 server 名而非常量。

**2. disabled/failed 可见标灰** — 未兑现
- 依据：未见 disabled/failed 态在 palette 条目上的样式/标记分支。无测试。
- 证据形状：断言 failed server 的工具条目**携带 disabled 标记且仍出现在列表中**（「可见」是这句的重点 —— 过滤掉 = 不算标灰），并断言选中它时被拒绝。

**3. 命名与模型可见一致** — 未兑现
- 依据：无对账逻辑，无测试。
- 证据形状：同一个 fake MCP server，分别取 palette 展示名与注册进 orchestrator 的工具名，断言两者**逐字相等**。**骗过去**：两边都调用同一个格式化函数再断言相等（重言式）—— 必须**一端取自真实 registry 快照**。

---

### A3/V16 — 通用 MCP Client · 台账 `partial` ⚠️ **第 3 句虚报**

> acceptance：stdio/HTTP server 可配可连；tools/resources 可用；启动超时/重连/权限检查有测试；命名冲突可诊断

**1. stdio/HTTP server 可配可连** — 部分
- 依据：`internal/mcp/manager.go`（stdio 为主路径；配置经 `internal/config` 的 mcp servers 段）。stdio 有连接测试；**HTTP 传输侧缺独立驱动**。
- 证据形状：两种传输各起一个 in-test server，断言 `ListTools` 返回的工具名集合与 server 声明一致。

**2. tools/resources 可用** — 部分
- 依据：tools 侧可用且有测试；**resources 侧未见完整实现，无测试**。
- 证据形状：断言 `resources/list` + `resources/read` 往返后拿到的内容字节与 server 提供的一致。

**3. 启动超时/重连/权限检查有测试** — 未兑现（**这句字面要求「有测试」，逐项查**）
- 依据：**启动超时：无测试**；**重连：无测试**；权限检查：部分 —— guard 的 mcp 维度 fail-closed opt-in 有测试，但未与 manager 端到端串起来。
- 证据形状：超时 —— 起一个永不响应 initialize 的 server，断言在配置超时内返回错误**且不阻塞其余 server 的装配**；重连 —— 杀掉子进程后断言下一次调用触发重启且工具集恢复；权限 —— 断言空 allowlist 时 MCP 工具调用被拒且拒绝理由可辨。

**4. 命名冲突可诊断** — 未兑现
- 依据：无冲突检测/命名空间化逻辑，无测试。
- 证据形状：两个 server 声明同名工具，断言启动后**要么**两者以带 server 前缀的不同名字共存、**要么**产生一条**指名两个 server** 的诊断；且断言那条诊断能被操作员读到（进 stderr 或 diagnostics 结果），而不只是内部 map 的覆盖行为。

---

### B2/LSP1 — LSP 诊断回喂 · 台账 `partial`

> acceptance：编辑后模型收到诊断；server 缺失安全降级；超时不阻塞 turn；Go/Python/TS 至少一种端到端可用

**1. 编辑后模型收到诊断** — 部分
- 依据：`internal/tools/fs_patch.go::diagForStaged`（编辑后取 `LSPFromContext` 拉诊断）、`internal/tools/lspctx.go::diagFor`、注入点 `orchestrator.go::Orchestrator.bindExecutionContext`。有「编辑工具返回值里带上诊断文本」的断言；**没有**测试证明诊断进入了**模型实际读到的消息**（工具返回值 → ADK → 模型这一段未被驱动）。
- 证据形状：跑一个完整 turn，用 `FakeModel` 记录它收到的消息序列，断言 tool_result 消息体中**逐字**含 stub LSP 给出的 diagnostic message。**骗过去**：只断言编辑工具（真实注册名是 `apply_patch`，实现在 `internal/tools/fs_patch.go`）的返回字符串含诊断。

**2. server 缺失安全降级** — 已兑现（台账 evidence 为空，属**低报**）
- 依据：`lspctx.go::LSPFromContext`（未绑定则 ok=false）、`diagnostics.go::runLSPProbe`（source==nil 或 !Enabled 返回空 lspDiag）；`internal/tools::TestDiagnosticsLSPUnavailableIsLocalDegradation` 真断言（JSON 中 `"lsp":{"available":false,...}`）。
- 证据形状：已满足。加强方向：断言 server 缺失时编辑工具**照常返回成功结果**而非报错。

**3. 超时不阻塞 turn** — 未兑现
- 依据：参数存在（`diagnostics.go::runLSPProbe` 传 2s、`internal/tools/fs_patch.go` 侧传入 timeout），但**没有任何测试让 LSP source 卡住超过 timeout**。
- 证据形状：stub 的 `Diagnostics` 阻塞 10s，断言整个 turn 在 timeout+ε 内完成、且结果里诊断段缺失但其余段完整。**骗过去**：断言 timeout 参数值等于 `2*time.Second`（对字面量断言）。

**4. Go/Python/TS 至少一种端到端可用** — 未兑现
- 依据：`internal/lsp/manager.go` 有子进程拉起逻辑，但**无「真实 language server 端到端」测试** —— 现有测试全部走 stub/内存实现，真实 server 路径零覆盖。
- 证据形状：起真实 `gopls`，改一个引入编译错误的 Go 文件，断言拿到的 diagnostic 的 `Range` 行号等于那一行；且该测试需以「PATH 上无 gopls 则 skip」的形式存在 —— ⚠️ **若 CI 从不装 gopls，这条就落回「不会被执行」类假证据，必须在 CI 里装。**

---

### B3/DT4 — run_tests 工具 · 台账 `partial` ⚠️ **第 1、2 句虚报**

> acceptance：至少 Go 解析正确；结构化计数+失败列表；超时/取消干净；大输出成 artifact

**1. 至少 Go 解析正确** — 部分（**新发现：对真实 `go test -json` 解析是错的**）
- 依据：`internal/tools/testrun.go` `parseGoJSON`。`TestParseGoJSONCountsPassFailSkip` 等断言真实，但**全部 fixture 是人造的、每条事件都带 `Test` 字段**。实测 `go test -json ./internal/version` 输出 9 条 test 级 pass + **1 条包级 pass（无 `Test` 字段）**；`parseGoJSON` **不过滤 `ev.Test != ""`** → `Passed` 按包数虚增；失败场景下包级 fail 事件会往 `Failures` 塞一条 `Test` 为空的**幻影条目**。类别：**fake 太宽 / fixture 系统性回避了会失败的输入** —— 全仓每一处 `"Action":"pass"` fixture 无一例外（现算：`grep -rn '"Action":"pass"' --include='*.go' internal/`；此处刻意不写死处数，写下的那一刻就开始腐烂，初版写「6 处」而当时实测已是 7）。
- 证据形状：把一次**真实** `go test -json` 的完整事件流（含 run/output/包级 pass）存成 fixture，断言 `Passed` 恰等于 test 级 pass 数、`Failures` 中**无空 `Test` 条目**。**骗过去**：继续手写只含 test 级事件的三行 JSON。

**2. 结构化计数+失败列表** — 部分
- 依据：`testrun.go`（`testResult` / `testFailure`）。四条真证据（`TestRunTestsExecutesGoTestJSONWithWorkspaceWriteTier`、`TestRunTestsReportsRunnerFailureInsteadOfPass`、`TestRunTestsKeepsFailWhenTestsActuallyFailed`、`TestRunTestsStaysPassOnCleanRun`）走 `runTool` 全链，断言 argv、sandbox tier、status、以及 summary 逐字含 runner 自己的 stderr 原文。但**计数正确性受 #1 的缺陷污染**。
- 证据形状：同 #1；另需断言 `Failures[i].Package` / `Test` 与真实事件流一一对应。

**3. 超时/取消干净** — 未兑现
- 依据：实现在（`testrun.go::runTests` 默认 10min + clamp、`secproc_capture.go::runSecureCapture` `context.WithTimeout`），但**两条候选都是假证据**：
  - `internal/tools::TestRunTestsDefaultTimeout`（`remaining_coverage_test.go`）—— **吞错空壳 + 不会被执行**。实跑日志显示 `decision=deny source=fail_closed reason_code=missing_profile`：ctx 没绑 profile，GuardedTool 在 guard 层就拒了，**`runTests` 一行都没执行**；且 err 被 `t.Logf` 吞、结果 `_ = out`，零断言。
  - `internal/tools::TestRunSecureCaptureCancellationStopsWait`（`secproc_capture_test.go`）—— 顶不上：(a) 它调的是下一层 `runSecureCapture`，`Tool:"run_tests"` 只是字符串标签，不经过 `runTests`；(b) ctx 在调用前已 `cancel()`，`exec.Cmd.Start` 在 ctx 已 done 时直接返回 `ctx.Err()`，**子进程从未启动**，drain/Wait/kill 整条路径没被执行 —— 名字里的 "StopsWait" 从未发生。
- 证据形状：scripted factory 返回 `Block:true`，以 `{"timeout_s":1}` 走 `runTool`，断言 (a) 墙钟 < 5s，(b) 结果串含 `"run_tests: "` + deadline 文本，(c) 子进程已被回收（Wait 已返回 / pid 不再存活）。另需一条钉住「默认 10 分钟」的断言（把 timeout 传进可捕获的 runner），否则改回 `clampInt(0,1,1800)` 那个 1 秒 bug 不会让任何测试变红。

**4. 大输出成 artifact** — 未兑现
- 依据：实现在 `testrun.go::runTests`，但**这条路径无测试**。`internal/tools` 里碰 `SpillThreshold` 的测试文件共 7 个（`spillover_test.go`、`spillover_coverage_test.go`、`artifact_output_test.go`、`fs_test.go`、`gate_test.go`、`guard_test.go`、`review_test.go`），分两种形状，**没有一种经过 run_tests**：
  - 直接调 `writeArtifactOrSpill` 这个**通用 helper**，其中 `"git-diff"` / `"task-7"` 只是**字符串字面量当 label**；
  - 或者走真实 `GuardedTool` 的溢出路径（`guard_test.go::TestGuardedTool_SpillsOversizedResult` / `::TestSpillRoundTrip_FsReadReadsSpilledFile` 用一个返回 `SpillThreshold+1` 字节的**合成工具**）—— 这一种证明的是 GuardedTool 的溢出层通用可用，仍然不触及 `runTests` 自己怎么处理大 stdout。
- 证据形状：factory 返回 > 64 KiB 的 stdout，断言 `ArtifactRef` 非空、`FakeManager.ReadArtifact` 能读回**全量字节**、`Summary` 长度被截到 4096。

---

### B3/DT5 — diagnostics 工具 · 台账 `partial`（本次拆解后由 `done` 退回）· 第 1 句有生产侧硬伤

> acceptance：一次调用聚合；各子项可独立失败不拖垮；toolchain 版本准确

**1. 一次调用聚合** — 已兑现，**但带一条生产侧硬伤**
- 依据：`internal/tools/diagnostics.go::runDiagnostics`；`TestDiagnosticsAggregatesIndependentProbes` 一次 `runTool` 同时断言 git/toolchain/lsp 三段，非假证据。
- ⚠️ **硬伤：LSP 维度在生产里是死的**。`defaultFileLister.recentFiles` 恒返回 `nil`（`diagnostics.go`），而 `bootstrap.go::Build` 传的是 `nil` → override 保持默认 → 生产环境 `open_diagnostics_count` **恒为 0**。测试之所以拿到 1，是因为注入了 `diagTestProbe{files:["a.go"]}` —— **这个 probe 在生产不存在**。这是一处占位实现（违反仓库「禁占位」约定），且让 `done` 的第 1 句在生产路径上只聚合了 4/5 个维度。
- 证据形状：要么补齐 `defaultFileLister` 的真实实现并断言**生产构造**（`NewDiagnosticsTool(nil)`）下也能拿到非零诊断数，要么把 LSP 行从「聚合」的承诺里摘出去。
- **已回退 `partial`**（2026-08-04）：硬伤经实测确认 —— 把 `TestDiagnosticsAggregatesIndependentProbes` 的构造换成生产形态 `NewDiagnosticsTool(nil)`、其余一律不变（stub LSP 仍报 `a.go` 有一条 error 诊断），结果是 `available=true open_diagnostics_count=0`。三条 `ledger: B3/DT5#n` marker 已随之删除。
- ⚠️ **2026-08-07 推翻上面这条判定，改回 `done`。** 那个 lister 是 **override**，在真实 source **之后**被查询；生产读的是 `internal/lsp/manager.go::OpenDocuments`，而 `internal/tools/fs_patch.go` 与 `internal/tools/lspctx.go` 在 agent 每次编辑后经 `DidChange` → `rememberOpen` 填充它。**这一行恰好在「agent 编辑过东西」时是活的**，而那也是它唯一有东西可报的时候。当时那次实测拿到 0，是因为**当时的 stub 的 `OpenDocuments()` 返回空**（它此后被改成返回 seed 的路径集合）—— 量到的是 stub 的性质，不是生产代码的洞。新证据 `internal/tools::TestDiagnosticsLSPRowIsLiveInTheProductionConstruction` 用生产构造 `NewDiagnosticsTool(nil)` 驱动，变异探针（让 `OpenDocuments` 不再被消费，即这条判定描述的那个洞）当场打红。**教训与 W10-T4 的 `^style` 同形：写下的实测结论会随被测物一起腐烂，复核时要重跑而不是重读。**

**2. 各子项可独立失败不拖垮** — 已兑现
- 依据：`diagnostics.go::runGitProbe`（git 吞错）、`::runToolchainProbes`（toolchain `continue`）、`::runLSPProbe`（LSP 短路）；`TestDiagnosticsGitFailureDoesNotHideOthers`（git exit 128 时断言 toolchain 行完整）、`TestDiagnosticsLSPUnavailableIsLocalDegradation`。
- 覆盖缺口（不足以推翻）：没有「toolchain 探针失败而 git/lsp 存活」的反向用例；也没有 `secureCommandRunner` 返回 **error**（而非非零退出）的用例。
- 证据形状：补一个只让 `cargo` 探针 launch 失败的用例，断言 go/node 两行仍在。

**3. toolchain 版本准确** — 已兑现
- 依据：`diagnostics.go::toolchainProbeArgv`（argv 表）、`::runToolchainProbes`；`TestDiagnosticsProbesEachToolchainWithItsOwnVersionArgv` 是**强证据** —— fake `toolchainProbeReply` 会像真实 `go` 一样**拒绝错误 argv**（返回 exit 2），专门堵住了「fake 太宽」这个曾让 bug 三次逃逸的漏洞；同时断言三行版本值非空。
- 证据形状：**这是本仓 fake 设计的范本** —— fake 必须对关键入参敏感、错误入参必须失败。

> 附带风险（非验收句）：`NewDiagnosticsTool` 会写包级全局 `diagFileListerOverride` 且测试不还原，是并行测试污染面。

---

### B3/GH1 — GitHub 工具集 · 台账 `partial`

> acceptance：只读 context 可用；写操作需审批且需证据；大 body 成 artifact；未认证明确降级；注入内容不被当指令执行

**1. 只读 context 可用** — 已兑现
- 依据：`internal/tools/github.go::runGitHubPRContext`（`gh pr view --json`）；`github_test.go` 有 scripted-factory 驱动的 argv + 结果解析断言。
- 证据形状：断言 argv 逐字 + 结果字段（现状即是）。

**2. 写操作需审批且需证据** — 部分
- 依据：`github.go::runGitHubComment/runGitHubApprove/runGitHubMerge`（comment / approve / merge）。审批门有覆盖（`TestApprovalPromisedInDescriptionIsEnforced` 遍历全部受管工具）；但**「需证据」这半句 —— 写操作必须携带可审计的依据 —— 无实现也无测试**。
- 证据形状：断言写工具在无 evidence 参数时被拒，且审批弹窗的 payload 中逐字含将要提交的 body。

**3. 大 body 成 artifact** — 未兑现
- 依据：GitHub 工具**未接** `writeArtifactOrSpill`（与 run_tests / git_diff 不同，这里**连分支都没有**）。无测试。
- 证据形状：让 `gh pr view` 返回 > 64 KiB JSON，断言结果携带 ArtifactRef 且 inline 部分 < SpillThreshold。

**4. 未认证明确降级** — 部分
- 依据：非零退出经 `commandFailureTail` 回传 `gh: not authenticated` 原文（`secproc_capture.go`），但**无专门驱动未认证退出码的用例**。
- 证据形状：factory 返回 exit 4 + `gh auth login` 提示，断言结果 status **明确标记为「未认证」**而非泛化 error，且**不是**空结果。

**5. 注入内容不被当指令执行** — 未兑现
- 依据：issue/PR 正文未做包裹、标注或转义。无测试。
- 证据形状：让 issue body 含 `IGNORE PREVIOUS INSTRUCTIONS…`，断言工具返回值中该段被**明确的定界标记**包住（一个模型侧可识别的 sentinel），且该 sentinel 不出现在正文可控范围内。**骗过去**：断言正文原样返回 —— 那恰恰是漏洞本身。

---

### B3/T11 — web_search 工具 · 台账 `missing`（第 3、4 句**低报**）

> acceptance：返回标题/摘要/URL；域名/时间过滤生效；重定向受策略约束；后端不可用降级

**1. 返回标题/摘要/URL** — 未兑现（**残桩**，比台账写的更糟）
- 依据：`internal/tools/web.go::WebTools.runSearch` —— `searchItem{Title: line, URL: ""}`：**Title 是整行原始 HTML，URL 恒为空，Snippet 从不填充**。`TestCov_WebRunSearchHTMLParse` 是**恒真空壳 / 对解析质量变异盲**：断言 `Contains(out,"Example")` 与 `"Test Org"`，而这两个词就在原始 HTML 行里 —— 把「解析」换成「原样透传」仍绿；全程从不断言 `URL != ""`。
- 证据形状：断言 `results[0].URL == "https://example.com"`、`Title == "Example"`（**不含尖括号**）、且 `Title` 中不出现 `<a`。

**2. 域名/时间过滤生效** — 未兑现
- 依据：`webSearchArgs`（`web.go::WebTools.runFetch`）只有 `Query` / `MaxResults`。无实现无测试。
- 证据形状：传 `allowed_domains:["a.com"]` 断言 b.com 结果被剔除；传时间窗断言请求 URL 携带对应参数**且**越窗结果被剔除。

**3. 重定向受策略约束** — 部分（台账在这一句上**低报**）
- 依据：`runSearch` 的 client **无 `CheckRedirect`**（对比 `runFetch` 的 `web.go` 有完整实现）；**但**其 Transport 是 `netpolicy.NewTransport(policy)` → `PolicyDialer.DialContext`（`internal/netpolicy/proxy.go`），**每次新建连接**都做 `CheckHost` + `CheckResolvedIPs` + IP 钉死 → 跨 host 重定向在拨号层会被拒，**不是「完全无约束」**。仍缺：无跳数上限（依赖 Go 默认 10）、同 host 重定向不受限、**无测试**（现有 4 条重定向测试全部打 `web_fetch`）。
- 证据形状：起两个 httptest server，A 302 到 B，policy 只允许 A，断言 `web_search` 请求失败且理由来自 netpolicy（含 B 的 host 名）；**再补一条 policy 允许 B 时能跟随成功**，否则测试无法区分「被策略拒」与「压根没跟随」。

**4. 后端不可用降级** — 已兑现（但台账原本引用的那条是空壳）
- 依据：`web.go::WebTools.runSearch`（网络错误返回空结果集而非硬失败）；**真证据是 `internal/tools::TestCov_WebRunSearchNetworkDegradation`**（断言 `Contains(out, "results":[]")`）。
- ⚠️ 四条假证据点名：`TestWebSearchReturnsEmptyOnUnreachable`（**恒真空壳** —— 只断言 `Contains(out,"results")`，而 `"results"` 是 `searchResult` 的固定 JSON key，**成功时也在**；台账原本挂的就是这条）、`TestWebRunSearchNoHostPolicy`（**吞错空壳**，`t.Logf` 吃 error、零断言）、`TestWebRunSearchDeniedWithoutPolicy`（**不会被执行** —— 实跑 guard `decision=deny` 且 `err==nil`，断言体全在 `if err != nil` 内）、`TestWebRunSearchInitialization`（**恒真空壳**，只断言 `searchBase` 等于源码里的字面常量）。
- 证据形状：断言降级后的**结构化空结果**（`"results":[]`），不是断言 JSON key 存在。**处置**：台账引用应换成 `TestCov_WebRunSearchNetworkDegradation`。

---

### B3/V13 — 结构化 code review · 台账 `partial`

> acceptance：支持三种 base；findings 结构化含 severity/file/line；clean 明确；只读不改

**1. 支持三种 base** — 未兑现
- 依据：`internal/tools/review.go` 的 `reviewInput` 只有 `{diff, task_id, repo, number}`，全仓无 base/baseline 参数，无 HEAD~ / branch / staged 基线选择逻辑 —— 调用方必须自己把 diff 文本塞进来。无测试。
- ℹ️ `git_diff` 工具**本身**已支持三种 scope（`working_tree`/`base_ref`/`commit`，`git.go::NewGitTools`）；若验收意图是「review 能消费这三种 base」，接线成本远低于台账注释给人的印象。
- 证据形状：三种 base 各跑一次，断言产生的 **git argv 各不相同**、且 findings 覆盖的文件集**因 base 不同而不同**（这一点是关键 —— 见 B3/W07#4 里同形状的失效案例）。

**2. findings 结构化含 severity/file/line** — 已兑现
- 依据：`reviewFinding` 含 `File`/`Line`/`Severity`；`internal/tools::TestStreamReviewDedupesAndSortsFindings` 真断言（去重 + 排序 + 字段）。

**3. clean 明确** — 已兑现
- 依据：无 findings 时返回明确的 clean 结果而非空对象，有覆盖。
- 证据形状：断言 clean 时结果含一个**与「零个 finding」可区分**的显式标记（模型不能靠 `findings.length===0` 猜）。

**4. 只读不改** — 已兑现
- 依据：review 工具无写路径，间接覆盖。
- 证据形状：跑完 review 后对工作树做 checksum 比对断言零变化。目前是「实现上没有写调用」而非「测试断言了不变量」—— 加强建议而非缺口。

---

### B3/W07 — git_status / git_diff 专用工具 · 台账 `partial` ⚠️ **第 1、4 句虚报（含一条安全缺陷）**

> acceptance：status/diff 结构化；不修改用户配置；大 diff 成 artifact；边界清晰

**1. status/diff 结构化** — 部分（**新发现两个真实解析 bug**）
- 依据：`internal/tools/git.go` `parseGitStatusZ`、`parseGitNumstatZ`。`TestGitStatusParsesPorcelainV2ZWithHostileNames` 的 **fixture 系统性回避了出 bug 的分支** —— 它写的文件**全部是未跟踪的**，走 `? ` 前缀分支（`record[2:]`，正确）；已跟踪条目（type `1`/`2`）分支**从未被驱动**。实测两个缺陷：
  1. 已跟踪且**路径含空格**时，`subFields[len-1]` 取路径 → `1 .M … a b.txt` 被解析成 `path="b.txt"`；
  2. **rename 的 origPath 是独立 NUL 记录**，若含 ≥2 个空格会被当成一条**伪造的 status 条目**：实测 `x y z.txt` → 输出 `{"xy":"y","path":"z.txt"}`（一个不存在的文件，带一个不存在的状态码）。
  `git_diff` 侧的 `TestGitDiffReturnsOneRecordPerFileWithBinaryMarker` / `TestGitDiffWorkingTreeIncludesStagedUnstagedUntracked` 是**真证据**（真 git、真断言），成立。
- 证据形状：fixture 必须包含**已 add/已 commit** 的含空格路径与一次 `git mv` 重命名，断言返回的 `path` 逐字等于原始文件名、且 **entries 数量恰等于 git 报告的变更数（不多不少）** —— 这条才抓得到幻影条目。

**2. 不修改用户配置** — 已兑现（观测量对），但守护弱
- 依据：`git.go` `gitEnvIsolation`；`TestGitToolsDoNotWriteGitConfig` 断言 global config 字节不变 —— **断言的正是子句本身**，不算假证据。但**变异盲**：`git status`/`git diff` 本来就不写 config，把 `gitEnvIsolation(root)` 从所有 spec 里删光，这条测试仍然绿。
- 证据形状：额外断言 spec.Env 含 `GIT_CONFIG_NOSYSTEM=1` 且 `XDG_CONFIG_HOME` 落在 root 内；并在 global config 里放一条**会改变输出**的设置（如 `status.showUntrackedFiles=no`），断言工具输出不受其影响 —— 这样删掉隔离就会红。

**3. 大 diff 成 artifact** — 未兑现
- 依据：实现在 `git.go::collectGitDiffFiles` `writeArtifactOrSpill`，但**无测试**。`TestWriteArtifactOrSpill*`（`artifact_output_test.go::TestWriteArtifactOrSpillUsesTaskManager/TestWriteArtifactOrSpillMarksFallbackDegraded/TestWriteArtifactOrSpillNoWorkRoot`）测的是通用 helper，其中 `"git-diff"` 只是**字符串字面量当 label**，不经过 git 工具；`git_test.go` 中无任何 fixture 超过 64 KiB。
- 证据形状：commit 一个 > 64 KiB 的文本文件改动，断言该文件的 `ArtifactRef` 非空、`Patch`（= `art.Summary`）显著短于原 patch、且从 FakeManager 读回的 artifact 字节与真实 patch 一致。

**4. 边界清晰** — 部分（安全缺陷已修，剩一条空壳证据）
- 依据：`git.go::runGitDiff`（路径越界）、`git.go::validateGitRef`。`TestGitDiffRejectsPathEscape` 与 `validateGitRef` 单测是真证据；但 **`TestGitDiffScopesBaseRefAndCommit` 恒真 / 不可区分** —— 三种 scope 都只断言 `Contains(out, "path":"a.go")`，而该 fixture 下三种 base 的答案**完全相同**，把 `base_ref`/`commit` 都退化成 `working_tree` 也照样绿。**这是本条判「部分」的唯一原因。**
- ✅ **曾经的参数注入漏洞已在 `03a6bb3`（W1 范围内）修复**，本段随之改写。原缺陷：`validateGitRef` 只拒空白与 NUL，`commit` scope 把 ref 原样放进 argv（无 `...HEAD` 拼接），`scope.ref = "--output=<绝对路径>"` 于是让一个声明 `sandbox.ReadOnly` 的工具在工作根外写出文件。现状是**双层防御**：① `git.go::validateGitRef` 委托 `argvsafe.go::validateArgvOperand`，拒绝一切 `-` 开头的操作数；② `gitDiffCommands` 的每条 argv 都在 ref 前放 `git.go::gitEndOfOptions`（`--end-of-options`），让 git 自己拒绝把操作数读成选项。两层刻意不冗余：前者能推广到没有 `--end-of-options` 的程序（go、cargo），后者能扛住未来某个忘记校验的调用方。
- 证据形状与现状：(a) 三种 scope 的 fixture 必须让三者答案**互不相同**（如各 base 下变更文件集不同），断言各自的文件集 ——【仍缺】；(b) 断言 `validateGitRef("--output=/tmp/x")` 与 `validateGitRef("-x")` 返回 error ——【已有】`argvsafe_test.go::TestValidateArgvOperandRejectsOnlyOptionShapes`；(c) 跑一次带该 ref 的 `git_diff`，断言工作根外的目标路径**不存在** ——【已有】`argvsafe_test.go::TestGitDiffRefCannotWriteFilesOutsideWorkRoot`，另有 `::TestGitDiffPassesEndOfOptionsBeforeRef` 钉住第二层、`::TestGitDiffAcceptsRefsContainingDashes` 作反向探针（含 `-` 但不以 `-` 开头的 ref 不被误伤）。

---

### M1/SPEC-TOOLIF — 工具接口标准 · 台账 `partial`

> acceptance：所有工具统一到 Tool 接口（DisplayName+DefaultTimeout+Stream）；输出走固定字段 ToolChunk；废弃 JSON 包装与 ToolProgressCallback/lineProgressWriter；subagent 进度喂 Status+Text；TUI 固定模板渲染

**1. 所有工具统一到 Tool 接口（DisplayName+DefaultTimeout+Stream）** — 部分
- 依据：`GuardedTool` 统一承载三者（`NewGuardedTool(name, display, desc, timeout, params, stream)`，见 `testrun.go::NewTestRunTool`、`diagnostics.go::NewDiagnosticsTool`、`git.go::NewGitTools`）。但**无「清点全部工具都实现新接口」的门禁测试** —— 新增一个绕过 `GuardedTool` 的工具不会让任何测试变红。
- 证据形状：一条**治理测试**，遍历 `App.ToolNames` / 注册表，断言每个工具都有非空 `DisplayName` 与非零 `DefaultTimeout`，并以**计数相等**（`len(all) == len(checked)`）防漏 —— 与 GOV5/GOV7 同形状。

**2. 输出走固定字段 ToolChunk** — 部分
- 依据：`SyncStream` 把返回值包成 `ToolChunk`。只有间接覆盖（各工具测试断言最终结果串），**无对 `ToolChunk` 字段契约本身的断言**。
- 证据形状：断言流式路径下每个 chunk 的字段集合恰为规范字段，且 `Err` 与 `Text` 的互斥/组合语义被钉死。

**3. 废弃 JSON 包装与 ToolProgressCallback/lineProgressWriter** — 部分（**事实已满足，缺的只是防回流门禁** —— 定档理由见「状态三档」那条 ⚠️）
- 依据：这句是**「废弃」类要求** —— 只要生产代码里还有调用点就不算兑现。上一版自陈「未完成清点」，现已补上（2026-08-05 实测，命令可重跑）：

  ```
  $ grep -rn 'ToolProgressCallback' --include='*.go' . | wc -l
  0
  $ grep -rln 'lineProgressWriter' --include='*.go' .
  internal/tools/shell.go
  ```

  即：一个全仓零命中，另一个只剩 `internal/tools/shell.go` 里的一行**注释**（提到旧名字，不构成声明或调用），零调用点、零声明。
- **为什么仍不翻**：「废弃」类要求的兑现物不是「今天数出来是 0」，而是**防回流**。当前没有任何门禁阻止这两个名字重新出现，所以翻成「已兑现」等于把一条无人看守的事实写进台账 —— 正是本文件反复点名的「断言与被断言物脱钩」。翻牌与那道禁止性扫描测试应当同一个提交落地（归 W6）。顺带：本条所属台账项还有第 4、5 两条「未查证」，它无论如何都到不了终态，翻这一条也解不开任何东西。
- 证据形状：一条 archtest 风格的**禁止性**扫描测试，断言非测试 `.go` 文件中 `ToolProgressCallback` / `lineProgressWriter` 的出现次数为 0（或只出现在一张只减不增的豁免表里）—— 这是唯一能防止废弃物回流的形状。功能测试对「废弃」类要求无效。

**4. subagent 进度喂 Status+Text** — 未查证
- 依据：待查（`internal/tools/agent_*.go`、`internal/agent/orchestrator` 一线）。
- 证据形状：跑一次子代理委派，断言父 turn 收到的 chunk 序列中至少一条 `Status` 非空、一条 `Text` 非空，且二者来自同一次子代理运行。

**5. TUI 固定模板渲染** — 未查证
- 依据：待查（`internal/cli/tui`）。
- 证据形状：对同一 `ToolChunk` 输入断言渲染输出**逐字等于模板展开结果**（golden），且换一个工具名只改模板里的那一处。

---

## W7 — 可观测性与运维

### C4/COST1 — $ 成本估算 · 台账 `partial`

> acceptance：/cost 显示 $；聚合正确；价格可配；缓存价区分

**1. /cost 显示 $** — 部分
- 依据：实现 `internal/cli/tui/commands.go`（`cmdCost` → `requestStatusBlock`）、`::statusEntry.render`（`fmt.Sprintf("    estimated cost: $%.6f")`）。`internal/cli/tui::TestModel_CommandCost_StatusBlock` 是**恒真空壳** —— 它跑了 `/cost`，但 `applyEvent` 里根本不设 `CostUSD`/`CostKnown`，也不断言任何 `$`。真断言 `$` 的是 `TestStatusEntryRendersKnownAndUnknownCost` 与 `TestStatusEntry_RenderVariants`，但 `costUSD` 是**手写字面量**塞进 struct 的，不经 `CostOK`/priceTab。「渲染 $」与「算出这个数」两半从未接上。另注：`einollm.FormatCost` 有完整测试却**零生产调用点**，TUI 自己重写了 known/unknown 分支。
- 证据形状：从 `StreamEvent{Kind:"status", CostUSD: <由 CostOK 算出的值>, CostKnown:true}` 走 `applyStatus` → `statusEntry.render`，断言输出含该值对应的 `$` 串。**骗过去**：任何直接 `&statusEntry{costUSD: 0.012345}` 的渲染测试。

**2. 聚合正确** — 部分
- 依据：`internal/api/http/ws_compaction.go::connSession.addProviderUsage/connSession.resetBilling`（`addProviderUsage`，`cs.costUSD +=`）；judge 也计入 `ws.go::Server.ChatWS`。`internal/api/http::TestConnSessionBillsEveryProviderUsageAndJudge` 是**真证据**（3 次 `addProviderUsage` 后 `want := (120*2.0 + 110*0.5 + 17*8.0)/1e6` 数值断言），但**测试名撒谎** —— body 里没有 judge，`ws.go::Server.ChatWS` 没被驱动。缺：跨 turn、restore 回灌（`ws_compaction.go::connSession.loadSession`）、`resetBilling`、SSE 独立台账（`chat.go::Server.handleSSEInternal`）全无测试。另：`einollm.Ledger.Cost` 有强测试但**零生产调用点** —— 被测的聚合函数和上线的聚合路径是两条代码。
- 证据形状：驱动两个真实 turn（含 judge usage）穿过 WS handler，断言第二个 status 帧的 `cost_usd` 等于两轮之和。**骗过去**：只对 `Ledger.Cost` 断言（生产不走它）。

**3. 价格可配** — 部分
- 依据：链条**断成两截且中间不接**。`internal/config::TestLoadObservabilityFeaturesAndPricing` 证 YAML→cfg；`internal/bootstrap::TestBuildWiresFeaturesAndPricing` 证 cfg→`App.Pricing`（且只查 `InputPerM`，配置里的 `CacheHitPerM`/`OutputPerM` 没验）。而 `ws_test.go::TestConnSessionBillsEveryProviderUsageAndJudge` **手搓 priceTab** 绕开了整条 config 路径。**没有任何测试让被 override 的模型产出一个美元数。**
- 证据形状：用带 `pricing.overrides` 的 config 起 `bootstrap.Build`，把 `app.Pricing` 喂进 `addProviderUsage`，断言 `costUSD` 用的是 override 价而非默认价。

**4. 缓存价区分** — 已兑现
- 依据：`internal/llm/eino/pricing.go`（`computeCost` 拆 cached/miss）；`TestComputeCost_ClampsCachedToPrompt`（`want := 1000/1e6*1 + 500/1e6*50`，真按 `CacheHitPerM` 计价）+ `internal/api/http::TestConnSessionBillsEveryProviderUsageAndJudge`（生产累加器上 0.5 与 2.0 两种费率分开）。注意 `TestCostKnownSplitsCacheAndOutput` 只断言 `cached < plain` 是**不等式**，单独拿它做证据不够。
- 证据形状：**等式**断言（cached×CacheHitPerM + miss×InputPerM），不是不等式。

---

### C4/O07 — doctor 增强 · 台账 `partial`

> acceptance：覆盖各子系统；JSON 可机读；失败明确指引
> ℹ️ 与 M1/O07 共用同一份实现（`internal/cli/doctor.go`）。分工：**M1/O07** 管「检查项齐不齐 + 输出/退出码/安全契约」，**C4/O07** 管「覆盖面 + 机读 + 指引质量」。真正只属于 C4/O07 的是第 3 句。

**1. 覆盖各子系统** — 部分
- 依据：`internal/cli/doctor.go::RunDoctor` 共 18 项检查，但报告级别只钉住 14 项。**`secrets` / `locale` / `keymap` / `high-contrast` 四项没有任何测试断言它们出现在 `RunDoctor` 的报告里** —— 只有函数级单测（`doctor_extra_test.go`）。把 `doctor.go` 四行删掉，全部测试仍绿（**GOV4 同形的装配缺口**）。
- 证据形状：把 `RunDoctor` 输出的 name 集合与一个显式清单做**全等**比对（多一项少一项都红），而不是逐个 `names[want]` 存在性检查。

**2. JSON 可机读** — 已兑现
- 依据：`doctor.go::jsonReport/DoctorReport.RenderJSON`（`RenderJSON`）、`cmd/yanshi/main.go::runDoctor`（`-json`）；`internal/cli::TestRenderJSON_Structure` + `cmd/yanshi::TestRunDoctor_JSON`（真 `json.Unmarshal` 并校验 checks/summary）+ `TestRunDoctor_TextNotJSONByDefault`（断言默认不出 `"checks"`）。
- 证据形状：反序列化 + 字段校验（现状即是）。

**3. 失败明确指引** — 未兑现
- 依据：**部分有**（`doctor.go::checkConfig` 「run `yanshi serve`…」、`::checkProviders`「set llm.providers in config」、`::checkConfigVersion`、`::checkLockfile`），**部分完全没有**（`::checkDatabase` 只有 `open %q: %v`、`::checkSecretsRefs` 不说什么才合法、`::checkLocaleConfig` 直接 `err.Error()`）。**指引测试是零星的、不成体系的**：`internal/cli::TestCheckKeymapConfig_InvalidBindingsFail` 确实断言 `::checkKeymapConfig` 的失败消息里含 `tui.bindings`（并反向断言不得含那个幻影斜杠命令名），但那是**唯一**一条这样的测试，其余能产生 `StatusFail` 的 check 一条都没有。
- 证据形状：对每个能产生 `StatusFail` 的 check，断言其 Message 匹配一个「指引词表」（含祈使动词 + 配置键名或命令名）。**骗过去**：只断言 Message 非空，或只断言某一个 check 的字符串。

---

### C4/OBS1 — slog 结构化日志 · 台账 `partial`

> acceptance：关键路径结构化日志；secret 不入日志；级别可配；采样不丢关键错误

**1. 关键路径结构化日志** — 部分
- 依据：`internal/tools/permctx.go`（`auditPermission`，每次权限决策）、`internal/api/http/ws.go::Server.ChatWS`（turn started/finished）、`obslog.WarnErr`（bootstrap 的 VCS 初始化与插件发现失败、`internal/cli/session.go` 的进程内 server 停止）、以及 `internal/observe/otel::Setup` 里三条 collector/exporter 不可用的 `slog.WarnContext`（`bootstrap.Build` 无条件调用它）。**当前全集现算，别信这里的枚举**：`grep -rnE 'slog\.(Debug|Info|Warn|Error|Log)[A-Za-z]*\(|obslog\.(Debug|Info|Warn|Error)[A-Za-z]*\(' --include='*.go' internal cmd | grep -v '_test\.go'`。工具执行、provider 调用、VCS 提交、压缩仍然一处都没有。`internal/tools::TestAuthorizeLogsDecisionWithoutArguments` / `TestAuthorizeLogsDenyDecision` 是**真证据**（换掉 `slog.Default()` 后驱动真实 `Authorize`，断言 `"decision":"allow"` / `"tool":"fs_read"` 出现）；`ws.go` 的两条 turn 日志**零测试**。
- 证据形状：驱动**生产入口**（不是直接调 logger），捕获 slog 输出，断言字段名+值。**骗过去**：在测试里自己 `logger.Info(...)` 再断言。

**2. secret 不入日志** — 已兑现
- 依据：`internal/observe/log/redact.go::sensitiveKeys/redactAttr`（key 表 + 值前缀嗅探 + group 递归 + `WithAttrs` 预绑定拦截）；`TestHandlerRedactsDirectBoundNestedAndErrorAttrs`（四类载体各注一个真 secret 并断言不出现）+ `TestRedactAttrBranches` / `TestWithGroupPreservesRedaction` + 生产路径的 `internal/tools::TestAuthorizeLogsDecisionWithoutArguments`（断言 `C:/secret`、`sk-test` 不入审计）+ `TestWarnErrNoopsOnNilAndLogsOnRealError`。
- 证据形状：注入**真实 secret** 后断言不出现（现状即是）。

**3. 级别可配** — 部分（**变异盲**）
- 依据：`config.go::LogConfig` → `bootstrap.go::Build` → `internal/observe/log/log.go::New`（`slog.HandlerOptions{Level: ParseLevel(cfg.Level)}`）。`TestParseLevelAllVariants` 只测纯函数映射；`config_test.go::TestLoadObservabilityFeaturesAndPricing` 只测 YAML 解析。**没有任何测试断言过滤行为** —— 把 `log.go` 改成硬编码 `slog.LevelInfo`，全部测试仍绿。
- 证据形状：`New(Config{Level:"warn"})` 后调 `logger.Info(...)` 断言 buffer **为空**，再调 `Warn` 断言非空。**骗过去**：只断言 `ParseLevel` 的返回值。

**4. 采样不丢关键错误** — 未兑现（**无实现**）
- 依据：全仓唯一的 "sample" 是 OTel 的 `SampleRatio`（与日志无关）；`config.example.yaml` 的 `observability.log` 块（键 `level`/`format`/`file`/`stderr_in_tui`）也没有采样项。
- 证据形状：需先有实现（如按 key 限流的 handler），再断言「高频 Info 被丢弃时，同批次里的 Error 记录 100% 保留」。**当前这条子句只能靠补实现或改写 acceptance 结项，不能靠补测试。**

---

### C4/OBS2 — OTel 遥测 · 台账 `partial`

> acceptance：trace 链可导出；latency/token/retry/error 可观测；可关闭；脱敏

**1. trace 链可导出** — 部分
- 依据：`internal/observe/otel/otel.go::Setup/setupWithFactories`（Setup / OTLP HTTP exporter / ParentBased 采样 / TraceContext 传播）；生产 span 只有 `agent.turn`（`orchestrator.go::Orchestrator.Query`）与 `tool.<name>`（`tools/guard.go::GuardedTool.Stream`）。`TestSetupWithLiveCollectorFlushesCleanly` 走了真 OTLP 导出路径，但 **stub handler 直接丢弃请求体**，断言只到 `Shutdown` 返回 nil —— **导出器若静默丢弃全部 span，此测试仍绿**。`otelobs.StartSession`/`SetSessionID` 有测试但**零生产调用点**；所谓「链」实际只有 turn→tool 两层，且**没有任何测试断言父子关系**。
- 证据形状：stub 记录收到的 request body 并断言其中含期望的 span name；另加一条对 `agent.turn` → `tool.x` 的 `Parent().SpanID()` 断言。**骗过去**：只断言 `Shutdown() == nil`。

**2. latency/token/retry/error 可观测** — 未兑现（偏部分；**接线缺口**）
- 依据：`internal/observe/otel/instrument.go::ensureInstruments` 定义 6 个 instrument。生产调用点只有 `StartTool`、`StartTurn`、`RecordRetry`。**`RecordUsage`（token 计数器）零生产调用点**；`StartSession`（session latency）同样。`TestRecordUsageClampsNegativesAndSkipsZeros` 与 `TestRecordRetryEmitsSafeAttributes` 是**恒真空壳** —— 注释自认「we assert only that emission does not panic」，函数体里没有一句断言。全仓**没有 `sdkmetric.NewManualReader` / `metricdata`**，即所有 metric 的值和属性从未被断言过。
- 证据形状：装 `ManualReader`，`Collect` 后断言 `yanshi.llm.tokens` 存在且 `token.kind=cache_hit` 的 datapoint 值等于输入；`yanshi.llm.retry` 计数等于重试次数。**骗过去**：调一遍函数不 panic 就完事（现状）。⚠️ 前提是**先给 `RecordUsage` 接上生产调用点**，否则这是接线缺口而非测试缺口。

**3. 可关闭** — 已兑现
- 依据：`otel.go::Setup`（`!cfg.Enabled` → `installNoop`）、`::Setup`（collector 不可达 → noop）、`::setupWithFactories`（exporter 失败 → noop）。四条测试（`TestSetupDisabledReturnsNoop`、`TestSetupCollectorUnreachableDegradesToNoop`、`TestSetupTraceExporterFailureSoftDegrades`、`TestSetupMetricExporterFailureShutsTraceDown`，含 `te.shutdowns == 1` 清理断言）都对 `rt.Enabled()` 做了否定断言。
- 证据形状：`Enabled()` 为 false + Shutdown 无错（现状即是）。

**4. 脱敏** — 已兑现
- 依据：`instrument.go::startOperation`（只写 `SafeErrorType`，不写 body，不用 `RecordError`）；`TestToolSpanNeverRecordsErrorBody`（注入 `sk-super-secret` 并断言 attributes 无泄漏 + **`len(span.Events()) == 0`**，后者精确堵住 `RecordError` 回归）+ `TestStartSessionErrorEndSetsStatus`。
- 证据形状：注入真 secret + 断言 `Events()` 为空（现状即是）。

---

### C4/OBS3 — feature flags · 台账 `partial`

> acceptance：flag 注册/切换；strict mode 报错未知 flag；新功能可灰度

**1. flag 注册/切换** — 已兑现
- 依据：`internal/features/features.go::Registry.Register/Registry.Set`；`TestRegistryDefaultsRuntimeSetAndList`（默认关→Set→开，两个状态都断言）、`TestRegisterPanicsOnDuplicate`、`TestRegisterPanicsOnMissingRequiredFields`、`TestNilRegistryAccessors`。
- 证据形状：切换前后两个方向都断言（现状即是）。

**2. strict mode 报错未知 flag** — 已兑现
- 依据：`features.go::Registry.ApplyMap`（两遍扫描保证原子性）；`TestRegistryStrictRejectsUnknownByNameAtomically`（同时钉住「具名」与「原子」）、`TestRegistryNonStrictIgnoresUnknown`、`TestRegistrySetAlwaysRejectsUnknown`，端到端 `internal/bootstrap::TestBuildStrictFeaturesNamesUnknownFlag`（断言 err 含 `typo_observe_flag`）。
- 证据形状：错误信息含出错的 key 名 + 已知 key 未被半提交（现状即是）。

**3. 新功能可灰度** — 未兑现（**比台账注释判得更重**）
- 依据：3 个 flag（`features.go::DefaultSpecs`）里**只有 1 个有真实消费点**（`bootstrap.go::Build` 的 OTel 门）—— `observe.slog_trace_id` 与 `observe.cost_in_status` 注册后**从未被读取**（**幻影 flag**）。更关键：唯一那个门是 **boot-time 一次性求值**，`internal/api/http/ws.go::Server.ChatWS` 的 `/features` 只做 list/set，从不回读去改变行为 → **运行时切任何 flag 对系统零影响**，这与 `bootstrap.go` 的注释（「让操作员通过 /features 在运行时关掉导出」）**直接矛盾**。`TestBuildSetsUpOTelAndShutsDown` 是**变异盲** —— 只断言 `app.OTel != nil` 与 Shutdown 无错，而 `otelobs.Setup` 在关闭时返回的也是非 nil no-op Runtime，所以把 `&& featureReg.Enabled("observe.otel_export")` 整段删掉、甚至把 `Enabled` 硬编码为 false，它都仍绿。
- 证据形状：同一份 config 起两次 `bootstrap.Build`，仅 `features.overrides.observe.otel_export` 取 true/false，断言 `app.OTel.Enabled()` 分别为 true/false（**必须断到 `Enabled()`，不能只断 non-nil**）。**骗过去**：只跑 flag=true 一侧、或只断 `app.OTel != nil`。

---

### F2/BENCH1 — 性能基准基线 · 台账 `partial`

> acceptance：关键路径有基准；CI 记录趋势；大回归可发现

**1. 关键路径有基准** — 部分
- 依据：4 个基准存在（`internal/vcs/vcs_bench_test.go` `BenchmarkVCSCommit` 与 `BenchmarkDAGApply`、`internal/tools/fs_bench_test.go` `BenchmarkFSEdit`、`internal/agent/orchestrator/orchestrator_bench_test.go` `BenchmarkOrchestratorTurn`）。**三个可跑，一个必挂** —— 已本地复现：`go test -bench=BenchmarkOrchestratorTurn -benchtime=3x ./internal/agent/orchestrator` → `FAIL: orchestrator: no assistant message produced`。根因是 `orchestrator_bench_test.go::BenchmarkOrchestratorTurn` 的 FakeModel 只脚本了 1 条响应且未设 `Repeat`，`b.N ≥ 2` 必失败；只有 `-benchtime=1x` 能过，而 CI 默认 benchtime 与 `scripts/bench.sh` 的 `-count=5` 都会踩中。另 `fs_bench_test.go::BenchmarkFSEdit` 的 restore write 在计时区内（与 `::BenchmarkFSEdit` 注释相反，且与 vcs 的正确做法不一致），恒定加项会**稀释**真实回归幅度。
- 证据形状：断言 `go test -list 'Benchmark.*'` 的清单非空**且每个基准在 `b.N>1` 下能跑完**。**骗过去**：只 `-list` 数名字。

**2. CI 记录趋势** — 未兑现（**N 类**）
- 依据：`.github/workflows/nightly.yml:35-60`。仓库内**没有任何 baseline 文件**；CI **从不调用 benchstat**，也从不下载上一次的 artifact 做比较 —— 只 `tee` 一份再 upload。唯一懂比较的 `scripts/bench.sh:27-40` 从没有被任何 workflow 调用（且该文件 mode 是 `100644`，按自身 usage 里写的 `./scripts/bench.sh` 调用会 permission denied）。附带两处**吞错**：`nightly.yml:48` 有 `set -e` 但**没有 `set -o pipefail`**，`:53` 的 `go test … | tee` 让 tee 的 0 覆盖 go test 的 rc=1 —— orchestrator 基准的失败在 `continue-on-error: true`（`:38`，注释「soft until F2 bench targets land」已过期）生效之前就已被吞掉一次；`:53` 用 `./...` 还会把整个单测套件混进 `bench-results.txt`。
- 证据形状：需要「上一次的数 + 这一次的数 + 差值落盘/上报」这条链**本身存在**。单次测量不构成断言。

**3. 大回归可发现** — 未兑现（**N 类**）
- 依据：无门禁。`scripts/bench.sh:19` 的 `THRESHOLD_PCT=20` 自带注释 "informational"，全仓无任何地方执行它。设计文档 `docs/superpowers/specs/2026-07-22-f2-resource-benchmarks-design.md:135,144` 明确要求 benchstat 比对与 >N% 告警，**两者都未实现**。全仓零 `testing.Benchmark(`、零 `b.ReportMetric`、零 `testing.AllocsPerRun`。附带：本地 baseline 还会被 `internal/tools/permctx.go` 的 `auditPermission` 打到 stderr 的日志污染（`bench.sh:25` 有 `2>&1`），使 benchstat 静默丢掉整行 FSEdit。
- 证据形状：需要门禁 —— CI 里 benchstat 比对超阈值即失败/告警，或改写为可断言形态（对 `AllocsPerRun` / 时长设上限的普通测试）。

---

### M1/O07 — yanshi doctor 自检子命令 · 台账 `partial`

> acceptance：检查 config/DB/provider/ACP CLI/端口/lockfile/目录/sandbox；人类可读 + --json；退出码 0/1/2；不打印 secret；不完整环境不 panic

**1. 检查 config/DB/provider/ACP CLI/端口/lockfile/目录/sandbox** — 部分
- 依据：`doctor.go::RunDoctor` 八项齐备，但 **sandbox 是诚实占位**：`doctor.go::checkSandbox` 恒返回 `StatusWarn` + "sandbox verification not implemented yet (arrives with S08 in M2)"，不做任何检查。ACP 检查（`::checkACP`）只 `LookPath(npx)`，不验 npm 包是否装上（doc 注释已自陈）。其余七项有真断言（含 `TestCheckLockfile_Stale` 用 PID 2147483647 造真 stale、`TestCheckPort_FreeAndInUse` 真占端口、`TestCheckDatabase_OpenError` 用目录路径钉住 fail）。sandbox 那项由 `TestCheckSandboxStillHonestAboutGap` **钉住占位状态** —— 它证明的是「诚实」，不是「检查」。
- 证据形状：sandbox 需要真实探测 + 断言；在 S08 落地前，这条只能靠**改写 acceptance**（把 sandbox 排除）结项。

**2. 人类可读 + --json** — 已兑现
- 依据：`doctor.go` / `::DoctorReport.RenderJSON`、`main.go::runDoctor`；`TestRenderText_Format` + `cmd/yanshi::TestRunDoctor_JSON` + `TestRunDoctor_TextNotJSONByDefault`（双向断言：有 `[WARN]`、无 `"checks"`）。
- 证据形状：两种模式互斥断言（现状即是）。

**3. 退出码 0/1/2** — 部分
- 依据：`doctor.go`（ExitCode）、`main.go::runDoctor`。`TestExitCodeMapping` 是**对字面量断言的空壳** —— 它构造合成 `DoctorReport{Checks: []CheckResult{{Status: StatusWarn}}}`，等于把映射表抄了一遍。真实集成只钉了 exit 1。**关键问题：`0` 在真实运行中不可达** —— `RunDoctor` 恒包含 `checkSandbox()`，后者恒 warn，所以任何真实调用最少返回 1（`TestRunDoctor_JSON` 的注释自己也承认「never 0」）。exit 2 **没有任何集成测试**。
- 证据形状：集成级别 —— 给一个必然 fail 的 config（如 `sqlite_path` 指向目录）驱动 `runDoctor` 断言返回 2；以及一条能返回 0 的路径（需先解决 sandbox 恒 warn）。**骗过去**：只对合成 report 调 `ExitCode()`。

**4. 不打印 secret** — 部分
- 依据：`doctor.go::CheckResult` / `::checkConfig` / `::checkProviders` / `::checkSecretsRefs` 契约齐。`cmd/yanshi::TestRunDoctor_JSON` 的整报告断言 `assert.NotContains(c.Message, "sk-test-not-a-real-key")` 是**恒真断言** —— 该字符串根本不在测试 config 里（config 用的是 `env://OPENAI_API_KEY_DOCTOR_JSON_TEST`）。真证据是两条函数级测试：`TestCheckProviders_MissingKeyIsFailAndRedacted`（注入真 `sk-secret-value-xyz`）与 `TestCheckSecretsRefs_InvalidCredentialIsRedacted`。缺口：**没有任何测试覆盖整报告**（18 项检查的 Message 联合），也没覆盖 `checkConfig` 在 config.Load 因 raw 字面量失败时不回显 cfgErr 这条最危险的路径。
- 证据形状：一份**真的含 raw secret** 的 config 走完整 `RunDoctor`，遍历**全部** `Checks` 断言该 secret 不出现在任何 Message 里。**骗过去**：断言一个输入里压根没有的字符串（现状）。

**5. 不完整环境不 panic** — 已兑现
- 依据：`doctor.go`（每项检查独立）+ `::skipped`（`skipped()` 降级为 warn）；`TestRunDoctor_ConfigLoadFailsDowngradesDeps`（config 缺失 → config fail、依赖项全 warn）+ 多条以 `ConfigPath: ""` 直接跑完整 `RunDoctor` 的测试 + 各 check 的 `_Skipped` 单测。
- 证据形状：无 config / 无 provider / 无 ACP CLI 的裸环境跑完整 RunDoctor 不 panic 且返回完整报告（现状即是）。

---

## W8 — TUI / 技能 / 本地化

> **本包最重要的跨条目发现：一个接线断点砸掉五条子句。** `newModel`（`internal/cli/tui/model.go`）把 theme / bundle / keymap **全部写死为字面量**（`theme: ThemeDefault`、`i18n.NewBundle("en")`、无 keymap），而 `internal/cli/tui/preferences.go` 的四级合并（`mergeTUIPrefs` / `PreferencesFromEnv` / `loadPreferences` / `persistPreferences`）**四个函数生产调用者全为 0**。D3/C15#1·#2·#3 与 D3/I18N1#1·#3 五条子句的缺口都在这一处 —— **修一处可同时改变五条子句的状态**。

### C2/UX1 — 全局命令面板 Ctrl+K · 台账 `partial`

> acceptance：Ctrl+K 打开全局面板;fuzzy 过滤;覆盖命令/模式/模型/会话;Esc 关闭

**1. Ctrl+K 打开全局面板** — 部分
- 依据：实现齐（`internal/cli/tui/handlers.go::model.handleKeyMsg` → `openActionPopup`；状态机 `action.go`；渲染 `view.go::model.renderScreen`）。`TestHandleKeyMsg_CtrlKCtrlSCtrlO`（真 `Update(KeyMsg{KeyCtrlK})`，断言 `m.action != nil` / 二次按下为 nil）与 `TestAction_ModelsReplyPopulatesCacheAndRefreshesPopup` 是真的。但**渲染接缝无锁** —— `TestCov_RenderScreen_Popups` 设了 `m.action = &actionState{visible:true}` 却**只断言 `"help-overlay"`**。**变异实验（实跑）**：把 `view.go::model.renderScreen` 的 `blocks = append(blocks, ap)` 整段删掉 → `go test ./internal/cli/tui` **全绿**。即「面板打开」只锁到了 model 字段，没锁到「用户看得见」。
- 证据形状：断言 `m.Update(KeyMsg{KeyCtrlK})` 后 **`m.View()`**（或 `renderScreen()`）含 `"Actions"` 标题行与至少一条候选行。**骗过去**：任何只断言 `m.action != nil`、或直接调 `m.actionPopup()` 而不过 `View()` 的测试（现有全部属此类）。

**2. fuzzy 过滤** — 部分
- 依据：`action.go` `rankedActions` + `fuzzy.go` `fuzzyScore`（子串优先 → `scatteredScore` 跳跃匹配）。`TestRankedActions_QueryFilters`（query `"zzzzzzzzz"` 断言 Empty）与 `TestAction_FuzzyRank` 有效，但**fuzzy 的定义性半边是变异盲**。**变异实验（实跑）**：把 `fuzzy.go::fuzzyScore` 的 `return scatteredScore(q, t)` 改成 `return 0`（退化为纯子串过滤，不再是 fuzzy）→ **全绿**。`TestFuzzyScore_ContiguousBeatsScattered` 抓不到（scattered 变成 0 仍 `< contiguous`）；`TestFuzzyScore_NonSubstringIsZero` 更是与变异同向。
- 证据形状：断言 query `"mdl"`（**非子串、按序跳跃**）能命中 `/model`，且 query `"ldm"`（乱序）命中不到。**骗过去**：所有 query 都写成目标的连续子串（现状）。

**3. 覆盖命令/模式/模型/会话** — 部分（**会话缺失，且是刻意 defer**）
- 依据：`action.go` `collectActions` 只有四个 source：command / mode / model / **theme**；`action.go` 注释自认「session + tool/MCP source DEFERRED」。会话选择器是独立的 `sessionPickerPopup`（`view.go::model.renderScreen`），由 `/restore` 驱动，**Ctrl+K 到不了**。命令源里有 `/sessions`、`/restore`、`/fork` 等会话*命令*，但不能从面板里选具体会话实例。`TestCollectActions_AllSources` 只断言 `{command, mode, model, theme}` 四个 source 存在 —— **验收里的第四项是「会话」，测试里的第四项是「主题」**，两者不是同一件事。
- 证据形状：断言 `collectActions()` 含 `Source=="session"` 且 Label 为某个真实 session ID，选中后发出 `NewRestoreSession(id)`。**骗过去**：用「四个 source 都在」暗示「验收的四类都覆盖」（现状）。

**4. Esc 关闭** — 已兑现
- 依据：`handlers.go::model.handleKeyMsg`（modal 分支优先吃 Esc）；`TestHandleKeyMsg_ActionPaletteKeys` 真 `Update(KeyMsg{KeyEscape})` 后断言 `m.action == nil`，同测试还覆盖 ↑↓/Runes。
- 证据形状：现状已足够；再加一条「Esc 不把 Esc 泄漏进 textarea」会更严。

---

### C2/UX2 — F1 可搜索帮助 · 台账 `partial`

> acceptance：F1 打开;可搜索;内容自动生成不漂移

**1. F1 打开** — 已兑现
- 依据：`handlers.go::model.handleKeyMsg`（`case tea.KeyF1` toggle）+ 渲染 `view.go::model.renderScreen`；`TestCov_F1Toggle`、`TestHandleKeyMsg_HelpPanel`（真按键 + Esc）、`TestCov_RenderScreen_Popups`。**变异实验（实跑）**：删掉 `view.go::model.renderScreen` 的 help 渲染分支 → `TestCov_RenderScreen_Popups` **失败**。渲染接缝有锁（与 UX1#1 相反）。
- 证据形状：按键 → `View()` 含 overlay 标记（现状即是）。

**2. 可搜索** — 未兑现（**有实现，无任何测试驱动**）
- 依据：`help.go` `rankedHelpEntries` 按 `fuzzyScore(query, Label+" "+Hint)` 过滤排序；按键侧 `handlers.go::model.handleKeyMsg`。但 `TestHelp_FuzzyFilter` 只断言 query `"mode"` 下 `/mode` **仍在**结果里；`TestHelp_PopupVisibilityAndQuery` 只断言 popup 含 `"search: mode"` 和 `"/mode"` —— **两者在「完全不过滤」时同样成立**。**变异实验（实跑）**：把 `help.go::model.rankedHelpEntries` 改成 `if true { return all }`（帮助搜索彻底不过滤）→ **全绿**。
- 证据形状：断言 `helpQuery="theme"` 后 `rankedHelpEntries()` 中**不含** `/clear`，且 `helpPopup()` 输出**不含** `"Ctrl+V"`；再断结果条数 < `collectHelpEntries()` 条数。**骗过去**：只断言「目标项还在」（现状两条）。

**3. 内容自动生成不漂移** — 未兑现（**现在就是漂的**）
- 依据：`help.go::model.collectHelpEntries` 的 commands/modes/themes 三段确为动态派生，**但键位段是三张互相独立的手工静态表**：
  - `help.go` `keyBindings`（F1 面板用，目前是准的），其上 `::keyBindings` 注释自认「Go 无法反射 keybinding，新增分支时同步更新此表」；
  - `commands.go::newCmdHelpEntry`（`/help` 命令的 "Keyboard shortcuts" 段）**已经错了**：`Ctrl+K → "clear input"`（真实是打开 action palette）、`Ctrl+S → "toggle spinner/sound"`（真实是存草稿）、`Ctrl+E → "toggle history view"`（`tea.KeyCtrlE` 在整个 TUI **不存在**）；并漏掉 Ctrl+V / F1 / Alt+R。
  - ~~台账外真实缺陷：`commands.go::commandTable` 里有**两条逐字相同的 `/features` 条目**~~ — ✅ **已修（`cf088f7`）**，该提交删掉了重复的那一行（`internal/cli/tui/commands.go`，1 行删除）。2026-08-04 复测：`awk '/^var commandTable/,/^}/' internal/cli/tui/commands.go | grep -oE 'name: "[a-z-]+"' | sort | uniq -d` **零输出**。⚠️ **但下面那道防重复门禁仍然欠着**：`cf088f7` 只删了行、没加断言，同一个重复随时可以再回来，`commandTable` 内 `name` 无重复至今无测试拦截。
  `TestHelp_KeybindingsCoreEntries` 是**恒真空壳**：它断言 `renderHelp()` 含 `"Enter"/"Ctrl+K"/"F1"`，而这些字符串正来自同文件的 `keyBindings` 字面量表；它对 `handlers.go` 的真实 `case tea.Key*` 分支一无所知（测试自己的 doc 注释也承认这点）。
- 证据形状：一条**防漂移门禁** —— 用 `go/ast` 解析 `handlers.go` 里的 `case tea.KeyCtrl*/KeyF*` 集合，断言它与 `keyBindings` 和 `/help` 的 shortcuts 表**双向**一一对应（多一条、少一条都红）；外加断言 `commandTable` 内 `name` 无重复。**骗过去**：拿静态表里的字符串去断言同一张静态表渲染出的字符串（现状）。

---

### C2/UX3 — @path 文件附加上下文 · 台账 `missing` ✅ **复核：维持 missing**

> acceptance：`@` 触发补全;附加有界;越权拒绝;超大提示 fs_read

**1. `@` 触发补全** — 未兑现
- 依据：**无实现**。`internal/cli/tui` 全包非测试代码里 `@` 字符只出现**一次**，且在注释里：`grep -rn '@' internal/cli/tui/ --include='*.go' | grep -v '_test.go'` 恰好输出一行 —— `images.go::buildSendFrame` doc 注释中的 "@path detection"，**零处可执行代码**读写这个字符。（本条上一版写「零出现」，与紧随其后的「唯一提及」自相矛盾；实质结论不变。）弹层只有 `pickerKind ∈ {"", model, mode, theme}` + Ctrl+K + F1 + Alt+R + sessionPicker，**无任何文件路径补全器**。`internal/tools/pathref.go` 确为**同名不同物**：服务端、只处理图片的展开器（`pathref.go::ResolveImagePathRefs` `if !IsImagePath(ref) { continue }`），挂在 `orchestrator.go::Orchestrator.EventsWithHistoryOpts`，是「用户已经打完字之后」的解析，不是补全。
- 证据形状：断言 textarea 值为 `"看 @sh"` 时 model 进入文件补全态且候选含 `shots/a.png`，↑↓ 可移动、Enter 写回 textarea。

**2. 附加有界** — 部分
- 依据：`pathref.go`（`maxPathRefImages = 8`）+ `pathref.go::resolvePathRefImage` 复用 `maxImageAttachBytes`（10 MiB）。界是真的，但只对**图片**成立 —— 非图片 `@ref` 根本不进管道。**两条界均无测试**：`maxPathRefImages` 零测试引用，pathref 的 size 分支零测试引用。`internal/tools::TestImageAttach_RejectsOversizeImage` 的第三个子测试注释声称「@path entry 用的是同一个常量」，但断言只有 `assert.Equal(maxImageAttachBytes, 10<<20)` —— **字面量自比，完全没触碰 pathref.go**（恒真空壳）。
- 证据形状：给一条含 9 个合法 `@x.png` 的消息，断言 `res.Images` 长度恰为 8 且 `res.Rejected` 含 "too many"；再给一个 10 MiB+1 的 png，断言未附加且 Rejected 非空。**骗过去**：断言常量等于自身（现状）。

**3. 越权拒绝** — 部分（拒绝真实，但**诊断被丢弃**）
- 依据：两道门都真 —— `pathref.go::resolvePathRefImage`（`withinRootAbs` 解 symlink 的 root-jail）、`pathref.go::resolvePathRefImage`（以 `fs_read` 名义 `Authorize`）。测试也真且强：`TestResolveImagePathRefs_RejectsEscapingRef`（断言 `Rejected[0].Reason` 含 "root"）、`TestImageDescribePathRefDeniedByGuard`、`internal/agent/orchestrator::TestE2E_PathRefTurnWiring`（子测试 "escaping ref reaches the model as text only" 断言 FakeModel 收到 `fake-vision(0 images)`，是真正的端到端拒绝证据）。**缺陷**：`internal/agent/orchestrator/multimodal.go::Orchestrator.expandPathRefs` 取到 `res` 后**只用 `res.Images` 和 `res.Text`，`res.Rejected` 整个被丢掉**，且 `if len(res.Images) == 0 { return }` 提前返回时也丢 → 越权确实被拦，但用户/模型**永远收不到「为什么没附上」**。
- 证据形状：补一条断言「拒绝理由抵达用户」—— 越权 `@ref` 后，turn 的可观察输出（frame 或 message）含该 ref 与理由。**骗过去**：只在 `tools` 包内断言 `res.Rejected`，而 orchestrator 把它扔了（现状）。

**4. 超大提示 fs_read** — 未兑现
- 依据：**无实现**。`pathref.go::resolvePathRefImage` 的理由串是 `"image exceeds the per-image size limit"`，不含 `fs_read`；隔壁 `imageattach.go` 的 `oversizeReason` 提示的是 `"use image_describe instead"`，且 pathref 并不调用它。`pathRefGuardTool = "fs_read"`（`pathref.go`）是**鉴权用的工具名**，不是给用户看的提示。叠加 #3 的缺陷：即便有这句提示，也会在 `multimodal.go::Orchestrator.expandPathRefs` 被丢掉。
- 证据形状：断言超大 `@big.png` 产生的用户可见文本含 `"fs_read"`，且该文本出现在 **turn 输出**里而非只存在于内存结构体。

---

### C2/UX4 — 文件 frecency · 台账 `partial`

> acceptance：近期选择靠前;衰减合理;可禁用

**1. 近期选择靠前** — 未兑现（**存储真实，排序无消费者**）
- 依据：`internal/cli/tui/frecency.go::Frecency.TopN` `TopN` 存在且正确，**但 `TopN` 的调用者只有 `frecency_test.go` / `frecency_cov_test.go`，零生产调用点**。写入侧倒是真接线的：`model.go` `recordToolFrecency` ← `applyEvent` 的 tool_result 分支，经 saveQueue 落盘。`frecency.go` 的注释「提供给后续 UX 批次排序使用」属实。**记录信号也与验收错位**：记的是**成功的 fs 写工具路径**，不是「文件补全的近期选择」（UX3 不存在，UX1 无文件源）。`TestFrecency_RecordAndTopN` / `TestModel_FirstFrecencyRecordEnqueuesSave` 是真断言，但证明的是「排序函数正确」与「写入已接线」，不是「近期选择靠前」这个用户可见行为。
- 证据形状：需要先有消费点。断言应形如：连续 `Record("b.go")` 后打开某个文件候选列表，`b.go` 出现在第一位，且该列表由**生产按键路径**产生。**骗过去**：对 `TopN` 直接断言顺序（现状全部如此）。

**2. 衰减合理** — 已兑现（单元层）
- 依据：`frecency.go::frecencyEntry.score`（`<1h→1.0`、`<1d→0.9`、`<7d→0.5`、`≥7d→0.1`）；`TestCov_Score_DecayBuckets`（count=10 在 2h/48h 下得 9.0/5.0）、`TestFrecency_DecayOldEntries`。**变异实验（实跑）**：把衰减下限 `0.1` 改成 `0.11` → `TestFrecency_DecayOldEntries` **失败**，非变异盲。
- ⚠️ 脆弱性：`TestFrecency_DecayOldEntries` 的两侧分数正好相等（10×0.1 = 1×1.0 = 1.0），它实际靠 `TopN` 的 lastSeen tiebreaker 通过，刚好卡在边界上。
- 证据形状：把用例挪离边界（如 old count=5），使断言靠**衰减本身**成立而非靠 tiebreaker。

**3. 可禁用** — 未兑现
- 依据：**无实现**。`config.example.yaml`、`internal/config`、`internal/features` 里都没有 frecency 开关；`LoadFrecency` 在 `model.go::newModel` 无条件执行。
- 证据形状：配置 `frecency.enabled: false` 后 `m.frecency == nil`（或 `recordToolFrecency` 不入队），且磁盘文件不被创建。

---

### C2/UX8 — 思考流式展示 · 台账 `done` ✅ **复核：done 站得住**

> acceptance：思考模型可见流式思考;正文与思考分离;非思考模型无影响;可折叠

生产链路逐段核过：`orchestrator/classify` 发 `thinking` 帧 → `internal/cli/wsbackend.go::toStreamEvent` `Kind: f.Type` 泛化透传 → `applyEvent` 的 `"thinking"` 分支。

**1. 思考模型可见流式思考** — 已兑现
- 依据：`TestClassifyEvents_EmitsThinkingForReasoning`（断言 thinking 帧在 agent_chunk **之前**）+ `TestModel_ThinkingLiveThenCollapses`（断言 `te.render(80, spinner)` 输出含 `"Thinking…"` 与流式正文）。**两端都挂**，是本批唯一把「渲染可见」真正断进去的条目。
- 证据形状：生产端发帧 + 渲染端可见，两端都要。**骗过去**：只测其中一端 —— 会重演本仓反复出现的「写完也测过，但没人调它」。

**2. 正文与思考分离** — 已兑现
- 依据：`TestClassifyEvents_ReasoningOnlyEmitsOnlyThinking`（线路层：两种 frame type）+ `TestThinkingParity_NonThinkingFinalizesLive`（TUI 层：detach 成 `assistantEntry.thought` 独立字段）。

**3. 非思考模型无影响** — 已兑现（证据略欠一端）
- 依据：`TestThinkingParity_NoThinkingNoEntry`（真断言不创建 `*thinkingEntry` 且 `thought == nil`）。
- ℹ️ **低报方向的小遗漏**：生产端的对称证据 `internal/agent/orchestrator::TestClassifyEvents_NoReasoningEmitsNoThinking` **存在且有效，但没有被台账引用**（因此也没有 ledger marker）。补上会让 #3 与 #1/#2 一样两端齐全。

**4. 可折叠** — 已兑现
- 依据：`TestModel_ThinkingCtrlOTogglesExpand`（真 `Update(KeyMsg{KeyCtrlO})`，断言展开后 `stripANSI(render)` 含全文、折叠提示消失、二次按下复原）+ `TestThinkingParity_LiveBlockNotExpandable`（负向边界）。

---

### C3/E03 — skill 从 GitHub 安装 + 管理 · 台账 `partial` ⚠️ **低报**

> acceptance：可安装/列出/启停/校验；恶意路径安全；重名可诊断；模型可 load 匹配技能

**1. 可安装/列出/启停/校验** — 部分
- 依据：安装 `internal/skills/install.go::Install`（真 `git clone --depth 1`，`::realClone.Clone`）；列出 `internal/api/http/ws_handlers.go::handleListSkills`；启/停 `internal/skills/skills.go::Registry.Enable`/`::Registry.Disable`，WS 调用点 `ws_handlers.go::handleSkillMutation`；TUI 入口 `internal/cli/tui/commands_skills.go::cmdSkill`。无独立 validate 子命令 —— 校验内嵌在 `Load`（`skills.go::Loader.Load`）与 `Install`（`install.go`）里。`internal/api/http::TestChatWS_Skills_AllRootsInstallDisableUninstall` 是强证据（真 WS 帧往返，install→list→disable→uninstall 全程断言 `ack.Action` 与 `ack.Skill.Enabled`）。**两个覆盖空洞**：① **enable 的 happy path 在生产链路上零覆盖** —— WS 层只有错误分支，把已 disable 的技能 enable 回来只在 `internal/skills::TestRegistryEnableRemovesDisabledMarker`（纯 unit，不过 `handleSkillMutation`）测过；② **「从 GitHub」这一维零测试** —— 所有测试走 `skills.CloneStub`（`install.go`，本地 `os.CopyFS`），唯一碰 `realClone` 的 `TestRealCloneError` 故意先 `cancel()` context 只打错误分支。
- 证据形状：WS 集成里 disable 后再 `NewEnableSkill(name)`，断言 `ack.Action=="enabled" && ack.Skill.Enabled==true`。**骗过去**：只断言 `Enable("不存在")` 报错就当「启停已验」。

**2. 恶意路径安全** — 已兑现
- 依据：`install.go::ParseInstallSource`（source 段拒 `.`/`..`/元字符）、`::Install`+`::rejectSymlinks`（symlink 全量拒绝）、`::Install`（dst 越界）、`skills.go`（ReadFile 穿越/绝对路径拒绝）。测试 `TestParseInstallSource_RejectsTraversal`、`TestInstall_RejectsSymlink`、`TestRegistry_ReadFile_RejectsTraversal` 系列均真断言错误串。最强的是 `internal/tools::TestInstalledSkill_FullLifecycleNeverExecutesScripts` —— 用**真恶意 fixture**（`internal/skills/fixtures/evil-scripts/scripts/evil.sh` 写 sentinel），走完 Install→Load→skill_use→Trust→Disable→Reload→Enable→ReadFile 后逐步断言 sentinel 不存在。
- ⚠️ 一处**标签不实**：`TestInstallSubdirEscape` 注释自称覆盖 `install.go` 的 `isWithin` 分支，但输入 `github:owner/repo/../escape` 在 `ParseInstallSource`就被拒了，**从未到达那行**。该 `isWithin` 在公开 API 下不可达，属未被驱动的防御代码 —— 不是造假，但「子目录逃逸」这条具体防线没有专属证据。
- 证据形状：直接单测 `isWithin`，或构造能过 `ParseInstallSource` 但 Join 后逃逸的输入。**骗过去**：只断言「有 err」而不检查 err 来源（现状）。

**3. 重名可诊断** — 部分（诊断分支存在但**零测试**）
- 依据：Load 时重名是**静默 first-seen-wins**（`skills.go::Loader.Load`，`internal/skills` 全包 `log.` 出现 0 次，无任何告警）。唯一诊断在 `ws_handlers.go::handleInstallSkill`：新装 user 副本被同名 builtin/plugin 遮蔽时返回 `"installed user copy %q but active entry is from %s; restart will not change root precedence"`，其上注释自认「Full conflict diagnostics … are SC1 scope cuts」。**该诊断文案零覆盖** —— WS 测试装的是全新名字，走的是不遮蔽分支（`assert.Empty(ack.Text)`）。`TestLoad_FirstSeenWins` 与 `TestLoad_BuiltinShadowsUser` 实质等价，都只断言 `s.Description == "from-builtin"`（**谁赢**），不断言任何「用户能发现重名」的信号。
- 证据形状：装一个与 builtin 撞名的 user skill，断言 `ack.Text` 非空且含来源（`"active entry is from builtin"`）。**骗过去**：只断言胜者是谁（现状）。

**4. 模型可 load 匹配技能** — 已兑现
- 依据：两条独立生产链路都真接线 —— ① `skill_use` 工具（`internal/tools/skill.go::NewSkillUseTool`）注册于 `bootstrap.go::Build`、profile 允许（`profile.go::DefaultOrchestratorProfile`）、技能清单注入 system prompt（`bootstrap.go::Build` → `orchestrator.go::New`）；② `/skill <name>` 前缀注入（`internal/api/http/skillprefix.go::resolveQuery`，调用点 `chat.go::Server.handleSSEInternal` 与 `ws.go::Server.ChatWS`）。测试：`internal/agent/orchestrator::TestE2E_SkillUseReturnsBodyThroughOrchestrator`（scripted FakeModel 真发 tool call，跑真 ADK Runner + 真 GuardedTool + 真 Registry，断言磁盘 SKILL.md 里的唯一 marker `UNIQUE_SKILL_MARKER_42` 穿透全链路）+ `internal/api/http::TestChat_SkillPrefix_Known`。均非 spy。
- 证据形状：唯一 marker 穿透全链路（现状即是）—— 这是本仓端到端断言的范本。

---

### D3/C15 — keymap 配置 · 台账 `partial`

> acceptance：核心按键可重映射；Vim 开关；高对比主题；冲突可诊断

**1. 核心按键可重映射** — 未兑现（**虚报：连第 1 句都没有**）
- 依据：核心存在但**不在生效路径** —— `internal/keymap/keymap.go::NewDefaultBuilder`（默认表 + overrides）、`::Map.Lookup`（`Lookup`）、配置项 `internal/config/config.go::TUIConfig`（`Bindings`）。**但 `internal/cli/tui` 不导入 `internal/keymap`**（全仓唯一 importer 是 `internal/cli/doctor.go`）；真实分发是 `handlers.go::model.handleKeyMsg` 的硬编码 `switch msg.Type`。`cfg.TUI.Bindings` **运行时永不被读**。`internal/keymap::TestBuild_DefaultLookupUsesRealKeyMessages` 是**变异盲**：它断言 `Lookup(KeyCtrlK)==ActionScrollUp`，而 TUI 里 Ctrl+K 实为打开 action palette —— **两者语义直接矛盾，测试照绿**。`internal/cli::TestCheckKeymapConfig_OKAndSkipped` 只是 spy（证明 doctor 会把 bindings 喂给 Builder）。
- 证据形状：`Bindings{"ctrl+g":"scroll_up"}` 经**生产构造**建 model，`Update(KeyMsg{KeyCtrlG})` 后断言 `viewport.YOffset` 变化，同时断言默认键不再触发旧行为。**骗过去**：任何只在 `internal/keymap` 内 `Build().Lookup()` 的测试。

**2. Vim 开关** — 未兑现（**虚报：连第 1 句都没有**）
- 依据：状态机 `internal/keymap/vim.go::VimMachine`/`::VimMachine.HandleKey` 存在但**零生产消费者**；`vim.go::effectiveVimMode` 的注释自称「production TUI calls this through (EffectivePreferences).Vim」—— **该路径不存在**。`commandTable`（`commands.go`）无 `/vim`；model struct 无 vim 字段。`/keymap`、`/vim`、`/contrast` **三个命令都不存在**，这一点现在是机器强制的（`internal/archtest/slashcmd_test.go::phantomSlashCommands` 是一张永久幻影命令黑名单，由 `internal/archtest/slashcmd_test.go::TestPhantomSlashCommandsNotAdvertised` 扫全部文本载体）；曾经宣称它们可运行时设置的那批 Go doc 注释已在 `cf088f7`（标题即 "retire the phantom-command comments"）删除，取而代之的 `internal/cli/tui/model.go::model` 字段注释现在明说这条 cascade 未接线、这些命令未注册。`internal/keymap/vim_test.go` 的 `TestVim_*` 与 `internal/keymap/keymap_cov_test.go` 的 `TestCov_Vim_*` 两组测试完整覆盖一个无调用方的状态机（**纯函数孤岛**）；现存条数用 `grep -rhoE '^func Test[A-Za-z0-9_]*[Vv]im[A-Za-z0-9_]*' --include='*_test.go' . | sort -u | wc -l` 现算。
- 证据形状：`/vim on` 后 `Update(KeyMsg{Runes:['j']})` 使 viewport 下移且 textarea 不含 `j`；`/vim off` 后同一按键使 `input.Value()=="j"`。

**3. 高对比主题** — 部分
- 依据：主题真实且可选中（`styles.go` `ThemeHighContrast`、`::themeHighContrast` 15 段配色、`::themeList` 注册进 `themeList`；选中 `commands.go::cmdTheme`；生效 `view.go::model.statusHeader`）。**缺口**：生效范围只有底部状态栏配色，transcript/弹窗/输入区的全局 `lipgloss.Style` 不随主题变；且三条配置入口（`config.go::TUIConfig`、`preferences.go::Preferences`、`YANSHI_HIGH_CONTRAST`）**全部失联**，`newModel`（`model.go`）写死 `theme: ThemeDefault`。`TestCmdTheme_PickerValidInvalid` 是唯一驱动 `/theme <name>` 的测试，但只测 `muted`，**从不测 high-contrast**，且只断言 `m.theme` 字段。~~`TestThemeNames`（对同一字面量表自我断言）与 `internal/cli::TestCheckHighContrastConfig`（被测函数无失败分支，永远 StatusOK）都是恒真空壳。~~ **撤回（第 26 轮，实测证伪）**：这两个测试都是变异敏感的，不是空壳。`themeList`（`styles.go::themeList`）移除 `themeHighContrast` → `TestThemeNames` 红在 `theme "high-contrast" found`；`doctor.go::checkHighContrastConfig` 的 `enabled` 恒 false → `TestCheckHighContrastConfig` 红在 `"enabled=false" does not contain "enabled=true"`。「永远 StatusOK」也不成立：`cfgErr != nil` 时走 `doctor.go::skipped` 返回 `StatusWarn`，而该测试的最后一行正是断言这条分支。本条子句判 `部分` 的依据是**前面的缺口**（生效范围只有状态栏、三条配置入口失联、`newModel` 写死默认主题、`/theme` 从不测 high-contrast），与这两个测试的质量无关。
- 证据形状：`m.theme` 取 default 与 high-contrast 两次 `statusHeader()` 输出**必须不等**，且高对比版含 bold SGR；再加一条「配置 `high_contrast:true` 启动后 `m.theme == ThemeHighContrast`」。

**4. 冲突可诊断** — 部分（四条里最实的一条）
- 依据：`keymap.go`（`Build` 聚合 error）、`::Builder.buildInternal`（区分 `conflict`/`normalized_duplicate`/`unknown_action`/`invalid_key`）、`::Map.Diagnostics`（`Diagnostics()`）。测试真实：`TestNewDefaultBuilder_DetectsNormalizedOverrideDuplicate`（`CTRL+K` vs `ctrl+k`，断言 `Kind=="normalized_duplicate"` 且 `Key=="ctrl+k"`）、`TestBuilder_RejectsInvalidConfigKey`、`TestBuilder_RejectsUnknownActionAfterCollection`、`internal/cli::TestCheckKeymapConfig_InvalidBindingsFail`。**缺的是用户可见半边**：唯一生产出口是 `internal/cli/doctor.go::checkKeymapConfig`，即 `yanshi doctor` 的一次性静态校验，TUI 运行时没有任何诊断出口。该函数的补救指引本身已经可执行 —— `cf088f7` 把它改成指向配置文件里的 `tui.bindings`，取代了原先指向一个从未注册的斜杠命令的死链，并由 `internal/cli::TestCheckKeymapConfig_InvalidBindingsFail` 两个方向同时钉住（消息必须含 `tui.bindings` 与诊断 `Kind`，且不得含那个幻影命令名，也不得回显未经信任的 binding 文本）。所以这一条现在缺的不是「指引指向不存在的东西」，而是**用户只能在 doctor 里看到冲突**。
- 证据形状：走生产构造让 TUI 自身暴露冲突（今天不存在的出口）。在此之前，doctor 侧的上界已由上面那条测试给到：`Bindings` 非法时 `CheckResult.Message` 同时命名可编辑字段与诊断 `Kind`。**骗过去**：只在 keymap 包内断言 `Diagnostic.Kind`。

---

### D3/I18N1 — locale / i18n（en / zh-Hans） · 台账 `partial`

> acceptance：至少 en/zh-Hans 切换；UI 与输出语言独立；自动检测

**1. 至少 en/zh-Hans 切换** — 未兑现（**虚报：连第 1 句都没有**）
- 依据：catalog 齐全（`internal/i18n/catalog/{en,zh-Hans}.json` + `i18n.go::NewBundle`），**但 TUI 运行时 locale 恒为 `"en"`** —— `internal/cli/tui/state.go::defaultBundle` 写死 `i18n.NewBundle("en")`，是 `newModel`（`model.go`）的唯一 bundle 来源；`cfg.I18N.UILocale` 从不到达 TUI（唯一消费者是 `doctor.go::checkLocaleConfig`）；`/locale` 命令从未注册。**走 i18n 的 UI 字符串只有 3 处**（`model.go::newModel` 的 placeholder，`commands.go::newCmdHelpEntry` 的标题键与 `b.Get(c.helpKey)`），catalog 里绝大多数 key 零引用 —— 总数与零引用数按 F1 **现算**而不写死：

```sh
python3 - <<'PY'
import json, os
cat = json.load(open('internal/i18n/catalog/en.json'))
src = {}
for root, dirs, files in os.walk('.'):
    # .claude 里可能停着 worktree（整棵树的副本）：不排除的话，只被副本引用的 key 会被算成有引用
    dirs[:] = [d for d in dirs if d not in ('.git', 'third_party', 'node_modules', '.claude')]
    for f in files:
        p = os.path.join(root, f)
        # internal/i18n 自己排除在外：那里的 key 清单是登记，不是消费
        if f.endswith('.go') and not f.endswith('_test.go') and not p.startswith('./internal/i18n/'):
            src[p] = open(p, encoding='utf8', errors='replace').read()
zero = [k for k in cat if not any('"' + k + '"' in t for t in src.values())]
print('keys', len(cat), 'zero-ref', len(zero))
PY
```

两个数以上面这段脚本的输出为准，本文不复述 —— 这不是洁癖：本条初稿写下的那对数字，被随后一次清理零消费幻影 key 的提交当场作废，而那次提交同时改了本文件却没回来改这一段。原稿更早写的「67 个 key 约 55 个零引用」则连漂移都算不上，是初稿就错：55 那个数把 `commandTable` 的全部 `helpKey` 当成了零引用，而它们无一例外经 `newCmdHelpEntry` 里那一个 `b.Get(c.helpKey)` 点消费 —— 这正是 F1 说的「写死的计数在写下的那一刻就开始腐烂」的更坏形态：写下的那一刻就已经不对。`TestCmdHelpEntry_NoHardcodedEnglish` 是**恒真空壳（断言选的字面量恰好避开硬编码）** —— 它用 zh-Hans bundle 渲染 /help 只断言输出**不含 `"Commands"`**，而同一函数（`commands.go::newCmdHelpEntry`）硬编码的 `"▌ Keyboard shortcuts"` 与 10 条英文描述原封不动进了 zh-Hans 输出，测试照绿。`TestCmdHelpEntry_PreRendered` 断言 `Contains(out, b.Get(c.helpKey))` —— 用同一 bundle 查同一 key 比对自身。两条都从参数注入 bundle，**绕开生产里 bundle 恒为 en 的事实**。
- 证据形状：从**生产构造路径**（`ui_locale: zh-Hans`）建 model，断言 `View()`/placeholder 含中文值且与 `en` 构造不等；再加反向断言「zh-Hans 渲染的 /help 不含任何 `helpKey==""` 项的静态英文」（现在会红，正因如此才有价值）。

**2. UI 与输出语言独立** — 部分（输出侧真接线，UI 侧不存在 → 「独立」是以「一维缺席」的方式成立）
- 依据：两个字段真实存在（`config.go::I18NConfig`）；输出侧真进组合根 —— `bootstrap.go::Build` 把 `cfg.I18N.OutputLanguage` 叠进 system instruction，实现在 `::AppendOutputLanguageInstruction`。`internal/bootstrap::TestOutputLanguageInstructionIndependentOfUILocale` 是**编译期恒真空壳**：它构造了带 `UILocale` 的 cfg，但被测函数签名是 `AppendOutputLanguageInstruction(base, outputLanguage string)` —— **在类型层面就收不到 UILocale**，「独立」不可能失败。它抓不到真正的风险变异：把 `bootstrap.go::Build` 的实参从 `cfg.I18N.OutputLanguage` 改成 `cfg.I18N.UILocale`（那是**调用点**，不在被测函数内），此测试全绿而两维立刻耦合。
- 证据形状：断言必须打在**调用点** —— 走 `bootstrap.Build`，`UILocale:"zh-Hans"` + `OutputLanguage:""` 时 system prompt 不含中文指令；`UILocale:"en"` + `OutputLanguage:"中文"` 时含中文指令而 bundle 仍是 en。一次运行同时观测两维且取相反值。

**3. 自动检测** — 部分（函数真实且被真驱动，但 TUI 不可达）
- 依据：`i18n.go::detectLocale`（`LC_ALL` → `LANG` → `en`）、`::normalizeLocale`（`normalizeLocale`）、`::NewBundle`（`"auto"` 时每次 `NewBundle` 重算）。**唯一传 `"auto"` 的生产调用者是 `doctor.go::checkLocaleConfig`**；TUI 传死 `"en"`。包内测试真实：`TestBundle_AutoRecomputedEachLoad`（`t.Setenv` 真改 env，并额外断言 `Persistent()` 未被冻结）、`TestBundle_LCAllCStopsLANGFallback`；生产侧唯一有效证据是 `internal/cli::TestCheckLocaleConfig_ValidAndSkipped`（只覆盖 `yanshi doctor`）。`internal/cli/tui::TestPreferences_AutoLocaleRecomputesAfterReload` **外观像端到端但 bundle 是测试自己 new 的**（`preferences_test.go::TestPreferences_AutoLocaleRecomputesAfterReload`），`loadPreferences` 生产调用者为 0 —— 把 `state.go::defaultBundle` 的 `"en"` 改成任何值，此测试仍绿。
- 证据形状：`t.Setenv("LANG","zh_CN.UTF-8")` 后走**生产 model 构造**，断言 `m.bundle.Effective()=="zh-Hans"` 且 placeholder 为 zh-Hans 值；再 `LC_ALL=C` 重建断言回落 en。

---

## W9 — 对外接口与 SDK

> **本包最重要的跨条目发现：`cmd/api-schema` 是伪生成器。** `cmd/api-schema/main.go::run` 从 `text := \`…\`` 起是一整段**硬编码 TS 字面量**，逐字符抄写接口；唯一与 Go 类型的接触点是 `main.go::run` 的 `_ = v1.SchemaBytes()` —— **返回值被丢弃**，而其上方注释自称「guards the generator against silent drift」，这句注释在语义上不可能成立。于是 `go run ./cmd/api-schema | diff - sdk/ts/v1.ts` 恒等，因为生成器与产物是同一段字面量（**循环自证**）。Python 侧 `sdk/python/src/yanshi_sdk/generated.py:1-16` **自述**「Hand-mirrored … D2 maintains the Python mirror by hand」，`pyproject.toml` 里的 `datamodel-code-generator` **从未被任何脚本调用**。这一条同时砸掉 D1/APS1#2 与 D2/V15#2。
>
> **第二个跨条目结构性障碍：SDK 测试全部不在 CI 里跑**（workflows 里 `pytest` / `vitest` / `npm test` 零命中），且它们住在 `sdk/` —— **GOV8 的 `evidenceScanRoots` 是 `{internal, cmd}`**（`internal/archtest/status_test.go`），`sdk/` 下的测试**永远不能作为终态证据**。D2/V15#1/#3 要翻终态，锚点必须落在 Go 侧。

### D1/APS1 — app-server (JSON-RPC) · 台账 `partial`

> acceptance：JSON-RPC thread/turn 可用;TS 类型可生成;与 HTTP 行为一致

**1. JSON-RPC thread/turn 可用** — 已兑现
- 依据：`internal/appserver/server.go`（dispatch 覆盖 initialize / capabilities / thread.start|resume|interrupt / turn.start|interrupt / config.read|write / shutdown）、`rpc.go`（标准错误码），已接线于 `cmd/yanshi/app.go::runApp`。测试 `TestJSONRPCStreamNotificationIsVersionedItem`（真跑一个 turn，断言通知是 versioned item）、`TestDispatchThreadResume`、`TestDispatchInterrupt`、`TestJSONRPCErrorCodes`、`TestJSONRPCNotificationHasNoResponseID` —— 断言的是 wire 上的具体字段与错误码，不是「调用发生了」。
- 证据形状：wire 字段 + 错误码逐个断言（现状即是）。

**2. TS 类型可生成** — **已作废**（W9 决定不生成，改为具名守门的手工镜像）
- 处置：伪生成器那一半**已删除**。`cmd/api-schema` 现在只有 `-markdown` 一个职责（它那半是真解析）；`sdk/ts/v1.ts` 头部改成「手工维护」，与 `sdk/python` 的模型一直以来的自述一致。
- 守门人换成 `internal/api/v1::TestContractParityAcrossFourSources`：比较 Go struct / JSON Schema / TS / pydantic 四路的字段集合，每条差异必须在 `intentionalDifferences` 里具名带理由，死条目判失败。这正是被丢弃的那个 `_ = v1.SchemaBytes()` 假装在做的事。
- 为什么不做真生成：真生成的额外收益只剩「注释与类型细节也不漂」，而代价是一个 Go→TS 类型映射器；且 TS 侧有 `ItemUpdatedNotification` 这类没有 Go 对应物的传输类型，生成器要么丢掉要么需要额外配置。**一段自称生成器的手抄字面量，不如没有。**

**3. 与 HTTP 行为一致** — 已兑现（W9）
- `internal/api/http::TestThreadStartAgreesAcrossTransports` / `TestThreadResumeAgreesAcrossTransports` / `TestInterruptAgreesAcrossTransports`：同一个 `*v1.Service`，同一组输入分别走两条门，比较解码后的 JSON。id/时间戳按值忽略但**按存在性检查**（丢字段是真分岔，换 id 不是）；resume 那条还让一条传输去 resume 另一条创建的 thread。双向变异探针（多一个字段 / 少一个字段）各自命中。
- `config/read|write` 的落盘缺口一并修掉：`cmd/yanshi/app.go::runApp` 现在按 `-config` 构造 `internal/appserver::FileConfig`（sidecar JSON，不写 YAML 本身），回归测试跑**两次 runApp 生命周期** —— 单进程测试对内存后端同样通过，这正是从来没有测试发现它的原因。

---

### D1/V12 — headless exec 增强 · 台账 `partial`

> acceptance：stdin/JSONL 可用;退出码稳定;可 resume;CI 可脚本化

**1. stdin/JSONL 可用** — 部分（**`--file` 路径实测有 bug**）
- 依据：`internal/cli/headless_input.go` `ReadHeadlessInputs`（text/lines/jsonl 三模式，真实）。**但 `cmd/yanshi/headless.go::runHeadlessCommand` 的 `--file` 分支把整个文件当一条 prompt**（`inputs = []cli.HeadlessInput{{Prompt: strings.TrimSpace(string(data))}}`），**完全绕过 `cfg.Input`、从不调用 `ReadHeadlessInputs`**。**实测复核**：`exec --input jsonl --file examples/headless-batch/sample.jsonl`（3 行）→ 输出 `(no real model configured)` **1 次**；同一文件走 stdin → **3 次**。bug 确认。`internal/cli::TestReadHeadlessInputs_JSONL` 及同族的其余各条（现算：`grep -rn '^func TestReadHeadlessInputs' internal/cli/*_test.go`，**注意这一族横跨 `headless_input_test.go` 与 `headless_extra_test.go` 两个文件** —— 只看前一个文件会少数一半）是真断言，但它们全部直接调 `ReadHeadlessInputs`，**覆盖不到出 bug 的那条 `--file` 分支**（那条根本不调用它，所以这一族无论有多少条都不可能变红）；`cmd/yanshi::TestRunHeadlessCommandStdinJSONL` / `TestRunHeadlessCommandFileInput` 是**吞错** —— 只 `assert.Equal(exitOK, code)`，一个吞掉全部输入的实现照样返回 0。
- 证据形状：断言**产出的 turn 条数**（或 stdout 上 assistant 段落数 / JSONL 行数）等于输入 prompt 条数，且对 `--file × {text,lines,jsonl}` 三组各测一次。**骗过去**：任何只看 exit code 或「输出非空」的断言 —— `docs.yml` 的「Headless smoke」步骤跑的正是这条 bug 命令但把输出丢给 `/dev/null`。

**2. 退出码稳定** — 部分
- 依据：`cmd/yanshi/main.go::exitOK/mapExecError`（0/1/2/124/130）。`cmd/yanshi::TestHeadlessExitCode` 是**对常量断言（变异盲于数值）** —— 它比对 `mapExecError(err)` 与 `exitTimeout` / `exitCancel` **常量本身**，所以把 `exitTimeout = 124` 改成 `99` 测试照样绿。它只钉住「哪个错误走哪个分支」，**没钉住脚本真正依赖的那五个数字**，而 doc 注释与 `getting-started.md` 都在对外承诺 124/130。
- 证据形状：断言必须写死字面量 —— `assert.Equal(t, 124, mapExecError(context.DeadlineExceeded))`；或更强：起子进程跑 `exec --timeout 1ns` 并断言 `ProcessState.ExitCode() == 124`。**骗过去**：常量对常量（现状）。

**3. 可 resume** — 部分
- 依据：`internal/cli/exec.go::execWithBackend`（发 `restore_session` 帧、消费回复）；`internal/api/http/ws_handlers.go handleRestoreSession`。`cmd/yanshi::TestRunHeadlessCommandResume` 是**空壳（零断言）** —— 测试体注释直说「non-zero expected (resume fails); the point is the resume branch ran」，只等它别超时。`internal/cli::TestExec_ResumeJSONLRendersRestoreEvent` 稍强但仍是 **spy + fake 太宽**：fakeExecBackend 自己回 `session_restored`，只断言 stdout 含该字符串 —— 证明的是「客户端发了帧并渲染了回复」，不是「历史真的被恢复了」。这批里最好的是 `internal/cli::TestExec_ResumeSendsRestoreBeforeUserMessage`（断言 restore 帧**先于** user turn）。
- 证据形状：端到端 —— 第一次 exec 说「记住 X」拿到 sessionID，第二次 `--resume <id>` 问「X 是什么」，断言**服务端喂给 model 的历史里含第一轮的消息**（fake model 可记录收到的 history）。**骗过去**：任何用 fake backend 自造 `session_restored` 回复的测试。

**4. CI 可脚本化** — 部分（**现成门禁可以顶，但有两个洞**）
- 依据：`.github/workflows/docs.yml` 的「Headless smoke」步骤：`out=$(./yanshi exec --fake-model -p "hi"); [ -n "$out" ]` + jsonl 那条。两个洞：① 第二条命令**零断言**（`>/dev/null`），正好掩盖了 #1 的 bug；② `docs.yml` 的 `paths:` 触发器是 `docs/** examples/** cmd/api-schema/** cmd/gendocs/** internal/docgen/** internal/config/** internal/api/v1/** .github/workflows/docs.yml` —— **`cmd/yanshi/**` 与 `internal/cli/**` 都不在里面**，改 headless 实现本身不会触发这道 smoke。
- 证据形状：把 headless smoke 挪进 `ci.yml`（无条件跑），或给 `docs.yml` 的 paths 加上 `cmd/yanshi/**`、`internal/cli/**`；且第二条命令必须断言输出条数。

---

### D1/V14 — 版本化 Agent API v1 · 台账 `divergent`

> acceptance：start/resume/interrupt + 流式 item 可用;有版本+Schema;兼容测试完善

**1. start/resume/interrupt + 流式 item 可用** — 已兑现（台账**低报**）
- 依据：`internal/api/http/agent_v1.go::Server.AgentV1/decodeAgentJSON`（5 条路由）+ `internal/api/v1/service.go`。测试 `TestV1TurnStreamItemsAreVersionedAndOrdered`（真跑 SSE 流，逐条断言 version/sequence/threadId/turnId）、`TestAgentV1ResumeEndpoint`、`TestAgentV1InterruptEndpoint`、`internal/api/v1::TestServiceStartTurnStreamsItemsInSequence`、`TestServiceInterruptIsIdempotent`。
- ⚠️ **一个对外契约谎报**：`ThreadSnapshot.Items`（`internal/api/v1/types.go::ThreadSnapshot/ThreadResumeResponse`）**永远为空** —— `service.go snapshot()` 返回 `ThreadSnapshot{Version, Thread}`，两条 Resume 路径都走它，无人填 Items；而 `agent_v1.go::Server.AgentV1` 与 `appserver/server.go::Server.dispatch` **都在转发 `snapshot.Items`**。两条传输同时转发同一个恒空切片 —— 但**恒空只暴露在类型 / schema / SDK / 生成文档层，不在 wire 层**：两处 Items 字段都**已经**带 `omitempty`，键在 JSON 里根本不出现，客户端看到的是「服务端没给」而不是「给了空数组」。核对命令与输出：

  ```sh
  $ grep -nE 'Items +\[\]Item' internal/api/v1/types.go
  101:	Items   []Item `json:"items,omitempty"`
  154:	Items   []Item `json:"items,omitempty"`
  ```

  （模式必须写成 `Items +\[\]Item`：gofmt 把字段名与类型对齐成多个空格，单空格的字面模式返回**空输出**，读者会据此得出与本段相反的结论。）

  所以恒空真正可见的地方，Go 侧是 `internal/api/v1/types.go::ThreadSnapshot`。**跨出 Go 之后这个类型就改了名** —— schema / SDK / 生成文档里它一律叫 `ThreadResumeResponse`，`ThreadSnapshot` 是**纯 Go 内部名**，在 **`sdk/` 下**零命中：

  ```
  $ grep -rn 'ThreadSnapshot' sdk/ | wc -l
  0
  ```

  所以照这个名字去 `sdk/` 里找要改哪几处会一无所获，得按 `ThreadResumeResponse` 找。**`docs/` 不在此列** —— `grep -rln 'ThreadSnapshot' docs/` 是有命中的，其中就有 W9 自己的计划 `docs/superpowers/plans/2026-08-03-s0-w9-contracts.md`（Task 3 的标题与那条验收勾选项用的都是 `ThreadSnapshot.Items` 这个名字）。把上面那条 grep 读成「全仓零命中」会把实施者从自己的任务清单上引开。逐层：

  - `sdk/schema/v1/agent-api.schema.json` 的 `$defs.ThreadResumeResponse.properties.items`
  - `sdk/python/src/yanshi_sdk/generated.py` 的 `class ThreadResumeResponse` 的 `items: Optional[list[Item]]`
  - `sdk/ts/v1.ts` 的 `export interface ThreadResumeResponse` 的 `items?: Item[]`（`sdk/ts/src/generated.ts` 只是 `export * from "../v1.js"` 的转发口，`sdk/ts/src/client.ts` 拿它当 `resume()` 的返回类型 —— 这一层是本条上一版整个漏掉的）
  - `docs/api/resources.md` 的 `### ThreadResumeResponse` 字段表里 `| items | Item[] |` 那一行（在 `api-defs:ThreadResumeResponse` 生成块内，改完要重跑 `go run ./cmd/api-schema -markdown docs/api/resources.md`）

  这几处承诺了一个 wire 上永不出现的字段。
- ℹ️ **纠正过时结论**：W9 旧核验 note 里「`TurnStartParams.Images` 是死字段」**已过时** —— `service.go::Service.runTurn` 现在真的把 `p.Images` 填进 `TurnOpts`，`internal/api/v1::TestServiceTurnCarriesImagesToTheOrchestrator` 是端到端断言（fake vision model 回 `fake-vision(1 image)`）。
- 证据形状（针对 Items）：断言 resume 一个有历史的 thread 后 `len(resp.Items) > 0` 且 sequence 单调；或从 wire 契约里删掉该字段（连带上面那四处镜像）。**骗过去**：任何只在 JSON 层看这个键的测试 —— 因为 `omitempty` **已经在了**，「响应体里没有 `items` 键」和「`resp.Items == nil`」这两条断言**今天就是绿的**，它们证明的恰好是缺陷本身。⚠️ 别把这条读成「可以事后加 `omitempty` 来掩盖」：它不是一条待防范的作弊路径，而是**已经生效的现状**，W9 照这句去加 tag 只会重复一次无操作，并对「为什么这个恒空字段一直没人发现」得出错误因果。

**2. 有版本+Schema** — 部分（`divergent` 判定成立）
- 依据：**三份不同的 schema 并存，`$id` 互不相同** —— `internal/api/v1/schema.go`（`schemaDocument`，仅 3 个 `$defs`：Thread/Turn/Item，`$id: …/agent-api-v1.json`，**唯一被 `GET /api/v1/schema/agent-v1.json` 服务的那份**）／`sdk/schema/v1/agent-api.schema.json`（21 个 `$defs`）／`sdk/schema/v1.1/…`（自述 "Not served by D1"）。`schema.go::SchemaBytes` 的注释仍写着「Task 9 expands this document」，那次扩展从未落到运行时。`TestSchemaDeclaresVersionAndCamelCaseResources` / `TestSchemaBytesAreStableForContractReview` 只测运行时那份自身自洽；**全仓没有任何测试比较任意两份 schema**。
- 证据形状：一条 Go 侧 parity 测试（**必须落在 `internal/api/v1`** —— `go test ./...` 是无条件硬跑，而 Node/Python 在 CI 里是可选步骤），把 `types.go` 的 struct tag 集合 ↔ `sdk/schema/v1` 的 properties 集合逐 `$def` 对账，**有意差异登记进一张只减不增的表**（初始内容是 `ContextItem` 与 `FileChange` —— `sdk/schema/CONTRACT_HANDOFF.md` 的「Diff IDE context」与「Diff fileChange」两节把它们记为 D2 前瞻字段，属有意；外加 `Range`，它在 CONTRACT_HANDOFF 里**没有自己的条目**，只作为 `sdk/schema/v1/agent-api.schema.json` 里 `ContextItem.range` 指向的 `$defs.Range` 传递性地存在，登记时别照 CONTRACT_HANDOFF 的节标题去找它）。**骗过去**：只断言两份文件都能 parse。

**3. 兼容测试完善** — 部分
- 依据：`X-Yanshi-API-Version` 头 + 请求体 `version` 字段；`internal/api/http::TestV1CompatibilityMatrix` 有 3 个 case（缺省 version、未知字段忽略、坏 JSON 400）+ 头断言。其中「所有 key 都是 camelCase」的断言是 `!strings.Contains(body, "_")` —— **过于粗糙**（response body 里任何值含下划线都会误报，且是启发式而非结构校验）。`TestUnknownFieldsAreIgnored`、`TestItemJSONUsesCamelCaseAndVersion` 是真断言。
- 证据形状：「完善」需要一张显式版本矩阵 —— `{v1, v1.1, v2, 缺省, 垃圾} × {每个端点}` 的接受/拒绝表（TS/Python 侧已有这个形状，Go 侧没有）。当前 Go 侧只覆盖 `thread/start` 一个端点。

---

### D2/V15 — TS / Python SDK · 台账 `partial`

> acceptance：start/resume/run/stream/cancel 可用；类型生成；契约测试

**1. start/resume/run/stream/cancel 可用** — 部分
- 依据：`sdk/ts/src/client.ts`、`sdk/python/src/yanshi_sdk/client.py` 两侧五个方法都在，transport 覆盖 HTTP+SSE+WS；`sdk/ts/tests/client.test.ts`、`sdk/python/tests/test_client.py` 断言不弱（路由、camelCase body、item 流、HttpError 包装、断线保留 lastSequence）。**两个致命问题**：① **全部走假 `fetch` / 假 httpx**，从不打真 Go server ——「可用」是对着 mock 证的；② **CI 从不执行它们**（`docs.yml` 只做 `tsc --noEmit` 和 `python -c "import yanshi_sdk"`）。
- 证据形状：起一个 `yanshi serve --fake-model`（或 httptest 起的 in-process server），SDK 打真实端点走完 start→run→interrupt。**骗过去**：现在这种全 mock 的 transport 测试。**并且必须加一个 CI job 真跑 vitest/pytest**，否则测试再好也只是死代码。

**2. 类型生成** — 未兑现（**虚报**，与 D1/APS1#2 同一根因）
- 依据：见本包开头。`sdk/ts/package.json` 的 `"generate"` script 是 `echo "…nothing to regenerate" && exit 0`。
- 证据形状：同 APS1#2 —— 「改 Go 类型 ⇒ 生成物变化」。或者**改写 acceptance**：如果决定接受手工镜像，就把子句改成「类型镜像有 parity 门禁」而不是「可生成」。

**3. 契约测试** — 部分（存在但无门禁，且 GOV8 引用不了）
- 依据：`sdk/ts/tests/contract.test.ts`、`sdk/python/tests/test_contract.py` 用 ajv/jsonschema 对 `sdk/schema/v1/fixtures/` 做真校验，断言不弱（sequence 严格单调、未知 item type 保留、坏 envelope 被拒）。**不是假证据，但有两个结构性问题**：① **不会被执行**（CI 无任何 job 跑它们）；② **fixture 是手写的**，不是从 Go server 生成的 —— 没有任何东西保证 `sdk/schema/v1/fixtures/*.json` 与真实响应一致，所以它校验的是「手写 fixture 符合手写 schema」。
- 证据形状：fixture 必须由 Go 侧生成（golden），或 Go 侧加一条测试读入这批 fixture 并断言它们能反序列化成 `v1.Item` / `v1.ThreadStartResponse`。⚠️ **GOV8 引用锚点必须落在 Go 侧。**

---

### H2/APIREF1 — v1 API/协议参考 · 台账 `partial`

> acceptance：v1 API 有参考；SDK 用法有示例；与 schema 一致

**1. v1 API 有参考** — 已兑现
- 依据：`docs/api/README.md`（索引）、`resources.md`（生成）、`schema.md`（生成）、`events.md`、`jsonrpc.md`。**现成门禁够顶**：`.github/workflows/docs.yml` 的「Generated snippet gate」步骤（生成快照 diff-gate）（`api-schema -markdown` × 2 + `gendocs` × 2，再 `git diff --exit-code docs/api/ docs/user-guide/`），触发路径含 `internal/api/v1/**`。
- 证据形状：CI diff-gate 形状正确（现状即是）。

**2. SDK 用法有示例** — 部分（示例存在但**Python 侧实测崩溃**）
- 依据：`docs/api/sdk-ts.md`、`docs/api/sdk-python.md` 存在。**实测复核**：`docs/api/sdk-python.md:20` 与 `examples/sdk-python/main.py:25` 都写 `item.text or item.toolName`。pydantic v2 的字段名是 `tool_name`，`toolName` 只是 alias，**不产生属性别名** —— 实测 `Item(...).toolName` → `AttributeError`。而 v1 的 `turn.started` / `turn.completed` item 没有 text，所以 `or` 右侧会在**第一条 item 上**就被求值 → **文档里的示例必崩**。CI 的 `docs.yml`「Python parse + import」步骤只做 `python -m py_compile`（纯语法）+ `import yanshi_sdk`，**吞掉运行时错误**。
- 证据形状：CI 必须**真跑**示例（起 `yanshi serve --fake-model` 后 `python examples/sdk-python/main.py`），或至少加一条 pytest 断言 `Item` 的公开读取属性名。**骗过去**：`py_compile` / `tsc --noEmit` 这类只看编译期的检查。

**3. 与 schema 一致** — 部分
- 依据：`cmd/api-schema/markdown.go::runMarkdown` 的 `renderBlocks(v1.SchemaBytes())` 确实从运行时 schema 生成。三处缺口：① `markdown.go` 的 `paramResponseDefs()` 是**手工维护表**（doc 注释自认「hand-maintained field map … mirrors the hand-written TS interfaces in main.go」）→ `resources.md` 的字段行可以与 TS/Python 的真实字段脱节而无人报警；② `markdown.go` 的 `schemaDocHeader` **自相矛盾**：同时声称是「`sdk/schema/v1/agent-api.schema.json` 的完整 JSON Schema」**且**「从 `internal/api/v1/schemaDocument` 生成」—— 而后者只有 3 个 `$defs`；③ `jsonrpc.md:15-16` 对 `config/read|write` 的描述与 in-memory 现实不符。
- 证据形状：`resources.md` 的字段表必须由 schema 派生（消灭 `paramResponseDefs` 手工表），否则 diff-gate 只能证明「手工表没被改过」。

---

## W10 — 覆盖率、发布与文档

> **本包大量子句是 N 类（测量不是断言 / 文档质量主张）**，逐条处置建议见文末汇总表。

### E1/COV2 — proto 覆盖 67% → 80%+ · 台账 `partial`

> acceptance：覆盖率 ≥80%；全帧往返；SSE golden 稳定；WS/SSE 词表对称

**实跑复核**：`go test ./internal/proto -cover` = **100.0% of statements** ✔

**1. 覆盖率 ≥80%** — 部分（数值达标，但**不可终态化** —— **N 类**）
- 依据：全仓没有任何测试对覆盖率设门禁。数值本身满足。
- 证据形状：见文末 N 类汇总。两条出路：(a) 子进程跑 `go test -cover` 并断言下限的门禁测试；(b) 改写 acceptance（须同步改 `acceptancePins`）。

**2. 全帧往返** — 部分（**「全」没有分母**）
- 依据：`TestServerFrame_ParityRoundTrip`（5 个子帧）、`TestClientFrame_ParityRoundTrip`（8 个）、两条 `*_UntestedConstructorsRoundTrip` —— 单条断言质量好（逐字段比对 + omitempty 反向断言）。**但** `frame.go` 有 **35 个 ServerFrame + 41 个 ClientFrame 构造器**，`goldenFrames()`（`frame_test.go`）只列 **35 条**；`grep -nEw "reflect|ast|parser" internal/proto/*_test.go` 零输出 —— **没有任何东西保证「全」**。（必须带 `-w`：不带词界的 `grep -nE` 会得两行，`"paste"` 里的 `ast` 与 `older parsers` 里的 `parser`，都不是枚举工具。本文第 19 行的规矩是否定式断言要附上产生空输出的那条命令，原先这里写的正是那条**有**输出的。）新增一个帧类型而不加进 `goldenFrames`，所有测试照绿。
- 证据形状：用 `go/ast` 或 `reflect` 枚举 `frame.go` 里全部 `func New*(...) ServerFrame`，断言每一个都在 `goldenFrames()` 的 Type 集合里（并做**死条目检测**）。**骗过去**：手写枚举列表 —— 那只是把遗漏从一处搬到另一处。

**3. SSE golden 稳定** — 已兑现（在 `goldenFrames` 覆盖范围内）
- 依据：`internal/proto/testdata/sse_golden.txt`（35 个 event 块）+ `TestSSEEvent_Golden` —— 真 golden（`-update` 才重写，CI 不带该 flag 则 byte-diff 失败）。
- ⚠️ 它的**分母**受 #2 的完整性缺口牵连。

**4. WS/SSE 词表对称** — 未兑现（**虚报 —— 恒真断言**）
- 依据：`internal/proto/frame.go`：`func (f ServerFrame) SSEEvent() { event = f.Type; data, _ = json.Marshal(f); return }`。`TestVocabulary_Symmetry` 断言 `assert.Equal(t, f.Type, event)` —— 而 `SSEEvent()` 的**第一行就是** `event = f.Type`。**这条断言在任何帧集合下都不可能失败。** 第二条 `assert.NotEmpty(data)` 同理（`json.Marshal` 一个 struct 至少返回 `{}`）。它测的是 `SSEEvent` 的一行赋值，**完全没有触及真正的风险面** —— `internal/api/http/ws.go` 发出的帧集合 vs SSE handler 发出的帧集合是否相同。全仓**没有任何测试比较这两个发射集**。
- 证据形状：AST 扫 `ws.go` 与 `chat.go`/`ssebackend.go` 里出现的 `proto.New*` 构造器集合，断言两者相等（差集登记进一张只减不增的豁免表）；或运行时对同一 turn 分别驱动 WS 与 SSE，断言收到的 `type` 序列相同。**骗过去**：现在这条 —— **任何只在 `proto` 包内自证的测试都够不到「两个 handler 是否同步」这个命题**。

---

### E1/COV3 — bootstrap 集成测试 23% → 50%+ · 台账 `partial`

> acceptance：覆盖率 ≥50%；最小 App 可构建并跑一轮 turn；软降级被验证

**实跑复核**：`go test ./internal/bootstrap -cover` = **94.1% of statements**，16.8s ✔

**1. 覆盖率 ≥50%** — 部分（数值达标，不可终态化 —— **N 类**，同 COV2#1）

**2. 最小 App 可构建并跑一轮 turn** — 部分（**虚报 —— 测试标题与实际被测对象不符**）
- 依据：「可构建」这半条是**真断言** —— `TestBuild_MinimalApp`（20 个 App 字段逐个 NotNil）配 `TestBuild_AssemblyOrder`（断言 `Orch.Profile()` 不含 `*` 且非空）。**但** `TestBuild_EndToEndTurn`（`bootstrap_test.go`）的 doc 注释宣称「a full user turn runs through a bootstrap-assembled stack」，实际代码 `buildMinimalApp(t)` 之后**只 require 了 `app.Store` 非 nil**，然后**另起炉灶** `orchestrator.New(orchestrator.Config{Model: mdl, Tools: []{tt.Now}, Profile: 手写 profile})` 跑 turn —— **`app.Orch`、`app.AgentTools`、`app.Model` 全程未被使用**。它证明的是「orchestrator 包能跑 ReAct 循环」，不是「bootstrap 装配出的栈能跑一轮 turn」。假证据类别：**替身太宽 —— 用本地重建的替身冒充被测对象**。
- 证据形状：必须用 `app.Orch`（或 `app.AgentAPI.StartTurn`）驱动，让 model 由 `bootstrap.Build(FakeModel:true)` 注入，断言 turn 跑出 tool_result + 终态 item。**骗过去**：正是现在这种「build 一个 App 然后不用它」。

**3. 软降级被验证** — 部分（3 条里 2 条真，1 条是空壳）
- 依据：
  - `TestBuild_PluginDiscoverySoftDegrade` — **真**：注入不存在的 `builtin_dir`（**真的诱发了失败**），断言 Build 不返错且 `app.Skills` 非 nil。
  - `TestBuild_MCPStartupSoftDegrade` — **真**：注入 `command: "does-not-exist"`，同上。
  - `TestBuild_VCSSoftDegrade` — **空壳 + 未诱发失败**：整个测试体只有 `require.NotNil(t, app.VCS)`，注释自认「the only hard requirement is no panic」。它**没有主动让 InitRepo 失败**（用的是共享的 `buildMinimalApp`），所以即便 VCS 初始化成功它也照样绿 —— **这条测试压根不知道自己在测降级路径**。
- 证据形状：降级测试必须**证明降级真的发生了** —— 断言 `app.VCSRepoID == ""`（降级信号）**且** `app.VCS != nil`（未级联失败），再加一条正向对照（正常 workroot 下 `VCSRepoID != ""`）。**骗过去**：只断言某字段非 nil 而不断言失败态被触发 —— 软降级测试尤其容易变成「happy path 换个名字」。

---

### H1/PKG1 — 多平台打包分发 · 台账 `partial`

> acceptance：多平台二进制可构建；release 产物完整；checksum；fork 行为保留

**1. 多平台二进制可构建** — 部分
- 依据：`.goreleaser.yaml`（4 target：windows/amd64、linux/amd64、linux/arm64、darwin/arm64）+ `build.sh`。`.github/workflows/ci.yml` 的 `build` job（`{ubuntu,windows,macos} × {default,nokeyring}`，`CGO_ENABLED=0`，加 `yanshi -h` 冒烟）是**真门禁**，但它是**原生编译 3 个 host**，`linux/arm64` 这个 goreleaser target **从不被构建**。
- 证据形状：CI 里跑一次 `goreleaser release --snapshot --clean`，或用 `GOOS/GOARCH` 交叉编译矩阵覆盖那 4 个组合。**骗过去**：`goreleaser check`（见 #2）。

**2. release 产物完整** — 未兑现（配置存在，零执行验证）
- 依据：`.goreleaser.yaml` 的 `archives` 块（`files:` 含 `config.example.yaml` + `README.md`，windows 覆盖为 zip）。`ci.yml` 的 `release-config` job 只跑 `goreleaser check` —— **它只做 config 语法/键名校验（UnmarshalStrict），不产出任何产物**。没有任何东西断言 archive 里真的有那两个文件、或真的产出了 4 个包。
- 证据形状：`goreleaser release --snapshot` 后断言 `dist/` 下有 4 个 archive，且 `tar tzf` / `unzip -l` 列表包含 `config.example.yaml` 与 `README.md`。

**3. checksum** — 未兑现（同上）
- 依据：`.goreleaser.yaml` 的 `checksum: {name_template: "checksums.txt", algorithm: sha256}`。无测试；`goreleaser check` 不校验这个块的运行结果。
- 证据形状：snapshot 后断言 `dist/checksums.txt` 存在、行数 == archive 数、且随机抽一行 `shasum -a 256` 复算相符。

**4. fork 行为保留** — 部分（**证据被两道结构性障碍挡住**）
- 依据：实现在 `third_party/bubbletea/key_windows.go::keyType`（VK_RETURN + ctrl → `KeyCtrlEnter`）+ `go.mod:117` 的 `replace`。
  - `third_party/bubbletea::TestKeyType_ReturnDistinguishesCtrl` 断言质量最好（5 个修饰键组合逐个比对），**但它永远不会被本仓的 CI 跑到**：① 文件头有 `//go:build windows`（GOV8 的 `Constrained` 判定直接拒，`internal/archtest/status_test.go::testRefInfo`）；② `third_party/bubbletea` **是独立 module**（有自己的 `go.mod`），实测 `go list ./... | grep -c third_party` = **0** —— 根目录 `go test ./...` **根本不包含它**；③ `sdk/`、`third_party/` 都不在 GOV8 的 `evidenceScanRoots`（`{internal, cmd}`）里。
  - **可用的那条是 `internal/cli/tui::TestModel_CtrlEnterInsertsNewline`**（`model_test.go`）：在主 module 内、跨平台、断言 `tea.KeyEnter != tea.KeyCtrlEnter`、`KeyCtrlEnter.String() == "ctrl+enter"`，且 Ctrl+Enter 插入换行而 Enter 提交。去掉 `replace` 会直接编译失败（上游 v1.3.10 没有 `KeyCtrlEnter`）。
- 证据形状：`internal/cli/tui::TestModel_CtrlEnterInsertsNewline` 可作为「fork 未被 revert」的锚点；但「Windows 上 VK_RETURN 解码正确」这一半在当前布局下**无法被 GOV8 引用** —— 要么把该断言搬进主 module 的一个跨平台 seam（把 keyType 判定逻辑参数化），要么在 acceptance 里明确这一半由 CI 的 windows leg 承担而非台账证据。

---

### H1/VER1 — 语义化版本 + CHANGELOG 自动化 · 台账 `partial`

> acceptance：版本号来自 git tag；CHANGELOG 可生成；发布流程文档化

**1. 版本号来自 git tag** — 部分
- 依据：`build.sh` 的 release 分支（`git describe --tags --abbrev=0 --match 'v[0-9]*'` → ldflags 注入）；`.goreleaser.yaml` 的 `-X …version.Version={{.Version}}`；`internal/version/version.go` `var Version = "0.4.0"`。`internal/version::TestVersionIsOverridable` 的**第二半是恒真空壳**：`version.Version = "1.0.0"; assert.Equal("1.0.0", version.Version)` —— **给包级 var 赋值再读回来**，唯一的信息量是「Version 不是 const」，而那本来就是编译期事实（对 const 赋值根本不通过编译）。**第一行不是恒真的**：`require.Equal(t, "0.4.0", version.Version, "dev default must stay 0.4.0")` 钉住了未注入 ldflags 时的源码内默认值，改 `version.go` 的字面量会让它变红 —— 这条有效，只是它守的是「默认值没被误改」而非「版本号来自 git tag」。**整条测试与 git tag 仍无关系**。`TestParseRejectsMilestoneTags` 是好测试但只覆盖解析器。
- 证据形状：起子进程 `go build -ldflags "-X …Version=9.9.9"` 后跑 `yanshi version` 并断言输出含 `9.9.9`；或至少断言 `build.sh` 的 `--match` 模式与 `cliff.toml:36` 的 `tag_pattern` 一致（两处都是 `v[0-9]*`，目前只靠注释同步）。**骗过去**：现在这条自赋值断言。

**2. CHANGELOG 可生成** — 部分（**N 类：从未真跑过**）
- 依据：`cliff.toml`（`tag_pattern = "v[0-9]*"`，10 组 `commit_parsers`）；`release.yml:32-44` 装 git-cliff v2.6.1 并生成。**无测试**。`CHANGELOG.md` 仍是**种子文件**，自述「The first tagged release (v1.0.0) will replace this seed」（复核用 `grep -n 'replace this seed' CHANGELOG.md`，有输出即仍未被 git-cliff 真正生成过）—— 即**这条流程从未被真跑过一次**。⚠️ 这里此前写的是行数，而 `CHANGELOG.md` 由 `git-cliff` 生成：**这条子句一旦被兑现，那个行数就会变**，于是「文件很短」这个论据会在它最该继续成立的时刻先失效。判据换成种子自述句，兑现时它自己会消失。
- 证据形状：CI 里跑一次 `git-cliff --config cliff.toml --unreleased` 并断言退出 0 且输出非空（不需要 tag）。**骗过去**：断言 `cliff.toml` 存在。

**3. 发布流程文档化** — 已兑现
- 依据：`docs/upgrade-guide.md:49-82` 是完整 runbook（`doctor --release` 退出码语义、`git tag v1.0.0` 打标、`release.yml` 三步、产物未签名的显式说明）；`docs/commit-convention.md` 补前缀表且**诚实声明**「enforced by review … **not** by a commit-lint tool」（无虚假门禁主张）。runbook 第 1 步引用的 `doctor --release` 行为有真断言支撑（`internal/cli::TestDoctorHasNewReleaseChecks` / `TestDoctorReleasePromotesConfigVersionWarnToFail` / `TestDoctorReleaseFlagDoesNotChangeCheckSet`）。
- 证据形状：**文档质量主张里最扎实的一条** —— 它引用的每个可执行步骤都有对应测试或 workflow。这是 N 类子句被正确行为化的范本。

---

### H2/CONTRIB1 — 贡献指南 + docs 归档 · 台账 `partial`

> acceptance：CONTRIBUTING 存在；约定可执行；docs 结构清晰

**1. CONTRIBUTING 存在** — 已兑现
- 依据：`/CONTRIBUTING.md`（「承重架构约定」H2 下若干 `###` 小节 + ADR 指针 + 与之平级的 commit 约定 H2；条数别写死，用下面子句 2 里那条 `grep` 现数）。**纯存在性主张**，`docs.yml` 的「Cross-doc relative links reachable」步骤已间接守住它（CONTRIBUTING 被 getting-started.md 相对链接引用，文件消失即断链失败）。
- 证据形状：现成门禁已顶。

**2. 约定可执行** — 部分
- 依据：分母是 CONTRIBUTING「承重架构约定」这个 H2 下的 `###` 小节，现数（`grep -nE '^### ' CONTRIBUTING.md`，别按印象数）。**每一条都给出裁决，两桶相加恰好等于分母**：
  - **机器强制**：六边形 / 唯一组合根 = GOV1；context 注入是横切模式 = GOV6；Guard fail-closed = `internal/guard::TestGuard_DeniesEmptyTools` + `internal/guard::TestCheckEmptyToolsAllowIsOverridableHardDeny`，外加 `ci.yml` 的 `fuzz-seed` 硬门禁；单文件 ≤1000 纯代码行 = GOV2；注释是承重文档 = GOV3；**本地 fork = 编译器**（最强的一档：生产代码 `internal/cli/tui/handlers.go` 直接引用 fork-only 标识符 `tea.KeyCtrlEnter`，实测把 `go.mod` 里那条 `replace` 拿掉后 `go build ./internal/cli/tui` 报 `undefined: tea.KeyCtrlEnter` / `undefined: tea.Repaint`；上游 v1.3.10 的 `key.go` 里 `KeyCtrlEnter` 零命中）。
  - **不可执行**：「Fake 优先于 mock」（无门禁，也难自动化）；「单 binary + 两种传输一套协议」（「单 binary」由构建形态天然成立，但承重的那半句「新增帧类型必须同时更新 `ws.go` 与 SSE handler」**没有任何门禁** —— 正是 E1/COV2#4 证明的那条）。
- ⚠️ **「conventional commit」不在这个分母里**：它住在 CONTRIBUTING 里与「承重架构约定」**平级的另一个 H2**「提交 / PR 约定」下。它同样不可执行（`docs/commit-convention.md` 开头诚实承认「enforced by review … **not** by a commit-lint tool」，`.github/` 下无 commitlint、`.git/hooks/` 无非 sample 钩子），但把它算进上面那两桶会让分母对不上，也会让「本地 fork」这条真正需要裁决的约定被挤掉 —— 这正是本条上一版发生过的事。
- 证据形状：一条「CONTRIBUTING 承诺的每条硬约定都有对应门禁」的对账测试（形如 GOV8 的台账镜像：约定条目 → 测试引用的映射）；或**改写 acceptance**，把「约定可执行」限定为「架构约定由 GOV1–GOV8 强制」，把 commit 约定明确划为 review 责任。

**3. docs 结构清晰** — 部分（**N 类：质量主张，不可直接断言**）
- 依据：`docs/` = `adr/` + `api/` + `archive/` + `superpowers/` + `user-guide/` + 若干散落 `.md`（`ls docs/*.md` 现数）；**`docs/` 下无顶层 README 索引** —— `ls docs/*/README.md` 的输出是 `adr/ api/ archive/ user-guide/` 四个，**只有 `superpowers/` 没有**，所以缺的是顶层那一层索引，不是子目录索引。`docs.yml` 有三道**真门禁**可部分顶：「ADR code-path reachability」（ADR 里引用的 `internal/...` 路径必须存在）、「Archive no dead path refs」（活文档不得引用已归档路径）、「Cross-doc relative links reachable」；另有 `internal/archtest::TestVSCodeExtensionNotAdvertisedInDocs` 守归档墓碑。
- 证据形状：「结构清晰」本身不可断言。可断言的替代物是**链接完整性 + 归档隔离 + 索引完备性**（每个 `docs/` 顶层目录必须被某个索引页链接到）—— 前两项已有门禁，第三项没有。建议**改写 acceptance** 为这三项。

---

### H2/EX1 — examples 目录 · 台账 `partial`

> acceptance：≥5 个示例；可跑；覆盖主要集成点

**1. ≥5 个示例** — 已兑现
- 依据：7 个 —— `headless-exec/`、`headless-batch/`、`sdk-typescript/`、`sdk-python/`、`custom-tool/`、`custom-skill/`、`goalloop-config/`。
- 证据形状：一条断言 `examples/README.md` 表格行数 == `examples/` 子目录数的对账测试即可；实质风险不在这条。

**2. 可跑** — 部分（**两条实测不可跑 / 说谎**）
- 依据：门禁现状（`docs.yml`）：`go build ./examples/custom-tool` ✔、`tsc --noEmit --project examples/sdk-typescript/tsconfig.json` ✔、`py_compile examples/sdk-python/main.py` ✔（**仅语法**）、`exec --input jsonl --file examples/headless-batch/sample.jsonl >/dev/null` ✔（**零断言**）。**三个 `run.sh`（headless-exec / headless-batch / goalloop-config）从不被 CI 执行**；`custom-skill/` 无任何检查。**实测两条缺陷**：
  1. `examples/sdk-python/main.py:25` 的 `item.toolName` → **AttributeError**（pydantic 字段名是 `tool_name`，`toolName` 只是 alias，不产生属性别名；首条 `turn.started` item 无 text，`or` 右侧必被求值）。CI 的 `py_compile` 吞掉它。
  2. `examples/headless-batch/run.sh` 用 `--input jsonl --file`，实测**只跑 1 个 turn**（见 D1/V12#1 的 bug），而脚本结尾却打印 `"headless-batch OK (processed $(grep -c . "$sample") prompts)"` —— **这个 "3" 是 grep 数出来的，不是跑出来的**。这是断言与被断言物脱钩的教科书形态。
- 假证据类别：`py_compile`/`tsc --noEmit` = 只覆盖编译期；`>/dev/null` = 吞输出；`grep -c` 报数 = 对**输入**而非**输出**断言。
- 证据形状：CI 必须**执行**每个 `run.sh`（它们本身已自带 `set -euo pipefail` + 非空断言，只差被调用），且 `headless-batch/run.sh` 的断言必须改成「**输出段落数 == 输入行数**」；两个 SDK 示例必须对着 `yanshi serve --fake-model` 真跑一遍。

**3. 覆盖主要集成点** — 部分（**N 类：「主要」无分母**）
- 依据：7 个示例覆盖 headless 单/批、两个 SDK、自定义工具、自定义技能、goal loop。**未覆盖**：`app`（JSON-RPC app-server）、`serve` + WS 客户端、autoVCS、guard profile。已知缺口**已诚实记录**：`examples/README.md` 明写 custom-tool 暴露的 API gap（无公开外部工具注册点，工具在 `bootstrap.Build` 装配）并声明本批不 hack —— 这是诚实的低报，不是虚报。
- 证据形状：「主要集成点」需要显式清单才能断言。建议**改写 acceptance** 为枚举式（「headless / SDK×2 / 自定义工具 / 技能 / goal loop 各有示例」），否则「主要」永远可争辩。

---

### H2/UDOC1 — 用户指南 · 台账 `partial`

> acceptance：覆盖主要用法；getting started 可零依赖跑通；与实际不漂移

**1. 覆盖主要用法** — 已兑现
- 依据：`docs/user-guide/` 8 页 + README 索引。**`cmd/gendocs::TestSubcommandListMatchesDispatch` 是真断言且双向**：正向扫 `main.go` 的每个顶层 `case "x":` 必须在 `yanshiSubcommands` 里，反向用 `reflect.DeepEqual` 把清单钉死成 `{yanshi,serve,chat,exec,app,goal,vcs-mcp,pr,auth,doctor}`。配合 `docs.yml` 把 `-help-all` 生成进 `entrypoints.md`/`tui.md` 并 diff-gate ⇒ **新增子命令而不写文档会红**。这是本包最好的一道漂移门禁。
- 证据形状：**这是 N 类子句被正确行为化的范本** —— 把「覆盖主要用法」翻译成「子命令清单双向对账 + 生成物 diff-gate」。

**2. getting started 可零依赖跑通** — 部分
- 依据：`docs/user-guide/getting-started.md` 4 步（build / cp config / TUI / headless），全程 `--fake-model`，并显式提示 `${VAR}` 展开会触发 raw-literal 校验、给了最小 config 替代方案 —— 内容质量高。门禁只覆盖 1/4~2/4：`docs.yml` 的「Headless smoke」步骤覆盖第 4 步；`ci.yml` 的 build job 覆盖第 1 步 + `yanshi -h`。**第 2、3 步（`cp config.example.yaml config.yaml` 后能否加载、TUI 能否启动）无门禁** —— 而文档自己承认第 2 步在有 `OPENAI_API_KEY` 环境时会失败。
- 证据形状：一条测试**逐字执行** getting-started 的命令序列（`cp config.example.yaml` → `bootstrap.Build` 断言成功），以及 `timeout 5 ./yanshi --fake-model -inprocess` 的启动自检。**骗过去**：只跑最容易的那一步（现状）。

**3. 与实际不漂移** — 部分（**实测已漂移一处**）
- 依据：已有门禁都是真的 —— `docs.yml` 的生成快照 diff-gate（configuration.md / tui.md / entrypoints.md）、`cmd/gendocs::TestConfigSkeletonFieldsMatchStruct`、`TestSubcommandListMatchesDispatch`、`TestRenderConfigSkeletonCoversExampleYAMLKeys`。**实测漂移**：README 的「Quick start」段写「type a message and press **Ctrl+J (Ctrl+Enter) to send**」，而 `docs/user-guide/getting-started.md` 写「**Enter 发送**，**Ctrl+Enter 换行**」，CLAUDE.md 与 `internal/cli/tui::TestModel_CtrlEnterInsertsNewline` 证实后者为真 —— **README 的按键说明是错的**，且它不在任何 diff-gate 的覆盖范围内（`docs.yml` 只 diff `docs/api/` 与 `docs/user-guide/`）。
- 证据形状：手写散文里的行为主张要么被生成器接管（把按键表从 `internal/keymap` 生成），要么加一条 grep 式一致性测试（README 与 getting-started 对同一按键的描述必须一致）。**骗过去**：只 diff-gate 生成区块 —— **漂移恰恰发生在生成区块之外的散文里**。

---

## 无工作包 — 已删除项

### D2/O12 — IDE 扩展（VS Code） · 台账 `removed`

> ⚠️ **本节的字面量被刻意打断（现已不再必需，保留作现场证据）。** `internal/archtest::TestVSCodeExtensionNotAdvertisedInDocs` 会扫描全仓 `*.md`，**包括本文件**；本文档初稿逐字复制了那两条被删路径与一句示例，提交前直接把门禁跑红了。为保留可读性又不触发扫描，下文在若干字面量中间插入了空 HTML 注释 `<!---->` —— 它在渲染后不可见，因此**渲染出来的文本与台账 acceptance 逐字一致**，但原始字节不再匹配那组正则。后来加宽正则时，本文件下方那张候选表里的写法**正是被加宽的那几类**，于是本文件被登记进 `d2HistoricalDocs` 并在文首带上墓碑 —— 转义从那一刻起变成冗余，但它是这道门禁真的会红的现场证据，删掉就少一份物证。

> acceptance：ide/vs<!---->code/ 与 scripts/check<!---->-d2.sh 不存在；文档无对其作为交付物的描述

**1. `ide/vs<!---->code/` 与 `scripts/check<!---->-d2.sh` 不存在** — 已兑现
- 依据：两条路径均已删除（`ls ide` → No such file；`scripts/` 只剩 `bench.sh`）。`internal/archtest::TestVSCodeExtensionRemoved`（`removal_test.go`）直接 `os.Stat` 两条路径，回归即红。
- 证据形状：路径存在性断言（现状即是）。已知边界：只钉这两条**字面路径**，改名重来（`ide/vs<!---->code-ext/`、`extensions/vs<!---->code/`）能绕过 —— 但 acceptance 本身就是逐字点名这两条路径，测试忠实于措辞。

**2. 文档无对其作为交付物的描述** — 已兑现（**曾因召回漏洞退回 `partial`，漏洞已按下表逐条关闭**）
- 依据：`removal_test.go` 的 `d2Mentions` + `d2HistoricalDocs` + `d2Tombstone`。`TestVSCodeExtensionNotAdvertisedInDocs`（`::TestVSCodeExtensionNotAdvertisedInDocs`）**是真证据，且结构上是这条子句唯一能被机器判的形态**：活文档零提及 / 档案文档必须带墓碑 / 死豁免判失败 —— 三个方向都实现了，当前 PASS。
- **召回率实测**（同一组候选句子，逐条拿正则跑；「曾」列是加宽前的结果）：

  | 候选表述 | 曾 | 现 |
  |---|---|---|
  | `yanshi ships a VS<!----> Code extension.`（缩写紧邻 extension） | HIT | HIT |
  | `yanshi ships a Visual Studio Code extension.` | MISS | **HIT** |
  | `yanshi 提供 Visual Studio Code 扩展` | MISS | **HIT** |
  | `IDE extension (VS Code) is available.` | MISS | **HIT** |
  | `IDE 扩展（VS Code）` | MISS | **HIT** |
  | `yanshi.vsix is published on each release.` | MISS | **HIT** |
  | `Run \`code --install-extension yanshi.vsix\`` | MISS | **HIT** |
  | `Third front end: the VS Code integration.` | MISS | MISS（有意） |
  | `Install the Yanshi extension from the Visual Studio Marketplace.` | MISS | MISS（有意） |

  关闭方式：产品名抽成 `d2Product`（缩写 + 官方全称，四条短语模式共用），补一条「名词 + 括号里的产品名」的倒装模式（中英文括号都认 —— **这正是本条目自己 title 的形状**，照抄标题即可绕过，是三个洞里最该关的），再补一条打包制品 token 的裸匹配。加宽后全仓 `*.md` 重扫，命中集合与 `d2HistoricalDocs` 恰好一一对应，零活文档误报。
  末两行仍 MISS 是**有意的精度取舍**，写在 `d2Mentions` 注释里：不把产品名与名词相邻放的描述句、只提商店不提产品名的句子，一旦收进来就会误伤无关编辑器笔记，而会误伤的门禁活不长。
- ⚠️ **扫描范围漏洞（未关闭）**：只扫 `*.md`（`::TestVSCodeExtensionNotAdvertisedInDocs`）；`docs/archive/` 整目录跳过（`::TestVSCodeExtensionNotAdvertisedInDocs`）。因此 `docs/feature-status.yaml`（含该字样）、Go doc 注释、`sdk/*/package.json`、`--help` 文本、`.github/` 都在射程外。这是**范围**而非**召回**问题：acceptance 说的是「文档」，`*.md` 是文档的主体，扩范围会把台账与测试源码自身卷进来，需要另一套豁免。
- 另注：`docs/commit-convention.md` 与 `docs/superpowers/plans/2026-07-22-h1-release-engineering.md` 在**活文档**里把 `ide-vscode` 列为合法 commit scope（就近搜该字符串）。测试注释把这个明确列为有意放行的精度取舍，也是反向探针的固定一条。
- 证据形状：正则组 + 档案豁免 + 墓碑三件套（现状即是），配一次全仓重扫做零误报核对。**骗过去**：任何不把产品名与名词相邻放的句子 —— 这条边界是显式声明的，不是遗漏。

---

## 汇总：分布、失真、N 类子句

### 状态分布（230 子句）

| 工作包 | 条目 | 子句 | 已兑现 | 部分 | 未兑现 | 未查证 |
|---|---:|---:|---:|---:|---:|---:|
| W1 | 9 | 37 | 14 | 14 | 8 | 1 |
| W2 | 4 | 13 | 5 | 6 | 2 | 0 |
| W3 | 5 | 21 | 2 | 12 | 7 | 0 |
| W4 | 2 | 6 | 4 | 0 | 2 | 0 |
| W5 | 4 | 14 | 0 | 9 | 5 | 0 |
| W6 | 11 | 43 | 10 | 15 | 16 | 2 |
| W7 | 7 | 26 | 9 | 11 | 6 | 0 |
| W8 | 8 | 29 | 9 | 11 | 9 | 0 |
| W9 | 5 | 16 | 3 | 10 | 3 | 0 |
| W10 | 7 | 23 | 5 | 15 | 3 | 0 |
| —（D2/O12） | 1 | 2 | 2 | 0 | 0 | 0 |
| **合计** | **63** | **230** | **63** | **103** | **61** | **3** |

**读法**：63 条子句（27%）有真正驱动它的可执行断言；103 条（45%）实现存在但关键分支无测试或只覆盖外延的一部分；61 条（27%）无实现或实现是残桩；3 条（W1 的 G/VISION-TOOL#3、W6 的 M1/SPEC-TOOLIF#4·#5）本轮未查证，**翻牌前必须补查**。M1/SPEC-TOOLIF#3 曾也列在这里，**它已经查证过**（正文附了两条可重跑命令）—— 缺的是防回流门禁不是复核，按上面「状态三档」那条 ⚠️ 归入「部分」。

**W5 全 0 已兑现**是本表最刺眼的一格：安全维度 14 条子句里没有一条有真正驱动它的证据。W4 的 4/6 与 W1 的 14/37 是密度最高的两块。

---

### 发现的失真

#### 低报（台账比实际保守 —— 可直接补 evidence 或升级）

| ID | 子句 | 差在哪 |
|---|---|---|
| **B2/LSP1** | #2 server 缺失安全降级 | 已有真证据 `internal/tools::TestDiagnosticsLSPUnavailableIsLocalDegradation`，台账 evidence 为空 |
| **B3/T11** | #3 重定向受策略约束 | 台账记「无」。实际 `netpolicy.NewTransport` → `PolicyDialer` 每次拨号做 `CheckHost` + `CheckResolvedIPs` + IP 钉死，跨 host 重定向在**拨号层**已被拒。缺的是 `CheckRedirect`、跳数上限与测试，不是「完全无约束」 |
| **B3/T11** | #4 后端不可用降级 | 台账挂的 `TestWebSearchReturnsEmptyOnUnreachable` 是恒真空壳；真证据是 `TestCov_WebRunSearchNetworkDegradation`（断言 `"results":[]`）。**引用应换掉** |
| **A1/S07** | #1 的持久化半边 | 持久化是**真的**（KV 落盘 + 重启重载 + bootstrap 接真 store），不是「会话内存 remember」；查看/撤销的 WS + TUI 通路也真实存在 |
| **A2/DT1** | #3 thread/turn 关联 | 注入链完整且有断言（`orchestrator.go::Orchestrator.withTurnContext` → `task.go::TaskTools.runCreate` → 落库），不该按「未做」记 |
| **B1/M04** | #2 的深度+并发半边、#4 resume | `TestAcceptance_DepthAndUsageQueryable` + `TestAcceptance_WorkflowUsesSharedLimitAndList` 是本次审查里质量最高的两条；#4 的 `TestAgentResumeModelMismatch` 是**带断言的**真两阶段重启测试 |
| **F2/LEAK2** | #2 满则拒绝 | 三条 `ErrorAs(&SpawnErrCap)` + Cap 值断言，干净兑现，可直接挂 evidence |
| **D1/V14** | #1 start/resume/interrupt + 流式 item | 真断言齐全；且旧 note 里「`TurnStartParams.Images` 是死字段」**已过时**（`service.go::Service.runTurn` 现在真填 `TurnOpts`，有端到端断言） |
| **C3/E03** | 整条 | #2/#4 已兑现（真恶意 fixture 全生命周期 + 唯一 marker 穿透 e2e），#1 只差 WS 层 enable happy-path，#3 只差为已存在的诊断分支补测试 —— 比 `partial` 应有的状态好得多 |
| **C2/UX8** | #3 | 生产端对称证据 `internal/agent/orchestrator::TestClassifyEvents_NoReasoningEmitsNoThinking` 存在且有效但未被引用，补上即两端齐全 |
| **B3/V13** | #1 | 仍未兑现，但 `git_diff` 已实现三种 scope（`git.go::NewGitTools`），**接线成本远低于台账注释给人的印象** |

#### 虚报（台账比实际乐观 —— 含终态站不住的）

**A. 终态条目里站不住的子句（最高优先级）**

| ID | 当前 | 子句 | 问题 |
|---|---|---|---|
| **C1/AU1** | `done`→已退回 `partial` | #3 生命周期可控 | update/pause/resume/delete 四步只有裸 `require.NoError`，而 `GuardedTool` 把拒绝写进 **result 串**不写进 error → 这四步对「操作没生效」完全是盲的 |
| **C1/AU1** | `done`→已退回 `partial` | #4 持久化 | 无跨重启（重开 DB 文件）断言，台账自记为「已知弱点」。按与 A1/T07/T08 / B1/M04b 相同的尺子应回退 `partial` |
| **B3/DT5** | `done`→已退回 `partial` | #1 一次调用聚合 | **LSP 维度在生产里是死的** —— `defaultFileLister.recentFiles` 恒返回 nil，`bootstrap.go::Build` 传 nil → 生产 `open_diagnostics_count` **恒为 0**；测试靠注入生产不存在的 `diagTestProbe` 才拿到非零。占位实现顶着终态 |
| **E2/PROP1** | `done` | #1·#2 的部分引用 | **`done` 本身站得住**（掏空 `EnforceToolCallPairs` 三个测试全 FAIL），但 evidence 里 `TestProperty_RunReducesTokens`（引 2 次）与 `TestProperty_EachSummaryCallWithinWindow`（引 1 次）共 **3 处引用是空心的** |

**B. 非终态条目里的实现缺口（不是测试缺口 —— 补测试无用）**

| ID | 子句 | 缺口 |
|---|---|---|
| **B0/TD1 #5 / M1/G02 #2** | budget 生产失效 | `Budget.MaxTokens` **无任何 flag/config 入口**，全部生产构造只设 `MaxIterations` → `overBudget()` 恒 false。累计器建好了，闸门没接线 |
| **F2/LEAK2 #1·#3 / B1/M04b #2 / C1/M07 #2 / B1/M04 #3** | 槽位泄漏 | `finishTerminal` 不摘 runtime、不 cancel child ctx → `runningLocked()` 单调不减，「并发上限」= 终身预算；叠加 `spawnWithRetry` 无限重试会**永久挂起** |
| **A2/DT1 #2** | 状态机不转 | `work.Manager.Start`/`Finish` **零生产调用点**，broker 不回写 `task_work` → 生产中 durable task 永远停在 `pending` |
| **A2/DT2 #3** | 外键失效 | `PRAGMA foreign_keys` 从未开启 → 四张 `task_work_*` 表的 FK/CASCADE 全部不生效；gate 不校验 task_id。且 **FakeManager 比真实 Store 更严格**，制造反向安全假象 |
| **A2/G05 #2** | 帧到不了用户 | `TurnOpts.EmitWorkFrame` **零生产赋值点** + TUI `applyEvent` 无 `plan_update`/`checklist_update` 分支 |
| **G/VISION #3** | 静默丢图 | `ApplyImages` 完全不看 visionAux，`appendPlaceholders` 有**四条静默路径、零 error 出口** |
| **G/VISION-TOOL #4** | 只写不读 | `visionUsageAccumulator` 无任何读取方 → 辅助模型费用进不了 `/cost` |
| **B1/M04 #2 的 usage** | 消费者缺失 | `tools.UsageSinkFrom` 零生产调用点 → `agent_list`/`agent_result` 的 Usage 恒为 0。⚠️ **GOV6 盲区形状**：注入器有调用点（GOV6 绿），消费者没有 |
| **C4/OBS2 #2** | 未接线 | `otelobs.RecordUsage`（token 计数器）与 `StartSession` 零生产调用点 |
| **C4/OBS3 #3** | 幻影 flag + 一次性求值 | 3 个 flag 里 2 个注册后从不被读；唯一真门是 boot-time 求值，`/features` 运行时切换**对系统零影响** —— 与 `bootstrap.go::Build` 的注释直接矛盾 |
| **D1/APS1 #2 / D2/V15 #2** | 伪生成器 | `cmd/api-schema` 输出是硬编码 TS 字面量，`_ = v1.SchemaBytes()` 丢弃返回值却自称防漂移；Python 侧自述手工镜像，生成器从未被调用 |
| **D3/C15 #1·#2 / D3/I18N1 #1** | 接线断点 | `internal/cli/tui` 不导入 `internal/keymap`；`newModel` 写死 theme/bundle；`preferences.go` 四级合并四个函数**生产调用者全为 0** |
| **C2/UX3 #3** | 诊断被丢弃 | `multimodal.go::Orchestrator.expandPathRefs` 把 `res.Rejected` 整个丢掉 → 越权/超大拒绝对用户完全不可见 |
| **A1/S09 全条** | 标题错配 | 生产里子进程只拿到一个**不 consult 任何策略字段**的死端口 env；真正被强制的 netpolicy 全在进程内 HTTP。port 维度在数据结构上就不存在 |
| **A1/S06 全条** | 出厂不执行 | 非测试代码零处构造 `ShellPerm.Rules` → execpolicy 出厂配置下**整条链不执行**；其独有能力（管道/重定向）即便启用也永远被上游元字符 HardDeny 挡住 |

**C. 实测确认的真 bug（不是记账问题）**

> **读法**：本列表按发现时刻记账，条目**修好后就地标注而不删除** —— 删掉会让「这条是否被处理过」变得不可查。反过来，**已修的条目继续用现在时挂着同样是虚报**：读者会重做已完成的工作，或据此低估当前安全态。第 1 条曾经就是这个形状（它在修复提交 `03a6bb3` 落地后仍写着「已实测复现」）。改状态时同步改上面对应子句那一段。
>
> **复测状态（2026-08-04 逐条实测，逐条列出，不再用「除第 N 条外」这种概括）**：第 1 条已修（`03a6bb3`）；第 7 条**部分已修**（重复 `/features` 条目由 `cf088f7` 删除，快捷键描述错配仍在）；第 2、3、4、5、6、8 条**仍然成立**。
>
> ⚠️ **上一版这行横幅写的是「除第 1 条外，以下 7 条…仍然成立」，而第 7 条的后半句在同一天被 `cf088f7` 修掉了**——那次提交删了行、没更新描述它的这两处文档。这正是本段开头那条规则（「已修的条目继续用现在时挂着同样是虚报」）说的形状，而违反者就是当天那次修复提交本身。**「除 X 外全部成立」这种概括横幅是这个失败模式的温床**：它把 N 条状态压成一个数字，任何一条状态变化都不会在文本上留下痕迹。改用逐条列举。
>
> 复测所用命令：第 7 条 `awk '/^var commandTable/,/^}/' internal/cli/tui/commands.go | grep -oE 'name: "[a-z-]+"' | sort | uniq -d`（零输出）与 `grep -n 'Ctrl+E' internal/cli/tui/commands.go`（`895:{"Ctrl+E", "toggle history view"}` 仍在）；第 6 条 `go test ./internal/agent/orchestrator -run '^$' -bench '^BenchmarkOrchestratorTurn$' -benchtime=3x`（`--- FAIL: orchestrator: no assistant message produced`）；第 8 条 `grep -n 'Ctrl+J' README.md`（`71:…press Ctrl+J (Ctrl+Enter)` 仍在）。

1. ~~**`git_diff` 参数注入 → 沙箱外写文件**（B3/W07#4）~~ — ✅ **已修（`03a6bb3`，W1 范围内）**。原缺陷：`validateGitRef` 不拒 `-` 开头的 ref，`commit` scope 原样透传 → `scope.ref="--output=<path>"` 让声明为 `sandbox.ReadOnly` 的工具在工作根外写出文件。现状：`argvsafe.go::validateArgvOperand` 拒绝一切 `-` 开头操作数，且每条 git argv 在 ref 前带 `git.go::gitEndOfOptions` 哨兵；证据见 `argvsafe_test.go::TestGitDiffRefCannotWriteFilesOutsideWorkRoot` 与 `::TestGitDiffPassesEndOfOptionsBeforeRef`，反向探针 `::TestGitDiffAcceptsRefsContainingDashes`。详见 B3/W07 第 4 句那一段。
2. **`parseGitStatusZ` 两个解析 bug**（B3/W07#1）：已跟踪且含空格的路径被截断；rename 的 origPath 含 ≥2 空格时**伪造出不存在的 status 条目**。
3. **`parseGoJSON` 计数错误**（B3/DT4#1）：不过滤包级事件 → `Passed` 虚增；失败列表混入空 `Test` 幻影条目。全部 `"Action":"pass"` fixture 系统性回避了这个输入（处数现算，见 B3/DT4 第 1 句那一段）。
4. **`--file` 绕过 `cfg.Input`**（D1/V12#1）：`--input jsonl --file <3 行>` 实测只跑 **1** 个 turn（stdin 同文件跑 3 个）。CI 跑的正是这条命令但输出丢 `/dev/null`；`examples/headless-batch/run.sh` 还用 `grep -c` **谎报** "processed 3 prompts"。
5. **`item.toolName` AttributeError**（H2/APIREF1#2 / H2/EX1#2）：pydantic 字段是 `tool_name`，`toolName` 只是 alias。首条 item 无 text 故必然触发。CI 的 `py_compile` 吞掉它。
6. **`BenchmarkOrchestratorTurn` 在 `b.N ≥ 2` 下必挂**（F2/BENCH1#1）：FakeModel 只脚本 1 条响应且未设 `Repeat`。失败被 `nightly.yml` 缺 `pipefail` + `continue-on-error` **吞掉两层**。
7. **`/help` 快捷键表已与实际绑定不符**（C2/UX2#3）—— **部分已修**：
   - 仍然成立：`Ctrl+K`/`Ctrl+S` 描述错误，`Ctrl+E` 根本不存在，漏 Ctrl+V/F1/Alt+R（2026-08-04 复测 `internal/cli/tui/commands.go::newCmdHelpEntry` 里那张快捷键字面量表，三行原样还在）。
   - ~~`commands.go::commandTable` 还有逐字重复的 `/features` 条目~~ — ✅ **已修（`cf088f7`）**，`uniq -d` 复测零输出。防重复门禁仍欠着（见 C2/UX2 #3 那一段）。
8. **README 按键说明是错的**（H2/UDOC1#3）：README 的「Quick start」段写「Ctrl+J (Ctrl+Enter) to send」，实际是 Enter 发送、Ctrl+Enter 换行。它在所有 diff-gate 覆盖范围之外。

---

### N 类子句（测量不是断言 / 质量主张 / 把证据当验收）—— 需单独决策

这些子句**不能靠找现有测试顶上**。它们要么需要补一道**门禁**把测量变成断言，要么需要**改写 acceptance**（改 acceptance 必须同步改 `internal/archtest/acceptance_pin_test.go` 的 pin —— 那是范围决策，不是台账对账）。

**(a) 纯测量 —— 一次数字，无门禁**

| 子句 | 现状 | 建议 |
|---|---|---|
| **E1/COV2#1** 覆盖率 ≥80% | 实测 **100.0%**，达标；全仓无覆盖率门禁 | 倾向**改写 acceptance**。补门禁需子进程嵌套跑 `go test -cover` 并在 -race 三平台稳定 |
| **E1/COV3#1** 覆盖率 ≥50% | 实测 **94.1%**，达标；同上 | **改写 acceptance**。bootstrap 本身跑 16.8s，嵌套再跑一遍 × 3 平台代价不成比例 |
| **F1/WAL1#3** 性能不退化 | `internal/store` 下**零个 Benchmark** | 补基准 + 基线比较，或改写为可断言形态 |
| **F2/BENCH1#2** CI 记录趋势 | 无 baseline 文件、从不调用 benchstat、从不下载上次 artifact | **补门禁**：「上次的数 + 这次的数 + 差值落盘」这条链本身必须存在 |
| **F2/BENCH1#3** 大回归可发现 | `THRESHOLD_PCT=20` 自标 informational 且从不执行；全仓零性能预算断言 | **补门禁**（benchstat 超阈值失败），或改写为 `AllocsPerRun` / 时长上限的普通测试 |
| **C1/RLM1#4** 成本显著低于 sub-agent | `cost_class=cheap` 是**声明**便宜，不是实测 | **改写子句**为可断言形式（如「LLM 调用次数 / prompt token 总量严格小于同等任务经 agent_spawn 的量」） |

**(b) 文档 / 质量主张 —— 没有可观察量**

| 子句 | 现状 | 建议 |
|---|---|---|
| **F2/CCL1#3** keepRecent 文档清晰 | 唯一沾边的测试是恒真空壳（`4/2>=2`） | **改写为行为主张**：断言 `maybeCompact` 传给 `ctxcompact.Plan` 的 `PlanOpts.KeepRecent` 恰为 `KeepRecent/2`，使两处语义漂移可被机器检出 |
| **F2/LEAK2#4** 与深度上限交互文档化 | 注释齐全但深度上限**三处各写一遍**、registry 硬编码 `3` | **改写为行为主张** + 跨包常量对账测试 |
| **H2/CONTRIB1#3** docs 结构清晰 | 不可断言 | **改写为**「链接零断裂 + 归档带墓碑 + 专题目录有索引」，前两项 `docs.yml` 现成门禁已顶 |
| **H2/EX1#3** 覆盖主要集成点 | 「主要」无分母 | **改写为枚举式清单** |
| **C4/OBS1#4** 采样不丢关键错误 | **无实现**（全仓唯一 "sample" 是 OTel 的，与日志无关） | 只能靠补实现或改写 acceptance —— 补测试无效 |
| **A1/S09#2** host/port 规则生效 | port 维度**在数据结构上不存在**，且被 `TestCheckHost_StripsPort` 固化为「忽略端口」 | 需改实现（加 Port 维度）或改 acceptance |
| **M1/O07#1** 含 sandbox | sandbox 是诚实占位（恒 warn，S08 才做） | S08 落地前只能**改写 acceptance**（把 sandbox 排除） |

**(c) 把证据当验收 —— acceptance 文本本身有缺陷**

| 子句 | 问题 |
|---|---|
| **B0/TD1#4** `TestLoop_BudgetStopsOnAccumulatedUsage 等测试守护'` | **把具体测试名写进验收**：任何人只要保留同名测试壳就能"满足"它 |
| **B0/TD1#1** 前半段 | 「路线图未给 B0 独立验收标准」是**元陈述**，不是可断言的行为 |
| **A3/V16#3** 启动超时/重连/权限检查**有测试** | 字面要求「有测试」而非「行为成立」—— 同族缺陷，但危害小于 B0/TD1#4（它至少不指名具体测试） |
| ~~**F1/WAL1#5**~~ **已修复** | 原文以字面省略号 `"Windows -r…"` 结尾 → **第 10 条细化验收读不到**，无法对账。这是 GOV8 的一个洞：读不到的子句既不能兑现也不能证伪。已按 spec §12 第 9、10 条补回原文并同步 `acceptancePins["F1/WAL1"]` |

**(d) 已被正确行为化的范本（供改写时参照）**

- **H2/UDOC1#1「覆盖主要用法」** → `cmd/gendocs::TestSubcommandListMatchesDispatch`（双向：`main.go` 的 case ↔ 清单，`reflect.DeepEqual` 钉死）+ `-help-all` diff-gate。质量主张被翻译成了可执行的双向对账。
- **D2/O12#2「文档无描述」** → `internal/archtest::TestVSCodeExtensionNotAdvertisedInDocs`（活文档零提及 / 档案强制墓碑 / 死豁免失败）。文档主张被翻译成三方向扫描。
- **H1/VER1#3「发布流程文档化」** → runbook 引用的每个可执行步骤都有对应测试或 workflow。
- **B3/DT5#3「toolchain 版本准确」** → fake 对**错误 argv 返回 exit 2**，堵住了 fake-太宽。这是本仓 fake 设计的范本。

---

### acceptance / pin 处置建议

拆解本身不改 acceptance 文本。以下是发现的**客观缺陷**及处置：

1. **F1/WAL1 —— 已修复。** 子句 5 以字面省略号截断，第 10 条细化验收在台账里不可读。已按 `docs/superpowers/specs/2026-07-22-f1-sqlite-wal-design.md` §12 第 9、10 条补回原文（「Windows CI 下并发/升级测试全绿」「doctor 报告 journal_mode 与 -wal/-shm 大小」），并同步 `acceptancePins["F1/WAL1"]`（子句数不变仍为 5，Digest 更新）。**先修这条的理由**：截断是**可读性**缺陷而非措辞缺陷 —— 读不到的子句既不能兑现也不能证伪，它会永远占一个承诺位；补回原文是纯还原，不改变任何一条承诺的内容，因此不需要工作包 owner 拍板。
2. **B0/TD1 —— 暂不修，留给 W2。** acceptance 是一段实测表引文被分号切碎的产物：子句 1 前半是元陈述、子句 4 把测试名当验收、引号跨子句 1→4 不闭合。但**五条子句在文件里全都读得到**，缺陷是形状而非可读性；把它重写成行为陈述会实质改变 W2 要交付什么（例如子句 4 从「有某某测试」改成「预算超限时循环真的停」是**加重**承诺）。这是范围决策，应由 W2 在动手时连同 `acceptancePins["B0/TD1"]` 一起改，而不是夹带在一次台账退回里悄悄完成。
3. 上表 (a)(b) 里标注「改写 acceptance」的各条，每改一条都要同步改对应的 pin 行。

**改 pin 的正确做法**：`internal/archtest/acceptance_pin_test.go` 的 `TestLedgerAcceptanceIsPinned` 在失配时会直接打印新的行（`fmt.Sprintf("%q: {Clauses: %d, Digest: %q},", …)`），把它粘回表里即可 —— 但**必须在同一个 diff 里**，让 `5 -> 1` 这类变化对评审可见。

---
