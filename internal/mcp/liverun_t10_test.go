package mcp

// liverun_t10_test.go — the authorization_code grant against a real OAuth
// server, over a real socket.
//
// The existing tests here stub the HTTP layer, which is the right way to check
// that a particular response is parsed. What they cannot check is the thing an
// operator depends on: that the requests this client puts on the wire are ones
// a real authorization server accepts, and that refresh-token ROTATION — the
// behaviour of essentially every provider — leaves the client able to make the
// call after next.
//
// The stub here is a server, not a transport stub: it validates what it
// receives (PKCE verifier against the challenge, the grant type, the client id,
// the redirect URI, single-use codes) and rejects anything wrong the way a real
// provider would. That inversion is the point — a client bug now shows up as an
// error from the server rather than as an assertion nobody wrote.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// oauthStub is a minimal but strict authorization server.
//
// It enforces the parts of RFC 6749 + RFC 7636 that a client can get wrong
// silently: a code is single-use, the PKCE verifier must hash to the challenge
// recorded at authorization time, and a refresh token is retired the moment it
// is spent (rotation).
type oauthStub struct {
	mu sync.Mutex
	// codes maps an outstanding authorization code to its PKCE challenge and
	// redirect URI.
	codes map[string]authCodeRecord
	// liveRefresh is the only refresh token the server will currently accept.
	liveRefresh string
	// retired records every refresh token that has been spent, so a replay is
	// answered the way a real provider answers it: invalid_grant.
	retired map[string]bool
	// issued counts access tokens handed out, which is how a test tells a
	// cached token from a fresh one.
	issued int
	// tokenTTL is how long issued access tokens last.
	tokenTTL time.Duration
	// refreshCalls counts token requests with grant_type=refresh_token.
	refreshCalls int
}

// authCodeRecord is one outstanding authorization code.
type authCodeRecord struct {
	challenge   string
	redirectURI string
}

// newOAuthStub starts the server and returns it with its base URL.
func newOAuthStub(t *testing.T, tokenTTL time.Duration) (*oauthStub, string) {
	t.Helper()
	s := &oauthStub{
		codes:    map[string]authCodeRecord{},
		retired:  map[string]bool{},
		tokenTTL: tokenTTL,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", s.handleToken)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, srv.URL
}

// authorize records a code as if the user had just approved the consent screen,
// and returns it.
func (s *oauthStub) authorize(code, challenge, redirectURI string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[code] = authCodeRecord{challenge: challenge, redirectURI: redirectURI}
}

// handleToken implements the token endpoint for both grants.
func (s *oauthStub) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if r.PostForm.Get("client_id") != testClientID {
		s.fail(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.handleAuthorizationCode(w, r.PostForm)
	case "refresh_token":
		s.handleRefresh(w, r.PostForm)
	default:
		s.fail(w, http.StatusBadRequest, "unsupported_grant_type")
	}
}

// handleAuthorizationCode redeems a code, enforcing PKCE and single use.
func (s *oauthStub) handleAuthorizationCode(w http.ResponseWriter, form url.Values) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.codes[form.Get("code")]
	if !ok {
		// Covers both "never issued" and "already redeemed": a real server
		// cannot tell them apart either, and single use is enforced by the
		// delete below.
		s.failLocked(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	delete(s.codes, form.Get("code"))
	if rec.redirectURI != form.Get("redirect_uri") {
		s.failLocked(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	verifier := form.Get("code_verifier")
	if verifier == "" {
		s.failLocked(w, http.StatusBadRequest, "invalid_request")
		return
	}
	sum := sha256.Sum256([]byte(verifier))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != rec.challenge {
		s.failLocked(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	s.issueLocked(w)
}

// handleRefresh spends a refresh token and rotates it.
func (s *oauthStub) handleRefresh(w http.ResponseWriter, form url.Values) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCalls++
	sent := form.Get("refresh_token")
	if sent == "" || sent != s.liveRefresh {
		// A retired token is the interesting case: it means the client kept
		// the one it already spent.
		s.failLocked(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	s.retired[sent] = true
	s.issueLocked(w)
}

// issueLocked writes a token response and rotates the refresh token. Caller
// holds mu.
func (s *oauthStub) issueLocked(w http.ResponseWriter) {
	s.issued++
	s.liveRefresh = fmt.Sprintf("refresh-%d", s.issued)
	body := map[string]any{
		"access_token":  fmt.Sprintf("access-%d", s.issued),
		"refresh_token": s.liveRefresh,
		"token_type":    "Bearer",
		"expires_in":    int(s.tokenTTL.Seconds()),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// fail writes an OAuth error response.
func (s *oauthStub) fail(w http.ResponseWriter, status int, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failLocked(w, status, code)
}

// failLocked is fail for callers already holding mu.
func (s *oauthStub) failLocked(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// stats returns the counters under the lock.
func (s *oauthStub) stats() (issued, refreshes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued, s.refreshCalls
}

const testClientID = "yanshi-test-client"

// The TokenStore used below is authcode_test.go's memTokenStore, reused rather
// than re-declared: it already records write counts, which is exactly what
// distinguishes "the client refreshed" from "the client refreshed and persisted
// the rotated token".

// TestLiveRun_T10AuthorizationCodeWithPKCEThenRefreshRotation walks the whole
// grant against the strict stub: redeem a code with PKCE, serve the token from
// cache, let it expire, refresh, and confirm the rotated token was persisted.
//
// The stub rejects a wrong verifier and a replayed refresh token, so the
// assertions below are mostly about the client having done the right thing —
// a client that sent no verifier, or that kept spending the old refresh token,
// fails with a server error rather than a silent pass.
func TestLiveRun_T10AuthorizationCodeWithPKCEThenRefreshRotation(t *testing.T) {
	// A short TTL so the refresh leg is reached by waiting rather than by
	// reaching into the source's internals.
	stub, base := newOAuthStub(t, 31*time.Second) // just past tokenRefreshSkew
	store := newMemTokenStore()

	src, err := NewAuthCodeSource(AuthCodeConfig{
		Server:   "acme",
		TokenURL: base + "/token",
		ClientID: testClientID,
		Scopes:   []string{"mcp.read"},
		Store:    store,
	})
	require.NoError(t, err)

	// --- interactive leg: PKCE pair, consent, code redemption.
	pkce, err := NewPKCE()
	require.NoError(t, err)
	require.NotEmpty(t, pkce.Verifier)
	require.NotEqual(t, pkce.Verifier, pkce.Challenge,
		"the challenge must be a hash of the verifier, not the verifier itself")
	sum := sha256.Sum256([]byte(pkce.Verifier))
	require.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), pkce.Challenge,
		"S256 challenge must be the base64url sha256 of the verifier")

	const redirect = "http://127.0.0.1:1234/callback"
	stub.authorize("the-auth-code", pkce.Challenge, redirect)

	tok, err := src.ExchangeAuthorizationCode(context.Background(),
		"the-auth-code", redirect, pkce.Verifier)
	require.NoError(t, err, "the server rejected the code exchange")
	t.Logf("exchanged code -> access=%q refresh=%q expires=%s",
		tok.AccessToken, tok.RefreshToken, tok.ExpiresAt.Format(time.RFC3339))
	require.Equal(t, "access-1", tok.AccessToken)
	require.NoError(t, src.SaveTokens(tok))

	// --- the token is served from cache; no second network round trip.
	got, err := src.Token(context.Background())
	require.NoError(t, err)
	require.Equal(t, "access-1", got)
	issued, refreshes := stub.stats()
	t.Logf("after first Token(): issued=%d refreshes=%d", issued, refreshes)
	if refreshes != 0 {
		t.Errorf("a valid cached token triggered %d refresh(es); every call would cost a round trip",
			refreshes)
	}

	// --- expire it and refresh. Rewriting the stored expiry is how a caller's
	// clock reaching the deadline looks from the outside; it does not reach
	// into the source.
	expired := tok
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	require.NoError(t, store.SaveTokens("acme", expired))
	src.Invalidate()

	got, err = src.Token(context.Background())
	require.NoError(t, err, "the refresh leg failed")
	t.Logf("after expiry, Token() -> %q", got)
	if got == "access-1" {
		t.Errorf("an expired token was served from cache")
	}

	// --- ROTATION: the persisted refresh token must be the NEW one. Keeping
	// the spent one is the failure that logs a user out silently on the call
	// after next, and it is invisible until then.
	persisted, writes := store.snapshot("acme")
	t.Logf("persisted after refresh: access=%q refresh=%q (store writes=%d)",
		persisted.AccessToken, persisted.RefreshToken, writes)
	if persisted.RefreshToken == tok.RefreshToken {
		t.Fatalf("the client stored the refresh token it just spent (%q); the server has "+
			"already retired it, so the next refresh fails with invalid_grant and the "+
			"user is logged out with no way to tell why", persisted.RefreshToken)
	}
	if persisted.AccessToken != got {
		t.Errorf("the access token returned (%q) is not the one persisted (%q)",
			got, persisted.AccessToken)
	}

	// --- and the rotated token really works: expire again and refresh again.
	again := persisted
	again.ExpiresAt = time.Now().Add(-time.Minute)
	require.NoError(t, store.SaveTokens("acme", again))
	src.Invalidate()
	third, err := src.Token(context.Background())
	require.NoError(t, err, "the second refresh failed, so rotation was not handled after all")
	t.Logf("second refresh -> %q", third)
	if third == got {
		t.Errorf("the second refresh returned the same access token %q", third)
	}
}

// TestLiveRun_T10WrongPKCEVerifierIsRejectedByTheServer is the negative control
// for the flow above: it proves the stub actually checks the proof key, so the
// passing test is evidence that the client sent a correct one rather than that
// the server accepts anything.
func TestLiveRun_T10WrongPKCEVerifierIsRejectedByTheServer(t *testing.T) {
	stub, base := newOAuthStub(t, time.Hour)
	src, err := NewAuthCodeSource(AuthCodeConfig{
		Server: "acme", TokenURL: base + "/token",
		ClientID: testClientID, Store: newMemTokenStore(),
	})
	require.NoError(t, err)

	pkce, err := NewPKCE()
	require.NoError(t, err)
	const redirect = "http://127.0.0.1:1234/callback"
	stub.authorize("code-A", pkce.Challenge, redirect)

	other, err := NewPKCE()
	require.NoError(t, err)
	_, err = src.ExchangeAuthorizationCode(context.Background(), "code-A", redirect, other.Verifier)
	t.Logf("exchange with a mismatched verifier -> %v", err)
	if err == nil {
		t.Fatalf("the server accepted a PKCE verifier that does not match the challenge; " +
			"the positive test proves nothing")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("the error does not carry the server's OAuth error code: %v", err)
	}
}

// TestLiveRun_T10ReplayedAuthorizationCodeIsRefused pins single use, which is
// what makes an intercepted redirect useless after the client has redeemed it.
func TestLiveRun_T10ReplayedAuthorizationCodeIsRefused(t *testing.T) {
	stub, base := newOAuthStub(t, time.Hour)
	store := newMemTokenStore()
	src, err := NewAuthCodeSource(AuthCodeConfig{
		Server: "acme", TokenURL: base + "/token",
		ClientID: testClientID, Store: store,
	})
	require.NoError(t, err)

	pkce, err := NewPKCE()
	require.NoError(t, err)
	const redirect = "http://127.0.0.1:1234/callback"
	stub.authorize("code-once", pkce.Challenge, redirect)

	_, err = src.ExchangeAuthorizationCode(context.Background(), "code-once", redirect, pkce.Verifier)
	require.NoError(t, err)

	_, err = src.ExchangeAuthorizationCode(context.Background(), "code-once", redirect, pkce.Verifier)
	t.Logf("replaying the same code -> %v", err)
	if err == nil {
		t.Errorf("an authorization code was redeemable twice")
	}
}

// TestLiveRun_T10ConcurrentRefreshSpendsTheTokenOnce is the single-flight
// requirement, exercised against a server that RETIRES a spent refresh token.
//
// Without deduplication both callers spend the same token, one response
// overwrites the other with a token the server has already retired, and the
// next refresh fails. That is a real logout, and it only ever happens under
// concurrency — so it cannot be found by a sequential test.
func TestLiveRun_T10ConcurrentRefreshSpendsTheTokenOnce(t *testing.T) {
	stub, base := newOAuthStub(t, time.Hour)
	store := newMemTokenStore()
	src, err := NewAuthCodeSource(AuthCodeConfig{
		Server: "acme", TokenURL: base + "/token",
		ClientID: testClientID, Store: store,
	})
	require.NoError(t, err)

	pkce, err := NewPKCE()
	require.NoError(t, err)
	const redirect = "http://127.0.0.1:1234/callback"
	stub.authorize("code-1", pkce.Challenge, redirect)
	tok, err := src.ExchangeAuthorizationCode(context.Background(), "code-1", redirect, pkce.Verifier)
	require.NoError(t, err)
	tok.ExpiresAt = time.Now().Add(-time.Minute)
	require.NoError(t, store.SaveTokens("acme", tok))
	src.Invalidate()

	const callers = 8
	var wg sync.WaitGroup
	results := make([]string, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = src.Token(context.Background())
		}(i)
	}
	wg.Wait()

	_, refreshes := stub.stats()
	t.Logf("%d concurrent Token() calls caused %d refresh request(s)", callers, refreshes)
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d failed: %v", i, err)
		}
	}
	if refreshes > 1 {
		t.Errorf("%d concurrent callers spent the refresh token %d times; with rotation, "+
			"every spend after the first is answered invalid_grant and the loser's response "+
			"overwrites the winner's stored token", callers, refreshes)
	}
	for i := 1; i < callers; i++ {
		if results[i] != results[0] {
			t.Errorf("callers disagree about the current token: %q vs %q", results[0], results[i])
		}
	}

	// The stored token must still work: expire and refresh once more.
	final, _ := store.snapshot("acme")
	final.ExpiresAt = time.Now().Add(-time.Minute)
	require.NoError(t, store.SaveTokens("acme", final))
	src.Invalidate()
	if _, err := src.Token(context.Background()); err != nil {
		t.Errorf("after the concurrent burst the stored refresh token no longer works: %v", err)
	}
}
