package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/secrets"
)

// TestRunVCSMCP_InitializeSmoke drives the testable core of the vcs-mcp
// subcommand (runVCSMCP) against a temp SQLite db: it pipes an MCP initialize
// request and asserts a well-formed initialize response advertising the
// yanshi-vcs server. This proves the env→store→vcs→mcp.Serve wiring of the
// spawnable entry point without spawning a subprocess.
func TestRunVCSMCP_InitializeSmoke(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "vcs-mcp-smoke.db")
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n")
	out := &bytes.Buffer{}

	err := runVCSMCP(
		context.Background(),
		dbPath,
		"repo-smoke",
		"wt-smoke",
		"acp",
		t.TempDir(),
		in,
		out,
	)
	require.NoError(t, err)

	// Serve writes one JSON line per request; pull the first non-blank line.
	var respLine []byte
	for _, l := range bytes.Split(out.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			respLine = l
			break
		}
	}
	require.NotEmpty(t, respLine, "expected an initialize response line")

	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			ProtocolVersion string            `json:"protocolVersion"`
			Capabilities    map[string]any    `json:"capabilities"`
			ServerInfo      map[string]string `json:"serverInfo"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(respLine, &resp))
	assert.Equal(t, "2.0", resp.JSONRPC)
	assert.JSONEq(t, `1`, string(resp.ID))
	assert.Contains(t, resp.Result.Capabilities, "tools")
	assert.Equal(t, "yanshi-vcs", resp.Result.ServerInfo["name"])
}

// TestRunVCSMCP_ToolsListSmoke verifies the vcs-mcp entry point also serves a
// tools/list request (proving the full store→vcs→mcp path, not just initialize).
func TestRunVCSMCP_ToolsListSmoke(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "vcs-mcp-tools.db")
	in := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}, "\n") + "\n")
	out := &bytes.Buffer{}

	require.NoError(t, runVCSMCP(
		context.Background(),
		dbPath, "repo", "wt", "acp", t.TempDir(), in, out,
	))

	var listResp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	require.Len(t, lines, 2, "expected one response per request")
	require.NoError(t, json.Unmarshal(lines[1], &listResp))
	names := map[string]bool{}
	for _, tl := range listResp.Result.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"vcs_commit", "vcs_log", "vcs_diff", "vcs_restore", "vcs_merge"} {
		assert.True(t, names[want], "tools/list must advertise %s", want)
	}
}

func TestRunDoctor_JSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := filepath.Join(dir, "yanshi.db")
	wtPath := filepath.Join(dir, "wt")
	cfgYAML := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
llm:
  providers:
    - { name: "openai", kind: "openai", model: "gpt-4o", api_key: "env://OPENAI_API_KEY_DOCTOR_JSON_TEST" }
vcs:
  worktree_dir: %q
auth:
  legacy_insecure: false
`, dbPath, wtPath)
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgYAML), 0o644))

	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	code := runDoctor(context.Background(), []string{"-config", cfgPath, "-json"}, out, errOut)

	// sandbox is always warn, so a clean config yields exit 1 (warn) — never 0
	// and never 2 (fail) on a well-formed config.
	assert.Equal(t, 1, code)

	var rep struct {
		Checks []struct {
			Name, Status, Message string
		} `json:"checks"`
		Summary struct {
			OK, Warn, Fail int
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &rep))

	names := map[string]bool{}
	for _, c := range rep.Checks {
		names[c.Name] = true
		// Secret redaction: the api key must never appear in any message.
		assert.NotContains(t, c.Message, "sk-test-not-a-real-key")
	}
	for _, want := range []string{"config", "database", "providers", "acp", "lockfile", "port", "directories", "sandbox"} {
		assert.True(t, names[want], "doctor report missing %q", want)
	}
	assert.GreaterOrEqual(t, rep.Summary.Warn, 1, "sandbox should contribute at least one warn")
}

func TestRunDoctor_TextNotJSONByDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgYAML := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
vcs: { worktree_dir: %q }
`, filepath.Join(dir, "yanshi.db"), filepath.Join(dir, "wt"))
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgYAML), 0o644))

	out := &bytes.Buffer{}
	code := runDoctor(context.Background(), []string{"-config", cfgPath}, out, &bytes.Buffer{})
	assert.Equal(t, 1, code)
	// Human-readable form uses [OK]/[WARN]/[FAIL] tags, not JSON braces.
	assert.Contains(t, out.String(), "[WARN]")
	assert.NotContains(t, out.String(), `"checks"`)
}

// TestMainAuth_E2E exercises the `yanshi auth` subcommand end-to-end via the
// main entry: status, logout, and the sibling-isolation property of a failed
// logout. Uses ONLY file-backed storage (no real keyring).
//
// Credentials are seeded through secrets.Manager rather than `auth set`,
// which no longer exists: provider api_keys live in config.yaml as literals
// or ${VAR}, so the only thing this store still holds is device-flow tokens.
func TestMainAuth_E2E(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YANSHI_PASSPHRASE", "e2e-pass")
	secretPath := filepath.Join(dir, "secrets.enc")
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgBody := fmt.Sprintf(`
storage:
  sqlite_path: %q
secrets:
  backend: file
  file_path: %q
  passphrase_env: YANSHI_PASSPHRASE
`, filepath.Join(dir, "auth.db"), secretPath)
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0600); err != nil {
		t.Fatal(err)
	}

	// 1) Seed two accounts directly in the backend.
	seed := func(account, value string) {
		t.Helper()
		smgr, err := secrets.NewManager(secrets.Config{
			Backend: "file", FilePath: secretPath, PassphraseEnv: "YANSHI_PASSPHRASE",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := smgr.Store().Set("openai", account, value); err != nil {
			t.Fatal(err)
		}
		if err := smgr.Close(); err != nil {
			t.Fatal(err)
		}
	}
	seed("main", "tok-e2e-main")
	seed("sibling", "tok-e2e-sibling")

	// 2) Status reports Authenticated.
	var out bytes.Buffer
	code := runCLI([]string{"yanshi", "--config", cfgPath, "auth", "status",
		"--provider", "openai", "--account", "main"}, nil, &out)
	if code != 0 || !strings.Contains(out.String(), "Authenticated: true") {
		t.Fatalf("auth status: code=%d out=%s", code, out.String())
	}

	// 3) Logout removes the credential.
	out.Reset()
	code = runCLI([]string{"yanshi", "--config", cfgPath, "auth", "logout",
		"--provider", "openai", "--account", "main"}, nil, &out)
	if code != 0 {
		t.Fatalf("auth logout: code=%d out=%s", code, out.String())
	}
	out.Reset()
	code = runCLI([]string{"yanshi", "--config", cfgPath, "auth", "status",
		"--provider", "openai", "--account", "main"}, nil, &out)
	if strings.Contains(out.String(), "Authenticated: true") {
		t.Fatalf("after logout, status still reports Authenticated: %s", out.String())
	}

	// 4) Logout of a missing account returns non-zero but does not corrupt the
	//    existing entry of a sibling account.
	out.Reset()
	code = runCLI([]string{"yanshi", "--config", cfgPath, "auth", "logout",
		"--provider", "openai", "--account", "nonexistent"}, nil, &out)
	if code == 0 {
		t.Fatal("logout nonexistent must exit non-zero")
	}
	out.Reset()
	code = runCLI([]string{"yanshi", "--config", cfgPath, "auth", "status",
		"--provider", "openai", "--account", "sibling"}, nil, &out)
	if !strings.Contains(out.String(), "Authenticated: true") {
		t.Fatalf("sibling corrupted by failed logout: %s", out.String())
	}
}

type authCLITestClock struct{ now time.Time }

func (c authCLITestClock) Now() time.Time { return c.now }

type authCLITestSleeper struct{}

func (authCLITestSleeper) Sleep(context.Context, time.Duration) error { return nil }

func TestMainAuth_DeviceE2E_ReopensPersistedToken(t *testing.T) {
	const accessToken = "device-access-sentinel"
	const refreshToken = "device-refresh-sentinel"
	var tokenPolls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":      "device-code-sentinel",
				"user_code":        "USER-CODE",
				"verification_uri": "https://example.com/device",
				"expires_in":       300,
				"interval":         1,
			})
		case "/token":
			tokenPolls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  accessToken,
				"refresh_token": refreshToken,
				"expires_in":    120,
				"token_type":    "bearer",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("YANSHI_PASSPHRASE", "device-pass")
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgBody := fmt.Sprintf(`
storage:
  sqlite_path: %q
secrets:
  backend: file
  file_path: %q
  passphrase_env: YANSHI_PASSPHRASE
auth:
  device:
    device_auth_enabled: true
    providers:
      - id: loopback
        client_id: test-client
        device_url: %q
        token_url: %q
`, filepath.Join(dir, "auth.db"), filepath.Join(dir, "secrets.enc"),
		srv.URL+"/device", srv.URL+"/token")
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runCLIWithAuthDeps(
		[]string{"yanshi", "--config", cfgPath, "auth", "device",
			"--provider", "loopback", "--account", "main"},
		strings.NewReader(""), &stdout, &stderr,
		authCLIDeps{
			Clock:   authCLITestClock{now: time.Unix(1000, 0)},
			Sleeper: authCLITestSleeper{},
		},
	)
	if code != 0 || tokenPolls != 1 {
		t.Fatalf("device flow: code=%d polls=%d stdout=%q stderr=%q",
			code, tokenPolls, stdout.String(), stderr.String())
	}
	for _, secret := range []string{
		accessToken, refreshToken, "device-code-sentinel",
	} {
		if strings.Contains(stdout.String(), secret) ||
			strings.Contains(stderr.String(), secret) {
			t.Fatalf("device CLI leaked %q: stdout=%q stderr=%q",
				secret, stdout.String(), stderr.String())
		}
	}

	// A fresh dispatcher invocation constructs a fresh Manager/FileStore and
	// proves the token is visible after process-style reopen, not merely in the
	// first Manager's in-memory snapshot.
	stdout.Reset()
	stderr.Reset()
	code = runCLI([]string{"yanshi", "--config", cfgPath, "auth", "status",
		"--provider", "loopback", "--account", "main"}, nil, &stdout)
	if code != 0 || !strings.Contains(stdout.String(), "Authenticated: true") {
		t.Fatalf("reopened status: code=%d stdout=%q stderr=%q",
			code, stdout.String(), stderr.String())
	}

	// Final security gate: scan the SQLite main + WAL files for any of the
	// device-flow secret sentinels. auth_metadata is allowed only
	// provider/account/source/expires_at/updated_at; if any of the secret
	// values land in the file, that proves a leak in the metadata adapter
	// or somewhere else along the persist path.
	dbPath := filepath.Join(dir, "auth.db")
	for _, path := range []string{dbPath, dbPath + "-wal"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		for _, forbidden := range []string{
			accessToken,
			refreshToken,
			"device-code-sentinel",
			"USER-CODE",
			"client-secret-value",
		} {
			if bytes.Contains(raw, []byte(forbidden)) {
				t.Fatalf("SQLite file %s contains secret %q", path, forbidden)
			}
		}
	}
}
