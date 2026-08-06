# JSON-RPC 2.0 app-server（`yanshi app`）

`yanshi app` 在 stdio 上跑一个 JSON-RPC 2.0 app-server，把 v1 agent service 暴露给本地 supervisor（IDE 扩展、notebook 运行时、CLI 包装）。它驱动**同一个** `*v1.Service`，所以 thread/turn/item 语义与 HTTP/SSE 不漂移。

## 方法

| 方法 | 说明 |
|---|---|
| `initialize` | 握手；返回版本与能力 |
| `capabilities` | 返回 `Capabilities`（methods / itemTypes / unknownFields / stream） |
| `thread/start` | 创建 thread（params：[ThreadStartParams](resources.md#threadstartparams)） |
| `thread/resume` | 按 id 加载 thread（不含历史 item —— v1 不承诺跨进程事件回放，item 只经流式通道到达） |
| `thread/interrupt` / `turn/interrupt` | 取消一个 thread 的活动 turn（幂等） |
| `turn/start` | 启动 turn（params：[TurnStartParams](resources.md#turnstartparams)）；items 经通知流式到达 |
| `config/read` | 读运行时配置（见下方「运行时配置存放在哪」） |
| `config/write` | 写运行时配置；成功即已落盘 |
| `shutdown` | 优雅关闭 |

未知方法返回 `-32601 method not found`。方法名与错误码定义在 `internal/appserver/rpc.go` / `server.go`。

## 标准错误码

JSON-RPC 2.0 标准码；dispatcher 把每条错误路径映射到其中之一，客户端可在一个小而稳定的集合上分支：

| 码 | 含义 |
|---|---|
| `-32700` | parse error（JSON 解析失败） |
| `-32600` | invalid request（`jsonrpc!="2.0"` 或 method 空） |
| `-32601` | method not found |
| `-32602` | invalid params |
| `-32603` | internal error |

## `item/updated` 通知

turn 期间产生的 items 以 `item/updated` 通知（`RPCNotification`，**无 ID**）流式推给客户端，每行一个 JSON 对象。通知不回响应。

## ID 回显

`id` 是 `json.RawMessage`——string / number / null 原样回显，响应里的 `id` 与请求字节一致。缺 ID 的消息是 notification，不回响应。

## 与 HTTP/SSE 的关系

> app-server 与 HTTP/SSE 共用同一个 `*v1.Service`，语义不漂移。

不对称点：HTTP 的 SSE 路径用**静态权限 profile**（无交互式弹窗）；WebSocket 与 app-server 走同一个 service，权限模式按 transport 配置（见 [../adr/0007-ws-holds-history-sse-replays-shared-proto.md](../adr/0007-ws-holds-history-sse-replays-shared-proto.md) 与 [../adr/0010-sse-static-profile-no-interactive-perm.md](../adr/0010-sse-static-profile-no-interactive-perm.md)）。

## 运行时配置存放在哪

`config/read|write` 操作的是一个**独立的 JSON sidecar**，路径由 `-config` 派生：
`config.yaml` → `config.appstate.json`（`internal/appserver::SidecarPath`）。

**不写进 `config.yaml` 本身**是有意的：那份 YAML 由操作员手工维护、带注释、
且在这套 RPC 存在之前就被 bootstrap 读过；让一个 JSON-RPC 调用方去重写它，
等于为了存一个 bootstrap 从不读的键而毁掉操作员的注释与排版。

这两个方法此前**根本不落盘** —— `cmd/yanshi/app.go` 无条件构造
`appserver::MemoryConfig`，传不传 `-config` 都一样，于是 `config/write` 收下值、
回报成功、进程退出即丢，而本文档一直把它们描述成「读/写运行时配置」。
照着这份文档配置的机群会在每次重启时静默复位。

**秘密路径在两个方向上都被拒绝**（`token` / `api_key` / `apikey` / `secret`，
以及任何含 `password` 的 dot-segment），且拒绝发生在 JSON decode **之前** ——
落盘之后这条更重要：泄漏一旦到达这个后端就是持久的。
