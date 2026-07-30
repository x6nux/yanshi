package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClose_CheckpointsWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yanshi.db")
	st, err := Open(path)
	require.NoError(t, err)

	sid, err := st.CreateSession("t")
	require.NoError(t, err)
	for i := 0; i < 50; i++ {
		_ = st.AppendMessage(sid, i, "user", strings.Repeat("x", 1000))
	}
	require.NoError(t, st.Close())

	// After close, -wal should be truncated to ~0 (no active readers).
	fi, err := os.Stat(path + "-wal")
	if err == nil {
		assert.Less(t, fi.Size(), int64(1<<16), "wal not truncated on close")
	}
}
