package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWALUpgradeFromRollback verifies that opening a non-WAL database with the
// new Open/OpenWith upgrades it to WAL mode in place, with zero data loss.
//
// ledger: F1/WAL1#4 旧 DB 平滑升级
func TestWALUpgradeFromRollback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade_test.db")

	// Step 1: open with bare modernc (no _pragma, no journal_mode) to create
	// a non-WAL database with some data.
	db1, err := openBareModernc(path)
	require.NoError(t, err)
	sid, err := insertTestData(t, db1)
	require.NoError(t, err)
	require.NoError(t, db1.Close())

	// Step 2: reopen with Open (which applies WAL). Data must survive.
	st, err := Open(path)
	require.NoError(t, err)
	defer st.Close()
	assertPragma(t, st.DB, "journal_mode", "wal")

	// Check data integrity
	count, err := st.SessionMessageCount(sid)
	require.NoError(t, err)
	assert.Equal(t, 3, count, "messages survived WAL upgrade")

	// Step 3: reopen again -- idempotent
	require.NoError(t, st.Close())
	st2, err := Open(path)
	require.NoError(t, err)
	defer st2.Close()
	assertPragma(t, st2.DB, "journal_mode", "wal")
	count2, err := st2.SessionMessageCount(sid)
	require.NoError(t, err)
	assert.Equal(t, 3, count2, "idempotent upgrade, data still intact")
}

// openBareModernc opens a SQLite db at path WITHOUT any WAL pragmas.
func openBareModernc(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// insertTestData creates a session and 3 messages.
func insertTestData(t *testing.T, s *Store) (string, error) {
	t.Helper()
	sid, err := s.CreateSession("wal-upgrade-test")
	if err != nil {
		return "", err
	}
	for i := 0; i < 3; i++ {
		if err := s.AppendMessage(sid, i, "user", strings.Repeat("x", i*100+10)); err != nil {
			return "", err
		}
	}
	return sid, nil
}
