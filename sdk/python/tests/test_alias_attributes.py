"""Guards documentation and examples against pydantic alias attribute access.

Every camelCase name on the wire is an *alias* on these models; the Python
attribute is snake_case. Reading ``item.toolName`` therefore raises
AttributeError at runtime — and docs/api/sdk-python.md and
examples/sdk-python/main.py both did exactly that, in the one line each of them
used to show how to consume the item stream.

CI could not see it: docs.yml ran ``python -m py_compile`` and
``python -c "import yanshi_sdk"``. Both pass on code that raises the moment it
executes, because neither one executes it.

This test scans the shipped snippets for attribute access on any alias the
models declare, so the check does not depend on someone remembering which
names are aliased.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest

from yanshi_sdk import generated

REPO_ROOT = Path(__file__).resolve().parents[3]

# Files that show users how to call the SDK. A wrong attribute here is a
# runtime crash in the first thing a new user runs.
SNIPPET_FILES = [
    REPO_ROOT / "docs" / "api" / "sdk-python.md",
    REPO_ROOT / "examples" / "sdk-python" / "main.py",
]


def _aliases() -> set[str]:
    """Every wire alias declared by the models, excluding ones that happen to
    equal their attribute name (those are safe to write either way)."""
    out: set[str] = set()
    for name in dir(generated):
        model = getattr(generated, name)
        fields = getattr(model, "model_fields", None)
        if not isinstance(fields, dict):
            continue
        for attr, field in fields.items():
            alias = getattr(field, "alias", None)
            if alias and alias != attr:
                out.add(alias)
    return out


def test_models_declare_aliases() -> None:
    """Positive control.

    Without it, a refactor that stopped declaring aliases would make the scan
    below vacuously true — it would find nothing to look for and report clean.
    """
    aliases = _aliases()
    assert "toolName" in aliases, f"expected camelCase aliases, got {sorted(aliases)}"
    assert len(aliases) >= 5


@pytest.mark.parametrize("path", SNIPPET_FILES, ids=lambda p: p.name)
def test_snippets_do_not_read_wire_aliases_as_attributes(path: Path) -> None:
    aliases = _aliases()
    text = path.read_text(encoding="utf-8")
    offenders = []
    for line_no, line in enumerate(text.splitlines(), start=1):
        stripped = line.strip()
        # Comments explain the trap on purpose; they are not executed.
        if stripped.startswith("#") or stripped.startswith("//"):
            continue
        for alias in aliases:
            if re.search(rf"\.{re.escape(alias)}\b", line):
                offenders.append(f"{path.name}:{line_no}: .{alias} — use the snake_case attribute")
    assert not offenders, (
        "attribute access on a pydantic wire alias raises AttributeError at runtime:\n  "
        + "\n  ".join(offenders)
    )
