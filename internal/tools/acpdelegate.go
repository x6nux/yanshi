package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/acp"
	"github.com/x6nux/yanshi/internal/vcs"
)

// ACPDelegateTimeout is the ceiling on one delegation, and the GuardedTool
// timeout the tool is registered with.
//
// It is generous because the whole point of delegating is to hand off work
// that is too long for the local ReAct loop, and it is BOUNDED because the
// alternative — NoTimeout — means an external CLI that wedges on a prompt it
// will never receive holds the turn until the user notices and interrupts.
// The per-call timeout_seconds argument narrows it; nothing widens it.
const ACPDelegateTimeout = 20 * time.Minute

// ACPDepthEnv is the environment variable carrying the delegation nesting
// depth into a spawned agent.
//
// It has to be an environment variable rather than a context value because the
// nesting it guards against crosses a PROCESS boundary: an ACP agent that runs
// `yanshi exec` (or receives a yanshi MCP server) starts a fresh process whose
// context is empty, so a purely in-process depth counter reads zero there and
// the recursion is unbounded. The variable survives the exec, which is exactly
// the property needed.
//
// It is not a security control. A child can unset it. It is a loop guard
// against the ordinary accident — an agent instructed to "delegate the hard
// parts" delegating in a circle — and treats a malformed value as depth 0
// rather than refusing, because an operator's stray export must not make every
// delegation fail.
const ACPDepthEnv = "YANSHI_ACP_DEPTH"

// ACPDelegateConfig carries the process-wide facts a delegation needs and the
// turn context cannot supply: where the SQLite store lives and where autoVCS
// keeps its worktrees. Both are needed to describe the yanshi-vcs MCP server
// to the spawned agent.
//
// Binary defaults to os.Args[0] when empty — the yanshi executable the ACP
// adapter re-spawns as `yanshi vcs-mcp`.
type ACPDelegateConfig struct {
	VCSDBPath   string
	WorktreeDir string
	Binary      string
}

// acpDelegateArgs is the model-facing argument shape.
type acpDelegateArgs struct {
	Agent          string `json:"agent"`
	Task           string `json:"task"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// acpDelegateResult is the JSON handed back to the model.
type acpDelegateResult struct {
	Agent      string `json:"agent"`
	StopReason string `json:"stop_reason"`
	// Worktree is the autoVCS worktree id the agent ran in, empty when VCS is
	// not configured and the agent ran directly in the work root.
	Worktree string `json:"worktree,omitempty"`
	// Merged reports whether the worktree was folded back into main.
	Merged bool `json:"merged,omitempty"`
	// Conflicts names the paths that could not be merged cleanly. A
	// non-empty list means main is unchanged for those paths and a human has
	// to look; the delegation itself still succeeded.
	Conflicts []string `json:"conflicts,omitempty"`
	// Transcript is the agent's own message output, so the model can read what
	// it actually did instead of only whether it stopped.
	Transcript string `json:"transcript,omitempty"`
	Note       string `json:"note,omitempty"`
}

// NewACPDelegateTool builds acp_delegate: it hands one self-contained subtask
// to an external agent CLI (codex / claudecode / opencode) over the Agent
// Client Protocol and returns what that agent did.
//
// Why this is a tool at all: internal/acp has always been able to drive these
// CLIs, but only the goal loop could reach it, so a model that knew a subtask
// was better suited to another agent had no way to say so. The protocol layer
// was complete and had exactly one caller.
//
// Isolation: when autoVCS is configured for the turn, the agent runs in a fresh
// worktree branched from main, its edits auto-track into that worktree's
// changeset, and the worktree is committed and merged back when it finishes.
// Nothing the agent does touches main until the merge, so a delegation that
// goes wrong is a worktree to discard rather than a working copy to repair.
// Without autoVCS it runs directly in the work root, which is the honest
// degradation — the alternative is refusing to delegate at all in a
// configuration where the local tools happily edit the same files.
//
// The tool is registered with NewGuardedTool rather than
// NewApprovalGuardedTool: the per-call approval decision belongs to the
// PROFILE, and the shipped default deliberately omits acp_delegate so the
// operator is prompted every time. Baking mandatory approval into the
// constructor would additionally make it fail closed on SSE forever, which is
// a different (and unasked-for) policy.
func NewACPDelegateTool(cfg ACPDelegateConfig) *GuardedTool {
	return NewGuardedTool(
		"acp_delegate", "Delegate to agent",
		"Hand one self-contained subtask to an external coding agent CLI (codex, claudecode, opencode) and wait for it to finish. "+
			"Use this when a task is large, long-running, or better suited to another agent's strengths; do NOT use it for work you can do "+
			"directly with fs_*/shell_run, which is faster and cheaper. The agent runs in an isolated VCS worktree that is merged back on "+
			"completion, so its edits cannot corrupt the working copy mid-flight. Describe the task completely: the agent gets the task text "+
			"and the project directory, and cannot ask you follow-up questions.",
		ACPDelegateTimeout,
		params(map[string]*schema.ParameterInfo{
			"agent": {
				Type: schema.String,
				Desc: "which agent CLI to use: " + strings.Join(acp.AgentNames(), ", "),
			},
			"task": {
				Type: schema.String,
				Desc: "the complete, self-contained task description handed to the agent. It sees only this text and the project files.",
			},
			"timeout_seconds": {
				Type: schema.Integer,
				Desc: fmt.Sprintf("give up after this many seconds (default and maximum %d)", int(ACPDelegateTimeout.Seconds())),
			},
		}),
		SyncStream(func(ctx context.Context, argsJSON string) (string, error) {
			return runACPDelegate(ctx, cfg, argsJSON)
		}),
	)
}

// acpLookPath is the PATH probe seam. Production is exec.LookPath; tests
// replace it so the delegation logic is reachable on a machine that does not
// have codex or npx installed — which is every CI runner, and would otherwise
// make the entire happy path untestable.
var acpLookPath = exec.LookPath

// runACPDelegate validates the request, refuses the cases that cannot work,
// and otherwise runs one delegation to completion.
//
// Every refusal here returns errorResult (a tool RESULT), not a Go error: the
// model can act on "codex is not installed, try claudecode" but a Go error
// aborts the whole turn. Genuine infrastructure failures still return errors.
func runACPDelegate(ctx context.Context, cfg ACPDelegateConfig, argsJSON string) (string, error) {
	var a acpDelegateArgs
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "", fmt.Errorf("acp_delegate: invalid arguments: %w", err)
	}
	a.Agent = strings.TrimSpace(a.Agent)
	a.Task = strings.TrimSpace(a.Task)
	if a.Task == "" {
		return errorResult("acp_delegate: task is required and must describe the whole subtask"), nil
	}
	if a.Agent == "" {
		return errorResult("acp_delegate: agent is required; choose one of: " +
			strings.Join(acp.AgentNames(), ", ")), nil
	}
	argv, err := acp.LaunchSpec(a.Agent)
	if err != nil {
		return errorResult(fmt.Sprintf("acp_delegate: unknown agent %q; available agents are: %s",
			a.Agent, strings.Join(acp.AgentNames(), ", "))), nil
	}
	if _, err := acpLookPath(argv[0]); err != nil {
		return errorResult(fmt.Sprintf(
			"acp_delegate: cannot run %q — the launcher %q is not on PATH, so this agent is unavailable on this machine. "+
				"Either pick a different agent (%s) or do the work yourself with the local tools.",
			a.Agent, argv[0], strings.Join(acp.AgentNames(), ", "))), nil
		// NOTE: this is a usability probe, not an authorization check. The
		// real gate is secproc.Launch below; a CLI that appears on PATH
		// between this line and the spawn is still subject to it.
	}
	if depth, over := acpDepthExceeded(ctx); over {
		return errorResult(fmt.Sprintf(
			"acp_delegate: refusing to delegate at nesting depth %d (limit %d). "+
				"This call is already running inside a delegated agent; do the work directly instead of nesting further.",
			depth, MaxSubAgentDepth)), nil
	}

	timeout := ACPDelegateTimeout
	if a.TimeoutSeconds > 0 {
		if requested := time.Duration(a.TimeoutSeconds) * time.Second; requested < timeout {
			timeout = requested
		}
	}
	// The tool's own GuardedTool deadline already bounds this, but only at the
	// same ceiling; a narrower per-call timeout has to be applied here. The
	// derived context is what makes cancellation reach the child, because
	// secproc's exec.CommandContext kills the process group when it fires.
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return delegateToAgent(runCtx, cfg, a)
}

// acpDepthExceeded reports the current delegation depth and whether a further
// delegation would exceed the limit.
//
// It takes the MAXIMUM of the two counters rather than one or the other. The
// in-process sub-agent depth catches agent_start -> acp_delegate, and the
// environment variable catches acp_delegate -> (agent runs yanshi) ->
// acp_delegate; a chain can mix both, and reading only one lets the mixed
// chain past a limit each half respects on its own.
func acpDepthExceeded(ctx context.Context) (int, bool) {
	depth := SubAgentDepth(ctx)
	if env := strings.TrimSpace(os.Getenv(ACPDepthEnv)); env != "" {
		// A malformed value is treated as 0: an operator's stray export must
		// not disable delegation, and this is a loop guard rather than a
		// security boundary.
		if n, err := strconv.Atoi(env); err == nil && n > depth {
			depth = n
		}
	}
	return depth, depth >= MaxSubAgentDepth
}

// delegateToAgent picks the isolated-worktree path when autoVCS is configured
// for this turn and the direct path otherwise, then runs one prompt.
func delegateToAgent(ctx context.Context, cfg ACPDelegateConfig, a acpDelegateArgs) (string, error) {
	scope, hasScope := VCSScopeFromContext(ctx)
	if hasScope && scope.VCS != nil && scope.RepoID != "" {
		return delegateInWorktree(ctx, cfg, a, scope)
	}
	root := WorkRootFromContext(ctx)
	if root == "" {
		return errorResult("acp_delegate: no work root bound to this turn; refusing to run an agent " +
			"in an unknown directory"), nil
	}
	res, err := runDelegation(ctx, a, root, delegateBinding{})
	if err != nil {
		return "", err
	}
	res.Note = "autoVCS is not configured for this turn, so the agent edited the working copy directly " +
		"(no worktree isolation, no merge step)."
	return marshalDelegateResult(res)
}

// delegateInWorktree branches a worktree from main, runs the agent bound to it,
// then commits and merges the result back.
//
// The worktree is marked inactive on EVERY exit path, including the ones where
// the agent failed: leaving it active would make the next orphan scan report a
// worktree whose process is long gone, which is the state that scan exists to
// find.
func delegateInWorktree(ctx context.Context, cfg ACPDelegateConfig, a acpDelegateArgs, scope VCSScope) (string, error) {
	wt, err := scope.VCS.AddWorktree(scope.RepoID, []string{a.Agent})
	if err != nil {
		return "", fmt.Errorf("acp_delegate: create worktree: %w", err)
	}
	defer scope.VCS.RemoveWorktree(wt.ID)

	recorder := func(worktreeID, agent, absPath string, content []byte) error {
		author := agent
		if author == "" {
			author = a.Agent
		}
		return scope.VCS.RecordEditWorktree(worktreeID, author, absPath, content)
	}
	mcpCmd := acp.VCSMCPConfig{
		Binary:      delegateBinary(cfg),
		DBPath:      cfg.VCSDBPath,
		RepoID:      scope.RepoID,
		WorktreeID:  wt.ID,
		Agent:       a.Agent,
		WorktreeDir: cfg.WorktreeDir,
	}.VCSMCPCommand()

	res, runErr := runDelegation(ctx, a, wt.Path, delegateBinding{
		WorktreeID: wt.ID,
		Recorder:   recorder,
		MCPCommand: mcpCmd,
	})
	if runErr != nil {
		return "", runErr
	}
	res.Worktree = wt.ID
	mergeWorktree(scope, a.Agent, wt.ID, &res)
	return marshalDelegateResult(res)
}

// mergeWorktree folds the agent's edits into a commit and merges them into
// main, recording the outcome on res.
//
// A merge conflict is NOT an error. The agent did its work; what failed is the
// integration, and main is left untouched for the conflicting paths. Reporting
// it as a tool failure would tell the model the delegation did not happen,
// which is false and would make it redo the work.
func mergeWorktree(scope VCSScope, agent, wtID string, res *acpDelegateResult) {
	if _, err := scope.VCS.CommitWorktree(wtID, agent, "acp_delegate: "+agent); err != nil {
		if !isNoChanges(err) {
			res.Note = "the agent's edits could not be committed to its worktree: " + err.Error()
			return
		}
		// No pending edits: the agent changed nothing. The merge below is an
		// empty no-op, which is the truthful outcome to report.
		res.Note = "the agent finished without editing any file."
	}
	_, conflicts, err := scope.VCS.MergeToMain(wtID, agent, false)
	if err != nil && !isConflictErr(err) {
		res.Note = "the agent's worktree could not be merged into main: " + err.Error()
		return
	}
	res.Conflicts = conflicts
	res.Merged = len(conflicts) == 0
	if len(conflicts) > 0 {
		res.Note = "merged with conflicts; those paths are unchanged in main and need manual resolution."
	}
}

// isNoChanges / isConflictErr keep the vcs sentinel checks in one place so the
// merge path reads as the policy it is rather than as error plumbing.
func isNoChanges(err error) bool { return errorsIs(err, vcs.ErrNoChanges) }

// isConflictErr reports whether err is the merge-conflict sentinel.
func isConflictErr(err error) bool { return errorsIs(err, vcs.ErrConflicts) }

// delegateBinary resolves the yanshi executable handed to the ACP adapter as
// the yanshi-vcs MCP server. os.Args[0] is the production answer; the config
// field exists so a test can pin it.
func delegateBinary(cfg ACPDelegateConfig) string {
	if strings.TrimSpace(cfg.Binary) != "" {
		return cfg.Binary
	}
	return os.Args[0]
}

// marshalDelegateResult renders the result as the JSON the model reads.
func marshalDelegateResult(res acpDelegateResult) (string, error) {
	data, err := json.Marshal(res)
	if err != nil {
		return "", fmt.Errorf("acp_delegate: encode result: %w", err)
	}
	return string(data), nil
}
