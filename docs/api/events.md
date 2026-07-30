# 事件参考（item.type）

turn 期间产生的事件以 `Item` 流的形式到达（HTTP/SSE 经 ServerFrame，JSON-RPC 经 `item/updated` 通知）。每个 Item 的 `type` 是下列枚举之一，或一个 `event.<legacyType>` 回退值。

## item.type 枚举

> 这是稳定契约的一部分，重命名即破坏性变更。枚举值源自 `internal/api/v1/types.go` 的常量；CI 守门断言每个值都在常量里出现。

| type | 含义 |
|---|---|
| `turn.started` | turn 已启动 |
| `message.delta` | 模型文本输出的增量 chunk |
| `reasoning.delta` | 模型推理（thinking）的增量 chunk |
| `tool.call` | 一个工具调用 |
| `tool.result` | 一个工具结果 |
| `tool.progress` | 工具执行进度（chunk / progress） |
| `structured.result` | 结构化 turn 的最终结果（配合 `outputSchema`） |
| `turn.error` | turn 出错（`error` 字段携带信息） |
| `turn.completed` | turn 完成 |

## legacy frame → item 映射

HTTP/SSE 传输底层用一套 legacy `proto.ServerFrame` 词表。`v1.ItemFromServerFrame`（`internal/api/v1/events.go`）是**唯一**的 legacy → v1 映射，集中维护以防两条传输对同一事件产生不同解读：

| legacy frame type | item.type |
|---|---|
| `agent_chunk` | `message.delta` |
| `thinking` | `reasoning.delta` |
| `tool_call` | `tool.call` |
| `tool_result` | `tool.result` |
| `tool_chunk` / `tool_progress` | `tool.progress` |
| `structured_result` | `structured.result` |
| `error` | `turn.error` |
| `done` | `turn.completed` |
| _（其它未知）_ | `event.<legacyType>` |

## 未知帧保留不丢

> 未知 legacy frame 映射成 `event.<legacyType>` 而非被丢弃——客户端忽略未知 item type，但服务端保留它，这样线上不会因为"新 frame 类型先到、v1 映射还没学"而静默丢事件（见 [README.md](README.md) 的未知字段策略）。
