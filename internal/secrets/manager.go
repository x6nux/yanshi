package secrets

import (
	"fmt"
	"io"
	"os"
)

// Manager is the unified credential resolver. It owns a Store (keyring,
// file, or none) and resolves CredentialRefs to plaintext secrets. The
// Manager itself never holds plaintext secrets beyond the Resolve return
// value; callers are responsible for registering them with the Redactor.
type Manager struct {
	cfg    Config
	store  Store
	redact *Redactor
	warn   *SafeLogger
}

// Config is the manager-level view of secrets configuration.
type Config struct {
	Backend       string
	FilePath      string
	PassphraseEnv string
	// Passphrase is a runtime-only injection used by CLI/tests. It is never
	// serialized and takes precedence over PassphraseEnv when non-empty.
	Passphrase []byte
	// Stderr is the optional warning sink. NewManager always wraps it in a
	// SafeLogger using the same Redactor owned by the Manager.
	Stderr io.Writer
}

// newKeyringStore is the seam over NewOSKeyringStore. The default delegates to
// the real factory; tests swap it to drive the Manager's auto/keyring backend
// selection with a fake Store (the real OS keyring is integration-only). This
// mirrors the replaceEncryptedFile seam already used by file_store tests.
var newKeyringStore = NewOSKeyringStore

// NewManager constructs a Manager per Backend:
//   - "auto": try OS keyring; if unavailable and passphrase is set, use
//     fileStore; otherwise return a Manager with store=nil (secret:// refs
//     will fail, but env:// and legacy still work). Soft-degrade: never fatals.
//   - "keyring": force OS keyring; if unavailable, return error.
//   - "file": force fileStore; require passphrase.
//   - "none": store stays nil; only env:// and legacy resolve.
func NewManager(cfg Config) (*Manager, error) {
	m := &Manager{cfg: cfg, redact: NewRedactor()}
	if cfg.Stderr != nil {
		m.warn = NewSafeLogger(cfg.Stderr, m.redact)
	}
	switch cfg.Backend {
	case "", "auto":
		ks := newKeyringStore()
		// Use Available() probe so "backend not installed" stays distinct
		// from "entry missing". A backend that answers "not found" to a
		// probe Get IS available; the old logic confused the two and fell
		// back to fileStore on every cold-cache lookup.
		if err := ks.Available(); err == nil {
			m.store = ks
			break
		}
		// Keyring unavailable: try fileStore if passphrase configured.
		pass := m.passphrase()
		if len(pass) > 0 && cfg.FilePath != "" {
			fs, ferr := NewFileStore(cfg.FilePath, pass)
			if ferr != nil {
				if m.warn != nil {
					m.warn.Printf("warn: keyring unavailable and fileStore failed: %v (secret:// refs will not resolve)", ferr)
				}
			} else {
				m.store = fs
			}
		} else if m.warn != nil {
			m.warn.Println("warn: keyring unavailable and no passphrase configured; secret:// refs will not resolve")
		}
	case "keyring":
		ks := newKeyringStore()
		if err := ks.Available(); err != nil {
			return nil, fmt.Errorf("secrets: backend=keyring but OS keyring unavailable: %w", err)
		}
		m.store = ks
	case "file":
		pass := m.passphrase()
		if len(pass) == 0 {
			return nil, fmt.Errorf("secrets: backend=file but no passphrase configured")
		}
		fs, err := NewFileStore(cfg.FilePath, pass)
		if err != nil {
			return nil, err
		}
		m.store = fs
	case "none":
		// store stays nil
	default:
		return nil, fmt.Errorf("secrets: unknown backend %q", cfg.Backend)
	}
	return m, nil
}

func (m *Manager) passphrase() []byte {
	if len(m.cfg.Passphrase) > 0 {
		return append([]byte(nil), m.cfg.Passphrase...)
	}
	if m.cfg.PassphraseEnv == "" {
		return nil
	}
	return []byte(os.Getenv(m.cfg.PassphraseEnv))
}

// Store returns the underlying Store (may be nil in "none" or soft-degrade).
// Exposed so the CLI can call Set/Delete directly.
func (m *Manager) Store() Store { return m.store }

// Redactor returns the Manager's redactor so callers can register resolved
// secrets in one place.
func (m *Manager) Redactor() *Redactor { return m.redact }

// Set stores a secret in the configured backend. Used by the CLI auth flow.
func (m *Manager) Set(service, account, secret string) error {
	if m.store == nil {
		return fmt.Errorf("secrets: no backend configured")
	}
	return m.store.Set(service, account, secret)
}

// Delete removes a secret. Errors from the underlying store (including
// "not found") are returned to the caller so the CLI can format them.
func (m *Manager) Delete(service, account string) error {
	if m.store == nil {
		return fmt.Errorf("secrets: no backend configured")
	}
	return m.store.Delete(service, account)
}

// Close is a no-op for current backends; exists for future keyed backends.
func (m *Manager) Close() error { return nil }
