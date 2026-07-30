//go:build nokeyring

package secrets

// noKeyringStore is the soft-degrade stub for `-tags nokeyring` builds. Every
// call returns ErrKeyringUnavailable; the Manager then falls back to fileStore
// if a passphrase is configured, or refuses secret:// refs (text values still
// resolve). This MUST NOT panic or return a different error type.
type noKeyringStore struct{}

func newOSKeyringStoreImpl() Store { return noKeyringStore{} }

func (noKeyringStore) Available() error                   { return ErrKeyringUnavailable }
func (noKeyringStore) Get(string, string) (string, error) { return "", ErrKeyringUnavailable }
func (noKeyringStore) Set(string, string, string) error   { return ErrKeyringUnavailable }
func (noKeyringStore) Delete(string, string) error        { return ErrKeyringUnavailable }
