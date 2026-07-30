package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

// errReader is a minimal io.Reader used to exercise error paths in the CLI
// read helpers. It returns a configurable error on the first Read.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// fakeStore is a controllable Store used to drive Manager mode-selection and
// the CLI without touching the real OS keyring. It records calls so tests can
// assert behavior.
type fakeStore struct {
	avail error
	items map[string]string // "service\x00account" -> secret
	setOK bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{items: map[string]string{}, setOK: true}
}

func (f *fakeStore) Available() error { return f.avail }
func (f *fakeStore) Get(svc, acct string) (string, error) {
	if f.avail != nil {
		return "", f.avail
	}
	v, ok := f.items[svc+"\x00"+acct]
	if !ok {
		return "", ErrSecretNotFound
	}
	return v, nil
}
func (f *fakeStore) Set(svc, acct, secret string) error {
	if !f.setOK {
		return errors.New("fake: set disabled")
	}
	f.items[svc+"\x00"+acct] = secret
	return nil
}
func (f *fakeStore) Delete(svc, acct string) error {
	k := svc + "\x00" + acct
	if _, ok := f.items[k]; !ok {
		return ErrSecretNotFound
	}
	delete(f.items, k)
	return nil
}

// withFakeKeyring swaps the newKeyringStore seam for the duration of the test.
func withFakeKeyring(t *testing.T, s Store) {
	t.Helper()
	orig := newKeyringStore
	newKeyringStore = func() Store { return s }
	t.Cleanup(func() { newKeyringStore = orig })
}

// ---------------------------------------------------------------------------
// secrets.go
// ---------------------------------------------------------------------------

func TestParseCredentialRef_EdgeCases(t *testing.T) {
	// Empty string → none ref (no credential configured).
	ref, err := ParseCredentialRef("", false)
	if err != nil || ref.Kind != "none" {
		t.Fatalf("empty parse = %+v, %v", ref, err)
	}
	// Malformed secret:// refs must fail (each missing service or account).
	for _, bad := range []string{"secret://svc", "secret:///acct", "secret:///", "secret://a/"} {
		if _, err := ParseCredentialRef(bad, false); err == nil {
			t.Fatalf("ParseCredentialRef(%q) must error", bad)
		}
	}
	// Malformed env:// must fail.
	if _, err := ParseCredentialRef("env://", false); err == nil {
		t.Fatal("ParseCredentialRef(env://) must error")
	}
}

func TestRedactor_RegisterEmptyAndIdempotent(t *testing.T) {
	r := NewRedactor()
	r.Register("") // empty ignored
	if r.Redact("nothing-here") != "nothing-here" {
		t.Fatal("empty registration must not redact anything")
	}
	r.Register("sk-1")
	r.Register("sk-1") // duplicate ignored
	r.Register("sk-2")
	got := r.Redact("sk-1 and sk-2")
	if got != "[REDACTED] and [REDACTED]" {
		t.Fatalf("redact = %q", got)
	}
}

func TestRedactor_RedactNoSecretsReturnsInput(t *testing.T) {
	r := NewRedactor()
	if got := r.Redact("plain-text"); got != "plain-text" {
		t.Fatalf("empty redactor changed input: %q", got)
	}
}

func TestRedactor_SafeErrorNilPassesThrough(t *testing.T) {
	r := NewRedactor()
	if err := r.SafeError(nil); err != nil {
		t.Fatalf("SafeError(nil) = %v, want nil", err)
	}
}

// TestRedactJSON covers both the plain and the JSON-escaped spelling. A secret
// containing a quote/backslash is missed by a naive ReplaceAll after json.Marshal
// escapes those characters — RedactJSON must catch both forms.
func TestRedactJSON(t *testing.T) {
	r := NewRedactor()
	secret := `abc"def\ghi`
	r.Register(secret)
	// json.Marshal escapes the quote and backslash; both spellings appear.
	payload, _ := json.Marshal(map[string]string{"k": secret})
	redacted := r.RedactJSON(payload)
	if strings.Contains(string(redacted), secret) {
		t.Fatalf("RedactJSON leaked raw secret: %q", redacted)
	}
	if strings.Contains(string(redacted), `abc\"def`) { // escaped spelling
		t.Fatalf("RedactJSON leaked escaped secret: %q", redacted)
	}
	if !strings.Contains(string(redacted), "[REDACTED]") {
		t.Fatalf("RedactJSON missing marker: %q", redacted)
	}
	// Empty redactor returns input unchanged.
	if got := NewRedactor().RedactJSON([]byte(`{"x":"y"}`)); string(got) != `{"x":"y"}` {
		t.Fatalf("empty RedactJSON changed input: %q", got)
	}
}

func TestSafeLogger_Println(t *testing.T) {
	var out strings.Builder
	r := NewRedactor()
	r.Register("sk-secret-999")
	log := NewSafeLogger(&out, r)
	log.Println("leaked", "sk-secret-999")
	if strings.Contains(out.String(), "sk-secret-999") {
		t.Fatalf("Println leaked secret: %q", out.String())
	}
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("Println must append trailing newline: %q", out.String())
	}
}

func TestMergeRedactors(t *testing.T) {
	a := NewRedactor()
	a.Register("sk-a")
	b := NewRedactor()
	b.Register("sk-b")
	merged := MergeRedactors(a, b, nil) // nil input must be skipped
	got := merged.Redact("sk-a sk-b")
	if strings.Contains(got, "sk-a") || strings.Contains(got, "sk-b") {
		t.Fatalf("MergeRedactors leaked: %q", got)
	}
	// Merging nothing yields an empty redactor.
	if got := MergeRedactors().Redact("x"); got != "x" {
		t.Fatalf("empty merge changed input: %q", got)
	}
}

func TestNewSafeOutput(t *testing.T) {
	// nil r → fresh empty Redactor; nil out → io.Discard.
	so := NewSafeOutput(nil, nil)
	if so.Redactor == nil {
		t.Fatal("nil r must yield a fresh non-nil Redactor")
	}
	// Registering on the Redactor must be observed by the shared Logger.
	so.Redactor.Register("sk-so-secret")
	so.Logger.Printf("echo sk-so-secret") // writes to io.Discard, must not panic
	// A real writer receives redacted output and shares the same registry.
	var out strings.Builder
	so2 := NewSafeOutput(&out, nil)
	so2.Redactor.Register("sk-shared")
	so2.Logger.Printf("got sk-shared here")
	if strings.Contains(out.String(), "sk-shared") {
		t.Fatalf("SafeOutput logger leaked: %q", out.String())
	}
}

// ---------------------------------------------------------------------------
// manager.go
// ---------------------------------------------------------------------------

func TestManager_NewManagerAllModes(t *testing.T) {
	t.Run("stderr-logger-wired", func(t *testing.T) {
		var stderr bytes.Buffer
		mgr, err := NewManager(Config{Backend: "none", Stderr: &stderr})
		if err != nil {
			t.Fatal(err)
		}
		defer mgr.Close()
		if mgr.Redactor() == nil {
			t.Fatal("Redactor() must be non-nil")
		}
	})
	t.Run("unknown-backend-errors", func(t *testing.T) {
		if _, err := NewManager(Config{Backend: "bogus"}); err == nil {
			t.Fatal("unknown backend must error")
		}
	})
	t.Run("file-no-passphrase-errors", func(t *testing.T) {
		if _, err := NewManager(Config{Backend: "file", FilePath: filepath.Join(t.TempDir(), "x.enc")}); err == nil {
			t.Fatal("file backend without passphrase must error")
		}
	})
	t.Run("file-store-open-error-propagates", func(t *testing.T) {
		// FilePath parent is a regular file → MkdirAll fails inside NewFileStore.
		parent := filepath.Join(t.TempDir(), "regularfile")
		if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := NewManager(Config{Backend: "file", FilePath: filepath.Join(parent, "x.enc"), Passphrase: []byte("p")})
		if err == nil {
			t.Fatal("file backend with unopenable path must error")
		}
	})
	t.Run("keyring-unavailable-errors", func(t *testing.T) {
		withFakeKeyring(t, &fakeStore{avail: ErrKeyringUnavailable})
		if _, err := NewManager(Config{Backend: "keyring"}); err == nil {
			t.Fatal("keyring backend must error when keyring unavailable")
		}
	})
	t.Run("keyring-available-wires-store", func(t *testing.T) {
		fs := newFakeStore()
		withFakeKeyring(t, fs)
		mgr, err := NewManager(Config{Backend: "keyring"})
		if err != nil {
			t.Fatalf("keyring backend with available store: %v", err)
		}
		defer mgr.Close()
		if mgr.Store() == nil {
			t.Fatal("available keyring must be wired as the store")
		}
	})
	t.Run("auto-falls-back-to-file-when-keyring-unavailable", func(t *testing.T) {
		withFakeKeyring(t, &fakeStore{avail: ErrKeyringUnavailable})
		path := filepath.Join(t.TempDir(), "secrets.enc")
		mgr, err := NewManager(Config{Backend: "auto", FilePath: path, Passphrase: []byte("p")})
		if err != nil {
			t.Fatalf("auto soft-degrade must not fatal: %v", err)
		}
		defer mgr.Close()
		if _, ok := mgr.Store().(*FileStore); !ok {
			t.Fatalf("auto must fall back to FileStore when keyring down, got %T", mgr.Store())
		}
	})
	t.Run("auto-file-fallback-failure-warns", func(t *testing.T) {
		// Keyring unavailable AND fileStore cannot open (bad path) → warn, store nil.
		withFakeKeyring(t, &fakeStore{avail: ErrKeyringUnavailable})
		var stderr bytes.Buffer
		parent := filepath.Join(t.TempDir(), "regularfile")
		if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		mgr, err := NewManager(Config{
			Backend:    "auto",
			FilePath:   filepath.Join(parent, "x.enc"),
			Passphrase: []byte("p"),
			Stderr:     &stderr,
		})
		if err != nil {
			t.Fatalf("auto must never fatal: %v", err)
		}
		defer mgr.Close()
		if mgr.Store() != nil {
			t.Fatal("auto with failed fileStore must leave store nil")
		}
		if !strings.Contains(stderr.String(), "fileStore failed") {
			t.Fatalf("expected fileStore-failed warning, got stderr=%q", stderr.String())
		}
	})
	t.Run("auto-no-passphrase-warns", func(t *testing.T) {
		withFakeKeyring(t, &fakeStore{avail: ErrKeyringUnavailable})
		var stderr bytes.Buffer
		mgr, err := NewManager(Config{Backend: "auto", Stderr: &stderr})
		if err != nil {
			t.Fatalf("auto must never fatal: %v", err)
		}
		defer mgr.Close()
		if mgr.Store() != nil {
			t.Fatal("auto without passphrase must leave store nil")
		}
		if !strings.Contains(stderr.String(), "no passphrase configured") {
			t.Fatalf("expected no-passphrase warning, got stderr=%q", stderr.String())
		}
	})
}

func TestManager_ResolveAllKinds(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Set("svc", "acct", "resolved-value")
	withFakeKeyring(t, fs)
	mgr, err := NewManager(Config{Backend: "keyring"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	// Empty / none kind → "", nil.
	if v, err := mgr.Resolve(CredentialRef{Kind: ""}); err != nil || v != "" {
		t.Fatalf("empty kind = %q, %v", v, err)
	}
	if v, err := mgr.Resolve(CredentialRef{Kind: "none"}); err != nil || v != "" {
		t.Fatalf("none kind = %q, %v", v, err)
	}
	// Secret kind with store → resolved.
	ref, _ := ParseCredentialRef("secret://svc/acct", false)
	if v, err := mgr.Resolve(ref); err != nil || v != "resolved-value" {
		t.Fatalf("secret kind = %q, %v", v, err)
	}
	// Secret missing entry → ErrSecretNotFound.
	refMiss, _ := ParseCredentialRef("secret://svc/missing", false)
	if _, err := mgr.Resolve(refMiss); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("missing secret = %v, want ErrSecretNotFound", err)
	}
	// Env set / unset.
	t.Setenv("YANSHI_TEST_RESOLVE_VAR", "env-value")
	if v, err := mgr.Resolve(CredentialRef{Kind: "env", VarName: "YANSHI_TEST_RESOLVE_VAR"}); err != nil || v != "env-value" {
		t.Fatalf("env set = %q, %v", v, err)
	}
	if _, err := mgr.Resolve(CredentialRef{Kind: "env", VarName: "YANSHI_TEST_RESOLVE_UNSET"}); err == nil {
		t.Fatal("unset env must error")
	}
	// Legacy passthrough.
	if v, err := mgr.Resolve(CredentialRef{Kind: "legacy", Raw: "sk-legacy"}); err != nil || v != "sk-legacy" {
		t.Fatalf("legacy = %q, %v", v, err)
	}
	// Unknown kind.
	if _, err := mgr.Resolve(CredentialRef{Kind: "weird"}); err == nil {
		t.Fatal("unknown kind must error")
	}
}

func TestManager_SetDeleteNoBackend(t *testing.T) {
	mgr, _ := NewManager(Config{Backend: "none"})
	defer mgr.Close()
	if err := mgr.Set("s", "a", "v"); err == nil {
		t.Fatal("Set on no-backend manager must error")
	}
	if err := mgr.Delete("s", "a"); err == nil {
		t.Fatal("Delete on no-backend manager must error")
	}
}

// ---------------------------------------------------------------------------
// file_store.go
// ---------------------------------------------------------------------------

// encryptTestEnvelope builds a valid YSCE file around an arbitrary plaintext so
// load-error tests past the GCM auth (e.g. corrupt JSON payload) are reachable.
func encryptTestEnvelope(t *testing.T, passphrase, plaintext []byte) []byte {
	t.Helper()
	salt := make([]byte, saltLen) // deterministic zero salt is fine for tests
	key := argon2.IDKey(passphrase, salt, kdfTime, kdfMemory, kdfThreads, keyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, nonceLen)
	paramsJSON, err := json.Marshal(kdfParams{Time: kdfTime, Memory: kdfMemory, Threads: kdfThreads})
	if err != nil {
		t.Fatal(err)
	}
	var aad []byte
	aad = append(aad, []byte(fileMagic)...)
	aad = append(aad, fileVersion)
	aad = append(aad, padKdf(kdfNameArgon2)...)
	aad = append(aad, salt...)
	plen := make([]byte, 2)
	binary.BigEndian.PutUint16(plen, uint16(len(paramsJSON)))
	aad = append(aad, plen...)
	aad = append(aad, paramsJSON...)
	aad = append(aad, nonce...)
	ciphertext := gcm.Seal(nil, nonce, plaintext, aad)
	return append(append([]byte(nil), aad...), ciphertext...)
}

func writeStoreFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestFileStore_LoadRejectsMalformedHeaders(t *testing.T) {
	pass := []byte("p")
	min := 4 + 1 + 16 + saltLen // shortest length that passes the size gate

	t.Run("too-short", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.enc")
		writeStoreFile(t, path, []byte("YSCE\x01short"))
		if _, err := NewFileStore(path, pass); err == nil {
			t.Fatal("too-short file must error")
		}
	})
	t.Run("bad-magic", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.enc")
		data := encryptTestEnvelope(t, pass, []byte("[]"))
		data[0] = 'X' // corrupt magic
		writeStoreFile(t, path, data)
		if _, err := NewFileStore(path, pass); err == nil {
			t.Fatal("bad magic must error")
		}
	})
	t.Run("bad-version", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.enc")
		data := encryptTestEnvelope(t, pass, []byte("[]"))
		data[4] = 0x02 // unsupported version
		writeStoreFile(t, path, data)
		if _, err := NewFileStore(path, pass); err == nil {
			t.Fatal("bad version must error")
		}
	})
	t.Run("bad-kdfname", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.enc")
		data := encryptTestEnvelope(t, pass, []byte("[]"))
		// kdfName field is bytes [5:21]; flip a char so it isn't "argon2id".
		copy(data[5:21], []byte("scrypt          "))
		writeStoreFile(t, path, data)
		if _, err := NewFileStore(path, pass); err == nil {
			t.Fatal("unsupported kdfName must error")
		}
	})
	t.Run("truncated-kdf-params-len", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.enc")
		// Header through salt (53 bytes) but missing the 2-byte params length.
		full := encryptTestEnvelope(t, pass, []byte("[]"))
		writeStoreFile(t, path, full[:4+1+16+saltLen+1]) // one byte short of params-len
		if _, err := NewFileStore(path, pass); err == nil {
			t.Fatal("truncated kdf params length must error")
		}
	})
	t.Run("truncated-kdf-params-body", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.enc")
		// Header through the params-length field, then a body SHORTER than the
		// declared length and nothing after, so off+paramsLen > len(data).
		hdr := append([]byte(nil), []byte(fileMagic)...)
		hdr = append(hdr, fileVersion)
		hdr = append(hdr, padKdf(kdfNameArgon2)...)
		hdr = append(hdr, make([]byte, saltLen)...)
		plen := make([]byte, 2)
		binary.BigEndian.PutUint16(plen, 16) // claim 16 bytes of params
		hdr = append(hdr, plen...)
		hdr = append(hdr, make([]byte, 5)...) // only 5 bytes actually follow
		writeStoreFile(t, path, hdr)
		if _, err := NewFileStore(path, pass); err == nil {
			t.Fatal("truncated kdf params body must error")
		}
	})
	t.Run("kdf-params-bad-json", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.enc")
		hdr := append([]byte(nil), []byte(fileMagic)...)
		hdr = append(hdr, fileVersion)
		hdr = append(hdr, padKdf(kdfNameArgon2)...)
		hdr = append(hdr, make([]byte, saltLen)...)
		bad := []byte("{not json")
		plen := make([]byte, 2)
		binary.BigEndian.PutUint16(plen, uint16(len(bad)))
		hdr = append(hdr, plen...)
		hdr = append(hdr, bad...)
		hdr = append(hdr, make([]byte, min)...)
		writeStoreFile(t, path, hdr)
		if _, err := NewFileStore(path, pass); err == nil {
			t.Fatal("bad kdf params json must error")
		}
	})
	t.Run("truncated-nonce", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.enc")
		full := encryptTestEnvelope(t, pass, []byte("[]"))
		// Keep header through params body but cut before the full nonce.
		paramsJSON, _ := json.Marshal(kdfParams{Time: kdfTime, Memory: kdfMemory, Threads: kdfThreads})
		cut := 4 + 1 + 16 + saltLen + 2 + len(paramsJSON) + 3 // 3 bytes of nonce
		writeStoreFile(t, path, full[:cut])
		if _, err := NewFileStore(path, pass); err == nil {
			t.Fatal("truncated nonce must error")
		}
	})
	t.Run("payload-not-json", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.enc")
		// Valid envelope but plaintext is not JSON → GCM ok, json fails.
		writeStoreFile(t, path, encryptTestEnvelope(t, pass, []byte("definitely-not-json")))
		if _, err := NewFileStore(path, pass); err == nil {
			t.Fatal("non-JSON plaintext must error")
		}
	})
}

func TestFileStore_NewFileStorePathErrors(t *testing.T) {
	t.Run("mkdir-fails-when-parent-is-file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "regularfile")
		if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewFileStore(filepath.Join(parent, "x.enc"), []byte("p")); err == nil {
			t.Fatal("mkdir under a regular file must error")
		}
	})
	t.Run("read-error-when-path-is-directory", func(t *testing.T) {
		dir := t.TempDir()
		// FilePath pointing at the directory itself: MkdirAll is a no-op, but
		// ReadFile on a directory yields a non-NotExist error.
		if _, err := NewFileStore(dir, []byte("p")); err == nil {
			t.Fatal("reading a directory must error")
		}
	})
}

func TestFileStore_PersistWriteFileFails(t *testing.T) {
	// Bypass NewFileStore's MkdirAll so persist's WriteFile hits a missing dir.
	fs := &FileStore{path: filepath.Join(t.TempDir(), "nodir", "x.enc"), passphrase: []byte("p")}
	if err := fs.Set("svc", "acct", "v"); err == nil {
		t.Fatal("Set must fail when the target directory does not exist")
	}
}

func TestFileStore_AvailableAndMiss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.enc")
	fs, err := NewFileStore(path, []byte("p"))
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Available(); err != nil {
		t.Fatalf("Available must be nil: %v", err)
	}
	if _, err := fs.Get("missing", "missing"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Get missing = %v, want ErrSecretNotFound", err)
	}
}

func TestFileStore_DeleteExistingPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.enc")
	fs, _ := NewFileStore(path, []byte("p"))
	if err := fs.Set("svc", "a1", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Set("svc", "a2", "v2"); err != nil {
		t.Fatal(err)
	}
	// Delete an existing entry → success and persisted to disk.
	if err := fs.Delete("svc", "a1"); err != nil {
		t.Fatalf("Delete existing: %v", err)
	}
	// Reopen: the deleted entry is gone, the other survives.
	reopened, err := NewFileStore(path, []byte("p"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Get("svc", "a1"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("deleted entry must be gone after reopen: %v", err)
	}
	if got, err := reopened.Get("svc", "a2"); err != nil || got != "v2" {
		t.Fatalf("surviving entry = %q, %v", got, err)
	}
	// Delete missing → ErrSecretNotFound.
	if err := fs.Delete("svc", "nope"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Delete missing = %v, want ErrSecretNotFound", err)
	}
}

// TestFileStore_DeletePersistFailure covers the persist-error branch inside
// Delete: when the atomic replace fails mid-delete, the error must propagate
// and the in-memory entry must survive (Delete rolls back like Set does).
func TestFileStore_DeletePersistFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.enc")
	fs, _ := NewFileStore(path, []byte("p"))
	if err := fs.Set("svc", "acct", "v"); err != nil {
		t.Fatal(err)
	}
	old := replaceEncryptedFile
	replaceEncryptedFile = func(src, dst string) error { return errors.New("injected delete replace") }
	t.Cleanup(func() { replaceEncryptedFile = old })
	if err := fs.Delete("svc", "acct"); err == nil || !strings.Contains(err.Error(), "injected delete replace") {
		t.Fatalf("want injected delete-replace error, got %v", err)
	}
	// Rollback: the entry must still be present in memory.
	if got, err := fs.Get("svc", "acct"); err != nil || got != "v" {
		t.Fatalf("entry must survive failed delete: got=%q err=%v", got, err)
	}
}

func TestFileStore_EnumerateDistinctAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.enc")
	fs, _ := NewFileStore(path, []byte("p"))
	_ = fs.Set("openai", "main", "v1")
	_ = fs.Set("openai", "main", "v1-updated") // duplicate account, idempotent
	_ = fs.Set("openai", "alt", "v2")
	_ = fs.Set("anthropic", "main", "v3") // different service, excluded

	got, err := fs.Enumerate("openai")
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("distinct openai accounts = %v, want 2", got)
	}
	seen := map[string]bool{}
	for _, a := range got {
		seen[a] = true
	}
	if !seen["main"] || !seen["alt"] {
		t.Fatalf("enumerate missing expected accounts: %v", got)
	}
	// Unknown service → empty (nil) list.
	if list, _ := fs.Enumerate("unknown"); len(list) != 0 {
		t.Fatalf("enumerate unknown service = %v, want empty", list)
	}
}

func TestPadKdfTruncatesLongNames(t *testing.T) {
	long := strings.Repeat("k", 20)
	if got := padKdf(long); got != long[:16] {
		t.Fatalf("padKdf(long) = %q, want first 16 bytes", got)
	}
	if got := padKdf("argon2id"); len(got) != 16 {
		t.Fatalf("padKdf short = %q, want padded to 16", got)
	}
}

// ---------------------------------------------------------------------------
// cli.go
// ---------------------------------------------------------------------------

func TestAuthCommand_RequiresProviderAndAccount(t *testing.T) {
	for _, c := range []AuthCommand{
		{Account: "a", Backend: "none"},
		{Provider: "p", Backend: "none"},
	} {
		if err := c.Run(); err == nil {
			t.Fatalf("Run must require provider and account: %+v", c)
		}
	}
}

func TestAuthCommand_NewManagerErrorPropagates(t *testing.T) {
	cmd := AuthCommand{Provider: "p", Account: "a", Backend: "bogus", Stdin: &bytes.Buffer{}, Stdout: &bytes.Buffer{}}
	if err := cmd.Run(); err == nil {
		t.Fatal("unknown backend must surface a NewManager error")
	}
}

func TestAuthCommand_DeleteSuccessWithNilStdoutStdin(t *testing.T) {
	// Inject a Manager holding the entry; delete with nil Stdin/Stdout so the
	// os.Stdin/os.Stdout defaults are wired (delete never reads stdin). Covers
	// the delete-success branch and the default IO wiring.
	path := filepath.Join(t.TempDir(), "s.enc")
	mgr, err := NewManager(Config{Backend: "file", FilePath: path, Passphrase: []byte("p")})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if err := mgr.Set("p", "a", "secret"); err != nil {
		t.Fatal(err)
	}
	cmd := AuthCommand{Provider: "p", Account: "a", Delete: true, Manager: mgr}
	// Stdin and Stdout intentionally nil → defaults assigned inside Run.
	if err := cmd.Run(); err != nil {
		t.Fatalf("delete success: %v", err)
	}
}

func TestAuthCommand_EmptyInteractiveKeyRefused(t *testing.T) {
	cmd := AuthCommand{
		Provider: "p", Account: "a", Backend: "file",
		FilePath:   filepath.Join(t.TempDir(), "s.enc"),
		Passphrase: []byte("p"),
		Stdin:      bytes.NewBufferString("\n"), // empty line → empty key
		Stdout:     &bytes.Buffer{},
	}
	if err := cmd.Run(); err == nil || !strings.Contains(err.Error(), "empty API key") {
		t.Fatalf("want empty-key refusal, got %v", err)
	}
}

func TestAuthCommand_SetErrorPropagates(t *testing.T) {
	// Inject a failing replace so the owned FileStore's Set fails.
	old := replaceEncryptedFile
	replaceEncryptedFile = func(src, dst string) error { return errors.New("injected") }
	t.Cleanup(func() { replaceEncryptedFile = old })
	cmd := AuthCommand{
		Provider: "p", Account: "a", Backend: "file",
		FilePath:   filepath.Join(t.TempDir(), "s.enc"),
		Passphrase: []byte("p"),
		Stdin:      bytes.NewBufferString("sk-key\n"),
		Stdout:     &bytes.Buffer{},
	}
	if err := cmd.Run(); err == nil || !strings.Contains(err.Error(), "auth: set") {
		t.Fatalf("want set error, got %v", err)
	}
}

func TestReadLimitedSecretError(t *testing.T) {
	if _, err := readLimitedSecret(errReader{err: errors.New("boom")}); err == nil {
		t.Fatal("readLimitedSecret must surface reader error")
	}
}

func TestReadLineErrorBranches(t *testing.T) {
	t.Run("eof-with-data-returns-buffer", func(t *testing.T) {
		// No trailing newline → reads all data then EOF with buf>0.
		got, err := readLine(strings.NewReader("no-newline"))
		if err != nil || got != "no-newline" {
			t.Fatalf("readLine = %q, %v", got, err)
		}
	})
	t.Run("reader-error", func(t *testing.T) {
		if _, err := readLine(errReader{err: errors.New("read-failed")}); err == nil {
			t.Fatal("readLine must surface reader error")
		}
	})
	t.Run("empty-eof-returns-error", func(t *testing.T) {
		if _, err := readLine(strings.NewReader("")); err == nil {
			t.Fatal("readLine on empty reader must error")
		}
	})
}

// ---------------------------------------------------------------------------
// file_store_replace_windows.go (Windows atomic replace)
// ---------------------------------------------------------------------------

func TestReplaceEncryptedFileOS(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		dir := t.TempDir()
		src := filepath.Join(dir, "src.bin")
		dst := filepath.Join(dir, "dst.bin")
		if err := os.WriteFile(src, []byte("payload"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := replaceEncryptedFileOS(src, dst); err != nil {
			t.Fatalf("replace: %v", err)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "payload" {
			t.Fatalf("dst content = %q", got)
		}
	})
	t.Run("missing-src-not-exist", func(t *testing.T) {
		dir := t.TempDir()
		err := replaceEncryptedFileOS(filepath.Join(dir, "absent"), filepath.Join(dir, "dst"))
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing src = %v, want os.ErrNotExist", err)
		}
	})
}

// ---------------------------------------------------------------------------
// keyring_enabled.go — real OS keyring round-trip (integration-style, gated).
// On hosts without a usable keyring these skip; where the keyring works they
// cover Set/Get/Delete success plus the Available probe hit branch.
// ---------------------------------------------------------------------------

func TestKeyring_RoundTripWhenAvailable(t *testing.T) {
	s := NewOSKeyringStore()
	if err := s.Available(); err != nil {
		t.Skipf("OS keyring unavailable on this host; skipping real round-trip: %v", err)
	}
	const svc = "__yanshi_test_cov__"
	const acct = "__roundtrip__"
	t.Cleanup(func() { _ = s.Delete(svc, acct) })

	if err := s.Set(svc, acct, "roundtrip-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(svc, acct)
	if err != nil || got != "roundtrip-secret" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := s.Delete(svc, acct); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(svc, acct); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Get after delete = %v, want ErrSecretNotFound", err)
	}
	// Deleting a now-missing entry surfaces ErrSecretNotFound.
	if err := s.Delete(svc, acct); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("Delete missing = %v, want ErrSecretNotFound", err)
	}
}

// TestKeyring_AvailableProbeHit covers the `err == nil` branch of Available:
// after setting the sentinel probe entry, the probe Get returns a value (not
// NotFound) and Available reports success via that path.
func TestKeyring_AvailableProbeHit(t *testing.T) {
	s := NewOSKeyringStore()
	if err := s.Available(); err != nil {
		t.Skipf("OS keyring unavailable; skipping probe-hit test: %v", err)
	}
	const probe = "__yanshi_probe__"
	t.Cleanup(func() { _ = s.Delete(probe, probe) })
	if err := s.Set(probe, probe, "x"); err != nil {
		t.Fatalf("set probe: %v", err)
	}
	if err := s.Available(); err != nil {
		t.Fatalf("Available with probe entry set must be nil: %v", err)
	}
}

// ---------------------------------------------------------------------------
// cli.go — interactive terminal (echo-off) password branch, driven via the
// isTerminal / readPasswordFromTerminal seams.
// ---------------------------------------------------------------------------

// withTerminalSeam forces Run to take the term.ReadPassword branch by reporting
// the stdin fd as a terminal and returning canned password bytes. Stdin must be
// a real *os.File so the (*os.File) type assertion inside Run succeeds.
func withTerminalSeam(t *testing.T, password []byte, readErr error) (*os.File, func()) {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "fake-tty")
	if err != nil {
		t.Fatal(err)
	}
	origIsTerm, origRead := isTerminal, readPasswordFromTerminal
	isTerminal = func(int) bool { return true }
	readPasswordFromTerminal = func(int) ([]byte, error) { return password, readErr }
	return tmp, func() {
		isTerminal, readPasswordFromTerminal = origIsTerm, origRead
		_ = tmp.Close()
	}
}

func TestAuthCommand_TerminalPasswordPaths(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		stdin, restore := withTerminalSeam(t, []byte("sk-tty-secret"), nil)
		defer restore()
		out := &bytes.Buffer{}
		cmd := AuthCommand{
			Provider: "p", Account: "a", Backend: "file",
			FilePath:   filepath.Join(t.TempDir(), "s.enc"),
			Passphrase: []byte("p"),
			Stdin:      stdin,
			Stdout:     out,
		}
		if err := cmd.Run(); err != nil {
			t.Fatalf("terminal password Run: %v", err)
		}
		if !strings.HasSuffix(out.String(), "\n") {
			t.Fatalf("terminal path must print newline after hidden input: %q", out.String())
		}
	})
	t.Run("read-error", func(t *testing.T) {
		stdin, restore := withTerminalSeam(t, nil, errors.New("tty read failed"))
		defer restore()
		cmd := AuthCommand{
			Provider: "p", Account: "a", Backend: "file",
			FilePath:   filepath.Join(t.TempDir(), "s.enc"),
			Passphrase: []byte("p"),
			Stdin:      stdin,
			Stdout:     &bytes.Buffer{},
		}
		if err := cmd.Run(); err == nil || !strings.Contains(err.Error(), "read password") {
			t.Fatalf("want read-password error, got %v", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		stdin, restore := withTerminalSeam(t, bytes.Repeat([]byte("x"), maxSecretInputBytes+1), nil)
		defer restore()
		cmd := AuthCommand{
			Provider: "p", Account: "a", Backend: "file",
			FilePath:   filepath.Join(t.TempDir(), "s.enc"),
			Passphrase: []byte("p"),
			Stdin:      stdin,
			Stdout:     &bytes.Buffer{},
		}
		if err := cmd.Run(); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("want oversized error, got %v", err)
		}
	})
}

// Reference io so the import stays used by the errReader contract above.
var _ = io.EOF

// TestTerminalSeamDefaults confirms the production defaults of the
// isTerminal / readPasswordFromTerminal seams delegate to the real term
// functions (executing the closure body rather than a test override). On a
// non-tty file descriptor term.ReadPassword returns an error, which is the
// expected delegation — we only assert it does not panic.
func TestTerminalSeamDefaults(t *testing.T) {
	orig := readPasswordFromTerminal
	t.Cleanup(func() { readPasswordFromTerminal = orig })
	tmp, err := os.CreateTemp(t.TempDir(), "not-a-tty")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	// isTerminal default is a direct alias to term.IsTerminal; reading it
	// executes the (trivial) lookup. readPasswordFromTerminal's closure body
	// calls term.ReadPassword, which errors on a plain file — that proves the
	// seam delegates instead of panicking.
	_ = isTerminal(int(tmp.Fd()))
	if _, err := readPasswordFromTerminal(int(tmp.Fd())); err == nil {
		// A file fd is not a console; an error is the expected outcome. If the
		// host somehow accepts it we still pass — the goal is to exercise the
		// delegation, not to assert terminal semantics.
	}
}
