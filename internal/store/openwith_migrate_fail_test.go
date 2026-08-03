package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/x6nux/yanshi/internal/testutil"
)

// TestOpenWith_MigrateFailsOnReadOnlyDB exercises the migrate() error path
// inside OpenWith. We create a fully initialized database, drop one table so
// the CREATE TABLE IF NOT EXISTS must write, then set the file read-only so
// the write fails, and finally reopen with Open to trigger the error.
func TestOpenWith_MigrateFailsOnReadOnlyDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.db")

	// Step 1: fully initialize the database.
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// Step 2: drop a table so a subsequent Open's schema Exec must CREATE it.
	if _, err := st.DB.Exec("DROP TABLE IF EXISTS vcs_repos"); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if _, err := st.DB.Exec("DROP TABLE IF EXISTS sessions"); err != nil {
		st.Close()
		t.Fatal(err)
	}

	// Step 3: close cleanly.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Step 4: make the file read-only so schema creation fails.
	testutil.SkipIfRoot(t) // root bypasses the 0444 guard below
	if err := os.Chmod(path, 0444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0644) })

	// Step 5: reopen — PRAGMA journal_mode=WAL should succeed (connection-level
	// setting is fine on read-only), but CREATE TABLE IF NOT EXISTS for the
	// dropped table should fail (can't write to read-only file).
	_, err = Open(path)
	if err == nil {
		t.Fatal("expected error opening a read-only database with missing tables")
	}
	t.Logf("error: %v", err)
}
