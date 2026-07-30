package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/secrets"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(sec int64) *fakeClock {
	return &fakeClock{t: time.Unix(sec, 0)}
}
func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}
func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

type fakeSleeper struct {
	clock *fakeClock
	slept time.Duration
}

func (f *fakeSleeper) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		f.slept += d
		if f.clock != nil {
			f.clock.Advance(d)
		}
		return nil
	}
}

// TestDeviceFlow_HappyPath exercises the standard poll-once-succeeds path.
func TestDeviceFlow_HappyPath(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device/code":
			json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "dc-123",
				"user_code":        "USER-CODE",
				"verification_uri": "https://example.com/device",
				"expires_in":       900,
				"interval":         5,
			})
		case "/token":
			atomic.AddInt32(&polls, 1)
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "sk-device-token",
				"token_type":   "bearer",
			})
		}
	}))
	defer srv.Close()

	clk := newFakeClock(1000)
	slp := &fakeSleeper{clock: clk}
	p := GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "test-id",
			DeviceURL:  srv.URL + "/device/code",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
		PollMin: time.Second,
	}
	tok, err := p.Authorize(context.Background(), clk, slp, func(st StatusUpdate) {})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if tok.AccessToken != "sk-device-token" {
		t.Fatalf("token: %q", tok.AccessToken)
	}
	if atomic.LoadInt32(&polls) != 1 {
		t.Fatalf("expected 1 poll, got %d", polls)
	}
}

// TestDeviceFlow_SlowDownBacksOff: first poll returns slow_down → interval
// MUST increase by 5s; subsequent poll succeeds.
func TestDeviceFlow_SlowDownBacksOff(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			n := atomic.AddInt32(&polls, 1)
			if n == 1 {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "sk-ok"})
		} else {
			json.NewEncoder(w).Encode(map[string]any{
				"device_code": "dc", "user_code": "U",
				"verification_uri": "https://example.com/device",
				"expires_in": 900, "interval": 2,
			})
		}
	}))
	defer srv.Close()
	clk := newFakeClock(0)
	slp := &fakeSleeper{clock: clk}
	p := GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "test",
			DeviceURL:  srv.URL + "/device/code",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}
	if _, err := p.Authorize(context.Background(), clk, slp, func(st StatusUpdate) {}); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if slp.slept < 7*time.Second { // 2 initial + 5 backoff
		t.Fatalf("slow_down backoff not applied: slept %v", slp.slept)
	}
}

// TestDeviceFlow_ExpiredDeviceCode: poll returns expired_token → Manager
// must return an error (not loop forever) AND must not leak the device_code.
func TestDeviceFlow_ExpiredDeviceCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": "expired_token"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc-SECRET", "user_code": "U",
			"verification_uri": "https://example.com/device",
			"expires_in": 900, "interval": 1,
		})
	}))
	defer srv.Close()
	p := GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID: "test", DeviceURL: srv.URL + "/device/code",
			TokenURL: srv.URL + "/token", HTTPClient: srv.Client(),
		},
	}
	clk := newFakeClock(0)
	_, err := p.Authorize(context.Background(), clk, &fakeSleeper{clock: clk}, func(st StatusUpdate) {})
	if err == nil {
		t.Fatal("expected error on expired_token")
	}
	if strings.Contains(err.Error(), "dc-SECRET") {
		t.Fatalf("device_code leaked in error: %v", err)
	}
}

// TestDeviceFlow_ContextCancel: context cancellation must abort the poll
// loop on the next iteration boundary (no later).
func TestDeviceFlow_ContextCancel(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			<-block
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc", "user_code": "U",
			"verification_uri": "https://example.com/device",
			"expires_in": 900, "interval": 1,
		})
	}))
	defer srv.Close()
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	p := GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID: "test", DeviceURL: srv.URL + "/device/code",
			TokenURL: srv.URL + "/token", HTTPClient: srv.Client(),
		},
	}
	clk := newFakeClock(0)
	_, err := p.Authorize(ctx, clk, &fakeSleeper{clock: clk}, func(st StatusUpdate) {})
	if err == nil {
		t.Fatal("expected context error")
	}
}

// TestDeviceFlow_ExpiresInZeroReturnsError: RFC 8628 requires expires_in > 0;
// a 0/negative value must be rejected immediately (not divide-by-zero).
func TestDeviceFlow_ExpiresInZeroReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc", "user_code": "U", "expires_in": 0, "interval": 1,
		})
	}))
	defer srv.Close()
	p := GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID: "test", DeviceURL: srv.URL + "/device/code",
			TokenURL: srv.URL + "/token", HTTPClient: srv.Client(),
		},
	}
	clk := newFakeClock(0)
	_, err := p.Authorize(context.Background(), clk, &fakeSleeper{clock: clk}, func(st StatusUpdate) {})
	if err == nil {
		t.Fatal("expected error on expires_in=0")
	}
}

// TestDeviceFlow_HTTPSOnlyByDefault: a non-loopback HTTP scheme must be
// rejected at provider construction. Loopback (127.0.0.1, ::1, localhost)
// is allowed for tests.
func TestDeviceFlow_HTTPSOnlyByDefault(t *testing.T) {
	_, err := NewGenericRFC8628Provider(GenericRFC8628Config{
		ClientID:  "x",
		DeviceURL: "http://api.example.com/device/code",
		TokenURL:  "http://api.example.com/token",
	})
	if err == nil {
		t.Fatal("expected error on http:// non-loopback URL")
	}
	// Loopback is allowed.
	p, err := NewGenericRFC8628Provider(GenericRFC8628Config{
		ClientID:  "x",
		DeviceURL: "http://127.0.0.1:9999/device/code",
		TokenURL:  "http://[::1]:9999/token",
	})
	if err != nil {
		t.Fatalf("loopback http must be allowed: %v", err)
	}
	_ = p
}

func TestDeviceFlow_ValidatesLiteralOnEveryAuthorize(t *testing.T) {
	tests := []struct {
		name string
		cfg  GenericRFC8628Config
	}{
		{
			name: "empty-client-id",
			cfg: GenericRFC8628Config{
				DeviceURL: "https://example.com/device",
				TokenURL:  "https://example.com/token",
			},
		},
		{
			name: "endpoint-userinfo",
			cfg: GenericRFC8628Config{
				ClientID:  "id",
				DeviceURL: "https://user:password@example.com/device",
				TokenURL:  "https://example.com/token",
			},
		},
		{
			name: "empty-endpoint-host",
			cfg: GenericRFC8628Config{
				ClientID:  "id",
				DeviceURL: "https:///device",
				TokenURL:  "https://example.com/token",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := GenericRFC8628Provider{GenericRFC8628Config: tt.cfg}
			_, err := p.Authorize(context.Background(), newFakeClock(0),
				&fakeSleeper{}, nil)
			if err == nil {
				t.Fatal("literal must be revalidated by Authorize")
			}
			for _, forbidden := range []string{
				"user:password", "example.com", "/device", "/token",
			} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("validation error leaked endpoint detail %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestDeviceFlow_RequiresVerificationURI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code": "dc", "user_code": "U", "expires_in": 60,
		})
	}))
	defer srv.Close()
	p := GenericRFC8628Provider{GenericRFC8628Config: GenericRFC8628Config{
		ClientID: "id", DeviceURL: srv.URL, TokenURL: srv.URL,
		HTTPClient: srv.Client(),
	}}
	_, err := p.Authorize(context.Background(), newFakeClock(0),
		&fakeSleeper{}, nil)
	if err == nil || err.Error() != "auth: invalid device authorization response" {
		t.Fatalf("want fixed invalid-response error, got %v", err)
	}
}

func TestDeviceFlow_LiteralProviderNormalizesDefaults(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			atomic.AddInt32(&polls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "sk-json-tag",
				"token_type":   "bearer",
			})
			return
		}
		// interval intentionally omitted. PollMin and Timeout are also zero in
		// the literal below; Authorize must normalize all three at runtime.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dc-defaults",
			"user_code":        "U",
			"verification_uri": "https://example.com/device",
			"expires_in":       60,
		})
	}))
	defer srv.Close()

	clk := newFakeClock(0)
	slp := &fakeSleeper{clock: clk}
	p := GenericRFC8628Provider{
		GenericRFC8628Config: GenericRFC8628Config{
			ClientID:   "id",
			DeviceURL:  srv.URL + "/device/code",
			TokenURL:   srv.URL + "/token",
			HTTPClient: srv.Client(),
		},
	}
	tok, err := p.Authorize(context.Background(), clk, slp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "sk-json-tag" {
		t.Fatalf("access_token JSON tag not decoded: %#v", tok)
	}
	if slp.slept != 5*time.Second || atomic.LoadInt32(&polls) != 1 {
		t.Fatalf("runtime defaults not normalized: slept=%v polls=%d", slp.slept, polls)
	}
}

func TestDeviceFlow_RejectsRedirectAndRegistersRequestSecrets(t *testing.T) {
	const deviceCode = "dc-redaction-sentinel"
	const clientSecret = "client-secret-sentinel"
	r := secrets.NewRedactor()
	var redirected int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      deviceCode,
				"user_code":        "U",
				"verification_uri": "https://example.com/device",
				"expires_in":       60,
				"interval":         1,
			})
		case "/token":
			http.Redirect(w, req, "/redirected?client_secret="+clientSecret, http.StatusFound)
		case "/redirected":
			atomic.AddInt32(&redirected, 1)
		}
	}))
	defer srv.Close()

	p, err := NewGenericRFC8628Provider(GenericRFC8628Config{
		ClientID:     "id",
		ClientSecret: clientSecret,
		DeviceURL:    srv.URL + "/device/code",
		TokenURL:     srv.URL + "/token",
		HTTPClient:   srv.Client(),
		Redactor:     r,
	})
	if err != nil {
		t.Fatal(err)
	}
	clk := newFakeClock(0)
	_, err = p.Authorize(context.Background(), clk, &fakeSleeper{clock: clk}, nil)
	if err == nil {
		t.Fatal("redirected token endpoint must fail")
	}
	if atomic.LoadInt32(&redirected) != 0 {
		t.Fatal("OAuth client followed redirect")
	}
	for _, sentinel := range []string{deviceCode, clientSecret} {
		if strings.Contains(err.Error(), sentinel) {
			t.Fatalf("error leaked %q: %v", sentinel, err)
		}
		if strings.Contains(r.Redact("value="+sentinel), sentinel) {
			t.Fatalf("redactor did not register %q", sentinel)
		}
	}
}

type fakeCredentialStore struct {
	mu          sync.RWMutex
	values      map[string]string
	setCalls    int
	failSetCall int
	failSetErr  error
	failDelete  error
}

func credentialKey(service, account string) string { return service + "\x00" + account }
func (f *fakeCredentialStore) Available() error { return nil }
func (f *fakeCredentialStore) Get(service, account string) (string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	value, ok := f.values[credentialKey(service, account)]
	if !ok {
		return "", secrets.ErrSecretNotFound
	}
	return value, nil
}
func (f *fakeCredentialStore) Set(service, account, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	if f.failSetCall != 0 && f.setCalls == f.failSetCall {
		return f.failSetErr
	}
	f.values[credentialKey(service, account)] = value
	return nil
}
func (f *fakeCredentialStore) Delete(service, account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete != nil {
		return f.failDelete
	}
	key := credentialKey(service, account)
	if _, ok := f.values[key]; !ok {
		return secrets.ErrSecretNotFound
	}
	delete(f.values, key)
	return nil
}

type metadataStoreFunc func(string, string, AuthMetadata) error

func (f metadataStoreFunc) SaveAuthMetadata(
	provider, account string,
	meta AuthMetadata,
) error {
	return f(provider, account, meta)
}

// LoadAuthMetadata / DeleteAuthMetadata satisfy the Task 14 MetadataStore
// signature. The device-token tests only exercise Save; the no-record paths
// return ErrAuthMetadataNotFound so any future Status/Logout call against
// these stubs behaves like the real adapter.
func (f metadataStoreFunc) LoadAuthMetadata(string, string) (AuthMetadata, error) {
	return AuthMetadata{}, ErrAuthMetadataNotFound
}
func (f metadataStoreFunc) DeleteAuthMetadata(string, string) error {
	return ErrAuthMetadataNotFound
}

func managerWithCredentialStore(t *testing.T, st secrets.Store) *Manager {
	t.Helper()
	sm, err := secrets.NewManager(secrets.Config{Backend: "none"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sm.Close() })
	m := NewManager(sm)
	m.store = st
	return m
}

func TestCommitDeviceToken_MetadataFailureCompensates(t *testing.T) {
	metadataErr := errors.New("metadata-write-sentinel")
	for _, hadOld := range []bool{false, true} {
		t.Run(fmt.Sprintf("had-old-%v", hadOld), func(t *testing.T) {
			st := &fakeCredentialStore{values: map[string]string{}}
			if hadOld {
				st.values[credentialKey("provider", "main")] = "old-token"
			}
			m := managerWithCredentialStore(t, st)
			m.SetMetadataStore(metadataStoreFunc(func(
				string, string, AuthMetadata,
			) error { return metadataErr }))

			err := m.commitDeviceToken("provider", "main", &DeviceToken{
				AccessToken: "new-token", ExpiresIn: 60,
			})
			if !errors.Is(err, metadataErr) {
				t.Fatalf("want metadata error, got %v", err)
			}
			got, getErr := st.Get("provider", "main")
			if hadOld {
				if getErr != nil || got != "old-token" {
					t.Fatalf("old token not restored: got=%q err=%v", got, getErr)
				}
			} else if !errors.Is(getErr, secrets.ErrSecretNotFound) {
				t.Fatalf("new token not removed: got=%q err=%v", got, getErr)
			}
		})
	}
}

func TestCommitDeviceToken_RollbackFailureIsJoined(t *testing.T) {
	metadataErr := errors.New("metadata-write-sentinel")
	rollbackErr := errors.New("rollback-sentinel")
	tests := []struct {
		name  string
		store *fakeCredentialStore
	}{
		{
			name: "restore-old-token-fails",
			store: &fakeCredentialStore{
				values: map[string]string{
					credentialKey("provider", "main"): "old-token",
				},
				failSetCall: 2,
				failSetErr:  rollbackErr,
			},
		},
		{
			name: "delete-new-token-fails",
			store: &fakeCredentialStore{
				values:     map[string]string{},
				failDelete: rollbackErr,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := managerWithCredentialStore(t, tt.store)
			m.SetMetadataStore(metadataStoreFunc(func(
				string, string, AuthMetadata,
			) error { return metadataErr }))
			err := m.commitDeviceToken("provider", "main", &DeviceToken{
				AccessToken: "new-token", ExpiresIn: 60,
			})
			if !errors.Is(err, metadataErr) || !errors.Is(err, rollbackErr) {
				t.Fatalf("errors.Join must preserve both causes: %v", err)
			}
		})
	}
}

func TestCommitDeviceToken_UsesInjectedClockForExpiry(t *testing.T) {
	st := &fakeCredentialStore{values: map[string]string{}}
	m := managerWithCredentialStore(t, st)
	m.SetClock(newFakeClock(1234))
	var saved AuthMetadata
	m.SetMetadataStore(metadataStoreFunc(func(
		_, _ string, meta AuthMetadata,
	) error {
		saved = meta
		return nil
	}))
	if err := m.commitDeviceToken("provider", "main", &DeviceToken{
		AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 60,
	}); err != nil {
		t.Fatal(err)
	}
	want := time.Unix(1294, 0)
	if !saved.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want injected-clock value %v", saved.ExpiresAt, want)
	}
}

// TestDeviceFlow_RejectsOversizedOAuthBodies verifies both the device-code
// response AND the token response are bounded by maxOAuthResponseBytes. A
// hostile endpoint could otherwise exhaust memory or stall the connection.
func TestDeviceFlow_RejectsOversizedOAuthBodies(t *testing.T) {
	const oversized = maxOAuthResponseBytes + 1
	tests := []struct {
		name       string
		deviceBody bool
	}{
		{name: "device response", deviceBody: true},
		{name: "token response", deviceBody: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/device" && !tc.deviceBody {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"device_code":      "dc-limit",
						"user_code":        "CODE",
						"verification_uri": "https://example.com/device",
						"expires_in":       60,
						"interval":         1,
					})
					return
				}
				_, _ = w.Write(bytes.Repeat([]byte("x"), oversized))
			}))
			defer srv.Close()
			p := &GenericRFC8628Provider{GenericRFC8628Config: GenericRFC8628Config{
				ClientID: "id", DeviceURL: srv.URL + "/device",
				TokenURL: srv.URL + "/token", HTTPClient: srv.Client(),
			}}
			clk := newFakeClock(0)
			if _, err := p.Authorize(context.Background(), clk,
				&fakeSleeper{clock: clk}, nil); err == nil {
				t.Fatal("oversized OAuth response accepted")
			}
		})
	}
}

// TestDeviceFlow_UnknownOAuthErrorHasFixedSafeText asserts that an unknown
// OAuth error code does NOT leak the response body, endpoint query, status
// code, unknown error identifier, device code, or client secret into the
// returned error. All secrets are also registered with the Redactor so the
// rollback path cannot leak them either.
func TestDeviceFlow_UnknownOAuthErrorHasFixedSafeText(t *testing.T) {
	sentinels := []string{
		"device-code-secret", "client-secret-value", "provider_weird_error",
		"provider-body-secret", "endpoint-query-secret", "418",
	}
	redactor := secrets.NewRedactor()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "device-code-secret",
				"user_code":        "CODE",
				"verification_uri": "https://example.com/device",
				"expires_in":       60,
				"interval":         1,
			})
		case "/token":
			w.WriteHeader(http.StatusTeapot)
			_, _ = io.WriteString(w,
				`{"error":"provider_weird_error","error_description":"provider-body-secret"}`)
		}
	}))
	defer srv.Close()

	p := &GenericRFC8628Provider{GenericRFC8628Config: GenericRFC8628Config{
		ClientID:     "id",
		ClientSecret: "client-secret-value",
		DeviceURL:    srv.URL + "/device",
		TokenURL:     srv.URL + "/token?detail=endpoint-query-secret",
		HTTPClient:   srv.Client(), Redactor: redactor,
	}}
	clk := newFakeClock(0)
	_, err := p.Authorize(context.Background(), clk,
		&fakeSleeper{clock: clk}, nil)
	if err == nil {
		t.Fatal("unknown OAuth error must fail")
	}
	for _, sentinel := range sentinels {
		if strings.Contains(err.Error(), sentinel) {
			t.Fatalf("OAuth error leaked %q: %v", sentinel, err)
		}
	}
	for _, secret := range []string{"device-code-secret", "client-secret-value"} {
		if strings.Contains(redactor.Redact("value="+secret), secret) {
			t.Fatalf("redactor did not register %q", secret)
		}
	}
}
