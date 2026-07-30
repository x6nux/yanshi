package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenWith_DefaultsApplied covers the three default-setting branches in
// OpenWith (maxOpen <= 0 → Default, busyMs <= 0 → Default, ckpt == 0 →
// Default). These are not triggered by Open (which passes DefaultOptions
// with non-zero values) so we call OpenWith directly with zero-valued options.
func TestOpenWith_DefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Zero-valued OpenOptions ensures all three defaults are applied.
	st, err := OpenWith(path, OpenOptions{})
	require.NoError(t, err)
	defer st.Close()

	// After applying defaults, maxOpen should be 4 for a file-based DB
	// (only :memory: forces it to 1).
	assert.Equal(t, 4, st.DB.Stats().MaxOpenConnections,
		"zero MaxOpenConns should trigger DefaultOptions.MaxOpenConns=4")
}

// TestOpenWith_ZeroDefaultsInMemory verifies the same default-setting branches
// are exercised for :memory: databases.
func TestOpenWith_ZeroDefaultsInMemory(t *testing.T) {
	// Zero-valued OpenOptions on :memory: — all defaults applied, then
	// :memory: forces maxOpen back to 1.
	st, err := OpenWith(":memory:", OpenOptions{})
	require.NoError(t, err)
	defer st.Close()

	assert.Equal(t, 1, st.DB.Stats().MaxOpenConnections,
		":memory: must force maxOpen to 1 even with default options")
}
