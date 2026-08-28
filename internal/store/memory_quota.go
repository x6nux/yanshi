// internal/store/memory_quota.go
//
// W-D-03's storage half: memories are counted when they are read, and pruned
// when there are too many.
//
// The counter is what makes "unused" mean anything. Without it a quota can only
// prune by age, and age is the wrong axis for memory: the note that has been
// answering questions for six months is the oldest one in the table.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
)

// markMemoriesUsed bumps the retrieval counter for the rows a search returned.
//
// It is called from the SHARED read paths (SearchMemoryRanked and
// RecallMemoryScoped) rather than from each caller, so every consumer —
// memory_search, memory_recall, autorecall — feeds the counter without knowing
// it exists. A per-caller version would have covered whichever call sites were
// remembered on the day it was written.
//
// A FAILURE HERE MUST NOT FAIL THE READ. The counter is bookkeeping for a
// prune that only runs if an operator turned a quota on; losing a tick of it
// costs a slightly worse eviction decision much later, whereas failing the
// search costs the model the memory it just asked for.
//
// COST, STATED: `memories` carries an AFTER UPDATE trigger that reindexes the
// row in memories_fts, so this write is not free — it re-indexes up to `limit`
// rows per search. That is acceptable here because searches are model-driven
// (single digits per turn) and limit defaults to 10. It would not be if this
// ever moved onto a hot loop.
func (s *Store) markMemoriesUsed(ids []string) {
	if len(ids) == 0 {
		return
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec(
			"UPDATE memories SET use_count = use_count + 1 WHERE id IN ("+
				placeholders(len(ids))+")", args...)
		return e
	})
	if err != nil {
		slog.Warn("store: could not record memory use", "count", len(ids), "error", err)
	}
}

// memoryIDs collects the ids of a hit list, for markMemoriesUsed.
func memoryIDs(hits []MemoryHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ID)
	}
	return out
}

// PruneUnusedMemories trims the table down to quota rows and returns how many
// it deleted.
//
// quota <= 0 DELETES NOTHING. That is the default, and it has to be: this is
// the only function in the memory subsystem that destroys a row rather than
// superseding one (contrast ApplyDistillation, which never deletes), so it must
// be something an operator switched on deliberately.
//
// WHAT IT DELETES, IN ORDER: only rows with use_count = 0, oldest first. A
// memory that has ever been retrieved is never pruned by this, however old —
// which is the property that separates a quota from a expiry, and the reason
// the counter exists at all. If every row over quota has been used, this
// deletes nothing and the table is allowed to exceed the quota; a quota that
// evicted proven-useful notes to hit a number would be worse than no quota.
//
// Superseded rows count toward the total and are eligible like any other. They
// are already invisible to retrieval, so they are the cheapest thing to lose —
// but they are not prioritised, because the lineage they carry is the audit
// trail for a distillation and oldest-first already reaches them first in
// practice.
func (s *Store) PruneUnusedMemories(quota int) (int, error) {
	if quota <= 0 {
		return 0, nil
	}
	var total int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM memories").Scan(&total); err != nil {
		return 0, fmt.Errorf("store: count memories: %w", err)
	}
	excess := total - quota
	if excess <= 0 {
		return 0, nil
	}
	var deleted int
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		res, e := tx.Exec(
			`DELETE FROM memories WHERE id IN (
			   SELECT id FROM memories WHERE use_count = 0
			   ORDER BY created_at ASC, rowid ASC LIMIT ?)`, excess)
		if e != nil {
			return fmt.Errorf("store: prune memories: %w", e)
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		deleted = int(n)
		return nil
	})
	return deleted, err
}

// MemoryUseCount returns how many times a memory has been retrieved. It exists
// so a test can assert the counter moves; production reads it through the
// prune's ORDER BY.
func (s *Store) MemoryUseCount(id string) (int, error) {
	var n int
	err := s.DB.QueryRow("SELECT use_count FROM memories WHERE id = ?", id).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("store: memory %q not found", id)
	}
	return n, err
}
