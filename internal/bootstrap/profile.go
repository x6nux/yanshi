// Package bootstrap — the factory-default permission profile.
//
// This file holds DefaultOrchestratorProfile and the machinery that keeps its
// allow list in step with the tool registry Build actually assembles. It lives
// apart from bootstrap.go for the reason c1wiring.go gives: bootstrap.go is
// within ~50 lines of the 1000-code-line GOV2 ceiling and lineExceptions is an
// empty, removal-only map, so there is no exemption to spend on it.
//
// It is also the more cohesive home: authorization (the allow list) and
// registration (App.ToolNames) are one invariant, and GOV5 exists precisely
// because they used to drift.
package bootstrap

import (
	"context"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/tools"
)

// ConditionalProfileTools names the tools that bootstrap.Build registers only
// when a runtime precondition holds, and that the default profile therefore
// must NOT hard-code.
//
// Everything in DefaultOrchestratorProfile is a name Build registers on every
// boot. A conditionally-registered tool listed there is a phantom name in the
// exact deployments where the condition fails — GOV5 cannot see it, because
// GOV5 builds one app and a phantom that only appears under another config is
// structurally invisible to it.
//
// Today the set holds two names:
//
//   - rlm_query — needs an explicitly configured cheap provider
//     (batch.rlm_model, see SelectRLMModel). Absent it, BuildC1 warns, leaves
//     C1Components.RLM nil, and wireC1 registers automation_* and agent_batch
//     without it. That is the DEFAULT config, so hard-coding "rlm_query" in
//     the profile made the shipped default advertise a tool most deployments
//     never get.
//
//   - skill_write — needs a user skills directory to write into. With none,
//     BuildSkillWriteTool returns nil and the tool is never registered, so a
//     static allow entry would name a phantom in exactly those deployments.
//     Its capability is narrow: it writes a SKILL.md beneath a root fixed at
//     construction, which the model cannot redirect through arguments.
//
// The eight automation_* tools and agent_batch stay in the static list on
// purpose: their only degradation path is a wireC1 error, which BuildC1 can
// only produce from a nil registry or nil adapter — arguments Build always
// supplies non-nil. If that ever stops being true, the production-shaped GOV5
// assertion (TestGOV5ProductionProfileHasNoPhantomNames) reddens, because it
// checks the EFFECTIVE profile against the EFFECTIVE registry rather than
// trusting which list a name was filed under.
//
// Do NOT "fix" a conditional tool by registering an always-failing stub so the
// name is always present: a tool that cannot do its job is a placeholder, and
// an allow list that names it is lying in a second way.
func ConditionalProfileTools() []string {
	return []string{"rlm_query", "skill_write"}
}

// extendProfileWithConditionalTools returns p with every ConditionalProfileTools
// name that is ACTUALLY in registered appended to the allow list.
//
// This is the single source that keeps authorization and registration in step:
// a conditional tool can only be authorized in a boot where it exists, because
// the allow entry is derived from the registry snapshot rather than written
// down next to it.
//
// It is applied ONLY to the factory default. An operator who declares
// profiles.orchestrator has made an explicit least-privilege statement, and
// silently widening it with tools they did not name would defeat the point.
func extendProfileWithConditionalTools(p guard.PermissionProfile, registered []string) guard.PermissionProfile {
	have := make(map[string]bool, len(registered))
	for _, n := range registered {
		have[n] = true
	}
	// Copy before appending: p.Tools.Allow comes from DefaultOrchestratorProfile,
	// whose slice literal has spare capacity in no defined amount — appending in
	// place would be fine today and aliasing tomorrow.
	allow := make([]string, len(p.Tools.Allow), len(p.Tools.Allow)+len(ConditionalProfileTools()))
	copy(allow, p.Tools.Allow)
	for _, name := range ConditionalProfileTools() {
		if have[name] {
			allow = append(allow, name)
		}
	}
	p.Tools.Allow = allow
	return p
}

// DefaultOrchestratorProfile returns the factory-default permission profile
// for the orchestrator. The orchestrator no longer falls back to
// Tools={"*"}: when the operator did not configure profiles.orchestrator, we
// ship this concrete "coding" profile naming the tools the orchestrator
// actually uses, so a forgotten profile block stays least-privilege rather
// than fail-open. Operators who need shell/net widening must declare it in
// config.yaml.
//
// INVARIANT: every concrete name here is registered by Build on EVERY boot.
// Tools whose registration depends on config belong in ConditionalProfileTools
// instead, where extendProfileWithConditionalTools adds them back only when
// they were really registered.
//
// Exported so GOV5 (internal/bootstrap/wiring_test.go) can compare the
// shipped allow list against the shipped tool registry without having to
// reach into Build.
func DefaultOrchestratorProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{
			// NB: "fs_patch" and "fs_mkdir" used to be listed here and were
			// dropped — neither has ever been a registered tool. The patch
			// tool's real name is "apply_patch" (registered in
			// internal/tools.NewFSTools; line numbers are omitted on purpose,
			// they drift silently while symbol names do not),
			// which is already allowed below; there is no mkdir tool at all.
			"fs_read", "fs_list", "fs_search", "fs_glob", "fs_write", "fs_edit",
			// T2 structural search. Allowed alongside fs_search because its
			// capability is a strict SUBSET: it authorizes the same "read" FS
			// action on the same jailed path, launches one read-only
			// subprocess through secproc, and cannot write. Leaving it out
			// while fs_search is in would be an inverted gradient — the
			// narrower tool costs a dialog, the broader one does not — and on
			// SSE, which has no permission callback, it would be permanently
			// fail-closed while the tool still burned schema tokens.
			"ast_search",
			"shell_run", "shell_start", "shell_read", "shell_write_stdin", "shell_wait", "shell_cancel",
			// shell_resize carries strictly less capability than shell_write_stdin,
			// which is already allowed: it changes a terminal's geometry and cannot
			// put a byte in front of the child. Leaving it out while shell_start's
			// pty parameter is allowed would be the inverted gradient this list has
			// been corrected for twice — the harmless tool costs a dialog, the one
			// that types into the session does not.
			"shell_resize",
			"task_shell_start", "task_shell_wait", "task_shell_stdin", "task_shell_cancel",
			// A2 durable tasks. These were registered but never allowed, so the
			// model saw four tools it could call and the guard refused each one:
			// Prompt on WS (a dialog per call) and fail-closed on SSE, which has
			// no callback. task_cancel additionally carries ForcePrompt, so
			// listing it here authorizes discovery, not unattended cancellation.
			"task_create", "task_list", "task_read", "task_cancel",
			// …and the two that make them useful. task_gate_run runs a command,
			// but its capability is a strict SUBSET of shell_run's, which is
			// already allowed above: it refuses shell metacharacters, jails cwd
			// to the work root, and additionally Authorizes an FS read of that
			// cwd. Leaving it out while shell_run is in is an inverted gradient
			// — the narrower tool costs a dialog and the wider one does not.
			// artifact_read is read-only and is the only way to retrieve a gate
			// log that spilled, so withholding it makes the spill a deletion.
			"task_gate_run", "artifact_read",
			"memory_search", "memory_recall", "memory_write",
			// W-D-07 provenance. READ-ONLY, and it reaches nothing
			// memory_search cannot already return — it answers "which
			// conversation produced this note" for a memory id the model got
			// from one of the two lines above. Registered but not allowed would
			// mean a dialog per call on WS and a fail-closed refusal on SSE.
			"memory_source",
			// C2 history recall. Both are READ-ONLY and scoped to the current
			// conversation by context (never by argument), so they widen
			// nothing: the model already saw this history, it is being handed
			// back what compaction took away. Withholding them makes eviction
			// permanent forgetting, which is the failure C1 exists to prevent.
			"history_search", "history_read",
			// C7: the model labels its own work so compaction can keep the
			// label after the underlying turns are evicted. Writes one row
			// scoped to the current conversation by context, never by argument.
			"milestone_set",
			// W-C-14: the model asks to skip straight to a trimmed context
			// window instead of waiting for automatic compaction. It writes
			// nothing durable — only a one-shot flag on THIS turn's own
			// context (einollm.RequestNewWindow), consumed at most once by
			// the next model call on the same turn — so it widens nothing a
			// dialog would meaningfully gate: worst case is an early,
			// unnecessary context trim, the same outcome an over-eager
			// automatic compaction threshold already produces silently.
			"context_new_window",
			// W-C-11: read-only — it reports numbers other production code
			// (loopguard's TokenBudgetGate, einollm's CompactingModel) already
			// computed and holds on the turn's own context; it takes no
			// argument and writes nothing anywhere. A dialog on a pure status
			// query is the same over-gating context_new_window's comment above
			// warns against.
			"context_budget",
			// T3 background offload. background_list and background_result are
			// READ-ONLY over this process's own offload registry, and
			// background_cancel only takes capability away. Withholding them
			// makes the run id in the offload notice a token the model is told
			// to remember and can never spend — and the id is the entire point
			// of offloading, so the tool that produced it (shell_run, allowed
			// above) would end up strictly worse off than before.
			"background_list", "background_result", "background_cancel",
			// A2 self-management. These let an agent keep its own plan and
			// todo list; none of them touches anything outside the task row it
			// already owns. Leaving them out inverted the permission gradient
			// against the one tool users notice: a dialog every time the agent
			// updates its own checklist is a feature the model learns not to
			// use, while shell_run — which executes arbitrary commands — was
			// already exempt.
			//
			// NOT added here, deliberately: "screenshot" (reads the user's
			// screen, a privacy boundary the model should have to ask for) and
			// "revert_turn" (discards edits, so a prompt is the point).
			// "rlm_query" stays in ConditionalProfileTools.
			"update_plan", "image_describe",
			"checklist_add", "checklist_list", "checklist_update", "checklist_write",
			"todo_add", "todo_list", "todo_update", "todo_write",
			// W-B-12. Grants nothing on its own: every path through it either
			// refuses without asking or goes through RequireApproval, which
			// sets req.Force so no permission MODE — yolo included — can answer
			// for the user. Withholding it would be an inverted gradient of the
			// worst kind: the tool whose entire purpose is to ASK would be the
			// one that costs a dialog to reach, and on SSE (no callback) it
			// would be permanently fail-closed while still burning schema
			// tokens telling the model to call it.
			"request_permission",
			"web_fetch", "web_search", "time_now", "skill_use", "vcs_*",
			"agent_start", "workflow_start", "analysis", "summarize",
			// B1 managed sub-agents. Deliberately NOT a widening of what the
			// model can do: agent_start is already allowed and already spawns a
			// sub-agent, so the capability was there. What was missing was the
			// ability to SEE and STOP one — list/result/wait are read-only, and
			// cancel only takes capability away. Withholding them left the one
			// spawn path that ships fire-and-forget.
			//
			// The predecessor deferred this to W5 as "an authorization change,
			// same class as apply_patch". That reading does not survive the
			// distinction above, and W5 closed without it, so the deferral had
			// become a permanent omission rather than a plan. Both limits that
			// bound a sub-agent (registry.MaxDepth, subagents.limit) still
			// apply to every spawn made through these.
			"agent_spawn", "agent_list", "agent_result", "agent_wait",
			"agent_send_input", "agent_assign", "agent_cancel", "agent_resume",
			"apply_patch",
			// B3 developer tools
			"git_status", "git_diff", "run_tests", "diagnostics",
			"github_pr_context", "github_comment", "github_approve", "github_merge",
			"review",
			// T1 LSP navigation. Read-only lookups whose capability is strictly
			// NARROWER than fs_search, which is already allowed above: they take
			// a symbol name or a file position and return locations plus the
			// source line at each, re-checking the FS read allowlist per file so
			// a denied path keeps its location but loses its snippet. Withholding
			// them while fs_search is allowed is an inverted gradient — the
			// accurate tool would cost a dialog on WS and be permanently
			// fail-closed on SSE, while the regex one costs nothing.
			"lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols",
			// C1. Spelled out one by one rather than as "automation_*": GOV5's
			// phantom-name check skips any entry containing a wildcard, so a
			// glob here would silently re-open the hole W0 just closed. Note
			// these eight are additionally approval-gated at call time
			// (NewApprovalGuardedTool), so listing them here authorizes
			// discovery, not unattended execution.
			"automation_create", "automation_list", "automation_read", "automation_update",
			"automation_pause", "automation_resume", "automation_delete", "automation_run",
			"agent_batch",
			// T12. Grants nothing on its own: dispatch re-runs the callee's own
			// Authorize for every step, so this entry authorizes the batch
			// wrapper and not one thing inside it. A step naming a tool that is
			// NOT in this allow list still costs a dialog (or is refused on
			// SSE), exactly as the same call made directly would.
			"tool_batch",
			// NB: "rlm_query" is deliberately NOT here — it registers only when
			// batch.rlm_model names a cheap provider. See ConditionalProfileTools.
		}},
		Net: guard.NetPerm{Allow: true},
	}
}

// BindAgentLaunchContext binds the two context values secproc.Launch fails
// closed without — a permission profile and a process factory — for a caller
// that spawns an external agent OUTSIDE an orchestrator turn.
//
// `yanshi goal` is that caller and, so far, the only one. Its worker used to
// reach exec.CommandContext directly (acp.Spawn, deleted by W-B-02); routing it
// through the same launcher as everything else means it now needs the same two
// bindings, and the composition root is where knowledge of both belongs.
//
// # Why a purpose-built profile rather than the orchestrator's
//
// DefaultOrchestratorProfile deliberately omits acp_delegate so the CHAT path
// prompts the operator every time (see tools.NewACPDelegateTool). That is the
// right answer when a model chose to delegate mid-turn. It is the wrong one
// here: the operator typed `yanshi goal -agent codex` at a shell prompt, which
// IS the approval, and there is no permission callback on this path — a Prompt
// would simply fail closed and the subcommand could never run.
//
// The profile therefore allows exactly one tool name and nothing else. Every
// other dimension stays at its zero value, which is the fail-closed one: no FS
// paths, no shell, no network, no MCP. Widening it is an authorization change.
func (a *App) BindAgentLaunchContext(ctx context.Context) context.Context {
	ctx = tools.WithProfile(ctx, agentLaunchProfile())
	return tools.WithSecureProcessFactory(ctx, a.SecureFactory)
}

// agentLaunchProfile is the single-tool profile BindAgentLaunchContext binds.
// Split out so the allow list is one greppable literal rather than an
// expression buried in a context chain.
func agentLaunchProfile() guard.PermissionProfile {
	return guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"acp_delegate"}},
	}
}
