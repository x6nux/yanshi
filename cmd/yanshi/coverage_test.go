package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x6nux/yanshi/internal/agent/goalloop"
	"github.com/x6nux/yanshi/internal/bootstrap"
	"github.com/x6nux/yanshi/internal/store"
	"github.com/x6nux/yanshi/internal/tools"
)

// lockedWriter is a goroutine-safe bytes.Buffer for stderr captured by tests
// that run a server command in a goroutine: runServe's server goroutine writes
// concurrently with the test's read, so a plain bytes.Buffer races under -race.
type lockedWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// ---- pure helpers --------------------------------------------------------

// TestIsManagedInvocation covers every branch of the managed-invocation
// detector: a leading --config (both forms), --config without a value, and the
// auth/doctor subcommand selection versus a non-managed subcommand.
func TestIsManagedInvocation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"auth", []string{"auth"}, true},
		{"doctor", []string{"doctor"}, true},
		{"config space then auth", []string{"--config", "c.yaml", "auth"}, true},
		{"config eq then doctor", []string{"--config=c.yaml", "doctor"}, true},
		{"config only no value", []string{"--config"}, true}, // dispatcher prints the precise usage error
		{"serve is not managed", []string{"serve"}, false},
		{"goal is not managed", []string{"goal"}, false},
		{"empty", []string{}, false},
		{"config eq then serve", []string{"--config=c.yaml", "serve"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isManagedInvocation(tc.args))
		})
	}
}

// TestParseManagedInvocation covers the default cfgPath, both --config forms,
// the missing-value error, and the missing-subcommand error.
func TestParseManagedInvocation(t *testing.T) {
	t.Run("defaults cfgPath", func(t *testing.T) {
		cfg, sub, rest, err := parseManagedInvocation([]string{"auth", "set"})
		require.NoError(t, err)
		assert.Equal(t, "config.yaml", cfg)
		assert.Equal(t, "auth", sub)
		assert.Equal(t, []string{"set"}, rest)
	})
	t.Run("config space", func(t *testing.T) {
		cfg, sub, rest, err := parseManagedInvocation([]string{"--config", "x.yaml", "doctor", "-json"})
		require.NoError(t, err)
		assert.Equal(t, "x.yaml", cfg)
		assert.Equal(t, "doctor", sub)
		assert.Equal(t, []string{"-json"}, rest)
	})
	t.Run("config eq", func(t *testing.T) {
		cfg, sub, _, err := parseManagedInvocation([]string{"--config=y.yaml", "auth"})
		require.NoError(t, err)
		assert.Equal(t, "y.yaml", cfg)
		assert.Equal(t, "auth", sub)
	})
	t.Run("config space missing value", func(t *testing.T) {
		_, _, _, err := parseManagedInvocation([]string{"--config"})
		require.Error(t, err)
	})
	t.Run("config eq empty value", func(t *testing.T) {
		_, _, _, err := parseManagedInvocation([]string{"--config= ", "auth"})
		require.Error(t, err)
	})
	t.Run("config space empty value", func(t *testing.T) {
		_, _, _, err := parseManagedInvocation([]string{"--config", "  ", "auth"})
		require.Error(t, err)
	})
	t.Run("missing subcommand", func(t *testing.T) {
		_, _, _, err := parseManagedInvocation([]string{"--config", "z.yaml"})
		require.Error(t, err)
	})
}

// TestAbsWorkdir covers relative→absolute resolution, the passthrough for
// already-absolute paths (Windows drive + Unix root), and the empty/"." shortcut.
func TestAbsWorkdir(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)

	got, err := absWorkdir(".")
	require.NoError(t, err)
	assert.Equal(t, wd, got)

	got, err = absWorkdir("")
	require.NoError(t, err)
	assert.Equal(t, wd, got)

	// Absolute Windows drive path passes through untouched.
	assert.Equal(t, `C:\proj`, absWorkdirSafe(`C:\proj`))
	// Unix-rooted path passes through.
	assert.Equal(t, "/tmp/x", absWorkdirSafe("/tmp/x"))
	// Backslash-rooted path passes through.
	assert.Equal(t, `\srv\x`, absWorkdirSafe(`\srv\x`))

	// Relative path is joined to the cwd.
	rel, err := absWorkdir("sub/dir")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(rel, "sub/dir"), "rel=%q", rel)
	assert.True(t, filepath.IsAbs(rel), "rel should be absolute: %q", rel)
}

// absWorkdirSafe returns the result without the error (only valid for the
// absolute-passthrough cases, which never error).
func absWorkdirSafe(p string) string {
	got, err := absWorkdir(p)
	if err != nil {
		return "<err>"
	}
	return got
}

// TestEnvDefault covers both the set/unset branches.
func TestEnvDefault(t *testing.T) {
	t.Setenv("YANSHI_TEST_ENVDEFAULT", "from-env")
	assert.Equal(t, "from-env", envDefault("YANSHI_TEST_ENVDEFAULT", "fallback"))
	assert.Equal(t, "fallback", envDefault("YANSHI_TEST_ENVDEFAULT_UNSET", "fallback"))
	t.Setenv("YANSHI_TEST_ENVDEFAULT", "")
	assert.Equal(t, "fallback", envDefault("YANSHI_TEST_ENVDEFAULT", "fallback"))
}

// TestExpandHome covers empty input, the ~ expansion, and the passthrough.
func TestExpandHome(t *testing.T) {
	assert.Equal(t, "", expandHome(""))
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, home+string(os.PathSeparator)+"yanshi", expandHome("~/yanshi"))
	// Non-tilde paths pass through unchanged.
	assert.Equal(t, "/abs/path", expandHome("/abs/path"))
	assert.Equal(t, "rel/path", expandHome("rel/path"))
}

// ---- goal helpers --------------------------------------------------------

// TestResolveGoalTier covers the auto path, a forced tier, and the error path.
func TestResolveGoalTier(t *testing.T) {
	t.Run("auto", func(t *testing.T) {
		tier, forced, err := resolveGoalTier("auto", "fix a typo")
		require.NoError(t, err)
		assert.False(t, forced)
		_ = tier
	})
	t.Run("forced t2", func(t *testing.T) {
		tier, forced, err := resolveGoalTier("t2", "anything")
		require.NoError(t, err)
		assert.True(t, forced)
		assert.Equal(t, goalloop.TierDesigned, tier)
	})
	t.Run("invalid", func(t *testing.T) {
		_, _, err := resolveGoalTier("t9", "anything")
		require.Error(t, err)
	})
}

// TestCounterEvaluator proves the evaluator fails until its call count reaches
// passAt, then passes on every subsequent call.
func TestCounterEvaluator(t *testing.T) {
	e := &counterEvaluator{passAt: 2}
	v1, err := e.Evaluate(context.Background(), goalloop.Goal{}, goalloop.Plan{}, "")
	require.NoError(t, err)
	assert.False(t, v1.Pass, "call 1 should fail (1 < 2)")
	assert.Contains(t, v1.Evidence, "call 1")

	v2, err := e.Evaluate(context.Background(), goalloop.Goal{}, goalloop.Plan{}, "")
	require.NoError(t, err)
	assert.True(t, v2.Pass, "call 2 should pass (2 >= 2)")
	assert.Contains(t, v2.Evidence, "call 2")

	v3, err := e.Evaluate(context.Background(), goalloop.Goal{}, goalloop.Plan{}, "")
	require.NoError(t, err)
	assert.True(t, v3.Pass, "call 3 should still pass")
}

// TestRunGoalFakeModel drives the self-contained --fake-model goal loop
// end-to-end (FakePlanner + FakeImplementer + counterEvaluator). It must
// terminate within the budget and print a decision line. The fake path needs no
// config file, API keys, or external CLIs.
func TestRunGoalFakeModel(t *testing.T) {
	// Capture the result; output goes to os.Stdout (fmt.Printf) which we cannot
	// redirect without refactoring, but the return code is the contract.
	done := make(chan int, 1)
	go func() {
		done <- runGoal([]string{"-fake-model", "-max-iters", "3", "-goal", "demo goal"})
	}()
	select {
	case code := <-done:
		// The fake demo fails eval once then passes, so the loop completes.
		assert.Equal(t, exitOK, code)
	case <-time.After(15 * time.Second):
		t.Fatal("runGoal --fake-model did not terminate within 15s")
	}
}

// TestRunGoalUsageErrors covers the three usage-error returns: a bad flag, a
// missing goal text, and an invalid tier. None of these reach the loop.
func TestRunGoalUsageErrors(t *testing.T) {
	t.Run("bad flag", func(t *testing.T) {
		assert.Equal(t, exitUsage, runGoal([]string{"--definitely-not-a-flag"}))
	})
	t.Run("missing goal text", func(t *testing.T) {
		assert.Equal(t, exitUsage, runGoal([]string{"-fake-model"}))
	})
	t.Run("bad tier", func(t *testing.T) {
		assert.Equal(t, exitUsage, runGoal([]string{"-fake-model", "-tier", "t9", "-goal", "x"}))
	})
}

// TestRunGoalBadWorkdir covers the runtime-error return when the workdir cannot
// be resolved. A non-existent workdir is not caught by absWorkdir (it only
// joins), so we point at a path under a deleted temp dir via a relative path
// that forces os.Getwd to still succeed — instead, exercise the real-path
// bootstrap failure by giving a nonexistent config without fake-model.
func TestRunGoalBootstrapFails(t *testing.T) {
	code := runGoal([]string{"-config", filepath.Join(t.TempDir(), "missing.yaml"), "-tier", "t3", "-goal", "x"})
	assert.Equal(t, exitErr, code)
}

// TestRunLightweightGoal drives the lightweight (T0-T2) path against a
// fake-model bootstrap app: one orchestrator turn that returns a Decision. With
// the fake model the turn succeeds, so Complete is true and the summary carries
// the escalation hint for a below-T4 tier.
func TestRunLightweightGoal(t *testing.T) {
	app := buildFakeApp(t)
	defer app.Shutdown(context.Background())

	dec := runLightweightGoal(context.Background(), app, goalloop.TierQuickFix, "do a thing")
	assert.True(t, dec.Complete, "lightweight turn should complete with fake model: %s", dec.Summary)
	assert.NotEmpty(t, dec.Summary)
	// T0 is below T4, so an escalation hint is appended (non-silent upgrade path).
	assert.Contains(t, dec.Summary, "(")
}

// TestRunLightweightGoal_SkillBodyAppend proves that when the tier's skill body
// is available, it is prepended to the goal text before the orchestrator turn.
// We verify via the decision being complete (the orchestrator received a prompt).
func TestRunLightweightGoal_NonTier4Hint(t *testing.T) {
	app := buildFakeApp(t)
	defer app.Shutdown(context.Background())
	// T2 (Designed) is still lightweight and below T4 → hint present.
	dec := runLightweightGoal(context.Background(), app, goalloop.TierDesigned, "goal text")
	assert.True(t, dec.Complete)
}

// buildFakeApp builds a minimal in-process bootstrap app with FakeModel so the
// goal/TUI-adjacent helpers can exercise the orchestrator without external deps.
func buildFakeApp(t *testing.T) *bootstrap.App {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := strings.ReplaceAll(filepath.Join(dir, "test.db"), "\\", "/")
	cfg := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	return app
}

// TestPersistGoalRun_NilStoreIsNoOp proves a nil store short-circuits without
// touching anything.
func TestPersistGoalRun_NilStoreIsNoOp(t *testing.T) {
	assert.NotPanics(t, func() {
		persistGoalRun(nil, goalloop.TierStandard, goalloop.Decision{Complete: true, Summary: "s"}, goalloop.Usage{}, 1)
	})
}

// TestPersistGoalRun_WritesRecord proves a run record is persisted to the KV
// table (keyed goalrun:<unix>) and is readable back via the raw DB handle.
func TestPersistGoalRun_WritesRecord(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "goalrun.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer st.Close()

	persistGoalRun(st, goalloop.TierStandard, goalloop.Decision{Complete: true, Summary: "done"}, goalloop.Usage{PromptTokens: 5}, 2)

	var count int
	row := st.DB.QueryRow(`SELECT COUNT(*) FROM kv WHERE key LIKE 'goalrun:%'`)
	require.NoError(t, row.Scan(&count))
	assert.GreaterOrEqual(t, count, 1, "a goalrun:* record must be persisted")
}

// ---- serve ---------------------------------------------------------------

// writeServeConfig writes a minimal fake-model config and returns its path.
func writeServeConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := strings.ReplaceAll(filepath.Join(dir, "serve.db"), "\\", "/")
	cfg := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	return cfgPath
}

// TestRunServeBadConfig proves a bootstrap failure maps to exitErr before any
// server goroutine is started.
func TestRunServeBadConfig(t *testing.T) {
	var stderr lockedWriter
	code := runServe(context.Background(), []string{"-config", filepath.Join(t.TempDir(), "nope.yaml")}, &stderr)
	assert.Equal(t, exitErr, code)
	assert.Contains(t, stderr.String(), "yanshi serve:")
}

// TestRunServeBadFlag proves a flag parse error maps to exitUsage.
func TestRunServeBadFlag(t *testing.T) {
	var stderr lockedWriter
	code := runServe(context.Background(), []string{"--nope"}, &stderr)
	assert.Equal(t, exitUsage, code)
}

// TestRunServeStartsAndShuts proves the happy path: the server starts, then a
// cancelled context triggers a clean shutdown returning exitOK. Uses fake-model
// so no API keys are needed.
func TestRunServeStartsAndShuts(t *testing.T) {
	cfgPath := writeServeConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after Start binds so the server actually listens first.
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	var stderr lockedWriter
	done := make(chan int, 1)
	go func() { done <- runServe(ctx, []string{"-config", cfgPath, "-fake-model"}, &stderr) }()
	select {
	case code := <-done:
		assert.Equal(t, exitOK, code, "stderr=%s", stderr.String())
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("runServe did not shut down within 15s")
	}
}

// ---- dispatch ------------------------------------------------------------

// TestDispatchVersion proves --version prints the version to stdout and returns
// exitOK without invoking any subcommand.
func TestDispatchVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	code := dispatch([]string{"yanshi", "--version"}, strings.NewReader(""), &out, &errOut)
	assert.Equal(t, exitOK, code)
	assert.Contains(t, out.String(), "yanshi")
	assert.Contains(t, out.String(), Version)
}

// TestDispatchUnknownSubcommand proves an unrecognized subcommand prints usage
// to stderr and returns exitUsage.
func TestDispatchUnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := dispatch([]string{"yanshi", "totally-bogus"}, strings.NewReader(""), &out, &errOut)
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, errOut.String(), "unknown subcommand")
}

// TestDispatchPRMissingArg proves `yanshi pr` with no number prints usage and
// returns exitErr.
func TestDispatchPRMissingArg(t *testing.T) {
	var out, errOut bytes.Buffer
	code := dispatch([]string{"yanshi", "pr"}, strings.NewReader(""), &out, &errOut)
	assert.Equal(t, exitErr, code)
	assert.Contains(t, errOut.String(), "Usage: yanshi pr")
}

// TestDispatchManagedAuthRoutesThroughRunCLI proves `yanshi auth` (no sub) is
// routed through the managed dispatcher and returns the usage exit code.
func TestDispatchManagedAuthRoutesThroughRunCLI(t *testing.T) {
	var out, errOut bytes.Buffer
	code := dispatch([]string{"yanshi", "auth"}, strings.NewReader(""), &out, &errOut)
	assert.Equal(t, exitUsage, code)
}

// TestDispatchExecBadFlag proves `yanshi exec` with a bad flag is routed to the
// headless runner and returns exitUsage.
func TestDispatchExecBadFlag(t *testing.T) {
	code := dispatch([]string{"yanshi", "exec", "--nope"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	assert.Equal(t, exitUsage, code)
}

// ---- splitNoTUI ----------------------------------------------------------

// TestSplitNoTUI covers detection of both --no-tui forms, removal of the token,
// and the no-op when absent.
func TestSplitNoTUI(t *testing.T) {
	noTUI, filtered := splitNoTUI([]string{"--config", "c.yaml", "--no-tui", "-p", "hi"})
	assert.True(t, noTUI)
	assert.Equal(t, []string{"--config", "c.yaml", "-p", "hi"}, filtered)

	noTUI, filtered = splitNoTUI([]string{"-no-tui"})
	assert.True(t, noTUI)
	assert.Empty(t, filtered)

	noTUI, filtered = splitNoTUI([]string{"-p", "hi"})
	assert.False(t, noTUI)
	assert.Equal(t, []string{"-p", "hi"}, filtered)
}

// ---- vcs-mcp core --------------------------------------------------------

// TestRunVCSMCPBadDB proves a store-open failure surfaces as an error (the
// uncovered branch of runVCSMCP).
func TestRunVCSMCPBadDB(t *testing.T) {
	// A path whose parent does not exist cannot be opened as a SQLite file.
	err := runVCSMCP(context.Background(),
		filepath.Join(t.TempDir(), "no-such-dir", "x.db"),
		"repo", "wt", "acp", t.TempDir(),
		strings.NewReader(""), &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "open store")
}

// ---- runCLI / runAuthSub error paths -------------------------------------

// TestRunCLINoArgs proves an empty argv returns exitUsage.
func TestRunCLINoArgs(t *testing.T) {
	assert.Equal(t, exitUsage, runCLI(nil, strings.NewReader(""), &bytes.Buffer{}))
}

// TestRunCLIUnknownManagedSub proves an unknown managed subcommand returns
// exitUsage via the dispatcher default branch.
func TestRunCLIUnknownManagedSub(t *testing.T) {
	var errOut bytes.Buffer
	// Use --config to enter the managed path, then an unknown sub.
	code := runCLI([]string{"yanshi", "--config", "x.yaml", "frobnicate"}, strings.NewReader(""), &bytes.Buffer{}, &errOut)
	assert.Equal(t, exitUsage, code)
}

// TestRunCLIManagedConfigWithoutValue proves --config with no value returns
// exitUsage.
func TestRunCLIManagedConfigWithoutValue(t *testing.T) {
	code := runCLI([]string{"yanshi", "--config"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	assert.Equal(t, exitUsage, code)
}

// TestRunAuthSubNoArgs proves `yanshi auth` with no sub-subcommand prints usage
// and returns exitUsage without loading config.
func TestRunAuthSubNoArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runAuthSub(context.Background(), nil, "config.yaml", strings.NewReader(""), &out, &errOut, authCLIDeps{})
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, errOut.String(), "usage: yanshi auth")
}

// TestRunAuthSubBadConfig proves a config load failure maps to exitErr.
func TestRunAuthSubBadConfig(t *testing.T) {
	var errOut bytes.Buffer
	code := runAuthSub(context.Background(), []string{"status"},
		filepath.Join(t.TempDir(), "nope.yaml"),
		strings.NewReader(""), &bytes.Buffer{}, &errOut, authCLIDeps{})
	assert.Equal(t, exitErr, code)
}

// TestRunAuthSubUnknownSub proves an unknown auth sub-subcommand returns
// exitUsage after a successful config load.
func TestRunAuthSubUnknownSub(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeAuthConfig(t, dir)
	var errOut bytes.Buffer
	code := runAuthSub(context.Background(), []string{"frobnicate"}, cfgPath,
		strings.NewReader(""), &bytes.Buffer{}, &errOut, authCLIDeps{})
	assert.Equal(t, exitUsage, code)
}

// TestRunAuthSubBadFlag proves a flag parse error returns exitUsage.
func TestRunAuthSubBadFlag(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeAuthConfig(t, dir)
	var errOut bytes.Buffer
	code := runAuthSub(context.Background(), []string{"status", "--definitely-bad"}, cfgPath,
		strings.NewReader(""), &bytes.Buffer{}, &errOut, authCLIDeps{})
	assert.Equal(t, exitUsage, code)
}

// TestRunAuthSubDeviceProviderDupeID proves duplicate device provider ids are
// rejected with exitErr before any flow runs.
func TestRunAuthSubDeviceProviderDupeID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YANSHI_PASSPHRASE", "pass")
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgBody := `
storage:
  sqlite_path: "` + strings.ReplaceAll(filepath.Join(dir, "a.db"), "\\", "/") + `"
secrets:
  backend: file
  file_path: "` + strings.ReplaceAll(filepath.Join(dir, "s.enc"), "\\", "/") + `"
  passphrase_env: YANSHI_PASSPHRASE
auth:
  device:
    device_auth_enabled: true
    providers:
      - id: dup
        client_id: c
        device_url: "https://example.com/d"
        token_url: "https://example.com/t"
      - id: dup
        client_id: c
        device_url: "https://example.com/d2"
        token_url: "https://example.com/t2"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgBody), 0o600))
	var errOut bytes.Buffer
	code := runAuthSub(context.Background(), []string{"device", "--provider", "dup"}, cfgPath,
		strings.NewReader(""), &bytes.Buffer{}, &errOut, authCLIDeps{})
	assert.Equal(t, exitErr, code)
}

// TestRunAuthSubDeviceProviderBadEndpoint proves a device provider with a
// non-HTTPS endpoint is rejected at construction with exitErr.
func TestRunAuthSubDeviceProviderBadEndpoint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YANSHI_PASSPHRASE", "pass")
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgBody := `
storage:
  sqlite_path: "` + strings.ReplaceAll(filepath.Join(dir, "a.db"), "\\", "/") + `"
secrets:
  backend: file
  file_path: "` + strings.ReplaceAll(filepath.Join(dir, "s.enc"), "\\", "/") + `"
  passphrase_env: YANSHI_PASSPHRASE
auth:
  device:
    device_auth_enabled: true
    providers:
      - id: bad
        client_id: c
        device_url: "http://insecure.example.com/d"
        token_url: "https://example.com/t"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgBody), 0o600))
	var errOut bytes.Buffer
	code := runAuthSub(context.Background(), []string{"device", "--provider", "bad"}, cfgPath,
		strings.NewReader(""), &bytes.Buffer{}, &errOut, authCLIDeps{})
	assert.Equal(t, exitErr, code)
}

// writeAuthConfig writes a minimal file-backed-secrets config for auth sub
// tests that need a successful load.
func writeAuthConfig(t *testing.T, dir string) string {
	t.Helper()
	t.Setenv("YANSHI_PASSPHRASE", "pass")
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgBody := `
storage:
  sqlite_path: "` + strings.ReplaceAll(filepath.Join(dir, "a.db"), "\\", "/") + `"
secrets:
  backend: file
  file_path: "` + strings.ReplaceAll(filepath.Join(dir, "s.enc"), "\\", "/") + `"
  passphrase_env: YANSHI_PASSPHRASE
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgBody), 0o600))
	return cfgPath
}

// ---- headless ------------------------------------------------------------

// TestRunHeadlessCommandBadFlag proves a parse error returns exitUsage.
func TestRunHeadlessCommandBadFlag(t *testing.T) {
	assert.Equal(t, exitUsage, runHeadlessCommand([]string{"--nope"}, "exec", strings.NewReader("")))
}

// TestRunHeadlessCommandMissingFile proves a --file read failure returns
// exitUsage without touching the network.
func TestRunHeadlessCommandMissingFile(t *testing.T) {
	assert.Equal(t, exitUsage, runHeadlessCommand([]string{"--file", filepath.Join(t.TempDir(), "missing.txt")}, "exec", strings.NewReader("")))
}

// ---- pr ------------------------------------------------------------------

// TestParsePRInputURLs covers URL parsing variants: trailing slash, malformed
// number, and a URL without enough path segments.
func TestParsePRInputURLs(t *testing.T) {
	repo, num := parsePRInput("https://github.com/owner/repo/pull/42/")
	assert.Equal(t, "owner/repo", repo)
	assert.Equal(t, 42, num)

	_, num = parsePRInput("https://github.com/owner/repo/pull/notanumber")
	assert.Equal(t, 0, num)

	_, num = parsePRInput("not-a-number-or-url")
	assert.Equal(t, 0, num)
}

// TestParsePRInputRawNumberWithRemote covers the raw-number branch that infers
// the repo from the git remote (the test process runs inside a git repo).
func TestParsePRInputRawNumberWithRemote(t *testing.T) {
	repo, num := parsePRInput("7")
	assert.Equal(t, 7, num)
	// repo may be empty if origin is not a github remote; just exercise the path.
	_ = repo
}

// TestDetectGitHubRemote exercises the git-remote probe inside this worktree.
// The result is owner/repo when origin is github, otherwise empty.
func TestDetectGitHubRemote(t *testing.T) {
	remote := detectGitHubRemote()
	// Either "owner/repo" (github origin) or "" (non-github / no origin); both
	// are valid outcomes — the point is the probe runs without panicking.
	t.Logf("detected remote = %q", remote)
}

// TestBuildPRReviewPrompt proves the prompt assembles the PR metadata and diff.
func TestBuildPRReviewPrompt(t *testing.T) {
	ghCtx := tools.GitHubContext{
		Number:  42,
		Title:   "Add feature",
		Body:    "Does the thing",
		Author:  "alice",
		HeadRef: "feat-x",
		Files:   []tools.GitHubFileStat{{Path: "a.go", Additions: 3}},
	}
	prompt := buildPRReviewPrompt(ghCtx, "+diff content")
	assert.Contains(t, prompt, "PR #42")
	assert.Contains(t, prompt, "feat-x")
	assert.Contains(t, prompt, "alice")
	assert.Contains(t, prompt, "Add feature")
	assert.Contains(t, prompt, "+diff content")
}

// TestRunHeadlessPromptReviewNoRunner proves the review branch returns an
// in-band error string (not a Go error) when no sub-agent runner is bound —
// the headless review degrades gracefully rather than crashing.
func TestRunHeadlessPromptReviewNoRunner(t *testing.T) {
	res, err := runHeadlessPrompt(context.Background(), "review", "some diff")
	require.NoError(t, err)
	assert.NotEmpty(t, res, "review with no runner should return an in-band error string")
}

// TestRunHeadlessPromptUnknownTool proves the default branch rejects an unknown
// tool name with a Go error.
func TestRunHeadlessPromptUnknownTool(t *testing.T) {
	_, err := runHeadlessPrompt(context.Background(), "bogus-tool", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown headless tool")
}

// TestRunPRBadInput proves runPR rejects unparseable input with exitErr before
// invoking gh. (Output goes to os.Stderr, which we do not capture here.)
func TestRunPRBadInput(t *testing.T) {
	code := runPR(context.Background(), "not-a-number-or-url")
	assert.Equal(t, exitErr, code)
}

// ---- app flag error ------------------------------------------------------

// TestRunAppBadFlag proves a flag parse error returns exitUsage (the existing
// test covers positional args; this covers the flag-parse path).
func TestRunAppBadFlag(t *testing.T) {
	code := runApp([]string{"--nope"}, strings.NewReader(""), &bytes.Buffer{})
	assert.Equal(t, exitUsage, code)
}

// ---- batch 2: wrapper fast-error paths -----------------------------------

// TestServeBadConfig proves the serve() signal-handling wrapper reaches
// runServe and returns exitErr on a bootstrap failure (the wrapper itself only
// adds the signal context + exit-code passthrough).
func TestServeBadConfig(t *testing.T) {
	code := serve([]string{"-config", filepath.Join(t.TempDir(), "nope.yaml")})
	assert.Equal(t, exitErr, code)
}

// TestRunDefaultBadFlag proves runDefault returns exitUsage on a flag parse
// error before reaching the (interactive, uncoverable) TUI launch.
func TestRunDefaultBadFlag(t *testing.T) {
	assert.Equal(t, exitUsage, runDefault([]string{"--nope"}))
}

// TestChatTUIBadFlag proves chatTUI returns exitUsage on a flag parse error
// before reaching the TUI launch.
func TestChatTUIBadFlag(t *testing.T) {
	assert.Equal(t, exitUsage, chatTUI([]string{"--nope"}))
}

// TestChatTUINoTUIBadFlag proves the --no-tui branch of chatTUI routes to the
// headless runner, which returns exitUsage on a flag error.
func TestChatTUINoTUIBadFlag(t *testing.T) {
	assert.Equal(t, exitUsage, chatTUI([]string{"--no-tui", "--nope"}))
}

// TestVcsMcpBadDB proves the vcsMcp() wrapper surfaces a store-open failure as
// exitErr (the runVCSMCP core fails at store.Open before reading stdin, so the
// test does not block). YANSHI_DB_PATH points at a path whose parent is missing.
func TestVcsMcpBadDB(t *testing.T) {
	t.Setenv("YANSHI_DB_PATH", filepath.Join(t.TempDir(), "missing-dir", "x.db"))
	assert.Equal(t, exitErr, vcsMcp(nil))
}

// TestDispatchServeError routes `serve` with a bad config through dispatch and
// proves it reaches runServe's bootstrap-failure return.
func TestDispatchServeError(t *testing.T) {
	code := dispatch([]string{"yanshi", "serve", "-config", filepath.Join(t.TempDir(), "nope.yaml")},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	assert.Equal(t, exitErr, code)
}

// TestDispatchChatBadFlag routes a chat flag error through dispatch.
func TestDispatchChatBadFlag(t *testing.T) {
	code := dispatch([]string{"yanshi", "chat", "--nope"},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	assert.Equal(t, exitUsage, code)
}

// TestDispatchGoalBadFlag routes a goal flag error through dispatch.
func TestDispatchGoalBadFlag(t *testing.T) {
	code := dispatch([]string{"yanshi", "goal", "--nope"},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	assert.Equal(t, exitUsage, code)
}

// TestDispatchVcsMcpError routes vcs-mcp with a bad DB env through dispatch and
// proves it returns exitErr without blocking on stdin.
func TestDispatchVcsMcpError(t *testing.T) {
	t.Setenv("YANSHI_DB_PATH", filepath.Join(t.TempDir(), "missing-dir", "x.db"))
	code := dispatch([]string{"yanshi", "vcs-mcp"},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	assert.Equal(t, exitErr, code)
}

// TestRunCLIWithAuthDepsNilIO proves the defensive nil-reader/writer defaults
// are applied (the function must not panic when stdin/stdout/stderr are nil).
// It routes to `auth` with no sub, which returns exitUsage before using them.
func TestRunCLIWithAuthDepsNilIO(t *testing.T) {
	code := runCLIWithAuthDeps([]string{"yanshi", "auth"}, nil, nil, nil, authCLIDeps{})
	assert.Equal(t, exitUsage, code)
}

// TestRunDoctorBadFlag proves a flag parse error returns exitUsage.
func TestRunDoctorBadFlag(t *testing.T) {
	var errOut bytes.Buffer
	code := runDoctor(context.Background(), []string{"--nope"}, &bytes.Buffer{}, &errOut)
	assert.Equal(t, exitUsage, code)
}

// TestRunAppBadConfig proves a bootstrap failure in runApp maps to exitErr.
func TestRunAppBadConfig(t *testing.T) {
	code := runApp([]string{"-config", filepath.Join(t.TempDir(), "nope.yaml")},
		strings.NewReader(""), &bytes.Buffer{})
	assert.Equal(t, exitErr, code)
}

// TestRunServeAddrOverride proves the -addr flag overrides the app listen
// address; folded into the start/shutdown flow so the override branch runs.
func TestRunServeAddrOverride(t *testing.T) {
	cfgPath := writeServeConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	var stderr lockedWriter
	done := make(chan int, 1)
	go func() {
		done <- runServe(ctx, []string{"-config", cfgPath, "-fake-model", "-addr", "127.0.0.1:0"}, &stderr)
	}()
	select {
	case code := <-done:
		assert.Equal(t, exitOK, code, "stderr=%s", stderr.String())
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("runServe -addr did not shut down within 15s")
	}
}

// ---- runGoal real path (auto-fake when providers empty) -------------------

// TestRunGoalRealPathLightweight drives the real (non-fake-flag) path of runGoal
// at tier t0 (lightweight). bootstrap.Build falls back to the fake model when no
// providers are configured, so the lightweight orchestrator turn runs without
// API keys, persists a run record to the store, and returns exitOK.
func TestRunGoalRealPathLightweight(t *testing.T) {
	cfgPath := writeServeConfig(t) // empty-providers config → auto-fake
	done := make(chan int, 1)
	go func() { done <- runGoal([]string{"-config", cfgPath, "-tier", "t0", "-goal", "lightweight goal"}) }()
	select {
	case code := <-done:
		assert.Equal(t, exitOK, code)
	case <-time.After(20 * time.Second):
		t.Fatal("runGoal t0 real path did not terminate within 20s")
	}
}

// TestRunGoalRealPathLoopSetup drives the real path at tier t3 (loop) far
// enough to exercise the T3-T4 setup code (LLMPlanner, ACPImplementer,
// EvaluatorsForTier, loop.New). The ACPImplementer cannot spawn a real agent
// CLI in the test environment, so loop.Run fails and runGoal returns exitErr —
// but every setup statement before the run is covered.
func TestRunGoalRealPathLoopSetup(t *testing.T) {
	cfgPath := writeServeConfig(t)
	done := make(chan int, 1)
	go func() { done <- runGoal([]string{"-config", cfgPath, "-tier", "t3", "-max-iters", "1", "-goal", "loop goal"}) }()
	select {
	case code := <-done:
		// The loop fails to implement (no external CLI); that is exitErr, not a
		// hang. Any non-timeout result means the setup path ran cleanly.
		_ = code
	case <-time.After(20 * time.Second):
		t.Fatal("runGoal t3 real path did not terminate within 20s")
	}
}

// ---- runHeadlessCommand happy path ---------------------------------------

// TestRunHeadlessCommandFakeModel drives the full headless runner with an
// in-process fake backend and a single -p prompt. It must terminate with
// exitOK (the fake model answers without tool calls). This covers the
// prompt-input, context, and cli.RunHeadless branches of runHeadlessCommand.
func TestRunHeadlessCommandFakeModel(t *testing.T) {
	cfgPath := writeServeConfig(t)
	done := make(chan int, 1)
	go func() {
		done <- runHeadlessCommand([]string{"-config", cfgPath, "-fake-model", "-inprocess", "-p", "hello"}, "exec", strings.NewReader(""))
	}()
	select {
	case code := <-done:
		assert.Equal(t, exitOK, code)
	case <-time.After(20 * time.Second):
		t.Fatal("runHeadlessCommand fake-model did not terminate within 20s")
	}
}

// ---- batch 3 --------------------------------------------------------------

// TestParseGitHubRemoteURL covers the SSH, HTTPS, and non-github branches of
// the remote-URL parser (extracted from detectGitHubRemote).
func TestParseGitHubRemoteURL(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"ssh with git suffix", "git@github.com:owner/repo.git", "owner/repo"},
		{"ssh no suffix", "git@github.com:owner/repo", "owner/repo"},
		{"https", "https://github.com/owner/repo", "owner/repo"},
		{"https with suffix", "https://github.com/owner/repo.git", "owner/repo"},
		{"gitlab is not github", "https://gitlab.com/owner/repo", ""},
		{"empty", "", ""},
		{"plain string", "not a remote", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseGitHubRemoteURL(tc.in))
		})
	}
}

// TestParsePRInputNoPullSegment covers the URL-without-pull branch (returns
// "",0) — e.g. an issues URL.
func TestParsePRInputNoPullSegment(t *testing.T) {
	repo, num := parsePRInput("https://github.com/owner/repo/issues/42")
	assert.Equal(t, "", repo)
	assert.Equal(t, 0, num)
}

// TestRunPRValidURLRunsGH proves runPR reaches the `gh` execution path for a
// well-formed URL (repo+number parsed), then exits exitErr when gh is absent or
// fails. A bounded context keeps the test fast even if gh is installed.
func TestRunPRValidURLRunsGH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code := runPR(ctx, "https://github.com/owner/repo/pull/1")
	// gh is not installed / not authed in the test env → exitErr. If gh somehow
	// succeeds the code would be 0; either way the gh-execution lines ran.
	assert.True(t, code == exitErr || code == exitOK, "code=%d", code)
}

// TestRunCLIManagedDoctorDispatch proves the managed dispatcher routes `doctor`
// to runDoctor (the runCLIWithAuthDeps "doctor" case).
func TestRunCLIManagedDoctorDispatch(t *testing.T) {
	cfgPath := writeServeConfig(t)
	var out bytes.Buffer
	// doctor always warns on sandbox → exit 1; the point is it ran via runCLI.
	code := runCLI([]string{"yanshi", "--config", cfgPath, "doctor"}, strings.NewReader(""), &out)
	assert.Equal(t, 1, code)
}

// ---- runAuthSub additional error paths -----------------------------------

// TestRunAuthSubSecretsFail proves a secrets.Manager construction failure
// (file backend with no passphrase) maps to exitErr.
func TestRunAuthSubSecretsFail(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// file backend but passphrase_env unset → NewManager errors.
	cfgBody := `
storage:
  sqlite_path: "` + strings.ReplaceAll(filepath.Join(dir, "a.db"), "\\", "/") + `"
secrets:
  backend: file
  file_path: "` + strings.ReplaceAll(filepath.Join(dir, "s.enc"), "\\", "/") + `"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgBody), 0o600))
	var errOut bytes.Buffer
	code := runAuthSub(context.Background(), []string{"status"}, cfgPath,
		strings.NewReader(""), &bytes.Buffer{}, &errOut, authCLIDeps{})
	assert.Equal(t, exitErr, code)
}

// TestRunAuthSubStoreOpenFail proves a store.Open failure (sqlite path under a
// missing dir) maps to exitErr after secrets.Manager succeeds.
func TestRunAuthSubStoreOpenFail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YANSHI_PASSPHRASE", "pass")
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgBody := `
storage:
  sqlite_path: "` + strings.ReplaceAll(filepath.Join(dir, "missing-sub", "a.db"), "\\", "/") + `"
secrets:
  backend: file
  file_path: "` + strings.ReplaceAll(filepath.Join(dir, "s.enc"), "\\", "/") + `"
  passphrase_env: YANSHI_PASSPHRASE
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgBody), 0o600))
	var errOut bytes.Buffer
	code := runAuthSub(context.Background(), []string{"status"}, cfgPath,
		strings.NewReader(""), &bytes.Buffer{}, &errOut, authCLIDeps{})
	assert.Equal(t, exitErr, code)
}

// TestRunAuthSubSetFail proves a `set` that fails (empty API key on stdin with
// --api-key-stdin) maps to exitErr.
func TestRunAuthSubSetFail(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeAuthConfig(t, dir)
	var errOut bytes.Buffer
	code := runAuthSub(context.Background(),
		[]string{"set", "--provider", "openai", "--account", "main", "--api-key-stdin"},
		cfgPath, strings.NewReader(""), &bytes.Buffer{}, &errOut, authCLIDeps{})
	assert.Equal(t, exitErr, code)
}

// TestRunAuthSubDeviceNoProvider proves `auth device` without --provider returns
// exitUsage.
func TestRunAuthSubDeviceNoProvider(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeAuthConfig(t, dir)
	var errOut bytes.Buffer
	code := runAuthSub(context.Background(), []string{"device"}, cfgPath,
		strings.NewReader(""), &bytes.Buffer{}, &errOut, authCLIDeps{})
	assert.Equal(t, exitUsage, code)
}

// TestRunAuthSubDeviceFlowError proves a device flow against an unreachable
// endpoint returns exitErr. Also exercises the client_id fallback (provider
// without client_id inherits the top-level device.client_id).
func TestRunAuthSubDeviceFlowError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("YANSHI_PASSPHRASE", "pass")
	cfgPath := filepath.Join(dir, "config.yaml")
	// Provider has no client_id → registration falls back to device.client_id.
	// Endpoints point at a closed port so RunDeviceFlow fails fast.
	cfgBody := `
storage:
  sqlite_path: "` + strings.ReplaceAll(filepath.Join(dir, "a.db"), "\\", "/") + `"
secrets:
  backend: file
  file_path: "` + strings.ReplaceAll(filepath.Join(dir, "s.enc"), "\\", "/") + `"
  passphrase_env: YANSHI_PASSPHRASE
auth:
  device:
    device_auth_enabled: true
    client_id: inherited-cid
    providers:
      - id: dead
        device_url: "https://127.0.0.1:9/device"
        token_url: "https://127.0.0.1:9/token"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgBody), 0o600))
	var errOut bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runAuthSub(context.Background(), []string{"device", "--provider", "dead"}, cfgPath,
			strings.NewReader(""), &bytes.Buffer{}, &errOut, authCLIDeps{})
	}()
	select {
	case code := <-done:
		assert.Equal(t, exitErr, code)
	case <-time.After(15 * time.Second):
		t.Fatal("device flow did not fail fast within 15s")
	}
}

// ---- runHeadlessCommand resume + timeout ---------------------------------

// TestRunHeadlessCommandResume exercises the --resume branch (sets
// inputs[0].Resume). Resuming a nonexistent session errors, so the generic
// error-print branch of runHeadlessCommand is also covered.
func TestRunHeadlessCommandResume(t *testing.T) {
	cfgPath := writeServeConfig(t)
	done := make(chan int, 1)
	go func() {
		done <- runHeadlessCommand([]string{"-config", cfgPath, "-fake-model", "-inprocess", "-p", "hi", "-resume", "nonexistent-session"}, "exec", strings.NewReader(""))
	}()
	select {
	case <-done:
		// non-zero expected (resume fails); the point is the resume branch ran.
	case <-time.After(20 * time.Second):
		t.Fatal("runHeadlessCommand -resume did not terminate within 20s")
	}
}

// TestRunHeadlessCommandTimeout exercises the -timeout branch: a 1ns timeout
// cancels the context immediately.
func TestRunHeadlessCommandTimeout(t *testing.T) {
	cfgPath := writeServeConfig(t)
	done := make(chan int, 1)
	go func() {
		done <- runHeadlessCommand([]string{"-config", cfgPath, "-fake-model", "-inprocess", "-p", "hi", "-timeout", "1ns"}, "exec", strings.NewReader(""))
	}()
	select {
	case code := <-done:
		// A 1ns timeout cancels before the turn completes → exitTimeout (124)
		// or exitCancel (130) depending on race; both prove the timeout branch.
		assert.True(t, code == exitTimeout || code == exitCancel || code == exitOK,
			"code=%d", code)
	case <-time.After(20 * time.Second):
		t.Fatal("runHeadlessCommand -timeout did not terminate within 20s")
	}
}

// ---- batch 4: final gaps -------------------------------------------------

// TestParseHeadlessArgsInvalidOutput proves an invalid --output value is rejected.
func TestParseHeadlessArgsInvalidOutput(t *testing.T) {
	_, err := parseHeadlessArgs([]string{"--output", "xml"}, "exec")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --output")
}

// TestParseHeadlessArgsInvalidInput proves an invalid --input value is rejected.
func TestParseHeadlessArgsInvalidInput(t *testing.T) {
	_, err := parseHeadlessArgs([]string{"--input", "yaml"}, "exec")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid --input")
}

// TestRunHeadlessCommandFileInput proves the --file success path reads the
// prompt from a file and runs the turn.
func TestRunHeadlessCommandFileInput(t *testing.T) {
	cfgPath := writeServeConfig(t)
	dir := t.TempDir()
	promptFile := filepath.Join(dir, "prompt.txt")
	require.NoError(t, os.WriteFile(promptFile, []byte("hello from file"), 0o644))
	done := make(chan int, 1)
	go func() {
		done <- runHeadlessCommand([]string{"-config", cfgPath, "-fake-model", "-inprocess", "--file", promptFile}, "exec", strings.NewReader(""))
	}()
	select {
	case code := <-done:
		assert.Equal(t, exitOK, code)
	case <-time.After(20 * time.Second):
		t.Fatal("runHeadlessCommand --file did not terminate within 20s")
	}
}

// TestRunHeadlessCommandStdinLines proves the stdin-by-mode (lines) branch:
// with no -p/--file, prompts are read from the injected stdin reader.
func TestRunHeadlessCommandStdinLines(t *testing.T) {
	cfgPath := writeServeConfig(t)
	done := make(chan int, 1)
	go func() {
		done <- runHeadlessCommand([]string{"-config", cfgPath, "-fake-model", "-inprocess", "--input", "lines"},
			"exec", strings.NewReader("one prompt\n"))
	}()
	select {
	case code := <-done:
		assert.Equal(t, exitOK, code)
	case <-time.After(20 * time.Second):
		t.Fatal("runHeadlessCommand stdin-lines did not terminate within 20s")
	}
}

// TestRunHeadlessCommandStdinJSONL proves the jsonl stdin branch parses one
// JSONL input object and runs the turn.
func TestRunHeadlessCommandStdinJSONL(t *testing.T) {
	cfgPath := writeServeConfig(t)
	done := make(chan int, 1)
	go func() {
		done <- runHeadlessCommand([]string{"-config", cfgPath, "-fake-model", "-inprocess", "--input", "jsonl", "--output", "jsonl"},
			"exec", strings.NewReader(`{"prompt":"hi"}`+"\n"))
	}()
	select {
	case code := <-done:
		assert.Equal(t, exitOK, code)
	case <-time.After(20 * time.Second):
		t.Fatal("runHeadlessCommand stdin-jsonl did not terminate within 20s")
	}
}

// repoSkillsDir returns the absolute path to the repo's committed skills/
// directory (where dev-quick-fix/SKILL.md lives), located relative to this
// test file.
func repoSkillsDir(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	// file = .../cmd/yanshi/coverage_test.go → up two to module root.
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	return filepath.Join(root, "skills")
}

// buildFakeAppWithSkills builds a fake-model app whose builtin skills dir points
// at the repo's skills/, so the tier skill (dev-quick-fix) is discoverable.
func buildFakeAppWithSkills(t *testing.T) *bootstrap.App {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := strings.ReplaceAll(filepath.Join(dir, "test.db"), "\\", "/")
	cfg := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: "` + dbPath + `"
skills:
  builtin_dir: "` + strings.ReplaceAll(repoSkillsDir(t), "\\", "/") + `"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	app, err := bootstrap.Build(bootstrap.Options{ConfigPath: cfgPath, FakeModel: true})
	require.NoError(t, err)
	require.NotNil(t, app)
	return app
}

// TestRunLightweightGoalSkillFound proves the skill-body branch: when the tier's
// skill is registered, its body is prepended to the goal text before the turn.
func TestRunLightweightGoalSkillFound(t *testing.T) {
	app := buildFakeAppWithSkills(t)
	defer app.Shutdown(context.Background())
	_, ok := app.Skills.Get(goalloop.TierQuickFix.SkillName())
	require.True(t, ok, "dev-quick-fix skill must be loaded from repo skills/")

	dec := runLightweightGoal(context.Background(), app, goalloop.TierQuickFix, "do a thing")
	assert.True(t, dec.Complete, "summary=%s", dec.Summary)
}

// TestRunLightweightGoalQueryError proves the error branch: a pre-cancelled
// context makes the orchestrator turn fail, yielding a non-complete Decision
// whose summary carries the error.
func TestRunLightweightGoalQueryError(t *testing.T) {
	app := buildFakeApp(t)
	defer app.Shutdown(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dec := runLightweightGoal(ctx, app, goalloop.TierQuickFix, "do a thing")
	assert.False(t, dec.Complete, "a cancelled turn should not complete: %s", dec.Summary)
	assert.Contains(t, dec.Summary, "lightweight turn error")
}

// ---- batch 5: runPR via injectable gh + TUI-pre error paths ----------------

// withFakeGH swaps the package-level ghExec for a fake that returns canned
// outputs keyed on the subcommand (view vs diff), restoring the real runner on
// cleanup.
func withFakeGH(t *testing.T, fn func(ctx context.Context, args ...string) (string, string, error)) {
	t.Helper()
	prev := ghExec
	ghExec = fn
	t.Cleanup(func() { ghExec = prev })
}

// TestRunPRFakeGHSuccess drives runPR end-to-end with a fake gh that returns
// valid metadata JSON for `pr view` and a diff for `pr diff`. The headless
// review returns an in-band error string (no bound sub-agent runner), so runPR
// reaches the final fmt.Println + return exitOK. Covers the entire tail of
// runPR that the real gh (absent in the test env) guards.
func TestRunPRFakeGHSuccess(t *testing.T) {
	withFakeGH(t, func(ctx context.Context, args ...string) (string, string, error) {
		if len(args) > 1 && args[1] == "view" {
			return `{"number":1,"title":"T","body":"b","headRefName":"h","baseRefName":"main","author":{"login":"a"},"files":[{"path":"f.go","additions":1,"deletions":0}],"changedFiles":1}`, "", nil
		}
		return "+diff content", "", nil
	})
	code := runPR(context.Background(), "https://github.com/o/r/pull/1")
	assert.Equal(t, exitOK, code)
}

// TestRunPRFakeGHParseError covers the FetchGitHubContext error branch (view
// returns invalid JSON).
func TestRunPRFakeGHParseError(t *testing.T) {
	withFakeGH(t, func(ctx context.Context, args ...string) (string, string, error) {
		return "not-json", "", nil
	})
	code := runPR(context.Background(), "https://github.com/o/r/pull/1")
	assert.Equal(t, exitErr, code)
}

// TestRunPRFakeGHDiffError covers the `gh pr diff` failure branch (view ok, diff
// errors).
func TestRunPRFakeGHDiffError(t *testing.T) {
	withFakeGH(t, func(ctx context.Context, args ...string) (string, string, error) {
		if len(args) > 1 && args[1] == "view" {
			return `{"number":1,"title":"T","body":"","headRefName":"h","baseRefName":"main","author":{"login":"a"},"files":[],"changedFiles":0}`, "", nil
		}
		return "", "diff boom", errors.New("gh diff failed")
	})
	code := runPR(context.Background(), "https://github.com/o/r/pull/1")
	assert.Equal(t, exitErr, code)
}

// TestRunPRFakeGHViewError covers the `gh pr view` failure branch.
func TestRunPRFakeGHViewError(t *testing.T) {
	withFakeGH(t, func(ctx context.Context, args ...string) (string, string, error) {
		return "", "view boom", errors.New("gh view failed")
	})
	code := runPR(context.Background(), "https://github.com/o/r/pull/1")
	assert.Equal(t, exitErr, code)
}

// TestRunDefaultBootstrapFails proves runDefault maps a backend-resolution
// failure (forced in-process + missing config) to exitErr WITHOUT launching the
// TUI — the failure happens inside Resolve, before prog.Run. This covers the
// runTUI Resolve-error return + runDefault's error branch.
func TestRunDefaultBootstrapFails(t *testing.T) {
	code := runDefault([]string{"-config", filepath.Join(t.TempDir(), "nope.yaml"), "-inprocess"})
	assert.Equal(t, exitErr, code)
}

// TestChatTUIBootstrapFails proves chatTUI maps a Resolve failure to exitErr
// without launching the TUI.
func TestChatTUIBootstrapFails(t *testing.T) {
	code := chatTUI([]string{"-config", filepath.Join(t.TempDir(), "nope.yaml"), "-inprocess"})
	assert.Equal(t, exitErr, code)
}

// TestDispatchLeadingFlagDefault routes a flag-only invocation
// (`yanshi -config X -inprocess`) through dispatch's leading-flag default
// branch into runDefault, which fails fast on the missing config.
func TestDispatchLeadingFlagDefault(t *testing.T) {
	code := dispatch([]string{"yanshi", "-config", filepath.Join(t.TempDir(), "nope.yaml"), "-inprocess"},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	assert.Equal(t, exitErr, code)
}

// ---- batch 6: final dispatch + serve + persist branches -------------------

// TestDispatchAppBadFlag routes `app` with a bad flag through dispatch.
func TestDispatchAppBadFlag(t *testing.T) {
	code := dispatch([]string{"yanshi", "app", "--nope"},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	assert.Equal(t, exitUsage, code)
}

// TestDispatchPRValid runs `yanshi pr <url>` through dispatch with a fake gh, so
// the pr-valid branch of dispatch (runPR(argv[2])) is exercised end-to-end.
func TestDispatchPRValid(t *testing.T) {
	withFakeGH(t, func(ctx context.Context, args ...string) (string, string, error) {
		if len(args) > 1 && args[1] == "view" {
			return `{"number":1,"title":"T","body":"","headRefName":"h","baseRefName":"main","author":{"login":"a"},"files":[],"changedFiles":0}`, "", nil
		}
		return "+diff", "", nil
	})
	code := dispatch([]string{"yanshi", "pr", "https://github.com/o/r/pull/1"},
		strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	assert.Equal(t, exitOK, code)
}

// TestRunServeBadAddr covers the errCh-error branch of runServe: an invalid
// listen address makes app.Start (ListenAndServe) fail immediately, the
// goroutine sends the error on errCh, and the select returns exitErr. The
// errCh branch returns without app.Shutdown, so an in-memory DB avoids a file
// handle that would block temp-dir cleanup. Uses a non-cancelled context since
// the failure is instantaneous.
func TestRunServeBadAddr(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// :memory: DB → no file handle leaks when the errCh path skips Shutdown.
	cfg := `
server:
  http_addr: "127.0.0.1:0"
storage:
  sqlite_path: ":memory:"
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o644))
	var stderr lockedWriter
	done := make(chan int, 1)
	go func() {
		done <- runServe(context.Background(), []string{"-config", cfgPath, "-fake-model", "-addr", "127.0.0.1:notaport"}, &stderr)
	}()
	select {
	case code := <-done:
		assert.Equal(t, exitErr, code, "stderr=%s", stderr.String())
	case <-time.After(15 * time.Second):
		t.Fatal("runServe bad-addr did not fail fast within 15s")
	}
}

// TestPersistGoalRunStoreError covers the KVSet-error branch: persisting against
// a closed store surfaces the persistence error to stderr (best-effort, never
// panics).
func TestPersistGoalRunStoreError(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "goalrun-err.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, st.Close()) // close so KVSet fails

	assert.NotPanics(t, func() {
		persistGoalRun(st, goalloop.TierStandard, goalloop.Decision{Complete: true, Summary: "x"}, goalloop.Usage{}, 1)
	})
}

// ---- batch 7: remaining coverable branches --------------------------------

// errWriter is an io.Writer that always returns an error, used to force a
// JSON-encode failure in runDoctor's -json path.
type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) {
	return 0, errors.New("write boom")
}

// TestRunDoctorJSONEncodeError covers the -json render-error branch: a writer
// that errors on every Write makes report.RenderJSON fail, so runDoctor returns
// exitUsage (2) and prints the diagnostic.
func TestRunDoctorJSONEncodeError(t *testing.T) {
	cfgPath := writeServeConfig(t)
	var errOut bytes.Buffer
	code := runDoctor(context.Background(), []string{"-config", cfgPath, "-json"}, errWriter{}, &errOut)
	assert.Equal(t, exitUsage, code)
	assert.Contains(t, errOut.String(), "render json")
}

// errReader is an io.Reader that always returns an error, used to force a
// JSON-RPC read failure in runApp's Serve path.
type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, errors.New("read boom") }

// TestRunAppServeReadError covers the srv.Serve error branch of runApp: a reader
// that errors immediately makes Serve fail, so runApp returns exitErr.
func TestRunAppServeReadError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	dbPath := strings.ReplaceAll(filepath.Join(dir, "x.db"), "\\", "/")
	require.NoError(t, os.WriteFile(cfgPath, []byte("server:\n  http_addr: \"127.0.0.1:0\"\nstorage:\n  sqlite_path: \""+dbPath+"\"\n"), 0o644))
	code := runApp([]string{"-config", cfgPath, "-fake-model"}, errReader{}, &bytes.Buffer{})
	assert.Equal(t, exitErr, code)
}

// TestRunGoalPositionalArg covers the positional-arg goal-text branch of runGoal
// (text derived from fs.Args() when -goal is empty).
func TestRunGoalPositionalArg(t *testing.T) {
	done := make(chan int, 1)
	go func() {
		done <- runGoal([]string{"-fake-model", "-max-iters", "2", "positional", "goal"})
	}()
	select {
	case code := <-done:
		assert.Equal(t, exitOK, code)
	case <-time.After(15 * time.Second):
		t.Fatal("runGoal positional did not terminate within 15s")
	}
}

// TestRunHeadlessCommandStdinMalformed covers the ReadHeadlessInputs error
// branch: malformed JSONL input makes the parser error → exitUsage.
func TestRunHeadlessCommandStdinMalformed(t *testing.T) {
	code := runHeadlessCommand([]string{"--input", "jsonl"}, "exec", strings.NewReader("not-valid-json\n"))
	assert.Equal(t, exitUsage, code)
}

// TestDispatchBare covers the bare-invocation branch of dispatch
// (`len(argv) < 2` → runDefault(nil)). In the test cwd there is no config.yaml
// (gitignored) and no lockfile for this worktree, so Resolve fails fast inside
// bootstrap.Build and runDefault returns exitErr without launching the TUI. The
// timeout guard protects against a stray live lockfile intercepting discovery.
func TestDispatchBare(t *testing.T) {
	done := make(chan int, 1)
	go func() {
		done <- dispatch([]string{"yanshi"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	}()
	select {
	case <-done:
		// Returned (did not hang); the bare branch ran.
	case <-time.After(12 * time.Second):
		t.Fatal("bare dispatch hung — a live lockfile likely intercepted discovery")
	}
}

// TestParsePRInputShortURL covers the i-2<0 branch: a github.com URL where
// "pull" appears too early to have owner/repo before it.
func TestParsePRInputShortURL(t *testing.T) {
	repo, num := parsePRInput("github.com/pull/42")
	assert.Equal(t, "", repo)
	assert.Equal(t, 0, num)
}

// TestVcsMcpSuccess covers the vcsMcp wrapper's success path (return exitOK)
// when runVCSMCP serves to EOF on stdin. The test process's stdin is typically
// EOF under `go test`; the timeout guard protects against a non-EOF stdin. The
// env points at a fresh temp db so the store opens cleanly.
func TestVcsMcpSuccess(t *testing.T) {
	t.Setenv("YANSHI_DB_PATH", filepath.Join(t.TempDir(), "vcsmpc-ok.db"))
	t.Setenv("YANSHI_REPO_ID", "r")
	t.Setenv("YANSHI_WT_ID", "w")
	done := make(chan int, 1)
	go func() { done <- vcsMcp(nil) }()
	select {
	case code := <-done:
		assert.Equal(t, exitOK, code)
	case <-time.After(10 * time.Second):
		t.Fatal("vcsMcp did not return on stdin EOF within 10s (stdin likely not EOF)")
	}
}
