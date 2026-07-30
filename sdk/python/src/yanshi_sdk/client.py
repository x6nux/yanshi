# sdk/python/src/yanshi_sdk/client.py
#
# Async AgentClient. Mirrors sdk/ts/src/client.ts. Five lifecycle methods
# mapped to D1's flat routes; all paths isolated in transport.py.

from __future__ import annotations

import json
from collections.abc import AsyncIterator
from dataclasses import dataclass, field
from typing import Any, Optional

import httpx

from .errors import ApiVersionError, HttpError, ProtocolError, StreamDisconnectedError
from .generated import (
    ContextItem,
    InterruptResponse,
    Item,
    ThreadInterruptParams,
    ThreadResumeParams,
    ThreadResumeResponse,
    ThreadStartParams,
    ThreadStartResponse,
    TurnStartParams,
    TurnStartResponse,
)
from .transport import Transport, is_valid_version, make_url


@dataclass(frozen=True)
class RunTurnParams:
    input: str
    context: Optional[list[ContextItem]] = None
    model: Optional[str] = None
    thinking: Optional[str] = None
    output_schema: Optional[Any] = None


@dataclass
class RunOptions:
    signal: Optional[Any] = None  # asyncio.AbstractEventLoop.create_task cancellation handle
    transport: str = "sse"
    on_started: Optional[Any] = field(default=None)  # Callable[[TurnStartResponse], Awaitable[None]]


class AgentClient:
    """Async lifecycle client for the Yanshi v1 Agent API."""

    def __init__(
        self,
        base_url: str,
        token: Optional[str] = None,
        *,
        stream_transport: str = "sse",
        supported_versions: tuple[str, ...] = ("v1",),
        transport: Optional[Transport] = None,
    ) -> None:
        if stream_transport not in {"sse", "ws"}:
            raise ValueError("stream_transport must be 'sse' or 'ws'")
        self.transport = transport if transport is not None else Transport(
            base_url, token, supported_versions
        )
        self.stream_transport = stream_transport
        self.supported_versions = supported_versions

    async def start(self, params: Optional[ThreadStartParams] = None) -> ThreadStartResponse:
        body = (params or ThreadStartParams()).model_dump(by_alias=True, exclude_none=True)
        value = await self.transport.request_json("/api/v1/thread/start", "POST", body)
        return ThreadStartResponse.model_validate(value)

    async def resume(self, thread_id: str) -> ThreadResumeResponse:
        if not thread_id:
            raise ProtocolError("threadId is required")
        body = ThreadResumeParams(thread_id=thread_id).model_dump(by_alias=True, exclude_none=True)
        value = await self.transport.request_json("/api/v1/thread/resume", "POST", body)
        return ThreadResumeResponse.model_validate(value)

    async def interrupt(
        self, thread_id: str, turn_id: Optional[str] = None
    ) -> InterruptResponse:
        if not thread_id:
            raise ProtocolError("threadId is required")
        body = ThreadInterruptParams(thread_id=thread_id, turn_id=turn_id).model_dump(
            by_alias=True, exclude_none=True
        )
        value = await self.transport.request_json("/api/v1/thread/interrupt", "POST", body)
        return InterruptResponse.model_validate(value)

    async def cancel(
        self, thread_id: str, turn_id: Optional[str] = None
    ) -> InterruptResponse:
        return await self.interrupt(thread_id, turn_id)

    async def run(
        self,
        thread_id: str,
        params: RunTurnParams,
        options: Optional[RunOptions] = None,
    ) -> AsyncIterator[Item]:
        """POST /api/v1/turn/start and yield items from the SSE stream.

        D1 emits one SSE response per turn/start that contains both the
        metadata (`event: turn` -> TurnStartResponse) and the item stream
        (`event: item` -> Item, repeated). `on_started` is invoked with the
        TurnStartResponse before the first item is yielded so callers can wire
        cancel/diff UI.
        """
        if not thread_id:
            raise ProtocolError("thread_id is required")
        if not params.input or not params.input.strip():
            raise ProtocolError("turn input must not be empty")
        options = options or RunOptions()
        body = self._build_turn_body(thread_id, params)
        if options.transport == "ws":
            # Forward-looking WS path; D1 does not yet serve WS. The first
            # event still needs to come from somewhere — use the SSE path for
            # metadata, then switch to WS. For now, raise so callers don't
            # silently get nothing.
            raise ProtocolError(
                "WebSocket transport requires D1 server support; use SSE (default)"
            )

        path = "/api/v1/turn/start"
        last_sequence: Optional[int] = None
        started_emitted = False
        try:
            # Use the transport's streaming helper directly so the version
            # header check is shared. We capture the TurnStartResponse via
            # the raw stream because transport.sse() skips non-item events.
            async with self.transport.http.stream(
                "POST",
                make_url(self.transport.base_url, path),
                headers={
                    **self.transport._headers("text/event-stream"),
                    "Content-Type": "application/json",
                },
                content=json.dumps(body),
            ) as response:
                header_version = response.headers.get("X-Yanshi-API-Version")
                if header_version and not is_valid_version(header_version):
                    raise ApiVersionError(header_version, self.supported_versions)
                if response.status_code >= 400:
                    raw = await response.aread()
                    try:
                        body_err = json.loads(raw) if raw else None
                    except json.JSONDecodeError:
                        body_err = raw
                    raise HttpError(response.status_code, body_err)
                event_name: Optional[str] = None
                data: list[str] = []
                async for line in response.aiter_lines():
                    if options.signal and options.signal.cancelled:
                        return
                    if line == "":
                        if not data:
                            event_name = None
                            continue
                        payload = "\n".join(data)
                        data = []
                        name = event_name
                        event_name = None
                        if name == "turn":
                            try:
                                started_raw = json.loads(payload)
                            except json.JSONDecodeError as cause:
                                raise ProtocolError(
                                    "invalid JSON in turn/start metadata"
                                ) from cause
                            started = TurnStartResponse.model_validate(started_raw)
                            if options.on_started is not None and not started_emitted:
                                started_emitted = True
                                await _maybe_await(options.on_started(started))
                            continue
                        if name != "item":
                            continue
                        try:
                            raw_item = json.loads(payload)
                        except json.JSONDecodeError as cause:
                            raise ProtocolError("invalid JSON item payload") from cause
                        from pydantic import ValidationError
                        try:
                            item = Item.model_validate(raw_item)
                        except ValidationError as cause:
                            raise ProtocolError(f"invalid item payload: {cause}") from cause
                        from .transport import version_supported
                        if not version_supported(item.version, self.supported_versions):
                            raise ApiVersionError(item.version, self.supported_versions)
                        last_sequence = item.sequence
                        yield item
                    elif line.startswith("event: "):
                        event_name = line[7:].strip()
                    elif line.startswith("data: "):
                        data.append(line[6:])
        except (ApiVersionError, ProtocolError, HttpError, StreamDisconnectedError):
            raise
        except Exception as cause:
            from .errors import StreamDisconnectedError as _SDE
            raise _SDE(last_sequence, cause) from cause

    def _build_turn_body(self, thread_id: str, params: RunTurnParams) -> dict[str, Any]:
        body: dict[str, Any] = {"threadId": thread_id, "input": params.input}
        if params.model is not None:
            body["model"] = params.model
        if params.thinking is not None:
            body["thinking"] = params.thinking
        if params.output_schema is not None:
            body["outputSchema"] = params.output_schema
        if params.context is not None:
            body["context"] = [
                c.model_dump(by_alias=True, exclude_none=True) for c in params.context
            ]
        return body

    async def aclose(self) -> None:
        await self.transport.close()


async def _maybe_await(value: Any) -> None:
    """Allow on_started callbacks to be either sync or async."""
    if hasattr(value, "__await__"):
        await value  # type: ignore[func-returns-value]
