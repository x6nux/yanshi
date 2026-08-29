// internal/tools/auditsink.go
//
// S6: the sink that gives permission decisions somewhere durable to go.
//
// auditPermission has always produced a complete structured record. Until this
// file existed it had exactly one consumer — slog — so the record's lifetime
// was the terminal scrollback. Under yolo/auto, where nobody is asked, that
// made "what did it approve last night, and on what grounds" unanswerable.
//
// WHY A PROCESS-WIDE REGISTRATION AND NOT A CONTEXT VALUE. Every other
// cross-cutting dependency in this package rides the turn context, so that is
// the obvious shape and it is the wrong one here. The audit sink is a property
// of the PROCESS (there is one store, and it exists before any turn does), not
// of a turn: binding it per-turn would mean each of the several context-building
// sites has to remember to bind it, and the ones that forget produce decisions
// that are simply never recorded — silently, because a missing audit row looks
// exactly like a decision that never happened. That is the failure mode an
// audit trail exists to rule out.
//
// The precedent is in permctx.go's own init(), which registers the production
// Authorize with secproc for the same structural reason: a leaf that must be
// reachable from call sites that cannot be handed a dependency.
package tools

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/x6nux/yanshi/internal/guard"
)

// PermissionAuditRecord is one permission decision, as handed to a sink.
//
// It mirrors what auditPermission already logged, plus the two things a durable
// trail needs and a log line does not: the session and agent the decision was
// made for (so records can be attributed months later) and a digest of what was
// actually being requested (so "deny shell_run" is distinguishable from the
// other four hundred "deny shell_run" rows).
//
// CmdDigest is caller-influenced text and is the ONLY field that is. Sinks are
// required to redact it; store.AppendPermissionAudit does.
type PermissionAuditRecord struct {
	SessionID  string
	AgentID    string
	Tool       string
	Decision   string
	Source     string
	ReasonCode string
	CmdDigest  string
}

// PermissionAuditSink receives permission decisions for durable recording.
//
// Record MUST NOT be able to change the caller's verdict — it is called from
// inside Authorize, after the decision is made, and a failing audit store must
// never turn an allowed action into a denied one (or vice versa). That is why
// it returns nothing: implementations swallow or log their own failures, and
// the signature makes the alternative unexpressible rather than merely
// discouraged.
type PermissionAuditSink interface {
	Record(ctx context.Context, rec PermissionAuditRecord)
}

// permissionAuditSink holds the process-wide sink. atomic.Value rather than a
// plain var because bootstrap installs it during startup while nothing else is
// running, but tests swap it while other tests may be running in parallel, and
// a torn read here would be a data race in the security path.
var permissionAuditSink atomic.Value // holds PermissionAuditSink

// SetPermissionAuditSink installs the process-wide durable sink for permission
// decisions. Called once by the composition root after the store is open.
//
// A nil sink clears it, which is the pre-S6 behaviour: decisions then reach
// slog only. That is the correct degradation for a deployment with no store,
// not an error — refusing to authorize anything because the archive is
// unavailable would make the audit trail a new way to take the agent down.
func SetPermissionAuditSink(sink PermissionAuditSink) {
	if sink == nil {
		permissionAuditSink.Store(permissionAuditHolder{})
		return
	}
	permissionAuditSink.Store(permissionAuditHolder{sink: sink})
}

// permissionAuditHolder boxes the interface so atomic.Value always sees one
// concrete type. Storing the interface directly panics as soon as two different
// implementations are installed (production store, then a test fake).
type permissionAuditHolder struct{ sink PermissionAuditSink }

// PermissionAuditSinkInstalled reports whether a durable sink is currently
// registered. Exposed so the composition root can log which mode it started in
// — "permission decisions are not being archived" is a fact an operator should
// be able to discover before they need the archive, not after.
func PermissionAuditSinkInstalled() bool {
	h, _ := permissionAuditSink.Load().(permissionAuditHolder)
	return h.sink != nil
}

// maxAuditDigestBytes truncates a digest before it leaves this package. The
// store truncates too; doing it here as well keeps an unbounded tool argument
// from being copied through the sink interface in the first place.
const maxAuditDigestBytes = 512

// auditDigest renders the part of an action worth recording: the shell command
// if there is one, else the filesystem operation and its paths, else the
// network host. Returns "" when the action carries none of them (a plain
// tool-name check), which is honest — there is nothing to summarise.
//
// This is a SUMMARY, not evidence. It is truncated, and the sink redacts it.
// Note what is NOT here: argsJSON. The digest is derived from the guard.Action's
// structured fields, so a tool that stuffs a credential into an argument the
// guard never inspects does not get it written to the archive.
func auditDigest(action guard.Action) string {
	var s string
	switch {
	case action.Shell != "":
		s = "shell: " + action.Shell
	case len(action.FS.Paths) > 0:
		op := action.FS.Op
		if op == "" {
			op = "fs"
		}
		s = op + ": " + strings.Join(action.FS.Paths, " ")
	case action.NetHost != "":
		s = "net: " + action.NetHost
	default:
		return ""
	}
	if len(s) > maxAuditDigestBytes {
		s = s[:maxAuditDigestBytes]
	}
	return s
}

// auditIdentity resolves the session and acting-agent ids for an audit record
// from the same context values the rest of the tool layer already reads.
//
// Session id comes from the approval context (the only place a WS connection's
// session id is bound into the tool layer) with the thread link as a fallback,
// because ThreadID is set to the session id on the WS path and left empty on
// v1/SSE. Agent id comes from the VCS scope, which is where the acting agent's
// identity is already carried for commit attribution.
//
// Both may be empty. An empty session on the SSE path is a fact about that
// path, not a failure to look — inventing an id would make the trail lie.
func auditIdentity(ctx context.Context) (sessionID, agentID string) {
	if ac, ok := approvalFromContext(ctx); ok {
		sessionID = ac.SessionID
	}
	if sessionID == "" {
		if link, ok := ThreadLinkFromContext(ctx); ok {
			sessionID = link.ThreadID
		}
	}
	if scope, ok := VCSScopeFromContext(ctx); ok {
		agentID = scope.Agent
	}
	return sessionID, agentID
}

// recordPermissionAudit forwards one decision to the installed sink, if any.
//
// Separate from auditPermission's slog call rather than replacing it: the log
// line is the live view and the row is the archive, and they fail
// independently (a full disk kills the row, a quiet log level kills the line).
// Collapsing them would mean losing both together.
func recordPermissionAudit(ctx context.Context, rec PermissionAuditRecord) {
	h, _ := permissionAuditSink.Load().(permissionAuditHolder)
	if h.sink == nil {
		return
	}
	h.sink.Record(ctx, rec)
}

// RecordPermissionAudit writes one decision to the installed durable sink from
// OUTSIDE this package.
//
// It exists for the transport layer, which makes exactly one authorization
// decision Authorize never sees: a human overriding ModeAuto's ASK verdict
// (W-B-15). That override happens inside the permission callback, so by the
// time Authorize records its own "allow / interactive_once" line the fact that
// a model had refused first is gone — and "the model said no and a human said
// yes anyway" is the single most interesting row in the archive.
//
// Deliberately not a general-purpose logging hook: everything else that
// authorizes goes through Authorize, which audits on every exit. A second
// writer for decisions Authorize already records would double-count them.
func RecordPermissionAudit(ctx context.Context, rec PermissionAuditRecord) {
	recordPermissionAudit(ctx, rec)
}

// StoreAuditSink adapts a durable store to PermissionAuditSink.
//
// The Append function is injected rather than the *store.Store itself so this
// package keeps its existing dependency shape — and so a test can substitute a
// failing writer to prove the failure is swallowed rather than propagated.
type StoreAuditSink struct {
	// Append persists one record. Its error is logged, never returned: see
	// PermissionAuditSink.Record.
	Append func(rec PermissionAuditRecord) error
}

// Record implements PermissionAuditSink. A nil Append or a write failure is
// logged at WARN and otherwise ignored — the guard's verdict has already been
// made and must not depend on the archive being writable.
func (s *StoreAuditSink) Record(ctx context.Context, rec PermissionAuditRecord) {
	if s == nil || s.Append == nil {
		return
	}
	if err := s.Append(rec); err != nil {
		slog.WarnContext(ctx, "permission audit write failed",
			"tool", rec.Tool, "decision", rec.Decision, "error", err)
	}
}
