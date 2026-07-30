package secrets

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFileStore_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	fs, err := NewFileStore(path, []byte("correct-passphrase"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Set("openai", "main", "sk-test-12345"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := fs.Get("openai", "main")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-test-12345" {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestFileStore_WrongPassphraseFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	fs1, _ := NewFileStore(path, []byte("correct"))
	_ = fs1.Set("svc", "acct", "v")
	// Wrong passphrase must fail to read existing file (GCM auth tag mismatch).
	_, err := NewFileStore(path, []byte("wrong"))
	if err == nil {
		t.Fatal("expected error on wrong passphrase")
	}
}

func TestFileStore_KDFHeaderIsArgon2id(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	fs, _ := NewFileStore(path, []byte("p"))
	_ = fs.Set("svc", "acct", "v")
	data, _ := os.ReadFile(path)
	if string(data[:4]) != "YSCE" {
		t.Fatalf("magic missing: %q", data[:4])
	}
	if data[4] != 0x01 {
		t.Fatalf("version: %d", data[4])
	}
	kdf := strings.TrimRight(string(data[5:21]), " ")
	if kdf != "argon2id" {
		t.Fatalf("kdfName: %q want argon2id", kdf)
	}
}

func maliciousKDFEnvelope(t *testing.T, params any) []byte {
	t.Helper()
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	data := append([]byte(nil), []byte("YSCE")...)
	data = append(data, 0x01)
	data = append(data, []byte("argon2id        ")...) // exactly 16 bytes
	data = append(data, make([]byte, 32)...)           // salt
	plen := make([]byte, 2)
	binary.BigEndian.PutUint16(plen, uint16(len(paramsJSON)))
	data = append(data, plen...)
	data = append(data, paramsJSON...)
	data = append(data, make([]byte, 12+16)...) // nonce + dummy GCM tag
	return data
}

// Untrusted header values are validated before Argon2id. Without this gate a
// tiny malicious file can request unbounded memory/CPU or pass zero threads to
// argon2.IDKey. Each case must return an error without reaching the KDF.
func TestFileStore_RejectsUnsafeKDFParameters(t *testing.T) {
	tests := []struct {
		name    string
		time    uint32
		memory  uint32
		threads uint8
	}{
		{"zero-time", 0, 64 * 1024, 2},
		{"excessive-time", 11, 64 * 1024, 2},
		{"low-memory", 1, 8*1024 - 1, 2},
		{"excessive-memory", 1, 256*1024 + 1, 2},
		{"zero-threads", 1, 64 * 1024, 0},
		{"excessive-threads", 1, 64 * 1024, 17},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secrets.enc")
			data := maliciousKDFEnvelope(t, struct {
				Time    uint32 `json:"time"`
				Memory  uint32 `json:"memory"`
				Threads uint8  `json:"threads"`
			}{tt.time, tt.memory, tt.threads})
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := NewFileStore(path, []byte("p"))
			if err == nil || !strings.Contains(err.Error(), "unsafe kdf params") {
				t.Fatalf("want bounded-kdf rejection, got %v", err)
			}
		})
	}
}

func TestFileStore_RejectsOversizedKDFParameterJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	data := append([]byte(nil), []byte("YSCE")...)
	data = append(data, 0x01)
	data = append(data, []byte("argon2id        ")...)
	data = append(data, make([]byte, 32)...)
	plen := make([]byte, 2)
	binary.BigEndian.PutUint16(plen, 1025)
	data = append(data, plen...)
	data = append(data, make([]byte, 1025+12+16)...)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := NewFileStore(path, []byte("p"))
	if err == nil || !strings.Contains(err.Error(), "kdf params length") {
		t.Fatalf("want bounded-header rejection, got %v", err)
	}
}

func TestFileStore_CorruptFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	_ = os.WriteFile(path, []byte("YSCE\x01garbage"), 0600)
	_, err := NewFileStore(path, []byte("p"))
	if err == nil {
		t.Fatal("expected error on corrupt file")
	}
}

// TestFileStore_PreservesJSONStructure validates that the ciphertext payload
// is a stable JSON shape (forward-compat: future KDFs still read v1 entries).
func TestFileStore_PreservesJSONStructure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	fs, _ := NewFileStore(path, []byte("p"))
	_ = fs.Set("openai", "main", "v1")
	// Inspect via internal accessor (see implementation).
	entries, _ := fs.debugEntries()
	if len(entries) != 1 || entries[0].Service != "openai" {
		var b []byte
		b, _ = json.Marshal(entries)
		t.Fatalf("unexpected entries: %s", b)
	}
}

// TestFileStore_ReplaceFailureRollsBackDiskAndMemory injects failure at the
// exact atomic-replace seam. It does not delete/restore the target itself: the
// failed Set must preserve both the prior encrypted bytes and fs.entries.
func TestFileStore_ReplaceFailureRollsBackDiskAndMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	fs, err := NewFileStore(path, []byte("pass"))
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Set("openai", "main", "old-secret"); err != nil {
		t.Fatal(err)
	}
	oldBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	oldReplace := replaceEncryptedFile
	replaceEncryptedFile = func(src, dst string) error {
		if dst != path || !strings.HasPrefix(src, path+".tmp.") {
			t.Fatalf("unexpected replace %q -> %q", src, dst)
		}
		return errors.New("injected encrypted-file replace failure")
	}
	t.Cleanup(func() { replaceEncryptedFile = oldReplace })

	err = fs.Set("openai", "main", "new-secret")
	if err == nil || !strings.Contains(err.Error(), "injected encrypted-file replace failure") {
		t.Fatalf("expected injected replace failure, got %v", err)
	}
	got, err := fs.Get("openai", "main")
	if err != nil || got != "old-secret" {
		t.Fatalf("in-memory rollback failed: got=%q err=%v", got, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, oldBytes) {
		t.Fatal("encrypted target changed after failed replace")
	}
	matches, err := filepath.Glob(path + ".tmp.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("new temp file not cleaned: %v", matches)
	}
}

func TestFileStore_ConcurrentAccessIsSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	fs, err := NewFileStore(path, []byte("pass"))
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			account := fmt.Sprintf("account-%d", i)
			value := fmt.Sprintf("secret-%d", i)
			if err := fs.Set("service", account, value); err != nil {
				errs <- err
				return
			}
			got, err := fs.Get("service", account)
			if err != nil {
				errs <- err
				return
			}
			if got != value {
				errs <- fmt.Errorf("%s: got %q want %q", account, got, value)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
