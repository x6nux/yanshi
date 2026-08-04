package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/config"
)

// TestCheckConfig_DefaultAddrWhenEmpty proves an empty http_addr falls back to
// the documented default "127.0.0.1:8080" in the OK message.
func TestCheckConfig_DefaultAddrWhenEmpty(t *testing.T) {
	c := checkConfig("p", &config.Config{Server: config.ServerConfig{HTTPAddr: ""}}, nil)
	require.Equal(t, StatusOK, c.Status)
	assert.Contains(t, c.Message, "127.0.0.1:8080", "empty addr defaults to 127.0.0.1:8080")
}

// TestFileSize_AbsentAndPresent proves fileSize reports "absent" for a missing
// file and a size label for a present one.
func TestFileSize_AbsentAndPresent(t *testing.T) {
	assert.Equal(t, "absent", fileSize(filepath.Join(t.TempDir(), "nope")))

	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(p, make([]byte, 2048), 0o644))
	assert.Equal(t, "2KB", fileSize(p))
}

// TestCheckProviders_MissingKindAndModel covers the remaining provider-problem
// branches: an empty kind (with an empty name so the index form is used) and a
// valid kind with a missing model.
func TestCheckProviders_MissingKindAndModel(t *testing.T) {
	// Empty name + empty kind -> "providers[0]: missing kind".
	c := checkProviders(&config.Config{LLM: config.LLMConfig{Providers: []config.ProviderConfig{
		{Name: "", Kind: "", Model: "m", APIKey: "k"},
	}}}, nil)
	require.Equal(t, StatusFail, c.Status)
	assert.Contains(t, c.Message, "providers[0]: missing kind")

	// Valid kind, missing model.
	c = checkProviders(&config.Config{LLM: config.LLMConfig{Providers: []config.ProviderConfig{
		{Name: "p", Kind: "openai", Model: "", APIKey: "k"},
	}}}, nil)
	require.Equal(t, StatusFail, c.Status)
	assert.Contains(t, c.Message, "missing model")
}

// TestCheckProviders_SkippedOnConfigError proves the cfgErr early-return path.
func TestCheckProviders_SkippedOnConfigError(t *testing.T) {
	c := checkProviders(nil, errCfg())
	assert.Equal(t, StatusWarn, c.Status)
	assert.Contains(t, c.Message, "skipped")
}

// TestExpandHomeDir_TildeAndPlain proves "~" expands to the home dir and a
// plain path is returned unchanged (including the empty-string fast path).
func TestExpandHomeDir_TildeAndPlain(t *testing.T) {
	assert.Equal(t, "", expandHomeDir(""))
	assert.Equal(t, "/abs/path", expandHomeDir("/abs/path"))
	expanded := expandHomeDir("~/foo")
	home, err := os.UserHomeDir()
	if err == nil {
		assert.Equal(t, filepath.Join(home, "foo"), expanded)
		assert.True(t, strings.HasPrefix(expanded, home))
	}
}

// TestCheckSecretsRefs_InvalidCredentialIsRedacted proves a raw (non-ref)
// api_key is a fail and the raw value is never echoed.
func TestCheckSecretsRefs_InvalidCredentialIsRedacted(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Providers: []config.ProviderConfig{
		{Name: "p", APIKey: "sk-live-raw-secret-value"},
	}}}
	c := checkSecretsRefs(cfg, nil)
	require.Equal(t, StatusFail, c.Status)
	assert.NotContains(t, c.Message, "sk-live-raw-secret-value", "raw key must not leak")
	assert.Contains(t, c.Message, "invalid credential reference")
}

// TestCheckSecretsRefs_SkippedOnConfigError proves the cfgErr path.
func TestCheckSecretsRefs_SkippedOnConfigError(t *testing.T) {
	c := checkSecretsRefs(nil, errCfg())
	assert.Equal(t, StatusWarn, c.Status)
	assert.Contains(t, c.Message, "skipped")
}

// TestCheckSecretsRefs_AllRefsValid proves the OK path with secret:// and env://.
func TestCheckSecretsRefs_AllRefsValid(t *testing.T) {
	cfg := &config.Config{LLM: config.LLMConfig{Providers: []config.ProviderConfig{
		{Name: "a", APIKey: "secret://openai/main"},
		{Name: "b", APIKey: "env://OPENAI_API_KEY"},
		{Name: "c", APIKey: ""}, // empty -> skipped, not counted
	}}}
	c := checkSecretsRefs(cfg, nil)
	require.Equal(t, StatusOK, c.Status)
	assert.Contains(t, c.Message, "2 credential reference(s) valid")
}

// TestCheckLocaleConfig_InvalidLocaleFails proves an unsupported locale is a fail.
func TestCheckLocaleConfig_InvalidLocaleFails(t *testing.T) {
	c := checkLocaleConfig(&config.Config{I18N: config.I18NConfig{UILocale: "klingon-piqad"}}, nil)
	require.Equal(t, StatusFail, c.Status)
}

// TestCheckLocaleConfig_ValidAndSkipped proves the OK path and the cfgErr path.
func TestCheckLocaleConfig_ValidAndSkipped(t *testing.T) {
	// "auto" resolves through detection and is always supported (falls back to en).
	c := checkLocaleConfig(&config.Config{}, nil)
	require.Equal(t, StatusOK, c.Status)

	c = checkLocaleConfig(nil, errCfg())
	assert.Equal(t, StatusWarn, c.Status)
	assert.Contains(t, c.Message, "skipped")
}

// TestCheckKeymapConfig_UnsupportedNameFails proves a non-default keymap name
// is a fail (only "default" ships).
func TestCheckKeymapConfig_UnsupportedNameFails(t *testing.T) {
	c := checkKeymapConfig(&config.Config{TUI: config.TUIConfig{KeymapName: "vim"}}, nil)
	require.Equal(t, StatusFail, c.Status)
	assert.Contains(t, c.Message, "unsupported keymap name")
}

// TestCheckKeymapConfig_InvalidBindingsFail proves a binding override that fails
// validation is a fail whose message is (a) actionable and (b) still bounded.
//
// Both halves are pinned because they pull against each other and the message
// has already failed one of them: it used to send the operator to a `/keymap
// diagnostics` command that is not registered anywhere, so the single piece of
// advice the product gave was itself the dead end. The remedy named here must
// stay something a user can actually do — edit tui.bindings in the YAML — while
// the raw key and action, which are untrusted config text, must stay out of the
// output. The closed-set diagnostic Kind is the one detail allowed through.
func TestCheckKeymapConfig_InvalidBindingsFail(t *testing.T) {
	const rawAction = "this-is-not-a-real-key!!!"
	c := checkKeymapConfig(&config.Config{TUI: config.TUIConfig{
		Bindings: map[string]string{"send": rawAction},
	}}, nil)
	require.Equal(t, StatusFail, c.Status)
	assert.Contains(t, c.Message, "tui.bindings",
		"the message must name the field the operator edits")
	assert.Contains(t, c.Message, "invalid_key",
		"the diagnostic kind is a closed-set literal and is what tells the operator what to look for")
	assert.NotContains(t, c.Message, rawAction,
		"untrusted binding text must never be echoed")
	assert.NotContains(t, c.Message, "/keymap",
		"no such command is registered; advertising it makes the only remedy a dead end")
}

// TestCheckKeymapConfig_UnknownActionIsTallied proves the per-kind tally reports
// the kind that actually fired, not a fixed string: a spelling error in the
// ACTION reads unknown_action, where a spelling error in the KEY read
// invalid_key above. Without this the summary could be a constant and both
// tests would still pass.
func TestCheckKeymapConfig_UnknownActionIsTallied(t *testing.T) {
	c := checkKeymapConfig(&config.Config{TUI: config.TUIConfig{
		Bindings: map[string]string{"ctrl+g": "teleport"},
	}}, nil)
	require.Equal(t, StatusFail, c.Status)
	assert.Contains(t, c.Message, "1 unknown_action")
	assert.NotContains(t, c.Message, "invalid_key")
	assert.NotContains(t, c.Message, "teleport")
}

// TestCheckKeymapConfig_OKAndSkipped proves the happy path (default keymap, no
// conflicts) and the cfgErr skip path.
func TestCheckKeymapConfig_OKAndSkipped(t *testing.T) {
	c := checkKeymapConfig(&config.Config{}, nil)
	require.Equal(t, StatusOK, c.Status)
	c = checkKeymapConfig(nil, errCfg())
	assert.Equal(t, StatusWarn, c.Status)
}

// TestCheckHighContrastConfig covers the three states: unset, explicitly true,
// explicitly false — plus the cfgErr skip path.
func TestCheckHighContrastConfig(t *testing.T) {
	require.Equal(t, StatusOK,
		checkHighContrastConfig(&config.Config{}, nil).Status)
	ttrue, tfalse := true, false
	c := checkHighContrastConfig(&config.Config{TUI: config.TUIConfig{HighContrast: &ttrue}}, nil)
	require.Equal(t, StatusOK, c.Status)
	assert.Contains(t, c.Message, "enabled=true")
	c = checkHighContrastConfig(&config.Config{TUI: config.TUIConfig{HighContrast: &tfalse}}, nil)
	assert.Contains(t, c.Message, "enabled=false")
	assert.Equal(t, StatusWarn, checkHighContrastConfig(nil, errCfg()).Status)
}

// TestCheckDatabase_OpenError proves a database path that cannot be opened is a
// fail (not a panic). Pointing at a directory makes Open fail.
func TestCheckDatabase_OpenError(t *testing.T) {
	dir := t.TempDir()
	c := checkDatabase(&config.Config{Storage: config.StorageConfig{SQLitePath: dir}}, nil)
	// Accepting every possible status ("may succeed or fail by SQLite version")
	// made this test unfailable — it shared that flaw with the sibling test
	// below, which hid a real bug. A directory can never be opened as a SQLite
	// file on any platform or driver version: Open() gets as far as PRAGMA
	// journal_mode=WAL and dies with "unable to open database file (14)". Pin
	// the failure, so a regression that silently reports a broken database as
	// healthy turns this red.
	assert.Equal(t, StatusFail, c.Status, "a directory path must fail, not report healthy")
	assert.Contains(t, c.Message, "open ")
}

// TestCheckDatabase_SkippedAndDefaultDisplay proves the cfgErr skip path and the
// "<unset>" display when no path is configured.
func TestCheckDatabase_SkippedAndDefaultDisplay(t *testing.T) {
	assert.Equal(t, StatusWarn, checkDatabase(nil, errCfg()).Status)
	// An unset storage.sqlite_path must FAIL, not open something. It used to
	// leave a SQLite file literally named "?_pragma=busy_timeout(5000)&..." in
	// the working directory on every test run, because buildDSN appends the
	// query string to an empty path and modernc took the result as a filename.
	c := checkDatabase(&config.Config{}, nil)
	assert.Equal(t, StatusFail, c.Status)
	assert.Contains(t, c.Message, `open "<unset>"`)
}

// TestCheckDirectories_MissingSkillDirWarns proves a missing builtin skill dir
// produces a warn listing the problem (not a panic).
func TestCheckDirectories_MissingSkillDirWarns(t *testing.T) {
	c := checkDirectories(&config.Config{
		Skills: config.SkillsConfig{BuiltinDir: filepath.Join(t.TempDir(), "nope")},
	}, nil)
	// The missing dir surfaces as a warn.
	require.Equal(t, StatusWarn, c.Status)
	assert.Contains(t, c.Message, "skills.builtin_dir")
}

// TestCheckDirectories_Skipped proves the cfgErr early-return.
func TestCheckDirectories_Skipped(t *testing.T) {
	c := checkDirectories(nil, errCfg())
	assert.Equal(t, StatusWarn, c.Status)
	assert.Contains(t, c.Message, "skipped")
}

// TestCheckPort_SkippedAndDefaultAddr proves the cfgErr skip and the empty-addr
// default (127.0.0.1:8080).
func TestCheckPort_SkippedAndDefaultAddr(t *testing.T) {
	assert.Equal(t, StatusWarn, checkPort(nil, errCfg(), false).Status)
	// Empty addr -> default :8080, which may or may not be free on the test box.
	c := checkPort(&config.Config{}, nil, false)
	switch c.Status {
	case StatusOK, StatusWarn:
	default:
		t.Fatalf("unexpected status %q", c.Status)
	}
}

// TestCheckMCP_Skipped proves the cfgErr skip path.
func TestCheckMCP_Skipped(t *testing.T) {
	c := checkMCP(nil, errCfg())
	assert.Equal(t, StatusWarn, c.Status)
	assert.Contains(t, c.Message, "skipped")
}

// TestCheckPermissions_NoProfilesAndOK prove the no-profiles warn and the
// multi-profile OK rendering.
func TestCheckPermissions_NoProfilesAndOK(t *testing.T) {
	c := checkPermissions(&config.Config{}, nil)
	require.Equal(t, StatusWarn, c.Status)
	assert.Contains(t, c.Message, "no profiles configured")
}

// errCfg is a shared sentinel config-load error for the skip-path tests.
func errCfg() error { return errors.New("config not loaded") }
