package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunAppStdioSmoke proves `yanshi app` drives the full JSON-RPC stdio
// lifecycle end-to-end without spawning a subprocess: initialize → shutdown.
// Every stdout line must be valid JSON-RPC, the first response must echo the
// initialize id (1), and the last must echo the shutdown id (2). The whole
// exchange uses FakeModel + a temp SQLite db so no external dependencies are
// required.
func TestRunAppStdioSmoke(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := strings.ReplaceAll(filepath.Join(dir, "test.db"), "\\", "/")
	cfg := fmt.Sprintf(`
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "%s"
`, dbPath)
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}`,
		"",
	}, "\n")
	var out bytes.Buffer
	code := runApp([]string{"-config", cfgPath, "-fake-model"}, strings.NewReader(input), &out)
	require.Equal(t, exitOK, code, "stdout = %q", out.String())

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "expected at least 2 responses, got %q", out.String())
	for i, ln := range lines {
		var msg map[string]any
		if err := json.Unmarshal([]byte(ln), &msg); err != nil {
			t.Fatalf("line %d not valid JSON-RPC: %v (%q)", i, err, ln)
		}
		if msg["jsonrpc"] != "2.0" {
			t.Fatalf("line %d missing jsonrpc field: %q", i, ln)
		}
	}
	var init map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &init))
	assert.Equal(t, float64(1), init["id"], "first response must echo initialize id")
	result, _ := init["result"].(map[string]any)
	require.NotNil(t, result, "initialize missing result: %s", lines[0])
	assert.Equal(t, "v1", result["version"], "initialize result.version must be v1")

	var shutdown map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &shutdown))
	assert.Equal(t, float64(2), shutdown["id"], "last response must echo shutdown id")
}

// TestRunAppRejectsPositional proves the `app` subcommand takes no positional
// args — only flags. A stray positional returns exitUsage without touching
// the network or DB.
func TestRunAppRejectsPositional(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("server:\n  http_addr: \"127.0.0.1:0\"\nstorage:\n  sqlite_path: \""+strings.ReplaceAll(filepath.Join(dir, "x.db"), "\\", "/")+"\"\n"), 0o644))

	code := runApp([]string{"-config", cfgPath, "-fake-model", "bogus"}, strings.NewReader(""), &bytes.Buffer{})
	if code != exitUsage {
		t.Fatalf("positional arg code = %d, want %d", code, exitUsage)
	}
}
