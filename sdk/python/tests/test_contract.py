# sdk/python/tests/test_contract.py
from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest
from jsonschema import Draft202012Validator
from pydantic import ValidationError

from yanshi_sdk.generated import Item, ThreadResumeResponse, KNOWN_ITEM_TYPES
from yanshi_sdk.transport import normalize_item_type

ROOT = Path(__file__).resolve().parents[2]
SCHEMA_PATH = ROOT / "schema" / "v1" / "agent-api.schema.json"
FIXTURES = ROOT / "schema" / "v1" / "fixtures"


def _load(name: str) -> Any:
    return json.loads((FIXTURES / name).read_text(encoding="utf-8"))


def _load_jsonl(name: str) -> list[dict[str, Any]]:
    text = (FIXTURES / name).read_text(encoding="utf-8")
    return [json.loads(line) for line in text.splitlines() if line.strip()]


SCHEMA = json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))
EVENTS = _load_jsonl("items.jsonl")


def test_response_fixtures_match_schema_and_models() -> None:
    validator = Draft202012Validator(SCHEMA)
    for name in (
        "thread-start.response.json",
        "thread-resume.response.json",
        "turn-start.response.json",
        "interrupt.response.json",
    ):
        data = _load(name)
        assert not list(validator.iter_errors(data)), name
    # Validate Pydantic models can construct from the same fixtures.
    ThreadResumeResponse.model_validate(_load("thread-resume.response.json"))


def test_items_fixture_is_strictly_monotonic_and_known() -> None:
    validator = Draft202012Validator(SCHEMA)
    for raw in EVENTS:
        assert not list(validator.iter_errors(raw)), raw
        Item.model_validate(raw)
    sequences = [raw["sequence"] for raw in EVENTS]
    assert sequences == list(range(1, len(EVENTS) + 1))


def test_structured_result_present_in_fixture() -> None:
    structured = next((e for e in EVENTS if e["type"] == "structured.result"), None)
    assert structured is not None
    assert structured["structuredResult"] == {"ok": True, "summary": "done", "artifacts": ["main.go"]}


def test_additive_server_fields_round_trip() -> None:
    raw = dict(EVENTS[0])
    raw["serverAddedField"] = {"future": True}
    item = Item.model_validate(raw)
    dumped = item.model_dump(by_alias=True, exclude_none=True)
    assert dumped["serverAddedField"] == {"future": True}
    assert dumped["threadId"] == "thread-001"


def test_unknown_item_type_preserves_payload_and_round_trips() -> None:
    raw = _load("unknown-item.json")
    # Pydantic validates as-is: type="event.future_telemetry" is allowed.
    item = Item.model_validate(raw)
    assert item.type == "event.future_telemetry"
    dumped = item.model_dump(by_alias=True, exclude_none=True)
    assert dumped["futurePayload"] == {"preserveSequence": True}


@pytest.mark.parametrize(
    "kind, expected",
    [
        ("turn.started", "turn.started"),
        ("message.delta", "message.delta"),
        ("event.future_telemetry", "event.future_telemetry"),
        ("nonsense", "unknown"),
        (None, "unknown"),
    ],
)
def test_normalize_item_type_maps_unknowns_to_unknown(kind: object, expected: str) -> None:
    assert normalize_item_type(kind) == expected


@pytest.mark.parametrize(
    "bad",
    [
        {"version": "v2", "id": "item-1", "sequence": 1, "threadId": "t", "turnId": "r", "type": "x"},
        {"version": "v1", "id": "bad-id", "sequence": 1, "threadId": "t", "turnId": "r", "type": "x"},
        {"version": "v1", "id": "item-1", "sequence": 0, "threadId": "t", "turnId": "r", "type": "x"},
        {"version": "v1", "id": "item-1", "sequence": 1, "threadId": "", "turnId": "r", "type": "x"},
        {"version": "v1", "id": "item-1", "sequence": 1, "threadId": "t", "turnId": "r", "type": ""},
    ],
)
def test_invalid_envelopes_rejected_by_pydantic(bad: dict[str, Any]) -> None:
    with pytest.raises(ValidationError):
        Item.model_validate(bad)


def test_known_item_types_are_all_canonical_v1_values() -> None:
    assert KNOWN_ITEM_TYPES == {
        "turn.started", "message.delta", "reasoning.delta",
        "tool.call", "tool.result", "tool.progress",
        "structured.result", "turn.error", "turn.completed",
    }
