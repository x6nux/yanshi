// Package auth provides provider-neutral authentication lifecycle: API key
// resolution from secret:// / env:// / legacy-insecure refs, RFC 8628 device
// authorization (Task 7), and Status / Logout queries. It depends only on
// internal/secrets.
package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/x6nux/yanshi/internal/secrets"
)

// ErrResolvedCredentialEmpty is returned when a credential ref resolves to
// the empty string. The auth layer treats an empty resolved value as a hard
// error rather than a "no credential" signal: secrets.Resolve returns "" for
// Kind="none", but a provider that configured any ref expects a real value.
var ErrResolvedCredentialEmpty = errors.New("auth: resolved credential is empty")

// ErrAuthMetadataNotFound is the distinct not-found sentinel returned by
// MetadataStore.LoadAuthMetadata / DeleteAuthMetadata when no row matches.
// Keeping it separate from the SQLite/secret backend errors lets Status
// distinguish "no metadata recorded yet" (normal — fall back to Source =
// "secret") from a real backend failure (must propagate).
var ErrAuthMetadataNotFound = errors.New("auth: metadata not found")

// Status reports the current authentication state for a (provider, account).
// Source is "secret", "device", or "" depending on how the credential was
// last stored; ExpiresAt is non-zero only for device flows whose metadata
// carried an expiry. Authenticated is true iff a non-empty credential exists
// in the secret backend.
type Status struct {
	Provider      string
	Account       string
	Authenticated bool
	Source        string
	ExpiresAt     time.Time
}

// AuthMetadata contains lifecycle data safe for SQLite. It must never grow
// access-token, refresh-token, device-code, user-code, or client-secret
// fields — those stay in the secrets backend. Source is one of "secret",
// "device", or "". ExpiresAt is the wall-clock expiry computed from the
// device token's expires_in (zero value when the provider did not return
// one or for non-expiring API keys).
type AuthMetadata struct {
	Source    string
	ExpiresAt time.Time
}

// MetadataStore provides atomic per-method access to auth lifecycle
// metadata. Each Save, Load, and Delete call MUST be serialised with
// respect to other calls on the same store instance: a goroutine observing
// a nil error from SaveAuthMetadata is guaranteed that a subsequent (or
// concurrent) Load will see the new row or the old row — never a partially-
// written mix of the two. Implementations use a sync.Mutex or a SQLite
// transaction internally.
//
// SaveAuthMetadata must be a single SQLite upsert so a returned error leaves
// the previous row intact — the compensating transaction in commitDeviceToken
// relies on this. LoadAuthMetadata returns ErrAuthMetadataNotFound when no
// row matches, distinct from any backend error. DeleteAuthMetadata returns
// ErrAuthMetadataNotFound when no row was deleted so callers can detect the
// no-op without a separate Load round-trip.
type MetadataStore interface {
	SaveAuthMetadata(provider, account string, meta AuthMetadata) error
	LoadAuthMetadata(provider, account string) (AuthMetadata, error)
	DeleteAuthMetadata(provider, account string) error
}

// Manager is the auth facade. It composes a secrets.Manager (for key storage)
// with a registry of DeviceProviders, injected Clock/Sleeper (so tests drive
// device-flow timing deterministically), and an optional MetadataStore
// (SQLite-backed in production, in-memory fake in tests).
//
// txMu serializes the three transactional methods — commitDeviceToken,
// Status, Logout — so concurrent calls observe a consistent snapshot of
// store + metadata. Without it, a Logout racing a commitDeviceToken could
// observe a half-restored token; a Status racing a commit could see the new
// token but the old metadata. The lock is held for the duration of each
// method (not per-backend-call) because the compensation logic is what
// needs to be atomic.
type Manager struct {
	secrets         *secrets.Manager
	store           secrets.Store
	deviceProviders map[string]DeviceProvider
	clk             Clock
	slp             Sleeper
	metadata        MetadataStore
	txMu            sync.Mutex
}

// NewManager constructs an auth Manager with real Clock/Sleeper defaults.
// SetClock / SetSleeper / SetMetadataStore / RegisterDeviceProvider are
// called by bootstrap (production) or tests (deterministic timing).
func NewManager(sm *secrets.Manager) *Manager {
	var credentialStore secrets.Store
	if sm != nil {
		credentialStore = sm.Store()
	}
	return &Manager{
		secrets:         sm,
		store:           credentialStore,
		deviceProviders: make(map[string]DeviceProvider),
		clk:             realClock{},
		slp:             realSleeper{},
	}
}

// SetClock replaces the Manager's Clock. Pass nil to keep the current clock.
// Used by tests to drive device-flow expiry without sleeping real time.
func (m *Manager) SetClock(c Clock) {
	if c != nil {
		m.clk = c
	}
}

// SetSleeper replaces the Manager's Sleeper. Pass nil to keep the current
// sleeper. Used by tests to advance a fakeClock deterministically.
func (m *Manager) SetSleeper(s Sleeper) {
	if s != nil {
		m.slp = s
	}
}

// SetMetadataStore attaches the SQLite-backed (or fake) MetadataStore.
// commitDeviceToken requires a non-nil metadata store; without one it
// refuses to commit, which is the fail-closed posture for partial writes.
func (m *Manager) SetMetadataStore(store MetadataStore) {
	m.metadata = store
}

// RegisterDeviceProvider adds dp to the registry under id. The id matches
// CredentialSource.DeviceProviderID so a provider's credential source can
// opt into device flow by name.
func (m *Manager) RegisterDeviceProvider(id string, dp DeviceProvider) {
	m.deviceProviders[id] = dp
}

// safeDurationFromSeconds converts an int seconds value to time.Duration
// without overflow. Negative or zero returns 0. When seconds * time.Second
// would overflow int64, the maximum representable time.Duration is returned
// (~292 years), so callers can still Add() without wrapping around. Used by
// the OAuth device flow where expires_in / interval arrive from untrusted
// HTTP responses and could otherwise cause int64 nanosecond overflow.
func safeDurationFromSeconds(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	s := int64(seconds)
	if s > math.MaxInt64/int64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(s) * time.Second
}

// RunDeviceFlow triggers an RFC 8628 flow for the registered provider id and
// stores the resulting token under (providerID, account) via the compensating
// commitDeviceToken path. stdout receives fixed progress lines per stage;
// pass io.Discard to silence. Returns the device token on success (the same
// value commitDeviceToken already persisted, so callers can inspect without
// re-reading the store).
//
// Errors:
//   - unknown provider id
//   - any Authorize error (fixed bounded strings; no device_code leak)
//   - any commitDeviceToken error (metadata + rollback causes joined)
//
// Progress messages are deliberately fixed strings — they MUST NOT echo the
// device_code, access_token, refresh_token, or any value the provider
// returned. The redactor registers all three secrets before commit so even
// the rollback path cannot leak them.
func (m *Manager) RunDeviceFlow(
	ctx context.Context,
	providerID, account string,
	stdout io.Writer,
) (*DeviceToken, error) {
	dp, ok := m.deviceProviders[providerID]
	if !ok {
		return nil, fmt.Errorf("auth: no device provider registered")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	tok, err := dp.Authorize(ctx, m.clk, m.slp, func(st StatusUpdate) {
		switch st.Stage {
		case "device_code_issued":
			fmt.Fprintf(stdout, "Visit %s and enter code: %s\n", st.VerificationURI, st.UserCode)
		case "polling":
			fmt.Fprintln(stdout, "polling...")
		case "slow_down":
			fmt.Fprintln(stdout, "slowing down...")
		case "success":
			// Do not print success yet: token + metadata commit is still pending.
		}
	})
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, errors.New("auth: device provider returned no token")
	}
	if m.secrets != nil {
		if tok.AccessToken != "" {
			m.secrets.Redactor().Register(tok.AccessToken)
		}
		if tok.RefreshToken != "" {
			m.secrets.Redactor().Register(tok.RefreshToken)
		}
	}
	if err := m.commitDeviceToken(providerID, account, tok); err != nil {
		return nil, err
	}
	fmt.Fprintln(stdout, "authenticated")
	return tok, nil
}

// Secrets returns the underlying secrets.Manager (for bootstrap to inject
// the resolved redactor and for CLI auth to call Set/Delete).
func (m *Manager) Secrets() *secrets.Manager { return m.secrets }

// Status queries the secret backend for an entry and overlays metadata when
// available. Authenticated is true iff a non-empty credential exists. Source
// defaults to "secret" when no metadata row exists, and is overwritten by
// the stored Source (typically "device") when the metadata adapter returns
// one. ExpiresAt is non-zero only for device flows whose metadata carried
// an expiry. A real backend error from either adapter propagates — only
// ErrSecretNotFound / ErrAuthMetadataNotFound are treated as "no record".
func (m *Manager) Status(provider, account string) (Status, error) {
	m.txMu.Lock()
	defer m.txMu.Unlock()
	base := Status{Provider: provider, Account: account}
	if m.store == nil {
		return base, nil
	}
	value, err := m.store.Get(provider, account)
	if errors.Is(err, secrets.ErrSecretNotFound) {
		return base, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("auth: query credential status: %w", err)
	}
	if value == "" {
		return base, nil
	}
	base.Authenticated = true
	base.Source = "secret"
	if m.metadata == nil {
		return base, nil
	}
	meta, err := m.metadata.LoadAuthMetadata(provider, account)
	if errors.Is(err, ErrAuthMetadataNotFound) {
		return base, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("auth: load metadata: %w", err)
	}
	base.Source = meta.Source
	base.ExpiresAt = meta.ExpiresAt
	return base, nil
}

// Logout is a compensating transaction across the secret backend and the
// metadata adapter. It snapshots both rows first, deletes the credential,
// then deletes the metadata. A metadata-delete failure restores the old
// credential (so a half-deleted state never leaves a stale token behind),
// and a restore failure is joined with errors.Join so neither cause is lost.
// If neither adapter has a row, Logout returns secrets.ErrSecretNotFound —
// callers can distinguish "nothing to delete" from "backend failed".
func (m *Manager) Logout(provider, account string) error {
	m.txMu.Lock()
	defer m.txMu.Unlock()
	if m.store == nil {
		return errors.New("auth: no secret backend configured")
	}
	oldToken, tokenErr := m.store.Get(provider, account)
	hadToken := tokenErr == nil
	if tokenErr != nil && !errors.Is(tokenErr, secrets.ErrSecretNotFound) {
		return fmt.Errorf("auth: snapshot credential: %w", tokenErr)
	}

	hadMeta := false
	if m.metadata != nil {
		_, err := m.metadata.LoadAuthMetadata(provider, account)
		hadMeta = err == nil
		if err != nil && !errors.Is(err, ErrAuthMetadataNotFound) {
			return fmt.Errorf("auth: snapshot metadata: %w", err)
		}
	}
	if !hadToken && !hadMeta {
		return secrets.ErrSecretNotFound
	}

	if hadToken {
		if err := m.store.Delete(provider, account); err != nil {
			return fmt.Errorf("auth: delete credential: %w", err)
		}
	}
	if !hadMeta {
		return nil
	}
	if err := m.metadata.DeleteAuthMetadata(provider, account); err != nil {
		base := fmt.Errorf("auth: delete metadata: %w", err)
		if !hadToken {
			// Nothing to restore; the credential was already absent.
			return base
		}
		if restoreErr := m.store.Set(provider, account, oldToken); restoreErr != nil {
			return errors.Join(base,
				fmt.Errorf("auth: restore credential: %w", restoreErr))
		}
		return base
	}
	return nil
}

// ListAccounts returns the unique set of accounts stored under provider.
// Implementation: scan the fileStore entries (CGO-enabled keyring backends
// may not support enumeration — those backends return an empty list with a
// nil error; callers must not treat empty as "fully scanned").
func (m *Manager) ListAccounts(provider string) ([]string, error) {
	type enumerator interface {
		Enumerate(prefix string) ([]string, error)
	}
	store := m.store
	if store == nil {
		return nil, nil
	}
	enum, ok := store.(enumerator)
	if !ok {
		return nil, nil
	}
	return enum.Enumerate(provider)
}

// commitDeviceToken is a compensating transaction across the secret backend
// and SQLite metadata. It snapshots an existing credential before replacement:
// a metadata failure restores that old token; only a newly-created credential
// is deleted. Compensation failures are joined so callers never lose evidence
// of a partial rollback. The redactor registers the new access/refresh tokens
// before they are persisted so the rollback path cannot leak them either.
//
// Task 14 renames this to commitDeviceTokenWithTx and adds Status / Logout
// compensation paths over the same SQLite transaction. The signature here is
// the Task 7 baseline.
func (m *Manager) commitDeviceToken(
	provider, account string,
	tok *DeviceToken,
) error {
	m.txMu.Lock()
	defer m.txMu.Unlock()
	if tok == nil || tok.AccessToken == "" {
		return errors.New("auth: empty device token refused")
	}
	if m.metadata == nil {
		return errors.New("auth: metadata store is not configured")
	}
	if m.clk == nil {
		return errors.New("auth: clock is not configured")
	}
	credentialStore := m.store
	if credentialStore == nil {
		return errors.New("auth: secret store is not configured")
	}

	oldToken, snapshotErr := credentialStore.Get(provider, account)
	hadOldToken := snapshotErr == nil
	if snapshotErr != nil && !errors.Is(snapshotErr, secrets.ErrSecretNotFound) {
		return fmt.Errorf("auth: snapshot existing device token: %w", snapshotErr)
	}

	if m.secrets != nil {
		m.secrets.Redactor().Register(tok.AccessToken)
		if tok.RefreshToken != "" {
			m.secrets.Redactor().Register(tok.RefreshToken)
		}
	}
	if err := credentialStore.Set(provider, account, tok.AccessToken); err != nil {
		return fmt.Errorf("auth: persist device token: %w", err)
	}

	expiresAt := time.Time{}
	if tok.ExpiresIn > 0 {
		expiresAt = m.clk.Now().Add(safeDurationFromSeconds(tok.ExpiresIn))
	}
	metadataErr := m.metadata.SaveAuthMetadata(
		provider,
		account,
		AuthMetadata{Source: "device", ExpiresAt: expiresAt},
	)
	if metadataErr == nil {
		return nil
	}

	var rollbackErr error
	if hadOldToken {
		rollbackErr = credentialStore.Set(provider, account, oldToken)
	} else {
		rollbackErr = credentialStore.Delete(provider, account)
	}
	base := fmt.Errorf("auth: persist device metadata: %w", metadataErr)
	if rollbackErr != nil {
		return errors.Join(base, fmt.Errorf("auth: compensate device token: %w", rollbackErr))
	}
	return base
}
