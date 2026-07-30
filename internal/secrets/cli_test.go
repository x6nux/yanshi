package secrets

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TestMainAuth exercises the complete CLI auth flow on ONE provider/account
// (per structural fix #13: unique test provider/account, no sweep across
// multiple). Covers: stdin prompt, --api-key-stdin, cleanup-failure path.
func TestMainAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	t.Setenv("YANSHI_TEST_PASS", "test-passphrase")

	// 1) Interactive stdin: provide API key via simulated tty input.
	in := &bytes.Buffer{}
	in.WriteString("sk-test-from-stdin\n")
	out := &bytes.Buffer{}
	cmd := AuthCommand{
		Provider:   "openai",
		Account:    "main",
		Backend:    "file",
		FilePath:   path,
		Stdin:      in,
		Stdout:     out,
		Passphrase: []byte("test-passphrase"),
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run interactive: %v", err)
	}
	mgr, _ := NewManager(Config{Backend: "file", FilePath: path, PassphraseEnv: "YANSHI_TEST_PASS"})
	defer func() { _ = mgr.Close() }()
	got, err := mgr.Store().Get("openai", "main")
	if err != nil {
		t.Fatalf("Get after set: %v", err)
	}
	if got != "sk-test-from-stdin" {
		t.Fatalf("got %q", got)
	}

	// 2) --api-key-stdin: read secret from raw stdin bytes (no prompt).
	raw := &bytes.Buffer{}
	raw.WriteString("sk-from-stdin-flag")
	out2 := &bytes.Buffer{}
	cmd2 := AuthCommand{
		Provider:    "openai",
		Account:     "main",
		Backend:     "file",
		FilePath:    path,
		APIKeyStdin: true,
		Stdin:       raw,
		Stdout:      out2,
		Passphrase:  []byte("test-passphrase"),
	}
	if err := cmd2.Run(); err != nil {
		t.Fatalf("Run api-key-stdin: %v", err)
	}
	// Each command owns a FileStore snapshot; reopen after cmd2 commits so the
	// assertion observes the newly encrypted on-disk state rather than mgr's
	// intentionally stale in-memory snapshot.
	if err := mgr.Close(); err != nil {
		t.Fatal(err)
	}
	mgr, err = NewManager(Config{
		Backend:       "file",
		FilePath:      path,
		PassphraseEnv: "YANSHI_TEST_PASS",
	})
	if err != nil {
		t.Fatal(err)
	}
	got2, err := mgr.Store().Get("openai", "main")
	if err != nil {
		t.Fatal(err)
	}
	if got2 != "sk-from-stdin-flag" {
		t.Fatalf("got %q after --api-key-stdin", got2)
	}

	// 3) Cleanup-failure path: --delete on a non-existent account must
	// execute the cleanup branch and return a non-nil error (recorded in
	// stderr) WITHOUT corrupting the existing entry.
	out3 := &bytes.Buffer{}
	cmd3 := AuthCommand{
		Provider:   "openai",
		Account:    "nonexistent",
		Backend:    "file",
		FilePath:   path,
		Delete:     true,
		Stdout:     out3,
		Passphrase: []byte("test-passphrase"),
	}
	err = cmd3.Run()
	if err == nil {
		t.Fatal("delete on missing account must return error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("delete error must be informative: %v", err)
	}
	// Existing entry must still be intact.
	got3, _ := mgr.Store().Get("openai", "main")
	if got3 != "sk-from-stdin-flag" {
		t.Fatalf("existing entry corrupted after failed delete: %q", got3)
	}
}

func TestAuthCommand_RejectsOversizedSecretInput(t *testing.T) {
	for _, tc := range []struct {
		name        string
		apiKeyStdin bool
		suffix      string
	}{
		{"api-key-stdin", true, ""},
		{"interactive-reader", false, "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := AuthCommand{
				Provider:    "openai",
				Account:     "main",
				Backend:     "file",
				FilePath:    filepath.Join(t.TempDir(), "secrets.enc"),
				Passphrase:  []byte("test-passphrase"),
				APIKeyStdin: tc.apiKeyStdin,
				Stdin: bytes.NewBufferString(
					strings.Repeat("x", maxSecretInputBytes+1) + tc.suffix,
				),
				Stdout: &bytes.Buffer{},
			}
			err := cmd.Run()
			if err == nil || !strings.Contains(err.Error(), "exceeds 65536 bytes") {
				t.Fatalf("want bounded-input error, got %v", err)
			}
		})
	}
}
