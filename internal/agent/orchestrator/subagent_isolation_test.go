package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
	"github.com/x6nux/yanshi/internal/vcs"
)

// newTestOrchestratorWithVCS builds an Orchestrator whose WorkRoot and
// VCSScope are backed by a real (in-memory-store) VCS repo, so
// acquireSubAgentWorkspace has a genuine worktree lifecycle to allocate
// against. The model is a FakeModel that is never actually invoked by these
// tests (they call acquireSubAgentWorkspace/workRootForSubAgentTurn
// directly, not Query), so its canned response is irrelevant.
func newTestOrchestratorWithVCS(t *testing.T) *Orchestrator {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	v := vcs.New(st, filepath.Join(t.TempDir(), "worktrees"))
	repoID, err := v.InitRepo(root)
	require.NoError(t, err)

	mdl := einollm.NewFakeModel([]string{"ok"}, nil)
	o, err := New(Config{
		Model:    mdl,
		WorkRoot: root,
		VCSScope: tools.VCSScope{VCS: v, RepoID: repoID, Agent: "orchestrator"},
	})
	require.NoError(t, err)
	return o
}

// ledger: A2/W-A-08#1 并发子代理各自在独立 worktree 中编辑且互不覆盖
func TestConcurrentIsolatedSubAgentsDoNotShareAWorkRoot(t *testing.T) {
	o := newTestOrchestratorWithVCS(t)

	const n = 4
	roots := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := tools.WithSubAgentIsolation(context.Background())
			roots[i] = o.workRootForSubAgentTurn(ctx, "agent-"+string(rune('a'+i)))
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, r := range roots {
		require.NotEmptyf(t, r, "sub-agent %d got no work root", i)
		require.Falsef(t, seen[r], "sub-agent %d reused work root %q; concurrent agents overwrite each other", i, r)
		seen[r] = true
	}
}

// ledger: A2/W-A-08#2 未请求隔离的子代理行为与本改动前一致
func TestNonIsolatedSubAgentKeepsTheSharedWorkRoot(t *testing.T) {
	o := newTestOrchestratorWithVCS(t)

	got := o.workRootForSubAgentTurn(context.Background(), "plain")

	require.Equal(t, o.workRoot, got,
		"an unrequested worktree per agent_spawn would fill ~/.yanshi/worktrees/ with one directory per call")
}

// ledger: A2/W-A-08#3 子代理结束后其 worktree 被合并回主干或显式丢弃
func TestIsolatedSubAgentWorktreeIsSettledOnExit(t *testing.T) {
	o := newTestOrchestratorWithVCS(t)
	ctx := tools.WithSubAgentIsolation(context.Background())

	root, _, settle := o.acquireSubAgentWorkspace(ctx, "writer")
	require.NotEqual(t, o.workRoot, root)

	require.NoError(t, os.WriteFile(filepath.Join(root, "note.txt"), []byte("hi"), 0o644))
	settle(nil) // 正常结束 → 合并

	states, err := o.vcsScope.VCS.ListWorktreeStates(o.vcsScope.RepoID)
	require.NoError(t, err)
	for _, s := range states {
		// WorktreeState 内嵌 vcs.Worktree，所以 id 是 s.ID。
		require.NotEqualf(t, "active", string(s.Lifecycle),
			"worktree %s was left active; orphan worktrees accumulate until GC", s.ID)
	}
}

// ledger: A2/W-A-08#4 子代理失败时其 worktree 被标记为放弃而不是合并
func TestFailedIsolatedSubAgentWorktreeIsAbandoned(t *testing.T) {
	o := newTestOrchestratorWithVCS(t)
	ctx := tools.WithSubAgentIsolation(context.Background())

	root, _, settle := o.acquireSubAgentWorkspace(ctx, "failer")
	require.NoError(t, os.WriteFile(filepath.Join(root, "half.txt"), []byte("partial"), 0o644))
	settle(context.Canceled) // 失败结束 → 放弃

	_, err := os.Stat(filepath.Join(o.workRoot, "half.txt"))
	require.True(t, os.IsNotExist(err),
		"a failed sub-agent's half-finished edits must not land on main")
}

// TestIsolatedSubAgentFSWriteLandsInItsOwnWorktree is NOT one of the four
// ledger clauses above (it is not pinned in docs/feature-status.yaml). The
// four tests above only exercise acquireSubAgentWorkspace's return values —
// none of them route a real fs_write/fs_edit tool call through the isolated
// root, so none would have caught the actual defect this task's production
// fix addresses: FSTools.abs resolved every model-supplied path against a
// single static f.root field set once at bootstrap (tools.NewFSTools(workRoot)),
// with zero ctx-awareness. bootstrap builds exactly ONE *FSTools instance
// and reuses it (via selectSubAgentTools) for every sub-agent turn, isolated
// or not — so even a sub-agent that was correctly handed an isolated
// worktree root by acquireSubAgentWorkspace would, without FSTools.rootFor
// consulting tools.WorkRootFromContext(ctx) first, still have every real
// fs_write/fs_edit land on the single shared process-wide root, making the
// entire worktree lifecycle above inert for its stated purpose. This test
// reuses a single shared *FSTools (mirroring bootstrap's one-instance
// reuse) and asserts the write lands under the worktree, not the shared
// root, and is tracked into the worktree's own VCS changeset.
func TestIsolatedSubAgentFSWriteLandsInItsOwnWorktree(t *testing.T) {
	o := newTestOrchestratorWithVCS(t)

	// One shared FSTools instance, exactly like bootstrap.go builds once and
	// selectSubAgentTools reuses across every sub-agent turn.
	fsTools := tools.NewFSTools(o.workRoot)

	ctx := tools.WithSubAgentIsolation(context.Background())
	root, scope, settle := o.acquireSubAgentWorkspace(ctx, "writer")
	require.NotEqual(t, o.workRoot, root)
	require.NotEmpty(t, scope.WorktreeID)

	// Mirrors what runSubAgentTurn actually binds onto ctx before dispatch
	// (see runSubAgentTurn's ctx = tools.WithWorkRoot(ctx, root) / WithVCS).
	fsCtx := tools.WithWorkRoot(ctx, root)
	fsCtx = tools.WithVCS(fsCtx, scope)
	fsCtx = tools.WithProfile(fsCtx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{root + "/**"}, Write: []string{root + "/**"}},
	})

	out, err := fsTools.Write.InvokableRun(fsCtx, `{"path":"note.txt","content":"hello"}`)
	require.NoError(t, err)
	require.NotContainsf(t, out, "denied", "fs_write was denied: %s", out)

	// The byte-for-byte proof: the file exists under the isolated worktree,
	// not under the shared root the single FSTools instance was constructed
	// with.
	got, rerr := os.ReadFile(filepath.Join(root, "note.txt"))
	require.NoError(t, rerr, "fs_write did not land in the isolated worktree")
	require.Equal(t, "hello", string(got))
	_, serr := os.Stat(filepath.Join(o.workRoot, "note.txt"))
	require.Truef(t, os.IsNotExist(serr),
		"fs_write leaked through the shared FSTools instance to the shared work root: %v", serr)

	// trackEdit (fs.go) dispatches on scope.WorktreeID, so a correctly
	// ctx-routed write must show up in the worktree's own uncommitted
	// changeset, not main's.
	pending := o.vcsScope.VCS.Uncommitted("worktree", scope.WorktreeID)
	require.Contains(t, pending, "note.txt")

	settle(nil)
}
