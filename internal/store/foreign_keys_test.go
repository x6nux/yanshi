package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestForeignKeysAreEnforcedOnEveryPooledConnection pins that the DSN pragma
// reaches ALL connections, not just the first one.
//
// foreign_keys is per-connection in SQLite and defaults to OFF. Setting it with
// a single Exec on *sql.DB arms whichever connection the pool happened to hand
// out, leaving the rest unenforced — which is worse than leaving it off, since
// the same orphan write would then succeed or fail depending on scheduling. The
// test holds every connection in the pool open SIMULTANEOUSLY so each one is
// distinct, and asks each in turn.
func TestForeignKeysAreEnforcedOnEveryPooledConnection(t *testing.T) {
	const pool = 4
	s, err := OpenWith(filepath.Join(t.TempDir(), "fk.db"), OpenOptions{MaxOpenConns: pool})
	require.NoError(t, err)
	defer s.Close()

	ctx := context.Background()
	conns := make([]*sql.Conn, 0, pool)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < pool; i++ {
		c, err := s.DB.Conn(ctx)
		require.NoError(t, err)
		conns = append(conns, c)
	}
	for i, c := range conns {
		var on int
		require.NoError(t, c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&on))
		require.Equalf(t, 1, on, "connection %d serves writes with referential integrity off", i)
	}
}

// TestForeignKeysAreEnforcedInMemory covers the one store shape the DSN cannot
// reach. buildDSN returns ":memory:" verbatim, so an in-memory database gets
// its pragma from applyConnectionPragmas instead. Most of this repo's tests
// open :memory:, so without this the cheapest tests would be the only ones
// running unenforced.
func TestForeignKeysAreEnforcedInMemory(t *testing.T) {
	s, err := Open(":memory:")
	require.NoError(t, err)
	defer s.Close()

	var on int
	require.NoError(t, s.DB.QueryRow("PRAGMA foreign_keys").Scan(&on))
	require.Equal(t, 1, on)

	// And it bites: messages.session_id names a session that does not exist.
	require.Error(t, s.AppendMessage("no-such-session", 0, "user", "x"))
}
