# S0/W7 可观测 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `yanshi doctor` 的硬编码占位符换成真实探测（顺带让 exit 0 第一次成为可达状态），修好两个让性能基线**根本没在跑**的 bench bug，接通成本渲染、slog 迁移、OTel 的 usage 记录与 feature flag 的两个消费点。

**Architecture:** 五条链路：① doctor 的三个瞎检查；② bench 的两个真 bug + CI 分层；③ 成本渲染最后一跳；④ slog / OTel 接线；⑤ feature flag 消费点。彼此独立，除 ③ 有一个**必须先做的文件拆分**。

**Tech Stack:** Go 1.26.4，`log/slog`，OTLP/HTTP，`benchstat`。

---

## 本计划的写法

**这是意图级计划，不是代码清单。** 每步说清楚：改哪个文件的哪个函数、断言什么行为、怎么观察、为什么重要、预期看到什么。**具体代码由实现者写** —— 你手里有编译器，本文档没有。

「已核对事实」是**实测结果**，可直接依赖；其余标识符请先 grep。

---

## 已核对事实（实测）

### doctor 的三个瞎检查

`internal/cli/doctor.go:512-517` **原文逐字**：

```go
// checkSandbox is a placeholder. Full sandbox verification arrives with S08
// (M2); until then doctor flags it as a known gap so the report is honest
// rather than silently omitting it.
func checkSandbox() CheckResult {
	return CheckResult{Name: "sandbox", Status: StatusWarn, Message: "sandbox verification not implemented yet (arrives with S08 in M2)"}
}
```

**不接任何参数，不读 cfg，无条件返回。** 调用点 `doctor.go:158`。

**实跑**（`yanshi doctor`）：`14 ok, 4 warn, 0 fail`，**exit code 实测 1**。

⚠️ **exit 0 在任何机器上都不可达** —— `ExitCode()`（`doctor.go:65-83`）只要有一个 warn 就返回 1，而 sandbox 恒 warn。**同时踩了两条**：验收标准点名的「退出码 0/1/2」，以及仓库「严禁占位符」的实现规则。

**已在误报**：`internal/sandbox`（`factory.go:33 New`、`types.go:55 CapabilityReport`）与 `config.SandboxConfig`（`config.go:248`）**都已存在**，`bootstrap.go:867-882` 已在装配并打印真实 Report —— **doctor 本可复用却没有。**

| 检查 | 位置 | 缺陷 |
|---|---|---|
| `checkSandbox` | `doctor.go:515` | 不读 cfg，恒定字符串 |
| ⚠️ `checkMCP` | `doctor.go:519` | **收了 cfg 参数却完全不读 `cfg.MCP.Servers`**（`config.go:26` 该字段存在）—— 配多少 server 都恒返回 OK + "no mcp servers exposed via chat"；连它自己的测试（`doctor_test.go:328`）也只断言了默认分支 |
| ⚠️ `checkLSP` | `doctor.go:530` | 只硬编码探 `gopls` **一个**二进制，而 `lsp.DefaultLanguages()`（`internal/lsp/manager.go:33-38`）有 go + python 两种；完全忽略 `cfg.LSP.Enabled`/`Override`（`config.go:654-658`） |

⚠️ **两条现存测试锁死了这个占位，必须一并改**：`doctor_release_test.go:93 TestCheckSandboxStillHonestAboutGap`（断言 message 含 "S08" 或 "M2"）、`doctor_test.go:322`（注释 "Sandbox must remain warn (S08 not done)"）。

> **spec/审计都没记录这条隐性阻塞**：不先改这两条，`checkSandbox` 的替换会以「测试变红」的形式**被误判为回归**。

### BENCH1 的两个真 bug

**① `BenchmarkOrchestratorTurn` 在任何 `b.N>=2` 时必挂**

```
go test ./internal/agent/orchestrator -run XXX -bench=BenchmarkOrchestratorTurn -benchtime=2x
BenchmarkOrchestratorTurn-10  --- FAIL
    orchestrator_bench_test.go:23: orchestrator: no assistant message produced
```

根因：`orchestrator_bench_test.go:14` 的 `NewFakeModel([]string{"hello from agent"}, nil)` **只脚本一条响应且未设 `Repeat`**，`fake.go:159-160` 耗尽后返回空 assistant message。

⚠️ **默认 benchtime（CI 实际用的）和 `-count=5`（`bench.sh` 实际用的）全挂** → `go test -bench=. ./...` 返回 rc=1。**审计的一审用 `-benchtime=1x` 恰好掩盖了它。**

**② bench 输出被 slog 污染，benchstat 静默丢样本**（审计未记）

```
BenchmarkFSEdit/ExactReplace-10   2026/08/03 18:48:40 INFO permission decision tool=fs_edit ...
2026/08/03 18:48:40 INFO permission decision tool=fs_edit ...
       3      172403 ns/op
```

来源 `internal/tools/permctx.go:190` 的 `auditPermission` → slog 默认 handler → stderr。**benchstat 只认「`Benchmark…` 行尾带迭代数与 ns/op」的完整行** —— 这里 Benchmark 名行被日志截断、数值行又不以 Benchmark 开头 → **FSEdit 整条从趋势里消失**。

⚠️ `bench.sh:25` 用 `2>&1 | tee`，把 stderr 灌进基线文件，**正好中招**；`nightly.yml:53` 没有 `2>&1`，artifact 反而干净。**两边都要修。**

### 其余五项 delta

| 项 | 缺口 | 位置 |
|---|---|---|
| **COST1** | ⚠️ `statsEntry.render` **从不读** `s.CostUSD`/`s.CostKnown` —— 数据传到最后一跳被丢掉；其 doc（`:1077`）却写着 "with USD cost"，**是假陈述** | `commands.go:1087-1140` |
| COST1 | ⚠️ 验收测试被**刻意禁用** —— 函数名 `testStatsEntryAggregates..._disabled`（小写 t + `_disabled`，go test 不执行） | `commands_test.go:845` |
| COST1 | `FormatCost` **生产代码零调用**，TUI 自己硬编码 `$%.6f`，**两套格式** | `commands.go:990` |
| COST1 | 内置表只有 claude-*，而 `config.example.yaml` 示例 provider 是 gpt-4o → **开箱即 N/A 且无指引** | `pricing.go:28` |
| **OBS1** | ⚠️ **采样完全没有** —— 整个包无任何 sample/rate-limit 代码，只有 slog Level 过滤。「采样不丢关键错误」无从谈起 | `internal/observe/log/` |
| OBS1 | 全仓真正的 slog 发射点只有 **10 处**；bootstrap 诊断输出仍是裸 stderr | `bootstrap.go:489/492/550/616/881/1365`、`api/http/server.go:239` |
| **OBS2** | ⚠️ `StartSession` / `SetSessionID` / `RecordUsage` 三个导出函数**全仓零生产调用点** → `yanshi.llm.tokens` counter **永不发射** | `internal/observe/otel/` |
| OBS2 | **WS 主路径无任何 otel**；`ws.go:506` 注释至今写着 "and (later) OTel spans"；`orchestrator.go:440-441` 注释说 "manage their own spans at the WS/SSE drain boundary" —— **那个 boundary 从没实现** | — |
| OBS2 | 💡 **现成的缝已摆好但被丢弃**：`ws_compaction.go:89` 的签名是 `addProviderUsage(_ context.Context, ...)` —— **ctx 收了就扔**，正是 `RecordUsage` 该落的地方；调用方 `ws.go:683`/`:862` 传的都是真 turnCtx | — |
| **OBS3** | `DefaultSpecs`（`:80`）三个 flag 里**只有 `observe.otel_export` 有真实消费点**（`bootstrap.go:369`，审计写 `:329` 已漂移）；另两个注册后**全仓非测试零消费** —— 「新功能可灰度」只对 1/3 成立 | `internal/features/features.go` |
| OBS3 | ⚠️ `commands.go:64` 与 `:65` 是**逐字相同的两行** `/features` 注册 → `/help` 里出现两次 | — |

**OBS3 的消费点已就位、只差接线**：`slog_trace_id` 该 gate 在 `ws.go:515` 的 `WithIDs`；`cost_in_status` 该 gate 在 `ws_compaction.go:208-209`（`statusFrame`，`:178` 起）与 `chat.go:311-312`。

⚠️ **`s.featuresReg` 可能为 nil，而 `Registry.Enabled` 对 nil 返回 false，但 `slog_trace_id` 的 Default 是 `true`** —— **直接调会静默关掉**，需要一个**带 fallback 的取值助手**。

### ⚠️ `commands.go` 已逼近 GOV2 上限

`go run ./cmd/codelines` 实测：`internal/cli/tui/commands.go` **943 纯代码行**，限额 1000，**距上限仅 57 行**，且已在 `approaching` 警告名单（`lines_test.go:46` 阈值 900）。**`lineExceptions` 是空 map，没有任何豁免可用。**

---

## 五条裁定

**裁定 1 — `checkSandbox` 只读 `CapabilityReport.Effective` 这一个字段，S1 落地后免重写。**

改签名为接收 root + cfg，用与 `bootstrap.go:867-878` **完全相同**的方式构造 `sandbox.Config`，然后**只读 `Report()` 自报的 `CapabilityReport`**，把 `Effective`/`Backend`/`Reason`/`Enforced`/`CanKillTree` 原样渲进 message。

⚠️ **状态映射只看 `Effective` 这一个字段** —— 绝不看构建标签、不看 phase 常量、不看 `cfg.Enabled`。三种已知 Effective（`os-isolated` / `disabled` / `host-guard-degraded`）**一律 StatusOK**，未知 Effective 才 warn。

**S1 把各平台 adapter 换成真后端时，它们开始返回 `Effective=os-isolated, Enforced=true`，这个函数一行不改就自动开始报告真实强制状态。**

**关于「degraded 报 OK 是否算过度声明」：不算，且必须如此。** Phase 0 是产品当前**已发布、已文档化**的状态，不是环境缺陷；一条每台机器都亮、操作员**无法消除**的 warn 不可操作，且会让 exit 0 永久不可达 —— **正是本次要消除的缺陷本身**。message 里逐字带上 `host-guard-degraded ... OS isolation NOT enforced`，**诚实性由文本承担，不由状态码承担**。

唯一判 warn 的**可操作**缺陷是 `security.sandbox.tier` 拼错 —— `bootstrap.go:867-873` 的 switch 对无法识别的值**静默回落 ReadOnly**。把这段 switch 提成一个共享的解析函数（bootstrap 与 doctor 共用，**同时消掉重复逻辑**），doctor 拿到「无法识别」时报 warn 并指出合法取值。

> ✅ **分层安全已核验**：`internal/cli` 不在 `deps_test.go` 的 `portAllowlists` 里（非 port 包），且 `internal/sandbox` 与 `internal/lsp` 均**零 internal 依赖**（`go list` 确认）—— **不会成环、不违反 GOV1**。

**裁定 2 — `M1/O07` 与 `C4/O07` 是两条独立台账，不合并。**

| 台账 | 角度 | W7 该做什么 |
|---|---|---|
| **`M1/O07`** doctor 子命令 | **交付物本身是否成立**。8 类检查全在且真跑，双渲染在，降级不 panic，secret 不外泄 —— **唯一未满足的是「退出码 0/1/2」中的 0**，被 `checkSandbox` 恒 warn 卡死 | exit 0 可达的端到端断言 + 不完整环境不 panic 的回归保护 + **零泄密断言** |
| **`C4/O07`** doctor 增强 | **每一项检查的质量**。三个硬编码占位或半瞎的检查 | `checkSandbox` + **`checkMCP`** + **`checkLSP`** 三个都修 |

两条的交集只有 `checkSandbox`。**`checkMCP` / `checkLSP` 只属 `C4/O07`。**

⚠️ **spec §4.3 W7 只点了 `checkSandbox`** —— 按验收标准 1，`C4/O07` 的「覆盖各子系统」**必须把另两个也修掉**。**W7 的实际范围比 spec 那一格宽。**

> doctor 已扩到 18 项，比原 plan 多约 10 项（WAL/keyring/LSP/secrets/locale/keymap/高对比度），是**超集 —— 不得删**。

**裁定 3 — bench 分两层：趋势软、能跑硬。**

- **基线存哪**：本地仍是 `.bench-results/old.txt`（`bench.sh:40`），**须补进 `.gitignore`**。**不进仓库** —— committed baseline 在异构 runner 上必然失真。CI 侧基线 = 上一次 nightly 的 artifact，用 `gh run download` 拉回（**不引第三方 action**）
- **回归阈值**：**不设自动阈值**。`bench.sh:19` 的 `THRESHOLD_PCT=20` 保持为**人工评审参考值**，明确标注 informational
- **趋势比对留 `nightly.yml`；新增一道硬门进 `ci.yml`**：`go test -run '^$' -bench=. -benchtime=10x ./internal/vcs ./internal/tools ./internal/agent/orchestrator`（实测 3x 时 vcs 0.9s / tools 0.3s，10x 合计仍在数秒级）

**为什么趋势不能做成硬门**：GitHub 共享 runner 的 ns/op 噪声轻松超 20%，把绝对阈值做成硬门**就是造 flake**，而 flaky 门禁**最后一定被 nolint 掉**。

**为什么「能跑」必须硬**：`BenchmarkOrchestratorTurn` **坏了不知多久而 CI 全绿** —— 这就是软门禁的代价。

⚠️ **`-benchtime` 不能用 `1x`** —— `1x` 恰恰是掩盖本 bug 的那个值。

**裁定 4 — 脱敏是复核而非重写，只做增量补强。**

`redact.go` 的 key 白名单（`:13-20`）**已覆盖验收主体**，做法比只按 key 更稳：`normalizedKey`（`:22`）剥非字母数字并小写（`api_key`/`apiKey`/`API-KEY` 都归一到 `apikey`）；`redactAttr`（`:49`）递归处理 group、`Resolve()` 后再判、`slog.KindAny` 里是 error 的直接整条 redact；`WithAttrs`（`log.go:132`）在绑定前就脱敏。

⚠️ **不要动这套机制。** 只做增量补强（见 Task 8）。

**裁定 5 — `commands.go` 必须先拆，再加 $ 渲染。**
943 行距上限只剩 57 行，而 `lineExceptions` 是空 map 没有豁免可用。**加渲染前先把 `statsEntry`（`:1077-1140`）拆到独立文件。**

---

## Task 1: 拆分 `commands.go`（COST1 的前置）

> **裁定 5。必须最先做。**

**Files:** Create `internal/cli/tui/commands_stats.go`；Modify `internal/cli/tui/commands.go`

- [ ] **Step 1: 确认当前行数**

Run: `go run ./cmd/codelines`
Expected: `commands.go` 约 943 行，在 `approaching` 名单里。

- [ ] **Step 2: 把 `statsEntry` 整体搬到新文件**

**搬什么**：`commands.go:1077-1140` 的 `statsEntry` 及其 `render`。

**风格对齐**：与既有的 `commands_skills.go` / `commands_logs.go` / `commands_session_memory.go` 一致。

⚠️ **纯搬迁，不改行为** —— 这一步不应有任何测试变红。

- [ ] **Step 3: 顺手删掉重复的 `/features` 注册**

`commands.go:64` 与 `:65` 是**逐字相同的两行**。`help.go:41` 遍历 `commandTable` **不去重** → `/help`、Ctrl+K palette、`/` palette **全部把 `features` 显示两遍**。

**实际只有 34 个不同命令，不是 35 个。**

- [ ] **Step 4: 验证与提交**

Run: `go run ./cmd/codelines && go test ./internal/cli/...`

---

## Task 2: 成本渲染最后一跳（COST1）

> 依赖 Task 1。

**Files:** Modify `internal/cli/tui/commands_stats.go`、`internal/llm/eino/pricing.go`（可能）、`config.example.yaml`；Test `internal/cli/tui/commands_test.go`

**背景：** `pricing.go` 全套已有，`pricing.overrides` 已接 bootstrap，WS/SSE 两条计费路径都在，CostUSD/CostKnown 一路 store→proto→TUI 齐全 —— **数据传到最后一跳被丢掉**。

- [ ] **Step 1: 启用被禁用的测试**

**文件**：`commands_test.go:845`

**做什么**：把 `testStatsEntryAggregatesKnownCostAndNamesUnknown_disabled` 改回正常的 `Test...` 命名，让它**真的跑**。

⚠️ **它是照着一个从未落地的 `%.4f` 格式写的** —— 见 Step 3。**先让它跑起来看它怎么红，再决定改哪一边。**

**预期**：FAIL —— `render` 从不读 `CostUSD`。

- [ ] **Step 2: 实现 $ 渲染**

**同时修 doc**：`commands.go:1077` 的 doc 写着 "with USD cost" —— **在渲染真的实现之前，那是假陈述**。

- [ ] **Step 3: 统一到 `FormatCost`**

`FormatCost` **生产代码零调用**（全仓仅测试引用），TUI 自己硬编码 `$%.6f` —— **两套格式**。

⚠️ **统一有连带影响**：`FormatCost` 是**分档精度**（<0.01→`%.4f`，<1→`%.3f`）。统一后：
- `TestStatusEntryRendersKnownAndUnknownCost`（`commands_test.go:837`）断言的 `$0.012345` 会变成 `$0.012`
- Step 1 那个被禁用测试里断言的 `$0.2500` 会变成 `$0.250`

**两个断言都得改。** 这不是回归 —— 是那个禁用测试**照着一个从未存在的格式**写的。

- [ ] **Step 4: 解决「开箱即 N/A」**

内置价表只有 claude-*，而 `config.example.yaml` 的示例 provider 是 **gpt-4o** → 用户开箱就看到 N/A 且**无指引**。

**做什么**：在 `config.example.yaml` 的 `pricing.overrides` 附近加注释说明如何为非 claude 模型配价，或补充内置表。**二选一，写清楚理由。**

- [ ] **Step 5: 全量与提交**

> 改了 `config.example.yaml` 但**没改 config struct** → `gendocs -config` **不应有 diff**。有 diff 说明动了 struct。

---

## Task 3: `checkSandbox` 换真实探测（M1/O07 + C4/O07）

**Files:** Modify `internal/cli/doctor.go`（`:512-517`、调用点 `:158`）、`internal/sandbox/`（提取共享的 tier 解析）、`internal/bootstrap/bootstrap.go`（`:867-873`）；Test `internal/cli/doctor_test.go`、`doctor_release_test.go`

- [ ] **Step 1: 先改锁死占位的两条测试**

⚠️ **这是 spec/审计都没记录的隐性阻塞**：不先改，替换会以「测试变红」的形式**被误判为回归**。

| 测试 | 位置 | 现状 |
|---|---|---|
| `TestCheckSandboxStillHonestAboutGap` | `doctor_release_test.go:93` | 断言 message 含 "S08" 或 "M2" |
| （sandbox 必须 warn） | `doctor_test.go:322` | 注释 "Sandbox must remain warn (S08 not done)" |

**改成什么**：断言 message **逐字包含 `Effective` 的取值**（如 `host-guard-degraded`）与「OS isolation NOT enforced」这类诚实文本 —— **诚实性由文本承担**。

- [ ] **Step 2: 写 exit 0 可达的失败测试**

**断言什么**：在一台配置正确的机器上，`yanshi doctor` 的退出码**可以是 0**。

**为什么重要**：这是 `M1/O07` 验收里「退出码 0/1/2」唯一未满足的一档，**在任何机器上都不可达**。

**预期**：FAIL，exit 1。

- [ ] **Step 3: 实现（裁定 1）**

**只读 `Report()` 的 `CapabilityReport`**，状态映射**只看 `Effective`**。三种已知值一律 OK，未知才 warn。

**提取 tier 解析为共享函数**，bootstrap 与 doctor 共用 —— 同时消掉重复逻辑，并让 doctor 能报告「tier 拼错」这个**唯一可操作**的缺陷。

⚠️ **绝不看构建标签、不看 phase 常量、不看 `cfg.Enabled`** —— 那样写的话 S1 落地时要重写。

- [ ] **Step 4: 全量与提交**

Run: `go test ./internal/cli/... ./internal/archtest && go test ./...`

> `go test ./internal/archtest` 确认 GOV1 分层没被破坏。

---

## Task 4: `checkMCP` 与 `checkLSP` 换真实探测（C4/O07）

> **裁定 2：spec §4.3 W7 没点这两个，但 `C4/O07` 的「覆盖各子系统」要求修。**

**Files:** Modify `internal/cli/doctor.go`（`:519`、`:530`）；Test `internal/cli/doctor_test.go`

- [ ] **Step 1: 写失败测试**

**断言什么**：
1. 配了 N 个 MCP server 时，`checkMCP` **报告出这 N 个**（当前恒返回 "no mcp servers exposed via chat"）
2. `checkLSP` 覆盖 `lsp.DefaultLanguages()` 的**全部**语言（go + python），**且**尊重 `cfg.LSP.Enabled` / `Override`

**为什么重要**：`checkMCP` **收了 cfg 参数却完全不读 `cfg.MCP.Servers`** —— 连它自己的测试（`doctor_test.go:328`）也只断言了默认分支，所以这个瞎检查一直是绿的。

**预期**：两条 FAIL。

- [ ] **Step 2: 实现**

⚠️ `checkLSP` 只硬编码探 `gopls` 一个二进制，而 `DefaultLanguages()`（`internal/lsp/manager.go:33-38`）有两种。

> 与 W6 的 LSP1 相关：W6 Task 9 会重验 `DefaultLanguages` 的覆盖面。**若 W6 已扩展了它，本任务自动受益** —— 但**不要依赖它**，本任务只需正确遍历当前的 `DefaultLanguages()`。

- [ ] **Step 3: 全量与提交**

---

## Task 5: doctor 零泄密回归保护（M1/O07）

**Files:** Test `internal/cli/doctor_test.go`

**背景：** doctor 输出**零泄密已验证，且机制是设计出来的不是巧合**。三处承重：`checkConfig`（`:192-196`）在 load 失败时**故意不回显 `cfgErr`**（config 解析失败常把 raw api_key 带进错误文本）；`skipped()`（`:181`）同理；`checkProviders`（`:353-389`）只报 "set/not set"；`checkSecretsRefs`（`:593`）拒绝时只说 "invalid credential reference"；`checkKeymapConfig`（`:633`）明确不回显原始 key/action。

⚠️ **但目前没有任何测试断言「raw api_key 不出现在 doctor 输出里」** —— **该属性只是「碰巧成立」。**

- [ ] **Step 1: 写 canary 测试**

**怎么观察**：`auth.legacy_insecure: true`（`config.go:450`/`:486` 的 gate 认这个开关）+ 一个 canary 明文 key，跑 `RunDoctor` 后**对 `RenderText` 与 `RenderJSON` 双双断言 canary 不出现**。

**为什么两个都要断**：JSON 渲染路径与文本渲染路径**是两段独立代码**，只测一个等于只保护一半。

- [ ] **Step 2: 补「不完整环境不 panic」的回归保护**

`skipped()`（`:181`）保证 config 失败降级不 panic —— **验收明写这条，但要有测试守住**。

- [ ] **Step 3: 提交**

---

## Task 6: 修 bench 的两个真 bug（BENCH1）

**Files:** Modify `internal/agent/orchestrator/orchestrator_bench_test.go`（`:14`）、bench 文件的 slog 设置、`scripts/bench.sh`（`:25`）、`.gitignore`

- [ ] **Step 1: 复现两个 bug**

Run: `go test ./internal/agent/orchestrator -run XXX -bench=BenchmarkOrchestratorTurn -benchtime=2x`
Expected: **FAIL**，`orchestrator: no assistant message produced`

Run: `go test -bench=. ./internal/tools 2>&1 | head -20`
Expected: 看到 `INFO permission decision` 混在 Benchmark 输出里。

⚠️ **不要用 `-benchtime=1x` 验证** —— `1x` 恰恰是掩盖 bug ① 的那个值。

- [ ] **Step 2: 修 bug ①**

给 `NewFakeModel` 设 `Repeat=true`（`fake.go:159-160` 在耗尽后返回空 assistant message）。

- [ ] **Step 3: 修 bug ②（治本 + 治标）**

**治本**：bench 里装一个 discard slog handler。
**治标**：去掉 `bench.sh:25` 的 `2>&1`（它把 stderr 灌进基线文件，**正好中招**；`nightly.yml:53` 没有 `2>&1`，artifact 反而干净）。

- [ ] **Step 4: 补 `.gitignore`**

`.bench-results/` **不在 `.gitignore` 里**（审计没记）。

- [ ] **Step 5: 验证与提交**

Run: `go test -run '^$' -bench=. -benchtime=10x ./internal/vcs ./internal/tools ./internal/agent/orchestrator`
Expected: **全部跑完且 rc=0**。

---

## Task 7: bench 的 CI 分层（BENCH1）

> **裁定 3。**

**Files:** Modify `.github/workflows/ci.yml`、`.github/workflows/nightly.yml`（`:38` 注释、`:53`）

- [ ] **Step 1: 在 `ci.yml` 加「benchmark 能跑」硬门**

**跑什么**：`go test -run '^$' -bench=. -benchtime=10x ./internal/vcs ./internal/tools ./internal/agent/orchestrator`

**为什么是硬门**：`BenchmarkOrchestratorTurn` 坏了不知多久而 CI 全绿 —— **这就是软门禁的代价**。

⚠️ **不能用 `-benchtime=1x`。**

- [ ] **Step 2: `nightly.yml` 改走 `bench.sh` 并做趋势比对**

**现状**：`nightly.yml:53` 直接 `go test -bench=. ./...`，**不调 `bench.sh`** —— CI 侧从不跑 benchstat，只传原始 artifact，「记录趋势」靠人工比对。

**改成**：基线用 `gh run download` 拉上一次 nightly 的 artifact（**不引第三方 action**），跑 `benchstat old new` 打进 job summary。

⚠️ **保持趋势为软失败** —— 共享 runner 的 ns/op 噪声轻松超 20%，硬门就是造 flake。

- [ ] **Step 3: 修 `nightly.yml:38` 的过期注释 + 标注阈值为 informational**

`bench.sh:19` 的 `THRESHOLD_PCT=20` **明确标注为人工评审参考值**。

- [ ] **Step 4: 提交**

---

## Task 8: slog 迁移与采样（OBS1）

**Files:** Modify `internal/observe/log/`、`internal/bootstrap/bootstrap.go`、`internal/api/http/server.go`（`:239`）；Test

- [ ] **Step 1: 实现采样**

⚠️ **整个包无任何 sample/rate-limit 代码** —— 只有 slog Level 过滤。「采样不丢关键错误」这条验收**无从谈起**。

**断言什么**：高频 INFO 被采样，而 **ERROR 级别一条不丢**。

- [ ] **Step 2: 迁移 5 处裸 stderr**

`bootstrap.go:489`/`:492`（auto-migrate）、`:550`（lsp disabled）、`:616`（vision disabled）、`:881`（**sandbox phase0，安全相关**）、`:1365`（mcp server failed），以及 `api/http/server.go:239`。

⚠️ **两处不能迁**：
- `bootstrap.go:334`（logs→path）—— **TUI 模式下故意给用户看的**
- `bootstrap.go:1068`（日志文件打不开）—— 在 `resolveLogWriter` 内，跑在 `obslog.Setup`（`:325`）**之前**

> ✅ 已 grep 确认**无测试依赖**上述 stderr 字符串。

- [ ] **Step 3: 增量补强脱敏白名单（裁定 4）**

**key 侧补**：`x-api-key`（归一为 `xapikey`，**不在当前表内**）、`credential(s)`、`passphrase`、`cookie`、`clientsecret`、`privatekey`，以及内容类 `content`/`text`/`body`/`stdout`/`stderr`、路径类 `file`/`filename`/`dir`/`cwd`。

**值侧补**：GitHub（`ghp_`/`gho_`/`github_pat_`）、GitLab（`glpat-`）、AWS（`akia`/`asia`）、Google（`aiza`）、PEM（`-----begin`）、JWT（`eyj`）。

⚠️ **`looksSensitiveValue` 先 `ToLower`，新前缀必须写成小写形式。**

⚠️ **只做增量，不改结构**（裁定 4）。

- [ ] **Step 4: 全量与提交**

---

## Task 9: OTel 的 usage 记录与 turn span（OBS2）

**Files:** Modify `internal/api/http/ws_compaction.go`（`:89`）、`internal/api/http/{ws,chat}.go`；Test

- [ ] **Step 1: 写失败测试**

**断言什么**：一次 WS turn 与一次 SSE turn 之后，`yanshi.llm.tokens` counter **有发射**。

**为什么重要**：`StartSession` / `SetSessionID` / `RecordUsage` 三个导出函数**全仓零生产调用点** → 该 counter **永不发射**，「token 可观测」不成立。

**预期**：FAIL，零发射。

- [ ] **Step 2: 接 `RecordUsage`**

💡 **现成的缝已经摆好但被丢弃**：`ws_compaction.go:89` 的签名是 `addProviderUsage(_ context.Context, ...)` —— **ctx 收了就扔**，正是 `RecordUsage` 该落的地方。调用方 `ws.go:683`/`:862` 传的都是**真 turnCtx**。

- [ ] **Step 3: 在 WS/SSE drain boundary 加 turn span**

`ws.go:506` 的注释至今写着 "and (later) OTel spans"；`orchestrator.go:440-441` 的注释明说 "Streaming Events* paths manage their own spans at the WS/SSE drain boundary" —— **那个 boundary 从没实现**（`:444` 的 `StartTurn` 只在同步 `Query` 路径）。

**做完后把这两处注释改成描述现实。**

- [ ] **Step 4: 全量与提交**

---

## Task 10: feature flag 的两个消费点（OBS3）

**Files:** Modify `internal/api/http/{ws,ws_compaction,chat}.go`；Test

**背景：** 三个 flag 里**只有 `observe.otel_export` 有真实消费点**（`bootstrap.go:369`）。另两个注册后**全仓非测试零消费** —— 「新功能可灰度」**只对 1/3 成立**。

- [ ] **Step 1: 写失败测试**

**断言什么**：
1. 关掉 `observe.slog_trace_id` 后，日志里**不再有** trace id
2. 关掉 `observe.cost_in_status` 后，status 帧里**不再有**成本字段
3. **registry 为 nil 时，两个 flag 都回落到各自的 Default**

**为什么第 3 条关键**：`s.featuresReg` 可能为 nil，而 `Registry.Enabled` 对 nil **返回 false**，但 `slog_trace_id` 的 **Default 是 `true`** —— **直接调会静默关掉一个默认开启的功能**。

**预期**：三条 FAIL。

- [ ] **Step 2: 写带 fallback 的取值助手**

⚠️ **这是第 3 条测试的实现** —— 不能到处写 `reg.Enabled(...)`。

- [ ] **Step 3: 接两个 gate**

`slog_trace_id` → `ws.go:515` 的 `WithIDs`；`cost_in_status` → `ws_compaction.go:208-209`（`statusFrame`，`:178` 起）与 `chat.go:311-312`。

- [ ] **Step 4: 全量与提交**

---

## Task 11: 台账翻牌 + 文档同步 + W7 收尾验证

- [ ] **Step 1: 翻牌 7 条**

| 条目 id | 证据来自 |
|---|---|
| `C4/COST1` | Task 2（含被启用的那条测试） |
| `C4/OBS1` | Task 8 |
| `C4/OBS2` | Task 9 |
| `C4/OBS3` | Task 10 |
| `F2/BENCH1` | Task 6/7 |
| `M1/O07` | Task 3（exit 0）+ Task 5（零泄密） |
| `C4/O07` | Task 3 + Task 4 |

⚠️ 任何验收未满足的**保持 `partial` 并写明原因** —— 与 W1–W6 用同一把尺子。

- [ ] **Step 2: 处理 W1 移交的 `G/VISION-TOOL` 的 `/cost` 部分**

W1 因「费用纳入 `/cost`」未处理而让 `G/VISION-TOOL` 保持 `partial`。

**做什么**：确认图像 token 是否已随 Task 2 的成本渲染一并纳入。

- 若已纳入 → **可翻牌 `G/VISION-TOOL`**（前提是它的其余验收也已满足，含 W8 的 `@path` TUI 触发面）
- 若未纳入 → 补上，或**诚实移交给 W8**

- [ ] **Step 3: 修审计的行号错误**

审计 OBS3 写 `observe.otel_export` 消费点在 `bootstrap.go:329`，实际 **`:369`**。
非阻塞：审计引 `help.go:43 collectHelpEntries`，函数实际在 **`:41`**。

- [ ] **Step 4: 全量验证**

```bash
go build ./... && go vet ./... && go test ./...
go test ./internal/archtest              # GOV1 分层 + GOV2 行数 + 台账
go run ./cmd/codelines                   # 确认 commands.go 已降下来
go test -run '^$' -bench=. -benchtime=10x ./internal/vcs ./internal/tools ./internal/agent/orchestrator
go run ./cmd/gendocs -config docs/user-guide/configuration.md
go run ./cmd/gendocs -help-all docs/user-guide/tui.md docs/user-guide/entrypoints.md
go run ./cmd/api-schema -markdown docs/api/schema.md
go run ./cmd/api-schema -markdown docs/api/resources.md
git diff --exit-code docs/
```

- [ ] **Step 5: 提交**

---

## W7 验收清单

- [ ] `yanshi doctor` 在配置正确的机器上**退出码为 0**
- [ ] `checkSandbox` **只读 `CapabilityReport.Effective`**，message 逐字带 `Effective` 取值
- [ ] `checkMCP` **真的报告** `cfg.MCP.Servers`；`checkLSP` 覆盖全部 `DefaultLanguages()`
- [ ] canary 明文 key **不出现在** `RenderText` 与 `RenderJSON` 任何一个的输出里
- [ ] `go test -bench=. -benchtime=10x` 三个包**全部跑完且 rc=0**
- [ ] bench 输出**无 slog 污染**；`.bench-results/` 已进 `.gitignore`
- [ ] `ci.yml` 有「benchmark 能跑」硬门（**非 `1x`**）
- [ ] `/help` 里 `/features` **只出现一次**
- [ ] `commands.go` 行数**已降下来**，不再在 `approaching` 名单
- [ ] `yanshi.llm.tokens` counter **有发射**
- [ ] registry 为 nil 时 `slog_trace_id` 回落到 **`true`**
- [ ] ERROR 级别日志**不被采样丢弃**

## 依赖与移交

| 事项 | 关系 |
|---|---|
| `checkSandbox` 与 S1 | 裁定 1 保证 **S1 落地后一行不改** |
| `checkLSP` 与 W6 的 LSP1 | W6 若扩展 `DefaultLanguages()`，本任务自动受益；**不要依赖它** |
| `G/VISION-TOOL` 的 `/cost` | W1 移交，Task 11 Step 2 处理；可能再移交 W8 |
| `fs_mkdir` 在 `frecency.go:217` 等三处 | **W1 与 W8 重叠区**，W7 不碰 |

## 审计 / spec 中已被证伪的论断（不要照着做）

1. ⚠️ **审计 BENCH1 说「两处细节偏差，均不影响验收」，与它自己的二审自相矛盾** —— 二审明确复现了 `BenchmarkOrchestratorTurn` 必挂并指出 `go test -bench=. ./...` rc=1。**这不是「不影响验收」，「CI 记录趋势」直接不成立。**
2. **审计漏了 bench 输出被 slog 污染这条**（benchstat 静默丢样本）。
3. ⚠️ **spec §4.3 W7 只点了 `checkSandbox`**，没提 `checkMCP` 与 `checkLSP` 这两个同类占位/半瞎检查。**W7 的实际范围比 spec 那一格宽。**
4. ⚠️ **`doctor_release_test.go:93` 与 `doctor_test.go:322` 两条现存测试主动锁死了占位行为** —— spec/审计都没记录的**隐性阻塞**。
5. **审计 OBS3 行号错**：`bootstrap.go:329` 实际 `:369`。
6. **审计 OBS2 表述不够准** —— 只说 WS/SSE 零 otel（属实），没提 `orchestrator.go:444` 已有一处 `StartTurn`（挂在同步 `Query` 路径）。结论不变，但「零 `StartTurn` 调用点」的说法会误导。
7. **审计没记 `.bench-results/` 不在 `.gitignore`。**
