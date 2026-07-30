# ADR-0005: 压缩 summary 用 User 角色（非 System 角色）

- 状态：accepted
- 日期：2026-07-22

## 背景（Context）

上下文压缩（compaction）把历史消息总结成一段 summary 放回历史末尾，腾出窗口空间。summary 该用什么消息角色？最自然的选择似乎是 `system`（它是对话级的元信息）。但编排器本身已经注入了一条 system prompt（ReAct 的角色/工具说明）。把 summary 也放成 system 消息会产生**双 system** 冲突：多数 provider 把多条 system 消息拼接或只保留最后一条，行为不可预测，且 summary 可能覆盖编排器的 system prompt。

## 决策（Decision）

`Assemble` 把 summary 作为一条 **user 角色消息**（带一个 sentinel 标记）放进历史末尾，而不是 system 角色。用一个可识别的 sentinel 前缀让后续逻辑能区分"这是压缩 summary"与"这是真实用户输入"。

## 后果（Consequences）

> 避免与编排器 system prompt 的双 system 冲突。

- summary 进入 user 槽，provider 不会把它和 system prompt 竞争/覆盖。
- **不可违反的约束**：summary **永远不要用 System 角色**。改动 `Assemble` 时必须保持 user + sentinel 的形态。
- 代价：summary 占用一个 user 消息位（而非元信息位）；这是为兼容性付出的可控代价。

## 关联

- 来源：synthesis §9.3；`CLAUDE.md`「编排器 / 上下文压缩」段、`docs/compaction.md`。
- 代码落点：`internal/ctxcompact/`（`Assemble` 的 user + sentinel 装配）。
