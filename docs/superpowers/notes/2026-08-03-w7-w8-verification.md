# W7 / W8 核验报告（合并）

> 2026-08-03 实测。所有行号为亲自核对的当前值。

---

# W7 可观测

## 1. 七项 delta

### `C4/COST1`（$ 成本估算）

**已有**：`internal/llm/eino/pricing.go` 全套（`DefaultPricing:28` 八个 claude-*、`MergePricing:44`、`CostOK:58` 缓存价单独计、`Ledger:107`、`FormatCost:141`）；`pricing.overrides` 已接 bootstrap；WS/SSE 两条计费路径都在（`ws_compaction.go:89 addProviderUsage`、`chat.go:155 sseOnUsage`）；CostUSD/CostKnown 一路 store→proto→TUI 齐全。

**差**：
1. ⚠️ `statsEntry.render`（`commands.go:1087-1140`）**从不读** `s.CostUSD`/`s.CostKnown` —— 数据传到最后一跳被丢掉；其 doc（`:1077`）却写着 "with USD cost"，**是假陈述**
2. 对应验收测试被**刻意禁用**：`commands_test.go:845` 函数名 `testStatsEntryAggregatesKnownCostAndNamesUnknown_disabled`（小写 t + `_disabled`，go test 不执行）
3. `FormatCost` **生产代码零调用**（全仓仅测试引用），TUI 自己硬编码 `commands.go:990` 的 `$%.6f`，**两套格式**
4. 内置表只有 claude-*，`config.example.yaml` 示例 provider 是 gpt-4o，**开箱即 N/A 且无指引**

⚠️ **修 ③ 有连带影响**：`FormatCost` 是分档精度（<0.01→%.4f，<1→%.3f）。统一后 `TestStatusEntryRendersKnownAndUnknownCost`（`commands_test.go:837`）断言的 `$0.012345` 会变成 `$0.012`；被禁用测试里断言的 `$0.2500` 会变成 `$0.250`。**两个断言都得改**——那个禁用测试是照着一个从未落地的 `%.4f` 格式写的。

⚠️ **`internal/cli/tui/commands.go` 当前 943 纯代码行**，`lineExceptions` 是空 map（`lines_test.go:20`），**距 1000 只剩 57 行**。加 $ 渲染前必须先把 `statsEntry`（`:1077-1140`）拆到独立文件。

### `C4/OBS1`（slog）

**已有**：`internal/observe/log/log.go` 的 `redactHandler`（`:104`/`:110` Handle、`:132` WithAttrs 预绑定也脱敏）、`New:141`/`Setup:157`、context 注入 trace/session/turn/tool、`SafeErrorType:165`（只出 `%T` 不出 err body）、`ParseLevel:91`。

**差**：
1. ⚠️ **采样完全没有** —— 整个包无任何 sample/rate-limit 代码，只有 slog Level 过滤。「采样不丢关键错误」这条无从谈起
2. 全仓真正的 slog 发射点只有 **10 处**；bootstrap 的诊断输出仍是裸 stderr：`bootstrap.go:489/:492`（auto-migrate）、`:550`（lsp disabled）、`:616`（vision disabled）、`:881`（**sandbox phase0，安全相关**）、`:1365`（mcp server failed），以及 `api/http/server.go:239`

⚠️ `bootstrap.go:334`（logs→path）与 `:1068`（日志文件打不开）**不能迁** —— 前者是 TUI 模式下故意给用户看的，后者在 `resolveLogWriter` 内、跑在 `obslog.Setup`（`:325`）**之前**。已 grep 确认无测试依赖上述 stderr 字符串。

### `C4/OBS2`（OTel）

**已有**：`otel.go:140 Setup` 真 OTLP/HTTP 双 exporter + ParentBased(TraceIDRatioBased) + PeriodicReader + 失败软降级 noop；`instrument.go` 六个 instrument 齐全；生产调用点三个 —— `tools/guard.go:199 StartTool`、`orchestrator.go:444 StartTurn`、`llm/eino/resilient.go:368 RecordRetry`。

**差**：
1. ⚠️ `StartSession` / `SetSessionID` / `RecordUsage` 三个导出函数**全仓零生产调用点** → `yanshi.llm.tokens` counter **永不发射**，「token 可观测」不成立
2. **WS 主路径无任何 otel**：`ws.go:506` 的注释至今写着 "and (later) OTel spans"；`orchestrator.go:440-441` 的注释明说 "Streaming Events* paths manage their own spans at the WS/SSE drain boundary" —— **那个 boundary 从没实现**（`:444` 的 StartTurn 只在同步 `Query` 路径）
3. **SSE（`chat.go`）零 otel**
4. 💡 **现成的缝已经摆好但被丢弃**：`ws_compaction.go:89` 的签名是 `addProviderUsage(_ context.Context, ...)` —— **ctx 收了就扔**，正是 `RecordUsage` 该落的地方；调用方 `ws.go:683`/`:862` 传的都是真 turnCtx

### `C4/OBS3`（feature flags）

**已有**：`internal/features/features.go` 三档 Stage、`Register:91`（重复/缺字段 panic）、`Enabled:108`、`Set:123`（始终拒未知）、`ApplyMap:140`（strict 模式**原子**拒批）、`List:164`；WS 控制帧 + TUI `/features` 都通。

**差**：
1. `DefaultSpecs`（`:80`）三个 flag 里**只有 `observe.otel_export` 有真实消费点**（`bootstrap.go:369`）。`observe.slog_trace_id`(`:82`) 与 `observe.cost_in_status`(`:84`) 注册后**全仓非测试零消费** —— 「新功能可灰度」只对 1/3 成立
2. ⚠️ `commands.go:64` 与 `:65` 是**逐字相同的两行** `/features` 注册；`help.go:43` 遍历 `commandTable` 不去重 → `/help` 里 `/features` 出现两次

**消费点已就位、只差接线**：trace_id 该 gate 在 `ws.go:515` 的 `WithIDs`；cost_in_status 该 gate 在 `ws_compaction.go:208-209`（`statusFrame`，`:178` 起）与 `chat.go:311-312`。⚠️ 注意 `s.featuresReg` 可能为 nil，而 `Registry.Enabled` 对 nil 返回 false，但 `slog_trace_id` 的 Default 是 **true** —— 直接调会静默关掉，**需要一个带 fallback 的取值助手**。

### `F2/BENCH1`（性能基线）

**已有**：三个 bench 文件（`internal/vcs/vcs_bench_test.go` VCSCommit/DAGApply、`internal/tools/fs_bench_test.go` FSEdit、`internal/agent/orchestrator/orchestrator_bench_test.go`）；`scripts/bench.sh` 含 benchstat 比对与 `THRESHOLD_PCT=20`（`:19`）；`nightly.yml` 已有 bench job（`:35-60`）。

**差**（两个真 bug，见 §5）+ `nightly.yml:38` 注释过期 + `:53` 直接 `go test -bench=. ./...` 不调 `bench.sh`（CI 侧从不跑 benchstat，只传原始 artifact，「记录趋势」靠人工比对）+ `.bench-results/` 不在 `.gitignore`。

## 2. `checkSandbox` 实测

`internal/cli/doctor.go:512-517` 原文逐字：

```go
// checkSandbox is a placeholder. Full sandbox verification arrives with S08
// (M2); until then doctor flags it as a known gap so the report is honest
// rather than silently omitting it.
func checkSandbox() CheckResult {
	return CheckResult{Name: "sandbox", Status: StatusWarn, Message: "sandbox verification not implemented yet (arrives with S08 in M2)"}
}
```

调用点 `doctor.go:158`。**不接任何参数**，不读 cfg，无条件返回。

**实跑**（`go build -o /tmp/yanshi_w7 ./cmd/yanshi && /tmp/yanshi_w7 doctor`）：

```
[OK]   directories      2 director(ies) ok
[WARN] sandbox          sandbox verification not implemented yet (arrives with S08 in M2)
[OK]   mcp              no mcp servers exposed via chat (vcs-mcp serves the ACP path)
14 ok, 4 warn, 0 fail
```

exit code 实测 **1**。

**exit 0 是否真不可达：是，在任何机器上都不可达。** `ExitCode()`（`doctor.go:65-83`）只要有一个 warn 就返回 1，而 sandbox 恒 warn。**同时踩了两条**：验收标准点名的「退出码 0/1/2」，以及仓库「严禁占位符」的实现规则。

已在误报：`internal/sandbox`（`factory.go:33 New`、`types.go:55 CapabilityReport`）与 `config.SandboxConfig`（`config.go:248`）都已存在，`bootstrap.go:867-882` 已在装配并打印真实 Report —— **doctor 本可复用却没有**。

⚠️ **两条现存测试锁死了这个占位，必须一并改**：`doctor_release_test.go:93 TestCheckSandboxStillHonestAboutGap`（断言 message 含 "S08" 或 "M2"）、`doctor_test.go:322`（注释 "Sandbox must remain warn (S08 not done)"）。

## 3. `checkSandbox` 替代设计（S1 落地后免重写）

改签名为 `checkSandbox(root string, cfg *config.Config, cfgErr error)`，用与 `bootstrap.go:867-878` **完全相同**的方式构造 `sandbox.Config`，然后**只读 `sandbox.New(...).Report()` 自报的 `CapabilityReport`**，把 `Effective`/`Backend`/`Reason`/`Enforced`/`CanKillTree` 原样渲进 message。

**状态映射只看 `Effective` 这一个字段**，绝不看构建标签、不看 phase 常量、不看 `cfg.Enabled`。三种已知 Effective（`os-isolated`/`disabled`/`host-guard-degraded`）一律 **StatusOK**，未知 Effective 才 warn。S1 把各平台 adapter 换成真后端时，它们开始返回 `Effective=os-isolated, Enforced=true`，**这个函数一行不改就自动开始报告真实强制状态**。

唯一判 warn 的**可操作**缺陷是 `security.sandbox.tier` 拼错 —— `bootstrap.go:867-873` 的 switch 对无法识别的值静默回落 ReadOnly，把这段 switch 提成 `sandbox.ParseTier(s) (AccessTier, bool)` 由 bootstrap 与 doctor 共用（同时消掉重复逻辑），doctor 拿 `ok=false` 报 warn 并指出合法取值。

**关于「degraded 报 OK 是否算过度声明」：不算，且必须如此** —— Phase 0 是产品当前**已发布、已文档化**的状态，不是环境缺陷；一条每台机器都亮、操作员无法消除的 warn 不可操作，且会让 exit 0 永久不可达，正是本次要消除的缺陷本身。message 里逐字带上 `host-guard-degraded ... OS isolation NOT enforced`，**诚实性由文本承担，不由状态码承担**。

已核验分层安全：`internal/cli` 不在 `deps_test.go` 的 `portAllowlists` 里（非 port 包），且 `internal/sandbox` 与 `internal/lsp` 均零 internal 依赖（`go list` 确认），**不会成环、不违反 GOV1**。

## 4. `M1/O07` 与 `C4/O07` 的区别（两条独立台账，不合并）

**`M1/O07` — doctor 自检子命令**（审计 `:452`）
验收：`检查 config/DB/provider/ACP CLI/端口/lockfile/目录/sandbox；人类可读 + --json；退出码 0/1/2；不打印 secret；不完整环境不 panic`
角度 = **子命令这个交付物本身是否成立**。
现状：8 类检查全在且真跑，`RenderText:104`/`RenderJSON:125` 双渲染在，`skipped():181` 保证 config 失败降级不 panic，secret 不外泄。**唯一未满足的是「退出码 0/1/2」中的 0**——被 checkSandbox 恒 warn 卡死。
→ W7 该给它的是：exit 0 可达的端到端断言 + 不完整环境不 panic 的回归保护 + 零泄密断言。

**`C4/O07` — doctor 增强**（审计 `:622`）
验收：`覆盖各子系统；JSON 可机读；失败明确指引`
角度 = **每一项检查的质量**。审计点名**三个**硬编码占位或半瞎的检查：
- `checkSandbox`（`:515`）—— 不读 cfg，恒定字符串
- ⚠️ `checkMCP`（`:519`）—— **收了 cfg 参数却完全不读 `cfg.MCP.Servers`**（`config.go:26` 该字段存在），配多少 server 都恒返回 OK + "no mcp servers exposed via chat"；连它自己的测试 `TestCheckMCP_ServersListedOrNoneConfigured`（`doctor_test.go:328`）也只断言了默认分支
- ⚠️ `checkLSP`（`:530`）—— 只硬编码探 `gopls` 一个二进制，而 `lsp.DefaultLanguages()`（`internal/lsp/manager.go:33-38`）有 go + python(`pyright-langserver`) 两种，且完全忽略 `cfg.LSP.Enabled`/`cfg.LSP.Override`（`config.go:654-658`）

两条的交集只有 checkSandbox；`M1/O07` 关心「它让 exit 0 不可达」，`C4/O07` 关心「它是三个瞎检查之一」。**checkMCP / checkLSP 只属 `C4/O07`**。

doctor 已扩到 18 项，比原 plan 多约 10 项（WAL/keyring/LSP/secrets/locale/keymap/高对比度），是超集 —— **不得删**。

## 5. BENCH1：两个必须先修的真 bug

### ① `BenchmarkOrchestratorTurn` 在任何 `b.N>=2` 时必挂

实跑复现：
```
go test ./internal/agent/orchestrator -run XXX -bench=BenchmarkOrchestratorTurn -benchtime=2x
BenchmarkOrchestratorTurn-10  --- FAIL
    orchestrator_bench_test.go:23: orchestrator: no assistant message produced
```

根因：`orchestrator_bench_test.go:14` 的 `NewFakeModel([]string{"hello from agent"}, nil)` **只脚本一条响应且未设 `Repeat`**，`fake.go:159-160` 在耗尽后返回空 assistant message。默认 benchtime（CI 实际用的）和 `-count=5`（bench.sh 实际用的）**全挂** → `go test -bench=. ./...` 返回 rc=1。

⚠️ **审计的一审用 `-benchtime=1x` 恰好掩盖了它。** 修法：给 FakeModel 设 `Repeat=true`。

### ② 审计未记录的新发现：bench 输出被 slog 污染，benchstat 静默丢样本

实跑 `go test -bench=. ./internal/tools`：
```
BenchmarkFSEdit/ExactReplace-10   2026/08/03 18:48:40 INFO permission decision tool=fs_edit ...
2026/08/03 18:48:40 INFO permission decision tool=fs_edit ...
       3      172403 ns/op
```

来源 `internal/tools/permctx.go:190` 的 `auditPermission` → slog 默认 handler → stderr。**benchstat 只认「`Benchmark…` 行尾带迭代数与 ns/op」的完整行**；这里 Benchmark 名行被日志截断、数值行又不以 Benchmark 开头 → **FSEdit 整条从趋势里消失**。

而 `bench.sh:25` 用的是 `2>&1 | tee`，把 stderr 灌进基线文件，**正好中招**；`nightly.yml:53` 没有 `2>&1`，artifact 反而干净。两边都要修：bench 里装一个 discard slog handler（治本），并去掉 `bench.sh` 的 `2>&1`。

### 落点判断

- **基线存哪**：本地仍是 `.bench-results/old.txt`（`bench.sh:40`），须补进 `.gitignore`。**不进仓库**——committed baseline 在异构 runner 上必然失真。CI 侧基线 = 上一次 nightly 的 artifact，用 `gh run download` 拉回（不引第三方 action），跑 `benchstat old new` 打进 job summary
- **回归阈值**：**不设自动阈值**。`bench.sh:19` 的 `THRESHOLD_PCT=20` 保持为**人工评审参考值**，明确标注 informational
- **哪个 CI job**：拆两层。趋势比对留 `nightly.yml`；**新增一道硬门**进 `ci.yml`：`go test -run '^$' -bench=. -benchtime=10x ./internal/vcs ./internal/tools ./internal/agent/orchestrator`（实测 3x 时 vcs 0.9s / tools 0.3s，10x 合计仍在数秒级）
- **nightly 的 trend-only + 软失败够不够**：**对「发现回归」够，对「基准真的在跑」不够**。GitHub 共享 runner 的 ns/op 噪声轻松超 20%，把绝对阈值做成硬门就是造 flake，而 flaky 门禁最后一定被 nolint 掉——**趋势软是正确的**。但「benchmark 编译得过且跑得完」必须硬：这次的 `BenchmarkOrchestratorTurn` 坏了不知多久而 CI 全绿，就是软门禁的代价。⚠️ **`-benchtime` 不能用 `1x`——`1x` 恰恰是掩盖本 bug 的那个值**

## 6. 覆盖率 / 脱敏现状

**`redact.go` 的 key 白名单（`:13-20`）已覆盖验收主体**，做法比只按 key 更稳：`normalizedKey`（`:22`）剥掉非字母数字并小写，所以 `api_key`/`apiKey`/`API-KEY` 都归一到 `apikey`；`redactAttr`（`:49`）递归处理 group、`Resolve()` 后再判、`slog.KindAny` 里是 error 的直接整条 redact；`WithAttrs`（`log.go:132`）在绑定前就脱敏。

当前白名单：apikey/authorization/password/secret/token/accesstoken/refreshtoken/prompt/messages/input/args/argsjson/toolargs/arguments/command/path/paths/host/url/headers/query。值兜底 `looksSensitiveValue`（`:36`）识别 `sk-`/`bearer `/`xoxb-`/`xoxp-`/`api_key=`/`authorization:`。

**结论是复核而非重写 —— 不要动这套机制。** 可识别的补强口子（增量，不改结构）：
- key 侧缺 `x-api-key`（归一为 `xapikey`，不在表内）、`credential(s)`、`passphrase`、`cookie`、`clientsecret`、`privatekey`，以及内容类 `content`/`text`/`body`/`stdout`/`stderr`、路径类 `file`/`filename`/`dir`/`cwd`
- 值侧缺 GitHub（`ghp_`/`gho_`/`github_pat_`）、GitLab（`glpat-`）、AWS（`akia`/`asia`）、Google（`aiza`）、PEM（`-----begin`）、JWT（`eyj`）。⚠️ `looksSensitiveValue` 先 `ToLower`，**新前缀必须写成小写形式**

**doctor 输出零泄密：已验证，且机制是设计出来的不是巧合。** 三处承重：`checkConfig`（`:192-196`）在 load 失败时**故意不回显 `cfgErr`**（config 解析失败常把 raw api_key 带进错误文本）；`skipped()`（`:181`）同理；`checkProviders`（`:353-389`）只报 "set/not set"；`checkSecretsRefs`（`:593`）拒绝时只说 "invalid credential reference"；`checkKeymapConfig`（`:633`）明确不回显原始 key/action。

⚠️ **需补的是回归保护**：目前**没有任何测试断言「raw api_key 不出现在 doctor 输出里」**。可行路径：`auth.legacy_insecure: true`（`config.go:450/:486` 的 gate 认这个开关）+ canary 明文 key，跑 `RunDoctor` 后对 `RenderText` 与 `RenderJSON` 双双断言 canary 不出现。**这条必须写进 W7，否则该属性只是「碰巧成立」**。

## 7. 审计的过时与错误

1. **审计 OBS3 行号错**：写 `observe.otel_export` 消费点在 `bootstrap.go:329`，实际 **`:369`**
2. **审计 OBS2 表述不够准**：只说 WS/SSE 零 otel（属实），没提 `orchestrator.go:444` 已有一处 `StartTurn`——只是挂在同步 `Query` 路径上。结论不变，但「零 StartTurn 调用点」的说法会误导
3. ⚠️ **审计 BENCH1 说「两处细节偏差，均不影响验收」，与它自己的二审自相矛盾** —— 二审段明确复现了 `BenchmarkOrchestratorTurn` 必挂并指出 `go test -bench=. ./...` rc=1。这不是「不影响验收」，「CI 记录趋势」直接不成立
4. **审计漏了 bench 输出被 slog 污染这条**（§5 ②）
5. ⚠️ **spec §4.3 W7 只点了 `checkSandbox`**，没提 `checkMCP` 与 `checkLSP` 这两个同类占位/半瞎检查。按 §4.5 验收标准 1，`C4/O07` 的「覆盖各子系统」必须把这两个也修掉。**W7 的实际范围比 spec 那一格宽**
6. ⚠️ **`doctor_release_test.go:93` 与 `doctor_test.go:322` 两条现存测试主动锁死了占位行为** —— spec/审计都没记录的隐性阻塞：不先改这两条，checkSandbox 的替换会以「测试变红」的形式被误判为回归
7. `nightly.yml:38` 注释过期（审计记了）；但审计没记 **`.bench-results/` 不在 `.gitignore`**
8. 非阻塞：审计引 `help.go:43 collectHelpEntries`，函数实际在 **`:41`**

---

# W8 TUI 体验

## 1. `/keymap` `/vim` `/contrast` 实测：**三个命令确实不存在**

- `commandTable` 定义在 `internal/cli/tui/commands.go:51`，字面量结束于 `:87`，**共 35 条**
- 临时探针调 `lookupCommand`（`commands.go:90`）：`keymap`/`vim`/`contrast`/`locale` **四个全部返回 `ok=false`**
- 唯一分发路径 `runCommand`（`commands.go:114`）：查不到走 `:121` 的 `errorEntry{text: "unknown command: /" + name}`。**用户键入 `/keymap` 只会得到 "unknown command"**

⚠️ **顺带查出审计未发现的缺陷**：`commands.go:64` 与 `:65` 是两条**完全相同**的 `{name: "features", ...}`。后果是 `/help`、Ctrl+K palette、`/` palette 全部把 `features` 显示两遍。实际只有 **34 个不同命令**。

**底层零件确实已存在**：

| 资产 | 位置 |
|---|---|
| `keymap.Action` 语义常量 | `internal/keymap/keymap.go:22`、`:25` 常量组、`:38 knownActions` |
| `NewDefaultBuilder(overrides)` | `keymap.go:67` |
| `Map.Lookup(tea.KeyMsg)` | `keymap.go:189` |
| `Map.Diagnostics()` 四类诊断 | `keymap.go:202`，`Diagnostic` 类型 `:217` |
| `NormalizeKey` | `keymap.go:240` |
| `VimMachine` 状态机 | `vim.go:17`、`NewVimMachine:24`、`HandleKey:46`、`effectiveVimMode:98` |
| `Preferences` 六字段 | `preferences.go:19`(UILocale) `:20`(ThemeName) `:21`(KeymapName) `:22`(HighContrast) `:23`(Vim) `:27`(KeymapReset tombstone) |
| 四层合并 | `preferences.go:110 mergeTUIPrefs` |
| 持久化 | `:53 loadPreferences`/`:71 persistPreferences`/`:145 preferencesPath`（`os.UserConfigDir()/yanshi/prefs.json`）/`:157 PreferencesFromEnv` |
| 高对比主题 | `styles.go:270 ThemeHighContrast`、`:293 themeList`、`:335 themeHighContrast` |
| i18n 文案 | **全部已在 catalog 里**：`tui.command.help.keymap\|vim\|contrast\|locale`、`tui.command.keymap.{usage,conflict,none,reset}`、`tui.command.vim.{usage,enabled,disabled}`、`tui.command.contrast.usage`、`tui.command.locale.{usage,current,changed}`、`tui.command.preference.persist_failed` |

**断链的确切位置**：`mergeTUIPrefs`/`loadPreferences`/`persistPreferences`/`PreferencesFromEnv` **全仓非测试代码零调用点**。`newModel`（`model.go:291`）硬写 `prefs: Preferences{}` + `prefsPath: ""`（`:310-313`），`NewProgram`（`model.go:340`）不接受任何 prefs 参数。TUI 按键仍是 `handlers.go` 的硬编码 `switch msg.Type`（`:224` 起，Ctrl+K `:347`、Ctrl+S `:364`、F1 `:382`）。

## 2. C15 决定：**实现三个命令**（不改文档）

1. **改文档等于把已付出的成本一笔勾销。** `internal/keymap` 是完整、有测试的叶子包，`Preferences` 六字段 + 四层合并 + 原子持久化全写好，i18n catalog 连每句提示文案都备齐 —— 缺的只是 `commandTable` 里四行和把 `mergeTUIPrefs` 接进 `NewProgram`。**删文档保留死代码是最坏的组合**
2. ⚠️ **`/keymap diagnostics` 已经是别处的对外承诺**：`internal/cli/doctor.go:646` 的失败文案逐字写着 `"key bindings are invalid; use /keymap diagnostics"` —— 改 tui.md 而不改这里，谎言只是换了个位置；两处都改则 doctor 失去唯一可执行的补救指引
3. **`KeymapReset` tombstone（`preferences.go:27`）没有写入者**，这个字段的存在本身就是 `/keymap reset` 的规格说明；不实现就必须删字段，而删字段会让 `mergeTUIPrefs` 的 tombstone 分支（`:132-134`）与其 doc 一起变成死代码

配套：`/locale` 一并做（I18N1 需要它，catalog 已备好）。文档侧仍要动 —— `tui.md:27` 与 `configuration.md:93` 需补 `/locale`，`tui.md:19` 措辞需对齐；**这两处都在 GENERATED 块之外**（tui.md 生成块是 `48-90`，configuration.md 是 `99-286`），手改安全，且 slash 命令不进 `-h`，**不触发 docs 门禁**。

## 3. UX3 服务端方案要点

**① frame 字段落点**：`internal/proto/frame.go:40` 的 `ClientFrame`，紧邻既有的 `Images []ImageAttach`（`:78`，Tier G 的先例）新增 `Attachments []AttachRef`，`AttachRef` **只带 `Path string`（绝不带内容）** —— 内容由服务端读，**这正是被否决过的 MVP 与本方案的分界线**。配套构造函数 `NewUserMessageWithAttachments`，对齐 `:118 NewUserMessageWithImages`。

**② 服务端 handler**：新建 `internal/api/http/attach.go`，签名形如 `resolveAttachments(root string, prof guard.PermissionProfile, refs []proto.AttachRef) (block string, notes []string)`。两个调用点：
- **WS**：`ws.go:461` 的 `runUserTurn` 闭包内，在 `resolveQuery`（`:467`）之后、`cs.history = append(...)`（`:473`）之前。`case "user_message"` 在 `ws.go:1170`
- **SSE**：`chat.go:39 handleSSEInternal`，请求结构体 `:42-57`，`resolveQuery` 调用 `:73`

**③ guard fs 校验（fail-closed，三道）**：
1. `pathjail.WithinRootAbs(workRoot, path)`（`internal/pathjail/pathjail.go:24`）—— 唯一 canonical root-jail，含 symlink eval + Windows 盘符 `EqualFold` + 大小写复核。**不得手写路径检查**
2. `guard.New().Check(prof, guard.Action{Tool: "fs_read", FS: guard.FSWant{Op: "read", Paths: []string{abs}}})`，命中 `checkFS`（`guard.go:227`）。profile 从 `Server.controlProfile`（`server.go:143`）取
3. ⚠️ **只有 `guard.Allow` 通过**。附件解析发生在 turn 开始之前、任何工具 context 之外，**没有 permission callback 可升级**，所以 `Prompt` 与 `HardDeny` 一律当拒绝处理 —— 与 SSE 无 callback 时的 fail-closed 语义一致

**④ 大小上限**：建议 **单文件 64 KiB / 单 turn 合计 256 KiB**。超阈值不读内容，改为注入一行 `attachment <path> is <N> bytes (limit 64KiB); use fs_read to read it in slices`。理由：64 KiB 约 16k token，已是常见 128k 窗口的 1/8；再大就该走 `fs_read` 分片，否则一次附件就能吃掉压缩预算（W4 的量纲问题会被放大）。

**⑤ ⚠️ 一处必须纠正的前提**：CLAUDE.md 写「WS 与 SSE 共用同一套帧词表……新增帧类型 → 同时更新 `ws.go` 与 `ssebackend.go`」。**这句对 `ServerFrame` 成立，对 `ClientFrame` 不成立。** 实测 `chat.go:42-57` 的 SSE 请求体是**匿名结构体**（Message/Messages/Model/Thinking/OutputSchema/ThreadID/TurnID），**从不 unmarshal `proto.ClientFrame`**。

**所以要改的是四处，不是两处**：

| 侧 | 文件:行 | 改什么 |
|---|---|---|
| 服务端 WS | `ws.go:461` / `:1170` | 读 `cf.Attachments` |
| 服务端 SSE | `chat.go:42-57` / `:73` | 结构体加 `Attachments`，同一 resolver |
| 客户端 WS | `internal/cli/wsbackend.go:68`（`Send`），`:86` 硬写 `proto.NewUserMessage(text)` | 需要能带附件的 turn 通道 |
| 客户端 SSE | `internal/cli/ssebackend.go:60`（`Send`），`:68-77` 匿名 body | 同上 |

**⑥ 客户端通道的架构缺口**：`ChatBackend`（`internal/cli/backend.go:120`）的 `Send(ctx, text string)` 只能发纯文本；`SendFrame`（`:122`）在 wsBackend 里会置 `controlMode = true`（`wsbackend.go:156`），按 control-reply 而非 turn 关闭通道，**不能拿来发 user_message**。必须动接口。实现方 4 个（`wsbackend.go:68`、`ssebackend.go:60`、`fakebackend.go:22`、`exec_test.go:164`）。建议新增 `SendTurn(ctx, cf proto.ClientFrame)`，`Send(ctx, text)` 退化为薄包装（DRY），`tuiSession`（`model.go:23`）同步加一条。

**⑦ 已有但无消费者的半成品**：`internal/cli/tui/images.go:11` 的 `buildSendFrame(text, images)` 是为这条路径预留的唯一 seam，**目前零生产调用点**。UX3 正好把它激活并扩展为三参数版。

## 4. UX4 frecency：**接受偏离，改验收标准**

**实测**（全部与审计一致）：
- 存储路径 `frecency.go:166-172` 返回 `os.UserConfigDir()/yanshi/frecency.json`。**规划要的 `~/.yanshi/file-frecency.jsonl` 确实没有**
- 格式：`Save`（`:121`）走 `json.Marshal(entries)`（`:128`），**单个 JSON 数组**，非 JSONL。写入是 `.tmp.<6字节hex>` + `os.Rename` 原子替换（`:137-146`）
- **`TopN`（`:79`）零生产消费者**
- **确无禁用配置** —— `config.example.yaml` 与 `config.go` 全文无 `frecency` 字样
- 记录信号 `extractPathFromToolArgs`（`:215`）只匹配 `case "fs_write", "fs_edit", "fs_mkdir"`（`:217`），由 `recordToolFrecency`（`:246`）在 applyEvent 的 tool_result 分支调用

⚠️ **另发现两个审计没提的缺陷**：
- `frecencyPath(root string)` 的 **`root` 参数声明了但函数体内完全不用**（`:166-172`），`newModel` 传了 `root` 进去（`model.go:325`）却被丢弃 —— 假装支持 per-project 而实际全局共享
- `:217` 的 `fs_mkdir` 是 W0 移交的**幽灵工具名**（消费侧四处残留之一）

**决定理由**：
1. **converge 到 `~/.yanshi/` 是 Windows 上的实质回退。** CLAUDE.md 明确本仓库在 Windows 上开发，`os.UserConfigDir()` 在 Windows 解析为 `%AppData%`，而 `~/.yanshi` 是 Unix 主义。当前路径还与同目录的 `prefs.json`（`preferences.go:150`）、`permModeFile` 保持一致
2. **JSONL 在这里买不到任何东西。** JSONL 的价值是增量追加，但 `Save()` 本来就是全量重写 + 原子 rename；换成 JSONL 只是多一条解析路径和多一种损坏形态，`LoadFrecency`（`:38`）现有的「坏 JSON 软降级为空」自愈逻辑反而要重写
3. **迁移成本落在用户身上而收益为零**

**改判后的验收建议**：把「近期选择靠前」改为「`@path` 补全候选按 frecency 排序」；「可禁用」新增 `tui.frecency: true|false`（⚠️ **会触发 docs 门禁，四个生成器必须重跑并提交**）；存储形态偏离在台账 acceptance 里写明并注明理由。

⚠️ **排序约束（必须写进计划）**：`TopN` 目前唯一可能的 UI 消费者就是 **UX3 的 `@path` 补全候选源**（UX1 的 action palette 四源 `command/mode/model/theme` 里没有文件源，见 `action.go:41-113`）。所以 **UX4 的验收依赖 UX3 先落地**，UX4 必须排在 UX3 之后；在 UX3 落地前，「近期选择靠前」**在任何 UI 上都无法被观察**。另需新增 `TopNUnder(root, n)` 做项目内过滤 —— 全局单文件会把 B 项目的文件推荐给 A 项目。

## 5. UX1 / UX2 / UX8 delta

- **UX1（Ctrl+K）**：Ctrl+K 开关（`handlers.go:347`）、fuzzy 排序（`action.go:118`）、Esc 关闭（`action.go:155`）、10 行窗口（`action.go:198 const maxRows = 10`）**全部真实可用**；缺口只有一条 —— **会话源缺失**，`action.go:40` 注释明写 `session + tool/MCP source DEFERRED`，`collectActions`（`:41`）只收 command/mode/model/theme 四源，而验收要求「命令/模式/模型/**会话**」。→ 补 session 源（复用 `proto.NewSessionList` + `cmdRestore`），顺带把 UX3 的**文件源**接进来（这就是 UX4 的消费者）
- **UX2（F1）**：内容三段防漂移（`help.go:41` 取自 `commandTable`/`guard.Modes()`/`themeList`）、F1 开关（`handlers.go:382`）、fuzzy 搜索（`help.go:57`）**都真实**；缺口是 ⚠️ **`helpPopup`（`help.go:102`）完全没有行数上限与滚动** —— 对比 `action.go:198`（10 行）与 `history.go:192`（8 行），它一次渲染全部 60+ 条；`view.go:181-183` 只用 `blockHeight` 记账不裁剪，最终由 `third_party/bubbletea/standard_renderer.go:186` 的 `newLines[len(newLines)-r.height:]` **从顶部截断**，导致 40 行终端下标题、"Commands:" 段头与前 35 条命令**全部不可见且无法滚动到**。→ 加 `maxRows` + 光标滚动，与另两个 popup 对齐
- **UX8（思考流式）**：四条验收在 TUI/编排层**全部成立**（`classify.go:363` 发 `proto.NewThinking`、`proto/frame.go:484` 独立帧类型、ctrl+o 折叠 `handlers.go:338`）；缺口在 ⚠️ **provider 侧从未「开启」thinking** —— `internal/llm/eino/anthropic.go:137` 的 `anthropicRequest` 结构体**没有 `thinking`/`budget_tokens` 字段**（全仓 `rg budget_tokens` 零命中），而 `ReasoningEffortOption`（`reasoning.go:26`）只产出 `openai.WithReasoningEffort`，被 `orchestrator.go:510` 唯一使用。→ 现成扩展点是 `outputSchemaOptions`（`outputschema.go:25`，被 `anthropic.go:210/:254` 与 `responses.go:299/:343` 解码）：加一个 thinking 字段并在 `anthropic.go:297 buildRequest` 里映射成 Anthropic 的 `thinking{type:"enabled",budget_tokens:N}`。⚠️ **DRY 注意**：不要新造第二个 impl-option 结构体，`GetImplSpecificOptions` 按 setter 类型断言，**两个结构体会互相看不见**

## 6. E03 的 CLI/TUI 侧缺口（Web 归 S4b，不碰）

**已有且真实**：`ParseInstallSource`（`internal/skills/install.go:37`，只收 `github:owner/repo[/subdir]`，逐段字符集校验 + 拒 `.`/`..`）、`Install`（`:111`，同盘 staging → `rejectSymlinks`（`:235`）→ `isWithin` containment（`:250`）→ frontmatter + `validName`）、`Uninstall`（`:204`）；服务端 handler `handleInstallSkill`（`ws_handlers.go:188`）、`handleSkillMutation`（`:231`）、`handleListSkills`（`:172`）；帧词表 `proto/frame.go:857-916` 全套。

**三个缺口，全在 CLI/TUI 侧**：
1. **无 `/skill validate`** —— `cmdSkill` 的 switch（`commands_skills.go:40-64`）只有六个子命令，`default`（`:59`）直接报 unknown。校验逻辑只内联在 `Install` 里，装完后无法主动重验。→ 新增 `validate_skill` ClientFrame + 服务端 handler（复用 `readFrontmatter`/`validName`/`validDesc`/`rejectSymlinks`）+ `/skill validate [name]`
2. ⚠️ **重名冲突不可诊断** —— `Loader.Load`（`internal/skills/skills.go:168`）在 `:188` 是 `continue // first-seen-wins`，**静默丢弃**被 shadow 的同名技能；`Registry` 里根本没有这些条目，`/skills` 只显示获胜者。→ `Load` 需额外收集 `[]Conflict{Name, WinnerSource, ShadowedSource, ShadowedDir}`，经 `skills_list` 帧回传，`/skills` 渲染 `⚠ shadowed by <source>`。**这是本项改动面最大的一块**（要动 `Loader`/`Registry` 的返回形状）
3. **无 `/skill update`** —— MVP 用 uninstall + install，属计划内的诚实声明，**建议维持不做**（YAGNI），但要在台账 acceptance 里显式记为「不做」而非「未做」

## 7. I18N1 缺口

核心库真实完整：`Bundle` persistent/effective 分离（`i18n.go:98`）、`NewBundle` 每次调用重算 auto（`:107-113`）、`detectLocale` 真走 `LC_ALL > LANG`（`:166-176`）、`normalizeLocale` 处理 `@modifier`/`.codeset`/`C`/`POSIX`/`zh-CN`/`zh-SG`→`zh-Hans`、`zh-TW`/`zh-HK`→`en`（`:183-207`）；`en.json` 与 `zh-Hans.json` **各 65 key，完全对齐**；`config.I18NConfig`（`config.go:162`）与 `config.example.yaml:181-183` 齐备。

**缺口四条**：
- **(a) TUI 硬编码英文** —— `state.go:119-120` 的 `defaultBundle()` 写死 `i18n.NewBundle("en")`，`newModel`（`model.go:291`）唯一调用它。`cfg.I18N.UILocale` 与 `YANSHI_UI_LOCALE` **都进不了 TUI**
- **(b) 无 `/locale` 命令** —— catalog 里三条文案已备好却无人使用
- **(c) 四层合并孤立** —— 与 C15 是**同一条断链**，两项应在同一个 PR 里一次接通
- **(d) 65 个 key 里只有约 22 个真被使用** —— 生产代码只有两处读 bundle：`commands.go:335`（`newCmdHelpEntry`）与 `model.go:315`（`input.Placeholder`）。其余 43 条是为本次要实现的命令预留的

⚠️ **不可违反的约束**：`ui_locale: auto` 必须保持「每次启动重算、**不持久化**」（`i18n.go:112-113` + `Persistent():138` 的 doc 已明确）；`output_language`（`config.go:163`）是**完全独立**的旋钮，**绝不能读 `ui_locale`** —— 模型看不见 TUI。`/locale` 写盘时必须写 `bundle.Persistent()` 而非 `Effective()`。

## 8. `internal/cli/tui/` 行数（>700）

`go run ./cmd/codelines` 实测，30 个非测试文件中**只有一个超过 700**：

| 文件 | 纯代码行 | 状态 |
|---|---:|---|
| `commands.go` | **943** | ⚠️ 距上限仅 **57 行**，已在 GOV2 的 `approaching` 警告名单（`lines_test.go:46` 阈值 900） |

次高 `model.go` 602、`view.go` 592、`entries.go` 588 —— 都安全。**`lineExceptions` 是空 map，没有任何豁免可用。**

⚠️ **结论**：四个新命令**绝不能加进 `commands.go`** —— 四个 handler 加参数解析、诊断渲染、持久化错误处理，稳超 57 行。必须新建 `internal/cli/tui/commands_prefs.go`（与既有 `commands_skills.go`/`commands_logs.go`/`commands_session_memory.go` 拆分风格一致），只在 `commands.go:87` 前的表里加四行条目（+ 删掉重复的 `features` 那行，**净增约 3 行**）。

## 9. PR 分组（8 项 → 5 个 PR）

| PR | 内容 | 依赖 |
|---|---|---|
| **PR-1** `fix(tui): wire preferences and add /keymap /vim /contrast /locale` | C15 + I18N1 | 无 |
| **PR-2** `fix(tui): bound the F1 help panel + add session source to Ctrl+K` | UX2 + UX1 | 无（不碰 `commands.go`，可与 PR-1 并行） |
| **PR-3** `feat(proto,api): server-side @path attachments` | UX3 | 无（但**必须先于 PR-4**） |
| **PR-4** `feat(tui): frecency-ranked @path completion + tui.frecency config` | UX4 | **依赖 PR-3** |
| **PR-5** `feat(llm,skills): enable anthropic thinking + skill validate/conflict diagnostics` | UX8 + E03 | 无（可再拆 5a/5b，零重叠） |

PR-1 细节：新建 `commands_prefs.go`；`NewProgram` 接受 project `Preferences`（在 `cmd/yanshi/runTUI`（`main.go:585`）里 `config.Load` 后注入）；接通 `mergeTUIPrefs` 四层；`keymap.Map` 接进 `handlers.go` 的 key 分发；删 `commands.go:65` 重复条目。**同一 commit 里改 `tui.md:19,27` 与 `configuration.md:93`** —— 文档与代码不得分离。

PR-3 **建议内部再切两刀**：先落服务端 resolver + frame + 越权/超限的负向测试，再落 TUI 的 `@` 交互。

**串行关键路径只有一条：PR-3 → PR-4。**

## 10. 审计 / spec / CLAUDE.md 的过时与错误

1. ⚠️ **【重要】CLAUDE.md 关于 SSE 的表述不准确** —— 「WS 与 SSE 共用同一套 JSON 帧词表……新增帧类型 → 同时更新 `ws.go` 与 `ssebackend.go`」**只对 `ServerFrame` 成立**。SSE 的**请求**侧（`chat.go:42-57`）是匿名结构体，从不 unmarshal `proto.ClientFrame`。UX3 要改四处而非两处。**建议 W8 顺手修正 CLAUDE.md 这一段**
2. **审计对 `commandTable` 的计数掩盖了一个 bug** —— 「共 35 项」数字对，但 `:64` 与 `:65` 是同一条 `features` 重复两次，**实际只有 34 个不同命令**
3. **审计漏报 `frecencyPath` 的死参数** —— `frecency.go:166` 声明了 `root` 却完全不用，而 `model.go:325` 认真地传了进去。这直接导致 frecency 全局共享、跨项目污染，**对 `@path` 补全是实质性错误**
4. ⚠️ **`internal/archtest/deps_test.go` 的 `portAllowlists` 有一条死条目** —— `ip("internal/proto")` 的允许集里列着 `ip("internal/pathjail"): true`，但 `internal/proto` **当前完全不 import pathjail**（零命中）。它与 GOV2/4/5/6 的「豁免只减不增、死条目必须失败」原则相悖，**只是 GOV1 的 allowlist 没有死条目检测**
5. **spec §4.3 W8 没有点出 UX4 对 UX3 的顺序依赖** —— UX4 的「近期选择靠前」在 UX3 落地前**在任何 UI 上都不可观察**。计划必须显式排序
6. ⚠️ **`fs_mkdir` 幽灵名在 `frecency.go:217` 仍在** —— W0 移交 W1 的四处消费侧残留中，`frecency.go:217`、`styles.go:406`、`entries.go:845` **三处都在 W8 的代码区**。**W8 与 W1 在这三行上有重叠** —— 建议由先落地的一方清理，另一方 review 时确认
