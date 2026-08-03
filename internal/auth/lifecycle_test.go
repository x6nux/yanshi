package auth

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/secrets"
)

type memoryMetadataStore struct {
	mu        sync.Mutex
	rows      map[string]AuthMetadata
	loadErr   error
	deleteErr error
}

func metadataKey(provider, account string) string {
	return provider + "\x00" + account
}

func (m *memoryMetadataStore) SaveAuthMetadata(
	provider, account string,
	meta AuthMetadata,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]AuthMetadata{}
	}
	m.rows[metadataKey(provider, account)] = meta
	return nil
}

func (m *memoryMetadataStore) LoadAuthMetadata(
	provider, account string,
) (AuthMetadata, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loadErr != nil {
		return AuthMetadata{}, m.loadErr
	}
	meta, ok := m.rows[metadataKey(provider, account)]
	if !ok {
		return AuthMetadata{}, ErrAuthMetadataNotFound
	}
	return meta, nil
}

func (m *memoryMetadataStore) DeleteAuthMetadata(provider, account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	key := metadataKey(provider, account)
	if _, ok := m.rows[key]; !ok {
		return ErrAuthMetadataNotFound
	}
	delete(m.rows, key)
	return nil
}

func TestManager_StatusUsesMetadataAndPropagatesBackendFailure(t *testing.T) {
	credentials := &fakeCredentialStore{values: map[string]string{
		credentialKey("openai", "main"): "token",
	}}
	expires := time.Unix(2222, 0).UTC()
	metadata := &memoryMetadataStore{rows: map[string]AuthMetadata{
		metadataKey("openai", "main"): {Source: "device", ExpiresAt: expires},
	}}
	manager := managerWithCredentialStore(t, credentials)
	manager.SetMetadataStore(metadata)

	got, err := manager.Status("openai", "main")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Authenticated || got.Source != "device" ||
		!got.ExpiresAt.Equal(expires) {
		t.Fatalf("status = %#v", got)
	}

	metadata.loadErr = errors.New("metadata-load-sentinel")
	if _, err := manager.Status("openai", "main"); !errors.Is(err, metadata.loadErr) {
		t.Fatalf("metadata load error swallowed: %v", err)
	}
}

func TestManager_StatusWithoutMetadataFallsBackToSecretSource(t *testing.T) {
	credentials := &fakeCredentialStore{values: map[string]string{
		credentialKey("openai", "main"): "token",
	}}
	manager := managerWithCredentialStore(t, credentials)
	manager.SetMetadataStore(&memoryMetadataStore{rows: map[string]AuthMetadata{}})
	got, err := manager.Status("openai", "main")
	if err != nil || !got.Authenticated || got.Source != "secret" ||
		!got.ExpiresAt.IsZero() {
		t.Fatalf("status = %#v err=%v", got, err)
	}
}

func TestManager_LogoutDeletesCredentialAndMetadata(t *testing.T) {
	credentials := &fakeCredentialStore{values: map[string]string{
		credentialKey("openai", "main"): "old-token",
	}}
	metadata := &memoryMetadataStore{rows: map[string]AuthMetadata{
		metadataKey("openai", "main"): {Source: "device"},
	}}
	manager := managerWithCredentialStore(t, credentials)
	manager.SetMetadataStore(metadata)

	if err := manager.Logout("openai", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.Get("openai", "main"); !errors.Is(err, secrets.ErrSecretNotFound) {
		t.Fatalf("credential survived logout: %v", err)
	}
	if _, err := metadata.LoadAuthMetadata("openai", "main"); !errors.Is(err, ErrAuthMetadataNotFound) {
		t.Fatalf("metadata survived logout: %v", err)
	}
}

func TestManager_LogoutMetadataFailureRestoresCredential(t *testing.T) {
	metadataErr := errors.New("metadata-delete-sentinel")
	credentials := &fakeCredentialStore{values: map[string]string{
		credentialKey("openai", "main"): "old-token",
	}}
	metadata := &memoryMetadataStore{
		rows: map[string]AuthMetadata{
			metadataKey("openai", "main"): {Source: "device"},
		},
		deleteErr: metadataErr,
	}
	manager := managerWithCredentialStore(t, credentials)
	manager.SetMetadataStore(metadata)

	err := manager.Logout("openai", "main")
	if !errors.Is(err, metadataErr) {
		t.Fatalf("want metadata error, got %v", err)
	}
	got, getErr := credentials.Get("openai", "main")
	if getErr != nil || got != "old-token" {
		t.Fatalf("credential not restored: %q, %v", got, getErr)
	}
}

func TestManager_LogoutRollbackFailureIsJoined(t *testing.T) {
	metadataErr := errors.New("metadata-delete-sentinel")
	rollbackErr := errors.New("credential-restore-sentinel")
	credentials := &fakeCredentialStore{
		values: map[string]string{
			credentialKey("openai", "main"): "old-token",
		},
		failSetCall: 1,
		failSetErr:  rollbackErr,
	}
	metadata := &memoryMetadataStore{
		rows: map[string]AuthMetadata{
			metadataKey("openai", "main"): {Source: "device"},
		},
		deleteErr: metadataErr,
	}
	manager := managerWithCredentialStore(t, credentials)
	manager.SetMetadataStore(metadata)

	err := manager.Logout("openai", "main")
	if !errors.Is(err, metadataErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("errors.Join lost a cause: %v", err)
	}
}

func TestManager_LogoutMissingReturnsNotFound(t *testing.T) {
	manager := managerWithCredentialStore(t,
		&fakeCredentialStore{values: map[string]string{}})
	manager.SetMetadataStore(&memoryMetadataStore{rows: map[string]AuthMetadata{}})
	if err := manager.Logout("openai", "main"); !errors.Is(err, secrets.ErrSecretNotFound) {
		t.Fatalf("missing logout = %v", err)
	}
}

// TestManager_ConcurrentAccessIsRaceFree exercises commitDeviceToken,
// Logout, and Status concurrently under -race. Requires C3's txMu and
// thread-safe fake adapters (fakeCredentialStore, memoryMetadataStore).
// Run with `go test -race -count=3` to expose timing-dependent races.
func TestManager_ConcurrentAccessIsRaceFree(t *testing.T) {
	t.Parallel() // run in parallel with other tests under -race

	credentials := &fakeCredentialStore{values: map[string]string{
		credentialKey("openai", "main"): "v1",
	}}
	metadata := &memoryMetadataStore{rows: map[string]AuthMetadata{
		metadataKey("openai", "main"): {Source: "device"},
	}}
	manager := managerWithCredentialStore(t, credentials)
	manager.SetMetadataStore(metadata)
	clk := newFakeClock(0)
	manager.SetClock(clk)
	manager.SetSleeper(&fakeSleeper{clock: clk})

	const goroutines = 16
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = manager.Status("openai", "main")
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = manager.commitDeviceToken("openai", "main", &DeviceToken{
				AccessToken: "v2",
				ExpiresIn:   3600,
				TokenType:   "Bearer",
			})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = manager.Logout("openai", "main")
		}()
	}
	wg.Wait()

	// After all goroutines complete, the metadata must be internally
	// consistent. It is legitimate for any one of the three call sequences
	// to have won, but the store and metadata must agree on a consistent
	// state.
	st, err := manager.Status("openai", "main")
	if err != nil {
		t.Fatalf("final Status: %v", err)
	}
	if st.Authenticated && st.Source != "secret" && st.Source != "device" {
		t.Fatalf("final Status has inconsistent source=%q", st.Source)
	}
}
