package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// openFileStore returns a file-backed store. The concurrency test needs one:
// ":memory:" forces MaxOpenConns=1, so a reader would queue behind the writer's
// connection and the test would prove serialisation rather than concurrency.
func openFileStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "yanshi.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	return st
}

// coldFixture creates a session with n messages of realistic bulk and returns
// its id. The bodies repeat a vocabulary on purpose: that is what a transcript
// looks like and what makes per-session compression worth doing at all.
func coldFixture(t *testing.T, s *Store, n int) string {
	t.Helper()
	sid, err := s.CreateSession("cold")
	require.NoError(t, err)
	msgs := make([]Message, 0, n)
	for i := range n {
		msgs = append(msgs, Message{
			Role: RoleToolResult,
			Content: fmt.Sprintf("step %d: ran the build, read the diff, applied "+
				"the fix, ran the build again, read the diff again\n%s", i,
				strings.Repeat("ok  \tgithub.com/x6nux/yanshi/internal/store\t0.312s\n", 8)),
			ToolName: "shell_run",
			ToolArgs: `{"command":"go test ./internal/store"}`,
		})
	}
	_, _, err = s.AppendMessages(sid, msgs)
	require.NoError(t, err)
	return sid
}

// TestColdStore_ShrinksColdSessions is the storage half of W-D-04: a compressed
// session must occupy substantially less than the rows it replaced.
//
// The assertion compares the blob against the RAW BYTES OF THE ROWS IT
// REPLACES, not against a page count: page counts move with SQLite's allocator
// and would make this test report on the storage engine instead of on the
// feature. It counts every stored column, because the per-row overhead is the
// interesting part — id and dedup_key are 24 and 64 hex characters of pure
// entropy each, so they compress to nothing and set the real floor on this
// ratio. A fixture of short messages is dominated by them.
func TestColdStore_ShrinksColdSessions(t *testing.T) {
	s := openTestStore(t)
	sid := coldFixture(t, s, 200)

	var raw int
	require.NoError(t, s.DB.QueryRow(
		`SELECT SUM(LENGTH(id) + LENGTH(session_id) + LENGTH(role) + LENGTH(content)
		            + LENGTH(tool_call_id) + LENGTH(tool_name) + LENGTH(tool_args)
		            + LENGTH(dedup_key))
		   FROM messages WHERE session_id = ?`,
		sid).Scan(&raw))

	n, err := s.CompressSession(sid)
	require.NoError(t, err)
	require.Equal(t, 200, n)

	var blobLen int
	require.NoError(t, s.DB.QueryRow(
		"SELECT LENGTH(blob) FROM cold_sessions WHERE session_id = ?", sid).Scan(&blobLen))
	require.Less(t, blobLen, raw/4,
		"gzipped transcript (%d bytes) should be well under a quarter of the raw text (%d)",
		blobLen, raw)

	var rows int
	require.NoError(t, s.DB.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE session_id = ?", sid).Scan(&rows))
	require.Zero(t, rows, "compressed session must leave no message rows behind")
}

// TestColdStore_ReadsBackIdentical: reading a compressed session returns the
// same slice, message for message and field for field, that the uncompressed
// one did. Compares the WHOLE slice — a length check would pass on a blob that
// silently reordered or blanked every row.
func TestColdStore_ReadsBackIdentical(t *testing.T) {
	s := openTestStore(t)
	sid := coldFixture(t, s, 12)

	before, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, before, 12)

	_, err = s.CompressSession(sid)
	require.NoError(t, err)

	after, err := s.Messages(sid)
	require.NoError(t, err)
	require.Equal(t, before, after)

	// ProjectWindow is the path the restore actually uses; an uncompacted
	// session must project identically through it too (ADR-0015 constraint 2).
	projected, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	require.Equal(t, before, projected)
}

// TestColdStore_ProjectWindowStillHonoursTheBoundary is RULING P-1.
//
// A session that was compacted before it went cold has a context boundary. The
// live path expresses that boundary as a WHERE clause on `messages`; the cold
// path has no rows to filter, so a fallback that returned the blob wholesale
// would hand the model back every original the summary replaced — ADR-0015's
// founding bug, re-entering through the storage door.
//
// Deleting the filterWindow call in messagesInWindow makes this red.
func TestColdStore_ProjectWindowStillHonoursTheBoundary(t *testing.T) {
	s := openTestStore(t)
	sid := coldFixture(t, s, 10)

	// Keep the tail from seq 7, plus a pinned user original down at seq 1.
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 7, []int{1}))

	want, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	require.Len(t, want, 4, "tail 7..9 plus the pin at 1")

	_, err = s.CompressSession(sid)
	require.NoError(t, err)

	got, err := s.ProjectWindow(sid)
	require.NoError(t, err)
	require.Equal(t, want, got,
		"a compressed session must project the same window, not the whole transcript")

	// And the full transcript is still reachable, which is the whole reason
	// compression is allowed to remove the rows in the first place.
	all, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, all, 10)
}

// TestColdStore_KeepsContextEvents is RULING P-2. Compression must not clear the
// event log: those rows ARE the compaction markers, and dropping them makes the
// next reconnect rebuild the window from the full transcript and buy a second
// summary of text it already summarised.
//
// ADR-0015 constraint 1 grants exactly one deletion exemption (DeleteSession).
// Compression is not a deletion.
func TestColdStore_KeepsContextEvents(t *testing.T) {
	s := openTestStore(t)
	sid := coldFixture(t, s, 8)
	require.NoError(t, s.AppendContextEvent(sid, ContextEventCompact, 5, nil))

	_, err := s.CompressSession(sid)
	require.NoError(t, err)

	events, err := s.ContextEvents(sid)
	require.NoError(t, err)
	require.Len(t, events, 1)
	hidden, err := s.HiddenSeq(sid)
	require.NoError(t, err)
	require.Equal(t, 5, hidden)
}

// TestColdStore_ZeroRetentionDeletesNothing: the off switch. With no retention
// configured the sweep must leave every row exactly where it was.
//
// Both directions are asserted. A test that only checked "0 does nothing" would
// also pass on a CompressColdSessions that never compressed anything at all.
func TestColdStore_ZeroRetentionDeletesNothing(t *testing.T) {
	s := openTestStore(t)
	sid := coldFixture(t, s, 6)
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()
	require.NoError(t, s.WriteTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE sessions SET updated_at = ? WHERE id = ?", old, sid)
		return err
	}))

	packed, err := s.CompressColdSessions(0, 10)
	require.NoError(t, err)
	require.Zero(t, packed)
	var rows int
	require.NoError(t, s.DB.QueryRow(
		"SELECT COUNT(*) FROM messages WHERE session_id = ?", sid).Scan(&rows))
	require.Equal(t, 6, rows, "zero retention must not move a single row")

	// The same sweep with a real cutoff does pack it, so the assertion above
	// is about the switch and not about a sweep that never works.
	packed, err = s.CompressColdSessions(time.Now().Unix(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, packed)
}

// TestColdStore_ConcurrentReadDuringCompressSucceeds: a reader attached to a
// session while the sweep compresses it must never see an error, and never an
// empty conversation. The window between DELETE and the reader's next query is
// exactly where a non-transactional design would hand back nothing.
func TestColdStore_ConcurrentReadDuringCompressSucceeds(t *testing.T) {
	s := openFileStore(t)
	sid := coldFixture(t, s, 40)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 1)
	short := make(chan int, 1)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			msgs, err := s.ProjectWindow(sid)
			if err != nil {
				select {
				case errs <- err:
				default:
				}
				return
			}
			if len(msgs) != 40 {
				select {
				case short <- len(msgs):
				default:
				}
				return
			}
		}
	}()

	_, err := s.CompressSession(sid)
	require.NoError(t, err)
	close(stop)
	wg.Wait()

	select {
	case err := <-errs:
		t.Fatalf("read during compression returned an error: %v", err)
	default:
	}
	select {
	case n := <-short:
		t.Fatalf("read during compression saw %d of 40 messages", n)
	default:
	}
}

// TestColdStore_CompressIsIdempotentAndSkipsEmpty: a second pass over an
// already-cold session must not re-pack it (which would read zero rows and
// store an empty blob over the real one), and an empty session must not create
// a row at all.
func TestColdStore_CompressIsIdempotentAndSkipsEmpty(t *testing.T) {
	s := openTestStore(t)
	sid := coldFixture(t, s, 5)
	n, err := s.CompressSession(sid)
	require.NoError(t, err)
	require.Equal(t, 5, n)

	n, err = s.CompressSession(sid)
	require.NoError(t, err)
	require.Zero(t, n)
	msgs, err := s.Messages(sid)
	require.NoError(t, err)
	require.Len(t, msgs, 5, "a second pass must not overwrite the blob with nothing")

	empty, err := s.CreateSession("empty")
	require.NoError(t, err)
	n, err = s.CompressSession(empty)
	require.NoError(t, err)
	require.Zero(t, n)
	var rows int
	require.NoError(t, s.DB.QueryRow(
		"SELECT COUNT(*) FROM cold_sessions WHERE session_id = ?", empty).Scan(&rows))
	require.Zero(t, rows)
}

// TestColdStore_CandidatesSkipWhatIsAlreadyPacked keeps the sweep from handing
// the worker the same ids forever once every old session is cold.
func TestColdStore_CandidatesSkipWhatIsAlreadyPacked(t *testing.T) {
	s := openTestStore(t)
	sid := coldFixture(t, s, 4)
	cutoff := time.Now().Unix() + 1

	ids, err := s.ColdCandidates(cutoff, 10)
	require.NoError(t, err)
	require.Contains(t, ids, sid)

	_, err = s.CompressSession(sid)
	require.NoError(t, err)

	ids, err = s.ColdCandidates(cutoff, 10)
	require.NoError(t, err)
	require.NotContains(t, ids, sid)
}

// TestColdStore_UnreadableBlobIsAnErrorNotAnEmptyWindow: a blob that cannot be
// decompressed must fail loudly. Returning "no messages" would be
// indistinguishable from an empty session, and the model would come back from
// the restore with no conversation and nothing logged.
func TestColdStore_UnreadableBlobIsAnErrorNotAnEmptyWindow(t *testing.T) {
	s := openTestStore(t)
	sid := coldFixture(t, s, 3)
	_, err := s.CompressSession(sid)
	require.NoError(t, err)

	require.NoError(t, s.WriteTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.Exec("UPDATE cold_sessions SET blob = ? WHERE session_id = ?",
			[]byte(strings.Repeat("x", 64)), sid)
		return err
	}))

	_, err = s.Messages(sid)
	require.Error(t, err)
}
