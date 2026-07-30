package goalloop

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/vcs"
)

// newTestVCSRepo builds an in-memory VCS over a fresh temp repo whose root
// contains a single seeded file hello.txt with the given content. Returns the
// VCS, the repo id, and the repo root. The store + temp dirs are cleaned up
// via t.Cleanup.
func newTestVCSRepo(t *testing.T, seedContent string) (*vcs.VCS, string, string) {
	t.Helper()

	s, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })

	v := vcs.New(s, t.TempDir())

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "hello.txt"), []byte(seedContent), 0o644))

	repoID, err := v.InitRepo(root)
	require.NoError(t, err)
	require.NotEmpty(t, repoID)

	return v, repoID, root
}

// mainFileAtHead materializes path as of the current main_head into the repo
// root and returns its content, failing the test on any error. Used to verify
// what landed on main after a commit+merge. B2-RB1: Restore now requires the
// destDir to be a known working copy (repo root or active worktree); restoring
// into the repo root records a pending edit and writes the blob there.
func mainFileAtHead(t *testing.T, v *vcs.VCS, repoID, path string) string {
	t.Helper()
	log, err := v.LogMain(repoID, 1)
	require.NoError(t, err)
	require.Len(t, log, 1, "main must have at least one commit")
	root, err := v.RepoRoot(repoID)
	require.NoError(t, err)
	require.NoError(t, v.Restore(log[0].ID, path, root))
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	require.NoError(t, err)
	return string(data)
}

// ---------------------------------------------------------------------------
// ACPImplementer.WithVCS
// ---------------------------------------------------------------------------

func TestACPImplementer_WithVCS_StoresFields(t *testing.T) {
	t.Parallel()
	v, repoID, _ := newTestVCSRepo(t, "hello")

	a := &ACPImplementer{Agent: "claudecode"}
	a = a.WithVCS(v, repoID, "test.db", "/tmp/wts")

	assert.Same(t, v, a.VCS, "WithVCS must store the VCS pointer")
	assert.Equal(t, repoID, a.RepoID, "WithVCS must store the repo id")
	assert.Equal(t, "test.db", a.VCSDBPath, "WithVCS must store the db path")
	assert.Equal(t, "/tmp/wts", a.WorktreeDir, "WithVCS must store the worktree dir")
}

func TestACPImplementer_WithVCS_ChainsWithProfile(t *testing.T) {
	t.Parallel()
	v, repoID, _ := newTestVCSRepo(t, "hello")

	// WithVCS must return *ACPImplementer so it chains with WithProfile.
	a := (&ACPImplementer{Agent: "claudecode"}).
		WithProfile(guard.PermissionProfile{Tools: guard.ToolsPerm{Allow: []string{"fs.*"}}}).
		WithVCS(v, repoID, "test.db", "/tmp/wts")

	assert.True(t, a.profileSet, "chained WithProfile must mark profileSet")
	assert.Same(t, v, a.VCS)
	assert.Equal(t, repoID, a.RepoID)
	require.NotNil(t, a.Profile.Tools.Allow)
	assert.Contains(t, a.Profile.Tools.Allow, "fs.*")
}

// ---------------------------------------------------------------------------
// worker.commitAndMerge — the post-spawn commit+merge sequence runWithAutoVCS
// uses, exercised directly (no ACP agent spawn required).
// ---------------------------------------------------------------------------

// TestWorker_CommitAndMerge_MergesEditsToMain proves the sequence the autoVCS
// worker uses after the agent prompt — RecordEditWorktree (via the recorder) ->
// CommitWorktree -> MergeToMain — lands the worktree's edit on main when there
// is no conflict. It drives commitAndMerge directly with a pre-recorded edit.
func TestWorker_CommitAndMerge_MergesEditsToMain(t *testing.T) {
	t.Parallel()
	v, repoID, _ := newTestVCSRepo(t, "hello")

	wt, err := v.AddWorktree(repoID, []string{"claudecode"})
	require.NoError(t, err)

	// Simulate the agent editing hello.txt in the worktree (the recorder routes
	// this into the worktree's pending changeset). The absPath is under the
	// worktree's working dir because RecordEditWorktree resolves paths relative
	// to wt.Path (where AddWorktree materialized main's tree).
	require.NoError(t,
		v.RecordEditWorktree(wt.ID, "claudecode", filepath.Join(wt.Path, "hello.txt"), []byte("edited")))

	w := &worker{agent: "claudecode", vcs: v, repoID: repoID}
	res, err := w.commitAndMerge(wt.ID, workerTask{Step: "edit hello", Index: 0})
	require.NoError(t, err)
	assert.Equal(t, "step 0 done (merged to main)", res)

	// main_head must now carry the edited content.
	assert.Equal(t, "edited", mainFileAtHead(t, v, repoID, "hello.txt"))

	// The worktree's changeset must have been consumed by the commit.
	assert.Empty(t, v.Uncommitted("worktree", wt.ID))
}

// TestWorker_CommitAndMerge_ReportsConflicts proves that when main and the
// worktree both changed the same file, commitAndMerge does not force-resolve:
// it returns a conflicts message and main keeps its own version.
func TestWorker_CommitAndMerge_ReportsConflicts(t *testing.T) {
	t.Parallel()
	v, repoID, root := newTestVCSRepo(t, "hello")

	wt, err := v.AddWorktree(repoID, []string{"claudecode"})
	require.NoError(t, err)

	// Move main forward AFTER the worktree was branched (so base != ours).
	require.NoError(t,
		v.RecordEditMain(repoID, "orchestrator", filepath.Join(root, "hello.txt"), []byte("main-edit")))
	_, err = v.CommitMain(repoID, "orchestrator", "main moves")
	require.NoError(t, err)

	// Worktree edits the same file to a different value + commits it.
	require.NoError(t,
		v.RecordEditWorktree(wt.ID, "claudecode", filepath.Join(wt.Path, "hello.txt"), []byte("wt-edit")))
	_, err = v.CommitWorktree(wt.ID, "claudecode", "wt edit")
	require.NoError(t, err)

	w := &worker{agent: "claudecode", vcs: v, repoID: repoID}
	res, err := w.commitAndMerge(wt.ID, workerTask{Step: "conflicting edit", Index: 1})
	require.NoError(t, err, "conflicts are reported, not returned as an error")
	assert.Contains(t, res, "merged with conflicts")
	assert.Contains(t, res, "hello.txt")

	// main must keep its own version (merge did not proceed on conflict).
	assert.Equal(t, "main-edit", mainFileAtHead(t, v, repoID, "hello.txt"))
}

// TestWorker_CommitAndMerge_NoEditsIsNoOp proves that when the worktree has no
// pending edits (CommitWorktree returns vcs.ErrNoChanges), commitAndMerge still
// succeeds and leaves main content unchanged. This is the path an agent that
// made no file edits would hit.
func TestWorker_CommitAndMerge_NoEditsIsNoOp(t *testing.T) {
	t.Parallel()
	v, repoID, _ := newTestVCSRepo(t, "hello")

	wt, err := v.AddWorktree(repoID, []string{"claudecode"})
	require.NoError(t, err)

	// No RecordEditWorktree: the worktree has zero pending edits.
	w := &worker{agent: "claudecode", vcs: v, repoID: repoID}
	res, err := w.commitAndMerge(wt.ID, workerTask{Step: "noop step", Index: 2})
	require.NoError(t, err, "commit error must be ignored; merge of identical trees must succeed")
	assert.Contains(t, res, "merged to main")

	// Content on main is unchanged.
	assert.Equal(t, "hello", mainFileAtHead(t, v, repoID, "hello.txt"))
}

// ---------------------------------------------------------------------------
// worker.vcsSpawnOpts — proves the recorder built into SpawnOptions routes
// agent diff edits into the worktree's changeset (what Spawn binds on the
// client via SetVCSTracking).
// ---------------------------------------------------------------------------

func TestWorker_VCSSpawnOpts_BindsWorktreeRecorder(t *testing.T) {
	t.Parallel()
	v, repoID, _ := newTestVCSRepo(t, "hello")

	wt, err := v.AddWorktree(repoID, []string{"claudecode"})
	require.NoError(t, err)

	w := &worker{agent: "claudecode", vcs: v, repoID: repoID, vcsDBPath: "test.db", worktreeDir: "/tmp/wts"}
	profile := worktreeScopedProfile(wt.Path)
	opts := w.vcsSpawnOpts(wt.Path, profile, wt.ID)

	assert.Equal(t, wt.ID, opts.WorktreeID, "WorktreeID must be set on SpawnOptions")
	require.NotNil(t, opts.Recorder, "Recorder callback must be wired")
	assert.Equal(t, "claudecode", opts.Agent, "base spawnOpts fields preserved")
	assert.Equal(t, wt.Path, opts.Cwd)

	// MCPCommand must deliver the yanshi-vcs MCP server (the vcs_* tools) to
	// the spawned ACP agent as a stdio subprocess of the yanshi binary.
	require.NotNil(t, opts.MCPCommand, "MCPCommand must be populated on the autoVCS path")
	assert.Equal(t, []string{"vcs-mcp"}, opts.MCPCommand["args"], "args must run the vcs-mcp subcommand")
	env, ok := opts.MCPCommand["env"].(map[string]string)
	require.True(t, ok, "env must be a map[string]string")
	assert.Equal(t, "test.db", env["YANSHI_DB_PATH"])
	assert.Equal(t, repoID, env["YANSHI_REPO_ID"])
	assert.Equal(t, wt.ID, env["YANSHI_WT_ID"])
	assert.Equal(t, "claudecode", env["YANSHI_AGENT"])
	assert.Equal(t, "/tmp/wts", env["YANSHI_WORKTREE_DIR"])

	// Driving the recorder must route the edit into the worktree changeset,
	// exactly as Spawn's SetVCSTracking + applyDiffContent would during a real
	// prompt. The absPath is under wt.Path (the agent's cwd).
	err = opts.Recorder(wt.ID, "acp", filepath.Join(wt.Path, "hello.txt"), []byte("via-recorder"))
	require.NoError(t, err)
	pending := v.Uncommitted("worktree", wt.ID)
	assert.Contains(t, pending, "hello.txt", "recorder must route edit to the worktree changeset")

	// main scope must be untouched by a worktree-scoped recorder call.
	assert.Empty(t, v.Uncommitted("main", repoID))
}

// ---------------------------------------------------------------------------
// worker.run dispatch
// ---------------------------------------------------------------------------

// TestWorker_Run_DispatchesToAutoVCS verifies that a worker constructed with
// vcs+repoID selects the autoVCS path. We can't drive runWithAutoVCS end-to-end
// without a real/fake ACP agent (Spawn builds an exec.Cmd), but we can prove
// the dispatch branch is taken by giving it an invalid repo and asserting the
// error is the autoVCS AddWorktree error (not a git worktree error).
func TestWorker_Run_DispatchesToAutoVCS(t *testing.T) {
	t.Parallel()
	v, _, _ := newTestVCSRepo(t, "hello")

	// A bogus repo id makes AddWorktree fail immediately — the error wrapping
	// "autovcs addworktree" proves run took the autoVCS branch (runWithGit would
	// instead fail on a nil WorktreeHelper or produce a "worktree add" error).
	w := &worker{agent: "claudecode", vcs: v, repoID: "no-such-repo"}
	_, err := w.run(context.Background(), workerTask{Step: "x", Index: 0})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "autovcs addworktree")
}

// TestWorker_Run_FallsBackToGitPath verifies that a worker with no VCS falls
// back to the git WorktreeHelper path. We point WorktreeHelper at a non-git
// dir so WorktreeHelper.Add fails cleanly; the "worktree add" error wrap (not
// "autovcs addworktree") proves run dispatched to runWithGit.
func TestWorker_Run_FallsBackToGitPath(t *testing.T) {
	t.Parallel()
	w := &worker{
		agent: "claudecode",
		wt:    &WorktreeHelper{RepoDir: t.TempDir()}, // not a git repo → Add fails
	}
	_, err := w.run(context.Background(), workerTask{Step: "x", Index: 0})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree add", "git-path error wrap must be present")
	assert.NotContains(t, err.Error(), "autovcs", "must not take the autoVCS branch")
}
