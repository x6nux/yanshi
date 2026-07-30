# ADR-0010: SSE 路径永久静态 profile，不支持交互式权限

- 状态：accepted
- 日期：2026-07-22

## 背景（Context）

交互式权限模式（`default`/`allow-edits`/`yolo`/`auto`）需要服务端与用户之间的往返：当工具调用需要授权时，服务端通过 WebSocket 询问用户、等待应答。这依赖一条**持久双向连接**。SSE 是单向无状态流（服务端 → 客户端），每次请求由客户端回放历史，服务端无法在请求中途向客户端征询权限并等待应答。

## 决策（Decision）

SSE 路径**永久使用一个静态权限 profile**（由后端配置决定），从不触发交互式权限弹窗。交互式权限仅在 WS 路径可用。`handleSSEInternal`（`internal/api/http/chat.go`）始终用静态 profile 处理整条请求。

## 后果（Consequences）

> SSE 的无状态性决定了它不能做交互式权限。

- SSE 保持无状态、可回放；权限策略在请求开始时就完全确定。
- **不可违反的约束**：**不可在 SSE 路径引入交互式权限**——SSE 没有持久连接来承载权限往返。WS 是唯一支持交互式权限的传输。
- 代价：SSE 的权限粒度粗（整条请求一个静态 profile），不能逐工具征询；这是传输能力差异的必然结果，与 [ADR-0007](0007-ws-holds-history-sse-replays-shared-proto.md) 的不对称点一致。

## 关联

- 来源：synthesis §9.2；`CLAUDE.md`「两种传输、一套通信协议」「Guard」段。
- 代码落点：`internal/api/http/chat.go`（`handleSSEInternal` 静态 profile）；权限模式定义见 `internal/guard/mode.go`。
