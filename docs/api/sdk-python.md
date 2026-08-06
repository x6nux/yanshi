# Python SDK（`yanshi_sdk`）

`yanshi_sdk` 是 v1 Agent API 的 Python 客户端（异步）。包源码在 `sdk/python/src/yanshi_sdk/`。

## 最小端到端

```python
import asyncio
from yanshi_sdk import AgentClient, RunTurnParams

async def main() -> None:
    client = AgentClient(base_url="http://127.0.0.1:8080")

    # 1. 建一个 thread
    started = await client.start()
    thread_id = started.thread.id

    # 2. 起 turn，消费 item 流
    async for item in client.run(thread_id, RunTurnParams(input="hello")):
        # 属性名是 tool_name；toolName 只是 pydantic 的 wire alias，
        # 直接属性访问会抛 AttributeError
        print(item.type, item.text or item.tool_name or "")

    # 3. 需要时取消
    await client.cancel(thread_id)
    await client.aclose()

asyncio.run(main())
```

`run` 内部做 `turn/start` 并把 item 流作为 async iterable 暴露；`interrupt` / `cancel` 取消活动 turn（幂等）；`aclose()` 关闭底层 transport。

## 对 fake-model 后端跑通

```sh
./yanshi serve --fake-model -addr 127.0.0.1:8080   # loopback 免 token
# 另一个终端：PYTHONPATH=sdk/python/src 把 base_url 指向 http://127.0.0.1:8080
```

loopback（127.0.0.1）免 bearer token。完整可跑样例见 [../../examples/sdk-python/](../../examples/sdk-python/)。

## 类型

`sdk/python/src/yanshi_sdk/` 的数据类与 [resources.md](resources.md) 的生成表同源（mirror `internal/api/v1/types.go`）。修改 wire contract 后同步更新。
