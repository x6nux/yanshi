# ADR-0007: WS 持有历史、SSE 回放、共用一套帧协议

- 状态：accepted
- 日期：2026-07-22

## 背景（Context）

yanshi 有两条 HTTP 传输：WebSocket（`/api/v1/chat/ws`，主）与 SSE（`/api/v1/chat`，备）。两者的语义不对称：WS 是单一持久双向连接，服务端可以持有会话历史并接受中途的控制帧（取消、交互式权限、流式压缩）；SSE 是无状态的单向流，每次请求由客户端回放它持有的历史。如果两条传输各定义一套事件词表，事件语义会在两边漂移。

## 决策（Decision）

WS 与 SSE **共用同一套 JSON 帧词表**（`ClientFrame`/`ServerFrame`，定义在 `internal/proto/frame.go`）。新增事件类型只在 `frame.go` 加一处，两边自动一致。关键不对称点固化在实现里：WS 在服务端持有历史；SSE 每次请求回放客户端持有的历史。

## 后果（Consequences）

> 帧词表是客户端和服务端的契约接口，是单一真相源。

- 两条传输不会对同一个事件产生不同解读；维护一份词表而非两份。
- **不可违反的约束**：**新增帧类型时必须同时更新 `ws.go` 与 SSE handler**（`internal/api/http/chat.go` 的 `handleSSEInternal`），否则两条传输对同一帧的处理会分叉。SSE handler 通过 `ServerFrame.SSEEvent()` 输出 `event:`/`data:` 行。

## 关联

- 来源：synthesis §9.4；`CLAUDE.md`「两种传输、一套通信协议」段。
- 代码落点：`internal/proto/frame.go`（共享词表）；`internal/api/http/ws.go`（WS 持有历史）；`internal/api/http/chat.go`（`handleSSEInternal` 回放）。
