# 资源参考（Thread / Turn / Item）

v1 Agent API 的三类核心资源与请求/响应形状。字段表由 `go run ./cmd/api-schema -markdown` 从 `internal/api/v1` 生成；不要手改本页的生成区块（修改 `internal/api/v1/types.go` 或 `sdk/schema/v1/agent-api.schema.json`（`internal/api/v1/schema.go::SchemaBytes` 原样返回它）后重生成，CI 守门）。

## Thread

一个持久会话资源。session id 即 thread id；`turns` 仅在快照包含时携带。

<!-- BEGIN GENERATED: api-defs:Thread -->
| 字段 | 类型 | required | 说明 |
|---|---|---|---|
| createdAt | integer | yes | |
| id | string | yes | |
| model | string |  | |
| status | ThreadStatus | yes | |
| thinking | string |  | |
| title | string |  | |
| turns | array |  | |
| updatedAt | integer | yes | |
| version | Version | yes | |
<!-- END GENERATED: api-defs:Thread -->

## Turn

一次用户输入的生命周期。一个 thread 同时至多一个活动 turn；`completedAt` 到达终态前省略。

<!-- BEGIN GENERATED: api-defs:Turn -->
| 字段 | 类型 | required | 说明 |
|---|---|---|---|
| completedAt | integer |  | |
| id | string | yes | |
| input | string | yes | |
| startedAt | integer | yes | |
| status | TurnStatus | yes | |
| threadId | string | yes | |
| version | Version | yes | |
<!-- END GENERATED: api-defs:Turn -->

## Item

最小可流式事件。每个 thread 内 `sequence` 从 1 单调递增，客户端按序消费。未知字段解码时忽略；未知 `type` 客户端忽略但服务端保留为 `event.<legacyType>`（见 [events.md](events.md)）。

<!-- BEGIN GENERATED: api-defs:Item -->
| 字段 | 类型 | required | 说明 |
|---|---|---|---|
| error | string |  | |
| fileChange | FileChange |  | |
| id | string | yes | |
| sequence | integer | yes | |
| status | string |  | |
| structuredResult | any |  | |
| text | string |  | |
| threadId | string | yes | |
| toolArgs | string |  | |
| toolName | string |  | |
| turnId | string | yes | |
| type | string | yes | |
| version | Version | yes | |
<!-- END GENERATED: api-defs:Item -->

## 请求参数

### ThreadStartParams

`thread/start` 的输入。所有字段可选；服务端填默认（title="New thread"、status="active" 等）。

<!-- BEGIN GENERATED: api-defs:ThreadStartParams -->
| 字段 | 类型 | required | 说明 |
|---|---|---|---|
| model | string |  | |
| thinking | string |  | |
| title | string |  | |
| version | Version |  | |
<!-- END GENERATED: api-defs:ThreadStartParams -->

### ThreadResumeParams

`thread/resume` 的输入。`threadId` 必填。

<!-- BEGIN GENERATED: api-defs:ThreadResumeParams -->
| 字段 | 类型 | required | 说明 |
|---|---|---|---|
| threadId | string | yes | |
| version | Version |  | |
<!-- END GENERATED: api-defs:ThreadResumeParams -->

### ThreadInterruptParams

`thread/interrupt` 的输入，取消一个 thread 的活动 turn。`turnId` 可选；设置时服务端拒绝取消不同的 turn（防过期客户端状态）。重复 interrupt 幂等。

<!-- BEGIN GENERATED: api-defs:ThreadInterruptParams -->
| 字段 | 类型 | required | 说明 |
|---|---|---|---|
| threadId | string | yes | |
| turnId | string |  | |
| version | Version |  | |
<!-- END GENERATED: api-defs:ThreadInterruptParams -->

### TurnStartParams

`turn/start` 的输入。`threadId`、`input` 必填。`outputSchema` 是结构化 turn 的可选 JSON Schema；`images` 是可选图像附件（Tier G，ADDITIVE）。

<!-- BEGIN GENERATED: api-defs:TurnStartParams -->
| 字段 | 类型 | required | 说明 |
|---|---|---|---|
| context | array |  | |
| images | array |  | |
| input | string | yes | |
| model | string |  | |
| outputSchema | any |  | |
| thinking | string |  | |
| threadId | string | yes | |
| version | Version |  | |
<!-- END GENERATED: api-defs:TurnStartParams -->

## 响应

### ThreadStartResponse

`thread/start` 的结果，携带新建的 Thread。

<!-- BEGIN GENERATED: api-defs:ThreadStartResponse -->
| 字段 | 类型 | required | 说明 |
|---|---|---|---|
| thread | Thread | yes | |
| version | Version | yes | |
<!-- END GENERATED: api-defs:ThreadStartResponse -->

### ThreadResumeResponse

`thread/resume` 的结果，携带 Thread 与服务端能再推导出的历史 Items。

<!-- BEGIN GENERATED: api-defs:ThreadResumeResponse -->
| 字段 | 类型 | required | 说明 |
|---|---|---|---|
| thread | Thread | yes | |
| version | Version | yes | |
<!-- END GENERATED: api-defs:ThreadResumeResponse -->

### TurnStartResponse

`turn/start` 的结果，携带刚启动的 Turn。turn 期间产生的 Items 经 SSE / JSON-RPC 通知流式到达。

<!-- BEGIN GENERATED: api-defs:TurnStartResponse -->
| 字段 | 类型 | required | 说明 |
|---|---|---|---|
| turn | Turn | yes | |
| version | Version | yes | |
<!-- END GENERATED: api-defs:TurnStartResponse -->

### InterruptResponse

`thread/interrupt` 的结果。`ok=true` 表示中断被接受（或 thread 无活动 turn 可中断——幂等）。

<!-- BEGIN GENERATED: api-defs:InterruptResponse -->
| 字段 | 类型 | required | 说明 |
|---|---|---|---|
| ok | boolean | yes | |
| threadId | string | yes | |
| turnId | string |  | |
| version | Version | yes | |
<!-- END GENERATED: api-defs:InterruptResponse -->
