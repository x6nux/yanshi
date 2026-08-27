package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// memTokenStore is an in-memory TokenStore that counts writes, so a test can
// tell "refreshed once" from "refreshed twice".
type memTokenStore struct {
	mu     sync.Mutex
	data   map[string]StoredTokens
	writes int
	loadEr error
	saveEr error
}

func newMemTokenStore() *memTokenStore {
	return &memTokenStore{data: map[string]StoredTokens{}}
}

func (m *memTokenStore) LoadTokens(server string) (StoredTokens, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadEr != nil {
		return StoredTokens{}, false, m.loadEr
	}
	tok, ok := m.data[server]
	return tok, ok, nil
}

func (m *memTokenStore) SaveTokens(server string, tok StoredTokens) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveEr != nil {
		return m.saveEr
	}
	m.writes++
	m.data[server] = tok
	return nil
}

func (m *memTokenStore) snapshot(server string) (StoredTokens, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[server], m.writes
}

// tokenServer is a scripted OAuth token endpoint. Each call pops the next
// scripted response; it records every form it received.
type tokenServer struct {
	mu        sync.Mutex
	responses []map[string]any
	statuses  []int
	forms     []url.Values
	delay     time.Duration
	calls     int
}

func (ts *tokenServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ts.mu.Lock()
		ts.forms = append(ts.forms, r.PostForm)
		i := ts.calls
		ts.calls++
		delay := ts.delay
		var body map[string]any
		status := http.StatusOK
		if i < len(ts.responses) {
			body = ts.responses[i]
		}
		if i < len(ts.statuses) {
			status = ts.statuses[i]
		}
		ts.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

func (ts *tokenServer) callCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.calls
}

func (ts *tokenServer) form(i int) url.Values {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if i >= len(ts.forms) {
		return nil
	}
	return ts.forms[i]
}

// newAuthCodeFixture wires a scripted endpoint to a source over a store.
func newAuthCodeFixture(t *testing.T, ts *tokenServer, store TokenStore) *AuthCodeSource {
	t.Helper()
	srv := httptest.NewServer(ts.handler())
	t.Cleanup(srv.Close)
	src, err := NewAuthCodeSource(AuthCodeConfig{
		Server: "corp", TokenURL: srv.URL, ClientID: "cid", ClientSecret: "csec",
		Scopes: []string{"read", "offline_access"}, Store: store,
	})
	if err != nil {
		t.Fatalf("NewAuthCodeSource: %v", err)
	}
	return src
}

// TestNewAuthCodeSourceRefusesIncompleteConfig: every one of these produces a
// source that cannot ever succeed, and failing at construction is the
// difference between a boot-time message and a mystery 401 on the first tool
// call.
func TestNewAuthCodeSourceRefusesIncompleteConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  AuthCodeConfig
		want string
	}{
		{"no token url", AuthCodeConfig{ClientID: "c", Store: newMemTokenStore()}, "token_url"},
		{"no client id", AuthCodeConfig{TokenURL: "https://x", Store: newMemTokenStore()}, "client_id"},
		{"no store", AuthCodeConfig{TokenURL: "https://x", ClientID: "c"}, "token store"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAuthCodeSource(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// TestAuthCodeNoStoredTokenIsADistinctSentinel: the remedy for "never logged
// in" is a browser flow and the remedy for a network failure is waiting. A
// caller that cannot tell them apart tells the operator the wrong thing.
func TestAuthCodeNoStoredTokenIsADistinctSentinel(t *testing.T) {
	src := newAuthCodeFixture(t, &tokenServer{}, newMemTokenStore())
	_, err := src.Token(context.Background())
	if !errors.Is(err, ErrNoRefreshToken) {
		t.Fatalf("err = %v, want ErrNoRefreshToken", err)
	}
}

// TestAuthCodeServesTheCachedTokenWithoutRefreshing: a valid token must not
// cost a token-endpoint round trip per tool call.
func TestAuthCodeServesTheCachedTokenWithoutRefreshing(t *testing.T) {
	store := newMemTokenStore()
	store.data["corp"] = StoredTokens{
		AccessToken: "live", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour),
	}
	ts := &tokenServer{}
	src := newAuthCodeFixture(t, ts, store)

	for i := 0; i < 3; i++ {
		got, err := src.Token(context.Background())
		if err != nil || got != "live" {
			t.Fatalf("Token = %q, %v", got, err)
		}
	}
	if ts.callCount() != 0 {
		t.Errorf("hit the token endpoint %d times for a valid token", ts.callCount())
	}
}

// TestAuthCodeRefreshRotation is the central correctness property: a provider
// that rotates the refresh token invalidates the old one, so the NEW one must
// be persisted or every later refresh fails with invalid_grant.
func TestAuthCodeRefreshRotation(t *testing.T) {
	store := newMemTokenStore()
	store.data["corp"] = StoredTokens{
		AccessToken: "stale", RefreshToken: "r1", ExpiresAt: time.Now().Add(-time.Minute),
	}
	ts := &tokenServer{responses: []map[string]any{
		{"access_token": "a2", "refresh_token": "r2", "expires_in": 3600, "token_type": "Bearer"},
	}}
	src := newAuthCodeFixture(t, ts, store)

	got, err := src.Token(context.Background())
	if err != nil || got != "a2" {
		t.Fatalf("Token = %q, %v", got, err)
	}
	saved, writes := store.snapshot("corp")
	if saved.RefreshToken != "r2" {
		t.Errorf("stored refresh token = %q, want the ROTATED r2; the old one is dead at the server", saved.RefreshToken)
	}
	if writes != 1 {
		t.Errorf("store writes = %d, want 1", writes)
	}
	if sent := ts.form(0).Get("refresh_token"); sent != "r1" {
		t.Errorf("sent refresh_token %q, want r1", sent)
	}
	if grant := ts.form(0).Get("grant_type"); grant != "refresh_token" {
		t.Errorf("grant_type = %q", grant)
	}
}

// TestAuthCodeNonRotatingProviderKeepsItsToken: a response that omits
// refresh_token means the provider did NOT rotate. Writing "" back logs the
// user out at the next refresh, and the happy path of a rotating provider looks
// identical, so this is easy to get wrong and invisible when you do.
func TestAuthCodeNonRotatingProviderKeepsItsToken(t *testing.T) {
	store := newMemTokenStore()
	store.data["corp"] = StoredTokens{
		AccessToken: "stale", RefreshToken: "keepme", ExpiresAt: time.Now().Add(-time.Minute),
	}
	ts := &tokenServer{responses: []map[string]any{
		{"access_token": "a2", "expires_in": 3600},
	}}
	src := newAuthCodeFixture(t, ts, store)

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	saved, _ := store.snapshot("corp")
	if saved.RefreshToken != "keepme" {
		t.Fatalf("stored refresh token = %q; a non-rotating response erased it", saved.RefreshToken)
	}
}

// TestAuthCodeConcurrentRefreshIsDeduplicated: with rotation, two simultaneous
// refreshes both spend the same token; the loser's response then overwrites the
// winner's stored token with one the server already retired.
func TestAuthCodeConcurrentRefreshIsDeduplicated(t *testing.T) {
	store := newMemTokenStore()
	store.data["corp"] = StoredTokens{
		AccessToken: "stale", RefreshToken: "r1", ExpiresAt: time.Now().Add(-time.Minute),
	}
	ts := &tokenServer{
		delay: 40 * time.Millisecond,
		responses: []map[string]any{
			{"access_token": "a2", "refresh_token": "r2", "expires_in": 3600},
			{"access_token": "SHOULD-NOT-HAPPEN", "refresh_token": "r3", "expires_in": 3600},
		},
	}
	src := newAuthCodeFixture(t, ts, store)

	const n = 8
	var wg sync.WaitGroup
	got := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = src.Token(context.Background())
		}(i)
	}
	wg.Wait()

	for i := range got {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if got[i] != "a2" {
			t.Errorf("caller %d got %q, want a2", i, got[i])
		}
	}
	if calls := ts.callCount(); calls != 1 {
		t.Errorf("token endpoint hit %d times; concurrent refreshes were not deduplicated "+
			"and each one spent the same rotating refresh token", calls)
	}
	saved, writes := store.snapshot("corp")
	if saved.RefreshToken != "r2" || writes != 1 {
		t.Errorf("stored = %+v after %d writes, want r2 after 1", saved, writes)
	}
}

// TestAuthCodePersistFailureFailsTheRefresh: if the rotated token could not be
// written, the old one is already dead at the server and the new one exists
// nowhere. Serving the access token anyway hides a login that is already lost.
func TestAuthCodePersistFailureFailsTheRefresh(t *testing.T) {
	store := newMemTokenStore()
	store.data["corp"] = StoredTokens{
		AccessToken: "stale", RefreshToken: "r1", ExpiresAt: time.Now().Add(-time.Minute),
	}
	store.saveEr = errors.New("keyring locked")
	ts := &tokenServer{responses: []map[string]any{
		{"access_token": "a2", "refresh_token": "r2", "expires_in": 3600},
	}}
	src := newAuthCodeFixture(t, ts, store)

	if _, err := src.Token(context.Background()); err == nil {
		t.Fatal("a refresh whose rotated token could not be persisted must fail")
	}
}

// TestAuthCodeLoadFailurePropagates: an unreadable credential backend must not
// present as "not logged in", or the operator runs a browser flow that also
// cannot save.
func TestAuthCodeLoadFailurePropagates(t *testing.T) {
	store := newMemTokenStore()
	store.loadEr = errors.New("passphrase rejected")
	src := newAuthCodeFixture(t, &tokenServer{}, store)
	_, err := src.Token(context.Background())
	if err == nil || errors.Is(err, ErrNoRefreshToken) {
		t.Fatalf("err = %v; a backend failure was reported as 'not logged in'", err)
	}
}

// TestAuthCodeInvalidateKeepsTheRefreshToken: a 401 rejects the ACCESS token.
// Discarding the refresh token there turns a routine expiry into a forced
// browser round trip.
func TestAuthCodeInvalidateKeepsTheRefreshToken(t *testing.T) {
	store := newMemTokenStore()
	store.data["corp"] = StoredTokens{
		AccessToken: "a1", RefreshToken: "r1", ExpiresAt: time.Now().Add(time.Hour),
	}
	ts := &tokenServer{responses: []map[string]any{
		{"access_token": "a2", "refresh_token": "r2", "expires_in": 3600},
	}}
	src := newAuthCodeFixture(t, ts, store)
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	src.Invalidate()
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("after Invalidate the source must refresh, not fail: %v", err)
	}
	if got != "a2" {
		t.Errorf("Token = %q, want the refreshed a2", got)
	}
}

// TestAuthCodeSkewRefreshesBeforeExpiry: a token that expires in five seconds
// is not usable for a call that takes ten.
func TestAuthCodeSkewRefreshesBeforeExpiry(t *testing.T) {
	store := newMemTokenStore()
	store.data["corp"] = StoredTokens{
		AccessToken: "almost-dead", RefreshToken: "r1",
		ExpiresAt: time.Now().Add(tokenRefreshSkew / 2),
	}
	ts := &tokenServer{responses: []map[string]any{
		{"access_token": "fresh", "refresh_token": "r2", "expires_in": 3600},
	}}
	src := newAuthCodeFixture(t, ts, store)
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "fresh" {
		t.Errorf("Token = %q; a token inside the skew window was served as valid", got)
	}
}

// TestAuthCodeNoExpiryIsTreatedAsFresh: a provider that omits expires_in must
// not cause a refresh on every single call.
func TestAuthCodeNoExpiryIsTreatedAsFresh(t *testing.T) {
	store := newMemTokenStore()
	store.data["corp"] = StoredTokens{AccessToken: "forever", RefreshToken: "r1"}
	ts := &tokenServer{}
	src := newAuthCodeFixture(t, ts, store)
	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if ts.callCount() != 0 {
		t.Errorf("refreshed %d times for a token with no stated expiry", ts.callCount())
	}
}

// TestAuthCodeErrorResponseDoesNotEchoTheRequest: OAuth error bodies routinely
// echo the request, which carries the client secret and the refresh token.
func TestAuthCodeErrorResponseDoesNotEchoTheRequest(t *testing.T) {
	store := newMemTokenStore()
	store.data["corp"] = StoredTokens{AccessToken: "x", RefreshToken: "SUPERSECRET",
		ExpiresAt: time.Now().Add(-time.Minute)}
	ts := &tokenServer{
		statuses: []int{http.StatusBadRequest},
		responses: []map[string]any{{
			"error":             "invalid_grant",
			"error_description": "refresh_token SUPERSECRET client_secret csec was rejected",
		}},
	}
	src := newAuthCodeFixture(t, ts, store)
	_, err := src.Token(context.Background())
	if err == nil {
		t.Fatal("a 400 must fail")
	}
	if strings.Contains(err.Error(), "SUPERSECRET") || strings.Contains(err.Error(), "csec") {
		t.Fatalf("error text leaked a credential: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error text %q drops the provider's error code, which is the actionable half", err)
	}
}

// TestExchangeAuthorizationCodeSendsPKCEAndRedirect: both are compared
// literally by every provider, and both produce the same opaque invalid_grant
// when wrong.
func TestExchangeAuthorizationCodeSendsPKCEAndRedirect(t *testing.T) {
	ts := &tokenServer{responses: []map[string]any{
		{"access_token": "a1", "refresh_token": "r1", "expires_in": 3600},
	}}
	src := newAuthCodeFixture(t, ts, newMemTokenStore())
	tok, err := src.ExchangeAuthorizationCode(context.Background(),
		"the-code", "http://127.0.0.1:5555/callback", "the-verifier")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok.AccessToken != "a1" || tok.RefreshToken != "r1" {
		t.Errorf("tokens = %+v", tok)
	}
	form := ts.form(0)
	for k, want := range map[string]string{
		"grant_type":    "authorization_code",
		"code":          "the-code",
		"redirect_uri":  "http://127.0.0.1:5555/callback",
		"code_verifier": "the-verifier",
		"client_id":     "cid",
	} {
		if form.Get(k) != want {
			t.Errorf("form[%s] = %q, want %q", k, form.Get(k), want)
		}
	}
}

// TestExchangeWithoutRefreshTokenIsReportedAtLoginTime: a login with no refresh
// token works for an hour and then silently stops. Saying so now is the
// difference between "add offline_access" and a mysterious outage later.
func TestExchangeWithoutRefreshTokenIsReportedAtLoginTime(t *testing.T) {
	ts := &tokenServer{responses: []map[string]any{
		{"access_token": "a1", "expires_in": 3600},
	}}
	src := newAuthCodeFixture(t, ts, newMemTokenStore())
	_, err := src.ExchangeAuthorizationCode(context.Background(), "c", "http://x/callback", "v")
	if err == nil || !strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("err = %v, want one naming the missing refresh_token", err)
	}
}

// TestPKCEIsS256AndFreshEachTime. The "plain" method offers zero protection
// against the exact attacker PKCE exists for, and a reused verifier defeats the
// per-request binding.
func TestPKCEIsS256AndFreshEachTime(t *testing.T) {
	a, err := NewPKCE()
	if err != nil {
		t.Fatalf("NewPKCE: %v", err)
	}
	b, _ := NewPKCE()
	if a.Verifier == b.Verifier {
		t.Fatal("two PKCE pairs shared a verifier")
	}
	if a.Challenge == a.Verifier {
		t.Fatal("challenge equals verifier — this is the 'plain' method, which protects nothing")
	}
	// RFC 7636 §4.1: 43-128 characters.
	if len(a.Verifier) < 43 || len(a.Verifier) > 128 {
		t.Errorf("verifier length %d is outside RFC 7636's 43..128", len(a.Verifier))
	}
	for _, s := range []string{a.Verifier, a.Challenge} {
		if strings.ContainsAny(s, "+/=") {
			t.Errorf("%q is standard base64, not the base64url the spec requires", s)
		}
	}
}

// TestNewOAuthStateIsUnguessableAndFresh: a fixed state parameter is the same
// as no state at all.
func TestNewOAuthStateIsUnguessableAndFresh(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s, err := NewOAuthState()
		if err != nil {
			t.Fatalf("NewOAuthState: %v", err)
		}
		if seen[s] {
			t.Fatalf("state %q repeated", s)
		}
		if len(s) < 16 {
			t.Fatalf("state %q is too short to be unguessable", s)
		}
		seen[s] = true
	}
}

// TestNormalizeGrant pins that the empty string keeps its historical meaning
// and that a typo is REFUSED rather than silently selecting the wrong flow.
func TestNormalizeGrant(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", GrantClientCredentials, true},
		{"  ", GrantClientCredentials, true},
		{"client_credentials", GrantClientCredentials, true},
		{"authorization_code", GrantAuthorizationCode, true},
		{"AUTHORIZATION_CODE", GrantAuthorizationCode, true},
		{"authorisation_code", "", false},
		{"password", "", false},
		{"implicit", "", false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			got, ok := NormalizeGrant(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("NormalizeGrant(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestBuildTokenSourceSelectsByGrant covers every branch of the config→source
// mapping, including the two soft-degrade paths. Each degradation must land on
// the configured bearer rather than on an unauthenticated client.
func TestBuildTokenSourceSelectsByGrant(t *testing.T) {
	oauth := func(grant string) *OAuthConfig {
		return &OAuthConfig{TokenURL: "https://t", ClientID: "c", Grant: grant}
	}
	cases := []struct {
		name  string
		cfg   *ServerConfig
		store TokenStore
		want  string // "" = nil source
	}{
		{"no oauth", &ServerConfig{Name: "s"}, newMemTokenStore(), ""},
		{"implicit client credentials", &ServerConfig{Name: "s", OAuth: oauth("")},
			newMemTokenStore(), "*mcp.ClientCredentialsSource"},
		{"explicit client credentials", &ServerConfig{Name: "s", OAuth: oauth("client_credentials")},
			newMemTokenStore(), "*mcp.ClientCredentialsSource"},
		{"authorization code", &ServerConfig{Name: "s", OAuth: oauth("authorization_code")},
			newMemTokenStore(), "*mcp.AuthCodeSource"},
		{"authorization code without a store degrades", &ServerConfig{Name: "s", OAuth: oauth("authorization_code")},
			nil, ""},
		{"unknown grant degrades", &ServerConfig{Name: "s", OAuth: oauth("nonsense")},
			newMemTokenStore(), ""},
		{"authorization code without a client id degrades",
			&ServerConfig{Name: "s", OAuth: &OAuthConfig{TokenURL: "https://t", Grant: "authorization_code"}},
			newMemTokenStore(), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := buildTokenSource(tc.cfg, tc.store)
			got := ""
			if src != nil {
				got = fmt.Sprintf("%T", src)
			}
			if got != tc.want {
				t.Fatalf("buildTokenSource = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNewHTTPClientForDegradesToBearer proves the degradation is not silent
// LOSS of auth: a misconfigured authorization_code server still sends whatever
// bearer the operator configured, rather than nothing.
func TestNewHTTPClientForDegradesToBearer(t *testing.T) {
	c := newHTTPClientFor(&ServerConfig{
		Name: "s", URL: "https://x", Bearer: "fallback",
		OAuth: &OAuthConfig{TokenURL: "https://t", ClientID: "c", Grant: "authorization_code"},
	}, nil)
	tok, err := c.tokens.Token(context.Background())
	if err != nil || tok != "fallback" {
		t.Fatalf("token = %q, %v; want the configured bearer", tok, err)
	}
}

// TestManagerAuthCodeAccessors covers the three CLI-facing lookups, including
// every refusal — each one is a message an operator reads instead of a silent
// no-op.
func TestManagerAuthCodeAccessors(t *testing.T) {
	store := newMemTokenStore()
	m := NewManager(map[string]*ServerConfig{
		"corp": {Enabled: true, Transport: TransportHTTP, URL: "https://c", OAuth: &OAuthConfig{
			Grant: "authorization_code", TokenURL: "https://t", AuthorizationURL: "https://a",
			ClientID: "cid", Scopes: []string{"read"},
		}},
		"m2m": {Enabled: true, Transport: TransportHTTP, URL: "https://m", OAuth: &OAuthConfig{
			Grant: "client_credentials", TokenURL: "https://t", ClientID: "cid",
		}},
		"plain": {Enabled: true, Transport: TransportHTTP, URL: "https://p"},
		"noauthz": {Enabled: true, Transport: TransportHTTP, URL: "https://n", OAuth: &OAuthConfig{
			Grant: "authorization_code", TokenURL: "https://t", ClientID: "cid",
		}},
	})
	m.SetTokenStore(store)

	got := m.AuthCodeServers()
	if len(got) != 2 || got[0] != "corp" || got[1] != "noauthz" {
		t.Errorf("AuthCodeServers = %v, want [corp noauthz] sorted", got)
	}
	if _, err := m.TokenStoreFor("corp"); err != nil {
		t.Errorf("TokenStoreFor(corp): %v", err)
	}
	for _, tc := range []struct{ server, want string }{
		{"nope", "no server named"},
		{"plain", "no oauth block"},
		{"m2m", "interactive login"},
	} {
		if _, err := m.TokenStoreFor(tc.server); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("TokenStoreFor(%s) err = %v, want one naming %q", tc.server, err, tc.want)
		}
	}

	authURL, clientID, scopes, err := m.OAuthEndpoints("corp")
	if err != nil || authURL != "https://a" || clientID != "cid" || len(scopes) != 1 {
		t.Errorf("OAuthEndpoints = %q %q %v %v", authURL, clientID, scopes, err)
	}
	if _, _, _, err := m.OAuthEndpoints("noauthz"); err == nil ||
		!strings.Contains(err.Error(), "authorization_url") {
		t.Errorf("a server with no authorization_url must say so, got %v", err)
	}

	// No store bound: the CLI must be told to configure secrets rather than
	// completing a login whose tokens go nowhere.
	m2 := NewManager(map[string]*ServerConfig{"corp": {OAuth: &OAuthConfig{
		Grant: "authorization_code", TokenURL: "https://t", ClientID: "cid"}}})
	if _, err := m2.TokenStoreFor("corp"); err == nil || !strings.Contains(err.Error(), "credential store") {
		t.Errorf("err = %v, want one naming the missing credential store", err)
	}
}
