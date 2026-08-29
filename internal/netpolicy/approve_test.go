package netpolicy

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
)

// recordingApprover answers `verdict` and counts how often it was consulted,
// keeping the last request so a test can check what the operator would have
// been shown.
type recordingApprover struct {
	verdict bool
	calls   int64
	last    atomic.Pointer[Request]
}

func (a *recordingApprover) ApproveEgress(_ context.Context, req Request) bool {
	atomic.AddInt64(&a.calls, 1)
	copied := req
	a.last.Store(&copied)
	return a.verdict
}

func (a *recordingApprover) count() int64 { return atomic.LoadInt64(&a.calls) }

// TestUnapprovedHostFailsClosedWithNobodyToAsk is the acceptance's third
// clause. No approver is the production reality for a headless run and for
// every moment before a client connects.
func TestUnapprovedHostFailsClosedWithNobodyToAsk(t *testing.T) {
	origin := newUpstream(t)
	p := newTestProxy(t, Policy{Default: "deny"})

	resp, err := proxyClient(p).Get("http://ask.test:" + origin.port() + "/x")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 with no approver bound", resp.StatusCode)
	}
	if n := origin.count(); n != 0 {
		t.Fatalf("origin saw %d requests", n)
	}
}

// TestApprovedHostIsRememberedForTheProcess covers "批准可保存" at the proxy's
// own layer: one approval, one question, and the second request does not ask
// again. Without this a build fetching forty files from one host raises forty
// dialogs.
func TestApprovedHostIsRememberedForTheProcess(t *testing.T) {
	origin := newUpstream(t)
	p := newTestProxy(t, Policy{Default: "deny"})
	approver := &recordingApprover{verdict: true}
	p.SetApprover(approver)

	client := proxyClient(p)
	for i := 0; i < 3; i++ {
		resp, err := client.Get("http://ask.test:" + origin.port() + "/x")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if n := approver.count(); n != 1 {
		t.Fatalf("approver consulted %d times, want 1", n)
	}
	if n := origin.count(); n != 3 {
		t.Fatalf("origin saw %d requests, want 3", n)
	}
}

// TestRefusedApprovalKeepsTheHostBlocked pins that "no" means no — and, since
// the refusal is not cached, that it is asked again rather than silently
// hardened into a permanent local deny the operator cannot undo.
func TestRefusedApprovalKeepsTheHostBlocked(t *testing.T) {
	origin := newUpstream(t)
	p := newTestProxy(t, Policy{Default: "deny"})
	approver := &recordingApprover{verdict: false}
	p.SetApprover(approver)

	client := proxyClient(p)
	for i := 0; i < 2; i++ {
		resp, err := client.Get("http://ask.test:" + origin.port() + "/x")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("request %d status = %d, want 403", i, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if n := approver.count(); n != 2 {
		t.Fatalf("approver consulted %d times, want 2", n)
	}
	if n := origin.count(); n != 0 {
		t.Fatalf("origin saw %d requests after a refusal", n)
	}
}

// TestAnExplicitDenyRuleIsNeverOfferedForApproval is the load-bearing limit on
// the dialog. security.network.deny is the operator naming a host and saying
// no; a prompt able to undo it would make that list advisory.
func TestAnExplicitDenyRuleIsNeverOfferedForApproval(t *testing.T) {
	origin := newUpstream(t)
	p := newTestProxy(t, Policy{Default: "allow", Deny: []string{"evil.test"}})
	approver := &recordingApprover{verdict: true}
	p.SetApprover(approver)

	resp, err := proxyClient(p).Get("http://evil.test:" + origin.port() + "/x")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if n := approver.count(); n != 0 {
		t.Fatalf("approver was consulted %d times about an explicit deny rule", n)
	}
	if n := origin.count(); n != 0 {
		t.Fatalf("origin saw %d requests", n)
	}
}

// TestAMethodRuleRefusalIsNeverOfferedForApproval is the same limit one
// dimension over. The host IS allowed; the operator narrowed which verbs. A
// dialog that could widen it back would let one approval reopen the narrowing,
// and the grant is per-host, so it would reopen it permanently.
func TestAMethodRuleRefusalIsNeverOfferedForApproval(t *testing.T) {
	origin := newTLSUpstream(t)
	p := newTestProxy(t, Policy{
		Default: "deny",
		Allow:   []string{"example.com"},
		Methods: []MethodRule{{Host: "example.com", Methods: []string{"DELETE"}, Allow: false}},
	})
	trustOrigin(t, p, origin)
	ca := newTestCA(t)
	p.SetCertAuthority(ca)
	approver := &recordingApprover{verdict: true}
	p.SetApprover(approver)

	req, err := http.NewRequest(http.MethodDelete, "https://example.com:"+origin.port()+"/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := inspectingClient(t, p, ca).Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if n := approver.count(); n != 0 {
		t.Fatalf("approver was consulted %d times about a method rule", n)
	}
	if n := origin.count(); n != 0 {
		t.Fatalf("origin saw %d requests", n)
	}
}

// TestApproverIsAskedOnEveryProtocol pins that the approval hook is not an
// HTTP-only feature. The single authorize() entry point is what makes this
// true; three separate policy checks is what it used to be.
func TestApproverIsAskedOnEveryProtocol(t *testing.T) {
	origin := newUpstream(t)

	t.Run("http", func(t *testing.T) {
		p := newTestProxy(t, Policy{Default: "deny"})
		approver := &recordingApprover{}
		p.SetApprover(approver)
		resp, err := proxyClient(p).Get("http://ask.test:" + origin.port() + "/x")
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		_ = resp.Body.Close()
		assertAsked(t, approver, "http", "ask.test", http.MethodGet)
	})

	t.Run("connect", func(t *testing.T) {
		p := newTestProxy(t, Policy{Default: "deny"})
		approver := &recordingApprover{}
		p.SetApprover(approver)
		_, _ = proxyClient(p).Get("https://ask.test:" + origin.port() + "/x")
		// A blind CONNECT carries no method; the empty string is the honest
		// answer and the dialog says so rather than inventing one.
		assertAsked(t, approver, "connect", "ask.test", "")
	})

	t.Run("socks5", func(t *testing.T) {
		p := newTestProxy(t, Policy{Default: "deny"})
		approver := &recordingApprover{}
		p.SetApprover(approver)
		port, err := parsePort(origin.port())
		if err != nil {
			t.Fatal(err)
		}
		_, code := socks5Dial(t, p.listener.Addr().String(), "ask.test", port)
		if code != socks5ReplyRuleset {
			t.Fatalf("reply = 0x%02x", code)
		}
		assertAsked(t, approver, "socks5", "ask.test", "")
	})
}

func assertAsked(t *testing.T, a *recordingApprover, protocol, host, method string) {
	t.Helper()
	if n := a.count(); n != 1 {
		t.Fatalf("approver consulted %d times, want 1", n)
	}
	got := a.last.Load()
	if got == nil {
		t.Fatal("approver recorded no request")
	}
	if got.Protocol != protocol || got.Host != host || got.Method != method {
		t.Fatalf("asked about %+v, want {Protocol:%s Host:%s Method:%s}", *got, protocol, host, method)
	}
}
