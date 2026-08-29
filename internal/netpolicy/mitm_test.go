package netpolicy

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// trustOrigin points the proxy's UPSTREAM transport at the httptest server's
// own root, so the proxy validates the origin the way it would validate a real
// site. Reaching into the client here (rather than adding a production knob)
// keeps the seam a test detail: the shipped proxy uses the system pool.
func trustOrigin(t *testing.T, p *Proxy, origin *upstream) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(origin.server.Certificate())
	transport, ok := p.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("proxy client transport is not *http.Transport")
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

func newTestCA(t *testing.T) *CertAuthority {
	t.Helper()
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	return ca
}

// inspectingClient trusts the proxy's generated root — which is exactly what
// CAEnv arranges for a real child process — so a handshake failure in these
// tests means the proxy minted something wrong, not that the test skipped
// validation.
func inspectingClient(t *testing.T, p *Proxy, ca *CertAuthority) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(ca.CertPath())
	if err != nil {
		t.Fatalf("read ca: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("generated CA PEM did not parse")
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(p.URL()),
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
}

// TestInspectedConnectJudgesGetAndPostSeparately is W-B-17's acceptance in one
// test: one host, two methods, two different verdicts, and the origin sees
// only the permitted one.
func TestInspectedConnectJudgesGetAndPostSeparately(t *testing.T) {
	origin := newTLSUpstream(t)
	p := newTestProxy(t, Policy{
		Default: "deny",
		Allow:   []string{"example.com"},
		Methods: []MethodRule{{Host: "example.com", Methods: []string{"POST"}, Allow: false}},
	})
	trustOrigin(t, p, origin)
	ca := newTestCA(t)
	p.SetCertAuthority(ca)
	client := inspectingClient(t, p, ca)

	base := "https://example.com:" + origin.port() + "/x"
	getResp, err := client.Get(base)
	if err != nil {
		t.Fatalf("GET through inspected tunnel: %v", err)
	}
	defer getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", getResp.StatusCode)
	}

	postResp, err := client.Post(base, "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("POST through inspected tunnel: %v", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST status = %d, want 403", postResp.StatusCode)
	}

	if n := origin.count(); n != 1 {
		t.Fatalf("origin saw %d requests, want exactly the GET", n)
	}
	select {
	case m := <-origin.methods:
		if m != http.MethodGet {
			t.Fatalf("origin saw %q, want GET", m)
		}
	default:
		t.Fatal("origin recorded no method")
	}
}

// TestBlindConnectCannotJudgeTheMethod is the negative control that makes the
// test above mean something.
//
// The SAME policy, the SAME two requests, inspection OFF: both get through,
// because inside a blind tunnel there is no method for a method rule to match.
// If this test ever went green while the one above did too, the method rules
// would be firing somewhere they cannot possibly have read a method.
func TestBlindConnectCannotJudgeTheMethod(t *testing.T) {
	origin := newTLSUpstream(t)
	p := newTestProxy(t, Policy{
		Default: "deny",
		Allow:   []string{"example.com"},
		Methods: []MethodRule{{Host: "example.com", Methods: []string{"POST"}, Allow: false}},
	})
	client := proxyClient(p) // InsecureSkipVerify: the tunnel is opaque, so the
	// client validates the ORIGIN's self-signed certificate directly.

	base := "https://example.com:" + origin.port() + "/x"
	for _, call := range []func() (*http.Response, error){
		func() (*http.Response, error) { return client.Get(base) },
		func() (*http.Response, error) { return client.Post(base, "text/plain", strings.NewReader("p")) },
	} {
		resp, err := call()
		if err != nil {
			t.Fatalf("request through blind tunnel: %v", err)
		}
		_ = resp.Body.Close()
	}
	if n := origin.count(); n != 2 {
		t.Fatalf("origin saw %d requests, want 2 — a blind tunnel cannot filter by method", n)
	}
}

// TestInspectedConnectStillEnforcesTheHostDeny checks that turning inspection
// on did not replace the host dimension with the method one.
func TestInspectedConnectStillEnforcesTheHostDeny(t *testing.T) {
	origin := newTLSUpstream(t)
	p := newTestProxy(t, Policy{Default: "deny"})
	trustOrigin(t, p, origin)
	ca := newTestCA(t)
	p.SetCertAuthority(ca)

	_, err := inspectingClient(t, p, ca).Get("https://denied.test:" + origin.port() + "/x")
	if err == nil {
		t.Fatal("a denied host reached the origin with inspection on")
	}
	if n := origin.count(); n != 0 {
		t.Fatalf("origin saw %d requests", n)
	}
}

// TestCertAuthorityMintsAValidChainForTheRequestedHost pins the two properties
// a child's TLS stack checks: the leaf validates up to the generated root, and
// it names the host that was asked for.
func TestCertAuthorityMintsAValidChainForTheRequestedHost(t *testing.T) {
	ca := newTestCA(t)
	leaf, err := ca.leafForHost("Example.Test")
	if err != nil {
		t.Fatalf("leafForHost: %v", err)
	}
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := parsed.Verify(x509.VerifyOptions{DNSName: "example.test", Roots: pool}); err != nil {
		t.Fatalf("minted leaf does not verify: %v", err)
	}
}

// TestCertAuthorityRefusesAClientHelloWithNoServerName pins the refusal: a
// certificate for a name nobody stated would have to be a wildcard, and this
// process is not issuing one of those.
func TestCertAuthorityRefusesAClientHelloWithNoServerName(t *testing.T) {
	ca := newTestCA(t)
	if _, err := ca.LeafFor(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("LeafFor accepted a ClientHello with no SNI")
	}
	if _, err := ca.LeafFor(nil); err == nil {
		t.Fatal("LeafFor accepted a nil ClientHello")
	}
}

// TestCertAuthorityRegeneratesWhenTheKeyIsWorldReadable is the private-key
// half of ADR-0023's trust boundary. A readable MITM root is a standing
// authority to impersonate every site the operator visits.
func TestCertAuthorityRegeneratesWhenTheKeyIsWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode says nothing about the ACL on windows; see checkKeyPerm")
	}
	dir := t.TempDir()
	first := newTestCAIn(t, dir)
	keyPath := filepath.Join(dir, caKeyFile)
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	second := newTestCAIn(t, dir)
	if first.cert.SerialNumber.Cmp(second.cert.SerialNumber) == 0 {
		t.Fatal("a world-readable CA key was loaded and reused")
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("regenerated key is mode %04o", mode)
	}
}

// TestCertAuthorityReusesAPrivateKey is the other direction: the check above
// must not be regenerating on every start.
func TestCertAuthorityReusesAPrivateKey(t *testing.T) {
	dir := t.TempDir()
	first := newTestCAIn(t, dir)
	second := newTestCAIn(t, dir)
	if first.cert.SerialNumber.Cmp(second.cert.SerialNumber) != 0 {
		t.Fatal("a correctly-permissioned CA was regenerated instead of loaded")
	}
}

func newTestCAIn(t *testing.T, dir string) *CertAuthority {
	t.Helper()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}
	return ca
}

// TestCAEnvIsEmptyWithoutInspection pins the opt-in end of ADR-0023: with
// inspection off, not one certificate variable appears in a child's
// environment, so its trust store is what it would have been with this feature
// absent.
func TestCAEnvIsEmptyWithoutInspection(t *testing.T) {
	if got := CAEnv(""); got != nil {
		t.Fatalf("CAEnv(\"\") = %v, want nil", got)
	}
	env := PrepareEnvFor([]string{"PATH=x", "SSL_CERT_FILE=/operator/bundle.pem"},
		ManagedProxy{HTTPURL: "http://127.0.0.1:1"})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "SSL_CERT_FILE=/operator/bundle.pem") {
		t.Fatalf("the operator's own SSL_CERT_FILE was stripped with inspection off: %v", env)
	}
	for _, key := range caTrustEnvKeys {
		if strings.Contains(joined, key+"=/") && key != "SSL_CERT_FILE" {
			t.Fatalf("%s published with inspection off: %v", key, env)
		}
	}
}

// TestCAEnvReplacesAnInheritedBundle is the on case: the child must trust OUR
// root, and an inherited bundle that does not contain it would fail every
// handshake with an error naming the site rather than the proxy.
func TestCAEnvReplacesAnInheritedBundle(t *testing.T) {
	env := PrepareEnvFor([]string{"PATH=x", "SSL_CERT_FILE=/operator/bundle.pem"},
		ManagedProxy{HTTPURL: "http://127.0.0.1:1", CAFile: "/tmp/ca.pem"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "/operator/bundle.pem") {
		t.Fatalf("inherited bundle survived: %v", env)
	}
	for _, key := range caTrustEnvKeys {
		if !strings.Contains(joined, key+"=/tmp/ca.pem") {
			t.Fatalf("%s not published: %v", key, env)
		}
	}
}

// TestManagedProxyPublishesTheSocksEndpoint pins the ALL_PROXY half of
// W-B-16. Without it the SOCKS5 listener has no producer and the clients it
// exists for keep connecting directly.
func TestManagedProxyPublishesTheSocksEndpoint(t *testing.T) {
	env := PrepareEnvFor([]string{"ALL_PROXY=inherited"},
		ManagedProxy{HTTPURL: "http://127.0.0.1:1", SOCKSURL: "socks5h://127.0.0.1:1"})
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "inherited") {
		t.Fatalf("inherited ALL_PROXY survived: %v", env)
	}
	for _, want := range []string{"ALL_PROXY=socks5h://127.0.0.1:1", "all_proxy=socks5h://127.0.0.1:1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q: %v", want, env)
		}
	}
}
