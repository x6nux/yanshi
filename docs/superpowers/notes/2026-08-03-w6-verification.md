# W6 工具面收口 — 核验报告

> 2026-08-03 实测产出。**审计对这 11 项的判定理由已大面积过时**，本报告取代审计作为 W6 计划的事实依据。
> 所有结论均为实跑或逐行读出；未验证项已明确标注。

## 0. 一句话结论

审计判 `W07`/`DT4`/`DT5`/`GH1` 为「未实现」的**首要理由**（生产 Factory 不填 `Cmd` 导致 panic）已被 `3db5197` 修复，四项都不再 panic。但**没有一项因此变成「已实现」**——每一项下面都压着独立的第二缺口，其中两个比 panic 更隐蔽（panic 至少会喊出来）。

**spec §4.3 W6 那格的工作量估计需要上调。**

## 1. 端到端复核结果

验法：临时建生产装配测试（`shell.DefaultSecureFactory{OS: shell.OSProcessFactory{}}`，绑 `WithProfile`/`WithSecureProcessFactory`/`WithWorkRoot`），profile 跑两档：allow-all 与 `bootstrap.DefaultOrchestratorProfile()`。

| 工具 | panic？ | 生产可用？ | 结论 |
|---|---|---|---|
| `git_status` | 否 | 部分 | panic 已修；**路径解析仍有缺口** |
| `git_diff` | 否 | 部分 | untracked 误判 binary、staged/unstaged 重复 |
| `run_tests` | 否 | **完全不可用** | **静默假 pass**，比 panic 更糟 |
| `diagnostics` | 否 | 部分 | Go 版本恒空、sandbox 恒 unknown、LSP 恒 0 |
| `github_*` ×4 | 否 | **完全不可用** | guard 工具名不匹配 + 退出码被当数据 |

## 2. 跨工具共享根因：生产 Factory 不继承环境变量 ⚠️ 最高优先级

`internal/shell/factory.go:51/55` 做 `env = netpolicy.PrepareEnv(spec.Env, proxyURL)`，`spec.Env` 是 nil，`PrepareEnv`（`internal/netpolicy/proxy.go:182`）只在输入基础上追加三个代理项。实测 dump 子进程环境：

```
child env = "HTTP_PROXY=http://127.0.0.1:0\nHTTPS_PROXY=http://127.0.0.1:0\nNO_PROXY=\n"
```

**整个子进程环境只有这 3 条。没有 PATH、没有 HOME、没有 GOPATH/GOMODCACHE。**

这是与 `Cmd`/`Wait` **完全同型**的「fake 比生产更完整」问题，而且就在同一批测试里：`internal/tools/git_test.go:60` 的 `realGitFactory` 写的是 `cmd.Env = append(os.Environ(), spec.Env...)`——测试替身继承了环境，生产实现没有。

`netpolicy.ManagedEnv`（`proxy.go:204`，即 `PrepareEnv(os.Environ(), ...)`）就是为此写的辅助函数，**全仓非测试代码零调用**。这是一条 GOV6 型的「只有定义没有调用点」，但签名不是 `With<X>(ctx,...) context.Context`，GOV6 抓不到。

**影响：`run_tests`、`github_*`、以及走 factory 路径的 `shell_run`。**

## 3. 逐项缺口

### W07 `git_status` / `git_diff`

**缺口 1（审计已记）—— 已跟踪路径含空格被截断。** `internal/tools/git.go:270` `parts := strings.SplitN(record, " ", 3)`，随后取 `subFields[len(subFields)-1]` 当 path。实测 porcelain v2：

```
1 .M N... 100644 100644 100644 587be6b4... 587be6b4... a b c.txt
→ 解析出 path="c.txt"
```

rename 记录（`2` 开头）后紧跟一个独立 NUL 字段装原路径，当前解析器把它当独立记录，产出幽灵条目。正解：按 v2 字段数定长切分（ordinary=8 个前置字段，rename=9 且额外吞掉下一个 NUL 字段）。

**为什么测试全绿**：`git_test.go:79` 的 `TestGitStatusParsesPorcelainV2ZWithHostileNames` 只 `os.WriteFile` 不 `git add`，全走 `? <path>` 分支（`record[2:]`，不截断）。**已跟踪分支从未被测过。**

**缺口 2（审计已记）—— untracked 恒判 binary。** `git.go:211` `binary := strings.Contains(res.Stdout, "Binary files") || res.Stdout == ""`。untracked 跑 `git diff -- <path>` 输出为空 → 判 binary。修法：untracked 走 `git diff --no-index /dev/null <path>`（`e.Untracked` 标志已在 `gitNumstatEntry` 里，只是 `gitPatchForFile` 没用）。

**缺口 3（审计未记，新查出）—— staged + unstaged 同一文件出两条。** `collectGitDiffFiles`（`git.go:117-145`）把 `diff --numstat` 与 `diff --cached --numstat` 直接 `append`，不去重。

### DT4 `run_tests` — 静默假 pass

```
run_tests out={"framework":"go","status":"pass","passed":0,...,"duration_ms":17}
RAW exit=1  STDOUT=go: module cache not found: neither GOMODCACHE nor GOPATH is set
```

**缺口 A**：见 §2 空环境。
**缺口 B**：`internal/tools/testrun.go:164` 起的 `parseGoJSON` 只扫 JSON 行数 pass/fail/skip；`runTests`（`testrun.go:80` 附近）从不看 `res.ExitCode`，非 JSON 的 stderr 被 `json.Unmarshal` 静默跳过，`Status` 默认 `"pass"`。构建失败/环境缺失/命令不存在 → 一律报 pass。**即使修好环境这条仍在，是独立正确性 bug。**

审计说的「包级 pass/fail 事件当成用例计数」也仍真（`ev.Test` 为空的包级事件同样计数）。

### DT5 `diagnostics`

**缺口 1（审计未记，新查出）—— `go` 版本恒空。** `diagnostics.go:107` 对三个工具链统一用 `Args: []string{"--version"}`：

```
$ go --version     → flag provided but not defined: -version, exit=2
$ go version       → exit=0
```

`diagnostics.go:109` 的 `res.ExitCode != 0` 分支把 Go 整个跳过。**在一个 Go 项目里，这正好是最该有的那一项。**（`cargo`/`node` 都正常。）

**缺口 2（审计已记）—— sandbox 三字段硬编码。** `diagnostics.go:92` `sandboxProbe` 直接返回 `{Requested:"unknown", Effective:"unknown", Enforced:false}`，注释还写着「等 A1c 暴露 SandboxFromContext + Report」，而两者早已存在。

**缺口 3（审计已记，与 LSP1 同源）—— `open_diagnostics_count` 恒 0。** `diagnostics.go:24` `defaultFileLister.recentFiles` 恒返回 nil；`bootstrap.go:655` 传 `nil`。

**缺口 4（审计已记）—— 五个子项串行。**

### GH1 四个 `github_*`

```
# allowAllProfile
pr_context out=✗ parse GitHub PR: invalid character 'T' looking for beginning of value
comment    out={"id":"To get started with GitHub CLI, please run:  gh auth login\n..."}

# DefaultOrchestratorProfile（出厂默认）
pr_context out=✗ gh: tools: denied: tool "github" not permitted
comment    out=✗ gh: tools: denied: tool "github" not permitted
```

**缺口 1（审计未记，新查出）—— guard 工具名不匹配，出厂 profile 下四个工具全死。** `internal/tools/github.go:115` 的 `ghSpec` 写死 `Tool: "github"`。调用链是双重鉴权：`GuardedTool.Stream` 先用真名 `github_pr_context` 过 `Authorize`（放行），随后 `secproc.Launch` 用 `spec.Tool` 再过一次。而 `"github"` 不在 allow 列表里（列表里是四个真名）。

**GOV5 抓不到这个**——GOV5 比的是「allow 列表 vs 注册表」，而 `"github"` 是**只出现在 spec 里、既不在 allow 也不在注册表**的第三类幽灵名。建议反馈治理层：GOV5 可顺带扫 `SecureProcessSpec.Tool` 字面量。

**缺口 2（审计未记，新查出）—— 退出码被无视，错误文本当数据回喂模型。** `gh` 未认证、退出码非 0，`runGitHubComment` 却把 stderr 原样塞进 `{"id": ...}` 返回，模型会以为评论发成功了。四个函数没有一个检查 `res.ExitCode`。GH1 验收标准里的「未认证明确降级」目前 0 实现。

**缺口 3**：见 §2 空环境。附带证据：跑完测试后工作树多出 `internal/tools/.local/state/gh/device-id`——HOME 缺失导致 `gh` 把状态写进 CWD，**污染仓库**。

**缺口 4（移交 W5/S09）—— 死代理挡住一切子进程出网。** `bootstrap.go:918` 传 `Policy: networkPolicy, ProxyURL: ""`，factory 注入 `HTTP_PROXY=http://127.0.0.1:0`。实测：

```
$ env HTTP_PROXY=http://127.0.0.1:0 gh api rate_limit
  → proxyconnect tcp: dial tcp 127.0.0.1:0: can't assign requested address
$ gh api rate_limit    # 无代理时正常
```

审计的 `A1 S09` 自己就把这个死代理判为缺陷。**W6 修完自己三条，`github_*` 仍到不了真实 GitHub，直到 W5 的 S09 起一个真代理。**

建议：W6 不碰代理（W5 地盘），改为修工具名 + 退出码降级 + 环境继承，用 **PATH 上的 stub `gh`（生产 factory + 真子进程）** 做端到端验收，把「真实出网」明确记为 W5 依赖。GH1 翻 `done` 时必须在计划与 `ghSpec` 注释里写死这条残留依赖，不能藏。

## 4. SPEC-TOOLIF — 8 个 `agent_*` 的 `DefaultTimeout()==0`

确认**恰好 8 个**。`internal/tools/agent.go` 中全部第 4 个位置参数传 `0`：

```
128 agent_spawn   138 agent_wait    144 agent_result   149 agent_send_input
156 agent_resume  162 agent_assign  168 agent_cancel   173 agent_list
```

消费点 `internal/tools/guard.go:213` `runCtx, cancel := context.WithTimeout(ctx, g.timeout)`。

**实测确认是真 bug**：构造 `timeout=0` 的 GuardedTool，streamFunc 第一行 `ctx.Err()` →

```
timeout=0 → out="✗ context deadline exceeded"
```

八个工具在真实 turn 里一调就废。且 `InvokableRun` 把错误当**工具结果**回喂模型，模型只会看到 "context deadline exceeded" 然后重试、再失败——**不崩，只静默空转**。

`guard.go:163` 的 `NewGuardedTool` 对 timeout 无任何校验。

### 超时设计建议

1. **需要哨兵。** `0` 在 Go 里最自然的读法是「未设置」，但 `WithTimeout` 解释成「立刻过期」——这个语义错配就是根源。不能塞巨大常数糊过去，那只是把 bug 变成难复现的 bug。
2. 加导出哨兵 `tools.NoTimeout`（建议 `= time.Duration(-1)`；负值在 `WithTimeout` 下同样立刻过期，所以**必须显式分支**），`Stream` 在 `g.timeout == NoTimeout` 时改用 `context.WithCancel(ctx)`——语义是「只受 turn ctx 约束」，这是诚实且真实存在的上界。
3. `NewGuardedTool` 对 `timeout == 0` **构造期 panic**。零值不再合法，要无界必须显式写 `NoTimeout`。
4. **只有 `agent_wait` 该用 `NoTimeout`**：`streamAgentWait`（`agent_lifecycle.go:109-113`）已支持可选 `timeout` 入参，为 0 时 `Manager.Wait`（`manager.go:499`）阻塞到终态——外层再套死超时是错的。
5. 其余 7 个都是内存 registry 操作，给 `30*time.Second`。配一条 GOV7 型断言：从注册表取每个工具的 `DefaultTimeout()`，**为 0 即硬失败**——seam 可复刻 W0 的 `App.ToolNames` 做法（`bootstrap.go:783-793`），加 `App.ToolTimeouts`。

## 5. 其余各项 delta

- **`SPEC-TOOLIF`**（亲验）：骨架真实且已接线；**唯一实缺就是 8 个 timeout=0**。审计提的「TUI 直接消费同一 channel」在 client/server 分离下本就不可能字面实现，属纸面差异，不修。
- **`T11` web_search**（亲验）：`web.go:218` 只认 `class="result-link"`、`URL` 写死空串、`Snippet` 全文件从未赋值；`web.go:29` 的 `lite.duckduckgo.com/lite` 实抓已变反爬 `anomaly-modal` 页，**0 命中**。已实测可行替代：`POST https://html.duckduckgo.com/html/` 稳定返回 10 条，`result__a`/`result__snippet`/`result__url` 三个 class 齐全。它走**进程内 http.Client + `netpolicy.NewTransport`**，不走子进程，**不受死代理影响，可在 W6 内独立收口**。另缺 `domains`/`since`/`ref_id` 三参数。
- **`V13` 结构化 review**（亲验）：`review.go:22` 流水线是真的；缺口三条——(a) `agent.go:183` 参数只有 `diff` 字符串，三种 base 一个没有；(b) `review_decode.go:10` 用 `Rule` 顶替规划的 `suggestion`，severity 无分级判据，clean 只是空数组无显式标记；(c) `cmd/yanshi/pr.go:50` → `tools.RunReviewHeadless(ctx, diff)`（`review.go:85`）传裸 ctx，**从未绑 SubAgentRunner**，必然命中 `review.go:25` 的 `"review requires a bound sub-agent runner"`。另 `commands.go:429` 的 `/review` 只是 prompt 糖，不调工具。
- **`LSP1`**（仅 diagnostics 侧亲验）：`diagnostics.go:24` + `bootstrap.go:655` 确认 count 恒 0。审计另称 `DefaultLanguages` 只有 go+python、无标志文件确认、诊断经 JSON 字段进 transcript 而非 activity——**这三条未逐行复核，动工前需重验**。
- **`C13` `/mcp`**（未亲验）：审计称五子命令齐全且真调 Manager，缺 resolved 配置路径/command|URL/timeout/resources/prompts 渲染，enable/disable/reload 只改内存不回写 config.yaml；`proto.NewMCPList` 死代码。**待重验。**
- **`MCP1` palette**（未亲验）：审计称启动不主动发 `list_mcp`、分组头 `── srv ──` 被 `updatePalette` 的 `HasPrefix` 过滤掉、disabled/failed 无标灰且 `Manager.Disable` 清空 toolMap。**待重验。**
- **`V16` MCP Client**（未亲验）：审计称四缺口——resources 不聚合（`ServerStatus.Resources` 恒 nil）、prompts 零实现、`internal/mcp/health.go` 完整但 `buildMCPManager` 只调 `StartAll` 从不 `SetHealthConfig`/`StartHealthLoop`（工具桥也用 `CallTool` 而非 `CallToolRetry`）、OAuth 半接。**典型的「零件造好总装没接」，与 GOV4 同型，待重验。**

## 6. PR 分组（11 项 → 6 个 PR）

**PR-B 必须先落**（含跨工具共享的环境根因）；PR-C/PR-D 依赖它。PR-A/PR-E/PR-F 可并行。

| PR | 覆盖项 | 内容 | 依赖 |
|---|---|---|---|
| **PR-A 工具超时契约** | SPEC-TOOLIF | `tools.NoTimeout` 哨兵 + `Stream` 分支 + 构造期拒 0 + 8 个赋值 + GOV7 型断言（`App.ToolTimeouts`） | — |
| **PR-B 子进程环境 + git 正确性** | W07 | Factory 改用 `os.Environ()` 基底（配**生产实现**测试）+ porcelain v2 定长解析 + `git_diff` 去重 + untracked 走 `--no-index` | — |
| **PR-C run_tests + diagnostics** | DT4, DT5 | 退出码/构建失败不得报 pass + 用例级计数 + `go version` 探针 + sandbox 真实上报 + 子项并发 | PR-B |
| **PR-D github** | GH1 | `ghSpec` 改真名 + 退出码结构化降级 + issue 侧两工具 + 大 body artifact + stub-`gh` 端到端测试 | PR-B |
| **PR-E web_search + review** | T11, V13 | 换 `html.duckduckgo.com` + 真解析 + domains/since；review 三种 base + severity 分级 + clean 显式 + `yanshi pr` 绑 SubAgentRunner | — |
| **PR-F LSP + MCP 三项** | LSP1, C13, MCP1, V16 | LSP 语言表/标志文件/`recentFiles` 生产实现；MCP 帧扩字段 + config 回写 + 启动 `list_mcp` + 分组头过滤 + `StartHealthLoop`/`CallToolRetry` 接线 | — |

PR-F 偏大但集中在三片，跨 PR 拆会互相 rebase。**C13 与 MCP1 共用 `mcp_status` 帧结构，必须同 PR**，拆开会连续两次改同一个 wire 结构。压力大时可切成 PR-F1（LSP1）/ PR-F2（MCP 三项）。

## 7. 审计 / spec / CLAUDE.md 的过时与错误

1. **审计对四项的「未实现」理由已全过时**（panic 已修）。结论碰巧还站得住，理由已不成立。
2. **`run_tests` 的空环境问题审计完全没记**——比 panic 更根本，同时废掉 `run_tests` 与 `github_*`。
3. **`ghSpec` 的 `Tool: "github"` guard 名不匹配，审计与 spec 都没记。**
4. **`diagnostics` 的 `go --version` 参数错，审计没记**，且只错在 Go 这一个上。
5. **`git_diff` 的 staged/unstaged 重复，审计没记。**
6. ⚠️ **`CLAUDE.md` 的「secproc 是仓库唯一的 `exec.CommandContext` 调用点」是错的。** 实测非测试代码有 20+ 处 `exec.Command*`：`internal/lsp/manager.go:191`、`internal/mcp/manager.go:90`、`internal/acp/spawn.go:148`、`internal/skills/install.go:77`、`cmd/yanshi/pr.go:67/108` 等。`secproc.go` 包头的原话限定了「spawns an **untrusted** program」，比 CLAUDE.md 准确；**CLAUDE.md 该跟着收窄**。顺带：`cmd/yanshi/pr.go:67` 用 `osexec.CommandContext(ctx, "gh", ...)` 直接起 `gh`，**完全绕过 guard**，与 `github_*` 是两条独立的 gh 路径。
7. **spec §4.3 W6 那格暗示「复验大概率通过」**——实测四组全部还有独立缺口，**工作量估计需上调**。
8. **`netpolicy.ManagedEnv` 全仓非测试零调用**——GOV6 型盲区（签名不匹配）。可作为治理层第二条线索。

## 8. 未完成的核验

`LSP1` 与 `C13`/`MCP1`/`V16` 的逐行复核**未完成**，§5 中这四项的 delta 基于审计原文 + 部分交叉验证。

**动工前必须按前四项同样的标准重验**——考虑到审计对 W07/DT4/DT5/GH1 的理由**全部**已过时，对这四项直接采信审计的风险不低。
