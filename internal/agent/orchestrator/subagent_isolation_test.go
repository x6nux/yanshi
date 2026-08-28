package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/registry"
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
//
// The work root is seeded with one tracked file BEFORE InitRepo so main_head
// carries it: InitRepo walks the root into the initial commit, and a repo whose
// main tree is empty cannot exercise the "an isolated agent EDITED an existing
// file" half of a merge, only the "added a new one" half.
func newTestOrchestratorWithVCS(t *testing.T) *Orchestrator {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "pre.txt"), []byte("base"), 0o644))
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

// 「被合并回主干」的判据是**主干工作副本里能读到那些编辑**，不是 lifecycle
// 字段翻成了 "merged"。第一版只断言 lifecycle != "active"，于是它在
// MergeToMain 只写 SQLite、一个字节都没落到工作副本的那半年里一直是绿的 ——
// 子代理写的文件在项目里根本不存在，而这条子句宣称它们已经合并了。
//
// 用真实的 fs_write / fs_edit 而不是 os.WriteFile：只有前者会把编辑记进
// worktree 的 changeset，后者写出来的文件 VCS 根本不知道，CommitWorktree 会
// 返回 ErrNoChanges，于是「合并」merge 的是一棵空树 —— 那正是让第一版测试
// 无论生产代码怎么坏都通过的原因。
//
// ledger: A2/W-A-08#3 子代理结束后其 worktree 被合并回主干或显式丢弃
func TestIsolatedSubAgentWorktreeIsSettledOnExit(t *testing.T) {
	o := newTestOrchestratorWithVCS(t)
	fsTools := tools.NewFSTools(o.workRoot)
	ctx := tools.WithSubAgentIsolation(context.Background())

	root, scope, settle := o.acquireSubAgentWorkspace(ctx, "writer")
	require.NotEqual(t, o.workRoot, root)

	fsCtx := tools.WithWorkRoot(ctx, root)
	fsCtx = tools.WithVCS(fsCtx, scope)
	fsCtx = tools.WithProfile(fsCtx, guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"fs_*"}},
		FS:    guard.FSPerm{Read: []string{root + "/**"}, Write: []string{root + "/**"}},
	})

	out, err := fsTools.Write.InvokableRun(fsCtx, `{"path":"note.txt","content":"hi"}`)
	require.NoError(t, err)
	require.NotContainsf(t, out, "denied", "fs_write was denied: %s", out)
	out, err = fsTools.Edit.InvokableRun(fsCtx,
		`{"path":"pre.txt","old_string":"base","new_string":"edited"}`)
	require.NoError(t, err)
	require.NotContainsf(t, out, "denied", "fs_edit was denied: %s", out)

	settle(nil) // 正常结束 → 合并

	states, err := o.vcsScope.VCS.ListWorktreeStates(o.vcsScope.RepoID)
	require.NoError(t, err)
	for _, s := range states {
		// WorktreeState 内嵌 vcs.Worktree，所以 id 是 s.ID。
		require.NotEqualf(t, "active", string(s.Lifecycle),
			"worktree %s was left active; orphan worktrees accumulate until GC", s.ID)
	}

	added, err := os.ReadFile(filepath.Join(o.workRoot, "note.txt"))
	require.NoErrorf(t, err,
		"the isolated sub-agent's new file never reached the main working copy; "+
			"a merge that only moves main_head in SQLite discards every isolated agent's output")
	require.Equal(t, "hi", string(added))

	edited, err := os.ReadFile(filepath.Join(o.workRoot, "pre.txt"))
	require.NoError(t, err)
	require.Equal(t, "edited", string(edited),
		"the isolated sub-agent's edit to an existing tracked file never reached the main working copy")
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

// TestManagedSubAgentFailureIsAnError pins a deliberate behaviour change that
// rode along with W-A-08 and belonged to no acceptance clause: a managed
// sub-agent that reaches a non-completed terminal state now makes
// runSubAgentTurn return an error instead of handing back its partial text
// with err == nil.
//
// It is load-bearing for the isolated path (settle keys off the named error
// return; without it a failed sub-agent's half-finished worktree gets merged
// into main), but it was NOT narrowed to that path, because the old behaviour
// was wrong everywhere: agent_spawn/agent_resume reported success on a run
// that had failed or been cancelled. The general form is kept on purpose, so
// it gets a test that runs WITHOUT a VCS scope — the non-isolated shape — or
// a later "narrow this to isolation" edit would stay green.
func TestManagedSubAgentFailureIsAnError(t *testing.T) {
	mdl := einollm.NewFakeModel([]string{"ok"}, nil)
	o, err := New(Config{Model: mdl, WorkRoot: t.TempDir()})
	require.NoError(t, err)
	require.Nil(t, o.vcsScope.VCS, "this test must exercise the non-isolated shape")

	mgr := registry.NewManager(registry.NewManagerOpts{
		RootContext: context.Background(),
		Path:        filepath.Join(t.TempDir(), "agents.json"),
	})
	t.Cleanup(mgr.Close)

	ctx := tools.WithManager(context.Background(), mgr)
	ctx = tools.WithManagedRunnerFactory(ctx,
		func(_ []string, _ string) registry.Runner {
			return registry.RunnerFunc(func(context.Context, string, string) (string, error) {
				return "half an answer", errors.New("boom")
			})
		})

	out, err := o.runSubAgentTurn(ctx, "do the thing", nil, "", 0)

	require.Error(t, err,
		"a failed managed sub-agent reported success; the parent then treats "+
			"partial output as a finished delegation")
	require.Contains(t, err.Error(), "boom")
	require.Empty(t, out)
}
