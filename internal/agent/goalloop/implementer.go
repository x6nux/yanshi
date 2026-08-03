package goalloop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/x6nux/yanshi/internal/acp"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/vcs"
)

// pathToGlob converts a directory path to a glob pattern matching everything
// under it (using forward slashes for the guard's glob matcher).
func pathToGlob(dir string) string {
	return strings.ReplaceAll(filepath.Join(dir, "**"), "\\", "/")
}

// firstNonEmpty returns the first non-empty argument, or "" if all are empty.
// Used to prefer an explicit agent name (e.g. from the ACP recorder callback)
// while falling back to the worker's configured agent.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// FakeImplementer — returns a scripted result after N failures (for tests).
// ---------------------------------------------------------------------------

// FakeImplementer simulates an implementer that fails the first FailFirst
// calls, then returns Result on subsequent calls. The zero value succeeds
// immediately (FailFirst=0) with an empty Result.
type FakeImplementer struct {
	Result    string // value returned on success
	FailFirst int    // number of calls that should fail before success
	mu        sync.Mutex
	calls     int
}

// Implement increments the call counter. If calls <= FailFirst it returns
// an error (simulating a failed attempt); otherwise it returns Result.
func (f *FakeImplementer) Implement(_ context.Context, _ Plan, _ string) (string, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()

	if n <= f.FailFirst {
		return "", fmt.Errorf("fake implementer: deliberate failure %d/%d", n, f.FailFirst)
	}
	return f.Result, nil
}

// Calls returns the number of times Implement has been invoked.
func (f *FakeImplementer) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// ---------------------------------------------------------------------------
// WorktreeHelper — manages git worktrees for isolated worker execution.
// ---------------------------------------------------------------------------

// WorktreeHelper creates, lists, and removes git worktrees under a temp dir.
// RepoDir is the root repository directory (where .git lives).
type WorktreeHelper struct {
	RepoDir string
}

// Add creates a new git worktree with a branch derived from name.
// The worktree path is placed under the OS temp dir. It returns the absolute
// path to the new worktree.
func (h *WorktreeHelper) Add(name string) (string, error) {
	// Reserve a unique path under the temp dir, then remove the empty dir
	// so git worktree add can create it fresh.
	tmpDir, err := os.MkdirTemp("", "goalloop-wt-*")
	if err != nil {
		return "", fmt.Errorf("worktree: temp dir: %w", err)
	}
	if err := os.Remove(tmpDir); err != nil {
		return "", fmt.Errorf("worktree: remove temp dir: %w", err)
	}

	branch := "goalloop/" + name
	cmd := exec.Command("git", "-C", h.RepoDir, "worktree", "add", "-b", branch, tmpDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("worktree: git worktree add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Normalize symlinks (macOS /tmp → /private/tmp) so the returned path
	// matches what git worktree list --porcelain reports later.
	if resolved, err := filepath.EvalSymlinks(tmpDir); err == nil {
		tmpDir = resolved
	}
	return tmpDir, nil
}

// Remove removes a git worktree at the given path.
func (h *WorktreeHelper) Remove(path string) error {
	cmd := exec.Command("git", "-C", h.RepoDir, "worktree", "remove", "--force", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("worktree: git worktree remove: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// List returns the paths of all git worktrees for the repository.
func (h *WorktreeHelper) List() ([]string, error) {
	cmd := exec.Command("git", "-C", h.RepoDir, "worktree", "list", "--porcelain")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("worktree: git worktree list: %w: %s", err, strings.TrimSpace(string(out)))
	}

	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "worktree ") {
			p := strings.TrimPrefix(line, "worktree ")
			// Normalise to OS-native separators so paths match Add() output.
			paths = append(paths, filepath.FromSlash(p))
		}
	}
	return paths, nil
}

// ---------------------------------------------------------------------------
// ACPImplementer — Lead/Worker/Integrator structure (skeleton).
// ---------------------------------------------------------------------------

// ACPImplementer implements the Implementer interface by orchestrating a team
// of ACP agents: a Lead decomposes the plan into per-step subtasks, Workers
// execute each step in an isolated git worktree, and an Integrator merges the
// worktrees back into the main working directory.
//
// For M6, the structure is in place but real multi-worker ACP execution is
// exercised only in flagged E2E tests (requires real CLI agents on PATH).
type ACPImplementer struct {
	Agent      string // "claudecode" | "opencode" | "codex"
	Profile    guard.PermissionProfile
	profileSet bool // true when WithProfile was called

	// VCS, when non-nil with RepoID, makes the worker use autoVCS worktrees
	// (branch from main, run the agent bound to the worktree, merge on
	// completion) instead of the git-worktree path. When nil/empty the worker
	// falls back to WorktreeHelper (keeps the fake-model goal demo working).
	VCS    *vcs.VCS
	RepoID string // required when VCS is set

	// VCSDBPath and WorktreeDir are the SQLite db path and the worktree working
	// dir used to describe the yanshi-vcs MCP stdio server to the spawned ACP
	// agent (vcsSpawnOpts builds MCPCommand from them). Required when VCS is set
	// so the agent's vcs_* MCP tools can locate the store and the worktree dir.
	VCSDBPath   string
	WorktreeDir string

	// Sink is the shared token accumulator (goalloop.UsageSink). When non-nil,
	// each ACP subprocess usage_report event is added to this sink so the goal
	// loop's budget and cost tracking reflect subprocess token consumption
	// (LEAK3). nil leaves subprocess usage uncounted (preserves the pre-F2 path
	// for callers that don't pass a sink).
	Sink *UsageSink
}

// WithProfile sets the permission profile used to gate spawned ACP agents.
// If not called, Implement constructs a worktree-scoped default profile.
func (a *ACPImplementer) WithProfile(p guard.PermissionProfile) *ACPImplementer {
	a.Profile = p
	a.profileSet = true
	return a
}

// WithVCS enables the autoVCS worker path: each step branches a worktree from
// main, runs the agent bound to it, and merges the result back into main on
// completion. repoID must be non-empty (the id returned by vcs.InitRepo).
// dbPath is the SQLite store path (cfg.Storage.SQLitePath) and worktreeDir is
// the expanded worktree working dir (cfg.VCS.WorktreeDir); both are passed to
// the spawned ACP agent so its yanshi-vcs MCP server can locate the store and
// the worktree dir. Returns a for chaining (mirrors WithProfile).
func (a *ACPImplementer) WithVCS(v *vcs.VCS, repoID, dbPath, worktreeDir string) *ACPImplementer {
	a.VCS = v
	a.RepoID = repoID
	a.VCSDBPath = dbPath
	a.WorktreeDir = worktreeDir
	return a
}

// worktreeScopedProfile returns a fail-closed profile scoped to the given
// worktree path: the agent can only read/write within the worktree, use
// fs/shell tools, run common dev commands, and has no network access.
func worktreeScopedProfile(wt string) guard.PermissionProfile {
	wtGlob := pathToGlob(wt)
	return guard.PermissionProfile{
		FS:    guard.FSPerm{Read: []string{wtGlob}, Write: []string{wtGlob}},
		Tools: guard.ToolsPerm{Allow: []string{"fs.*", "shell.*"}},
		Shell: guard.ShellPerm{
			Policy:   "allowlist",
			Patterns: []string{"go *", "git *", "npm *", "python *"},
		},
		Net: guard.NetPerm{Allow: false},
	}
}

// workerTask represents a single step's implementation task.
type workerTask struct {
	Step  string // the implementation step text
	Index int    // step index (0-based)
}

// lead decomposes a Plan into worker tasks.
type lead struct {
	agent string
}

// decompose splits Plan.Steps into one workerTask per step (naive: 1 worker per step).
func (l *lead) decompose(p Plan) []workerTask {
	tasks := make([]workerTask, len(p.Steps))
	for i, step := range p.Steps {
		tasks[i] = workerTask{Step: step, Index: i}
	}
	return tasks
}

// worker executes a single step in an isolated worktree using an ACP agent.
type worker struct {
	agent      string
	wt         *WorktreeHelper
	profile    guard.PermissionProfile
	profileSet bool // true when the profile was explicitly set via WithProfile

	// When vcs is non-nil and repoID non-empty, run dispatches to the autoVCS
	// path (worktree branched from main + merge on completion); otherwise the
	// git WorktreeHelper path is used.
	vcs    *vcs.VCS
	repoID string

	// vcsDBPath and worktreeDir describe the yanshi-vcs MCP stdio server to
	// the spawned ACP agent (vcsSpawnOpts builds MCPCommand from them). Used
	// only on the autoVCS path.
	vcsDBPath   string
	worktreeDir string

	// sink receives subprocess token usage from ACP usage_report events (LEAK3).
	// nil leaves subprocess usage uncounted (preserves callers without a sink).
	sink *UsageSink
}

// resolveProfile returns the profile to use for a worktree at wtPath. If
// profileSet is true the explicitly-set profile is used as-is; otherwise a
// worktree-scoped default is constructed for wtPath.
func resolveProfile(profile guard.PermissionProfile, profileSet bool, wtPath string) guard.PermissionProfile {
	if profileSet {
		return profile
	}
	return worktreeScopedProfile(wtPath)
}

// spawnOpts builds the SpawnOptions for this worker, including the Policy
// derived from the given permission profile (scoped to cwd when default).
func (w *worker) spawnOpts(cwd string, profile guard.PermissionProfile) acp.SpawnOptions {
	return acp.SpawnOptions{
		Agent:  w.agent,
		Cwd:    cwd,
		Policy: acp.NewGuardPolicy(profile),
	}
}

// vcsSpawnOpts builds SpawnOptions for an autoVCS worktree run: the base
// spawnOpts plus WorktreeID, a Recorder that routes the agent's diff edits
// into the worktree's pending changeset via RecordEditWorktree, and an
// MCPCommand that delivers the yanshi-vcs MCP server (the vcs_* tools) to the
// spawned ACP agent as a stdio subprocess. Spawn reads these fields and binds
// them on the Client (SetVCSTracking) + session/new mcpServers, so the returned
// options are self-contained — callers need not also call SetVCSTracking.
func (w *worker) vcsSpawnOpts(cwd string, profile guard.PermissionProfile, wtID string) acp.SpawnOptions {
	opts := w.spawnOpts(cwd, profile)
	opts.WorktreeID = wtID
	opts.Recorder = func(worktreeID, agent, absPath string, content []byte) error {
		return w.vcs.RecordEditWorktree(worktreeID, firstNonEmpty(agent, w.agent), absPath, content)
	}
	// Describe the yanshi-vcs MCP stdio server so the ACP agent receives the
	// vcs_* tools. os.Args[0] is the yanshi binary; "vcs-mcp" is the
	// subcommand that runs the MCP server. The env tells the server which store,
	// repo, worktree, agent, and worktree dir to bind to.
	opts.MCPCommand = map[string]any{
		"command": os.Args[0],
		"args":    []string{"vcs-mcp"},
		"env": map[string]string{
			"YANSHI_DB_PATH":      w.vcsDBPath,
			"YANSHI_REPO_ID":      w.repoID,
			"YANSHI_WT_ID":        wtID,
			"YANSHI_AGENT":        w.agent,
			"YANSHI_WORKTREE_DIR": w.worktreeDir,
		},
	}
	return opts
}

// run dispatches a step to the autoVCS path (when a VCS + repo are configured)
// or the git-worktree fallback. The branch keeps the fake-model goal demo
// (which has no VCS) on the original WorktreeHelper path.
func (w *worker) run(ctx context.Context, task workerTask) (string, error) {
	if w.vcs != nil && w.repoID != "" {
		return w.runWithAutoVCS(ctx, task)
	}
	return w.runWithGit(ctx, task)
}

// runWithGit spawns an ACP agent in a git worktree, prompts it with the step,
// and returns a result summary. The worktree is removed after the agent
// finishes. This is the original path used when no autoVCS is configured.
func (w *worker) runWithGit(ctx context.Context, task workerTask) (string, error) {
	wtName := fmt.Sprintf("step-%d", task.Index)
	wtPath, err := w.wt.Add(wtName)
	if err != nil {
		return "", fmt.Errorf("worker %d: worktree add: %w", task.Index, err)
	}
	defer w.wt.Remove(wtPath)

	profile := resolveProfile(w.profile, w.profileSet, wtPath)
	spawned, err := acp.Spawn(ctx, w.spawnOpts(wtPath, profile))
	if err != nil {
		return "", fmt.Errorf("worker %d: spawn %q: %w", task.Index, w.agent, err)
	}
	// Cleanup: close the client (closes stdin pipe -> agent sees EOF), kill
	// the process (in case it doesn't exit on EOF), and Wait to reap it.
	// Order matters: Close must happen before Kill/Wait so the agent can
	// exit gracefully; Kill ensures exit even if the agent ignores EOF;
	// Wait reaps the process to avoid zombies.
	defer func() {
		spawned.Client.Close()
		spawned.Cmd.Process.Kill()
		spawned.Cmd.Wait()
	}()

	stopReason, err := spawned.Client.Prompt(ctx, spawned.SessionID, task.Step, w.usageForwarder())
	if err != nil {
		return "", fmt.Errorf("worker %d: prompt: %w", task.Index, err)
	}
	return fmt.Sprintf("step %d done (stop=%s)", task.Index, stopReason), nil
}

// runWithAutoVCS branches a fresh autoVCS worktree from main, spawns the ACP
// agent bound to it (so its diff edits auto-track into the worktree's pending
// changeset), prompts it with the step, then commits and merges the worktree
// back into main. The worktree is marked inactive on exit (history retained).
func (w *worker) runWithAutoVCS(ctx context.Context, task workerTask) (string, error) {
	wt, err := w.vcs.AddWorktree(w.repoID, []string{w.agent})
	if err != nil {
		return "", fmt.Errorf("worker %d: autovcs addworktree: %w", task.Index, err)
	}
	defer w.vcs.RemoveWorktree(wt.ID) // mark inactive on every exit path

	profile := resolveProfile(w.profile, w.profileSet, wt.Path)
	spawned, err := acp.Spawn(ctx, w.vcsSpawnOpts(wt.Path, profile, wt.ID))
	if err != nil {
		return "", fmt.Errorf("worker %d: spawn %q: %w", task.Index, w.agent, err)
	}
	defer func() {
		spawned.Client.Close()
		spawned.Cmd.Process.Kill()
		spawned.Cmd.Wait()
	}()

	if _, err := spawned.Client.Prompt(ctx, spawned.SessionID, task.Step, w.usageForwarder()); err != nil {
		return "", fmt.Errorf("worker %d: prompt: %w", task.Index, err)
	}
	return w.commitAndMerge(wt.ID, task)
}

// usageForwarder returns a Prompt onEvent callback that forwards ACP
// usage_report events into the worker's shared UsageSink (LEAK3). Returns nil
// when no sink is configured, preserving the pre-F2 behavior of not installing
// an event handler. The mapping is input→prompt, output→completion, total→total.
func (w *worker) usageForwarder() func(acp.Event) {
	if w.sink == nil {
		return nil
	}
	return func(ev acp.Event) {
		if ev.Usage == nil {
			return
		}
		w.sink.Add(Usage{
			PromptTokens:     ev.Usage.InputTokens,
			CompletionTokens: ev.Usage.OutputTokens,
			TotalTokens:      ev.Usage.TotalTokens,
		})
	}
}

// commitAndMerge folds any pending worktree edits into a worktree commit and
// then merges the worktree into main (force=false; conflicts are reported, not
// force-resolved). It is extracted from runWithAutoVCS so the commit+merge
// sequence can be exercised directly without spawning an ACP agent.
//
// A commit error is treated as best-effort ONLY for vcs.ErrNoChanges (the
// worktree had no pending edits — an agent that changed nothing); genuine store
// failures are surfaced. The merge proceeds regardless: MergeToMain integrates
// whatever is on the worktree tip, which is the base commit (a no-op empty
// merge) when nothing was edited.
func (w *worker) commitAndMerge(wtID string, task workerTask) (string, error) {
	if _, cErr := w.vcs.CommitWorktree(wtID, w.agent, fmt.Sprintf("worker step %d", task.Index)); cErr != nil {
		// ErrNoChanges is expected when the agent made no edits — swallow it and
		// let the merge run (a no-op empty merge). Any other error is a real
		// store failure; surface it rather than masking it as "no changes".
		if !errors.Is(cErr, vcs.ErrNoChanges) {
			return "", fmt.Errorf("worker %d: commit: %w", task.Index, cErr)
		}
	}
	_, conflicts, mergeErr := w.vcs.MergeToMain(wtID, w.agent, false)
	if mergeErr != nil && !errors.Is(mergeErr, vcs.ErrConflicts) {
		return "", fmt.Errorf("worker %d: merge: %w", task.Index, mergeErr)
	}
	if len(conflicts) > 0 {
		return fmt.Sprintf("step %d merged with conflicts: %s", task.Index, strings.Join(conflicts, ", ")), nil
	}
	return fmt.Sprintf("step %d done (merged to main)", task.Index), nil
}

// integrator merges worker worktrees back into the main working directory.
// For M6 this is a placeholder: the real merge logic (git merge, conflict
// resolution, acceptance test run) will be implemented when the E2E path is
// activated.
type integrator struct {
	repoDir string
}

// merge merges a worktree's branch back into the current branch of repoDir.
// For M6 this is a no-op placeholder that logs the intent.
func (in *integrator) merge(_ context.Context, _ string) error {
	// TODO(M7): implement git merge of worker branches into repoDir.
	return nil
}

// Implement orchestrates the Lead -> Worker -> Integrator pipeline.
func (a *ACPImplementer) Implement(ctx context.Context, p Plan, workdir string) (string, error) {
	l := &lead{agent: a.Agent}
	wt := &WorktreeHelper{RepoDir: workdir}

	// Lead: decompose the plan into per-step tasks.
	tasks := l.decompose(p)

	// Workers: execute each step in its own worktree.
	var results []string
	for _, task := range tasks {
		w := &worker{
			agent:       a.Agent,
			wt:          wt,
			profile:     a.Profile,
			profileSet:  a.profileSet,
			vcs:         a.VCS,
			repoID:      a.RepoID,
			vcsDBPath:   a.VCSDBPath,
			worktreeDir: a.WorktreeDir,
			sink:        a.Sink,
		}
		result, err := w.run(ctx, task)
		if err != nil {
			return "", fmt.Errorf("acp implementer: %w", err)
		}
		results = append(results, result)
	}

	// Integrator: merge worktrees back (placeholder for M6).
	in := &integrator{repoDir: workdir}
	if err := in.merge(ctx, ""); err != nil {
		return "", fmt.Errorf("acp implementer: integrate: %w", err)
	}

	return strings.Join(results, "; "), nil
}
