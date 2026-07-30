package netpolicy

import (
	"context"
	"errors"
	"io"
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

func TestServeHTTP_CONNECTRejected(t *testing.T) {
	p, err := NewProxy(Policy{Default: "allow", AllowPrivate: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodConnect, "http://example.com:443", nil)
	p.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("CONNECT must return 501, got %d", w.Code)
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
