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
	// allow-edits gate would have denied. Narrowing that divergence is an
	// authorization change, so it is deferred — to S0/W5 (安全底座) Task 7
	// 移交项 C, filed in docs/superpowers/plans/2026-08-03-s0-w5-security.md
	// with the three candidate fixes. W5 owns it because it is the same
	// allow-edits auto-approval surface as that task's other two handovers,
	// not because the code lives here.
	ProfileHardDeny bool
	// Shell carries the shell command for shell_run (empty otherwise) so the
	// interactive mode layer can apply its destructive-deletion gate
	// (guard.ClassifyDestruction) regardless of why the call reached the
	// callback. Workdir is the project root used as the in-scope boundary.
	Shell   string
	Workdir string
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
// shell_run, the command is parsed via execpolicy so the scope identifies the
// exact program + argument prefix; otherwise the action's tool/fs/net fields
// are mirrored directly. A shell command that fails to parse or contains a
// control operator cannot become an approval scope (fail-closed — the user
// cannot pre-approve a chained command).
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
	cmd, err := execpolicy.Parse(action.Shell)
	if err != nil {
		return approval.Scope{}, err
	}
	if len(cmd.Segments) != 1 {
		return approval.Scope{}, fmt.Errorf("approval: shell scope requires one executable segment")
	}
	scope.Program = cmd.Segments[0].Program
	scope.Prefix = append([]string(nil), cmd.Segments[0].Args...)
	return scope, nil
}

// auditPermission writes a structured audit record for a permission decision.
// Only safe fields are emitted: tool name, decision, source and a fixed
// reason_code drawn from a closed vocabulary. The raw argsJSON, the guard's
// Reason text, FS paths, shell commands and hosts are deliberately omitted so
// the audit trail cannot leak sensitive input. The slog default handler is the
// redacting one installed by observe/log at bootstrap, so this remains
// defense-in-depth rather than the only layer.
func auditPermission(ctx context.Context, tool, decision, source, reasonCode string) {
	attrs := []slog.Attr{
		slog.String("tool", tool),
		slog.String("decision", decision),
		slog.String("source", source),
	}
	if reasonCode != "" {
		attrs = append(attrs, slog.String("reason_code", reasonCode))
	}
	slog.LogAttrs(ctx, slog.LevelInfo, "permission decision", attrs...)
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
	if err := CheckRolePolicy(ctx, action); err != nil {
		auditPermission(ctx, action.Tool, "deny", "role_policy", "role_denied")
		return err
	}
	prof, ok := ProfileFromContext(ctx)
	if !ok {
		auditPermission(ctx, action.Tool, "deny", "fail_closed", "missing_profile")
		return &DenyErr{Reason: "no permission profile in context"}
	}

	// (0) Plan mode firewall: 只读模式拒绝任何不在 planAllowedTools 的工具。
	//     这条与 orchestrator 的 filterPlanTools（Task 11）是两层独立防线：
	//     filter 在 ToolsNode 配置时收窄、Authorize 在工具调用时 fail-closed。
	//     task_cancel 在 forcePromptTools 里，会先在 (1) 触发 callback；但 Plan
	//     模式下 callback 也会 deny（resolvePermissionMode 返回 deny+resolved），
	//     所以 Plan 下 task_cancel 必拒绝，与白名单一致。
	if PlanModeActive(ctx) && !guard.PlanToolAllowed(action.Tool) {
		auditPermission(ctx, action.Tool, "deny", "plan_mode", "plan_filter")
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
			auditPermission(ctx, action.Tool, "deny", "force_prompt", "no_callback")
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
			auditPermission(ctx, action.Tool, "allow", "force_prompt", "")
			return nil
		default:
			auditPermission(ctx, action.Tool, "deny", "force_prompt", "user_denied")
			return &DenyErr{Reason: req.Reason}
		}
	}

	dec := guard.New().Check(prof, action)
	// profileDenied marks an OVERRIDABLE profile-policy HardDeny routed through
	// the shared escalation path (approval manager -> callback) so the interactive
	// mode gate in resolvePermissionMode can decide (YOLO/Auto override; others
	// deny silently). Prompt sets it false (normal escalation).
	profileDenied := false
	switch dec.Verdict {
	case guard.Allow:
		auditPermission(ctx, action.Tool, "allow", "static_profile", "")
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
			auditPermission(ctx, action.Tool, "deny", "hard_deny", "firewall")
			return &DenyErr{Reason: dec.Reason}
		}
		// Overridable profile-policy deny: YOLO/Auto may override via the callback
		// (resolvePermissionMode gates by mode); the SSE path (no callback) and
		// default/allow-edits/plan modes still deny. Falls through to the shared
		// escalation path with ProfileHardDeny=true.
		profileDenied = true
	case guard.Prompt:
		if !dec.Promptable {
			auditPermission(ctx, action.Tool, "deny", "guard", "unpromptable")
			return &DenyErr{Reason: "guard returned Prompt without Promptable"}
		}
	default:
		auditPermission(ctx, action.Tool, "deny", "guard", "unknown_verdict")
		return &DenyErr{Reason: "unknown guard verdict"}
	}

	// (3) Approval manager: short-circuit on a prior session/persistent rule.
	scope, err := scopeFromAction(action)
	if err != nil {
		auditPermission(ctx, action.Tool, "deny", "approval_scope", "scope_error")
		return &DenyErr{Reason: "approval scope: " + err.Error()}
	}
	if ac, ok := approvalFromContext(ctx); ok {
		if hit, _ := ac.Manager.Match(ac.SessionID, scope, time.Now()); hit {
			auditPermission(ctx, action.Tool, "allow", "approval_manager", "")
			return nil
		}
	}

	// (4) No prior approval. Consult the interactive callback when one is bound.
	ask, hasCallback := permissionCallback(ctx)
	if !hasCallback {
		auditPermission(ctx, action.Tool, "deny", "no_callback", "static_denied")
		return &DenyErr{Reason: dec.Reason}
	}
	decision := ask(PermissionRequest{
		Tool: action.Tool, Args: argsJSON, Reason: dec.Reason,
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
		auditPermission(ctx, action.Tool, "allow", source, "")
		return nil
	case PermissionAlwaysAllow, PermissionAllowSession:
		if ac, ok := approvalFromContext(ctx); ok {
			rule := approval.Rule{ID: newApprovalID(), Action: action.Tool, Scope: scope, TTL: approval.TTLSession, Source: approval.SourceUser}
			if err := ac.Manager.Record(ac.SessionID, rule); err != nil {
				auditPermission(ctx, action.Tool, "deny", "approval_record", "record_error")
				return &DenyErr{Reason: err.Error()}
			}
			auditPermission(ctx, action.Tool, "allow", "interactive_session", "")
			return nil
		}
		auditPermission(ctx, action.Tool, "deny", "approval_record", "no_manager")
		return &DenyErr{Reason: "approval manager unavailable"}
	case PermissionAllowPersistent:
		if ac, ok := approvalFromContext(ctx); ok {
			rule := approval.Rule{ID: newApprovalID(), Action: action.Tool, Scope: scope, TTL: approval.TTLPersistent, Source: approval.SourceUser}
			if err := ac.Manager.Record(ac.SessionID, rule); err != nil {
				auditPermission(ctx, action.Tool, "deny", "approval_record", "record_error")
				return &DenyErr{Reason: err.Error()}
			}
			auditPermission(ctx, action.Tool, "allow", "interactive_persistent", "")
			return nil
		}
		auditPermission(ctx, action.Tool, "deny", "approval_record", "no_kv")
		return &DenyErr{Reason: "persistent approval store unavailable"}
	default:
		auditPermission(ctx, action.Tool, "deny", "interactive", "user_denied")
		return &DenyErr{Reason: dec.Reason}
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
func WithSecureProcessFactory(ctx context.Context, f secproc.Factory) context.Context {
	return secproc.WithFactory(ctx, f)
}

// SecureProcessFactoryFromContext reads back a Factory bound by
// WithSecureProcessFactory.
func SecureProcessFactoryFromContext(ctx context.Context) (secproc.Factory, bool) {
	return secproc.FromContext(ctx)
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
