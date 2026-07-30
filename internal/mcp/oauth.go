package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TokenSource returns an Authorization bearer token. Invalidate forces the next
// call to fetch a new token (used after HTTP 401). Implementations must be safe
// for concurrent Manager tool calls.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// StaticTokenSource wraps a configured bearer token. Invalidate is a no-op.
type StaticTokenSource struct{ Value string }

// Token returns the static bearer token.
func (s *StaticTokenSource) Token(context.Context) (string, error) { return s.Value, nil }

// Invalidate is a no-op for the static token source.
func (s *StaticTokenSource) Invalidate() {}

// ClientCredentialsSource caches an OAuth 2.0 client-credentials access token.
type ClientCredentialsSource struct {
	mu           sync.Mutex
	tokenURL     string
	clientID     string
	clientSecret string
	scopes       []string
	httpClient   *http.Client
	accessToken  string
	expiresAt    time.Time
	now          func() time.Time
}

// NewClientCredentialsSource constructs a caching client-credentials source.
func NewClientCredentialsSource(tokenURL, clientID, clientSecret string, scopes []string, hc *http.Client) *ClientCredentialsSource {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &ClientCredentialsSource{
		tokenURL: tokenURL, clientID: clientID, clientSecret: clientSecret,
		scopes: append([]string(nil), scopes...), httpClient: hc, now: time.Now,
	}
}

// Token returns a valid access token, fetching a new one via client-credentials
// grant if the cached token is expired or absent.
func (s *ClientCredentialsSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.accessToken != "" && now.Add(30*time.Second).Before(s.expiresAt) {
		return s.accessToken, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {s.clientID},
		"client_secret": {s.clientSecret},
	}
	if len(s.scopes) > 0 {
		form.Set("scope", strings.Join(s.scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("mcp oauth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("mcp oauth: token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("mcp oauth: token endpoint returned %s", resp.Status)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("mcp oauth: decode token response: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("mcp oauth: token response has empty access_token")
	}
	if body.TokenType != "" && !strings.EqualFold(body.TokenType, "Bearer") {
		return "", fmt.Errorf("mcp oauth: unsupported token_type %q", body.TokenType)
	}
	lifetime := time.Duration(body.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = 5 * time.Minute
	}
	s.accessToken = body.AccessToken
	s.expiresAt = now.Add(lifetime)
	return s.accessToken, nil
}

// Invalidate clears the cached token so the next Token() call fetches a fresh one.
func (s *ClientCredentialsSource) Invalidate() {
	s.mu.Lock()
	s.accessToken = ""
	s.expiresAt = time.Time{}
	s.mu.Unlock()
}
