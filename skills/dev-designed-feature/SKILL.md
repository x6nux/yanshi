---
name: dev-designed-feature
description: Use when a feature needs a design first because it is multi-file, adds an API surface, or has unclear scope
---

# Designed Feature (T2)

## Workflow
1. Brainstorm: clarify purpose, constraints, success criteria.
2. Write a spec: `fs_write docs/superpowers/specs/<date>-<topic>-design.md`.
3. Write a plan: `fs_write docs/superpowers/plans/<date>-<topic>.md` with bite-sized TDD tasks.
4. Implement task-by-task (red → green → commit each).
5. Review against the spec; fix gaps.
6. Merge.

## Escalate
If the work is parallelizable across components or needs a team, escalate to T3 (`dev-team-feature`).
