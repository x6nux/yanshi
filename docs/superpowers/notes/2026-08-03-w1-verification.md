# W1 装配线 — 核验报告

> 2026-08-03 实测。所有行号已对当前工作树逐条核对。

## 0. 最重要的发现：`bootstrap.go` 的五行死赋值

**审计、2026-08-03 复核、W0 计划均未记录。它比 T07/T08 原有的两条缺口更靠前。**

`bootstrap.go:837` 是 `orch, err := orchestrator.New(orchConfig)`，而 `:926-930` 才做：

```go
orchConfig.Sandbox = sb
orchConfig.NetworkPolicy = networkPolicy
orchConfig.Approvals = approvalMgr
orchConfig.ShellManager = shellManager
orchConfig.SecureFactory = secureFactory
```

`orchestrator.New(cfg Config)` **按值取参**（`orchestrator.go:214-215` 在 New 内部拷贝），且全包**没有任何 post-New setter**（`grep 'func (o \*Orchestrator) Set'` 无命中）。

**后果**：生产环境里 `o.shellManager` / `o.secureFactory` / `o.sandbox` / `o.networkPolicy` / `o.approvals` **恒为 nil**，`orchestrator.go:278-289` 的五个 `if o.X != nil` 全部短路 → `tools.WithShellManager` / `WithSecureProcessFactory` / `WithSandbox` / `WithNetworkPolicy` / `WithApprovalManager` **在生产 turn 里从不执行**。

即使把九个 shell 工具注册进去、把 `Config.Factory` 填上，`ShellV2Tools.manager(ctx)` 仍会返回 `shell_v2.go:111` 的 `"shell: runtime unavailable"`。**必须先修这条。**

**为什么测试没抓到**：`bootstrap_test.go:74-76` 断言的是 `app.Sandbox`/`app.ShellManager`/`app.SecureFactory`（`App` 字面量 `:1034-1042` 直接填的），不是 orchestrator 拿到了什么。正是 spec §6.1 描述的那类假绿。

**影响面超出 W1**：`sandbox`/`netpolicy`/`approval`/`secureFactory` 在生产 turn 中全部未生效——需评估它是否影响 W5 的安全底座验收结论。

## 1. 五个接线点实测状态

### ① `BuildC1` 未被调用 — 确认

- `bootstrap.go`（1369 行）**零处**出现 `BuildC1`/`BuildRLM`/`BuildAutomation`/`NewA2Adapter`/`NewAutomationTools`/`NewBatchTools`/`NewRLMTools`
- `c1.go:134 BuildC1` 唯一调用者是 `c1_test.go:251/312/337`
- `a2_adapter.go:43 NewA2Adapter` 同样只有测试调用
- `allTools` 的 24 处 `append` 全在 `bootstrap.go:629–781`，最后一处 `:781`，紧接 `:786–793` 是 W0 的 `toolNames` 快照

### ② shell v2 — 确认，且见 §0

- `shell_v2.go:49 NewShellV2Tools` 非测试调用点 0
- `bootstrap.go:906-910` 的 `shell.NewManager(shell.Config{Root, MaxOutputBytes, IdleTimeout})` **无 `Factory:` 字段**；`manager.go:20` 是 `Factory ProcessFactory`，`manager.go:87` 无 factory 时返回 `"shell: no process factory configured"`
- **附带**：`NewShellV2Tools()` 不接 root。`shell_v2.go:121` 的 `Authorize(ctx, guard.Action{Tool:"shell_start", Shell: a.Command}, raw)` **没有 `Workdir` 字段**，而 legacy `shell.go:128` 是 `guard.Action{..., Workdir: s.root}`。开启九工具前必须补齐，否则破坏性删除门的 workdir 判定退化。`shell_v2.go:137` 的 `Dir: a.Workdir` 也没有像 `shell.go:144-146` 那样空值回落到 root

### ③ `registry.WithRole` — 确认

- `context.go:25` 定义，全仓非测试调用点 0
- 派生点：`manager.go:217` `childCtx, childCancel := context.WithCancel(parentCtx)`，`:221` `go m.runAgentLoop(childCtx, id, rt)`
- `runAgentLoop` 在 `manager.go:626`；`:639` 已在用 `m.recordRole(agentID)`（helper 在 `:684`），`:649` `rt.runner.Run(ctx, ...)`
- 消费侧齐全：`orchestrator.go:717 RoleFromContext` → `:719-720` PromptPrefix → `:723 tools.WithRolePolicy`。角色目录 `agentroles.go:14 AgentRoles()`（7 个），`:65 outputContractPrefix`
- `agent_lifecycle.go:46-47` 只有 `if a.Role == "" { a.Role = "general" }`，**无大小写归一、无未知值校验、无 custom 必须带 allowed_tools 的校验**；`:65` 是 `factory(allowed, "")`，`RoleDef.AllowedTools` 从不参与求交

### ④ `ApplyImages` — 确认，且 **turn 路径是三条不是两条**

- `multimodal.go:16` 调用者只有测试
- `TurnOpts` 在 `orchestrator.go:490-499`：`Model/ThinkingEffort/OutputSchema/PlanMode/ThreadID/TurnID/EmitWorkFrame/ConnectionSessionID`——**无图像字段，也无 model 名字段**（只有 `model.BaseChatModel`，拿不到 multimodalMap 的 key）
- WS：`ws.go:474` 只取 `cf.Text`，`cf.Images` 从不读；opts 构造在 `:644-649`
- v1：`service.go:219` 只取 `p.Input`，`p.Images`（`types.go:139` 已存在）从不读；opts 构造在 `:313`
- ⚠️ **第三条路径（spec 与审计都漏了）**：SSE `chat.go:132` 也自建 `TurnOpts`，其请求结构体（`chat.go:44-56` 匿名 struct）**没有 `Images` 字段**，同样静默丢图
- ⚠️ **正确性约束**：`ws.go:796` 的 `EventsWithHistoryOpts` 位于一个 **schema/task_end 重试循环**内，同一 `opts` 会被重复调用（`:780 history := cs.history`，只有 attempt>0 才在 `:791-793` 拷贝）。若把 ApplyImages 放进 `EventsWithHistoryOpts` 且原地改写 `history[n-1]`，**重试会重复注入图片**。必须先 clone slice 再 apply

### ⑤ `ws.go` 不设 `PlanMode` — 确认

- `ws.go:644-649` 的字面量只有 `Model/ThinkingEffort/OutputSchema/ConnectionSessionID`
- 模式实际值在 `cs.perm`（`ws.go:45`，类型 `permModeState`，`ws_perm.go:94`），读法 `cs.perm.get()`（`ws_perm.go:108`）
- 消费侧齐全：`orchestrator.go:305 tools.WithPlanMode`、`:525 runnerFor(model, opts.PlanMode)`、`:392-394 filterPlanTools`
- ⚠️ **审计对 G05 的第 (2) 条已被证伪**：审计说 `FlushRunners` 无调用者是缺口。实测 `orchestrator.go:386` 的缓存 key 是 `runnerCacheKey{model, mode}`（`:375-377` doc 明说按 mode 分桶），**plan 与 agent 本来就是两个 runner**，不需要 flush。`FlushRunners`（`:431`）保持零生产调用是正确的。**W1 不应为此写任何代码**

## 2. 接线改法

**① BuildC1**：把 `broker := task.NewBroker(...)` + `broker.SetVCS(...)`（现 `:981-987`）与 `ctx, cancel := context.WithCancel(...)`（现 `:990`）、`dispRef.Bind(...)`（现 `:992`）整体上移到 `workMgr := work.NewManager(...)`（`:747`）之后；在那里构造 `NewA2Adapter(workMgr, broker, st)` 并调 `BuildC1(...)`，把返回的 8 个 `automation_*` + `agent_batch` + `rlm_query` 追加进 `allTools`（**必须在 `:786` 的 toolNames 快照之前**）。`srv.TaskAPI`/`StartArtifactJanitor`/`StartSweeper` 留原位。另需 `App` 加 `C1Scheduler *automation.Scheduler`，`Shutdown`（`:1232`）在 `a.cancel()`（`:1234`）后、`a.Store.Close()`（`:1262`）前调 `Scheduler.Wait()`；Build 中途失败要 `cancel()`（scheduler goroutine 在 `BuildAutomation` 里已 `go scheduler.Start(parent)`，现有 `closeStoreOnError` defer 没覆盖它）。

**② shell v2**：分两步。先修 §0 死赋值——把五段（现 `:867-923`）整体上移到 `orchConfig := orchestrator.Config{...}`（`:806`）之前直接写进字面量，删掉 `:925-930`。再做注册：`internal/shell/factory.go` 新增 `ProcessFactory` 实现（把 `DefaultSecureFactory.Start` 里的 `netpolicy.PrepareEnv` + Phase 0 sandbox seam 抽成共享 helper 复用），填进 `shell.Config{Factory:...}`；`NewShellV2Tools` 改签名收 `root string`，`start` 补 `Action.Workdir` 与 `Dir` 空值回落；`allTools` 追加九个字段。

**③ WithRole**：`runAgentLoop`（`:626`）体内、进入 `:648` 的 run 循环前，用已有的 `m.recordRole(agentID)`（`:684`）取角色并 `if role != "" { ctx = WithRole(ctx, role) }`。同时 `agent_lifecycle.go:46` 附近补角色归一/校验，`:65` 把 `RoleDef.AllowedTools` 与 caller allowed 求交。

**④ ApplyImages**：`TurnOpts`（`:490`）加 `ModelID string` 与 `Images []proto.ImageAttach`；在 `EventsWithHistoryOpts`（`:502`）内 `withTurnContext` 之后、`runner.Run`（`:526`）之前，**先 `slices.Clone(messages)` 再** `o.ApplyImages(...)`——单一收敛点，三条路径只需填 opts。然后 `ws.go:644` 填 `ModelID: cs.displayModel()`（`ws_compaction.go:216`）+ `Images: cf.Images`；`service.go:313` 填 `ModelID: p.Model`（空时按 `s.models` 排序首名回落，与 `ws.go:382` 同规则）+ `Images: p.Images`；`chat.go:44` 的请求结构体加 `Images` 并在 `:132` 一起填。

**⑤ PlanMode**：`ws.go:644` 前取 `mode, _ := cs.perm.get()`，字面量加 `PlanMode: mode == guard.ModePlan`。一行。

## 3. `BuildC1` 签名与依赖可用性

```go
func BuildC1(parent context.Context, cfg config.Config, db *store.Store,
             queueAdapter automation.QueuePort, registryMgr *registry.Manager,
             models map[string]model.BaseChatModel, fakeModel model.BaseChatModel)
             (*C1Components, error)          // c1.go:134
```

| 形参 | `Build` 里的现成变量 | 位置 |
|---|---|---|
| `parent` | `ctx` | 现 `:990`，**需上移** |
| `cfg` | `cfg` | 全程可用 |
| `db` | `st` | 全程可用 |
| `queueAdapter` | 需新建 `NewA2Adapter(workMgr, broker, st)` | `workMgr` 在 `:747`；`broker` 现 `:981`，**需上移** |
| `registryMgr` | `subagentManager` | `:670` |
| `models` | `providerModels` | `:588` |
| `fakeModel` | 仅 fake 模式传 `chatModel` | 非 fake 且 `cfg.Batch.RLMModel==""` 时 `SelectRLMModel`（`c1.go:29`）**返回错误** |

`a2_adapter.go:46-50` 已有编译期断言：`*A2Adapter` 满足 `automation.QueuePort`，`*task.Broker` 满足 `BrokerSubmitter`，`*store.Store` 满足 `KVStore`。**`work.Manager` 不直接满足 `QueuePort`。**

### ⚠️ 必须处理的契约问题

`BuildC1` 目前在 `BuildRLM` 失败时**整体返回 error**（`c1.go:147-150`）。照搬会让任何没配 `batch.rlm_model` 的生产配置把 automation + agent_batch 一起废掉——AU1/M07 对绝大多数用户仍是死的。

**建议改 `BuildC1` 为 RLM 可降级**（`C1Components` 加 `Warnings []string`，RLM 失败时 `RLM=nil` 并打 stderr，automation/batch 照常构造），同步改写 `c1_test.go:301 TestBuildC1BuildRLMError`。这与 CLAUDE.md「非致命启动失败打 stderr 并以该子系统禁用继续」一致。

### 另两点必做配套

- 十个新工具名（8 个 `automation_*`、`agent_batch`、`rlm_query`）**必须加进 `DefaultOrchestratorProfile()`（`bootstrap.go:276`）的 `Tools.Allow`**，否则 guard 会拒掉每次调用。**逐名列出而非用 `automation_*` 通配**——GOV5 的幽灵检测会跳过通配项
- 八个 automation 工具与 `agent_batch` 的 desc 都写着 "Approval required."，但用的是 `NewGuardedTool`（`automation.go:37/46/54/62/70/78/86/94`、`batch.go:25`）而非 `NewApprovalGuardedTool`（`guard.go:174`）。AU1 验收含「approval 门禁」，应改为后者
- `AutomationTools`（`automation.go:16-25`）没有 `Tools()` 方法（`BatchTools`/`RLMTools` 同样没有），建议补一个避免 bootstrap 里逐字段 append 八行

## 4. `fs_mkdir` 四处消费侧残留

`fs_mkdir` 从来不是注册工具（`FSTools.Tools()` 只有 read/write/edit/list/glob/search/apply_patch）。

| 位置 | 所属声明 | 清理方式 |
|---|---|---|
| `guard/mode.go:123` | `var editTools`（`:120-124`） | **删条目**。`:117-119` 的 doc 写着 "and directory creation" 须一并改。⚠️ 顺带发现：真实写工具 `apply_patch` **不在** `editTools` 里——补它属扩大 allow-edits 自动批准面，是行为变更，**留给 W5** |
| `tui/styles.go:406` | `var toolDisplayNames`（`:399`，闭合 `:414`） | 删条目 |
| `tui/entries.go:845` | `toolDisplayFor`（`:843`）的 case 标签 | 删标签（case 本身是活的） |
| `tui/frecency.go:217` | `extractPathFromToolArgs`（`:215`）的 case 标签 | 删标签。建议**加 `apply_patch`**——真实写工具，frecency 现在漏记它 |

注释 4 处：`entries.go:405`、`entries.go:410`、`frecency.go:212`、`commands.go:383`。

连带测试 3 处：`guard/mode_test.go:106` 与 `:122`、`tui/model_test.go:191`、`tui/frecency_cov_test.go:152`。另 `model_test.go:1687` 是注释。

⚠️ **与 W8 重叠**：`styles.go:406`、`entries.go:845`、`frecency.go:217` 三处都在 W8 代码区。建议先落地的一方清理，另一方 review 时确认。

## 5. GOV7 建议：**加窄版，只覆盖 guard 侧**

放在 `internal/bootstrap/wiring_test.go`（**运行时**，复用 `buildMinimalApp` + `App.ToolNames`）：断言 `guard` 的 `editTools` ⊆ 已注册工具名。只需给 `internal/guard` 加一个导出访问器（如 `EditToolNames() []string`），不引新 import，不动 `portAllowlists`。

理由：
1. `editTools` 是**授权语义**的一部分（`ModeAllowEdits` 的免提示自动批准集），不存在的名字占着自动批准名额，与 GOV5 同类，只是发生在消费侧
2. 清理后集合为 `{fs_write, fs_edit}`，两者必然注册，**违规集为空 ⇒ 不需要第四张豁免表**
3. 成本是一个访问器 + 约 20 行测试，与 GOV5 共用夹具，顺带继续推 COV3

**不建议覆盖 TUI 三张表**：纯展示/排序数据，死条目后果是「永不渲染」而非「错误授权」；覆盖需从 `internal/cli/tui` 导出三个内部表并把 bubbletea 拖进 bootstrap 测试二进制，投入产出不成立。静态 AST 更不可行——散落在 map 字面量与 case 标签里的字符串常量，靠启发式判断「哪个字符串是工具名」会让误报淹没信号。

## 6. 九项 ↔ 接线点对应

| 项 | 接线点 | 额外工作 |
|---|---|---|
| `C1/AU1` | ① | RLM 降级改造；8 工具名进 profile；改 `NewApprovalGuardedTool`；`App.C1Scheduler` + Shutdown 等待 |
| `C1/M07` | ① | `agent_batch` 进 profile；同上 approval |
| `C1/RLM1` | ① | `rlm_query` 进 profile |
| `A1/T07/T08` | ② | **先修 §0 死赋值**；新建 `ProcessFactory` 安全实现；`NewShellV2Tools(root)` + `Action.Workdir`；一条走**生产实现**的端到端测试（可用 `app.Orch.EventsWithHistoryOpts` + `TurnOpts.Model` 注入 `einollm.NewFakeModelWithMessages` 脚本化 tool_call，绑 `tools.WithPermissionCallback` 放行——默认 profile 的 `Shell.Policy` 为空 ⇒ `guard.go:318` 返回 Prompt）。**正是 spec §6.1 点名的形状** |
| `B1/M05` | ③ | 角色归一/未知值/custom 校验 + role∩caller 求交（`agent_lifecycle.go:46,65`） |
| `B1/M04b` | ③ | `ParentWorkingSetHint`（`subagent.go:376`，零生产调用）接进 `agent.go:336`；`config.example.yaml` 补 `subagents:` 块（当前**完全没有**）。⚠️ 验收里「并发上限生效」依赖 legacy 三入口接 Manager cap，而 spec §4.3 把那条划给了 **W3 的 M04**——需裁定：W1 顺手接，还是 M04b 的 done 等 W3 |
| `G/VISION` | ④ | `TurnOpts` 加 `ModelID`；先 clone 后 apply；SSE 第三条路径补 `Images` |
| `G/VISION-TOOL` | ④ + TUI | 五入口里 D（screenshot）已通，E（协议）由 ④ 解决；**A（Ctrl+V）、B（@path）、C（fs_read/web_fetch 产生附件）都需新代码**。A：`internal/clipimg` 零外部导入方，TUI 无 `KeyCtrlV`（`handlers.go:224` 的 switch 只有 CtrlC/CtrlEnter/CtrlO/CtrlK/CtrlS），model 无 pending images 状态；`tui/images.go:11 buildSendFrame` 是孤立函数，`tuiSession.SendFrame`（`model.go:29`）通道已具备，但 `queue.go:40` 走的是只收字符串的 `m.sess.Send(text)`。B：TUI 完全没有 `@path` 检测，与 W8 的 UX3 同源——**建议 W1 只做服务端 path-ref 解析**（可复用 `vision.go:88-115` 的 `withinRootAbs` + `Authorize`），TUI 触发面留给 W8。C：`fs.go:298` 与 `web.go:145` 目前只返回提示文本。**这一项工作量最大、最易失控，需先定死范围** |
| `A2/G05` | ⑤ | 无（一行）。审计第 (2) 条已证伪，第 (3) 条 TUI 渲染属 W8 |

## 7. PR 分组（6 个）

1. **PR-A（先做，单独）**：修 §0 死赋值——安全五件套移到 `orchestrator.New` 之前。约 20 行，阻塞 PR-C，本身是独立生产缺陷，值得单独 review。配「orchestrator 真的拿到了 shellManager」的回归测试
2. **PR-B**：`BuildC1` 接线（含顺序重排、RLM 降级、profile 补 10 名、approval 改造、`App.C1Scheduler`）→ 一次解 3 项，清空 `assemblyExceptions`
3. **PR-C**：shell v2 九工具 + `ProcessFactory` + `Config.Factory` + 生产实现端到端测试 → 清空 `toolWiringExceptions`（9 条）
4. **PR-D**：`WithRole` + 角色校验/求交 + `ParentWorkingSetHint` + `config.example.yaml` 的 `subagents:` → 解 M05 + M04b，清空 `ctxInjectExceptions`
5. **PR-E**：`TurnOpts.PlanMode`（一行）+ `Images/ModelID` + 三条 turn 路径透传 → 解 G05 + VISION。两者都只碰 `TurnOpts` 与 turn 入口，冲突面最小
6. **PR-F**：VISION-TOOL 五入口 + `fs_mkdir` 清理 + GOV7 + 台账翻牌 + 文档

三张豁免表分别在 PR-B/C/D 清空，**PR-D 合并后应同时为空**——这是 W1 的验收信号。收尾跑 `go test ./...` + `go run ./cmd/featurestatus`（应显示 `terminal: 10/63`）+ 四个文档生成器（改了 SSE 请求结构体与 config 示例）。

## 8. 审计 / spec 的过时与错误

1. **审计对 G05 第 (2) 条已被证伪**（`FlushRunners`）——W1 不应为此写代码
2. **审计与 spec 都遗漏了第三条 turn 路径**（SSE `chat.go:132`）。spec §4.3 只写「WS 与 v1 两条」。三条都要接
3. **`bootstrap.go:926-930` 死赋值是全新发现**，见 §0。建议补记进 spec，并评估是否影响 W5 的验收结论
4. **行号漂移**（W0 落地后的正常漂移）：审计 VISION 引的 `bootstrap.go:556-565` 现在 `:592-613`；`:630-631` 的 profile allowlist 已由 W0 提取到 `DefaultOrchestratorProfile()`（`:276`）；审计 M04b 引的 `orchestrator.go:697-700` 现在 `:717-724`
5. **spec §4.3 的 W1/W3 边界模糊**：M04b（W1）验收含「并发上限生效」，实现它所需的「legacy 三入口接 Manager cap」写在 W3（属 M04）。需明确裁定
6. **`config.example.yaml` 有 `batch:` 块（`:88-93`）但完全没有 `subagents:` 块**——审计对 M04b 第 (c) 条描述准确
