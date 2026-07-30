// Minimal end-to-end TypeScript SDK example against a local fake-model backend.
//
//   1. Start the backend:  ./yanshi serve --fake-model -addr 127.0.0.1:8080
//   2. Run this example:   npx tsx index.ts   (or compile + run)
//
// Loopback (127.0.0.1) needs no bearer token. The SDK types come from
// @x6nux/yanshi-sdk (mapped to ../../sdk/ts/src in tsconfig.json).
import { AgentClient } from "@x6nux/yanshi-sdk";

async function main(): Promise<void> {
  const client = new AgentClient({ baseUrl: "http://127.0.0.1:8080" });

  // 1. Create a thread.
  const started = await client.start({ title: "sdk-typescript demo" });
  const threadId = started.thread.id;
  console.log("thread:", threadId);

  // 2. Run a turn, consuming the item stream.
  for await (const item of client.run(threadId, { input: "hello" })) {
    console.log("item:", item.type, item.text ?? item.toolName ?? "");
  }

  // 3. Cancel if still running (idempotent).
  await client.interrupt(threadId);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
