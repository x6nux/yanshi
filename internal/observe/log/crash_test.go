package log

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// fixedRedactor stands in for *secrets.Redactor: it replaces each registered
// substring with the same sentinel the real one writes, so a test can prove
// the dump goes THROUGH a redactor without importing the credential store.
type fixedRedactor struct{ secrets []string }

func (r fixedRedactor) Redact(s string) string {
	out := s
	for _, sec := range r.secrets {
		out = strings.ReplaceAll(out, sec, "[REDACTED]")
	}
	return out
}

func readReport(t *testing.T, path string) CrashReport {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var report CrashReport
	require.NoError(t, json.Unmarshal(data, &report))
	return report
}

// TestCrashDumpRedactsEveryStringField is the hard requirement: a report must
// pass every string it writes through the Redactor. The secret is planted in
// each field independently so a field added later without redaction is caught
// by its own row rather than by luck.
func TestCrashDumpRedactsEveryStringField(t *testing.T) {
	const secret = "sk-live-DEADBEEF"
	d, err := NewCrashDumper(t.TempDir(), fixedRedactor{secrets: []string{secret}})
	require.NoError(t, err)
	d.IncludeBodies = true

	cases := []struct {
		name     string
		messages []MessageMeta
		err      error
	}{
		{
			name: "secret in the error chain",
			err:  fmt.Errorf("call provider: %w", errors.New("auth failed for "+secret)),
		},
		{
			name:     "secret in a tool name",
			err:      errors.New("boom"),
			messages: []MessageMeta{{Role: "tool", ToolName: "fetch_" + secret}},
		},
		{
			name:     "secret in a message body",
			err:      errors.New("boom"),
			messages: []MessageMeta{{Role: "user", Body: "my key is " + secret}},
		},
		{
			name:     "secret in a role",
			err:      errors.New("boom"),
			messages: []MessageMeta{{Role: "user-" + secret}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, derr := d.Dump(context.Background(), "error", tc.err, tc.messages)
			require.NoError(t, derr)
			raw, rerr := os.ReadFile(path)
			require.NoError(t, rerr)
			require.NotContains(t, string(raw), secret,
				"a crash report must never carry a registered secret verbatim")
			require.Contains(t, string(raw), "[REDACTED]",
				"the secret must have been replaced, not merely absent")
		})
	}
}

// TestCrashDumpOmitsBodiesByDefault pins the disclosure posture O10 must NOT
// overturn: the reason the plain log path prints no error body is that bodies
// carry prompts and credentials, so bodies are off unless explicitly enabled.
func TestCrashDumpOmitsBodiesByDefault(t *testing.T) {
	d, err := NewCrashDumper(t.TempDir(), nil)
	require.NoError(t, err)

	messages := []MessageMeta{
		{Role: "user", Bytes: 42, Body: "the user's private prompt"},
		{Role: "tool", Bytes: 9, ToolName: "fs_read", Body: "file contents"},
	}
	path, err := d.Dump(context.Background(), "error", errors.New("x"), messages)
	require.NoError(t, err)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "the user's private prompt")
	require.NotContains(t, string(raw), "file contents")

	report := readReport(t, path)
	require.False(t, report.BodiesIncluded)
	require.Len(t, report.Messages, 2)
	// The metadata that makes the report useful must survive.
	require.Equal(t, "user", report.Messages[0].Role)
	require.Equal(t, 42, report.Messages[0].Bytes)
	require.Equal(t, "fs_read", report.Messages[1].ToolName)
	require.Empty(t, report.Messages[0].Body)
}

// TestCrashDumpIncludesBodiesWhenEnabled proves the debug switch is a real
// switch and not decoration -- and that the report says so, so a reader knows
// whether an empty Body means "not captured" or "empty message".
func TestCrashDumpIncludesBodiesWhenEnabled(t *testing.T) {
	d, err := NewCrashDumper(t.TempDir(), nil)
	require.NoError(t, err)
	d.IncludeBodies = true

	path, err := d.Dump(context.Background(), "error", errors.New("x"),
		[]MessageMeta{{Role: "user", Body: "explicit debug capture"}})
	require.NoError(t, err)

	report := readReport(t, path)
	require.True(t, report.BodiesIncluded)
	require.Equal(t, "explicit debug capture", report.Messages[0].Body)
}

// TestCrashDumpCapturesErrorChain asserts the report carries the whole unwrap
// chain, not just the outermost wrap. The top level usually says "turn failed"
// and the cause three levels down names the actual fault.
func TestCrashDumpCapturesErrorChain(t *testing.T) {
	d, err := NewCrashDumper(t.TempDir(), nil)
	require.NoError(t, err)

	root := errors.New("connection refused")
	mid := fmt.Errorf("dial provider: %w", root)
	top := fmt.Errorf("turn failed: %w", mid)

	path, err := d.Dump(context.Background(), "error", top, nil)
	require.NoError(t, err)

	report := readReport(t, path)
	require.Len(t, report.ErrorChain, 3)
	require.Equal(t, "turn failed: dial provider: connection refused", report.ErrorChain[0])
	require.Equal(t, "connection refused", report.ErrorChain[2])
	require.Equal(t, "error", report.Kind)
	require.NotEmpty(t, report.ErrorType)
}

// TestCrashDumpBoundsErrorChain proves a self-referential or pathologically
// deep chain cannot make the dumper spin: the walk is capped.
func TestCrashDumpBoundsErrorChain(t *testing.T) {
	d, err := NewCrashDumper(t.TempDir(), nil)
	require.NoError(t, err)

	deep := errors.New("root")
	for i := 0; i < 100; i++ {
		deep = fmt.Errorf("layer %d: %w", i, deep)
	}
	path, err := d.Dump(context.Background(), "error", deep, nil)
	require.NoError(t, err)

	report := readReport(t, path)
	require.LessOrEqual(t, len(report.ErrorChain), 16)
	require.NotEmpty(t, report.ErrorChain)
}

// TestCrashDumpCarriesCorrelationIDs proves a report can be joined back to the
// log lines of the turn that produced it. A report with no ids is a file
// nobody can place in a timeline.
func TestCrashDumpCarriesCorrelationIDs(t *testing.T) {
	d, err := NewCrashDumper(t.TempDir(), nil)
	require.NoError(t, err)

	ctx := WithIDs(context.Background(), IDs{
		TraceID: "trace-1", SessionID: "sess-2", TurnID: "turn-3", Tool: "shell_run",
	})
	path, err := d.Dump(ctx, "error", errors.New("x"), nil)
	require.NoError(t, err)

	report := readReport(t, path)
	require.Equal(t, "trace-1", report.TraceID)
	require.Equal(t, "sess-2", report.SessionID)
	require.Equal(t, "turn-3", report.TurnID)
	require.Equal(t, "shell_run", report.Tool)
}

// TestCrashDumpKeepsOnlyTheTrailingWindow asserts the message cap keeps the
// frames NEAREST the crash: those are the ones that explain it.
func TestCrashDumpKeepsOnlyTheTrailingWindow(t *testing.T) {
	d, err := NewCrashDumper(t.TempDir(), nil)
	require.NoError(t, err)
	d.MaxMessages = 3

	var messages []MessageMeta
	for i := 0; i < 10; i++ {
		messages = append(messages, MessageMeta{Role: fmt.Sprintf("r%d", i), Bytes: i})
	}
	path, err := d.Dump(context.Background(), "error", errors.New("x"), messages)
	require.NoError(t, err)

	report := readReport(t, path)
	require.Len(t, report.Messages, 3)
	require.Equal(t, "r7", report.Messages[0].Role)
	require.Equal(t, "r9", report.Messages[2].Role)
}

// TestDumpPanicCapturesStack asserts the panic entry point records goroutine
// frames, and that the panic value lands in the same shape as an error crash.
func TestDumpPanicCapturesStack(t *testing.T) {
	d, err := NewCrashDumper(t.TempDir(), nil)
	require.NoError(t, err)

	var path string
	func() {
		defer func() {
			r := recover()
			require.NotNil(t, r)
			var derr error
			path, derr = d.DumpPanic(context.Background(), r, nil)
			require.NoError(t, derr)
		}()
		panic("deliberate test panic")
	}()

	report := readReport(t, path)
	require.Equal(t, "panic", report.Kind)
	require.Contains(t, report.ErrorChain[0], "deliberate test panic")
	require.NotEmpty(t, report.Stack)
	require.Contains(t, report.Stack, "goroutine ")
	require.False(t, report.StackTruncated)
}

// TestConfigFingerprintNamesKeysNeverValues is the config half of the
// disclosure rule: a fingerprint must identify a configuration without
// reproducing it, because a config is where api keys live.
func TestConfigFingerprintNamesKeysNeverValues(t *testing.T) {
	fp := FingerprintConfig(map[string]string{
		"llm.providers.0.api_key": "sk-live-SECRET",
		"server.http_addr":        "127.0.0.1:8080",
	})
	require.Equal(t, []string{"llm.providers.0.api_key", "server.http_addr"}, fp.Keys)
	require.NotContains(t, strings.Join(fp.Keys, "|"), "sk-live-SECRET")
	require.Len(t, fp.SHA256, 64)

	// The digest must cover the VALUES: two configs with the same keys and
	// different values must be distinguishable, or the fingerprint proves
	// nothing.
	other := FingerprintConfig(map[string]string{
		"llm.providers.0.api_key": "sk-live-DIFFERENT",
		"server.http_addr":        "127.0.0.1:8080",
	})
	require.Equal(t, fp.Keys, other.Keys)
	require.NotEqual(t, fp.SHA256, other.SHA256)

	// Same input, same digest -- otherwise reports cannot be compared.
	again := FingerprintConfig(map[string]string{
		"server.http_addr":        "127.0.0.1:8080",
		"llm.providers.0.api_key": "sk-live-SECRET",
	})
	require.Equal(t, fp.SHA256, again.SHA256, "map order must not change the digest")

	require.Equal(t, ConfigFingerprint{}, FingerprintConfig(nil))
}

// TestCrashDumpFingerprintNeverCarriesConfigValues proves the dumper wires the
// fingerprint in without letting the values reach the file.
func TestCrashDumpFingerprintNeverCarriesConfigValues(t *testing.T) {
	d, err := NewCrashDumper(t.TempDir(), nil)
	require.NoError(t, err)
	d.ConfigValues = map[string]string{"llm.providers.0.api_key": "sk-live-NEVER"}

	path, err := d.Dump(context.Background(), "error", errors.New("x"), nil)
	require.NoError(t, err)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "sk-live-NEVER")
	require.Contains(t, string(raw), "llm.providers.0.api_key")
}

// TestReportCrashAnnouncesPathNotError pins the stderr contract: the operator
// must learn WHERE the report is (a file nobody is told about is a file that
// gets deleted), while stderr keeps the same disclosure posture as the log --
// so the error text itself must not appear on it.
func TestReportCrashAnnouncesPathNotError(t *testing.T) {
	d, err := NewCrashDumper(t.TempDir(), nil)
	require.NoError(t, err)

	var stderr bytes.Buffer
	path := ReportCrash(context.Background(), d, "error",
		errors.New("provider said sk-live-OOPS"), nil, &stderr)

	require.NotEmpty(t, path)
	require.Contains(t, stderr.String(), path)
	require.NotContains(t, stderr.String(), "sk-live-OOPS",
		"stderr announces the path only; the body stays in the file")
	_, statErr := os.Stat(path)
	require.NoError(t, statErr)
}

// TestReportCrashToleratesNilDumper asserts the crash path cannot itself
// crash: a process with no dumper configured must get an empty path, not a nil
// dereference at the worst possible moment.
func TestReportCrashToleratesNilDumper(t *testing.T) {
	var stderr bytes.Buffer
	require.Empty(t, ReportCrash(context.Background(), nil, "error", errors.New("x"), nil, &stderr))
	require.Empty(t, stderr.String())
}

// TestCrashDumperConcurrentDumpsGetDistinctFiles is the concurrency contract:
// several goroutines dying at once must each get their own report rather than
// racing for one filename and losing all but the last.
func TestCrashDumperConcurrentDumpsGetDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	d, err := NewCrashDumper(dir, nil)
	require.NoError(t, err)

	const n = 24
	paths := make([]string, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			p, derr := d.Dump(context.Background(), "error", fmt.Errorf("crash %d", i), nil)
			if derr != nil {
				t.Errorf("dump %d: %v", i, derr)
				return
			}
			paths[i] = p
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, p := range paths {
		require.NotEmpty(t, p)
		require.False(t, seen[p], "duplicate crash report path %q", p)
		seen[p] = true
	}
	entries, err := CrashDirEntries(dir)
	require.NoError(t, err)
	require.Len(t, entries, n)
}

// TestCrashDirEntriesIsNewestFirst proves the listing order operators rely on,
// and that it ignores files that are not reports.
func TestCrashDirEntriesIsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"crash-20260101T000000.000000000Z.json",
		"crash-20260301T000000.000000000Z.json",
		"crash-20260201T000000.000000000Z.json",
		"notes.txt",
		"crash-broken.log",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600))
	}
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "crash-subdir.json"), 0o755))

	entries, err := CrashDirEntries(dir)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	require.Equal(t, "crash-20260301T000000.000000000Z.json", filepath.Base(entries[0]))
	require.Equal(t, "crash-20260101T000000.000000000Z.json", filepath.Base(entries[2]))

	_, err = CrashDirEntries(filepath.Join(dir, "does-not-exist"))
	require.Error(t, err)
}

// TestCrashReportFilesAreOwnerOnly asserts the file mode: a report is allowed
// to hold a redacted error chain precisely because it stays local, so it must
// not be world-readable on a shared machine.
func TestCrashReportFilesAreOwnerOnly(t *testing.T) {
	if os.Getenv("GOOS") == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}
	dir := t.TempDir()
	d, err := NewCrashDumper(dir, nil)
	require.NoError(t, err)
	path, err := d.Dump(context.Background(), "error", errors.New("x"), nil)
	require.NoError(t, err)

	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.Zero(t, fi.Mode().Perm()&0o077,
		"a crash report must not be readable by group or other, got %v", fi.Mode().Perm())
}

// TestTruncateForCrash is the table for the body-size clamp used by callers
// assembling bodies under the debug switch.
func TestTruncateForCrash(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{name: "under the limit is untouched", in: "hello", n: 10, want: "hello"},
		{name: "exactly the limit is untouched", in: "hello", n: 5, want: "hello"},
		{name: "over the limit is marked", in: "hello world", n: 5, want: "hello…[truncated]"},
		{name: "zero limit drops everything", in: "hello", n: 0, want: ""},
		{name: "negative limit drops everything", in: "hello", n: -1, want: ""},
		{name: "counts runes not bytes", in: "日本語テキスト", n: 3, want: "日本語…[truncated]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, TruncateForCrash(tc.in, tc.n))
		})
	}
}

// TestNewCrashDumperRejectsUnusableDir asserts construction fails loudly rather
// than handing back a dumper that discovers its own misconfiguration mid-crash.
func TestNewCrashDumperRejectsUnusableDir(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "afile")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	_, err := NewCrashDumper(filepath.Join(blocker, "under-a-file"), nil)
	require.Error(t, err)
}

// TestNewCrashDumperNilRedactorDoesNotPanic asserts the nil-redactor fallback:
// reporting a crash with no process redactor yet must produce a report, not a
// nil dereference. Losing redaction is bad; losing the only diagnosis of a
// crash that already happened is worse.
func TestNewCrashDumperNilRedactorDoesNotPanic(t *testing.T) {
	d, err := NewCrashDumper(t.TempDir(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, d.Dir())

	path, err := d.Dump(context.Background(), "error", errors.New("plain"), nil)
	require.NoError(t, err)
	report := readReport(t, path)
	require.Equal(t, []string{"plain"}, report.ErrorChain)
}

// TestCrashReportRecordsRuntime asserts the platform triple is present: it is
// the first thing asked about a crash that only reproduces on one machine.
func TestCrashReportRecordsRuntime(t *testing.T) {
	d, err := NewCrashDumper(t.TempDir(), nil)
	require.NoError(t, err)
	path, err := d.Dump(context.Background(), "error", errors.New("x"), nil)
	require.NoError(t, err)

	report := readReport(t, path)
	require.NotEmpty(t, report.Runtime.GoVersion)
	require.NotEmpty(t, report.Runtime.GOOS)
	require.NotEmpty(t, report.Runtime.GOARCH)
	require.False(t, report.Time.IsZero())
}

// TestDefaultCrashDirSitsBesideLogs asserts the crash directory is under the
// same yanshi config root as the log file, so an operator finds both together.
func TestDefaultCrashDirSitsBesideLogs(t *testing.T) {
	dir, err := DefaultCrashDir()
	if err != nil {
		t.Skipf("no user config dir on this machine: %v", err)
	}
	require.Equal(t, "crash", filepath.Base(dir))
	require.Equal(t, "yanshi", filepath.Base(filepath.Dir(dir)))
}
