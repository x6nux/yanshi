package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurity_NoPlaintextInEncryptedFile asserts the on-disk FileStore
// format never contains a registered secret in plaintext. AES-256-GCM with
// Argon2id-derived keys is the load-bearing control; this test is the
// last-line regression catch if a future refactor accidentally writes the
// plaintext payload before encrypting.
func TestSecurity_NoPlaintextInEncryptedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	fs, err := NewFileStore(path, []byte("strong-test-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	secret := "sk-security-gate-1234567890"
	if err := fs.Set("openai", "main", secret); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("encrypted file contains plaintext API key")
	}
}

// TestSecurity_RawLiteralFailsClosedWithoutLegacyOptIn asserts S10's
// fail-closed posture: a raw API key pasted into config silently working
// would defeat the entire threat model, so ParseCredentialRef must reject
// it unless the caller explicitly opted into legacy-insecure.
func TestSecurity_RawLiteralFailsClosedWithoutLegacyOptIn(t *testing.T) {
	if _, err := ParseCredentialRef("sk-raw-live-key", false); err == nil {
		t.Fatal("raw literal accepted without legacy-insecure opt-in")
	}
}
