// Package secrets — OS keyring adapter. The implementation is split by build
// tag so a `-tags nokeyring` build (used for static release binaries and CI
// without a keyring daemon) gets the stub-only fallback that always returns
// ErrKeyringUnavailable, while the default build gets the real platform
// keyring call. cgo / non-cgo is orthogonal: zalando/go-keyring is pure Go.

package secrets

// Store is the credential storage adapter. Implementations:
//   - osKeyringStore: backed by OS keyring (zalando/go-keyring)
//   - fileStore: AES-256-GCM encrypted local file (Argon2id KDF)
//   - noKeyringStore: stub returned when -tags nokeyring is active
type Store interface {
	Available() error
	Get(service, account string) (string, error)
	Set(service, account, secret string) error
	Delete(service, account string) error
}

// Available reports whether this concrete backend can be used. It is NOT the
// same as "an entry exists": a missing entry returns ErrSecretNotFound from
// Get, while a missing *backend* (no D-Bus, keyring locked, no session, ...)
// returns ErrKeyringUnavailable. FileStore always returns nil once opened.
// Manager uses this explicit port instead of inferring backend health from a
// sentinel Get result.

// NewOSKeyringStore returns the OS keyring Store. The default build returns a
// real keyring-backed store; the nokeyring build returns a stub. This
// indirection keeps the public API stable across build configurations.
func NewOSKeyringStore() Store {
	return newOSKeyringStoreImpl()
}
