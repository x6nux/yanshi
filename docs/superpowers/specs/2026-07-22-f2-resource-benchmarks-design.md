# Batch F2 — 资源治理与压测基线 设计

> **日期**：2026-07-22
> **归属**：E-H roadmap 的 Tier F（`docs/feature-roadmap-e-h.md` §7）
> **命题**：A-D 功能面已对齐后，把"长跑稳定 + 成本可见 + 性能可回归"补到 v1.0 可用——清理 `createdWT` 泄漏、核对子代理并发上限、给 mid-turn 压缩加 cooldown、建性能基准基线、让 ACP 子进程 usage 回流到 budget。
> **范围**：**只补质量/性能/可观测的既有缺口，不加新功能面**（沿用 E-H 命题）。改的是 broker 清理、CompactingModel 节流、ACP usage 解析、各包 `_bench_test.go`。
> **状态**：设计稿，待用户审阅 → writing-plans。
> **依赖前置**：[RAC1]（E2）固化 `-race` CI 并暴露 broker/registry 既有竞态；本批 LEAK1 依赖其结论。执行前须 re-verify D3 未改写 `bootstrap`/`store`/`config` 落点。

---

## 1. 目标与非目标

### 目标

- **[LEAK1]** `task.Broker` 的 `createdWT` map 与对应 worktree 在任务**终态**（含 `RequeueStale` 判失败这一被遗漏的路径）被显式回收；长跑测试断言 map 不增长。
- **[LEAK2]** 核对 B1/M04b 的子代理并发上限现状（见 §5 结论：**已实现**）；本批降级为"核对 + 交互文档化 + 查漏"，不重复造轮子。
- **[CCL1]** mid-turn 压缩（`CompactingModel`）加显式 cooldown，同 turn 内不重复压缩，但逼近硬窗口时仍强制触发；统一 `keepRecent` 两套语义的文档。
- **[BENCH1]** 给 VCS commit / 三方合并 / `fs_edit` / orchestrator turn 建基准基线，CI 用 benchstat 记录趋势（不硬门禁，回归 >N% 告警）。
- **[LEAK3]** ACP 子进程（codex/claudecode）的 usage 解析回流入 goalloop `UsageSink`，budget 含子进程；解析失败安全降级不阻塞。

### 非目标（本批不做）

- 新增功能面（任何 A-D 未完成项）。
- 跨重启的 `createdWT`/worktree 持久化回收（见 §10 out-of-scope，归 broker 持久化后续）。
- 压缩**算法**本身的优化（换摘要模型、改 pin 策略）——只加节流。
- 基准的**硬门禁**阻断合并（只告警 + 记录趋势，防 CI 噪声误杀）。
- ACP usage 的**精确**逐 token 审计（只把能解析的 token 累加进 budget）。

---

## 2. 背景（落点实测）

- `task.Broker.createdWT`（`internal/task/broker.go:41`）记录 broker 在 `Claim` 中**自己创建**的 worktree（`taskID → worktreeID`），供终态回收。共享（pre-set）worktree 不进此 map。
- **清理现状**：`RecordResult`（`broker.go:165-173`）在终态 finalize 后删 map 项 + `RemoveWorktree`；`Cancel`（`broker.go:201-207`）同样清理。**但 `RequeueStale`（`broker.go:219-241`）漏了**：它调 `store.RequeueStaleTask`，该 SQL 在 `attempts+1 > maxRetries` 时直接把 status 置 `'failed'`（`internal/store/task.go:273-293`），而 broker 此路径既不删 `createdWT` 也不 `RemoveWorktree` —— **这是 LEAK1 的真实泄漏点**。
- **子代理并发**：`registry.NewManagerOpts.MaxConcurrent`（`internal/agent/registry/manager.go:21`，0→默认 10，clamp 1..20）、`Spawn` 在 `runningLocked() >= limit` 时返回 `*SpawnErrCap`（`manager.go:130-135`）、`spawnWithRetry` 退避重试（`internal/tools/subagent.go:303-326`）、config `subagents.limit` 已接 `bootstrap.go:529`。**LEAK2 已由 B1/M04b 完成**（详见 §5）。
- **mid-turn 压缩**：`einollm.CompactingModel`（`internal/llm/eino/compacting.go:56`）在每个 ReAct 迭代 `maybeCompact`，仅由 `shouldCompact`（`compacting.go:116`，threshold + KeepRecent 门控）判定，**无任何"上次压缩"状态**，长 tool-heavy turn 内可能多次压缩。`keepRecent` 桥接：`CompactingModel.KeepRecent`（消息数，`compacting.go:67`）→ `ctxcompact.PlanOpts.KeepRecent`（对数）经 `/2`（`compacting.go:104`），语义双轨需文档化。
- **基准目标**：`vcs.CommitMain/CommitWorktree`（`internal/vcs/vcs.go:950/958`）、三方合并 `mergeToMainLocked`（`vcs.go:1224`，base/ours/theirs 三树逐路径归并）、`tools.FSTools.runEdit`（`internal/tools/fs.go:427`）、orchestrator turn 入口 `Orchestrator.Query`（`internal/agent/orchestrator/orchestrator.go:407`，同步返回 answer；或 `EventsWithHistoryOpts` 流式）。
- **ACP usage**：`internal/acp` 全包 `grep usage|token` **零命中**。`acp.Client.handleNotify`（`internal/acp/client.go:195`）只处理 `agent_message_chunk`/`tool_call`/`tool_call_update`（`client.go:206-237`），`Event` 结构（`client.go:15`）与 `PromptResult`（`types.go:143`，仅 `StopReason`）均无 usage 字段。**ACP `session/update` 的 `usage_report` discriminator 当前被 `default` 分支吞掉**（`client.go:231-237`）—— LEAK3 的落点。
- **goalloop budget**：`UsageSink`（`internal/agent/goalloop/usage.go:38`）由 planner/evaluator/tierer 各自 `addUsage`（`usage.go:77`）累加；`Loop.spent()`（`loop.go:41`）据此判 `overBudget`。`ACPImplementer.Implement`（`internal/agent/goalloop/implementer.go:410`）不持有 sink，子进程 token 永不回流。

---

## 3. 与 E2/RAC1 的衔接

roadmap（附录 B 关键链）写明 **E2-RAC1 → F2-LEAK**：race 发现驱动泄漏修复。本批的接法：

1. **RAC1 先固化** `go test -race ./internal/task/...` 与 `createdWT` 并发热点测试，**暴露** `createdWTMu` 与 `RemoveWorktree` 的既有竞态（如 sweeper 与 Cancel 并发删同一 taskID）。
2. **LEAK1 收尾**：在 RAC1 登记的竞态基础上，本批补 `RequeueStale` 的终态清理，并保证清理路径在 `-race` 下无新竞态（删 map 项与 `RemoveWorktree` 仍走 `createdWTMu`，与 Cancel/RecordResult 互斥）。
3. 若 RAC1 发现 registry `runningLocked()`（`manager.go:536`，`runtime` 与 `records` 双计数取大）有竞态，登记到本批 LEAK2 的"核对/扩展"清单（见 §5）。

> 即：RAC1 产出"竞态清单"，LEAK1 是该清单在 broker 侧的修复落点；二者可紧邻施工（RAC1 的 broker 测试与 LEAK1 的清理改动同一文件）。

---

## 4. [LEAK1] createdWT map 泄漏清理  (P1 | 缺失 | synthesis R13/A19)

- **缺口**：`RequeueStale` 经 `store.RequeueStaleTask` 把超 maxRetries 的 stale 任务直接置 `'failed'`，但 broker 不经 `RecordResult`/`Cancel`，故 `createdWT[id]` 与磁盘 worktree 双双泄漏。长跑（大量 worker 崩溃/超时）下 map 与 `~/.yanshi/worktrees/` 无界增长。
- **落点**：`internal/task/broker.go`（改 `RequeueStale`）+ `broker_test.go`（长跑断言）。新/改文件：改 2。
- **设计**：
  - `RequeueStale` 拿到 `changed=true` 的 stale 任务后，重新 `GetTask(id)` 读回新 status：若已终态（`failed`）→ 走与 `RecordResult` 相同的回收逻辑（删 `createdWT[id]` + `RemoveWorktree`，best-effort）。若回 `pending`（将重试）→ **保留**（worktree 由重试任务复用，Claim 见非空 `worktree_id` 不重建、不重录 map，见 `broker.go:109`）。
  - 抽公共回收 helper `reclaimWorktree(id string)`（删 map + RemoveWorktree），供 `RecordResult`、`Cancel`、`RequeueStale` 三处复用（CLAUDE.md 约定"重复逻辑必须抽公共函数"）。
  - worktree 生命周期文档化（claim→finalize/requeue-fail/cancel→reclaim；requeue-pending→复用）。
- **依赖**：[RAC1]（并发正确性；`reclaimWorktree` 须在 `createdWTMu` 下与 Cancel/RecordResult 互斥）。
- **风险**：过早清理误删可复用 worktree → 状态机明确：**仅终态（failed/completed/cancelled）才回收**，pending 一律保留；`GetTask` 读 status 后再判，避免与并发 finalize 双删（`delete` 幂等 + RemoveWorktree 容错已存在）。`RequeueStaleTask` 返回 `changed=false`（已被并发 finalize）时跳过。
- **验收**：
  - 模拟 worker 崩溃 → sweeper 判失败 → `createdWT` 不含该 id 且 worktree 被移除。
  - sweeper 判 pending（重试）→ `createdWT` 保留、worktree 复用。
  - 长跑测试（提交 N 任务、全部超 maxRetries 失败）断言 `len(createdWT)` 终态归零、不随 N 增长。
  - `-race` 下 sweeper + Cancel + RecordResult 并发不报竞态。
- **预估**：1d。

---

## 5. [LEAK2] 子代理并发上限 —— 核对结论：B1/M04b 已完成  (P1 | 已完成 | synthesis A10)

> **执行前核对结果（spec 阶段实测）**：B1/M04b **已完整实现并发上限**，本项**降级为"核对 + 交互文档化 + 查漏"**，不新增计数/上限逻辑。

- **现状（实测）**：
  - 上限：`registry.NewManagerOpts.MaxConcurrent`（`manager.go:21`），`NewManager` clamp 1..20、0→默认 10（`manager.go:67-74`）。
  - 计数只数 running：`runningLocked()`（`manager.go:536`）取 `runtime` 与 `StatusRunning` 计数的较大值。
  - 满则拒绝：`Spawn`（`manager.go:130-135`）与 `Resume`（`manager.go:388-392`）返回 `*SpawnErrCap`（`types.go:215`）。
  - 重试：`spawnWithRetry`（`subagent.go:303-326`）退避重试 `*SpawnErrCap`，不中断 turn。
  - 配置接线：config `subagents.limit`（`config.go:246`，默认 10）→ `applyDefaults`（`config.go:442`）→ `validate` clamp 1..20（`config.go:501`）→ `bootstrap.go:529` `MaxConcurrent: cfg.Subagents.Limit`。
  - 测试：`TestRunnerCapsAtRegistryMaxConcurrent`（`internal/agent/batch/runner_test.go:93`）、`registry/manager_spawn_test.go` MaxConcurrent=1 用例。
- **缺口（仅文档/查漏级）**：
  1. **与深度上限的交互未文档化**：并发上限（横向，`MaxConcurrent`）与深度上限（纵向，`tools.MaxSubAgentDepth=3`，`subagent.go:99`）是两个正交维度，但 `Spawn` 里深度判定（`manager.go:121`）与并发判定（`manager.go:131`）先后顺序、二者同时超限时的错误优先级无文档。→ 在 `Spawn` 与 `MaxSubAgentDepth` 处补注释说明。
  2. **`runningLocked` 双计数取大值的语义**：`runtime` map 与 `records` StatusRunning 在极端竞态下可能短暂不一致，取大值偏保守（宁拒不滥）。若 RAC1 在此报竞态，本批顺带收紧（统一以 `runtime` 为准 + 注释为何）。
  3. （可选）`List` 返回的 `Running/Limit`（`types.go:203`）已在 `agent_list` 暴露，核对 UI 是否展示。
- **落点**：`internal/agent/registry/manager.go`（注释 + 可能的 `runningLocked` 收紧）、`internal/tools/subagent.go`（深度/并发交互注释）。新/改：改 2，纯注释/微调，0 新文件。
- **依赖**：[RAC1]（若 `runningLocked` 需收紧）。
- **风险**：收紧 `runningLocked` 改计数语义可能影响现有 `TestRunnerCapsAtRegistryMaxConcurrent` → 改动需同步测试；纯注释无风险。
- **验收**：
  - 并发上限行为已有测试守护（引用既有测试，不重复写）。
  - 深度/并发双上限的交互、`Spawn` 内判定顺序有承重注释。
  - 若 RAC1 报 `runningLocked` 竞态 → 修复后 `-race` 通过且既有 cap 测试仍绿。
- **预估**：0.5d（核对 + 注释；若 RAC1 触发 `runningLocked` 收紧则 +0.5d）。

---

## 6. [CCL1] mid-turn 压缩 cooldown  (P2 | 缺失 | synthesis R3/A8 + B0 残余)

- **缺口**：`CompactingModel.maybeCompact`（`compacting.go:98`）无"上次压缩"状态。长 tool-heavy turn 中，每次 ReAct 迭代追加 tool_call/result 后历史可能重回 threshold 之上，导致同 turn 内反复压缩（每次都跑一遍摘要模型 turn，费 token + 延迟）。B0 称 threshold 天然门控，但仅防"刚压完立刻再压"，防不住"追加几条 tool 消息后又过阈"。
- **落点**：`internal/llm/eino/compacting.go`（加 cooldown 字段与判定）+ `compacting_test.go`；可选 `internal/config/config.go` `CompactionConfig`（cooldown 参数）。新/改：改 2。
- **设计**：
  - `CompactingModel` 增字段 `lastCompactTokens int`（上次压缩后的 TokensAfter）与 `lastCompactAt time.Time`（可选，时间维 cooldown）。这两个字段随 `runners sync.Map` 缓存的 CompactingModel 实例跨 turn 保留（per-model 指针为键，见 CLAUDE.md），天然提供"近期刚压过则延后"语义。
  - `shouldCompact` 增 cooldown 判定：若 `lastCompactAt` 在 cooldown 窗口内 **且** 当前 tokens 未逼近硬上限（`< hardForceFraction*ContextWindow`，默认 `hardForceFraction=0.95`）→ 返回 false（延后）。逼近硬上限 → **强制**压缩（否则下一个真实模型调用会 over-window 报错）。
  - 压缩成功后更新 `lastCompactTokens`/`lastCompactAt`（在 `maybeCompact` 返回 `true` 分支）。
  - **不动** `ctxcompact.Run` 核心与 `PlanOpts` 语义（PROP1/E2 在做核心属性测试，避免交叉）。
  - `keepRecent` 双语义文档化（承重注释）：`CompactingModel.KeepRecent` = 尾部**消息数**；`PlanOpts.KeepRecent` = 尾部**对数**（Plan pin 末尾 `2*KeepRecent` 条，`plan.go:39`）；桥接是 `compacting.go:104` 的 `/2`。config `compaction.keep_recent`（`config.go:261`，对数语义，默认 4）→ bootstrap 装配时 ×2 写入 `CompactingModel.KeepRecent`（消息数）。
- **依赖**：- （与 PROP1 落点同包但不改核心，可并行）。
- **风险**：
  - cooldown 漏压导致 over-window 真实报错 → `hardForceFraction` 兜底强制压缩；首次压缩前 `lastCompactTokens=0` 不触发 cooldown。
  - cooldown 状态跨 turn 残留误延后 → 时间维 cooldown（如 ≤几秒）+ 强制阈值双保险；或按 turn 重置（实现时二选一，见 open question）。
  - 破坏现有 mid-turn 压缩测试 → 保留 `/2` 桥接不变，cooldown 默认参数使既有用例（历史远超 threshold）仍触发。
- **验收**：
  - 同 turn 连续多次 ReAct 迭代（追加 tool 消息重回 threshold）→ 仅压缩一次（cooldown 内延后）。
  - 历史逼近 `0.95*ContextWindow` → 即使在 cooldown 内也强制压缩。
  - `keepRecent` 双语义有承重注释；既有压缩测试全绿。
  - （可选）cooldown 参数可配且默认值不改变现有行为。
- **预估**：1-2d。

---

## 7. [BENCH1] 性能基准基线  (P2 | 缺失 | synthesis A25)

- **缺口**：无性能基线，VCS commit / 三方合并 / `fs_edit` / orchestrator turn 的性能不可观测、回归不可发现。
- **落点**：`internal/vcs/vcs_bench_test.go`（新）、`internal/tools/fs_bench_test.go`（新）、`internal/agent/orchestrator/orchestrator_bench_test.go`（新）+ CI benchstat 脚本。新/改：新 3 + CI 配置。
- **设计**：
  - **BenchmarkVCSCommit**：初始化一个 repo（`InitRepo` + N 次 `RecordEditMain` + `CommitMain`），bench 单次 commit 耗时（tree 写入 + commit 行）。子场景：小 tree（10 文件）/ 大 tree（1000 文件）。
  - **BenchmarkDAGApply**（三方合并）：建 base/ours/theirs 三 commit（`mergeToMainLocked` 的输入形态），bench `MergeToMain`（`vcs.go:1214`）单次合并（含 `commitTree` 三次 + 逐路径归并 + 写 merge commit）。子场景：无冲突 / 有冲突路径。
  - **BenchmarkFSEdit**：临时目录写一中等文件，bench `FSTools.runEdit`（`fs.go:427`）单次精确替换 + 一次 lenient 匹配。
  - **BenchmarkOrchestratorTurn**：`einollm.FakeModel`（确定性、无 API key）+ 最小工具集，bench `Orchestrator.Query`（`orchestrator.go:407`）单 turn（1 次 model 调用 + 0/1 次工具调用）。测的是编排开销，不是模型延迟。
  - 全部用 `t.TempDir()` / 内存 store / FakeModel，零外部依赖（CLAUDE.md "Fake 优先"）。
  - CI：`go test -bench=. -benchmem` 跑这些 bench，`benchstat` 比对 main 基线，**记录趋势**（artifact 上传），**不做硬门禁**；回归 >N%（默认 20%，见 open question）打 warning/PR comment，不 fail。
- **依赖**：- （G1/CIG1 把 bench job 接入 CI 矩阵；本批只产 bench + 脚本）。
- **风险**：
  - 噪声（CI runner 抖动）→ 多次运行 + benchstat 置信区间；只做相对比较不绝对。
  - 环境差异（Windows/Linux）→ 基线分平台记录。
  - bench 本身慢拖 CI → bench 入 nightly/merge job，PR 只跑快速子集（CIG1 分层）。
- **验收**：
  - 四个基准存在且 `go test -bench` 本地可跑、零外部依赖。
  - CI 记录趋势（benchstat 输出存档）。
  - 人为制造回归（如 commit 内加无谓循环）能被 benchstat 标出 >N%。
- **预估**：2d。

---

## 8. [LEAK3] ACPImplementer usage 回流  (P2 | 缺失 | B0 残余)

- **缺口**：ACP 子进程（codex/claudecode）的 token 完全不计入 goalloop budget——`internal/acp` 零 usage 解析，`ACPImplementer` 不持 sink。goal loop 的 `overBudget`（`loop.go:41`）只算 planner/evaluator，子进程白嫖。
- **落点**：`internal/acp/client.go`（解析 `usage_report`）+ `types.go`（usage 字段）+ `internal/agent/goalloop/implementer.go`（接 sink）+ `acp/client_test.go`/`fakeagent.go`。新/改：改 4。
- **设计**：
  - **解析源**：ACP `session/update` 的 `usage_report` discriminator（codex/claudecode adapter 按 ACP 规范发出，含 input/output/total tokens）。在 `handleNotify`（`client.go:195`）的 `default` 分支前加 `case "usage_report"`：解析 tokens → 累加到 per-Prompt 的 `Event`（`Event` 增 `Usage` 字段，或新增 `Usage acp.Usage`）。
  - **回流**：`Client.Prompt` 的 `onEvent` 回调把 usage 透传给调用方；`worker.run`（`implementer.go:296`）在 Prompt 的 onEvent 里把 usage 累加进 `*goalloop.UsageSink`（`ACPImplementer` 增 `Sink *goalloop.UsageSink` 字段，bootstrap/goalloop 装配时注入）。
  - **降级**：adapter 不发 `usage_report`（或字段缺失）→ 解析为零 usage，`addUsage` nil-safe（`usage.go:77`）已自然 no-op，**不阻塞 turn、不报错**。可选兜底：解析 agent stdout 的 usage 行（codex JSONL/claudecode 统计行）——列为可选增强，v1 只做 `usage_report` 事件源。
  - `UsageSink.Add`（`usage.go:44`）已并发安全，子进程事件来自 transport ReadLoop 单线程，无需额外锁。
  - **budget 含子进程**：无需改 `Loop.spent()`（`loop.go:41`）——它读的是同一个 sink，子进程 usage 一旦进 sink，`overBudget` 天然包含。
- **依赖**：- （与 C4 pricing `COST1` 共享 sink；ACP usage 自动进 `/cost`）。
- **风险**：
  - 各 CLI 的 `usage_report` 字段名/单位不一 → 尽力解析 + 未知字段忽略 + 零值降级；建 fixture（抓真实 adapter 输出）测多形态。
  - 异步事件在 Prompt 返回后到达 → usage_report 通常在 turn 内发；实现时 onEvent 在 Prompt 阻塞期间累加，Prompt 返回前保证收到（或容忍少量延迟到下一轮 budget 检查）。
  - 解析异常 panic → 加 recover + 降级（绝不因 usage 解析崩掉 agent turn）。
- **验收**：
  - FakeAgent 发 `usage_report` → `worker.run` 后 sink 含对应 tokens；`overBudget` 能被子进程 usage 触发。
  - adapter 不发 usage_report → sink 不增、turn 正常完成、无错误。
  - 解析畸形 usage payload → 不 panic、降级为零。
  - （真实路径）`-tags e2e_real` 测试若 PATH 有 codex/claudecode，断言 sink 非零（门禁同既有 e2e）。
- **预估**：1-2d。

---

## 9. 文件结构

| 文件 | 职责 | 新/改 |
|---|---|---|
| `internal/task/broker.go` | `reclaimWorktree` 公共回收；`RequeueStale` 终态清理；worktree 生命周期注释 | 改 |
| `internal/task/broker_test.go` | sweeper 判失败/重试两路径回收断言；长跑 map 不增长；`-race` 并发 | 改 |
| `internal/agent/registry/manager.go` | 深度/并发双上限交互注释；（条件）`runningLocked` 收紧 | 改 |
| `internal/tools/subagent.go` | `MaxSubAgentDepth` 与并发上限正交维度注释 | 改 |
| `internal/llm/eino/compacting.go` | `lastCompactTokens/At` cooldown 字段 + `shouldCompact` 延后/强制判定；`keepRecent` 双语义注释 | 改 |
| `internal/llm/eino/compacting_test.go` | cooldown 内延后、逼近硬上限强制、单 turn 不重复压缩 | 改 |
| `internal/config/config.go` | （可选）`CompactionConfig` cooldown/hardForce 参数 + defaults | 改 |
| `internal/vcs/vcs_bench_test.go` | BenchmarkVCSCommit / BenchmarkDAGApply | 新 |
| `internal/tools/fs_bench_test.go` | BenchmarkFSEdit | 新 |
| `internal/agent/orchestrator/orchestrator_bench_test.go` | BenchmarkOrchestratorTurn（FakeModel） | 新 |
| CI 配置（`.github/workflows/`） | bench job + benchstat 趋势记录 + >N% 告警 | 改/新 |
| `internal/acp/types.go` | `Event.Usage`（或 `acp.Usage`）字段 | 改 |
| `internal/acp/client.go` | `handleNotify` 解析 `usage_report`；`Prompt` onEvent 透传 usage | 改 |
| `internal/acp/client_test.go` / `fakeagent.go` | usage_report 解析 fixture；不发 usage 的降级；畸形 payload 不 panic | 改 |
| `internal/agent/goalloop/implementer.go` | `ACPImplementer.Sink` 字段；`worker.run` onEvent 累加 sink | 改 |

---

## 10. 测试策略（Fake 优先，沿用 A-D §0.3）

- **LEAK1**：`task.Broker` + 临时 store + fake VCS（记录 `RemoveWorktree` 调用）。三路径覆盖：RecordResult finalize、Cancel、RequeueStale 判 failed 各自回收；RequeueStale 判 pending 保留。长跑：提交 K 个任务全超 maxRetries，断言 `len(createdWT)==0`。并发：`-race` 下 sweeper + Cancel + worker RecordResult 并发。
- **LEAK2**：引用既有 `TestRunnerCapsAtRegistryMaxConcurrent` 等守护测试（不重写）；新增仅"深度+并发双超限的错误优先级"断言（若改 `Spawn` 顺序）。
- **CCL1**：`CompactingModel` + `einollm.FakeModel`。构造历史略超 threshold → 首次压缩；追加 tool 消息重回 threshold（cooldown 内）→ 不压缩；追加到 0.95×window → 强制压缩。验证 `keepRecent` /2 桥接注释与既有用例一致。
- **BENCH1**：bench 本身即测试（`go test -bench`）；CI benchstat 比对。零外部依赖（FakeModel/临时 store/`t.TempDir`）。
- **LEAK3**：`acp.FakeAgent` 发 `usage_report` → 断言 sink 累加；不发 → sink 零、turn 完成；畸形 JSON → 不 panic。goalloop 层：`FakeImplementer` 替换为带 sink 的 ACP fake，断言 budget 含子进程 tokens。

---

## 11. 风险与缓解

| 风险 | 缓解 |
|---|---|
| LEAK1 清理误删可复用 worktree | 仅终态回收，`GetTask` 读 status 后判；`delete` 幂等 + RemoveWorktree 容错 |
| LEAK1 与 RAC1 并发改动冲突 | RAC1 先出竞态清单，LEAK1 紧随；`reclaimWorktree` 统一在 `createdWTMu` 下 |
| LEAK2 收紧 `runningLocked` 破坏既有 cap 测试 | 默认仅注释；收紧需同步改测试 + `-race` 验证 |
| CCL1 cooldown 跨 turn 残留误延后 | 时间维 cooldown + `hardForceFraction` 强制兜底；实现时定 per-turn 重置 or 跨 turn（open question） |
| BENCH1 CI 噪声误报 | 多次运行 + benchstat 置信区间；只告警不门禁；分平台基线 |
| LEAK3 各 CLI usage_report 格式不一 | 尽力解析 + 未知字段忽略 + 零值降级；多 adapter fixture |
| D3 同时改 bootstrap/store/config 落点冲突 | 执行前 `git log` + 实测 re-verify（roadmap §1.5 已要求） |
| 与 E2/PROP1 同改 ctxcompact 包 | CCL1 不动 `Run`/`Plan` 核心，仅 CompactingModel 节流；可并行 |

---

## 12. 验收标准（整批）

1. `createdWT` 在任务终态（含 RequeueStale 判失败）被回收；长跑测试断言 map 有界（LEAK1）。
2. 子代理并发上限行为由既有测试守护；深度/并发双上限交互有承重注释（LEAK2 核对完成）。
3. mid-turn 压缩同 turn 不重复（cooldown 生效），逼近硬窗口仍强制；`keepRecent` 双语义文档清晰（CCL1）。
4. 四个基准（VCSCommit/DAGApply/FSEdit/OrchestratorTurn）存在、零外部依赖、CI 记录趋势、人为回归可被发现（BENCH1）。
5. ACP 子进程 usage 进 `UsageSink`，budget 含子进程；不发/畸形 usage 安全降级（LEAK3）。
6. `-race ./internal/task/... ./internal/agent/registry/...` 通过（衔接 RAC1）。
7. 全量 `go test ./...` 绿（含既有 e2e 跳过预期）。

---

## 13. out-of-scope（明确不做）

- 跨重启 `createdWT`/worktree 持久化回收（broker createdWT 是进程内 map，重启即失；worktree 在磁盘残留归 broker 持久化/`doctor` 后续）。
- 压缩摘要算法优化（换模型、改 pin 策略、summary 质量评估）。
- 基准硬门禁阻断合并（只告警 + 趋势）。
- ACP usage 的 stdout 逐行解析（v1 只做 `usage_report` 事件源；stdout 兜底列可选增强）。
- 子代理并发上限的新实现（B1 已做，本批不重复）。
- OTel 采样率调优、`/cost` 子进程明细展示（归 C4/F2-LEAK3 衍生，非本批验收项）。

---

## 14. 需人决策的 open question

1. **LEAK2 范围**：B1/M04b 既已完成，F2 是"纯注释核对"即可，还是仍纳入一轮查漏（如跨重启 worktree 残留、broker 与 registry 是否共享并发预算）？影响预估 0.5d vs 1.5d。
2. **CCL1 cooldown 状态生命周期**：跨 turn 保留（per-model 实例字段，缓存在 `runners sync.Map`）还是每 turn 重置？以及参数（cooldown 时长/ token 间隔、`hardForceFraction` 默认 0.95）走固定默认还是 `compaction.cooldown_*` 可配？
3. **BENCH1 告警阈值 N**：回归 >15% / 20% / 25% 打 warning？PR 跑快速子集、merge/nightly 跑全集的切分边界？
4. **LEAK3 usage 源优先级**：v1 只解析 ACP `usage_report` 事件，是否同时实现 agent stdout 兜底（codex JSONL usage 行 / claudecode 统计行）？budget 超限时是硬停 goal loop 还是仅告警？
