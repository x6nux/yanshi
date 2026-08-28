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

// goalState is the durable slice of an in-flight goal run: enough to restart
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
type goalState struct {
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
// directory. Keying on the directory rather than the goal text means one row
// per repo that gets overwritten, instead of a row per goal text accumulating
// forever; a different objective in the same directory is rejected by the
// objective check in loadState, which is what makes the single row safe to
// reuse.
//
// Nothing enforces one run at a time, and this is worth stating plainly
// because the obvious reading of "one row per directory" is that something
// does. Two `yanshi goal` processes in the same directory share this key: with
// the same objective they will each resume from whatever the other last wrote
// and both count against one budget; with different objectives each load
// invalidates the other's row and neither ever resumes. Last write wins, and
// the damage is confined to the resume point — no partial rows, since each
// write is a whole blob in one transaction.
//
// ponytail: not locked. The failure needs two concurrent goal runs in one
// directory, which is not a normal way to use this command, and the honest
// mechanism (a lease with a liveness check, as internal/lockfile does for
// backend election) is a lot of machinery for it. Add one if goal runs ever
// become something a scheduler fires off.
func goalStateKey(workdir string) string {
	return "goalstate:" + workdir
}

// ResetGoalState discards any resume point for workdir, so the next run of
// that goal starts from iteration 1 with a full budget.
//
// It marks the stored run finished rather than deleting the row: "finished" is
// already the state loadState refuses to resume from, so this reuses the
// existing invalidation instead of adding a delete to the storage port for one
// caller. The row is overwritten by the next run anyway.
//
// This is the supported way out of a resumed budget the operator no longer
// wants. Without it the escape hatch is editing SQLite by hand.
func ResetGoalState(st StateStore, workdir string) error {
	blob, err := json.Marshal(goalState{Complete: true})
	if err != nil {
		return err
	}
	return st.KVSet(goalStateKey(workdir), string(blob))
}

// loadState returns the resumable state for g, if any. ok is false when there
// is no state, when persistence is not wired, or when the stored state belongs
// to a different or already-finished goal.
//
// A decode error is returned rather than swallowed, but it is not fatal: Run
// reports it and starts from iteration 1, because a corrupt state row is a
// reason to lose the resume point, not a reason to refuse to work.
func (l *Loop) loadState(g Goal) (goalState, bool, error) {
	if l.cfg.State == nil {
		return goalState{}, false, nil
	}
	blob, ok, err := l.cfg.State.KVGet(goalStateKey(g.Workdir))
	if err != nil || !ok {
		return goalState{}, false, err
	}
	var st goalState
	if err := json.Unmarshal([]byte(blob), &st); err != nil {
		return goalState{}, false, fmt.Errorf("decode goal state: %w", err)
	}
	if st.Complete || st.Objective != g.Text {
		return goalState{}, false, nil
	}
	return st, true, nil
}

// saveState writes the run's position for g: the iterations that COMPLETED,
// and the spend as of this moment.
//
// Run calls it once per finished iteration and once more on the way out. The
// per-iteration call is the one that matters, because the only interruptions
// that let a deferred flush run are the polite ones; a SIGKILL, an OOM kill or
// a power cut leave behind exactly what the last iteration committed. So the
// persisted position can lag the live one by at most the iteration in flight,
// and it never leads it.
//
// Overwriting rather than appending is deliberate — one row per working
// directory, holding the latest position. See goalStateKey for what that costs.
//
// A nil State disables persistence entirely and Run behaves byte-for-byte as
// it did before, which is why the zero Config stays usable.
func (l *Loop) saveState(g Goal, complete bool) error {
	if l.cfg.State == nil {
		return nil
	}
	blob, err := json.Marshal(goalState{
		Objective:  g.Text,
		Budget:     l.cfg.Budget,
		Iterations: l.persistedIter,
		Usage:      l.usageSnapshot(),
		Complete:   complete,
	})
	if err != nil {
		return err
	}
	return l.cfg.State.KVSet(goalStateKey(g.Workdir), string(blob))
}
