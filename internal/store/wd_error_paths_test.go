package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestColdStore_ErrorsAreReportedNotSwallowed covers the failure half of the
// W-D-04 read and write paths.
//
// Every one of these returns an error rather than a plausible-looking zero
// value, and the difference matters at exactly one place: a compressed session
// whose blob cannot be read must not be reported as an EMPTY session, or the
// model comes back from the restore with no conversation and nothing logged.
func TestColdStore_ErrorsAreReportedNotSwallowed(t *testing.T) {
	t.Run("compress probes cold_sessions first", func(t *testing.T) {
		s := openTestStore(t)
		sid := coldFixture(t, s, 2)
		dropTable(t, s, "cold_sessions")
		_, err := s.CompressSession(sid)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "probe cold session")
	})

	t.Run("empty session id", func(t *testing.T) {
		s := openTestStore(t)
		_, err := s.CompressSession("")
		require.Error(t, err)
	})

	t.Run("a session with no rows packs nothing", func(t *testing.T) {
		s := openTestStore(t)
		sid, err := s.CreateSession("empty")
		require.NoError(t, err)
		n, err := s.CompressSession(sid)
		require.NoError(t, err)
		assert.Zero(t, n)
	})

	t.Run("coldMaxSeq and SessionMessageCount report a broken table", func(t *testing.T) {
		s := openTestStore(t)
		sid := coldFixture(t, s, 2)
		_, err := s.CompressSession(sid)
		require.NoError(t, err)
		dropTable(t, s, "cold_sessions")

		_, _, err = s.coldMaxSeq(sid)
		require.Error(t, err)
		_, err = s.SessionMessageCount(sid)
		require.Error(t, err, "a count that cannot see cold storage must say so, not answer 0")
		_, err = s.maxMessageSeq(sid)
		require.Error(t, err)
	})

	t.Run("the sweep skips what is already packed and keeps going", func(t *testing.T) {
		s := openTestStore(t)
		good := coldFixture(t, s, 3)
		already := coldFixture(t, s, 3)
		packedFirst, err := s.CompressSession(already)
		require.NoError(t, err)
		require.Equal(t, 3, packedFirst)

		packed, err := s.CompressColdSessions(1<<40, 10)
		require.NoError(t, err)
		assert.Equal(t, 1, packed, "only the session that was not already packed counts")
		msgs, err := s.Messages(good)
		require.NoError(t, err)
		require.Len(t, msgs, 3, "the newly packed session is still readable through its blob")
	})

	t.Run("a cutoff at or below the epoch compresses nothing", func(t *testing.T) {
		s := openTestStore(t)
		coldFixture(t, s, 2)
		n, err := s.CompressColdSessions(0, 10)
		require.NoError(t, err)
		assert.Zero(t, n)
		n, err = s.CompressColdSessions(-1, 10)
		require.NoError(t, err)
		assert.Zero(t, n)
	})

	t.Run("IdleSessions reports a broken table", func(t *testing.T) {
		s := openTestStore(t)
		dropTable(t, s, "sessions")
		_, err := s.IdleSessions(1<<40, 10, false)
		require.Error(t, err)
	})
}

// TestThawColdSession_FailuresLeaveTheBlobAlone covers thawColdSessionTx's error
// returns, which are the ones that decide whether a write to a compressed
// session can destroy the archive.
//
// Every failure here has to abort the whole append: the thaw and the insert are
// one transaction precisely so a half-thawed session cannot exist.
func TestThawColdSession_FailuresLeaveTheBlobAlone(t *testing.T) {
	t.Run("probe failure aborts the append", func(t *testing.T) {
		s := openTestStore(t)
		sid := coldFixture(t, s, 2)
		dropTable(t, s, "cold_sessions")
		_, _, err := s.AppendMessages(sid, []Message{{Role: RoleUser, Content: "hi"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "probe cold session")
	})

	t.Run("an unreadable blob aborts the append", func(t *testing.T) {
		s := openTestStore(t)
		sid := coldFixture(t, s, 2)
		_, err := s.CompressSession(sid)
		require.NoError(t, err)
		require.NoError(t, s.WriteTx(t.Context(), func(tx *sql.Tx) error {
			_, e := tx.Exec("UPDATE cold_sessions SET blob = ? WHERE session_id = ?",
				[]byte(strings.Repeat("x", 32)), sid)
			return e
		}))

		_, _, err = s.AppendMessages(sid, []Message{{Role: RoleUser, Content: "hi"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "thaw cold session")

		// The append rolled back, so `messages` is still empty and the archive
		// row is still there to be repaired.
		var live, cold int
		require.NoError(t, s.DB.QueryRow(
			"SELECT COUNT(*) FROM messages WHERE session_id = ?", sid).Scan(&live))
		require.Zero(t, live)
		require.NoError(t, s.DB.QueryRow(
			"SELECT COUNT(*) FROM cold_sessions WHERE session_id = ?", sid).Scan(&cold))
		require.Equal(t, 1, cold)
	})

	t.Run("a colliding row aborts the append", func(t *testing.T) {
		s := openTestStore(t)
		sid := coldFixture(t, s, 2)
		archived, err := s.Messages(sid)
		require.NoError(t, err)
		_, err = s.CompressSession(sid)
		require.NoError(t, err)

		// Put one of the archived rows back by hand. The thaw's INSERT then hits
		// the partial unique index on (session_id, dedup_key) — a plain INSERT,
		// not ON CONFLICT DO NOTHING, because a collision here means the archive
		// and the live table disagree and silently keeping one is the wrong half.
		require.NoError(t, s.WriteTx(t.Context(), func(tx *sql.Tx) error {
			m := archived[0]
			_, e := tx.Exec(
				`INSERT INTO messages
				   (id, session_id, seq, role, content, tool_call_id, tool_name, tool_args, dedup_key, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				m.ID, m.SessionID, m.Seq, m.Role, m.Content,
				m.ToolCallID, m.ToolName, m.ToolArgs, m.DedupKey, m.CreatedAt)
			return e
		}))

		_, _, err = s.AppendMessages(sid, []Message{{Role: RoleUser, Content: "hi"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "thaw message")
	})

	t.Run("AppendMessage refuses the same way", func(t *testing.T) {
		s := openTestStore(t)
		sid := coldFixture(t, s, 2)
		dropTable(t, s, "cold_sessions")
		require.Error(t, s.AppendMessage(sid, 2, RoleUser, "hi"))
	})
}

// TestContextEvents_ErrorPaths covers the projection's failure returns.
//
// ProjectWindow is on every restore path, so an error it swallows is a window
// the model silently gets wrong.
func TestContextEvents_ErrorPaths(t *testing.T) {
	t.Run("a broken event table fails every reader", func(t *testing.T) {
		s := openTestStore(t)
		sid := coldFixture(t, s, 3)
		dropTable(t, s, "context_events")
		_, err := s.ContextEvents(sid)
		require.Error(t, err)
		_, err = s.HiddenSeq(sid)
		require.Error(t, err)
		_, err = s.ProjectWindow(sid)
		require.Error(t, err)
		require.Error(t, s.AppendContextEvent(sid, ContextEventCompact, 1, nil))
		_, err = s.SnapshotSessionForRevert(sid)
		require.Error(t, err)
		_, err = s.TruncateSessionForRevert(sid, 1, 0)
		require.Error(t, err)
	})

	t.Run("a broken message table fails the projection", func(t *testing.T) {
		s := openTestStore(t)
		sid := coldFixture(t, s, 3)
		require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 1, nil))
		dropTable(t, s, "messages_fts")
		dropTable(t, s, "messages")
		_, err := s.ProjectWindow(sid)
		require.Error(t, err)
	})

	t.Run("rejected events", func(t *testing.T) {
		s := openTestStore(t)
		require.Error(t, s.AppendContextEvent("", ContextEventCompact, 1, nil))
		require.Error(t, s.AppendContextEvent("x", "invented", 1, nil))
		require.Error(t, s.AppendContextEvent("x", ContextEventCompact, -1, nil))
	})

	t.Run("unreadable pins degrade to the kept tail", func(t *testing.T) {
		s := openTestStore(t)
		sid := coldFixture(t, s, 6)
		// A pinned_seqs value no decoder can read. The window must come back as
		// the suffix — smaller than intended, never wrong in content, and never
		// empty — rather than taking the session down over one column.
		require.NoError(t, s.WriteTx(t.Context(), func(tx *sql.Tx) error {
			return appendContextEventTx(tx, sid, ContextEventCompact, 4, "{not json}", 1)
		}))
		win, err := s.ProjectWindow(sid)
		require.NoError(t, err)
		require.Len(t, win, 2, "seq 4 and 5: the kept tail without the pins")
	})

	t.Run("a pin naming a row that is gone costs one message, not the window", func(t *testing.T) {
		s := openTestStore(t)
		sid := coldFixture(t, s, 6)
		require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 4, []int{1, 99}))
		win, err := s.ProjectWindow(sid)
		require.NoError(t, err)
		require.Len(t, win, 3, "the tail plus the pin that still exists")
	})

	t.Run("an undo on an empty stack is a no-op", func(t *testing.T) {
		s := openTestStore(t)
		sid := coldFixture(t, s, 4)
		require.NoError(t, s.AppendContextEvent(sid, ContextEventUndo, 0, nil))
		win, err := s.ProjectWindow(sid)
		require.NoError(t, err)
		require.Len(t, win, 4)
	})

	t.Run("encodePinnedSeqs drops negatives and duplicates", func(t *testing.T) {
		got, err := encodePinnedSeqs([]int{5, 5, -1, 2})
		require.NoError(t, err)
		assert.Equal(t, "[2,5]", got)
		got, err = encodePinnedSeqs([]int{-3})
		require.NoError(t, err)
		assert.Empty(t, got, "a list with nothing usable in it stores as empty, not as []")
	})
}

// TestMessageQueue_ErrorPaths covers the W-D-08 queue's failure returns. A
// queue that reports success while losing a row is worse than one that refuses.
func TestMessageQueue_ErrorPaths(t *testing.T) {
	s := openTestStore(t)
	sid, err := s.CreateSession("queued")
	require.NoError(t, err)
	_, err = s.EnqueueMessage(sid, "hello")
	require.NoError(t, err)

	dropTable(t, s, "queued_messages")
	_, err = s.PendingQueuedMessages(sid)
	require.Error(t, err)
	_, err = s.ConsumeQueuedMessages(sid)
	require.Error(t, err)
	_, err = s.EnqueueMessage(sid, "again")
	require.Error(t, err)
}

// TestMemoryQuota_ErrorPaths covers PruneUnusedMemories' failure returns and its
// two no-op shapes.
func TestMemoryQuota_ErrorPaths(t *testing.T) {
	s := openTestStore(t)
	for range 4 {
		_, err := s.WriteMemory("note", "keep me")
		require.NoError(t, err)
	}

	t.Run("a non-positive quota prunes nothing", func(t *testing.T) {
		n, err := s.PruneUnusedMemories(0)
		require.NoError(t, err)
		assert.Zero(t, n)
		n, err = s.PruneUnusedMemories(-5)
		require.NoError(t, err)
		assert.Zero(t, n)
	})

	t.Run("a quota above the row count prunes nothing", func(t *testing.T) {
		n, err := s.PruneUnusedMemories(100)
		require.NoError(t, err)
		assert.Zero(t, n)
	})

	t.Run("a broken table is an error", func(t *testing.T) {
		dropTable(t, s, "memories_fts")
		dropTable(t, s, "memories")
		_, err := s.PruneUnusedMemories(1)
		require.Error(t, err)
		_, err = s.ClearMemories(MemoryFilter{})
		require.Error(t, err)
		_, err = s.MemorySource("anything")
		require.Error(t, err)
	})
}

// TestCheckpoint_ErrorPaths covers the restore's failure returns, including the
// one that decides whether a corrupt snapshot silently empties the table.
func TestCheckpoint_ErrorPaths(t *testing.T) {
	t.Run("an unreadable snapshot aborts the restore", func(t *testing.T) {
		s, _ := checkpointFixture(t)
		cp, err := s.CreateCheckpoint("cp", "", "")
		require.NoError(t, err)
		require.NoError(t, s.WriteTx(t.Context(), func(tx *sql.Tx) error {
			_, e := tx.Exec("UPDATE checkpoints SET memories = ? WHERE id = ?",
				[]byte("not gzip"), cp.ID)
			return e
		}))
		_, err = s.RestoreCheckpoint(cp.ID, CheckpointMemory)
		require.Error(t, err)

		// The memories are untouched: the DELETE and the re-insert are one
		// transaction, so a decode failure cannot leave an emptied table.
		mems, err := s.RecallMemory(50)
		require.NoError(t, err)
		require.Len(t, mems, 3)
	})

	t.Run("a snapshot that names no session refuses the session dimension", func(t *testing.T) {
		s, _ := checkpointFixture(t)
		cp, err := s.CreateCheckpoint("no session", "", "")
		require.NoError(t, err)
		_, err = s.RestoreCheckpoint(cp.ID, CheckpointSession)
		require.ErrorIs(t, err, ErrNoCheckpointDimension)
	})

	t.Run("unknown dimensions name the catalog", func(t *testing.T) {
		s, _ := checkpointFixture(t)
		cp, err := s.CreateCheckpoint("cp", "", "")
		require.NoError(t, err)
		_, err = s.RestoreCheckpoint(cp.ID, CheckpointDimension("everything"))
		require.Error(t, err)
		for _, d := range CheckpointDimensions() {
			assert.Contains(t, err.Error(), string(d),
				"the error must name every dimension that does exist")
		}
		_, err = s.RestoreCheckpoint(cp.ID, CheckpointFiles)
		require.Error(t, err)
		_, err = s.RestoreCheckpoint("no-such-checkpoint", CheckpointMemory)
		require.Error(t, err)
	})

	t.Run("a broken checkpoints table is an error", func(t *testing.T) {
		s, sid := checkpointFixture(t)
		dropTable(t, s, "checkpoints")
		_, err := s.CreateCheckpoint("cp", sid, "")
		require.Error(t, err)
		_, err = s.Checkpoints(5)
		require.Error(t, err)
		_, err = s.CheckpointByID("x")
		require.Error(t, err)
	})
}

// TestSnippetForTerms covers the CJK excerpt builder, which had no test of its
// own and is what a Chinese history_search result actually shows the user.
func TestSnippetForTerms(t *testing.T) {
	const content = "部署脚本在周二跑，负责人是张伟，截止日期没有变"

	t.Run("centres on the first term present", func(t *testing.T) {
		got := snippetForTerms(content, []string{"缺席的词", "张伟"})
		assert.Contains(t, got, "张伟")
	})

	t.Run("falls through to the first term when none is present", func(t *testing.T) {
		// The match came from tool_args, not content: the function must stay
		// total rather than return an empty excerpt for a row that did match.
		got := snippetForTerms(content, []string{"不在里面"})
		assert.NotEmpty(t, got)
	})

	t.Run("no terms at all yields the head of the content", func(t *testing.T) {
		got := snippetForTerms(content, nil)
		assert.NotEmpty(t, got)
		assert.True(t, strings.HasPrefix(got, "部署脚本"), got)
	})

	t.Run("headRunes counts runes, not bytes", func(t *testing.T) {
		assert.Equal(t, "部署脚本", headRunes([]rune("部署脚本"), 4))
		assert.Equal(t, "部署脚本", headRunes([]rune("部署脚本"), 99))
		assert.Equal(t, "部署 … ", headRunes([]rune("部署脚本"), 2),
			"a byte-based cut would split a rune here")
	})
}

// TestMigrateMessageLog_IsIdempotentAndSelfHealing covers the C1 log migration's
// re-entry, which every open after the first takes.
func TestMigrateMessageLog_IsIdempotentAndSelfHealing(t *testing.T) {
	s := openTestStore(t)
	require.NoError(t, s.migrateMessageLog(), "a second run must be a no-op")

	// The FTS shadow table and its triggers are rebuilt when they are missing,
	// which is the shape an interrupted first migration leaves behind.
	for _, stmt := range []string{
		"DROP TRIGGER messages_ai", "DROP TRIGGER messages_ad",
		"DROP TRIGGER messages_au", "DROP TABLE messages_fts",
	} {
		require.NoError(t, s.WriteTx(context.Background(), func(tx *sql.Tx) error {
			_, e := tx.Exec(stmt)
			return e
		}))
	}
	require.NoError(t, s.migrateMessageLog())

	sid, err := s.CreateSession("after the repair")
	require.NoError(t, err)
	_, _, err = s.AppendMessages(sid, []Message{{Role: RoleUser, Content: "findable again"}})
	require.NoError(t, err)
	hits, err := s.SearchMessages(sid, "findable", 5)
	require.NoError(t, err)
	require.Len(t, hits, 1, "the rebuilt index must actually index new rows")
}

// TestBoundaryCompensation_PopsEveryDoomedBoundary covers the multi-boundary
// shapes of the two compensations, which the single-compaction fixtures never
// reach.
//
// Both are the mechanism ADR-0015 requires around any path that removes message
// rows: a boundary that outlives the rows it points at selects nothing, and a
// zero-row window is an agent with no memory of the conversation and no error to
// show for it.
func TestBoundaryCompensation_PopsEveryDoomedBoundary(t *testing.T) {
	s := openTestStore(t)
	sid := coldFixture(t, s, 8)
	// Two stacked compactions, both describing rows the truncation will delete.
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 4, nil))
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 6, nil))
	hidden, err := s.HiddenSeq(sid)
	require.NoError(t, err)
	require.Equal(t, 6, hidden)

	snap, err := s.TruncateSessionForRevert(sid, 3, 1)
	require.NoError(t, err)
	require.Len(t, snap.Messages, 8, "the snapshot is the whole log as it stood")
	require.Equal(t, 6, snap.HiddenSeq)

	hidden, err = s.HiddenSeq(sid)
	require.NoError(t, err)
	require.Zero(t, hidden, "both boundaries pointed into the doomed range; both popped")
	win, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	require.Len(t, win, 3, "the surviving rows, not an empty window")

	// The compensating restore pushes the captured boundary back, and doing it
	// twice appends nothing the second time.
	require.NoError(t, s.RestoreSessionAfterFailedRevert(snap))
	hidden, err = s.HiddenSeq(sid)
	require.NoError(t, err)
	require.Equal(t, 6, hidden)
	before, err := s.ContextEvents(sid)
	require.NoError(t, err)
	require.NoError(t, s.RestoreSessionAfterFailedRevert(snap))
	after, err := s.ContextEvents(sid)
	require.NoError(t, err)
	require.Len(t, after, len(before),
		"a compensation that changed nothing must append nothing, or the stack grows per retry")

	// And restoring a snapshot taken BEFORE any compaction pops all the way back
	// to the raw transcript rather than leaving the boundary standing.
	zero := snap
	zero.HiddenSeq, zero.PinnedSeqs = 0, nil
	require.NoError(t, s.RestoreSessionAfterFailedRevert(zero))
	hidden, err = s.HiddenSeq(sid)
	require.NoError(t, err)
	require.Zero(t, hidden)
	win, err = s.ProjectWindow(sid)
	require.NoError(t, err)
	require.Len(t, win, 8)
}

// TestForkSession_InheritsOnlyABoundaryItCopied covers copyBoundaryTx's two
// declining shapes.
//
// A fork branches the conversation, so it inherits the compacted window rather
// than the raw transcript — but only when the fork point is above the boundary.
// A fork taken from BELOW it copies none of the kept tail, and inheriting a
// boundary the copied rows cannot satisfy would project an empty window.
func TestForkSession_InheritsOnlyABoundaryItCopied(t *testing.T) {
	s := openTestStore(t)
	sid := coldFixture(t, s, 8)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5, []int{1, 7}))

	above, err := s.ForkSession(sid, 7)
	require.NoError(t, err)
	hidden, err := s.HiddenSeq(above)
	require.NoError(t, err)
	require.Equal(t, 5, hidden, "a fork past the boundary inherits it")
	win, err := s.ProjectWindow(above)
	require.NoError(t, err)
	require.Len(t, win, 4, "seqs 5,6,7 plus the pin at 1; the pin at 7 is already in the tail")

	below, err := s.ForkSession(sid, 2)
	require.NoError(t, err)
	hidden, err = s.HiddenSeq(below)
	require.NoError(t, err)
	require.Zero(t, hidden, "the fork point predates the compaction; inheriting nothing is correct")
	win, err = s.ProjectWindow(below)
	require.NoError(t, err)
	require.Len(t, win, 3)

	// A source with no boundary at all leaves the fork with none.
	plain := coldFixture(t, s, 3)
	forked, err := s.ForkSession(plain, -1)
	require.NoError(t, err)
	events, err := s.ContextEvents(forked)
	require.NoError(t, err)
	require.Empty(t, events)
}

// TestWriteMemoryFromSession_ProvenanceFailureStillWritesTheMemory pins the
// direction the doc chose: the memory is the asset, provenance is metadata about
// it, and losing the note because the event log could not be read would trade
// the thing being recorded for the record of where it came from.
func TestWriteMemoryFromSession_ProvenanceFailureStillWritesTheMemory(t *testing.T) {
	s := openTestStore(t)
	sid := coldFixture(t, s, 3)
	dropTable(t, s, "context_events")

	id, err := s.WriteMemoryFromSession("note", "still worth keeping",
		MemoryFilter{SessionID: sid})
	require.NoError(t, err, "an unreadable boundary must not lose the memory")

	m, err := s.MemoryByID(id)
	require.NoError(t, err)
	require.Equal(t, "still worth keeping", m.Content)

	_, err = s.MemorySource(id)
	require.ErrorIs(t, err, ErrNoMemorySource,
		"no provenance was recorded, and that is what the caller must be told")
}

// TestMemoryReaders_ReportABrokenTable: CountMemories and MemoryByID answer with
// an error rather than 0 / the zero Memory, which are both indistinguishable
// from a legitimate empty result.
func TestMemoryReaders_ReportABrokenTable(t *testing.T) {
	s := openTestStore(t)
	_, err := s.WriteMemory("note", "one")
	require.NoError(t, err)

	n, err := s.CountMemories(MemoryFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	dropTable(t, s, "memories_fts")
	dropTable(t, s, "memories")
	_, err = s.CountMemories(MemoryFilter{})
	require.Error(t, err)
	_, err = s.MemoryByID("anything")
	require.Error(t, err)
}

// TestMessageQueue_DegenerateShapes covers the queue's two silent-success paths,
// which have to stay distinguishable from a delivery.
func TestMessageQueue_DegenerateShapes(t *testing.T) {
	s := openTestStore(t)
	_, err := s.ConsumeQueuedMessages("")
	require.Error(t, err)
	// PendingQueuedMessages takes no session guard on purpose: it is a read, and
	// an empty id simply matches nothing.
	pendingNone, err := s.PendingQueuedMessages("")
	require.NoError(t, err)
	require.Empty(t, pendingNone)
	_, err = s.EnqueueMessage("", "x")
	require.Error(t, err)

	sid, err := s.CreateSession("quiet")
	require.NoError(t, err)
	got, err := s.ConsumeQueuedMessages(sid)
	require.NoError(t, err)
	require.Empty(t, got, "an empty queue is not an error")
	pending, err := s.PendingQueuedMessages(sid)
	require.NoError(t, err)
	require.Empty(t, pending)
}

// TestStore_RejectsDegenerateArguments covers the validators that stand between
// a caller's mistake and a query that would do something plausible-looking with
// it — an empty lease name that every caller would then share, a limit of zero
// that means "everything", a negative revert boundary.
func TestStore_RejectsDegenerateArguments(t *testing.T) {
	s := openTestStore(t)

	t.Run("leases", func(t *testing.T) {
		_, err := s.ClaimLease("", time.Minute)
		require.Error(t, err)
		_, err = s.ClaimLease("named", 0)
		require.Error(t, err, "a zero ttl would expire the instant it was taken")
		_, err = s.ClaimLease("named", -time.Minute)
		require.Error(t, err)
		require.Error(t, s.RetireLease(""))

		_, ok, err := s.LeaseHeldUntil("never-claimed")
		require.NoError(t, err)
		require.False(t, ok)

		// A value that is not a number reads as "expired long ago" rather than as
		// an error: the row is ours to overwrite, and wedging the work forever
		// over a corrupt scalar buys nothing.
		require.NoError(t, s.KVSet("lease:garbled", "not a number"))
		until, ok, err := s.LeaseHeldUntil("garbled")
		require.NoError(t, err)
		require.True(t, ok)
		require.Zero(t, until)
		won, err := s.ClaimLease("garbled", time.Minute)
		require.NoError(t, err)
		require.True(t, won)
	})

	t.Run("revert boundaries", func(t *testing.T) {
		_, err := s.TruncateSessionForRevert("", 0, 0)
		require.Error(t, err)
		_, err = s.TruncateSessionForRevert("x", -1, 0)
		require.Error(t, err)
		_, err = s.TruncateSessionForRevert("x", 0, -1)
		require.Error(t, err)
		_, err = s.SnapshotSessionForRevert("")
		require.Error(t, err)
		require.Error(t, s.RestoreSessionAfterFailedRevert(SessionRevertSnapshot{}))
	})

	t.Run("limits fall back rather than returning nothing", func(t *testing.T) {
		sid := coldFixture(t, s, 3)
		idle, err := s.IdleSessions(1<<40, 0, false)
		require.NoError(t, err)
		require.Contains(t, idle, sid, "a non-positive limit takes the default, not zero rows")

		page, err := s.MessagesPage(MessageRange{SessionID: sid, Limit: -1})
		require.NoError(t, err)
		require.Len(t, page, 3)
		_, err = s.MessagesPage(MessageRange{})
		require.Error(t, err)
	})

	t.Run("appends", func(t *testing.T) {
		_, _, err := s.AppendMessages("", []Message{{Role: RoleUser, Content: "x"}})
		require.Error(t, err)
		_, err = s.SearchMessages("", "   ", 5)
		require.Error(t, err, "an empty query would match the whole log")
	})
}

// TestCJKSearch_FindsWhatThePorterTokenizerCannot covers the LIKE fallback both
// readers share. The FTS tokenizer does not segment Chinese, so an entire
// sentence collapses into one token and every term inside it returns zero hits —
// which would make history_search and memory search dead in this repo's own
// working language.
func TestCJKSearch_FindsWhatThePorterTokenizerCannot(t *testing.T) {
	s := openTestStore(t)
	sid, err := s.CreateSession("中文")
	require.NoError(t, err)
	_, _, err = s.AppendMessages(sid, []Message{{
		Role:     RoleUser,
		Content:  "截止日期是周二，负责人是张伟，项目要在那之前完成",
		ToolName: "shell_run",
	}})
	require.NoError(t, err)
	_, err = s.WriteMemoryScoped("note", "张伟负责这个项目", MemoryFilter{SessionID: sid})
	require.NoError(t, err)

	hits, err := s.SearchMessages(sid, "张伟", 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Contains(t, hits[0].Snippet, "张伟", "the hand-built excerpt must centre on the term")

	// Cross-session (empty id) has to work too, or a Chinese query would be
	// silently single-session while its English sibling searched everything.
	hits, err = s.SearchMessages("", "截止日期", 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)

	// Quoted / OR'd syntax arrives from history_search's own error message.
	hits, err = s.SearchMessages(sid, `"周二" OR "缺席的词"`, 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)

	mems, err := s.SearchMemoryScoped("张伟", 5, MemoryFilter{})
	require.NoError(t, err)
	require.Len(t, mems, 1)
}

// TestMigrateMessageLog_ReportsABrokenTable: the C1 log migration runs on every
// open, so a failure it swallowed would leave a database whose messages table is
// missing a column the whole read path selects.
func TestMigrateMessageLog_ReportsABrokenTable(t *testing.T) {
	s := openTestStore(t)
	dropTable(t, s, "messages_fts")
	dropTable(t, s, "messages")

	err := s.migrateMessageLog()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "messages")

	// columns() on a table that is not there answers with an empty list rather
	// than an error, which is what makes addColumnIfMissing attempt the ALTER
	// and produce the message above.
	cols, err := s.columns("messages")
	require.NoError(t, err)
	assert.Empty(t, cols)
}

// TestForkSession_ReportsABrokenEventLog: a fork that could not read the
// source's boundary must fail rather than produce a session whose window is
// silently the raw transcript.
func TestForkSession_ReportsABrokenEventLog(t *testing.T) {
	s := openTestStore(t)
	sid := coldFixture(t, s, 4)
	dropTable(t, s, "context_events")
	_, err := s.ForkSession(sid, -1)
	require.Error(t, err)
}

// TestProjectWindow_EmptySessionHasNothingToBeStaleAbout covers the backstop's
// short circuit. A boundary over a session with no rows at all is not a stale
// boundary — there is nothing it could be describing — and escalating it to a
// full-transcript projection would log a warning on every read of an empty
// session.
func TestProjectWindow_EmptySessionHasNothingToBeStaleAbout(t *testing.T) {
	s := openTestStore(t)
	sid, err := s.CreateSession("no messages at all")
	require.NoError(t, err)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5, nil))

	win, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	require.Empty(t, win)
}
