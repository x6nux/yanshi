# E2 Race Findings 登记表

> **生成日期**：2026-07-22
> **归属**：E2（RAC1）→ F2（LEAK1/BENCH1/GOV）
> **来源**：代码审查 + `-race` 测试 + spec §7 预判

## 登记项

### F-1: `createdWT` 在 RequeueStale 路径不回收

| 字段 | 值 |
|---|---|
| 位置 | `internal/task/broker.go` `RequeueStale` |
| 现象 | `createdWT[taskID]worktreeID` 在 stale-requeue 后不被 reclaim；`len(createdWT)` 随长跑单调增长 |
| 分类 | leak |
| 归属 | **LEAK1** 修复 |
| 当前处置 | RAC1 `TestBroker_LeakProbeCreatedWT` 用 `assert.GreaterOrEqual` 记录现状（绿）；F2 翻转为 `assert.Zero` |

### F-2: Claim 重入时 worktree_id 残留

| 字段 | 值 |
|---|---|
| 位置 | `internal/task/broker.go` `Claim` |
| 现象 | RequeueStale 把 task 回 pending 但不清 `worktree_id`；重 Claim 时 `got.WorktreeID != ""` 跳过建新 WT → 首次建的 WT 被孤儿化（无人 reclaim） |
| 分类 | leak |
| 归属 | **LEAK1** 修复 |
| 当前处置 | 登记不修；`TestBroker_LeakProbeCreatedWT` 间接覆盖 |

### F-3: `runnerCacheKey.model` 为接口类型

| 字段 | 值 |
|---|---|
| 位置 | `internal/agent/orchestrator/orchestrator.go` |
| 现象 | `runnerCacheKey{model model.BaseChatModel, mode runnerToolMode}` 中 model 是接口字段。作为 `sync.Map` 键要求动态类型可比较；当前 model 都是指针（可比较），安全。若未来出现非可比较具体类型则 panic |
| 分类 | latent-footgun |
| 归属 | GOV/文档 |
| 当前处置 | 登记；测试注释已标注 |

### F-4: `runnerFor` Load→build→LoadOrStore 冗余构建

| 字段 | 值 |
|---|---|
| 位置 | `internal/agent/orchestrator/orchestrator.go` `runnerFor` |
| 现象 | 两个 goroutine 同时 miss cache 都会 build（expensive model 构造），胜者存、败者弃。非 data race，是性能冗余 |
| 分类 | perf |
| 归属 | BENCH1 |
| 当前处置 | 登记；`TestRunners_SameModelReturnsSamePointer` 已确认最终一致性 |

### F-5: `globToRegexp` 每调用重编译 regex

| 字段 | 值 |
|---|---|
| 位置 | `internal/guard/glob.go` |
| 现象 | `MatchGlob` 每次调用都 `globToRegexp` → `regexp.Compile`；`guard.Check` 对每 pattern 每维每调用都编译，是热路径性能债 |
| 分类 | perf |
| 归属 | BENCH1/E3 |
| 当前处置 | 登记；FUZ1 `FuzzMatchGlob` 已覆盖此路径 |

### F-6: `Registry.entries` 无锁

| 字段 | 值 |
|---|---|
| 位置 | `internal/agent/registry/registry.go` |
| 现象 | `Registry.entries map[string]Entry` 无 mutex；`Register`/`Get`/`All`/`ByCapability` 均无同步。实际仅 bootstrap 单线程写入、后续只读 |
| 分类 | latent-footgun |
| 归属 | GOV/F2 |
| 当前处置 | RAC1 `TestRegistry_BootstrapFrozenConvention` 文档化此契约 |

## CI `-skip` 排除记录

| 测试 | 位置 | issue # (required) | `-skip` 表达式 | F2 修后去除？ |
|---|---|---|---|---|
| | | | | |
