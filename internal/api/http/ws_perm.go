// internal/api/http/ws_perm.go
//
// Permission tracking, WebSocket upgrade, and interactive mode resolution.
// Split from ws.go to keep that file under the 1000 pure-code-line cap.
// Contains the permTracker, wsUpgrader, permModeState, and auto-resolution
// logic for interactive permission modes (default|allow-edits|yolo|auto|plan).

package http

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/gorilla/websocket"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// permTracker holds the per-WebSocket pending permission requests: each
// permission_request minted by the interactive callback registers a channel
// here, and the reader goroutine delivers the matching permission_response to
// it. The callback (which runs on the main loop goroutine, blocked inside the
// tool's InvokableRun) selects on that channel; the reader goroutine (separate
// from the main loop) can still read permission_response frames while the turn
// is blocked — that is what makes interactive permissions work over a transport
// whose turn runner is synchronous.
type permTracker struct {
	mu      sync.Mutex
	pending map[string]pendingPerm
	nextID  uint64
}

// pendingPerm is one in-flight permission_request. It carries the two facts
// deliver needs to re-check the client's answer: whether the static profile
// had already denied this action, and which interactive mode we were in when
// we asked. See deliver for why the mode at ask-time is the load-bearing one.
type pendingPerm struct {
	ch              chan tools.PermissionDecision
	profileHardDeny bool
	modeAtSend      guard.PermissionMode
}

func newPermTracker() *permTracker {
	return &permTracker{pending: make(map[string]pendingPerm)}
}

// newID returns a unique per-connection permission-request id.
func (p *permTracker) newID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	return strconv.FormatUint(p.nextID, 10)
}

// register records ch as the recipient for id's response, along with the
// request facts and the mode in effect at ask-time that deliver re-checks.
func (p *permTracker) register(id string, ch chan tools.PermissionDecision,
	req tools.PermissionRequest, mode guard.PermissionMode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending[id] = pendingPerm{
		ch:              ch,
		profileHardDeny: req.ProfileHardDeny,
		modeAtSend:      mode,
	}
}

// take pops (and removes) the channel for id, or returns nil if absent / already
// taken. Removal is unconditional so a late/second response cannot deliver to a
// channel whose ask has already returned.
func (p *permTracker) take(id string) (pendingPerm, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pp, ok := p.pending[id]
	delete(p.pending, id)
	return pp, ok
}

// deliver attempts to send decision to the pending channel for id. It is
// non-blocking (the channel is buffered size 1): if the ask already returned
// (timeout/cancel) the entry is gone and the send is a no-op.
//
// curMode is the interactive mode NOW. An `allow` for a request the static
// profile had already denied is honoured only if the mode has not changed
// since we asked, or if the new mode is one whose own gate would have allowed
// it anyway (yolo). The reason is that ProfileHardDeny never reaches the wire:
// under ModeAuto an unrateable request goes out as an ordinary
// permission_request, and the TUI's autoResolvePendingByMode will answer
// `allow` for any IsEditTool the moment the user switches to allow-edits --
// while resolvePermissionMode, given that same request under allow-edits,
// returns deny. Without this check a client-side mode switch silently
// overrides a server-side profile policy.
//
// Only tightening is possible here: a deny is never turned into an allow, and
// a request that was never profile-denied is passed through untouched. In
// particular this does not re-check force-prompt or approval-required
// requests -- those reach the client precisely because a human must answer
// them, and their answer is the point.
func (p *permTracker) deliver(id string, decision tools.PermissionDecision, curMode guard.PermissionMode) {
	pp, ok := p.take(id)
	if !ok {
		return
	}
	if decision == tools.PermissionAllow && pp.profileHardDeny &&
		curMode != pp.modeAtSend && curMode != guard.ModeYOLO {
		decision = tools.PermissionDeny
	}
	select {
	case pp.ch <- decision:
	default:
	}
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true }, // local trust; auth middleware gates access
}

// permModeState is the live, concurrency-safe holder for a connection's
// interactive permission mode (default|allow-edits|yolo|auto).
// The reader goroutine updates it on every set_mode frame so a
// mode switch takes effect immediately — even while a turn is running and the
// main loop (the only drainer of the frames channel) is blocked. The permission
// callback and statusFrame read it. Holding it behind a mutex rather than as
// plain connSession fields is what lets set_mode bypass the frames channel and
// reach mid-turn tool calls (notably inside sub-agents).
type permModeState struct {
	mu   sync.RWMutex
	mode guard.PermissionMode
}

func (p *permModeState) set(m guard.PermissionMode) {
	p.mu.Lock()
	p.mode = m
	p.mu.Unlock()
}

// get returns the current mode.
func (p *permModeState) get() guard.PermissionMode {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mode
}

// authorizeControlAction runs tools.Authorize against the named tool using a
// context derived from the connection's profile + approval manager (Task 22).
// Control frames (jobs_list, job_read, job_write, job_cancel) arrive on the
// WS connection outside any in-flight turn, so they cannot reuse the turn
// context. This helper builds an equivalent context from the connection-level
// state so the same HardDeny firewall + approval matching path applies.
//
// connectionSessionID is the same id minted at the top of ChatWS and bound
// into connCtx. The profile comes from Server.controlProfile (set when ChatWS
// was registered). The returned error is suitable for direct use as an error
// frame's text.
func (s *Server) authorizeControlAction(connCtx context.Context, connectionSessionID string, _ *connSession, toolName string) error {
	controlCtx := tools.WithProfile(connCtx, s.controlProfile)
	if s.approvals != nil && connectionSessionID != "" {
		controlCtx = tools.WithApprovalManager(controlCtx, s.approvals, connectionSessionID)
	}
	return tools.Authorize(controlCtx, guard.Action{Tool: toolName}, "")
}

// applySetMode normalizes a set_mode frame and writes it to the live permission
// state. Called from BOTH the reader goroutine (so the switch lands immediately,
// mid-turn) and the main loop (the canonical re-apply + status echo). Mode
// update is conditional on a recognizable mode string (fail-safe: an unknown
// value leaves the mode unchanged).
func (cs *connSession) applySetMode(cf proto.ClientFrame) (oldMode, newMode guard.PermissionMode) {
	oldMode = cs.perm.get()
	mode := oldMode
	if m, ok := guard.NormalizeMode(cf.Mode); ok {
		mode = m
	}
	cs.perm.set(mode)
	return oldMode, mode
}

// resolvePermissionMode applies the session's permission mode to a tool call
// that the static PermissionProfile would deny (or that the destructive-deletion
// dimension escalated). It returns (decision, true) when the mode auto-resolves
// WITHOUT prompting the user, and (PermissionDeny, false) — meaning "not
// resolved, prompt the user" — otherwise. The decision is the verdict the caller
// should return from the callback.
//
// Mode rules (after the destructive-deletion gate, which is profile-independent):
//   - yolo:  allow everything EXCEPT catastrophic mass deletion (blocked
//     structurally in guard), out-of-workdir deletion (blocked here), and an
//     UNREAD PAYLOAD (DestructionOpaque), which is not blocked but is asked
//     about — see the case below and ADR-0020.
//     Bypasses profile-policy denies (PolicyHardDeny).
//   - auto:  AI risk-assesses everything (including profile denies and
//     out-of-workdir deletion) except catastrophic mass deletion, which
//     is blocked structurally in guard and fail-safely here.
//   - allow-edits / default: prompt for ordinary denies; deny silently for
//     profile-policy denies (policy="deny" means block, not ask).
//   - strict: identical to default HERE. The difference is that under strict
//     tools.Authorize also sends the calls the profile ALLOWED down this path
//     (W-B-20), so "prompt for ordinary denies" covers every call.
//   - plan:  deny (read-only).
//
// Plan 模式（已决策约束 §4、§6）：
//   - 任何写操作（fs_write / shell_run / task_cancel / ...）直接 deny+resolved=true，
//     不再 prompt。这是 plan 只读模式的硬性防火墙。
//   - force-prompt 工具（task_cancel）即使 req.ForcePrompt=true 也走 deny+resolved=true
//     （plan 模式不批准任何 force-prompt）。但 ForcePrompt 的 fail-closed 默认
//     语义由 Authorize 保证（无 callback 时直接 DenyErr）。
//   - 任何非 plan 模式下，ForcePrompt 工具一律不 auto-resolve（必须显式 prompt）。
//
// fail-safe: any error or timeout in the auto-mode risk assessment falls
// through to prompting (returns not-resolved) rather than guessing.
func resolvePermissionMode(ctx context.Context, cs *connSession,
	models map[string]model.BaseChatModel, req *tools.PermissionRequest) (tools.PermissionDecision, bool) {

	// force-prompt 工具永远不走 YOLO/Auto auto-resolve（已决策约束 §5）。
	// 返回 (deny, false)：让 Authorize 的 force-prompt 分支继续走 callback
	// 显式审批（如果是非 plan 模式且有 callback），或 fail-closed（无 callback）。
	if req.ForcePrompt {
		return tools.PermissionDeny, false
	}

	// Mandatory-approval tools (NewApprovalGuardedTool — e.g. GitHub mutations)
	// also bypass YOLO/Auto auto-resolve: the user MUST explicitly click allow
	// each time. Returns (deny, false) so the callback path runs.
	if req.ApprovalRequired {
		return tools.PermissionDeny, false
	}

	// Read the LIVE mode state: the reader goroutine updates it the instant a
	// set_mode frame arrives, so a mid-turn switch (e.g. while a sub-agent's
	// tool call is pending) is honored by the very next callback invocation.
	mode := cs.perm.get()

	// Destructive-deletion gate (profile-independent). Catastrophic mass deletion
	// is already blocked structurally in guard.Check, so this is a fail-safe.
	// YOLO additionally blocks out-of-workdir deletion (per spec); auto lets the
	// AI risk assessment below judge out-of-workdir deletion like anything else,
	// and default/allow-edits/plan surface it as a normal interactive prompt.
	if req.Shell != "" {
		switch guard.ClassifyDestruction(req.Shell, req.Workdir) {
		case guard.DestructionCatastrophic, guard.DestructionUnreadable:
			// Unreadable joins Catastrophic here rather than falling through to
			// the mode switch: both are structural refusals in guard.Check, and
			// a fail-safe that silently declined to cover one of them would let
			// yolo auto-resolve a command whose real program the guard was
			// never able to identify.
			return tools.PermissionDeny, true
		case guard.DestructionOpaque:
			// "I could not read this command" followed by "approved
			// automatically" denies the tier its entire reason for existing.
			// DestructionOpaque was absent from this switch, so yolo passed the
			// whole tier — including the family V-2 had just closed
			// (`pkexec rm -rf /`) and the one ADR-0020 closes
			// (`GIT_SSH_COMMAND='rm -rf /' git fetch`).
			//
			// It takes the ForcePrompt route — (deny, FALSE), "not resolved,
			// hand it back for an explicit answer" — rather than the
			// OutOfScope-under-yolo route one case down, which is (deny, true),
			// a refusal. The difference is the tier's own claim: OutOfScope
			// knows the deletion leaves the project, while Opaque knows only
			// that nobody read the payload, and a refusal nobody can appeal is
			// defensible only when the reason can be stated (ADR-0018). So yolo
			// still ASKS here; it just no longer answers for the user.
			//
			// auto is deliberately not special-cased: it falls through to the
			// model, which reads the full command text and can see the payload
			// this package could not. Its error policy is already one-way
			// (no model, timeout, unreadable reply -> prompt).
			if mode == guard.ModeYOLO {
				return tools.PermissionDeny, false
			}
		case guard.DestructionOutOfScope:
			if mode == guard.ModeYOLO {
				return tools.PermissionDeny, true
			}
		}
	}

	// Profile-policy deny (overridable HardDeny): YOLO bypasses the profile
	// directly. Auto does NOT bypass — it AI-judges everything except the
	// catastrophic commands already handled above — so it falls through to the
	// ModeAuto risk assessment. default/allow-edits/plan deny SILENTLY
	// (policy="deny" means block, not ask), handled per-case in the switch.
	if req.ProfileHardDeny && mode == guard.ModeYOLO {
		return tools.PermissionAllow, true
	}

	switch mode {
	case guard.ModeStrict:
		// W-B-20's fourth execution level. At the callback it behaves exactly
		// like ModeDefault — auto-resolve nothing, deny a profile policy
		// silently — because there is nothing STRICTER available to a mode
		// gate that only ever runs on a denied call. Its extra strictness is
		// upstream, in tools.Authorize, where a guard ALLOW is rewritten into a
		// prompt (tools.WithConfirmEveryCall); the prompt then arrives here and
		// has to reach the user, which is what the fallthrough to (deny, false)
		// below does.
		//
		// The ProfileHardDeny arm is NOT relaxed into a prompt. Turning
		// policy="deny" into a question would make the strictest mode the only
		// one in which a profile refusal is appealable, which is the opposite
		// of what the name promises.
		if req.ProfileHardDeny {
			return tools.PermissionDeny, true
		}
		return tools.PermissionDeny, false
	case guard.ModePlan:
		// Plan 是只读模式：任何到达 callback 的写操作一律 deny+resolved。
		// 这条与 tools.Authorize 的 PlanToolAllowed 是两层独立防线。
		return tools.PermissionDeny, true
	case guard.ModeYOLO:
		return tools.PermissionAllow, true
	case guard.ModeAllowEdits:
		if req.ProfileHardDeny {
			return tools.PermissionDeny, true // profile says no writes at all; allow-edits respects it
		}
		if guard.IsEditTool(req.Tool) {
			return tools.PermissionAllow, true
		}
		return tools.PermissionDeny, false
	case guard.ModeAuto:
		// The model decides, with the session's context. There is no static
		// list beside it: the categories that would have been one live in
		// guard.AutoApprovalPrompt instead, where they read the full command
		// text rather than a tokenised program word (see that function's doc
		// for why the static version was weaker at exactly the wrapper case
		// it existed to catch).
		//
		// Two layers still sit underneath and are NOT the model's to decide:
		// catastrophic mass deletion and shell metacharacters are structural
		// HardDenies, and out-of-workdir deletion was already graded above. A
		// delete reaching this point is one inside the workdir.
		if !askAutoApproval(ctx, models, cs, *req) {
			// W-B-15. The user could already override this — an unresolved
			// verdict has always reached the prompt — but the prompt did not
			// say WHO refused, so "the profile does not allow this" and "a
			// model judged this risky" arrived as the same dialog. Naming the
			// source is what makes the override an informed one; the callback
			// additionally keeps it one-shot and records it.
			req.AIDeclined = true
			req.Reason = autoDeclinedReason(req.Reason)
			return tools.PermissionDeny, false
		}
		// A script the model just cleared is worth remembering: the same
		// script re-run in a loop would otherwise cost one model round-trip
		// per iteration. AllowPersistent (rather than Allow) makes Authorize
		// record an approval rule, and that rule's scope carries the script's
		// content hash — so the memory lasts exactly as long as the script is
		// unchanged, and editing one byte asks again.
		//
		// Only script executions get this. Every other call returns a plain
		// Allow, because their scope is just program+args: remembering those
		// would turn one model verdict into a standing rule for a command
		// shape whose meaning can change entirely with its operands.
		if tools.CommandRunsAScript(req.Shell, req.Workdir) {
			return tools.PermissionAllowPersistent, true
		}
		return tools.PermissionAllow, true
	default: // ModeDefault
		if req.ProfileHardDeny {
			return tools.PermissionDeny, true // policy="deny" means block, not ask
		}
		return tools.PermissionDeny, false
	}
}

// resolvePermissionRequest applies auto-mode resolution only to ordinary
// requests. A forced destructive request (B2-RB1 req.Force — set by
// tools.RequireApproval) must stay unresolved so the caller emits
// permission_request and waits for an explicit one-shot decision. This is the
// D3 gate that keeps yolo / allow-edits / auto from silently admitting a
// destructive revert_turn.
func resolvePermissionRequest(ctx context.Context, cs *connSession,
	models map[string]model.BaseChatModel, req *tools.PermissionRequest,
) (tools.PermissionDecision, bool) {
	if req.Force {
		return tools.PermissionDeny, false
	}
	return resolvePermissionMode(ctx, cs, models, req)
}

// autoDeclinedReason prefixes the guard's own reason with the fact that the
// model refused (W-B-15).
//
// Prefix rather than replace: the static reason is still the more actionable
// half when there is one ("shell command not on allowlist" tells the operator
// which line of their profile to look at), and a Prompt that reached auto mode
// with no reason at all is common — the FS and tool dimensions produce plenty.
func autoDeclinedReason(reason string) string {
	const lead = "the automatic risk assessment declined to approve this " +
		"unattended and asked for a human decision"
	if strings.TrimSpace(reason) == "" {
		return lead
	}
	return lead + "; the static policy also said: " + reason
}

// oneShotIfAIDeclined downgrades a sticky approval to a single-call one when
// the model had declined (W-B-15).
//
// Overriding a verdict is not switching the judge off. "allow for this session"
// on an AI-declined call would record an approval rule for that scope, and
// Authorize's approval-manager short-circuit runs BEFORE the mode gate — so
// every later call in that scope would skip the risk assessment entirely,
// silently, for the rest of the session. One yes, one call.
//
// Everything else passes through untouched, including a deny.
func oneShotIfAIDeclined(req tools.PermissionRequest, d tools.PermissionDecision) tools.PermissionDecision {
	if !req.AIDeclined {
		return d
	}
	switch d {
	case tools.PermissionAlwaysAllow, tools.PermissionAllowSession, tools.PermissionAllowPersistent:
		return tools.PermissionAllow
	}
	return d
}

// auditAIOverride records a human overriding ModeAuto's ASK verdict (W-B-15).
//
// Written here rather than left to Authorize because Authorize cannot see it:
// the flag lives on the callback's own copy of the request, and by the time
// Authorize logs "allow / interactive_once" the fact that a model refused first
// is gone. A refusal that a human reversed is the row an audit is FOR.
//
// Only an approval is recorded as an override. A human agreeing with the model
// is the ordinary outcome and Authorize already logs the denial.
func auditAIOverride(ctx context.Context, req tools.PermissionRequest, d tools.PermissionDecision) {
	if !req.AIDeclined {
		return
	}
	switch d {
	case tools.PermissionAllow, tools.PermissionAlwaysAllow,
		tools.PermissionAllowSession, tools.PermissionAllowPersistent:
	default:
		return
	}
	digest := req.Shell
	if digest != "" {
		digest = "shell: " + digest
	}
	tools.RecordPermissionAudit(ctx, tools.PermissionAuditRecord{
		Tool:       req.Tool,
		Decision:   "allow",
		Source:     "auto_approval_override",
		ReasonCode: "human_overrode_model_refusal",
		CmdDigest:  digest,
	})
}

// forcePromptFlag reports whether a request may NOT be auto-approved or
// pre-approved — only an explicit per-call decision counts. It is the single
// source for two things that must never drift apart:
//
//   - the server's own refusal to auto-resolve (req.Force here in
//     resolvePermissionRequest, req.ForcePrompt at the top of
//     resolvePermissionMode);
//   - the force_prompt flag put on the permission_request frame, which is what
//     the client's own auto-approve pass (the TUI's autoResolvePendingByMode,
//     which fires on every permission-mode switch) reads.
//
// Deriving both from one predicate is the fix for the shape where only the
// server half existed: the prompt was emitted correctly, and then the TUI
// answered "allow" for a user who had merely switched to YOLO.
//
// req.ApprovalRequired is deliberately NOT folded in — it travels on its own
// wire field (approval_required) and the client ORs the two.
func forcePromptFlag(req tools.PermissionRequest) bool {
	return req.ForcePrompt || req.Force
}

// askAutoApproval asks the session model whether a tool call may run
// unattended. It returns true ONLY on an explicit ALLOW: no model, a timeout,
// an API error and an unreadable reply all return false, which the caller
// turns into an ordinary user prompt. That is the whole error policy — auto
// mode degrades to manual mode, never to permissive.
//
// The 15s ceiling is generous because the model is idle at this point: the
// ReAct loop is paused between iterations waiting for this very verdict.
func askAutoApproval(ctx context.Context, models map[string]model.BaseChatModel,
	cs *connSession, req tools.PermissionRequest) bool {

	m := cs.selectModel(models)
	if m == nil {
		// Fall back to any registered model (common when cs.model == "").
		for _, mm := range models {
			m = mm
			break
		}
	}
	if m == nil {
		return false
	}
	askCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := m.Generate(askCtx, []*schema.Message{
		schema.UserMessage(autoApprovalPromptFor(cs, req)),
	})
	if err != nil || resp == nil {
		return false
	}
	allow, ok := guard.ParseAutoApproval(resp.Content)
	return ok && allow
}

// autoApprovalPromptFor assembles what the model is shown. It is a named
// function rather than an inline literal so a test can assert the session
// context actually reaches the prompt: every field here has its own source,
// and a field silently dropped on the way in is indistinguishable from one
// that was never collected.
func autoApprovalPromptFor(cs *connSession, req tools.PermissionRequest) string {
	return guard.AutoApprovalPromptWith(cs.guardianPrompt, guard.AutoApprovalRequest{
		Tool:     req.Tool,
		Args:     req.Args,
		Shell:    req.Shell,
		Workdir:  req.Workdir,
		Reason:   req.Reason,
		UserGoal: cs.latestUserMessage(),
	})
}

// latestUserMessage returns the text of the most recent user turn, which is
// the agent's stated goal for everything it is currently doing. Auto mode
// shows it to the model so "is this call part of what was asked for" is
// answerable at all; without it the model sees a bare command with no purpose
// to judge it against.
func (cs *connSession) latestUserMessage() string {
	for i := len(cs.history) - 1; i >= 0; i-- {
		if cs.history[i] != nil && cs.history[i].Role == schema.User {
			return cs.history[i].Content
		}
	}
	return ""
}

// sessionRuleRecorder is the slice of the orchestrator that S9 needs: turn one
// answered shell prompt into (or out of) a session rule.
//
// An interface rather than *orchestrator.Orchestrator so the WS tests can
// count calls without standing up a model, a tool registry and a runner —
// which is the shape of test that makes a "zero readers" regression easy to
// reintroduce, because the expensive setup is exactly what gets dropped.
type sessionRuleRecorder interface {
	ApproveShellForSession(connectionSessionID, command string) bool
	DemoteShellForSession(connectionSessionID, command string) bool
}

// recordSessionApproval feeds one answered permission prompt back into the
// connection's approval rule set (S9), so a long goal loop stops asking about
// `go test ./internal/a`, `./internal/b`, `./internal/c` one prompt at a time.
//
// This is THE consumer that internal/guard/generalize.go never had. Everything
// that file implements — the widening, the high-risk verb refusal, the
// irreversible demotion — described behaviour nothing could produce until this
// call existed.
//
// Only SHELL prompts participate. req.Shell is empty for every other tool, and
// a generalized rule is an execpolicy rule, which is a statement about a
// program and its argument vector; there is nothing for it to say about
// fs_write.
//
// The mapping is deliberately asymmetric:
//
//   - ALLOW (in any of its four spellings) widens. The user said yes to this
//     command shape.
//   - DENY demotes, IRREVERSIBLY for the session. A refusal inside a family a
//     previous approval widened is direct evidence that the widening was
//     wrong, and the only heuristic available to re-widen it later is the one
//     that just produced the bad rule.
//
// A prompt that TIMED OUT arrives here as PermissionDeny too, and demoting on
// it is the conservative reading rather than a bug: an unattended connection
// that stops answering is not evidence for keeping a widened rule alive.
//
// Scope caveat, repeated from orchestrator/sessionrules.go because this is
// where an operator would look: WithSessionRules is a no-op on a profile with
// no shell.rules, so this changes nothing for the factory-default coding
// profile. It is live for the operator profile.
func recordSessionApproval(rec sessionRuleRecorder, connectionSessionID string,
	req tools.PermissionRequest, decision tools.PermissionDecision) {
	if rec == nil || connectionSessionID == "" || req.Shell == "" {
		return
	}
	// W-B-15: a command the model refused and a human waved through once must
	// not widen a rule family. The caller already downgraded the DECISION to a
	// one-shot allow, which stops Authorize recording an approval rule — this
	// is the second store, and it has its own recording path.
	if req.AIDeclined {
		return
	}
	// A force-prompt or approval-required tool must ask EVERY time; recording
	// a rule for one would be a standing grant for a call whose whole contract
	// is that it has none. Neither reaches Authorize's approval manager for the
	// same reason.
	if req.ForcePrompt || req.ApprovalRequired || req.Force {
		return
	}
	switch decision {
	case tools.PermissionAllow, tools.PermissionAlwaysAllow,
		tools.PermissionAllowSession, tools.PermissionAllowPersistent:
		rec.ApproveShellForSession(connectionSessionID, req.Shell)
	default:
		rec.DemoteShellForSession(connectionSessionID, req.Shell)
	}
}
