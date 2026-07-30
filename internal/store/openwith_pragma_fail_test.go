package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenWith_PragmaFailsOnDirectory covers the applyConnectionPragmas error
// path inside OpenWith. Opening a directory as a SQLite database causes the
// PRAGMA journal_mode=WAL to fail ("unable to open database file").
func TestOpenWith_PragmaFailsOnDirectory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "testdb")
	if err := os.Mkdir(dbPath, 0755); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dbPath)
	if err == nil {
		t.Fatal("expected error opening a directory as SQLite database")
	}
}
