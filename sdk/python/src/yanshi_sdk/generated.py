# sdk/python/src/yanshi_sdk/generated.py
#
# Hand-mirrored Pydantic v2 models for the Yanshi Agent API v1.
# Mirrors internal/api/v1/types.go and sdk/ts/v1.ts.
#
# The D2 plan originally called for `datamodel-code-generator` against the
# provisional JSON Schema. In practice the canonical types are owned by D1
# (Go structs + a tiny generated TS mirror) and D2 maintains the Python
# mirror by hand. This keeps the round-trip behavior obvious: every camelCase
# alias, every required field, and every UnknownFields="ignored" guarantee
# lives next to its test rather than behind a generator step.
#
# When D1 publishes the canonical schema via `/api/v1/schema/agent-v1.json`,
# re-run `python -m datamodel_code_generator` against it and replace this
# file; the public API in client.py / __init__.py is unaffected because the
# class names below match the schema `$defs` exactly.

from __future__ import annotations

from datetime import datetime
from typing import Any, Optional, Union

from pydantic import BaseModel, ConfigDict, Field

# D1 known item types. Unknown server values arrive as `event.<legacyType>`;
# clients normalize them to "unknown" before validating when they want a
# strictly typed enum, but the v1 wire contract explicitly preserves them.
KNOWN_ITEM_TYPES = {
    "turn.started",
    "message.delta",
    "reasoning.delta",
    "tool.call",
    "tool.result",
    "tool.progress",
    "structured.result",
    "turn.error",
    "turn.completed",
}

# Pydantic v2 has first-class support for `str` with pattern constraints.
VersionPattern = r"^v1(\.[0-9]+)?$"
ItemIdPattern = r"^item-[1-9][0-9]*$"


class ModelBase(BaseModel):
    """Base for every v1 model. Allows extra fields so unknown server-added
    fields round-trip without raising — this is the v1 wire contract."""

    model_config = ConfigDict(populate_by_name=True, extra="allow")


class Range(ModelBase):
    """D2-provisional IDE range. D1 does not emit this today; the IDE context
    collector populates it and the server ignores unknown fields."""

    start: int = Field(ge=0)
    end: int = Field(ge=0)


class SelectionContext(ModelBase):
    """D2-provisional IDE selection context."""

    kind: str = Field(pattern=r"^selection$")
    path: str
    content: str
    range_: Range = Field(alias="range")
    truncated: Optional[bool] = None


class OpenFileContext(ModelBase):
    """D2-provisional IDE open-file context."""

    kind: str = Field(pattern=r"^openFile$")
    path: str
    content: str
    truncated: Optional[bool] = None


ContextItem = Union[SelectionContext, OpenFileContext]


class FileChange(ModelBase):
    """D2-provisional. D1 does not yet emit fileChange items."""

    path: str
    before: Optional[str] = None
    after: Optional[str] = None
    unified_diff: Optional[str] = Field(default=None, alias="unifiedDiff")


class Thread(ModelBase):
    version: str = Field(pattern=VersionPattern)
    id: str
    status: str
    title: Optional[str] = None
    created_at: int = Field(alias="createdAt")
    updated_at: int = Field(alias="updatedAt")
    model: Optional[str] = None
    thinking: Optional[str] = None
    turns: Optional[list["Turn"]] = None


class Turn(ModelBase):
    version: str = Field(pattern=VersionPattern)
    id: str
    thread_id: str = Field(alias="threadId")
    status: str
    input: str
    started_at: int = Field(alias="startedAt")
    completed_at: Optional[int] = Field(default=None, alias="completedAt")


class Item(ModelBase):
    version: str = Field(pattern=VersionPattern)
    id: str = Field(pattern=ItemIdPattern)
    sequence: int = Field(ge=1)
    thread_id: str = Field(min_length=1, alias="threadId")
    turn_id: str = Field(min_length=1, alias="turnId")
    type: str = Field(min_length=1)
    text: Optional[str] = None
    tool_name: Optional[str] = Field(default=None, alias="toolName")
    tool_args: Optional[str] = Field(default=None, alias="toolArgs")
    status: Optional[str] = None
    error: Optional[str] = None
    structured_result: Optional[Any] = Field(default=None, alias="structuredResult")
    file_change: Optional[FileChange] = Field(default=None, alias="fileChange")


class ThreadStartParams(ModelBase):
    version: Optional[str] = Field(default=None, pattern=VersionPattern)
    title: Optional[str] = None
    model: Optional[str] = None
    thinking: Optional[str] = None


class ThreadResumeParams(ModelBase):
    version: Optional[str] = Field(default=None, pattern=VersionPattern)
    thread_id: str = Field(alias="threadId")


class ThreadInterruptParams(ModelBase):
    version: Optional[str] = Field(default=None, pattern=VersionPattern)
    thread_id: str = Field(alias="threadId")
    turn_id: Optional[str] = Field(default=None, alias="turnId")


class TurnStartParams(ModelBase):
    version: Optional[str] = Field(default=None, pattern=VersionPattern)
    thread_id: str = Field(alias="threadId")
    input: str
    model: Optional[str] = None
    thinking: Optional[str] = None
    output_schema: Optional[Any] = Field(default=None, alias="outputSchema")
    # D2-provisional IDE context. The wire field name is `context`; D1 ignores
    # unknown fields today. Listed here so the IDE's selection/openFiles round
    # trip without an extra SDK type.
    context: Optional[list[ContextItem]] = None


class ThreadStartResponse(ModelBase):
    version: str = Field(pattern=VersionPattern)
    thread: Thread


class ThreadResumeResponse(ModelBase):
    version: str = Field(pattern=VersionPattern)
    thread: Thread


class TurnStartResponse(ModelBase):
    version: str = Field(pattern=VersionPattern)
    turn: Turn


class InterruptResponse(ModelBase):
    version: str = Field(pattern=VersionPattern)
    ok: bool
    thread_id: str = Field(alias="threadId")
    turn_id: Optional[str] = Field(default=None, alias="turnId")


class Capabilities(ModelBase):
    version: str = Field(pattern=VersionPattern)
    methods: list[str]
    item_types: list[str] = Field(alias="itemTypes")
    unknown_fields: str = Field(alias="unknownFields")
    stream: str
