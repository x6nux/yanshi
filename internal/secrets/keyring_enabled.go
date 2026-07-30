//go:build !nokeyring

package secrets

import (
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

type osKeyringStore struct{}

func newOSKeyringStoreImpl() Store { return &osKeyringStore{} }

// Available reports whether the OS keyring is usable on this host. zalando/
// go-keyring does not expose a probe directly, so we issue a Get against a
// sentinel (service, account) pair: ErrNotFound means the backend works (it
// answered with a real "not found" response), any other non-nil error means
// the backend is unavailable (no D-Bus, locked keyring, platform error).
func (osKeyringStore) Available() error {
	if _, err := keyring.Get("__yanshi_probe__", "__yanshi_probe__"); err == nil {
		return nil
	} else if errors.Is(err, keyring.ErrNotFound) {
		return nil
	} else {
		return fmt.Errorf("%w: %v", ErrKeyringUnavailable, err)
	}
}

func (osKeyringStore) Get(service, account string) (string, error) {
	s, err := keyring.Get(service, account)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", ErrSecretNotFound
		}
		return "", fmt.Errorf("%w: get failed: %v", ErrKeyringUnavailable, err)
	}
	return s, nil
}

func (osKeyringStore) Set(service, account, secret string) error {
	if err := keyring.Set(service, account, secret); err != nil {
		return fmt.Errorf("%w: set failed: %v", ErrKeyringUnavailable, err)
	}
	return nil
}

func (osKeyringStore) Delete(service, account string) error {
	err := keyring.Delete(service, account)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, keyring.ErrNotFound):
		return ErrSecretNotFound
	default:
		return fmt.Errorf("%w: delete failed: %v", ErrKeyringUnavailable, err)
	}
}
