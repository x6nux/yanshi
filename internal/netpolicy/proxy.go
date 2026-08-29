package netpolicy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
	// Granted, when set, is consulted for a host the rules refuse. It is how a
	// runtime approval reaches the DIAL: the proxy's approver widens what may
	// be reached, and without this hook the dialer would re-apply the
	// unwidened rules and refuse the connection the operator just allowed.
	//
	// nil for every non-proxy caller (web_fetch's NewTransport), which is why
	// those keep the plain rules-only behaviour.
	//
	// It does NOT bypass the resolved-IP check. See Policy.checkIPRanges.
	Granted func(host string) bool
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
	granted := d.Granted != nil && d.Granted(host)
	if d.Policy != nil && !granted {
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
		// The IP half runs on BOTH paths, granted or not.
		if dec := d.Policy.checkIPRanges(ips); !dec.Allowed {
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

// Request is one egress attempt presented for a decision. Protocol is "http",
// "connect", "https" (a request read INSIDE an inspected CONNECT) or "socks5".
// Method is empty whenever this layer did not read one — a blind CONNECT and a
// SOCKS5 tunnel both carry bytes, not verbs.
//
// It deliberately carries no URL path, no headers and no body. Those are the
// fields an approval dialog would be tempted to show and the audit log would
// then keep, and a path routinely carries a bearer token in a query parameter.
// See ADR-0023.
type Request struct {
	Protocol string
	Host     string
	Method   string
}

// Approver is asked about an egress request the static policy did not admit.
// It returns true to let this request through.
//
// A nil Approver means nobody is available to ask, and the policy verdict
// stands — which for the default policy is a refusal. That is the fail-closed
// direction and it is the behaviour that existed before this hook: an
// unapproved host does not reach the network because there was no one to
// approve it.
//
// Implementations MUST NOT block indefinitely; the child process is sitting on
// a half-open connection for the whole call. bootstrap's implementation bounds
// itself and answers "deny" on timeout.
type Approver interface {
	ApproveEgress(ctx context.Context, req Request) bool
}

// Proxy is the loopback proxy yanshi runs so child processes spawned with
// HTTP_PROXY/HTTPS_PROXY/ALL_PROXY pointing at it have their traffic filtered.
// It speaks plain HTTP, HTTP CONNECT and SOCKS5 on ONE loopback port
// (protocols are told apart by their first byte; see socks5.go).
//
// CONNECT has two modes. Blind by default: host policy, then an opaque splice,
// which is what ADR-0014 decided. When an inspecting CertAuthority is
// installed (opt-in, ADR-0023) the tunnel is terminated here instead and each
// request inside it is judged on host AND method.
type Proxy struct {
	policy   Policy
	resolver Resolver
	dialer   *PolicyDialer
	listener net.Listener
	server   *http.Server
	client   *http.Client
	// conns feeds the HTTP server from the demultiplexing accept loop.
	conns *chanListener
	// ctx is canceled by Close and is the parent of every dial this proxy
	// makes on behalf of a tunnel, which has no request context of its own.
	ctx    context.Context
	cancel context.CancelFunc

	// mu guards the two fields that are installed after construction (the
	// approver is wired once the HTTP server exists, which is later) and the
	// runtime grant set.
	mu       sync.RWMutex
	approver Approver
	ca       *CertAuthority
	// granted holds hosts an Approver admitted, so a build that fetches forty
	// files from one host asks once. Cleared only by process exit; a durable
	// grant is the approval manager's job, not this map's.
	granted map[string]bool
}

// NewProxy binds a loopback listener on 127.0.0.1:0 (kernel-chosen port) and
// starts serving. The returned Proxy's URL() is what callers hand to child
// processes via PrepareEnvFor. resolver may be nil (defaults to
// net.DefaultResolver).
func NewProxy(policy Policy, resolver Resolver) (*Proxy, error) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Proxy{
		policy:   policy,
		resolver: resolver,
		listener: ln,
		ctx:      ctx,
		cancel:   cancel,
		granted:  map[string]bool{},
	}
	// Point the dialer at &p.policy (the struct field, not the local param) so
	// the pointer stays valid for the Proxy's whole lifetime.
	p.dialer = &PolicyDialer{Policy: &p.policy, Resolver: resolver, Granted: p.isGranted}
	transport := &http.Transport{Proxy: nil, DialContext: p.dialer.DialContext, ForceAttemptHTTP2: false}
	// CheckRedirect = ErrUseLastResponse so the proxy itself does not follow
	// redirects. The downstream client (child process) sees the 3xx and issues
	// a new request via the proxy, which re-runs CheckHost + CheckResolvedIPs
	// on the redirect target — per-hop authorization.
	p.client = &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	p.conns = newChanListener(ln.Addr())
	go p.acceptLoop()
	go func() { _ = p.server.Serve(p.conns) }()
	return p, nil
}

// SetApprover installs the interactive approver. It is a setter rather than a
// constructor argument because the thing that can ask a human — the WebSocket
// server — is built after the proxy is: bootstrap needs the proxy URL to
// assemble the launch posture that the server's tools use.
//
// Safe to call at any time; requests already in flight see whichever value
// they read.
func (p *Proxy) SetApprover(a Approver) {
	p.mu.Lock()
	p.approver = a
	p.mu.Unlock()
}

// SetCertAuthority turns HTTPS inspection on. Passing nil (the default) leaves
// CONNECT as the blind tunnel ADR-0014 specified.
func (p *Proxy) SetCertAuthority(ca *CertAuthority) {
	p.mu.Lock()
	p.ca = ca
	p.mu.Unlock()
}

// HasApprover reports whether anything is able to answer an egress question
// this proxy cannot answer from the static policy.
//
// It exists for a wiring assertion at the composition root. SetApprover
// happens several hundred lines after NewProxy, because the object that can
// reach a human is the HTTP server and that is built out of the proxy's own
// URL — so "the call was never made" is a live failure mode, and from outside
// it is indistinguishable from an approver that refuses everything.
func (p *Proxy) HasApprover() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.approver != nil
}

// certAuthority reads the installed CA under the lock.
func (p *Proxy) certAuthority() *CertAuthority {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ca
}

// baseCtx is the parent context for work with no request of its own (a SOCKS5
// dial, an inspected tunnel's upstream call). Close cancels it.
func (p *Proxy) baseCtx() context.Context { return p.ctx }

// URL is the proxy URL to hand to child processes (via PrepareEnvFor →
// HTTP_PROXY/HTTPS_PROXY).
func (p *Proxy) URL() *url.URL {
	u, _ := url.Parse("http://" + p.listener.Addr().String())
	return u
}

// SOCKSURL is the same endpoint spelled for SOCKS-speaking clients, which is
// what ALL_PROXY is set to. socks5h (rather than socks5) keeps NAME
// RESOLUTION on the proxy side: with socks5 the client resolves the host
// itself and sends an IP, and an IP defeats both the host policy and the
// per-domain approval, since neither has a name left to judge.
func (p *Proxy) SOCKSURL() string { return "socks5h://" + p.listener.Addr().String() }

// Close shuts the proxy down: the accept loop stops, in-flight tunnels have
// their base context canceled, and the HTTP server closes. Safe to call
// multiple times.
func (p *Proxy) Close() error {
	p.cancel()
	err := p.server.Close()
	_ = p.listener.Close()
	p.conns.close()
	return err
}

// acceptLoop demultiplexes SOCKS5 from HTTP on the single listener.
//
// The peek happens on a per-connection goroutine, never in this loop: Peek
// blocks until the client writes, so peeking inline would let one silent
// connection stall every other client's accept.
func (p *Proxy) acceptLoop() {
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			p.conns.close()
			return
		}
		go p.route(conn)
	}
}

// route reads the first byte and hands the connection to the right protocol.
// 0x05 is the SOCKS5 version byte and cannot begin an HTTP request line (no
// HTTP method starts with a control character), so one byte separates them
// unambiguously.
func (p *Proxy) route(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(socks5HandshakeDeadline))
	reader := bufio.NewReader(conn)
	first, err := reader.Peek(1)
	if err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	buffered := &peekConn{Conn: conn, reader: reader}
	if first[0] == socks5Version {
		p.serveSOCKS5(buffered)
		return
	}
	if !p.conns.offer(buffered) {
		_ = conn.Close()
	}
}

// ServeHTTP implements http.Handler. Every method is gated on the policy;
// CONNECT is handled separately (blind splice, or terminated and inspected).
// Per-hop authorization on redirects falls out of CheckRedirect above: each
// new upstream request re-enters ServeHTTP and re-runs the policy check.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.serveConnect(w, r)
		return
	}
	// Plain HTTP through a proxy carries an absolute-form request target, so
	// the method IS readable here without any decryption — the method
	// dimension has always been available on this half and only CONNECT hid
	// it.
	host := r.URL.Hostname()
	d := p.authorize(r.Context(), "http", host, r.Method)
	p.audit("http", host, d)
	if !d.Allowed {
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

// authorize is the single decision point every protocol path goes through:
// static policy, then the runtime grant set, then the Approver.
//
// One function rather than a check per path is the whole point. The HTTP,
// CONNECT, inspected-HTTPS and SOCKS5 paths reached the policy by four
// different routes before, and "which of them consults approvals" would have
// been four separate answers that drift. A caller cannot opt out: there is no
// route to the upstream dialer that does not come through here.
//
// An explicit deny rule is never offered for approval. GrantHost already draws
// that line for the tool-facing dialog and the reason is identical here: a
// deny entry is the operator naming this host and saying no, and a prompt able
// to undo it would make security.network.deny advisory.
func (p *Proxy) authorize(ctx context.Context, protocol, host, method string) Decision {
	d := p.policy.CheckRequest(host, method)
	if d.Allowed {
		return d
	}
	normalized := normalizeHost(host)
	if normalized == "" {
		return d
	}
	// A method rule refusing a method inside an admitted host is a policy
	// statement about THIS request, not a "not permitted yet" — asking about
	// it would let one approval reopen what the operator narrowed.
	if p.policy.CheckHost(normalized).Allowed {
		return d
	}
	if _, grantable := p.policy.GrantHost(normalized); !grantable {
		return d
	}
	p.mu.RLock()
	granted, approver := p.granted[normalized], p.approver
	p.mu.RUnlock()
	if granted {
		return Decision{Allowed: true, Rule: "approval:" + normalized, Reason: "host approved earlier in this process"}
	}
	if approver == nil {
		return d
	}
	if !approver.ApproveEgress(ctx, Request{Protocol: protocol, Host: normalized, Method: method}) {
		return d
	}
	p.mu.Lock()
	p.granted[normalized] = true
	p.mu.Unlock()
	return Decision{Allowed: true, Rule: "approval:" + normalized, Reason: "host approved interactively"}
}

// isGranted reports whether an approver already admitted this host in this
// process. It is the dialer's half of the grant; authorize() is the handler's.
//
// Two consumers of one map rather than one, because the decision and the dial
// are separate layers and the policy the dialer re-checks is the unwidened
// one. Everything in this map got there through authorize(), which never
// offers an explicitly denied host — so this cannot admit anything
// security.network.deny refused.
func (p *Proxy) isGranted(host string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.granted[normalizeHost(host)]
}

// peekConn is a net.Conn whose reads come from a bufio.Reader that has already
// consumed the first byte. Without it the byte used to identify the protocol
// would be missing from the stream the protocol handler then parses.
type peekConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *peekConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

// chanListener is a net.Listener fed by the demultiplexing accept loop rather
// than by the kernel, so http.Server can keep its ordinary Serve loop while
// SOCKS connections are diverted before they ever reach it.
type chanListener struct {
	ch   chan net.Conn
	done chan struct{}
	addr net.Addr
	once sync.Once
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{ch: make(chan net.Conn), done: make(chan struct{}), addr: addr}
}

// offer hands conn to the HTTP server, reporting false once the listener is
// closed so the caller can close the connection instead of leaking it.
func (l *chanListener) offer(conn net.Conn) bool {
	select {
	case l.ch <- conn:
		return true
	case <-l.done:
		return false
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.ch:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error { l.close(); return nil }

func (l *chanListener) close() { l.once.Do(func() { close(l.done) }) }

func (l *chanListener) Addr() net.Addr { return l.addr }

// ManagedProxy is everything a child needs in order to reach the managed proxy
// AND to believe what comes back through it.
//
// SOCKSURL and CAFile are separate fields rather than derived from HTTPURL
// because each is independently optional: SOCKS is always available when a
// proxy is running, but the CA exists only when the operator opted into HTTPS
// inspection, and a child pointed at a CA file that does not exist fails every
// TLS handshake it attempts.
type ManagedProxy struct {
	HTTPURL  string
	SOCKSURL string
	// CAFile is the inspecting root's PEM path, or "" when inspection is off.
	// Empty means the child's certificate trust is left exactly as inherited —
	// no variable is added AND none is stripped, so an operator's own
	// SSL_CERT_FILE keeps working.
	CAFile string
}

// PrepareEnvFor returns a new environment slice derived from `in` with all
// inherited proxy-related variables (HTTP_PROXY/HTTPS_PROXY/NO_PROXY/ALL_PROXY
// in any case) stripped, then appends the managed proxy entries pointing at
// mp in BOTH cases. Returning a new slice (rather than mutating in place)
// keeps the caller's slice clean for reuse.
//
// The case-insensitive strip is deliberate: child processes inherit envs in
// arbitrary case depending on the parent (Windows cmd.exe uppercases; POSIX
// shells preserve). Without the strip, a stale HTTPS_PROXY from the parent
// would shadow the managed URL and the policy would be silently bypassed.
//
// Publishing BOTH cases is equally deliberate and is a correctness fix, not
// belt-and-braces. curl (and therefore libcurl, git-over-HTTP, and everything
// shelling out to curl) deliberately IGNORES uppercase HTTP_PROXY for plain
// http:// requests — it honors only lowercase http_proxy there, because a CGI
// script's inbound "Proxy:" header arrives as HTTP_PROXY and would otherwise
// redirect the script's own egress (CVE-2016-5385, "httpoxy"). https_proxy is
// honored in both cases; http_proxy is not. Publishing only the uppercase set
// therefore left plain-HTTP egress WIDE OPEN for the single most common client
// in a shell tool:
//
//	env HTTP_PROXY=http://127.0.0.1:0 curl http://example.com/  -> 200
//	env http_proxy=http://127.0.0.1:0 curl http://example.com/  -> connect refused
//
// The two cases always carry the SAME value here, so the duplicate keys are
// inert on Windows (exec dedups env case-insensitively, last wins) and are two
// distinct, consistent variables on POSIX.
//
// This publishes proxy variables; it does not enforce them. See
// shell.childLaunchPosture.proxy for what that buys and what it does not.
func PrepareEnvFor(in []string, mp ManagedProxy) []string {
	blocked := map[string]bool{
		"http_proxy": true, "https_proxy": true, "no_proxy": true, "all_proxy": true,
	}
	caEnv := CAEnv(mp.CAFile)
	if len(caEnv) > 0 {
		// Only strip the trust variables when we are about to publish our own.
		// A child that inherited SSL_CERT_FILE from the operator and is now
		// being handed a forged certificate chain would otherwise fail every
		// handshake, with an error that names the site rather than the proxy.
		for _, key := range caTrustEnvKeys {
			blocked[strings.ToLower(key)] = true
		}
	}
	out := make([]string, 0, len(in)+len(managedProxyKeys)+len(caEnv)+2)
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
	for _, key := range managedProxyKeys {
		if key == "NO_PROXY" || key == "no_proxy" {
			out = append(out, key+"=")
			continue
		}
		out = append(out, key+"="+mp.HTTPURL)
	}
	if mp.SOCKSURL != "" {
		// Both cases again, and for the same reason as the http pair: which
		// spelling a client honours is client-specific.
		out = append(out, "ALL_PROXY="+mp.SOCKSURL, "all_proxy="+mp.SOCKSURL)
	}
	return append(out, caEnv...)
}

// managedProxyKeys is the exact set PrepareEnvFor publishes. Both cases of
// each name are present on purpose — see PrepareEnvFor for why the lowercase
// spellings are load-bearing rather than redundant.
var managedProxyKeys = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// serveConnect gates a CONNECT on its target host and, when allowed, splices
// the two connections together without looking inside.
//
// Blind on purpose. Terminating TLS here would put every secret a child sends
// -- API keys, tokens, the contents of its prompts -- in this process's
// memory and its logs, trading a network boundary for a much larger secret
// exposure. Host-level policy is what this layer can enforce honestly.
//
// Refusing CONNECT outright, as this did before, is not the safe default it
// looks like: it means the managed proxy cannot be switched on at all without
// breaking every HTTPS-speaking child, which is why the proxy was never
// started and children got a placeholder URL instead.
func (p *Proxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	// A CONNECT request line carries "host:port", not a URL, so r.Host is the
	// authority and r.URL is whatever url.Parse made of it. Reading r.URL
	// first looks equivalent and is not: a client that sends an absolute-form
	// target makes Hostname() return the scheme, and the policy then judges
	// the string "http" instead of the host being reached. Found by the audit
	// test, which printed host=http for a request aimed at denied.example.
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		host = r.URL.Hostname()
	}
	// Audit the decision the policy actually returned, on BOTH branches. An
	// allow carries a Reason too (which rule admitted the host), and
	// fabricating a bare Decision{Allowed: true} here would drop it -- the
	// same shape of loss as execpolicy's Justification dying at the tool
	// boundary. "决策入审计" means the decision, not a re-derived verdict.
	//
	// The method is empty at THIS point on both branches, including the
	// inspecting one: the CONNECT line names a host and nothing else. Under
	// inspection each request read inside the tunnel is authorized again, with
	// its method, by serveInspected.
	d := p.authorize(r.Context(), "connect", host, "")
	p.audit("connect", host, d)
	if !d.Allowed {
		http.Error(w, d.Reason, http.StatusForbidden)
		return
	}

	target := r.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "proxy: connection cannot be hijacked", http.StatusInternalServerError)
		return
	}

	if ca := p.certAuthority(); ca != nil {
		// Hijack BEFORE dialing upstream. The inspected path opens its
		// upstream connections per request through p.client, so dialing here
		// first would open a socket that the TLS handshake below may never
		// use — and leave it open for the life of a handshake that fails.
		client, _, err := hj.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer client.Close()
		if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return
		}
		p.serveInspected(client, ca, host)
		return
	}

	upstream, err := p.dialer.DialContext(r.Context(), "tcp", target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	client, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer client.Close()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	splice(client, upstream)
}

// serveInspected terminates the child's TLS session with a certificate this
// process minted, then reads and authorizes each request the child sends
// inside it.
//
// This is the whole of "CONNECT is no longer a blind tunnel". Before it, the
// only fact available about an HTTPS request was the host in the CONNECT line,
// so `GET https://api.example/read` and `POST https://api.example/admin/delete`
// were one decision. Here they are two, because the request line is in
// plaintext by the time it is read.
//
// # What is NOT done here
//
// Nothing reads or copies the body: it is streamed through io.Copy inside
// http.Client and never lands in a variable this package owns. Nothing logs a
// URL path or a header. The proxy holds provider API keys in the same process,
// and the reason ADR-0014 refused this capability was the RECORD it would
// create — so the record is host and method, exactly what the decision needed.
//
// # Error handling
//
// A handshake the child refuses (it does not trust the root) ends the
// connection with no reply, which is the only thing TLS allows. That is a
// visible failure at the child, not a silent fallthrough to an unfiltered
// connection.
func (p *Proxy) serveInspected(client net.Conn, ca *CertAuthority, host string) {
	tlsConn := tls.Server(client, &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			// A client that sends no SNI still told us the host in the CONNECT
			// line; fall back to it rather than failing the handshake over a
			// name we already have.
			if hello != nil && hello.ServerName != "" {
				return ca.LeafFor(hello)
			}
			return ca.leafForHost(host)
		},
	})
	defer tlsConn.Close()
	if err := tlsConn.HandshakeContext(p.baseCtx()); err != nil {
		return
	}
	reader := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		if !p.forwardInspected(tlsConn, req, host) {
			return
		}
	}
}

// forwardInspected authorizes and forwards one request read inside an
// inspected tunnel. It reports whether the connection may carry another.
func (p *Proxy) forwardInspected(conn net.Conn, req *http.Request, host string) bool {
	defer func() {
		if req.Body != nil {
			_ = req.Body.Close()
		}
	}()
	// The request inside a tunnel is origin-form (no scheme, no authority), so
	// the URL has to be completed from the CONNECT target before it can be
	// sent anywhere.
	req.URL.Scheme = "https"
	if req.URL.Host == "" {
		req.URL.Host = req.Host
	}
	if req.URL.Host == "" {
		req.URL.Host = host
	}
	target := req.URL.Hostname()
	if target == "" {
		target = host
	}

	d := p.authorize(p.baseCtx(), "https", target, req.Method)
	p.audit("https "+req.Method, target, d)
	if !d.Allowed {
		return writeInspectedError(conn, req, http.StatusForbidden, d.Reason)
	}

	out := req.Clone(p.baseCtx())
	out.RequestURI = ""
	out.Header = req.Header.Clone()
	out.Header.Del("Proxy-Authorization")
	resp, err := p.client.Do(out)
	if err != nil {
		return writeInspectedError(conn, req, http.StatusBadGateway, err.Error())
	}
	defer resp.Body.Close()
	// resp.Write emits the status line, headers and body straight onto the
	// TLS connection; the body is streamed, not buffered.
	if err := resp.Write(conn); err != nil {
		return false
	}
	return !req.Close && !resp.Close
}

// writeInspectedError answers one inspected request with a status and reason.
// It reports whether the connection survives, which it does only when the
// request was keep-alive.
func writeInspectedError(conn net.Conn, req *http.Request, status int, reason string) bool {
	body := reason + "\n"
	resp := &http.Response{
		Status:        http.StatusText(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Request:       req,
		Header:        http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	if err := resp.Write(conn); err != nil {
		return false
	}
	return !req.Close
}

// audit records a proxy decision. netpolicy had no logging at all, so a
// refused connection left nothing behind for an operator to find -- the
// acceptance criteria ask for decisions to reach the audit trail, and a
// policy nobody can observe is indistinguishable from no policy.
func (p *Proxy) audit(op, host string, d Decision) {
	verdict := "allow"
	if !d.Allowed {
		verdict = "deny"
	}
	slog.Info("netpolicy decision", "op", op, "host", host, "decision", verdict, "reason", d.Reason)
}
