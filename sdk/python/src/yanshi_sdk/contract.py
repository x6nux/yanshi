# sdk/python/src/yanshi_sdk/contract.py
#
# Package-internal contract entry point. Runs the shared fixtures through the
# JSON Schema and the generated Pydantic models. CI invokes it via
# `python -m yanshi_sdk.contract`; pyproject.toml does NOT declare a console
# script because this is not a stable public CLI.

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from jsonschema import Draft202012Validator

from .generated import Item, ThreadResumeResponse

ROOT = Path(__file__).resolve().parents[3]
SCHEMA_PATH = ROOT / "schema" / "v1" / "agent-api.schema.json"
FIXTURES_DIR = ROOT / "schema" / "v1" / "fixtures"


def _load_schema() -> dict[str, Any]:
    return json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))


def _load_jsonl(name: str) -> list[dict[str, Any]]:
    text = (FIXTURES_DIR / name).read_text(encoding="utf-8")
    return [json.loads(line) for line in text.splitlines() if line.strip()]


def _load_json(name: str) -> dict[str, Any]:
    return json.loads((FIXTURES_DIR / name).read_text(encoding="utf-8"))


def main() -> int:
    schema = _load_schema()
    validator = Draft202012Validator(schema)
    failures = 0

    # Response fixtures
    for name in (
        "thread-start.response.json",
        "thread-resume.response.json",
        "turn-start.response.json",
        "interrupt.response.json",
        "unknown-item.json",
    ):
        data = _load_json(name)
        errs = list(validator.iter_errors(data))
        if errs:
            print(f"FAIL {name}: {errs[:3]}")
            failures += 1
    # Items
    for i, raw in enumerate(_load_jsonl("items.jsonl"), 1):
        errs = list(validator.iter_errors(raw))
        if errs:
            print(f"FAIL items.jsonl:{i}: {errs[:3]}")
            failures += 1
    if failures:
        return 1

    # Pydantic round-trip
    resume = _load_json("thread-resume.response.json")
    ThreadResumeResponse.model_validate(resume)
    for raw in _load_jsonl("items.jsonl"):
        Item.model_validate(raw)

    # Aliases preserved on round-trip
    item0 = Item.model_validate(_load_jsonl("items.jsonl")[0])
    dumped = item0.model_dump(by_alias=True, exclude_none=True)
    assert dumped["threadId"] == "thread-001", dumped
    assert dumped["turnId"] == "thread-001-turn-1", dumped

    print("contract OK")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
