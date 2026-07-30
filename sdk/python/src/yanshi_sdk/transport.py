# sdk/python/src/yanshi_sdk/transport.py
#
# HTTP + SSE + WebSocket transport for the Yanshi v1 Agent API.
#
# D1 contract served by this module (verified against
# internal/api/http/agent_v1.go and internal/api/v1/types.go):
#   POST /api/v1/thread/start     -> ThreadStartResponse
#   POST /api/v1/thread/resume    -> ThreadResumeResponse
#   POST /api/v1/thread/interrupt -> InterruptResponse
#   POST /api/v1/turn/start       -> SSE stream: `event: turn` once with the
#                                    TurnStartResponse, then `event: item` per
#                                    v1.Item, then closes.
#
# All paths are isolated here so D1's route adapter is a single replace point.
# asyncio.CancelledError is always re-raised (Python 3.11+ moved it to
# BaseException); we never wrap cancellation as StreamDisconnectedError.

from __future__ import annotations

import asyncio
import json
import re
from collections.abc import AsyncIterator, Awaitable, Callable
from typing import Any, Optional

import httpx

from .errors import (
    ApiVersionError,
    HttpError,
    ProtocolError,
    StreamDisconnectedError,
)
from .generated import Item, KNOWN_ITEM_TYPES, ModelBase

VERSION_PATTERN = re.compile(r"^v1(\.[0-9]+)?$")
ITEM_ID_PATTERN = re.compile(r"^item-[1-9][0-9]*$")
SUPPORTED_DEFAULT: tuple[str, ...] = ("v1",)


def make_url(base_url: str, path: str) -> str:
    """Join base URL and path without producing duplicate slashes."""
    return f"{base_url.rstrip('/')}/{path.lstrip('/')}"


def is_valid_version(version: object) -> bool:
    return isinstance(version, str) and VERSION_PATTERN.match(version) is not None


def version_supported(version: object, supported: tuple[str, ...]) -> bool:
    if not isinstance(version, str):
        return False
    if not is_valid_version(version):
        return False
    major = version.split(".", 1)[0]
    return major in supported


def normalize_item_type(value: object) -> str:
    """Map unknown server item types to "unknown" but leave known types
    untouched. D1's contract says unknown item types must not be silently
    dropped; SDK callers that consume the canonical model still see the
    original `type` string because ModelBase stores it (extra="allow")."""
    if isinstance(value, str) and (value in KNOWN_ITEM_TYPES or value.startswith("event.")):
        return value
    return "unknown"


HttpFactory = Callable[[], httpx.AsyncClient]


class Transport:
    """Async HTTP/SSE/WS transport. The client composes paths and passes them
    here; this class owns the wire (httpx connection pool, headers, version
    validation, SSE framing, WS JSON-RPC notification parsing)."""

    def __init__(
        self,
        base_url: str,
        token: Optional[str] = None,
        supported: tuple[str, ...] = SUPPORTED_DEFAULT,
        http_factory: Optional[HttpFactory] = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.supported = supported
        self.http: httpx.AsyncClient = (
            http_factory() if http_factory else httpx.AsyncClient(timeout=None)
        )

    def _headers(self, accept: str = "application/json") -> dict[str, str]:
        headers = {"Accept": accept}
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"
        return headers

    async def request_json(
        self,
        path: str,
        method: str,
        body: object | None = None,
    ) -> dict[str, Any]:
        """Send a JSON request and validate the response envelope."""
        response = await self.http.request(
            method,
            make_url(self.base_url, path),
            headers={**self._headers(), "Content-Type": "application/json"},
            content=json.dumps(body if body is not None else {}),
        )
        return await self._decode_json_response(path, response)

    async def _decode_json_response(
        self,
        path: str,
        response: httpx.Response,
    ) -> dict[str, Any]:
        try:
            value: Any = response.json()
        except ValueError as cause:
            raise ProtocolError(f"non-JSON response from {path}") from cause
        if response.status_code >= 400:
            raise HttpError(response.status_code, value)
        if not isinstance(value, dict):
            raise ProtocolError(f"JSON response from {path} is not an object")
        header_version = response.headers.get("X-Yanshi-API-Version")
        version = header_version or (
            str(value.get("version")) if value.get("version") else None
        )
        if not version_supported(version, self.supported):
            raise ApiVersionError(version, self.supported)
        return value

    async def sse(
        self,
        path: str,
        body: object | None = None,
        after_sequence: Optional[int] = None,
    ) -> AsyncIterator[Item]:
        """POST to `path` with a JSON `body` and stream the SSE response.

        D1 emits `event: turn` once (TurnStartResponse) followed by zero or
        more `event: item` blocks. We yield only Items, skipping the metadata
        block. Callers who need the TurnStartResponse should consume the
        client.run() generator which captures metadata before the first item.
        """
        last_sequence: Optional[int] = after_sequence
        try:
            async with self.http.stream(
                "POST",
                make_url(self.base_url, path),
                headers={**self._headers("text/event-stream"), "Content-Type": "application/json"},
                content=json.dumps(body if body is not None else {}),
            ) as response:
                header_version = response.headers.get("X-Yanshi-API-Version")
                if header_version and not version_supported(header_version, self.supported):
                    raise ApiVersionError(header_version, self.supported)
                if response.status_code >= 400:
                    raise HttpError(response.status_code, await response.aread())
                event_name: Optional[str] = None
                data: list[str] = []
                async for line in response.aiter_lines():
                    if line == "":
                        if not data:
                            event_name = None
                            continue
                        payload = "\n".join(data)
                        data = []
                        name = event_name
                        event_name = None
                        if name != "item":
                            continue
                        try:
                            raw: Any = json.loads(payload)
                        except json.JSONDecodeError as cause:
                            raise ProtocolError("invalid JSON SSE payload") from cause
                        if not isinstance(raw, dict):
                            raise ProtocolError("SSE payload is not an object")
                        item = _build_item(raw)
                        if not version_supported(item.version, self.supported):
                            raise ApiVersionError(item.version, self.supported)
                        last_sequence = item.sequence
                        yield item
                    elif line.startswith("event: "):
                        event_name = line[7:].strip()
                    elif line.startswith("data: "):
                        data.append(line[6:])
        except asyncio.CancelledError:
            raise
        except (ApiVersionError, ProtocolError, HttpError):
            raise
        except Exception as cause:
            raise StreamDisconnectedError(last_sequence, cause) from cause

    async def websocket(
        self,
        path: str,
        after_sequence: Optional[int] = None,
    ) -> AsyncIterator[Item]:
        """Connect to a WebSocket and yield items.

        D1 does not serve WS today; this transport is forward-looking and
        accepts either bare Item JSON or JSON-RPC item/updated notifications,
        matching the shape D1's appserver emits over stdio. Tests inject a
        fake WS; production callers should prefer sse().
        """
        ws_url = (
            make_url(self.base_url, path)
            .replace("http://", "ws://", 1)
            .replace("https://", "wss://", 1)
        )
        headers = self._headers()
        last_sequence: Optional[int] = after_sequence
        try:
            # Lazy import: websockets is in [test]/[generate] extras so the
            # core SDK install (without extras) can still import this module
            # for the SSE path without the WS dependency.
            from websockets.asyncio.client import connect

            async with connect(ws_url, additional_headers=headers) as socket:
                await socket.send(json.dumps({"type": "resume", "afterSequence": after_sequence}))
                async for raw in socket:
                    if isinstance(raw, bytes):
                        raw = raw.decode("utf-8")
                    try:
                        value: Any = json.loads(raw)
                    except json.JSONDecodeError as cause:
                        raise ProtocolError("invalid JSON websocket payload") from cause
                    candidate = value
                    if isinstance(value, dict) and value.get("jsonrpc") == "2.0" and value.get("method") == "item/updated":
                        candidate = value.get("params", {})
                    if not isinstance(candidate, dict):
                        raise ProtocolError("websocket payload is not an object")
                    item = _build_item(candidate)
                    if not version_supported(item.version, self.supported):
                        raise ApiVersionError(item.version, self.supported)
                    last_sequence = item.sequence
                    yield item
        except asyncio.CancelledError:
            raise
        except (ApiVersionError, ProtocolError, HttpError, StreamDisconnectedError):
            raise
        except Exception as cause:
            raise StreamDisconnectedError(last_sequence, cause) from cause

    async def close(self) -> None:
        await self.http.aclose()


def _build_item(raw: dict[str, Any]) -> Item:
    """Validate a raw dict against the Item model, raising ProtocolError on
    shape failures. Pydantic raises ValidationError on contract violations;
    we re-wrap it so callers catch one exception family."""
    from pydantic import ValidationError

    try:
        return Item.model_validate(raw)
    except ValidationError as cause:
        raise ProtocolError(f"invalid item payload: {cause}") from cause


# Re-export for symmetry with the TS SDK.
__all__ = [
    "ITEM_ID_PATTERN",
    "SUPPORTED_DEFAULT",
    "VERSION_PATTERN",
    "Transport",
    "is_valid_version",
    "make_url",
    "normalize_item_type",
    "version_supported",
]
