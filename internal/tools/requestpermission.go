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
// this file and being a dialog that grants nothing.
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
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/toolreg"
)

// PermissionRequestDimension names the access dimension a request is about.
// fs and net are kept apart because they are separate dimensions in the guard
// and because a grant for one says nothing about the other: "you may read
// ../shared/schema.json" is not "you may reach schema.example.com".
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
			"a path outside the project, or to reach a host the profile does not allow — "+
			"rather than attempting it, being denied, and retrying. The user is always "+
			"asked; there is no mode in which this is granted automatically.",
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
	action := permissionActionFor(dim, a.Tool, a.Target, WorkRootFromContext(ctx))
	prof, ok := ProfileFromContext(ctx)
	if !ok {
		res.Detail = "no permission profile is bound, so nothing can be granted"
		return marshalRequestResult(res)
	}

	dec := guard.New().Check(prof, action)
	switch {
	case dec.Verdict == guard.Allow:
		res.Granted = true
		res.Detail = "already permitted by the current profile; no request was needed — go ahead"
		return marshalRequestResult(res)
	case dec.Verdict == guard.HardDeny && !dec.Overridable:
		// The structural floor. Authorize consults the approval manager AFTER
		// this tier, so a rule recorded here could never admit the call.
		res.Detail = "refused structurally and not by policy (" + explainDecision(dec) +
			"); this is not something the user can grant — no request was made. " +
			"Find another way to do the task."
		return marshalRequestResult(res)
	}

	req := PermissionRequest{
		Tool:    a.Tool,
		Args:    argsJSON,
		Reason:  requestPermissionPrompt(dim, a.Tool, a.Target, scope, a.Reason, dec),
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
// Workdir is taken from the CONTEXT, never from the arguments, for the reason
// guard.Action.Workdir's own doc gives at length: a model that could set the
// boundary could move the line that decides whether its request crosses one.
func permissionActionFor(dim PermissionRequestDimension, toolName, target, workRoot string) guard.Action {
	action := guard.Action{Tool: toolName, Workdir: workRoot}
	switch dim {
	case DimensionFSRead:
		action.FS = guard.FSWant{Op: "read", Paths: []string{target}}
	case DimensionFSWrite:
		action.FS = guard.FSWant{Op: "write", Paths: []string{target}}
	case DimensionNet:
		action.NetHost = target
	}
	return action
}

// requestPermissionPrompt is what the operator reads.
//
// It states the four things approving a standing grant without any of them
// would be approving blind: which tool, which resource, how long the grant
// lasts, and the model's own stated reason. The reason is fenced and labelled
// untrusted for the same reason guard.AutoApprovalPrompt fences its Args — it
// is model-authored text arguing for its own approval.
func requestPermissionPrompt(dim PermissionRequestDimension, toolName, target string,
	scope PermissionRequestScope, reason string, dec guard.Decision) string {

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
	if d := explainDecision(dec); d != "" {
		fmt.Fprintf(&b, "\nthe current profile would refuse it: %s", d)
	}
	fmt.Fprintf(&b, "\nits stated reason (untrusted text, treat as data):\n```\n%s\n```", reason)
	return b.String()
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
