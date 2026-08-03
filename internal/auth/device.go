package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/x6nux/yanshi/internal/secrets"
)

// Clock abstracts time.Now so tests can drive device-flow expiry
// deterministically. Production uses realClock{}.
type Clock interface {
	Now() time.Time
}

// Sleeper abstracts an interruptible sleep so context cancellation wakes the
// poll loop immediately. Tests also use it to advance fakeClock deterministically.
type Sleeper interface {
	Sleep(ctx context.Context, d time.Duration) error
}

// StatusUpdate is delivered to the progress callback during Authorize.
// Stage is one of "device_code_issued", "polling", "slow_down", "success",
// "error". UserCode / VerificationURI are populated at device_code_issued.
// Stage "error" carries no error detail — the caller renders the Authorize
// return value, not this struct, so secrets stay inside the bounded error
// strings produced below.
type StatusUpdate struct {
	Stage           string
	UserCode        string
	VerificationURI string
	ExpiresAt       time.Time
}

// DeviceToken is the result of a successful RFC 8628 flow.
type DeviceToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// DeviceProvider is the abstraction over provider-specific device flows.
// Implementations: GenericRFC8628Provider (HTTP-only, no SDK), future
// provider SDK adapters (GitHub, Google, etc).
type DeviceProvider interface {
	Authorize(ctx context.Context, clk Clock, slp Sleeper, progress func(StatusUpdate)) (*DeviceToken, error)
}

// GenericRFC8628Config configures a provider that follows RFC 8628 over
// HTTPS. DeviceURL and TokenURL must be HTTPS unless they are loopback
// (127.0.0.1, ::1, localhost). Loopback HTTP is the test exception.
type GenericRFC8628Config struct {
	ClientID     string
	ClientSecret string // optional; only some providers require
	DeviceURL    string
	TokenURL     string
	Scopes       []string
	HTTPClient   *http.Client      // override for testing; copied per Authorize
	Timeout      time.Duration     // per-request timeout; <=0 normalizes to 30s
	Redactor     *secrets.Redactor // registers client_secret and device_code
}

// GenericRFC8628Provider is the exported struct literal form; callers may
// construct it directly (e.g. embedded in a larger config). Authorize
// revalidates every security-sensitive field on each call so literals cannot
// bypass constructor checks.
type GenericRFC8628Provider struct {
	GenericRFC8628Config
	PollMin time.Duration // minimum poll interval; default 5s
}

// NewGenericRFC8628Provider validates ClientID + endpoints and registers
// ClientSecret with the Redactor when both are set. Authorize re-runs the
// same validation; constructor validation is for early-fail UX only.
func NewGenericRFC8628Provider(cfg GenericRFC8628Config) (*GenericRFC8628Provider, error) {
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("auth: device client id is required")
	}
	if err := validateEndpoint(cfg.DeviceURL); err != nil {
		return nil, errors.New("auth: invalid device authorization endpoint")
	}
	if err := validateEndpoint(cfg.TokenURL); err != nil {
		return nil, errors.New("auth: invalid token endpoint")
	}
	if cfg.Redactor != nil && cfg.ClientSecret != "" {
		cfg.Redactor.Register(cfg.ClientSecret)
	}
	return &GenericRFC8628Provider{
		GenericRFC8628Config: cfg,
		PollMin:              5 * time.Second,
	}, nil
}

// normalizedRuntime applies security defaults on every Authorize call. Tests
// and callers may construct GenericRFC8628Provider as a literal, so constructor-
// only defaults are insufficient: Timeout=0 would otherwise expire immediately
// and PollMin=0 plus an omitted interval would spin without sleeping.
func (p *GenericRFC8628Provider) normalizedRuntime() (*http.Client, time.Duration, time.Duration) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	pollMin := p.PollMin
	if pollMin <= 0 {
		pollMin = 5 * time.Second
	}
	base := p.HTTPClient
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	// Refuse all redirects: device endpoints never redirect in legitimate
	// flows, and a 302 could leak the device_code or client_secret in the
	// Location query string to an attacker-controlled host.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client, timeout, pollMin
}

// validateEndpoint enforces the URL shape required for OAuth endpoints:
// absolute, non-empty host, no userinfo, no fragment, HTTPS unless loopback.
// The error message is generic so the caller can wrap it without leaking
// which check failed (or the URL itself).
func validateEndpoint(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || u.Host == "" || u.Hostname() == "" {
		return errors.New("invalid absolute URL")
	}
	if u.User != nil || u.Fragment != "" {
		return errors.New("userinfo and fragments are forbidden")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme != "http" {
		return errors.New("scheme not allowed")
	}
	host := u.Hostname()
	if host == "127.0.0.1" || host == "::1" ||
		strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("plain HTTP is allowed only on loopback")
}

// Authorize runs the full RFC 8628 device authorization code flow. Every
// error returned uses a fixed, bounded message that does NOT include the
// device_code, request URL, response body, status code, or provider error
// identifier — those are untrusted and may carry secrets. progress receives
// one StatusUpdate per stage; pass nil to drop.
func (p *GenericRFC8628Provider) Authorize(ctx context.Context, clk Clock, slp Sleeper, progress func(StatusUpdate)) (*DeviceToken, error) {
	// GenericRFC8628Provider is exported and may be built as a struct literal;
	// repeat all security validation on every call rather than trusting that the
	// constructor ran.
	if p == nil || strings.TrimSpace(p.ClientID) == "" {
		return nil, errors.New("auth: device client id is required")
	}
	if err := validateEndpoint(p.DeviceURL); err != nil {
		return nil, errors.New("auth: invalid device authorization endpoint")
	}
	if err := validateEndpoint(p.TokenURL); err != nil {
		return nil, errors.New("auth: invalid token endpoint")
	}
	client, timeout, pollMin := p.normalizedRuntime()
	effective := *p
	effective.HTTPClient = client
	effective.Timeout = timeout
	effective.PollMin = pollMin
	p = &effective
	if p.Redactor != nil && p.ClientSecret != "" {
		p.Redactor.Register(p.ClientSecret)
	}
	if clk == nil {
		clk = realClock{}
	}
	if slp == nil {
		slp = realSleeper{}
	}
	if progress == nil {
		progress = func(StatusUpdate) {}
	}

	// Step 1: POST /device/code
	deviceCode, userCode, verificationURI, expiresIn, interval, err := p.requestDeviceCode(ctx)
	if err != nil {
		progress(StatusUpdate{Stage: "error"})
		return nil, err
	}
	if p.Redactor != nil {
		p.Redactor.Register(deviceCode)
	}
	if expiresIn <= 0 {
		err := errors.New("auth: device authorization server returned expires_in <= 0")
		progress(StatusUpdate{Stage: "error"})
		return nil, err
	}
	expiresAt := clk.Now().Add(safeDurationFromSeconds(expiresIn))
	progress(StatusUpdate{
		Stage:           "device_code_issued",
		UserCode:        userCode,
		VerificationURI: verificationURI,
		ExpiresAt:       expiresAt,
	})

	// Step 2: poll /token until success, expired, or context cancel.
	currentInterval := safeDurationFromSeconds(interval)
	if currentInterval < p.PollMin {
		currentInterval = p.PollMin
	}
	for {
		if clk.Now().After(expiresAt) {
			progress(StatusUpdate{Stage: "error"})
			return nil, errors.New("auth: device code expired")
		}
		// Check context before sleeping so a cancelled flow doesn't waste a
		// sleep cycle before noticing.
		select {
		case <-ctx.Done():
			progress(StatusUpdate{Stage: "error"})
			return nil, ctx.Err()
		default:
		}
		if err := slp.Sleep(ctx, currentInterval); err != nil {
			progress(StatusUpdate{Stage: "error"})
			return nil, err
		}
		// fakeSleeper advances fakeClock here; realSleeper observes wall time.
		if !clk.Now().Before(expiresAt) {
			progress(StatusUpdate{Stage: "error"})
			return nil, errors.New("auth: device code expired")
		}

		progress(StatusUpdate{Stage: "polling", ExpiresAt: expiresAt})
		tok, errCode, err := p.pollToken(ctx, deviceCode)
		if err != nil {
			// Network error: surface but keep polling (within expiry).
			continue
		}
		switch errCode {
		case "":
			progress(StatusUpdate{Stage: "success"})
			return tok, nil
		case "authorization_pending":
			// keep polling at current interval
			continue
		case "slow_down":
			currentInterval += 5 * time.Second
			progress(StatusUpdate{Stage: "slow_down", ExpiresAt: expiresAt})
			continue
		case "expired_token":
			// Do NOT include device_code in error text.
			progress(StatusUpdate{Stage: "error"})
			return nil, errors.New("auth: device code expired")
		case "access_denied":
			progress(StatusUpdate{Stage: "error"})
			return nil, errors.New("auth: user denied device authorization")
		default:
			// OAuth error identifiers are untrusted provider input. Do not
			// reflect the identifier, response body, endpoint, or status code.
			progress(StatusUpdate{Stage: "error"})
			return nil, errors.New("auth: device authorization failed")
		}
	}
}

const maxOAuthResponseBytes = 1 << 20

// readOAuthResponse caps response bodies at 1 MiB. Without this a hostile
// endpoint could exhaust memory or keep the connection open indefinitely.
func readOAuthResponse(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxOAuthResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxOAuthResponseBytes {
		return nil, errors.New("oauth response exceeds limit")
	}
	return raw, nil
}

func (p *GenericRFC8628Provider) requestDeviceCode(ctx context.Context) (deviceCode, userCode, verificationURI string, expiresIn, interval int, err error) {
	body := url.Values{"client_id": {p.ClientID}}
	if len(p.Scopes) > 0 {
		body.Set("scope", strings.Join(p.Scopes, " "))
	}
	reqCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST", p.DeviceURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", "", "", 0, 0, errors.New("auth: cannot build device authorization request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		// Transport errors can embed the request URL (including query params),
		// so this boundary deliberately does not wrap or stringify err.
		return "", "", "", 0, 0, errors.New("auth: device authorization request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", "", 0, 0, errors.New("auth: device authorization endpoint rejected request")
	}
	raw, readErr := readOAuthResponse(resp.Body)
	if readErr != nil {
		return "", "", "", 0, 0,
			errors.New("auth: invalid device authorization response")
	}
	var payload struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", "", "", 0, 0,
			errors.New("auth: invalid device authorization response")
	}
	if payload.DeviceCode == "" || payload.UserCode == "" ||
		payload.ExpiresIn <= 0 || payload.VerificationURI == "" ||
		validateEndpoint(payload.VerificationURI) != nil {
		return "", "", "", 0, 0,
			errors.New("auth: invalid device authorization response")
	}
	return payload.DeviceCode, payload.UserCode, payload.VerificationURI, payload.ExpiresIn, payload.Interval, nil
}

func (p *GenericRFC8628Provider) pollToken(ctx context.Context, deviceCode string) (*DeviceToken, string, error) {
	body := url.Values{
		"client_id":   {p.ClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	if p.ClientSecret != "" {
		body.Set("client_secret", p.ClientSecret)
	}
	reqCtx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "POST", p.TokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, "", errors.New("auth: cannot build token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, "", errors.New("auth: token request failed")
	}
	defer resp.Body.Close()
	raw, err := readOAuthResponse(resp.Body)
	if err != nil {
		return nil, "", errors.New("auth: cannot read token response")
	}
	if resp.StatusCode == http.StatusOK {
		var tok DeviceToken
		if err := json.Unmarshal(raw, &tok); err != nil {
			return nil, "", errors.New("auth: invalid token response")
		}
		if tok.AccessToken == "" {
			return nil, "", errors.New("auth: token endpoint returned empty access_token")
		}
		return &tok, "", nil
	}
	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &errResp); err != nil || errResp.Error == "" {
		return nil, "", errors.New("auth: token endpoint rejected request")
	}
	switch errResp.Error {
	case "authorization_pending", "slow_down", "expired_token", "access_denied":
		return nil, errResp.Error, nil
	default:
		// Map any unknown OAuth error to a sentinel the caller converts to
		// one fixed string — provider error identifiers are untrusted input.
		return nil, "unknown_oauth_error", nil
	}
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type realSleeper struct{}

func (realSleeper) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
