# Yanshi 功能实现审计：规划 vs 实际

> ⚠️ **本文件是 2026-07-31 的历史快照，不是当前状态的权威描述。**
> 当前状态以 `docs/feature-status.yaml` 为唯一真相源（`go run ./cmd/featurestatus` 查看统计）。
> 本报告的价值在于它记录了每一项判定的 `file:line` 证据与对抗式证伪过程，可用于追溯「为什么当初这么判」。
> 分解与修复计划见 `docs/superpowers/specs/2026-08-03-yanshi-roadmap-design.md`。

> ⚠️ **D2/O12 已作废** —— VS Code 扩展（`ide/vscode/`）与 `scripts/check-d2.sh` 已于 2026-08
> 以移除方式结案（spec `docs/superpowers/specs/2026-08-03-yanshi-roadmap-design.md` §3.2 ④），
> 由 `internal/archtest::TestVSCodeExtensionRemoved` 守住。本文中一切把它当作交付物/待办的
> 描述均已失效，**不要照做**。

> **审计日期**：2026-07-31 · **模块** `github.com/x6nux/yanshi` · **Go** 1.26.4
> **对照基线**：`docs/archive/feature-roadmap-codex-deepseek.md`（Tier A–D，55 项）+ `docs/archive/feature-roadmap-e-h.md`（Tier E–H，25 项）+ `docs/superpowers/plans/` 下 07-18/19/20 的早期 lane 计划（M1，14 项）
> **审计范围**：**94 个编号功能项 / 23 个批次**，逐项对**代码**实测取证，不采信任何文档的自述状态。

---

## 0. 为什么需要这份文档

仓库 git 历史被压扁为 8 个 squash commit，**无法从提交记录推断任何功能的完成度**。同时两份路线图自成文后从未回写过状态——它们把大量早已落地的能力仍标为「缺失」，也把若干实际断线的能力当作已完成的前置基线来引用。

`docs/archive/README.md` 已声明这些归档文档「不是当前态的权威描述」。本报告即是对「当前态」的一次机器化实测重建：

- **一审**：23 个批次并行，每批读规划条目 → 定位落点包 → 打开代码判断真实实现 vs 占位 → 验证是否接进 `bootstrap.Build` / 工具注册表 / config / CLI / TUI 命令表 → 对照验收标准。
- **二审（对抗式证伪）**：对一审的每一条判定，派独立代理**专门尝试推翻**它。判「已实现」的去找空壳、死代码、零引用；判「未实现」的换关键词重搜、查别名实现、查配置与文档入口。不确定时强制降级。
- **结果**：94 项中 **40 项被二审改判**（37 项降级、3 项升级），说明证伪层确实拦下了大量「看起来做完了」的假阳性。

**判定口径**

| 判定 | 含义 |
|---|---|
| **已实现** | 功能存在、非占位、已接进运行时、验收标准基本满足 |
| **部分实现** | 核心在但有明确缺口（未接进 bootstrap / 缺子能力 / 验收只部分满足 / 只有一半入口） |
| **未实现** | 搜遍相关路径找不到实现，**或**代码存在但在生产路径上不可达 / 必然崩溃 / 核心零产出 |
| **有差别** | 实现了，但方案、接口、命名或范围与规划明显不同 |

> 「代码存在但生产不可达」按**未实现**计，是本次审计最关键的口径选择。对用户而言，一个模型永远调不到、或一调就崩的工具，与不存在没有区别。

---

## 1. 总览

### 1.1 终判分布

| 判定 | 项数 | 占比 |
|---|---:|---:|
| **未实现** | 12 | 13% |
| **有差别** | 2 | 2% |
| **部分实现** | 50 | 53% |
| **已实现** | 30 | 32% |
| 合计 | 94 | 100% |

> **一句话结论**：yanshi 的规划功能面**基本都写出来了**（94 项里只有 12 项是真的没写），但**只有 32% 真正端到端可用**。主导失效模式不是「没做」，而是 **「零件造好了，总装线没接上」**——包写完、测试全绿、却没有任何生产调用点。

### 1.2 按批次分布

| 批次 | 名称 | Tier | 已实现 | 部分实现 | 未实现 | 有差别 |
|---|---|---|---:|---:|---:|---:|
| **M1** | M1 六 lane + 三专项 | 早期(07-18/19/20) | 9 | 4 | 0 | 0 |
| **B0** | 前置技术债 | A-D | 2 | 1 | 0 | 0 |
| **A1** | 安全执行底座 | A-D | 0 | 3 | 2 | 0 |
| **A2** | 任务与计划模型 | A-D | 1 | 2 | 0 | 1 |
| **A3** | MCP 生态 | A-D | 0 | 3 | 0 | 0 |
| **B1** | 子代理增强 | A-D | 0 | 2 | 1 | 0 |
| **B2** | 编辑反馈(LSP+回滚) | A-D | 1 | 1 | 0 | 0 |
| **B3** | 开发者工具 | A-D | 0 | 2 | 4 | 0 |
| **C1** | 批量与自动化 | A-D | 0 | 1 | 2 | 0 |
| **C2** | TUI 体验(8 项) | A-D | 3 | 4 | 1 | 0 |
| **C3** | 会话与记忆 | A-D | 3 | 1 | 0 | 0 |
| **C4** | 可观测运维 | A-D | 0 | 5 | 0 | 0 |
| **D1** | headless+API+app-server | A-D | 0 | 2 | 0 | 1 |
| **D2** | SDK + IDE | A-D | 0 | 1 | 1 | 0 |
| **D3** | secrets+auth+i18n+keymap | A-D | 1 | 3 | 0 | 0 |
| **E1** | 测试覆盖补齐 | E-H | 1 | 2 | 0 | 0 |
| **E2** | fuzz/property/race | E-H | 2 | 1 | 0 | 0 |
| **E3** | 架构治理测试 | E-H | 3 | 0 | 0 | 0 |
| **F1** | SQLite 并发(WAL) | E-H | 0 | 1 | 0 | 0 |
| **F2** | 资源治理与压测基线 | E-H | 1 | 3 | 1 | 0 |
| **G** | 多模态理解 | E-H | 0 | 2 | 0 | 0 |
| **H1** | 发布工程 | E-H | 2 | 2 | 0 | 0 |
| **H2** | 文档/示例/贡献 | E-H | 1 | 4 | 0 | 0 |

### 1.3 二审改判清单（40 项）

对抗式证伪推翻或修正的判定。**降级**说明一审高估（多为「代码在但运行时不可达」），**升级**说明一审误判缺失。

| 功能 | 一审 | 终判 | 方向 |
|---|---|---|---|
| `M1` G02 | 已实现 | **部分实现** | ⬇ 降级 |
| `M1` G03 | 已实现 | **部分实现** | ⬇ 降级 |
| `M1` O07 | 已实现 | **部分实现** | ⬇ 降级 |
| `M1` SPEC-TOOLIF | 已实现 | **部分实现** | ⬇ 降级 |
| `M1` V12 | 有差别 | **已实现** | ⬆ 升级 |
| `A1` S06 | 已实现 | **部分实现** | ⬇ 降级 |
| `A1` S07 | 已实现 | **部分实现** | ⬇ 降级 |
| `A1` S08 | 部分实现 | **未实现** | ⬇ 降级 |
| `A1` T07/T08 | 部分实现 | **未实现** | ⬇ 降级 |
| `A2` DT1 | 已实现 | **部分实现** | ⬇ 降级 |
| `A2` DT2 | 已实现 | **有差别** | ⬇ 降级 |
| `B1` M04b | 部分实现 | **部分实现** | ↔ 同级修正 |
| `B1` M05 | 部分实现 | **未实现** | ⬇ 降级 |
| `B3` DT4 | 部分实现 | **未实现** | ⬇ 降级 |
| `B3` DT5 | 部分实现 | **未实现** | ⬇ 降级 |
| `B3` GH1 | 有差别 | **部分实现** | ⬆ 升级 |
| `B3` T11 | 部分实现 | **未实现** | ⬇ 降级 |
| `B3` W07 | 部分实现 | **未实现** | ⬇ 降级 |
| `C1` AU1 | 部分实现 | **未实现** | ⬇ 降级 |
| `C1` M07 | 部分实现 | **未实现** | ⬇ 降级 |
| `C2` UX2 | 已实现 | **部分实现** | ⬇ 降级 |
| `C2` UX8 | 已实现 | **部分实现** | ⬇ 降级 |
| `D1` APS1 | 已实现 | **部分实现** | ⬇ 降级 |
| `D1` V12 | 已实现 | **部分实现** | ⬇ 降级 |
| `D1` V14 | 已实现 | **有差别** | ⬇ 降级 |
| `D2` O12 | 部分实现 | **未实现** | ⬇ 降级 |
| `D2` V15 | 已实现 | **部分实现** | ⬇ 降级 |
| `D3` S10 | 已实现 | **部分实现** | ⬇ 降级 |
| `E1` COV2 | 已实现 | **部分实现** | ⬇ 降级 |
| `E1` COV3 | 已实现 | **部分实现** | ⬇ 降级 |
| `E2` FUZ1 | 部分实现 | **已实现** | ⬆ 升级 |
| `F2` BENCH1 | 已实现 | **部分实现** | ⬇ 降级 |
| `F2` CCL1 | 部分实现 | **未实现** | ⬇ 降级 |
| `F2` LEAK2 | 已实现 | **部分实现** | ⬇ 降级 |
| `F2` LEAK3 | 已实现 | **部分实现** | ⬇ 降级 |
| `H1` VER1 | 已实现 | **部分实现** | ⬇ 降级 |
| `H2` APIREF1 | 已实现 | **部分实现** | ⬇ 降级 |
| `H2` CONTRIB1 | 已实现 | **部分实现** | ⬇ 降级 |
| `H2` EX1 | 已实现 | **部分实现** | ⬇ 降级 |
| `H2` UDOC1 | 已实现 | **部分实现** | ⬇ 降级 |

---

## 2. 跨批次系统性发现

这一节是本报告的核心。以下问题**跨越多个批次**，逐批看会被稀释成若干条独立小缺口，合起来看才显出根因。

### 发现 1 ｜ 一个 nil 字段打穿 8 个已注册工具 ⚠️ 最高优先级

**根因**：`internal/shell/factory.go:74` 是全仓库**非测试代码中唯一**构造 `secproc.StartedProcess` 的地方，它只填 `PID` / `Stdout` / `Stderr`，**从不填 `Cmd`**：

```go
return &secproc.StartedProcess{
    PID:    proc.PID(),
    Stdout: consoleReader{console},
    Stderr: discardReader{},
}, nil            // ← Cmd *exec.Cmd 恒为 nil
```

而 `internal/tools/secproc_capture.go:85` 无条件解引用它：

```go
waitErr := started.Cmd.Wait()   // ← nil pointer dereference
```

**生产装配链**（全部实测确认）：
`bootstrap.go:887 shell.DefaultSecureFactory{OS: shell.OSProcessFactory{}}` → `orchConfig.SecureFactory` → `orchestrator.go:280 tools.WithSecureProcessFactory(ctx, ...)` → 工具从 context 取 factory。

**受害工具**（8 个，全部已在 `bootstrap.Build` 注册、模型可见、调用即进程级 panic）：

| 工具 | 消费点 |
|---|---|
| `git_status` / `git_diff` | `internal/tools/git.go:73,117,130,142,207` |
| `run_tests` | `internal/tools/testrun.go:81` |
| `diagnostics` | `internal/tools/diagnostics.go:85,108` |
| `github_pr_context` / `github_comment` / `github_approve` / `github_merge` | `internal/tools/github.go:128,149,172,200` |

**为什么测试全绿**：测试用自制 Factory 填了 `Cmd`，掩盖了生产 Factory 的这条断裂。这是「fake 太好用」的反面教材——fake 比真实现**多**填了一个字段。

**这是 B3 整批（W07 / DT4 / DT5 / GH1）从「部分实现」降为「未实现」的直接原因。**

**修法**：`factory.go:74` 补上 `Cmd:` 字段；或让 `runSecureCapture` 在 `Cmd == nil` 时退回按 PID 等待。一处改动即可恢复 8 个工具。

---

### 发现 2 ｜ C1 整批是死代码

`internal/bootstrap/c1.go` 全文（`BuildRLM` / `BuildAutomation` / `BuildC1`）在非测试代码中**零调用点**——`bootstrap.Build` 从未引用过它，连 `cfg.Batch` 配置块都从未被 `bootstrap.go` 读取。

**后果**：
- `rlm_query` 从不进 `allTools`，模型永远看不到
- 8 个 `automation_*` 工具同上；scheduler goroutine 从不启动
- `agent_batch` 同上

**影响**：RLM1 / AU1 / M07 三项。三个包（`internal/agent/rlm`、`internal/agent/automation`、`internal/agent/batch`）的实现质量都不低、测试也齐全，纯粹缺 `Build` 里的一次调用。

---

### 发现 3 ｜ 「子代理角色」在运行时不存在

`registry.WithRole`（`internal/agent/registry/context.go:25`）在**全仓生产代码中零调用**。`Manager.runAgentLoop` 派生 child ctx 时不绑 role，`managedTurnRunner.Run` 也不从 Record 读 Role 回绑。

**后果**：`orchestrator.go:697` 的 `registry.RoleFromContext(ctx)` 恒返回空串 →
- 7 个角色的 `PromptPrefix` **永不注入**
- `RolePolicy`（角色只能收紧父 guard 的安全不变量）**永不生效**
- 五段式输出契约 `outputContractPrefix` **永不下发**

`Record.Role` 被正确持久化，但只是一个写进数据库、运行时从不读回的字段。**影响**：M05（判未实现）、M04b。

---

### 发现 4 ｜ 多模态整条通路断在最后一跳

`Orchestrator.ApplyImages`（`internal/agent/orchestrator/multimodal.go:16`）是图像分流的唯一入口，**全仓零生产调用**，只有 4 个测试调用它。

WS 的 `runUserTurn`（`ws.go:461`）与 v1 的 `runTurn`（`api/v1/service.go:299`）都只处理纯文本，从不读 `Images` 字段；`TurnOpts` 也没有承载图像的字段。

**后果**：协议字段（`proto.ClientFrame.Images`、`v1.TurnStartParams.Images`、`sdk/schema/v1` 的 `ImageAttach`）构成一份**只写不读的契约**——客户端按协议发图，服务端静默丢弃。规划的「五入口」只有截图工具一个真正打通。

**影响**：VISION、VISION-TOOL 两项。`internal/imagestore`、`internal/clipimg`、`image_describe` 工具本体质量都很高（`image_describe` 的路径校验用 `withinRootAbs` 阻断 symlink 逃逸，**严于**规划要求的前缀检查），缺的纯粹是装配。

---

### 发现 5 ｜ goal token 预算：计量修好了，闸门没接线

`overBudget()` 的判定是 `MaxTokens > 0 && spent >= MaxTokens`。但 `cmd/yanshi/main.go:723` 与 `:774` 两处 `Budget` 字面量**只设 `MaxIterations`**，CLI 只有 `-max-iters` 没有 `-max-tokens`，`internal/config` 与 `config.example.yaml` 也没有对应项。

**后果**：生产中 `MaxTokens` 恒为 0 → `overBudget()` 恒为 false → **token 预算在发行二进制里 100% 不可达**。只有单元测试手工构造 `Budget{MaxTokens: 100}` 才走得到这条分支。

计量管道（`UsageSink` 累加、循环双检查、planner/evaluator/tier 全接线、ACP 子进程 usage 回流）确实是真实完整实现——路线图 §2 说的「TD1 死代码」已不成立，但闸门空转。**影响**：TD1、G02、LEAK3。

---

### 发现 6 ｜ 首次 release 会直接失败

`.goreleaser.yaml:58` 使用 `changelog: skip: true`——这是 goreleaser v1 时代的字段，**v2.0 已彻底移除**（v2 的 `Changelog` struct 只有 `Disable`）。而 goreleaser v2 的 `pkg/config/load.go:59` 用 `yaml.UnmarshalStrict`（即 `KnownFields(true)`）严格解码，未知字段**报错而非忽略**。

**后果**：`release.yml` 一旦被 `v*` tag 触发，goreleaser 步骤直接解析失败，四个平台的 archive 与 `checksums.txt` 一个都发不出来。

**为什么至今没暴露**：仓库当前**没有任何 `v*` semver tag**（只有 m1..m9 里程碑 tag），`release.yml` 从未被触发过；CI 里也没有任何 `goreleaser check` 步骤。

**修法**：删掉整个 `changelog:` 段。（不要简单改成 `disable: true`——`release.yml:55` 靠 `--release-notes RELEASE_NOTES.md` 喂 git-cliff 产物，而 goreleaser 的 changelog pipe 本就会在 `ReleaseNotesFile` 非空时提前 return，不会重复。）建议同时在 `ci.yml` 加一个 `goreleaser check` job 把这类配置错误左移。

---

### 发现 7 ｜ mid-turn 压缩冷却量纲错配，主功能失效

`internal/llm/eino/compacting.go` 里：
- `:143` 压缩成功后存的是 `c.lastCompactTokens = res.TokensAfter`（压缩**后**的 token 数）
- `:158` 下次进来时比较的是 `ctxcompact.EstimateTokens(msgs)`（**未压缩**的完整历史）

两者不同量纲。**后果**：出厂默认配置下「同 turn 不重复压缩」这条核心验收完全失效（CCL1 判未实现）。

**叠加问题**：`bootstrap.go:794` 的 `CooldownTokens` 基于 `cfg.Compaction.ContextWindow` 这个全局回退值（默认 256000），而非 per-provider 的 `context_window`。对 128K 窗口的模型，cooldown 阈值翻倍、hard-force 触发点推迟到实际窗口的 1.9 倍（等于永不触发）。

这也意味着 `CLAUDE.md` 里「上下文窗口按模型配置，`/model` 切换自动用新窗口」的承诺**只在 pre-turn 路径成立**，mid-turn（`CompactingModel`）路径不成立。

---

### 发现 8 ｜ 两次 `adk.WithChatModelOptions` 覆盖，`reasoning_effort` 被静默丢弃

`internal/agent/orchestrator/orchestrator.go:494-499` 连续两次独立调用 `adk.WithChatModelOptions`（第一次传 `ReasoningEffortOption`，第二次传 `OutputSchemaOption`）。但 eino v0.9.12 的 `adk/chatmodel.go:96-101` 该 setter 是**赋值**不是追加：

```go
t.chatModelOptions = opts   // 第二次调用覆盖第一次
```

**触发条件**：同时开 thinking 且传 output schema 的 turn——`ws.go:644-648` 与 `v1/service.go:313` 两条生产路径都可达这个组合。

**为什么测试没抓到**：`orchestrator_test.go:746` 只传 `ThinkingEffort`，`:771` 只传 `OutputSchema`，两者从不同时出现，冲突恰好落在测试盲区。

**修法**：累积到单个 `[]model.Option` 后只调一次 `adk.WithChatModelOptions`。

---

### 发现 9 ｜ 子代理并发上限计数不准，槽位永不释放

`internal/agent/registry/manager.go` 的 `finishTerminal` 只写 record + emit，**从不 `delete(m.runtime, agentID)`**；而 `runningLocked()`（`:570`）取 runtime map 长度与 `StatusRunning` 记录数的**较大值**。

**后果**：终态 agent 长期占用并发槽。实测 `MaxConcurrent=2` 时第 3 次 Spawn 直接 `SpawnErrCap`，而 `List` 报 `Running=0`。计划文档 Task 6 明确写了 `finishTerminal` 末尾要调 `detachRuntime` + `cancel`，实现漏掉了这一段（`detachRuntime` 函数本身存在但无调用者）。

**并发上限还只覆盖新入口**：只有 `agent_spawn` 走 Manager；`agent_start` / `workflow_start` / `analysis` 三个 legacy 入口仍走 `runSubAgent` + `NumCPU` 信号量，Manager 的 cap 对它们完全不生效。为此写的适配器 `ManagedSubAgentRun` / `spawnWithRetry` 只有测试调用。

---

### 发现 10 ｜ 文档超前于代码

以下是**文档声称存在、代码里不存在**的能力，会直接误导用户与后续维护者：

| 文档位置 | 声称 | 实际 |
|---|---|---|
| `docs/user-guide/tui.md:19,27` | `/keymap`、`/vim`、`/contrast` 可用，写入 `preferences.json` | 三个命令都不在 `commandTable` 里（从未注册） |
| `docs/user-guide/configuration.md:93` | 同上 | 同上 |
| `ide/vscode/README.md`「Recovery model」 | 扩展支持断线自动重连 | `runWithRecovery` 从未被 `extension.ts` import |
| `docs/api/jsonrpc.md` | `config/read\|write` 是运行时配置读写 | 只操作进程内 `MemoryConfig`，与 `-config` 指向的 YAML 无关 |
| `docs/archive/README.md` | synthesis-* 三份「未进入仓库历史，故不在本目录」 | 三份就在目录里且已被 git 跟踪 |
| `internal/task/work/store.go` 包头注释 | 「All write paths route through the injected WriteTxer」 | 11 个写方法一个都没调 `wt().WriteTx` |

---

### 发现 11 ｜ 软门禁未收紧 + 覆盖率无门禁

`.github/workflows/ci.yml:116` 与 `:138` 的 `governance` 与 `fuzz-seed` 两个 job 仍带 `continue-on-error: true`，注释写着「soft until E3/E2 lands, drop once stable」——但 **E3 与 E2 的资产都已落地且全绿**。收紧动作只差删两行。

> 注：`governance` 不构成实际漏洞——`internal/archtest` 在 `go list ./...` 范围内，主 `test` job 的 `go test ./...` 会硬跑它。但 `fuzz-seed` 与 `nightly` 的 `fuzz-long`、`bench` 是真软的。

**覆盖率三条线（store 75% / proto 80% / bootstrap 50%）无任何 CI 强制**——`rg -- '-cover' .github/workflows/` 零命中。当前实测值（95.7% / 100% / 94.2%）远超目标，但没有任何机制防止回退。

---

### 发现 12 ｜ 路线图状态标记全面滞后

两份路线图**从未回写过完成状态**。标为「缺失」但实际已落地或部分落地的包括：APS1、V15、O12、S10、O03、I18N1、C15、LEAK1、LEAK2、LEAK3、CCL1、BENCH1、VISION、COV3、FUZ1、PROP1、GOV1、GOV3、WAL1…

反向也有：被当作「已完成前置基线」引用的能力（如 T07/T08 的 shell runtime）实际是断的，导致依赖它的 DT2 只能绕开走 `exec.CommandContext`。

`docs/archive/feature-roadmap-e-h.md` 更有一处文档损坏：E2 三项的标题 ID 被批量替换成了字面量 `n`（`### [n] 功能名`），正文依赖引用也变成 `[n]`——FUZ1 / PROP1 / RAC1 的编号在归档里已经丢失。

**结论**：那两份路线图只能当历史快照读，**本报告取代它们作为当前状态依据**。

---

---

## 3. 未实现（12 项）

包含两类：(a) 搜遍相关路径确实没有实现；(b) **代码存在但在生产路径上不可达、必然崩溃或核心零产出**——对用户而言等同于不存在。这一节是最需要立刻处理的部分。

#### `A1` S08 — OS 级 Sandbox

- **优先级** P0 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：Windows 至少 job object 拒绝越界文件写；Unix adapter 有统一接口+测试；越界操作被系统拒绝；不支持平台安全降级
- **实测缺口**：规划说：Windows 至少用 job object 拒绝越界文件写、Unix 用 landlock+seccomp、macOS 用 sandbox-exec，『越界操作被系统拒绝』。实际做的是：只有抽象层 + 四平台空骨架，Prepare 恒 return nil，四个平台一律 DegradedHostGuard/Enforced=false，任何越界操作都不会被系统拒绝（只有 host guard 拦）。代码与文档对此完全诚实（自称 Phase 0，bootstrap 打 warning），但按功能验收就是核心能力未实现。而且 DefaultSecureFactory.Start 连 Prepare 都没调（factory.go:69 是 `_ = f.Sandbox`），意味着即使将来换成真实 adapter 也需要改这个调用点。
- **二审改判理由**：我试图为「部分实现」找翻案证据（换关键词全仓搜 OS 隔离原语、查 secproc/securityctx/netpolicy 等一审未提的兄弟包、查 doctor/diagnostics 等用户可见入口），结论是应当**降级为未实现**，理由如下： 1. **全仓零 OS 隔离原语。** `grep -rniE "jobobject|CreateJobObject|landlock|seccomp|bubblewrap|sandbox-exec|seatbelt|CreateRestrictedToken|unshare|chroot|pledge"` 在非测试 .go 文件中的命中**全部是注释**（internal/sandbox/types.go:8-9、factory.go:18、sandbox_{windows,linux,darwin}.go 的 doc 注释）。`grep -rn "SysProcAttr"` 全仓唯一命中是 internal/sandbox/factory.go:18 的注释。go.mod:29 有 `golang.org/x/sys v0.44.0` 但未被用于任何沙箱用途。四个平台文件合计 49 行，全是 `return phase0(cfg, "<os>-selection-gate", "…not decided; host guard only")`。 2. **Prepare 在生产代码中零调用点。** `grep -rn "\.Prepare("` 全仓只有两处：internal/shell/factory.go:35（注释里写"a real adapter *would* call f.Sandbox.Prepare"）和 internal/sa…
- **证据**：/Users/ll/code/yanshi/internal/sandbox/types.go:25-32 AccessTier{ReadOnly,WorkspaceWrite,FullAccess} 三档；:41-48 EffectiveMode{OSIsolated,DegradedHostGuard,Disabled}；:55-63 CapabilityReport{Platform,Requested,Effective,Backend,Reason,Enforced,CanKillTree}；:91-95 Sandbox interface{Prepare,Report,Close}…

#### `A1` T07/T08 — Shell runtime v2 + 后台 /jobs

- **优先级** P0 ｜ **路线图原状态** 部分 ｜ **接进运行时** 否 ｜ **有针对性测试** 是
- **验收标准**：长进程返回 session id；可续读/stdin；yield/timeout/输出上限/显式关闭；进程树取消干净；session 关闭按策略回收
- **实测缺口**：规划说：持久会话 shell_start→{session_id,pid,PTY}，后续 shell_write_stdin/read/wait/cancel 按 id 操作；后台 task_shell_start/wait；全部经 S06/S08/S09；超时/取消杀整个进程树（PDEATHSIG/JobObject）；TUI /jobs 中心。实际做的是：Manager/9 工具/WS 帧/TUI /jobs 四层代码都写了且质量不低，但**运行时是断的**——(1) 9 个 shell v2 工具从未注册进 bootstrap 的 allTools，模型无法调用；(2) bootstrap 构造的 shell.Manager 缺 Config.Factory，任何 Start/StartJob 都会返回 'no process factory configured'，DefaultSecureFactory 只被 legacy shell_run 用作 secproc.Factory，没有作为 shell.ProcessFactory 接进 Manager；(3) PTY 全平台 ErrPTYUnavailable、非 PTY stdin 只读，故『可续读/stdin』不成立；(4) Unix 的 CanKillTree=true 无 setpgid 支撑，『进程树取消干净』不成立；(5) sandbox 未 Prepare、netpolicy 只给了一个无效 proxy URL，故『全部经 S08/S09』不成立。净效果：/jobs 永远显示 (none)，shell v2 是编译进二进制但不可达的死代码。
- **二审改判理由**：我独立复核后认为一审过于宽容，应从「部分实现」降级为「未实现」——五条验收标准在运行时无一成立，且我另外查到三处一审漏掉的硬伤。 【1】9 个 v2 工具零运行时引用（死代码）。`grep -rn "NewShellV2Tools" --include="*.go"` 的非测试命中只有定义处 /Users/ll/code/yanshi/internal/tools/shell_v2.go:49 自身，其余全是 _test.go。bootstrap 的工具注册面我逐条枚举过（`grep -n "allTools = append" internal/bootstrap/bootstrap.go` → 589/591/598/600/607/613/614/615/617/618/711/718/730/733/736/739/746/754/758/762），没有任何一条追加 ShellV2Tools；orchestrator 的工具集唯一来源是 cfg.Tools（orchestrator.go:187-191 `agentTools := make(...); for _, t := range cfg.Tools`），且 runnerFor 只用 o.agentTools（orchestrator.go:379-393），没有任何动态追加路径。bootstrap.go:630-631 profile allowlist 里那 9 个名字对应的工具根本不存在于工具表 → 模型看不到 → 「长进程返回 session id」不可达。 【2】Manager 无 Factory，Start/StartJob 必然失败。bootstrap.go:875-879 只填了 Root/MaxOutputBy…
- **证据**：/Users/ll/code/yanshi/internal/shell/manager.go:28 Manager；:85 Start（context.WithoutCancel 独立 lifecycle）；:130 StartJob；:169 Read / :180 ReadJob（ring buffer）；:194 Write；:209 Wait（ctx-aware + IdleTimeout）；:240 Cancel；:269 ListJobs；:284 RestoreJobs（重启后标 StateStale）；:311 Close（flush jobs） ； /Users/ll/co…

#### `B1` M05 — 子代理 7 角色

- **优先级** P1 ｜ **路线图原状态** 部分（路线图声明缺口：子代理只能继承 profile/instruction，缺 role/model override… ｜ **接进运行时** 否 ｜ **有针对性测试** 是
- **验收标准**：7 角色可选；权限矩阵符合；越权拒绝；别名大小写不敏感；未知值返回可接受集
- **实测缺口**：角色**目录与策略实现是真的，但角色到执行的接线断了一环**。orchestrator.go:697 依赖 `registry.RoleFromContext(ctx)` 拿角色，可是 registry.WithRole 在整个生产代码路径里从来没被调用过——Manager.runAgentLoop（manager.go:625）派生 child ctx 时不绑 role，managedTurnRunner.Run 也不从 Record 读 Role 回绑。结果：Record.Role 被正确持久化，但运行时 RoleFromContext 恒返回 ''，于是 **PromptPrefix 永不注入、RolePolicy 永不生效**。这直接击穿计划锁定项 #11『角色 catalog 必须接入执行』和 #10『角色只能收紧』。次要缺口：(a) 计划要求 effective tools = role allowlist ∩ caller allowlist ∩ parent 工具面，实现只把 caller 的 allowed 透传给 factory（agent_lifecycle.go:65 `factory(allowed, "")`），RoleDef.AllowedTools 从未参与求交；(b) 验收里的『别名大小写不敏感』『未知值返回可接受集』『custom 需显式 allowed_tools』三条在 agent_spawn 里完全没有对应代码，写 role:"EXPLORE" 或 role:"bogus" 都会被原样接受并当作无策略处理；(c) agent_spawn 参数表（agent.go:128-136）没有 custom 角色配置入口，registry.CustomRole（t…
- **二审改判理由**：一审判「部分实现」过于宽容，应降级为「未实现」。我自己把 ctx 链从头到尾走了一遍，证实角色 catalog 在生产路径上是**可证明不可达的死代码**，5 条验收标准无一在运行时成立。 (1) 死链完整闭合（我逐跳验证，不是复述一审）：`internal/bootstrap/bootstrap.go:651-656` 用 `RootContext: context.Background()` 建 Manager → `internal/agent/registry/manager.go:74` `rootCtx, rootCancel := context.WithCancel(opts.RootContext)` → `manager.go:190` `parentCtx := m.rootCtx`（或 `manager.go:433` resume 同理）→ `manager.go:217` `childCtx, childCancel := context.WithCancel(parentCtx)` → `manager.go:221` `go m.runAgentLoop(childCtx, id, rt)` → `manager.go:648` `rt.runner.Run(ctx, agentID, assignment)`。这条 ctx 从 `context.Background()` 派生，全程只经过 `context.WithCancel`，**从未注入任何 value**。而 `registry.WithRole`（`internal/agent/registry/context.go:25`）全仓 `rg 'WithRole('` 只有 3 处命中：定义本身 + `c…
- **证据**：/Users/ll/code/yanshi/internal/tools/agentroles.go:15 AgentRoles() 返回全部 7 角色 general/explore/plan/review/implementer/verifier/custom，各带 PromptPrefix + AllowedTools + Policy ； /Users/ll/code/yanshi/internal/tools/agentroles.go:65 outputContractPrefix 生成 SUMMARY/CHANGES/EVIDENCE/RISKS/BLOCKERS 五段式前缀；:…

#### `B3` DT4 — run_tests 工具

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：至少 Go 解析正确；结构化计数+失败列表；超时/取消干净；大输出成 artifact
- **实测缺口**：规划要的 Go/cargo/npm 探测 + 结构化计数 + 失败列表 + artifact 全都写了，但：(1) Go 解析器把包级 pass/fail 事件当成用例计数，验收标准「至少 Go 解析正确」不成立；(2) 生产 Factory 路径 nil-panic，工具在真实运行时跑不起来；(3) 规划的「长测试走后台 task_shell」完全没做。
- **二审改判理由**：我逐项复核后不认同"部分实现"，应降级为"未实现"——不是因为代码是空壳（parseCargoOutput/parseNPMOutput/detectRunner 等确实是真实逻辑），而是因为**生产路径 100% 崩溃 + 首要验收标准硬性不成立 + 新发现的静默假 pass**，该工具对真实用户的价值为零。 【1｜生产调用必崩，且是不可恢复的进程级 panic（我用真实生产装配复现了完整栈）】 /Users/ll/code/yanshi/internal/shell/factory.go:74-78 `DefaultSecureFactory.Start` 返回 `&secproc.StartedProcess{PID, Stdout, Stderr}`——**没有 Cmd 字段**。而 /Users/ll/code/yanshi/internal/tools/secproc_capture.go:85 无条件执行 `started.Cmd.Wait()`。全仓 grep `Cmd != nil|Cmd == nil` 在 internal/ 下零命中（唯一命中在 third_party/bubbletea/tea.go:685，无关），即无任何 nil 守卫。 我用 bootstrap.go:887-892 的**完全一致装配**（`shell.DefaultSecureFactory{OS: shell.OSProcessFactory{}}`）跑 run_tests，实测栈： `panic: SIGSEGV → os/exec.(*Cmd).Wait → tools.runSecureCapture(secproc_capture.go:85) → tools.runTests(tes…
- **证据**：/Users/ll/code/yanshi/internal/tools/testrun.go:45-55 NewTestRunTool（run_tests GuardedTool，参数 framework/packages/filter/timeout_s） ； /Users/ll/code/yanshi/internal/tools/testrun.go:98-126 testSpec：go → `go test -json`、cargo → `cargo test`、npm → `npm test -- --json` ； /Users/ll/code/yanshi/internal/t…

#### `B3` DT5 — diagnostics 工具

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：一次调用聚合；各子项可独立失败不拖垮；toolchain 版本准确
- **实测缺口**：骨架与 LSP 聚合入口都在，但三处返回硬编码/恒空：sandbox 三字段硬编码 "unknown"（数据源 SandboxFromContext + Report() 明明已存在）、recentFiles 恒 nil 使 open_diagnostics_count 生产恒为 0、workspace 只有 root。规划要的「各子项并发」未实现（串行）。加上生产 Factory nil-panic，实际调用会崩。
- **二审改判理由**：我自己逐条复核后认为一审的「部分实现」仍然高估，应降级为「未实现」。核心新证据：生产路径下该工具**必然 panic 且不返回任何字段**，不是「部分能用」。 (1) nil-Cmd panic 是无条件的，我完整追了调用链并实测验证：唯一的生产 Factory 是 /Users/ll/code/yanshi/internal/shell/factory.go:74-78 `DefaultSecureFactory.Start`，其返回的 `&secproc.StartedProcess{PID:..., Stdout:..., Stderr:...}` **根本没有 Cmd 字段赋值**；而 /Users/ll/code/yanshi/internal/tools/secproc_capture.go:85 无条件执行 `waitErr := started.Cmd.Wait()`，全仓库 grep 只有这一处用 `started.Cmd` 且**没有任何 nil 守卫**。我 grep 了全仓非测试代码的 `Cmd:` 赋值，只有 /Users/ll/code/yanshi/internal/acp/spawn.go:112（另一个 Spawned 类型），**没有任何生产代码给 StartedProcess.Cmd 赋值**；只有测试工厂赋值（secproc_capture_test.go:90、git_test.go:73），这正是 3 个测试能通过而生产会崩的原因。我另写最小程序实测 nil *exec.Cmd 调 Wait() → "invalid memory address or nil pointer dereference"。 (2) panic 无人 recover 且会打…
- **证据**：/Users/ll/code/yanshi/internal/tools/diagnostics.go:32-41 NewDiagnosticsTool（diagnostics GuardedTool） ； /Users/ll/code/yanshi/internal/tools/diagnostics.go:43-58 runDiagnostics 聚合 workspace/git/sandbox/toolchain/lsp 五段 ； /Users/ll/code/yanshi/internal/tools/diagnostics.go:101-123 runToolchainProbes（…

#### `B3` T11 — web_search 工具

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：返回标题/摘要/URL；域名/时间过滤生效；重定向受策略约束；后端不可用降级
- **实测缺口**：规划怎么说：web_search{query, domains[], since} → {title, snippet, url, ref_id}[]，域名/时间过滤生效，支持 DuckDuckGo（无 key）+ 可配 Bing/Tavily。实际怎么做：参数只有 query/max_results，无 domains/since/ref_id；URL 字段恒为空串、Title 是未解析的整行 HTML、Snippet 从不填；选择器 class="result-link" 与真实 DDG lite 页面不匹配（实抓验证 0 命中）→ 生产恒返回空结果集；后端硬编码不可配。网络策略约束（netpolicy CheckHost + Transport）与「后端不可用降级为空结果」这两项是真做了的。
- **二审改判理由**：推翻一审「部分实现」，降级为「未实现」。一审的方向对，但低估了缺陷的性质：这不是"做了一半"，而是搜索功能的核心零产出，且是输入无关的死路。 我自己的证据链（不依赖一审）： 1）URL 是硬编码空串，与输入无关。/Users/ll/code/yanshi/internal/tools/web.go:216-221 的解析循环体只有一行 `results = append(results, searchItem{Title: line, URL: ""})`。`searchItem` 三个字段（web.go:166-170 Title/URL/Snippet），其中 URL 被字面量 `""` 覆盖、Snippet 全文件从未被赋值（`rg 'Snippet' internal/tools/web.go` 只有 struct 定义处 1 命中）。Title 塞的是 `line`，即 `strings.TrimSpace` 后的整行原始 HTML。 2）我实际运行了这条路径（临时 test，已删除，工作树未改动）。用 httptest 喂标准夹具后，工具真实输出为： `{"results":[{"title":"<a class=\"result-link\" href=\"https://example.com\">Example</a>","url":""}]}` 即便在最理想的、选择器完全匹配的输入下，`url` 依然是空串、无 `snippet`、`title` 是未解析的 `<a>` 标签原文。验收标准第 1 条「返回标题/摘要/URL」在任何输入下都 0/3 成立 —— 这是输入无关的结构性失败，不是"后端不给数据"。 3）真实后端永不命中。searchBase 硬编码 `https:/…
- **证据**：/Users/ll/code/yanshi/internal/tools/web.go:44-53 w.Search = NewGuardedTool("web_search", ...)，参数只有 query + max_results ； /Users/ll/code/yanshi/internal/tools/web.go:175-226 runSearch：走 netpolicy 策略校验（CheckHost）+ netpolicy.NewTransport ； /Users/ll/code/yanshi/internal/bootstrap/bootstrap.go:583,591 …

#### `B3` W07 — git_status / git_diff 专用工具

- **优先级** P2 ｜ **路线图原状态** 部分（路线图标注：只能经 shell 调 git，无产品级结构化语义） ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：status/diff 结构化；不修改用户配置；大 diff 成 artifact；边界清晰
- **实测缺口**：结构化 status/diff 与 scope/artifact/config 隔离都真做了，但三处实测缺陷：(1) 含空格的已跟踪路径在 git_status 里被截断；(2) staged 与 untracked 文件被误判 binary 且不返 patch；(3) 生产 Factory 不填 StartedProcess.Cmd，工具在真实运行时路径上 nil-panic，即 git_status/git_diff 在 bootstrap 装配下根本跑不通。测试用自制 Factory 填了 Cmd，掩盖了这条断裂。
- **二审改判理由**：我用 `go test -overlay`（不写入仓库）以 bootstrap.go:887-892 + orchestrator.go:280 的完全相同装配（shell.DefaultSecureFactory{OS: shell.OSProcessFactory{}} → tools.WithSecureProcessFactory）直接调用 tools.NewGitTools().Status.InvokableRun，得到进程级崩溃：panic: runtime error: invalid memory address，栈为 os/exec.(*Cmd).Wait ← internal/tools/secproc_capture.go:85 ← internal/tools/git.go:73 runGitStatus ← internal/tools/guard.go:84 SyncStream.func1.1。根因确凿：secproc.StartedProcess 定义了 Cmd *exec.Cmd（internal/secproc/secproc.go:50-55），而 internal/shell/factory.go:74-78 的 return 只填 PID/Stdout/Stderr，Cmd 恒为 nil；grep 全仓非测试代码，`Start(ctx, spec) (*secproc.StartedProcess, error)` 的唯一生产实现就是 DefaultSecureFactory（internal/shell/factory.go:41），且唯一的 ctx 绑定点是 orchestrator.go:280 `if o.secureFactory != ni…
- **证据**：/Users/ll/code/yanshi/internal/tools/git.go:15-63 GitTools/NewGitTools（Status + Diff 两个 GuardedTool，非占位） ； /Users/ll/code/yanshi/internal/tools/git.go:171-198 gitDiffCommands 支持 working_tree/base_ref/commit 三种 scope ； /Users/ll/code/yanshi/internal/tools/git.go:39-48 gitEnvIsolation（GIT_CONFIG_NOSYS…

#### `C1` AU1 — Automations（计划任务）

- **优先级** P2 ｜ **路线图原状态** 缺失（deepseek automation_*） ｜ **接进运行时** 否 ｜ **有针对性测试** 是
- **验收标准**：可创建计划任务；按时触发入队；生命周期可控；持久化；approval 门禁
- **实测缺口**：领域层（Manager/Scheduler/Repository/Schedule/QueuePort/A2Adapter）与八个工具全部是真实实现，持久化走 store KV + JSON envelope（注意：规划文档说"internal/store 持久"，实际选择的是 KV envelope 而非新建表，这一点 plan 已显式说明，不算偏离）。缺口有三层：(1) 完全未接进 bootstrap.Build —— BuildAutomation/BuildC1/NewA2Adapter 无任何生产调用点，scheduler goroutine 从不启动，八个 automation_* 工具从不进 allTools；(2) App.Shutdown 未等待 scheduler 退出，plan Task 8 要求的 App.c1Scheduler 字段不存在；(3) 端到端断链——A2Adapter 提交 broker 任务类型 "automation.run"，但仓库里没有任何 worker/executor 按类型分派它，唯一的 Executor 实现是 EchoExecutor，所以即便接线了，automation run 也只会被回显而不会真正跑 agent。验收中的"按时触发入队"在包级测试里成立（fakeQueue），在真实运行时不成立。
- **二审改判理由**：一审「部分实现」高估了。我试图找升级证据（换关键词搜 cron/schedul/CLI 子命令/HTTP 路由/TUI 斜杠命令/sdk 契约）全部落空，反而找到一审遗漏的两条硬伤，判定应降到「未实现」。 (1) 组合根零感知，不是"少接一根线"而是完全不知道 C1 存在：/Users/ll/code/yanshi/internal/bootstrap/bootstrap.go 全文 1337 行，`grep 'C1|c1|RLM|rlm|Batch|batch|Automation|automation'` 零命中——连 `cfg.Batch` 这个配置块在 bootstrap.go 里都从未被读取。全仓非测试代码中 BuildC1/BuildAutomation/BuildRLM/NewA2Adapter 的调用点只有 c1.go:155 自我调用一处；三个真实入口 /Users/ll/code/yanshi/cmd/yanshi/app.go:36、/Users/ll/code/yanshi/cmd/yanshi/main.go:450 与 :730、/Users/ll/code/yanshi/internal/cli/session.go:123 全部走 bootstrap.Build，与 C1 无任何交集。八个 automation_* 工具从未进 allTools（allTools 的全部 24 处 append 都在 bootstrap.go，c1.go 里的 "allTools" 只出现在注释 c1.go:19 和 c1.go:80 中，是对未兑现意图的描述）。 (2) 一审未发现的断链第二层——durable work task 本身也不派发：/Users/ll/code/ya…
- **证据**：/Users/ll/code/yanshi/internal/agent/automation/model.go:57 QueuePort 接口 {SubmitRun, Lookup} ； /Users/ll/code/yanshi/internal/agent/automation/model.go:13-20 Run 状态常量 + StateSchemaVersion ； /Users/ll/code/yanshi/internal/agent/automation/model.go MapTaskStatus 显式映射表（cancelled→canceled、timeout→failed…

#### `C1` M07 — CSV 批量 agent jobs

- **优先级** P2 ｜ **路线图原状态** 部分（codex comparison M07） ｜ **接进运行时** 否 ｜ **有针对性测试** 是
- **验收标准**：可提交批量任务；限并发；逐项结果+汇总可查
- **实测缺口**：规划落点是 internal/tools/agent.go（批量入口）+ internal/task/（job result 模型），实际做成了独立的 internal/agent/batch 包 + 独立的 internal/tools/batch.go 工具 agent_batch，且没有 internal/task/ 侧的 job result 模型，也没有规划里点名的 report_agent_job_result 汇总工具——汇总以 batch.Report 的 JSON 字符串直接作为工具结果返回。并发上限确实复用了 M04b/B1 registry 的统一 cap（SpawnErrCap 非阻塞退避），这点符合规划意图。失败项策略是"记录 per-row Error"，规划风险条提到的"失败项重试策略"未实现（只有 cap 满载重试，不重试业务失败）。同样完全未接进 bootstrap.Build，agent_batch 不进 allTools，默认 profile 白名单也没有它。
- **二审改判理由**：推翻方向是**继续降级**（部分实现 → 未实现）。我尝试为「部分实现」找运行时落地证据，全部落空，且发现一审低估了断链的深度。 1) 断链比一审说的更彻底。一审只说「NewBatchTools 未接进 bootstrap.Build」，实际是**整个 /Users/ll/code/yanshi/internal/bootstrap/c1.go 文件都是死代码**：我执行 `rg -n "BuildC1|BuildRLM|BuildAutomation|C1Components" --glob '*.go' | grep -v '_test.go'`，命中全部落在 c1.go 自身（:56 BuildRLM、:91 BuildAutomation、:134 BuildC1、:163 `Batch: tools.NewBatchTools(registryMgr)`），**无任何外部生产调用者**。即 M07 上游的组装函数本身就没人调，不只是「最后一根线没接」。 2) 组合根确认零引用。`rg -n "c1|C1|RLM|rlm|automation|Automation|Batch" /Users/ll/code/yanshi/internal/bootstrap/bootstrap.go` 在这个 1337 行的唯一组合根里对 C1 零命中；`allTools` 装配区间（bootstrap.go:587–762）逐条列举了 web/shell/git/gh/skill/task/plan/gate/artifact/vcs/mcp/image 等工具，**没有 agent_batch**。`bootstrap.go:777 Tools: allTools` 是编排器唯一的工具来源，`rg …
- **证据**：/Users/ll/code/yanshi/internal/agent/batch/input.go:11 ParseCSV — encoding/csv，header 校验（空/重复/列数不一致全拒），稳定 Row.Index ； /Users/ll/code/yanshi/internal/agent/batch/input.go:47 ParseStructured — []map[string]string → Row，空行/空键拒绝 ； /Users/ll/code/yanshi/internal/agent/batch/runner.go:60 Runner{Spawn, Man…

#### `C2` UX3 — @path 文件附加上下文

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 否 ｜ **有针对性测试** 否
- **验收标准**：`@` 触发补全;附加有界;越权拒绝;超大提示 fs_read
- **实测缺口**：规划：@ 触发文件补全 → 选中后作为有界 user context item 注入（硬大小上限，超阈值提示 fs_read，路径经 guard fs 校验）。实际：完全没有任何 @ 触发、文件补全、附件 frame 字段或服务端 handler。plan 文档在三轮 review 后主动把 UX3 移出本批（理由：附件读取必须在服务端做真实 profile+guard.Check，MVP 做不到），因此这是**有意识的未实现**，但路线图条目本身仍未兑现。
- **证据**：搜索路径：/Users/ll/code/yanshi/internal/cli/、/Users/ll/code/yanshi/internal/tools/、/Users/ll/code/yanshi/internal/proto/、全仓 *.go ； 搜索词：AttachmentRef / Attachments / attachment / @path / atPath / at_path / pathComplete / fileComplete / globComplete / contextItem / ctxItem / IndexByte(s,'@') / HasPrefix("…

#### `D2` O12 — IDE 扩展（VS Code）

- **优先级** P3 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 否 ｜ **有针对性测试** 是
- **验收标准**：turn 发起/取消/流式；selection/open files 注入；断线恢复
- **实测缺口**：规划说“turn 发起/取消/流式；selection/open files 注入；断线恢复”。实际：前两项完整可用且有测试；**断线恢复只写了状态机、没接进扩展**——runWithRecovery 是一个孤立的纯函数模块，extension.ts 完全没有 import 它，真实断线时只会在 OutputChannel 打一行 `[connection error]` 就结束（ide/vscode/src/extension.ts:93-97）。ide/vscode/README.md 的“Recovery model”一节把它描述成扩展的实际行为，与代码不符。另有三处次级缺口：(1) selection/open files 虽然被采集并作为 `context` 字段发出（sdk/ts/src/client.ts:85-87 强制 cast 附加），但服务端 internal/api/v1/types.go:131-140 的 TurnStartParams 根本没有 Context 字段，agent_v1.go 用标准 encoding/json 解码会静默丢弃——即“注入”目前是单向空转，服务端收不到；(2) diff 依赖的 fileChange item 服务端从不产生（`rg -rn "fileChange|FileChange" internal/` 零命中），showDiff 命令实际永远是“no file diff received yet”；(3) token 只能读不能写，扩展缺少设置 token 的命令，非 loopback 场景不可用。整个 ide/vscode 与两个 SDK 的测试都游离在 CI 之外。
- **二审改判理由**：一审判「部分实现」仍然高估了。我自己实测发现两个一审完全没查到的、致命且可复现的打包缺陷，导致这个扩展在真实 VS Code 里根本无法激活——三条验收标准无一条端到端可用。 【致命缺陷一：声明的入口文件永远不会被生成】/Users/ll/code/yanshi/ide/vscode/package.json:15 声明 `"main": "./dist/extension.js"`，但 /Users/ll/code/yanshi/ide/vscode/tsconfig.json:9 是 `"rootDir": "."`、:15 `"include": ["src/**/*.ts"]`、:8 `"outDir": "dist"`。实跑 `npx tsc -p tsconfig.json --outDir /tmp/dist-check` 产出的是 `/tmp/dist-check/src/extension.js`（即真实产物为 `dist/src/extension.js`）。`dist/extension.js` 这个路径永远不存在，VS Code 加载扩展时直接 "Cannot find module"。且 `dist/` 被 .gitignore（ide/vscode/.gitignore:2），仓库里也确实无 dist 目录，从未有人验证过 `npm run compile` 的产物路径。 【致命缺陷二：CJS/ESM 不兼容，即使路径修对了 require 也会抛】ide/vscode/package.json 没有 `"type"` 字段（node -e 验证 `type= undefined`）→ 该包是 CommonJS；tsconfig `"module": "NodeNex…
- **证据**：/Users/ll/code/yanshi/ide/vscode/package.json:31-36 contributes.commands 声明 yanshi.run / yanshi.cancel / yanshi.showDiff；:38-68 contributes.configuration 声明 yanshi.serverUrl / streamTransport / maxOpenFiles / maxContextBytes ； /Users/ll/code/yanshi/ide/vscode/src/extension.ts:26 `export class TurnCo…

#### `F2` CCL1 — mid-turn 压缩 cooldown

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：同 turn 不重复压缩；逼近上限仍触发；keepRecent 文档清晰
- **实测缺口**：两条缺口： (1) **CooldownTokens 用了全局回退 window 而非 per-model window**。bootstrap.go:794 写的是 `int(cfg.Compaction.CooldownFraction * float64(cfg.Compaction.ContextWindow))`，而 cfg.Compaction.ContextWindow 是 applyDefaults 的 256000 回退值（config.go:517-518）。plan Step 5b 原文写的是 `int(cfg.Compaction.CooldownFraction * float64(contextWindow))`（局部变量 contextWindow = per-provider 窗口）。同一处 orchestrator.CompactionConfig.ContextWindow 也传的是全局回退值。结果：给 128K 窗口模型配的 cooldown 阈值是 12800（0.05×256000）而不是 6400，且 hard-force 判定按 256000 算 → 对小窗口模型 cooldown 偏严、hard-force 偏晚。注意这是 F2 之前就有的既存偏差（CompactionConfig.ContextWindow 一直传全局值，per-model 窗口只在 api/http 的 contextWindowFor 里做），CCL1 只是把 cooldown 挂在了同一个偏差上。 (2) **keepRecent 双语义文档化只做了一半**。规划验收要求"`keepRecent` 文档清晰"，代码承重注释（compacting.go:66-68）和 CLA…
- **二审改判理由**：推翻一审「部分实现」，降级为「未实现」。代码确实存在且接进运行时（一审行号基本准确），但核心验收标准「同 turn 不重复压缩」在出厂默认配置下完全失效——这不是一审所说的两条边缘缺口，而是主功能不生效。 根因：token 会计口径错配（compacting.go:143 vs :158/:192）。maybeCompact 成功分支存的是 c.lastCompactTokens = res.TokensAfter（:143），即压缩「后」token 数；而 inCooldown 比较的 tokens := ctxcompact.EstimateTokens(msgs)（:158）是下一次进来的完整未压缩历史。二者不同量纲，:192 直接相减。 为什么下一轮拿到的仍是完整历史：压缩结果从未写回 ADK state。CompactingModel.Generate/Stream（compacting.go:104-118）只把压缩后切片传给 c.Inner，返回值不含消息列表；ADK 侧 typedStateModelWrapper.Generate（eino@v0.9.12/adk/wrappers.go:1257）以 state.Messages 调内层模型，随后 state.Messages = append(state.Messages, result)（:1273）并回写（:1294-1299）——压缩后的切片没有任何路径进入 st.Messages。react.go:369/455 的 st.Messages 也只做 append。我 grep 全仓 `state.Messages =`（排除 _test.go）零命中；completion.go:99-108 的 messageRec…
- **证据**：/Users/ll/code/yanshi/internal/llm/eino/compacting.go:71-96 — CompactingModel 新增 CooldownTokens / CooldownDuration / HardForceFraction / lastCompactTokens / lastCompactAt / cmMu sync.Mutex ； /Users/ll/code/yanshi/internal/llm/eino/compacting.go:149-178 shouldCompact：HardForceFraction 优先短路 → threshol…

---

## 4. 实现有差别（2 项）

功能做出来了，但方案、接口、命名或范围与规划明显不同。差异本身不一定是问题，但需要回写路线图/文档以免后续决策踩空。

#### `A2` DT2 — 验证门 (task_gate_run + evidence)

- **优先级** P1 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：gate 证据结构完整；大输出成 artifact；挂到正确 task；退出码/duration 准确
- **实测缺口**：无差异。Evidence 结构完整、大输出成 artifact、挂到正确 task、退出码与 duration 真实测量。唯一次要偏差：gate 通过 exec.CommandContext 直接执行（gate.go:105 shellCommand），未经路线图"依赖 [T07/T08] 经 shell runtime 执行"提到的 shell session runtime；但仍走了 guard Authorize，安全边界未降级。另 EmitWorkEvent(gate.go:146) 发出的 task_update 帧同样受 G05 的 EmitWorkFrame 断链影响，前端看不到。
- **二审改判理由**：核心实现真实、非占位、已接入运行时，四条验收标准在正常路径下我用真实 store 跑通验证了（见 extraEvidence 的实测数据）。但一审"无差异、安全边界未降级"的结论可证伪，故推翻为「有差别」。 【已核实为真的部分】/Users/ll/code/yanshi/internal/tools/gate.go 行号与一审描述一致：NewGateTools:33-50、withinRootAbs:81-84、Authorize:88-94、CombinedOutput+exitCode:107-127、spill:128-137、RecordGate 传播:139-141。注册链完整：bootstrap.go:735-737 → allTools；workMgr 经 bootstrap.go:783 orchConfig.TaskManager → orchestrator.go:274 tools.WithTaskManager 注入 per-turn ctx，requireTaskManager(gate.go:95) 能取到。store.go:454 RecordGate 是真 SQL（INSERT OR REPLACE task_work_gates + 同事务 task_work_timeline），Store.Get:207-220 回读 Gates。 【一审漏掉的差异 1 —— 破坏性删除门被绕过（最严重）】gate.go:88-92 构造 guard.Action 时**没有填 Workdir**，而 shell.go:128 是 `guard.Action{Tool:"shell_run", Shell:a.Command, Workdir:s.root}`。guard.g…
- **证据**：/Users/ll/code/yanshi/internal/tools/gate.go:33-50 — NewGateTools 构造 task_gate_run，参数 task_id/gate/command/cwd/timeout/env ； /Users/ll/code/yanshi/internal/tools/gate.go:81-84 — cwd 先经 withinRootAbs（pathjail canonical kernel）jail ； /Users/ll/code/yanshi/internal/tools/gate.go:88-94 — Authorize 带 She…

#### `D1` V14 — 版本化 Agent API v1

- **优先级** P1 ｜ **路线图原状态** 部分 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：start/resume/interrupt + 流式 item 可用;有版本+Schema;兼容测试完善
- **实测缺口**：两处与规划的偏差：(1) **两份 schema 并存且详略不同** —— `internal/api/v1/schema.go` 的 schemaDocument 只有 3 个 $defs（Thread/Turn/Item），$id 为 https://yanshi.dev/schema/agent-api-v1.json；而 sdk/schema/v1/agent-api.schema.json 有 21 个 $defs（含 Params/Response/Capabilities/ItemType enum/ImageAttach），$id 为 .../agent-api/v1/agent-api.schema.json。运行时 GET /api/v1/schema/agent-v1.json 与 docs/api/schema.md 都只吐前者（已用 python 解析 schema.md 确认只有 Item/Thread/Turn），且没有任何测试或 CI 步骤校验二者一致。schema.go:13 自己的注释还写着「Task 9 expands this document (params/response shapes, item type enum)」——即 plan 的 Task 9 扩展并未落地到运行时 schema。(2) TurnStartParams.Images（types.go:138）在 wire 上存在、有 camelCase 测试（types_test.go:49），但 service.go 的 runTurn 从不读 p.Images（rg 确认 service.go/agent_v1.go 里零个 Images 引用），也从不调用 orchestrator.…
- **二审改判理由**：核心不是空壳，一审的行号也基本准确（我逐一核对：internal/api/v1/types.go:21 `const Version = "v1"`、service.go:85 NewService / :194 StartTurn / :252 Interrupt / :299 runTurn 全部存在且是真实现；runTurn:317 确实调用 `s.orch.EventsWithHistoryOpts` + :319 `ClassifyEventsWithUsage`，不是桩）。接线也是真的：bootstrap.go:820 NewService、:945 `srv.AgentV1(agentAPI)`、:996 `AgentAPI: agentAPI` → cmd/yanshi/app.go:47 `appserver.New(app.AgentAPI, cfg)`，HTTP 与 JSON-RPC 双传输共用同一 *v1.Service（internal/appserver/server.go:110-160 dispatch 覆盖 thread/start|resume|interrupt、turn/start|interrupt、capabilities）。我实跑 `go test ./internal/api/v1 ./internal/appserver ./internal/proto ./internal/api/http` 四个包全部 ok。 但我推翻「已实现」，降级为「有差别」，因为一审漏掉了一处、且低估了另两处的严重性——v1 对外契约里有 **三个字段是声明了但服务端永远不实现的死字段**： (1) **一审漏掉的：`ThreadSnapshot.Items` 永远为…
- **证据**：/Users/ll/code/yanshi/internal/api/v1/types.go:21 `const Version = "v1"`；:51 Thread / :66 Turn / :80 Item 全 camelCase tag（threadId/turnId/createdAt/structuredResult）；:106-140 ThreadStart/Resume/Interrupt/TurnStartParams；:144-171 四个 *Response；:177 Capabilities ； /Users/ll/code/yanshi/internal/api/v1/…

---

## 5. 部分实现（50 项）

核心机制真实存在且多数已接进运行时，但验收标准有明确未闭环之处。**这是数量最大的一档（50 项）**，也是投入产出比最高的改进池——大多只差最后一到两跳接线。

#### `M1` G02 — Goal Token 预算累计（UsageSink）

- **优先级** P1 ｜ **路线图原状态** roadmap §2 TD1 判定「✅ 已完成」 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：SpentTokens 随 LLM 调用累计；预算耗尽可靠停止并把原因持久化
- **实测缺口**：唯一残余：`yanshi goal` 只暴露 `-max-iters`（main.go:661），没有 `-max-tokens` flag，Budget.MaxTokens 在 CLI 上永远为 0，即 token 预算在命令行不可设。sink 与 overBudget 逻辑本身完整可用（库层与测试都在），仅 CLI 入口缺一个 flag。
- **二审改判理由**：一审的代码定位全部属实（usage.go:13/23/38/44/54、loop.go:45/54/68/110/126、planner.go:63、evaluators.go:185/231、tier.go:110/171、implementer.go:385 usageForwarder 在 :346/:375 两条 run 路径都被传入、main.go:743/758/759/767/775/794、store/kv.go:9 KVSet 是真 SQL upsert），代码非空壳、go test ./internal/agent/goalloop 通过。但一审把"缺 -max-tokens flag"轻描淡写为"唯一残余"，并据此仍判已实现，这是高估。我查到两处独立的运行时断裂，它们各自打掉验收标准的一半： （1）预算停止在发行二进制里 100% 不可达。全仓非测试代码里 `goalloop.Budget{` 只出现两次（cmd/yanshi/main.go:723 和 :774），两处都是 `Budget{MaxIterations: *maxIters}`，MaxTokens 恒为 0；而 loop.go:55 的 overBudget 首个条件就是 `l.cfg.Budget.MaxTokens > 0`，因此 overBudget() 在运行时永远返回 false，loop.go:110/:126 两处检查…
- **证据**：/Users/ll/code/yanshi/internal/agent/goalloop/usage.go:13 Usage、:19 Total()（TotalTokens 为 0 时回退 prompt+completion）、:38 UsageSink（sync.Mutex）、:44 Add ； /Users/ll/code/yanshi/internal/agent/goalloop/loop.go:20 Config.Sink、:45 spent()（sink 优先，无 sink 回退 Budget.SpentTokens）、:54 overBudget()、:110/:126 循环顶…

#### `M1` G03 — T0–T4 难度路由接通

- **优先级** P1 ｜ **路线图原状态** plan 声明验收覆盖 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：每个 tier（T0–T4）都产生真实结果；auto/强制 tier 可测；升级不再静默退出
- **实测缺口**：无差异。--fake-model 两轮 demo 路径按 plan 保留未动（main.go:711-723）。
- **二审改判理由**：一审引用的行号与符号全部真实存在（tier.go:128 SkillName / :153 Path / :171 EvaluatorsForTier / :197 ResolveTierFlag / :226 EscalationHint；main.go:690/:745/:838 确有接线；loop.go:171-179 确有 StopReasonEscalate），路由也确实从 CLI 可达（main.go:120 case "goal" → runGoal），不是死代码。但验收标准「每个 tier（T0–T4）都产生真实结果」有四处未满足，故降级为部分实现： （1）**已复现的真实缺陷：--fake-model 路径丢掉了 Tier**。main.go:718-724 构造 goalloop.Config 时只填 Planner/Implementer/Evaluators/Judge/Budget，**没有 Tier 字段**（对比 :769-777 真实路径有 `Tier: resolvedTier`）。零值 = TierQuickFix，于是 loop.go:171 EscalationHint(TierQuickFix) 产出错误提示。我实际跑了 `./yanshi goal -fake-model -tier t3 -max-iters 1 -goal "build a platform"`，输出为：`…
- **证据**：/Users/ll/code/yanshi/internal/agent/goalloop/tier.go:128 SkillName()、:153 Path()、:171 EvaluatorsForTier、:197 ResolveTierFlag、:226 EscalationHint ； /Users/ll/code/yanshi/cmd/yanshi/main.go:683 注释明确「silent lightweight path print-and-return for forced T0-T2 is removed」、:690 打印 tier+path、:745 if resolv…

#### `M1` O07 — yanshi doctor 自检子命令

- **优先级** P2 ｜ **路线图原状态** plan 声明 O07 验收逐条覆盖 ✅ ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：检查 config/DB/provider/ACP CLI/端口/lockfile/目录/sandbox；人类可读 + --json；退出码 0/1/2；不打印 secret；不完整环境不 panic
- **实测缺口**：一处过时：checkSandbox（doctor.go:515）仍硬编码返回「sandbox verification not implemented yet (arrives with S08 in M2)」的 warn 占位，但 `internal/sandbox` 包与 `config.SandboxConfig`（config.go:238）现在都已存在 —— 这个检查项没跟上实现，会在真配了 sandbox 的环境里误报 warn。其余 17 项都是真实检查。另外 doctor 后续扩了 10 个 plan 之外的检查项（WAL/keyring/LSP/secrets/locale/keymap/高对比度），是超集。
- **二审改判理由**：主体确实存在且真接进运行时（我实测过：`go build ./cmd/yanshi` 后 `yanshi doctor -config X [-json] [-release]` 真跑起来，18 项检查、文本/JSON 双渲染、缺失/畸形/空 config 都不 panic、raw api_key 不外泄 grep 计数为 0）。但一审把两个硬编码占位当作"一处过时"轻描淡写地放过了，这直接踩了两条：验收标准点名的 sandbox 项，以及"严禁占位符"的实现规则。 【硬伤 1｜checkSandbox 是纯常量占位，且导致 exit 0 不可达】internal/cli/doctor.go:515-517 的 `func checkSandbox() CheckResult` **不接任何参数**、不读 cfg、无条件 `return CheckResult{Name:"sandbox", Status:StatusWarn, Message:"sandbox verification not implemented yet (arrives with S08 in M2)"}`。而 internal/sandbox/factory.go:33 `func New(cfg Config) Sandbox` 与 types.go:55 `CapabilityReport{Platform/Requested/Effect…
- **证据**：/Users/ll/code/yanshi/internal/cli/doctor.go（665 行）:140 RunDoctor、:65 ExitCode、:104 RenderText、:125 RenderJSON、:181 skipped（config 失败降级不 panic） ； 检查项 18 个：checkConfig:192、checkConfigVersion:218、checkWAL:254、checkKeyringAvailability:285、checkDatabase:307、checkProviders:353、checkACP:396、checkLockfile:…

#### `M1` SPEC-TOOLIF — 工具接口标准（Tool 接口 + ToolChunk 流）

- **优先级** P1 ｜ **路线图原状态** plan 分 Lane 1-N，roadmap §2 提到「工具接口标准化」已把技术债清掉 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：所有工具统一到 Tool 接口（DisplayName+DefaultTimeout+Stream）；输出走固定字段 ToolChunk；废弃 JSON 包装与 ToolProgressCallback/lineProgressWriter；subagent 进度喂 Status+Text；TUI 固定模板渲染
- **实测缺口**：与 plan 的一处实现差异：plan 写「TUI 用一套固定模板消费同一个 Stream channel」，实际 TUI 是跨进程客户端，消费的是经 WS `tool_chunk` ServerFrame 中转后的字段（ws.go:594 toolChunkFrames buffered channel → proto 帧 → model.go:748），而非直接持有 tools.ToolChunk channel。语义等价（固定字段 Text/Status/Err 原样过桥），但架构上多了一层帧转换 —— plan 的表述在 client/server 分离下本就不可能字面实现。
- **二审改判理由**：核心骨架属实且已接进运行时（我逐行核验过，一审行号无编造）：internal/tools/guard.go:52 ToolChunk、:62 StreamFunc、:65-69 Tool 接口、:79 SyncStream、:198 Stream（Authorize→WithTimeout→streamFunc→tee）、:243 InvokableRun 只收 Result；internal/bootstrap/bootstrap.go:587-762 组装 allTools → :777 orchestrator.Config.Tools；ws.go:610 WithToolChunkCallback → tool_chunk 帧；tui/model.go:748 消费。但一审漏掉了三处硬伤，其中两处我用真实测试跑出来了： （1）【实测出的运行时 BUG，非纸面差异】8 个工具的 DefaultTimeout()==0：internal/tools/agent.go:128/138/144/149/156/162/168/173 的 agent_spawn/agent_wait/agent_result/agent_send_input/agent_resume/agent_assign/agent_cancel/agent_list 全部传 timeout=0。而 guard.go:213 `context.Wit…
- **证据**：/Users/ll/code/yanshi/internal/tools/guard.go:52 ToolChunk{Text,Status,Result,Overwrite,Err}、:62 StreamFunc、:65 Tool 接口（:67 DisplayName、:68 DefaultTimeout）、:79 SyncStream、:198 GuardedTool.Stream（Authorize + ctx 超时 + streamFunc，唯一执行入口） ； /Users/ll/code/yanshi/internal/tools/guard.go:163 NewGuardedToo…

#### `B0` TD1 — SpentTokens 死代码 / goal loop token budget 失效

- **优先级** -（B0 前置技术债，路线图未标 P 级，按上下文等价 P0 阻塞项） ｜ **路线图原状态** 已结项 ✅ 已完成（roadmap 原说法：budget 控制完全失效） ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：路线图未给 B0 独立验收标准，仅在实测表里给出判定依据："goalloop/usage.go 完整 UsageSink+addUsage/usageFromMeta；loop.go spent()→sink、overBudget() 在循环顶 & plan 后双检查；planner/evaluators/tier 全接 Sink；TestLoop_BudgetStopsOnAccumulatedUsage 等测试守护"；隐含目标为原缺口"budget 控制完全失效"被修复
- **实测缺口**：计量管道（sink 累加 + loop 双检查 + 全组件接线 + ACP 子进程 usage 回流）确实是真实完整实现，路线图说的"死代码"已不成立。但**预算阈值在生产路径上永远为 0**：overBudget() 的门是 `MaxTokens > 0 &&`，而 cmd/yanshi/main.go:723 与 :774 两处 Budget 字面量只设 MaxIterations，CLI 只有 -max-iters 没有 -max-tokens，config.example.yaml / internal/config 也没有对应配置项。结果是 spent() 无论累到多少，overBudget() 恒为 false，token 预算在真实运行时**从不生效**——只有单元测试通过手工构造 Budget{MaxTokens: 100} 才走得到这条分支。即"计量已修好，闸门没接线"。另外 ACP 的 usage 依赖外部 CLI 真的发 usage_report 事件（acp/client.go:250 解析该 discriminator），codex/claudecode 不发时该路径静默计 0。附带发现：implementer.go:434-443 的 integrator.merge 仍是 `return nil` 的 TODO(M7) 占位（非 TD1 范围，但在同文件内）。
- **证据**：/Users/ll/code/yanshi/internal/agent/goalloop/usage.go:13-82 — Usage/UsageSink/Add/Snapshot/usageFromMeta/addUsage 全部为真实实现，UsageSink 用 sync.Mutex 保护，Total() 在 TotalTokens==0 时回落 prompt+completion ； /Users/ll/code/yanshi/internal/agent/goalloop/loop.go:45-56 — spent() 优先读 l.cfg.Sink.Snapshot().Total(…

#### `A1` S06 — 结构化 Shell 策略 (execpolicy)

- **优先级** P0 ｜ **路线图原状态** 部分 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：能识别程序/参数/管道/重定向；规则结果可解释；已知绕过样例（IFS、$()、glob 注入）有回归测试
- **实测缺口**：无差异（功能层面）。两个非阻塞的边角：(1) config.example.yaml 与 docs/user-guide/configuration.md 从未出现 `rules:` 示例（grep `rules:` 在 config.example.yaml 零命中），操作员无从发现该能力；(2) Evaluate 对含 &&/|| 的 Command 直接返回 hard_deny('control-token')，即规划设计里说的『对管道每段独立判定』只对 `|` 生效，&&/|| 是解析后拒绝而非分段判定 —— 与 guard 元字符 HardDeny 叠加是一致的深度防御，但与设计描述的措辞有出入。
- **二审改判理由**：引擎本体是真代码（lexer.go/parser.go/policy.go，99.4% 覆盖，无桩），一审引用的行号我逐一核对属实：guard.go:292-308 确为 execpolicy 块，profile.go:107 确为 Rules []execpolicy.Rule `yaml:"rules"`，permctx.go:162 确调 execpolicy.Parse。但我自己用 go -overlay 探针（不落盘到仓库）打真实 guard.Check 后发现三处一审漏掉的实质缺口，足以从「已实现」降为「部分实现」：(1) 规则引擎存在可证明的 deny 绕过——policy.go:135-147 的 containsAny 只匹配 arg==flag 或 strings.HasPrefix(arg, flag+"=")，实测 `go test -tags=e2e_real ./internal/acp` → HardDeny/no-real-e2e，而 `go test -tags e2e_real ./internal/acp`（空格分隔，Go 工具链合法写法）→ Allow/go-test，`--tags=e2e_real` 同样 → Allow。这恰好击穿 policy.go:13-18 文档注释自己标榜的 no-real-e2e 招牌用例，说明 deny 规则不可靠。(2) 规则表在所有出厂配置中处…
- **证据**：/Users/ll/code/yanshi/internal/execpolicy/lexer.go — Lex()，byte-index 扫描，正确消费 && / \|\| / >> / 2>> / fd redirect；forbiddenExpansionAt 对 $ ` * ? [ %VAR% fail-closed 拒绝 ； /Users/ll/code/yanshi/internal/execpolicy/parser.go — Parse() 产出 Command{Segments[]Segment{Program,Args,Redirects},Control}；normali…

#### `A1` S07 — 持久审批规则

- **优先级** P0 ｜ **路线图原状态** 部分 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：规则含来源/scope/过期；每次命中可审计；用户可查看撤销；前缀规则有绕过回归测试
- **实测缺口**：核心全通，但有两点与规划语义不完全吻合：(1) 规划验收要求『前缀规则有绕过回归测试』—— Manager.Match 用 reflect.DeepEqual(r.Scope, scope) 做**精确相等**匹配，并非前缀匹配，因此不存在『前缀规则被绕过』的攻击面，但也意味着 S07 设计里的『session(prefix) 前缀规则』这一档实际没实现（scope.Prefix 存的是完整 argv，比对时全量相等）。approval 与 tools 的测试里 grep 不到任何前缀绕过回归测试。(2) TTLOnce 与 Rule.ExpiresAt 在生产代码路径中从未被写入 —— permctx.go 只会 Record TTLSession/TTLPersistent，grep ExpiresAt 在 internal/tools 与 internal/approval 生产代码零命中（只有 ws.go:1071 读它渲染），所以『规则含过期时间/过期自动失效』只在 manager 内部实现且有单测，运行时无人产生带 ExpiresAt 的规则。
- **二审改判理由**：核心真实且已接进运行时（非空壳）：internal/approval/manager.go:67 Match / :102 Record / :136 List / :149 Revoke / :197 persistLocked 全为真实实现；bootstrap.go:867 approval.New(st,...) 用真实 *store.Store（:335 OpenWith 失败即硬错，st 不可能为 nil）→ :897 orchConfig.Approvals → orchestrator.go:275-276 WithApprovalManager → permctx.go:312 Match 位于授权热路径 → ws.go:1050/1075 → tui/commands.go:72,803,806 /permissions。审计与查看/撤销两项验收完全达标。但验收 4 项中有 2 项自查不成立，故降级为部分实现：(1)『规则含过期』在运行时恒假——permctx.go:340 与 :352 两处唯一的 Record 调用点构造 approval.Rule{ID,Action,Scope,TTL,Source}，均不设 ExpiresAt；全仓非测试代码里 ExpiresAt 在 approval 链路上只有 ws.go:1071（渲染读取）与 manager.go:185（内部判断），无任何生产写入方，导…
- **证据**：/Users/ll/code/yanshi/internal/approval/types.go:25-29 TTLOnce/TTLSession/TTLPersistent；types.go:38-40 SourceUser/SourceMode；types.go:47-54 Scope{Tool,Program,Prefix,FSOp,Paths,Host}；types.go:60-69 Rule{ID,Action,Scope,TTL,Source,CreatedAt,ExpiresAt,ProcessID}；types.go:75-82 AuditEvent ； /Users/ll/c…

#### `A1` S09 — 子进程网络隔离

- **优先级** P0 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：未授权连接失败；host/port 规则生效；DNS/重定向不能绕过；决策入审计
- **实测缺口**：规划说：sandbox 内所有子进程默认 deny 出站，经本地 network proxy 代理放行，决策写审计事件。实际做的是：(a) 代理服务器 NewProxy 写好了但**从未在生产代码中启动过**（bootstrap 传 ProxyURL:""），子进程拿到的 HTTP_PROXY 是无效的 http://127.0.0.1:0，效果是『HTTP 客户端型子进程连不上网』而非『按规则放行』；(b) 无 sandbox 支撑，子进程直接 connect() / 原始 socket / 无视 HTTP_PROXY 的程序完全不受约束（规划风险栏提到的 seccomp 收紧 socket 调用族未做）；(c) 规划要求『决策入审计』—— netpolicy 生产代码里 grep slog/audit 零命中，Decision.Rule/Reason 只回传给调用方，没有审计事件发射。真正生效的部分是宿主进程内的 web_fetch/web_search（走 NewTransport 的 resolve+pin+复检）。
- **证据**：/Users/ll/code/yanshi/internal/netpolicy/policy.go:37-42 Policy{Default,Allow,Deny,AllowPrivate}；:49 CheckHost（deny-wins、空/未知 Default fail-closed）；:77 CheckResolvedIPs（loopback/RFC1918/link-local/unspecified 拒绝，SSRF/DNS-重绑定防线） ； /Users/ll/code/yanshi/internal/netpolicy/proxy.go:40 PolicyDialer.DialC…

#### `A2` DT1 — Durable tasks (TaskManager)

- **优先级** P0 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：可创建/列出/读取/取消；状态机正确；thread/turn 关联准确；重启后持久恢复
- **实测缺口**：核心全部落地且接进 bootstrap + orchestrator context，验收的 create/list/read/cancel、状态机、持久恢复都真实可用。两点小差异：(1) thread/turn 关联字段存在且工具会写入（task.go:104-110 读 ThreadLinkFromContext），但 WS 层从未把真实 thread/turn 灌进 TurnOpts（见 notes），所以生产中 ThreadID/TurnID 恒为空串，task_list 的 thread filter 退化成等价 all=true；(2) 路线图提到的 createdWT 泄漏清理已在 broker.Cancel 中通过 worktree 回收处理。
- **二审改判理由**：代码本身真实、非桩（store.go 5 张表全是真 SQL，manager.go Create/Cancel 真落库，工具注册进 bootstrap.go:722-731 + orchConfig.TaskManager=workMgr @783 + dispRef.Bind @960 + orchestrator.go:274 WithTaskManager 注入），一审这部分没错。但验收标准 4 条中有 2 条在生产路径上不成立，一审把它们轻描淡写成"小差异"是错判： (1) 「thread/turn 关联准确」= 完全未实现，而非一审说的"小差异"。全仓 `TurnOpts{` 构造点只有 6 处，3 个生产入口 ws.go:644、chat.go:132、v1/service.go:313 全都没填 ThreadID/TurnID；唯一填的是 orchestrator.go:611 的子代理，而它读的是 `tools.ThreadLinkFromContext(ctx)`，父 ctx 本身就是空的，等于把空串往下传。所以 orchestrator.go:294 `tools.WithThreadLink(ctx, "", "")` 恒绑空串，task_work.thread_id/turn_id 列恒为 ''，task_list 的 thread filter 永久退化。铁证：v1/service.go:307…
- **证据**：/Users/ll/code/yanshi/internal/task/work/types.go:1-249 — WorkTask/Status/Checklist/Evidence/Artifact/TimelineEntry/Summary/CreateReq/Event/ManagerLike/Dispatcher 全部真实定义；Status.CanTransitionTo 终态拒绝转移；ClassificationFromExitCode ； /Users/ll/code/yanshi/internal/task/work/store.go:1-537 — SQLite 5 张表 t…

#### `A2` G05 — Plan mode + update_plan/checklist 工具

- **优先级** P1 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 否 ｜ **有针对性测试** 是
- **验收标准**：plan 模式禁编辑类工具；计划可流式更新；确认后切执行且历史连续；checklist 状态持久
- **实测缺口**：规划：guard 加 plan 门禁 + plan 工具 + /plan 命令 + 计划状态走 activity/sentinel 帧。实际：guard/工具/TUI 命令三块都真实落地且测试全绿，但 **plan 模式没有真正接进服务端 turn 路径**。三处断链：(1) internal/api/http/ws.go:644 构造 TurnOpts 时从不设置 PlanMode 字段，所以 orchestrator.withTurnContext 拿到的永远是 PlanMode=false → tools.WithPlanMode(ctx,false)，Authorize 的 plan firewall 与 filterPlanTools 在生产中恒不触发；plan 模式实际只靠 ws_perm.go 的 callback 层拦截（即只有走到 callback 的调用被拦，profile 静态放行的编辑类工具不经 callback，会漏过）。(2) FlushRunners 定义了但无人调用，plan→execute 切换时 runner 缓存不 flush（计划 Task 11 明确要求模式边界显式 flush）。(3) plan_update/checklist_update 帧从 proto 到 wsbackend.go:358-360 的 StreamEvent 映射都在，但 TUI 侧无 update c…
- **证据**：/Users/ll/code/yanshi/internal/guard/mode.go:30 — ModePlan PermissionMode = "plan" 已定义 ； /Users/ll/code/yanshi/internal/guard/mode.go:44-45 — allModes 含 ModePlan；cycleOrder 不含（Shift+Tab 不进 plan，符合约束 §7） ； /Users/ll/code/yanshi/internal/guard/mode.go:59-77 — planAllowedTools 白名单 22 项 + PlanToolAllowe…

#### `A3` C13 — `/mcp` 实化管理界面

- **优先级** P1 ｜ **路线图原状态** 占位（路线图原文：`/mcp` 占位返回空 server list，无状态/启停/错误详情） ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：展示 server/tool/status/error；enable/disable 生效；状态与 client 实际连接一致
- **实测缺口**：规划说：展示「resolved 配置路径、每 server 的 enabled/transport/command|URL/timeout/连接错误/发现到的 tools-resources-prompts」，且「支持 enable/disable/validate/reload（配置即时写，model-visible 工具池重启生效）」。实际做的：五个子命令齐全且都真调 Manager；渲染只有 name / transport / status / error / tool_count 五项 —— 缺 resolved 配置路径、command|URL、timeout、resources、prompts。「配置即时写」未做：enable/disable/reload 只作用于内存 Manager，config.yaml 不回写，进程重启后状态回到 YAML 原值。「model-visible 工具池重启生效」这一条与实现一致（工具在 bootstrap 一次性注册）。附带一处遗留：旧 mcp_list 帧构造器 proto.NewMCPList 变成死代码，服务端把 list_mcp 也回 mcp_status 了，与 frame.go:159-162 的 doc 注释描述不一致。
- **证据**：/Users/ll/code/yanshi/internal/cli/tui/commands.go:62 — 命令表注册 {name: "mcp", run: cmdMCP} ； /Users/ll/code/yanshi/internal/cli/tui/commands.go:633-654 cmdMCP — 子命令 list / validate / enable <s> / disable <s> / reload <s>，缺参与未知子命令都有 errorEntry 提示 ； /Users/ll/code/yanshi/internal/proto/frame.go:59-60 Cl…

#### `A3` MCP1 — MCP palette 发现

- **优先级** P2 ｜ **路线图原状态** 缺失（路线图原文：命令 palette 不含 MCP 工具，模型与用户难发现已接入 server 能力） ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：palette 含 MCP 工具分组；disabled/failed 可见标灰；命名与模型可见一致
- **实测缺口**：规划说：「palette 按 server 分组列出 MCP 工具，用 runtime 名 mcp_<server>_<tool>；disabled/failed server 仍可见（标灰）」。实际：分组结构、runtime 名、分组头跳过、选中插入都实现了且有测试，但有三处实效缺口。(1) 数据来源被动：paletteMCPServers 只由 mcp_status 帧填充，TUI 启动时不主动发 list_mcp，用户必须先手动执行一次 /mcp 才能在 palette 里看到任何 MCP 条目 —— 这与「让用户易发现 server 能力」的初衷相悖。(2) 分组头几乎必被过滤掉：updatePalette 用 strings.HasPrefix(c.name, prefix) 统一过滤，分组头名是 "── srv ──"，只有输入恰为单个 "/"（prefix 为空）时才留存；一旦用户输入 "/mcp"，工具条目留下、分组头被滤掉，实际呈现为无分组的平铺列表。(3) 「标灰」未实现：分组头统一用 toolMeta 样式，没有区分 disabled/failed 的灰化样式；且 Manager.Disable(manager.go:200-204) 会清空该 server 的 toolMap，disabled server 在 palette 里只剩一个大概率被过滤掉的空分组头。另外 Ctrl+K action …
- **证据**：/Users/ll/code/yanshi/internal/cli/tui/commands.go:19-29 — commandKind 枚举新增 cmdMCPTool / cmdMCPGroup ； /Users/ll/code/yanshi/internal/cli/tui/commands.go:306-319 paletteMCPItems — 按 server 生成 "── <name> ──" 分组头（非 ready 时追加 "[status]"），其下逐个 MCP 工具条目（name = qualified 运行时名） ； /Users/ll/code/yanshi/inte…

#### `A3` V16 — 通用 MCP Client

- **优先级** P0 ｜ **路线图原状态** 缺失（路线图原文：yanshi 只有 VCS MCP server，无 client 接入外部 MCP server，`… ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：stdio/HTTP server 可配可连；tools/resources 可用；启动超时/重连/权限检查有测试；命名冲突可诊断
- **实测缺口**：核心链路（stdio/HTTP 双传输 → Manager → GuardedTool → 工具注册表 → orchestrator per-turn 注入 → guard MCP 独立门禁 → YAML 配置）全部真实且已接进运行时，这部分与规划一致。四处明确缺口：(1) resources —— 规划设计写「`tools/list`+`call` 与 `resources/list`+`read` + prompts」，Client 层两个方法都实现了，但 Manager 不聚合、工具桥不暴露、ws_handlers 不下发，ServerStatus.Resources 永远是 nil，模型与用户都拿不到资源；(2) prompts 完全未实现，全仓库无任何 prompts/list 代码；(3) 断线重连 + 健康检查：internal/mcp/health.go 是完整实现且有 4 个测试，但 bootstrap.buildMCPManager 只调 StartAll，从不 SetHealthConfig / StartHealthLoop，工具桥也用 CallTool 而非 CallToolRetry —— 能力孤立在包内，运行时等于关闭；(4) OAuth：mcp.ServerConfig.OAuth + ClientCredentialsSource 已实现且被 newHTTPClientFor(manag…
- **证据**：/Users/ll/code/yanshi/internal/mcp/types.go:1-133 — ServerConfig/ServerStatus/ToolDescriptor/ResourceDescriptor/OAuthConfig + QualifyToolName（`mcp_<server>_<tool>`，>64 字符截 51+12hex 哈希）+ ParseQualifiedName，真实实现 ； /Users/ll/code/yanshi/internal/mcp/wire.go — WriteMessage(Content-Length) / WriteLineMes…

#### `B1` M04 — 完整生命周期 (wait/result/send_input/resume/assign/list)

- **优先级** P1 ｜ **路线图原状态** 部分（路线图声明缺口：只有同步 spawn，缺 list/message/follow-up/wait/interrup… ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：全部生命周期操作可用；线程树/深度/并发/usage 可查；取消不泄漏；resume 跨重启可尝试
- **实测缺口**：三个实质缺口。(1) **运行时槽位泄漏（严重）**：finishTerminal 只写 record + emit，从不 delete(m.runtime, agentID)；runningLocked()（manager.go:570）取 runtime map 长度与 StatusRunning 记录数的**较大值**，于是终态 agent 仍长期占用并发槽。我用临时测试实测：MaxConcurrent=2 时第 3 次 Spawn 直接 SpawnErrCap，而 List 报 Running=0。计划文档 Task 6 明确写了 finishTerminal 末尾要 `if rt := m.detachRuntime(agentID); rt != nil && rt.cancel != nil { rt.cancel() }`，实现漏掉了这一段，detachRuntime 函数本身在仓库里根本不存在。这直接违反验收里的『取消不泄漏』。(2) **interrupt 语义未接线**：turnCancel 字段有、SendInput 里读它、但生产路径没人给它赋值，interrupt=true 与 false 行为相同。(3) **usage 链路断在中段**：orchestrator.go:693 绑了 WithUsageSink → mgr.AddUsage，但全仓没有任何生产代码调用 UsageSinkFr…
- **证据**：/Users/ll/code/yanshi/internal/agent/registry/manager.go:36 type Manager（records/runtime/limit/path/bootID/rootCtx/closed/persistMu，真实实现非占位） ； /Users/ll/code/yanshi/internal/agent/registry/manager.go:104 Spawn、232 Result、249 List、278 SendInput、314 Assign、365 Cancel、387 Resume、498 Wait、539 Close、795 …

#### `B1` M04b — 持久化 + 并发上限 + 输出契约

- **优先级** P1 ｜ **路线图原状态** 部分（路线图声明缺口：子代理无跨重启持久化、无并发上限、无结构化输出契约） ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：重启后可 list/resume；并发上限生效；输出 5 段可解析；父可消费 EVIDENCE
- **实测缺口**：持久化与并发上限的**核心机制是真的且已接进 bootstrap/config**，但三处验收未闭环。(1) **并发上限只覆盖新入口**：路线图/计划要求『所有子代理入口先向同一个 Manager 申请 running slot』，实际只有 agent_spawn 走 Manager；agent_start / workflow_start / analysis 四个 legacy 入口仍走 runSubAgent + NumCPU 信号量，Manager 的 cap 对它们完全不生效。为此写的适配器 ManagedSubAgentRun（subagent.go:274）+ spawnWithRetry（:315）**只有测试调用，无任何生产调用方**，是标准的孤立代码。(2) **『父可消费 EVIDENCE』未实现**：解析器 ParentWorkingSetHint 写好了但没人调，agent_start 原样透传子代理输出，正是计划锁定项 #15 明确禁止的形态。(3) **config.example.yaml 缺 subagents 块**，也缺计划 Task 10 要求补的 `profiles.orchestrator.subagent: {models, max_reasoning_effort}` 示例——运行时读得到（有代码默认值），但操作者在示例配置里看不到这两个开关。另外并发计数受 M04 的 …
- **二审改判理由**：同意"部分实现"这个桶，但**不认同一审的证据构成**——它把一条实际是死代码的东西当作正面证据记入了，且低估了并发缺陷的严重性。逐条验收： (1) **「输出 5 段可解析」= 完全未实现，一审判错方向。** 一审把 internal/tools/agentroles.go:65 outputContractPrefix 列为正面证据，说"五段式 prompt 约束"已做。实际上**生产者一侧也是死的**：五段 prefix 唯一的注入点是 internal/agent/orchestrator/orchestrator.go:697-700 `if role := registry.RoleFromContext(ctx); role != ""`，而 `registry.WithRole`（internal/agent/registry/context.go:25）**全仓零生产调用方**——`rg --type go "WithRole\("` 只命中 context.go:25 定义 + coverage_test.go:25 + manager_coverage_test.go:33 两个测试。runner 的 ctx 链路我完整追了：manager.go:648 `rt.runner.Run(ctx,...)` 的 ctx 派生自 m.rootCtx = bootstrap.go:652 的 `contex…
- **证据**：/Users/ll/code/yanshi/internal/agent/registry/persist.go:9 persistenceSchemaVersion=1；:17 persistedState{schema_version, session_boot_id, agents}；:26 loadState 容忍未知字段、缺文件返回空态 ； /Users/ll/code/yanshi/internal/agent/registry/writeatomic_unix.go:11 temp+fsync+os.Rename；writeatomic_windows.go 存在（MoveFil…

#### `B2` LSP1 — LSP 诊断回喂

- **优先级** P1 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：编辑后模型收到诊断；server 缺失安全降级；超时不阻塞 turn；Go/Python/TS 至少一种端到端可用
- **实测缺口**：三处偏差。(1) 语言覆盖收窄：规划要 gopls/pyright/typescript-language-server/clangd/rust-analyzer 五套，实际 DefaultLanguages 只内置 go+python 两套（config.lsp.languages 可手工补，但开箱无 TS/C++/Rust）。detectLanguage 认得 ts/js/cpp/rust 扩展名，但没有对应默认 server 命令，等于死枝。(2) workspace 探测降级：规划要求'按已知标志文件(go.mod/package.json/Cargo.toml)确认'防误探测，实际只按文件扩展名 detectLanguage，无任何标志文件检查。(3) 回喂通道与规划不同：规划说'结果作为 sentinel/tool-result 回喂 + WS 诊断走 activity/sentinel 不进 transcript'，实际是把诊断文本塞进 fs_write/fs_edit/apply_patch 返回 JSON 的 diagnostics 字段，经 SyncStream 同时进 Result 与 Text，因此**会**进 TUI transcript 的工具输出块（plan 文档第 9 行已把这点明确记为有意设计）。没有 activity/sentinel 路径。另：规划落点提到的 internal/too…
- **证据**：/Users/ll/code/yanshi/internal/lsp/client.go:29 Client（持久 bufio.Reader + 单 goroutine readLoop demux；pending map[int64]chan 投递响应） ； /Users/ll/code/yanshi/internal/lsp/client.go:250 storeDiags 解析 textDocument/publishDiagnostics；client.go:298 notifyChange（didOpen/didChange 全量替换 + editGen 递增）；client.go:…

#### `B3` GH1 — GitHub 工具集 (issue/PR context, comment, close)

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：只读 context 可用；写操作需审批且需证据；大 body 成 artifact；未认证明确降级；注入内容不被当指令执行
- **实测缺口**：规划怎么说：只读 github_issue_context + github_pr_context（含 gh pr diff --patch），写操作 github_comment + github_close_issue（后者要非空验收标准+证据、拒绝脏工作树、绝不因 agent 停止就关 issue），大 body/diff→artifact，未认证软降级，PR 内容当不可信输入。实际怎么做：只做了 PR 侧（github_pr_context/comment/approve/merge），issue 侧（issue_context/close_issue）零实现，反而多做了规划没提的 approve/merge；证据门/脏树门/artifact/注入防护/未认证区分全部缺失。审批门（NewApprovalGuardedTool，YOLO 不可绕）是真做扎实了，yanshi pr <N> 入口也真接线了。
- **二审改判理由**：一审的差异清单我逐条复核过，事实全部属实，但结论应当从"有差别"下调为"部分实现"——差异不是等价的替代设计，而是净缺失，且交付的那一半在生产路径上根本跑不通。 我自己查到的核心证据： (1) 生产路径必 panic（一审提到但未验证机理，我实证了）。/Users/ll/code/yanshi/internal/shell/factory.go:74-78 是唯一的生产 secproc.Factory 实现（bootstrap.go:887 `shell.DefaultSecureFactory{OS: shell.OSProcessFactory{}}` 是唯一构造点），它返回 `&secproc.StartedProcess{PID:..., Stdout:..., Stderr:...}` —— **Cmd 字段从未赋值**。而 /Users/ll/code/yanshi/internal/tools/secproc_capture.go:85 无条件执行 `waitErr := started.Cmd.Wait()`。我写了独立程序实测 nil *exec.Cmd 调 Wait()：`PANIC: runtime error: invalid memory address or nil pointer dereference`。且 `grep -rn "recover()" internal/ | grep -v…
- **证据**：/Users/ll/code/yanshi/internal/tools/github.go:15-21 GitHubTools{PRContext, Comment, Approve, Merge} ； /Users/ll/code/yanshi/internal/tools/github.go:42-76 FetchGitHubContext（窄导出解析器，被工具与 cmd/yanshi/pr.go 共用） ； /Users/ll/code/yanshi/internal/tools/github.go:88-108 Comment/Approve/Merge 均用 NewApproval…

#### `B3` V13 — 结构化 code review

- **优先级** P1 ｜ **路线图原状态** 部分（analysis 工具无 review 基线和 findings 契约；无专用 /review 命令） ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：支持三种 base；findings 结构化含 severity/file/line；clean 明确；只读不改
- **实测缺口**：规划怎么说：支持 working tree / base ref / commit 三种 base，findings 含 severity/file/line/suggestion，无问题明确返回 clean，severity 有明确分级定义，交付专用 /review 命令。实际怎么做：review 只接受一个 diff 字符串参数（三种 base 一种没有，需模型手动串 git_diff）；契约用 rule 取代 suggestion；clean 只是空数组无显式标记；severity 无分级判据；/review 命令只是把文本加前缀 dispatchSend 给模型的 prompt 糖；yanshi pr 的 headless review 因未绑定 SubAgentRunner 实测恒返回「requires a bound sub-agent runner」错误串，且该行为被测试固化为预期。分块/去重/排序/artifact/只读 review 角色这几块是真做扎实了。
- **证据**：/Users/ll/code/yanshi/internal/tools/review.go:17-70 streamReview 完整流水线（chunk → 子代理 → decode → dedupe/sort → artifact） ； /Users/ll/code/yanshi/internal/tools/review_chunk.go:11-52 chunkDiff（48 KiB hunk-safe 切分 + 超长行硬切兜底） ； /Users/ll/code/yanshi/internal/tools/review_decode.go:10-16 reviewFinding{Fil…

#### `C1` RLM1 — rlm_query 批量并行 LLM

- **优先级** P2 ｜ **路线图原状态** 缺失（deepseek rlm_query） ｜ **接进运行时** 否 ｜ **有针对性测试** 是
- **验收标准**：1-16 并发；顺序对应；cap 生效；成本显著低于 sub-agent
- **实测缺口**：核心算法与工具层是真实完整实现，验收里的"1-16 并发/顺序对应/cap 生效"在包级与工具级都成立。唯一但致命的缺口是运行时接线：BuildRLM/BuildC1 定义在 internal/bootstrap/c1.go，却从未被 internal/bootstrap/bootstrap.go 的 Build 调用，rlm_query 从不进入 allTools，模型永远看不到这个工具。BuildRLM 只被 c1_test.go 调用。另外默认 orchestrator profile 的 tools.allow 也没有加 rlm_query，config.example.yaml 的 profiles 块同样没有。
- **证据**：/Users/ll/code/yanshi/internal/agent/rlm/runner.go:17 const MaxBatchSize = 16 ； /Users/ll/code/yanshi/internal/agent/rlm/runner.go:37 func (r Runner) Run — 真实 worker pool，jobs chan + sync.WaitGroup，limit 夹到 min(MaxConcurrency,16,len(prompts)) ； /Users/ll/code/yanshi/internal/agent/rlm/runner.go:55 f…

#### `C2` UX1 — 全局命令面板 Ctrl+K

- **优先级** P2 ｜ **路线图原状态** 部分 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：Ctrl+K 打开全局面板;fuzzy 过滤;覆盖命令/模式/模型/会话;Esc 关闭
- **实测缺口**：规划验收要求面板覆盖「命令/模式/模型/会话」四类；实际只做了 command/mode/model/theme 四源，**会话跳转源缺失**（action.go:40 注释标 DEFERRED，理由是 /sessions+/restore picker 已覆盖），设计里提到的 MCP 工具源也未接入（list_mcp 只有 server 名，无可执行 tool registry）。theme 源是规划外新增的。其余（Ctrl+K 打开、fuzzy 过滤、Esc 关闭、模态导航）均真实可用，非占位。
- **证据**：/Users/ll/code/yanshi/internal/cli/tui/action.go:41 collectActions（四源：command/mode/model/theme） ； /Users/ll/code/yanshi/internal/cli/tui/action.go:118 rankedActions（fuzzyScore 排序） ； /Users/ll/code/yanshi/internal/cli/tui/action.go:147 openActionPopup / :155 closeActionPopup / :161 actionConfirm / :1…

#### `C2` UX2 — F1 可搜索帮助

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：F1 打开;可搜索;内容自动生成不漂移
- **实测缺口**：无差异。规划落点写 view.go，实际拆到独立 help.go（更好），三段内容自动生成自 commandTable/guard.Modes()/themeList，键位段因 Go 无法反射只能静态维护并配了哨兵测试。
- **二审改判理由**：代码真实存在且已接进运行时（handlers.go:382 case tea.KeyF1 → handleKeyMsg，由 model.go:495 在 Update 的 tea.KeyMsg 分支调用；三段内容确实动态取自 commandTable/guard.Modes()/themeList），不是空壳，故不判"未实现"。但验收标准"F1 打开"在真实终端上不成立：help.go:102 helpPopup 无任何行数上限（对比 action.go:198 const maxRows = 10、history.go:192 const maxRows = 8），空 query 下渲染 64 行；view.go:182 只用 blockHeight 记账、viewport 被 max(3, ...) 兜底，40 行终端下 View() 共 71 行，而 third_party/bubbletea/standard_renderer.go:186 的 newLines = newLines[len(newLines)-r.height:] 只保留最后 40 行。我实测截断后 "Help" 标题、"Commands:" 段头、"/model" 全部不可见，只剩尾部 "Keys:" 段——面板最核心的 35 条命令在默认打开状态下既看不到也无法滚动到。其次 handlers.go:30-50 的 helpVisible 分…
- **证据**：/Users/ll/code/yanshi/internal/cli/tui/help.go:19 keyBindings 静态键位表（11 条） ； /Users/ll/code/yanshi/internal/cli/tui/help.go:41 collectHelpEntries（commands 来自 commandTable、modes 来自 guard.Modes()、themes 来自 themeList，防漂移） ； /Users/ll/code/yanshi/internal/cli/tui/help.go:57 rankedHelpEntries（fuzzyScore，L…

#### `C2` UX4 — 文件 frecency

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：近期选择靠前;衰减合理;可禁用
- **实测缺口**：核心存储/衰减/持久化/TopN 查询 API 是真实实现且已接进运行时（成功 fs 写入自动记录、经单 worker saveQueue 串行落盘）。但三处与规划有落差：① **TopN 无任何生产消费者**——规划说要「影响 UX3 @path 与 UX1 面板排序」，UX3 已移出、UX1 无文件源，所以「近期选择靠前」这条验收无法在 UI 上验证；② **无「可禁用」配置**（config.example.yaml 与 internal/config 均无 frecency 项）；③ **存储位置/格式与规划不同**——规划写 `~/.yanshi/file-frecency.jsonl`（JSONL），实际是 `os.UserConfigDir()/yanshi/frecency.json`（单个 JSON 数组）；④ 记录信号也不同——规划是「文件补全的近期选择」，实际是「成功的 fs 写工具调用路径」。
- **证据**：/Users/ll/code/yanshi/internal/cli/tui/frecency.go:23 Frecency（带 sync.Mutex）/ :29 frecencyEntry ； /Users/ll/code/yanshi/internal/cli/tui/frecency.go:38 LoadFrecency（损坏 JSON 自愈）/ :58 Record / :79 TopN（含 lastSeen/firstSeen tiebreaker）/ :121 Save（random 后缀 tmp + os.Rename 原子写） ； /Users/ll/code/yanshi/i…

#### `C2` UX8 — 思考流式展示

- **优先级** P2 ｜ **路线图原状态** 部分 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：思考模型可见流式思考;正文与思考分离;非思考模型无影响;可折叠
- **实测缺口**：验收四条全部满足（流式可见、正文与思考分离为不同帧类型与不同 entry、非思考模型不产生 thinking 帧因而无影响、ctrl+o 可折叠展开）。与规划的**表现形式差异**：规划设计说「TUI 在折叠区/**侧栏**实时展示」，实际是 transcript 内联的独立 thinkingEntry 块（流式时显示计时+尾部 10 行，结束后折叠成一行）。plan 文档 §范围决策明确删掉了侧栏/阶段计时/summaryEntry 提议（YAGNI + summaryEntry 违反「思考不进正文」原则），本批只补 parity 回归测试——即该链路在 C2 之前就已完整，C2 只做了结项验证。
- **二审改判理由**：展示链路确属真实实现，非占位：classify.go:361-368 emitAssistantContent 先发 proto.NewThinking 再发 NewAgentChunk，且真正接进运行时（ws.go:798 ClassifyEventsWithUsage 的回调 conn.write(f) 无类型过滤，v1/service.go:319 同）；wsbackend.go:316 Kind: f.Type 直通；TUI model.go:603-625 / events.go:214,256,285 / commands.go:1245 三态渲染 / handlers.go:338 ctrl+o 均存在，我实跑测试 internal/cli/tui 26 个 thinking 用例与 internal/agent/orchestrator 6 个用例全部 PASS，其中 TestClassifyEvents_NoReasoningEmitsNoThinking 证实验收标准3。但一审漏掉了 provider 侧「开启」环节的缺口，导致验收标准1只对三种 provider kind 中的一种成立：(1) internal/llm/eino/anthropic.go:138-150 的 anthropicRequest 结构体没有 thinking / budget_tokens 字段，全仓库 grep bud…
- **证据**：provider 归一化：/Users/ll/code/yanshi/internal/llm/eino/anthropic.go:509（非流式 thinking blocks → ReasoningContent）、:633/:655/:659（流式 ContentBlock.Thinking / Delta.Thinking → ReasoningContent）；openai 侧由 eino-ext acl chat_model.go:1218 choice.Delta.ReasoningContent 归一到同一字段 ； /Users/ll/code/yanshi/internal/…

#### `C3` E03 — skill 从 GitHub 安装 + 管理

- **优先级** P2 ｜ **路线图原状态** 部分（deepseek /skill install github:） ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：可安装/列出/启停/校验；恶意路径安全；重名可诊断；模型可 load 匹配技能
- **实测缺口**：规划验收里的"校验（validate）"与"重名可诊断"两项未实现，是明确缺口而非误判：(1) 无 `/skill validate` 子命令 —— cmdSkill 的 switch（commands_skills.go:39-63）只有 install/uninstall/trust/untrust/enable/disable，校验只在 Install 内联做（frontmatter + validName + symlink + containment），装完后无法主动重验已装技能；(2) 跨 builtin/user/plugin 的重名冲突诊断与 source-prefix 选址未做 —— Loader.Load 仍是 first-seen-wins 静默跳过（skills.go:187-189 `continue // first-seen-wins`），`/skills` 只显示获胜条目；仅在 install/uninstall 时通过 sk.Source != "user" 的 ack 文案间接暴露被 shadow（ws_handlers.go:208-214、249-257）。(3) 无 `/skill update`（MVP 用 uninstall+install）。这三项在 plan v3 的"范围诚实声明"（docs/superpowers/plans/2026-07-21-c3-session…
- **证据**：/Users/ll/code/yanshi/internal/skills/install.go:37 ParseInstallSource（只支持 github:owner/repo[/subdir]，逐段字符集校验 + 拒 "."/".."） ； /Users/ll/code/yanshi/internal/skills/install.go:111 Install（同盘 staging → 全树 rejectSymlinks → subdir containment → frontmatter+validName → 删远端 .trusted/.disabled marker → dst…

#### `C4` COST1 — $ 成本估算

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：/cost 显示 $;聚合正确;价格可配;缓存价区分
- **实测缺口**：三处与规划不符。(1) 验收明确要求 "/stats 显示历史会话 $ 聚合"，但 statsEntry.render(internal/cli/tui/commands.go:1087-1139)只画 token 直方图，代码里从不读 s.CostUSD/s.CostKnown —— $ 数据已经一路从 store 传到 proto.SessionInfo 却在最后一步没被渲染。对应验收测试被刻意禁用：commands_test.go:845 函数名为 testStatsEntryAggregatesKnownCostAndNamesUnknown_disabled（小写 test 前缀 + _disabled 后缀，go test 不会执行），而 plan Task 10 里它叫 TestStatsEntryAggregatesKnownCostAndNamesUnknown。(2) pricing.go:141-157 导出的 FormatCost（含 <$0.0001 / $0.0000 分档语义）在生产代码零调用（只有测试引用），TUI 自己硬编码 fmt.Sprintf("$%.6f")，两套格式不一致。(3) 规划说"内置各 model 的单价表"，实际只有 Anthropic claude-*；config.example.yaml:19 的示例 provider 是 gpt-4o，不在表内，开箱即 co…
- **证据**：/Users/ll/code/yanshi/internal/llm/eino/pricing.go:28-39 DefaultPricing 内置 8 个 claude-* 单价（input/cache_hit/output 三档）；:44-53 MergePricing 覆盖不改基表 ； /Users/ll/code/yanshi/internal/llm/eino/pricing.go:58-81 CostOK/computeCost —— 缓存命中按 CacheHitPerM 单独计价，未知模型返回 (0,false) ； /Users/ll/code/yanshi/internal/…

#### `C4` O07 — doctor 增强

- **优先级** P2 ｜ **路线图原状态** 部分 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：覆盖各子系统;JSON 可机读;失败明确指引
- **实测缺口**：规划验收"覆盖各子系统"，但三个子系统的检查是硬编码占位而非真检。(1) checkSandbox(doctor.go:515-517)无条件返回固定字符串 warn "sandbox verification not implemented yet (arrives with S08 in M2)"，既不读 cfg.Security.Sandbox 也不调 sandbox.New(...).Report()——而 internal/sandbox 包已存在（factory.go:33 New、types.go:50 CapabilityReport）且 bootstrap.go:843-851 已装配并能打印真实 Report，doctor 本可复用却没有。(2) checkMCP(doctor.go:519-528)接了 cfg 参数却完全不读 cfg.MCP.Servers（config.go:25-26 该字段存在），无论配置多少 MCP server 都恒返回 OK + "no mcp servers exposed via chat"；连它自己的测试 TestCheckMCP_ServersListedOrNoneConfigured 也只断言了默认分支，没有任何 "ServersListed" 分支可断言。(3) checkLSP(doctor.go:530-545)只硬编码探测 gopls 一个二进制，忽…
- **证据**：/Users/ll/code/yanshi/internal/cli/doctor.go:140-173 RunDoctor 固定顺序 18 项检查：config/config-version/database/providers/acp/lockfile/port/directories/sandbox/mcp/lsp/permissions/secrets-refs/wal/keyring/locale/keymap/high-contrast ； /Users/ll/code/yanshi/internal/cli/doctor.go:104-111 RenderText（[TAG] n…

#### `C4` OBS1 — slog 结构化日志

- **优先级** P0/P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：关键路径结构化日志;secret 不入日志;级别可配;采样不丢关键错误
- **实测缺口**：两处缺口。(1) 验收里的"采样不丢关键错误"完全没有实现——整个 internal/observe/log 无任何 sample/rate-limit 代码（rg 'sampl' 在该包零命中），只有 slog 的 Level 过滤。(2) 规划落点写"全仓库逐步替换 fmt.Print"，guard/orchestrator/api 三个优先区确实迁移了，但 bootstrap 的诊断输出仍是裸 fmt.Fprintf(os.Stderr)：bootstrap.go:449/452(auto-migrate)、:510(lsp disabled)、:576(vision disabled)、:850(sandbox phase0)、:1036(log file open fail)、:1333(mcp server failed)，以及 api/http/server.go:239(seam failed)。全仓库真正的 slog 发射点只有 10 处。
- **证据**：/Users/ll/code/yanshi/internal/observe/log/log.go:104-138 redactHandler.Handle 真实实现：逐 attr 脱敏 + 从 context 注入 trace_id/session_id/turn_id/tool ； /Users/ll/code/yanshi/internal/observe/log/log.go:141-161 New/Setup（json\|text handler，SetDefault） ； /Users/ll/code/yanshi/internal/observe/log/log.go:73-88…

#### `C4` OBS2 — OTel 遥测

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：trace 链可导出;latency/token/retry/error 可观测;可关闭;脱敏
- **实测缺口**：规划说"session/turn/tool 有 trace id 与 span;记录 latency/token/retry/error"，实际 trace 链在主运行路径断裂：(1) WS 路径 internal/api/http/ws.go 里 grep 不到任何 otelobs 引用，ws.go:506 注释仍写着 "and (later) OTel spans"；plan Task 13 Step 9 明确要求在此处 StartSession(connCtx)+SetSessionID+StartTurn(turnCtx, cs.displayModel())，未实现。(2) SSE 路径 internal/api/http/chat.go 同样零 otel。(3) 导出函数 StartSession / SetSessionID / RecordUsage 三个在整个仓库无任何生产调用点（grep 'otelobs.RecordUsage|otelobs.StartSession|otelobs.SetSessionID' 全仓库 exit=1，仅 observe/otel 内部测试调用），因此 yanshi.llm.tokens counter 永不发射，"token 可观测"验收不成立。(4) ws_compaction.go:89 addProviderUsage(_ context.Context, .…
- **证据**：/Users/ll/code/yanshi/internal/observe/otel/otel.go:140-188 Setup/setupWithFactories 真实 OTLP/HTTP exporter（otlptracehttp + otlpmetrichttp）、ParentBased(TraceIDRatioBased) 采样器、PeriodicReader，失败软降级 installNoop ； /Users/ll/code/yanshi/internal/observe/otel/otel.go:121-133 collectorAvailable TCP 探测 500ms…

#### `C4` OBS3 — feature flags

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：flag 注册/切换;strict mode 报错未知 flag;新功能可灰度
- **实测缺口**：两处缺口。(1) DefaultSpecs 注册的三个 flag 里只有 observe.otel_export 有真实运行时消费点（bootstrap.go:329）；observe.slog_trace_id 与 observe.cost_in_status 在 features.go:82,84 注册后，全仓库非测试代码零消费（rg 'slog_trace_id|cost_in_status' 只命中 features.go 与测试），即 trace id 注入和 status 里的成本显示实际上无视这两个开关，"新功能可灰度"只对 1/3 flag 成立。(2) internal/cli/tui/commands.go:64 与 :65 是完全相同的两行 features 注册（同名同 help 同 handler）；help.go:43 collectHelpEntries 直接遍历 commandTable 无去重，导致 /help 弹窗里 /features 出现两次。此外规划落点写 "CLI /features"，实际做成 TUI slash 命令 + WS 控制帧，无独立 yanshi 子命令（范围差异，但设计合理）。
- **证据**：/Users/ll/code/yanshi/internal/features/features.go:20-51 Stage(stable\|beta\|experimental)/Spec{Key,Stage,Default,Owner}/Row 与规划的 stage/default/owner 一致 ； /Users/ll/code/yanshi/internal/features/features.go:91-102 Register(重复/缺字段 panic)；:108-116 Enabled；:123-131 Set（始终拒未知）；:140-159 ApplyMap（strict …

#### `D1` APS1 — app-server (JSON-RPC)

- **优先级** P3 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：JSON-RPC thread/turn 可用;TS 类型可生成;与 HTTP 行为一致
- **实测缺口**：功能齐全且接线完整，但有一处**规划与实现的实质偏差**：plan 的 Task 11 明确要求「YAML 文件写入使用 temp + rename，失败不覆盖原文件。无 config path 时使用 in-memory backend」，即 config/read|write 应对真实 config.yaml 生效；实际 cmd/yanshi/app.go:46 **无条件**用 appserver.NewMemoryConfig()，即使传了 -config 也一样。config.go:24-25 自己承认「The YAML-backed variant lives at the supervisor layer (Task 11 future work)」。后果：config/read 读不到任何真实配置（实测读 server.http_addr 返回 -32602 "is not set"），config/write 只写进程内存、进程退出即丢失、从不落盘。docs/api/jsonrpc.md 却把这两个方法描述成「读/写运行时配置」，与实现不符。另外 -32603（internal error）在 config backend 为 nil 时才会触发，而生产路径永不为 nil。缺 HTTP↔JSON-RPC 的显式行为一致性对照测试（两侧各自测，靠共用 *v1.Service 保证，无跨 transport…
- **二审改判理由**：核心 JSON-RPC 层是真的，我用自己构建的二进制实测确认（不是靠读代码）：`/tmp/yanshi_rev app --fake-model -config <tmp>` 喂入 12 行请求，得到 initialize/capabilities/thread/start/thread/resume/turn/start/shutdown 全部正常，`turn/start` 后按 sequence 1/2/3 流出 `item/updated`（turn.started → message.delta → turn.completed），错误码 -32700/-32600/-32601/-32602 全部按预期返回；`{"jsonrpc":"2.0","method":"capabilities"}`（无 ID）确实无响应。接线也真实：cmd/yanshi/main.go:118 `case "app": return runApp(argv[2:], stdin, stdout)`，非死代码。所以"空壳/桩"这条攻击不成立。但我找到了三处一审低估或漏掉的实质缺口，合计不足以称"已实现"：(1) **config/read|write 是接了真实接口的空转**。cmd/yanshi/app.go:46 `cfg := appserver.NewMemoryConfig()` 无条件构造，`-config` 参数只喂给…
- **证据**：/Users/ll/code/yanshi/internal/appserver/rpc.go:15-21 标准错误码 -32700/-32600/-32601/-32602/-32603；:26 RPCRequest（ID 为 json.RawMessage 以字节级回显）、:35 RPCResponse、:44 RPCNotification、:53 RPCError；:65 parseRPCLine 校验 jsonrpc=="2.0" 与非空 method；:99 decodeParams 未知字段忽略 ； /Users/ll/code/yanshi/internal/appserver…

#### `D1` V12 — headless exec 增强

- **优先级** P2 ｜ **路线图原状态** 部分 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：stdin/JSONL 可用;退出码稳定;可 resume;CI 可脚本化
- **实测缺口**：核心全部落地且接线完整，一处实现细节与 plan/示例不一致：cmd/yanshi/headless.go:99-105 的 --file 分支把整个文件当成**一条** prompt（`inputs = []cli.HeadlessInput{{Prompt: strings.TrimSpace(string(data))}}`），完全绕过 cfg.Input 模式，从不调用 ReadHeadlessInputs。实测 `exec --input jsonl --file examples/headless-batch/sample.jsonl`（3 行）只跑出 1 个 turn，而同一文件走 stdin 重定向跑出 3 个 turn。这条正是 docs.yml:91 CI smoke 与 examples/headless-batch/README.md 宣称的用法，CI 只判非空所以掩盖了此差异。属于小 bug，不影响 stdin 主路径的验收。
- **二审改判理由**：核心实现是真的、非占位、已接进运行时，这部分我复核确认：/Users/ll/code/yanshi/internal/cli/headless_input.go:37 ReadHeadlessInputs 是真实的 bufio.Scanner+json.Unmarshal 解析（1MiB 上限，jsonl 逐行报错带行号）；/Users/ll/code/yanshi/internal/cli/headless.go:46 runHeadlessWithBackend 的 `if i == 0 && resume == ""`(:64) + `resume = ""`(:82) 确实实现了"只 resume 一次"；dispatch 接线真实存在于 /Users/ll/code/yanshi/cmd/yanshi/main.go:117（exec）与 :534（chat --no-tui → runHeadlessCommand(filtered,"chat",os.Stdin)）。我实测通过 stdin 的 4 条验收标准全部成立：`exec --input jsonl --output jsonl < sample.jsonl` 输出 3 个 turn、同一 sessionId、turns 1/2/3；退出码 0/1/2/124 实测吻合；`--resume <id>` 与 JSONL 首行 `"resume"` 字段…
- **证据**：/Users/ll/code/yanshi/internal/cli/headless_input.go:37 ReadHeadlessInputs — text/lines/jsonl 三种输入模式的真实解析（bufio.Scanner + json.Unmarshal，1MiB 行上限），非占位 ； /Users/ll/code/yanshi/internal/cli/headless.go:28 RunHeadless / :46 runHeadlessWithBackend — 多 prompt 单 backend 循环，resume 只在第 0 条生效（:64 `if i == 0 …

#### `D2` V15 — TS / Python SDK

- **优先级** P3 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：start/resume/run/stream/cancel 可用；类型生成；契约测试
- **实测缺口**：三处与规划有出入但不影响判定为已实现：(1) 规划说“类型由 V14 协议生成”，TS 侧确实由 `cmd/api-schema` 生成（sdk/ts/v1.ts），但 Python 侧不是生成的——sdk/python/src/yanshi_sdk/generated.py:1-16 自述“Hand-mirrored Pydantic v2 models…D2 maintains the Python mirror by hand”，且 pyproject.toml 的 [generate] extra 里 datamodel-code-generator 从未被任何脚本调用，即 Python 类型是手写镜像、无生成器保障，Go 结构体变更时无自动漂移检测（rg 全仓无任何 *_test.go 交叉校验 sdk/python/generated.py 或 sdk/schema 与 internal/api/v1 的一致性）。(2) 规划说“stream/cancel 可用”，cancel 已实现且服务端有对应路由；stream 的 WS 分支是前瞻性的：D1 未提供 /api/v1/threads/{id}/stream（实测 curl 该路径返回 404 page not found），Python 侧 client.py:113-118 在 transport="ws" 时直接 `raise ProtocolErr…
- **二审改判理由**：运行时主流程为真（我自建 /tmp/yanshi_v15 起 127.0.0.1:18211，Python SDK 实跑 start→run(turn.started/message.delta/turn.completed 三项)→resume→cancel(ok=True)，TS SDK 同样跑通并触发 onStarted），故不是空壳。但验收标准三项中有两项不成立，一审判「已实现」高估： (1) **「类型生成」是伪生成器 —— 这是最硬的证伪。** cmd/api-schema/main.go:52 `text := \`// Code generated by cmd/api-schema; DO NOT EDIT. ...\`` 到 :169 是一整段**硬编码的 Go 原始字符串字面量**，逐字符抄写了 TS 接口；它从不解析 internal/api/v1 的任何东西。唯一与真实契约的接触点是 main.go:176 `_ = v1.SchemaBytes()` —— 返回值被**直接丢弃**（注释自称「smoke check … guards the generator against silent drift」，但 `_ =` 赋值在语义上不可能检测任何漂移）。一审说「实测 go run ./cmd/api-schema 输出与 sdk/ts/v1.ts 逐字节一致 → 生成器真实驱动，无漂移」是循环…
- **证据**：/Users/ll/code/yanshi/sdk/ts/src/client.ts:91 `export class AgentClient`，方法 start(:104) / resume(:113) / interrupt(:124) / cancel(:136) / run(:149 async generator) / startTurnMetadata(:278) ； /Users/ll/code/yanshi/sdk/ts/src/transport.ts:114 requestJson、:192 readSse、:267 readWebSocket、:153 parseItem…

#### `D3` C15 — keymap 配置（重映射 / Vim 开关 / 高对比 / 冲突诊断）

- **优先级** P3 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 否 ｜ **有针对性测试** 是
- **验收标准**：核心按键可重映射；Vim 开关；高对比主题；冲突可诊断
- **实测缺口**：规划落点是 `internal/cli/tui/`（keymap 加载）+ 配置文件；实际做成了一个**独立叶子包 internal/keymap**，语义 Action / 冲突诊断 / Vim 状态机都是真实完整实现，但**没有任何一条路径把它接进 TUI**：TUI 按键仍走 handlers.go 的硬编码 tea.Key* switch，internal/keymap 唯一生产消费者是 doctor 的静态配置校验。逐条验收：核心按键可重映射=否（配置 tui.bindings 只被 doctor 校验，运行时不生效）；Vim 开关=否（VimMachine 无消费者，无 /vim 命令，cfg.TUI.Vim 无运行时读取）；高对比主题=是（但这是 C15 之前既有的 /theme + ThemeHighContrast，非本批新增，且 cfg.TUI.high_contrast 不影响启动主题）；冲突可诊断=部分（`yanshi doctor` 能报 keymap 检查，但 plan/文档承诺的 `/keymap diagnostics` 与 `/keymap reset` 从未注册，KeymapReset tombstone 字段定义在 preferences.go:27 却无写入者）。另：plan 自述“C15 正式豁免 OBS3 前置门控并精确同步 roadmap”，但 docs/archive/fea…
- **证据**：/Users/ll/code/yanshi/internal/keymap/keymap.go:21-298 — Action 语义常量（send/newline/cancel/scroll_up/scroll_down/clear/help/quit/command_mode）、Builder 收集后统一 Build 校验、NewDefaultBuilder(overrides) 内建 8 条默认绑定 + 用户覆盖、Diagnostic 四类（conflict / normalized_duplicate / unknown_action / invalid_key）确定性排序、Map.Lo…

#### `D3` I18N1 — locale / i18n（en / zh-Hans）

- **优先级** P3 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 否 ｜ **有针对性测试** 是
- **验收标准**：至少 en/zh-Hans 切换；UI 与输出语言独立；自动检测
- **实测缺口**：规划：TUI 文案外提 + `/config locale` 切换 + 自动检测；实际：i18n 核心库、catalog、自动检测、output_language 独立、doctor 校验都是真实完整实现，但**用户无法在运行时切到 zh-Hans**。具体缺口：(a) TUI 固定用英文 bundle（state.go:120 硬编码 "en"），cfg.I18N.UILocale 与 YANSHI_UI_LOCALE 环境变量都进不了 TUI；(b) 没有 /locale（或 /config locale）slash 命令，从未注册；(c) 四层 preferences 合并（mergeTUIPrefs）与持久化写好了却零生产调用点，是孤立代码；(d) 65 个 catalog key 只有 22 个（全是 /help 行 + placeholder）真被使用，其余 43 个是为尚未实现的命令预留的死条目。plan 文档 Task 11 明确要求“TUI 接线 + output_language”，output_language 那一半做了，TUI locale 接线这一半没做。
- **证据**：/Users/ll/code/yanshi/internal/i18n/i18n.go:96-228 — Bundle（persistent/effective 分离）、NewBundle 每次调用重算 auto（L112-113）、detectLocale 真 LC_ALL > LANG（L166-176）、normalizeLocale 处理 @modifier/.codeset/C/POSIX/zh-CN/zh-SG→zh-Hans、zh-TW/zh-HK/zh-Hant→en（L183-207）、Get/GetF 占位符替换、缺 key 回退 key 本身 ； /Users/ll/co…

#### `D3` S10 — secrets / keyring（OS keyring + 统一脱敏）

- **优先级** P3 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：secret 不入日志/DB 明文；keyring 读写删；无 keyring 安全降级
- **实测缺口**：实现范围大于规划。两处小偏差：(1) 规划落点只写 internal/secrets，实际还改动了 store/http/config/bootstrap 五个包做边界脱敏——这是加分不是减分；(2) bootstrap 仍有 15 处 fmt.Fprintf(os.Stderr,...) 未走 SafeLogger（bootstrap.go:294/449/452/510/576/850/1036/1333 等），其中 L449/L452 的 auto-migrate 警告路径正好在处理 API key 上下文，虽然只打印 cfgPath 和 err，但没有经过 redactor，与 plan 里“新 D3 代码不得直接 Fprintf os.Stderr”的自述有出入。(3) 本机 `go test ./internal/secrets` 有 2 个 keyring 真实往返测试失败（TestKeyring_RoundTripWhenAvailable / TestKeyring_AvailableProbeHit，macOS Security exit status 36），`-tags nokeyring` 下全绿——属环境依赖测试未做 skip 门禁，是测试健壮性缺口。
- **二审改判理由**：核心非占位、非空壳，我用编译出的真实二进制端到端验证过 file backend 全链路（auth set 写入 secrets.enc 后 strings 查不到明文；auth status 解密回读成功；auth logout 删除成功、二次 logout 返回 ErrSecretNotFound 退出 1；-tags nokeyring 构建打印 warn 后继续不 fatal）。但验收标准三条子条件中有两条存在可复现缺陷，故降级为部分实现。(1) 【脱敏注册链断裂】secrets.go:196 MergeRedactors 内 `merged := NewRedactor()` 是快照拷贝而非别名；bootstrap.go:376 把 redactor 重绑到该拷贝，bootstrap.go:465 再在拷贝上 Register(resolved)。我写探针测试证实 output.Redactor（即 SafeOutput.Logger 背后的注册表）自始至终看不到任何 provider key，直接违反 secrets.go:213-216 自述的不变量“registering a secret on Redactor makes SafeLogger redact it”。反向同理：auth.go:221/224/414/416 通过 m.secrets.Redactor() 注册的 device token…
- **证据**：/Users/ll/code/yanshi/internal/secrets/secrets.go:29-234 — ParseCredentialRef（secret:// / env:// / legacy，raw literal fail-closed 返回 ErrRawLiteralRefused）、Redactor.Register/Redact/RedactJSON、SafeError（Unwrap 保留 cause）、SafeLogger、MergeRedactors、SafeOutput，全部为真实实现 ； /Users/ll/code/yanshi/internal/secr…

#### `E1` COV2 — proto 覆盖 67% → 80%+

- **优先级** P0 ｜ **路线图原状态** 部分（路线图第 192 行标注 "P0 \| 部分"） ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：覆盖率 ≥80%；全帧往返；SSE golden 稳定；WS/SSE 词表对称
- **实测缺口**：两个真实弱点：(1) golden 文件第 82-83 行是退化条目 —— goldenFrames() 里的 NewTaskUpdate(nil) 因 frame.go:726 的 nil 短路返回零值 ServerFrame{}，导致 golden 记的是 `event: `（空）/ `data: {"type":""}`，task_update 帧的真实线形态并未被冻结；(2) 验收里的 "WS/SSE 词表对称" 被实现为自指断言（TestVocabulary_Symmetry 只在 goldenFrames 内部断言 event==Type），并没有交叉比对 internal/api/http/ws.go 与 internal/cli/ssebackend.go 实际处理的帧集合，也没有断言 ServerFrame 的全部 31 个 Type 都进了 golden（实测 31 个 ServerFrame Type 中 task_update 因上述退化未真正入 golden）。
- **二审改判理由**：覆盖率与 golden 两项我自己复现确认为真，但四条验收里的「WS/SSE 词表对称」是一条**永远不可能失败的重言式断言**，不构成任何守卫，因此判为部分实现。 【真实的部分（我自己验证）】 1. 覆盖率：`go test -cover ./internal/proto/` → 100.0%，且 `go tool cover -func=/tmp/proto.out` 逐函数全为 100.0%（无一条低于 100%），远超 ≥80% 目标。39 个顶层测试全部 PASS（`go test -v -count=1` 实测）。 2. golden 真实且已入库：`git ls-files internal/proto/testdata/sse_golden.txt` 有输出，105 行 / 35 条 `event:` 记录。 3. golden 稳定性：我跑 `-update -count=1` 后 `git diff --quiet -- internal/proto/testdata/` 返回 IDENTICAL，重建幂等。 4. golden **确实有守卫力**（一审没做的变异测试我做了）：把 /Users/ll/code/yanshi/internal/proto/frame.go:473 `NewAgentChunk` 改成 `Text: text + "X"` 后 `TestSSEEvent_Golden`…
- **证据**：实测覆盖率：go test -cover ./internal/proto/ → coverage: 100.0% of statements（目标 ≥80%） ； /Users/ll/code/yanshi/internal/proto/frame_test.go:17 var updateGolden = flag.Bool("update", false, "regenerate SSE golden file") ； /Users/ll/code/yanshi/internal/proto/frame_test.go:416 goldenFrames()（35 个代表帧）、:461 T…

#### `E1` COV3 — bootstrap 集成测试 23% → 50%+

- **优先级** P0 ｜ **路线图原状态** 缺失（路线图第 202 行标注 "P0 \| 缺失"） ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：覆盖率 ≥50%；最小 App 可构建并跑一轮 turn；软降级被验证
- **实测缺口**：两个测试语义偏弱（存在但断言力度低于名字暗示）：(1) TestBuild_AssemblyOrder（:87）名为"装配顺序"，实际只断言 app.Store/app.Orch 非 nil + Orch profile 的 Tools.Allow 不含 "*" 且非空，并没有验证 config→store→vcs→model→tools→orchestrator→http→task 这个顺序本身（顺序不可从最终 App 快照反推，属设计固有限制）；(2) TestBuild_VCSSoftDegrade（:101）并未真正制造 VCS 失败，只是复用 buildMinimalApp 后断言 app.VCS 非 nil，与 TestBuild_MinimalApp 的同名断言重复，实质是一条冗余用例——三条软降级里只有插件（:110）与 MCP（:135）真的注入了失败配置。
- **二审改判理由**：一审只核对了「测试存在 + PASS + 总覆盖率」，没有验证这些测试到底走没走目标分支。我用 coverage profile 逐分支比对后发现，验收标准第 3 条「软降级被验证」三条路径里只有 1 条是真的，另外 2 条是空壳： (1) TestBuild_VCSSoftDegrade（bootstrap_test.go:101-105）完全没有注入 VCS 失败。它调 buildMinimalApp（用真实临时 sqlite + 包 cwd 作 workRoot），只断言 app.VCS != nil。而 internal/bootstrap/bootstrap.go:486 的 `vcsRepoID, vcsErr := vcsInstance.InitRepo(workRoot)` 在同样配置下是**成功**的——同文件 TestBuild_VCSWired:586 对同一份 config 断言 `assert.NotEmpty(t, app.VCSRepoID)`。也就是说该测试的 doc 注释「memory store + empty workroot has nothing to scan」是事实错误的描述。硬证据：`go test -coverprofile` 全包 126 个测试跑完，`bootstrap.go:487.19,489.3 1 0` —— `if vcsErr != nil` 分支执行次…
- **证据**：实测覆盖率：go test -cover ./internal/bootstrap/ → coverage: 94.2% of statements（目标 ≥50%，接近翻倍）；逐函数 Build 92.9%、Shutdown 81.0% ； /Users/ll/code/yanshi/internal/bootstrap/bootstrap_test.go:40 buildMinimalApp(t) 共用 fixture（临时 config.yaml + 127.0.0.1:0 + 临时 sqlite + FakeModel:true + t.Cleanup Shutdown） ； /Use…

#### `E2` PROP1 — ctxcompact 属性测试

- **优先级** P1 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：≥3 个属性；随机输入通过；工具对配对不变量成立
- **实测缺口**：核心 5 条属性都在且通过，但有三处与规划的实质偏差。①【P2 在 ~22% 的随机历史上被 t.Skip 掉】plan_property_test.go:112-114 引入了 plan 里没有的 `pinnedSetIsConsistent` 前置守卫，当历史以 summary sentinel 结尾（Plan 走 plan.go:27 短路、pin 全部索引含 orphan）时直接 `t.Skip`。实测 P2 三个测试 50/30/30 轮里分别 skip 11/8/8 次（合计 27 次 skip）。规划验收写「P2 在含 orphan 的随机历史上成立」——现状是「含 orphan 且历史以 summary 结尾时不检查」，这条不变量在最容易出问题的分支上被绕开了，而不是被证明成立。②【P4 语义反转】plan 原文（summarize_property_test.go 前身 Task 4）要求 `TestProperty_RunReturnsErrorForEmptySummary`：summarizer 返回空串时 `Run` 必须报错（bug⑥）。实际实现改名为 `TestProperty_NoEmptySummaryMessage`（:80），断言方向完全相反：`if err != nil { t.Fatalf("Run must not error with empty summarizer out…
- **证据**：/Users/ll/code/yanshi/internal/ctxcompact/gen_test.go:13-74 — `genHistory` 随机历史生成器，真实实现：混合 user/assistant-with-toolcalls/tool-result/orphan-call/orphan-result/working-set 路径/error 标记/diff 标记，15% 概率追加 SummarySentinel 尾消息 ； /Users/ll/code/yanshi/internal/ctxcompact/gen_test.go:94-122 — `TestGenHistory…

#### `F1` WAL1 — WAL 模式 + 连接池 + busy_timeout

- **优先级** P1 ｜ **路线图原状态** 缺失（路线图 286-296 行原文：P1 \| 缺失 \| synthesis R10/A15） ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：WAL 启用；并发写不报 locked；性能不退化；旧 DB 平滑升级；WAL 文件有界（roadmap:295）。plan 另有 10 条细化验收：每条池连接 PRAGMA 生效、MaxOpenConns 按配置且 :memory: 强制 1、16×50 零 BUSY、读不阻塞写、双 Open 跨进程 busy_timeout、rollback→WAL 幂等零丢失、Close 执行 wal_checkpoint(TRUNCATE)、work/vcs/auth/bootstrap 现有测试全绿、Windows -r…
- **实测缺口**：核心机制真实落地且已接进 bootstrap/config/doctor 运行时，但相对 plan 有 6 处明确缺口： (1) 【最重】work 包写路径完全没走 WriteTxer。规划（plan T5 + 职责边界章）说：work.Store 所有写方法经注入的 writeTx.WriteTx，与 store/auth/vcs 共用同一 writeMu。实际做的：WriteTxer 接口、unlockedWriteTxer 回退、wt() 访问器、FromDB(db, writeTx) 双参签名、bootstrap.go:722 的注入全部写齐，包头注释甚至声称「All write paths route through the injected WriteTxer」，但 11 个写方法一个都没调用 wt().WriteTx，仍全部 s.db.BeginTx / s.db.ExecContext（store.go:138,312,348,385,408,455,367,434,448,477,533）。唯一「用到」WriteTxer 的是 manager_extra_test.go:607-640 的覆盖率测试，其注释自承「even though the current Store methods call s.db.BeginTx inline」。后果：MaxOpenConns=4 下 work 的并发写落到不同…
- **证据**：/Users/ll/code/yanshi/internal/store/store.go:82-93 OpenOptions{MaxOpenConns,BusyTimeoutMs,WALAutoCheckpoint} + DefaultOptions{4,5000,1000}（真实实现，非占位） ； /Users/ll/code/yanshi/internal/store/store.go:96-134 OpenWith：buildDSN → sqlOpener → :memory: 强制 maxOpen=1 → SetMaxOpenConns → SetConnMaxIdleTime(5m…

#### `F2` BENCH1 — 性能基准基线

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：关键路径有基准；CI 记录趋势；大回归可发现
- **实测缺口**：实现比规划更完整（plan 说"F2 只产 bench 文件，CI 接线归 CIG1"，但 nightly.yml 的 bench job 已经在）。两处细节偏差，均不影响验收：(1) nightly.yml bench job 仍写着 continue-on-error 注释 "soft until F2 bench targets land" —— targets 已 land，注释过期；(2) nightly job 直接 `go test -bench=. ./...` 而不是调用 scripts/bench.sh，所以 CI 侧并没有跑 benchstat 比对，只上传原始 bench-results.txt artifact，"benchstat 记录趋势"这一条在 CI 里是靠 artifact 人工比对，脚本只在本地可用。规划验收里的"人为制造回归能被 benchstat 标出 >N%"没有自动化。
- **二审改判理由**：一审核心证据是"实测全部 PASS"，但它用的是 `-benchtime=1x`，而这个 flag 恰好掩盖了唯一的真 bug。我自己复跑发现：/Users/ll/code/yanshi/internal/agent/orchestrator/orchestrator_bench_test.go:13 的 BenchmarkOrchestratorTurn 在任何 b.N>=2 时必然失败——`-benchtime=1x` 通过、`-benchtime=2x` 就 `--- FAIL: orchestrator_bench_test.go:23: orchestrator: no assistant message produced`，默认 benchtime（CI 实际用的）和 `-count=5`（bench.sh 实际用的）均 5/5 全 FAIL，整包 `go test -bench=. ./...` 返回 rc=1。根因确定：bench 第 14 行 `einollm.NewFakeModel([]string{"hello from agent"}, nil)` 只脚本了一条响应且未设 Repeat，/Users/ll/code/yanshi/internal/llm/eino/fake.go:159-160 在 responses 耗尽后返回 `schema.AssistantMessage("", nil…
- **证据**：/Users/ll/code/yanshi/internal/vcs/vcs_bench_test.go — BenchmarkVCSCommit（SmallTree n=10 / LargeTree n=1000，每轮 StopTimer 下新增一文件避免 no-op commit 短路）+ BenchmarkDAGApply（AddFile / ModifyFile，每轮 StopTimer 下重建 base/ours 状态，因为 MergeToMain 会改 main head） ； /Users/ll/code/yanshi/internal/tools/fs_bench_test.g…

#### `F2` LEAK2 — 子代理并发上限

- **优先级** P1 ｜ **路线图原状态** 缺失（spec 阶段自我降级为"B1/M04b 已完成，本批只补注释"） ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：并发上限生效；满则拒绝；计数准确；与深度上限交互文档化
- **实测缺口**：两处小缺口，均属规划自己列为"可选"的项：(1) plan Task 3 Step 3 的 TestSpawnDepthBeforeConcurrency（深度优先于并发的判定顺序断言）在 manager_spawn_test.go 中不存在（grep 无命中），plan 本身标注"可选/否则注释即足"；(2) config.example.yaml 完全没有 subagents: 段（grep 'subagent' 该文件零命中，顶层 key 只有 schema_version/server/storage/token/llm/agents/skills/vcs/compaction/profiles/security/batch/lsp/mcp/observability/features/pricing/secrets/auth/tui），限制只在 docs/user-guide/configuration.md:51,209 有记录 —— 用户不看文档就发现不了 subagents.limit 可调。核心并发上限功能本身完全落地。
- **二审改判理由**：一审引用的代码确实存在（行号基本准确），但它只验证了"满则拒绝"，漏掉了验收标准里最关键的两条：**计数准确**和**上限对主路径生效**。我用 -overlay 注入测试（未改仓库任何文件）实测到两个硬缺陷。 **缺陷 1（承重 bug）：计数不准确，槽位永不释放。** internal/agent/registry/manager.go:570-587 的 runningLocked() 取 `len(runtime 非 nil 项)` 与 `StatusRunning records` 的**最大值**。但 runtime map 在 agent 终止时**从不删除**：搜遍全文件，`delete(m.runtime, ...)` 只出现在 3 处 —— :202/:210（Spawn 持久化失败回滚）和 :486（restoreRecord/Resume 回滚），全是错误回滚路径。正常终止路径 runAgentLoop(:625-680) → finishTerminal(:721-752) 只改 records[id].Status，**从不 delete(m.runtime, id)**。于是 runtime 长度单调递增，runningLocked() 因取 max 永远返回"历史累计 spawn 数"而非"当前在跑数"。 实测（MaxConcurrent=2，两个 agent 跑完并 Wait 到 ter…
- **证据**：/Users/ll/code/yanshi/internal/agent/registry/manager.go:20 MaxConcurrent 字段；:66-72 clamp（0→10，<1→1，>20→20） ； /Users/ll/code/yanshi/internal/agent/registry/manager.go:147-158 — 并发上限门：runningLocked() >= m.limit → return &SpawnErrCap{Cap: m.limit}，且带有规划要求的"concurrency cap gate (second dimension)"承重注释 …

#### `F2` LEAK3 — ACPImplementer usage 回流

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：ACP turn usage 进 sink；budget 含子进程；解析失败安全降级
- **实测缺口**：实现完整。两点观察：(1) implementer_test.go:254 的 goalloop 侧测试是**重建了一份等价闭包**而非直接调 w.usageForwarder()（注释自陈"Re-constructed here so the test is independent of the spawn path"），所以 usageForwarder 本身的 nil-sink 分支和真实 worker 接线没有直接断言 —— 若有人改了 usageForwarder 的映射方向，这个测试不会红；grep 显示 usageForwarder 在测试文件里零引用。(2) goal CLI 只设 Budget{MaxIterations}，从不设 MaxTokens（cmd/yanshi/main.go:723,774；全仓 grep MaxTokens 在 cmd/ 下零命中），而 loop.go:53-55 的 overBudget 要求 MaxTokens>0 才生效 → 子进程 usage 确实进了 sink 并被 Snapshot 持久化（persistGoalRun），但"budget 含子进程"在当前 CLI 下没有实际硬停效果，因为 token budget 根本没开。这与 plan §7 决策记录的 Option A（硬停，用户可设 MaxTokens=0 关闭）方向一致但缺了开启入口。
- **二审改判理由**：管道是真的，但数据源是虚构的 —— 对任何真实 ACP agent，进 sink 的 token 恒为 0，与 F2 之前无差别。 1) **协议 discriminator 不存在。** `internal/acp/client.go:250` 的 `case "usage_report":` 匹配的字符串在 ACP 协议中根本不存在。本机 ACP 官方 SDK 两份独立来源均证实唯一的用量变体叫 `usage_update`：Rust `agent-client-protocol-schema-0.11.2/src/client.rs:104 UsageUpdate(UsageUpdate)`（枚举 `#[serde(tag="sessionUpdate", rename_all="snake_case")]`，client.rs:71）、TS `@agentclientprotocol/sdk@0.21.1 dist/schema/types.gen.d.ts:4352 sessionUpdate: "usage_update"`。全量 grep `usage_report` 在两份 SDK 中零命中。因此 `handleNotify` 的 `case "usage_report"` 是死分支，真实 agent 发的 `usage_update` 落到 `default:`（client.go:275-281）被丢…
- **证据**：/Users/ll/code/yanshi/internal/acp/types.go:181-188 — Usage 结构体（InputTokens/OutputTokens/TotalTokens，omitempty） ； /Users/ll/code/yanshi/internal/acp/client.go:22-24 — Event.Usage *Usage 字段 ； /Users/ll/code/yanshi/internal/acp/client.go:200-231 parseUsageReport — defer recover 防 panic，先试 {update:{usa…

#### `G` VISION — 能力声明 + 辅助模型 + turn 分流

- **优先级** P1 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 否 ｜ **有针对性测试** 是
- **验收标准**：主多模态：图直接通过消息内容到达；主非多模态+有辅助：占位+image_describe 走通；无辅助：error 而非静默
- **实测缺口**：config 声明、辅助模型自动选、multimodalMap、启动 warning、两路分流算法全部真实且已装配，但**分流函数从未在运行时被调用**——ApplyImages 只有测试调用者。WS 的 runUserTurn（ws.go:461）与 v1 Service.runTurn（api/v1/service.go:299）都只把 cf.Text/p.Input 追加成纯文本 user 消息，从不读 Images 字段，也从不调 o.ApplyImages。TurnOpts 也没有承载图像的字段。结果：即使客户端按协议发了 images，服务端整条 turn 路径会静默丢弃，主多模态模型永远拿不到 image part，非多模态模型永远拿不到占位符与 store 条目。另有一处次要偏差：bootstrap.go:556-565 的 multimodalMap key 用 p.Model→p.Name→model-N 回退推导，而 ApplyImages 的 modelID 入参来源未定义（因为没有调用点），键对齐关系无法验证。
- **证据**：/Users/ll/code/yanshi/internal/config/config.go:375-379 — ProviderConfig.Multimodal bool `yaml:"multimodal"` 真实字段，带 Tier G doc 注释 ； /Users/ll/code/yanshi/config.example.yaml:22 与 :28 — multimodal 注释样例 + claude provider 实配 multimodal: true ； /Users/ll/code/yanshi/internal/config/config_test.go:693 Te…

#### `G` VISION-TOOL — image_describe + image store + 五入口

- **优先级** P1 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 否 ｜ **有针对性测试** 是
- **验收标准**：五入口各自可产生图像附件；image_describe/id-ref+path-ref 走通；超限/越权被拒；费用纳入 /cost
- **实测缺口**：工具与基础设施层是真实且高质量的（image_describe、screenshot、imagestore、clipimg 全部非占位，且 image_describe/screenshot 已在 bootstrap.go:758-762 注册进工具表），但**五入口只有 2 个真正打通**： - 入口 C（fs_read/web_fetch 遇图）：已接线（fs.go:298、web.go:145），但只输出提示文本，不产生图像附件，也不入 store —— 与验收「五入口各自可产生图像附件」不符。 - 入口 D（截图工具）：完整打通（approval-required + 入 store + 返回 placeholder）。 - 入口 A（剪贴板 Ctrl+V）：规划说 TUI 绑 Ctrl+V 调 clipimg.Read；实际 clipimg 包**零导入方**，TUI 没有任何 Ctrl+V 绑定，是彻底的孤立包。 - 入口 B（@path）：规划说 TUI 检测 @path 图像并注入附件；实际只写了 tools.IsImagePath 一个判定函数，TUI 无 @path 图像检测逻辑，无 pending images 状态。 - 入口 E（headless/SDK/IDE 协议传图）：规划说协议加 image 字段 additive 并走通；实际字段（proto.ClientFrame.Images、v1…
- **证据**：/Users/ll/code/yanshi/internal/tools/vision.go:45-56 NewImageDescribeTool — 真实 GuardedTool，参数 image_ref(required)/question(optional)，60s 超时 ； /Users/ll/code/yanshi/internal/tools/vision.go:58-84 run — 无辅助时返回明确中文配置错误 result（:63-65），默认问题 defaultVisionQuestion（:42） ； /Users/ll/code/yanshi/internal/tool…

#### `H1` PKG1 — 多平台打包分发

- **优先级** P1 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 否
- **验收标准**：多平台二进制可构建；release 产物完整；checksum；fork 行为保留
- **实测缺口**：规划怎么说：“多平台二进制可构建；release 产物完整；checksum；fork 行为保留”，并在 plan Step 9.4 明确要求本地跑 `goreleaser check` 通过后才算 GREEN。实际怎么做：配置文件写全了、四目标/checksum/tag 触发/git-cliff 接线都在，但 `.goreleaser.yaml:58` 用的是 goreleaser **v1 时代已删除**的 `changelog.skip`，而 goreleaser v2 是 `KnownFields(true)` 严格解码 —— 该配置会在 `goreleaser check` / `goreleaser release` 阶段直接解析失败，导致 release.yml 一旦被 `v*` tag 触发就会**在 goreleaser 步骤挂掉，发不出任何产物**。正确写法是 `changelog:\n disable: true`。这是从 plan 文档 Step 9.2 的代码块一字不差抄进仓库的（plan :1370 同样写 `skip: true`），说明 Step 9.4 的 `goreleaser check` 验证从未真正执行过。次要问题：`format_overrides.formats`（复数）只在 goreleaser v2.7.0+ 存在。另外 CI 里没有任何 `goreleaser chec…
- **证据**：/Users/ll/code/yanshi/.goreleaser.yaml:5-33 — version 2，builds 四目标：goos [linux,windows,darwin] × goarch [amd64,arm64]，用 ignore 排掉 windows/arm64 与 darwin/amd64 → 恰好 windows/amd64、linux/amd64、linux/arm64、darwin/arm64 ； /Users/ll/code/yanshi/.goreleaser.yaml:17-25 — `CGO_ENABLED=0`、`-tags=nokeyring`、`-…

#### `H1` VER1 — 语义化版本 + CHANGELOG 自动化

- **优先级** P1 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：版本号来自 git tag；CHANGELOG 可生成；发布流程文档化
- **实测缺口**：无差异。const→var 的硬内部依赖（plan 文档 :30 标注的 VER1→PKG1 前置）已落实并实测生效。唯一的现实状态说明：仓库当前无任何 `v*` semver tag（`git tag -l 'v[0-9]*'` 为空，只有 m1..m9 里程碑 tag），所以“版本号来自 git tag”这条目前只在机制层面成立，尚未有真实 tag 触发过一次；build.sh 的无 tag 回落分支正是为此设计。git-cliff 是 CI-only 外部二进制（本地 `which git-cliff` 未安装），符合设计而非缺陷。
- **二审改判理由**：核心机制真实非占位，但一审漏掉三处实质缺陷，且证据本身有事实错误，故降级为"部分实现"。 【确认为真的部分】internal/version/version.go:21 `var Version = "0.4.0"` 确为 var；Parse(:83)/parseNum(:127)/String(:147) 是真实实现（拆 build → prerelease → major.minor.patch，拒前导零）。我实测 `go build -ldflags "-X github.com/x6nux/yanshi/internal/version.Version=9.9.9" ./cmd/yanshi` → `--version` 输出 `yanshi 9.9.9`，注入闭环成立。docs/upgrade-guide.md:47-92「Release runbook」四步（doctor --release / git tag v1.0.0 / release.yml / 校验产物）内容详实，`yanshi doctor --release` 也真实存在（cmd/yanshi/main.go:978 `fs.Bool("release", ...)`，internal/cli/doctor.go:453 实现，doctor_release_test.go 通过）。故验收标准第 3 条「发布流程文档化」满足。 【缺陷一：ver…
- **证据**：/Users/ll/code/yanshi/internal/version/version.go:21 — `var Version = "0.4.0"`（已从 const 改为 var，注释明确说明 -X 只能 patch string var） ； /Users/ll/code/yanshi/internal/version/version.go:83 — `func Parse(v string) (Semver, error)`，真实实现：拆 build metadata(+) → prerelease(-) → major.minor.patch，含 parseNum 拒绝前导零/…

#### `H2` APIREF1 — v1 API/协议参考

- **优先级** P2 ｜ **路线图原状态** 部分 \| D1/D2 产出 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：v1 API 有参考；SDK 用法有示例；与 schema 一致
- **实测缺口**：无差异。规划的"从 sdk/schema 与 internal/api/v1/types.go 生成片段"完全按设计落地，且 COORD 的 G image 字段依赖已闭环（images 字段已在生成表内）。唯一可议之处：events.md 的 item.type 枚举表按 plan Task 8 Step 3 的设计是**手写** prose（不走生成区块），一致性只靠 CI 的 grep 弱断言而非生成守门——这是规划本身就锁定的取舍，非实现偏离。
- **二审改判理由**：文档骨架和生成器都是真的（我复现了 `go run ./cmd/api-schema -markdown docs/api/{schema,resources}.md` + `git diff --exit-code docs/api/` 无 diff；`cmd/api-schema/markdown.go:296-335 runMarkdown` 是真实幂等实现），HTTP 路由也确实在运行时（`internal/api/http/agent_v1.go:29/42/61/81` 注册，`internal/bootstrap/bootstrap.go:945 srv.AgentV1(agentAPI)` 接线；我起 `yanshi serve --fake-model -addr 127.0.0.1:8099` 实测 POST /api/v1/thread/start 返回 `{"version":"v1","thread":{...}}`，turn/start 的 SSE 依次吐出 turn.started / message.delta / turn.completed，响应头带 `X-Yanshi-Api-Version: v1`）。但验收标准「SDK 用法有示例」与「与 schema 一致」有实打实的缺口，一审的「各给可跑最小端到端」是错的： （1）**Python 文档示例根本跑不起来**。`docs/api…
- **证据**：/Users/ll/code/yanshi/docs/api/README.md:1-27 — 版本契约总述（version:"v1"、camelCase、unknownFields:"ignored"、additionalProperties:true 容忍语义）+ 页面索引 + 指向 sdk/schema/CONTRACT_HANDOFF.md ； /Users/ll/code/yanshi/docs/api/resources.md — 11 个生成区块：api-defs:{Thread,Turn,Item,ThreadStartParams,ThreadResumeParams,Thr…

#### `H2` CONTRIB1 — 贡献指南 + docs 归档

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：CONTRIBUTING 存在；约定可执行；docs 结构清晰
- **实测缺口**：CONTRIBUTING 与归档均落地且验收满足，但有两处不精确：(1) docs/archive/README.md 的映射表只列了 3 条（analysis-report / feature-comparison-with-codex / feature-roadmap-codex-deepseek），且末尾"说明"段明确写着"综合报告（synthesis-final.md / synthesis-report.md / synthesis-report-v2.md）与依赖分析在本批归档时为未跟踪文件，未进入仓库历史，故不在本目录"——但实测这 3 份 synthesis-* 文件**就在** docs/archive/ 里且已被 git 跟踪（git ls-files 确认），feature-roadmap-e-h.md 也在目录内却未进映射表。README 与目录实际内容自相矛盾，映射表漏 4 条。(2) CONTRIBUTING.md 未提及 internal/archtest 机器强制治理（GOV1 依赖分层 / GOV2 1000 行 / GOV3 doc 注释）与 cmd/codelines，也未提 cmd/gendocs / cmd/api-schema 的生成文档必须重跑并提交（CLAUDE.md 明确写这是 CI diff-gate）——贡献者按 CONTRIBUTING 走会在 governanc…
- **二审改判理由**：CONTRIB1 是两半交付（CONTRIBUTING + docs 归档），第一半确实完整，第二半有我自己核到的实质缺口，不足以判「已实现」。 【第一半：CONTRIBUTING.md 真实非空壳，我逐符号验过】/Users/ll/code/yanshi/CONTRIBUTING.md 66 行、git ls-files 已跟踪。它引用的每个符号都落到真实代码：bootstrap.Build → internal/bootstrap/bootstrap.go:260（且装配顺序与正文一致：config→store→...）；tools.WithProfile → internal/tools/guard.go:21；WithSubAgentRunner → internal/tools/subagent.go:30；WithVCS → internal/tools/vcsctx.go:24；permctx.go / vcsctx.go 两文件均存在；FakeModel → internal/llm/eino/fake.go:16；FakePlanner → goalloop/planner.go:14；FakeImplementer → goalloop/implementer.go:43；FakeAgent → internal/acp/fakeagent.go:25；go.mod:117 `replace gith…
- **证据**：/Users/ll/code/yanshi/CONTRIBUTING.md（66 行）— 覆盖 plan Task 13 全部 13 个必选约定段：怎么开始（go build / go test / cmd/testchanged / --fake-model / cp config.example.yaml）、六边形唯一组合根 bootstrap.Build 及固定装配顺序、context 注入（WithProfile/WithSubAgentRunner/WithVCS）、guard fail-closed、Fake 优先、≤1000 纯代码行、注释密度、单 binary、两种传输一套协议…

#### `H2` EX1 — examples 目录

- **优先级** P2 ｜ **路线图原状态** 缺失 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：≥5 个示例；可跑；覆盖主要集成点
- **实测缺口**：7 个示例全部实测可跑/可编译，超额完成验收。轻微缺口：CI（docs.yml）覆盖了 custom-tool build、headless exec/batch、TS typecheck、Python parse+import 五项，但**未**跑 goalloop-config/run.sh，也未对 custom-skill 的 SKILL.md 结构字段做断言——这两项在 plan Task 12 Step 4 是列了的（"goalloop 两轮冒烟（按 run.sh）"、"custom-skill：断言 SKILL.md 结构字段齐全"）。二者本地实测均通过，但缺 CI 阻断，漂移风险未被守门。
- **二审改判理由**：7 个示例目录确实存在，一审的符号行号也经我核实全部准确（internal/tools/guard.go:21 WithProfile、:79 SyncStream、:163 NewGuardedTool）。但一审只看退出码为 0 就判"可跑"，未验证示例是否真的做了 README 声称的事。我实测发现 7 个中有 2 个是坏的： 【缺陷1 sdk-python 运行期崩溃，退出码 1】examples/sdk-python/main.py:25 用 item.toolName，但 Python SDK 模型字段是 tool_name，toolName 只是 pydantic alias（sdk/python/src/yanshi_sdk/generated.py:121: tool_name: Optional[str] = Field(default=None, alias="toolName")）。alias 只作输入键、不是属性名，属性访问必抛 AttributeError。对着 --fake-model 后端实跑：stdout 输出 "thread: 6c12c47d..." 后 stderr 报 "error: 'Item' object has no attribute 'toolName'"，EXIT_CODE=1。TS 孪生示例正确是因为 sdk/ts/v1.ts:48 确实叫 toolName——作者…
- **证据**：/Users/ll/code/yanshi/examples/ — 7 个示例（超过验收要求的 ≥5）：headless-exec / headless-batch / sdk-typescript / sdk-python / custom-tool / custom-skill / goalloop-config，共 17 个文件 ； /Users/ll/code/yanshi/examples/README.md — 每示例一行 + 怎么跑 + API gap 汇总段 ； /Users/ll/code/yanshi/examples/custom-tool/main.go — 真实可运行…

#### `H2` UDOC1 — 用户指南

- **优先级** P2 ｜ **路线图原状态** 部分 ｜ **接进运行时** 是 ｜ **有针对性测试** 是
- **验收标准**：覆盖主要用法；getting started 可零依赖跑通；与实际不漂移
- **实测缺口**：核心全部落地，但有两处守门与入口的缺口：(1) docs.yml 的 paths 过滤器只列了 docs/** examples/** cmd/api-schema/** cmd/gendocs/** internal/docgen/** internal/config/** internal/api/v1/**，**缺 cmd/yanshi/****——而 CLI 帮助快照的真相源正是 cmd/yanshi/main.go 的 25 个 FlagSet 定义（如 main.go:445 的 -addr）。只改 main.go 的 flag 文案/新增 flag 的 PR 不会触发 docs job，help:* 快照可静默漂移，直到下次有人碰 docs/ 才被抓到。这正是 UDOC1 验收"与实际不漂移"要防的场景。(2) 根 README.md（254 行）对 docs/user-guide、docs/api、docs/adr、examples/、CONTRIBUTING.md 的引用数为 0（实测 grep -cE 结果 0），只提了 docs/skills-authoring.md 与 docs/vcs.md——plan Architecture 段明确写"README=精简入口"，但入口没接上，新用户从 README 找不到用户指南。
- **二审改判理由**：一审的"结构与生成器为真"我复核后确认属实，但它漏掉了验收标准第三条"与实际不漂移"的**实锤违反**：手写 prose 已经漂移，且是可证伪的事实性错误，不是一审所说的"只是守门缺口"。 【实锤 1：文档里的三个斜杠命令在运行时根本不存在】 docs/user-guide/tui.md:27 写「`/keymap`、`/vim`、`/contrast`：切换键位 / Vim 模式 / 高对比度主题（写入 preferences.json）」，tui.md:19 又写「`/keymap` 可切换键位方案」。configuration.md:93 同样写「C15 TUI 偏好（也可运行时 `/keymap`、`/vim`、`/contrast` 改）」。 但 /Users/ll/code/yanshi/internal/cli/tui/commands.go:51 的 `commandTable` 共 35 项，逐项枚举后**不含 keymap / vim / contrast**（三者从未注册）。commands.go:114 `runCommand` 的唯一分发路径是：`name == "help"` 特判 → `lookupCommand(name)`（commands.go:90）→ 查不到就走 commands.go:121 `m.entries = append(m.entries, errorEntry{text: "unk…
- **证据**：/Users/ll/code/yanshi/docs/user-guide/README.md:13-22 — 索引表列出全部 8 个专页且链接全部可达 ； /Users/ll/code/yanshi/docs/user-guide/getting-started.md:1-57 — 全程 --fake-model 零 API key 四步（build / cp config / -inprocess TUI / exec headless） ； /Users/ll/code/yanshi/docs/user-guide/configuration.md:1-50+ — server/stor…

---

## 6. 已实现（30 项）

经二审证伪后仍成立：功能存在、非占位、已接进运行时、验收标准基本满足。

| 批次 | ID | 功能 | 优先级 | 备注 |
|---|---|---|---|---|
| `M1` | **A11** | 分层项目指令（AGENTS.md 父→子合并） | P2 | 无差异。按 plan 只在 fs_read 注入（fs_write/fs_edit 返回 JSON，前缀会破坏契约）。 |
| `M1` | **A12-core** | Per-Turn 结构化输出核心（provider 无关校验+重试） | P1 | 无差异。WS/SSE 两条传输都接通，帧词表同步，客户端 StreamEvent 也能消费。 |
| `M1` | **A12-providers** | per-turn output schema 注入 anthropic / re… | P1 | 与 plan 的 DESIGN NOTE 一致：Anthropic 侧实现的是 `output_config.format`（而非 brief 里写的顶层 `output_schema`），这是 plan 自己已声明并说明理由的偏差，非实现缺陷。openai（eino-ext chat completions）路径按计划不注入，靠 A12… |
| `M1` | **C07** | 消息排队模式（queue / batch / single） | P2 | 无差异。plan 里注明的「现有 doc 注释 single 语义与验收冲突」已按验收改正。 |
| `M1` | **SPEC-COMPACT** | 上下文压缩重写（internal/ctxcompact 统一核心） | P0 | 两点小差异：(1) roadmap §2 明确记的残余「mid-turn 压缩显式冷却」——compacting.go:85 有 lastCompactTokens 字段做重复压缩抑制，但没有时间维度 cooldown，与 §2 记述一致（边际加固，非阻塞）。(2) config.ContextWindowFor（config.go:64… |
| `M1` | **SPEC-TOOLOUT** | 工具输出治理（64KiB spillover + fs_read 分页 + su… | P1 | 无差异，且已扩展：SpillThreshold 后来被 artifact_output.go:14、review.go:62、gate.go:128 复用为统一阈值（超出 plan 范围的正向复用）。 |
| `M1` | **T06** | 多文件 Patch 工具 apply_patch | P1 | 无差异，且已超出 plan：fs_patch_test.go:261/286 显示后来又接了 LSP diagnostics 回喂（B2 lane 的增量）。 |
| `M1` | **V10** | 会话生命周期 rename / archive / unarchive / de… | P2 | 无差异。SSE 按 plan 决策不支持 session 管理帧（无状态），属有意设计。 |
| `M1` | **V12** | 无头 exec 子命令（headless exec） | P1 | **⬆ 二审推翻一审缺口**：推翻一审「有差别」。一审的核心差异论据之一是错的，另一条不构成功能差别。 (1) `--output-schema` 缺失属于一审自造需求。我读了真正的 V12 plan `/Users/ll/code/yanshi/docs/superpowers/plans/2026-07-18-m1-lane… |
| `B0` | **TD2** | compactNow hack 消除 + 双压缩路径协调（冷却） | — | 无差异，且**优于路线图记载**。路线图（2026-07-21）称 "grep cooldown/lastCompact 无命中"，把冷却列为未做的边际加固；代码实测冷却已完整落地：CompactingModel 具备 token 增长 + 时间双维度冷却、HardForceFraction 近窗口边缘越过冷却的安全网、cmMu 保护并发… |
| `B0` | **TD3** | ws.go 超 1000 行（GOV2 纯代码行治理） | — | 无差异。路线图判定"按纯代码行 > 1000 标准已不再违规"成立，且当前实测（636）比路线图记录的 857 又低了 221 行。需要澄清一点口径：路线图"实测 1480 总行"与当前 `wc -l` 1187 不一致，说明 07-21 之后 ws.go 本身仍在继续瘦身；但 GOV2 门禁只看纯代码行，两个口径下结论一致。此项无施工需… |
| `A2` | **DT3** | Artifacts (大输出→摘要) | P1 | 实现与规划一致：Artifact 含 summary + content_ref，工具结果只给模型 summary + artifact_ref，详查走 artifact_read，quota(64MiB/task) + TTL(7天) + 6 小时 janitor 齐备。次要差异：规划的 Artifact 存储路径写 `internal… |
| `B2` | **RB1** | 逐轮回滚 UI (seam 快照 + /restore-turn + rever… | P1 | 验收 5 条全部满足，实现范围反而超出规划（额外做了会话历史截断+快照回滚、full-head 乐观锁绑定、undo/audit 双 seam、磁盘补偿）。两处已在 plan 文档 Limitations 明示的 scope cut 值得记录：(1) D4——agent 侧 revert_turn 工具是 VCS-only，只回滚 mai… |
| `C2` | **UX5** | 草稿 stash /stash | P2 | 功能与验收对齐（暂存/列出/恢复/删除/持久/与 queue 互不干扰——stash 走 Ctrl+S 与 /stash，queue 走 enqueue，两条独立路径）。**唯一方案差异**：规划设计写「持久到 store」（即 SQLite store），实际持久化到 `os.UserConfigDir()/yanshi/stash.j… |
| `C2` | **UX6** | prompt 历史 Alt+R | P2 | 无差异，且比规划多做了 Alt+↑（plan 文档 §范围决策补的）。历史上限 500 + 去重 + JSONL 自愈均落地；只记录 dispatchSend 实际发出的 prompt（排队未发的不进历史）。持久化位置同样是 os.UserConfigDir()/yanshi/history.jsonl（规划未指定位置，不算偏差）。 |
| `C2` | **UX7** | 堆叠 toast 通知 | P2 | 验收三条全部满足：叠放（≤5，FIFO）、按级别自动过期（error 需 Esc 手动关）、不覆盖内容（toast 高度进 reflow 预算，viewport 相应收缩）。**唯一观察**：pushToast 目前只有 5 处调用点（handlers.go:175/374/381、stash.go:186/199/202/205/209… |
| `C3` | **MEM1** | 用户 memory 文件 | P1 | 无差异（规划落点 internal/instruct 扩展，实际另起叶子包 internal/memory，但这是 plan 明示的等价选择，且 instruct 的 bounded-read 模式被复刻）。诚实边界：memory 在 bootstrap 一次性 bake 进 system prompt，remember 写入的新条目要下… |
| `C3` | **V09** | 会话 fork | P1 | 实现方式与规划一致（store 层 fork + /fork 命令）。两处已声明的 scope cut：(1) 规划风险项提到"引用计数/COW"，实际是逐行复制消息（session_fork.go:114-122），大历史 fork 成本是 O(n) 行插入，注释里以"rows are not shared, so COW is imp… |
| `C3` | **V11** | ephemeral / side 对话 | P2 | 核心与规划一致（side 隔离、可返回、不持久、清理不影响主 session）。两处与规划的细节差异：(1) 规划落点写 `internal/store/`（side thread），实际 side 完全是 WS 层 connSession 的纯内存栈，store 里没有 side thread 概念——这更贴合"默认不持久"的设计意图，… |
| `D3` | **O03** | auth manager（provider-neutral 认证生命周期） | P3 | 功能与规划一致且更完整，仅有两处“接线可见性”缺口：(1) `yanshi auth` 没有出现在 cmd/yanshi/main.go:38-77 的 usage 文本里（Usage/Subcommands 两个块 grep 均无 auth），只有 doctor 被列出；用户执行 `yanshi -h` 看不到 auth 子命令，doc… |
| `E1` | **COV1** | store 覆盖 58% → 75%+ | P0 | 两处与计划文档的细节偏差（均为计划猜错、测试按真实行为写，属正确处理）：(1) 计划 Task 3 假设 AppendMessage 到不存在 session 后 Messages 返回空，真实实现返回孤儿行——session_test.go:132 的断言改成了 require.Len(msgs, 1) 并加注释说明 FK 未强制；(2… |
| `E2` | **FUZ1** | guard.MatchGlob fuzz | P1 | **⬆ 二审推翻一审缺口**：推翻一审的「部分实现」。一审列的两条缺口经我实测均不成立。 【缺口①「种子语料未入仓」不成立 —— 一审对 Go fuzz 工具链行为的理解有误】我实跑了 `go test -fuzz=FuzzMatchGlob -fuzztime=40s ./internal/guard`：`gathering … |
| `E2` | **RAC1** | race detector 固化 + 并发热点测试 | P0 | 无实质差异，且在两点上超出规划。①CI `-race` 是真硬门禁（无 continue-on-error），比规划的「PR 跑变更包 / nightly 跑全量」分层更严：ci.yml 每次 PR 就逐包跑全量 `-race`，nightly 再跑三平台全量。逐包 3 次重试是对「-race 时序 flake」的务实处理，注释说明真 r… |
| `E3` | **GOV1** | 依赖分层治理测试 | P0 | 实现比规划更完整（R1–R5 五条规则 + 合成变异测试）。两点细微差异：(1) portExceptions 比 plan 多一条 store→secrets（plan 只列 store→auth / config→guard，实际代码额外登记了 secrets，属于 exceptions 数量比计划终态多 1 条，但每条带 reaso… |
| `E3` | **GOV2** | 文件纯代码行数门禁 + 超长文件拆分 | P1 | 功能满足验收，但有一个口径不一致值得记录：门禁 archtest 用 go/parser 字节区间法（块注释行不计），而 CLAUDE.md 推荐的即时检查工具 cmd/codelines/main.go:39-44 用的是 strings.TrimSpace + HasPrefix("//") 的启发式——它会把 /* */ 块注释的中… |
| `E3` | **GOV3** | exported symbol 文档覆盖 | P2 | 两处与 plan 的细微差异：(1) plan Task 7 的 docExceptionPkgs 注释说键形如 "internal/version.BuildStamp"，实际 docs_test.go:46 的 key 构造是 pkgRel + "." + d.Name，语义一致；(2) 整包豁免按'文件所在目录'近似判定（docs_… |
| `F2` | **LEAK1** | createdWT map 泄漏清理 | P1 | 无差异。规划要求的四条验收（终态回收、pending 保留、长跑 map 有界、-race 无竞态）全部有对应实现与测试。plan 文档的 reclaimWorktree 代码与仓库实际代码逐行一致。 |
| `H1` | **CIG1** | CI 门禁矩阵 | P0 | 核心矩阵完整且为硬门禁，与规划一致。一处**残留偏差（非阻断）**：governance 与 fuzz-seed 两个 job 仍带 `continue-on-error: true`（ci.yml:116、:138），注释写的是“soft until E3/E2 lands, drop continue-on-error once st… |
| `H1` | **UPG1** | 升级兼容 + release doctor | P1 | 无差异，四项子能力（配置 schema 版本化、迁移框架、doctor --release、升级指南）全部落地并接进运行时（config.Load 门 → config.example.yaml → doctor 检查表 → main.go flag → 文档）。唯一保留的已知空洞是 `internal/cli/doctor.go:512… |
| `H2` | **ADR1** | 架构决策记录 | P2 | 无差异。≥10 条、模板在、索引在、关联路径 CI 可达，且流程已写进 CLAUDE.md 与 CONTRIBUTING.md。 |

> 值得单独点名的三个批次：
> - **E3 架构治理**（GOV1/GOV2/GOV3 全绿）—— 本次审计中唯一「文档说到、代码全部做到」的批次，且门禁经三次真实变异测试（加禁止导入 / 造 1010 行文件 / 造无注释导出函数）确认会红，不是空壳测试。
> - **E2 RAC1** —— CI 的 `-race` 是真硬门禁（无 `continue-on-error`），且实现范围超出规划（多做了 `internal/vcs/seam_race_test.go` 与生产侧加锁）。
> - **B2 RB1 逐轮回滚** —— 验收 5 条全满足，且额外做了会话历史截断、full-head 乐观锁、undo/audit 双 seam、磁盘补偿。

---

## 7. 超出规划的实现

代码里已经落地、但两份路线图**都没有规划过**的能力。这部分说明实际实现的广度超过了规划文档，也意味着这些能力目前**没有任何规划文档为它们背书**——缺失设计依据与验收标准。

（两份路线图均无对应条目）

### 一、安全 / 权限

**1. 破坏性删除门（`ClassifyDestruction` / `checkDestructive`）**
- 位置：`/Users/ll/code/yanshi/internal/guard/destructive.go`（+ `internal/guard/guard.go` 的第一维度接线）
- 做什么：在任何 profile 检查之前，用独立的宽容词法器 `lexShellLite` 把 shell 删除命令分成 `DestructionNone` / `OutOfScope` / `Catastrophic` 三级；`rm -rf /`、`~`、`$HOME`、`*`、workdir 自身或祖先、裸 `rm -rf` 判为 Catastrophic，**所有模式（含 yolo）都拦**。
- 为什么超出规划：路线图的安全批次只有 S06（execpolicy）/S07（持久审批）/S08（sandbox）/S09（网络隔离）。两份文档全文对 `rm -rf`、"破坏性"、`Catastrophic` 零命中。这是一个 profile 无关、模式无关的第五道独立防线，且刻意**不复用** execpolicy 的 lexer（execpolicy 会拒掉 `*`/`$HOME`/`C:\` 这些恰恰是灾难形态的 token），属于自发设计的安全语义。

**2. 交互式权限模式体系（yolo / auto / allow-edits + AI 风险评分）**
- 位置：`/Users/ll/code/yanshi/internal/guard/mode.go`、`/Users/ll/code/yanshi/internal/api/http/ws_perm.go`（`assessRisk`，第 280-286 行）
- 做什么：在静态 profile 之上叠加 5 种会话级权限模式；`auto` 模式实际调用 LLM 给每次工具调用打 1-10 风险分，`<= AutoThreshold`（默认 4）自动放行，否则弹窗。Shift+Tab 循环切换，yolo 还有两次 Enter 确认。
- 为什么超出规划：S07 规划的是"持久审批规则"（`{action, scope, ttl, source}` + `/permissions` 查看撤销）——那部分确实落地了。但"AI 给风险打分决定是否放行"是完全独立的一层，两份文档对 `yolo`、`allow-edits`、`Shift+Tab`、`assessRisk`、"风险评分"全部零命中。

**3. HardDeny 两档语义（`Decision.Overridable`）**
- 位置：`/Users/ll/code/yanshi/internal/guard/guard.go`
- 做什么：把硬拒绝拆成"结构性"（shell 元字符、execpolicy parse-error、未知 policy —— 任何模式不可越过）与"可覆盖"（空 allowlist、`policy: deny`、denylist 命中、MCP 空白名单 —— yolo 直接越过、auto 交 AI 判）。
- 为什么超出规划：路线图只写了 guard "四维 fail-closed"。实现是六维（destructive/tools/fs/shell/net/mcp）且带可覆盖性分级——这套"profile 策略 vs 语法防线"的二分是 ADR-0004 之后自演化出来的，路线图里没有任何条目描述它。



### 二、工具面

**4. `github_approve` / `github_merge`（PR 写操作）**
- 位置：`/Users/ll/code/yanshi/internal/tools/github.go`（第 158、181 行）
- 做什么：`gh pr review --approve` 与 `gh pr merge --merge|--squash|--rebase`，让 agent 能批准并合并 PR。
- 为什么超出规划：GH1 明确列举的写操作只有 `github_comment` 与 `github_close_issue`，且强调"绝不因 agent 停止就关 issue"的克制姿态。approve/merge 是比关 issue **更高危**的写权限（直接改仓库主干），路线图里两个名字零命中。

**5. `agent_dag`（依赖图工作流引擎）**
- 位置：`/Users/ll/code/yanshi/internal/tools/agent_dag.go`
- 做什么：`workflow_start` 接受带 `deps` 的步骤定义，做 ID 展开（`step[1-5]` 式扇出）、依赖解析、拓扑分层排序，然后逐层限并发执行子代理，并支持 `interpolatePrompt` 把上游结果注入下游 prompt 模板。
- 为什么超出规划：路线图 §6 引言只把 `workflow_start` 当作"已有"一笔带过，M07 规划的是"CSV 批量 agent jobs（限并发、逐项 spawn、汇总）"——那是**扁平批量**。带拓扑排序 + 步骤间结果插值的 DAG 编排是另一个量级的能力，`agent_dag` 在两份文档零命中。

**6. `RolePolicy` / `AgentRoles` 的策略收紧机制**
- 位置：`/Users/ll/code/yanshi/internal/tools/rolepolicy.go`、`/Users/ll/code/yanshi/internal/tools/agentroles.go`
- 做什么：每个子代理角色带 `Policy *RolePolicy`，在**父 guard 之上再收紧**工具/路径权限（注释明确 "non-nil == tighten parent"），越权配置直接 `ErrRolePolicyDenied`。
- 为什么超出规划：M05 规划的是"7 个角色 + 可选 model/reasoning override"，即角色是**提示词与模型的差异**。"角色自带一层只能更严不能更松的权限策略"是额外的安全不变量，`RolePolicy` 在两份文档零命中。

**7. 连续工具错误熔断（`errcnt`）**
- 位置：`/Users/ll/code/yanshi/internal/tools/errcnt.go`
- 做什么：per-turn 计数器，工具连续失败 5 次后 `GuardedTool` 返回 Go error 中断整个 turn，而不是继续把失败转成 tool result 让模型无限重试。成功调用清零。
- 为什么超出规划：这与 ADR-0001 的 `UnknownToolsHandler` 设计（失败当结果回喂）是刻意的例外，属于成本/死循环防护。两份文档对"熔断"、"circuit"、"5 consecutive" 零命中。



### 三、架构 / 基础设施

**8. `internal/secproc` —— 统一子进程发射点 + Authorizer 防火墙**
- 位置：`/Users/ll/code/yanshi/internal/secproc/secproc.go`
- 做什么：仓库内**唯一**的 `exec.CommandContext` 出口。`shell_run`、ACP agent、`gh` 调用全部必须走 `secproc.Launch` 过 Authorize 防火墙。为打破依赖环，Authorizer 是由 tools 包 `init` 填充的函数变量，secproc 因此保持叶子包。
- 为什么超出规划：S08/S09 规划的是 sandbox 与网络隔离两个**能力**，没有规划"把所有子进程发射收敛到单一强制入口"这个架构约束。`secproc` 在两份文档零命中。同类的还有 `internal/securityctx`（打破 tools/secproc/shell 依赖环的 context key 专用包）与 `internal/pathjail`（canonical root-jail 唯一实现，含 Windows 盘符/大小写绕过防护），同样零命中。

**9. `internal/execprobe` —— 抗挂死的工具链探测**
- 位置：`/Users/ll/code/yanshi/internal/execprobe/probe.go`
- 做什么：跑 `tool --version` 时同时防两种挂死——进程不退出，以及 **CreateProcess 系统调用本身挂住**（Windows App Execution Alias 的 python3.exe 存根会阻塞在跳转 Microsoft Store）。超时后主动"抛弃"goroutine，保证 orchestrator 启动与 TUI banner 永不被单个探测拖死。
- 为什么超出规划：路线图里"探测"只出现在 sandbox 能力探测、LSP 语言探测、run_tests 构建系统探测的语境。这是一个为特定平台故障模式写的独立包，零命中。

**10. `internal/lockfile` + 后端发现自愈选举**
- 位置：`/Users/ll/code/yanshi/internal/lockfile/`、`/Users/ll/code/yanshi/internal/cli/session.go`
- 做什么：TUI 通过 OS cache 目录下按项目划分的 lockfile + 一次 `/healthz` 探测找现存后端；找不到就在 `127.0.0.1:0` 进程内引导一个并认领 lockfile。owner 退出后，断开的多个客户端里第一个发现无存活后端的会重新引导（带 PID 存活回收的原子选举）。
- 为什么超出规划：这是"单二进制同时是客户端与服务端"这一产品形态的承重实现。两份文档对 `lockfile`、`healthz`、"多窗口"、"后端发现"、"自愈" 全部零命中。

**11. 分布式 Task API + `cmd/agent-worker` 远程 worker**
- 位置：`/Users/ll/code/yanshi/internal/api/http/taskapi.go`、`/Users/ll/code/yanshi/internal/agent/worker/`、`/Users/ll/code/yanshi/cmd/agent-worker/`
- 做什么：一整套 claim/heartbeat/progress/result + SSE `task_available` 信号的分发协议（7 个 HTTP 路由），加一个独立二进制，可以从另一台机器连上 yanshi 服务端认领并执行任务。
- 为什么超出规划：DT1 规划的 durable task 是"面向模型/用户的持久工作单元"，且明确"broker=传输分发，work=工作单元语义"。**跨进程/跨主机的 worker 分发协议 + 独立 worker 二进制**是另一件事。`task_available`、`heartbeat`、"Task API"、`agent-worker` 在两份文档零命中。

**12. `internal/observe/log/redact.go` —— 结构化日志脱敏器**
- 位置：`/Users/ll/code/yanshi/internal/observe/log/redact.go`
- 做什么：按 key 名白名单（`apikey`/`token`/`prompt`/`messages`/`args`/`command`/`path`/`host`/`url`/`headers`…）在 slog handler 层自动替换为 `[REDACTED]`。
- 为什么超出规划：OBS1/OBS2 只写了"默认脱敏(secret/prompt 不入日志)"这一句验收标准，S10 把"统一脱敏层"落在 `internal/secrets/`。实际实现把脱敏做成了 observe 层的独立组件，且脱敏面远超 secret——把 `path`/`host`/`url`/`command` 也纳入（隐私而非仅凭据）。这是超出验收标准的实现深度。



### 四、开发者体验

**13. `cmd/testchanged` —— 变更包增量测试**
- 位置：`/Users/ll/code/yanshi/cmd/testchanged/main.go`
- 做什么：`git diff --name-only HEAD`（含未跟踪）→ 提取目录 → `go list` 过滤非包目录 → 只对有变更的包跑 `go test`，透传所有 `go test` 参数。
- 为什么超出规划：CIG1 提过"CI 分层（PR 跑变更包）"，但那是 CI 侧策略。一个供本地开发用的独立 Go 命令零命中。

**14. `cmd/depsanalyze` / `cmd/codelines` —— 治理的可视化前置**
- 位置：`/Users/ll/code/yanshi/cmd/depsanalyze/main.go`、`/Users/ll/code/yanshi/cmd/codelines/main.go`
- 做什么：depsanalyze 打印 internal 包的 fan-in/fan-out、依赖分层、体积离群、覆盖率比、风险标记（高扇出/超千行/低覆盖/sink/外部供应链依赖）；codelines 做 GOV2 口径的纯代码行即时检查。
- 为什么超出规划：GOV1/GOV2 规划的是**CI 门禁测试**（红/绿）。这两个是给人看的诊断工具——门禁告诉你"违规了"，它们告诉你"违规在哪、风险分布如何"。两份文档零命中。

**15. `internal/docgen` + 四个生成器的 diff-gate 闭环**
- 位置：`/Users/ll/code/yanshi/internal/docgen/docgen.go`、`cmd/gendocs`、`cmd/api-schema`、`.github/workflows/docs.yml`
- 做什么：`BEGIN GENERATED` / `END GENERATED` 标记块的替换-或-追加原语（抽出来避免两个 generator 复制粘贴），配置文档 / API schema / 全部子命令 `-h` 文本都由生成器写入标记块，CI 用 `git diff --exit-code` 卡住漂移。
- 为什么超出规划：UDOC1 提到"命令表/配置骨架自动生成片段（新 `cmd/gendocs`）+ CI git diff 守门"——**生成器本身在规划内**。超出的是把标记块原语抽成一个受 GOV1 分层治理约束的叶子包（"只导入标准库，不参与任何 port allowlist"），以及把 `-h` 文本也纳入生成范围（`docs/user-guide/tui.md`、`entrypoints.md`）。属于规划的实现深度溢出，价值在架构层。



### 五、边界情况（列出但价值判断偏低）

**16. `third_party/bubbletea` fork —— Windows Ctrl+Enter 区分**
- 位置：`/Users/ll/code/yanshi/third_party/bubbletea/key_windows.go:237-241`、`key.go:259-263, 543`
- 做什么：上游 bubbletea 在 Windows coninput 驱动下把 `VK_RETURN` 无条件收敛为 `KeyEnter`（VK_RETURN 无修饰键字段），fork 加了 `KeyCtrlEnter`，让 TUI 能绑 Enter=发送 / Ctrl+Enter=换行。同时补了 kitty/CSI-u 的 `\x1b[13;5u` 映射。
- 为什么超出规划：C15（keymap 配置）规划的是"核心按键可重映射 + Vim 开关 + 冲突诊断"，且实现确实有 `internal/keymap`。但**为了一个键位而 fork 并长期维护上游依赖**（go.mod replace 指令）是路线图完全没有预期的成本承诺。价值判断偏低是因为用户可见收益很小（一个键位），但架构负担真实存在。



### 说明：以下曾疑似超出，核查后**不算**

- `/stats`、`/think`、`/cost`——`/think` 在 §10 引言中列为"yanshi 已有"，`/cost`+`/stats` 在 COST1 中明确出现（"`/cost` 显示 token+$，`/stats` 显示历史会话 $ 聚合"）。
- `/theme`——C15 中"高对比主题(接现有 `/theme`)"确认为既有。
- `/logs`——虽字面零命中，但属 OBS1 slog 落地的自然 TUI 出口。
- `vcs_merge`/`vcs_restore`/worktree——autoVCS 在两份文档中被反复当作"已有基座"引用（S07 的 §0.3 装配约定、RB1 的"复用 autoVCS"）。
- `internal/appserver`、`internal/i18n`、`internal/secrets`、`internal/auth`、`internal/features`、`internal/imagestore`、`internal/clipimg`、`internal/tools/screenshot.go`——分别对应 APS1 / I18N1 / S10 / O03 / OBS3 / VISION-TOOL（VISION-TOOL 明确点名了 imagestore、clipimg、screenshot 三处落点）。
- `internal/instruct`——附录 A 明确标注 "A11 | (已完成) | 分层指令 — `instruct` 包"。
- goalloop T0-T4 分层技能 + `RuleTierer`——附录 A 的 "G02/G03/G04/G05 | TD1/G03/G04/G05 | goal budget/**tier**/resume/plan" 覆盖。

---

## 8. 优先修复建议

按「性价比 = 影响面 ÷ 改动量」排序。

### P0 — 一行到几十行，解锁大片功能

| # | 问题 | 改动 | 解锁 |
|---|---|---|---|
| 1 | 生产 Factory 不填 `Cmd` | `internal/shell/factory.go:74` 补一个字段 | **8 个已注册工具**从必崩变可用（W07/DT4/DT5/GH1 整批） |
| 2 | `bootstrap/c1.go` 零调用 | `bootstrap.Build` 里调一次 `BuildC1` | `rlm_query` + 8 个 `automation_*` + `agent_batch`（RLM1/AU1/M07） |
| 3 | `.goreleaser.yaml` 用已移除字段 | 删掉 `changelog:` 段 | 首次 `v*` tag 发布不会直接失败（PKG1） |
| 4 | 两次 `WithChatModelOptions` 覆盖 | `orchestrator.go:494-499` 合并为一次 | `reasoning_effort` 不再被静默丢弃 |
| 5 | `registry.WithRole` 零调用 | `runAgentLoop` 派生 ctx 时绑 role | 7 角色的 prompt 前缀 + `RolePolicy` 安全不变量 + 五段输出契约（M05/M04b） |

### P1 — 补最后一跳接线

| # | 问题 | 改动 |
|---|---|---|
| 6 | `ApplyImages` 零调用 | WS `runUserTurn` 与 v1 `runTurn` 读 `Images` 并调分流；`TurnOpts` 加图像字段（VISION/VISION-TOOL） |
| 7 | shell v2 九工具未注册 | `bootstrap` 注册 `NewShellV2Tools` 并给 `shell.Manager` 填 `Config.Factory`（T07/T08） |
| 8 | `TurnOpts.PlanMode` 从不设置 | `ws.go:644` 填 `PlanMode`，让 guard 的 plan 防火墙与 `filterPlanTools` 真正生效（G05） |
| 9 | goal token 预算不可设 | 加 `-max-tokens` flag + config 项（TD1/G02/LEAK3） |
| 10 | 压缩 cooldown 量纲错配 | 统一 `lastCompactTokens` 与比较值的量纲；`CooldownTokens` 改用 per-provider 窗口（CCL1） |
| 11 | 并发槽位泄漏 | `finishTerminal` 末尾调 `detachRuntime` + `cancel`；legacy 入口接入 Manager（M04/LEAK2） |
| 12 | `work` 包写路径不走 `WriteTxer` | 11 个写方法接上 `wt().WriteTx`，否则包头注释是假的（WAL1） |

### P2 — 一致性与守门

| # | 问题 | 改动 |
|---|---|---|
| 13 | 文档超前于代码（发现 10 的 6 处） | 删掉不存在的命令描述，或补实现 |
| 14 | 软门禁未收紧 | 删 `ci.yml:116/138` 的 `continue-on-error` |
| 15 | 覆盖率无门禁 | 把 75%/80%/50% 纳入 CI 或 `archtest` |
| 16 | `docs.yml` paths 缺 `cmd/yanshi/**` | 补一行，防止 CLI 帮助快照静默漂移（UDOC1） |
| 17 | 两份 schema 并存（3 $defs vs 21 $defs） | 加 parity 测试或统一来源（V14/V15） |
| 18 | SDK / IDE 测试完全不在 CI | `scripts/check-d2.sh` 已写好但从未被任何 workflow 引用，接上即可（V15/O12） ⚠️ **已作废：该脚本与 `ide/vscode/` 已随 D2/O12 删除，不要「接上」——见本文顶部 D2/O12 作废声明** |
| 19 | `-h` 里没有 `auth` 子命令 | `cmd/yanshi/main.go:38-77` 补进 usage（O03） |
| 20 | 路线图状态全面滞后 | 用本报告替代，或在两份路线图头部加指向说明（发现 12） |

---

## 9. 审计元数据

| 项 | 值 |
|---|---|
| 编排方式 | Workflow 两阶段：23 批次并行扫描 → 每项独立对抗式证伪 → 遗漏发现 |
| 子代理数 | 118 |
| 子代理 token | 13,029,689 |
| 工具调用 | 6,776 |
| 墙钟耗时 | 约 4.3 小时 |
| 覆盖功能项 | 94（A–D 55 + E–H 25 + M1 早期 14） |
| 二审改判率 | 40 / 94 = 43% |
| 失败的证伪代理 | 2（`UX5`、`O07`，504 网关超时，保留一审判定） |

### 已知局限

- **2 项未经二审**：`UX5`（草稿 stash）与 `O07`（doctor 增强）的证伪代理因网关 504 失败，两项采用一审判定。考虑到二审的降级倾向，这两项的实际状态可能略低于本报告所载。
- **判定基于静态代码分析 + 局部实跑**，未做完整的端到端手工验收。标注「已实现」不等于无 bug，只表示功能存在且验收标准基本满足。
- **`internal/acp` 有一个与本次审计无关的既存测试失败**（`TestSpawnCmdStartFailure`，疑似本机 PATH 上存在 `opencode` 二进制所致）。
- 审计过程中一个子代理删除了一个测试残留的畸形文件名目录（`internal/cli/?_pragma=busy_timeout(5000)&...`，167KB 未跟踪 SQLite 文件）。建议排查 `internal/cli` 下把 DSN 直接当路径用的测试，并把该模式加进 `.gitignore`。

---

*本报告由机器化审计生成，所有判定均可通过报告中的 `file:line` 证据独立复核。*
