package netpolicy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Resolver is the small DNS-resolution interface netpolicy consumes. The
// standard library's net.DefaultResolver satisfies it; tests substitute a
// fake to avoid hitting the real network.
type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// PolicyDialer is the shared host-policy dial path used by BOTH the loopback
// Proxy (Task 12) and web_fetch's direct HTTP transport (Task 13). It resolves
// the host, re-checks every resolved IP against CheckResolvedIPs (rejecting
// private/loopback/link-local), then pins the connection to the first allowed
// IP — closing the DNS-rebinding seam (net.Dialer would otherwise re-resolve
// the hostname). Factoring this into one type means ordinary web_fetch also
// runs resolve→CheckResolvedIPs→pin, not just the proxy path.
type PolicyDialer struct {
	Policy   *Policy
	Resolver Resolver
}

// DialContext implements the http.Transport.DialContext contract. It runs the
// host-policy check, resolves the host via the configured Resolver (default
// net.DefaultResolver), runs the resolved-IP re-check, then pins the dial to
// the first surviving IP. The pin is what defeats DNS rebinding: net.Dialer
// given the hostname would re-resolve it and could reach a different IP the
// second time.
func (d *PolicyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	resolver := d.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if d.Policy != nil {
		if dec := d.Policy.CheckHost(host); !dec.Allowed {
			return nil, fmt.Errorf("netpolicy: %s", dec.Reason)
		}
	}
	resolved, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(resolved))
	for _, item := range resolved {
		ips = append(ips, item.IP)
	}
	if d.Policy != nil {
		if dec := d.Policy.CheckResolvedIPs(host, ips); !dec.Allowed {
			return nil, fmt.Errorf("netpolicy: %s", dec.Reason)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("netpolicy: no addresses resolved for %q", host)
	}
	// Pin to the exact IP that passed CheckResolvedIPs. Do not re-resolve via
	// net.Dialer with the hostname (DNS rebinding defense).
	pinned := net.JoinHostPort(ips[0].String(), port)
	return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, pinned)
}

// NewTransport returns an http.Transport whose DialContext is a PolicyDialer,
// so non-proxy HTTP clients (web_fetch) reuse the same host-policy +
// resolved-IP re-check + pinning as the loopback proxy. A nil policy disables
// the policy checks (test-only).
func NewTransport(policy *Policy) *http.Transport {
	return &http.Transport{
		DialContext:       (&PolicyDialer{Policy: policy, Resolver: net.DefaultResolver}).DialContext,
		ForceAttemptHTTP2: false,
	}
}

// Proxy is the loopback HTTP proxy yanshi runs so child processes spawned with
// HTTP_PROXY/HTTPS_PROXY pointing at it have their traffic filtered. It is
// Phase-0 honest: no MITM TLS interception (only plain HTTP upstream is
// proxied). HTTPS via CONNECT is explicitly unsupported in this phase — child
// processes that need HTTPS go through the direct path (NewTransport) instead.
type Proxy struct {
	policy   Policy
	resolver Resolver
	dialer   *PolicyDialer
	listener net.Listener
	server   *http.Server
	client   *http.Client
}

// NewProxy binds a loopback listener on 127.0.0.1:0 (kernel-chosen port) and
// starts serving. The returned Proxy's URL() is what callers hand to child
// processes via PrepareEnv. resolver may be nil (defaults to
// net.DefaultResolver).
func NewProxy(policy Policy, resolver Resolver) (*Proxy, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{policy: policy, resolver: resolver, listener: ln}
	// Point the dialer at &p.policy (the struct field, not the local param) so
	// the pointer stays valid for the Proxy's whole lifetime.
	p.dialer = &PolicyDialer{Policy: &p.policy, Resolver: resolver}
	transport := &http.Transport{Proxy: nil, DialContext: p.dialer.DialContext, ForceAttemptHTTP2: false}
	// CheckRedirect = ErrUseLastResponse so the proxy itself does not follow
	// redirects. The downstream client (child process) sees the 3xx and issues
	// a new request via the proxy, which re-runs CheckHost + CheckResolvedIPs
	// on the redirect target — per-hop authorization.
	p.client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.server.Serve(ln) }()
	return p, nil
}

// URL is the proxy URL to hand to child processes (via PrepareEnv →
// HTTP_PROXY/HTTPS_PROXY).
func (p *Proxy) URL() *url.URL {
	u, _ := url.Parse("http://" + p.listener.Addr().String())
	return u
}

// Close shuts the proxy server down. Safe to call multiple times.
func (p *Proxy) Close() error { return p.server.Close() }

// ServeHTTP implements http.Handler. CONNECT is rejected (Phase 0 scope);
// every other method is forwarded to the upstream after a CheckHost gate.
// Per-hop authorization on redirects falls out of CheckRedirect above: each
// new upstream request re-enters ServeHTTP and re-runs the policy check.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		http.Error(w, "CONNECT not enabled in Phase 0", http.StatusNotImplemented)
		return
	}
	if d := p.policy.CheckHost(r.URL.Hostname()); !d.Allowed {
		http.Error(w, d.Reason, http.StatusForbidden)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.Header = r.Header.Clone()
	out.Header.Del("Proxy-Authorization")
	resp, err := p.client.Do(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// PrepareEnv returns a new environment slice derived from `in` with all
// inherited proxy-related variables (HTTP_PROXY/HTTPS_PROXY/NO_PROXY/ALL_PROXY
// in any case) stripped, then appends the managed HTTP_PROXY/HTTPS_PROXY/NO_PROXY
// entries pointing at proxyURL. Returning a new slice (rather than mutating in
// place) keeps the caller's slice clean for reuse.
//
// The case-insensitive strip is deliberate: child processes inherit envs in
// arbitrary case depending on the parent (Windows cmd.exe uppercases; POSIX
// shells preserve). Without the strip, a stale HTTPS_PROXY from the parent
// would shadow the managed URL and the policy would be silently bypassed.
func PrepareEnv(in []string, proxyURL string) []string {
	blocked := map[string]bool{
		"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
	}
	out := make([]string, 0, len(in)+3)
	for _, item := range in {
		key := item
		if i := strings.IndexByte(key, '='); i >= 0 {
			key = key[:i]
		}
		if blocked[strings.ToLower(key)] {
			continue
		}
		out = append(out, item)
	}
	out = append(out, "HTTP_PROXY="+proxyURL, "HTTPS_PROXY="+proxyURL, "NO_PROXY=")
	return out
}

// ManagedEnv is the convenience wrapper for child-process spawning: it
// starts from os.Environ() and applies PrepareEnv in one call so the
// SecureProcessFactory (Task 14) has a single helper to call.
func ManagedEnv(proxyURL string) []string { return PrepareEnv(os.Environ(), proxyURL) }
