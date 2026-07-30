# ADR-0006: 压缩双路径共享统一核心、单次严格不超窗口

- 状态：accepted
- 日期：2026-07-22

## 背景（Context）

yanshi 有两条压缩触发路径：mid-turn（`CompactingModel` 在 ReAct 迭代之间触发）与 pre-turn（`MaybeCompact` 在 user_message 之前触发）。如果两条路径各自实现一套压缩流水线，行为会漂移：同一种历史在两条路径下被压缩成不同结果，且两边各自处理"summary 输入放不进窗口"的方式不一致（直接发可能触发 provider 400）。

## 决策（Decision）

两条路径都委托给**统一核心** `ctxcompact.Run`：

- `Plan` 决定 pin 哪些消息原文（尾部 + user 原文 + working-set 路径 + 错误/diff 标记）。
- `EnforceToolCallPairs` 用 fixpoint 算法保证 tool_call/result 配对不被切断。
- `RunSummary` 在 summary 输入 ≤ 0.9×窗口时走**cache-aligned 单次**总结；超过时走**携带式分块**——每次调用**严格不超过窗口**（滚动执行：每块总结后折进下一块）。
- `Assemble` 把 summary 作为 user+sentinel 消息放历史末尾（见 [ADR-0005](0005-compaction-summary-user-role.md)）。

## 后果（Consequences）

> 修改核心时两条路径同时受影响——这是有意为之的单一真相源。

- mid-turn 与 pre-turn 行为一致；没有"哪条路径触发结果不同"的漂移。
- **不可违反的约束**：携带式分块的**每次单次调用严格不超窗口**——这是防止 provider 400 的硬保证。`RunSummary` 的 cache-aligned 单次 vs 分块阈值由 `chunk_threshold`（默认 0.9）控制。
- 桥接细节：`KeepRecent` 在 `CompactingModel` 里是消息数、在 `ctxcompact.PlanOpts` 里是对数（pair 数），桥接是 `/2`。上下文窗口按 provider `context_window`，`/model` 切换自动用新窗口。

## 关联

- 来源：synthesis §9.3；`CLAUDE.md`「编排器 / 上下文压缩」段、`docs/compaction.md`。
- 代码落点：`internal/ctxcompact/`（`Run` 统一核心 + 携带式分块）。
