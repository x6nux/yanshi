# sdk/python/tests/test_transport.py
#
# Tests for SSE/WS helpers and asyncio.CancelledError propagation. Mirrors
# sdk/ts/tests/transport.test.ts at the Python level.

from __future__ import annotations

import asyncio

import httpx
import pytest

from yanshi_sdk.transport import Transport, normalize_item_type, version_supported


@pytest.mark.asyncio
async def test_sse_normalizes_unknown_preserves_extra_and_keeps_duplicates() -> None:
    body = (
        'event: item\n'
        'data: {"version":"v1","id":"item-1","sequence":1,"threadId":"t","turnId":"r","type":"event.futureKind","futurePayload":{"keep":true}}\n'
        '\n'
        'event: item\n'
        'data: {"version":"v1","id":"item-1","sequence":2,"threadId":"t","turnId":"r","type":"message.delta"}\n'
        '\n'
        'event: item\n'
        'data: {"version":"v1","id":"item-3","sequence":3,"threadId":"t","turnId":"r","type":"turn.completed"}\n'
        '\n'
    )

    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            headers={"X-Yanshi-API-Version": "v1", "Content-Type": "text/event-stream"},
            content=body.encode("utf-8"),
        )

    transport = Transport("http://localhost")
    await transport.http.aclose()
    transport.http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    try:
        events = [event async for event in transport.sse("/api/v1/turn/start", body={"threadId": "t", "input": "hi"})]
    finally:
        await transport.close()

    # The duplicate id (item-1) is preserved; the SDK does not de-duplicate.
    # Sequence is monotonic. Unknown type is preserved as event.<x> (not
    # rewritten) so the IDE can show what the server actually sent.
    assert [e.type for e in events] == ["event.futureKind", "message.delta", "turn.completed"]
    assert [e.id for e in events] == ["item-1", "item-1", "item-3"]
    assert [e.sequence for e in events] == [1, 2, 3]
    assert events[0].model_dump(by_alias=True)["futurePayload"] == {"keep": True}


class _CancelledStream:
    async def __aenter__(self):
        raise asyncio.CancelledError()

    async def __aexit__(self, exc_type, exc, tb) -> bool:
        return False


class _CancelledHttp:
    def stream(self, *args, **kwargs):
        return _CancelledStream()

    async def aclose(self) -> None:
        return None


@pytest.mark.asyncio
async def test_sse_propagates_cancelled_error_without_wrapping() -> None:
    transport = Transport("http://localhost")
    await transport.http.aclose()
    transport.http = _CancelledHttp()  # type: ignore[assignment]
    iterator = transport.sse("/api/v1/turn/start", body={})
    with pytest.raises(asyncio.CancelledError):
        await anext(iterator)


def test_normalize_item_type_handles_known_and_unknown() -> None:
    assert normalize_item_type("turn.started") == "turn.started"
    assert normalize_item_type("event.future") == "event.future"
    assert normalize_item_type("nonsense") == "unknown"
    assert normalize_item_type(None) == "unknown"


def test_version_supported_matrix() -> None:
    assert version_supported("v1", ("v1",))
    assert version_supported("v1.1", ("v1",))
    assert not version_supported("v2", ("v1",))
    assert not version_supported(None, ("v1",))
    assert not version_supported("", ("v1",))
    assert not version_supported("v1.", ("v1",))
    assert not version_supported("garbage", ("v1",))
