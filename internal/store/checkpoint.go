// internal/store/checkpoint.go
//
// W-D-06: one named moment, three dimensions, restorable one at a time.
//
// (checkpoint_test.go in this package is about SQLite's WAL checkpoint and is
// unrelated; the tests for this file are in checkpoint_restore_test.go.)
//
// THIS FILE ORCHESTRATES; IT DOES NOT INVENT A SECOND SNAPSHOT MECHANISM. Every
// dimension already had one before W-D-06, and the interesting part of the work
// was deciding which:
//
//   - The SESSION dimension needs nothing stored but a boundary. `messages` and
//     `context_events` are both append-only, so no past state is ever destroyed;
//     restoring means appending one more event that puts the window back where
//     it was. That is ADR-0015's "checkpoints degrade to appending one event",
//     and restoreBoundaryTx is the existing function that does it.
//
//   - The MEMORY dimension needs a real copy, because `memories` is the one
//     table here that is UPDATEd and DELETEd (use_count, distillation lineage,
//     the quota prune, /memory-clear). There is no append-only history to
//     project a past state out of, so the rows are serialised with W-D-04's
//     gzip encoder.
//
//   - The FILE dimension is not in this file at all. internal/vcs already
//     stores every version of every tracked file and already has preview,
//     freeze, external-mutation detection and apply. A checkpoint records the
//     commit id and nothing else; see vcs.VCS.RestoreCheckpointFiles.
//
// THE THREE ACCEPTANCE PROPERTIES ARE STRUCTURAL, NOT SEPARATE FEATURES:
//
//	auto-snapshot before restore  RestoreCheckpoint's first act is
//	                              createCheckpointTx, in the same transaction.
//	writers paused during restore that transaction is a WriteTx, which holds
//	                              writeMu — the mutex EVERY writer in this
//	                              process takes. There is no second lock.
//	dry-run first                 PlanCheckpointRestore only reads.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CheckpointDimension names one restorable half of a checkpoint.
type CheckpointDimension string

// The three dimensions. They are restored ONE AT A TIME (the acceptance says
// "selectively restore one of session/memory/files") — there is deliberately no
// "all" value, because the three have different blast radii and different
// failure modes, and a caller that wants two can ask twice and see two plans.
const (
	// CheckpointSession restores the context window boundary of the session the
	// checkpoint was taken in.
	CheckpointSession CheckpointDimension = "session"
	// CheckpointMemory restores the whole memories table.
	CheckpointMemory CheckpointDimension = "memory"
	// CheckpointFiles restores the working copy. Handled by internal/vcs; the
	// store only records the commit id.
	CheckpointFiles CheckpointDimension = "files"
)

// CheckpointDimensions lists the dimensions in a stable order, for help text
// and for tests that must cover all of them.
func CheckpointDimensions() []CheckpointDimension {
	return []CheckpointDimension{CheckpointSession, CheckpointMemory, CheckpointFiles}
}

// Checkpoint is one recorded moment.
//
// The memory blob is NOT a field. It is an implementation detail of the memory
// dimension, it can be megabytes, and every caller that has wanted a Checkpoint
// so far wanted to list or name one. It is read straight from the row at
// restore time.
type Checkpoint struct {
	ID        string
	Label     string
	CreatedAt int64

	// SessionID is the session whose window boundary was captured, or "" when
	// the checkpoint was taken outside a session. A session-dimension restore
	// of such a checkpoint is an error rather than a no-op: silently restoring
	// nothing is the failure this work package keeps finding.
	SessionID string
	// HiddenSeq / PinnedSeqs are the captured context boundary (ADR-0015).
	HiddenSeq  int
	PinnedSeqs []int
	// FileCommit is the vcs commit the working copy stood at, or "" when no
	// repository was configured.
	FileCommit string
	// Memories counts the rows in the snapshot. Kept so a plan can report the
	// size of a restore without decompressing the blob.
	Memories int
}

// ErrNoCheckpointDimension reports that a checkpoint carries nothing for the
// dimension asked for — no session, or no file commit.
//
// A distinct error because the alternative is worse in a specific way: a
// restore that finds nothing to restore and reports success has told the user
// their state was rolled back when it was not.
var ErrNoCheckpointDimension = errors.New("store: checkpoint has nothing for that dimension")

// checkpointMemory is one memories row as it is snapshotted.
//
// It is a separate type from Memory and lists EVERY column, including the two
// Memory deliberately omits (use_count, and the W-D-07 provenance pair). A
// snapshot that restored the rows but reset their retrieval counts would hand
// the quota a table that looks entirely unused and prune it on the next sweep.
type checkpointMemory struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Content         string `json:"content"`
	SessionID       string `json:"session_id"`
	AgentID         string `json:"agent_id"`
	CreatedAt       int64  `json:"created_at"`
	DistilledFrom   string `json:"distilled_from"`
	SupersededBy    string `json:"superseded_by"`
	DistilledAt     int64  `json:"distilled_at"`
	UseCount        int    `json:"use_count"`
	SourceSessionID string `json:"source_session_id"`
	SourceSeq       int    `json:"source_seq"`
}

// checkpointMemoryColumns is the snapshot's column list. Shared by the read and
// the write so a new column cannot be captured and not restored.
const checkpointMemoryColumns = "id, kind, content, session_id, agent_id, created_at, " +
	"distilled_from, superseded_by, distilled_at, use_count, source_session_id, source_seq"

// snapshotMemoriesTx reads every memories row for a checkpoint.
func snapshotMemoriesTx(tx *sql.Tx) ([]checkpointMemory, error) {
	rows, err := tx.Query("SELECT " + checkpointMemoryColumns + " FROM memories ORDER BY rowid ASC")
	if err != nil {
		return nil, fmt.Errorf("store: snapshot memories: %w", err)
	}
	defer rows.Close()
	out := []checkpointMemory{}
	for rows.Next() {
		var m checkpointMemory
		if err := rows.Scan(&m.ID, &m.Kind, &m.Content, &m.SessionID, &m.AgentID,
			&m.CreatedAt, &m.DistilledFrom, &m.SupersededBy, &m.DistilledAt,
			&m.UseCount, &m.SourceSessionID, &m.SourceSeq); err != nil {
			return nil, fmt.Errorf("store: scan snapshot memory: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreateCheckpoint records the current state and returns the checkpoint.
//
// sessionID and fileCommit are supplied by the caller because the store cannot
// know either: the active session belongs to the connection, and the head
// commit belongs to internal/vcs, which imports this package and therefore
// cannot be imported back. Both may be empty; the dimensions they feed then
// report ErrNoCheckpointDimension at restore time instead of silently doing
// nothing.
func (s *Store) CreateCheckpoint(label, sessionID, fileCommit string) (Checkpoint, error) {
	var cp Checkpoint
	err := s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		var e error
		cp, e = createCheckpointTx(tx, label, sessionID, fileCommit)
		return e
	})
	if err != nil {
		return Checkpoint{}, err
	}
	return cp, nil
}

// createCheckpointTx is CreateCheckpoint's body, exposed to the restore path so
// the automatic pre-restore snapshot and the restore itself are ONE atomic act.
// Two transactions would leave a window in which the snapshot exists and the
// restore did not happen, which is the state that looks like a successful
// rollback and is not one.
func createCheckpointTx(tx *sql.Tx, label, sessionID, fileCommit string) (Checkpoint, error) {
	cp := Checkpoint{
		ID:         newID(),
		Label:      label,
		SessionID:  sessionID,
		FileCommit: fileCommit,
		CreatedAt:  time.Now().Unix(),
	}
	if sessionID != "" {
		b, err := boundaryTx(tx, sessionID)
		if err != nil {
			return Checkpoint{}, err
		}
		cp.HiddenSeq, cp.PinnedSeqs = b.HiddenSeq, b.PinnedSeqs
	}
	pinned, err := encodePinnedSeqs(cp.PinnedSeqs)
	if err != nil {
		return Checkpoint{}, err
	}
	mems, err := snapshotMemoriesTx(tx)
	if err != nil {
		return Checkpoint{}, err
	}
	blob, err := encodeGzipJSON(mems)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("store: encode checkpoint memories: %w", err)
	}
	cp.Memories = len(mems)
	if _, err := tx.Exec(
		`INSERT INTO checkpoints
		   (id, label, session_id, hidden_seq, pinned_seqs, memories, memory_count,
		    file_commit, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		cp.ID, cp.Label, cp.SessionID, cp.HiddenSeq, pinned, blob, cp.Memories,
		cp.FileCommit, cp.CreatedAt,
	); err != nil {
		return Checkpoint{}, fmt.Errorf("store: write checkpoint: %w", err)
	}
	return cp, nil
}

// checkpointColumns is the canonical SELECT list for the metadata half. The
// blob is excluded on purpose: listing checkpoints must not read megabytes.
const checkpointColumns = "id, label, session_id, hidden_seq, pinned_seqs, " +
	"memory_count, file_commit, created_at"

// scanCheckpoint reads one metadata row.
func scanCheckpoint(row interface{ Scan(...any) error }) (Checkpoint, error) {
	var cp Checkpoint
	var pinned string
	if err := row.Scan(&cp.ID, &cp.Label, &cp.SessionID, &cp.HiddenSeq, &pinned,
		&cp.Memories, &cp.FileCommit, &cp.CreatedAt); err != nil {
		return Checkpoint{}, err
	}
	cp.PinnedSeqs = decodePinnedSeqs(pinned)
	return cp, nil
}

// Checkpoints returns the most recent checkpoints, newest first.
func (s *Store) Checkpoints(limit int) ([]Checkpoint, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.DB.Query(
		"SELECT "+checkpointColumns+" FROM checkpoints ORDER BY created_at DESC, id DESC LIMIT ?",
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Checkpoint
	for rows.Next() {
		cp, err := scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

// CheckpointByID returns one checkpoint's metadata.
func (s *Store) CheckpointByID(id string) (Checkpoint, error) {
	cp, err := scanCheckpoint(s.DB.QueryRow(
		"SELECT "+checkpointColumns+" FROM checkpoints WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Checkpoint{}, fmt.Errorf("store: no checkpoint %q", id)
	}
	return cp, err
}

// CheckpointPlan is what a restore WOULD do. It is produced without touching
// anything, which is the fourth acceptance clause.
//
// Before/After are counts of the thing the dimension restores — window messages
// for the session dimension, memory rows for the memory one — because that is
// the number a human needs to decide whether to confirm. A plan that only said
// "will restore the session" tells the operator nothing they did not type.
type CheckpointPlan struct {
	Checkpoint Checkpoint
	Dimension  CheckpointDimension
	Before     int
	After      int
	// Summary is a one-line human rendering, built here so every carrier (TUI,
	// logs, an ack frame) shows the same words.
	Summary string
}

// PlanCheckpointRestore describes what restoring dim from a checkpoint would
// do, WITHOUT writing anything.
//
// The files dimension is planned by internal/vcs (PlanRestore), which is where
// the working copy is; this function reports the target commit and refuses to
// guess at file counts it cannot see.
func (s *Store) PlanCheckpointRestore(id string, dim CheckpointDimension) (CheckpointPlan, error) {
	cp, err := s.CheckpointByID(id)
	if err != nil {
		return CheckpointPlan{}, err
	}
	plan := CheckpointPlan{Checkpoint: cp, Dimension: dim}
	switch dim {
	case CheckpointSession:
		if cp.SessionID == "" {
			return CheckpointPlan{}, fmt.Errorf("%w: no session (checkpoint %s)",
				ErrNoCheckpointDimension, cp.ID)
		}
		here, err := s.boundary(cp.SessionID)
		if err != nil {
			return CheckpointPlan{}, err
		}
		now, err := s.messagesInWindow(cp.SessionID, here.HiddenSeq, here.PinnedSeqs)
		if err != nil {
			return CheckpointPlan{}, err
		}
		then, err := s.messagesInWindow(cp.SessionID, cp.HiddenSeq, cp.PinnedSeqs)
		if err != nil {
			return CheckpointPlan{}, err
		}
		plan.Before, plan.After = len(now), len(then)
		plan.Summary = fmt.Sprintf(
			"session %s: context window %d → %d messages (boundary %d → %d, one appended event)",
			cp.SessionID, plan.Before, plan.After, here.HiddenSeq, cp.HiddenSeq)
	case CheckpointMemory:
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM memories").Scan(&plan.Before); err != nil {
			return CheckpointPlan{}, err
		}
		plan.After = cp.Memories
		plan.Summary = fmt.Sprintf("memories: %d → %d rows (the whole table is replaced)",
			plan.Before, plan.After)
	case CheckpointFiles:
		if cp.FileCommit == "" {
			return CheckpointPlan{}, fmt.Errorf("%w: no file commit (checkpoint %s)",
				ErrNoCheckpointDimension, cp.ID)
		}
		plan.Summary = "files: working copy → commit " + cp.FileCommit +
			" (planned by the vcs layer, which can see the working copy)"
	default:
		return CheckpointPlan{}, fmt.Errorf("store: unknown checkpoint dimension %q", dim)
	}
	return plan, nil
}

// RestoreCheckpoint rolls one dimension back and returns the checkpoint it took
// on the way in, so the restore itself is undoable.
//
// EVERYTHING HAPPENS IN ONE WriteTx, AND THAT IS THREE ACCEPTANCE CLAUSES AT
// ONCE. The transaction holds writeMu, the mutex every writer in this process
// takes, so no other write lands between the snapshot and the restore — that is
// "writers are paused during a restore", implemented by using the lock that
// already exists rather than inventing a second one. It also makes the
// automatic snapshot atomic with the restore: two transactions would leave a
// reachable state where the snapshot exists and the rollback did not happen,
// which reads exactly like a successful rollback and is not one.
//
// THE FILE DIMENSION IS NOT SERVED HERE. Materialising a working copy is
// filesystem work under a repo lane and a freeze, none of which belongs inside
// a database transaction, and internal/vcs owns all three. Asking for it here
// is an error naming the function that does serve it, rather than a silent
// no-op.
func (s *Store) RestoreCheckpoint(id string, dim CheckpointDimension) (Checkpoint, error) {
	if dim == CheckpointFiles {
		return Checkpoint{}, fmt.Errorf(
			"store: the files dimension is restored by vcs.VCS.RestoreCheckpointFiles, not here")
	}
	if dim != CheckpointSession && dim != CheckpointMemory {
		return Checkpoint{}, fmt.Errorf("store: unknown checkpoint dimension %q", dim)
	}
	// Read the target OUTSIDE the transaction: a missing checkpoint must not
	// cost an automatic snapshot, and CheckpointByID takes no write lane.
	cp, err := s.CheckpointByID(id)
	if err != nil {
		return Checkpoint{}, err
	}
	if dim == CheckpointSession && cp.SessionID == "" {
		return Checkpoint{}, fmt.Errorf("%w: no session (checkpoint %s)",
			ErrNoCheckpointDimension, cp.ID)
	}

	var undo Checkpoint
	err = s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		// Snapshot FIRST. The label names what it is for, because the only time
		// anyone looks at this row is after a restore they want to take back.
		var e error
		undo, e = createCheckpointTx(tx, "before restoring "+cp.ID+" ("+string(dim)+")",
			cp.SessionID, cp.FileCommit)
		if e != nil {
			return e
		}
		if dim == CheckpointSession {
			return restoreBoundaryTx(tx, cp.SessionID, contextBoundary{
				HiddenSeq: cp.HiddenSeq, PinnedSeqs: cp.PinnedSeqs,
			})
		}
		return restoreMemoriesTx(tx, id)
	})
	if err != nil {
		return Checkpoint{}, err
	}
	return undo, nil
}

// restoreMemoriesTx replaces the whole memories table with a checkpoint's blob.
//
// DELETE-then-INSERT rather than a merge. A merge would have to decide what to
// do with rows written after the checkpoint, and there is no answer that is
// right for both of the reasons someone restores memories: undoing a bad
// distillation (keep nothing new) and undoing an accidental wipe (keep
// everything new). Replacing is the one behaviour that matches what the word
// means, and the automatic pre-restore snapshot is what makes it safe.
//
// The FTS index follows automatically: memories_ad fires on the delete and
// memories_ai on each insert, so the shadow table ends up describing exactly
// the rows that are there. Rebuilding it by hand would be a second mechanism
// that could disagree with the triggers.
func restoreMemoriesTx(tx *sql.Tx, checkpointID string) error {
	var blob []byte
	if err := tx.QueryRow(
		"SELECT memories FROM checkpoints WHERE id = ?", checkpointID,
	).Scan(&blob); err != nil {
		return fmt.Errorf("store: read checkpoint memories: %w", err)
	}
	rows, err := decodeGzipJSON[checkpointMemory](blob)
	if err != nil {
		return fmt.Errorf("store: decode checkpoint memories: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM memories"); err != nil {
		return fmt.Errorf("store: clear memories for restore: %w", err)
	}
	for _, m := range rows {
		if _, err := tx.Exec(
			`INSERT INTO memories (`+checkpointMemoryColumns+`)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.Kind, m.Content, m.SessionID, m.AgentID, m.CreatedAt,
			m.DistilledFrom, m.SupersededBy, m.DistilledAt, m.UseCount,
			m.SourceSessionID, m.SourceSeq,
		); err != nil {
			return fmt.Errorf("store: restore memory %s: %w", m.ID, err)
		}
	}
	return nil
}
