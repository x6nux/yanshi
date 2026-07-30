package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

const (
	fileMagic     = "YSCE"
	fileVersion   = 0x01
	kdfNameArgon2 = "argon2id"
	saltLen       = 32
	nonceLen      = 12
	kdfTime       = 1
	kdfMemory     = 64 * 1024
	kdfThreads    = 2
	keyLen        = 32 // AES-256

	minKDFTime       = 1
	maxKDFTime       = 10
	minKDFMemoryKiB  = 8 * 1024
	maxKDFMemoryKiB  = 256 * 1024
	minKDFThreads    = 1
	maxKDFThreads    = 16
	maxKDFParamsJSON = 1024
)

type kdfParams struct {
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"`
	Threads uint8  `json:"threads"`
}

type fileEntry struct {
	Service string `json:"service"`
	Account string `json:"account"`
	Secret  string `json:"secret"`
}

type filePayload struct {
	Entries []fileEntry `json:"entries"`
}

// FileStore implements Store backed by an AES-256-GCM encrypted file. The KDF
// is Argon2id; parameters and salt are stored in the file header so future
// upgrades can read legacy files without re-encrypting.
type FileStore struct {
	mu         sync.RWMutex
	path       string
	passphrase []byte
	entries    []fileEntry
}

// replaceEncryptedFile is the injectable seam around the OS-specific atomic
// replacement. Tests restore it with t.Cleanup.
var replaceEncryptedFile = replaceEncryptedFileOS

// NewFileStore opens or creates the encrypted file. If the file does not
// exist, the store starts empty and persists on the first Set. If it exists,
// it must have the YSCE magic and version 0x01 with kdfName "argon2id".
func NewFileStore(path string, passphrase []byte) (*FileStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("secrets: mkdir: %w", err)
	}
	fs := &FileStore{path: path, passphrase: passphrase}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fs, nil // empty store; persisted on first Set
		}
		return nil, fmt.Errorf("secrets: read file: %w", err)
	}
	if err := fs.load(data); err != nil {
		return nil, fmt.Errorf("secrets: load: %w", err)
	}
	return fs, nil
}

func (fs *FileStore) load(data []byte) error {
	if len(data) < 4+1+16+saltLen {
		return errors.New("secrets: file too short")
	}
	if string(data[:4]) != fileMagic {
		return fmt.Errorf("secrets: bad magic %q", data[:4])
	}
	if data[4] != fileVersion {
		return fmt.Errorf("secrets: unsupported version %d", data[4])
	}
	kdfName := trimPad(string(data[5:21]))
	if kdfName != kdfNameArgon2 {
		return fmt.Errorf("secrets: unsupported kdfName %q", kdfName)
	}
	off := 21
	salt := data[off : off+saltLen]
	off += saltLen
	if off+2 > len(data) {
		return errors.New("secrets: truncated kdf params")
	}
	paramsLen := int(binary.BigEndian.Uint16(data[off : off+2]))
	off += 2
	if paramsLen == 0 || paramsLen > maxKDFParamsJSON {
		return fmt.Errorf("secrets: kdf params length %d outside 1..%d",
			paramsLen, maxKDFParamsJSON)
	}
	if off+paramsLen > len(data) {
		return errors.New("secrets: truncated kdf params body")
	}
	var params kdfParams
	if err := json.Unmarshal(data[off:off+paramsLen], &params); err != nil {
		return fmt.Errorf("secrets: kdf params json: %w", err)
	}
	if err := validateKDFParams(params); err != nil {
		return err
	}
	off += paramsLen
	if off+nonceLen > len(data) {
		return errors.New("secrets: truncated nonce")
	}
	nonce := data[off : off+nonceLen]
	off += nonceLen
	aad := data[:off] // authenticate every header byte, including the nonce
	ciphertext := data[off:]
	key := argon2.IDKey(fs.passphrase, salt, params.Time, params.Memory, params.Threads, keyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("secrets: aes new: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("secrets: gcm new: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return errors.New("secrets: wrong passphrase or corrupt ciphertext (GCM auth failed)")
	}
	var payload filePayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return fmt.Errorf("secrets: payload json: %w", err)
	}
	fs.entries = payload.Entries
	return nil
}

func validateKDFParams(p kdfParams) error {
	if p.Time < minKDFTime || p.Time > maxKDFTime ||
		p.Memory < minKDFMemoryKiB || p.Memory > maxKDFMemoryKiB ||
		p.Threads < minKDFThreads || p.Threads > maxKDFThreads {
		return fmt.Errorf(
			"secrets: unsafe kdf params: time=%d memory=%d threads=%d",
			p.Time, p.Memory, p.Threads,
		)
	}
	return nil
}

func (fs *FileStore) persist(entries []fileEntry) error {
	plaintext, err := json.Marshal(filePayload{Entries: entries})
	if err != nil {
		return fmt.Errorf("secrets: marshal: %w", err)
	}
	salt := make([]byte, saltLen)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return fmt.Errorf("secrets: salt: %w", err)
	}
	key := argon2.IDKey(fs.passphrase, salt, kdfTime, kdfMemory, kdfThreads, keyLen)
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("secrets: aes new: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("secrets: gcm new: %w", err)
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("secrets: nonce: %w", err)
	}
	paramsJSON, err := json.Marshal(kdfParams{
		Time: kdfTime, Memory: kdfMemory, Threads: kdfThreads,
	})
	if err != nil {
		return fmt.Errorf("secrets: marshal kdf params: %w", err)
	}
	if len(paramsJSON) > maxKDFParamsJSON {
		return fmt.Errorf("secrets: kdf params length %d exceeds %d",
			len(paramsJSON), maxKDFParamsJSON)
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
	buf := append(append([]byte(nil), aad...), ciphertext...)

	suffix, err := randomHex(8)
	if err != nil {
		return err
	}
	tmp := fs.path + ".tmp." + suffix
	if err := os.WriteFile(tmp, buf, 0600); err != nil {
		return fmt.Errorf("secrets: write tmp: %w", err)
	}
	if err := replaceEncryptedFile(tmp, fs.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("secrets: replace encrypted file: %w", err)
	}
	return nil
}

// Available returns nil, indicating the file store is always usable.
func (fs *FileStore) Available() error { return nil }

// Get retrieves a stored credential by service and account.
func (fs *FileStore) Get(service, account string) (string, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	for _, e := range fs.entries {
		if e.Service == service && e.Account == account {
			return e.Secret, nil
		}
	}
	return "", ErrSecretNotFound
}

// Set stores or updates a credential, persisting to disk on success.
func (fs *FileStore) Set(service, account, secret string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	next := append([]fileEntry(nil), fs.entries...)
	found := false
	for i, e := range next {
		if e.Service == service && e.Account == account {
			next[i].Secret = secret
			found = true
			break
		}
	}
	if !found {
		next = append(next, fileEntry{Service: service, Account: account, Secret: secret})
	}
	if err := fs.persist(next); err != nil {
		return err
	}
	fs.entries = next
	return nil
}

// Delete removes a stored credential by service and account.
func (fs *FileStore) Delete(service, account string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	next := append([]fileEntry(nil), fs.entries...)
	for i, e := range next {
		if e.Service == service && e.Account == account {
			next = append(next[:i], next[i+1:]...)
			if err := fs.persist(next); err != nil {
				return err
			}
			fs.entries = next
			return nil
		}
	}
	return ErrSecretNotFound
}

// Enumerate returns the distinct accounts stored under service. Used by
// auth.Manager.ListAccounts so CLI `yanshi auth status` can list every
// account for a provider without scanning arbitrary unrelated services.
func (fs *FileStore) Enumerate(service string) ([]string, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for _, e := range fs.entries {
		if e.Service == service && !seen[e.Account] {
			seen[e.Account] = true
			out = append(out, e.Account)
		}
	}
	return out, nil
}

// debugEntries is test-only (not exported beyond the package) and lets the
// JSON-shape test inspect the in-memory payload without re-decrypting.
func (fs *FileStore) debugEntries() ([]fileEntry, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return append([]fileEntry(nil), fs.entries...), nil
}

func padKdf(name string) string {
	if len(name) >= 16 {
		return name[:16]
	}
	return name + strings.Repeat(" ", 16-len(name))
}
func trimPad(s string) string { return strings.TrimRight(s, " ") }

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("secrets: generate temp suffix: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}
