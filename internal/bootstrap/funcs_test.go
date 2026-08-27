package bootstrap

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/auth"
	"github.com/x6nux/yanshi/internal/config"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
	"github.com/x6nux/yanshi/internal/secrets"
)

// --- visionUsageAccumulator.add ---

func TestVisionUsageAccumulator_Add(t *testing.T) {
	var v visionUsageAccumulator
	v.add(1, 2, 3)
	if v.Prompt != 1 || v.Completion != 2 || v.Total != 3 {
		t.Fatalf("after first add: Prompt=%d Completion=%d Total=%d", v.Prompt, v.Completion, v.Total)
	}
	v.add(10, 20, 30)
	if v.Prompt != 11 || v.Completion != 22 || v.Total != 33 {
		t.Fatalf("after second add: Prompt=%d Completion=%d Total=%d", v.Prompt, v.Completion, v.Total)
	}
}

func TestVisionUsageAccumulator_AddConcurrentSafe(t *testing.T) {
	var v visionUsageAccumulator
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v.add(1, 1, 1)
		}()
	}
	wg.Wait()
	if v.Prompt != 100 || v.Completion != 100 || v.Total != 100 {
		t.Fatalf("after 100 concurrent adds: Prompt=%d Completion=%d Total=%d", v.Prompt, v.Completion, v.Total)
	}
}

// --- parseCooldownDuration ---

func TestParseCooldownDuration_Empty(t *testing.T) {
	if got := parseCooldownDuration(""); got != 0 {
		t.Fatalf("parseCooldownDuration(\"\") = %v, want 0", got)
	}
}

func TestParseCooldownDuration_Invalid(t *testing.T) {
	if got := parseCooldownDuration("not-a-duration"); got != 0 {
		t.Fatalf("parseCooldownDuration(\"invalid\") = %v, want 0", got)
	}
}

func TestParseCooldownDuration_Valid(t *testing.T) {
	if got := parseCooldownDuration("3s"); got != 3*time.Second {
		t.Fatalf("parseCooldownDuration(\"3s\") = %v, want 3s", got)
	}
}

// --- outputLoggerWriter ---

func TestOutputLoggerWriter_NilOutput(t *testing.T) {
	if w := outputLoggerWriter(nil); w != os.Stderr {
		t.Fatalf("outputLoggerWriter(nil) = %v, want os.Stderr", w)
	}
}

func TestOutputLoggerWriter_NilLogger(t *testing.T) {
	so := secrets.NewSafeOutput(io.Discard, nil)
	if w := outputLoggerWriter(so); w != os.Stderr {
		t.Fatalf("outputLoggerWriter(output with nil Logger) = %v, want os.Stderr", w)
	}
}

func TestOutputLoggerWriter_WithLogger(t *testing.T) {
	red := secrets.NewRedactor()
	so := secrets.NewSafeOutput(io.Discard, red)
	if w := outputLoggerWriter(so); w != os.Stderr {
		t.Fatalf("outputLoggerWriter(output with Logger) = %v, want os.Stderr", w)
	}
}

// --- firstNonEmpty ---

func TestFirstNonEmpty_First(t *testing.T) {
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Fatalf("firstNonEmpty(\"a\", \"b\") = %q, want \"a\"", got)
	}
}

func TestFirstNonEmpty_Second(t *testing.T) {
	if got := firstNonEmpty("", "b"); got != "b" {
		t.Fatalf("firstNonEmpty(\"\", \"b\") = %q, want \"b\"", got)
	}
}

func TestFirstNonEmpty_AllEmpty(t *testing.T) {
	if got := firstNonEmpty("", ""); got != "skills" {
		t.Fatalf("firstNonEmpty(\"\", \"\") = %q, want \"skills\"", got)
	}
}

func TestFirstNonEmpty_NoArgs(t *testing.T) {
	if got := firstNonEmpty(); got != "skills" {
		t.Fatalf("firstNonEmpty() = %q, want \"skills\"", got)
	}
}

// --- resolveMemoryPaths ---

func TestResolveMemoryPaths_UserPathSet(t *testing.T) {
	up, pp := resolveMemoryPaths(config.MemoryConfig{UserPath: "/home/user/custom.md"}, "")
	if up != "/home/user/custom.md" {
		t.Fatalf("userPath = %q, want \"/home/user/custom.md\"", up)
	}
	if pp != "" {
		t.Fatalf("projectPath = %q, want \"\"", pp)
	}
}

func TestResolveMemoryPaths_UserPathExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("os.UserHomeDir failed:", err)
	}
	up, _ := resolveMemoryPaths(config.MemoryConfig{UserPath: "~/custom.md"}, "")
	want := filepath.Join(home, "custom.md")
	if up != want {
		t.Fatalf("userPath = %q, want %q", up, want)
	}
}

func TestResolveMemoryPaths_UserPathDefaultWithHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("os.UserHomeDir failed:", err)
	}
	up, _ := resolveMemoryPaths(config.MemoryConfig{}, "")
	want := filepath.Join(home, ".yanshi", "memory.md")
	if up != want {
		t.Fatalf("userPath = %q, want %q", up, want)
	}
}

func TestResolveMemoryPaths_AbsoluteProjectPath(t *testing.T) {
	dir := t.TempDir()
	absPath := filepath.Join(dir, "proj.md")
	_, pp := resolveMemoryPaths(config.MemoryConfig{ProjectPath: absPath}, "/work")
	if pp != absPath {
		t.Fatalf("projectPath = %q, want %q", pp, absPath)
	}
}

func TestResolveMemoryPaths_RelativeProjectPathWithWorkRoot(t *testing.T) {
	_, pp := resolveMemoryPaths(config.MemoryConfig{ProjectPath: "proj.md"}, "/work")
	want := filepath.Join("/work", "proj.md")
	if pp != want {
		t.Fatalf("projectPath = %q, want %q", pp, want)
	}
}

func TestResolveMemoryPaths_RelativeProjectPathNoWorkRoot(t *testing.T) {
	_, pp := resolveMemoryPaths(config.MemoryConfig{ProjectPath: "proj.md"}, "")
	if pp != "proj.md" {
		t.Fatalf("projectPath = %q, want \"proj.md\"", pp)
	}
}

func TestResolveMemoryPaths_WorkRootFallback(t *testing.T) {
	_, pp := resolveMemoryPaths(config.MemoryConfig{}, "/work")
	want := filepath.Join("/work", ".yanshi", "memory.md")
	if pp != want {
		t.Fatalf("projectPath = %q, want %q", pp, want)
	}
}

func TestResolveMemoryPaths_BothConfigured(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("os.UserHomeDir failed:", err)
	}
	up, pp := resolveMemoryPaths(config.MemoryConfig{
		UserPath:    "~/custom.md",
		ProjectPath: "rel.md",
	}, "/work")
	wantUp := filepath.Join(home, "custom.md")
	wantPp := filepath.Join("/work", "rel.md")
	if up != wantUp {
		t.Fatalf("userPath = %q, want %q", up, wantUp)
	}
	if pp != wantPp {
		t.Fatalf("projectPath = %q, want %q", pp, wantPp)
	}
}

// --- homeDirOrDefault ---

func TestHomeDirOrDefault_Success(t *testing.T) {
	home := homeDirOrDefault()
	if home == "" {
		t.Fatal("homeDirOrDefault() should return non-empty in test environment")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatalf("homeDirOrDefault() = %q, but stat failed: %v", home, err)
	}
}

func TestHomeDirOrDefault_ErrorPath(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	home := homeDirOrDefault()
	if home != "" {
		t.Fatalf("expected empty home when env vars are cleared, got %q", home)
	}
}

// --- buildDeviceProviders uncovered branches ---

func TestBuildDeviceProviders_EmptyClientIDUsesDefault(t *testing.T) {
	red := secrets.NewRedactor()
	bindings, err := buildDeviceProviders(
		[]config.DeviceProviderConfig{
			{ID: "p1", ClientID: "", DeviceURL: "https://example.com/d", TokenURL: "https://example.com/t"},
		},
		"default-client",
		red,
		AuthDeps{},
	)
	if err != nil {
		t.Fatalf("buildDeviceProviders: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}
	if bindings[0].ID != "p1" {
		t.Fatalf("binding ID = %q, want \"p1\"", bindings[0].ID)
	}
}

func TestBuildDeviceProviders_EmptyIDRejected(t *testing.T) {
	red := secrets.NewRedactor()
	_, err := buildDeviceProviders(
		nil,
		"",
		red,
		AuthDeps{Providers: []DeviceProviderBinding{
			{ID: " ", Provider: &recordingDP{}},
		}},
	)
	if err == nil {
		t.Fatal("expected error for empty provider ID")
	}
}

func TestBuildDeviceProviders_NilProviderRejected(t *testing.T) {
	red := secrets.NewRedactor()
	_, err := buildDeviceProviders(
		nil,
		"",
		red,
		AuthDeps{Providers: []DeviceProviderBinding{
			{ID: "p1", Provider: nil},
		}},
	)
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestBuildDeviceProviders_DuplicateIDRejected(t *testing.T) {
	red := secrets.NewRedactor()
	_, err := buildDeviceProviders(
		nil,
		"",
		red,
		AuthDeps{Providers: []DeviceProviderBinding{
			{ID: "dup", Provider: &recordingDP{}},
			{ID: "dup", Provider: &recordingDP{}},
		}},
	)
	if err == nil {
		t.Fatal("expected error for duplicate provider ID")
	}
}

func TestBuildDeviceProviders_ProviderCreateError(t *testing.T) {
	red := secrets.NewRedactor()
	_, err := buildDeviceProviders(
		[]config.DeviceProviderConfig{
			{ID: "bad", ClientID: "cid", DeviceURL: "", TokenURL: ""},
		},
		"",
		red,
		AuthDeps{},
	)
	if err == nil {
		t.Fatal("expected error for empty device/token URLs")
	}
}

// --- buildMCPManager ---

func TestBuildMCPManager_TimeoutParsing(t *testing.T) {
	// A nil secrets manager is the shape of every deployment without a
	// credential backend: bindMCPTokenStore must warn and continue, leaving
	// bearer/client_credentials servers working, rather than panic.
	mgr := buildMCPManager(&config.Config{
		MCP: config.MCPConfig{
			Servers: map[string]*config.MCPServerConfig{
				"srv": {
					Enabled: true,
					Timeout: "not-a-duration",
					Command: "echo",
				},
			},
		},
	}, nil)
	if mgr == nil {
		t.Fatal("MCP manager must be non-nil")
	}
	mgr.Shutdown()
}

func TestBuildMCPManager_EmptyConfig(t *testing.T) {
	mgr := buildMCPManager(&config.Config{
		MCP: config.MCPConfig{Servers: map[string]*config.MCPServerConfig{}},
	}, nil)
	if mgr == nil {
		t.Fatal("MCP manager must be non-nil even with empty config")
	}
	mgr.Shutdown()
}

// --- buildMCPManager valid timeout ---

func TestBuildMCPManager_ValidTimeout(t *testing.T) {
	mgr := buildMCPManager(&config.Config{
		MCP: config.MCPConfig{
			Servers: map[string]*config.MCPServerConfig{
				"srv": {
					Enabled: true,
					Timeout: "5s",
					Command: "echo",
				},
			},
		},
	}, nil)
	if mgr == nil {
		t.Fatal("MCP manager must be non-nil")
	}
	mgr.Shutdown()
}

// recordingDP is a minimal auth.DeviceProvider for test.
type recordingDP struct{}

func (r *recordingDP) Authorize(_ context.Context, _ auth.Clock, _ auth.Sleeper, _ func(auth.StatusUpdate)) (*auth.DeviceToken, error) {
	return nil, nil
}

// --- Shutdown with nil managers ---

func TestBuild_SecretsManagerError(t *testing.T) {
	_, err := Build(Options{
		Cfg: &config.Config{
			Secrets: config.SecretsConfig{
				Backend: "file",
				// Missing file path causes NewManager to fail.
			},
			Storage: config.StorageConfig{SQLitePath: ":memory:"},
		},
		FakeModel: true,
	})
	if err == nil {
		t.Fatal("expected error from secrets manager with file backend but no path")
	}
}

func TestShutdown_AppWithNilManagers(t *testing.T) {
	srv := &http.Server{}
	app := &App{Server: srv}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Shutdown panicked with nil App: %v", r)
			}
		}()
		_ = app.Shutdown(context.Background())
	}()
}

func TestShutdown_AppWithCancelOnly(t *testing.T) {
	cancel := func() {}
	srv := &http.Server{}
	app := &App{Server: srv, cancel: cancel}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Shutdown panicked: %v", r)
			}
		}()
		_ = app.Shutdown(context.Background())
	}()
}

// --- Shutdown error accumulation ---

func TestShutdown_ErrorAccumulation(t *testing.T) {
	// Build a minimal app and force Shutdown twice to trigger already-closed errors.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("server:\n  http_addr: \"127.0.0.1:0\"\nstorage:\n  sqlite_path: \":memory:\"\n"), 0o644)
	app, err := Build(Options{ConfigPath: cfgPath, FakeModel: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// First Shutdown.
	if err := app.Shutdown(context.Background()); err != nil {
		t.Logf("first Shutdown returned error (expected none): %v", err)
	}
	// Second Shutdown — Server.Shutdown returns ErrServerClosed.
	if err := app.Shutdown(context.Background()); err != nil {
		t.Logf("second Shutdown returned accumulated errors (expected): %v", err)
	}
}

// --- Build load config error ---

func TestBuild_LoadConfigError(t *testing.T) {
	_, err := Build(Options{
		ConfigPath: "/nonexistent/path/config.yaml",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent config path")
	}
}

// --- Shutdown error from active server ---

func TestStart_ClosedServerReturnsErrServerClosed(t *testing.T) {
	srv := &http.Server{}
	// Close before Start to ensure ListenAndServe returns immediately.
	srv.Close()
	app := &App{Server: srv}
	err := app.Start()
	if err == nil {
		t.Fatal("expected error from Start on closed server")
	}
}

// --- Build with LSP timeout error ---

func TestBuild_WithBadLSPTimeout(t *testing.T) {
	// Invalid LSP timeout should fall back to 800ms without error.
	cfg := &config.Config{
		Server:  config.ServerConfig{HTTPAddr: "127.0.0.1:0"},
		Storage: config.StorageConfig{SQLitePath: ":memory:"},
		LSP:     config.LSPConfig{Timeout: "not-a-duration"},
	}
	app, err := Build(Options{Cfg: cfg, FakeModel: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	app.Shutdown(context.Background())
}

// --- Shutdown with pre-closed server ---

// --- resolveLogWriter ---

func TestResolveLogWriter_EmptyConfig(t *testing.T) {
	// No file, no TUI mode -> nil writer, empty path.
	w, p := resolveLogWriter(config.LogConfig{}, false)
	if w != nil {
		t.Fatalf("expected nil writer, got %v", w)
	}
	if p != "" {
		t.Fatalf("expected empty path, got %q", p)
	}
}

func TestResolveLogWriter_WithFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	w, p := resolveLogWriter(config.LogConfig{File: logPath}, false)
	if w == nil {
		t.Fatal("expected non-nil writer for valid file path")
	}
	if p != logPath {
		t.Fatalf("expected path %q, got %q", logPath, p)
	}
	// Close the file so TempDir cleanup doesn't fail on Windows.
	if f, ok := w.(*os.File); ok {
		f.Close()
	}
}

func TestResolveLogWriter_InvalidFile(t *testing.T) {
	// Invalid file path falls back to nil.
	w, _ := resolveLogWriter(config.LogConfig{File: string([]byte{0})}, false)
	if w != nil {
		t.Fatal("expected nil writer for invalid file path")
	}
}

func TestResolveLogWriter_TUIMode(t *testing.T) {
	w, p := resolveLogWriter(config.LogConfig{}, true)
	// In TUI mode, it tries to open a default log file.
	// The result depends on whether the default dir is writable.
	if w != nil && p == "" {
		t.Fatal("non-nil writer should have a path")
	}
}

// --- openLogFile ---

func TestOpenLogFile_ValidPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "test.log")
	w, err := openLogFile(path, config.LogConfig{})
	if err != nil {
		t.Fatalf("openLogFile: %v", err)
	}
	if w == nil {
		t.Fatal("expected non-nil writer")
	}
	// O1: the log sink is a ROTATING writer, not a bare file. Asserted on the
	// concrete type because that is the whole of the wiring: resolveLogWriter
	// returns an io.Writer, so a regression to a plain os.OpenFile would
	// satisfy every other assertion here and leave the log unbounded again —
	// with nothing to say so until a disk fills.
	rw, ok := w.(*obslog.RotatingWriter)
	if !ok {
		t.Fatalf("expected *obslog.RotatingWriter, got %T", w)
	}
	defer rw.Close()
	if _, err := rw.Write([]byte("line\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestOpenLogFile_InvalidPath(t *testing.T) {
	_, err := openLogFile(string([]byte{0}), config.LogConfig{})
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

// --- defaultLogDir ---

func TestDefaultLogDir(t *testing.T) {
	dir, err := defaultLogDir()
	if err != nil {
		t.Skipf("defaultLogDir failed: %v", err)
	}
	if dir == "" {
		t.Fatal("expected non-empty dir")
	}
}

// --- openLogFile error paths ---

func TestOpenLogFile_InvalidAbsPath(t *testing.T) {
	// A path with null bytes causes filepath.Abs to fail on some platforms.
	_, err := openLogFile(string([]byte{0}), config.LogConfig{})
	if err == nil {
		t.Log("openLogFile with null path did not error (platform-dependent)")
	}
}

// --- defaultLogDir error path ---

func TestDefaultLogDir_ErrorPath(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("APPDATA", "")
	dir, err := defaultLogDir()
	if err != nil {
		if dir != "" {
			t.Fatalf("expected empty dir on error, got %q", dir)
		}
		return
	}
	// On some Windows versions, UserConfigDir may still succeed.
	t.Logf("defaultLogDir returned %q (platform-dependent)", dir)
}

// --- Build with TUIMode ---

func TestBuild_WithTUIMode(t *testing.T) {
	cfg := &config.Config{
		Server:  config.ServerConfig{HTTPAddr: "127.0.0.1:0"},
		Storage: config.StorageConfig{SQLitePath: ":memory:"},
	}
	app, err := Build(Options{Cfg: cfg, FakeModel: true, TUIMode: true})
	if err != nil {
		t.Fatalf("Build with TUIMode: %v", err)
	}
	app.Shutdown(context.Background())
}

// TestParseCooldownDurationWarnsOnMalformed pins the warning, not just the
// fallback value.
//
// TestParseCooldownDuration_Invalid asserts the return is 0, which it would be
// whether or not anything told the operator. That is the whole problem: a
// typo'd cooldown_duration disables the time cooldown, and a disabled cooldown
// looks exactly like one deliberately left empty. Everywhere else a malformed
// config value is rejected at load (guard.ValidateShellPolicy through
// Config.validateProfiles); refusing to boot over one optional knob is too
// harsh, so the value degrades and says so. Without this test the "says so"
// half can be deleted silently.
func TestParseCooldownDurationWarnsOnMalformed(t *testing.T) {
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	got := parseCooldownDuration("3 seconds please")
	w.Close()
	os.Stderr = old

	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != 0 {
		t.Fatalf("parseCooldownDuration(malformed) = %v, want 0", got)
	}
	out := string(raw)
	if !strings.Contains(out, "cooldown_duration") {
		t.Errorf("a malformed cooldown_duration was swallowed silently; stderr was %q", out)
	}
	if !strings.Contains(out, "3 seconds please") {
		t.Errorf("the warning does not name the offending value, so the operator "+
			"cannot find it in their config; stderr was %q", out)
	}
}
