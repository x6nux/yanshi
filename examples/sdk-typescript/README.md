# sdk-typescript

TypeScript SDK 端到端示例：建 thread → 起 turn 消费 item 流 → cancel。用 `@x6nux/yanshi-sdk`（映射到 `../../sdk/ts/src`）。

## 跑

两步（两个终端）：

```sh
# 1. 起一个 fake-model 后端（loopback 免 token）
./yanshi serve --fake-model -addr 127.0.0.1:8080

# 2. 跑示例（需先在 sdk/ts 装依赖：npm --prefix sdk/ts install）
npx --prefix sdk/ts tsx examples/sdk-typescript/index.ts
```

## 类型检查

```sh
npm --prefix sdk/ts install   # 一次
npx tsc --noEmit --project examples/sdk-typescript/tsconfig.json
```

`tsconfig.json` 用 `paths` 把 `@x6nux/yanshi-sdk` 指向本地 SDK 源码、用 `typeRoots` 借 `sdk/ts` 的 `@types/node`，所以无需在示例目录另装依赖。详见 [../../docs/api/sdk-ts.md](../../docs/api/sdk-ts.md)。
