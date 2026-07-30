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
	pending map[string]chan tools.PermissionDecision
	nextID  uint64
}

func newPermTracker() *permTracker {
	return &permTracker{pending: make(map[string]chan tools.PermissionDecision)}
}

// newID returns a unique per-connection permission-request id.
func (p *permTracker) newID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	return strconv.FormatUint(p.nextID, 10)
}

// register records ch as the recipient for id's response.
func (p *permTracker) register(id string, ch chan tools.PermissionDecision) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending[id] = ch
}

// take pops (and removes) the channel for id, or returns nil if absent / already
// taken. Removal is unconditional so a late/second response cannot deliver to a
// channel whose ask has already returned.
func (p *permTracker) take(id string) chan tools.PermissionDecision {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := p.pending[id]
	delete(p.pending, id)
	return ch
}

// deliver attempts to send decision to the pending channel for id. It is
// non-blocking (the channel is buffered size 1): if the ask already returned
// (timeout/cancel) the entry is gone and the send is a no-op.
func (p *permTracker) deliver(id string, decision tools.PermissionDecision) {
	if ch := p.take(id); ch != nil {
		select {
		case ch <- decision:
		default:
		}
	}
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true }, // local trust; auth middleware gates access
}

// permModeState is the live, concurrency-safe holder for a connection's
// interactive permission mode (default|allow-edits|yolo|auto) + the ModeAuto
// risk ceiling. The reader goroutine updates it on every set_mode frame so a
// mode switch takes effect immediately — even while a turn is running and the
// main loop (the only drainer of the frames channel) is blocked. The permission
// callback and statusFrame read it. Holding it behind a mutex rather than as
// plain connSession fields is what lets set_mode bypass the frames channel and
// reach mid-turn tool calls (notably inside sub-agents).
type permModeState struct {
	mu   sync.RWMutex
	mode guard.PermissionMode
	auto int
}

func (p *permModeState) set(m guard.PermissionMode, auto int) {
	p.mu.Lock()
	p.mode = m
	p.auto = auto
	p.mu.Unlock()
}

// get returns the current mode + auto threshold.
func (p *permModeState) get() (guard.PermissionMode, int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.mode, p.auto
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
// value leaves the mode unchanged); the threshold is sticky — a prior value is
// kept unless the frame overrides it, defaulting when still zero.
func (cs *connSession) applySetMode(cf proto.ClientFrame) (oldMode, newMode guard.PermissionMode) {
	oldMode, auto := cs.perm.get()
	mode := oldMode
	if m, ok := guard.NormalizeMode(cf.Mode); ok {
		mode = m
	}
	if cf.AutoThreshold > 0 {
		auto = cf.AutoThreshold
	} else if auto == 0 {
		auto = guard.DefaultAutoThreshold
	}
	cs.perm.set(mode, auto)
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
//            structurally in guard) and out-of-workdir deletion (blocked here).
//            Bypasses profile-policy denies (PolicyHardDeny).
//   - auto:  AI risk-assesses everything (including profile denies and
//            out-of-workdir deletion) except catastrophic mass deletion, which
//            is blocked structurally in guard and fail-safely here.
//   - allow-edits / default: prompt for ordinary denies; deny silently for
//            profile-policy denies (policy="deny" means block, not ask).
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
	models map[string]model.BaseChatModel, req tools.PermissionRequest) (tools.PermissionDecision, bool) {

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
	mode, auto := cs.perm.get()

	// Destructive-deletion gate (profile-independent). Catastrophic mass deletion
	// is already blocked structurally in guard.Check, so this is a fail-safe.
	// YOLO additionally blocks out-of-workdir deletion (per spec); auto lets the
	// AI risk assessment below judge out-of-workdir deletion like anything else,
	// and default/allow-edits/plan surface it as a normal interactive prompt.
	if req.Shell != "" {
		switch guard.ClassifyDestruction(req.Shell, req.Workdir) {
		case guard.DestructionCatastrophic:
			return tools.PermissionDeny, true
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
		threshold := auto
		if threshold <= 0 {
			threshold = guard.DefaultAutoThreshold
		}
		score, ok := assessRisk(ctx, models, cs, req)
		if !ok {
			return tools.PermissionDeny, false // couldn't rate it — ask the user
		}
		if score <= threshold {
			return tools.PermissionAllow, true
		}
		return tools.PermissionDeny, false
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
	models map[string]model.BaseChatModel, req tools.PermissionRequest,
) (tools.PermissionDecision, bool) {
	if req.Force {
		return tools.PermissionDeny, false
	}
	return resolvePermissionMode(ctx, cs, models, req)
}

// assessRisk asks the model to rate a tool call's risk 1-10 for ModeAuto. It
// uses the session's selected model when set, else the first registered model.
// A 15s timeout guards a hung/slow model; on timeout or any error it returns
// (0, false) so the caller falls back to prompting the user. The assessment is
// a single Generate call (the model is idle here: the ReAct loop is paused
// between iterations awaiting the tool's permission verdict).
func assessRisk(ctx context.Context, models map[string]model.BaseChatModel,
	cs *connSession, req tools.PermissionRequest) (int, bool) {

	m := cs.selectModel(models)
	if m == nil {
		// Fall back to any registered model (common when cs.model == "").
		for _, mm := range models {
			m = mm
			break
		}
	}
	if m == nil {
		return 0, false
	}
	askCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := m.Generate(askCtx, []*schema.Message{schema.UserMessage(guard.RiskPrompt(req.Tool, req.Args))})
	if err != nil || resp == nil {
		return 0, false
	}
	return guard.ParseRiskScore(resp.Content)
}
