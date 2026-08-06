package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// appConfigFixture writes a minimal config.yaml and returns its path.
func appConfigFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := strings.ReplaceAll(filepath.Join(dir, "test.db"), "\\", "/")
	require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf(`
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "%s"
`, dbPath)), 0o644))
	return cfgPath
}

// rpcExchange drives one `yanshi app` lifetime and returns the responses by id.
func rpcExchange(t *testing.T, cfgPath string, requests ...string) map[float64]map[string]any {
	t.Helper()
	input := strings.Join(append(requests, ""), "\n")
	var out bytes.Buffer
	code := runApp([]string{"-config", cfgPath, "-fake-model"}, strings.NewReader(input), &out)
	require.Equal(t, exitOK, code, "stdout = %q", out.String())

	byID := map[float64]map[string]any{}
	for _, ln := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if ln == "" {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(ln), &msg); err != nil {
			t.Fatalf("not valid JSON-RPC: %v (%q)", err, ln)
		}
		if id, ok := msg["id"].(float64); ok {
			byID[id] = msg
		}
	}
	return byID
}

// TestConfigWriteSurvivesRestart is the whole point of `config/write`.
//
// cmd/yanshi/app.go constructed appserver.NewMemoryConfig() UNCONDITIONALLY —
// passing -config changed nothing — so config/write accepted a value, reported
// success, and dropped it when the process exited. docs/api/jsonrpc.md
// described both methods as reading and writing "运行时配置". A supervisor
// following that document would have configured a fleet that silently reset on
// every restart, and the failure is invisible until something reads back.
//
// Two separate runApp lifetimes, not one: a single-process test passes against
// the in-memory backend too, which is exactly why nothing caught this.
func TestConfigWriteSurvivesRestart(t *testing.T) {
	cfgPath := appConfigFixture(t)

	first := rpcExchange(t, cfgPath,
		`{"jsonrpc":"2.0","id":1,"method":"config/write","params":{"key":"ui.theme","value":"dark"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}`)
	require.Nil(t, first[1]["error"], "config/write failed: %v", first[1])

	second := rpcExchange(t, cfgPath,
		`{"jsonrpc":"2.0","id":1,"method":"config/read","params":{"key":"ui.theme"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}`)
	require.Nil(t, second[1]["error"],
		"config/read after restart failed — the write did not reach disk: %v", second[1])

	result, _ := second[1]["result"].(map[string]any)
	require.Equal(t, "dark", result["value"],
		"config/read returned %v after a restart; the value did not persist", result)
}

// TestConfigRejectsSecretPathsOnDisk carries the fail-closed half forward.
//
// The secret-path rule (token/api_key/apikey/secret plus any segment
// containing "password") rejects on BOTH read and write, BEFORE the JSON
// decode, so a malformed secret payload never reaches storage. Persisting the
// backend moved that rule next to a file, where a leak becomes durable rather
// than process-scoped — so the rule is re-asserted against the real transport,
// not just against the backend type in a unit test.
func TestConfigRejectsSecretPathsOnDisk(t *testing.T) {
	cfgPath := appConfigFixture(t)
	for _, key := range []string{"llm.api_key", "auth.TOKEN", "db.db_password", "x.Secret"} {
		resp := rpcExchange(t, cfgPath,
			fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"config/write","params":{"key":%q,"value":"leaked"}}`, key),
			`{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}`)
		require.NotNil(t, resp[1]["error"], "config/write accepted secret path %q", key)
	}
	// Nothing may have been written, not even a rejected key's file entry.
	for _, name := range []string{"config.appstate.json", "config.yaml.appstate.json"} {
		if data, err := os.ReadFile(filepath.Join(filepath.Dir(cfgPath), name)); err == nil {
			require.NotContains(t, string(data), "leaked",
				"a rejected secret write still reached disk in %s", name)
		}
	}
}
