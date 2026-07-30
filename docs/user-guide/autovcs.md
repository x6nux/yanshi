# autoVCS（自动版本控制）

autoVCS 是 yanshi 内置的、基于 SQLite 的类 git 版本控制。它的核心卖点是**自动追踪 agent 的每一次编辑**——agent 无需显式配合，只要通过 yanshi 的 fs 工具编辑文件，编辑就被捕获进 VCS。

## 自动追踪怎么工作

编排器每个 turn 通过 context 注入 VCS scope（见 [../adr/0008-autovcs-context-injection-overrides-scope.md](../adr/0008-autovcs-context-injection-overrides-scope.md)）。fs 工具编辑文件时从 context 读取 scope，把这次编辑记进 VCS。对 agent 来说是透明的：它只管调 fs 工具，追踪自动发生。

> 当 VCS 已配置时，其 scope **覆盖**调用方传入的 scope——VCS 的 scope 是权威单一来源。

## main 与 worktree

- `main` 是规范主干（仓库根是它的工作副本）。聊天 / 编排器的编辑追踪到 `main`。
- worktree 从 `main_head` 分出，位于 `~/.yanshi/worktrees/`（由 [configuration.md](configuration.md) 的 `vcs.worktree_dir` 配置）。task-agent 与 ACP-agent 的编辑追踪到当前活动的 worktree。
- worktree 的更改通过树级三方合并合并回 main。

## 对外暴露

autoVCS 还作为 MCP server 暴露（`yanshi vcs-mcp`，环境变量驱动），把 `vcs_*` 工具交付给被拉起的外部 agent（ACP）。详见 [entrypoints.md](entrypoints.md) 的 vcs-mcp 段。

> 内部合并算法与数据模型细节见 [../vcs.md](../vcs.md) 与 [../adr/0009-sqlite-pseudogit-tree-merge.md](../adr/0009-sqlite-pseudogit-tree-merge.md)；本页只给用户面视角。
