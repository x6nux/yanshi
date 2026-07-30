package tui

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cmdLogsResult runs /logs against m and returns the rendered text of the
// entry it appended (the last entry).
func cmdLogsResult(t *testing.T, m model) string {
	t.Helper()
	out, cmd := cmdLogs(m, nil)
	assert.Nil(t, cmd, "/logs never schedules a cmd")
	mm, ok := out.(model)
	require.True(t, ok, "cmdLogs returns the model")
	require.NotEmpty(t, mm.entries, "cmdLogs always appends an entry")
	last := mm.entries[len(mm.entries)-1]
	return last.render(200, newSpinner())
}

// TestCov_CmdLogs_StderrWhenNoPath covers the empty-logPath branch: the
// command tells the user logs are on stderr rather than guessing a path.
func TestCov_CmdLogs_StderrWhenNoPath(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.logPath = ""
	out := cmdLogsResult(t, m)
	assert.Contains(t, out, "stderr")
}

// TestCov_CmdLogs_ReadError covers the unreadable-path branch.
func TestCov_CmdLogs_ReadError(t *testing.T) {
	m := newModel(&fakeSession{}, "/proj")
	m.logPath = filepath.Join(t.TempDir(), "does-not-exist.log")
	out := cmdLogsResult(t, m)
	assert.Contains(t, out, "cannot read")
}

// TestCov_CmdLogs_EmptyFile covers the empty-file branch (file exists, no lines).
func TestCov_CmdLogs_EmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty.log")
	require.NoError(t, os.WriteFile(p, nil, 0o644))
	m := newModel(&fakeSession{}, "/proj")
	m.logPath = p
	out := cmdLogsResult(t, m)
	assert.Contains(t, out, "empty")
}

// TestCov_CmdLogs_Tail covers the happy path: a file with content renders a
// header plus the tailed lines.
func TestCov_CmdLogs_Tail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.log")
	require.NoError(t, os.WriteFile(p, []byte("boot\ncore ready\nserving\n"), 0o644))
	m := newModel(&fakeSession{}, "/proj")
	m.logPath = p
	out := cmdLogsResult(t, m)
	assert.Contains(t, out, "── logs:")
	assert.Contains(t, out, "serving")
	assert.Contains(t, out, "last 3 lines")
}

// TestCov_TailFile_RingTruncation proves tailFile keeps only the last n lines.
func TestCov_TailFile_RingTruncation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ring.log")
	var body strings.Builder
	for i := 0; i < 10; i++ {
		body.WriteString("line-")
		body.WriteByte(byte('a' + i))
		body.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(p, []byte(body.String()), 0o644))
	got, err := tailFile(p, 3)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "line-h", got[0], "ring drops everything older than the last n")
	assert.Equal(t, "line-j", got[2])
}

// TestCov_TailFile_InvalidUTF8 covers the ToValidUTF8 sanitization branch — a
// binary/corrupt log line must not break rendering.
func TestCov_TailFile_InvalidUTF8(t *testing.T) {
	p := filepath.Join(t.TempDir(), "utf8.log")
	// \xff/\xfe are invalid UTF-8 leading bytes; "ok" is a valid trailing line.
	require.NoError(t, os.WriteFile(p, []byte("\xff\xfe garbage\nok\n"), 0o644))
	got, err := tailFile(p, 80)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.NotContains(t, got[0], "\xff", "invalid bytes are replaced")
	assert.Equal(t, "ok", got[1])
}

// TestCov_TailFile_OpenError covers the os.Open failure branch (path missing).
func TestCov_TailFile_OpenError(t *testing.T) {
	_, err := tailFile(filepath.Join(t.TempDir(), "nope"), 10)
	assert.Error(t, err)
}

// TestCov_TailFile_ScanError covers the scanner.Err() branch. Reading a
// directory yields a scan error on Unix; on Windows os.Open of a directory may
// itself fail (covered by the open-error path instead), so the assertion is
// "some error" either way.
func TestCov_TailFile_ScanError(t *testing.T) {
	dir := t.TempDir()
	_, err := tailFile(dir, 10)
	assert.Error(t, err, "reading a directory must error (open or scan path)")
	_ = runtime.GOOS // scan-vs-open branch is OS-dependent; either is acceptable
}
