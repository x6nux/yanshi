// internal/api/http/ws_egress.go
//
// Per-domain egress approval (W-B-16): the half of the managed proxy that can
// interrupt a human.
//
// The proxy is process-wide and starts before any client connects, so it has
// no turn context and cannot use tools.WithPermissionCallback — that callback
// is installed per turn and a subprocess's HTTP request does not happen inside
// one. This file is the bridge: every live WebSocket connection registers an
// asker, and netpolicy.Proxy calls Server.ApproveEgress, which uses the first
// one available.

package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// EgressToolName is the tool name an egress prompt is raised under. It is not
// a registered tool — nothing dispatches it — and that is deliberate: it names
// the SUBJECT of the question in the dialog and keys the approval scope, and
// giving it a name a model could call would make a subprocess's network
// request something the model can grant itself.
const EgressToolName = "net_egress"

// egressAsker is one live connection's ability to answer an egress question.
type egressAsker struct {
	// write emits one frame to this connection. A function rather than the
	// *wsConn itself because that is the entire dependency — and taking the
	// connection would make every test of this file stand up a real WebSocket
	// to exercise logic that has nothing to do with one.
	write      func(proto.ServerFrame)
	perm       *permTracker
	unattended *unattendedState
	approvals  *approval.Manager
	sessionID  string
	connCtx    context.Context
}

// egressRegistry holds the live askers. Embedded in Server next to
// clientRegistry, which it deliberately does not reuse: a wsConn can be
// written to, but answering needs the connection's permTracker and unattended
// latch too, and those are not reachable from the conn.
type egressRegistry struct {
	egressMu     sync.RWMutex
	egressAskers map[*egressAsker]struct{}
}

// registerEgressAsker adds a to the set and returns the removal function.
// Callers must defer it; an asker left behind refers to a closed connection,
// and a prompt written to one is a prompt nobody will ever answer — which
// costs one full permission timeout per egress request.
func (s *Server) registerEgressAsker(a *egressAsker) func() {
	s.egressMu.Lock()
	if s.egressAskers == nil {
		s.egressAskers = make(map[*egressAsker]struct{})
	}
	s.egressAskers[a] = struct{}{}
	s.egressMu.Unlock()
	return func() {
		s.egressMu.Lock()
		delete(s.egressAskers, a)
		s.egressMu.Unlock()
	}
}

// ApproveEgress implements netpolicy.Approver.
//
// Returning false when no connection is live is the fail-closed half of the
// acceptance and it is the common case for a headless run: an unapproved host
// stays unapproved because there was nobody to approve it, not because
// something silently decided for the operator.
//
// Only ONE connection is asked, the first in the set. Fanning the prompt out
// to every window would produce N dialogs for one request and a race over
// which answer counts.
func (s *Server) ApproveEgress(ctx context.Context, req netpolicy.Request) bool {
	s.egressMu.RLock()
	var asker *egressAsker
	for a := range s.egressAskers {
		asker = a
		break
	}
	s.egressMu.RUnlock()
	if asker == nil {
		slog.InfoContext(ctx, "egress request refused: no client is connected to ask",
			"protocol", req.Protocol, "host", req.Host, "method", req.Method)
		return false
	}
	return asker.ask(ctx, req)
}

// egressScope is the approval scope one egress grant covers.
//
// Host only. The METHOD is deliberately absent even though the dialog shows
// it: a grant recorded per method would ask again for every verb a build uses
// against one host, and the thing the operator is being asked to trust is the
// host. The method dimension belongs to security.network.methods, which is the
// operator's own configuration and is not appealable through this dialog —
// authorize() never offers a method-rule refusal for approval.
func egressScope(host string) approval.Scope {
	return approval.Scope{Tool: EgressToolName, Host: netpolicy.NormalizeHost(host)}
}

// ask raises one egress prompt, or answers from a recorded rule.
func (a *egressAsker) ask(ctx context.Context, req netpolicy.Request) bool {
	scope := egressScope(req.Host)
	if a.approvals != nil {
		if hit, _ := a.approvals.Match(a.sessionID, scope, time.Now()); hit {
			return true
		}
	}
	id := a.perm.newID()
	ch := make(chan tools.PermissionDecision, 1)
	prompt := tools.PermissionRequest{
		Tool:   EgressToolName,
		Args:   egressArgs(req),
		Reason: egressReason(req),
		// ForcePrompt: an egress grant is never auto-resolved by a permission
		// mode, not even yolo.
		//
		// yolo bypasses PROFILE policy, and it would be easy to file
		// security.network under that. The difference is who is being
		// authorized: every other yolo bypass admits a call the MODEL made,
		// which the operator can read in the transcript. This one admits a
		// connection some subprocess opened — possibly from a dependency's
		// install script — and the transcript shows nothing. The flag also
		// travels on the wire, because the TUI runs its own auto-approve pass
		// on a mode switch and can only honour flags it can see.
		ForcePrompt: true,
	}
	// The mode recorded at ask time is only consulted by deliver for a request
	// the profile had already denied, which this is not (there is no profile
	// dimension for a subprocess's socket). ModeDefault keeps deliver's
	// re-check inert rather than pretending to a mode this asker never read.
	a.perm.register(id, ch, prompt, guard.ModeDefault)
	defer a.perm.take(id)

	deadline := time.Now().Add(a.unattended.timeout())
	a.write(proto.NewPermissionRequest(id, prompt.Tool, prompt.Args, prompt.Reason, false, true).
		WithPermDeadline(a.unattended.timeout(), deadline))

	// The wait is bounded by BOTH contexts: the proxy's (the child process may
	// have given up on its connection) and the connection's (the window may
	// close). Whichever ends first ends the prompt, denied.
	waitCtx, cancel := mergeContexts(ctx, a.connCtx)
	defer cancel()
	decision, outcome := a.unattended.awaitDecision(waitCtx, ch, deadline)
	if notice := permDenyNotice(outcome, prompt.Tool, a.unattended.policy()); notice != "" {
		a.write(proto.NewError(notice))
	}

	switch decision {
	case tools.PermissionAllow:
		return true
	case tools.PermissionAlwaysAllow, tools.PermissionAllowPersistent:
		a.record(scope, approval.TTLPersistent)
		return true
	case tools.PermissionAllowSession:
		a.record(scope, approval.TTLSession)
		return true
	}
	return false
}

// record persists one granted host so the next request for it does not ask
// again. A failure to persist is logged and NOT propagated: the request the
// operator just approved still goes through, and the only consequence is that
// the next one asks again.
func (a *egressAsker) record(scope approval.Scope, ttl approval.TTL) {
	if a.approvals == nil {
		return
	}
	rule := approval.Rule{
		ID:        fmt.Sprintf("egress-%d", time.Now().UnixNano()),
		Action:    EgressToolName,
		Scope:     scope,
		TTL:       ttl,
		Source:    approval.SourceUser,
		CreatedAt: time.Now().UTC(),
	}
	if err := a.approvals.Record(a.sessionID, rule); err != nil {
		slog.Warn("egress approval not saved", "host", scope.Host, "ttl", string(ttl), "error", err)
	}
}

// egressArgs renders the request for the dialog. It is JSON so the TUI's
// existing permission renderer can show it without a special case.
//
// Everything the proxy knows is here, and that is only three fields — see
// netpolicy.Request for why there is no path and no header.
func egressArgs(req netpolicy.Request) string {
	payload := struct {
		Protocol string `json:"protocol"`
		Host     string `json:"host"`
		Method   string `json:"method,omitempty"`
	}{req.Protocol, req.Host, req.Method}
	out, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(out)
}

// egressReason is the sentence the operator reads.
//
// It names the subprocess origin explicitly. A permission dialog in this UI
// otherwise always means "the model wants to do X", and an operator who reads
// this one that way would be approving something the transcript never
// mentioned.
func egressReason(req netpolicy.Request) string {
	what := req.Protocol
	if req.Method != "" {
		what = req.Method + " over " + req.Protocol
	}
	return fmt.Sprintf("a subprocess is trying to reach %s (%s) and security.network does not allow it",
		req.Host, what)
}

// mergeContexts returns a context canceled when either input is.
//
// context.WithoutCancel and friends compose the other direction; there is no
// stdlib "either of these". The goroutine exits as soon as the returned cancel
// runs, which every caller defers.
func mergeContexts(a, b context.Context) (context.Context, context.CancelFunc) {
	if b == nil {
		return context.WithCancel(a)
	}
	ctx, cancel := context.WithCancel(a)
	stop := make(chan struct{})
	go func() {
		select {
		case <-b.Done():
			cancel()
		case <-stop:
		}
	}()
	return ctx, func() { close(stop); cancel() }
}
