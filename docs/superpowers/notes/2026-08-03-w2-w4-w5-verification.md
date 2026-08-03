# W2 / W4 / W5 核验报告（合并）

> 2026-08-03 实测。所有行号为亲自核对的当前值。

---

# W2 预算闸门

## 1. `goalloop.Budget` 非测试构造点：**恰好 2 处，都只设 `MaxIterations`**

- `cmd/yanshi/main.go:723`（fake 路径）`Budget: goalloop.Budget{MaxIterations: *maxIters},`
- `cmd/yanshi/main.go:774`（真实 T3–T4 路径）同上

`Budget` 定义在 `internal/agent/goalloop/types.go:58`，`MaxTokens int` 在 `:60`。**`MaxTokens`/`SpentTokens` 恒为零值。**

## 2. `overBudget()`：`internal/agent/goalloop/loop.go:54-55`

```go
// :52-53 doc: A zero MaxTokens disables the token budget entirely
func (l *Loop) overBudget() bool {
	return l.cfg.Budget.MaxTokens > 0 && l.spent() > l.cfg.Budget.MaxTokens
}
```

两个调用点 `:110`（before iteration）、`:126`（after plan）。`spent()` 在 `:45`，sink 优先——**计量侧是真的**。但因 §1，两处检查**在生产路径恒为 dead branch**。

## 3. `-max-tokens` / config 项确实不存在

```
grep -rn "max-tokens\|max_tokens\|MaxTokens" --include="*.go" cmd/yanshi/ internal/config/   → 零命中
grep -n "max_tokens\|budget\|goal" config.example.yaml                                        → 零命中
```

`yanshi goal -h` 实际只有 7 个 flag：`-agent -config -fake-model -goal -max-iters -tier -workdir`（`-max-iters` 在 `main.go:661`）。

⚠️ **`internal/config/config.go:52-108` 的顶层 `Config` 没有任何 goal/goalloop/budget 段** —— 新增 config 项是「开一个新 section」，不是「加一个字段」。审计的修复建议低估了这部分工作量。

## 4. G03 tier bug —— **已复现**

代码差异（`cmd/yanshi/main.go`）：
- fake 路径 `:718-724`：`goalloop.Config{Planner, Implementer, Evaluators, Judge, Budget}` —— **缺 `Tier`，也缺 `Sink`**
- 真实路径 `:769-777`：**+ `Sink: loopSink`（:775）+ `Tier: resolvedTier`（:776）**

`Config.Tier` 在 `loop.go:25`，唯一消费点 `loop.go:171 EscalationHint(l.cfg.Tier)`；`tier.go:226-232` 是 `next := t + 1`。零值 = `TierQuickFix`(0) → next = `TierStandard`(1)。

**实跑**（`/tmp/yanshi_w2 goal -fake-model -tier t3 -max-iters 1 -goal "build a platform"`）：

```
[tier: team] path: loop
[iter 1] Plan: 1 steps, 1 tests
[iter 1] Implement: error: fake implementer: deliberate failure 1/1
[iter 1] Evaluate: 1 verdicts
[iter 1] Judge: gaps: counter: call 1 < passAt 2
decision: complete=false, summary=max iterations (1) reached without completion;
          consider escalating to tier standard (-tier t1)
```

第 1 行正确解析为 `tier: team`(T3)，但末行建议「升级到 tier standard (`-tier t1`)」——**向下降级的荒谬提示**。正确应为 `-tier t4 / autonomous`。

⚠️ **新发现（审计只提了缺 `Tier`）**：fake 路径**同时也缺 `Sink`**，使 `loop.go:46` 的 sink 分支在 fake 下走不到。这意味着 **fake 路径无法用于验证预算闸门**——W2 若想加冒烟测试需一并补 `Sink`。

## 5. LEAK3 —— 管道完整，但**数据源是死的**（比审计判断更硬）

Go 侧链路逐点核实完整：`acp/client.go:250 case "usage_report":` → `parseUsageReport`（`:205`，defer recover，双格式尝试，全零返回 nil 避免污染 sink）→ `Event.Usage`（`client.go:24`）→ `types.go:186-190 Usage` → `implementer.go:385 usageForwarder()`（`:346`/`:375` 两条 Prompt 路径都传入，`:390` nil 检查，`:393-396` 映射进 `UsageSink`，sink 字段 `:255` 由 `:465` 从 `ACPImplementer.Sink`（`:172`）注入）。

### ⚠️ 但 `usage_report` 不是 ACP 协议里的东西

核对本机 ACP 官方 TS SDK（`@agentclientprotocol/sdk/dist/schema/types.gen.d.ts`），`sessionUpdate` 判别式的**全部 11 个合法取值**：

```
user_message_chunk, agent_message_chunk, agent_thought_chunk, tool_call,
tool_call_update, plan, available_commands_update, current_mode_update,
config_option_update, session_info_update, usage_update
```

用量变体叫 **`usage_update`**（该文件 `:2533`），`usage_report` 在 SDK 中**零命中**。

**所以 `client.go:250` 是永不命中的死分支**，真实 agent 发的 `usage_update` 会落到 `client.go:275` 的 `default:` 被静默丢弃。

**codex/claudecode 是否真发——无法确定**（`reference/codex/codex-rs/` 下没有 acp 目录，claudecode 无本地源码）。但这已不重要：**无论它们发不发，字段名对不上，进 sink 的就恒为 0。这是我方的实现 bug，不是外部不配合。**

### 判断：诚实计零 + 告警，**绝不做兜底估算**

1. **先修 bug 再谈兜底**。根因是判别式写错（`usage_report` → `usage_update`）。没改这个字符串前讨论兜底是本末倒置——改完后 usage 很可能真的流进来，估算反而污染真实数据
2. **估算会把硬停闸门变成不可信的闸门**。预算的价值在「到点必停」；用字符估算驱动会中断用户长任务的硬停，误差方向不可控：高估导致任务被无故砍断，低估导致闸门形同虚设
3. **零 + 告警可观测可归因**。turn 结束时若 ACP usage 为零而 turn 明显非空，打一条 warn，用户立刻知道预算覆盖范围有洞，可自行退回 `-max-iters` 兜底。符合 guard 一贯的 fail-closed / 显式化风格，也符合「严禁占位符与伪实现」——估算数字本质是伪数据

若日后确认某 CLI 永不上报，正确做法是在该 agent 的 adapter 层显式标记 `usage: unsupported`，让 loop 用**迭代预算**约束它。

## 6. 修复方案要点

- **CLI flag**：`-max-tokens int` 加在 `main.go:661` 旁，usage 文案 `"maximum total LLM tokens across the goal run (0 = unlimited)"`；同时填进 `:723` 与 `:774`
- **config key**：仓库无 goal 段，需**新增顶层 section**。建议 `goal.max_tokens`（`Config.Goal.MaxTokens`），新 struct `GoalConfig` 插在 `Batch`（`config.go:60`）附近；顺带把 `goal.max_iterations` 一并纳入避免以后再开一次。flag 显式给出时覆盖 config
- **默认值 `0`（不限）写进 struct**，`config.example.yaml` 里以**注释掉的示例**给务实数值（`# max_tokens: 2000000`）并说明「0 = 关闭」。非零默认会让现存用户在无预警下被新硬停打断，属破坏性变更
- **文档门禁必做**：`gendocs -config`（config key）与 `gendocs -help-all`（flag）**必定产生 diff**，四个生成器全跑最省事。GOV3 要求新导出符号（`GoalConfig`、`MaxTokens`）**必须带 doc 注释**

## 7. 审计的过时/错误

**行号基本没漂**（`git log --since=2026-07-31` 只有一条 gofmt）。但**内容层面有三处一审/二审自相矛盾**：

1. **G02 的「实测缺口」段（`:440`）仍是一审旧文本**，写「唯一残余：仅 CLI 入口缺一个 flag」，与紧随的二审段（`:441`，「预算停止在发行二进制里 100% 不可达」）**自相矛盾**。照「实测缺口」估工会低估工作量。**以二审为准**
2. **LEAK3 同样**：`:744` 写「实现完整」，`:745` 二审说「数据源是虚构的，进 sink 的 token 恒为 0」。以二审为准
3. **G03 的「实测缺口」写「无差异」（`:446`）**，而二审（`:447`）明确复现了 Tier 丢失——本次已独立复现，**二审正确，「实测缺口」段错误**

唯一明确行号漂移：`acp/types.go` 的 `Usage` 结构体审计写 `:181-188`，实际 `type Usage struct` 在 **`:186`**，字段 `:187-189`，收尾 `:190`。

---

# W4 压缩正确性

## 1. 量纲错配的确切位置

**存值** `internal/llm/eino/compacting.go:143`：
```go
c.lastCompactTokens = res.TokensAfter
```
`res` 是 `ctxcompact.Run` 返回，`TokensAfter = EstimateTokens(out)`（`internal/ctxcompact/run.go:37`），即**压缩后**输出切片的 token 数。

**比值** 同文件：
- `:158` `tokens := ctxcompact.EstimateTokens(msgs)`（`shouldCompact` 内，`msgs` 是 ADK 传进来的完整历史）
- `:174` `if c.inCooldown(tokens) {`
- `:192` `tokenCool := c.CooldownTokens > 0 && lastT > 0 && tokens-lastT < c.CooldownTokens`

### 为什么量纲不同（根因）

`lastCompactTokens` 是 CompactingModel **输出给 Inner** 的精简历史大小；`tokens` 是 **ADK 下一次迭代传进来** 的历史大小。

关键在于**压缩结果从不写回 ADK state** —— `Generate`/`Stream`（`:104-118`）只把 `res.Messages` 传给 `c.Inner`，返回值不含消息列表，ADK 的 `state.Messages` 只做 append。所以下次迭代进来的仍是完整未压缩历史。

`tokens - lastT` 实际等于「未压缩历史 − 压缩后历史」，是一个**恒定巨大的正数**，而非设计意图的「自上次压缩以来的增长量」。

**对照：pre-turn 路径没有这个问题** —— `ws_compaction.go` 把 `MaybeCompact` 的返回直接赋回 `cs.history`，下次估算的就是压缩后历史，量纲自洽。

## 2. `CooldownTokens` 取全局窗口

`internal/bootstrap/bootstrap.go`：
- `:823` `ContextWindow: cfg.Compaction.ContextWindow,`
- `:825` `CooldownTokens: int(cfg.Compaction.CooldownFraction * float64(cfg.Compaction.ContextWindow)),`

取**全局回退** `cfg.Compaction.ContextWindow`（`internal/config/config.go:517-518` 兜底 256000）。

per-provider `context_window` 走**完全独立**的通道：`einollm.BuildProviders` 返回 `windows` map → `apihttp.CompactionConfig.ProviderWindows` → `ws_compaction.go:422 contextWindowFor`。这条通道**只服务 pre-turn/WS，从未流向 orchestrator**。

mid-turn 确认：`orchestrator.go:234 ContextWindow: cc.ContextWindow`（`wrapCompaction`，`:227-239`）。`runnerFor`（`:381`）只拿到 `model.BaseChatModel` 指针，`TurnOpts`（`:490-499`）**没有模型名字段**——这一层根本没有信息去查 per-provider 窗口。

**后果（128K 模型）**：cooldown 阈值 12800 而非 6400；threshold 门在 204800 而非 102400；hard-force 在 243200 而非 121600 —— **在实际窗口的 1.9 倍，等于永不触发**。

⚠️ **审计行号已漂移**：两处（`:236`、`:404`）写 `bootstrap.go:794`，当前实际 **`:825`**。

## 3. cooldown bug —— **已复现**

临时探针（`hist()` 每次返回同一份未压缩历史，模拟 ADK ReAct 迭代重新递交完整历史）：

```
iter1: did=true innerCalls=1 storedLastCompactTokens=94
iter2: incomingUncompactedTokens=228  lastCompactTokens=94  growth=134  CooldownTokens=100
iter2: inCooldown=false (want true)
iter2: did=true (want false) innerSummarizeCalls=1 (want 0)
BUG REPRODUCED: re-compacted inside cooldown window
```

参数 `Threshold=0.5, ContextWindow=400, KeepRecent=2, CooldownTokens=100, HardForceFraction=0.95`（现有测试同款）。**历史一字未变、增长为 0，第二次迭代却又压缩了一次。**

**出厂默认下更严重**：window 256000、threshold 0.8 → 204800 触发；压缩后 `lastCompactTokens` 约几千到两万；`CooldownTokens = 0.05 × 256000 = 12800`。`204800 − 20000 = 184800 ≫ 12800`，**cooldown 永远不成立，每一次 ReAct 迭代都会重跑一次完整的 summarization turn**。这就是「主功能完全失效」的准确含义。

### ⚠️ 现有测试为什么绿：它主动掩盖了 bug

`internal/llm/eino/compacting_test.go:310 TestCompactingModel_CooldownDefersReCompact` 在第一次 `maybeCompact` 后**手工覆写** `cm.lastCompactTokens = 180`（`:336`），把它人为拉到与未压缩历史同一量纲，再断言 cooldown 生效。**它从头到尾没有断言 `maybeCompact` 实际存进去的是什么。**

W4 必须把它改成不覆写、直接断言存值语义。（`:356` 还有第二处同样的覆写。）

## 4. `ctxcompact` 现有测试覆盖

14 个测试文件、~1500 测试行。**Fuzz target：0 个**（全仓唯一 fuzz 是 `internal/guard/glob_test.go:62 FuzzMatchGlob`）。

CI 的 `fuzz-seed` job（`ci.yml:126-143`）在「hard gates」块下、**无 `continue-on-error`**，已是硬门禁；其 guard 是 `go test -list 'Fuzz.*|TestProperty.*'` 后 grep —— ctxcompact 有 8 个 `TestProperty_*`，guard 通过，job 真跑。**W4 新增属性测试会直接进硬门禁。**

现有 8 个属性测试：

| 属性 | 位置 | 覆盖 |
|---|---|---|
| pin 是输出前缀、指针逐字相等、索引升序 | `plan_property_test.go:72` | 好（比的是**指针**，逐字保留有保证） |
| tool_call/result 配对 fixpoint | `plan_property_test.go:102` | ⚠️ **有洞**：`:112` 的 `pinnedSetIsConsistent` 前置守卫在历史以 summary sentinel 结尾时 `t.Skip`，实测 50/30/30 轮里 **skip 27 次（约 22%）**——恰好是最容易出问题的分支被绕开 |
| fixpoint 幂等 | `plan_property_test.go:147` | 好 |
| fixpoint 修复人为损坏 | `plan_property_test.go:175` | 好 |
| 每次 summary 调用输入 ≤ ModelWindow | `summarize_property_test.go:41` | 好，4 个窗口参数化 |
| 空 summary 不产出空消息 | `summarize_property_test.go:80` | ⚠️ 语义与规划**相反**（规划要「空串→Run 报错」，实现断言「不报错」） |
| 不二次压缩 | `run_property_test.go:9` | 好 |
| Run 单调减少 token | `run_property_test.go:49` | ⚠️ **只有单一固定种子** `rand.NewPCG(123,0)`、n=40 一组 |

### 尚未被测的不变式（PROP1 缺口清单）

1. ⚠️ **组装结果永不超窗 —— 完全未测**。现在只保证「每次 summary *调用* 不超窗」，不保证 `Assemble` 的**产物**（pin 集合 + summary）≤ ModelWindow。pin 规则（尾部 + 全部 user 原文 + working-set 路径 + error/diff 标记）在长会话里可以把大半历史 pin 住，**产物超窗是可达状态，且 `Run` 对此没有任何断言或降级**。这是最有价值的新属性
2. `EnforceToolCallPairs` 输出无孤儿（在 skip 掉的 22% 分支上）——应把 `t.Skip` 换成「对短路分支断言正确的弱不变量」
3. 单调性的种子多样化 —— 改成种子表 × 多 n × 多 KeepRecent
4. `Plan` 短路分支（历史已以 summary 结尾）的不变式 —— 目前只有 `plan_test.go:50` 一个固定用例
5. **Fuzz target** —— 建议至少一个 `FuzzEnforceToolCallPairs` 或 `FuzzPlanAssemble` 配种子语料，与 FUZ1 原始验收对齐

## 5. 修复方案

### (a) 量纲统一 —— 只改 `compacting.go`，**不动 `ctxcompact`**，pre-turn 零影响

- 存值改为 `c.lastCompactTokens = res.TokensBefore`（`:143`）。语义变成「上次触发压缩时未压缩历史有多大」，与 `:158` 同量纲。`tokens - lastT` 于是真正表示增长量
- 顺手把「是否压缩过」的判定从 `lastT == 0` 哨兵（`:189`、`:192`）换成显式 `hasCompacted bool`。`TokensBefore` 恒非 0，靠 0 判空本就脆
- `ctxcompact.Run` 的 `Result` **已同时返回 `TokensBefore`/`TokensAfter`**，无需新增字段；`ctxcompact` 一行不动
- 测试侧必须删掉 `compacting_test.go:336` 与 `:356` 的手工覆写，改为「连续两次 `maybeCompact` 同一份历史 → 第二次 `did=false`、`inner.calls` 不增」，并单独断言存进去的值等于 `EstimateTokens(输入)`

### (b) `CooldownTokens` 取 per-provider 窗口（窗口才是根因，一次修到位）

- `orchestrator.CompactionConfig` 加 `ProviderWindows map[string]int` 与 `CooldownFraction float64`（**取代**预乘好的 `CooldownTokens`），bootstrap 把 `providerWindows`（已作为局部变量存在、已传给 `apihttp.Config`）一并传给 `orchConfig.Compaction`，删掉 `:825` 的乘法
- `orchestrator.TurnOpts` 加 `ModelName string`；WS 层有 `cs.model`，SSE 层有 `req.Model`。`runnerFor` 增加 `modelName` 形参，用与 `contextWindowFor` 相同的查表规则解析，未命中回退 `cc.ContextWindow`
- `wrapCompaction` 改签名接收解析后的 `window int`，内部算 `CooldownTokens`、设 `ContextWindow` —— **threshold、hard-force、cooldown 三个门自动全部用同一个正确窗口**
- `runners` 缓存键仍用 model 指针即可（指针↔名字在 `BuildProviders` 里 1:1）；把 `modelName` 加进 `runnerCacheKey` 是零成本保险
- ⚠️ **DRY 提示**：`contextWindowFor` 住在 `internal/api/http`，orchestrator 依赖它会**破坏 GOV1 分层**。正确做法是复用 `config.ContextWindowFor`（`internal/config/config.go:642`），或在 orchestrator 内写三行等价私有函数 —— **不要让 orchestrator import `api/http`**

## 6. 建议新增 ADR-0011

引入一条新的可被违反的承重约束：**「mid-turn 压缩的所有 token 会计必须以『ADK 递交的未压缩历史』为统一量纲，因为压缩结果按设计不回写 ADK state」**。且 ADR-0006 的 Consequences 第 25 行有一句需被收窄的旧断言。

同步必改：
- `docs/compaction.md` —— **通篇零处提到 cooldown**（`grep -i cooldown` 零命中），而 `config.example.yaml:48-50` 已暴露三个键。需新增一节讲三门（threshold/cooldown/hard-force）的关系与量纲
- `docs/adr/0006-*.md:25`、`CLAUDE.md` 压缩段 —— 收窄「`/model` 切换自动用新窗口」
- `docs/feature-status-audit.md` —— 行号 794 → 825

## 7. ⚠️ CLAUDE.md 与 docs 的确证错误

**「`/model` 切换自动用新窗口」在 mid-turn 路径确实不成立。确认为文档错误。** 三处同源：

1. **`CLAUDE.md`** 编排器→压缩段
2. **`docs/compaction.md:88`** ——「`/model` 切换自动用新窗口——**因为 `CompactingModel` 按 model 指针缓存**」。**这句的因果推理是错的**：按指针缓存确实会为新模型建新 `CompactingModel` 实例，但 `wrapCompaction`（`orchestrator.go:227-239`）给新实例填的 `ContextWindow` 仍是同一个全局值。**换了实例，没换窗口。** 同段说「查询走 `BuildProviders` 返回的 `windows` map」——这**只描述 pre-turn/WS 路径**，却写在不区分路径的通用小节里
3. **`docs/adr/0006-*.md:25`** 同样断言

**准确表述**：per-provider `context_window` **目前只在 pre-turn / WS / `/compact` 路径生效**；mid-turn `CompactingModel` 的 threshold、hard-force、cooldown 三门一律使用全局 `compaction.context_window`（默认 256000）。修完 §5(b) 后这三处才能改成现在写的样子。

**另一处 CLAUDE.md 已过时**：「`fuzz-seed` 目前是 `continue-on-error` 的软门禁」—— W0 已收紧，`governance` 与 `fuzz-seed` 两个 job 现在**都是硬门禁**。

---

# W5 安全底座

## 1. 四项 delta

| id | 已有 | 差什么 |
|---|---|---|
| **A1/S06** execpolicy | lexer/parser/规则引擎均真，已接 `guard.checkShell`（`guard.go:292`），profile 有 `Rules`（`profile.go:107`） | **deny 规则可被平凡绕过**（实测）；出厂配置零 `rules:` 示例，能力不可发现 |
| **A1/S07** 持久审批 | Match/Record/List/Revoke 全真（`manager.go:67/102/136/149`），已接热路径（`permctx.go:312/340/352`）、WS `/permissions` 可查可撤（`ws.go:1060-1089`） | 生产代码从不写 `ExpiresAt` → 「过期」运行时恒假；`Scope.Prefix` 语义是全量 argv 精确相等、非前缀，且无对应回归测试 |
| **A1/S09** 网络隔离 | `Policy` deny-wins + fail-closed（`policy.go:49/77`）；`PolicyDialer` resolve→recheck→pin（`proxy.go:40-74`）；`Proxy` 已写好含 per-hop 重定向复检（`proxy.go:105-170`）；`PrepareEnv` 大小写无关剥离继承代理变量（`proxy.go:182`） | **代理从未启动**（`bootstrap.go:921 ProxyURL: ""`），子进程拿到假 URL `http://127.0.0.1:0`（`shell/factory.go:47-51`）——**是黑洞不是策略**；`Proxy` 拒绝 CONNECT（`proxy.go:145-147`）→ 子进程 HTTPS 全断；netpolicy 生产代码 slog/audit **零命中** |
| **D3/S10** secrets | `Redactor`/`SafeLogger`/`SafeOutput` 真实（`secrets.go:97/165/217`）；keyring 按 `nokeyring` 分文件双实现完整；file backend AES-256-GCM 端到端可用 | **脱敏注册链断裂**（见 §5） |

## 2. S06 绕过路径 —— 已实测

临时探针，规则表 = `deny{program:go, prefix:[test], deny_flags:["-tags=e2e_real"]}` + `allow{program:go, prefix:[test]}`（profile 必须给 `Tools.Allow=["shell_run"]`，否则先被 tools 维度 fail-closed 拦掉——**这本身验证了 ADR-0003**）：

```
go test -tags=e2e_real ./internal/acp      -> HardDeny  rule="no-real-e2e"
go test -tags e2e_real ./internal/acp      -> Allow     rule="go-test"     ← 绕过
go test --tags=e2e_real ./internal/acp     -> Allow     rule="go-test"     ← 绕过
go test -tags='e2e_real' ./internal/acp    -> HardDeny  rule="no-real-e2e"
go test -tags=e2e_real,foo ./internal/acp  -> Allow     rule="go-test"     ← 绕过（审计未记录）
```

根因 `policy.go:135-147` 的 `containsAny`：只认 `arg == flag` 或 `strings.HasPrefix(arg, flag+"=")`。因此 (a) 空格分隔、(b) 双横线、(c) 逗号追加 tag 列表全部漏网。deny 命中失败后 `policy.go:78-80` 走 `continue`，落到后面的 allow 规则 → 放行。

**这恰好击穿 `policy.go:13-18` doc 自己标榜的 `no-real-e2e` 招牌用例。**

新发现：`-tags='e2e_real'` 被拦住，因为 lexer 会剥引号——说明引擎并非全无归一化，只是 flag 比对这一处没做。

**验收逐条**：识别程序/参数/管道/重定向 ✅；规则可解释 ✅（`RuleID`/`Justification` 一路透到 `Decision`）；**已知绕过样例有回归测试 ❌**。

附带：`&&`/`||` 是解析后整体 `hard_deny("control-token")`（`policy.go:62-64`），不是设计描述的「分段判定」，与 guard 元字符 HardDeny 叠加属一致的深度防御，只是措辞出入，**不构成安全缺口**。

## 3. S07 缺口

**已达标**：三档 TTL（`types.go:25-29`）、`Scope`（`types.go:47-54`）、per-session 内存 + per-process 持久化（COW 到 KV，持久失败不改内存态，`manager.go:120-127`）、五种审计事件经 `AuditBus` 扇出（`manager.go:214/241`，`bootstrap.go:897-898`）、`/permissions` 列表与撤销。**来源、命中可审计、可查看撤销三条验收已满足。**

**差两条**：
1. **过期恒假**。`permctx.go:340` 与 `:352` 是全仓仅有的两个生产 `Record` 调用点，构造的 `approval.Rule` **都不设 `ExpiresAt`**。`manager.go:181-192` 的 `expireLocked` 实现正确但永远遇不到非零值
2. **「前缀规则」这一档不存在**。`manager.go:71`/`:85` 用 `reflect.DeepEqual(r.Scope, scope)`，而 `scopeFromAction`（`permctx.go:153-172`）把整条 argv 塞进 `Scope.Prefix`。**既没有前缀绕过面，也没有前缀能力**

## 4. S09 决定：**Option B**（只做不依赖沙箱的部分，其余移交 S1）

1. 验收四条里有三条半（host 规则生效、DNS 不绕过、重定向不绕过、决策入审计）完全落在 `netpolicy` 的 dial/proxy 层，**零 OS 支持需求**；只有「任意子进程的未授权连接失败」需要内核强制
2. **当前真正的缺陷不是「没有沙箱」，而是接线谎报**：代理写好了却从不启动（`bootstrap.go:921`），fallback URL `http://127.0.0.1:0`（`factory.go:49`）是个**伪装成强制的黑洞**——它让 HTTP 客户端型子进程连不上网，看起来像在执行策略，实际既不放行合法流量也不产生任何决策记录。这是现在就该修的 bug，无论 S1 怎么做都得修
3. `netpolicy` 生产代码零审计发射，加审计是纯增量、无沙箱依赖
4. Option A 要把强制建在 `sandbox.Sandbox.Prepare` 上，而四个 adapter 的 `Prepare` 全是文档化 no-op（`sandbox/types.go:87-95`，`Effective` 恒 `DegradedHostGuard`，`types.go:46`）——**写在这上面的东西没有可观测行为可断言，测试写不出来**，且 S1 必定重做
5. ⚠️ **Option B 有一个必须一并解决的硬阻塞**：`Proxy` 明确拒绝 CONNECT（`proxy.go:145-147`），一旦真启动代理，所有需要 HTTPS 的子进程（含 ACP 拉起的 codex/claudecode 调自己的 LLM API）会**立刻全断**。CONNECT 的策略化实现同样不需要沙箱（从 CONNECT 请求行取目标 host 跑 `CheckHost`，通过后盲隧道，不做 MITM），**属于 B 的范围且是 B 的先决条件**

### 移交 S1 的精确边界

- seccomp（Linux socket 调用族）/ Seatbelt（macOS network deny）/ AppContainer（Windows network capability）——即「无视 `HTTP_PROXY`、直接 `connect()` 或裸 socket 的子进程被阻断」
- `CapabilityReport.Effective` 脱离 `DegradedHostGuard`，及 `bootstrap.go:880-882` 那条诚实降级 stderr 警告的移除
- `doctor` 的 `checkSandbox` 真实探测（spec 已归 W7，与 S1 交叉，需协调）

### ⚠️ 必须裁决的范围冲突

spec §4.1 要求 S0 完成 = 63 项全终态，而 §3 又说 S1 依赖 S0 的 W5。台账里 S09 的 `acceptance` 原文含「未授权连接失败」——**Option B 下这条只对协作型子进程成立**。

要么 (a) 在台账 `acceptance` 加显式作用域限定（「经受管代理通道的连接」）再判 `done`，要么 (b) S09 停在非终态、S0 收敛不了。**建议 (a)**，并把作用域限定写进 ADR 的 Consequences。

## 5. S10 缺口

**OS keyring 已达标**：读写删三件事都真实（`keyring_enabled.go` 走 `zalando/go-keyring`，`Available()` 用哨兵 Get 区分「后端缺失」与「条目缺失」）。`nokeyring` 分支四方法一律返回 `ErrKeyringUnavailable`，Manager 降级到 fileStore 或拒绝 `secret://`。**「无 keyring 安全降级」验收已满足，未发现缺口。**

**统一脱敏断在 `MergeRedactors`**：`bootstrap.go:411` 取 `output.Redactor`，`:416` 被 `MergeRedactors` 换成**快照拷贝**（`secrets.go:196` 内 `merged := NewRedactor()`），`:505` 的 `redactor.Register(resolved)` 落在拷贝上 → **`SafeLogger` 背后的注册表自始至终看不到任何 provider key**，直接违反 `secrets.go:215-216` 自述的不变量。

注意 `st.SetRedactor(redactor)`（`:513`）和 `httpCfg.Redactor = redactor`（`:972`）拿的是**拷贝**，所以 **DB/WS/SSE 边界是好的——只有 SafeLogger 这条路瞎了**。

次要项：`bootstrap` 仍有多处 `fmt.Fprintf(os.Stderr, ...)` 绕开 SafeLogger（`:492`、`:495` 的 auto-migrate 警告路径，正好在处理 API key 的上下文里，虽只打印 cfgPath 和 err）。

**双标签构建实跑，都绿**：
```
go test -tags nokeyring -count=1 ./internal/secrets   → ok  1.215s
go test -count=1 ./internal/secrets                    → ok  1.287s
```

## 6. 是否可能放宽访问（逐项）

| 项 | 可否放宽 | 如何钉边界 |
|---|---|---|
| **S06** | **否**（若严格限定只改 `containsAny` 的 flag 归一化，该改动只让 deny 匹配更多形式 = 收紧）。**但**若顺手动 `hasPrefix`/`normalizeProgram` 就会放宽 | **双向测试**：三种绕过形式全部 `HardDeny`；**同时**断言普通 `go test ./...`、`go build`、`go test -run X` 仍 `Allow`（防过度拒绝把 deny 规则变成全局封杀——那正是 `policy.go:13-18` 记录的原始回归） |
| **S07** | **是**，如果实现「前缀匹配」这一档。**建议不实现**：把验收里「前缀规则有绕过回归测试」用一条**钉死精确相等语义**的测试来满足（证明不存在前缀放宽面），而不是新增放宽能力。加 `ExpiresAt` 是纯收紧 | 若坚持实现前缀档：必须有测试证明 `Prefix=["test"]` 的规则不会准入 `go test -tags=e2e_real ...`，即前缀规则不得跨过 execpolicy 的 deny |
| **S09** | ⚠️ **是，这是本包最大的放宽点**。今天子进程网络≈全黑（假 URL），启动真代理 + 支持 CONNECT 后变成「策略允许的都能通」。这是从「意外的严格」走向「设计的严格」，但客观上是放宽 | 三条必须钉：① 默认 `Default:""` 下任意 host 经代理返回 403（fail-closed 不因启动代理而失效）；② CONNECT 目标 host 走 `CheckHost`，deny 时不建立隧道；③ `PrepareEnv` 剥离继承变量的行为在真 URL 下仍成立（否则父进程的 `https_proxy` 可影子覆盖）。另需一条测试断言「未授权连接失败」的 Decision 进入了审计 |
| **S10** | **否**。修复 `MergeRedactors` 别名语义只增加被脱敏的字符串集合 = 收紧 | 探针式测试：在 `output.Redactor` 之外注册 secret 后，`SafeOutput.Logger.Printf` 必须输出 `[REDACTED]`——即直接断言 `secrets.go:215-216` 那条自述不变量。另建议加最小长度门（防超短 secret 把无关输出打成马赛克） |

## 7. 需要新 ADR（编号 0011，当前最大 0010）

**是，一条** —— S09 的 Option B 改变了一条对外可见的安全姿态（从「子进程无网」变成「子进程经受管代理按策略出网」），且要写下不可违反的边界。

Consequences 必须固化：
- 子进程出站的**唯一**受管通道是 loopback 代理；`shell.DefaultSecureFactory` **不得再用 `http://127.0.0.1:0` 这类伪装成强制的占位 URL**——要么真代理，要么显式无策略
- CONNECT **只做 host 策略判定 + 盲隧道，禁止 MITM**（否则会成为新的 secret 泄漏面）
- **明示残留风险**：无视 `HTTP_PROXY`、直接 `connect()` 的子进程不受此约束；该缺口归 S1，在 `CapabilityReport.Effective` 脱离 `DegradedHostGuard` 之前**不得声称「未授权连接失败」是全覆盖的**

S06/S07/S10 **不需要新 ADR**：S06 是 ADR-0004 精神内的 bug 修复；S07 若按建议不引入前缀档则无不变量变化；S10 是修复代码与既有 doc 不变量的偏离。

## 8. 审计的过时与错误

1. ⚠️ **【已过时，应从 W5 范围剔除】审计 S10 第 (3) 条** ——「本机 `go test ./internal/secrets` 有 2 个 keyring 真实往返测试失败，属环境依赖测试未做 skip 门禁」。**已修好**：`internal/secrets/secrets_extra_test.go` 已有 `requireKeyringWritable` 跳过助手（约 `:790-798`），两个测试都先调它。macOS 双标签实跑均绿
2. **【行号错误】审计 S07** 引 `bootstrap.go:867 approval.New(st,...)`，实际在 **`:898`**（`approval.NewAuditBus()` 在 `:897`）
3. **【不完整】审计 S06 的绕过清单**漏了 `-tags=e2e_real,foo`，也没记录 `-tags='e2e_real'` 其实被正确拦住。回归测试应覆盖**全部四种形式**而非审计列的两种
4. **【核实为准确】** 审计 S06 引的 `guard.go:292-308`、`profile.go:107`，S09 引的 netpolicy 零 slog、`policy.go:37-42/49/77`、`proxy.go:40`，S10 引的 `secrets.go:196`——逐条读过，判断属实。`bootstrap.go:376/465` 语义对应实际的 `:411/:416/:505`，行号漂移但结论成立
5. ⚠️ **【spec 范围冲突，需裁决】** §4.1 与 §3 在 S09 上互锁，见 §4 末尾
6. **【补充确认】** `config.example.yaml` 里 `rules:` **零命中**（`security:` 段只有 sandbox/network/shell 三块，`profiles:` 段的 shell 全是 legacy `policy`/`patterns` 写法）。**execpolicy 在出厂配置下完全不可达**，操作员无从发现。这意味着 S06 的 deny 绕过在默认配置下**不可利用**，但一旦操作员按文档启用 `rules:` 就立即可利用——修复优先级不因此降低，只是不构成 0-day
