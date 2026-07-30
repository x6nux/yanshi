# Yanshi Agent API v1 参考

yanshi 暴露一个 **v1 Agent API**：Thread / Turn / Item 资源 + 一套 item 事件流。HTTP（WebSocket 主 / SSE 备）与 JSON-RPC app-server **共用同一个 `*v1.Service`**，所以资源 / 事件语义在两条传输间不漂移。

## 版本契约总述

- **版本**：`version: "v1"`。每个资源 / 请求 / 响应都带 `version` 字段；HTTP 还经 `X-Yanshi-API-Version` 头广告；JSON-RPC 在 result/envelope 内携带（`jsonrpc:"2.0"` 是传输版本，**不**替代资源版本）。
- **camelCase**：所有 JSON 字段名 camelCase；Go struct tag 与 JSON Schema 在 `internal/api/v1` 强制（见 [schema.md](schema.md)）。
- **未知字段策略**：`unknownFields: "ignored"`——客户端解码时忽略未知字段；服务端对未知 `item.type` 保留为 `event.<legacyType>` 而非丢弃（见 [events.md](events.md)）。
- **容忍语义**：`additionalProperties: true`，新增字段是 ADDITIVE（如 Tier G 的 `images`）。

## 本节页面

| 页面 | 内容 |
|---|---|
| [resources.md](resources.md) | Thread / Turn / Item + params/responses 字段表（生成） |
| [events.md](events.md) | item.type 枚举 + legacy frame → item 映射 |
| [schema.md](schema.md) | 完整 JSON Schema（生成） |
| [jsonrpc.md](jsonrpc.md) | app-server JSON-RPC 2.0 方法 / 错误码 / 通知 |
| [sdk-ts.md](sdk-ts.md) | TypeScript SDK 最小端到端 |
| [sdk-python.md](sdk-python.md) | Python SDK 最小端到端 |

## 相关

- 契约交接与版本矩阵：[`sdk/schema/CONTRACT_HANDOFF.md`](../../sdk/schema/CONTRACT_HANDOFF.md)。
- 传输不对称（WS 持有历史 / SSE 回放 / 共享帧协议）：[../adr/0007-ws-holds-history-sse-replays-shared-proto.md](../adr/0007-ws-holds-history-sse-replays-shared-proto.md)。
- SSE 静态 profile（不支持交互式权限）：[../adr/0010-sse-static-profile-no-interactive-perm.md](../adr/0010-sse-static-profile-no-interactive-perm.md)。
