package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/cli"
)

func writeTempFile(t *testing.T, path, content string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

const opsExample = `schema_version: 1
server:
  http_addr: "127.0.0.1:8080"
storage:
  sqlite_path: "yanshi.db"
llm:
  providers:
    - name: "openai"
      kind: "openai"
      model: "gpt-4o"
      api_key: "${OPS_TEST_KEY}"
`

// TestRunInitWritesAndThenRefuses covers the whole `yanshi init` command
// surface: the first run writes, the second refuses with a USAGE exit code
// (the operator has to decide, so it is not a runtime failure), and -force
// overwrites while preserving the original.
func TestRunInitWritesAndThenRefuses(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	tmplPath := writeTempFile(t, filepath.Join(dir, "tmpl.yaml"), opsExample)

	var stdout, stderr bytes.Buffer
	code := runInit([]string{"-config", cfgPath, "-template", tmplPath}, &stdout, &stderr)
	require.Equal(t, exitOK, code, stderr.String())
	require.Contains(t, stdout.String(), "wrote "+cfgPath)
	require.Contains(t, stdout.String(), "OPS_TEST_KEY",
		"the summary must name the provider variable that is still unset")

	stdout.Reset()
	stderr.Reset()
	code = runInit([]string{"-config", cfgPath, "-template", tmplPath}, &stdout, &stderr)
	require.Equal(t, exitUsage, code,
		"refusing to clobber is the operator's decision to make, not a runtime error")
	require.Contains(t, stderr.String(), "-force")

	stdout.Reset()
	stderr.Reset()
	code = runInit([]string{"-config", cfgPath, "-template", tmplPath, "-force"}, &stdout, &stderr)
	require.Equal(t, exitOK, code, stderr.String())
	require.Contains(t, stdout.String(), "preserved at")
}

// TestRunInitRejectsBadFlagsAndMissingTemplate covers the two error exits.
func TestRunInitRejectsBadFlagsAndMissingTemplate(t *testing.T) {
	var stdout, stderr bytes.Buffer
	require.Equal(t, exitUsage, runInit([]string{"-nope"}, &stdout, &stderr))

	dir := t.TempDir()
	stdout.Reset()
	stderr.Reset()
	code := runInit([]string{
		"-config", filepath.Join(dir, "config.yaml"),
		"-template", filepath.Join(dir, "absent.yaml"),
	}, &stdout, &stderr)
	require.Equal(t, exitErr, code, "a missing template is a runtime failure, not a usage error")
}

// TestRunDaemonUsageAndUnknownSubcommand covers the routing errors.
func TestRunDaemonUsageAndUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	require.Equal(t, exitUsage, runDaemon(nil, &stdout, &stderr))
	require.Contains(t, stderr.String(), "status|stop|reload")

	stderr.Reset()
	require.Equal(t, exitUsage, runDaemon([]string{"restart"}, &stdout, &stderr))
	require.Contains(t, stderr.String(), "unknown daemon subcommand")

	stderr.Reset()
	require.Equal(t, exitUsage, runDaemon([]string{"status", "-nope"}, &stdout, &stderr))
}

// TestDaemonStatusExitCodeIsScriptable pins the machine-readable half: a
// monitoring check that has to grep the text for "ready" breaks the first time
// the wording improves, so readiness lives in the exit code.
func TestDaemonStatusExitCodeIsScriptable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := filepath.Join(t.TempDir(), "no-daemon")
	code := runDaemon([]string{"status", "-root", root}, &stdout, &stderr)
	require.Equal(t, exitErr, code, "no daemon is not a ready daemon")
	require.Contains(t, stdout.String(), "no daemon lockfile")

	stdout.Reset()
	code = runDaemon([]string{"status", "-root", root, "-json"}, &stdout, &stderr)
	require.Equal(t, exitErr, code)
	var status cli.DaemonStatus
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &status))
	require.False(t, status.Found)
}

// TestRunScheduleUsageAndUnknownOp covers the two client-side rejections. A
// typo must fail with a usage message rather than a dial error, which means it
// has to be caught before the network call.
func TestRunScheduleUsageAndUnknownOp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	require.Equal(t, exitUsage, runSchedule(nil, &stdout, &stderr))
	require.Contains(t, stderr.String(), "list|show|pause|resume|run-now|delete")

	stderr.Reset()
	code := runSchedule([]string{"paws", "auto-1", "-root", t.TempDir()}, &stdout, &stderr)
	require.Equal(t, exitUsage, code)
	require.Contains(t, stderr.String(), "unknown schedule operation")

	stderr.Reset()
	code = runSchedule([]string{"list", "-root", filepath.Join(t.TempDir(), "none")}, &stdout, &stderr)
	require.Equal(t, exitErr, code, "no running daemon is a runtime failure")
}

// TestSchedulePositionalIDIsParsed proves `yanshi schedule pause auto-123`
// reads the way an operator types it, with flags allowed after the id.
func TestSchedulePositionalIDIsParsed(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// A nonexistent root makes the call fail at discovery, which is fine: the
	// point is that the flag parser accepted the positional id rather than
	// rejecting it as an unknown flag.
	code := runSchedule([]string{"pause", "auto-123", "-root",
		filepath.Join(t.TempDir(), "none")}, &stdout, &stderr)
	require.Equal(t, exitErr, code)
	require.NotContains(t, stderr.String(), "flag provided but not defined")
}

// TestOpsEndpointsAreLoopbackOnly pins the deliberately stricter policy. The
// server's own middleware admits a non-loopback client bearing the token;
// "stop this process" and "delete this automation" are a different class of
// operation, and a leaked token must not be enough to terminate a backend.
func TestOpsEndpointsAreLoopbackOnly(t *testing.T) {
	var reachedApp bool
	app := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedApp = true
		w.WriteHeader(http.StatusOK)
	})
	var stopped bool
	h := withOpsEndpoints(app, opsConfig{
		ConfigPath: "config.yaml",
		Stop:       func() { stopped = true },
	})

	for _, path := range []string{cli.ControlPath, cli.SchedulePath} {
		t.Run("remote is refused: "+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"op":"stop"}`))
			req.RemoteAddr = "203.0.113.7:44444"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusForbidden, rec.Code)
			require.False(t, stopped, "a remote caller must not reach the stop hook")
			require.False(t, reachedApp, "a refused ops call must not fall through to the app")
		})
	}

	t.Run("unrelated paths still reach the app", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/chat", nil)
		req.RemoteAddr = "203.0.113.7:44444"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.True(t, reachedApp, "wrapping must not shadow any existing route")
		require.Equal(t, http.StatusOK, rec.Code)
	})
}

// TestOpsEndpointsServeLoopbackCallers is the positive half: a local caller
// reaches the handlers.
func TestOpsEndpointsServeLoopbackCallers(t *testing.T) {
	h := withOpsEndpoints(http.NotFoundHandler(), opsConfig{
		ConfigPath: "config.yaml",
		Stop:       func() {},
	})

	req := httptest.NewRequest(http.MethodPost, cli.ControlPath, strings.NewReader(`{"op":"stop"}`))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code)

	// The schedule endpoint reports the missing scheduler honestly rather than
	// showing an empty list, which would read as "your automations are gone".
	req = httptest.NewRequest(http.MethodPost, cli.SchedulePath, strings.NewReader(`{"op":"list"}`))
	req.RemoteAddr = "[::1]:5555"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotImplemented, rec.Code)
	require.Contains(t, rec.Body.String(), "without the automation scheduler")
}

// TestRequestIsLoopback is the table for the address check, including the
// fail-closed direction: an address shape this code cannot parse is one it
// cannot vouch for.
func TestRequestIsLoopback(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want bool
	}{
		{name: "ipv4 loopback with port", addr: "127.0.0.1:8080", want: true},
		{name: "ipv4 loopback alternate", addr: "127.0.0.53:8080", want: true},
		{name: "ipv6 loopback with port", addr: "[::1]:8080", want: true},
		{name: "localhost name", addr: "localhost:8080", want: true},
		{name: "bare ipv4 loopback", addr: "127.0.0.1", want: true},
		{name: "public ipv4", addr: "203.0.113.7:44444", want: false},
		{name: "private lan", addr: "192.168.1.10:8080", want: false},
		{name: "public ipv6", addr: "[2001:db8::1]:8080", want: false},
		{name: "empty is refused", addr: "", want: false},
		{name: "garbage is refused", addr: "not-an-address", want: false},
		{name: "hostname is refused", addr: "attacker.example.com:80", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, requestIsLoopback(tc.addr))
		})
	}
}

// TestApplyReloadToNilAppHasNoApplyPath pins the honest-reporting contract at
// the composition-root end.
//
// It asserts the nil case directly because the interesting positive case
// requires an assembled *bootstrap.App (a SQLite store and a bound listener),
// which belongs in the bootstrap wiring tests rather than here. What this file
// can and must pin is the DIRECTION: a nil apply path yields nil, which the
// control handler turns into "every classified-safe section is demoted to a
// refusal" rather than into a false claim that something was reloaded.
func TestApplyReloadToNilAppHasNoApplyPath(t *testing.T) {
	require.Nil(t, applyReloadTo(nil), "no app means no apply path at all")
}

// TestClassifiedReloadableSectionsWithoutAnApplyPathAreRefused is the paired
// assertion: `observability.log` is classified reloadable but this daemon has
// no apply path for it (re-installing the logger needs the file writer
// bootstrap resolved at boot, which App does not expose). The operator must be
// told to restart rather than be told the log level changed when it did not.
func TestClassifiedReloadableSectionsWithoutAnApplyPathAreRefused(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTempFile(t, filepath.Join(dir, "config.yaml"),
		"schema_version: 1\nserver:\n  http_addr: \"127.0.0.1:8080\"\n"+
			"storage:\n  sqlite_path: \"yanshi.db\"\n")

	// A nil App yields a nil ApplyReload, which is exactly the shape the
	// handler must not treat as "applied everything".
	h := withOpsEndpoints(http.NotFoundHandler(), opsConfig{ConfigPath: cfgPath})
	req := httptest.NewRequest(http.MethodPost, cli.ControlPath, strings.NewReader(`{"op":"reload"}`))
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp cli.ControlResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Empty(t, resp.Applied,
		"a daemon with no apply path must not claim it reloaded anything")

	var rejected []string
	for _, r := range resp.Rejected {
		rejected = append(rejected, r.Section)
	}
	require.Contains(t, rejected, "observability.log")
	require.Contains(t, rejected, "server.http_addr")
}

// TestRunDoctorFixFlagsAreAccepted covers the -fix flag surface reaching the
// repair path, and its non-interactive refusal (tests never run on a TTY).
func TestRunDoctorFixFlagsAreAccepted(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTempFile(t, filepath.Join(dir, "config.yaml"),
		"schema_version: 1\nserver:\n  http_addr: \"127.0.0.1:0\"\n"+
			"storage:\n  sqlite_path: \""+filepath.ToSlash(filepath.Join(dir, "t.db"))+"\"\n")

	var stdout, stderr bytes.Buffer
	code := runDoctor(t.Context(), []string{
		"-config", cfgPath, "-fix", "-fix-dry-run",
	}, &stdout, &stderr)
	require.NotEqual(t, 2, code, stderr.String())
	require.Contains(t, stdout.String(), "dry run")
	require.Contains(t, stdout.String(), "REFUSED",
		"the test process has no terminal, so file-editing repairs must be refused")

	// An unknown repair name is a hard error, not a silent no-op.
	stdout.Reset()
	stderr.Reset()
	code = runDoctor(t.Context(), []string{
		"-config", cfgPath, "-fix", "-fix-only", "rewrite-api-key",
	}, &stdout, &stderr)
	require.Equal(t, 2, code)
	require.Contains(t, stderr.String(), "unknown doctor fix action")
}

// TestRunDoctorWithoutFixIsUnchanged guards the default path: without -fix,
// doctor must behave exactly as before and never touch anything.
func TestRunDoctorWithoutFixIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTempFile(t, filepath.Join(dir, "config.yaml"),
		"schema_version: 1\nserver:\n  http_addr: \"127.0.0.1:0\"\n"+
			"storage:\n  sqlite_path: \""+filepath.ToSlash(filepath.Join(dir, "t.db"))+"\"\n")

	before, err := os.ReadFile(cfgPath)
	require.NoError(t, err)

	var stdout, stderr bytes.Buffer
	runDoctor(t.Context(), []string{"-config", cfgPath}, &stdout, &stderr)
	require.NotContains(t, stdout.String(), "REFUSED")
	require.NotContains(t, stdout.String(), "dry run")

	after, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

// TestDispatchRoutesTheNewSubcommands proves the three commands are reachable
// from argv. A subcommand implemented and not routed is the "zero parts, no
// assembly line" failure the governance tests exist to catch, and dispatch is
// where it would happen.
func TestDispatchRoutesTheNewSubcommands(t *testing.T) {
	for _, sub := range []string{"init", "daemon", "schedule"} {
		t.Run(sub, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := dispatch([]string{"yanshi", sub, "--definitely-not-a-flag"},
				strings.NewReader(""), &stdout, &stderr)
			require.NotContains(t, stderr.String(), "unknown subcommand",
				"%q must be routed by dispatch, not fall through", sub)
			require.NotEqual(t, exitOK, code,
				"a bogus flag must not succeed")
		})
	}
}

// TestUsageDocumentsTheNewSubcommands guards the help text against drifting
// away from what dispatch actually routes.
func TestUsageDocumentsTheNewSubcommands(t *testing.T) {
	for _, sub := range []string{"yanshi init", "yanshi daemon", "yanshi schedule"} {
		require.Contains(t, usage, sub)
	}
	require.Contains(t, usage, "-fix", "doctor's repair flag must be documented")
}
