package netpolicy

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Interception proof for W-B-16/W-B-17.
//
// Every test here asserts on the UPSTREAM SERVER's request counter as well as
// on the status the client saw. A proxy that answers 403 and forwards anyway
// looks identical from the client side, and "intercepted but forwarded" is
// exactly the shape these tests exist to make impossible to ship.

// upstream is a counting HTTP origin server plus the pieces a test needs to
// aim a proxy at it: a host name to use in URLs and a resolver that maps that
// name to the server's loopback address.
type upstream struct {
	server *httptest.Server
	hits   *int64
	// methods records every method that actually reached the origin, which is
	// what makes "GET got through and POST did not" checkable.
	methods chan string
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	var hits int64
	u := &upstream{hits: &hits, methods: make(chan string, 16)}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(u.hits, 1)
		select {
		case u.methods <- r.Method:
		default:
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "origin")
	}))
	t.Cleanup(u.server.Close)
	return u
}

// tlsUpstream is the same thing over TLS, for the inspected-CONNECT tests. The
// proxy talks to it as a real HTTPS client, so its certificate has to validate
// — the test hands the proxy's transport the server's own root.
func newTLSUpstream(t *testing.T) *upstream {
	t.Helper()
	var hits int64
	u := &upstream{hits: &hits, methods: make(chan string, 16)}
	u.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(u.hits, 1)
		select {
		case u.methods <- r.Method:
		default:
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "origin")
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *upstream) count() int64 { return atomic.LoadInt64(u.hits) }

// port is the origin's TCP port, which every proxied URL has to carry because
// the resolver only rewrites the host.
func (u *upstream) port() string {
	parsed, err := url.Parse(u.server.URL)
	if err != nil {
		panic(err)
	}
	return parsed.Port()
}

// loopbackResolver answers every lookup with 127.0.0.1, so a test can use a
// made-up host name and still reach httptest. Policies in these tests set
// AllowPrivate so CheckResolvedIPs does not reject the loopback answer.
type loopbackResolver struct{}

func (loopbackResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}, nil
}

func newTestProxy(t *testing.T, policy Policy) *Proxy {
	t.Helper()
	policy.AllowPrivate = true
	p, err := NewProxy(policy, loopbackResolver{})
	if err != nil {
		t.Fatalf("NewProxy: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// proxyClient is an http.Client that routes everything through p.
func proxyClient(p *Proxy) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(p.URL()),
			// The origin in these tests is httptest's own self-signed cert and
			// the proxy re-signs it; validation is exercised by its own test
			// (TestInspectedProxyPresentsItsOwnRoot), not by every case here.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test client
		},
	}
}

// TestProxyRefusesAnUnapprovedHostOverPlainHTTP is the base interception
// claim: a host the policy does not admit gets 403 from the proxy and ZERO
// requests reach the origin.
func TestProxyRefusesAnUnapprovedHostOverPlainHTTP(t *testing.T) {
	origin := newUpstream(t)
	p := newTestProxy(t, Policy{Default: "deny"})

	resp, err := proxyClient(p).Get("http://denied.test:" + origin.port() + "/x")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if n := origin.count(); n != 0 {
		t.Fatalf("origin saw %d requests; the proxy refused and forwarded anyway", n)
	}
}

// TestProxyForwardsAnAllowedHostOverPlainHTTP is the other direction, so the
// test above cannot pass by the proxy being broken for everything.
func TestProxyForwardsAnAllowedHostOverPlainHTTP(t *testing.T) {
	origin := newUpstream(t)
	p := newTestProxy(t, Policy{Default: "deny", Allow: []string{"allowed.test"}})

	resp, err := proxyClient(p).Get("http://allowed.test:" + origin.port() + "/x")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if n := origin.count(); n != 1 {
		t.Fatalf("origin saw %d requests, want 1", n)
	}
}

// TestProxyRefusesAnUnapprovedHostOverCONNECT covers the HTTPS path in its
// default (blind) mode. The refusal must happen BEFORE the tunnel is spliced,
// which is observable as the origin never being dialled.
func TestProxyRefusesAnUnapprovedHostOverCONNECT(t *testing.T) {
	origin := newTLSUpstream(t)
	p := newTestProxy(t, Policy{Default: "deny"})

	_, err := proxyClient(p).Get("https://denied.test:" + origin.port() + "/x")
	if err == nil {
		t.Fatal("CONNECT to a denied host succeeded")
	}
	if !strings.Contains(err.Error(), "Forbidden") && !strings.Contains(err.Error(), "403") {
		t.Fatalf("error does not name the refusal: %v", err)
	}
	if n := origin.count(); n != 0 {
		t.Fatalf("origin saw %d requests through a refused CONNECT", n)
	}
}

// socks5Connect performs a SOCKS5 handshake against addr for host:port and
// returns the connection plus the reply code. It is written out rather than
// pulled from a dependency because the point is to exercise the wire format
// this proxy claims to speak.
func socks5Dial(t *testing.T, addr, host string, port uint16) (net.Conn, byte) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := conn.Write([]byte{socks5Version, 1, socks5NoAuth}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		t.Fatalf("greeting reply: %v", err)
	}
	if greeting[0] != socks5Version || greeting[1] != socks5NoAuth {
		t.Fatalf("greeting reply = %v", greeting)
	}

	req := []byte{socks5Version, socks5Connect, 0x00, socks5AddrDomain, byte(len(host))}
	req = append(req, host...)
	req = binary.BigEndian.AppendUint16(req, port)
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("connect request: %v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("connect reply: %v", err)
	}
	return conn, reply[1]
}

// TestProxyRefusesAnUnapprovedHostOverSOCKS5 is the third protocol. SOCKS5 is
// the one an ALL_PROXY-honouring client uses, and before this it was not
// spoken at all — those clients simply connected directly.
func TestProxyRefusesAnUnapprovedHostOverSOCKS5(t *testing.T) {
	origin := newUpstream(t)
	p := newTestProxy(t, Policy{Default: "deny"})

	port, err := parsePort(origin.port())
	if err != nil {
		t.Fatal(err)
	}
	_, code := socks5Dial(t, p.listener.Addr().String(), "denied.test", port)
	if code != socks5ReplyRuleset {
		t.Fatalf("SOCKS5 reply = 0x%02x, want 0x%02x (not allowed by ruleset)", code, socks5ReplyRuleset)
	}
	if n := origin.count(); n != 0 {
		t.Fatalf("origin saw %d requests through a refused SOCKS5 CONNECT", n)
	}
}

// TestProxyTunnelsAnAllowedHostOverSOCKS5 proves the SOCKS5 path is a working
// tunnel and not a handler that refuses everything — without it the test above
// would pass on a stub.
func TestProxyTunnelsAnAllowedHostOverSOCKS5(t *testing.T) {
	origin := newUpstream(t)
	p := newTestProxy(t, Policy{Default: "deny", Allow: []string{"allowed.test"}})

	port, err := parsePort(origin.port())
	if err != nil {
		t.Fatal(err)
	}
	conn, code := socks5Dial(t, p.listener.Addr().String(), "allowed.test", port)
	if code != socks5ReplyOK {
		t.Fatalf("SOCKS5 reply = 0x%02x, want 0x00", code)
	}
	if _, err := fmt.Fprintf(conn, "GET /x HTTP/1.1\r\nHost: allowed.test\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if !strings.Contains(string(body), "200 OK") {
		t.Fatalf("tunnelled response = %q", body)
	}
	if n := origin.count(); n != 1 {
		t.Fatalf("origin saw %d requests, want 1", n)
	}
}

// TestProxyRefusesSOCKS5Bind pins the refusal of the commands this proxy
// cannot police. Answering 0x00 to a BIND would hand the child a listening
// socket nothing watches.
func TestProxyRefusesSOCKS5Bind(t *testing.T) {
	p := newTestProxy(t, Policy{Default: "allow"})
	conn, err := net.DialTimeout("tcp", p.listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{socks5Version, 1, socks5NoAuth}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	if _, err := io.ReadFull(conn, make([]byte, 2)); err != nil {
		t.Fatalf("greeting reply: %v", err)
	}
	// 0x02 = BIND.
	req := []byte{socks5Version, 0x02, 0x00, socks5AddrDomain, byte(len("x.test"))}
	req = append(req, "x.test"...)
	req = binary.BigEndian.AppendUint16(req, 80)
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("bind request: %v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("bind reply: %v", err)
	}
	if reply[1] != socks5ReplyCmdNotSupp {
		t.Fatalf("BIND reply = 0x%02x, want 0x%02x", reply[1], socks5ReplyCmdNotSupp)
	}
}

func parsePort(s string) (uint16, error) {
	var port uint16
	_, err := fmt.Sscanf(s, "%d", &port)
	return port, err
}
