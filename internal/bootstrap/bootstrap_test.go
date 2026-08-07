package bootstrap_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	apiV1 "github.com/x6nux/yanshi/internal/api/v1"
	"github.com/x6nux/yanshi/internal/auth"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/config"
	"github.com/x6nux/yanshi/internal/guard"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	"github.com/x6nux/yanshi/internal/secrets"
	"github.com/x6nux/yanshi/internal/tools"
)

// toYAMLPath converts an OS-native path to a YAML-safe string (forward slashes).
func toYAMLPath(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// buildMinimalApp is the shared COV3 fixture: a minimal config (ephemeral
// port + temp sqlite + token) built with FakeModel so it boots with zero
// external deps. Returns the App and a cleanup that Shutdowns it. Every COV3
// test reuses this so the "does a minimal app boot?" question has one answer.
func buildMinimalApp(t *testing.T) *bootstrap.App {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
	return app
}

// TestBuild_MinimalApp proves a minimal FakeModel app boots and every App
// field documented in spec §3.3 is non-nil. This consolidates the per-field
// assertions currently scattered across TestBuild_RegistersAllSecuritySubsystems
// / TestBuild_LSPWired / TestBuildWiresFeaturesAndPricing into one assembly-
// order gate.
//
// ledger: E1/COV3#2 最小 App 可构建并跑一轮 turn
func TestBuild_MinimalApp(t *testing.T) {
	app := buildMinimalApp(t)
	for _, c := range []struct {
		name string
		v    any
	}{
		{"Server", app.Server}, {"Store", app.Store}, {"Orch", app.Orch},
		{"Broker", app.Broker}, {"Model", app.Model}, {"Skills", app.Skills},
		{"AgentAPI", app.AgentAPI}, {"VCS", app.VCS},
		{"Sandbox", app.Sandbox}, {"NetworkPolicy", app.NetworkPolicy},
		{"Approvals", app.Approvals}, {"ShellManager", app.ShellManager},
		{"SecureFactory", app.SecureFactory},
		{"SubagentManager", app.SubagentManager}, {"AgentTools", app.AgentTools},
		{"LSP", app.LSP}, {"MCP", app.MCP},
		{"Features", app.Features}, {"Redactor", app.Redactor}, {"Auth", app.Auth},
	} {
		assert.NotNilf(t, c.v, "App.%s must be non-nil after Build", c.name)
	}
}

// TestBuild_AssemblyOrder echoes the CLAUDE.md "config→store→vcs→model→tools→
// orchestrator→http server→task broker" order by asserting the downstream
// fields that only exist when their upstream succeeded: Broker needs Store,
// Orch needs Model+Store, AgentAPI needs Store. If any upstream silently
// nil'd, these would be nil/empty.
func TestBuild_AssemblyOrder(t *testing.T) {
	app := buildMinimalApp(t)
	require.NotNil(t, app.Store)
	require.NotNil(t, app.Orch)
	// Orch carries a real profile (not the wildcard fail-open) — guards A1 Task 2.
	prof := app.Orch.Profile()
	assert.NotContains(t, prof.Tools.Allow, "*")
	assert.NotEmpty(t, prof.Tools.Allow)
}

// TestBuild_VCSSoftDegrade drives the degraded path instead of asserting a
// property that holds on the healthy one.
//
// The previous version called buildMinimalApp and checked App.VCS != nil.
// InitRepo SUCCEEDS there — the process cwd is a real directory — so the
// branch the test names had never executed, and the assertion held for a boot
// where nothing degraded at all. Options.WorkRoot exists so a test can point
// the scan at a directory that does not exist, which is what makes
// canonicalRepoRoot's EvalSymlinks fail, reliably, on every platform.
//
// Both halves are asserted: Build must not fail, AND VCSRepoID must be empty.
// Without the second, a boot that quietly succeeded would still pass.
//
// ledger: E1/COV3#3 软降级被验证
func TestBuild_VCSSoftDegrade(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "`+dbPath+`"
token: "test-token"
`), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{
		ConfigPath: cfgPath,
		FakeModel:  true,
		WorkRoot:   filepath.Join(dir, "does-not-exist"),
	})
	require.NoError(t, err, "a VCS init failure must not abort the boot")
	require.NotNil(t, app)
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	require.NotNil(t, app.VCS, "VCS instance must exist even if InitRepo failed")
	require.Empty(t, app.VCSRepoID,
		"InitRepo was pointed at a non-existent directory and still reported a repo: "+
			"the soft-degrade branch did not run, so this test proves nothing")

	// The healthy control. Without it, an InitRepo that failed for EVERY root
	// would satisfy the assertions above.
	healthy := buildMinimalApp(t)
	require.NotEmpty(t, healthy.VCSRepoID,
		"a normal boot produced no repo id; the degraded assertion above is vacuous")
}

// TestBuild_PluginDiscoverySoftDegrade proves a non-existent builtin_dir does
// not abort Build; Skills stays non-nil (possibly empty). Mirrors CLAUDE.md's
// "non-fatal startup failures log to stderr and continue".
func TestBuild_PluginDiscoverySoftDegrade(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
skills:
  builtin_dir: "` + toYAMLPath(filepath.Join(dir, "does-not-exist")) + `"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err, "Build must not fail on plugin discovery error")
	require.NotNil(t, app)
	require.NotNil(t, app.Skills, "Skills must be non-nil even on discovery failure")
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
}

// TestBuild_MCPStartupSoftDegrade proves a bogus MCP server config (command
// does not exist) does not abort Build — the Manager is still non-nil
// (Enabled() may be false if no servers succeeded). Mirrors CLAUDE.md's
// "non-fatal startup failures log to stderr and continue".
func TestBuild_MCPStartupSoftDegrade(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
mcp:
  servers:
    bogus:
      enabled: true
      transport: stdio
      command: "does-not-exist"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	require.NotNil(t, app.MCP, "MCP Manager must be non-nil even on startup failure")
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
}

// TestBuild_OptionsCfgInjection proves Options.Cfg builds from an in-memory
// *config.Config without any YAML file on disk. Secrets/Auth must be set so
// the strict pipeline (D3) does not reject the build.
func TestBuild_OptionsCfgInjection(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		Server:  config.ServerConfig{HTTPAddr: "127.0.0.1:0"},
		Storage: config.StorageConfig{SQLitePath: filepath.Join(dir, "yanshi.db")},
		Secrets: config.SecretsConfig{Backend: "none"},
		Auth:    config.AuthConfig{},
	}
	app, err := bootstrap.Build(bootstrap.Options{Cfg: cfg, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	require.NotNil(t, app.Store)
	require.NotNil(t, app.Orch)
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
}

// TestBuild_OptionsOutputInjection proves Options.Output is adopted by Build
// (the caller's SafeOutput becomes the process redactor/logger). Build always
// makes Redactor non-nil; the injection proof is that the caller-supplied
// SafeOutput is the one in use. We assert Redactor is non-nil AND that the
// custom output didn't cause a crash.
func TestBuild_OptionsOutputInjection(t *testing.T) {
	dir := t.TempDir()
	out := secrets.NewSafeOutput(io.Discard, secrets.NewRedactor())
	cfg := &config.Config{
		Server:  config.ServerConfig{HTTPAddr: "127.0.0.1:0"},
		Storage: config.StorageConfig{SQLitePath: filepath.Join(dir, "yanshi.db")},
		Secrets: config.SecretsConfig{Backend: "none"},
		Auth:    config.AuthConfig{},
	}
	app, err := bootstrap.Build(bootstrap.Options{Cfg: cfg, FakeModel: true, Output: out})
	require.NoError(t, err)
	require.NotNil(t, app.Redactor, "Redactor must be non-nil after Build")
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
}

// TestBuild_EndToEndTurn proves a full user turn runs through a bootstrap-
// assembled stack: Build gives a live App, a scripted FakeModel emits one
// tool_call then a closing assistant message, a rebuilt orchestrator executes
// the tool and feeds the result back, and the iterator yields the tool_result
// and a final agent_chunk. This is the only bootstrap test that drives a real
// ReAct turn end-to-end; it reuses the proven "rebuild orchestrator with a
// scripted model" pattern from TestBuild_VCSToolsRunThroughOrchestrator.
//
// ledger: E1/COV3#2 最小 App 可构建并跑一轮 turn
func TestBuild_EndToEndTurn(t *testing.T) {
	app := buildMinimalApp(t)
	require.NotNil(t, app.Store)

	// Scripted model: (1) call time_now, (2) emit a final message.
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "time_now",
			Arguments: `{}`,
		}},
	})
	step2 := schema.AssistantMessage("done", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	// time_now requires no VCS scope, no params, and is side-effect-free.
	tt := tools.NewTimeTools()
	orchTools := []orchestrator.BaseTool{tt.Now}
	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"time_now"}},
	}
	o, err := orchestrator.New(orchestrator.Config{
		Model:   mdl,
		Tools:   orchTools,
		Profile: profile,
	})
	require.NoError(t, err)

	var sawToolResult, sawAgentChunk bool
	iter := o.Events(context.Background(), "what time is it?")
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		require.NoError(t, ev.Err, "tool must be recognized, not rejected as unknown")
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		mv := ev.Output.MessageOutput
		if mv.IsStreaming && mv.MessageStream != nil {
			msg, err := mv.GetMessage()
			if err != nil || msg == nil {
				continue
			}
			if mv.Role == schema.Tool && mv.ToolName == "time_now" {
				sawToolResult = true
			}
			if mv.Role == schema.Assistant && msg.Content != "" {
				sawAgentChunk = true
			}
			continue
		}
		msg := mv.Message
		if msg == nil {
			continue
		}
		if msg.Role == schema.Tool && mv.ToolName == "time_now" {
			sawToolResult = true
		}
		if msg.Role == schema.Assistant && msg.Content != "" {
			sawAgentChunk = true
		}
	}
	assert.True(t, sawAgentChunk, "turn must produce at least one agent_chunk")
	assert.True(t, sawToolResult, "turn must produce a tool_result for time_now")
}

// TestApp_Shutdown_Idempotent proves Shutdown can be called twice without
// error/panic. Currently Shutdown is only exercised in other tests' defer; a
// double-Shutdown guards against a future regression that closes an already-
// closed server/manager.
func TestApp_Shutdown_Idempotent(t *testing.T) {
	app := buildMinimalApp(t)
	require.NotPanics(t, func() {
		_ = app.Shutdown(context.Background())
	})
	require.NotPanics(t, func() {
		_ = app.Shutdown(context.Background()) // second call must be safe
	})
}

// TestBuild_WithSandboxTier exercises the non-default sandbox tier switch
// branches in Build: workspace-write and full-access. Both must build without
// error even when external isolation is not available (Phase 0).
func TestBuild_WithSandboxTier(t *testing.T) {
	dir := t.TempDir()
	for _, tier := range []string{"workspace-write", "full-access"} {
		cfgPath := filepath.Join(dir, "config-"+tier+".yaml")
		dbPath := toYAMLPath(filepath.Join(dir, "test-"+tier+".db"))
		cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
security:
  sandbox:
    tier: ` + tier + `
`
		require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))
		app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
		require.NoErrorf(t, err, "sandbox tier %q must build", tier)
		require.NotNil(t, app)
		app.Shutdown(context.Background())
	}
}

// TestBuild_ModelRegistry verifies that Build exposes a model map (App.Models)
// built from the configured providers, keyed by the REAL model id (not the
// provider's config name), so sessions switch on concrete model names. With
// ≥2 providers the map keys are the model ids, and the resilient default
// (App.Model) is still non-nil.
func TestBuild_ModelRegistry(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
llm:
  providers:
    - name: "alpha"
      model: "gpt-4o"
      api_key: "fake-alpha"
      base_url: "http://127.0.0.1:9/v1"
    - name: "beta"
      model: "gpt-4o-mini"
      api_key: "fake-beta"
      base_url: "http://127.0.0.1:9/v1"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: false})
	// If the installed eino-ext openai adapter rejects construction in this
	// version, skip rather than fail the suite (mirrors TestBuildProviders_*).
	if err != nil {
		t.Skipf("bootstrap: provider build unavailable in this eino version: %v", err)
	}
	require.NotNil(t, app)
	defer app.Shutdown(context.Background())

	// Models is keyed by the concrete model id; both configured models are present.
	require.NotNil(t, app.Models, "App.Models must be populated when providers are configured")
	assert.Len(t, app.Models, 2, "one entry per provider")
	assert.Contains(t, app.Models, "gpt-4o")
	assert.Contains(t, app.Models, "gpt-4o-mini")
	assert.NotNil(t, app.Models["gpt-4o"], "gpt-4o model must be non-nil")
	assert.NotNil(t, app.Models["gpt-4o-mini"], "gpt-4o-mini model must be non-nil")

	// The default model is the resilient chain over all providers — non-nil and
	// usable as the session default; Models exposes the individuals for switching.
	assert.NotNil(t, app.Model, "App.Model (resilient default) must be non-nil")
}

func TestBuild_FakeModel(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	require.NotNil(t, app.Broker)
	defer app.Shutdown(context.Background())

	// Drive the server handler via httptest.
	ts := httptest.NewServer(app.Server.Handler)
	defer ts.Close()

	// healthz is public (no token required).
	resp, err := ts.Client().Get(ts.URL + "/healthz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Task API is wired: GET /api/v1/agent/profile?worker=w1 with Bearer → 200.
	profReq, _ := http.NewRequest("GET", ts.URL+"/api/v1/agent/profile?worker=w1", nil)
	profReq.Header.Set("Authorization", "Bearer test-token")
	profResp, err := ts.Client().Do(profReq)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, profResp.StatusCode)
	profResp.Body.Close()

	// POST /api/v1/chat with Bearer token returns SSE containing fake response.
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/chat", strings.NewReader(`{"message":"hi"}`))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "(no real model configured)")
}

// TestBuild_OrchestratorProfileFromConfig verifies that when the config
// defines a "profiles.orchestrator" entry, it is wired into the orchestrator
// instead of the hardcoded permissive default.
func TestBuild_OrchestratorProfileFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
profiles:
  orchestrator:
    tools:
      allow: ["*"]
    shell:
      policy: "deny"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	defer app.Shutdown(context.Background())

	// The orchestrator's profile should reflect the config: Shell.Policy="deny".
	prof := app.Orch.Profile()
	assert.Equal(t, "deny", prof.Shell.Policy,
		"orchestrator profile should come from config, not the hardcoded default")
}

// TestBuild_OrchestratorProfileDefault verifies that when no profiles are
// configured, the orchestrator falls back to the permissive default.
func TestBuild_OrchestratorProfileDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	defer app.Shutdown(context.Background())

	// No profiles in config → bootstrap ships a concrete coding profile
	// naming the tools the orchestrator actually uses (Task 2: no more
	// wildcard "*" fail-open). The profile MUST NOT contain "*".
	prof := app.Orch.Profile()
	assert.NotContains(t, prof.Tools.Allow, "*", "must not fall back to wildcard allow")
	assert.True(t, prof.Net.Allow, "default profile should allow network")
	// And it MUST name a real tool — if the list were empty, the guard would
	// HardDeny every call (a different fail-closed footgun).
	assert.NotEmpty(t, prof.Tools.Allow, "must name concrete tools")
}

// TestBuild_WithM7Tools verifies that the new fs/shell/time tool groups wire
// into the orchestrator without error and that Build returns a live App.
func TestBuild_WithM7Tools(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	require.NotNil(t, app.Orch, "orchestrator must be non-nil")
	defer app.Shutdown(context.Background())

	// Confirm the default profile does NOT grant shell or fs write perms
	// (least-privilege default — a later task adds the coding profile).
	prof := app.Orch.Profile()
	assert.Empty(t, prof.Shell.Policy, "default profile should have empty shell policy (deny)")
	assert.Empty(t, prof.FS.Write, "default profile should not grant fs write")
}

// TestBuild_LoadsBuiltinSkills verifies that Build loads the skill registry
// from the configured builtin dir and wires it onto the App. The builtin dir
// points at testdata/skills, a committed fixture containing a single "hi" skill.
func TestBuild_LoadsBuiltinSkills(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	// "testdata/skills" is resolved relative to the package working directory,
	// which is where `go test` runs.
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
skills:
  builtin_dir: "testdata/skills"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	defer app.Shutdown(context.Background())

	require.NotNil(t, app.Skills, "Skills registry must be wired onto App")
	s, ok := app.Skills.Get("hi")
	require.True(t, ok, `builtin skill "hi" should be loaded`)
	assert.Equal(t, "builtin", s.Source, "skill loaded from builtin_dir has builtin source")
	assert.Equal(t, "Use when greeting", s.Description)
}

// TestBuild_LoadsBuiltinSkills_DefaultDir verifies that when no skills dirs
// are configured, Build still succeeds (falling back to the "skills" default
// builtin dir, which simply doesn't exist in the test cwd and loads empty).
func TestBuild_LoadsBuiltinSkills_DefaultDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	require.NotNil(t, app.Skills, "Skills registry must be non-nil even when no skills configured")
	defer app.Shutdown(context.Background())
	assert.Empty(t, app.Skills.List(), "no skills configured → empty registry")
}

// TestBuild_VCSWired verifies that Build constructs the autoVCS tracker over
// the same store, runs InitRepo on the working directory, and wires both onto
// the App. InitRepo scans os.Getwd() (the package directory during `go test`),
// which contains non-ignored files, so it must succeed and produce a repo id.
func TestBuild_VCSWired(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	defer app.Shutdown(context.Background())

	// InitRepo scanned the package working directory (non-ignored files exist),
	// so the tracker must be wired with a non-empty repo id.
	require.NotNil(t, app.VCS, "VCS instance must be wired onto App")
	assert.NotEmpty(t, app.VCSRepoID, "InitRepo must produce a repo id for the workRoot")

	// 2a: the broker must be wired with the VCS + repo id so Claim auto-assigns
	// a worktree to each claimed task.
	require.NotNil(t, app.Broker, "Broker must be built")
	assert.Same(t, app.VCS, app.Broker.VCS, "broker.SetVCS must wire the app's VCS")
	assert.Equal(t, app.VCSRepoID, app.Broker.RepoID, "broker.SetVCS must wire the repo id")

	// The db path + worktree dir are exposed so the goal CLI can describe the
	// yanshi-vcs MCP server to spawned ACP agents.
	assert.Equal(t, dbPath, app.VCSDBPath, "VCSDBPath must echo cfg.Storage.SQLitePath")
	assert.NotEmpty(t, app.WorktreeDir, "WorktreeDir must be resolved to a default")
}

// TestBuild_VCSToolsRunThroughOrchestrator proves the vcs_* GuardedTools that
// bootstrap.Build appends to allTools are real, orchestrator-recognizable tools
// that execute against the VCS scope Build sets up. Build's own FakeModel emits
// only text (no tool calls), so this test rebuilds an orchestrator with the same
// vcs tools Build wires plus a scripted model that emits a vcs_log call, and
// drives it with the app's real VCS scope (app.VCS + app.VCSRepoID). A tool-result
// event for vcs_log carrying real commit data is the proof; an "unknown tool"
// rejection would fail the iteration with an error event.
func TestBuild_VCSToolsRunThroughOrchestrator(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	// Build the real app: this both confirms Build succeeds with the vcs tools
	// appended AND gives us a live VCS scope (InitRepo scans the package cwd).
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	require.NotEmpty(t, app.VCSRepoID, "InitRepo must succeed so the scope is live")
	defer app.Shutdown(context.Background())

	// Scripted model: (1) call vcs_log, (2) emit a final message so ReAct ends.
	step1 := schema.AssistantMessage("", []schema.ToolCall{
		{ID: "c1", Type: "function", Function: schema.FunctionCall{
			Name:      "vcs_log",
			Arguments: `{}`,
		}},
	})
	step2 := schema.AssistantMessage("log shown", nil)
	mdl := einollm.NewFakeModelWithMessages([]*schema.Message{step1, step2}, nil)

	// Rebuild the orchestrator with the SAME vcs tools Build appends, a scripted
	// model, and the app's real main scope. This is the integration surface V17
	// wires; a full Build→chat→vcs_log path is exercised in V22 E2E.
	vcsToolSet := tools.NewVCSTools().Tools()
	orchTools := make([]orchestrator.BaseTool, 0, len(vcsToolSet))
	for _, gt := range vcsToolSet {
		orchTools = append(orchTools, gt)
	}
	profile := guard.PermissionProfile{
		Tools: guard.ToolsPerm{Allow: []string{"vcs_*"}},
	}
	o, err := orchestrator.New(orchestrator.Config{
		Model:    mdl,
		Tools:    orchTools,
		Profile:  profile,
		VCSScope: tools.VCSScope{VCS: app.VCS, RepoID: app.VCSRepoID, Agent: "orchestrator"},
	})
	require.NoError(t, err)

	// Drive the orchestrator and capture the vcs_log tool-result event.
	iter := o.Events(context.Background(), "show the commit log")
	var toolResult string
	var sawLogEvent bool
	var finalAssistant string
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		require.NoError(t, ev.Err, "vcs_log must be recognized, not rejected as unknown")
		if ev.Output == nil || ev.Output.MessageOutput == nil {
			continue
		}
		mv := ev.Output.MessageOutput
		// Streaming variant (EnableStreaming): materialize the message by
		// concatenating its stream so the role/content checks below still work.
		if mv.IsStreaming && mv.MessageStream != nil {
			msg, err := mv.GetMessage()
			if err != nil {
				t.Fatalf("drain stream: %v", err)
			}
			if msg == nil {
				continue
			}
			if mv.Role == schema.Tool && mv.ToolName == "vcs_log" {
				toolResult = msg.Content
				sawLogEvent = true
			}
			if mv.Role == schema.Assistant && msg.Content != "" {
				finalAssistant = msg.Content
			}
			continue
		}
		msg := mv.Message
		if msg == nil {
			continue
		}
		if msg.Role == schema.Tool && mv.ToolName == "vcs_log" {
			toolResult = msg.Content
			sawLogEvent = true
		}
		if msg.Role == schema.Assistant && msg.Content != "" {
			finalAssistant = msg.Content
		}
	}

	require.True(t, sawLogEvent, "expected a vcs_log tool-result event from the runner")
	// The log must carry the InitRepo commit's real data (not an error stub). If
	// the scope were missing, runLog would have returned an error and surfaced it
	// here instead of commit JSON. Commit fields serialize under their Go field
	// names (the vcs.Commit struct has no json tags), so assert "ID" and the
	// init commit message.
	assert.Contains(t, toolResult, `"ID"`, "vcs_log result must carry commit ids")
	assert.Contains(t, toolResult, "vcs init", "vcs_log result must carry the InitRepo commit")
	assert.Equal(t, "log shown", finalAssistant)
}

// TestServe_EphemeralPort_ReachableViaHealthz verifies App.Serve(listener)
// binds a caller-supplied net.Listener (here an ephemeral 127.0.0.1:0 port)
// and serves the same handler tree as Start/ListenAndServe. The caller reads
// back the chosen address from the listener before Serve blocks. Used by the
// in-process CLI path to obtain a free loopback port it can hand to the TUI.
func TestServe_EphemeralPort_ReachableViaHealthz(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	defer app.Shutdown(context.Background())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()

	serveErr := make(chan error, 1)
	go func() { serveErr <- app.Serve(ln) }()

	// Healthz must respond on the ephemeral addr.
	resp, err := http.Get("http://" + addr + "/healthz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Stopping the listener surfaces ErrServerClosed.
	app.Server.Close()
}

// TestBuild_RegistersAllSecuritySubsystems verifies that Build wires every
// A1 security subsystem into the App struct so they are available at runtime.
func TestBuild_RegistersAllSecuritySubsystems(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	if app.Sandbox == nil {
		t.Fatal("sandbox missing")
	}
	if app.NetworkPolicy == nil {
		t.Fatal("netpolicy missing")
	}
	if app.Approvals == nil {
		t.Fatal("approvals missing")
	}
	if app.ShellManager == nil {
		t.Fatal("shell manager missing")
	}
	if app.SecureFactory == nil {
		t.Fatal("secure factory missing")
	}
}

func TestBuild_LSPWired(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	require.NotNil(t, app.LSP, "App.LSP must be non-nil (soft-degrade Manager is also non-nil no-op)")
	defer app.Shutdown(context.Background())

	require.NotPanics(t, func() { _ = app.Shutdown(context.Background()) })
}

func TestBuild_LSPDisabledByConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
lsp:
  enabled: false
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	defer app.Shutdown(context.Background())
	assert.False(t, app.LSP.Enabled(), "enabled:false should disable Manager")
}

// TestBuildWiresFeaturesAndPricing proves the C4 OBS3 / COST1 wiring: the
// YAML features.strict + features.overrides seed App.Features, and the
// pricing.overrides seed App.Pricing (layered on top of DefaultPricing). This
// is the only test that asserts the C4 dependency-injection shape end-to-end.
//
// ledger: C4/COST1#3 价格可配
func TestBuildWiresFeaturesAndPricing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
features:
  strict: true
  overrides:
    observe.cost_in_status: true
pricing:
  overrides:
    custom-model:
      input_per_million: 2
      cache_hit_per_million: 0.2
      output_per_million: 8
`, toYAMLPath(filepath.Join(dir, "yanshi.db")))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Shutdown(context.Background())
	if app.Features == nil || !app.Features.Enabled("observe.cost_in_status") {
		t.Fatalf("features not wired: %#v", app.Features)
	}
	if app.Pricing["custom-model"].InputPerM != 2 {
		t.Fatalf("pricing override not wired: %+v", app.Pricing["custom-model"])
	}
	if app.Pricing["claude-opus-4-8"].InputPerM != 5 {
		t.Fatalf("default pricing missing: %+v", app.Pricing)
	}
}

// TestBuildStrictFeaturesNamesUnknownFlag proves a typo in a strict-mode
// features.overrides map FAILS the boot with a named error. Naming the typo in
// the error message (not just "unknown flag") is load-bearing — operators
// copy-paste from YAML and need to know WHICH key was wrong.
func TestBuildStrictFeaturesNamesUnknownFlag(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`
storage: { sqlite_path: %q }
features:
  strict: true
  overrides:
    typo_observe_flag: true
`, toYAMLPath(filepath.Join(dir, "yanshi.db")))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	if err == nil || !strings.Contains(err.Error(), "typo_observe_flag") {
		t.Fatalf("expected named strict flag error, got %v", err)
	}
}

// TestBuildSetsUpOTelAndShutsDown proves the C4 OBS2 wiring: Build constructs
// the otelobs.Runtime from the observability.otel block + feature flag and
// Shutdown flushes it. We only assert the Runtime is non-nil and Shutdown
// succeeds; exporter behavior is covered by internal/observe/otel tests.
func TestBuildSetsUpOTelAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
observability:
  otel:
    enabled: true
    endpoint: "127.0.0.1:4318"
    sample_ratio: 1
features:
  overrides:
    observe.otel_export: true
`, toYAMLPath(filepath.Join(dir, "yanshi.db")))
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	if err != nil {
		t.Fatal(err)
	}
	if app.OTel == nil {
		t.Fatal("otel runtime must be wired")
	}
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestBuildExposesAgentV1Service proves bootstrap constructs and exposes the
// shared v1 Agent API service. The service must be non-nil after Build so the
// HTTP layer (srv.AgentV1) and the JSON-RPC app-server (yanshi app) can both
// consume it. The FakeModel path still wires the service so tests and ephemeral
// servers can drive thread/start end-to-end without external dependencies.
func TestBuildExposesAgentV1Service(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "test.db"))
	cfgContent := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
token: "test-token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())
	if app.AgentAPI == nil {
		t.Fatal("AgentAPI must be wired by bootstrap")
	}

	// Drive the service end-to-end: start a thread and confirm the v1 wire
	// contract (version="v1", camelCase thread fields). This proves bootstrap
	// assembled a working service, not just a non-nil pointer.
	thread, err := app.AgentAPI.Start(context.Background(), apiV1.ThreadStartParams{Title: "smoke"})
	require.NoError(t, err)
	assert.Equal(t, "v1", thread.Version, "thread.Version must be v1")
	assert.Equal(t, "smoke", thread.Title, "thread.Title must round-trip")
	assert.Equal(t, "active", thread.Status, "thread.Status must be active")
	assert.NotEmpty(t, thread.ID, "thread.ID must be populated")
}

// TestBuild_APIKeysAreUsedVerbatimAndRedacted pins the only two api_key
// shapes yanshi accepts: a literal written straight into config.yaml, and a
// ${VAR} that config.Load expanded before Build ever saw it. Neither is
// resolved through the secrets backend — Build must hand both to
// BuildProviders unchanged.
//
// The redaction half is not incidental. Taking keys verbatim removes the
// resolution step that used to call Redactor.Register, so registration had to
// be re-established on its own; without it a plaintext key reaches WS/SSE
// frames and SQLite rows. The env case is what proves registration is not
// keyed to how the value arrived.
func TestBuild_APIKeysAreUsedVerbatimAndRedacted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YANSHI_TEST_OPENAI_KEY", "sk-from-env-expansion")

	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := toYAMLPath(filepath.Join(dir, "yanshi.db"))
	cfgContent := "server:\n  http_addr: \"127.0.0.1:0\"\nstorage:\n  sqlite_path: \"" + dbPath + "\"\n" +
		"llm:\n  providers:\n" +
		"    - name: literal\n      kind: openai\n      model: gpt-fake\n      api_key: sk-plain-literal-key\n" +
		"    - name: fromenv\n      kind: openai\n      model: gpt-fake\n      api_key: ${YANSHI_TEST_OPENAI_KEY}\n"
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0o644))

	cfg, err := config.Load(cfgPath)
	require.NoError(t, err, "a literal api_key must load without any opt-in flag")
	require.Equal(t, "sk-plain-literal-key", cfg.LLM.Providers[0].APIKey)
	require.Equal(t, "sk-from-env-expansion", cfg.LLM.Providers[1].APIKey,
		"${VAR} must be expanded by config.Load")

	app, err := bootstrap.Build(bootstrap.Options{Cfg: cfg, FakeModel: true})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())

	// Build must not have rewritten either key.
	assert.Equal(t, "sk-plain-literal-key", cfg.LLM.Providers[0].APIKey)
	assert.Equal(t, "sk-from-env-expansion", cfg.LLM.Providers[1].APIKey)

	require.NotNil(t, app.Redactor, "app.Redactor must be non-nil after Build")
	for _, key := range []string{"sk-plain-literal-key", "sk-from-env-expansion"} {
		assert.NotContains(t, app.Redactor.Redact("error containing "+key), key,
			"redactor must have registered %q regardless of how it arrived", key)
	}
}


// TestBuild_DeviceProviderInjection (structural fix #2) covers both sources:
//
//	(a) cfg-driven providers get NewGenericRFC8628Provider validation, and a
//	    non-HTTPS non-loopback URL aborts Build;
//	(b) AuthDeps.Providers override cfg entirely (replacement, not merge),
//	    so tests can inject an httptest endpoint without it being re-
//	    validated through NewGenericRFC8628Provider;
//	(c) duplicate / empty IDs are rejected whether from cfg or injection.
func TestBuild_DeviceProviderInjection(t *testing.T) {
	dir := t.TempDir()
	base := &config.Config{
		Secrets: config.SecretsConfig{Backend: "none"},
		Storage: config.StorageConfig{SQLitePath: filepath.Join(dir, "yanshi.db")},
	}

	// (a) bad cfg endpoint fails-closed.
	bad := *base
	bad.Auth.Device.DeviceAuthEnabled = true
	bad.Auth.Device.Providers = []config.DeviceProviderConfig{{
		ID: "p1", DeviceURL: "http://api.example.com/d", TokenURL: "https://api.example.com/t",
	}}
	if _, err := bootstrap.Build(bootstrap.Options{Cfg: &bad, FakeModel: true}); err == nil {
		t.Fatal("Build must reject non-HTTPS non-loopback device_url")
	}

	// (b) AuthDeps.Providers replaces cfg and skips NewGenericRFC8628Provider
	// validation — the test injects a stub that records calls, asserting the
	// injected provider reaches auth.Manager verbatim.
	good := *base
	good.Auth.Device.Providers = []config.DeviceProviderConfig{{
		ID: "ignored", DeviceURL: "http://example.invalid", TokenURL: "http://example.invalid",
	}}
	stub := &recordingDeviceProvider{}
	clock := &recordingClock{now: time.Unix(123, 0)}
	sleeper := &recordingSleeper{}
	app, err := bootstrap.Build(bootstrap.Options{
		Cfg:       &good,
		FakeModel: true,
		AuthDeps: bootstrap.AuthDeps{
			Providers: []bootstrap.DeviceProviderBinding{
				{ID: "test-only", Provider: stub},
			},
			Clock:   clock,
			Sleeper: sleeper,
		},
	})
	if err != nil {
		t.Fatalf("Build with injected provider: %v", err)
	}
	defer app.Shutdown(context.Background())
	_, _ = app.Auth.RunDeviceFlow(
		context.Background(), "test-only", "main", io.Discard,
	)
	if stub.clockSeen != clock || stub.sleeperSeen != sleeper {
		t.Fatalf("device flow did not receive bootstrap Clock/Sleeper")
	}

	// (c) duplicate IDs in injection fail.
	dup := *base
	_, err = bootstrap.Build(bootstrap.Options{
		Cfg: &dup, FakeModel: true,
		AuthDeps: bootstrap.AuthDeps{Providers: []bootstrap.DeviceProviderBinding{
			{ID: "x", Provider: stub},
			{ID: "x", Provider: stub},
		}},
	})
	if err == nil {
		t.Fatal("Build must reject duplicate injected provider IDs")
	}
	if !strings.Contains(err.Error(), `"x"`) {
		t.Fatalf("duplicate error must name the id: %v", err)
	}
}

type recordingClock struct{ now time.Time }

func (c *recordingClock) Now() time.Time { return c.now }

type recordingSleeper struct{}

func (*recordingSleeper) Sleep(context.Context, time.Duration) error { return nil }

// recordingDeviceProvider is a Task 8 test double. It implements
// auth.DeviceProvider by recording the Authorize call's Clock/Sleeper pair
// so the bootstrap test can prove they were wired through. It returns a
// fixed error so no fake HTTP server is needed.
type recordingDeviceProvider struct {
	clockSeen   auth.Clock
	sleeperSeen auth.Sleeper
}

func (r *recordingDeviceProvider) Authorize(
	_ context.Context,
	clk auth.Clock,
	slp auth.Sleeper,
	_ func(auth.StatusUpdate),
) (*auth.DeviceToken, error) {
	r.clockSeen = clk
	r.sleeperSeen = slp
	return nil, errors.New("recordingDeviceProvider: intentional failure")
}

// TestOutputLanguageInstructionIndependentOfUILocale verifies that the model
// output-language directive is independent of the UI locale: it reads only
// cfg.I18N.OutputLanguage and never cfg.I18N.UILocale. Empty output_language
// must leave the base instruction unchanged; non-empty must layer on top of
// orchestrator.DefaultInstruction rather than replace it.
func TestOutputLanguageInstructionIndependentOfUILocale(t *testing.T) {
	cfg := config.Config{
		I18N: config.I18NConfig{UILocale: "zh-Hans", OutputLanguage: "English"},
	}
	got := bootstrap.AppendOutputLanguageInstruction("project rule", cfg.I18N.OutputLanguage)
	if !strings.Contains(got, "Respond to the user in English") {
		t.Fatalf("missing output-language directive: %q", got)
	}
	if strings.Contains(got, cfg.I18N.UILocale) {
		t.Fatalf("UI locale leaked into model directive: %q", got)
	}
	if empty := bootstrap.AppendOutputLanguageInstruction("project rule", ""); empty != "project rule" {
		t.Fatalf("empty output language must follow user input: %q", empty)
	}
	withoutProject := bootstrap.AppendOutputLanguageInstruction("", "English")
	if !strings.Contains(withoutProject, orchestrator.DefaultInstruction) ||
		!strings.Contains(withoutProject, "Respond to the user in English") {
		t.Fatalf("language directive replaced default instruction: %q", withoutProject)
	}
}

func fakeProviderBuilder(cfg *config.Config) (map[string]model.BaseChatModel, []model.BaseChatModel, map[string]int, error) {
	named := make(map[string]model.BaseChatModel)
	var chain []model.BaseChatModel
	for _, p := range cfg.LLM.Providers {
		fm := einollm.NewFakeModel([]string{"reply"}, nil)
		named[p.Model] = fm
		chain = append(chain, fm)
	}
	return named, chain, nil, nil
}

func TestBuildSelectsFirstMultimodalProviderAsAux(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yamlBytes := []byte(fmt.Sprintf(`
llm:
  providers:
    - name: text
      model: text-model
      multimodal: false
    - name: vision
      model: vision-model
      multimodal: true
storage:
  sqlite_path: %q
`, filepath.Join(dir, "test.db")))
	require.NoError(t, os.WriteFile(cfgPath, yamlBytes, 0644))
	app, err := bootstrap.Build(bootstrap.Options{
		ConfigPath:      cfgPath,
		ProviderBuilder: fakeProviderBuilder,
	})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())
	require.NotNil(t, app.VisionAux)
	require.True(t, app.MultimodalMap["vision-model"])
	require.False(t, app.MultimodalMap["text-model"])
}

func TestBuildNoAuxWhenNoMultimodalProvider(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yamlBytes := []byte(fmt.Sprintf(`
llm:
  providers:
    - name: onlytext
      model: onlytext-model
      multimodal: false
storage:
  sqlite_path: %q
`, filepath.Join(dir, "test.db")))
	require.NoError(t, os.WriteFile(cfgPath, yamlBytes, 0644))
	app, err := bootstrap.Build(bootstrap.Options{
		ConfigPath:      cfgPath,
		ProviderBuilder: fakeProviderBuilder,
	})
	require.NoError(t, err)
	defer app.Shutdown(context.Background())
	require.Nil(t, app.VisionAux)
}





// TestOTelExportIsOffUnlessBothSwitchesAgree closes the gap the old ledger note
// named and the previous test could not: TestBuildSetsUpOTelAndShutsDown drives
// only the enabled side, and App.OTel is non-nil either way (a disabled runtime
// is a no-op object, not nil), so it is insensitive to whether either switch
// works at all.
//
// The two switches are ANDed on purpose -- observability.otel.enabled is the
// operator's YAML, observe.otel_export is the runtime flag -- and "can be
// switched off" has to hold for each of them independently. A test that only
// flipped both together would pass with either one ignored.
//
// A LIVE positive control is mandatory here, and the first version of this
// test did not have one. otelobs.Setup degrades to a no-op when the collector
// is unreachable, so with a dead endpoint every case reports Enabled()==false
// and the test passes no matter which switch is ignored -- both mutations
// (dropping either side of the AND) stayed green. The stub below makes the
// enabled case observably enabled, which is what gives the three off-cases
// their meaning.
//
// ledger: C4/OBS2#3 可关闭
func TestOTelExportIsOffUnlessBothSwitchesAgree(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer stub.Close()
	endpoint := stub.Listener.Addr().String()

	build := func(t *testing.T, yamlEnabled, flagEnabled bool) *bootstrap.App {
		t.Helper()
		dir := t.TempDir()
		cfgPath := filepath.Join(dir, "config.yaml")
		body := fmt.Sprintf(`
server: { http_addr: "127.0.0.1:0" }
storage: { sqlite_path: %q }
observability:
  otel:
    enabled: %t
    endpoint: %q
    sample_ratio: 1
features:
  overrides:
    observe.otel_export: %t
`, toYAMLPath(filepath.Join(dir, "yanshi.db")), yamlEnabled, endpoint, flagEnabled)
		if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = app.Shutdown(context.Background()) })
		return app
	}

	for _, tc := range []struct {
		name               string
		yaml, flag, wantOn bool
	}{
		{"both off", false, false, false},
		{"yaml off, flag on", false, true, false},
		{"yaml on, flag off", true, false, false},
		// The positive control. Without it the three rows above are satisfied
		// by any implementation that never enables anything.
		{"both on", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := build(t, tc.yaml, tc.flag)
			if app.OTel == nil {
				t.Fatal("App.OTel is nil: a disabled runtime is a no-op object, not nil")
			}
			if app.OTel.Enabled() != tc.wantOn {
				t.Errorf("Enabled() = %v, want %v: this switch does not switch anything",
					app.OTel.Enabled(), tc.wantOn)
			}
		})
	}
}
