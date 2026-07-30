package secrets

import (
	"path/filepath"
	"testing"
)

func TestManager_ForcedFileRoundTrip(t *testing.T) {
	// Force FileStore by pointing backend at "file" with a passphrase.
	t.Setenv("YANSHI_TEST_PASS", "test-passphrase")
	path := filepath.Join(t.TempDir(), "secrets.enc")
	mgr, err := NewManager(Config{
		Backend:       "file",
		FilePath:      path,
		PassphraseEnv: "YANSHI_TEST_PASS",
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()

	if err := mgr.Set("openai", "main", "sk-from-file"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := mgr.Store().Get("openai", "main")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-from-file" {
		t.Fatalf("got %q", got)
	}
}

func TestManager_NoBackendRefusesSecretRef(t *testing.T) {
	mgr, err := NewManager(Config{Backend: "none"})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer mgr.Close()
	_, err = mgr.Resolve(CredentialRef{Kind: "secret", Service: "x", Account: "y"})
	if err == nil {
		t.Fatal("expected error resolving secret:// with no backend")
	}
}

func TestManager_LegacyLiteralPassesThroughWhenAllowed(t *testing.T) {
	mgr, _ := NewManager(Config{Backend: "none"})
	defer mgr.Close()
	got, err := mgr.Resolve(CredentialRef{Kind: "legacy", Raw: "sk-legacy"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "sk-legacy" {
		t.Fatalf("got %q", got)
	}
}

func TestManager_AutoWithoutPassphraseSkipsFileStore(t *testing.T) {
	// Backend=auto but no passphrase set: must not fatal, only warn.
	// t.Setenv is the only safe test env mutation on Windows; use a unique
	// name we know is not exported by the test runner. Do not call os.Unsetenv.
	path := filepath.Join(t.TempDir(), "secrets.enc")
	mgr, err := NewManager(Config{
		Backend:       "auto",
		FilePath:      path,
		PassphraseEnv: "YANSHI_TEST_PASS_AUTO_ABSENT",
	})
	if err != nil {
		t.Fatalf("auto mode must not fatal without passphrase: %v", err)
	}
	defer mgr.Close()
	// secret:// must fail (no keyring, no fileStore); env:// and legacy must still work.
	if _, err := mgr.Resolve(CredentialRef{Kind: "secret", Service: "x", Account: "y"}); err == nil {
		t.Fatal("secret:// must fail when no backend available")
	}
}
