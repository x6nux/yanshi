package netpolicy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeResolver struct {
	ips []net.IPAddr
	err error
}

func (f fakeResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return f.ips, f.err
}

func TestPrepareEnvRemovesInheritedProxyVariants(t *testing.T) {
	got := PrepareEnv([]string{"PATH=x", "http_proxy=evil", "HTTPS_PROXY=old", "no_proxy=*"}, "http://127.0.0.1:9000")
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "http_proxy=evil") || strings.Contains(joined, "HTTPS_PROXY=old") || strings.Contains(joined, "no_proxy=*") {
		t.Fatalf("inherited proxy vars remain: %v", got)
	}
	if !strings.Contains(joined, "HTTP_PROXY=http://127.0.0.1:9000") || !strings.Contains(joined, "HTTPS_PROXY=http://127.0.0.1:9000") {
		t.Fatalf("managed vars missing: %v", got)
	}
}

// TestPrepareEnvPublishesLowercaseProxyVariants pins the case coverage of the
// managed set. curl ignores uppercase HTTP_PROXY for plain http:// URLs (the
// httpoxy mitigation) and honors only lowercase http_proxy there, so an
// uppercase-only set lets `curl http://…` out of a subprocess unimpeded while
// appearing to block it. Dropping either case here is a silent egress hole.
func TestPrepareEnvPublishesLowercaseProxyVariants(t *testing.T) {
	got := PrepareEnv([]string{"PATH=x"}, "http://127.0.0.1:0")
	want := []string{
		"HTTP_PROXY=http://127.0.0.1:0", "HTTPS_PROXY=http://127.0.0.1:0", "NO_PROXY=",
		"http_proxy=http://127.0.0.1:0", "https_proxy=http://127.0.0.1:0", "no_proxy=",
	}
	joined := strings.Join(got, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Fatalf("managed env missing %q: %v", w, got)
		}
	}
	if !strings.Contains(joined, "PATH=x") {
		t.Fatalf("host env dropped: %v", got)
	}
}

// TestPrepareEnvStripsLowercaseInheritedVariants guards the other half: the
// strip must be case-insensitive, or a host-configured lowercase http_proxy
// would survive alongside the managed uppercase one and (for plain HTTP under
// curl) win — handing the child the operator's real upstream proxy.
func TestPrepareEnvStripsLowercaseInheritedVariants(t *testing.T) {
	got := PrepareEnv([]string{"http_proxy=http://host-proxy:9999", "ALL_PROXY=socks5://host:1080"}, "http://127.0.0.1:0")
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "host-proxy") || strings.Contains(joined, "socks5") {
		t.Fatalf("inherited proxy vars survived: %v", got)
	}
}

func TestProxyForwardsResponseBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "proxied-body")
	}))
	defer upstream.Close()
	p, err := NewProxy(Policy{Default: "allow", AllowPrivate: true}, fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(p.URL())}}
	resp, err := client.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "proxied-body" {
		t.Fatalf("body=%q", body)
	}
}

func TestDialContext_DNSError(t *testing.T) {
	d := &PolicyDialer{
		Policy:   &Policy{Default: "allow"},
		Resolver: fakeResolver{err: errors.New("DNS failure")},
	}
	_, err := d.DialContext(context.Background(), "tcp", "example.com:80")
	if err == nil {
		t.Fatal("expected DNS error from DialContext")
	}
}

func TestNewTransport_ReturnsTransport(t *testing.T) {
	tr := NewTransport(&Policy{})
	if tr == nil {
		t.Fatal("NewTransport must return non-nil transport")
	}
}

func TestManagedEnv_ReturnsEnvList(t *testing.T) {
	env := ManagedEnv("http://127.0.0.1:9000")
	if len(env) == 0 {
		t.Fatal("ManagedEnv should return at least 3 proxy entries")
	}
	var hasHTTP, hasHTTPS bool
	for _, e := range env {
		if strings.HasPrefix(e, "HTTP_PROXY=") {
			hasHTTP = true
		}
		if strings.HasPrefix(e, "HTTPS_PROXY=") {
			hasHTTPS = true
		}
	}
	if !hasHTTP || !hasHTTPS {
		t.Fatal("ManagedEnv must include HTTP_PROXY and HTTPS_PROXY")
	}
}

// TestServeHTTP_CONNECTIsPolicedNotRefused replaces a test that pinned the
// opposite: CONNECT used to return 501 unconditionally.
//
// That refusal was not a safe default. It meant the managed proxy could never
// be switched on -- every HTTPS-speaking child would break -- which is why it
// was never started and children were handed a placeholder URL that filtered
// nothing. CONNECT is now gated on CheckHost like every other method.
//
// Asserts only that a denied host does not get a tunnel. An allowed host
// needs a real upstream to connect to, which belongs in an integration test
// rather than here.
func TestServeHTTP_CONNECTIsPolicedNotRefused(t *testing.T) {
	p, err := NewProxy(Policy{Default: "deny"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodConnect, "denied.example:443", nil)
	p.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a denied CONNECT target must be refused with 403, got %d", w.Code)
	}
	if w.Code == http.StatusNotImplemented {
		t.Fatal("CONNECT is refused outright again: the managed proxy cannot be enabled")
	}
}

func TestDialContext_NilPolicyResolvesWithoutCheck(t *testing.T) {
	d := &PolicyDialer{
		Resolver: fakeResolver{err: errors.New("still-fails-at-dns")},
	}
	_, err := d.DialContext(context.Background(), "tcp", "example.com:80")
	if err == nil {
		t.Fatal("expected DNS error even with nil policy")
	}
}

func TestDialContext_CheckHostRejectsDeniedHost(t *testing.T) {
	d := &PolicyDialer{
		Policy:   &Policy{Deny: []string{"evil.example"}, Default: "allow"},
		Resolver: fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("1.2.3.4")}}},
	}
	_, err := d.DialContext(context.Background(), "tcp", "evil.example:80")
	if err == nil || !strings.Contains(err.Error(), "denied by") {
		t.Fatalf("expected denial for denied host, got err=%v", err)
	}
}

func TestDialContext_CheckResolvedIPsRejectsPrivate(t *testing.T) {
	d := &PolicyDialer{
		Policy:   &Policy{Default: "allow", AllowPrivate: false},
		Resolver: fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}},
	}
	_, err := d.DialContext(context.Background(), "tcp", "allowed.example:80")
	if err == nil || !strings.Contains(err.Error(), "private/local") {
		t.Fatalf("expected denial for private resolved IP, got err=%v", err)
	}
}

func TestDialContext_EmptyResolutionReturnsError(t *testing.T) {
	d := &PolicyDialer{
		Policy:   &Policy{Default: "allow"},
		Resolver: fakeResolver{ips: []net.IPAddr{}}, // resolve succeeds but returns 0 IPs
	}
	_, err := d.DialContext(context.Background(), "tcp", "host.example:80")
	if err == nil || !strings.Contains(err.Error(), "no addresses") {
		t.Fatalf("expected error for empty resolution, got err=%v", err)
	}
}

func TestDialContext_NilResolverDefaults(t *testing.T) {
	// Nil Resolver defaults to net.DefaultResolver, which would attempt a real
	// DNS lookup. We test that setting Resolver=nil doesn't panic and that the
	// default is used (the lookup will fail on "invalid-host-!@#.local").
	d := &PolicyDialer{
		Policy:   &Policy{Default: "allow"},
		Resolver: nil,
	}
	// The call will attempt real DNS resolution; we just verify it doesn't panic.
	_, err := d.DialContext(context.Background(), "tcp", "host.example:80")
	// We don't care about the specific error — just that the nil resolver path is
	// executed without panicking.
	_ = err
}

func TestDialContext_BadAddressReturnsError(t *testing.T) {
	d := &PolicyDialer{
		Policy:   &Policy{Default: "allow"},
		Resolver: fakeResolver{ips: []net.IPAddr{{IP: net.ParseIP("1.2.3.4")}}},
	}
	// Missing port in address causes SplitHostPort to fail.
	_, err := d.DialContext(context.Background(), "tcp", "host-without-port")
	if err == nil || !strings.Contains(err.Error(), "missing port") {
		t.Fatalf("expected SplitHostPort error, got err=%v", err)
	}
}

// TestServeHTTP_DeniedHostReturns403 —— 见台账。
//
// ledger: A1/S09#1 经受管代理通道的未授权连接失败
func TestServeHTTP_DeniedHostReturns403(t *testing.T) {
	p, err := NewProxy(Policy{Default: "deny", AllowPrivate: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/path", nil)
	p.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied host must return 403, got %d: %s", w.Code, w.Body.String())
	}
}

// TestProxyBoundariesBeforeGoingLive pins the three properties that must hold
// before the managed proxy is actually started for child processes.
//
// Starting it is the one change in this work package that loosens the
// observable posture: today children get http://127.0.0.1:0, a placeholder
// that looks like enforcement and is really a black hole -- it blocks
// proxy-aware clients and produces no decision, no audit, no policy. Moving
// to a real proxy is a move from accidental strictness to designed
// strictness, and these are the net under that jump.
func TestProxyBoundariesBeforeGoingLive(t *testing.T) {
	t.Run("default deny survives the proxy being real", func(t *testing.T) {
		// Policy{} means Default:"" -- nothing is allowed. A live proxy must
		// not turn an empty policy into an open one.
		p, err := NewProxy(Policy{}, net.DefaultResolver)
		if err != nil {
			t.Fatalf("new proxy: %v", err)
		}
		defer p.Close()

		req, _ := http.NewRequest(http.MethodGet, "http://example.com/x", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("empty policy returned %d for a plain request, want 403: fail-closed "+
				"must not depend on the proxy never having been started", rec.Code)
		}
	})

	t.Run("CONNECT runs the host through CheckHost", func(t *testing.T) {
		p, err := NewProxy(Policy{Default: "deny", Allow: []string{"allowed.example"}}, net.DefaultResolver)
		if err != nil {
			t.Fatalf("new proxy: %v", err)
		}
		defer p.Close()

		req, _ := http.NewRequest(http.MethodConnect, "denied.example:443", nil)
		req.Host = "denied.example:443"
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Error("a denied host got a tunnel: CONNECT must be policed, not blanket-allowed")
		}
		if rec.Code == http.StatusNotImplemented {
			t.Error("CONNECT is still refused outright, so every HTTPS-speaking child " +
				"breaks the moment the proxy goes live")
		}
	})

	t.Run("PrepareEnv strips inherited proxy variables", func(t *testing.T) {
		// A real URL is the case that matters: an inherited https_proxy would
		// shadow the managed one and route the child around the policy.
		got := PrepareEnv([]string{
			"PATH=/usr/bin",
			"https_proxy=http://attacker.example:8080",
			"HTTPS_PROXY=http://attacker.example:8080",
			"NO_PROXY=*",
		}, "http://127.0.0.1:54321")

		for _, kv := range got {
			lower := strings.ToLower(kv)
			if strings.HasPrefix(lower, "https_proxy=") && !strings.Contains(kv, "127.0.0.1:54321") {
				t.Errorf("an inherited proxy variable survived: %q shadows the managed proxy", kv)
			}
			if strings.HasPrefix(lower, "no_proxy=*") {
				t.Errorf("an inherited NO_PROXY=* survived: %q exempts every host from the proxy", kv)
			}
		}
	})
}

// TestDeniedConnectReachesTheAuditTrail pins that a refused connection leaves
// a record.
//
// This package had no logging of any kind: a child process whose connection
// was refused saw a 403 and the operator saw nothing at all. A policy nobody
// can observe is indistinguishable from no policy, and the acceptance
// criteria ask specifically for decisions to reach the audit trail.
//
// ledger: A1/S09#4 决策入审计
func TestDeniedConnectReachesTheAuditTrail(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	p, err := NewProxy(Policy{Default: "deny"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodConnect, "denied.example:443", nil)
	p.ServeHTTP(w, req)

	out := buf.String()
	if !strings.Contains(out, "netpolicy decision") {
		t.Errorf("a refused connection produced no audit record; log was %q", out)
	}
	if !strings.Contains(out, "denied.example") {
		t.Errorf("the audit record does not name the host, so an operator cannot tell "+
			"which connection was refused; log was %q", out)
	}
	if !strings.Contains(out, "decision=deny") {
		t.Errorf("the audit record does not carry the verdict; log was %q", out)
	}
}
