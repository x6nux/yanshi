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
	mu     sync.Mutex
	path   string
	values map[string]any
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

// NewFileConfig opens (or starts) the sidecar store for a YAML config path.
//
// A missing file is an empty store, not an error: the first `yanshi app` run
// against a fresh config has nothing to load, and refusing to start would make
// persistence a setup step instead of a default. A file that exists but does
// not parse IS an error — silently starting empty there would present a
// corrupted store as an unconfigured one, and the next write would overwrite
// whatever the operator still had.
func NewFileConfig(configPath string) (*FileConfig, error) {
	c := &FileConfig{path: SidecarPath(configPath), values: map[string]any{}}
	data, err := os.ReadFile(c.path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read runtime config %s: %w", c.path, err)
	}
	if err := json.Unmarshal(data, &c.values); err != nil {
		return nil, fmt.Errorf("parse runtime config %s: %w", c.path, err)
	}
	if c.values == nil {
		c.values = map[string]any{}
	}
	return c, nil
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
	value, ok := c.values[key]
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
	prev, had := c.values[key]
	c.values[key] = decoded
	if err := c.flushLocked(); err != nil {
		// Roll back so the in-memory view never claims a value the next
		// process will not find. Reporting success on a failed flush is the
		// same defect this type exists to fix, one layer down.
		if had {
			c.values[key] = prev
		} else {
			delete(c.values, key)
		}
		return err
	}
	return nil
}

// flushLocked writes the store atomically: a temp file in the same directory
// followed by a rename, so a crash mid-write leaves the previous contents
// rather than a truncated file.
func (c *FileConfig) flushLocked() error {
	data, err := json.MarshalIndent(c.values, "", "  ")
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
