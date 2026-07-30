# TypeScript SDK（`@x6nux/yanshi-sdk`）

`@x6nux/yanshi-sdk` 是 v1 Agent API 的类型化 TypeScript 客户端。类型由 `cmd/api-schema` 生成（见 `sdk/ts/v1.ts`），transport 支持 HTTP + SSE + WebSocket。

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

`sdk/ts/v1.ts` 是 `cmd/api-schema -out` 生成的契约镜像（`Thread`/`Turn`/`Item`/各 params/responses），与 [resources.md](resources.md) 的生成表同源。修改 wire contract 后重生成。
