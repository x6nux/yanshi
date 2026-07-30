package goalloop

import "context"

// Planner turns a Goal into a Plan (acceptance tests + implementation steps).
type Planner interface {
	Plan(ctx context.Context, g Goal) (Plan, error)
}

// Implementer executes a Plan's steps in the given working directory.
// It returns a result string (e.g. diff summary or log) and any error.
type Implementer interface {
	Implement(ctx context.Context, p Plan, workdir string) (result string, err error)
}

// Evaluator examines the implementation against the goal and plan,
// returning a verdict with pass/fail, evidence, and gaps.
type Evaluator interface {
	Evaluate(ctx context.Context, g Goal, p Plan, workdir string) (EvalVerdict, error)
}

// Judge aggregates all evaluator verdicts into a single Decision.
type Judge interface {
	Judge(ctx context.Context, verdicts []EvalVerdict) (Decision, error)
}
