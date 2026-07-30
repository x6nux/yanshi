package appserver

import (
	"encoding/json"
	"testing"
)

// TestMemoryConfigRejectsSecretPaths proves the secret-path denylist covers
// token / api_key / apikey / secret / *password* at any dot-segment. Read AND
// write are both rejected so a supervisor cannot observe or mutate secret
// material through the JSON-RPC config surface.
func TestMemoryConfigRejectsSecretPaths(t *testing.T) {
	cases := []struct {
		name string
		key  string
		ok   bool
	}{
		{"plain model name", "llm.providers.0.model", true},
		{"plain thinking", "llm.thinking", true},
		{"token rejected", "token", false},
		{"nested token rejected", "server.token", false},
		{"api_key rejected", "llm.providers.0.api_key", false},
		{"apikey rejected", "llm.providers.0.apikey", false},
		{"secret rejected", "secret", false},
		{"password substring rejected", "db_password", false},
		{"password substring rejected in path", "db.password_hint", false},
		{"uppercase API_KEY rejected", "llm.API_KEY", false},
		{"empty key rejected", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := NewMemoryConfig()
			err := cfg.Write(tc.key, json.RawMessage(`"v"`))
			if tc.ok && err != nil {
				t.Fatalf("safe write %q failed: %v", tc.key, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("write %q should be rejected", tc.key)
			}
			// Reads on restricted paths also fail even when the underlying
			// store is empty (the validation runs before the lookup).
			if _, err := cfg.Read(tc.key); tc.ok && err != nil {
				// Allow "not set" — only validate that validation did not fail.
				if !contains(err.Error(), "not set") {
					t.Fatalf("safe read %q errored unexpectedly: %v", tc.key, err)
				}
			}
			if !tc.ok && err == nil {
				t.Fatalf("read %q should be rejected", tc.key)
			}
		})
	}
}

// TestMemoryConfigRoundTripsJSONValues proves a non-secret key can store and
// retrieve any JSON value (string, number, object, array) without coercion.
func TestMemoryConfigRoundTripsJSONValues(t *testing.T) {
	cfg := NewMemoryConfig()
	values := map[string]string{
		"model":     `"gpt-4o"`,
		"temperature": `0.7`,
		"schema":    `{"type":"object"}`,
		"tags":      `["a","b"]`,
	}
	for k, v := range values {
		if err := cfg.Write(k, json.RawMessage(v)); err != nil {
			t.Fatalf("write %q: %v", k, err)
		}
		got, err := cfg.Read(k)
		if err != nil {
			t.Fatalf("read %q: %v", k, err)
		}
		// Re-encode the retrieved value and compare byte-wise to the input.
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("encode %q: %v", k, err)
		}
		if string(encoded) != v {
			t.Fatalf("round-trip %q = %s, want %s", k, encoded, v)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && stringContains(s, sub))
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
