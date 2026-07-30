package secrets

import (
	"errors"
	"testing"
)

// TestKeyring_FailsGracefullyWhenUnavailable exercises the soft-degrade path:
// when the OS keyring is not present (CI without D-Bus, `-tags nokeyring`
// build), the adapter's Available probe AND its Get must return
// ErrKeyringUnavailable (not a panic, not a generic error, and NOT
// ErrSecretNotFound — a missing entry is a different condition that the
// Manager must be able to distinguish from a missing backend).
func TestKeyring_FailsGracefullyWhenUnavailable(t *testing.T) {
	s := NewOSKeyringStore()
	// Available() probe: distinguishes "no backend" from "missing entry".
	if err := s.Available(); err == nil {
		t.Skip("OS keyring IS available on this host; soft-degrade path skipped")
	} else if !errors.Is(err, ErrKeyringUnavailable) {
		// Any other error means the keyring backend answered (so it exists
		// but is locked / errored). That's a different test path; skip here.
		t.Skipf("keyring returned non-unavailable error; backend is present: %v", err)
	}
	// Backend unavailable: Get must also report unavailable, NOT not-found.
	if _, err := s.Get("any", "any"); !errors.Is(err, ErrKeyringUnavailable) {
		t.Fatalf("Get on unavailable backend: want ErrKeyringUnavailable, got %v", err)
	}
}

// TestKeyring_MissingEntryReturnsSecretNotFound verifies the OTHER direction
// of the availability contract: when the backend is reachable but the entry
// is absent, Get must return ErrSecretNotFound (not ErrKeyringUnavailable).
// This keeps Manager.auto from mis-classifying a cold-cache lookup as a
// backend failure and falling back to fileStore when it should not.
func TestKeyring_MissingEntryReturnsSecretNotFound(t *testing.T) {
	s := NewOSKeyringStore()
	if err := s.Available(); err != nil {
		t.Skipf("keyring backend unavailable on this host; skipping: %v", err)
	}
	if _, err := s.Get("__definitely_not_stored__", "__test__"); err != ErrSecretNotFound {
		t.Fatalf("missing entry: want ErrSecretNotFound, got %v", err)
	}
}
