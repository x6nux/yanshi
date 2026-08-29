package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/execpolicy"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/secproc"
	"github.com/x6nux/yanshi/internal/shell"
	"github.com/x6nux/yanshi/internal/toolreg"
)

// PermissionRequest describes a tool call awaiting user approval. The GuardedTool
// layer builds one when the static PermissionProfile returns Prompt and a
// permission callback is bound in the context (interactive mode, WS only).
//
// ForcePrompt: 当工具在 forcePromptTools 名单里（A2 Task 6：仅 task_cancel）
// 时为 true，标识该请求"不可被 allowlist 短路、不可被 always_allow 持久化"。
// callback 拿到这个字段后可以走更突出的 UI（如 yanshi TUI 的 modal 而非 inline
// yes/no）。即使 callback 返回 PermissionAlwaysAllow/PermissionAllowSession，
// force-prompt 工具也不会被 approval.Manager 记录 —— 下次同一调用仍 prompt。
type PermissionRequest struct {
	Tool             string // tool name, e.g. "fs_write", "shell_run"
	Args             string // raw JSON args blob, for display (matches the tool_call frame)
	Reason           string // why the static profile denied (e.g. "shell command not on allowlist")
	ForcePrompt      bool   // true if the tool is on forcePromptTools; cannot be pre-approved
	ApprovalRequired bool   // true for mandatory-approval tools (e.g. GitHub mutations); YOLO/auto cannot bypass
	// Force (B2-RB1 D3): marks a destructive action that must ALWAYS prompt, even
	// under yolo / allow-edits / auto interactive mode. Set by RequireApproval
	// (revert_turn). The WS-installed permission callback reads this: when true
	// it skips mode resolution and emits the interactive prompt unconditionally.
	Force bool
	// ProfileHardDeny is true when the guard returned an OVERRIDABLE HardDeny —
	// a profile-policy default (empty Tools/FS allowlist, shell policy="deny",
	// net.allow=false). resolvePermissionMode gates it by interactive mode:
	// YOLO overrides it outright (the user opted out of the profile), while
	// default/allow-edits/plan deny SILENTLY without prompting (so policy="deny"
	// still means "block", never "ask").
	//
	// Auto is the exception, and this comment used to deny that it existed
	// ("always resolved server-side and therefore never reaches the TUI"). Auto
	// does NOT short-circuit: it falls through to the risk assessment, which
	// returns unresolved whenever it scores the call above the ceiling or
	// cannot score it at all (no model, timeout). Those requests DO reach the
	// TUI as an ordinary permission_request. That is the intended outcome (auto
	// means "ask when unsure"), but the flag itself stays server-side, so the
	// client cannot tell such a prompt apart from a plain one — switching to
	// allow-edits then auto-approves an edit tool that the server's own
	// allow-edits gate would have denied.
	//
	// RESOLVED in W5: the flag still does not go on the wire, and the client
	// is still free to answer however it likes. The server stopped taking its
	// word for it instead. permTracker.register stores this flag plus the mode
	// in effect when the prompt was sent, and permTracker.deliver downgrades
	// an `allow` to a deny when the flag is set AND the mode has changed since
	// (yolo excepted, because yolo's own gate allows profile denies anyway).
	//
	// Why the mode comparison rather than a wire flag: the wire cannot
	// distinguish a human clicking allow from the TUI auto-resolving on the
	// user's behalf, but the mode can. Answering in the mode we asked in is a
	// human answering the question; an allow arriving under a mode we never
	// asked in was produced by a client-side RULE, and profile policy is the
	// server's rule to apply. Putting the flag on the wire would also have
	// left the server trusting a client to honour it.
	//
	// Pinned by internal/api/http::TestDeliverRejectsModeSwitchAllowOnProfileDeny.
	ProfileHardDeny bool
	// Shell carries the shell command for shell_run (empty otherwise) so the
	// interactive mode layer can apply its destructive-deletion gate
	// (guard.ClassifyDestruction) regardless of why the call reached the
	// callback. Workdir is the project root used as the in-scope boundary.
	Shell   string
	Workdir string
	// AIDeclined marks a request that reached the interactive prompt because
	// ModeAuto's risk assessment answered ASK (W-B-15). It is set by the mode
	// gate, not by Authorize, and is SERVER-SIDE ONLY — the reason it conveys
	// travels to the client inside Reason, which the user reads, while the flag
	// itself stays where the decisions are made. Same rule as ProfileHardDeny
	// and for the same reason: a flag on the wire is a flag the server has to
	// take a client's word for.
	//
	// Two consequences, both in the WS callback:
	//
	//   - the prompt says the model declined and why, so an approval is an
	//     informed one rather than a click on an unexplained dialog;
	//   - a "session"/"persistent" answer is downgraded to a one-shot allow.
	//     Overriding a verdict is not the same as switching the judge off, and
	//     a standing rule for a scope the model flagged would do the second.
	AIDeclined bool
}

// forcePromptTools 列出"必须每次显式审批、不可被任何 allowlist/always_allow 短路"
// 的工具。已决策约束 §5：task_cancel 在 wildcard profile（Tools.Allow=["*"]）、
// YOLO、Auto 下都不能绕过显式审批；SSE 没 callback → fail-closed。
//
// 加新工具到这里时，**必须同时**：
//  1. 在 Authorize 的 force-prompt 分支测试它；
//  2. 确认服务端的模式闸门仍把它交回用户 ——
//     `internal/api/http/ws_perm.go::resolvePermissionMode` 在函数最开头就对
//     `req.ForcePrompt` 返回 `(deny, false)`，即"不自动放行、交回 callback 显式
//     审批"。这是唯一一处 resolvePermissionMode，它住在 WS 服务端，**不在
//     internal/cli/tui 下**（TUI 侧那半边叫
//     `internal/cli/tui/permissions.go::modeAutoAllows`，只在用户切换模式时经
//     `autoResolvePendingByMode` 生效）。
//  3. 确认这个字段**真的上了 wire**。TUI 那半边读的不是这里的 Go 结构体，而是
//     `internal/proto/frame.go` 的 `ServerFrame.ForcePrompt`，由
//     `internal/api/http/ws.go` 从 `req.ForcePrompt || req.Force` 填充。这两半
//     曾经看的是不同字段 —— ServerFrame 当时根本没有 ForcePrompt，于是服务端
//     拒绝 auto-resolve、弹窗照常出现，可用户一切到 YOLO，TUI 就替他答了
//     allow，**用户从未做出授权表示**。这条链**分三段**钉住，缺任何一段都会让
//     整条链在全绿状态下断开（第二段一度就是空的：删掉映射那一行，全量套件不红）：
//     `internal/api/http/ws_perm_test.go::TestPermissionRequestFrameCarriesForcePrompt`
//     （服务端两个 flag -> ServerFrame）、
//     `internal/cli/streamevent_parity_test.go::TestWSBackend_ForcePromptReachesStreamEvent`
//     （ServerFrame -> wire -> cli.StreamEvent）、
//     `internal/cli/tui/perm_mode_test.go::TestForcePromptPermissionSurvivesYOLOAndAuto`
//     （StreamEvent -> TUI 的 mandatory）。第二段还有一道结构性孪生
//     `internal/cli/streamevent_parity_test.go::TestToStreamEventCarriesEveryServerFrameField`
//     ——它对 ServerFrame 的**每个**字段要求一条「carried / not carried」声明，
//     所以下一个上 wire 的字段不会重蹈同样的静默丢弃。
var forcePromptTools = map[string]struct{}{
	"task_cancel": {},
}

// isForcePromptTool 报告 tool 是否在 forcePromptTools 名单中。
func isForcePromptTool(tool string) bool {
	_, ok := forcePromptTools[tool]
	return ok
}

// PermissionDecision is the user's answer to a PermissionRequest.
type PermissionDecision string

const (
	// PermissionAllow permits this single call; the next identical call prompts again.
	PermissionAllow PermissionDecision = "allow"
	// PermissionDeny blocks the call; the tool returns its DenyErr (as if static-denied).
	PermissionDeny PermissionDecision = "deny"
	// PermissionAlwaysAllow is kept as a backwards-compatible alias for
	// PermissionAllowSession. New code SHOULD emit PermissionAllowSession
	// explicitly so the verdict is unambiguous at the wire layer, but the
	// legacy TUI / WS code paths still send "always_allow" for one release.
	PermissionAlwaysAllow PermissionDecision = "always_allow"
	// PermissionAllowSession admits this call AND records a TTL=session rule
	// in the approval manager so subsequent identical actions in the same
	// session skip the prompt. The rule is dropped at process exit.
	PermissionAllowSession PermissionDecision = "allow_session"
	// PermissionAllowPersistent admits this call AND records a TTL=persistent
	// rule mirrored to the KV store. The rule survives process restarts until
	// the operator revokes it via /permissions.
	PermissionAllowPersistent PermissionDecision = "allow_persistent"
)

// permCallbackKey is the context key for the per-turn ask callback. The WS
// handler installs a fresh one each turn (it captures that turn's context for
// cancellation/timeout).
type permCallbackKey struct{}

type confirmEveryCallKey struct{}

// WithConfirmEveryCall binds the predicate ModeStrict is enforced through
// (W-B-20): when it reports true, Authorize turns a guard ALLOW into a prompt.
//
// # Why a predicate and not a bool
//
// The permission mode is live. The reader goroutine writes permModeState the
// instant a set_mode frame arrives, precisely so a mid-turn switch reaches tool
// calls already in flight (including a sub-agent's). A bool captured when the
// turn context was built would freeze the mode at turn start, which is the one
// property the whole permModeState design exists to avoid — and it would freeze
// it in the LOOSE direction for anyone who switched INTO strict mid-turn.
//
// # Why this is the only widening-shaped seam here that cannot widen
//
// The predicate is consulted in exactly one place and its only effect is
// Allow -> Prompt (guard.ConfirmPrompt). There is no branch in which returning
// true admits something, and returning false restores the pre-W-B-20 behaviour
// byte for byte. An unbound predicate — every sub-agent context, every test,
// the whole SSE path — is false.
func WithConfirmEveryCall(ctx context.Context, confirm func() bool) context.Context {
	if confirm == nil {
		return ctx
	}
	return context.WithValue(ctx, confirmEveryCallKey{}, confirm)
}

// confirmEveryCall reports whether this execution scope confirms even allowed
// calls. Unbound = false, which is what keeps every non-WS caller unchanged.
func confirmEveryCall(ctx context.Context) bool {
	fn, ok := ctx.Value(confirmEveryCallKey{}).(func() bool)
	return ok && fn != nil && fn()
}

// approvalContext bundles an approval Manager + the session ID under which
// approvals should be recorded / matched for this connection. Installed once
// per WS connection by bootstrap/HTTP server; absent on the SSE/static path.
type approvalContext struct {
	Manager   *approval.Manager
	SessionID string
}

type approvalManagerKey struct{}

// WithApprovalManager binds manager (and the session ID approvals are scoped
// to) to ctx so subsequent Authorize calls can match/record against it. A nil
// manager or empty sessionID is a no-op (the SSE/static path).
func WithApprovalManager(ctx context.Context, manager *approval.Manager, sessionID string) context.Context {
	if manager == nil || sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, approvalManagerKey{}, approvalContext{Manager: manager, SessionID: sessionID})
}

func approvalFromContext(ctx context.Context) (approvalContext, bool) {
	v, ok := ctx.Value(approvalManagerKey{}).(approvalContext)
	return v, ok && v.Manager != nil && v.SessionID != ""
}

// WithPermissionCallback installs a callback the GuardedTool layer consults when
// the static PermissionProfile returns Prompt. The callback BLOCKS the calling
// goroutine (the tool run, inside the ADK ReAct loop) until the user responds —
// that is intended: the turn pauses for approval. The reader side (WS read
// loop) delivers the response independently, so the connection can still
// receive frames while the turn is blocked.
//
// When ask is nil or no callback is bound, behavior is unchanged: Authorize is
// the static guard. This is the SSE path — interactive permissions are WS-only.
func WithPermissionCallback(ctx context.Context, ask func(PermissionRequest) PermissionDecision) context.Context {
	if ask == nil {
		return ctx
	}
	return context.WithValue(ctx, permCallbackKey{}, ask)
}

// permissionCallback returns the bound ask callback, if any.
func permissionCallback(ctx context.Context) (func(PermissionRequest) PermissionDecision, bool) {
	cb, _ := ctx.Value(permCallbackKey{}).(func(PermissionRequest) PermissionDecision)
	return cb, cb != nil
}

// scopeFromAction converts a guard.Action to the matching approval.Scope. For
// shell_run the command is parsed so the scope identifies the exact program,
// argument vector and redirections; otherwise the action's tool/fs/net fields
// are mirrored directly. A shell command that cannot be read, or that carries
// more than one executable segment, cannot become an approval scope
// (fail-closed — the user cannot pre-approve a chained command).
//
// # Why ParseCommandList and not Parse (W-B-04 / W-B-06)
//
// It used the STRICT lexer, and that was wrong in both directions at once.
//
// Too strict on word CONTENT: Parse rejects globs and $VAR because the rule
// engine cannot honestly match a word whose value it cannot see. Correct for
// rules; here a scope error is a hard DenyErr, so `ls *.go` under a glob
// profile was refused OUTRIGHT — the guard said Prompt and no prompt was ever
// shown. That also made the approval cache unreachable for the whole class of
// commands most likely to be re-run.
//
// Too loose on STRUCTURE: Parse's lexer never emits ";", so `ls; rm -rf /`
// came back as one segment whose program word is `ls;`. Subshell parens and a
// raw newline got through the same way. This function's own doc claimed to
// refuse chains, and CLAUDE.md cited that claim as the last line of defence
// keeping a chain from reaching an interactive approval; it was measured false
// for three spellings.
//
// ParseCommandList is lenient about content and strict about structure, which
// is exactly the pair a scope needs. Whitespace folding and quote stripping —
// W-B-06's "same command, same cache entry" — then come for free, because the
// scope is built from PARSED WORDS. Argument order is deliberately not
// normalized: the spec's warning is that under-normalization is an annoyance
// while over-normalization is a security hole, and two orders are two commands.
//
// # Why the reader is chosen by action.Interpreter (B-5)
//
// It was not, and that was the over-normalization the paragraph above warns
// about, arriving one commit after the warning was quoted as satisfied. The
// same batch taught guard.segmentsFor to read a PowerShell command with the
// PowerShell reader and left this function calling ParseCommandList
// unconditionally, so the POSIX reader ate the backslashes:
//
//	Remove-Item -Recurse C:\temp   ->  Program=remove-item Prefix=[-Recurse C:temp]
//	Remove-Item -Recurse C:temp    ->  Program=remove-item Prefix=[-Recurse C:temp]
//
// Two different directories in PowerShell, one approval scope. Approving the
// relative one for the session admitted the absolute one with no callback
// consulted at all — measured end to end.
//
// The SECOND half of the same divergence was a silent DENIAL: any PowerShell
// command ending in `\` (`Get-ChildItem C:\` is the ordinary spelling) made the
// POSIX reader report a trailing escape, and a scope error here is a hard
// DenyErr raised BEFORE the callback. The guard said Prompt and the user never
// saw one — the exact failure this function's reader was changed to fix, on the
// language the change did not reach. Both disappear with one shared reader.
//
// The language ALSO goes into the scope (approval.Scope.Interpreter), because
// picking the right reader stops two spellings from colliding within a language
// and does nothing about the same text meaning different things across two.
func scopeFromAction(action guard.Action) (approval.Scope, error) {
	scope := approval.Scope{
		Tool:  action.Tool,
		FSOp:  action.FS.Op,
		Paths: append([]string(nil), action.FS.Paths...),
		Host:  strings.ToLower(action.NetHost),
	}
	if action.Shell == "" {
		return scope, nil
	}
	segs, err := execpolicy.ParseCommandListFor(action.Interpreter, action.Shell)
	if err != nil {
		return approval.Scope{}, err
	}
	if len(segs) != 1 {
		return approval.Scope{}, fmt.Errorf("approval: shell scope requires one executable segment")
	}
	scope.Interpreter = execpolicy.CommandLanguage(action.Interpreter)
	scope.Program = segs[0].Program
	scope.Prefix = append([]string(nil), segs[0].Args...)
	for _, r := range segs[0].Redirects {
		scope.Redirects = append(scope.Redirects, r.Operator+" "+r.Target)
	}
	// A command that executes a script file carries the script's content hash
	// too, so an approval for "run install.sh" stops applying the moment
	// install.sh changes. See approval.Scope.ScriptHash.
	scope.ScriptHash = hashScriptForCommand(action.Shell, action.Workdir)
	return scope, nil
}

// auditPermission writes a structured audit record for a permission decision.
//
// TWO sinks, deliberately independent (S6):
//
//   - slog, the live view. Only safe fields reach it: tool name, decision,
//     source and a fixed reason_code drawn from a closed vocabulary. The raw
//     argsJSON, the guard's Reason text, FS paths, shell commands and hosts are
//     omitted so the log cannot leak sensitive input. The slog default handler
//     is the redacting one installed by observe/log at bootstrap, so this
//     remains defense-in-depth rather than the only layer.
//   - the durable sink bound by WithPermissionAuditSink, the archive. It
//     additionally receives the session/agent identity and a truncated digest
//     of the command or paths, because "deny shell_run" repeated four hundred
//     times answers nothing about which one mattered. The digest is redacted by
//     the sink before it is persisted.
//
// They are separate calls because they fail independently: a quiet log level
// kills the line, a full disk kills the row, and folding them together would
// lose both at once. The action is passed whole (rather than just its name)
// solely so the digest can be derived; slog still sees only the name.
func auditPermission(ctx context.Context, action guard.Action, decision, source, reasonCode string) {
	attrs := []slog.Attr{
		slog.String("tool", action.Tool),
		slog.String("decision", decision),
		slog.String("source", source),
	}
	if reasonCode != "" {
		attrs = append(attrs, slog.String("reason_code", reasonCode))
	}
	slog.LogAttrs(ctx, slog.LevelInfo, "permission decision", attrs...)

	sessionID, agentID := auditIdentity(ctx)
	recordPermissionAudit(ctx, PermissionAuditRecord{
		SessionID:  sessionID,
		AgentID:    agentID,
		Tool:       action.Tool,
		Decision:   decision,
		Source:     source,
		ReasonCode: reasonCode,
		CmdDigest:  auditDigest(action),
	})
}

// explainDecision renders a guard Decision as the one-line reason a user sees.
//
// It exists because execpolicy's Justification -- the human-written "why" on
// the rule that fired -- had zero readers outside internal/guard. checkShell
// dutifully copied it into Decision.Justification and every exit in Authorize
// then wrote only Decision.Reason into DenyErr, so the explanation died at the
// tool boundary. A rule engine whose explanations never reach the person being
// denied is not explainable, whatever its structs contain.
//
// Reason says WHAT matched ("deny flag matched"); Justification says why the
// operator wrote that rule ("real-CLI e2e tests cost money"). The second is
// the useful half and it is optional, so it is appended in parentheses only
// when present.
//
// A1/S06#2「规则结果可解释」的实现在这里；台账引的是驱动它的那条测试。
func explainDecision(dec guard.Decision) string {
	if dec.Justification == "" {
		return dec.Reason
	}
	if dec.Reason == "" {
		return dec.Justification
	}
	return dec.Reason + " (" + dec.Justification + ")"
}

// Authorize checks the acting agent's PermissionProfile against the action and,
// when the static profile returns Prompt, consults the approval manager and the
// permission callback (in that order). It is the single consultation point used
// by every GuardedTool check site (tool-name, fs, shell, net dimensions), so
// interactive permission logic lives in exactly one place.
//
// Resolution order:
//  1. No profile bound       -> DenyErr (fail-closed). Callback/manager are NOT consulted.
//  2. guard.Check verdict    -> Allow: allow. Structural HardDeny (Overridable=false:
//     catastrophic mass deletion, shell metachar, execpolicy
//     parse-error, unknown shell policy, unknown execpolicy
//     verdict): DenyErr firewall — callback/manager MUST NOT
//     override. Overridable HardDeny (anything a profile merely
//     has an opinion about, INCLUDING a denylist match and an
//     empty MCP allowlist) + Prompt: continue to escalation.
//  3. Approval manager hit   -> allow (a prior session/persistent rule for this scope).
//  4. Callback resolves      -> honor the decision:
//     - allow            -> allow
//     - always_allow     -> record TTL=session rule + allow
//     - allow_session    -> record TTL=session rule + allow
//     - allow_persistent -> record TTL=persistent rule + allow
//     - deny/unknown     -> DenyErr
//  5. No callback            -> DenyErr (the static behavior; SSE path).
//
// argsJSON is carried in the PermissionRequest.Args for display (it matches the
// tool_call frame's ToolArgs). It is NEVER written to the audit log. Returns
// nil when the action is allowed.
//
// CRITICAL: the structural HardDeny branch is the security firewall. YOLO/auto
// override ONLY overridable (profile-policy) HardDenies, and they do so inside
// the callback (resolvePermissionMode) — which structural HardDenies never
// reach because this branch returns before consulting it.
func Authorize(ctx context.Context, action guard.Action, argsJSON string) error {
	// S8: refuse names that no registered tool of this agent answers to.
	//
	// Measured before this check existed: with Tools.Allow=["*"] and a bound
	// callback, authorizing "fs_mkdir" returned nil after consulting the
	// callback exactly once — i.e. a phantom name reached the operator as a
	// clickable Allow dialog for a tool nothing can execute.
	//
	// Placed FIRST because the refusal is STRUCTURAL rather than policy-based:
	// it must not depend on any policy state being present or well-formed. The
	// observable consequence is the fail-closed path — when no profile is
	// bound at all, this still reports the real cause ("not a registered
	// tool") instead of the generic missing-profile denial, which is the
	// difference between a diagnosable wiring bug and a dead end. Note that
	// merely moving this below the profile lookup does NOT reopen the dialog
	// hazard above (both orderings return before the callback); that was
	// verified by moving it and watching these tests stay green, so the
	// ordering is justified on the narrower ground stated here.
	//
	// toolreg.Check is a no-op when no set is bound (sub-agents and tests that
	// never call WithRegistered), so this cannot fail closed on a caller that
	// simply does not participate.
	if err := toolreg.Check(ctx, action.Tool); err != nil {
		auditPermission(ctx, action, "deny", "unregistered_tool", "not_registered")
		return &DenyErr{Reason: err.Error()}
	}
	if err := CheckRolePolicy(ctx, action); err != nil {
		auditPermission(ctx, action, "deny", "role_policy", "role_denied")
		return err
	}
	prof, ok := ProfileFromContext(ctx)
	if !ok {
		auditPermission(ctx, action, "deny", "fail_closed", "missing_profile")
		return &DenyErr{Reason: "no permission profile in context"}
	}

	// (0) Plan mode firewall: 只读模式拒绝任何不在 planAllowedTools 的工具。
	//     这条与 orchestrator 的 filterPlanTools（Task 11）是两层独立防线：
	//     filter 在 ToolsNode 配置时收窄、Authorize 在工具调用时 fail-closed。
	//     task_cancel 在 forcePromptTools 里，会先在 (1) 触发 callback；但 Plan
	//     模式下 callback 也会 deny（resolvePermissionMode 返回 deny+resolved），
	//     所以 Plan 下 task_cancel 必拒绝，与白名单一致。
	if PlanModeActive(ctx) && !guard.PlanToolAllowed(action.Tool) {
		auditPermission(ctx, action, "deny", "plan_mode", "plan_filter")
		return &DenyErr{Reason: "tool " + action.Tool + " not available in plan mode"}
	}

	// (1) force-prompt tools: 永远走 callback，跳过 approval.Manager 与 guard.Check
	//     —— 即便 profile.Tools.Allow=["*"]，即便之前用 PermissionAlwaysAllow 应答过，
	//     下次同一调用仍必须 prompt（已决策约束 §5）。SSE 没 callback → fail-closed。
	//     PermissionAlwaysAllow/PermissionAllowSession 不会进 approval.Manager，
	//     保证 force-prompt 工具永远不进持久化 allowlist。
	if isForcePromptTool(action.Tool) {
		ask, hasCallback := permissionCallback(ctx)
		if !hasCallback {
			auditPermission(ctx, action, "deny", "force_prompt", "no_callback")
			return &DenyErr{Reason: "tool requires explicit approval"}
		}
		req := PermissionRequest{
			Tool: action.Tool, Args: argsJSON,
			Reason:      "tool requires explicit approval",
			ForcePrompt: true,
		}
		switch ask(req) {
		case PermissionAllow, PermissionAlwaysAllow, PermissionAllowSession, PermissionAllowPersistent:
			// 故意不调 approval.Manager.Record：force-prompt 每次都要问。
			auditPermission(ctx, action, "allow", "force_prompt", "")
			return nil
		default:
			auditPermission(ctx, action, "deny", "force_prompt", "user_denied")
			return &DenyErr{Reason: req.Reason}
		}
	}

	dec := guard.New().Check(prof, action)
	// ModeStrict (W-B-20): the only place a guard ALLOW becomes a question.
	//
	// Rewriting the verdict here rather than adding a branch to the Allow case
	// below is what makes the mode work at all: everything the escalation path
	// already does — the approval-manager short-circuit that stops the same
	// confirmed command asking twice, allow_session / allow_persistent
	// recording, the audit source, the force-prompt and plan-mode exits that
	// ran BEFORE this line — applies unchanged. A parallel prompt branch would
	// have had to reimplement each of those, and a strict mode with no memory
	// is a strict mode nobody leaves switched on.
	//
	// Placed AFTER guard.Check so it can only ever tighten: a Prompt stays a
	// Prompt and a HardDeny of either tier is untouched, so strict mode cannot
	// downgrade the structural floor into something a callback may answer.
	strictConfirm := false
	if dec.Verdict == guard.Allow && confirmEveryCall(ctx) {
		dec = guard.ConfirmPrompt("strict mode: every tool call is confirmed, " +
			"including ones the permission profile already allows")
		strictConfirm = true
	}
	// profileDenied marks an OVERRIDABLE profile-policy HardDeny routed through
	// the shared escalation path (approval manager -> callback) so the interactive
	// mode gate in resolvePermissionMode can decide (YOLO/Auto override; others
	// deny silently). Prompt sets it false (normal escalation).
	profileDenied := false
	switch dec.Verdict {
	case guard.Allow:
		auditPermission(ctx, action, "allow", "static_profile", "")
		return nil
	case guard.HardDeny:
		if !dec.Overridable {
			// Structural firewall (catastrophic mass deletion, shell metachar,
			// execpolicy parse-error, unknown shell policy, unknown execpolicy
			// verdict): never overridable — not by the approval manager, not by
			// the callback, not by YOLO/auto.
			//
			// A denylist match and an empty MCP allowlist are NOT in this set,
			// though this comment used to claim they were. Both are profile
			// policy, both arrive here with Overridable=true, and both therefore
			// take the escalation path below. Reading them as structural makes
			// yolo look narrower than it is.
			auditPermission(ctx, action, "deny", "hard_deny", "firewall")
			return &DenyErr{Reason: explainDecision(dec)}
		}
		// Overridable profile-policy deny: YOLO/Auto may override via the callback
		// (resolvePermissionMode gates by mode); the SSE path (no callback) and
		// default/allow-edits/plan modes still deny. Falls through to the shared
		// escalation path with ProfileHardDeny=true.
		profileDenied = true
	case guard.Prompt:
		if !dec.Promptable {
			auditPermission(ctx, action, "deny", "guard", "unpromptable")
			return &DenyErr{Reason: "guard returned Prompt without Promptable"}
		}
	default:
		auditPermission(ctx, action, "deny", "guard", "unknown_verdict")
		return &DenyErr{Reason: "unknown guard verdict"}
	}

	// (3) Approval manager: short-circuit on a prior session/persistent rule.
	//
	// A scope that cannot be derived is a hard refusal for a GENUINE prompt —
	// the shape scopeFromAction's own doc explains at length, where a silent
	// denial once hid behind it. It must NOT be one for a call strict mode
	// rewrote: the guard said Allow, and a UX mode whose promise is "you get
	// asked" turning that into a refusal the user never sees is a regression the
	// mode introduced, not a policy anyone chose. Multi-segment commands
	// (`echo a && echo b`, which guard evaluates per segment and allows) landed
	// there with asked=0 and an internal error string.
	//
	// The memory is what is actually lost. Without a scope there is nothing to
	// look up and nothing to store, so a confirmed call is honoured ONCE and
	// "always allow" degrades to "allow" — which is the conservative reading of
	// a button whose scope nobody can write down.
	scope, scopeErr := scopeFromAction(action)
	if scopeErr != nil && !strictConfirm {
		auditPermission(ctx, action, "deny", "approval_scope", "scope_error")
		return &DenyErr{Reason: "approval scope: " + scopeErr.Error()}
	}
	scoped := scopeErr == nil
	if ac, ok := approvalFromContext(ctx); ok && scoped {
		if hit, _ := ac.Manager.Match(ac.SessionID, scope, time.Now()); hit {
			auditPermission(ctx, action, "allow", "approval_manager", "")
			return nil
		}
	}

	// (4) No prior approval. Consult the interactive callback when one is bound.
	ask, hasCallback := permissionCallback(ctx)
	if !hasCallback {
		auditPermission(ctx, action, "deny", "no_callback", "static_denied")
		return &DenyErr{Reason: explainDecision(dec)}
	}
	decision := ask(PermissionRequest{
		Tool: action.Tool, Args: argsJSON, Reason: explainDecision(dec),
		ProfileHardDeny: profileDenied,
		Shell:           action.Shell,
		Workdir:         action.Workdir,
	})
	switch decision {
	case PermissionAllow:
		source := "interactive_once"
		if profileDenied {
			source = "mode_override"
		}
		auditPermission(ctx, action, "allow", source, "")
		return nil
	case PermissionAlwaysAllow, PermissionAllowSession:
		if !scoped {
			// Confirmed, but unstorable — see the scopeErr branch above.
			auditPermission(ctx, action, "allow", "interactive_once", "unscopable")
			return nil
		}
		if ac, ok := approvalFromContext(ctx); ok {
			rule := approval.Rule{ID: newApprovalID(), Action: action.Tool, Scope: scope, TTL: approval.TTLSession, Source: approval.SourceUser, ExpiresAt: approvalExpiry(approval.TTLSession, time.Now())}
			if err := ac.Manager.Record(ac.SessionID, rule); err != nil {
				auditPermission(ctx, action, "deny", "approval_record", "record_error")
				return &DenyErr{Reason: err.Error()}
			}
			auditPermission(ctx, action, "allow", "interactive_session", "")
			return nil
		}
		auditPermission(ctx, action, "deny", "approval_record", "no_manager")
		return &DenyErr{Reason: "approval manager unavailable"}
	case PermissionAllowPersistent:
		if !scoped {
			auditPermission(ctx, action, "allow", "interactive_once", "unscopable")
			return nil
		}
		if ac, ok := approvalFromContext(ctx); ok {
			rule := approval.Rule{ID: newApprovalID(), Action: action.Tool, Scope: scope, TTL: approval.TTLPersistent, Source: approval.SourceUser, ExpiresAt: approvalExpiry(approval.TTLPersistent, time.Now())}
			if err := ac.Manager.Record(ac.SessionID, rule); err != nil {
				auditPermission(ctx, action, "deny", "approval_record", "record_error")
				return &DenyErr{Reason: err.Error()}
			}
			auditPermission(ctx, action, "allow", "interactive_persistent", "")
			return nil
		}
		auditPermission(ctx, action, "deny", "approval_record", "no_kv")
		return &DenyErr{Reason: "persistent approval store unavailable"}
	default:
		auditPermission(ctx, action, "deny", "interactive", "user_denied")
		return &DenyErr{Reason: explainDecision(dec)}
	}
}

// AuthorizeApprovalRequired is the mandatory-approval variant of Authorize,
// used by tools registered via NewApprovalGuardedTool (e.g. GitHub mutations).
// Unlike Authorize, it ALWAYS prompts the user via the callback and cannot be
// short-circuited by:
//   - the static PermissionProfile (even Tools.Allow=["*"]),
//   - the approval.Manager (no prior session/persistent rule applies),
//   - YOLO / auto modes (the callback still has to explicitly return Allow),
//   - PermissionAlwaysAllow / PermissionAllowSession / PermissionAllowPersistent
//     (all three are rejected — mandatory tools require per-call approval).
//
// When no callback is bound (the SSE path), it fails closed with a DenyErr.
// argsJSON is carried in PermissionRequest.Args for display. The
// PermissionRequest.ApprovalRequired field is set to true so the TUI/WS can
// render the appropriate UX (hide "always allow", etc.).
func AuthorizeApprovalRequired(ctx context.Context, action guard.Action, argsJSON string) error {
	// S8, same structural refusal as in Authorize and for a sharper reason:
	// this path prompts UNCONDITIONALLY, so without this line a phantom name
	// is guaranteed to reach the operator as an approval dialog for a tool
	// that cannot be executed by anything.
	if err := toolreg.Check(ctx, action.Tool); err != nil {
		return &DenyErr{Reason: err.Error()}
	}
	if err := CheckRolePolicy(ctx, action); err != nil {
		return err
	}
	prof, ok := ProfileFromContext(ctx)
	if !ok {
		return &DenyErr{Reason: "no permission profile in context"}
	}
	if PlanModeActive(ctx) && !guard.PlanToolAllowed(action.Tool) {
		return &DenyErr{Reason: "tool " + action.Tool + " not available in plan mode"}
	}
	ask, hasCallback := permissionCallback(ctx)
	if !hasCallback {
		return &DenyErr{Reason: "tool requires explicit approval"}
	}
	reason := "tool requires explicit approval"
	if d := guard.New().Check(prof, action); d.Reason != "" {
		reason = d.Reason
	}
	req := PermissionRequest{
		Tool:             action.Tool,
		Args:             argsJSON,
		Reason:           reason,
		ApprovalRequired: true,
	}
	switch ask(req) {
	case PermissionAllow:
		return nil
	default:
		// PermissionDeny, PermissionAlwaysAllow, PermissionAllowSession,
		// PermissionAllowPersistent — ALL are rejected for mandatory tools.
		// always_allow cannot bypass per-call approval; the caller must fix
		// the TUI to hide that option for ApprovalRequired requests.
		return &DenyErr{Reason: reason}
	}
}

// newApprovalID is the per-rule identifier the UI uses for revoke. Uses
// UnixNano so IDs are unique within a process; the approval manager never
// enforces uniqueness, but a duplicate ID would surface as "rule X not found"
// when revoking only the first match.
func newApprovalID() string {
	return fmt.Sprintf("approval-%d", time.Now().UnixNano())
}

// --- SecureProcessFactory re-exports (Task 14) ---
//
// thin re-exports so production callers stay in the tools namespace while the
// secproc package remains a leaf with no dependency on tools. tools.init
// registers the real Authorizer below; secproc.Launch uses it via the
// Authorizer seam.

// WithSecureProcessFactory binds f to ctx via secproc.WithFactory so any
// downstream Launch call can find it. A nil f is a no-op (Launch then fails
// closed).
//
// There is deliberately no matching FromContext re-export any more. There was
// one, and its only consumer was shell_run's "if a factory is bound, use it,
// otherwise spawn the command myself" branch — the second spawn implementation
// W-B-02 deleted. Reading the factory back at a call site is the shape of that
// mistake: callers spawn through LaunchSecureProcess, which reads it once and
// fails closed when it is absent.
func WithSecureProcessFactory(ctx context.Context, f secproc.Factory) context.Context {
	return secproc.WithFactory(ctx, f)
}

// LaunchSecureProcess is the canonical spawn entry point. Every call site
// that used to call exec.CommandContext directly MUST delegate here so the
// Authorize firewall is enforced uniformly.
func LaunchSecureProcess(ctx context.Context, spec secproc.SecureProcessSpec) (*secproc.StartedProcess, error) {
	return secproc.Launch(ctx, spec)
}

func init() {
	// Bind the production Authorize once at process start. secproc.Launch
	// fails closed before this runs (currentAuthorizer == nil → ErrNoAuthorizer)
	// so a test that imports secproc without importing tools cannot accidentally
	// bypass the firewall.
	secproc.RegisterAuthorizer(func(ctx context.Context, action guard.Action, argsJSON string) error {
		return Authorize(ctx, action, argsJSON)
	})
}

// --- shell.Manager context seam (Task 20) ---
//
// ShellV2Tools reads the *shell.Manager bound here so the v2 surface (Start /
// Read / Write / Wait / Cancel / task_* family) can manage persistent sessions
// without each tool looking up the manager itself. Bootstrap binds one
// manager per process; tests inject a fresh one per test.

type shellManagerKey struct{}

// WithShellManager binds manager to ctx so ShellV2Tools can find it. A nil
// manager is a no-op (the v2 tools then return "runtime unavailable").
func WithShellManager(ctx context.Context, manager *shell.Manager) context.Context {
	if manager == nil {
		return ctx
	}
	return context.WithValue(ctx, shellManagerKey{}, manager)
}

// ShellManagerFromContext reads back a manager bound by WithShellManager.
func ShellManagerFromContext(ctx context.Context) (*shell.Manager, bool) {
	m, ok := ctx.Value(shellManagerKey{}).(*shell.Manager)
	return m, ok && m != nil
}

// RequireApproval forces a permission prompt for destructive actions
// (revert_turn, future: shell_run with rm, etc.) REGARDLESS of the static
// profile or session allowlist. 必修项 E.
//
// Semantics:
//   - No profile bound              -> DenyErr (fail-closed, same as Authorize).
//   - No callback bound (SSE/static) -> DenyErr (destructive actions cannot
//     be approved without an interactive prompt).
//   - Callback returns PermissionAllow       -> nil (this one call proceeds).
//   - Callback returns PermissionAlwaysAllow -> nil (this one call proceeds;
//     the session allowlist is NOT updated, so the NEXT call still prompts —
//     destructive actions must NEVER be sticky-approved).
//   - Callback returns PermissionDeny / unknown -> DenyErr.
//   - Callback blocked by cancel / timeout -> the callback itself returns
//     PermissionDeny (the WS handler installs a callback that does this), so
//     RequireApproval surfaces DenyErr — fail-closed.
//
// argsJSON / reason are carried in the PermissionRequest for the callback's
// display (the WS handler builds the TUI prompt from them).
func RequireApproval(ctx context.Context, req PermissionRequest) error {
	if _, ok := ProfileFromContext(ctx); !ok {
		return &DenyErr{Reason: "no permission profile in context"}
	}
	ask, hasCallback := permissionCallback(ctx)
	if !hasCallback {
		return &DenyErr{Reason: "destructive action requires interactive approval (no callback bound)"}
	}
	// D3: mark the request as Force so the WS-installed callback knows to SKIP
	// interactive-mode auto-resolution (yolo / allow-edits / auto). The callback
	// reads req.Force and, when true, always emits an interactive prompt instead
	// of resolving the mode first.
	req.Force = true
	switch ask(req) {
	case PermissionAllow, PermissionAlwaysAllow:
		return nil
	default:
		return &DenyErr{Reason: req.Reason}
	}
}

// approvalExpiry turns a rule's TTL class into a concrete deadline.
//
// The manager expires rules by comparing ExpiresAt, and can only act on what
// it is given: before this existed, neither recording site set the field, so
// every approval -- including the ones the user was asked to grant "for this
// session" -- outlived the session and every session after it.
//
// TTLOnce used to return zero, on the reasoning that such a rule "is consumed
// at the prompt and never recorded, so a rule carrying it is a bug elsewhere".
// W-B-12 made that false in the same batch that wrote it: request_permission
// records a TTLOnce rule for a call that has not happened yet, and it is the
// DEFAULT scope. Manager.expireLocked does nothing with a zero ExpiresAt, so an
// unconsumed pre-emptive grant outlived the eight-hour "session" one — the
// narrow choice living longer, in wall-clock terms, than the wide one.
//
// It gets the same eight-hour bound rather than a shorter one because the
// question it answers is the same: how long may a decision the operator made
// keep applying while they are not looking. Consumption still ends it sooner;
// this only stops "never used" from meaning "never expires".
//
// A zero return survives for a TTL class this function does not recognise,
// which is the fail-safe direction only because Manager.Match still requires an
// exact scope hit — a rule nothing matches grants nothing.
func approvalExpiry(ttl approval.TTL, now time.Time) time.Time {
	switch ttl {
	case approval.TTLOnce, approval.TTLSession:
		// Long enough not to interrupt a working session, short enough that a
		// forgotten terminal does not keep granting a decision made yesterday.
		return now.Add(8 * time.Hour)
	case approval.TTLPersistent:
		// "Persistent" is a convenience, not a permanent grant: the operator
		// is re-asked eventually, and a stolen or stale rule stops working.
		return now.Add(30 * 24 * time.Hour)
	default:
		return time.Time{}
	}
}
