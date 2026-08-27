package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/mcp"
)

// memMCPTokens is an in-memory mcp.TokenStore that also satisfies
// mcp.TokenDeleter, so the logout path is reachable.
type memMCPTokens struct {
	mu      sync.Mutex
	data    map[string]mcp.StoredTokens
	deleted []string
}

func newMemMCPTokens() *memMCPTokens {
	return &memMCPTokens{data: map[string]mcp.StoredTokens{}}
}

func (m *memMCPTokens) LoadTokens(server string) (mcp.StoredTokens, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tok, ok := m.data[server]
	return tok, ok, nil
}

func (m *memMCPTokens) SaveTokens(server string, tok mcp.StoredTokens) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[server] = tok
	return nil
}

func (m *memMCPTokens) DeleteTokens(server string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, server)
	delete(m.data, server)
	return nil
}

// loginFixture wires a Manager with one authorization_code server pointed at a
// scripted token endpoint, plus an in-memory store.
func loginFixture(t *testing.T, tokenHandler http.HandlerFunc) (*mcp.Manager, *memMCPTokens, string) {
	t.Helper()
	tokenSrv := httptest.NewServer(tokenHandler)
	t.Cleanup(tokenSrv.Close)
	// The authorize endpoint is never fetched by the CLI (the URL is printed
	// for a human), so a loopback URL that nothing serves is fine — and it is
	// what AuthorizationURL's cleartext exemption exists for.
	authURL := "http://127.0.0.1:1/authorize"

	store := newMemMCPTokens()
	m := mcp.NewManager(map[string]*mcp.ServerConfig{
		"corp": {Enabled: true, Transport: mcp.TransportHTTP, URL: "https://corp/mcp",
			OAuth: &mcp.OAuthConfig{
				Grant: mcp.GrantAuthorizationCode, TokenURL: tokenSrv.URL,
				AuthorizationURL: authURL, ClientID: "cid", Scopes: []string{"offline_access"},
			}},
		"m2m": {Enabled: true, Transport: mcp.TransportHTTP, URL: "https://m2m/mcp",
			OAuth: &mcp.OAuthConfig{Grant: mcp.GrantClientCredentials, TokenURL: tokenSrv.URL, ClientID: "cid"}},
	})
	m.SetTokenStore(store)
	return m, store, tokenSrv.URL
}

// tokenOK is a token endpoint that returns a usable pair and records the form.
func tokenOK(forms *[]url.Values, mu *sync.Mutex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		*forms = append(*forms, r.PostForm)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-1", "refresh_token": "rt-1", "expires_in": 3600,
		})
	}
}

// driveRedirect extracts the loopback callback URL from the printed
// authorization URL and fetches it, which is what the browser would do.
func driveRedirect(t *testing.T, printed string, code string, tamperState bool) {
	t.Helper()
	// The printed block contains the authorization URL on its own indented
	// line; pull it back out the way a user's browser would receive it.
	var raw string
	for _, line := range strings.Split(printed, "\n") {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "http") {
			raw = s
			break
		}
	}
	if raw == "" {
		t.Fatalf("no authorization URL was printed:\n%s", printed)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("printed URL does not parse: %v", err)
	}
	redirect := u.Query().Get("redirect_uri")
	state := u.Query().Get("state")
	if tamperState {
		state = "attacker-state"
	}
	q := url.Values{"code": {code}, "state": {state}}
	resp, err := http.Get(redirect + "?" + q.Encode())
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	resp.Body.Close()
}

// pipeWriter lets the test read the printed URL while RunMCPLogin is still
// blocked waiting for the redirect.
type pipeWriter struct {
	mu   sync.Mutex
	buf  strings.Builder
	seen chan struct{}
	once sync.Once
}

func (p *pipeWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	p.buf.Write(b)
	has := strings.Contains(p.buf.String(), "waiting for the redirect")
	p.mu.Unlock()
	if has {
		p.once.Do(func() { close(p.seen) })
	}
	return len(b), nil
}

func (p *pipeWriter) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

// TestMCPLoginCompletesTheFlow drives the whole authorization_code leg: print
// the URL, receive the redirect, exchange the code with the verifier, persist.
func TestMCPLoginCompletesTheFlow(t *testing.T) {
	var forms []url.Values
	var mu sync.Mutex
	m, store, _ := loginFixture(t, tokenOK(&forms, &mu))

	out := &pipeWriter{seen: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- RunMCPLogin(context.Background(), MCPLoginOptions{
			Server: "corp", Manager: m, Out: out, Timeout: 10 * time.Second,
		})
	}()
	select {
	case <-out.seen:
	case <-time.After(5 * time.Second):
		t.Fatalf("the URL was never printed:\n%s", out.String())
	}
	driveRedirect(t, out.String(), "the-code", false)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunMCPLogin: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunMCPLogin did not return")
	}

	tok, ok, _ := store.LoadTokens("corp")
	if !ok || tok.AccessToken != "at-1" || tok.RefreshToken != "rt-1" {
		t.Fatalf("stored = %+v ok=%v", tok, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(forms) != 1 {
		t.Fatalf("token endpoint hit %d times", len(forms))
	}
	f := forms[0]
	if f.Get("grant_type") != "authorization_code" || f.Get("code") != "the-code" {
		t.Errorf("form = %v", f)
	}
	// PKCE: the verifier is sent on the token leg and is NOT the challenge that
	// went out on the authorization leg. Sending the challenge would make the
	// proof self-satisfying.
	verifier := f.Get("code_verifier")
	if verifier == "" {
		t.Fatal("no code_verifier was sent; PKCE is decorative")
	}
	if strings.Contains(out.String(), verifier) {
		t.Fatal("the verifier appeared in the authorization URL; it must never leave this process")
	}
	if !strings.Contains(out.String(), "code_challenge_method=S256") {
		t.Errorf("the authorization URL does not request S256:\n%s", out.String())
	}
}

// TestMCPLoginRejectsATamperedState is the CSRF property at the CLI level: any
// page the user visits during the flow can drive their browser to the loopback
// callback with an attacker-obtained code.
func TestMCPLoginRejectsATamperedState(t *testing.T) {
	var forms []url.Values
	var mu sync.Mutex
	m, store, _ := loginFixture(t, tokenOK(&forms, &mu))

	out := &pipeWriter{seen: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- RunMCPLogin(context.Background(), MCPLoginOptions{
			Server: "corp", Manager: m, Out: out, Timeout: 10 * time.Second,
		})
	}()
	<-out.seen
	driveRedirect(t, out.String(), "attacker-code", true)

	err := <-done
	if err == nil {
		t.Fatal("a callback with the wrong state was accepted")
	}
	if !strings.Contains(err.Error(), "state") {
		t.Errorf("err = %v, want one naming the state mismatch", err)
	}
	if _, ok, _ := store.LoadTokens("corp"); ok {
		t.Fatal("the attacker's code was exchanged and stored")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(forms) != 0 {
		t.Fatalf("the token endpoint was called %d times for a rejected callback", len(forms))
	}
}

// TestMCPLoginRefusals covers the inputs that cannot work, each naming what to
// do instead rather than failing generically.
func TestMCPLoginRefusals(t *testing.T) {
	var forms []url.Values
	var mu sync.Mutex
	m, _, _ := loginFixture(t, tokenOK(&forms, &mu))

	cases := []struct {
		name, server, want string
	}{
		{"no server", "", "server name is required"},
		{"unknown server", "nope", "no server named"},
		// A client-credentials server has no browser leg at all; offering one
		// would produce a URL nothing serves.
		{"machine-to-machine server", "m2m", "interactive login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RunMCPLogin(context.Background(), MCPLoginOptions{
				Server: tc.server, Manager: m, Out: io.Discard, Timeout: time.Second,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
	if err := RunMCPLogin(context.Background(), MCPLoginOptions{Server: "corp"}); err == nil {
		t.Error("a nil manager must be refused")
	}
}

// TestMCPLoginNamesTheCandidates: a bare invocation should say which server
// names are valid, not only that one is required.
func TestMCPLoginNamesTheCandidates(t *testing.T) {
	var forms []url.Values
	var mu sync.Mutex
	m, _, _ := loginFixture(t, tokenOK(&forms, &mu))
	err := RunMCPLogin(context.Background(), MCPLoginOptions{Manager: m, Out: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "corp") {
		t.Fatalf("err = %v; it must list the authorization_code servers", err)
	}
	if strings.Contains(err.Error(), "m2m") {
		t.Errorf("err = %v; a client-credentials server has no interactive login", err)
	}
}

// TestMCPLoginTimesOut: a user who closed the tab must not leave a process the
// operator has to hunt down and kill.
func TestMCPLoginTimesOut(t *testing.T) {
	var forms []url.Values
	var mu sync.Mutex
	m, _, _ := loginFixture(t, tokenOK(&forms, &mu))
	start := time.Now()
	err := RunMCPLogin(context.Background(), MCPLoginOptions{
		Server: "corp", Manager: m, Out: io.Discard, Timeout: 100 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the timeout took %v; it is not being honoured", elapsed)
	}
}

// TestMCPLogout deletes the stored login and refuses the shapes it cannot.
func TestMCPLogout(t *testing.T) {
	store := newMemMCPTokens()
	_ = store.SaveTokens("corp", mcp.StoredTokens{AccessToken: "a", RefreshToken: "r"})

	var out strings.Builder
	if err := RunMCPLogout("corp", store, &out); err != nil {
		t.Fatalf("RunMCPLogout: %v", err)
	}
	if _, ok, _ := store.LoadTokens("corp"); ok {
		t.Fatal("the tokens survived logout")
	}
	if !strings.Contains(out.String(), "corp") {
		t.Errorf("output %q does not name the server", out.String())
	}
	if err := RunMCPLogout("", store, io.Discard); err == nil {
		t.Error("an empty server name must be refused")
	}
	// A store with no delete must say so rather than reporting success.
	if err := RunMCPLogout("corp", readOnlyTokens{}, io.Discard); err == nil {
		t.Error("a store that cannot delete must be refused, not silently succeed")
	}
}

// readOnlyTokens satisfies TokenStore but NOT TokenDeleter, which is the shape
// the refresh path is supposed to be limited to.
type readOnlyTokens struct{}

func (readOnlyTokens) LoadTokens(string) (mcp.StoredTokens, bool, error) {
	return mcp.StoredTokens{}, false, nil
}
func (readOnlyTokens) SaveTokens(string, mcp.StoredTokens) error { return nil }
