// internal/tools/requestpermission.go
//
// W-B-12: asking BEFORE hitting the wall.
//
// EscalateOnSandboxViolation (sandboxescalate.go, next door) is the reactive
// half of the same idea and its shape is the reason this file exists: it can
// only run AFTER a command has already been refused, it raises the sandbox by
// exactly one rung, and it does it at most once per call. A model that knows in
// advance it needs to write one file outside the allowlist has no way to say so
// — its only move is to attempt the write, be refused, and hope the refusal was
// the kind the escalation loop recognises.
//
// # What a grant is, and what it is not
//
// A granted request records an approval RULE for one exact scope — the same
// approval.Scope Authorize computes from the guard.Action a later call will
// produce. Not a glob, not a directory, not a tier: one tool name plus one path
// or one host. That exactness is deliberate and it is also the failure mode to
// watch, because approval.Manager matches with reflect.DeepEqual: a grant whose
// scope does not reproduce the later action's byte for byte is silently inert.
// TestGrantedPermissionAdmitsTheLaterCall is the only thing standing between
// this file and being a dialog that grants nothing — and it only counts because
// it drives the REAL tool afterwards. The first version of it built the later
// guard.Action by hand, which is a shape production never produces: it asserted
// that the approval manager matches what this file records, not that the tool
// records what a real call produces. Both halves were right and the middle was
// not connected, and three of the four ways to spell an fs target were inert.
//
// # The four refusals, and why two of them never ask
//
//	unregistered tool      -> refused without asking. A dialog for a name
//	                          nothing can execute is the S8 failure verbatim.
//	already permitted      -> refused without asking, as a SUCCESS. The model
//	                          gets "you already have this"; putting a dialog in
//	                          front of the operator for a capability they
//	                          already granted teaches them to click through.
//	structural HardDeny    -> refused without asking. That tier is defined as
//	                          the one no mode and no callback crosses, and the
//	                          approval manager is consulted AFTER it in
//	                          Authorize — so a rule recorded here could not
//	                          admit the call even if the operator approved. A
//	                          dialog would be asking for a signature on
//	                          something that cannot take effect.
//	                          DEFENSIVE, and currently unreachable: an fs or net
//	                          action carries no Shell, so checkDestructive and
//	                          checkShell both pass and every remaining dimension
//	                          tops out at an OVERRIDABLE deny. It is here for
//	                          the dimension somebody adds next, on the same
//	                          terms as guard's own unknown-verdict default, and
//	                          TestStructuralFloorIsUnreachableFromTheSupported-
//	                          Dimensions says so out loud rather than leaving
//	                          the branch to look live.
//	operator says no       -> refused, having asked. The ordinary outcome.
//
// # Why RequireApproval and not Authorize
//
// Same reason askEscalation gives: RequireApproval sets req.Force, which
// resolvePermissionRequest checks first and refuses to auto-resolve. yolo,
// allow-edits and auto therefore cannot grant a standing permission on the
// user's behalf, and a transport with no interactive channel (SSE) fails
// closed. A pre-emptive grant that a permission MODE could hand out would be a
// way for the model to write its own profile.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/toolreg"
)

// PermissionRequestDimension names the access dimension a request is about.
// fs and net are kept apart because they are enforced by two different
// authorities — the permission profile for fs, netpolicy.Policy for net — and
// because a grant for one says nothing about the other: "you may read
// vendor/schema.json" is not "you may reach schema.example.com".
type PermissionRequestDimension string

// The dimensions a model may request.
const (
	// DimensionFSRead requests a filesystem READ of one path.
	DimensionFSRead PermissionRequestDimension = "fs_read"
	// DimensionFSWrite requests a filesystem WRITE of one path.
	DimensionFSWrite PermissionRequestDimension = "fs_write"
	// DimensionNet requests outbound access to one host.
	DimensionNet PermissionRequestDimension = "net"
)

// PermissionRequestScope is how long a granted request lasts.
type PermissionRequestScope string

// The scopes a model may request.
const (
	// ScopeOnce admits the NEXT matching call and is then consumed. It is the
	// default, and it is what "本轮" means here: the grant does not survive the
	// call it was asked for.
	ScopeOnce PermissionRequestScope = "once"
	// ScopeSession admits matching calls until the session ends or the rule
	// expires (approvalExpiry bounds it at 8 hours).
	ScopeSession PermissionRequestScope = "session"
)

// permissionRequestArgs is the model-facing argument shape.
type permissionRequestArgs struct {
	Dimension string `json:"dimension"`
	Tool      string `json:"tool"`
	Target    string `json:"target"`
	Scope     string `json:"scope"`
	Reason    string `json:"reason"`
}

// permissionRequestResult is what the model reads back. Granted is the only
// field a caller should branch on; Detail explains every other outcome in terms
// the model can act on, which is the same contract explainSandboxFailure holds
// itself to next door.
type permissionRequestResult struct {
	Granted   bool   `json:"granted"`
	Dimension string `json:"dimension"`
	Tool      string `json:"tool"`
	Target    string `json:"target"`
	Scope     string `json:"scope,omitempty"`
	Detail    string `json:"detail"`
}

// NewRequestPermissionTool builds the request_permission tool (W-B-12).
//
// It takes no constructor arguments: everything it needs — the profile, the
// approval manager, the permission callback, the work root — rides the turn
// context, which is the pattern every authorization-aware tool in this package
// follows. See the file header for the four outcomes.
func NewRequestPermissionTool() *GuardedTool {
	return NewGuardedTool("request_permission", "Request permission",
		"Ask the user for a specific permission BEFORE attempting an action that "+
			"would be refused. Use it when you already know a step needs to read or write "+
			"a path inside the project that the current profile does not cover, or to reach "+
			"a host the network policy does not allow — rather than attempting it, being "+
			"denied, and retrying. Paths OUTSIDE the project root cannot be granted here "+
			"or anywhere else: the fs tools refuse them before permissions are consulted. "+
			"The user is always asked; there is no mode in which this is granted automatically.",
		2*time.Minute,
		params(map[string]*schema.ParameterInfo{
			"dimension": {Type: schema.String, Required: true,
				Enum: []string{string(DimensionFSRead), string(DimensionFSWrite), string(DimensionNet)},
				Desc: "which access dimension"},
			"tool": {Type: schema.String, Required: true,
				Desc: "the exact tool name you will call afterwards (e.g. fs_read, fs_write, web_fetch). " +
					"The grant covers that tool and no other."},
			"target": {Type: schema.String, Required: true,
				Desc: "the single path (fs dimensions) or host (net dimension) you need"},
			"scope": {Type: schema.String, Enum: []string{string(ScopeOnce), string(ScopeSession)},
				Desc: `"once" (default; admits the next matching call only) or "session"`},
			"reason": {Type: schema.String, Required: true,
				Desc: "why this is needed, in one sentence. The user reads it."},
		}),
		SyncStream(runRequestPermission))
}

// runRequestPermission is the tool body. It returns a JSON
// permissionRequestResult on every path, including refusals: a Go error would
// reach the model as a failed tool call, and "the user said no" is an ANSWER to
// the question that was asked, not a malfunction.
func runRequestPermission(ctx context.Context, argsJSON string) (string, error) {
	var a permissionRequestArgs
	if err := ParseArgs(argsJSON, &a); err != nil {
		return "", err
	}
	dim, err := normalizeDimension(a.Dimension)
	if err != nil {
		return "", err
	}
	scope, err := normalizeRequestScope(a.Scope)
	if err != nil {
		return "", err
	}
	a.Tool = strings.TrimSpace(a.Tool)
	a.Target = strings.TrimSpace(a.Target)
	a.Reason = strings.TrimSpace(a.Reason)
	if a.Tool == "" || a.Target == "" {
		return "", fmt.Errorf("request_permission: tool and target are both required")
	}
	if a.Reason == "" {
		return "", fmt.Errorf("request_permission: reason is required — it is the only thing " +
			"the user has to judge the request by")
	}

	res := permissionRequestResult{Dimension: string(dim), Tool: a.Tool, Target: a.Target}

	// S8's refusal, for S8's reason: this path ends in a dialog, so a name no
	// registered tool answers to would put a clickable Allow in front of the
	// operator for something nothing can execute.
	if err := toolreg.Check(ctx, a.Tool); err != nil {
		res.Detail = "no such tool: " + err.Error()
		return marshalRequestResult(res)
	}
	action, err := permissionActionFor(dim, a.Tool, a.Target, WorkRootFromContext(ctx))
	if err != nil {
		// Not a policy refusal: the target is one no approval could admit,
		// because the fs jail refuses it before the guard is ever consulted.
		// Saying so is the whole point — the alternative is a rule that never
		// matches and a model that was told it may proceed.
		res.Detail = "this request cannot be granted by anyone: " + err.Error() + "."
		if dim != DimensionNet {
			res.Detail += " Every fs tool resolves paths against the project root and " +
				"refuses anything outside it before permissions are considered, so no " +
				"approval would let that call through. Work inside the project root, or " +
				"ask the user to run yanshi with a root that contains the path."
		}
		return marshalRequestResult(res)
	}
	pre := preflightRequest(ctx, dim, action)
	switch {
	case pre.unbound != "":
		res.Detail = pre.unbound
		return marshalRequestResult(res)
	case pre.allowed:
		res.Granted = true
		res.Detail = "already permitted by the current " + pre.authority +
			"; no request was needed — go ahead"
		return marshalRequestResult(res)
	case pre.ungrantable != "":
		res.Detail = pre.ungrantable +
			"; this is not something the user can grant — no request was made. " +
			"Find another way to do the task."
		return marshalRequestResult(res)
	}

	req := PermissionRequest{
		Tool:    a.Tool,
		Args:    argsJSON,
		Reason:  requestPermissionPrompt(dim, a.Tool, a.Target, scope, a.Reason, pre.denial),
		Workdir: action.Workdir,
	}
	if err := RequireApproval(ctx, req); err != nil {
		res.Detail = "the user did not approve this request (a prompt that expires, or a " +
			"transport with no interactive channel, counts as a refusal): " + err.Error()
		auditPermissionRequest(ctx, action, false, scope)
		return marshalRequestResult(res)
	}

	if err := recordRequestedPermission(ctx, action, scope); err != nil {
		res.Detail = "the user approved, but the grant could not be recorded (" + err.Error() +
			"); the call will still be asked about individually"
		return marshalRequestResult(res)
	}
	auditPermissionRequest(ctx, action, true, scope)
	res.Granted = true
	res.Scope = string(scope)
	res.Detail = fmt.Sprintf("granted for scope %q; call %s on %q now", scope, a.Tool, a.Target)
	return marshalRequestResult(res)
}

func marshalRequestResult(res permissionRequestResult) (string, error) {
	body, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// normalizeDimension maps the model's string to a dimension, refusing anything
// else rather than defaulting. A silent default here would grant the wrong
// dimension — the one thing "net 与 fs 分维" exists to prevent.
func normalizeDimension(s string) (PermissionRequestDimension, error) {
	switch PermissionRequestDimension(strings.ToLower(strings.TrimSpace(s))) {
	case DimensionFSRead:
		return DimensionFSRead, nil
	case DimensionFSWrite:
		return DimensionFSWrite, nil
	case DimensionNet:
		return DimensionNet, nil
	}
	return "", fmt.Errorf("request_permission: dimension must be one of %q, %q or %q, got %q",
		DimensionFSRead, DimensionFSWrite, DimensionNet, s)
}

// normalizeRequestScope maps the model's string to a scope. An OMITTED scope is
// "once" — the narrow one — because a model that did not think about lifetime
// should not get the long one.
func normalizeRequestScope(s string) (PermissionRequestScope, error) {
	switch v := strings.ToLower(strings.TrimSpace(s)); PermissionRequestScope(v) {
	case "":
		return ScopeOnce, nil
	case ScopeOnce, ScopeSession:
		return PermissionRequestScope(v), nil
	default:
		if v == "turn" {
			// The spec's word for it (本轮). Same rule.
			return ScopeOnce, nil
		}
	}
	return "", fmt.Errorf("request_permission: scope must be %q or %q, got %q",
		ScopeOnce, ScopeSession, s)
}

// permissionActionFor builds the guard.Action a later call will produce.
//
// This is the load-bearing function of the whole file: the recorded approval
// scope is derived from it (scopeFromAction), and approval.Manager matches
// scopes with reflect.DeepEqual, so an Action that differs from the real one in
// any field produces a grant that is inert and says nothing about being inert.
//
// # The fs path goes through the fs tools' own jail, and that is the whole fix
//
// It used to record the model's raw string. Every real fs tool resolves its
// paths through FSTools.abs FIRST and hands Authorize the resolved absolute
// path, so a grant for "outside/x.json" and a call that produced
// "<root>/outside/x.json" were two different scopes and DeepEqual said no —
// silently, which is the failure mode this file's header warns about. Sharing
// resolveWithinRoot removes the second spelling.
//
// The refusal it also inherits is the more important half. The jail runs BEFORE
// Authorize, so a path outside the project root is not a permission question:
// no rule recorded here could admit that call, because the call never reaches
// the guard. Returning the jail's error lets runRequestPermission tell the model
// that instead of minting a rule and reporting granted for a wall that has not
// moved. This is why the ORDER (jail, then authorize) does not need changing —
// and must not change: moving the jail after Authorize would turn a structural
// boundary into a policy question an approval could answer, which would let one
// dialog grant reads anywhere on the machine.
//
// Workdir is taken from the CONTEXT, never from the arguments, for the reason
// guard.Action.Workdir's own doc gives at length: a model that could set the
// boundary could move the line that decides whether its request crosses one.
func permissionActionFor(dim PermissionRequestDimension, toolName, target, workRoot string) (guard.Action, error) {
	action := guard.Action{Tool: toolName, Workdir: workRoot}
	switch dim {
	case DimensionFSRead, DimensionFSWrite:
		if workRoot == "" {
			return guard.Action{}, fmt.Errorf("no project root is bound on this turn, so the path " +
				"a later fs call would resolve to cannot be predicted")
		}
		resolved, err := resolveWithinRoot(workRoot, target)
		if err != nil {
			return guard.Action{}, err
		}
		op := "read"
		if dim == DimensionFSWrite {
			op = "write"
		}
		action.FS = guard.FSWant{Op: op, Paths: []string{resolved}}
	case DimensionNet:
		// Normalized by netpolicy's OWN folding, for the fs branch's reason one
		// dimension over: the host recorded here and the host web_fetch derives
		// from a URL at call time are compared with reflect.DeepEqual, so
		// "API.Example.test:8443" and "api.example.test" have to arrive as one
		// string or the grant is inert.
		action.NetHost = netpolicy.NormalizeHost(target)
		if action.NetHost == "" {
			return guard.Action{}, fmt.Errorf("%q is not a host", target)
		}
		if !slices.Contains(netGrantConsumers(), toolName) {
			return guard.Action{}, fmt.Errorf("%q does not consult network grants; only %s do",
				toolName, strings.Join(netGrantConsumers(), " and "))
		}
	}
	return action, nil
}

// requestPreflight is what a request looks like before anyone is interrupted.
//
// unbound is set when the authority for this dimension is not bound at all
// (nothing can be granted). allowed means it is already permitted. ungrantable
// means refused by something no approval can undo. denial is what the dialog
// tells the operator the current policy says, and is only meaningful when none
// of the other three are.
type requestPreflight struct {
	authority   string
	unbound     string
	allowed     bool
	ungrantable string
	denial      string
}

// preflightRequest asks the authority that will ACTUALLY judge the later call.
//
// The two dimensions have two different authorities and asking the wrong one is
// how the net dimension spent its first release granting nothing:
//
//   - fs is the permission profile, enforced by guard.Check inside Authorize.
//   - net is netpolicy.Policy. web_fetch's profile-based guard.NetHost check was
//     replaced by it in Task 11 so the operator's security.network block applies
//     uniformly to the tool and to the loopback proxy — which means guard.checkNet
//     has no production producer left, and a request_permission that consulted it
//     would report a verdict nothing enforces and record a rule nothing reads.
//     That was measured: the operator approved, the model was told granted=true,
//     and the next web_fetch was judged byte-for-byte as before.
func preflightRequest(ctx context.Context, dim PermissionRequestDimension, action guard.Action) requestPreflight {
	if dim == DimensionNet {
		out := requestPreflight{authority: "network policy"}
		policy, ok := NetworkPolicyFromContext(ctx)
		if !ok {
			out.unbound = "no network policy is bound on this transport, so nothing can be granted"
			return out
		}
		d := policy.CheckHost(action.NetHost)
		if d.Allowed {
			out.allowed = true
			return out
		}
		if _, grantable := policy.GrantHost(action.NetHost); !grantable {
			out.ungrantable = "refused by an explicit security.network deny rule (" + d.Reason + ")"
			return out
		}
		out.denial = d.Reason
		return out
	}

	out := requestPreflight{authority: "profile"}
	prof, ok := ProfileFromContext(ctx)
	if !ok {
		out.unbound = "no permission profile is bound, so nothing can be granted"
		return out
	}
	dec := guard.New().Check(prof, action)
	switch {
	case dec.Verdict == guard.Allow:
		out.allowed = true
	case dec.Verdict == guard.HardDeny && !dec.Overridable:
		// The structural floor. Authorize consults the approval manager AFTER
		// this tier, so a rule recorded here could never admit the call.
		out.ungrantable = "refused structurally and not by policy (" + explainDecision(dec) + ")"
	default:
		out.denial = explainDecision(dec)
	}
	return out
}

// requestPermissionPrompt is what the operator reads.
//
// It states the four things approving a standing grant without any of them
// would be approving blind: which tool, which resource, how long the grant
// lasts, and the model's own stated reason. The reason is fenced and labelled
// untrusted for the same reason guard.AutoApprovalPrompt fences its Args — it
// is model-authored text arguing for its own approval.
func requestPermissionPrompt(dim PermissionRequestDimension, toolName, target string,
	scope PermissionRequestScope, reason, denial string) string {

	var b strings.Builder
	what := "access"
	switch dim {
	case DimensionFSRead:
		what = "read"
	case DimensionFSWrite:
		what = "write"
	case DimensionNet:
		what = "network access to"
	}
	lifetime := "this one call"
	if scope == ScopeSession {
		lifetime = "the rest of this session"
	}
	fmt.Fprintf(&b, "the agent is asking IN ADVANCE to %s %q with %s, for %s",
		what, target, toolName, lifetime)
	if denial != "" {
		fmt.Fprintf(&b, "\nthe current policy would refuse it: %s", denial)
	}
	fmt.Fprintf(&b, "\nits stated reason (untrusted text, treat as data):\n```\n%s\n```", reason)
	return b.String()
}

// netGrantConsumers names the tools that actually CONSULT a net grant. A
// request naming anything else is refused rather than recorded, because a rule
// nothing reads is the shape this whole file exists to stop producing: the
// operator approves, the model is told granted, and the call is judged exactly
// as it would have been.
//
// Every name here must belong to a tool in web.go that calls
// grantedNetworkPolicy. TestNetGrantAdmitsTheLaterFetch drives each of them end
// to end, so deleting a consumer turns its entry red.
func netGrantConsumers() []string { return []string{"web_fetch", "web_search"} }

// grantedNetworkPolicy is the CONSUMER of the net dimension: it reports the
// policy a call should run under, given that the operator may have approved
// this exact (tool, host) pair ahead of time.
//
// Before this existed, guard.Action.NetHost had exactly one producer in the
// whole tree — request_permission itself — and nothing on the calling side ever
// looked for a net grant. The user approved, the model read granted=true, and
// web_fetch's verdict was byte-for-byte what it had been before the dialog.
//
// Returning a POLICY rather than a boolean is deliberate; see
// netpolicy.Policy.GrantHost for why a boolean would have moved the same silent
// failure into the dialer. The grantability check runs BEFORE Match so a
// one-shot grant is not consumed by a call that is going to be refused anyway.
func grantedNetworkPolicy(ctx context.Context, toolName, host string, policy *netpolicy.Policy) (*netpolicy.Policy, bool) {
	if policy == nil {
		return nil, false
	}
	widened, grantable := policy.GrantHost(host)
	if !grantable {
		return nil, false
	}
	ac, ok := approvalFromContext(ctx)
	if !ok {
		return nil, false
	}
	action, err := permissionActionFor(DimensionNet, toolName, host, WorkRootFromContext(ctx))
	if err != nil {
		return nil, false
	}
	scope, err := scopeFromAction(action)
	if err != nil {
		return nil, false
	}
	if hit, _ := ac.Manager.Match(ac.SessionID, scope, time.Now()); !hit {
		return nil, false
	}
	auditPermission(ctx, action, "allow", "approval_manager", "")
	return &widened, true
}

// recordRequestedPermission writes the approved grant into the approval
// manager, using the SAME scope derivation Authorize will use on the real call.
//
// Sharing scopeFromAction rather than building a Scope here is the entire
// correctness argument: two constructions of "the same scope" is two things
// that can disagree, and the disagreement is invisible because the failure mode
// is a rule that never matches.
func recordRequestedPermission(ctx context.Context, action guard.Action, scope PermissionRequestScope) error {
	ac, ok := approvalFromContext(ctx)
	if !ok {
		return fmt.Errorf("no approval manager is bound on this transport")
	}
	sc, err := scopeFromAction(action)
	if err != nil {
		return err
	}
	ttl := approval.TTLOnce
	if scope == ScopeSession {
		ttl = approval.TTLSession
	}
	return ac.Manager.Record(ac.SessionID, approval.Rule{
		ID:        newApprovalID(),
		Action:    action.Tool,
		Scope:     sc,
		TTL:       ttl,
		Source:    approval.SourceUser,
		ExpiresAt: approvalExpiry(ttl, time.Now()),
	})
}

// auditPermissionRequest records a pre-emptive grant decision in the durable
// archive.
//
// It uses its own Source rather than auditPermission's, because "the model
// asked for this ahead of time and a human answered" is a different event from
// "a call was authorized": the grant it creates is spent later, possibly many
// tool calls later, and joining the two afterwards is only possible if the
// first one is findable.
func auditPermissionRequest(ctx context.Context, action guard.Action, granted bool, scope PermissionRequestScope) {
	decision := "deny"
	if granted {
		decision = "allow"
	}
	sessionID, agentID := auditIdentity(ctx)
	recordPermissionAudit(ctx, PermissionAuditRecord{
		SessionID:  sessionID,
		AgentID:    agentID,
		Tool:       action.Tool,
		Decision:   decision,
		Source:     "permission_request:" + string(scope),
		ReasonCode: "model_requested",
		CmdDigest:  auditDigest(action),
	})
}
