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

// TestCheckLSP_ReportsProbes asserts the check DISTINGUISHES workspaces.
//
// It previously required the literal string "lsp" in the message, which any
// wording satisfies and which the old one-binary probe satisfied while
// answering a question nobody has -- gopls being installed says nothing about
// whether yanshi will use it here. Since W6 there is a second gate (a
// workspace marker), so "installed" and "will run here" are different facts.
//
// An empty directory and a Go module must therefore not produce the same
// answer. If the toolchain under test has no language server installed at all
// both are the warn case, and the test says so rather than pretending to have
// checked.
func TestCheckLSP_ReportsProbes(t *testing.T) {
	empty := checkLSP(context.Background(), t.TempDir())
	if empty.Status != StatusOK && empty.Status != StatusWarn {
		t.Errorf("got %s (%s)", empty.Status, empty.Message)
	}

	goRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(goRoot, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withMod := checkLSP(context.Background(), goRoot)

	if withMod.Status == StatusWarn && empty.Status == StatusWarn {
		t.Skip("no language server installed in this environment: both cases are the warn path")
	}
	if withMod.Message == empty.Message {
		t.Fatalf("a Go module and an empty directory report identically (%q): "+
			"the check is not looking at the workspace", empty.Message)
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

// TestCheckSandboxReportsTheRuntimePosture pins the replacement of a constant.
//
// checkSandbox returned a fixed "not implemented yet" warning regardless of
// configuration: an operator running with tier full-access and one on the
// default got the same line, and neither learned whether OS isolation was
// enforced. It was honest about itself and silent about the system.
//
// The assertions are about DISTINGUISHING configurations, not about matching
// exact prose -- a check that cannot tell two postures apart is the defect,
// whatever it prints.
func TestCheckSandboxReportsTheRuntimePosture(t *testing.T) {
	root := t.TempDir()

	t.Run("disabled is called out specifically", func(t *testing.T) {
		off := false
		cfg := &config.Config{}
		cfg.Security.Sandbox.Enabled = &off
		got := checkSandbox(cfg, nil, root)
		if got.Status != StatusWarn {
			t.Fatalf("status = %v", got.Status)
		}
		if !strings.Contains(got.Message, "disabled") {
			t.Fatalf("a disabled sandbox must say so: %q", got.Message)
		}
	})

	t.Run("different tiers produce different reports", func(t *testing.T) {
		mk := func(tier string) CheckResult {
			cfg := &config.Config{}
			cfg.Security.Sandbox.Tier = tier
			return checkSandbox(cfg, nil, root)
		}
		ro := mk("read-only")
		full := mk("full-access")
		if ro.Message == full.Message {
			t.Fatalf("read-only and full-access report identically (%q): "+
				"the check is not reading the configuration", ro.Message)
		}
	})

	t.Run("an unknown tier reports the fallback, not the typo", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Security.Sandbox.Tier = "compleeetly-wrong"
		got := checkSandbox(cfg, nil, root)
		if strings.Contains(got.Message, "compleeetly-wrong") {
			t.Fatalf("doctor echoed the config string instead of what the runtime will use: %q",
				got.Message)
		}
		if !strings.Contains(got.Message, "read-only") {
			t.Fatalf("an unrecognised tier falls back to read-only and doctor must show that: %q",
				got.Message)
		}
	})

	t.Run("a config error skips rather than guesses", func(t *testing.T) {
		got := checkSandbox(nil, errors.New("boom"), root)
		if got.Status != StatusWarn || !strings.Contains(got.Message, "skipped") {
			t.Fatalf("a config error must skip rather than guess a posture: %+v", got)
		}
	})
}

// TestCheckMCPReadsTheConfiguration replaces a constant with a report.
//
// checkMCP returned "no mcp servers exposed via chat" whatever the config
// said -- false for anyone who configured one, since the tools bridge
// registers every ready server's tools into the chat tool set. Someone
// debugging a server that would not connect was told none were expected.
//
// The unusable case is a FAIL rather than a warning on purpose: a stdio entry
// with no command, or an http entry with no url, cannot start at any boot.
// It is a configuration error, and the runtime's own failure message names
// the transport rather than the missing field.
func TestCheckMCPReadsTheConfiguration(t *testing.T) {
	mk := func(servers map[string]*config.MCPServerConfig) CheckResult {
		cfg := &config.Config{}
		cfg.MCP.Servers = servers
		return checkMCP(cfg, nil)
	}

	if got := mk(nil); got.Status != StatusOK || !strings.Contains(got.Message, "no mcp servers") {
		t.Errorf("empty config: %+v", got)
	}

	got := mk(map[string]*config.MCPServerConfig{
		"files": {Enabled: true, Command: "mcp-files"},
		"old":   {Enabled: false, Command: "mcp-old"},
	})
	if got.Status != StatusOK {
		t.Fatalf("a coherent config must not warn: %+v", got)
	}
	if !strings.Contains(got.Message, "files") || !strings.Contains(got.Message, "old") {
		t.Fatalf("both servers must appear: %q", got.Message)
	}
	if !strings.Contains(got.Message, "disabled") {
		t.Fatalf("a disabled server must be labelled, not just listed: %q", got.Message)
	}

	broken := mk(map[string]*config.MCPServerConfig{
		"nocmd": {Enabled: true},
	})
	if broken.Status != StatusFail {
		t.Fatalf("a stdio server with no command can never start; want fail, got %+v", broken)
	}
	if !strings.Contains(broken.Message, "nocmd") {
		t.Fatalf("the failure must name the server: %q", broken.Message)
	}
}

// TestDoctorNeverEchoesACredential is the canary for a property doctor was
// getting right by construction and nothing was holding it to.
//
// Five call sites deliberately withhold detail so a credential cannot reach
// the report: checkConfig and skipped() drop cfgErr entirely (a YAML parse
// failure routinely quotes the offending line, which is where the api_key
// is), checkProviders reports only set/not set, checkSecretsRefs says only
// "invalid credential reference", and checkKeymapConfig does not echo the
// raw key or action. Every one of those is a decision someone made, and any
// of them could be "improved" into a leak by a well-meaning edit that adds
// %v to an error message.
//
// Both renderers are asserted because they are independent code paths --
// testing one protects half the surface. The config deliberately uses
// legacy_insecure with a raw literal, which is the only shape where a real
// credential sits in the config file in plaintext, and it is also the shape
// most likely to make a parse error quote it.
func TestDoctorNeverEchoesACredential(t *testing.T) {
	const canary = "sk-CANARY-3f9d1a7c4b2e8065-DO-NOT-LEAK"

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "" +
		"auth:\n  legacy_insecure: true\n" +
		"llm:\n  providers:\n    - name: p1\n      type: openai\n      api_key: \"" + canary + "\"\n" +
		"storage:\n  sqlite_path: \"" + strings.ReplaceAll(filepath.Join(dir, "d.db"), "\\", "/") + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: cfgPath})

	var text bytes.Buffer
	rep.RenderText(&text)
	if strings.Contains(text.String(), canary) {
		t.Errorf("RenderText leaked the credential:\n%s", text.String())
	}

	var jsonOut bytes.Buffer
	if err := rep.RenderJSON(&jsonOut); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jsonOut.String(), canary) {
		t.Errorf("RenderJSON leaked the credential:\n%s", jsonOut.String())
	}

	// A malformed config is the other half: the parse error is the most
	// likely carrier, and checkConfig drops it on purpose.
	badPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badPath, []byte("llm: [\n  api_key: \""+canary+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	badRep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: badPath})

	var badText, badJSON bytes.Buffer
	badRep.RenderText(&badText)
	if err := badRep.RenderJSON(&badJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(badText.String(), canary) {
		t.Errorf("a parse failure leaked the credential through RenderText:\n%s", badText.String())
	}
	if strings.Contains(badJSON.String(), canary) {
		t.Errorf("a parse failure leaked the credential through RenderJSON:\n%s", badJSON.String())
	}
}

// TestDoctorSurvivesAnIncompleteEnvironment pins the degradation skipped()
// provides: every check must return a result rather than panic when the
// config never loaded. doctor is what an operator runs WHEN things are
// broken, so a panic here removes the tool exactly when it is needed.
func TestDoctorSurvivesAnIncompleteEnvironment(t *testing.T) {
	for _, path := range []string{
		filepath.Join(t.TempDir(), "does-not-exist.yaml"),
		filepath.Join(t.TempDir(), "empty.yaml"),
	} {
		if strings.HasSuffix(path, "empty.yaml") {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		rep := RunDoctor(context.Background(), DoctorOptions{ConfigPath: path})
		if len(rep.Checks) == 0 {
			t.Fatalf("%s: doctor produced no checks at all", path)
		}
		var b bytes.Buffer
		rep.RenderText(&b)
		if err := rep.RenderJSON(&b); err != nil {
			t.Fatalf("%s: RenderJSON: %v", path, err)
		}
	}
}
