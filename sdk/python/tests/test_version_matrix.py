# sdk/python/tests/test_version_matrix.py
#
# Mirrors sdk/ts/tests/version-matrix.test.ts. Drives the public AgentClient
# against every cross-version shape the wire contract considers load-bearing.

from __future__ import annotations

import json

import httpx
import pytest

from yanshi_sdk import AgentClient
from yanshi_sdk.errors import ApiVersionError

BASE_THREAD = {
    "version": "v1",
    "thread": {
        "version": "v1",
        "id": "thread-001",
        "status": "active",
        "createdAt": 1,
        "updatedAt": 1,
    },
}

# Shape that omits BOTH the top-level version and the in-thread version, so
# the SDK truly cannot determine a version (neither header nor body has it).
VERSIONLESS_THREAD = {
    "thread": {
        "id": "thread-001",
        "status": "active",
        "createdAt": 1,
        "updatedAt": 1,
    },
}


def _response(version: str | None, payload: dict) -> httpx.Response:
    headers = {"Content-Type": "application/json"}
    if version:
        headers["X-Yanshi-API-Version"] = version
        payload = {**payload, "version": version}
    return httpx.Response(200, headers=headers, content=json.dumps(payload))


def _client_with_version(version: str | None) -> tuple[AgentClient, httpx.MockTransport]:
    def handler(request: httpx.Request) -> httpx.Response:
        body = dict(BASE_THREAD)
        if version is None:
            body = dict(VERSIONLESS_THREAD)
        else:
            body["futureField"] = True
        return _response(version, body)

    mock = httpx.MockTransport(handler)
    client = AgentClient("http://localhost")
    client.transport.http = httpx.AsyncClient(transport=mock)
    return client, mock


@pytest.mark.asyncio
async def test_accepts_v1() -> None:
    client, _ = _client_with_version("v1")
    try:
        result = await client.start()
        assert result.version == "v1"
        assert result.thread.id == "thread-001"
    finally:
        await client.transport.http.aclose()


@pytest.mark.asyncio
async def test_accepts_v1_1_as_additive_minor() -> None:
    client, _ = _client_with_version("v1.1")
    try:
        result = await client.start()
        assert result.thread.id == "thread-001"
        dumped = result.model_dump(by_alias=True, exclude_none=True)
        assert dumped.get("futureField") is True
    finally:
        await client.transport.http.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("version", ["v2", None, "garbage", "v1."])
async def test_rejects_incompatible_version(version: str | None) -> None:
    client, _ = _client_with_version(version)
    try:
        with pytest.raises(ApiVersionError):
            await client.start()
    finally:
        await client.transport.http.aclose()


@pytest.mark.asyncio
async def test_tolerates_unknown_server_added_fields() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        body = {**BASE_THREAD, "serverRubric": {"future": True}, "anotherUnknown": 42}
        return _response("v1", body)

    mock = httpx.MockTransport(handler)
    client = AgentClient("http://localhost")
    client.transport.http = httpx.AsyncClient(transport=mock)
    try:
        result = await client.start()
        dumped = result.model_dump(by_alias=True, exclude_none=True)
        assert dumped["serverRubric"] == {"future": True}
        assert dumped["anotherUnknown"] == 42
    finally:
        await client.transport.http.aclose()
