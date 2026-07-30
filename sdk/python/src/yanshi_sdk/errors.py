# sdk/python/src/yanshi_sdk/errors.py
#
# Python exception hierarchy. Mirrors sdk/ts/src/errors.ts so the cross-SDK
# contract tests can assert identical class names on both sides.

from __future__ import annotations

from typing import Optional


class YanshiSdkError(Exception):
    """Base error for transport and contract failures."""


class ApiVersionError(YanshiSdkError):
    """Server reported a version the SDK does not support (major bump, missing,
    or malformed). Tolerated versions today are exactly v1 plus any future
    v1.x minor; anything else fails closed."""

    def __init__(self, received: Optional[str], supported: tuple[str, ...]) -> None:
        super().__init__(
            f"unsupported Yanshi API version: received={received or 'missing'}, "
            f"supported={','.join(supported)}"
        )
        self.received = received
        self.supported = supported


class HttpError(YanshiSdkError):
    """Non-2xx HTTP response. Body captured for diagnostics."""

    def __init__(self, status: int, body: object) -> None:
        message = body.get("message") if isinstance(body, dict) else None
        super().__init__(str(message or f"HTTP {status}"))
        self.status = status
        self.body = body


class ProtocolError(YanshiSdkError):
    """Malformed JSON, missing required envelope fields, or invalid item
    payload (sequence < 1, empty threadId/turnId, etc.)."""


class StreamDisconnectedError(YanshiSdkError):
    """SSE/WS stream ended before turn.completed. Carries last_sequence so the
    IDE recovery layer can call resume() / cursor replay."""

    def __init__(
        self,
        last_sequence: Optional[int],
        cause: Optional[BaseException] = None,
    ) -> None:
        super().__init__(
            f"Yanshi item stream disconnected at sequence {last_sequence or '<none>'}"
        )
        self.last_sequence = last_sequence
        self.__cause__ = cause
