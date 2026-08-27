// internal/store/memory_distill.go
//
// C13: memory consolidation that cannot lose a memory.
//
// The memories table is a monotonically growing log. Retrieval takes the newest
// N, so once the table outgrows any sane N the OLDEST rows fall off the end —
// and the oldest rows are the durable preferences ("always run gofmt", "this
// project uses Go 1.26"), i.e. exactly the ones worth keeping. The truncation
// is precisely backwards.
//
// Consolidation fixes the ordering problem by merging: several rows about one
// subject become one row that states the current position, which is then young
// and therefore survives. The danger is obvious and is what shapes this file —
// a merge is a REWRITE of memory, and a bad one is indistinguishable from
// amnesia after the fact.
//
// TWO RULES, BOTH ENFORCED HERE RATHER THAN BY THE CALLER:
//
//  1. NOTHING IS DELETED. ApplyDistillation inserts the merged row and marks
//     its inputs superseded. The originals keep their bytes, their ids and
//     their FTS entries; they are only hidden from the default query. A wrong
//     merge is therefore reversible, and an operator can always ask what the
//     distiller actually consumed.
//
//  2. THE LINEAGE IS RECORDED IN THE SAME TRANSACTION as the insert. An id
//     list written afterwards is an id list that can be missing, and a merged
//     row whose provenance is unknown is worse than no merge: it asserts a
//     consolidated preference nobody can trace.
//
// The failure direction is the same one C1 chose for eviction: when the
// distillation cannot be completed, the originals stay current and visible.
// Losing a merge costs one model call; losing the memories costs the memories.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MemoryDistillation is one merge: the rows consumed and the row to write in
// their place.
//
// It is a VALUE the caller builds and this package applies, so the model call
// that decides what to merge lives outside the store and the transaction that
// records it lives inside. The split is what lets the distiller be tested
// without a database and the store be tested without a model.
type MemoryDistillation struct {
	// SourceIDs are the memories being consolidated, and must all exist.
	SourceIDs []string
	// Content is the merged text.
	Content string
	// Kind categorises the merged row; '' becomes DistilledKind.
	Kind string
	// Dims are the retrieval dimensions to tag the merged row with. A caller
	// distilling one session's memories should pass that session, so the
	// merged row stays findable under the same scope as its inputs.
	Dims MemoryFilter
}

// DistilledKind is the kind assigned to a merged memory when the caller names
// none. It is distinct from "note" so a reader — human or model — can tell a
// consolidated statement from something somebody wrote directly.
const DistilledKind = "distilled"

// MaxDistillInputs caps how many rows one distillation may consume.
//
// The cap is not about SQL limits; it is about blast radius. A distillation is
// driven by a model, and a model that misreads its instructions merges what it
// was given — so the quantity it is given is the quantity at risk from one bad
// call. Bounding it means a wrong merge hides at most this many memories, and
// the next pass gets another chance at the rest. It also keeps the merge input
// inside any reasonable summary window without a second chunking scheme.
const MaxDistillInputs = 40

// ApplyDistillation writes a merge atomically and returns the new row's id.
//
// It fails — changing nothing — when the merged content is empty, when fewer
// than two sources are named (a "merge" of one row is a rewrite, and this is
// not the API for rewriting a memory), when more than MaxDistillInputs are
// named, when any source id does not exist, or when any source has already
// been superseded.
//
// THE ALREADY-SUPERSEDED CHECK IS THE INTERESTING ONE. Without it, two
// distillation passes racing over the same rows both succeed, and the second
// one's merged row cites inputs that are themselves hidden — a lineage
// pointing at rows the default query will not return, which is exactly the
// "the trace exists but leads nowhere" shape this file was written to avoid.
// Making it an error means the loser retries against the current state.
func (s *Store) ApplyDistillation(d MemoryDistillation) (string, error) {
	if strings.TrimSpace(d.Content) == "" {
		return "", fmt.Errorf("store: distillation: merged content is empty; " +
			"refusing to replace memories with nothing")
	}
	if len(d.SourceIDs) < 2 {
		return "", fmt.Errorf("store: distillation: %d source(s); a merge needs at least 2",
			len(d.SourceIDs))
	}
	if len(d.SourceIDs) > MaxDistillInputs {
		return "", fmt.Errorf("store: distillation: %d sources exceeds the limit of %d",
			len(d.SourceIDs), MaxDistillInputs)
	}
	seen := make(map[string]bool, len(d.SourceIDs))
	for _, id := range d.SourceIDs {
		if id == "" {
			return "", fmt.Errorf("store: distillation: empty source id")
		}
		if seen[id] {
			return "", fmt.Errorf("store: distillation: source %q listed twice", id)
		}
		seen[id] = true
	}

	kind := d.Kind
	if kind == "" {
		kind = DistilledKind
	}
	newID := newID()
	now := time.Now().Unix()

	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		// Verify every source first, inside the transaction, so the insert and
		// the check cannot disagree with a concurrent writer.
		for _, id := range d.SourceIDs {
			var superseded string
			e := tx.QueryRow("SELECT superseded_by FROM memories WHERE id = ?", id).Scan(&superseded)
			if e == sql.ErrNoRows {
				return fmt.Errorf("store: distillation: source memory %q does not exist", id)
			}
			if e != nil {
				return fmt.Errorf("store: distillation: read source %q: %w", id, e)
			}
			if superseded != "" {
				return fmt.Errorf("store: distillation: source memory %q was already superseded by %q",
					id, superseded)
			}
		}
		if _, e := tx.Exec(
			`INSERT INTO memories
			   (id, kind, content, session_id, agent_id, created_at,
			    distilled_from, superseded_by, distilled_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, '', ?)`,
			newID, kind, d.Content, d.Dims.SessionID, d.Dims.AgentID, now,
			joinIDs(d.SourceIDs), now,
		); e != nil {
			return fmt.Errorf("store: distillation: insert merged memory: %w", e)
		}
		for _, id := range d.SourceIDs {
			if _, e := tx.Exec(
				"UPDATE memories SET superseded_by = ? WHERE id = ?", newID, id,
			); e != nil {
				return fmt.Errorf("store: distillation: supersede %q: %w", id, e)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return newID, nil
}

// DistillCandidates returns the current (non-superseded) memories a
// distillation pass should consider, OLDEST FIRST and capped at limit.
//
// Oldest-first is the whole point and is the opposite of every other read in
// this file. Retrieval wants the newest because they are the most likely to be
// relevant; consolidation wants the oldest because they are the ones about to
// fall off the end of every retrieval, and merging a batch of fresh rows would
// leave the endangered ones exactly where they were.
//
// THE TIEBREAK IS rowid, NOT id, and that is not a detail. created_at has
// one-second resolution, so a burst of memories written in one turn all carry
// the same timestamp — which is the normal case, not an edge one. id is a
// random 12-byte value, so ordering by it shuffles that burst arbitrarily and
// two calls can hand back different "oldest" rows. rowid is SQLite's insertion
// counter, so it recovers the true write order inside a second and makes the
// batch a caller sees reproducible.
//
// limit <= 0 selects MaxDistillInputs. Values above it are clamped, so a
// caller cannot assemble a batch this package would then refuse to apply.
func (s *Store) DistillCandidates(limit int, dims MemoryFilter) ([]Memory, error) {
	if limit <= 0 || limit > MaxDistillInputs {
		limit = MaxDistillInputs
	}
	dims.IncludeSuperseded = false
	cond, args := dims.where("")
	q := "SELECT " + memoryColumns + " FROM memories WHERE 1=1" + cond +
		" ORDER BY created_at ASC, rowid ASC LIMIT ?"
	all := append([]any{}, args...)
	all = append(all, limit)
	rows, err := s.DB.Query(q, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMemories(rows)
}

// CountMemories returns how many current (non-superseded) memories match dims.
// It is what a caller polls to decide whether a distillation pass is due, so
// it counts exactly the rows that retrieval would consider.
func (s *Store) CountMemories(dims MemoryFilter) (int, error) {
	dims.IncludeSuperseded = false
	cond, args := dims.where("")
	var n int
	if err := s.DB.QueryRow(
		"SELECT COUNT(*) FROM memories WHERE 1=1"+cond, args...,
	).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// MemoryByID returns one memory regardless of whether it has been superseded.
//
// Deliberately unfiltered: this is the read that makes the lineage usable. A
// caller following DistilledFrom is asking for rows the default query hides by
// construction, and a lookup that also hid them would make the audit trail
// unreadable — the failure mode where the data is kept and nothing can see it.
func (s *Store) MemoryByID(id string) (Memory, error) {
	rows, err := s.DB.Query("SELECT "+memoryColumns+" FROM memories WHERE id = ?", id)
	if err != nil {
		return Memory{}, err
	}
	defer rows.Close()
	ms, err := scanMemories(rows)
	if err != nil {
		return Memory{}, err
	}
	if len(ms) == 0 {
		return Memory{}, fmt.Errorf("store: memory %q not found", id)
	}
	return ms[0], nil
}
