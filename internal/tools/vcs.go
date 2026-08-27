package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

	// Preview / RestoreFiles are the two halves of the mandatory dry-run
	// protocol (V1 + V4). Preview is read-only and never prompts; RestoreFiles
	// writes and always does. They are split into two TOOLS rather than one
	// tool with a dry_run flag on purpose: a boolean argument is something a
	// model can forget, and forgetting it would default the destructive
	// direction. Two names make the read-only call unable to write at all.
	Preview      *GuardedTool
	RestoreFiles *GuardedTool
	// Timeline renders seams as "what you asked at that moment" (V3).
	Timeline *GuardedTool
	// Worktrees reports branch lifecycle and orphan status (V7).
	Worktrees *GuardedTool
	// GC prunes unreachable history (V2). Its dry-run form is the default.
	GC *GuardedTool
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
	t.registerRestoreTools()
	return t
}

// Tools returns all vcs tools.
func (t *VCSTools) Tools() []*GuardedTool {
	return []*GuardedTool{
		t.Commit, t.Log, t.Diff, t.Restore, t.Merge, t.Revert,
		t.Preview, t.RestoreFiles, t.Timeline, t.Worktrees, t.GC,
	}
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

// --- V1/V4/V3/V7/V2 tools ---
//
// These five share one design decision worth stating once: every destructive
// entry point here requires a token that only the corresponding READ-ONLY tool
// can produce. vcs_restore_files will not act without a confirm_token from
// vcs_preview_restore; vcs_gc will not delete without confirm=true after a
// dry-run report. The pattern is QwenPaw's mandatory `--dry-run` → `--confirm`
// for checkpoint restore and gc, and its value is that "I forgot to preview"
// is not a reachable state: the model cannot construct a valid token by
// guessing, so the preview is not advice, it is a precondition.

// registerRestoreTools builds the preview/restore/timeline/worktree/gc tools.
// Split out of NewVCSTools so that constructor stays readable rather than
// growing into one 200-line function.
func (t *VCSTools) registerRestoreTools() {
	t.Preview = NewGuardedTool(
		"vcs_preview_restore", "VCS Preview Restore",
		"Preview (read-only) what restoring main to a commit or seam would do: which files are created, overwritten or deleted, how many lines change, and which of them hold uncommitted work that would be lost. Returns a confirm_token required by vcs_restore_files. Never writes.",
		60*time.Second,
		params(map[string]*schema.ParameterInfo{
			"seam_id": {Type: schema.String, Desc: "seam id to preview reverting to (from vcs_timeline); mutually exclusive with commit"},
			"commit":  {Type: schema.String, Desc: "commit id to preview restoring to; mutually exclusive with seam_id"},
			"paths":   {Type: schema.String, Desc: "optional comma-separated repo-relative glob patterns; omit to preview the whole tree"},
		}),
		SyncStream(t.runPreviewRestore),
	)
	t.RestoreFiles = NewGuardedTool(
		"vcs_restore_files", "VCS Restore Files",
		"Apply a previewed restore. Requires the confirm_token from vcs_preview_restore, with identical seam_id/commit and paths. Destructive: overwrites and deletes working-copy files, and always prompts even in yolo/allow-edits mode.",
		120*time.Second,
		params(map[string]*schema.ParameterInfo{
			"seam_id":       {Type: schema.String, Desc: "same seam_id passed to vcs_preview_restore"},
			"commit":        {Type: schema.String, Desc: "same commit passed to vcs_preview_restore"},
			"paths":         {Type: schema.String, Desc: "same comma-separated glob patterns passed to vcs_preview_restore"},
			"confirm_token": {Type: schema.String, Desc: "confirm_token returned by vcs_preview_restore", Required: true},
		}),
		SyncStream(t.runRestoreFiles),
	)
	t.Timeline = NewGuardedTool(
		"vcs_timeline", "VCS Timeline",
		"List snapshot points as a readable timeline: for each one, the time, the first line of the user message that opened that turn, how many files it changed, and its seam/commit ids. Use this to find the seam_id to preview.",
		60*time.Second,
		params(map[string]*schema.ParameterInfo{
			"limit":          {Type: schema.Integer, Desc: "max entries, newest first (default 30)"},
			"include_revert": {Type: schema.Boolean, Desc: "also list rollback audit seams (default false)"},
		}),
		SyncStream(t.runTimeline),
	)
	t.Worktrees = NewGuardedTool(
		"vcs_worktrees", "VCS Worktrees",
		"List this repo's worktree branches with their lifecycle state (active/merged/abandoned/orphaned), owner PID and heartbeat, and flag the orphans whose owning process is gone and whose work never reached main.",
		60*time.Second,
		params(map[string]*schema.ParameterInfo{
			"orphans_only": {Type: schema.Boolean, Desc: "list only orphaned worktrees (default false)"},
		}),
		SyncStream(t.runWorktrees),
	)
	t.GC = NewGuardedTool(
		"vcs_gc", "VCS GC",
		"Prune unreachable VCS history and reclaim space. Defaults to a dry run that reports what WOULD be deleted; pass confirm=true to actually delete. Commits referenced by main, any worktree or any seam are never deleted.",
		300*time.Second,
		params(map[string]*schema.ParameterInfo{
			"keep_recent": {Type: schema.Integer, Desc: "retain at least this many newest commits (default 100)"},
			"keep_days":   {Type: schema.Integer, Desc: "retain commits newer than this many days (default 14); -1 disables the age floor, which is what a same-session goal loop needs since all of its commits are minutes old"},
			"confirm":     {Type: schema.Boolean, Desc: "actually delete; omit or false for a dry run"},
			"vacuum":      {Type: schema.Boolean, Desc: "rewrite the database afterwards so the file shrinks (slow; requires confirm)"},
		}),
		SyncStream(t.runGC),
	)
}

// vcsPreviewArgs are shared by vcs_preview_restore and vcs_restore_files: the
// two tools MUST take the same target so a token can pin one to the other.
type vcsPreviewArgs struct {
	SeamID       string `json:"seam_id"`
	Commit       string `json:"commit"`
	Paths        string `json:"paths"`
	ConfirmToken string `json:"confirm_token"`
}

type vcsTimelineArgs struct {
	Limit         int  `json:"limit"`
	IncludeRevert bool `json:"include_revert"`
}

type vcsWorktreesArgs struct {
	OrphansOnly bool `json:"orphans_only"`
}

type vcsGCArgs struct {
	KeepRecent int  `json:"keep_recent"`
	KeepDays   int  `json:"keep_days"`
	Confirm    bool `json:"confirm"`
	Vacuum     bool `json:"vacuum"`
}

// mainScopeFromCtx resolves the VCS scope and rejects a worktree scope.
//
// All five tools below are main-only for the same reason revert_turn is: the
// seams, the retention roots and the orphan ledger are all main-line concepts,
// and a worktree's own head is not a rollback target. Rejecting at the tool
// layer produces a message the model can act on, rather than a deeper error
// about a commit belonging to the wrong scope.
func mainScopeFromCtx(ctx context.Context) (VCSScope, string) {
	sc, err := vcsScopeFromCtx(ctx)
	if err != nil {
		return VCSScope{}, err.Error()
	}
	if sc.WorktreeID != "" {
		return VCSScope{}, "this tool operates on the main scope only; the active scope is worktree " + sc.WorktreeID
	}
	return sc, ""
}

// splitSelectors parses the comma-separated glob list. Blank entries are
// dropped so a trailing comma is not a selector that matches nothing (which
// PlanRestore would, correctly, reject).
func splitSelectors(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// resolveRestoreTarget turns the seam_id / commit pair into one commit id.
// Exactly one must be given: accepting both and preferring one would let a
// preview and an apply that disagree about which field matters silently target
// different commits.
func resolveRestoreTarget(sc VCSScope, a vcsPreviewArgs) (string, error) {
	switch {
	case a.SeamID != "" && a.Commit != "":
		return "", fmt.Errorf("pass either seam_id or commit, not both")
	case a.SeamID != "":
		seam, err := sc.VCS.FindSeam(a.SeamID)
		if err != nil {
			return "", fmt.Errorf("seam %s: %w", a.SeamID, err)
		}
		if seam.RepoID != sc.RepoID {
			return "", fmt.Errorf("seam %s belongs to another repository", a.SeamID)
		}
		return seam.CommitID, nil
	case a.Commit != "":
		return a.Commit, nil
	default:
		return "", fmt.Errorf("pass seam_id or commit")
	}
}

// planJSON renders a RestorePlan for the model. Line counts and the dirty list
// are included because they are the two things that decide whether a human
// should be asked before confirming.
func planJSON(plan *vcs.RestorePlan, applied bool) string {
	create, overwrite, del := plan.Counts()
	changes := make([]map[string]any, 0, len(plan.Changes))
	for _, c := range plan.Changes {
		changes = append(changes, map[string]any{
			"path":          c.Path,
			"op":            string(c.Op),
			"lines_before":  c.LinesBefore,
			"lines_after":   c.LinesAfter,
			"lines_added":   c.LinesAdded,
			"lines_removed": c.LinesRemoved,
			"approx":        c.Approx,
			"dirty":         c.Dirty,
		})
	}
	out := map[string]any{
		"applied":       applied,
		"from_commit":   plan.FromCommit,
		"target_commit": plan.TargetCommit,
		"selectors":     plan.Selectors,
		"create":        create,
		"overwrite":     overwrite,
		"delete":        del,
		"unchanged":     plan.Unchanged,
		"changes":       changes,
		"dirty_paths":   plan.DirtyPaths(),
		"confirm_token": plan.ConfirmToken,
	}
	if !applied {
		out["next_step"] = "call vcs_restore_files with the SAME seam_id/commit and paths plus this confirm_token"
		if len(plan.DirtyPaths()) > 0 {
			out["warning"] = "some listed paths hold uncommitted working-copy changes that this restore would destroy; no seam can bring them back"
		}
	}
	return toJSON(out)
}

func (t *VCSTools) runPreviewRestore(ctx context.Context, argsJSON string) (string, error) {
	var a vcsPreviewArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	sc, reject := mainScopeFromCtx(ctx)
	if reject != "" {
		return toJSON(map[string]string{"error": reject}), nil
	}
	target, err := resolveRestoreTarget(sc, a)
	if err != nil {
		return toJSON(map[string]string{"error": err.Error()}), nil
	}
	plan, err := sc.VCS.PlanRestore(sc.RepoID, target, splitSelectors(a.Paths))
	if err != nil {
		return "", err
	}
	return planJSON(plan, false), nil
}

func (t *VCSTools) runRestoreFiles(ctx context.Context, argsJSON string) (string, error) {
	var a vcsPreviewArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	if a.ConfirmToken == "" {
		return toJSON(map[string]string{
			"error": "confirm_token is required; run vcs_preview_restore first",
		}), nil
	}
	sc, reject := mainScopeFromCtx(ctx)
	if reject != "" {
		return toJSON(map[string]string{"error": reject}), nil
	}
	target, err := resolveRestoreTarget(sc, a)
	if err != nil {
		return toJSON(map[string]string{"error": err.Error()}), nil
	}
	// Forced destructive approval, exactly like revert_turn: Force=true tells
	// the WS callback to skip interactive-mode auto-resolution, so yolo /
	// allow-edits / auto cannot silently overwrite a working copy. The confirm
	// token proves the MODEL previewed; this proves the HUMAN agreed. Neither
	// substitutes for the other.
	if err := RequireApproval(ctx, PermissionRequest{
		Tool:   "vcs_restore_files",
		Args:   argsJSON,
		Reason: "overwrite working-copy files from VCS history (previewed restore)",
	}); err != nil {
		return toJSON(map[string]string{
			"error":  "vcs_restore_files denied: " + err.Error(),
			"denied": "true",
		}), nil
	}
	plan, err := sc.VCS.ApplyRestore(sc.RepoID, target, splitSelectors(a.Paths), a.ConfirmToken)
	if err != nil {
		// A stale plan and an external mutation are both "look again", not
		// tool failures: the model should re-preview rather than see a Go error
		// it cannot interpret. Everything else is a genuine error.
		if errors.Is(err, vcs.ErrPlanStale) || errors.Is(err, vcs.ErrExternalMutation) {
			return toJSON(map[string]string{
				"error":     err.Error(),
				"next_step": "run vcs_preview_restore again; the repository or working copy changed since the preview",
			}), nil
		}
		return "", err
	}
	return planJSON(plan, true), nil
}

func (t *VCSTools) runTimeline(ctx context.Context, argsJSON string) (string, error) {
	var a vcsTimelineArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	sc, reject := mainScopeFromCtx(ctx)
	if reject != "" {
		return toJSON(map[string]string{"error": reject}), nil
	}
	// SessionID is left empty on purpose: VCSScope carries no session id, and
	// the agent has no way to name one. The repo-wide listing is not a
	// degradation — vcs.Timeline resolves each seam's question through that
	// seam's OWN session, so a cross-session list is still correctly labelled;
	// the entries just span more than one conversation. The per-entry
	// session id is not surfaced because it is an internal handle the model
	// cannot use for anything.
	entries, err := sc.VCS.Timeline(sc.RepoID, vcs.TimelineOptions{
		Limit:              a.Limit,
		IncludeRevertSeams: a.IncludeRevert,
	})
	if err != nil {
		return "", err
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		question := e.Question
		if e.QuestionTruncated {
			question += "…"
		}
		out = append(out, map[string]any{
			"seam_id":       e.SeamID,
			"kind":          string(e.Kind),
			"commit":        e.CommitID,
			"turn":          e.TurnSeq,
			"question":      question,
			"files_changed": e.FilesChanged,
			"created_at":    e.CreatedAt,
			"is_head":       e.IsHead,
		})
	}
	return toJSON(map[string]any{"timeline": out}), nil
}

func (t *VCSTools) runWorktrees(ctx context.Context, argsJSON string) (string, error) {
	var a vcsWorktreesArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	sc, reject := mainScopeFromCtx(ctx)
	if reject != "" {
		return toJSON(map[string]string{"error": reject}), nil
	}
	orphans, err := sc.VCS.ScanOrphanWorktrees(sc.RepoID)
	if err != nil {
		return "", err
	}
	orphanIDs := map[string]bool{}
	for _, o := range orphans {
		orphanIDs[o.ID] = true
	}
	states := orphans
	if !a.OrphansOnly {
		states, err = sc.VCS.ListWorktreeStates(sc.RepoID)
		if err != nil {
			return "", err
		}
	}
	out := make([]map[string]any, 0, len(states))
	for _, s := range states {
		out = append(out, map[string]any{
			"worktree_id":  s.ID,
			"lifecycle":    string(s.Lifecycle),
			"owner_pid":    s.OwnerPID,
			"heartbeat_at": s.HeartbeatAt,
			"base_commit":  s.BaseCommit,
			"tip":          s.Tip,
			"dir_present":  s.Active,
			"orphaned":     orphanIDs[s.ID],
		})
	}
	return toJSON(map[string]any{
		"worktrees":    out,
		"orphan_count": len(orphans),
	}), nil
}

func (t *VCSTools) runGC(ctx context.Context, argsJSON string) (string, error) {
	var a vcsGCArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	sc, reject := mainScopeFromCtx(ctx)
	if reject != "" {
		return toJSON(map[string]string{"error": reject}), nil
	}
	if a.Confirm {
		// Deleting history is destructive in the same way a rollback is, so it
		// takes the same forced prompt. The dry run below does not: it writes
		// nothing, and making the safe half cost a dialog would train the model
		// to skip straight to the destructive one.
		if err := RequireApproval(ctx, PermissionRequest{
			Tool:   "vcs_gc",
			Args:   argsJSON,
			Reason: "permanently delete unreachable VCS commits and blobs",
		}); err != nil {
			return toJSON(map[string]string{
				"error":  "vcs_gc denied: " + err.Error(),
				"denied": "true",
			}), nil
		}
	}
	res, err := sc.VCS.RunGC(sc.RepoID, vcs.GCOptions{
		KeepRecent: a.KeepRecent,
		KeepDays:   a.KeepDays,
		DryRun:     !a.Confirm,
		Vacuum:     a.Vacuum && a.Confirm,
	})
	if err != nil {
		return "", err
	}
	out := map[string]any{
		"dry_run":                   res.DryRun,
		"deleted_commits":           len(res.DeletedCommits),
		"deleted_blobs":             len(res.DeletedBlobs),
		"kept_commits":              res.KeptCommits,
		"protected_by_reachability": res.ProtectedByReachability,
		"freed_bytes":               res.FreedBytes,
		"vacuumed":                  res.Vacuumed,
	}
	if res.DryRun {
		out["next_step"] = "call vcs_gc again with confirm=true to delete (add vacuum=true to shrink the database file)"
	}
	return toJSON(out), nil
}
