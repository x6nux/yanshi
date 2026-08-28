package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checkpointFixture lays down a session with a six-row log compacted at seq 3
// and three memories, then returns the store and the session id.
func checkpointFixture(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "cp.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	sid, err := s.CreateSession("checkpointed")
	require.NoError(t, err)
	for i := 0; i < 6; i++ {
		require.NoError(t, s.AppendMessage(sid, i, "user", fmt.Sprintf("line %d", i)))
	}
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 3, nil))
	for _, c := range []string{"memory one", "memory two", "memory three"} {
		_, err := s.WriteMemoryScoped("note", c, MemoryFilter{SessionID: sid})
		require.NoError(t, err)
	}
	return s, sid
}

// dbFingerprint hashes every row of every user table.
//
// A hash of the CONTENT rather than of the file: with WAL journalling the bytes
// on disk move for reasons that have nothing to do with a write (checkpointing,
// page reuse), so a file comparison would be both flaky and, when it passed,
// evidence of nothing in particular. This compares what the database SAYS,
// across every table, so a dry run that wrote anywhere at all is caught —
// including in a table the test author did not think of.
func dbFingerprint(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.DB.Query(
		"SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	require.NoError(t, err)
	var tables []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		tables = append(tables, n)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.NotEmpty(t, tables)

	h := sha256.New()
	for _, tbl := range tables {
		fmt.Fprintf(h, "\n== %s\n", tbl)
		// The FTS5 shadow tables are not readable with SELECT *; their content
		// is derived from `memories` by trigger, which IS covered.
		r, err := s.DB.Query("SELECT * FROM " + tbl) //nolint:gosec // table names come from sqlite_master
		if err != nil {
			fmt.Fprintf(h, "unreadable: %v\n", err)
			continue
		}
		cols, err := r.Columns()
		require.NoError(t, err)
		var lines []string
		for r.Next() {
			cells := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range cells {
				ptrs[i] = &cells[i]
			}
			require.NoError(t, r.Scan(ptrs...))
			lines = append(lines, fmt.Sprintf("%v", cells))
		}
		require.NoError(t, r.Err())
		require.NoError(t, r.Close())
		sort.Strings(lines) // row order is not part of the content
		for _, l := range lines {
			fmt.Fprintln(h, l)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TestCheckpoint_RestoresSelectedDimensionOnly is acceptance clause 1.
//
// Both directions are asserted for both dimensions. "the session came back" is
// only interesting alongside "and the memories did not", because a restore that
// rolled back everything would satisfy the first half of each.
func TestCheckpoint_RestoresSelectedDimensionOnly(t *testing.T) {
	s, sid := checkpointFixture(t)
	cp, err := s.CreateCheckpoint("before the risky bit", sid, "")
	require.NoError(t, err)
	require.Equal(t, 3, cp.HiddenSeq)
	require.Equal(t, 3, cp.Memories)

	// Move BOTH dimensions away from the checkpoint.
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5, nil))
	_, err = s.ClearMemories(MemoryFilter{})
	require.NoError(t, err)
	_, err = s.WriteMemoryScoped("note", "written after the checkpoint", MemoryFilter{})
	require.NoError(t, err)

	win, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	require.Len(t, win, 1, "precondition: the window moved")

	// Session only.
	_, err = s.RestoreCheckpoint(cp.ID, CheckpointSession)
	require.NoError(t, err)
	win, err = s.ProjectWindow(sid)
	require.NoError(t, err)
	assert.Len(t, win, 3, "the session dimension came back")
	mems, err := s.RecallMemory(50)
	require.NoError(t, err)
	require.Len(t, mems, 1, "the memory dimension must NOT have moved")
	assert.Equal(t, "written after the checkpoint", mems[0].Content)

	// Memory only. Move the session away again first so "unchanged" is a real
	// claim rather than a value that happens to already match.
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5, nil))
	_, err = s.RestoreCheckpoint(cp.ID, CheckpointMemory)
	require.NoError(t, err)
	mems, err = s.RecallMemory(50)
	require.NoError(t, err)
	require.Len(t, mems, 3, "the memory dimension came back")
	for _, m := range mems {
		assert.NotEqual(t, "written after the checkpoint", m.Content)
	}
	win, err = s.ProjectWindow(sid)
	require.NoError(t, err)
	assert.Len(t, win, 1, "the session dimension must NOT have moved")
}

// TestCheckpoint_SnapshotsBeforeRestore is acceptance clause 2. The returned
// checkpoint is not merely present — restoring IT must undo the restore, which
// is the only property that makes an automatic snapshot worth taking.
func TestCheckpoint_SnapshotsBeforeRestore(t *testing.T) {
	s, sid := checkpointFixture(t)
	cp, err := s.CreateCheckpoint("original", sid, "")
	require.NoError(t, err)

	_, err = s.ClearMemories(MemoryFilter{})
	require.NoError(t, err)
	_, err = s.WriteMemoryScoped("note", "the state we are about to lose", MemoryFilter{})
	require.NoError(t, err)

	undo, err := s.RestoreCheckpoint(cp.ID, CheckpointMemory)
	require.NoError(t, err)
	require.NotEmpty(t, undo.ID)
	require.NotEqual(t, cp.ID, undo.ID)
	assert.Contains(t, undo.Label, cp.ID, "the undo point must name what it undoes")

	mems, err := s.RecallMemory(50)
	require.NoError(t, err)
	require.Len(t, mems, 3)

	// The whole point: the automatic snapshot is itself restorable.
	_, err = s.RestoreCheckpoint(undo.ID, CheckpointMemory)
	require.NoError(t, err)
	mems, err = s.RecallMemory(50)
	require.NoError(t, err)
	require.Len(t, mems, 1)
	assert.Equal(t, "the state we are about to lose", mems[0].Content)
}

// TestCheckpoint_PausesWritersDuringRestore is acceptance clause 3.
//
// It asserts the restore runs in the SAME write lane every other writer uses,
// by holding that lane and observing that the restore waits. That is the
// property "writers are paused" reduces to in a single-process store: there is
// one writeMu, WriteTx takes it, and anything that takes it excludes everything
// else that does.
//
// Testing it from this side (hold the lock, watch the restore block) rather
// than the other (start a restore, watch a write block) is what avoids needing
// a seam inside the restore to pause at — a seam whose only consumer would be
// this test.
func TestCheckpoint_PausesWritersDuringRestore(t *testing.T) {
	s, sid := checkpointFixture(t)
	cp, err := s.CreateCheckpoint("original", sid, "")
	require.NoError(t, err)

	s.writeMu.Lock()
	done := make(chan error, 1)
	go func() {
		_, e := s.RestoreCheckpoint(cp.ID, CheckpointMemory)
		done <- e
	}()

	select {
	case err := <-done:
		s.writeMu.Unlock()
		t.Fatalf("the restore did not wait for the write lane (err=%v); "+
			"a restore outside writeMu can interleave with any other writer", err)
	case <-time.After(200 * time.Millisecond):
	}

	s.writeMu.Unlock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the restore never completed after the write lane was released")
	}
}

// TestCheckpoint_DryRunProducesPlanWithoutMutating is acceptance clause 4.
//
// The assertion is that the DATABASE CONTENT is unchanged across the plan — not
// merely that a plan came back. A planner that took its own snapshot, or wrote
// an audit row, or bumped a use counter would return a perfectly good plan and
// still have made the "dry" in dry-run false.
func TestCheckpoint_DryRunProducesPlanWithoutMutating(t *testing.T) {
	s, sid := checkpointFixture(t)
	cp, err := s.CreateCheckpoint("original", sid, "cm-1")
	require.NoError(t, err)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5, nil))

	before := dbFingerprint(t, s)

	sessionPlan, err := s.PlanCheckpointRestore(cp.ID, CheckpointSession)
	require.NoError(t, err)
	assert.Equal(t, 1, sessionPlan.Before, "the window is one message today")
	assert.Equal(t, 3, sessionPlan.After, "restoring would put three back")
	assert.Contains(t, sessionPlan.Summary, sid)

	memPlan, err := s.PlanCheckpointRestore(cp.ID, CheckpointMemory)
	require.NoError(t, err)
	assert.Equal(t, 3, memPlan.Before)
	assert.Equal(t, 3, memPlan.After)

	filePlan, err := s.PlanCheckpointRestore(cp.ID, CheckpointFiles)
	require.NoError(t, err)
	assert.Contains(t, filePlan.Summary, "cm-1")

	assert.Equal(t, before, dbFingerprint(t, s),
		"a dry run wrote to the database")

	// Positive control: the fingerprint DOES move when something writes, so the
	// equality above is a real observation and not a hash that never changes.
	_, err = s.WriteMemory("note", "a real write")
	require.NoError(t, err)
	assert.NotEqual(t, before, dbFingerprint(t, s))
}

// TestCheckpoint_MissingDimensionIsAnError: a checkpoint taken with no session
// or no repository must refuse those dimensions rather than report a rollback
// that rolled nothing back.
func TestCheckpoint_MissingDimensionIsAnError(t *testing.T) {
	s, _ := checkpointFixture(t)
	cp, err := s.CreateCheckpoint("no session, no repo", "", "")
	require.NoError(t, err)

	_, err = s.RestoreCheckpoint(cp.ID, CheckpointSession)
	assert.ErrorIs(t, err, ErrNoCheckpointDimension)
	_, err = s.PlanCheckpointRestore(cp.ID, CheckpointSession)
	assert.ErrorIs(t, err, ErrNoCheckpointDimension)
	_, err = s.PlanCheckpointRestore(cp.ID, CheckpointFiles)
	assert.ErrorIs(t, err, ErrNoCheckpointDimension)

	// The files dimension is never served from the store, even when present.
	withFiles, err := s.CreateCheckpoint("has a commit", "", "cm-1")
	require.NoError(t, err)
	_, err = s.RestoreCheckpoint(withFiles.ID, CheckpointFiles)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RestoreCheckpointFiles")

	_, err = s.RestoreCheckpoint(cp.ID, CheckpointDimension("everything"))
	assert.Error(t, err)
	_, err = s.RestoreCheckpoint("nope", CheckpointMemory)
	assert.Error(t, err)
}

// TestCheckpoint_MemorySnapshotKeepsEveryColumn guards the columns the Memory
// struct deliberately does not carry.
//
// use_count is what the quota prunes by, so a restore that reset it would hand
// the next sweep a table that looks entirely unused. The W-D-07 provenance pair
// is the same shape of loss: silent, and unrecoverable once the log moves on.
func TestCheckpoint_MemorySnapshotKeepsEveryColumn(t *testing.T) {
	s, sid := checkpointFixture(t)
	id, err := s.WriteMemoryFromSession("note", "traced and used", MemoryFilter{SessionID: sid})
	require.NoError(t, err)
	_, err = s.SearchMemory("traced used", 5) // bumps use_count
	require.NoError(t, err)
	used, err := s.MemoryUseCount(id)
	require.NoError(t, err)
	require.Equal(t, 1, used)

	cp, err := s.CreateCheckpoint("with counters", sid, "")
	require.NoError(t, err)
	_, err = s.ClearMemories(MemoryFilter{})
	require.NoError(t, err)
	_, err = s.RestoreCheckpoint(cp.ID, CheckpointMemory)
	require.NoError(t, err)

	used, err = s.MemoryUseCount(id)
	require.NoError(t, err)
	assert.Equal(t, 1, used, "the retrieval counter must survive a restore")
	src, err := s.MemorySource(id)
	require.NoError(t, err)
	assert.Len(t, src, 3, "provenance must survive a restore")

	// And the FTS index came back with the rows, rather than staying empty.
	hits, err := s.SearchMemory("traced used", 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)
}

// TestCheckpoint_ListAndLookup covers the metadata reads the carriers use.
func TestCheckpoint_ListAndLookup(t *testing.T) {
	s, sid := checkpointFixture(t)
	first, err := s.CreateCheckpoint("first", sid, "")
	require.NoError(t, err)
	second, err := s.CreateCheckpoint("second", sid, "cm-9")
	require.NoError(t, err)

	got, err := s.Checkpoints(10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	ids := []string{got[0].ID, got[1].ID}
	assert.Contains(t, ids, first.ID)
	assert.Contains(t, ids, second.ID)

	one, err := s.CheckpointByID(second.ID)
	require.NoError(t, err)
	assert.Equal(t, "second", one.Label)
	assert.Equal(t, "cm-9", one.FileCommit)
	assert.Equal(t, sid, one.SessionID)
	assert.Equal(t, 3, one.HiddenSeq)
	assert.Equal(t, 3, one.Memories)

	_, err = s.CheckpointByID("nope")
	assert.Error(t, err)
}

// TestCheckpoint_PinnedSeqsSurvive: ADR-0015's boundary is two fields, and the
// second one is the reason the first is not enough. A checkpoint that dropped
// the pins would restore a window missing exactly the messages Plan judged most
// worth keeping.
func TestCheckpoint_PinnedSeqsSurvive(t *testing.T) {
	s, sid := checkpointFixture(t)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 4, []int{0, 2}))

	cp, err := s.CreateCheckpoint("pinned", sid, "")
	require.NoError(t, err)
	require.Equal(t, []int{0, 2}, cp.PinnedSeqs)

	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5, nil))
	_, err = s.RestoreCheckpoint(cp.ID, CheckpointSession)
	require.NoError(t, err)

	win, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	var seqs []int
	for _, m := range win {
		seqs = append(seqs, m.Seq)
	}
	assert.Equal(t, []int{0, 2, 4, 5}, seqs, "the pins came back with the tail")
}

// TestCheckpoint_RestoreIsAppendOnly holds ADR-0015's first constraint: the
// context event log only ever grows. A restore that rewrote or removed events
// would make the log unable to explain how the window got where it is.
func TestCheckpoint_RestoreIsAppendOnly(t *testing.T) {
	s, sid := checkpointFixture(t)
	cp, err := s.CreateCheckpoint("original", sid, "")
	require.NoError(t, err)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5, nil))

	before, err := s.ContextEvents(sid)
	require.NoError(t, err)
	_, err = s.RestoreCheckpoint(cp.ID, CheckpointSession)
	require.NoError(t, err)
	after, err := s.ContextEvents(sid)
	require.NoError(t, err)

	require.Greater(t, len(after), len(before), "a restore appends")
	assert.Equal(t, before, after[:len(before)],
		"every pre-existing event must be byte-identical afterwards")
}

// TestCheckpoint_FailedRestoreRollsBackTheSnapshotToo: the automatic snapshot
// and the restore are one transaction, so a failure must leave neither. A
// surviving snapshot with no restore is the state that reads like a completed
// rollback.
func TestCheckpoint_FailedRestoreRollsBackTheSnapshotToo(t *testing.T) {
	s, sid := checkpointFixture(t)
	cp, err := s.CreateCheckpoint("original", sid, "")
	require.NoError(t, err)

	// Corrupt the blob so restoreMemoriesTx fails after the snapshot is written.
	require.NoError(t, s.WriteTx(t.Context(), func(tx *sql.Tx) error {
		_, e := tx.Exec("UPDATE checkpoints SET memories = ? WHERE id = ?",
			[]byte("not gzip"), cp.ID)
		return e
	}))

	before, err := s.Checkpoints(50)
	require.NoError(t, err)
	_, err = s.RestoreCheckpoint(cp.ID, CheckpointMemory)
	require.Error(t, err)
	after, err := s.Checkpoints(50)
	require.NoError(t, err)
	assert.Len(t, after, len(before), "the pre-restore snapshot must have rolled back too")

	mems, err := s.RecallMemory(50)
	require.NoError(t, err)
	assert.Len(t, mems, 3, "and the memories are untouched")
}

// TestCheckpoint_ClosedDBErrorPaths walks the checkpoint API against a closed
// pool, the way error_paths_test.go does for the rest of the store.
//
// Every one of these functions has an error return that production reads and
// nothing else would exercise. The reason to cover them is not the number: a
// checkpoint call that swallows a database error and reports a zero-value
// Checkpoint has told the caller a snapshot exists.
func TestCheckpoint_ClosedDBErrorPaths(t *testing.T) {
	s := storeWithClosedDB(t)

	t.Run("CreateCheckpoint", func(t *testing.T) {
		_, err := s.CreateCheckpoint("l", "s", "c")
		assert.Error(t, err)
	})
	t.Run("Checkpoints", func(t *testing.T) {
		_, err := s.Checkpoints(10)
		assert.Error(t, err)
	})
	t.Run("CheckpointByID", func(t *testing.T) {
		_, err := s.CheckpointByID("x")
		assert.Error(t, err)
	})
	t.Run("PlanCheckpointRestore", func(t *testing.T) {
		for _, dim := range CheckpointDimensions() {
			_, err := s.PlanCheckpointRestore("x", dim)
			assert.Errorf(t, err, "dimension %s", dim)
		}
	})
	t.Run("RestoreCheckpoint", func(t *testing.T) {
		_, err := s.RestoreCheckpoint("x", CheckpointMemory)
		assert.Error(t, err)
	})
	t.Run("MemorySource", func(t *testing.T) {
		_, err := s.MemorySource("x")
		assert.Error(t, err)
	})
	t.Run("ClearMemories", func(t *testing.T) {
		_, err := s.ClearMemories(MemoryFilter{})
		assert.Error(t, err)
	})
	t.Run("WriteMemoryFromSession", func(t *testing.T) {
		// The boundary read fails, which must NOT fail the write — but the
		// write fails too here, on the same closed pool. What is asserted is
		// that it reports the failure rather than returning an empty id and nil.
		id, err := s.WriteMemoryFromSession("note", "x", MemoryFilter{SessionID: "s"})
		assert.Error(t, err)
		assert.Empty(t, id)
	})
}

// TestCheckpoint_PlanCountsTheWholeMemoryTable: the memory plan must not be
// scoped, because the restore is not. A plan that counted only one session's
// rows would understate what the restore is about to replace.
func TestCheckpoint_PlanCountsTheWholeMemoryTable(t *testing.T) {
	s, sid := checkpointFixture(t)
	_, err := s.WriteMemoryScoped("note", "another session's", MemoryFilter{SessionID: "elsewhere"})
	require.NoError(t, err)

	cp, err := s.CreateCheckpoint("all four", sid, "")
	require.NoError(t, err)
	assert.Equal(t, 4, cp.Memories)

	plan, err := s.PlanCheckpointRestore(cp.ID, CheckpointMemory)
	require.NoError(t, err)
	assert.Equal(t, 4, plan.Before)
	assert.Equal(t, 4, plan.After)
	assert.Equal(t, CheckpointMemory, plan.Dimension)
	assert.Equal(t, cp.ID, plan.Checkpoint.ID)
}

// TestCheckpointDimensions_IsTheCanonicalEnumeration.
//
// The list exists so a consumer in another package can be held to the same
// three words without hard-coding them — internal/api/http compares it to
// proto's copy. It is asserted here as well so the two ends of that comparison
// are each pinned to the constants, rather than only to each other: two lists
// that drift together would still agree.
func TestCheckpointDimensions_IsTheCanonicalEnumeration(t *testing.T) {
	assert.Equal(t,
		[]CheckpointDimension{CheckpointSession, CheckpointMemory, CheckpointFiles},
		CheckpointDimensions())
}

// TestCheckpoint_MemorySnapshotIsEmptyNotNull: a checkpoint taken with no
// memories at all must still produce a decodable blob, and restoring it must
// empty the table rather than fail. The NOT NULL column makes the first half a
// real constraint.
func TestCheckpoint_MemorySnapshotIsEmptyNotNull(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "empty.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	cp, err := s.CreateCheckpoint("nothing yet", "", "")
	require.NoError(t, err)
	assert.Zero(t, cp.Memories)

	_, err = s.WriteMemory("note", "written after")
	require.NoError(t, err)
	_, err = s.RestoreCheckpoint(cp.ID, CheckpointMemory)
	require.NoError(t, err)

	left, err := s.RecallMemory(10)
	require.NoError(t, err)
	assert.Empty(t, left, "restoring an empty snapshot empties the table")
}

// dropTable removes a table from an open store, so a call that reads or writes
// it fails while everything around it keeps working.
//
// A closed pool cannot reach these branches: WriteTx fails at BeginTx and never
// calls its body, so every error path INSIDE a transaction stays unexercised.
// Dropping one table is the smallest fault that gets past the transaction
// boundary, and it targets exactly the statement under test.
func dropTable(t *testing.T, s *Store, table string) {
	t.Helper()
	require.NoError(t, s.WriteTx(t.Context(), func(tx *sql.Tx) error {
		_, e := tx.Exec("DROP TABLE " + table) //nolint:gosec // fixed literals from the callers below
		return e
	}))
}

// TestCheckpoint_InTransactionFailures covers the error paths that only exist
// once a transaction has started.
//
// These are not decorative. Each one is a point where a checkpoint could
// otherwise report a snapshot that does not exist, or a restore that did not
// happen — the failure mode this whole file is arranged to make impossible.
func TestCheckpoint_InTransactionFailures(t *testing.T) {
	t.Run("boundary unreadable", func(t *testing.T) {
		s, sid := checkpointFixture(t)
		dropTable(t, s, "context_events")
		_, err := s.CreateCheckpoint("l", sid, "")
		assert.Error(t, err, "a checkpoint that cannot read the boundary must not claim one")
	})
	t.Run("memories unreadable", func(t *testing.T) {
		s, sid := checkpointFixture(t)
		dropTable(t, s, "memories")
		_, err := s.CreateCheckpoint("l", sid, "")
		assert.Error(t, err)
	})
	t.Run("checkpoints table gone", func(t *testing.T) {
		s, sid := checkpointFixture(t)
		dropTable(t, s, "checkpoints")
		_, err := s.CreateCheckpoint("l", sid, "")
		assert.Error(t, err)
	})
	t.Run("list unreadable", func(t *testing.T) {
		s, sid := checkpointFixture(t)
		_, err := s.CreateCheckpoint("l", sid, "")
		require.NoError(t, err)
		dropTable(t, s, "checkpoints")
		_, err = s.Checkpoints(10)
		assert.Error(t, err)
		_, err = s.CheckpointByID("whatever")
		assert.Error(t, err)
	})
	t.Run("plan cannot read the current boundary", func(t *testing.T) {
		s, sid := checkpointFixture(t)
		cp, err := s.CreateCheckpoint("l", sid, "")
		require.NoError(t, err)
		dropTable(t, s, "context_events")
		_, err = s.PlanCheckpointRestore(cp.ID, CheckpointSession)
		assert.Error(t, err)
	})
	t.Run("plan cannot project the window", func(t *testing.T) {
		s, sid := checkpointFixture(t)
		cp, err := s.CreateCheckpoint("l", sid, "")
		require.NoError(t, err)
		dropTable(t, s, "messages")
		_, err = s.PlanCheckpointRestore(cp.ID, CheckpointSession)
		assert.Error(t, err)
	})
	t.Run("plan cannot count memories", func(t *testing.T) {
		s, sid := checkpointFixture(t)
		cp, err := s.CreateCheckpoint("l", sid, "")
		require.NoError(t, err)
		dropTable(t, s, "memories")
		_, err = s.PlanCheckpointRestore(cp.ID, CheckpointMemory)
		assert.Error(t, err)
	})
	t.Run("restore cannot rewrite memories", func(t *testing.T) {
		s, sid := checkpointFixture(t)
		cp, err := s.CreateCheckpoint("l", sid, "")
		require.NoError(t, err)
		dropTable(t, s, "memories")
		_, err = s.RestoreCheckpoint(cp.ID, CheckpointMemory)
		assert.Error(t, err)
	})
	t.Run("session restore cannot append", func(t *testing.T) {
		s, sid := checkpointFixture(t)
		cp, err := s.CreateCheckpoint("l", sid, "")
		require.NoError(t, err)
		dropTable(t, s, "context_events")
		_, err = s.RestoreCheckpoint(cp.ID, CheckpointSession)
		assert.Error(t, err)
	})
}

// TestWriteMemoryFromSession_KeepsTheMemoryWhenProvenanceFails is the one
// documented degradation on the write path, asserted rather than asserted in
// prose: the memory is the asset and provenance is metadata about it, so an
// unreadable event log must cost the record of where the note came from and not
// the note.
func TestWriteMemoryFromSession_KeepsTheMemoryWhenProvenanceFails(t *testing.T) {
	s, sid := checkpointFixture(t)
	dropTable(t, s, "context_events")

	id, err := s.WriteMemoryFromSession("note", "still worth keeping", MemoryFilter{SessionID: sid})
	require.NoError(t, err, "the memory must survive a provenance failure")
	require.NotEmpty(t, id)

	got, err := s.RecallMemoryScoped(10, MemoryFilter{SessionID: sid})
	require.NoError(t, err)
	require.Len(t, got, 4)

	_, err = s.MemorySource(id)
	assert.ErrorIs(t, err, ErrNoMemorySource,
		"and it must say it has no source rather than inventing one")
}

// TestClearMemories_ReportsAFailedDelete: "cleared 0" and "the delete failed"
// are different answers, and a wipe that reported the first for the second
// would tell the user their memories are gone while they are still there.
func TestClearMemories_ReportsAFailedDelete(t *testing.T) {
	s, _ := checkpointFixture(t)
	dropTable(t, s, "memories")
	n, err := s.ClearMemories(MemoryFilter{})
	assert.Error(t, err)
	assert.Zero(t, n)
}
