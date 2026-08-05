# 架构决策记录（ADR）索引

> **ADR 是单决策的演进档案**：每条记录一个架构决策的背景、决策、后果与不可违反的约束，以及它后续是否被取代。**`CLAUDE.md` 是全景当前态**（仓库怎么工作的权威描述），ADR 补充"为什么是这个决策、它曾经是什么、被什么取代"。新架构决策或修改承重决策时，先写/更新一条 ADR（见 [CONTRIBUTING.md](../../CONTRIBUTING.md)）。

ADR 不复制 `CLAUDE.md` 的内容：每条 ADR 的"关联"段指向 `CLAUDE.md` 对应段与代码落点，独有价值是**带 `superseded` 状态的演进历史**与**不可违反的约束清单**。

## 模板

新建 ADR 时从 [0000-template.md](0000-template.md) 复制，编号取当前最大编号 +1。

## 决策清单

| 编号 | 标题 | 状态 | 来源 |
|---|---|---|---|
| [0001](0001-unknown-tools-handler-result-not-error.md) | UnknownToolsHandler 以结果返回而非 error | accepted | synthesis §9.1 |
| [0002](0002-runners-cache-key-model-pointer.md) | runners 缓存以 model 指针为键 | accepted | synthesis §9.1 |
| [0003](0003-guard-fail-closed-empty-allow.md) | Guard fail-closed：空 Allow 拒绝一切 | accepted | synthesis §9.2 |
| [0004](0004-guard-stateless-and-shell-metachar-hardblock.md) | Guard 无状态 + shell 元字符硬拦截 | accepted | synthesis §9.2 |
| [0005](0005-compaction-summary-user-role.md) | 压缩 summary 用 User 角色而非 System | accepted | synthesis §9.3 |
| [0006](0006-compaction-unified-core-strict-window.md) | 压缩双路径共享统一核心、单次严格不超窗口 | accepted | synthesis §9.3 |
| [0007](0007-ws-holds-history-sse-replays-shared-proto.md) | WS 持有历史、SSE 回放、共用一套帧协议 | accepted | synthesis §9.4 |
| [0008](0008-autovcs-context-injection-overrides-scope.md) | autoVCS 经 context 注入并覆盖调用方 scope | accepted | synthesis §9.5 |
| [0009](0009-sqlite-pseudogit-tree-merge.md) | autoVCS 用 SQLite 类 git 树级三方合并 | accepted | synthesis §9.5 |
| [0010](0010-sse-static-profile-no-interactive-perm.md) | SSE 路径永久静态 profile，不支持交互式权限 | accepted | synthesis §9.2 |
| [0011](0011-ledger-clause-level-evidence-handshake.md) | 台账终态证据逐句对账 + 测试侧双向握手（GOV8） | accepted | S0/W1 评审 |
| [0012](0012-gate-runs-argv-directly-not-through-shell-session.md) | task_gate_run 直接执行 argv，不经 shell session | accepted | W3 裁定 3 |
| [0013](0013-mid-turn-compaction-token-dimension.md) | mid-turn 压缩的 token 会计以未压缩历史为统一量纲 | accepted | W4 |

## 状态图例

- **accepted**：决策生效中，约束必须遵守。
- **deprecated**：决策已弃用（但未被另一条 ADR 取代）。
- **superseded by ADR-MMMM**：被 ADR-MMMM 取代；本条保留为历史。
- **proposed**：草案，尚未被采纳。
