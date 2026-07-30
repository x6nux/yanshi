package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/store"
)

// TestCov_OpenLogFile_MkdirAllError covers the MkdirAll-error branch (a file
// blocks the log parent directory).
func TestCov_OpenLogFile_MkdirAllError(t *testing.T) {
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "blocked"), []byte("x"), 0o644))
	_, err := openLogFile(filepath.Join(tmp, "blocked", "app.log"))
	assert.Error(t, err)
}

// TestCov_OpenLogFile_OpenFileError covers the OpenFile-error branch (the path
// is a directory, which cannot be opened for append).
func TestCov_OpenLogFile_OpenFileError(t *testing.T) {
	_, err := openLogFile(t.TempDir())
	assert.Error(t, err)
}

// TestCov_BuildAutomation_NilStore covers the db==nil validation branch.
func TestCov_BuildAutomation_NilStore(t *testing.T) {
	_, err := BuildAutomation(context.Background(), config.Config{}, nil, nil)
	assert.Error(t, err)
}

// TestCov_BuildAutomation_NilAdapter covers the adapter==nil validation branch
// (db is a real in-memory store, so the db-nil check passes).
func TestCov_BuildAutomation_NilAdapter(t *testing.T) {
	db, err := store.Open(":memory:")
	require.NoError(t, err)
	defer db.Close()
	_, err = BuildAutomation(context.Background(), config.Config{}, db, nil)
	assert.Error(t, err)
}
