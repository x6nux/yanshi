---
name: dev-autonomous-project
description: Use when the goal is open-ended and the agent should self-drive across multiple plan-implement-evaluate-judge iterations until a terminal condition is met
---

# Autonomous Project (T4)

Uses the full Goal Loop.

## Workflow
1. Plan: produce a plan with acceptance tests.
2. Implement: Lead + Workers + Integrator.
3. Evaluate from three angles: tests (TestEvaluator), intent (IntentEvaluator), quality (QualityEvaluator).
4. Judge: aggregate verdict.
5. Loop (fix gaps) or Complete (terminal condition met).

## Terminal conditions
- All evaluators pass for one full iteration, OR
- The iteration budget is exhausted (report and stop).

Use `/dev t4 <goal>` or let auto-detection choose this tier.
