# Tier E — Batch E2：fuzz / property / race 设计

> **日期**：2026-07-22
> **归属**：E-H roadmap 的 Tier E，Batch E2（`docs/feature-roadmap-e-h.md` §4）
> **命题**：给 A-D 引入的安全与压缩核心补**机器保证**——`guard.MatchGlob` 的 fuzz、`ctxcompact` 的属性测试、`-race` 的 CI 固化。三者都不加新功能面，只把"不 panic / 不丢不变量 / 不竞态"变成可回归、可在 CI 持续运行的检查。
> **范围**：只做 FUZ1 / PROP1 / RAC1 三项；发现的既有生产 bug **修在 F2**（LEAK 系列），本批只产出测试 + 登记。
> **状态**：设计稿，待用户审阅 → writing-plans。

---

## 1. 目标与非目标

### 目标

- **[FUZ1]** `guard.MatchGlob` 有 `go test -fuzz` 目标；种子语料覆盖 IFS / glob 注入 / `../` / 嵌套通配 / 超长串；断言**任意输入不 panic** + 匹配结果与 `glob.go` 文档语义一致；种子语料入仓，CI 跑语料、本地跑长 fuzz。
- **[PROP1]** `ctxcompact` 有 ≥3 个属性测试（不变量）：pin 集 ⊆ 压缩后历史、tool_call/tool_result 配对不被切断（`EnforceToolCallPairs` fixpoint 成立）、每次 summary 模型调用的输入严格 ≤ 窗口；用随机历史生成器（混合 role / 长度 / 工具对 / orphan）。
- **[RAC1]** `go test -race` 入 CI 并分层（PR 变更包 / merge 全量 / nightly）；补并发热点测试（`wsConn.write`、`runners sync.Map`、task broker Claim/RequeueStale、registry Manager、`permTracker`）；暴露的既有竞态与泄漏**登记到 F2**。

### 非目标（本批不做）

- **不修既有生产竞态/泄漏**——那是 F2（LEAK1 `createdWT`、LEAK2 子代理并发上限）的职责。E2 只发现 + 登记。
- 不引入 mock 框架（沿用 Fake 优先：`einollm.FakeModel` / 自写 fake summarizer / `FakeAgent`）。
- 不做性能基准（BENCH1，F2）；不做架构治理 CI（GOV 系列，E3）。
- 不改 `guard.MatchGlob` / `ctxcompact.Run` 的**行为**——若 fuzz/属性发现语义 bug，先登记，行为修复另开（见各条风险）。
- 不把 `-race` 在 day 1 就设为 PR 阻塞门禁（先 nightly/merge，待既有竞态清理后再升格，见 §7、§9）。

---

## 2. 背景

- 仓库**现状实测**（2026-07-22）：无 `func Fuzz` / `testing.F` / `testing/quick` / `-race` 的任何使用；`t.Parallel()` 已在 `acp`、`agent/goalloop` 等大量使用——并发执行已在发生，但从未上过 race detector，属"裸奔"状态。
- **无 CI**：`.github/workflows/` 不存在（CIG1/G1 才建 CI 矩阵）。RAC1 与 CIG1 有交接：RAC1 产出 `-race` job 的**内容与命令**，CIG1 产出 workflow 文件壳子。谁先落地见 open question Q1。
- **synthesis-final.md 最弱维度**：正确性 7 / 测试 7。E2 直接打这两维：fuzz + 属性给"正确性"以机器保证，`-race` 给"并发正确性"以机器保证。
- **`guard.MatchGlob`**（`internal/guard/glob.go:26`）是 tools/fs/shell/net 四维白名单的核心匹配器，`guard.Check` 每次 per-pattern 调用；其语义有若干反直觉点（trailing `*` 编译为 `.*` 会跨 `/`，见 `glob.go:18-22` 的 over-grant WARNING），正是 fuzz 最该钉死的地方。
- **`ctxcompact`**（`internal/ctxcompact/`）是上下文压缩统一核心，mid-turn 与 pre-turn 两路都委托它（CLAUDE.md）；其不变量（不切断工具对、summary 不超窗口、不产生空 summary）目前只有手写用例，无随机/属性保证。
- **`-race` 反哺 F2**（roadmap §1.4）：race detector 固化后"大概率暴露 `createdWT`/子代理并发的既有竞态，直接喂给 F2"。本 spec 在 §6/§7 把这条衔接写实。

---

## 3. 总体设计

三条线，独立可并行，但共享一个原则：**测试本身必须 deterministic 且自身无竞态**。

| 线 | 手段 | 钉死的不变量 | 产物 |
|---|---|---|---|
| FUZ1 | `go test -fuzz`（语料驱动） | 任意字节 pattern/name 不 panic；已知绕过有回归 | `FuzzMatchGlob` + `testdata/fuzz/` corpus |
| PROP1 | 属性测试（随机历史 + 不变量断言） | pin ⊆ out、工具对不被切断、summary 输入 ≤ 窗口、不空 summary、summary-of-summary 短路 | `*_property_test.go` + seeded 生成器 |
| RAC1 | `go test -race` + 并发热点测试 + CI 分层 | 并发热点无 data race；既有竞态有登记 | 并发测试文件 + CI job 片段 + F2 登记 |

**确定性约定**（三条共用）：
- 随机源一律 `math/rand/v2` + **固定种子**（写入测试常量），失败可复现；不在测试里用全局时间或 `rand` 全局源。
- 模型一律 fake（`einollm.FakeModel` 或自写 `fakeSummarizer` 记录每次调用输入），不触网、不耗 token、无 flaky。
- 不依赖 OS 状态（不读真实文件系统、不 dial 真实端口）；WS 测试用 `httptest` + in-process dial。

---

## 4. [FUZ1] `guard.MatchGlob` fuzz  (P1 | 缺失 | synthesis A24/R16)

- **缺口**：glob 匹配（四维白名单核心）无 fuzz；边缘（嵌套 `**`、转义、超长、`../`、重复通配、混合通配如 `**/*/**`）无覆盖。手写表驱动（`internal/guard/glob_test.go:9` 的 `TestMatchGlob` + `:40` 的 trailing-`*` 钉死用例）只覆盖"已想到的"。

- **落点**：
  - `internal/guard/glob_test.go`（新增 `FuzzMatchGlob`，与现有表驱动同文件）。
  - `internal/guard/testdata/fuzz/FuzzMatchGlob/`（种子语料，入仓）。

- **设计**：
  1. **fuzz 目标签名**：`func FuzzMatchGlob(f *testing.F)`，输入 `(pattern string, name string)`。`f.Add(...)` 注入种子（见下）。
  2. **不变量 1（最强）——不 panic**：对任意 pattern/name，`MatchGlob` 必须 return（含 `(_, err)`），绝不 panic。这是 `defer recover` 之外由 fuzz 引擎保证的硬性质。
  3. **不变量 2——与文档语义一致**：对**无通配符**的 pattern，`MatchGlob(p, name) == (p == name, nil)`（纯字面匹配，因 `globToRegexp` 对字面字符只转义元字符）。
  4. **不变量 3——通配恒等**：`MatchGlob("**", x)` 对任意非含让 `regexp` 报错的 x 返回 `true`；`MatchGlob("*", x)` 在 x 不含 `/` 时返回 `true`。
  5. **不变量 4——自洽**：`globToRegexp` 产出的 regex 必然 `Compile` 成功（fuzz 里把 `err != nil` 也当发现，但目前实现里几乎不可能 compile 失败，除非输入含非法 UTF-8 / 非法字节——这恰是 fuzz 要钉的：非法字节不应让 `regexp.Compile` 返回 error 之外的 panic）。
  6. **种子语料**（`f.Add`，从已知绕过/反直觉点提炼）：
     - IFS / shell 元字符：`{"go *", "go build ./..."}`、`{"go *\t;", "go build"}`、`{"a|b", "a|b"}`（`|` 是 regex 元字符，须被转义，否则变成 alternation 绕过）。
     - glob 注入：`{"*.go", "dir/main.go"}`（`*` 不跨 `/`，须 false）、`{"**.go", "a/b.go"}`（`**` 跨 `/`）。
     - `../` 序列：`{"D:/code/**", "D:/code/../etc/passwd"}`、`{"a/..", "a/.."}`。
     - 嵌套/重复通配：`{"***", "a/b/c"}`、`{"**/**", "x"}`、`{"a**b**c", "aXYZbXYZc"}`、`{"*?", "ab"}`。
     - trailing `*` over-grant（钉死 `glob.go:18-22` 文档）：`{"D:/code/*", "D:/code/secret/deep/x.go"}`（须 true，防回归缩窄）。
     - 超长串：`{strings.Repeat("*", 1000), strings.Repeat("a", 10000)}`（钉 ReDoS / 超时）。
     - 空 / 边界：`{"", ""}`、`{"", "x"}`、`{"?", ""}`。
     - 非 ASCII / UTF-8 边界：`{"中文*", "中文测试"}`、`{"\xff\xfe*", "\xff\xfe"}`（非法 UTF-8——`regexp` 对此返回 error，fuzz 须断言 error 而非 panic）。
  7. **回归 corpus**：fuzz 发现的任何 crash / 语义偏差，用 `go test -fuzz=FuzzMatchGlob` 产出的 `testdata/fuzz/FuzzMatchGlob/<hash>` 文件入仓（这是 Go fuzz 的标准回归机制）。

- **依赖**：无。

- **风险与缓解**：
  - **fuzz 发现语义 bug**（如 `*` 跨 `/` 与文档不符、`|` 未转义致 alternation 绕过）→ **先入回归 corpus 钉死现状**，行为修复（若是安全问题）另开 issue，不在 E2 改 `glob.go`（避免 E2 越界改生产代码；安全修复走 guard 的专项）。spec 阶段预估：`|` 这类 regex 元字符在 `globToRegexp`（`glob.go:54`）的 `strings.IndexByte(\\.+()|[]{}^$, c)` 转义表里**已覆盖** `|`，故 alternation 绕过应不存在——fuzz 用来**证实**这点。
  - **ReDoS / 超时**（嵌套通配致 catastrophic backtracking）→ fuzz 目标加 `f.Fuzz` 内不设超时，但 CI 仅跑语料（`-fuzz=time`，见 §9）；本地长 fuzz 由人盯。若发现指数级回退，登记为性能 bug（归 BENCH1/E3 的范畴），E2 不修。
  - **每调用重编译 regex**：`MatchGlob` 每次都 `globToRegexp` → `regexp.Compile`（`glob.go:27-31`），guard.Check 对每 pattern 每维每调用都编译——是热路径性能债。**E2 只登记**（写入 F2/BENCH1 findings），不在本批加 regex 缓存（缓存是行为中性优化，但属性能范畴）。

- **验收**：
  - `FuzzMatchGlob` 存在且 `go test -run FuzzMatchGlob`（跑语料）通过、无 panic。
  - `testdata/fuzz/FuzzMatchGlob/` 至少含上述种子（每个 `f.Add` 对应一个文件或由 `go test -fuzz` 固化）。
  - 已知绕过（trailing-`*` over-grant、`*` 不跨 `/`、`|` 转义）有语料条目。
  - CI 跑语料（`go test -run FuzzMatchGlob ./internal/guard`）绿。
  - 本地 `go test -fuzz=FuzzMatchGlob -fuzz=30s ./internal/guard` 无 crash（人工验收，非 CI）。

- **预估**：1-2d。

---

## 5. [PROP1] `ctxcompact` 属性测试  (P1 | 缺失 | synthesis A16)

- **缺口**：压缩核心无属性测试，"压缩不丢 pin / 不切断工具对 / summary 不超窗口"只有手写用例（`run_test.go` / `plan_test.go` / `pairs_test.go` / `summarize_test.go`），无随机输入的机器保证。`EnforceToolCallPairs` 的 fixpoint（`pairs.go:18`）和 `RunSummary` 的窗口预算（`summarize.go:54`）是最该被随机化压力的两个算法。

- **落点**：
  - `internal/ctxcompact/run_property_test.go`（新；`Run` 端到端属性）。
  - `internal/ctxcompact/plan_property_test.go`（新；`Plan` + `EnforceToolCallPairs` 属性）。
  - `internal/ctxcompact/summarize_property_test.go`（新；`RunSummary` 窗口属性）。
  - `internal/ctxcompact/gen_test.go`（新；随机历史生成器，供三处复用——遵循"重复逻辑抽公共函数"约定）。

- **设计**：

  ### 5.1 随机历史生成器（`gen_test.go`）

  ```go
  // genHistory 用固定种子生成一条混合历史：user/assistant/tool 角色、
  // 随机内容长度、配对与 orphan 的 tool_call/tool_result、偶发 working-set
  // 路径 / error 标记 / diff 标记 / 尾部 summary sentinel。返回 (msgs, seed)
  // 以便失败时打印种子复现。
  func genHistory(rng *rand.Rand, n int) []*schema.Message
  ```
  - **角色混合**：按概率分布选 user（含 `isUserOriginal` 命中与 tool-result-as-user 两种）/ assistant（带随机数量 ToolCall）/ tool（带 ToolCallID）。
  - **工具对**：以可调概率生成"配对"（assistant 带 ToolCall[i]、后续 tool 带 ToolCallID==i）与"orphan"（只有 call 无 result，或只有 result 无 call）——专门压力 `EnforceToolCallPairs` 的 orphan 移除分支（`pairs.go:58-67`）。
  - **标记注入**：随机消息里掺 working-set 路径（`D:/code/foo.go`）、error 标记、diff 标记，覆盖 Plan 的 pin 规则 3/4/5（`plan.go:67`）。
  - **尾部 summary**：部分用例尾部放一个 `IsSummaryMessage` 命中的消息，覆盖 bug⑦ 短路（`plan.go:27`）。
  - **长度与窗口**：生成器接受目标 token 规模，造出"需要分块"的长历史（触发 `RunSummary` carry 路径，`summarize.go:70`）与"单次即可"的短历史（触发 single 路径，`summarize.go:62`）。

  ### 5.2 属性（≥3 个核心 + 2 个加固）

  **属性 P1 —— pin 集 ⊆ 压缩后历史（保序、同_identity）**：
  对随机 `msgs`，`plan := Plan(msgs, opts)`，`out := Assemble(msgs, plan, summary)`：
  - `out` 前 `len(plan.PinnedIndices)` 条与 `msgs[plan.PinnedIndices[i]]` **指针相等**（`Assemble` 直接 append 原指针，`assemble.go:13-16`）。
  - pin 集顺序与原历史顺序一致（`PinnedIndices` 升序，`plan.go:76`）。
  - 即 pin 的原文一字不丢（codex 风格"user intent never lost"的机器化）。

  **属性 P2 —— tool_call/tool_result 配对不被切断（`EnforceToolCallPairs` fixpoint 成立）**：
  对随机 `msgs`，`Plan` 后取 `pinned := plan.PinnedIndices`，构造 pinned 子历史，断言：
  - pinned 子历史里**每个** tool_call.ID 都能在 pinned 里找到其 tool_result（`ToolCallID` 匹配）。
  - pinned 子历史里**每个** tool_result.ToolCallID 都能在 pinned 里找到其 call。
  - 即 pinned 中无"半截"工具对（半截会被 API 400 拒，bug②）。`EnforceToolCallPairs`（`pairs.go:18`）的 fixpoint + `permanentlyRemoved` 防振荡（`pairs.go:37,46`）是这条属性的实现保证。
  - 额外：`EnforceToolCallPairs` 是幂等的——对已 fixpoint 的 pinned 集再跑一次，集合不变（钉死 fixpoint 收敛）。

  **属性 P3 —— 每次 summary 模型调用的输入严格 ≤ 窗口（最强，"不发给 API 超窗请求"）**：
  用一个 `recordingSummarizer`（fake，实现 `ModelSummarizer`），它记录每次 `Generate`/`Stream` 收到的 `msgs` 的 `EstimateTokens(msgs)`。对随机长历史 + 随机 `ModelWindow`（含触发 carry 分块的小窗口）跑 `RunSummary`，断言：
  - 每条记录的输入 token 数 **≤ `opts.ModelWindow`**。
  - single 路径：`EstimateTokens(msgs)+instructionTok ≤ singleBudget`（`summarize.go:62`）。
  - carry 路径：每次 chunk 调用的输入 = carry(前缀) + ack + chunk + instruction ≤ ModelWindow（`chunkBudgetFor`，`summarize.go:110`）。
  - `takeChunk` 在工具对横跨切点时宁可让**单个** chunk 超预算也不切断对（`summarize.go:127-137` + `splitIsSafe`）——属性改为断言"切断点不半截工具对"，而非严格 ≤（因成对保留允许单 chunk 超预算，这是文档化的有意识取舍，`summarize.go:53` 注释）。

  **属性 P4（加固）—— 不产生空 summary（bug⑥）**：
  `Run` 在 summary 失败时返回 error 而非空 summary（`run.go:16` 注释 + `:34`）；fake summarizer 返回 `""` 时，`Run` 要么 error、要么（若全部 pinned、无需 summary）返回原文。断言：非空 summarize 集 + fake 返回空 → Run 返回 error。

  **属性 P5（加固）—— summary-of-summary 短路（bug⑦）**：
  对尾部已是 summary sentinel 的历史，`Plan` 返回全 pin、空 summarize 集（`plan.go:27`）；把 `Run` 的输出再喂一次 `Run`，第二次不再调 summarizer（`recordingSummarizer` 第二轮调用计数 == 0）。

- **依赖**：无（`ctxcompact` 已是纯算法包，`ModelSummarizer` 接口足够窄，`options.go:18`）。

- **风险与缓解**：
  - **属性太强导致 false fail**（如 P3 的成对超预算）→ 属性措辞严格对齐代码的**文档化取舍**（成对保留允许单 chunk 超），不把"有意识取舍"当 bug。实现时每条属性旁注明对应的代码注释行号。
  - **随机生成器本身有 bug** → 生成器先用手写 `TestGenHistory` 钉死（固定种子 → 固定输出），再用于属性；生成器是测试基础设施，必须先 deterministic。
  - **`EstimateTokens` 是启发式（chars/4）**（`tokens.go:11`），与真实 token 数有偏差 → P3 断言用的是同一个 `EstimateTokens`（与生产判定同一把尺子），属性保证的是"按这把尺子不超窗"，与生产语义一致；真实 token 溢出是另一层问题（不属 E2）。
  - **发现真不变量违反**（如 fixpoint 不收敛、pin 丢消息）→ 这是高价值发现，**修复属 ctxcompact 行为 bug，可在本批内修**（与 FUZ1 的"不改 glob 行为"不同：ctxcompact 不变量违反=正确性 bug，且属本批命题范围；但若修复面大则降级另开）。

- **验收**：
  - ≥3 个核心属性（P1/P2/P3）+ 2 个加固（P4/P5）存在并通过。
  - 随机生成器固定种子可复现；`go test -run Property -count=50 ./internal/ctxcompact`（重复跑加大随机覆盖）通过。
  - P2（工具对配对）在含 orphan 的随机历史上成立。
  - P3（窗口）在触发 carry 分块的小窗口用例上成立。

- **预估**：2d。

---

## 6. [RAC1] race detector 固化 + 并发热点测试  (P0 | 部分 | synthesis R2)

- **缺口**：无 CI `-race` 固化；`wsConn.write`（多 goroutine 并发写）、`runners sync.Map`（按 model 指针键缓存）、task broker（Claim/RequeueStale/`createdWT`）、registry Manager（读写锁）、`permTracker`（多 goroutine register/take/deliver）是竞态热点，从未上过 race detector。

- **落点**：
  - `internal/api/http/ws_race_test.go`（新；`wsConn.write` 并发写 + `permTracker` 并发 + `connSession` 帧交错集成）。
  - `internal/agent/orchestrator/orchestrator_race_test.go`（新；`runners sync.Map` 并发 `runnerFor`/`FlushRunners`）。
  - `internal/task/broker_race_test.go`（新；Claim/RecordResult/Cancel/RequeueStale 并发 + `createdWT` 有界性）。
  - `internal/agent/registry/manager_race_test.go`（新；Spawn/Query/Terminal/SendInput/Resume 并发）。
  - CI：`.github/workflows/`（**与 CIG1 共建**，见 Q1）；RAC1 至少提供 `-race` job 的命令与分层策略。

- **设计**：

  ### 6.1 `wsConn.write` 并发写（`ws.go:2257`）

  `wsConn`（`ws.go:2214`）的 `mu sync.Mutex`（`:2216`）序列化 `WriteMessage`（`:2272`）。生产中至少三路 goroutine 并发调 `conn.write`：主循环（turn 流式帧）、reader goroutine（set_mode 绕过 frames channel 直写，见 `permModeState` 注释 `ws.go:97-104`）、子代理 relay 回调（`newSubagentRelay(func(f){conn.write(f)})`，`ws.go:816`）。

  - **测试**：用 `httptest.NewServer` + gorilla/websocket（仓库已用）起一对 in-process WS；一端 `wsConn`，另一端 dialer 读。启 N（如 16）goroutine，每个循环 M 次 `conn.write(不同 ServerFrame)`；收端累计帧数。断言：`-race` 下无 DATA RACE；收端帧数 == N×M（无丢帧、无交错损坏——TextMessage 是原子帧）。`json.Marshal` 在锁外（`:2258`），多 goroutine 各自 marshal 不同 `f`，无共享状态，本身无竞态——测试同时证实这点。

  ### 6.2 `permTracker` 并发（`ws.go:45`）

  `permTracker`（`mu sync.Mutex` + `pending map` + `nextID uint64`，`ws.go:45-49`）的 `newID`/`register`/`take`/`deliver`（`:56-91`）跨 reader goroutine（deliver 响应）与主循环 goroutine（register/take 在权限回调里）并发。

  - **测试**：并发 `newID`（断言 id 唯一、单调）+ `register` + `take` + `deliver`（deliver 到已 take 的 id 须 no-op，`ws.go:84-90`）。`-race` 下无竞态；无 goroutine 泄漏（deliver 非阻塞，`:86-89`）。

  ### 6.3 `runners sync.Map`（`orchestrator.go:93`）

  `Orchestrator.runners`（`orchestrator.go:93`）以 `runnerCacheKey{model model.BaseChatModel, mode runnerToolMode}`（`:130`）为键。`runnerFor`（`:346`）Load → 未命中则 build → LoadOrStore（`:352,378`）；`FlushRunners`（`:395`）Range+Delete。

  - **测试**：构造 Orchestrator（fake model），启 goroutine：A 组并发 `runnerFor(sameModel, plan=false)`（断言同一 key 返回**同一** `*adk.Runner` 指针）；B 组并发 `runnerFor` 不同 model 指针（模拟 `/model` 切换）；C 组并发 `FlushRunners`。`-race` 下无竞态。
  - **注意点（登记，非本批修）**：`runnerCacheKey.model` 是**接口**字段（`model.BaseChatModel`，`:131`）。作为 `sync.Map` 键要求动态类型可比较；现实 model 都是指针（可比较），故当前安全，但若未来出现非可比较的具体 model 类型，`sync.Map.Load` 会 panic。RAC1 在测试注释里登记此 latent footgun。
  - **注意点（登记，非本批修）**：Load → build → LoadOrStore 期间两个 goroutine 可能都 build（expensive），胜者存、败者弃——非数据竞态，是冗余构建。性能登记归 BENCH1。

  ### 6.4 task broker Claim/RequeueStale/`createdWT`（`broker.go`）

  `Broker.createdWT`（`broker.go:41`）/`createdWTMu`（`:42`）的写入在 `Claim`（`:116-118`）、删除在 `RecordResult`（`:165-170`）与 `Cancel`（`:201-204`）。`RequeueStale`（`:219`）由 `StartSweeper`（`:245`）后台 goroutine 周期调，**不删 `createdWT`**。

  - **测试（并发正确性）**：`Submit` 一批 task；并发：worker 组 `Claim`→`RecordResult`/`Cancel`；sweeper goroutine 周期 `RequeueStale`。`-race` 下 `createdWTMu` 保护下无 map 竞态。
  - **测试（LEAK 探针，喂给 F2/LEAK1）**：长跑（Submit 多轮、部分 task 故意不 Heartbeat 触发 RequeueStale），断言**当前**行为：`createdWT` 在 RequeueStale 路径**不回收**（`len(createdWT)` 随 stale-requeue 增长）。此测试用 `t.Log` 记录"当前泄漏行为"，**预期通过**（它记录现状而非断言已修）；修复后（F2/LEAK1）改为断言"终态 `createdWT` 为空"。这样 RAC1 不阻塞、F2 接手时改断言即可。衔接见 §7。
  - **注意点（登记）**：RequeueStale 把 task 回 pending 但不清 `worktree_id`，再 Claim 时 `got.WorktreeID != ""`（`broker.go:109`）跳过建新 worktree——首次建的 worktree 被孤儿化（无人 reclaim）。这是 LEAK1 的根因，RAC1 只登记。

  ### 6.5 registry Manager 并发（`manager.go:37`）

  `Manager`（`mtx sync.RWMutex` 守 records+runtime、`persistMu sync.Mutex` 序列化写、`closed atomic.Bool`、`wg sync.WaitGroup`、`agentIDSeq atomic.Int64`，`manager.go:37-50`）。

  - **测试**：用 fake `Runner`（实现 `registry.Runner`），并发 `Spawn`（至 `limit` 上限，`:67-74`）/ `Query` / `Terminal` / `SendInput` / `Resume`。`-race` 下无竞态；`limit` 上限生效（超出 Spawn 被拒）；终态 `wg.Wait` 不挂。
  - **`Registry`（`registry.go:27`）无锁——登记为 finding**：`Registry.entries`（`registry.go:28`）无 mutex，`Register`/`Get`/`All`/`ByCapability`（`:35-66`）均无同步。实测 `Register` 仅在 bootstrap（`external.go:9`）与测试调用，生产中 bootstrap 后只读——Go map 并发只读安全。**RAC1 登记**：此为"按惯例 bootstrap 冻结"的隐式契约，无文档无守卫；建议（F2/GOV）加 `RWMutex` 或加一个"冻结后不可写"的 panic-on-late-write 断言。RAC1 写一个 `TestRegistry_BootstrapFrozenConvention` 文档化此契约（只读并发不 panic），不修代码。

  ### 6.6 `connSession` 帧交错集成（`ws.go:131`）

  `connSession`（`history`/`model`/`tokensIn`，`ws.go:131-159`）的 history 由主循环（user_message turn append）写、model 由 control frame（set_model）写。架构上 control frame 走 frames channel → 仅主循环 drain（与 turn 串行），**set_mode 例外**（直写 `permModeState`，故有独立 RWMutex，`ws.go:105`）。

  - **测试（集成级）**：起 in-process WS handler，从客户端并发投递交错帧（`user_message` turn + `set_model` + `set_mode` + `cancel`），`-race` 下证实 history/model 访问无竞态（即"frames channel 串行化"契约成立）、set_mode 经独立锁安全。若发现 history/model 有竞态 → 说明有 control frame 绕过了 frames channel，登记为既有竞态（→ F2）。

- **依赖**：
  - **CIG1（G1）**：RAC1 的 CI 落地需要 workflow 文件壳子。RAC1 提供 `-race` job 内容，CIG1 提供矩阵骨架（Q1 定谁先）。
  - 无生产代码依赖（不改 guard/ctxcompact/broker/registry 行为）。

- **风险与缓解**：
  - **`-race` 慢（2-10×）** → CI 分层（§9）：PR 只跑变更包 `-race`，merge 跑全量，nightly 跑长 fuzz + bench。绝不 PR 全量 `-race ./...`。
  - **既有竞态让 `-race` day-1 全红** → `-race` 先上 **nightly/merge**（非 PR 阻塞），发现的竞态入 F2 登记表；F2 清完后升格为 PR 门禁（§7、§9）。这是 roadmap "CI 分层" 的具体化。
  - **测试自身竞态**（`t.Parallel` + 共享 fixture）→ RAC1 的并发测试**不**盲目加 `t.Parallel()`（`-race` 下并发测试本身已多 goroutine），fixture 每用例独立构造，不共享可变状态。
  - **发现既有生产竞态** → 登记到 F2 findings 表（§7），**不在 RAC1 修**（除非是 RAC1 新增测试代码自身的竞态，那必须本批修干净）。

- **验收**：
  - CI 有 `-race` job（分层，见 §9），至少 nightly + merge 跑 `-race ./...` 绿（或既有竞态已登记并被有意识 exclude，见 §7）。
  - 6 个并发热点（6.1-6.6）各有测试；本地 `go test -race ./internal/api/http ./internal/agent/... ./internal/task` 通过。
  - 发现的既有竞态/泄漏有登记表条目（§7）。
  - `Registry` 无锁契约有文档化测试。

- **预估**：2-3d。

---

## 7. RAC1 → F2（LEAK）衔接

roadmap §1.4 明确"R2 的 RAC 会暴露 F2 的 LEAK"。本 spec 把这条衔接机制化：

### 7.1 findings 登记表

RAC1 在执行中维护一份**并发 findings 登记**（落地为 F2 plan doc 的一节，或 `docs/superpowers/notes/2026-07-22-e2-race-findings.md`——由 writing-plans 阶段定载体，**不属本设计 spec 的产物**）。每条至少：

| 字段 | 说明 |
|---|---|
| 位置 | `file:line` + 类型/函数 |
| 现象 | `-race` 报告原文 / LEAK 探针的 `t.Log` 摘要 |
| 分类 | data-race / leak / latent-footgun / perf |
| 归属 | LEAK1（`createdWT`）/ LEAK2（子代理并发）/ 其他 |
| 当前处置 | RAC1 登记不修；F2 接手 |

### 7.2 已预判的 finding（spec 阶段即知，非等运行才发现）

| # | 位置 | 现象 | 分类 | 归属 |
|---|---|---|---|---|
| F-1 | `broker.go:219` `RequeueStale` | `createdWT` 在 stale-requeue 路径不回收，`len(createdWT)` 随长跑增长；孤儿 worktree 不被 reclaim | leak | **LEAK1** |
| F-2 | `broker.go:109` Claim 重入 | requeue 后 `worktree_id` 残留，重 Claim 跳过建新 WT，首次 WT 孤儿 | leak | **LEAK1** |
| F-3 | `orchestrator.go:131` `runnerCacheKey.model` | 接口作 sync.Map 键，非可比较动态类型会 panic（当前指针，安全） | latent-footgun | GOV/文档 |
| F-4 | `orchestrator.go:346` `runnerFor` | Load→build→LoadOrStore 两 goroutine 冗余构建 | perf | BENCH1 |
| F-5 | `glob.go:27` `MatchGlob` | 每调用重编译 regex，热路径（guard.Check 每 pattern 每维） | perf | BENCH1/E3 |
| F-6 | `registry.go:28` `Registry.entries` | 无锁，靠"bootstrap 冻结"隐式契约 | latent-footgun | GOV/F2 |

### 7.3 "预期通过的现状探针"模式（关键）

F-1/F-2 属泄漏，**不是 data race**（`createdWTMu` 已保护 map 访问，`-race` 不会报）。RAC1 的 broker 测试用一个**断言现状**（`t.Log` 记录 `len(createdWT)` 增长，`assert.GreaterOrEqual(len, baseline)` 而非 `assert.Zero`）的探针，使测试**当前绿**；F2/LEAK1 修复后，把同一断言翻转为 `assert.Zero`（终态 `createdWT` 空）。这样：
- RAC1 不被既有泄漏阻塞（CI 绿）。
- F2 有现成的、已在 `-race` 下验证过的测试接手，只改断言方向。

**真正的 data race**（如 6.6 若发现 history 访问竞态）则不同——`-race` 会硬报，不能"断言现状绿"。这类走 §9 的分层（先 nightly，exclude 已登记项，F2 修后去 exclude）。

---

## 8. 文件结构

| 文件 | 职责 | 新/改 |
|---|---|---|
| `internal/guard/glob_test.go` | 新增 `FuzzMatchGlob`（与现有 `TestMatchGlob` 同文件） | 改 |
| `internal/guard/testdata/fuzz/FuzzMatchGlob/*` | fuzz 种子 + 回归语料（入仓） | 新 |
| `internal/ctxcompact/gen_test.go` | 固定种子随机历史生成器（P1-P5 共用） | 新 |
| `internal/ctxcompact/plan_property_test.go` | P1（pin⊆out）、P2（工具对 fixpoint）属性 | 新 |
| `internal/ctxcompact/summarize_property_test.go` | P3（窗口≤）、P4（不空 summary）属性 + `recordingSummarizer` | 新 |
| `internal/ctxcompact/run_property_test.go` | P5（summary-of-summary 短路）+ Run 端到端属性 | 新 |
| `internal/api/http/ws_race_test.go` | 6.1 wsConn.write、6.2 permTracker、6.6 connSession 帧交错 | 新 |
| `internal/agent/orchestrator/orchestrator_race_test.go` | 6.3 runners sync.Map 并发 | 新 |
| `internal/task/broker_race_test.go` | 6.4 broker 并发 + createdWT LEAK 探针 | 新 |
| `internal/agent/registry/manager_race_test.go` | 6.5 Manager 并发 + Registry 冻结契约文档化 | 新 |
| `.github/workflows/*.yml` | CI job（`-race` 分层 + fuzz 语料 + property），**与 CIG1 共建** | 新（Q1） |
| `docs/superpowers/notes/2026-07-22-e2-race-findings.md` | §7 findings 登记（writing-plans 阶段定载体） | 新（可选） |

**不改**任何 `internal/**` 非 `_test.go` / 非 `testdata` 的生产代码。`broker_race_test.go` 的 LEAK 探针若需访问私有字段 `createdWT`，用同包测试（`package task`）直接访问，不导出。

---

## 9. CI 分层策略

`-race` / fuzz / property 在 CI 的分层（CIG1 落地，RAC1 提供内容）：

| 层 | 触发 | 跑什么 | 阻塞合并？ |
|---|---|---|---|
| **PR** | 每次 push | `go test -race`（**仅变更包**，复用 `cmd/testchanged` 的 diff→包逻辑，加 `-race`）+ `go test -run 'Fuzz\|Property'`（跑语料/属性，不跑长 fuzz）+ `go vet` | 是 |
| **merge** | 合入 main | `go test -race ./...`（全量 `-race`）+ 全属性 `-count=10` | 是（若红，revert 或热修） |
| **nightly** | 定时 | `go test -fuzz=FuzzMatchGlob -fuzz=2m ./internal/guard`（短时长 fuzz）+ BENCH1 基准（F2） | 否（发现 → 登记-issue） |

**既有竞态的 CI 处置**：merge/nightly 若 `-race` 报既有竞态（如 6.6 发现的 history 竞态），用 `//go:build` 或 `-skip` **临时排除**该测试 + 在 findings 表登记 + 指向 F2 issue，**不长期掩盖**——F2 修复后立即去 exclude。排除必须有 findings 表条目与 issue 号（防"排除即遗忘"）。

**本地长 fuzz 不入 CI**：`go test -fuzz=FuzzMatchGlob -fuzz=30m` 由人按需本地跑；发现的 crash → 固化进 `testdata/fuzz/`（入仓）→ 此后 PR 层自动跑该回归。

---

## 10. 测试策略（Fake 优先，无 mock 框架）

- **FUZ1**：fuzz 引擎自身即输入源；种子手工提炼；无 fake 需求。
- **PROP1**：`recordingSummarizer`（自写，实现 `ModelSummarizer`，`options.go:18`，记录每次调用输入与次数）；随机生成器 `math/rand/v2` 固定种子。不引 `testing/quick`（其随机性弱、不便复现），用显式生成器（可控、可打印种子）。
- **RAC1**：
  - WS：`httptest` + gorilla/websocket in-process dial（仓库已在 `ws_test.go` 用）。
  - Orchestrator：`einollm.FakeModel` 作 model 指针键。
  - broker：`store.Store` 用内存 SQLite（`:memory:`，已有用法）；VCS 用 nil 或 fake（LEAK 探针不需真 VCS，只看 `createdWT` map）。
  - registry Manager：fake `Runner`（实现 `registry.Runner` 接口）。
- **确定性**：所有随机源固定种子；不依赖 wall clock（broker 的 `RequeueStale` 用 `time.Now()`，测试用可控的 heartbeat/timeout 注入足够老的时间戳触发 stale，不让真测试 `time.Sleep`）；不依赖 OS 网络/文件。

---

## 11. 风险与缓解（汇总）

| 风险 | 缓解 |
|---|---|
| fuzz 发现 glob 语义 bug（安全相关） | 入回归 corpus 钉现状；行为修复另开（guard 专项），E2 不越界改生产 |
| 属性测试 false-fail（措辞与有意识取舍不符） | 每属性旁注对应代码注释行号；P3 的成对超预算按 `summarize.go:53` 文档措辞 |
| `-race` 发现既有竞态致 CI 全红 | 分层（§9）：PR 只变更包；merge/nightly 全量；既有竞态 exclude+登记+F2 接手 |
| `-race` 全量慢 | 分层 + nightly 长任务；PR 用 `cmd/testchanged` 缩范围 |
| LEAK 探针被误当 bug 修 | 探针用 `t.Log` + `assert.GreaterOrEqual`（现状绿），注释标明"F2 修复后翻转为 Zero" |
| 随机生成器自身 bug | 生成器先 `TestGenHistory` 钉固定种子输出，再用于属性 |
| CIG1 与 RAC1 谁先建 `.github/workflows` | open question Q1；最小依赖：RAC1 可先只交付测试文件 + 一份 CI 命令清单，CIG1 组装 workflow |
| fuzz corpus 膨胀（大文件入仓） | 语料文件小（pattern/name 对）；超长串用短种子 + `strings.Repeat`，不入超长 raw |

---

## 12. 验收标准

1. **FUZ1**：`FuzzMatchGlob` 存在；`go test -run FuzzMatchGlob ./internal/guard`（跑语料）通过、无 panic；种子含 IFS/注入/`../`/嵌套/trailing-`*`/超长/UTF-8 边界；`testdata/fuzz/` 入仓。
2. **PROP1**：P1-P5 五条属性存在并通过；随机生成器固定种子可复现；P2 在 orphan 历史上成立、P3 在 carry 分块小窗口上成立；`-run Property -count=50` 通过。
3. **RAC1**：6 个并发热点（wsConn.write / permTracker / runners sync.Map / broker / Manager / connSession 帧交错）各有测试；本地 `go test -race`（热点包）通过。
4. **CI**：`-race` job 分层落地（至少 nightly + merge 全量、PR 变更包），或 RAC1 已交付命令清单且 CIG1 依赖明确（Q1）。
5. **F2 衔接**：§7.2 的 findings 表（至少 F-1/F-2 createdWT leak）已登记；broker LEAK 探针就位（现状绿、标注待翻转）。
6. **契约文档化**：`Registry` bootstrap-冻结契约有测试；`runnerCacheKey` 接口键 footgun 有注释登记。
7. **无生产代码改动**：本批仅新增 `_test.go` / `testdata` / CI workflow；`internal/**` 非 `*_test.go` 零改动（ctxcompact 不变量违反除外，见 PROP1 风险）。

---

## 13. 后续 / out-of-scope

- **F2/LEAK1**：修 `createdWT` 在 RequeueStale 路径的回收（终态删 map 项 + reclaim 孤儿 worktree）；翻转 RAC1 的 LEAK 探针断言。
- **F2/LEAK2**：子代理并发上限（`MaxSubAgentDepth` 之外的 running 总数 cap）。
- **F2/BENCH1**：regex 缓存（F-5）、runner 冗余构建（F-4）、性能基准基线。
- **E3/GOV**：`Registry` 加锁或冻结断言（F-6）、`runnerCacheKey` 文档（F-3）、1000 纯代码行 / 分层 CI 门禁。
- **CIG1**：CI 矩阵骨架（跨平台 × Go 版本 × `-race` × build），RAC1 的 `-race` job 并入。
- **长 fuzz 自动化**：nightly 短 fuzz 之外，可考虑分布式/更长时 fuzz（留 v1.0 后）。

---

## 14. Open Questions（需人决策）

- **Q1（CI 归属）**：`.github/workflows/` 由 RAC1 首创还是等 CIG1（G1）？RAC1 只交付"测试文件 + `-race` 命令清单 + 分层策略"，还是顺带建首个 workflow 文件？建议：RAC1 交付内容、CIG1 交付壳子并依赖 `[RAC1][FUZ1][PROP1]`（与 roadmap CIG1 依赖一致）；但若 CIG1 排期靠后，RAC1 可先建最小 workflow 避免 `-race` 长期无 CI。**需人定排序。**
- **Q2（glob 语义 bug 处置）**：若 fuzz 发现 `MatchGlob` 有真实语义/安全偏差（如某元字符未转义致绕过），E2 是否允许就地修 `glob.go`？spec 默认**不修、只钉现状**（避免 E2 越界），但这与"安全 fail-closed"优先级冲突——若发现的是 over-grant 类安全 bug，是否破例本批修？**需人定原则。**
- **Q3（property 发现的不变量违反）**：若 P2/P3 发现 `ctxcompact` 真实违反（如 fixpoint 偶发不收敛、summary 偶发超窗），修复面小时本批内修、大时降级另开——"大/小"的阈值？**建议：单文件、<50 行改动的本批修；超出另开 plan。需人确认。**
- **Q4（`-race` 既有竞态的 exclude 机制）**：exclude 用 build tag、`-skip`，还是环境变量门控？需与 CIG1 的 CI 约定一致。**建议 `-skip=<TestName>` + findings 表 issue 号注释。需人定。**
- **Q5（findings 登记载体）**：§7 findings 表是放 F2 plan doc 里、还是独立 notes 文件、还是 issue tracker？**建议独立 notes 文件 + issue（可追踪）。需人定。**
