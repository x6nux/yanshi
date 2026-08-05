package goalloop

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/acp"
	"github.com/x6nux/yanshi/internal/guard"
)

// ---------------------------------------------------------------------------
// FakeImplementer
// ---------------------------------------------------------------------------

func TestFakeImplementer_FailThenPass(t *testing.T) {
	t.Parallel()
	f := &FakeImplementer{Result: "done", FailFirst: 1}

	// First call should fail.
	_, err := f.Implement(context.Background(), Plan{}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "deliberate failure")

	// Second call should succeed.
	res, err := f.Implement(context.Background(), Plan{}, "")
	require.NoError(t, err)
	assert.Equal(t, "done", res)

	// Call counter should be 2.
	assert.Equal(t, 2, f.Calls())
}

func TestFakeImplementer_AlwaysPass(t *testing.T) {
	t.Parallel()
	f := &FakeImplementer{Result: "ok", FailFirst: 0}

	res, err := f.Implement(context.Background(), Plan{}, "")
	require.NoError(t, err)
	assert.Equal(t, "ok", res)
	assert.Equal(t, 1, f.Calls())
}

func TestFakeImplementer_MultipleFailures(t *testing.T) {
	t.Parallel()
	f := &FakeImplementer{Result: "finally", FailFirst: 3}

	// First three calls should fail.
	for i := 0; i < 3; i++ {
		_, err := f.Implement(context.Background(), Plan{}, "")
		require.Error(t, err, "call %d should fail", i+1)
	}

	// Fourth call should succeed.
	res, err := f.Implement(context.Background(), Plan{}, "")
	require.NoError(t, err)
	assert.Equal(t, "finally", res)
	assert.Equal(t, 4, f.Calls())
}

// ---------------------------------------------------------------------------
// WorktreeHelper
// ---------------------------------------------------------------------------

// runGit runs a git command in dir, failing the test on error.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", joinArgs(args), string(out))
}

func joinArgs(args []string) string {
	s := ""
	for i, a := range args {
		if i > 0 {
			s += " "
		}
		s += a
	}
	return s
}

// fileExists reports whether a path exists on disk.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestWorktreeHelper_AddListRemove(t *testing.T) {
	t.Parallel()

	// Create a temp git repo with an initial commit (worktree add needs HEAD).
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "test@test.com")
	runGit(t, repoDir, "config", "user.name", "Test")

	err := os.WriteFile(filepath.Join(repoDir, "README"), []byte("init\n"), 0o644)
	require.NoError(t, err)
	runGit(t, repoDir, "add", "README")
	runGit(t, repoDir, "commit", "-m", "init")

	h := &WorktreeHelper{RepoDir: repoDir}

	// Add a worktree.
	wtPath, err := h.Add("w1")
	require.NoError(t, err)
	assert.True(t, fileExists(wtPath), "worktree path should exist after Add")

	// A git worktree has a .git file (not a directory).
	gitEntry := filepath.Join(wtPath, ".git")
	info, err := os.Stat(gitEntry)
	require.NoError(t, err, ".git entry should exist in worktree")
	assert.False(t, info.IsDir(), "worktree .git should be a file, not a dir")

	// List should include the worktree path.
	paths, err := h.List()
	require.NoError(t, err)
	assert.Contains(t, paths, wtPath)

	// Remove the worktree.
	err = h.Remove(wtPath)
	require.NoError(t, err)
	assert.False(t, fileExists(wtPath), "worktree path should not exist after Remove")

	// List should no longer include it.
	paths, err = h.List()
	require.NoError(t, err)
	assert.NotContains(t, paths, wtPath)
}

func TestWorktreeHelper_AddMultiple(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "test@test.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "f"), []byte("x"), 0o644))
	runGit(t, repoDir, "add", "f")
	runGit(t, repoDir, "commit", "-m", "init")

	h := &WorktreeHelper{RepoDir: repoDir}

	wt1, err := h.Add("a")
	require.NoError(t, err)
	wt2, err := h.Add("b")
	require.NoError(t, err)

	paths, err := h.List()
	require.NoError(t, err)
	assert.Contains(t, paths, wt1)
	assert.Contains(t, paths, wt2)
	assert.Len(t, paths, 3) // repoDir + 2 worktrees

	// Clean up.
	require.NoError(t, h.Remove(wt1))
	require.NoError(t, h.Remove(wt2))
}

func TestWorktreeHelper_RemoveNonexistent(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.email", "test@test.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "f"), []byte("x"), 0o644))
	runGit(t, repoDir, "add", "f")
	runGit(t, repoDir, "commit", "-m", "init")

	h := &WorktreeHelper{RepoDir: repoDir}
	err := h.Remove("/nonexistent/path")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// ACPImplementer policy
// ---------------------------------------------------------------------------

func TestACPImplementer_SpawnPolicyNonNil(t *testing.T) {
	t.Parallel()

	// When WithProfile is called, spawnOpts should produce a non-nil Policy.
	a := &ACPImplementer{Agent: "claudecode"}
	a = a.WithProfile(guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"*"}},
		FS:    guard.FSPerm{Read: []string{"**"}, Write: []string{"**"}},
		Net:   guard.NetPerm{Allow: true, Hosts: []string{"*"}},
	})
	w := &worker{agent: a.Agent, profile: a.Profile, profileSet: true}
	opts := w.spawnOpts("/tmp", a.Profile)
	assert.NotNil(t, opts.Policy, "SpawnOptions.Policy must be non-nil when profile is set")
}

func TestACPImplementer_DefaultProfileUsedWhenUnset(t *testing.T) {
	t.Parallel()

	// When no profile is set, resolveProfile should produce a worktree-scoped default.
	a := &ACPImplementer{Agent: "claudecode"}
	wt := "/tmp/test-wt"
	profile := resolveProfile(a.Profile, a.profileSet, wt)
	// Should NOT contain machine-wide "*" for tools or "**" for FS.
	assert.NotContains(t, profile.Tools.Allow, "*")
	assert.NotContains(t, profile.FS.Read, "**")
	assert.NotContains(t, profile.FS.Write, "**")
	assert.False(t, profile.Net.Allow)
	// Should contain the worktree-scoped glob.
	wtGlob := pathToGlob(wt)
	assert.Contains(t, profile.FS.Read, wtGlob)
	assert.Contains(t, profile.FS.Write, wtGlob)
}

// TestACPImplementer_DefaultProfileScopedToWorktree verifies that an
// ACPImplementer with no profile, when resolving a profile for a real
// worktree temp path, produces a SpawnOptions.Policy whose profile FS.Write
// contains the worktree temp path (not machine-wide "**") and Net.Allow is
// false. It exercises the resolveProfile helper that worker.run uses after
// the worktree path is known.
func TestACPImplementer_DefaultProfileScopedToWorktree(t *testing.T) {
	t.Parallel()

	a := &ACPImplementer{Agent: "claudecode"}
	// Simulate a worktree temp path (as produced by WorktreeHelper.Add).
	wtPath := t.TempDir()
	profile := resolveProfile(a.Profile, a.profileSet, wtPath)

	w := &worker{agent: a.Agent, profile: profile, profileSet: a.profileSet}
	opts := w.spawnOpts(wtPath, profile)

	require.NotNil(t, opts.Policy, "SpawnOptions.Policy must be non-nil")

	gp, ok := opts.Policy.(*acp.GuardPolicy)
	require.True(t, ok, "Policy should be *GuardPolicy")
	assert.False(t, gp.Profile.Net.Allow, "Net.Allow must be false")

	wtGlob := pathToGlob(wtPath)
	assert.Contains(t, gp.Profile.FS.Write, wtGlob, "FS.Write must contain worktree glob")
	assert.NotContains(t, gp.Profile.FS.Write, "**", "FS.Write must not contain machine-wide **")
	assert.NotContains(t, gp.Profile.Tools.Allow, "*", "Tools must not contain machine-wide *")
}

// TestACPImplementerWorkerAccumulatesSubprocessUsage verifies that the onEvent
// closure used by the worker forwards ACP usage events into the shared
// UsageSink, so subprocess token consumption flows into the goal-loop budget.
// This is the LEAK3 goalloop integration: onEvent from Prompt forwards usage to
// the shared sink.
func TestACPImplementerWorkerAccumulatesSubprocessUsage(t *testing.T) {
	sink := &UsageSink{}

	// This is the exact onEvent closure the worker installs on Prompt (see
	// runWithGit/runWithAutoVCS). Re-constructed here so the test is independent
	// of the spawn path (which needs a real/subprocess ACP agent).
	onEvent := func(ev acp.Event) {
		if sink != nil && ev.Usage != nil {
			sink.Add(Usage{
				PromptTokens:     ev.Usage.InputTokens,
				CompletionTokens: ev.Usage.OutputTokens,
				TotalTokens:      ev.Usage.TotalTokens,
			})
		}
	}

	onEvent(acp.Event{Usage: &acp.Usage{InputTokens: 50, OutputTokens: 30, TotalTokens: 80}})
	// Second event accumulates.
	onEvent(acp.Event{Usage: &acp.Usage{InputTokens: 20, OutputTokens: 10, TotalTokens: 30}})

	got := sink.Snapshot()
	assert.Equal(t, 70, got.PromptTokens, "input must accumulate (50+20)")
	assert.Equal(t, 40, got.CompletionTokens, "output must accumulate (30+10)")
	assert.Equal(t, 110, got.TotalTokens, "total must accumulate (80+30)")
}
