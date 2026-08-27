package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrNoRefreshToken is returned by AuthCodeSource.Token when there is no stored
// refresh token to exchange.
//
// It is a distinct sentinel rather than a generic error because the CALLER's
// remedy is specific and unambiguous: run `yanshi auth mcp-login`. Every other
// token failure is transient or a server problem; this one means a human has to
// open a browser, and a server rendering it as "token request failed" sends the
// operator looking at the wrong thing.
var ErrNoRefreshToken = errors.New("mcp oauth: no refresh token stored; run `yanshi auth mcp-login`")

// TokenStore persists an MCP server's OAuth tokens.
//
// It is an interface so internal/mcp does not depend on internal/secrets: mcp
// is reachable from the ports layer and the credential backend is a service
// concern. The production implementation wraps secrets.Manager; tests use an
// in-memory map.
//
// Implementations MUST be safe for concurrent use: two Manager tool calls on
// the same server can hit a refresh at the same instant.
type TokenStore interface {
	// LoadTokens returns the stored tokens for server. A missing entry must
	// return ErrNoRefreshToken-compatible behaviour by returning ok=false
	// rather than an error.
	LoadTokens(server string) (tok StoredTokens, ok bool, err error)
	// SaveTokens replaces the stored tokens for server atomically.
	SaveTokens(server string, tok StoredTokens) error
}

// StoredTokens is the persisted OAuth material for one MCP server.
//
// AccessToken is stored alongside the refresh token rather than being kept
// purely in memory so a freshly-started process can make its first call without
// a refresh round trip. Both are credentials; neither ever appears in a
// ServerStatus, a log line, or the config file.
type StoredTokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at,omitzero"`
}

// AuthCodeSource is a TokenSource for the authorization_code grant: it serves
// the stored access token until it nears expiry, then exchanges the refresh
// token for a new one.
//
// It never runs the interactive leg. Opening a browser from inside a tool call
// would block a model turn on a human, and on a headless daemon it would block
// on nothing at all. The interactive leg is `yanshi auth mcp-login`, which
// writes the tokens this type reads.
type AuthCodeSource struct {
	server       string
	tokenURL     string
	clientID     string
	clientSecret string
	scopes       []string
	httpClient   *http.Client
	store        TokenStore
	now          func() time.Time

	// mu guards cached + the single-flight state below.
	mu     sync.Mutex
	cached StoredTokens
	loaded bool

	// inflight is the single-flight handle for a refresh in progress.
	//
	// Deduplication is not an optimisation here, it is a correctness
	// requirement, because most providers ROTATE the refresh token: the
	// response to a refresh contains a new refresh token and invalidates the
	// one that was sent. Two concurrent refreshes therefore both spend the same
	// token, the loser's response arrives second and overwrites the winner's
	// stored token with one the server already retired, and every later refresh
	// fails with invalid_grant — i.e. the user is silently logged out and the
	// only fix is a browser round trip.
	inflight *refreshCall
}

// refreshCall is one in-flight refresh other callers wait on.
type refreshCall struct {
	done chan struct{}
	tok  StoredTokens
	err  error
}

// AuthCodeConfig configures NewAuthCodeSource.
type AuthCodeConfig struct {
	// Server is the MCP server name; it keys the TokenStore.
	Server string
	// TokenURL is the OAuth token endpoint.
	TokenURL string
	// ClientID is required. ClientSecret is optional: a public client
	// (no secret) is legitimate under PKCE and many providers issue exactly
	// that shape for desktop applications.
	ClientID     string
	ClientSecret string
	Scopes       []string
	// Store persists the tokens. Required — without one there is nothing to
	// read and nothing to write, so the constructor refuses.
	Store TokenStore
	// HTTPClient is optional; nil gets a 30-second-timeout client.
	HTTPClient *http.Client
}

// NewAuthCodeSource constructs an authorization_code token source.
//
// It refuses a nil Store rather than degrading to memory-only. A memory-only
// source works for exactly one process lifetime and then silently starts
// failing every call with "no refresh token", which reads as a server outage.
func NewAuthCodeSource(cfg AuthCodeConfig) (*AuthCodeSource, error) {
	if strings.TrimSpace(cfg.TokenURL) == "" {
		return nil, fmt.Errorf("mcp oauth: authorization_code requires token_url")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, fmt.Errorf("mcp oauth: authorization_code requires client_id")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("mcp oauth: authorization_code requires a token store")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &AuthCodeSource{
		server: cfg.Server, tokenURL: cfg.TokenURL,
		clientID: cfg.ClientID, clientSecret: cfg.ClientSecret,
		scopes:     append([]string(nil), cfg.Scopes...),
		httpClient: hc, store: cfg.Store, now: time.Now,
	}, nil
}

// tokenRefreshSkew is how long before expiry a token is treated as stale.
//
// It matches ClientCredentialsSource's 30 seconds deliberately: the two sources
// are interchangeable behind TokenSource, and a caller that switched grant
// types should not see the refresh cadence change as a side effect.
const tokenRefreshSkew = 30 * time.Second

// Token returns a valid access token, refreshing when the cached one is absent
// or within tokenRefreshSkew of expiry.
func (s *AuthCodeSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	if err := s.ensureLoadedLocked(); err != nil {
		s.mu.Unlock()
		return "", err
	}
	if s.cached.AccessToken != "" && s.tokenIsFreshLocked() {
		token := s.cached.AccessToken
		s.mu.Unlock()
		return token, nil
	}
	if s.cached.RefreshToken == "" {
		s.mu.Unlock()
		return "", ErrNoRefreshToken
	}
	call, leader := s.joinRefreshLocked()
	s.mu.Unlock()

	if leader {
		s.runRefresh(ctx, call)
	} else {
		// A follower must still honour ITS OWN context: the leader's request
		// may outlive this caller's deadline, and blocking past it would make
		// a per-call timeout meaningless.
		select {
		case <-call.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if call.err != nil {
		return "", call.err
	}
	return call.tok.AccessToken, nil
}

// Invalidate marks the cached access token stale so the next Token call
// refreshes. The refresh token is deliberately kept: a 401 means the ACCESS
// token was rejected, and discarding the refresh token would turn a routine
// expiry into a forced browser round trip.
func (s *AuthCodeSource) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached.AccessToken = ""
	s.cached.ExpiresAt = time.Time{}
}

// tokenIsFreshLocked reports whether the cached token is good for at least
// tokenRefreshSkew more. A zero ExpiresAt means the server did not say, which
// is treated as fresh — refreshing on every call because the provider omitted
// expires_in would hammer the token endpoint.
func (s *AuthCodeSource) tokenIsFreshLocked() bool {
	if s.cached.ExpiresAt.IsZero() {
		return true
	}
	return s.now().Add(tokenRefreshSkew).Before(s.cached.ExpiresAt)
}

// ensureLoadedLocked reads the store once per process.
//
// A read failure is returned rather than swallowed: an unreadable credential
// backend (locked keyring, wrong passphrase) presenting as "not logged in"
// would send the operator through a browser flow that then also fails to save.
func (s *AuthCodeSource) ensureLoadedLocked() error {
	if s.loaded {
		return nil
	}
	tok, ok, err := s.store.LoadTokens(s.server)
	if err != nil {
		return fmt.Errorf("mcp oauth: load tokens for %q: %w", s.server, err)
	}
	s.loaded = true
	if ok {
		s.cached = tok
	}
	return nil
}

// joinRefreshLocked returns the in-flight refresh, creating one when this
// caller is first. leader reports whether this caller must perform the request.
func (s *AuthCodeSource) joinRefreshLocked() (call *refreshCall, leader bool) {
	if s.inflight != nil {
		return s.inflight, false
	}
	s.inflight = &refreshCall{done: make(chan struct{})}
	return s.inflight, true
}

// runRefresh performs one refresh, persists the rotated tokens, and releases
// every waiter.
//
// The store write happens BEFORE the waiters are released and before the cache
// is updated. If persistence fails, the whole refresh fails: serving a token
// whose rotated refresh token was never written means the old refresh token is
// already dead at the server and the new one exists nowhere, so the next
// process start is logged out with no error ever having been reported.
func (s *AuthCodeSource) runRefresh(ctx context.Context, call *refreshCall) {
	s.mu.Lock()
	refreshToken := s.cached.RefreshToken
	s.mu.Unlock()

	tok, err := s.exchangeRefresh(ctx, refreshToken)
	if err == nil {
		if saveErr := s.store.SaveTokens(s.server, tok); saveErr != nil {
			err = fmt.Errorf("mcp oauth: persist refreshed tokens for %q: %w", s.server, saveErr)
		}
	}

	s.mu.Lock()
	if err == nil {
		s.cached = tok
	}
	call.tok, call.err = tok, err
	s.inflight = nil
	s.mu.Unlock()
	close(call.done)
}

// exchangeRefresh performs the refresh_token grant.
//
// A response that omits refresh_token means the provider did NOT rotate, so the
// token that was sent stays valid and is carried forward. Writing the empty
// string back would log the user out on the next refresh — an easy mistake,
// because the happy path of a rotating provider looks identical.
func (s *AuthCodeSource) exchangeRefresh(ctx context.Context, refreshToken string) (StoredTokens, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {s.clientID},
	}
	if s.clientSecret != "" {
		form.Set("client_secret", s.clientSecret)
	}
	if len(s.scopes) > 0 {
		form.Set("scope", strings.Join(s.scopes, " "))
	}
	body, err := s.postForm(ctx, form)
	if err != nil {
		return StoredTokens{}, err
	}
	out := s.tokensFromResponse(body)
	if out.RefreshToken == "" {
		out.RefreshToken = refreshToken
	}
	return out, nil
}

// ExchangeAuthorizationCode performs the initial code-for-token exchange.
//
// It is a method on the source rather than a free function so the interactive
// CLI leg and the background refresh share one HTTP client, one response
// parser, and one definition of what a valid token response is. The redirectURI
// must be byte-identical to the one sent on the authorization request; every
// provider checks this, and a mismatch produces an opaque invalid_grant.
func (s *AuthCodeSource) ExchangeAuthorizationCode(ctx context.Context, code, redirectURI, verifier string) (StoredTokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {s.clientID},
		"code_verifier": {verifier},
	}
	if s.clientSecret != "" {
		form.Set("client_secret", s.clientSecret)
	}
	body, err := s.postForm(ctx, form)
	if err != nil {
		return StoredTokens{}, err
	}
	tok := s.tokensFromResponse(body)
	if tok.RefreshToken == "" {
		// Without one, this login is good until the access token expires and
		// then silently stops working. Saying so at login time is the whole
		// difference between "you need offline_access in your scopes" and a
		// mysterious outage an hour later.
		return tok, fmt.Errorf("mcp oauth: the provider returned no refresh_token; " +
			"request an offline-access scope or this login will expire and cannot be renewed")
	}
	return tok, nil
}

// SaveTokens persists tokens under this source's server name, so the CLI login
// leg does not need its own handle on the store.
func (s *AuthCodeSource) SaveTokens(tok StoredTokens) error {
	if err := s.store.SaveTokens(s.server, tok); err != nil {
		return err
	}
	s.mu.Lock()
	s.cached, s.loaded = tok, true
	s.mu.Unlock()
	return nil
}

// oauthTokenResponse is the RFC 6749 token endpoint payload.
type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

// tokensFromResponse maps a decoded token response onto StoredTokens.
func (s *AuthCodeSource) tokensFromResponse(body oauthTokenResponse) StoredTokens {
	out := StoredTokens{AccessToken: body.AccessToken, RefreshToken: body.RefreshToken}
	if body.ExpiresIn > 0 {
		out.ExpiresAt = s.now().Add(time.Duration(body.ExpiresIn) * time.Second)
	}
	return out
}

// postForm issues one token-endpoint request and validates the response.
//
// The error text carries the STATUS and the provider's error code, never the
// body verbatim: OAuth error responses routinely echo the request, and the
// request contains the client secret and the refresh token.
func (s *AuthCodeSource) postForm(ctx context.Context, form url.Values) (oauthTokenResponse, error) {
	var zero oauthTokenResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return zero, fmt.Errorf("mcp oauth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return zero, fmt.Errorf("mcp oauth: token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return zero, fmt.Errorf("mcp oauth: token endpoint returned %s (%s)",
			resp.Status, oauthErrorCode(resp))
	}
	var body oauthTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return zero, fmt.Errorf("mcp oauth: decode token response: %w", err)
	}
	if body.AccessToken == "" {
		return zero, fmt.Errorf("mcp oauth: token response has empty access_token")
	}
	if body.TokenType != "" && !strings.EqualFold(body.TokenType, "Bearer") {
		return zero, fmt.Errorf("mcp oauth: unsupported token_type %q", body.TokenType)
	}
	return body, nil
}

// oauthErrorCode extracts the RFC 6749 `error` field from a failed response,
// or "no error code" when the body is not the documented shape.
//
// Only the CODE is returned. The companion error_description is provider-
// authored free text that is neither bounded nor guaranteed to omit the
// credential the request carried.
func oauthErrorCode(resp *http.Response) string {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Error == "" {
		return "no error code"
	}
	return body.Error
}
