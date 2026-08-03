package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/testutil"
)

// TestMigrate_AddColumnFailurePaths exercises the return-err branches inside
// migrate() for each addColumnIfMissing call. We create a Store directly
// (bypassing Open) on a database file that is already fully initialized
// (WAL mode, all tables) but is read-only, so the schema Exec succeeds but
// ALTER TABLE ADD COLUMN fails.
//
// Each sub-test:
//  1. Fully initializes the database via Open (WAL + tables + columns).
//  2. Selectively drops one migration-added column.
//  3. Closes and reopens the Store directly (without Open).
//  4. Makes the file read-only.
//  5. Calls migrate() — schema Exec succeeds (all tables exist, IF NOT
//     EXISTS), then addColumnIfMissing for the dropped column attempts
//     ALTER TABLE ADD COLUMN which fails on the read-only file.
func TestMigrate_AddColumnFailurePaths(t *testing.T) {
	// Map from addColumnIfMissing argument to the line number and
	// column name it targets. These are the columns NOT in the base
	// schema — they were added via addColumnIfMissing as migrations.
	//
	// The order follows migrate() in store.go:
	migrationCols := []struct {
		table string
		col   string
		line  string // approximate, for debugging
	}{
		{"tasks", "worktree_id", "L162"},
		{"vcs_worktrees", "tip", "L165"},
		{"vcs_tree", "op", "L168"},
		{"sessions", "model", "L174"},
		{"sessions", "thinking", "L177"},
		{"sessions", "tokens_in", "L180"},
		{"sessions", "tokens_out", "L183"},
		{"sessions", "turns", "L186"},
		{"sessions", "cached_tokens", "L192"},
		{"sessions", "reasoning_tokens", "L195"},
		{"sessions", "archived", "L201"},
		{"sessions", "billed_input_tokens", "L211"},
		{"sessions", "billed_cached_tokens", "L214"},
		{"sessions", "billed_output_tokens", "L217"},
		{"sessions", "cost_usd", "L220"},
		{"sessions", "cost_known", "L223"},
	}

	for _, mc := range migrationCols {
		t.Run(mc.col, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "yanshi.db")

			// Step 1: fully initialize via Open.
			st, err := Open(path)
			require.NoError(t, err)

			// Step 2: drop the target column.
			_, err = st.DB.Exec("ALTER TABLE " + mc.table + " DROP COLUMN " + mc.col)
			if err != nil {
				st.Close()
				t.Skipf("DROP COLUMN %s not supported: %v", mc.col, err)
			}
			require.NoError(t, st.Close())

			// Step 3: reopen as raw Store (bypass Open) so we can call
			// migrate() without applyConnectionPragmas failing.
			rawDB, err := sql.Open("sqlite", path+buildDSN("", 5000, 1000))
			require.NoError(t, err)
			s := &Store{DB: rawDB}

			// Step 4: make file read-only so ALTER TABLE ADD COLUMN fails.
			testutil.SkipIfRoot(t) // root bypasses the 0444 guard below
			require.NoError(t, os.Chmod(path, 0444))
			t.Cleanup(func() { os.Chmod(path, 0644) })

			// Step 5: call migrate — schema Exec succeeds, but
			// addColumnIfMissing for the dropped column fails.
			err = s.migrate()
			assert.Error(t, err)

			// Cleanup.
			s.DB.Close()
		})
	}
}
