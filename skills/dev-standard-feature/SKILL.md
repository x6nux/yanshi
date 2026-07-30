---
name: dev-standard-feature
description: Use when implementing one focused feature or bugfix that follows TDD and touches 1-3 files with no upfront design needed
---

# Standard Feature (T1)

## Workflow (TDD)
1. Reproduce/understand: `fs_read` the relevant code; `fs_search` for symbols.
2. Write a failing test: `fs_write` or `fs_edit` a `*_test.go`.
3. Run it red: `shell_run go test ./<pkg>/...` — it MUST fail.
4. Implement minimally: `fs_edit` the production code.
5. Run it green: `shell_run go test ./<pkg>/...` — it passes.
6. Refactor; re-run green.
7. Commit.

## Rules
- Never write production code before the failing test.
- One `shell_run` per command (no && or |).

## Escalate
If the feature needs a design or spans more than 3 files, escalate to T2 (`dev-designed-feature`).
