"""Minimal end-to-end Python SDK example against a local fake-model backend.

1. Start the backend:  ./yanshi serve --fake-model -addr 127.0.0.1:8080
2. Run this example:  PYTHONPATH=sdk/python/src python examples/sdk-python/main.py

Loopback (127.0.0.1) needs no bearer token. The SDK lives in
sdk/python/src/yanshi_sdk.
"""
import asyncio
import sys

from yanshi_sdk import AgentClient, RunTurnParams


async def main() -> None:
    client = AgentClient(base_url="http://127.0.0.1:8080")

    # 1. Create a thread.
    started = await client.start()
    thread_id = started.thread.id
    print("thread:", thread_id)

    # 2. Run a turn, consuming the item stream.
    async for item in client.run(thread_id, RunTurnParams(input="hello")):
        # tool_name, not toolName: toolName is the pydantic wire ALIAS, and
        # attribute access on an alias raises AttributeError.
        print("item:", item.type, item.text or item.tool_name or "")

    # 3. Cancel if still running (idempotent), then close.
    await client.cancel(thread_id)
    await client.aclose()


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except Exception as exc:  # noqa: BLE001 - top-level boundary
        print(f"error: {exc}", file=sys.stderr)
        sys.exit(1)
