package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/lockfile"
)

// findCheck returns the named check from a report, failing the test if absent.
// Tests look checks up by name (not by index) so adding checks in later tasks
// does not destabilize earlier assertions.
func findCheck(t *testing.T, rep DoctorReport, name string) CheckResult {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q not found in report: %+v", name, rep.Checks)
	return CheckResult{}
}

// writeTempConfig writes body to a fresh temp config.yaml and returns its path.
func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestRunDoctor_HappyPath(t *testing.T) {
	dir := t.TempDir()
	cfgBody := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
llm:
  providers:
    - { name: "openai", kind: "openai", model: "gpt-4o", api_key: "env://OPENAI_API_KEY_DOCTOR_TEST" }
vcs:
  worktree_dir: %q
auth:
  legacy_insecure: false
`, filepath.Join(dir, "yanshi.db"), filepath.Join(dir, "wt"))
	cfgPath := writeTempConfig(t, cfgBody)

	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: cfgPath, Root: t.TempDir()})

	// Task 2's RunDoctor emits these 5 checks; Task 3 adds acp/lockfile/port
	// (covered by TestRunDoctor_IncludesEnvChecks). Look up by name, not index,
	// so adding checks later does not destabilize this test.
	for _, name := range []string{"config", "database", "providers", "directories", "sandbox"} {
		_ = findCheck(t, rep, name)
	}
	if c := findCheck(t, rep, "config"); c.Status != StatusOK {
		t.Errorf("config: got %s (%s)", c.Status, c.Message)
	}
	if c := findCheck(t, rep, "database"); c.Status != StatusOK {
		t.Errorf("database: got %s (%s)", c.Status, c.Message)
	}
	if c := findCheck(t, rep, "providers"); c.Status != StatusOK {
		t.Errorf("providers: got %s (%s)", c.Status, c.Message)
	}
	if c := findCheck(t, rep, "sandbox"); c.Status != StatusWarn {
		t.Errorf("sandbox: got %s, want warn", c.Status)
	}
}

func TestRunDoctor_ConfigLoadFailsDowngradesDeps(t *testing.T) {
	rep := RunDoctor(context.Background(), DoctorOptions{
		ConfigPath: filepath.Join(t.TempDir(), "does-not-exist.yaml"),
		Root:       t.TempDir(),
	})
	if c := findCheck(t, rep, "config"); c.Status != StatusFail {
		t.Errorf("config: got %s, want fail", c.Status)
	}
	// Dependent checks must degrade to warn (skipped), never panic or fail.
	// (port is added in Task 3; the same skipped() path it shares is exercised
	// by its own unit test, so it is not asserted here.)
	for _, name := range []string{"database", "providers", "directories"} {
		c := findCheck(t, rep, name)
		if c.Status != StatusWarn {
			t.Errorf("%s: got %s (%s), want warn (skipped)", name, c.Status, c.Message)
		}
	}
}

func TestCheckProviders_NoProviders(t *testing.T) {
	c := checkProviders(&config.Config{}, nil)
	if c.Status != StatusWarn {
		t.Errorf("got %s, want warn", c.Status)
	}
}

func TestCheckProviders_MissingKeyIsFailAndRedacted(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Providers: []config.ProviderConfig{
		{Name: "openai", Kind: "openai", Model: "gpt-4o", APIKey: "sk-secret-value-xyz"},
		{Name: "claude", Kind: "anthropic", Model: "claude-opus-4-8"}, // no key
	}}}
	c := checkProviders(cfg, nil)
	if c.Status != StatusFail {
		t.Errorf("got %s, want fail", c.Status)
	}
	if strings.Contains(c.Message, "sk-secret-value-xyz") {
		t.Errorf("api key leaked into message: %s", c.Message)
	}
	if !strings.Contains(c.Message, "api_key not set") {
		t.Errorf("want 'api_key not set' in message, got: %s", c.Message)
	}
}

func TestCheckProviders_UnknownKind(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Providers: []config.ProviderConfig{
		{Name: "x", Kind: "weird", Model: "m", APIKey: "k"},
	}}}
	c := checkProviders(cfg, nil)
	if c.Status != StatusFail {
		t.Errorf("got %s, want fail", c.Status)
	}
	if !strings.Contains(c.Message, "unknown kind") {
		t.Errorf("want 'unknown kind', got: %s", c.Message)
	}
}

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		name   string
		checks []CheckResult
		want   int
	}{
		{"empty", nil, 0},
		{"only ok", []CheckResult{{Status: StatusOK}}, 0},
		{"warn", []CheckResult{{Status: StatusWarn}}, 1},
		{"fail", []CheckResult{{Status: StatusFail}}, 2},
		{"ok+warn", []CheckResult{{Status: StatusOK}, {Status: StatusWarn}}, 1},
		{"warn+fail", []CheckResult{{Status: StatusWarn}, {Status: StatusFail}}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DoctorReport{Checks: tc.checks}.ExitCode()
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestRenderText_Format(t *testing.T) {
	rep := DoctorReport{Checks: []CheckResult{
		{Name: "config", Status: StatusOK, Message: "loaded"},
		{Name: "sandbox", Status: StatusWarn, Message: "deferred"},
		{Name: "database", Status: StatusFail, Message: "boom"},
	}}
	var buf bytes.Buffer
	rep.RenderText(&buf)
	out := buf.String()
	for _, want := range []string{"[OK]", "[WARN]", "[FAIL]", "config", "sandbox", "database", "1 ok, 1 warn, 1 fail"} {
		if !strings.Contains(out, want) {
			t.Errorf("render text missing %q:\n%s", want, out)
		}
	}
}

func TestRenderJSON_Structure(t *testing.T) {
	rep := DoctorReport{Checks: []CheckResult{
		{Name: "config", Status: StatusOK, Message: "loaded"},
		{Name: "sandbox", Status: StatusWarn, Message: "deferred"},
	}}
	var buf bytes.Buffer
	if err := rep.RenderJSON(&buf); err != nil {
		t.Fatalf("render json: %v", err)
	}
	var got struct {
		Checks  []CheckResult `json:"checks"`
		Summary struct {
			OK   int `json:"ok"`
			Warn int `json:"warn"`
			Fail int `json:"fail"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(got.Checks) != 2 {
		t.Fatalf("checks: got %d", len(got.Checks))
	}
	if got.Summary.OK != 1 || got.Summary.Warn != 1 || got.Summary.Fail != 0 {
		t.Errorf("summary: got ok=%d warn=%d fail=%d", got.Summary.OK, got.Summary.Warn, got.Summary.Fail)
	}
}

func TestCheckACP_RunsAndMentionsAgents(t *testing.T) {
	c := checkACP()
	if c.Name != "acp" {
		t.Fatalf("name = %q", c.Name)
	}
	// Status is environment-dependent (npx/opencode may or may not be on PATH),
	// so only assert it ran and mentioned each known agent.
	for _, want := range []string{"claudecode", "codex", "opencode"} {
		if !strings.Contains(c.Message, want) {
			t.Errorf("acp message missing %q: %s", want, c.Message)
		}
	}
}

func TestCheckLockfile_Absent(t *testing.T) {
	c := checkLockfile(t.TempDir())
	if c.Status != StatusOK {
		t.Errorf("got %s (%s), want ok", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "no lockfile") {
		t.Errorf("want 'no lockfile', got: %s", c.Message)
	}
}

func TestCheckLockfile_Stale(t *testing.T) {
	root := t.TempDir()
	// A PID at int32 max is vanishingly unlikely to be alive on any platform.
	if err := lockfile.Write(root, lockfile.Lockfile{
		PID: 2147483647, Addr: "127.0.0.1:9", Auth: "none", Root: root,
	}); err != nil {
		t.Fatalf("write lockfile: %v", err)
	}
	c := checkLockfile(root)
	if c.Status != StatusWarn {
		t.Errorf("got %s (%s), want warn", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "stale") {
		t.Errorf("want 'stale', got: %s", c.Message)
	}
}

func TestCheckPort_FreeAndInUse(t *testing.T) {
	// Free: an ephemeral port we can bind.
	free := checkPort(&config.Config{Server: config.ServerConfig{HTTPAddr: "127.0.0.1:0"}}, nil, false)
	if free.Status != StatusOK {
		t.Errorf("free port: got %s (%s), want ok", free.Status, free.Message)
	}

	// In use: bind a listener, then ask checkPort to bind the same addr.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	inUse := checkPort(&config.Config{Server: config.ServerConfig{HTTPAddr: ln.Addr().String()}}, nil, false)
	if inUse.Status != StatusWarn {
		t.Errorf("in-use port: got %s (%s), want warn", inUse.Status, inUse.Message)
	}
}

func TestRunDoctor_IncludesEnvChecks(t *testing.T) {
	dir := t.TempDir()
	cfgBody := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
vcs:
  worktree_dir: %q
`, filepath.Join(dir, "yanshi.db"), filepath.Join(dir, "wt"))
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: writeTempConfig(t, cfgBody), Root: t.TempDir()})
	// Task 3 adds these three; all must be present and non-failing on a clean box.
	for _, name := range []string{"acp", "lockfile", "port"} {
		c := findCheck(t, rep, name)
		if c.Status == StatusFail {
			t.Errorf("%s: unexpected fail %s", name, c.Message)
		}
	}
}

func TestRunDoctor_IncludesLSPCheck(t *testing.T) {
	dir := t.TempDir()
	dbPath := strings.ReplaceAll(filepath.Join(dir, "test.db"), "\\", "/")
	cfgPath := writeTempConfig(t, `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "`+dbPath+`"
token: "test-token"
`)
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: cfgPath, Root: t.TempDir()})
	c := findCheck(t, rep, "lsp")
	// Status ok/warn both legal (depends on whether gopls is installed).
	// Message should mention "go" or "disabled".
	if !strings.Contains(strings.ToLower(c.Message), "go") {
		t.Errorf("lsp check message should mention go, got: %s", c.Message)
	}
}

func TestRunDoctor_IncludesObservabilityChecks(t *testing.T) {
	dir := t.TempDir()
	cfgBody := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
llm:
  providers:
    - { name: "openai", kind: "openai", model: "gpt-4o", api_key: "env://OPENAI_API_KEY_DOCTOR_TEST" }
vcs:
  worktree_dir: %q
auth:
  legacy_insecure: false
`, filepath.Join(dir, "yanshi.db"), filepath.Join(dir, "wt"))
	cfgPath := writeTempConfig(t, cfgBody)
	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: cfgPath, Root: t.TempDir()})
	for _, name := range []string{"mcp", "lsp", "permissions"} {
		c := findCheck(t, rep, name)
		if c.Status != StatusOK && c.Status != StatusWarn {
			t.Errorf("%s: got %s (%s), want ok or warn", name, c.Status, c.Message)
		}
	}
	// Sandbox must remain warn (S08 not done).
	if c := findCheck(t, rep, "sandbox"); c.Status != StatusWarn {
		t.Errorf("sandbox: got %s, want warn", c.Status)
	}
}

func TestCheckMCP_ServersListedOrNoneConfigured(t *testing.T) {
	c := checkMCP(&config.Config{}, nil)
	if c.Status != StatusOK || !strings.Contains(c.Message, "no mcp servers") {
		t.Errorf("default: got %s (%s)", c.Status, c.Message)
	}
}

func TestCheckLSP_ReportsProbes(t *testing.T) {
	c := checkLSP(context.Background(), t.TempDir())
	if c.Status != StatusOK && c.Status != StatusWarn {
		t.Errorf("got %s (%s)", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "lsp") {
		t.Errorf("expected 'lsp' in message: %s", c.Message)
	}
}

func TestCheckPermissions_ProfilesAndInteractiveMode(t *testing.T) {
	c := checkPermissions(&config.Config{
		Profiles: map[string]guard.PermissionProfile{
			"coding": {Tools: guard.ToolsPerm{Allow: []string{"fs_read", "fs_write"}}},
		},
	}, nil)
	if c.Status != StatusOK && c.Status != StatusWarn {
		t.Errorf("got %s (%s)", c.Status, c.Message)
	}
	if !strings.Contains(c.Message, "coding") {
		t.Errorf("expected profile name in message: %s", c.Message)
	}
}

func TestCheckPermissions_ConfigLoadSkipped(t *testing.T) {
	c := checkPermissions(nil, errors.New("missing"))
	if c.Status != StatusWarn || !strings.Contains(c.Message, "skipped") {
		t.Errorf("got %s (%s)", c.Status, c.Message)
	}
}
