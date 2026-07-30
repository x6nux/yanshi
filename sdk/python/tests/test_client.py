# sdk/python/tests/test_client.py
from __future__ import annotations

import json
from collections.abc import AsyncIterator

import httpx
import pytest

from yanshi_sdk import (
    AgentClient,
    Item,
    RunOptions,
    RunTurnParams,
)
from yanshi_sdk.errors import ApiVersionError, HttpError, StreamDisconnectedError
from yanshi_sdk.transport import Transport


def _sse_lines(events: list[tuple[str, dict]]) -> bytes:
    out = []
    for name, payload in events:
        out.append(f"event: {name}\n".encode("utf-8"))
        out.append(b"data: " + json.dumps(payload).encode("utf-8") + b"\n")
        out.append(b"\n")
    return b"".join(out)


@pytest.mark.asyncio
async def test_lifecycle_uses_d1_flat_routes_and_camel_case_bodies() -> None:
    calls: list[tuple[str, str, object]] = []

    def handler(request: httpx.Request) -> httpx.Response:
        body = json.loads(request.content.decode("utf-8")) if request.content else {}
        calls.append((str(request.url.path), request.method, body))
        if request.url.path == "/api/v1/thread/start":
            return httpx.Response(
                200,
                headers={"X-Yanshi-API-Version": "v1", "Content-Type": "application/json"},
                content=json.dumps({
                    "version": "v1",
                    "thread": {
                        "version": "v1", "id": "thread-001", "status": "active",
                        "createdAt": 1, "updatedAt": 1,
                    },
                }),
            )
        if request.url.path == "/api/v1/thread/resume":
            return httpx.Response(
                200,
                headers={"X-Yanshi-API-Version": "v1", "Content-Type": "application/json"},
                content=json.dumps({
                    "version": "v1",
                    "thread": {
                        "version": "v1", "id": "thread-001", "status": "active",
                        "createdAt": 1, "updatedAt": 2,
                    },
                    "items": [],
                }),
            )
        if request.url.path == "/api/v1/thread/interrupt":
            return httpx.Response(
                200,
                headers={"X-Yanshi-API-Version": "v1", "Content-Type": "application/json"},
                content=json.dumps({
                    "version": "v1", "ok": True,
                    "threadId": "thread-001", "turnId": "thread-001-turn-1",
                }),
            )
        raise AssertionError(request.url.path)

    mock = httpx.MockTransport(handler)
    client = AgentClient("http://localhost")
    client.transport.http = httpx.AsyncClient(transport=mock)
    try:
        started = await client.start()
        await client.resume(started.thread.id)
        interrupted = await client.interrupt(started.thread.id, "thread-001-turn-1")
        cancelled = await client.cancel(started.thread.id)
    finally:
        await client.transport.http.aclose()

    assert started.thread.id == "thread-001"
    assert interrupted.ok is True
    assert cancelled.ok is True
    assert [c[0] for c in calls] == [
        "/api/v1/thread/start",
        "/api/v1/thread/resume",
        "/api/v1/thread/interrupt",
        "/api/v1/thread/interrupt",
    ]
    # Bodies are camelCase; context is omitted when not set.
    assert calls[1][2] == {"threadId": "thread-001"}
    assert calls[2][2] == {"threadId": "thread-001", "turnId": "thread-001-turn-1"}
    assert "turnId" not in calls[3][2]


@pytest.mark.asyncio
async def test_run_yields_items_and_invokes_on_started() -> None:
    sse = _sse_lines([
        ("turn", {"version": "v1", "turn": {
            "version": "v1", "id": "thread-001-turn-1", "threadId": "thread-001",
            "status": "inProgress", "input": "hello", "startedAt": 42,
        }}),
        ("item", {"version": "v1", "id": "item-1", "sequence": 1,
                  "threadId": "thread-001", "turnId": "thread-001-turn-1",
                  "type": "turn.started"}),
        ("item", {"version": "v1", "id": "item-2", "sequence": 2,
                  "threadId": "thread-001", "turnId": "thread-001-turn-1",
                  "type": "message.delta", "text": "hello"}),
        ("item", {"version": "v1", "id": "item-3", "sequence": 3,
                  "threadId": "thread-001", "turnId": "thread-001-turn-1",
                  "type": "structured.result",
                  "structuredResult": {"ok": True, "summary": "done"}}),
        ("item", {"version": "v1", "id": "item-4", "sequence": 4,
                  "threadId": "thread-001", "turnId": "thread-001-turn-1",
                  "type": "turn.completed", "status": "completed"}),
    ])

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            headers={"X-Yanshi-API-Version": "v1", "Content-Type": "text/event-stream"},
            content=sse,
        )

    mock = httpx.MockTransport(handler)
    client = AgentClient("http://localhost")
    client.transport.http = httpx.AsyncClient(transport=mock)
    started_turn_ids: list[str] = []
    items: list[Item] = []
    try:
        async def on_started(started):
            started_turn_ids.append(started.turn.id)

        async for item in client.run(
            "thread-001",
            RunTurnParams(input="hello"),
            RunOptions(transport="sse", on_started=on_started),
        ):
            items.append(item)
    finally:
        await client.transport.http.aclose()

    assert started_turn_ids == ["thread-001-turn-1"]
    assert [i.sequence for i in items] == [1, 2, 3, 4]
    assert [i.type for i in items] == ["turn.started", "message.delta", "structured.result", "turn.completed"]
    assert items[2].structured_result == {"ok": True, "summary": "done"}
    # Round-trip via alias keeps camelCase wire keys.
    dumped = items[0].model_dump(by_alias=True, exclude_none=True)
    assert dumped["threadId"] == "thread-001"
    assert dumped["turnId"] == "thread-001-turn-1"


@pytest.mark.asyncio
async def test_run_wraps_http_error() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            409,
            headers={"X-Yanshi-API-Version": "v1", "Content-Type": "application/json"},
            content=json.dumps({"version": "v1", "error": {"message": "turn already active"}}),
        )

    mock = httpx.MockTransport(handler)
    client = AgentClient("http://localhost")
    client.transport.http = httpx.AsyncClient(transport=mock)
    try:
        with pytest.raises(HttpError) as exc_info:
            async for _ in client.run("thread-001", RunTurnParams(input="hello")):
                pass
        assert exc_info.value.status == 409
    finally:
        await client.transport.http.aclose()


@pytest.mark.asyncio
async def test_start_rejects_incompatible_version() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        # Server missing version header AND body version — SDK must fail closed.
        return httpx.Response(
            200,
            headers={"Content-Type": "application/json"},
            content=json.dumps({"thread": {"id": "x"}}),
        )

    mock = httpx.MockTransport(handler)
    client = AgentClient("http://localhost")
    client.transport.http = httpx.AsyncClient(transport=mock)
    try:
        with pytest.raises(ApiVersionError):
            await client.start()
    finally:
        await client.transport.http.aclose()


@pytest.mark.asyncio
async def test_run_propagates_disconnect_with_last_sequence() -> None:
    """If the underlying stream raises mid-flight, the SDK surfaces a
    StreamDisconnectedError carrying the highest item sequence observed so
    the IDE recovery layer can resume from there."""
    sse = _sse_lines([
        ("turn", {"version": "v1", "turn": {
            "version": "v1", "id": "t-1", "threadId": "t",
            "status": "inProgress", "input": "hi", "startedAt": 1,
        }}),
        ("item", {"version": "v1", "id": "item-1", "sequence": 1,
                  "threadId": "t", "turnId": "t-1",
                  "type": "message.delta", "text": "x"}),
    ])

    class DroppingTransport(httpx.AsyncBaseTransport):
        def __init__(self) -> None:
            self._requests = 0

        async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
            self._requests += 1
            # Raise a ConnectError on the second call to simulate a mid-stream
            # network drop after the first request succeeded.
            if self._requests > 1:
                raise httpx.ConnectError("socket dropped")
            return httpx.Response(
                200,
                headers={"X-Yanshi-API-Version": "v1", "Content-Type": "text/event-stream"},
                content=sse,
            )

    client = AgentClient("http://localhost")
    client.transport.http = httpx.AsyncClient(transport=DroppingTransport())
    # First call: item-1 yields, stream closes cleanly (no turn.completed).
    last_seq: list[int] = []
    async for item in client.run("t", RunTurnParams(input="hi")):
        last_seq.append(item.sequence)
    assert last_seq == [1]
    await client.transport.http.aclose()
