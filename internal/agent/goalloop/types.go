// Package goalloop implements the self-driven Goal Loop:
// given a feature goal, the loop plans, implements, evaluates, and judges
// until the goal is complete or the budget is exhausted.
package goalloop

// Goal is a user-facing feature request with a working directory.
type Goal struct {
	Text    string // natural-language description of the feature
	Workdir string // absolute path to the repo working directory
}

// AcceptanceTest is a single executable check that must pass for the goal to be complete.
type AcceptanceTest struct {
	Name    string // human-readable test name
	Command string // shell command, e.g. "go test ./..."
}

// Plan is the output of the Planner: the goal, acceptance tests, and implementation steps.
type Plan struct {
	Goal  string           // echoed goal text
	Tests []AcceptanceTest // acceptance tests that must pass
	Steps []string         // ordered implementation steps
}

// EvalVerdict is the result of one Evaluator examining the implementation.
type EvalVerdict struct {
	Evaluator string   // name of the evaluator (e.g. "test", "intent", "quality")
	Pass      bool     // whether this evaluator considers the goal met
	Evidence  string   // supporting output (e.g. test logs, LLM reasoning)
	Gaps      []string // specific gaps if not passing
}

// Decision is the Judge's aggregate verdict: is the goal complete?
type Decision struct {
	Complete bool     // true if all evaluators pass
	Gaps     []string // concatenated gaps from all failing evaluators
	Summary  string   // one-line summary of the decision
	// StopReason explains why the loop terminated (G02 persistence + G03
	// escalation). Empty (StopReasonComplete) when the goal completed; otherwise
	// one of StopReasonTokenBudget / StopReasonMaxIters / StopReasonEscalate.
	StopReason string
	// Usage is the accumulated token spend at termination (G02). Mirrored from
	// the shared UsageSink so callers (e.g. the CLI's persisted run record) get
	// the final tally without a back-channel.
	Usage Usage
}

// Stop reason constants carried on Decision.StopReason. The zero value
// (StopReasonComplete) means the goal completed successfully.
const (
	StopReasonComplete    = ""
	StopReasonTokenBudget = "token_budget"
	StopReasonMaxIters    = "max_iterations"
	StopReasonEscalate    = "escalate"
)

// Budget limits the Goal Loop's resource consumption.
type Budget struct {
	MaxIterations int // maximum number of plan-implement-evaluate-judge cycles
	MaxTokens     int // token budget across all LLM calls
	SpentTokens   int // tokens consumed so far
}
