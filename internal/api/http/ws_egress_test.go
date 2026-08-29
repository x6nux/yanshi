package http

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/approval"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/netpolicy"
	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

// TestApproveEgressRefusesWithNoConnectedClient is W-B-16's fail-closed
// clause. It is the posture of every headless run and of every moment before
// the TUI attaches, so it is the DEFAULT answer rather than an edge case.
func TestApproveEgressRefusesWithNoConnectedClient(t *testing.T) {
	s := New(Config{Token: "t"})
	if s.ApproveEgress(context.Background(), netpolicy.Request{
		Protocol: "http", Host: "example.test", Method: "GET",
	}) {
		t.Fatal("an egress request was approved with nobody connected to approve it")
	}
}

// egressHarness builds an asker with no real WebSocket behind it: frames go to
// a capturing function and the answer is delivered through the permTracker the
// way the reader goroutine would.
type egressHarness struct {
	asker  *egressAsker
	perm   *permTracker
	server *Server
}

func newEgressHarness(t *testing.T, timeout time.Duration) *egressHarness {
	t.Helper()
	manager, err := approval.New(nil, "test", nil)
	if err != nil {
		t.Fatalf("approval.New: %v", err)
	}
	perm := newPermTracker()
	h := &egressHarness{
		perm:   perm,
		server: New(Config{Token: "t"}),
		asker: &egressAsker{
			write:      func(proto.ServerFrame) {},
			perm:       perm,
			unattended: newUnattendedState(PermissionTimeoutPolicy{Timeout: timeout}),
			approvals:  manager,
			sessionID:  "sess-1",
			connCtx:    context.Background(),
		},
	}
	t.Cleanup(h.server.registerEgressAsker(h.asker))
	return h
}

// answer delivers a decision to whichever prompt is pending, polling because
// the ask registers its channel on another goroutine.
func (h *egressHarness) answer(t *testing.T, decision tools.PermissionDecision) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.perm.mu.Lock()
		ids := make([]string, 0, len(h.perm.pending))
		for id := range h.perm.pending {
			ids = append(ids, id)
		}
		h.perm.mu.Unlock()
		if len(ids) > 0 {
			h.perm.deliver(ids[0], decision, guard.ModeDefault)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no egress prompt was raised")
}

// TestEgressPromptAllowsWhenTheOperatorSaysYes is the interactive half: the
// question reaches a connection, the answer comes back, and the request is
// admitted.
func TestEgressPromptAllowsWhenTheOperatorSaysYes(t *testing.T) {
	h := newEgressHarness(t, 5*time.Second)
	result := make(chan bool, 1)
	go func() {
		result <- h.server.ApproveEgress(context.Background(), netpolicy.Request{
			Protocol: "http", Host: "ask.test", Method: "GET",
		})
	}()
	h.answer(t, tools.PermissionAllow)
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("an approved egress request was reported as refused")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ApproveEgress never returned")
	}
}

// TestEgressPromptDeniesWhenTheOperatorSaysNo is the other direction.
func TestEgressPromptDeniesWhenTheOperatorSaysNo(t *testing.T) {
	h := newEgressHarness(t, 5*time.Second)
	result := make(chan bool, 1)
	go func() {
		result <- h.server.ApproveEgress(context.Background(), netpolicy.Request{
			Protocol: "connect", Host: "ask.test",
		})
	}()
	h.answer(t, tools.PermissionDeny)
	select {
	case ok := <-result:
		if ok {
			t.Fatal("a refused egress request was reported as approved")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ApproveEgress never returned")
	}
}

// TestEgressGrantIsSavedAndReused covers "批准可保存". A session grant must
// suppress the next prompt for the same host, or a build fetching forty files
// raises forty dialogs.
func TestEgressGrantIsSavedAndReused(t *testing.T) {
	h := newEgressHarness(t, 5*time.Second)
	result := make(chan bool, 1)
	go func() {
		result <- h.server.ApproveEgress(context.Background(), netpolicy.Request{
			Protocol: "http", Host: "Saved.Test", Method: "GET",
		})
	}()
	h.answer(t, tools.PermissionAllowSession)
	if ok := <-result; !ok {
		t.Fatal("the session grant was reported as a refusal")
	}

	// Second time: nothing may be pending, because nothing should be asked.
	done := make(chan bool, 1)
	go func() {
		done <- h.server.ApproveEgress(context.Background(), netpolicy.Request{
			// A different case and a port, to prove the scope is normalized
			// the same way netpolicy normalizes a host.
			Protocol: "http", Host: "saved.test:8443", Method: "POST",
		})
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("the saved grant did not admit the second request")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second request raised a prompt instead of matching the saved grant")
	}
}

// TestEgressPromptTimesOutDenied pins the wall-clock half. An unattended
// connection that stops answering must not hold a subprocess's socket open
// forever, and the direction it fails in is refusal.
func TestEgressPromptTimesOutDenied(t *testing.T) {
	h := newEgressHarness(t, 50*time.Millisecond)
	start := time.Now()
	if h.server.ApproveEgress(context.Background(), netpolicy.Request{
		Protocol: "socks5", Host: "silent.test",
	}) {
		t.Fatal("an unanswered egress prompt was approved")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the prompt took %v to give up", elapsed)
	}
}

// TestEgressPromptRespectsTheProxyContext covers the case where the CHILD gave
// up: the proxy's context is canceled and the prompt must stop waiting rather
// than hold a permTracker entry for a connection nobody is on.
func TestEgressPromptRespectsTheProxyContext(t *testing.T) {
	h := newEgressHarness(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan bool, 1)
	go func() {
		result <- h.server.ApproveEgress(ctx, netpolicy.Request{Protocol: "http", Host: "gone.test"})
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("a canceled egress request was approved")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ApproveEgress ignored the proxy's context and waited for the full timeout")
	}
}

// TestEgressPromptIsAlwaysForcePrompt pins the flag that keeps a permission
// MODE from answering for the operator.
//
// yolo bypasses profile policy, and security.network looks like profile
// policy. The difference is who is being authorized: every other yolo bypass
// admits a call the model made and the transcript shows, while this one admits
// a connection some subprocess opened — possibly from a dependency's install
// script — that the transcript never mentions. The flag also has to reach the
// wire, because the TUI runs its own auto-approve pass on a mode switch and
// can only honour flags it can see.
func TestEgressPromptIsAlwaysForcePrompt(t *testing.T) {
	h := newEgressHarness(t, 5*time.Second)
	var seen []proto.ServerFrame
	var mu sync.Mutex
	h.asker.write = func(f proto.ServerFrame) {
		mu.Lock()
		seen = append(seen, f)
		mu.Unlock()
	}
	go func() {
		_ = h.server.ApproveEgress(context.Background(), netpolicy.Request{
			Protocol: "http", Host: "x.test", Method: "GET",
		})
	}()
	h.answer(t, tools.PermissionDeny)

	mu.Lock()
	defer mu.Unlock()
	var prompt *proto.ServerFrame
	for i := range seen {
		if seen[i].Type == "permission_request" {
			prompt = &seen[i]
		}
	}
	if prompt == nil {
		t.Fatal("no permission_request frame was written")
	}
	if !prompt.ForcePrompt {
		t.Fatal("the egress prompt does not carry force_prompt, so the TUI's auto-approve " +
			"pass will answer it the moment the user switches to yolo")
	}
	if prompt.ToolName != EgressToolName {
		t.Fatalf("prompt tool = %q, want %q", prompt.ToolName, EgressToolName)
	}
}

// TestEgressArgsCarryNoPathOrHeaders is ADR-0023's recording constraint at the
// place an operator would see it. A URL path routinely carries a bearer token
// in a query parameter, and the dialog is the most tempting place to put one.
func TestEgressArgsCarryNoPathOrHeaders(t *testing.T) {
	got := egressArgs(netpolicy.Request{Protocol: "https", Host: "api.test", Method: "POST"})
	for _, forbidden := range []string{"path", "url", "header", "query", "body"} {
		if strings.Contains(strings.ToLower(got), forbidden) {
			t.Fatalf("egressArgs mentions %q: %s", forbidden, got)
		}
	}
	for _, want := range []string{"api.test", "POST", "https"} {
		if !strings.Contains(got, want) {
			t.Fatalf("egressArgs omits %q: %s", want, got)
		}
	}
}
