package vcs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/store"
)

// TestStorage_DeltaGrowthIsLinearInChanges is the regression test for delta
// storage: a commit stores only the CHANGED paths, not a full path→hash snapshot.
// Editing one file across many commits must grow vcs_tree by ~one row per commit
// (O(changes)), not ~files rows per commit (O(files×commits)).
//
// Before the fix, 500 single-file commits on a 40-file repo produced
// 40×501 = 20,040 vcs_tree rows and ~10 MB of bloat. After: 40 + 500 = 540 rows.
func TestStorage_DeltaGrowthIsLinearInChanges(t *testing.T) {
	const trackedFiles = 40
	const commits = 500

	root := t.TempDir()
	for i := 0; i < trackedFiles; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("f%02d.go", i)), []byte("init"), 0o644))
	}

	dbPath := filepath.Join(t.TempDir(), "acc.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer st.Close()
	v := New(st, t.TempDir())
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	countTree := func() int {
		var n int
		require.NoError(t, st.DB.QueryRow("SELECT COUNT(*) FROM vcs_tree").Scan(&n))
		return n
	}
	size := func() int64 {
		fi, _ := os.Stat(dbPath)
		if fi == nil {
			return 0
		}
		return fi.Size()
	}

	initRows := countTree()
	initSize := size()
	require.Equal(t, trackedFiles, initRows, "root commit = one add-row per tracked file")

	// Edit ONE file across `commits` commits.
	target := filepath.Join(root, "f00.go")
	for i := 0; i < commits; i++ {
		require.NoError(t, v.RecordEditMain(repoID, "orchestrator", target, []byte(fmt.Sprintf("edit %d", i))))
		_, err := v.CommitMain(repoID, "orchestrator", fmt.Sprintf("edit %d", i))
		require.NoError(t, err)
	}

	finalRows := countTree()
	finalSize := size()

	assert.Equal(t, initRows+commits, finalRows,
		"vcs_tree must grow by one delta row per commit; full-snapshot would add %d×%d rows", trackedFiles, commits)
	assert.Less(t, finalRows, initRows*50, "row count must stay O(changes), not O(files×commits)")
	assert.Less(t, finalSize-initSize, int64(2*1024*1024), "db growth over %d single-file edits must be < 2 MB", commits)

	var commitCount int
	require.NoError(t, st.DB.QueryRow("SELECT COUNT(*) FROM vcs_commits").Scan(&commitCount))
	assert.Equal(t, commits+1, commitCount)

	t.Logf("delta storage: %d files, %d commits → vcs_tree %d→%d rows, db %.2f→%.2f MB",
		trackedFiles, commits, initRows, finalRows, float64(initSize)/1e6, float64(finalSize)/1e6)
}
