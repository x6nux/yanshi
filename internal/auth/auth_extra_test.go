package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/secrets"
)

// fakeDeviceProvider is a controllable DeviceProvider stub for testing
// Manager.RunDeviceFlow.
type fakeDeviceProvider struct {
	tok *DeviceToken
	err error
}

func (f *fakeDeviceProvider) Authorize(
	_ context.Context, _ Clock, _ Sleeper, _ func(StatusUpdate),
) (*DeviceToken, error) {
	return f.tok, f.err
}

// TestManager_Secrets verifies that Secrets() returns the underlying
// secrets.Manager and its store is accessible.
func TestManager_Secrets(t *testing.T) {
	m := newTestManager(t)
	got := m.Secrets()
	if got == nil {
		t.Fatal("Secrets() returned nil")
	}
	if err := got.Store().Set("p", "a", "v"); err != nil {
		t.Fatalf("store.Set: %v", err)
	}
	val, err := got.Store().Get("p", "a")
	if err != nil || val != "v" {
		t.Fatalf("store.Get: %q, %v", val, err)
	}
}

// TestManager_RegisterDeviceProvider trivially covers RegisterDeviceProvider
// and verifies RunDeviceFlow rejects an unknown provider.
func TestManager_RegisterDeviceProvider(t *testing.T) {
	m := newTestManager(t)
	m.RegisterDeviceProvider("test-dp", &fakeDeviceProvider{})
	_, err := m.RunDeviceFlow(context.Background(), "unknown", "acct", nil)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

// TestManager_RunDeviceFlow_HappyPath exercises RunDeviceFlow through to
// successful token commit.
func TestManager_RunDeviceFlow_HappyPath(t *testing.T) {
	m := newTestManager(t)
	meta := &memoryMetadataStore{rows: map[string]AuthMetadata{}}
	m.SetMetadataStore(meta)

	m.RegisterDeviceProvider("test-dp", &fakeDeviceProvider{
		tok: &DeviceToken{AccessToken: "flow-access-token", ExpiresIn: 3600, TokenType: "Bearer"},
	})

	var stdout bytes.Buffer
	tok, err := m.RunDeviceFlow(context.Background(), "test-dp", "acct", &stdout)
	if err != nil {
		t.Fatalf("RunDeviceFlow: %v", err)
	}
	if tok.AccessToken != "flow-access-token" {
		t.Fatalf("token: %q", tok.AccessToken)
	}

	val, getErr := m.secrets.Store().Get("test-dp", "acct")
	if getErr != nil {
		t.Fatalf("store.Get: %v", getErr)
	}
	if val != "flow-access-token" {
		t.Fatalf("stored credential: %q", val)
	}

	md, metaErr := meta.LoadAuthMetadata("test-dp", "acct")
	if metaErr != nil {
		t.Fatalf("LoadAuthMetadata: %v", metaErr)
	}
	if md.Source != "device" {
		t.Fatalf("metadata source: %q", md.Source)
	}
	if out := stdout.String(); out != "authenticated\n" {
		t.Fatalf("stdout: %q", out)
	}
}

// TestManager_RunDeviceFlow_AuthorizeError propagates the Authorize error.
func TestManager_RunDeviceFlow_AuthorizeError(t *testing.T) {
	m := newTestManager(t)
	sentinel := errors.New("authorize-sentinel")
	m.RegisterDeviceProvider("test-dp", &fakeDeviceProvider{err: sentinel})
	_, err := m.RunDeviceFlow(context.Background(), "test-dp", "acct", nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
}

// TestManager_RunDeviceFlow_NilToken returns an error when provider returns (nil, nil).
func TestManager_RunDeviceFlow_NilToken(t *testing.T) {
	m := newTestManager(t)
	m.RegisterDeviceProvider("test-dp", &fakeDeviceProvider{})
	_, err := m.RunDeviceFlow(context.Background(), "test-dp", "acct", nil)
	if err == nil {
		t.Fatal("expected nil-token error")
	}
}

// TestManager_RunDeviceFlow_stdoutDiscardNil passes nil stdout to verify
// the io.Discard default path.
func TestManager_RunDeviceFlow_stdoutDiscardNil(t *testing.T) {
	m := newTestManager(t)
	meta := &memoryMetadataStore{rows: map[string]AuthMetadata{}}
	m.SetMetadataStore(meta)
	m.RegisterDeviceProvider("test-dp", &fakeDeviceProvider{
		tok: &DeviceToken{AccessToken: "discard-token", ExpiresIn: 3600},
	})
	if _, err := m.RunDeviceFlow(context.Background(), "test-dp", "acct", nil); err != nil {
		t.Fatalf("RunDeviceFlow (nil stdout): %v", err)
	}
}

// TestManager_RunDeviceFlow_commitDeviceTokenError propagates the
// commitDeviceToken error when metadata store is nil.
func TestManager_RunDeviceFlow_commitDeviceTokenError(t *testing.T) {
	m := newTestManager(t)
	m.RegisterDeviceProvider("test-dp", &fakeDeviceProvider{
		tok: &DeviceToken{AccessToken: "token", ExpiresIn: 3600},
	})
	_, err := m.RunDeviceFlow(context.Background(), "test-dp", "acct", nil)
	if err == nil {
		t.Fatal("expected commitDeviceToken error")
	}
}

// TestManager_RunDeviceFlow_SecretsRegistration registers access and refresh
// tokens when secrets manager is available.
func TestManager_RunDeviceFlow_SecretsRegistration(t *testing.T) {
	m := newTestManager(t)
	meta := &memoryMetadataStore{rows: map[string]AuthMetadata{}}
	m.SetMetadataStore(meta)
	redactor := m.secrets.Redactor()

	access := "access-for-redaction"
	refresh := "refresh-for-redaction"
	m.RegisterDeviceProvider("test-dp", &fakeDeviceProvider{
		tok: &DeviceToken{AccessToken: access, RefreshToken: refresh, ExpiresIn: 3600},
	})
	if _, err := m.RunDeviceFlow(context.Background(), "test-dp", "acct", nil); err != nil {
		t.Fatalf("RunDeviceFlow: %v", err)
	}
	for _, secret := range []string{access, refresh} {
		if redacted := redactor.Redact("prefix-" + secret); !strings.Contains(redacted, "[REDACTED]") {
			t.Fatalf("secret %q was not registered by redactor, got %q", secret, redacted)
		}
	}
}

// TestManager_RunDeviceFlow_ProgressCallback exercises stage callbacks in the
// progress function, verifying that expected strings reach stdout.
func TestManager_RunDeviceFlow_ProgressCallback(t *testing.T) {
	m := newTestManager(t)
	meta := &memoryMetadataStore{rows: map[string]AuthMetadata{}}
	m.SetMetadataStore(meta)

	m.RegisterDeviceProvider("test-dp", &deviceProviderWithProgress{
		fn: func(ctx context.Context, clk Clock, slp Sleeper, progress func(StatusUpdate)) (*DeviceToken, error) {
			progress(StatusUpdate{Stage: "device_code_issued", UserCode: "XYZ-123", VerificationURI: "https://example.com/device"})
			progress(StatusUpdate{Stage: "polling"})
			progress(StatusUpdate{Stage: "slow_down"})
			progress(StatusUpdate{Stage: "success"})
			return &DeviceToken{AccessToken: "tok-progress", ExpiresIn: 3600}, nil
		},
	})
	var stdout bytes.Buffer
	if _, err := m.RunDeviceFlow(context.Background(), "test-dp", "acct", &stdout); err != nil {
		t.Fatalf("RunDeviceFlow: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "XYZ-123") || !strings.Contains(out, "polling") ||
		!strings.Contains(out, "slowing down") || !strings.Contains(out, "authenticated") {
		t.Fatalf("progress output missing stages: %q", out)
	}
}

type deviceProviderWithProgress struct {
	fn func(ctx context.Context, clk Clock, slp Sleeper, progress func(StatusUpdate)) (*DeviceToken, error)
}

func (d *deviceProviderWithProgress) Authorize(ctx context.Context, clk Clock, slp Sleeper, progress func(StatusUpdate)) (*DeviceToken, error) {
	return d.fn(ctx, clk, slp, progress)
}

// --- Status edge cases ---

func TestManager_Status_StoreNil(t *testing.T) {
	m := NewManager(nil)
	st, err := m.Status("p", "a")
	if err != nil {
		t.Fatalf("Status with nil store: %v", err)
	}
	if st.Authenticated {
		t.Fatal("expected unauthenticated")
	}
}

func TestManager_Status_GetError(t *testing.T) {
	getErr := errors.New("get-sentinel")
	manager := managerWithCredentialStore(t, &errorCredentialStore{getErr: getErr})
	_, err := manager.Status("p", "a")
	if !errors.Is(err, getErr) {
		t.Fatalf("want getErr, got %v", err)
	}
}

type errorCredentialStore struct {
	getErr    error
	setErr    error
	deleteErr error
}

func (e *errorCredentialStore) Available() error                { return nil }
func (e *errorCredentialStore) Get(_, _ string) (string, error) { return "", e.getErr }
func (e *errorCredentialStore) Set(_, _, _ string) error        { return e.setErr }
func (e *errorCredentialStore) Delete(_, _ string) error        { return e.deleteErr }

func TestManager_Status_EmptyValue(t *testing.T) {
	credentials := &fakeCredentialStore{values: map[string]string{
		credentialKey("p", "a"): "",
	}}
	manager := managerWithCredentialStore(t, credentials)
	// Hit the empty-value path.
	_ = credentials.Set("p", "a", "")
	st, err := manager.Status("p", "a")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Authenticated {
		t.Fatal("expected unauthenticated for empty value")
	}
}

func TestManager_Status_NoMetadata(t *testing.T) {
	credentials := &fakeCredentialStore{values: map[string]string{
		credentialKey("p", "a"): "token",
	}}
	manager := managerWithCredentialStore(t, credentials)
	st, err := manager.Status("p", "a")
	if err != nil {
		t.Fatalf("Status without metadata: %v", err)
	}
	if !st.Authenticated || st.Source != "secret" {
		t.Fatalf("Status = %#v", st)
	}
}

// --- Logout edge cases ---

func TestManager_Logout_StoreNil(t *testing.T) {
	m := NewManager(nil)
	err := m.Logout("p", "a")
	if err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestManager_Logout_SnapshotCredentialError(t *testing.T) {
	snapErr := errors.New("snapshot-sentinel")
	manager := managerWithCredentialStore(t, &errorCredentialStore{getErr: snapErr})
	err := manager.Logout("p", "a")
	if !errors.Is(err, snapErr) {
		t.Fatalf("want snapshot error, got %v", err)
	}
}

func TestManager_Logout_SnapshotMetadataError(t *testing.T) {
	metaErr := errors.New("meta-snapshot-sentinel")
	credentials := &fakeCredentialStore{values: map[string]string{
		credentialKey("p", "a"): "some-token",
	}}
	manager := managerWithCredentialStore(t, credentials)
	meta := &memoryMetadataStore{
		rows: map[string]AuthMetadata{
			metadataKey("p", "a"): {Source: "device"},
		},
		loadErr: metaErr,
	}
	manager.SetMetadataStore(meta)
	err := manager.Logout("p", "a")
	if !errors.Is(err, metaErr) {
		t.Fatalf("want metadata error, got %v", err)
	}
}

func TestManager_Logout_DeleteCredentialError(t *testing.T) {
	deleteErr := errors.New("delete-sentinel")
	credentials := &fakeCredentialStore{
		values:     map[string]string{credentialKey("p", "a"): "tok"},
		failDelete: deleteErr,
	}
	manager := managerWithCredentialStore(t, credentials)
	manager.SetMetadataStore(&memoryMetadataStore{
		rows: map[string]AuthMetadata{metadataKey("p", "a"): {Source: "device"}},
	})
	err := manager.Logout("p", "a")
	if !errors.Is(err, deleteErr) {
		t.Fatalf("want delete error, got %v", err)
	}
}

// TestManager_Logout_MetadataDeleteFailNoToken covers the path where
// hadToken=false and metadata delete fails. The error should be the metadata
// error only, with no restore attempt.
func TestManager_Logout_MetadataDeleteFailNoToken(t *testing.T) {
	metaErr := errors.New("meta-delete-sentinel")
	credentials := &fakeCredentialStore{values: map[string]string{}}
	manager := managerWithCredentialStore(t, credentials)
	meta := &memoryMetadataStore{
		rows:      map[string]AuthMetadata{metadataKey("p", "a"): {Source: "device"}},
		deleteErr: metaErr,
	}
	manager.SetMetadataStore(meta)
	err := manager.Logout("p", "a")
	if !errors.Is(err, metaErr) {
		t.Fatalf("want metadata error, got %v", err)
	}
}

// --- commitDeviceToken edge cases ---

func TestManager_commitDeviceToken_NilOrEmptyToken(t *testing.T) {
	cases := []struct {
		name string
		tok  *DeviceToken
	}{
		{name: "nil token", tok: nil},
		{name: "empty access_token", tok: &DeviceToken{AccessToken: ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := managerWithCredentialStore(t, &fakeCredentialStore{values: map[string]string{}})
			m.SetMetadataStore(&memoryMetadataStore{rows: map[string]AuthMetadata{}})
			err := m.commitDeviceToken("p", "a", c.tok)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestManager_commitDeviceToken_MetadataNil(t *testing.T) {
	m := managerWithCredentialStore(t, &fakeCredentialStore{values: map[string]string{}})
	err := m.commitDeviceToken("p", "a", &DeviceToken{AccessToken: "tok"})
	if err == nil {
		t.Fatal("expected error for nil metadata")
	}
}

func TestManager_commitDeviceToken_ClockNil(t *testing.T) {
	m := managerWithCredentialStore(t, &fakeCredentialStore{values: map[string]string{}})
	m.SetMetadataStore(&memoryMetadataStore{rows: map[string]AuthMetadata{}})
	m.clk = nil
	err := m.commitDeviceToken("p", "a", &DeviceToken{AccessToken: "tok"})
	if err == nil {
		t.Fatal("expected error for nil clock")
	}
}

func TestManager_commitDeviceToken_StoreNil(t *testing.T) {
	m := managerWithCredentialStore(t, &fakeCredentialStore{values: map[string]string{}})
	m.SetMetadataStore(&memoryMetadataStore{rows: map[string]AuthMetadata{}})
	m.store = nil
	err := m.commitDeviceToken("p", "a", &DeviceToken{AccessToken: "tok"})
	if err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestManager_commitDeviceToken_SnapshotError(t *testing.T) {
	snapErr := errors.New("snapshot-sentinel")
	manager := managerWithCredentialStore(t, &errorCredentialStore{getErr: snapErr})
	manager.SetMetadataStore(&memoryMetadataStore{rows: map[string]AuthMetadata{}})
	err := manager.commitDeviceToken("p", "a", &DeviceToken{AccessToken: "tok"})
	if !errors.Is(err, snapErr) {
		t.Fatalf("want snapshot error, got %v", err)
	}
}

func TestManager_commitDeviceToken_PersistError(t *testing.T) {
	setErr := errors.New("set-sentinel")
	credentials := &errorCredentialStore{}
	credentials.getErr = secrets.ErrSecretNotFound
	credentials.setErr = setErr
	manager := managerWithCredentialStore(t, credentials)
	manager.SetMetadataStore(&memoryMetadataStore{rows: map[string]AuthMetadata{}})
	err := manager.commitDeviceToken("p", "a", &DeviceToken{AccessToken: "tok"})
	if !errors.Is(err, setErr) {
		t.Fatalf("want set error, got %v", err)
	}
}

func TestManager_commitDeviceToken_HappyPath(t *testing.T) {
	credentials := &fakeCredentialStore{values: map[string]string{}}
	manager := managerWithCredentialStore(t, credentials)
	meta := &memoryMetadataStore{rows: map[string]AuthMetadata{}}
	manager.SetMetadataStore(meta)
	clk := newFakeClock(5000)
	manager.SetClock(clk)
	err := manager.commitDeviceToken("p", "a", &DeviceToken{
		AccessToken: "new-tok", ExpiresIn: 3600, TokenType: "Bearer",
	})
	if err != nil {
		t.Fatalf("commitDeviceToken: %v", err)
	}
	got, getErr := credentials.Get("p", "a")
	if getErr != nil || got != "new-tok" {
		t.Fatalf("stored: %q, %v", got, getErr)
	}
	md, metaErr := meta.LoadAuthMetadata("p", "a")
	if metaErr != nil {
		t.Fatalf("LoadAuthMetadata: %v", metaErr)
	}
	if md.Source != "device" {
		t.Fatalf("source: %q", md.Source)
	}
	wantExpiry := time.Unix(8600, 0)
	if !md.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("ExpiresAt = %v, want %v", md.ExpiresAt, wantExpiry)
	}
}

// TestManager_commitDeviceToken_NoExpiry covers the path where ExpiresIn <= 0
// so ExpiresAt should remain zero.
func TestManager_commitDeviceToken_NoExpiry(t *testing.T) {
	credentials := &fakeCredentialStore{values: map[string]string{}}
	manager := managerWithCredentialStore(t, credentials)
	meta := &memoryMetadataStore{rows: map[string]AuthMetadata{}}
	manager.SetMetadataStore(meta)
	clk := newFakeClock(1000)
	manager.SetClock(clk)
	err := manager.commitDeviceToken("p", "a", &DeviceToken{
		AccessToken: "no-expiry", ExpiresIn: 0,
	})
	if err != nil {
		t.Fatalf("commitDeviceToken: %v", err)
	}
	md, _ := meta.LoadAuthMetadata("p", "a")
	if !md.ExpiresAt.IsZero() {
		t.Fatalf("expected zero ExpiresAt, got %v", md.ExpiresAt)
	}
}

// --- ListAccounts edge cases ---

func TestManager_ListAccounts_NilStore(t *testing.T) {
	// Use "none" backend which has a nil store.
	sm, err := secrets.NewManager(secrets.Config{Backend: "none"})
	if err != nil {
		t.Fatal(err)
	}
	defer sm.Close()
	m := NewManager(sm)
	got, err := m.ListAccounts("p")
	if err != nil {
		t.Fatalf("ListAccounts with nil store: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestManager_ListAccounts_NonEnumerator(t *testing.T) {
	m := newTestManager(t)
	m.store = &nonEnumerateStore{}
	got, err := m.ListAccounts("p")
	if err != nil {
		t.Fatalf("ListAccounts with non-enumerator: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil from non-enumerator, got %v", got)
	}
}

type nonEnumerateStore struct{}

func (n *nonEnumerateStore) Available() error                { return nil }
func (n *nonEnumerateStore) Get(_, _ string) (string, error) { return "", secrets.ErrSecretNotFound }
func (n *nonEnumerateStore) Set(_, _, _ string) error        { return nil }
func (n *nonEnumerateStore) Delete(_, _ string) error        { return nil }

// --- SetClock/SetSleeper nop paths ---

func TestManager_SetClockNil(t *testing.T) {
	m := newTestManager(t)
	clk := newFakeClock(42)
	m.SetClock(clk)
	m.SetClock(nil)
	if m.clk.Now().Unix() != 42 {
		t.Fatalf("clock reset by nil: %d", m.clk.Now().Unix())
	}
}

func TestManager_SetSleeperNil(t *testing.T) {
	m := newTestManager(t)
	m.SetSleeper(nil)
	if m.slp == nil {
		t.Fatal("sleeper should not be nil after SetSleeper(nil)")
	}
}

// TestManager_Secrets_Nil covers the nil secrets branch.
func TestManager_Secrets_Nil(t *testing.T) {
	m := NewManager(nil)
	if s := m.Secrets(); s != nil {
		t.Fatal("expected nil secrets")
	}
}
