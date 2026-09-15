package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenWith_NotADBMidSwapIsNotProofOfCorruption covers the one window the
// set-WAL retry exists for, and it is the only test that can tell the retry
// apart from its absence.
//
// PRAGMA journal_mode=WAL is the first statement that reads the file at path,
// so it is where a concurrent repair becomes visible: a sibling healer renames
// the corrupt database away and creates a fresh one, and an opener that reads
// the path inside that gap is told SQLITE_NOTADB. That is a claim about the FILE
// UNDER US, exactly like the SQLITE_BUSY the same race is documented producing
// (see healUnderLock), and NOT evidence about the data — which matters because
// NOTADB is also the primary trigger for quarantine (isCorruptDB).
//
// Without the retry the first NOTADB ends the open, so the corruption path runs:
// the file is renamed to yanshi.db.corrupt-* and an EMPTY database takes its
// place. This test therefore asserts on the two things that outcome destroys —
// the surviving session, and the absence of a quarantine — rather than on
// timing, so a passing run cannot mean "the retry happened to be slow enough".
//
// The negative half is covered behaviourally elsewhere and deliberately not
// repeated here as a predicate assertion:
// internal/store::TestOpenWith_RecoversFromCorruptDatabase fails if NOTADB is
// ever promoted into isTransientOpenErr, because healUnderLock classifies its
// recheck three ways and would then route a genuinely malformed database down
// the "still contended, do not quarantine" branch.
func TestOpenWith_NotADBMidSwapIsNotProofOfCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.db")

	// A real database whose contents must survive the swap.
	st, err := Open(path)
	require.NoError(t, err)
	wanted, err := st.CreateSession("data that must survive")
	require.NoError(t, err)
	require.NoError(t, st.Close())
	good, err := os.ReadFile(path)
	require.NoError(t, err)

	// Put the path into the state the rename→create gap leaves it in: bytes that
	// are not a database at all.
	require.NoError(t, os.WriteFile(path, []byte("not a database: read mid-swap"), 0o600))

	// Then complete the swap, the way the sibling would, well inside the retry
	// budget. The write is retried because on Windows the opener may hold the
	// file without sharing write access while it is being probed.
	restored := make(chan struct{})
	go func() {
		defer close(restored)
		time.Sleep(50 * time.Millisecond)
		deadline := time.Now().Add(5 * time.Second)
		for {
			if werr := os.WriteFile(path, good, 0o600); werr == nil {
				return
			} else if time.Now().After(deadline) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	got, err := OpenWith(path, healingOptions())
	<-restored
	require.NoError(t, err, "a NOTADB read mid-swap must not be reported as corruption")
	require.NotNil(t, got)
	defer got.Close()

	list, err := got.ListSessions(0)
	require.NoError(t, err)
	require.Len(t, list, 1,
		"the store must be the database that was swapped back, not a freshly built empty one")
	assert.Equal(t, wanted, list[0].ID, "the session written before the swap must still be there")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".corrupt-",
			"nothing was corrupt: a NOTADB observed mid-swap must not quarantine a healthy database")
	}
}
