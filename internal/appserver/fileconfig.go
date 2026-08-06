package appserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileConfig is a ConfigBackend that survives the process.
//
// `yanshi app` used to construct MemoryConfig unconditionally, even when
// -config named a file, so config/write accepted a value, reported success and
// dropped it at exit — while docs/api/jsonrpc.md described the pair as reading
// and writing runtime configuration. A supervisor following that document
// would have configured a fleet that silently reset on every restart.
//
// It deliberately does NOT write into the YAML config file itself. That file
// is hand-maintained, comment-bearing, and read by bootstrap before any of
// this exists; a JSON-RPC caller rewriting it would destroy the operator's
// comments and formatting to store a key bootstrap never reads. The runtime
// key/value store is a separate sidecar next to it, so "which file did my
// write go to" has one answer and neither file can clobber the other.
type FileConfig struct {
	mu   sync.Mutex
	path string
}

// SidecarPath returns the runtime-config file that accompanies a YAML config
// path. Exported so operators and tests can name the same file the server
// writes rather than re-deriving the suffix.
//
// The whole filename is kept and the suffix appended, rather than replacing
// the extension. Replacing it collapsed distinct configs onto one store:
// config.yaml and config.yml both trimmed to "config", and every
// extension-less dotfile trimmed to nothing (filepath.Ext(".hidden") is the
// WHOLE name — there is no stem) and hit the fallback. Two `yanshi app`
// processes with different -config values then read and wrote the same runtime
// store, each silently overwriting the other.
func SidecarPath(configPath string) string {
	dir, base := filepath.Split(configPath)
	if base == "" {
		base = "config"
	}
	return filepath.Join(dir, base+".appstate.json")
}

// NewFileConfig opens the sidecar store for a YAML config path.
//
// A missing file is an empty store, not an error: the first `yanshi app` run
// against a fresh config has nothing to load, and refusing to start would make
// persistence a setup step instead of a default. A file that exists but does
// not parse IS an error — silently starting empty there would present a
// corrupted store as an unconfigured one, and the next write would overwrite
// whatever the operator still had.
func NewFileConfig(configPath string) (*FileConfig, error) {
	c := &FileConfig{path: SidecarPath(configPath)}
	if _, err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

// load reads the store from disk. There is no in-memory cache, deliberately.
//
// Nothing stops two `yanshi app` processes sharing one -config, and while each
// held its own snapshot and flushed the WHOLE document, the second writer
// erased every key the first had written — last-writer-wins over the entire
// store rather than per key — and a long-running reader served a view frozen
// at startup. The file is the truth; it is a handful of keys read on a
// JSON-RPC call, so re-reading costs nothing worth caching around.
//
// The residual race is narrow and named: two processes writing DIFFERENT keys
// in the same instant can still lose one, because read-modify-write is not
// atomic across processes. Closing that needs file locking, which is a real
// dependency for a store nobody writes concurrently in practice.
func (c *FileConfig) load() (map[string]any, error) {
	data, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read runtime config %s: %w", c.path, err)
	}
	values := map[string]any{}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("parse runtime config %s: %w", c.path, err)
	}
	return values, nil
}

// Read returns the stored value for key, applying the same restricted-path
// rule as MemoryConfig. An unset key is an error so a supervisor sees its typo
// instead of a null value.
func (c *FileConfig) Read(key string) (any, error) {
	if err := validateConfigKey(key); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	values, err := c.load()
	if err != nil {
		return nil, err
	}
	value, ok := values[key]
	if !ok {
		return nil, fmt.Errorf("config key %q is not set", key)
	}
	return value, nil
}

// Write stores value at key and flushes to disk.
//
// The restricted-path check runs BEFORE the JSON decode and before any file
// is touched, matching MemoryConfig. That ordering matters more here than it
// did in memory: a leak that reaches this backend is durable, and outlives the
// process that made the mistake.
//
// The store is re-read before the merge so this write adds a key rather than
// replacing the document with one process's idea of it.
func (c *FileConfig) Write(key string, value json.RawMessage) error {
	if err := validateConfigKey(key); err != nil {
		return err
	}
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return fmt.Errorf("config value: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	values, err := c.load()
	if err != nil {
		return err
	}
	values[key] = decoded
	return c.flushLocked(values)
}

// flushLocked writes the store atomically: a temp file in the same directory
// followed by a rename, so a crash mid-write leaves the previous contents
// rather than a truncated file.
func (c *FileConfig) flushLocked(values map[string]any) error {
	data, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runtime config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".appstate-*.json")
	if err != nil {
		return fmt.Errorf("write runtime config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write runtime config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write runtime config: %w", err)
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		return fmt.Errorf("write runtime config: %w", err)
	}
	return nil
}
