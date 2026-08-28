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

// GoalState is the durable slice of an in-flight goal run: enough to restart
// the process and pick the run back up where it stopped, with the budget it
// had left rather than a fresh one.
//
// Every field is read on resume, none is write-only:
//   - Objective is the resume predicate (see Loop.loadState).
//   - Budget is authoritative for the resumed run — the whole point of
//     persisting it is that a restart must not silently hand the run a new
//     budget out of config defaults.
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
