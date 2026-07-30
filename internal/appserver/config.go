package appserver

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// secretPathFragments are the dot-path segments that mark a config path as
// restricted. Any path whose final segment (case-insensitive) matches one of
// these — or whose segment contains "password" — is rejected on read AND
// write. The goal is to keep secret material from ever crossing the JSON-RPC
// wire (defense in depth alongside guard and the redacting logger).
var secretPathFragments = map[string]bool{
	"token":    true,
	"api_key":  true,
	"apikey":   true,
	"secret":   true,
}

// MemoryConfig is the default ConfigBackend: a thread-safe in-memory key/value
// store. It is used by `yanshi app --fake-model` tests and by supervisors that
// do not want a YAML file on disk. The YAML-backed variant lives at the
// supervisor layer (Task 11 future work) — the contract is the same.
type MemoryConfig struct {
	mu     sync.Mutex
	values map[string]any
}

// NewMemoryConfig constructs an empty in-memory config backend.
func NewMemoryConfig() *MemoryConfig {
	return &MemoryConfig{values: make(map[string]any)}
}

// Read returns the stored value for key. A restricted key returns an error;
// an unset key returns an error so the supervisor surfaces the typo rather
// than silently observing a null value.
func (c *MemoryConfig) Read(key string) (any, error) {
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

// Write stores value at key after JSON-decoding it. A restricted key is
// rejected BEFORE the decode so even a malformed secret payload never reaches
// the store.
func (c *MemoryConfig) Write(key string, value json.RawMessage) error {
	if err := validateConfigKey(key); err != nil {
		return err
	}
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return fmt.Errorf("config value: %w", err)
	}
	c.mu.Lock()
	c.values[key] = decoded
	c.mu.Unlock()
	return nil
}

// validateConfigKey enforces the no-secret-paths rule. The check is case-
// insensitive on the final dot-segment so api_key, API_KEY, and ApiKey are all
// rejected. The "password" substring is matched anywhere in a segment so
// "db_password" and "password_hint" both fail.
func validateConfigKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("config key is required")
	}
	for _, part := range strings.Split(strings.ToLower(key), ".") {
		if secretPathFragments[part] {
			return fmt.Errorf("config key %q is restricted", key)
		}
		if strings.Contains(part, "password") {
			return fmt.Errorf("config key %q is restricted", key)
		}
	}
	return nil
}
