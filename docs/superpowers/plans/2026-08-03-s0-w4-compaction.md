# S0/W4 压缩正确性 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修好 mid-turn 压缩的 token 量纲错配（导致 cooldown 在出厂默认下**永远不成立**，每次 ReAct 迭代都重跑一次完整 summarization），让三个门（threshold / cooldown / hard-force）改用 per-provider 上下文窗口，并补齐 `ctxcompact` 的属性测试缺口。

**Architecture:** 两条链路：① `CompactingModel` 的量纲与窗口（`internal/llm/eino` + `internal/agent/orchestrator` + `internal/bootstrap`）；② `ctxcompact` 的属性/fuzz 测试（纯测试增量）。**`ctxcompact` 的生产代码一行不动**，pre-turn 路径零影响。

**Tech Stack:** Go 1.26.4，Eino ADK，`testing/quick` 风格的属性测试 + Go fuzz。

---

## 本计划的写法

**这是意图级计划，不是代码清单。** 每步说清楚：改哪个文件的哪个函数、断言什么行为、怎么观察、为什么重要、预期看到什么。**具体代码由实现者写** —— 你手里有编译器，本文档没有。

「已核对事实」是**实测结果**，可直接依赖；其余标识符请先 grep。

---

## 已核对事实（实测，不要重新推导）

| 事实 | 位置 |
|---|---|
| **存值**：`c.lastCompactTokens = res.TokensAfter` —— 压缩**后**输出的 token 数 | `internal/llm/eino/compacting.go:143` |
| **比值**：`tokens := ctxcompact.EstimateTokens(msgs)` —— ADK 传进来的**完整未压缩**历史 | `compacting.go:158`（`shouldCompact` 内） |
| cooldown 判据 `tokens - lastT < c.CooldownTokens` —— **两个量纲不同** | `compacting.go:192`，调用在 `:174` |
| **根因**：压缩结果**从不写回 ADK state** —— `Generate`/`Stream` 只把结果传给 `Inner`，返回值不含消息列表，ADK 的 `state.Messages` 只做 append | `compacting.go:104-118` |
| **对照**：pre-turn 路径把 `MaybeCompact` 的返回**直接赋回** `cs.history`，量纲自洽 | `internal/api/http/ws_compaction.go` |
| `ctxcompact.Run` 的 `Result` **已同时返回** `TokensBefore` / `TokensAfter` —— 无需新增字段 | `internal/ctxcompact/run.go:37` |
| `CooldownTokens` 取**全局回退** `cfg.Compaction.ContextWindow`（默认兜底 256000） | `bootstrap.go:825`（审计写 `:794` 已漂移）；默认在 `config.go:517-518` |
| per-provider `context_window` 走**完全独立**的通道，**从未流向 orchestrator** | `BuildProviders` → `apihttp.CompactionConfig.ProviderWindows` → `ws_compaction.go:422 contextWindowFor` |
| mid-turn 拿到的是同一个全局值 | `orchestrator.go:234`（`wrapCompaction`，`:227-239`） |
| `runnerFor`（`:381`）只拿到 `model.BaseChatModel` 指针；`TurnOpts`（`:490-499`）**无模型名字段** —— 这一层根本没有信息去查 per-provider 窗口 | `orchestrator.go` |
| `config.ContextWindowFor` **已存在**，可复用 | `internal/config/config.go:642` |
| ⚠️ `contextWindowFor` 住在 `internal/api/http` —— orchestrator 依赖它会**破坏 GOV1 分层** | — |
| `ctxcompact` 有 14 个测试文件、~1500 测试行、**0 个 fuzz target** | 全仓唯一 fuzz 是 `internal/guard/glob_test.go:62 FuzzMatchGlob` |
| CI 的 `fuzz-seed` job **无 `continue-on-error`，已是硬门禁**；其 guard 是 `go test -list 'Fuzz.*\|TestProperty.*'` 后 grep，ctxcompact 的 8 个 `TestProperty_*` 让它真跑 | `.github/workflows/ci.yml:126-143` |
| `docs/compaction.md` **通篇零处提到 cooldown**（`grep -i cooldown` 零命中），而 `config.example.yaml:48-50` 已暴露三个键 | — |

### cooldown bug 已实测复现

探针（`hist()` 每次返回同一份未压缩历史，模拟 ADK ReAct 迭代重新递交完整历史），参数 `Threshold=0.5, ContextWindow=400, KeepRecent=2, CooldownTokens=100, HardForceFraction=0.95`（现有测试同款）：

```
iter1: did=true innerCalls=1 storedLastCompactTokens=94
iter2: incomingUncompactedTokens=228  lastCompactTokens=94  growth=134  CooldownTokens=100
iter2: inCooldown=false (want true)
iter2: did=true (want false) innerSummarizeCalls=1 (want 0)
BUG REPRODUCED: re-compacted inside cooldown window
```

**历史一字未变、增长为 0，第二次迭代却又压缩了一次。**

**出厂默认下更严重**：window 256000、threshold 0.8 → 204800 触发；压缩后 `lastCompactTokens` 约几千到两万；`CooldownTokens = 0.05 × 256000 = 12800`。`204800 − 20000 = 184800 ≫ 12800` —— **cooldown 永远不成立，每一次 ReAct 迭代都会重跑一次完整的 summarization turn**。这就是「主功能完全失效」的准确含义。

### ⚠️ 现有测试为什么绿：它主动掩盖了 bug

`internal/llm/eino/compacting_test.go:310 TestCompactingModel_CooldownDefersReCompact` 在第一次 `maybeCompact` 后**手工覆写** `cm.lastCompactTokens = 180`（`:336`），把它人为拉到与未压缩历史同一量纲，再断言 cooldown 生效。**它从头到尾没有断言 `maybeCompact` 实际存进去的是什么。**（`:356` 还有第二处同样的覆写。）

### 窗口错配的后果（128K 模型）

| 门 | 当前阈值 | 应为 |
|---|---:|---:|
| cooldown | 12800 | 6400 |
| threshold | 204800 | 102400 |
| hard-force | 243200 | 121600 |

threshold 门在**实际窗口的 1.9 倍** —— 等于永不触发。

---

## 三条裁定

**裁定 1 — 只改 `internal/llm/eino`，`ctxcompact` 生产代码一行不动。**
`Run` 的 `Result` 已同时返回 `TokensBefore`/`TokensAfter`，量纲修复只需换用哪个字段。pre-turn 路径本来就自洽，**零影响**。

**裁定 2 — 窗口错配与量纲错配一起修，不分两次。**
`CooldownTokens` 用错窗口是**根因级**问题：即使量纲修对了，128K 模型的 threshold 门仍在 1.9 倍处永不触发。而修法（把窗口传到 orchestrator 层）会顺带让 threshold / cooldown / hard-force **三个门自动全部用同一个正确窗口** —— 一次修到位比修两次便宜。

**裁定 3 — 属性测试优先补「组装结果永不超窗」，不是凑数量。**
`E2/PROP1` 的验收是「≥3 个属性」，现有 8 个已达标，所以**数量不是缺口**。真正的缺口是最有价值的那条不变式完全没测（见 Task 5）。

---

## 文件结构

**修改**

| 文件 | 改什么 |
|---|---|
| `internal/llm/eino/compacting.go` | 存值改用 `TokensBefore`；`hasCompacted` 显式标志 |
| `internal/llm/eino/compacting_test.go` | **删掉两处手工覆写**，改为断言真实存值语义 |
| `internal/agent/orchestrator/orchestrator.go` | `CompactionConfig` 加窗口表与 fraction；`TurnOpts` 加模型名；`runnerFor` / `wrapCompaction` 传窗口 |
| `internal/bootstrap/bootstrap.go` | 传 `providerWindows`，删掉 `:825` 的预乘 |
| `internal/api/http/ws.go`、`internal/api/v1/service.go`、`internal/api/http/chat.go` | 填模型名 |
| `internal/ctxcompact/*_property_test.go` | 补属性；修两处已知弱点 |
| `internal/ctxcompact/` | **新增 fuzz target + 种子语料** |
| `docs/compaction.md`、`docs/adr/0006-*.md`、`CLAUDE.md` | 收窄错误断言；补 cooldown 一节 |
| `docs/adr/00NN-*.md` | 新 ADR：mid-turn 量纲不变量 |

---

## ⚠️ 与 W1 / W3 的冲突

**本计划要给 `TurnOpts` 加模型名字段**，而 W1 要加 `PlanMode` / `ModelID` / `Images`，W3 要加 `ThreadID` / `TurnID` —— **同一个 struct**。

> **W1 已经加了 `ModelID`**（见 W1 Task 12）。**本计划应优先复用它，而不是新加一个字段。**
>
> **执行顺序：W1 先落地。** 落地后第一件事是确认 `TurnOpts.ModelID` 是否已满足本计划的需要；若满足，Task 3 缩减为「接线」而非「加字段」。

---

## Task 1: 修 `lastCompactTokens` 的量纲错配

**Files:** Modify `internal/llm/eino/compacting.go`（`:143`、`:189`、`:192`）；Test `internal/llm/eino/compacting_test.go`

- [ ] **Step 1: 先删掉测试里的手工覆写，让 bug 暴露**

**文件**：`internal/llm/eino/compacting_test.go`

**做什么**：删掉 `:336` 与 `:356` 两处对 `cm.lastCompactTokens` 的手工覆写。

**为什么先做这一步**：这两行是 bug 的**遮羞布**。不删掉它们，任何修复都无法被验证 —— 测试断言的是人为塞进去的值，不是代码实际存的值。

**预期**：`TestCompactingModel_CooldownDefersReCompact` **变红**。这证明它此前一直在测一个虚构状态。

- [ ] **Step 2: 写真正能证伪的测试**

**断言什么**：
1. **存值语义** —— `maybeCompact` 之后，`lastCompactTokens` 等于**输入历史**的 token 估算值（不是输出的）
2. **cooldown 真的生效** —— 用**同一份历史**连续调两次 `maybeCompact`，第二次 `did=false` 且 inner 的 summarize 调用数**不增加**

**为什么第 2 条是关键**：这正是已复现的 bug —— 历史一字未变、增长为 0，第二次却又压缩了一次。

**预期**：两条都 FAIL。

- [ ] **Step 3: 实现**

**怎么改**：
1. 存值改用 `Result` 里表示**压缩前**输入大小的那个字段，使它与 `:158` 的比值同量纲。语义变成「上次触发压缩时未压缩历史有多大」，于是 `tokens - lastT` 真正表示**增长量**
2. 把「是否压缩过」的判定从 `lastT == 0` 哨兵（`:189`、`:192`）换成**显式布尔标志** —— 压缩前 token 数恒非 0，靠 0 判空本就脆

⚠️ **`ctxcompact` 一行不动。**

**预期**：两条测试 PASS。

- [ ] **Step 4: 全量与提交**

Run: `go test ./internal/llm/... ./internal/ctxcompact/... ./internal/api/... && go test ./...`

> pre-turn 路径本来就自洽，**不应有任何 pre-turn 测试变红**。若有，说明改动越界了 —— **停下来汇报**。

---

## Task 2: `CompactionConfig` 改收窗口表与 fraction

**Files:** Modify `internal/agent/orchestrator/orchestrator.go`（`CompactionConfig`、`wrapCompaction` `:227-239`）、`internal/bootstrap/bootstrap.go`（`:823`、`:825`）；Test 两个包

**背景：** `CooldownTokens` 在 bootstrap 里被**预乘**成一个数（`:825`），用的是全局回退窗口。这让 orchestrator 层永远拿不到「真实窗口是多少」这个信息。

- [ ] **Step 1: 写失败测试**

**断言什么**：给定一个 128K 窗口的 provider，`wrapCompaction` 产出的 `CompactingModel` 里，**threshold / cooldown / hard-force 三个门都基于 128K 计算**，不是全局的 256K。

**为什么重要**：见「窗口错配的后果」表 —— threshold 门在实际窗口的 1.9 倍等于永不触发。这不是精度问题，是**功能失效**。

**预期**：FAIL，三个门都基于全局值。

- [ ] **Step 2: 实现**

**怎么改**：
1. `orchestrator.CompactionConfig` 加一个 provider→窗口的映射，以及 cooldown 的**比例**（**取代**预乘好的绝对值）
2. bootstrap 把已作为局部变量存在、已传给 `apihttp.Config` 的 `providerWindows` **一并**传给 `orchConfig.Compaction`，删掉 `:825` 的乘法
3. `wrapCompaction` 改为接收**解析后的窗口**，内部自己算 cooldown、设 ContextWindow

**收益**：threshold、hard-force、cooldown **三个门自动全部用同一个正确窗口**。

⚠️ **DRY 警告**：`contextWindowFor` 住在 `internal/api/http`。**orchestrator 依赖它会破坏 GOV1 分层。** 正确做法是复用 `config.ContextWindowFor`（`config.go:642`），或在 orchestrator 内写一个等价的私有函数 —— **绝不能让 orchestrator import `api/http`**。

- [ ] **Step 3: 全量与提交**

Run: `go test ./internal/archtest -run TestGOV1` —— **必须确认分层没被破坏**。

---

## Task 3: 把模型名传到 `runnerFor`

> ⚠️ **依赖 W1**：W1 已给 `TurnOpts` 加了 `ModelID`。**先确认它是否满足本任务需要 —— 若满足，本任务只做接线，不加字段。**

**Files:** Modify `internal/agent/orchestrator/orchestrator.go`（`runnerFor` `:381`）、三个 turn 入口；Test

**背景：** `runnerFor` 只拿到 `model.BaseChatModel` 指针，从指针无法反查模型名 —— 所以这一层**根本没有信息**去查 per-provider 窗口。

- [ ] **Step 1: 写失败测试**

**断言什么**：三个 turn 入口发起的 turn，其 `CompactingModel` 用的是**该 turn 实际模型**的窗口。

**怎么观察**：WS 层有 `cs.model`，SSE 层有 `req.Model`，v1 有 `p.Model`。

**预期**：FAIL，都用全局窗口。

- [ ] **Step 2: 实现**

**怎么改**：`runnerFor` 增加模型名形参，用与 `contextWindowFor` **相同的查表规则**解析窗口，未命中回退全局值。

**关于 runner 缓存键**：仍用 model 指针即可（指针↔名字在 `BuildProviders` 里是 1:1）。把模型名加进 `runnerCacheKey` 是**零成本保险**，可以加。

⚠️ W1 已在 `runnerCacheKey` 里有 `{model, mode}` 两个维度 —— **加第三个维度前先读一遍 W1 落地后的实际结构**。

- [ ] **Step 3: 全量与提交**

---

## Task 4: 修 `ctxcompact` 现有属性测试的两个已知弱点

**Files:** Modify `internal/ctxcompact/plan_property_test.go`（`:102-112`）、`internal/ctxcompact/run_property_test.go`（`:49`）

**现有 8 个属性测试的实测评估**：

| 属性 | 位置 | 评价 |
|---|---|---|
| pin 是输出前缀、指针逐字相等、索引升序 | `plan_property_test.go:72` | 好（比的是**指针**，逐字保留有保证） |
| tool_call/result 配对 fixpoint | `plan_property_test.go:102` | ⚠️ **有洞**（见下） |
| fixpoint 幂等 | `:147` | 好 |
| fixpoint 修复人为损坏 | `:175` | 好 |
| 每次 summary 调用输入 ≤ ModelWindow | `summarize_property_test.go:41` | 好，4 个窗口参数化 |
| 空 summary 不产出空消息 | `:80` | ⚠️ 语义与规划**相反**（规划要「空串→Run 报错」，实现断言「不报错」） |
| 不二次压缩 | `run_property_test.go:9` | 好 |
| Run 单调减少 token | `:49` | ⚠️ **只有单一固定种子** `rand.NewPCG(123,0)`、n=40 一组 |

- [ ] **Step 1: 修配对不变量测试的 skip 洞**

**问题**：`:112` 的 `pinnedSetIsConsistent` 前置守卫在历史以 summary sentinel 结尾时 `t.Skip`。**实测 50/30/30 轮里 skip 27 次（约 22%）—— 恰好是最容易出问题的分支被绕开。**

**怎么改**：把 `t.Skip` 换成「对短路分支断言**正确的弱不变量**」。

⚠️ **不要简单删掉守卫** —— 那会让测试在短路分支上断言错误的强不变量而误报。**先搞清楚短路分支上什么是真的**，再写断言。

- [ ] **Step 2: 单调性测试种子多样化**

**怎么改**：从单一固定种子改成**种子表 × 多 n × 多 KeepRecent**。

- [ ] **Step 3: 裁决「空 summary」那条的语义分歧**

**做什么**：规划文档要求「空串 → `Run` 报错」，而实现断言「不报错」。**先读 `Run` 的实际行为与它的 doc 注释**，判断哪一方是对的。

- 若实现对 → **改规划文档**，并在测试里写明为什么不报错是正确的
- 若规划对 → 这是一个真实缺陷，**修实现**

⚠️ **不要两边都不动就把它当作已知差异放过** —— 那正是这类分歧长期存在的原因。

- [ ] **Step 4: 全量与提交**

---

## Task 5: 补最有价值的那条属性 + fuzz target（PROP1 核心）

**Files:** Modify/Create `internal/ctxcompact/` 下的属性测试与 fuzz 文件 + 种子语料

**裁定 3 重申**：`E2/PROP1` 的「≥3 个属性」验收现有 8 个**已达标**，数量不是缺口。

- [ ] **Step 1: 写「组装结果永不超窗」属性（最有价值的一条）**

**断言什么**：`Assemble` 的**产物**（pin 集合 + summary）的 token 数 **≤ ModelWindow**。

**为什么这是最有价值的**：现在只保证「每次 summary **调用**不超窗」，**不保证产物不超窗**。pin 规则（尾部 + 全部 user 原文 + working-set 路径 + error/diff 标记）在长会话里可以把大半历史 pin 住 —— **产物超窗是可达状态，而 `Run` 对此没有任何断言或降级**。

**预期**：⚠️ **这条很可能一写就红。** 若红了，**那不是测试写错了，是发现了一个真实缺陷** —— 需要在 `Run` 里加降级（或至少显式报错），**不要为了让测试绿而放宽断言**。

- [ ] **Step 2: 补 `Plan` 短路分支的不变式**

**背景**：历史已以 summary 结尾时走短路分支，目前只有 `plan_test.go:50` 一个固定用例。

- [ ] **Step 3: 加 fuzz target + 种子语料**

**做什么**：至少一个针对配对修复或规划/组装的 fuzz target，配种子语料。

**为什么重要**：`ctxcompact` 现在 **0 个 fuzz target**，而 CI 的 `fuzz-seed` job **已是硬门禁**（W0 已收紧，无 `continue-on-error`）—— 新增 fuzz target 会**直接进硬门禁**。

⚠️ 种子语料要包含已知的边界形状（空历史、只有 sentinel、孤儿 tool_call、超长单条消息）。

- [ ] **Step 4: 全量与提交**

Run: `go test ./internal/ctxcompact/... -run 'TestProperty|Fuzz' -v`
Run: `go test ./internal/ctxcompact/... -fuzz=FuzzXxx -fuzztime=30s`（本地验证 fuzz 真能跑）

---

## Task 6: ADR + 文档同步

**Files:** Create `docs/adr/00NN-*.md`；Modify `docs/compaction.md`、`docs/adr/0006-*.md:25`、`CLAUDE.md`

- [ ] **Step 1: 新 ADR —— mid-turn 量纲不变量**

**编号**：取当前最大 +1（从 `docs/adr/0000-template.md` 复制）。

**要固化的约束**：**mid-turn 压缩的所有 token 会计必须以「ADK 递交的未压缩历史」为统一量纲，因为压缩结果按设计不回写 ADK state。**

**为什么值得一条 ADR**：这是一条**可被违反**的承重约束。下一个改 `CompactingModel` 的人若不知道「结果不回写 state」，会很自然地再次存进压缩后的值。

- [ ] **Step 2: 修 `docs/compaction.md:88` 的错误因果**

**原文**：「`/model` 切换自动用新窗口——**因为 `CompactingModel` 按 model 指针缓存**」。

⚠️ **这句的因果推理是错的**：按指针缓存确实会为新模型建新 `CompactingModel` 实例，但 `wrapCompaction` 给新实例填的 `ContextWindow` 仍是同一个全局值。**换了实例，没换窗口。**

同段说「查询走 `BuildProviders` 返回的 `windows` map」——这**只描述 pre-turn/WS 路径**，却写在不区分路径的通用小节里。

**准确表述**（Task 2/3 完成**之前**的现实）：per-provider `context_window` 只在 pre-turn / WS / `/compact` 路径生效；mid-turn 的三个门一律用全局值。

> **Task 2/3 做完后，这三处才能改成现在文档里写的样子。** 顺序不能反。

- [ ] **Step 3: 补 `docs/compaction.md` 的 cooldown 一节**

**问题**：该文档 `grep -i cooldown` **零命中**，而 `config.example.yaml:48-50` 已经暴露了三个键给用户。

**要写**：三个门（threshold / cooldown / hard-force）的关系与**量纲**。

- [ ] **Step 4: 收窄 `docs/adr/0006-*.md:25` 与 `CLAUDE.md` 的同源断言**

三处同源（`CLAUDE.md` 压缩段、`compaction.md:88`、`adr/0006:25`）都断言「`/model` 切换自动用新窗口」。

**另一处 `CLAUDE.md` 已过时**：「`fuzz-seed` 目前是 `continue-on-error` 的软门禁」—— **W0 已收紧，`governance` 与 `fuzz-seed` 现在都是硬门禁。**

> 若 W2 已改这一条，本步跳过并确认。

- [ ] **Step 5: 提交**

---

## Task 7: 台账翻牌 + W4 收尾验证

- [ ] **Step 1: 翻牌 2 条**

| 条目 id | 现 verdict | 证据来自 |
|---|---|---|
| `E2/PROP1` | partial | Task 5 的「组装结果永不超窗」+ fuzz target |
| `F2/CCL1` | **missing** | Task 1 的 cooldown 真实生效测试 |

⚠️ **`F2/CCL1` 现值是 `missing` 不是 `partial`。**

⚠️ **`F2/CCL1` 的验收含「keepRecent 文档清晰」** —— 依赖 Task 6 Step 3 的 cooldown 一节写完。**文档没写完不能翻。**

⚠️ 若 Task 5 Step 1 的「组装结果永不超窗」暴露了 `Run` 的真实缺陷而本工作包未能修复，**`E2/PROP1` 保持 `partial`** 并移交 —— 与 W1/W3 用同一把尺子。

- [ ] **Step 2: 台账门与计数**

Run: `go test ./internal/archtest -run TestFeatureStatus` → PASS（总数仍为 63）
Run: `go run ./cmd/featurestatus`

- [ ] **Step 3: 修 `docs/feature-status-audit.md` 的行号**

审计两处（`:236`、`:404`）写 `bootstrap.go:794`，实际 **`:825`**。

- [ ] **Step 4: 全量验证**

```bash
go build ./... && go vet ./... && go test ./...
go test ./internal/archtest              # GOV1 分层 + 台账
go run ./cmd/codelines
go run ./cmd/gendocs -config docs/user-guide/configuration.md
go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md
go run ./cmd/api-schema -markdown docs/api/schema.md
go run ./cmd/api-schema -markdown docs/api/resources.md
git diff --exit-code docs/
```

- [ ] **Step 5: 提交**

---

## W4 验收清单

- [ ] `compacting_test.go` 里**不再有**对 `lastCompactTokens` 的手工覆写
- [ ] 同一份历史连续两次 `maybeCompact`，第二次 `did=false` 且 inner summarize 调用数不增
- [ ] 128K provider 的 threshold / cooldown / hard-force **三个门都基于 128K**
- [ ] `go test ./internal/archtest -run TestGOV1` PASS（orchestrator **没有** import `api/http`）
- [ ] `ctxcompact` 至少一个 fuzz target 能跑
- [ ] 配对不变量测试**不再 skip 22%** 的输入
- [ ] `docs/compaction.md` 有 cooldown 一节；`:88` 的错误因果已修
- [ ] 台账 2 条终态（或诚实保持 partial 并写明原因）

## 依赖与移交

| 事项 | 关系 |
|---|---|
| **W1 先落地** | `TurnOpts.ModelID` 复用；Task 3 依赖它 |
| 若「组装结果永不超窗」暴露 `Run` 缺陷且本包未修 | 移交，`E2/PROP1` 保持 `partial` |

## 审计 / 文档中已被证伪的论断（不要照着做）

1. ⚠️ **`docs/compaction.md:88` 的因果推理是错的** —— 按指针缓存会建新实例，但填进去的窗口仍是全局值。**换了实例，没换窗口。**
2. **`CLAUDE.md` 与 `adr/0006:25` 的「`/model` 切换自动用新窗口」在 mid-turn 路径不成立** —— 确认为文档错误。
3. **`CLAUDE.md` 说 `fuzz-seed` 是软门禁已过时** —— W0 已收紧为硬门禁。
4. **审计行号漂移**：`bootstrap.go:794` 实际 `:825`（两处）。
5. **现有 cooldown 测试是假绿** —— 它手工覆写被测状态，从未断言过 `maybeCompact` 实际存的值。
