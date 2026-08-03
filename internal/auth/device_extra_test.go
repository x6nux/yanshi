package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/secrets"
)

// --- validateEndpoint edge cases ---

func TestValidateEndpoint_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool // true = valid
	}{
		{name: "fragment forbidden", url: "https://example.com/device#frag", want: false},
		{name: "userinfo forbidden", url: "https://user@example.com/device", want: false},
		{name: "no host", url: "https:///device", want: false},
		{name: "ftp scheme", url: "ftp://example.com/device", want: false},
		{name: "http non-loopback", url: "http://8.8.8.8/device", want: false},
		{name: "http loopback IP", url: "http://127.0.0.1:8080/device", want: true},
		{name: "http loopback v6", url: "http://[::1]:9999/token", want: true},
		{name: "http localhost", url: "http://localhost:7890/device", want: true},
		{name: "https any", url: "https://api.example.com/device", want: true},
		{name: "empty scheme", url: "://host/", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEndpoint(tc.url)
			got := err == nil
			if got != tc.want {
				t.Fatalf("validateEndpoint(%q) = %v, want valid=%v", tc.url, err, tc.want)
			}
		})
	}
}

// --- NewGenericRFC8628Provider edge cases ---

func TestNewGenericRFC8628Provider_EmptyClientID(t *testing.T) {
	_, err := NewGenericRFC8628Provider(GenericRFC8628Config{
		DeviceURL: "https://example.com/device",
		TokenURL:  "https://example.com/token",
	})
	if err == nil {
		t.Fatal("expected error for empty client id")
	}
}

func TestNewGenericRFC8628Provider_InvalidDeviceURL(t *testing.T) {
	_, err := NewGenericRFC8628Provider(GenericRFC8628Config{
		ClientID:  "id",
		DeviceURL: "not-a-url",
		TokenURL:  "https://example.com/token",
	})
	if err == nil {
		t.Fatal("expected error for invalid device URL")
	}
}

func TestNewGenericRFC8628Provider_InvalidTokenURL(t *testing.T) {
	_, err := NewGenericRFC8628Provider(GenericRFC8628Config{
		ClientID:  "id",
		DeviceURL: "https://example.com/device",
		TokenURL:  "not-a-url",
	})
	if err == nil {
		t.Fatal("expected error for invalid token URL")
	}
}

func TestNewGenericRFC8628Provider_ClientSecretRegistered(t *testing.T) {
	r := secrets.NewRedactor()
	p, err := NewGenericRFC8628Provider(GenericRFC8628Config{
		ClientID:     "id",
		ClientSecret: "my-secret",
		DeviceURL:    "https://example.com/device",
		TokenURL:     "https://example.com/token",
		Redactor:     r,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p == nil {
		t.Fatal("provider is nil")
	}
	// Redactor may wrap the secret in [REDACTED] brackets depending on
	// implementation; just verify it's not the raw secret.
	redacted := r.Redact("prefix-my-secret")
	if redacted == "prefix-my-secret" {
		t.Fatalf("client_secret not registered: %q", redacted)
	}
}

// --- normalizedRuntime edge cases ---

func TestNormalizedRuntime_Defaults(t *testing.T) {
	p := GenericRFC8628Provider{}
	client, timeout, pollMin := p.normalizedRuntime()
	if client == nil {
		t.Fatal("client is nil")
	}
	if timeout != 30*time.Second {
		t.Fatalf("timeout = %v", timeout)
	}
	if pollMin != 5*time.Second {
		t.Fatalf("pollMin = %v", pollMin)
	}
	// CheckRedirect must be set to refuse all redirects.
	if client.CheckRedirect == nil {
		t.Fatal("CheckRedirect not set")
	}
	if err := client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect = %v", err)
	}
}

// --- readOAuthResponse edge cases ---

func TestReadOAuthResponse_ReadError(t *testing.T) {
	_, err := readOAuthResponse(&errorReader{err: errors.New("read-sentinel")})
	if err == nil {
		t.Fatal("expected read error")
	}
}

func TestReadOAuthResponse_Oversized(t *testing.T) {
	_, err := readOAuthResponse(&sizedReader{n: maxOAuthResponseBytes + 1})
	if err == nil {
		t.Fatal("expected oversized error")
	}
}

func TestReadOAuthResponse_ExactMax(t *testing.T) {
	data := make([]byte, maxOAuthResponseBytes)
	got, err := readOAuthResponse(&sizedReader{n: len(data)})
	if err != nil || len(got) != maxOAuthResponseBytes {
		t.Fatalf("got len=%d err=%v", len(got), err)
	}
}

type errorReader struct{ err error }

func (e *errorReader) Read([]byte) (int, error) { return 0, e.err }

type sizedReader struct {
	n int
}

func (s *sizedReader) Read(p []byte) (int, error) {
	if s.n <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > s.n {
		n = s.n
	}
	for i := 0; i < n; i++ {
		p[i] = byte('x')
	}
	s.n -= n
	return n, nil
}

// --- requestDeviceCode edge cases ---

func TestRequestDeviceCode_WithScopes(t *testing.T) {
	var (
		gotBody    string
		deviceDone bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !deviceDone {
			deviceDone = true
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			jsonOrDie(w, map[string]any{
				"device_code":      "dc",
				"user_code":        "U",
				"verification_uri": "https://example.com/device",
				"expires_in":       60,
				"interval":         5,
			})
		} else {
			jsonOrDie(w, map[string]any{"access_token": "sk-scopes-ok", "token_type": "bearer"})
		}
	}))
	defer srv.Close()

	clk := newFakeClock(0)
	p := &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			Scopes:     []string{"read", "write"},
			DeviceURL:  srv.URL + "/device",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}
	tok, err := p.Authorize(context.Background(), clk, &fakeSleeper{clock: clk}, nil)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if tok.AccessToken != "sk-scopes-ok" {
		t.Fatalf("token: %q", tok.AccessToken)
	}
	if !strings.Contains(gotBody, "scope") {
		t.Fatalf("scopes not in request body: %q", gotBody)
	}
}

func TestRequestDeviceCode_HTTPError(t *testing.T) {
	// Server that returns non-2xx status
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	p := &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			DeviceURL:  srv.URL + "/device",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}

	clk := newFakeClock(0)
	_, err := p.Authorize(context.Background(), clk, &fakeSleeper{clock: clk}, nil)
	if err == nil {
		t.Fatal("expected error for non-2xx device response")
	}
}

func TestRequestDeviceCode_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	p := &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			DeviceURL:  srv.URL + "/device",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}

	clk := newFakeClock(0)
	_, err := p.Authorize(context.Background(), clk, &fakeSleeper{clock: clk}, nil)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestRequestDeviceCode_InvalidFields(t *testing.T) {
	tests := []struct {
		name   string
		fields string
	}{
		{name: "empty device_code", fields: `{}`},
		{name: "empty user_code", fields: `{"device_code":"dc","user_code":"","verification_uri":"https://example.com/device","expires_in":60}`},
		{name: "expires_in zero", fields: `{"device_code":"dc","user_code":"U","verification_uri":"https://example.com/device","expires_in":0}`},
		{name: "invalid verification_uri", fields: `{"device_code":"dc","user_code":"U","verification_uri":"not-a-url","expires_in":60}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(tc.fields))
			}))
			defer srv.Close()
			p := &GenericRFC8628Provider{
				GenericRFC8628Config: GenericRFC8628Config{
					ClientID:   "id",
					DeviceURL:  srv.URL + "/device",
					TokenURL:   srv.URL + "/token",
					HTTPClient: srv.Client(),
				},
			}
			clk := newFakeClock(0)
			_, err := p.Authorize(context.Background(), clk, &fakeSleeper{clock: clk}, nil)
			if err == nil {
				t.Fatal("expected error for invalid fields")
			}
		})
	}
}

// --- pollToken edge cases ---

func TestPollToken_BadJSON(t *testing.T) {
	clk := newFakeClock(0)
	slp := &fakeSleeper{clock: clk}
	var (
		deviceDone bool
		tokenCalls int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !deviceDone:
			deviceDone = true
			jsonOrDie(w, map[string]any{
				"device_code":      "dc",
				"user_code":        "U",
				"verification_uri": "https://example.com/device",
				"expires_in":       60,
				"interval":         1,
			})
		default:
			atomic.AddInt32(&tokenCalls, 1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`not json`))
		}
	}))
	defer srv.Close()

	p := &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			DeviceURL:  srv.URL + "/device",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}
	_, err := p.Authorize(context.Background(), clk, slp, nil)
	if err == nil {
		t.Fatal("expected error for bad token JSON")
	}
}

func TestPollToken_EmptyAccessToken(t *testing.T) {
	clk := newFakeClock(0)
	slp := &fakeSleeper{clock: clk}
	var deviceCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !deviceCalled:
			deviceCalled = true
			jsonOrDie(w, map[string]any{
				"device_code":      "dc",
				"user_code":        "U",
				"verification_uri": "https://example.com/device",
				"expires_in":       60,
				"interval":         1,
			})
		default:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"token_type":"bearer"}`))
		}
	}))
	defer srv.Close()

	p := &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			DeviceURL:  srv.URL + "/device",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}
	_, err := p.Authorize(context.Background(), clk, slp, nil)
	if err == nil {
		t.Fatal("expected error for empty access_token")
	}
}

func TestPollToken_ErrorResponseNoField(t *testing.T) {
	clk := newFakeClock(0)
	slp := &fakeSleeper{clock: clk}
	var deviceCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !deviceCalled:
			deviceCalled = true
			jsonOrDie(w, map[string]any{
				"device_code":      "dc",
				"user_code":        "U",
				"verification_uri": "https://example.com/device",
				"expires_in":       60,
				"interval":         1,
			})
		default:
			w.WriteHeader(400)
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	p := &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			DeviceURL:  srv.URL + "/device",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}
	_, err := p.Authorize(context.Background(), clk, slp, nil)
	if err == nil {
		t.Fatal("expected error for empty err response field")
	}
}

func TestPollToken_DoError(t *testing.T) {
	// Simulate a transport error by abruptly closing the connection.
	clk := newFakeClock(0)
	slp := &fakeSleeper{clock: clk}
	var (
		deviceDone bool
		tokenCalls int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !deviceDone:
			deviceDone = true
			jsonOrDie(w, map[string]any{
				"device_code":      "dc",
				"user_code":        "U",
				"verification_uri": "https://example.com/device",
				"expires_in":       60,
				"interval":         1,
			})
		default:
			// On token request, hijack and close the connection immediately
			// to simulate a transport-level error.
			if n := atomic.AddInt32(&tokenCalls, 1); n >= 5 {
				// After enough retries, return success so the test finishes.
				jsonOrDie(w, map[string]any{"access_token": "sk-after-err", "token_type": "bearer"})
				return
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("server does not support hijacking")
			}
			conn, _, hjErr := hj.Hijack()
			if hjErr != nil {
				t.Fatalf("hijack: %v", hjErr)
			}
			conn.Close()
		}
	}))
	defer srv.Close()

	p := &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			DeviceURL:  srv.URL + "/device",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}
	tok, err := p.Authorize(context.Background(), clk, slp, nil)
	if err != nil {
		t.Fatalf("Authorize after network error: %v", err)
	}
	if tok.AccessToken != "sk-after-err" {
		t.Fatalf("token: %q", tok.AccessToken)
	}
}

// --- Authorize edge cases ---

func TestAuthorize_AccessDenied(t *testing.T) {
	clk := newFakeClock(0)
	slp := &fakeSleeper{clock: clk}
	var deviceCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !deviceCalled:
			deviceCalled = true
			jsonOrDie(w, map[string]any{
				"device_code":      "dc",
				"user_code":        "U",
				"verification_uri": "https://example.com/device",
				"expires_in":       60,
				"interval":         1,
			})
		default:
			w.WriteHeader(400)
			jsonOrDie(w, map[string]any{"error": "access_denied"})
		}
	}))
	defer srv.Close()

	p := &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			DeviceURL:  srv.URL + "/device",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}
	_, err := p.Authorize(context.Background(), clk, slp, nil)
	if err == nil {
		t.Fatal("expected error for access_denied")
	}
}

func TestAuthorize_NilClkSlpProgress(t *testing.T) {
	// Test nil clk, nil slp, nil progress use defaults.
	// The server returns 400 on device code request, so the flow exits before
	// the poll loop (no real Sleep), but the nil defaults have been assigned.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	var p *GenericRFC8628Provider = &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			DeviceURL:  srv.URL + "/device",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
			Timeout:    time.Second,
		},
	}
	// Pass nil for clk, slp, progress.
	_, err := p.Authorize(context.Background(), nil, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- authorization_pending -> success path ---

func TestAuthorize_AuthorizationPendingThenSuccess(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/device":
			jsonOrDie(w, map[string]any{
				"device_code":      "dc",
				"user_code":        "U",
				"verification_uri": "https://example.com/device",
				"expires_in":       60,
				"interval":         1,
			})
		default:
			n := atomic.AddInt32(&polls, 1)
			if n == 1 {
				w.WriteHeader(400)
				jsonOrDie(w, map[string]any{"error": "authorization_pending"})
				return
			}
			jsonOrDie(w, map[string]any{"access_token": "sk-pending-ok", "token_type": "bearer"})
		}
	}))
	defer srv.Close()

	p := &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			DeviceURL:  srv.URL + "/device",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}
	clk := newFakeClock(0)
	tok, err := p.Authorize(context.Background(), clk, &fakeSleeper{clock: clk}, func(st StatusUpdate) {})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if tok.AccessToken != "sk-pending-ok" {
		t.Fatalf("token: %q", tok.AccessToken)
	}
}

// --- network error continue ---

func TestAuthorize_NetworkErrorPollContinue(t *testing.T) {
	var polls int32
	clk := newFakeClock(0)
	slp := &fakeSleeper{clock: clk}
	var deviceCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !deviceCalled && r.URL.Path == "/device":
			deviceCalled = true
			jsonOrDie(w, map[string]any{
				"device_code":      "dc",
				"user_code":        "U",
				"verification_uri": "https://example.com/device",
				"expires_in":       60,
				"interval":         1,
			})
		default:
			n := atomic.AddInt32(&polls, 1)
			// The first token request: resetting the connection causes a transport error.
			// Subsequent: success.
			if n == 1 {
				// Hijack the connection and close it to simulate a network error.
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("server does not support hijacking")
				}
				conn, _, _ := hj.Hijack()
				conn.Close()
				return
			}
			jsonOrDie(w, map[string]any{"access_token": "sk-after-err", "token_type": "bearer"})
		}
	}))
	defer srv.Close()

	p := &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			DeviceURL:  srv.URL + "/device",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}
	tok, err := p.Authorize(context.Background(), clk, slp, nil)
	if err != nil {
		t.Fatalf("Authorize (network error continue): %v", err)
	}
	if tok.AccessToken != "sk-after-err" {
		t.Fatalf("token: %q", tok.AccessToken)
	}
}

// --- client_secret in pollToken ---

func TestPollToken_SendsClientSecret(t *testing.T) {
	var gotClientSecret string
	clk := newFakeClock(0)
	slp := &fakeSleeper{clock: clk}
	var deviceCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !deviceCalled && r.URL.Path == "/device":
			deviceCalled = true
			jsonOrDie(w, map[string]any{
				"device_code":      "dc",
				"user_code":        "U",
				"verification_uri": "https://example.com/device",
				"expires_in":       60,
				"interval":         1,
			})
		default:
			b, _ := io.ReadAll(r.Body)
			gotClientSecret = string(b)
			jsonOrDie(w, map[string]any{"access_token": "sk-secret", "token_type": "bearer"})
		}
	}))
	defer srv.Close()

	p := &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:     "id",
			ClientSecret: "cs-value",
			DeviceURL:    srv.URL + "/device",
			TokenURL:     srv.URL + "/token",
			HTTPClient:   srv.Client(),
		},
	}
	_, err := p.Authorize(context.Background(), clk, slp, nil)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !strings.Contains(gotClientSecret, "client_secret=cs-value") {
		t.Fatalf("client_secret not in token body: %q", gotClientSecret)
	}
}

// --- realSleeper.Sleep coverage ---

func TestRealSleeper_Sleep_CancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (realSleeper{}).Sleep(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestRealSleeper_Sleep_TimerPath(t *testing.T) {
	ctx := context.Background()
	err := (realSleeper{}).Sleep(ctx, time.Millisecond)
	if err != nil {
		t.Fatalf("Sleep: %v", err)
	}
}

// --- realClock.Now coverage ---

func TestRealClock_Now(t *testing.T) {
	now := realClock{}.Now()
	if now.IsZero() {
		t.Fatal("realClock.Now returned zero time")
	}
}

// --- Authorize nil provider ---

func TestAuthorize_NilProvider(t *testing.T) {
	var p *GenericRFC8628Provider
	_, err := p.Authorize(context.Background(), newFakeClock(0), &fakeSleeper{}, nil)
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

// --- Authorize default error (unknown oauth error) ---

func TestAuthorize_UnknownOAuthError(t *testing.T) {
	clk := newFakeClock(0)
	slp := &fakeSleeper{clock: clk}
	var deviceCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !deviceCalled:
			deviceCalled = true
			jsonOrDie(w, map[string]any{
				"device_code":      "dc",
				"user_code":        "U",
				"verification_uri": "https://example.com/device",
				"expires_in":       60,
				"interval":         1,
			})
		default:
			w.WriteHeader(400)
			jsonOrDie(w, map[string]any{"error": "some_weird_error"})
		}
	}))
	defer srv.Close()

	p := &GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			DeviceURL:  srv.URL + "/device",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}
	_, err := p.Authorize(context.Background(), clk, slp, nil)
	if err == nil {
		t.Fatal("expected error for unknown OAuth error")
	}
	if !strings.Contains(err.Error(), "authorization failed") &&
		!strings.Contains(err.Error(), "auth: device authorization failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func jsonOrDie(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Encode manually without importing encoding/json in every test.
	switch m := v.(type) {
	case map[string]any:
		writeJSONMap(w, m)
	default:
		panic("jsonOrDie: unsupported type")
	}
}

func writeJSONMap(w http.ResponseWriter, m map[string]any) {
	w.Write([]byte(encodeMap(m)))
}

func encodeMap(m map[string]any) string {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for k, v := range m {
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteByte('"')
		b.WriteString(k)
		b.WriteString("\":")
		switch val := v.(type) {
		case string:
			b.WriteByte('"')
			b.WriteString(val)
			b.WriteByte('"')
		case int:
			fmt.Fprintf(&b, "%d", val)
		default:
			b.WriteString(`"?"`)
		}
	}
	b.WriteByte('}')
	return b.String()
}
