package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/x6nux/yanshi/internal/vcs"
)

// VCSTools exposes the vcs_* GuardedTools. Each tool reads the active VCSScope
// from context (bound via WithVCS) and routes main-vs-worktree based on whether
// the scope's WorktreeID is set.
type VCSTools struct {
	Commit  *GuardedTool
	Log     *GuardedTool
	Diff    *GuardedTool
	Restore *GuardedTool
	Merge   *GuardedTool
	Revert  *GuardedTool // B2-RB1: revert_turn agent tool (VCS-only, destructive)
}

// NewVCSTools builds the vcs_* tools. They read the active VCSScope from context.
func NewVCSTools() *VCSTools {
	t := &VCSTools{}
	t.Commit = NewGuardedTool(
		"vcs_commit", "VCS Commit", "Snapshot the active scope's pending edits as a commit (author = acting agent).",
		60*time.Second,
		params(map[string]*schema.ParameterInfo{
			"message": {Type: schema.String, Desc: "commit message", Required: true},
		}),
		SyncStream(t.runCommit),
	)
	t.Log = NewGuardedTool(
		"vcs_log", "VCS Log", "List commits for the active scope (newest-first).",
		60*time.Second,
		params(map[string]*schema.ParameterInfo{
			"limit": {Type: schema.Integer, Desc: "max entries (default 20)"},
		}),
		SyncStream(t.runLog),
	)
	t.Diff = NewGuardedTool(
		"vcs_diff", "VCS Diff", "Show file-level changes between two commits (or vs the active head).",
		60*time.Second,
		params(map[string]*schema.ParameterInfo{
			"ref_a": {Type: schema.String, Desc: "from commit id (default: active head)"},
			"ref_b": {Type: schema.String, Desc: "to commit id (default: active head parent / init)"},
		}),
		SyncStream(t.runDiff),
	)
	t.Restore = NewGuardedTool(
		"vcs_restore", "VCS Restore", "Restore a file from a commit into the active working copy.",
		60*time.Second,
		params(map[string]*schema.ParameterInfo{
			"ref":  {Type: schema.String, Desc: "commit id", Required: true},
			"path": {Type: schema.String, Desc: "repo-relative file path", Required: true},
		}),
		SyncStream(t.runRestore),
	)
	t.Merge = NewGuardedTool(
		"vcs_merge", "VCS Merge", "Merge a worktree into main (tree-level 3-way). Returns conflicts; force overrides.",
		60*time.Second,
		params(map[string]*schema.ParameterInfo{
			"worktree": {Type: schema.String, Desc: "worktree id to merge", Required: true},
			"force":    {Type: schema.Boolean, Desc: "on conflict, let the worktree version win"},
		}),
		SyncStream(t.runMerge),
	)
	t.Revert = NewGuardedTool(
		"revert_turn", "Revert Turn",
		"Revert the main working copy and VCS head to a prior turn seam. This agent tool is VCS-only; use WS /restore-turn to restore conversation history too. Destructive: always prompts even in yolo/allow-edits mode.",
		60*time.Second,
		params(map[string]*schema.ParameterInfo{
			"seam_id": {Type: schema.String, Desc: "seam id (from vcs_log or /restore-turn)", Required: true},
		}),
		SyncStream(t.runRevert),
	)
	return t
}

// Tools returns all vcs tools.
func (t *VCSTools) Tools() []*GuardedTool {
	return []*GuardedTool{t.Commit, t.Log, t.Diff, t.Restore, t.Merge, t.Revert}
}

// vcsScopeFromCtx returns the active VCSScope or an error if none is bound / the
// VCS is unconfigured. Named to avoid clashing with the VCSScope type.
func vcsScopeFromCtx(ctx context.Context) (VCSScope, error) {
	sc, ok := VCSScopeFromContext(ctx)
	if !ok || sc.VCS == nil {
		return VCSScope{}, fmt.Errorf("vcs: no VCS scope in context")
	}
	return sc, nil
}

// scopeHeadCommit returns the active head commit id (worktree tip or main_head)
// and its parent id, using the scope's Log accessor with limit 1. Both are empty
// if the scope has no commits yet.
func scopeHeadCommit(sc VCSScope) (head, parent string, err error) {
	var commits []vcs.Commit
	if sc.WorktreeID != "" {
		commits, err = sc.VCS.LogWorktree(sc.WorktreeID, 1)
	} else {
		commits, err = sc.VCS.LogMain(sc.RepoID, 1)
	}
	if err != nil {
		return "", "", err
	}
	if len(commits) > 0 {
		return commits[0].ID, commits[0].ParentID, nil
	}
	return "", "", nil
}

// --- arg types ---

type vcsCommitArgs struct {
	Message string `json:"message"`
}

type vcsLogArgs struct {
	Limit int `json:"limit"`
}

type vcsDiffArgs struct {
	RefA string `json:"ref_a"`
	RefB string `json:"ref_b"`
}

type vcsRestoreArgs struct {
	Ref  string `json:"ref"`
	Path string `json:"path"`
}

type vcsMergeArgs struct {
	Worktree string `json:"worktree"`
	Force    bool   `json:"force"`
}

// --- run functions ---

func (t *VCSTools) runCommit(ctx context.Context, argsJSON string) (string, error) {
	var a vcsCommitArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	sc, err := vcsScopeFromCtx(ctx)
	if err != nil {
		return "", err
	}
	var id string
	if sc.WorktreeID != "" {
		id, err = sc.VCS.CommitWorktree(sc.WorktreeID, sc.Agent, a.Message)
	} else {
		id, err = sc.VCS.CommitMain(sc.RepoID, sc.Agent, a.Message)
	}
	if err != nil {
		return "", err
	}
	return toJSON(map[string]string{"commit": id}), nil
}

func (t *VCSTools) runLog(ctx context.Context, argsJSON string) (string, error) {
	var a vcsLogArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	limit := a.Limit
	if limit == 0 {
		limit = 20
	}
	sc, err := vcsScopeFromCtx(ctx)
	if err != nil {
		return "", err
	}
	var commits []vcs.Commit
	if sc.WorktreeID != "" {
		commits, err = sc.VCS.LogWorktree(sc.WorktreeID, limit)
	} else {
		commits, err = sc.VCS.LogMain(sc.RepoID, limit)
	}
	if err != nil {
		return "", err
	}
	return toJSON(commits), nil
}

func (t *VCSTools) runDiff(ctx context.Context, argsJSON string) (string, error) {
	var a vcsDiffArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	sc, err := vcsScopeFromCtx(ctx)
	if err != nil {
		return "", err
	}
	head, parent, err := scopeHeadCommit(sc)
	if err != nil {
		return "", err
	}
	refA, refB := a.RefA, a.RefB
	if refA == "" {
		refA = head
	}
	if refB == "" {
		// Default to the head's parent (the last commit's changes); if the head
		// is a root commit (no parent), fall back to the head itself so a
		// brand-new scope still produces an empty diff rather than an error.
		if parent != "" {
			refB = parent
		} else {
			refB = head
		}
	}
	diffs, err := sc.VCS.Diff(sc.RepoID, refA, refB)
	if err != nil {
		return "", err
	}
	return toJSON(diffs), nil
}

func (t *VCSTools) runRestore(ctx context.Context, argsJSON string) (string, error) {
	var a vcsRestoreArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	sc, err := vcsScopeFromCtx(ctx)
	if err != nil {
		return "", err
	}
	var destDir string
	if sc.WorktreeID != "" {
		destDir, err = sc.VCS.WorktreePath(sc.WorktreeID)
	} else {
		destDir, err = sc.VCS.RepoRoot(sc.RepoID)
	}
	if err != nil {
		return "", err
	}
	if err := sc.VCS.Restore(a.Ref, a.Path, destDir); err != nil {
		return "", err
	}
	return toJSON(map[string]string{"restored": a.Path}), nil
}

func (t *VCSTools) runMerge(ctx context.Context, argsJSON string) (string, error) {
	var a vcsMergeArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	sc, err := vcsScopeFromCtx(ctx)
	if err != nil {
		return "", err
	}
	merged, conflicts, mErr := sc.VCS.MergeToMain(a.Worktree, sc.Agent, a.Force)
	if mErr != nil && !errors.Is(mErr, vcs.ErrConflicts) {
		return "", mErr
	}
	// ErrConflicts is a normal result: the tool ran successfully and found
	// conflicts (merged is "" without force). Report them as JSON so the agent
	// can react, rather than surfacing a tool error.
	if conflicts == nil {
		conflicts = []string{}
	}
	return toJSON(map[string]any{
		"merged":    merged,
		"conflicts": conflicts,
	}), nil
}

// vcsRevertArgs are the args for the revert_turn tool.
type vcsRevertArgs struct {
	SeamID string `json:"seam_id"`
}

// runRevert is the revert_turn tool core. It:
//  1. Resolves the VCS scope + rejects worktree scope (RB1 is main-only).
//  2. Calls tools.RequireApproval — forced prompt, fail-closed on no-callback
//     (SSE) / Deny / timeout. 必修项 E.
//  3. Invokes VCS.RevertToSeam, which materializes the target tree + inserts
//     an undo seam (pointing at the PRE-revert head) atomically.
//  4. Returns the undo seam id as JSON (so the model / TUI can offer "undo").
//
// Worktree scope is rejected at the tool layer (not inside VCS) so the model
// gets a clear result message; VCS.MaterializeMain would also reject (the
// seam's commit is a main commit, not the worktree's head), but the tool-level
// check produces a friendlier message.
func (t *VCSTools) runRevert(ctx context.Context, argsJSON string) (string, error) {
	var a vcsRevertArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	sc, err := vcsScopeFromCtx(ctx)
	if err != nil {
		return "", err
	}
	if sc.WorktreeID != "" {
		return toJSON(map[string]string{
			"error": "revert_turn operates on main only; worktree commits are not rollback targets",
		}), nil
	}
	// Forced destructive approval. 必修项 E + D3: Force=true tells the WS
	// permission callback to SKIP interactive-mode auto-resolution (yolo /
	// allow-edits / auto would otherwise silently allow this destructive op).
	if err := RequireApproval(ctx, PermissionRequest{
		Tool:   "revert_turn",
		Args:   argsJSON,
		Reason: "revert main working copy + VCS head to a prior turn (agent path does not restore chat history)",
	}); err != nil {
		return toJSON(map[string]string{
			"error":  "revert_turn denied: " + err.Error(),
			"denied": "true",
		}), nil
	}
	// Agent path passes 0,0,nil: the agent tool does NOT own WS conversation
	// history, so it cannot truncate/restore it and stores no history_snapshot on
	// the returned seam (D4 scope cut: agent revert_turn is VCS-only).
	undoID, err := sc.VCS.RevertToSeam(sc.RepoID, a.SeamID, "agent:"+sc.Agent, 0, 0, nil)
	if err != nil {
		return "", err
	}
	return toJSON(map[string]string{
		"undo_seam_id": undoID,
		"hint":         "call revert_turn again with seam_id=" + undoID + " to undo this revert",
	}), nil
}
