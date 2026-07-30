# ADR-0008: autoVCS 经 context 注入并覆盖调用方 scope

- 状态：accepted
- 日期：2026-07-22

## 背景（Context）

autoVCS 要在 agent 编辑流经 fs 工具时**自动追踪每一次编辑**——agent 无需显式配合。如果靠工具参数传递 VCS scope（"这次编辑属于哪个 repo/worktree"），每个工具的参数 schema 都要加 VCS 字段，调用方（编排器、子代理、ACP agent）都要记得传，极易漏传或传错。同时，当 VCS 已配置时，调用方自己传入的 scope 可能与 VCS 的权威 scope 冲突。

## 决策（Decision）

VCS scope 通过 **context value** 注入（`tools.WithVCS`），而非工具参数。每个编排器 turn 按固定顺序注入：`tools.WithProfile`（权限）→ `tools.WithSubAgentRunner` → `tools.WithVCS`（仅当已配置）。工具从 context 读取 scope（`internal/tools/vcsctx.go`）。

当 VCS 已配置时，其 scope 注入**覆盖**调用方传入的 scope——只有 VCS 为 nil 时，调用方传入的 scope 才保留。

## 后果（Consequences）

> 上下文注入是横切模式，工具参数保持简洁。

- 工具函数签名不需要 VCS 参数；新增工具自动获得追踪能力，只要通过 yanshi 的 fs 工具编辑。
- **不可违反的约束**：
  - 新增需要鉴权/追踪/acting-agent 信息的工具时，**从 context 读取**（`permctx.go`/`vcsctx.go`），**不要塞进工具参数**。
  - **VCS 非 nil 时覆盖调用方 scope** 这一行为必须保留——VCS 的 scope 是权威，调用方传入值仅在 VCS 缺席时才用。
- 代价：调用方传入的 scope 在 VCS 在场时被忽略；这是为"VCS 权威单一来源"付出的取舍。

## 关联

- 来源：synthesis §9.5；`CLAUDE.md`「上下文注入是横切模式」段、`docs/vcs.md`。
- 代码落点：`internal/tools/vcsctx.go`（scope 从 context 读取）；`internal/vcs/`（追踪实现）。
