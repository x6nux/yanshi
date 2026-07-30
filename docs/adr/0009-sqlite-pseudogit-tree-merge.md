# ADR-0009: autoVCS 用 SQLite 类 git 树级三方合并

- 状态：accepted
- 日期：2026-07-22

## 背景（Context）

autoVCS 要给 agent 的编辑提供版本控制（worktree、分支、合并），用于 goalloop 的实现-评估-回滚循环与 ACP agent 的隔离工作副本。引入真实 git 作为依赖意味着运行环境必须装 git、跨进程调用 git CLI、处理 git 的复杂状态机——对一个"嵌入 Go 二进制、零外部依赖"的目标是倒退。

## 决策（Decision）

autoVCS 是基于 **SQLite 的类 git VCS**，完全嵌入 yanshi 二进制：`main` 是规范主干（仓库根是它的工作副本）；worktree 从 `main_head` 分出，位于 `~/.yanshi/worktrees/`；worktree 的更改通过**树级三方合并**合并回 main。聊天/编排器的编辑追踪到 `main`；task-agent 与 ACP-agent 的编辑追踪到当前活动的 worktree。

## 后果（Consequences）

> 嵌入 Go 二进制，无需安装 git。

- 零外部依赖、单一二进制；VCS 数据与 session/task 共用同一 SQLite 存储。
- **不可违反的约束**：合并语义是**树级三方合并**，分支粒度；不要把它当作完整 git（它不实现 git 的全部能力）。`main` 是权威主干，worktree 合并目标始终是 main。
- 代价：不是真实 git，无法与外部 git 仓库互操作；这是为"零依赖嵌入"付出的取舍。VCS 还作为 MCP server 暴露（`yanshi vcs-mcp`），交付给被拉起的 ACP agent。

## 关联

- 来源：synthesis §9.5；`CLAUDE.md`「autoVCS」段、`docs/vcs.md`。
- 代码落点：`internal/vcs/`（树级三方合并）。
