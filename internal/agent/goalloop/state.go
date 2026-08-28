package goalloop

import (
	"encoding/json"
	"fmt"
)

// StateStore is the narrow persistence port the Goal Loop needs in order to
// survive a process restart: a durable, string-keyed blob store.
//
// It is deliberately NOT internal/store's concrete type. goalloop has never
// imported the persistence layer, and it should not start: every goal-loop
// test would then need a SQLite file to construct a Loop, and the package's
// dependency direction would flow outward instead of inward. Declaring the
// port here and letting the composition site supply the implementation keeps
// both properties.
//
// *store.Store satisfies this interface as it stands — no adapter, no new
// table — because its kv table already holds arbitrary string values. That is
// the same reasoning RunRecord was built on: a JSON round-trip through kv
// needs no schema migration, and the goal loop's state is a handful of scalars
// that nothing queries by column.
type StateStore interface {
	KVGet(key string) (value string, ok bool, err error)
	KVSet(key, value string) error
}

// BudgetSet records which budget limits the operator set EXPLICITLY for this
// run, per Budget field. It is the tie-breaker between a persisted budget and
// the one the caller supplied, and it exists because the two look identical by
// the time they reach the Loop: a value typed on the command line and a value
// that fell out of a config default are both just ints.
//
// Value-based guesses ("it differs from the default, so it was typed") are
// wrong in both directions here — -max-tokens 0 is a real instruction to lift
// the limit, and typing the default value is still typing it. Only the flag
// package knows, via FlagSet.Visit, which is why this arrives as its own field
// instead of being inferred.
//
// The zero value means nothing was explicit, so a resumed run keeps its
// persisted budget.
type BudgetSet struct {
	MaxIterations bool
	MaxTokens     bool
}

// GoalState is the durable slice of an in-flight goal run: enough to restart
// the process and pick the run back up where it stopped, with the budget it
// had left rather than a fresh one.
//
// Every field is read on resume, none is write-only:
//   - Objective is the resume predicate (see Loop.loadState).
//   - Budget is what a resumed run falls back on, so that a restart does not
//     silently reset the limits to whatever config defaults happen to say.
//     An explicitly typed flag still beats it — see resolveResumeBudget.
//   - Iterations is the count already executed; the resumed run starts at
//     Iterations+1.
//   - Usage seeds the shared UsageSink so the token budget resumes mid-spend
//     instead of restarting at zero. Iteration counts are visible in the
//     terminal output; token spend is not, so this is the half that silently
//     multiplies a run's cost when it is missing.
//   - Complete invalidates the state, so re-running a finished goal starts
//     over instead of loading Iterations == MaxIterations and exiting at once.
type GoalState struct {
	Objective  string `json:"objective"`
	Budget     Budget `json:"budget"`
	Iterations int    `json:"iterations"`
	Usage      Usage  `json:"usage"`
	Complete   bool   `json:"complete"`
}

// resolveResumeBudget picks the budget a resumed run must use, per field:
// an explicitly typed limit wins, anything else falls back to the persisted
// one.
//
// The precedence is that way round for a reason on each side. Config defaults
// must NOT win, because nobody re-types their budget after a crash and letting
// the defaults back in is exactly how a budget gets silently reset — the bug
// this whole file exists to fix. But an explicit flag must win, because the
// alternative is the operator editing a value, seeing no effect, and having to
// go dig the old one out of the database (the priority INF2 fixes elsewhere in
// this repo, in the same direction).
//
// A newly typed limit that is already below what the run has spent needs no
// special case, and deliberately does not get one: the resumed run is simply
// over budget on the new ceiling, so the existing termination paths fire on
// the first check — StopReasonTokenBudget before iterating for MaxTokens, and
// the "max iterations reached" decision for MaxIterations, because the resumed
// start index is past the new limit. Lowering a budget below the spend ends
// the run; it does not error and it does not refund.
func resolveResumeBudget(caller, saved Budget, explicit BudgetSet) Budget {
	out := saved
	if explicit.MaxIterations {
		out.MaxIterations = caller.MaxIterations
	}
	if explicit.MaxTokens {
		out.MaxTokens = caller.MaxTokens
	}
	return out
}

// goalStateKey is the kv key holding the resumable state for a working
// directory. The working directory is the identity: `yanshi goal` runs against
// one repo, and one goal is in flight there at a time. A different objective
// in the same directory is rejected by the objective check on load rather than
// by a different key, so the stale row is simply overwritten instead of
// accumulating one row per goal text forever.
func goalStateKey(workdir string) string {
	return "goalstate:" + workdir
}

// loadState returns the resumable state for g, if any. ok is false when there
// is no state, when persistence is not wired, or when the stored state belongs
// to a different or already-finished goal.
//
// A decode error is returned rather than swallowed, but it is not fatal: Run
// reports it and starts from iteration 1, because a corrupt state row is a
// reason to lose the resume point, not a reason to refuse to work.
func (l *Loop) loadState(g Goal) (GoalState, bool, error) {
	if l.cfg.State == nil {
		return GoalState{}, false, nil
	}
	blob, ok, err := l.cfg.State.KVGet(goalStateKey(g.Workdir))
	if err != nil || !ok {
		return GoalState{}, false, err
	}
	var st GoalState
	if err := json.Unmarshal([]byte(blob), &st); err != nil {
		return GoalState{}, false, fmt.Errorf("decode goal state: %w", err)
	}
	if st.Complete || st.Objective != g.Text {
		return GoalState{}, false, nil
	}
	return st, true, nil
}

// saveState writes the run's current position for g. It is called after every
// exit path of Run — including context cancellation, which is the interruption
// this whole mechanism exists for — so the persisted spend is never behind the
// spend that actually happened.
//
// A nil State disables persistence entirely and Run behaves byte-for-byte as
// it did before, which is why the zero Config stays usable.
func (l *Loop) saveState(g Goal, complete bool) error {
	if l.cfg.State == nil {
		return nil
	}
	blob, err := json.Marshal(GoalState{
		Objective:  g.Text,
		Budget:     l.cfg.Budget,
		Iterations: l.iterations,
		Usage:      l.usageSnapshot(),
		Complete:   complete,
	})
	if err != nil {
		return err
	}
	return l.cfg.State.KVSet(goalStateKey(g.Workdir), string(blob))
}
