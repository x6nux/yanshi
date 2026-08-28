package vcs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// mergeFixture builds a repo whose main tree already carries two tracked files
// and returns (vcs, repoID, rootPath).
func mergeFixture(t *testing.T) (*VCS, string, string) {
	t.Helper()
	v := newTestVCS(t)
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "kept.txt"), []byte("kept-v1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "shared.txt"), []byte("shared-v1"), 0o644))
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	// InitRepo canonicalizes the root (macOS /var -> /private/var); read the
	// stored path back so the assertions below stat the same directory the
	// materializer writes to.
	r, err := v.getRepo(repoID)
	require.NoError(t, err)
	return v, repoID, r.RootPath
}

// TestMergeToMainWritesMergedBytesToTheWorkingCopy is the regression test for
// the defect that made worktree isolation a net loss: MergeToMain wrote a
// commit row and moved main_head and stopped. Every caller — sub-agent settle,
// acp_delegate, goalloop's worker, the vcs_merge tool, the VCS MCP server —
// reported a successful merge while the files on disk never changed.
func TestMergeToMainWritesMergedBytesToTheWorkingCopy(t *testing.T) {
	v, repoID, root := mergeFixture(t)

	wt, err := v.AddWorktree(repoID, []string{"agent"})
	require.NoError(t, err)
	require.NoError(t, v.RecordEditWorktree(wt.ID, "agent",
		filepath.Join(wt.Path, "added.txt"), []byte("added-v1")))
	require.NoError(t, v.RecordEditWorktree(wt.ID, "agent",
		filepath.Join(wt.Path, "shared.txt"), []byte("shared-v2")))
	_, err = v.CommitWorktree(wt.ID, "agent", "work")
	require.NoError(t, err)

	cid, conflicts, err := v.MergeToMain(wt.ID, "agent", false)
	require.NoError(t, err)
	require.Empty(t, conflicts)
	require.NotEmpty(t, cid)

	added, err := os.ReadFile(filepath.Join(root, "added.txt"))
	require.NoError(t, err, "a file the worktree created never reached main's working copy")
	require.Equal(t, "added-v1", string(added))

	shared, err := os.ReadFile(filepath.Join(root, "shared.txt"))
	require.NoError(t, err)
	require.Equal(t, "shared-v2", string(shared),
		"a file the worktree modified never reached main's working copy")
}

// TestMergeToMainDoesNotClobberUncommittedMainEdits pins WHY the merge
// materializes only the paths it changed instead of the whole merged tree.
//
// main's working copy routinely carries edits that fs_write already put on
// disk and recorded into the uncommitted changeset, but that no CommitMain has
// folded into main_head yet (CommitMain is only ever called by the vcs_commit
// tool). A full-tree materialize would rewrite every tracked path from
// main_head and silently revert all of them — trading the data loss this fix
// closes for a different one.
func TestMergeToMainDoesNotClobberUncommittedMainEdits(t *testing.T) {
	v, repoID, root := mergeFixture(t)

	wt, err := v.AddWorktree(repoID, []string{"agent"})
	require.NoError(t, err)
	require.NoError(t, v.RecordEditWorktree(wt.ID, "agent",
		filepath.Join(wt.Path, "added.txt"), []byte("added-v1")))
	_, err = v.CommitWorktree(wt.ID, "agent", "work")
	require.NoError(t, err)

	// An uncommitted main edit: on disk and in the changeset, not in main_head.
	keptPath := filepath.Join(root, "kept.txt")
	require.NoError(t, os.WriteFile(keptPath, []byte("kept-uncommitted"), 0o644))
	require.NoError(t, v.RecordEditMain(repoID, "orchestrator", keptPath, []byte("kept-uncommitted")))

	_, _, err = v.MergeToMain(wt.ID, "agent", false)
	require.NoError(t, err)

	kept, err := os.ReadFile(keptPath)
	require.NoError(t, err)
	require.Equal(t, "kept-uncommitted", string(kept),
		"the merge reverted an uncommitted main edit it had no business touching")
}

// TestMergeToMainMaterializesDeletions covers the third shape: the worktree
// removed a tracked file, so the merged tree lacks it and the working copy
// must lose it too. Without this the merged tree and the disk disagree in the
// one direction a "write the changed paths" implementation is most likely to
// skip.
func TestMergeToMainMaterializesDeletions(t *testing.T) {
	v, repoID, root := mergeFixture(t)

	wt, err := v.AddWorktree(repoID, []string{"agent"})
	require.NoError(t, err)
	require.NoError(t, v.RecordDeleteWorktree(wt.ID, "agent", filepath.Join(wt.Path, "shared.txt")))
	_, err = v.CommitWorktree(wt.ID, "agent", "drop shared")
	require.NoError(t, err)

	_, _, err = v.MergeToMain(wt.ID, "agent", false)
	require.NoError(t, err)

	_, statErr := os.Stat(filepath.Join(root, "shared.txt"))
	require.True(t, os.IsNotExist(statErr),
		"the merge kept a file the merged tree had deleted: %v", statErr)
}
