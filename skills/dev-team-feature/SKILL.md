---
name: dev-team-feature
description: Use when a feature is multi-component, parallelizable, and needs a Lead plus Workers and an Integrator
---

# Team Feature (T3)

Uses Yanshi's Goal Loop implementer team.

## Workflow
1. Spec + plan (as T2).
2. Lead decomposes the plan into independent tasks.
3. Workers implement in parallel, each in its own worktree-isolated copy.
4. Integrator merges results and resolves conflicts.
5. Evaluate: tests pass, intent met, quality bar held.
6. Adjudicate; loop or finish.

## Escalate
If the goal is open-ended and must self-drive across multiple iterations with its own terminal condition, escalate to T4 (`dev-autonomous-project`).
