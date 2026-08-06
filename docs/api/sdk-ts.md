# TypeScript SDK（`@x6nux/yanshi-sdk`）

`@x6nux/yanshi-sdk` 是 v1 Agent API 的类型化 TypeScript 客户端。类型是 `sdk/ts/v1.ts` 里**手工维护**的镜像，transport 支持 HTTP + SSE + WebSocket。

## 最小端到端

```ts
import { AgentClient } from "@x6nux/yanshi-sdk";

const client = new AgentClient({ baseUrl: "http://127.0.0.1:8080" });

// 1. 建一个 thread
const { thread } = await client.start({ title: "demo" });

// 2. 起 turn，消费 item 流
for await (const item of client.run(thread.id, { input: "hello" })) {
  console.log(item.type, item.text ?? item.toolName ?? "");
}

// 3. 需要时中断
await client.interrupt(thread.id);
```

`run` 内部做 `turn/start` 并把 SSE / WS 的 item 流作为 async iterable 暴露；`interrupt` / `cancel` 取消活动 turn（幂等）。

## 对 fake-model 后端跑通

```sh
./yanshi serve --fake-model -addr 127.0.0.1:8080   # loopback 免 token
# 另一个终端：把 baseUrl 指向 http://127.0.0.1:8080 跑上面的脚本
```

loopback（127.0.0.1）免 bearer token。完整可跑样例（含依赖安装步骤）见 [../../examples/sdk-typescript/](../../examples/sdk-typescript/)。

## 类型

`sdk/ts/v1.ts` 是契约的**手工镜像**（`Thread`/`Turn`/`Item`/各 params/responses）。改 wire contract 时手改它。

它曾自称由 `cmd/api-schema -out` 生成，而那不是生成器 —— 命令里逐字符抄着这份文件的一份 Go 字符串字面量拷贝，跑它再对拍恒等，因为两边是同一段转写。那一半已删除。

守门人是 `internal/api/v1/parity_test.go::TestContractParityAcrossFourSources`：它比较 Go struct、JSON Schema、本文件与 Python 模型四路的字段集合，每条差异必须在表里具名带理由。漏改一处会在那里变红。
