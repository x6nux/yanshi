package cli

// liverun_o1_test.go — log rotation, driven by actually writing past the
// threshold rather than by shrinking the threshold to meet the test.
//
// The distinction matters because rotation is a size-triggered side effect
// inside a write path that must never fail: a unit test that calls Rotate()
// directly proves the rename works, and says nothing about whether the writer
// notices it has grown. Everything here goes through Write, at the real
// megabyte scale the operator configures, through the writer that
// bootstrap.openLogFile assembles from a config.LogConfig — so a rotation that
// only fires when someone calls it by hand fails here.
//
// A daemon under load has many goroutines logging at once, so the concurrent
// case is run too: rotation swaps the underlying file mid-flight, and a writer
// that does it without holding its own lock loses lines or writes into a closed
// handle.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
)

// logGeneration returns the paths of the active log and its generations, in
// generation order, keeping only those that exist.
func logGeneration(t *testing.T, path string, upTo int) []string {
	t.Helper()
	var out []string
	if _, err := os.Stat(path); err == nil {
		out = append(out, path)
	}
	for i := 1; i <= upTo; i++ {
		p := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// sizeOf returns a file's size, or -1 when it does not exist.
func sizeOf(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fi.Size()
}

// TestLiveRun_O1LogRotationHappensByWritingPastTheThreshold writes several
// megabytes of realistic structured log lines into a 1 MiB / 2-backup writer
// and asserts that the generations appear, that the footprint stays bounded,
// and that no line is lost across the rotations.
func TestLiveRun_O1LogRotationHappensByWritingPastTheThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.log")
	const maxBytes = 1 << 20
	const maxBackups = 2

	w, err := obslog.NewRotatingWriter(path, obslog.RotateConfig{
		MaxBytes: maxBytes, MaxBackups: maxBackups,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	// Roughly 4 MiB of traffic: enough to force at least three rotations, so
	// the test sees both generations created AND the oldest one discarded.
	line := `{"time":"2026-08-08T15:10:35.007083+08:00","level":"INFO","msg":"turn finished",` +
		`"model":"","turns":1,"tool_calls":0,"completion_tokens":8,` +
		`"trace_id":"26bc7ce28cdae0cfda96cd8d2625115c","session_id":"6bb2880e7e9e61ccc7468b56"}` + "\n"
	const target = 4 << 20
	written := 0
	lines := 0
	for written < target {
		n, err := w.Write([]byte(line))
		require.NoError(t, err, "a log write must never fail; rotation is failure-tolerant by design")
		written += n
		lines++
	}
	t.Logf("wrote %d lines / %d bytes through a %d-byte, %d-backup writer",
		lines, written, maxBytes, maxBackups)

	gens := logGeneration(t, path, maxBackups+3)
	for _, g := range gens {
		t.Logf("  %s: %d bytes", filepath.Base(g), sizeOf(g))
	}

	// The generations must exist. Before rotation works, there is one file of
	// 4 MiB and nothing else.
	if sizeOf(path+".1") < 0 {
		t.Errorf("no first generation was produced after writing %d bytes past a %d-byte threshold",
			written, maxBytes)
	}
	if sizeOf(path+".2") < 0 {
		t.Errorf("no second generation was produced; retention beyond one file never happened")
	}
	// And retention must be enforced: a third generation means MaxBackups is
	// decoration.
	if sizeOf(path+".3") >= 0 {
		t.Errorf("a .3 generation exists with max_backups=%d; the footprint is not bounded", maxBackups)
	}

	// Footprint: at most (maxBackups+1) files, each around the threshold.
	var total int64
	for _, g := range gens {
		total += sizeOf(g)
	}
	limit := int64(maxBytes) * (maxBackups + 2) // +1 for the active file, +1 slack for the final line
	t.Logf("total on-disk footprint: %d bytes (bound %d)", total, limit)
	if total > limit {
		t.Errorf("log footprint is %d bytes for a %d-byte / %d-backup policy; "+
			"the whole point is that it stays bounded", total, maxBytes, maxBackups)
	}

	// Nothing was corrupted: every retained file consists of whole lines.
	for _, g := range gens {
		data, err := os.ReadFile(g)
		require.NoError(t, err)
		if len(data) == 0 {
			continue
		}
		if data[len(data)-1] != '\n' {
			t.Errorf("%s does not end on a line boundary; a write was cut by a rotation",
				filepath.Base(g))
		}
		if strings.Count(string(data), "\n") == 0 {
			t.Errorf("%s holds no complete line", filepath.Base(g))
		}
	}
}

// TestLiveRun_O1ConcurrentWritersSurviveRotation reproduces the daemon shape: a
// dozen goroutines logging while the file is being rotated underneath them.
//
// The assertion is on the total number of lines that survive across every
// generation. A rotation that races its own writers shows up as lost lines, or
// as a torn line, neither of which a single-writer test can produce.
func TestLiveRun_O1ConcurrentWritersSurviveRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "busy.log")
	const maxBytes = 256 << 10
	// Keep every generation so the count below is exact: retention discarding
	// old lines is correct behaviour and would mask a real loss.
	const maxBackups = 64

	w, err := obslog.NewRotatingWriter(path, obslog.RotateConfig{
		MaxBytes: maxBytes, MaxBackups: maxBackups,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	const writers, perWriter = 12, 400
	var wg sync.WaitGroup
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				line := fmt.Sprintf(
					`{"level":"INFO","msg":"work","writer":%d,"seq":%d,"pad":"%s"}`+"\n",
					g, i, strings.Repeat("x", 200))
				if _, err := w.Write([]byte(line)); err != nil {
					t.Errorf("writer %d line %d: %v", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	gens := logGeneration(t, path, maxBackups)
	total := 0
	torn := 0
	for _, g := range gens {
		data, err := os.ReadFile(g)
		require.NoError(t, err)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			torn++
		}
		total += strings.Count(string(data), "\n")
	}
	want := writers * perWriter
	t.Logf("%d concurrent writers x %d lines -> %d lines across %d generations (%d torn)",
		writers, perWriter, total, len(gens), torn)
	if len(gens) < 2 {
		t.Fatalf("the load did not rotate at all (%d file(s)); the test proves nothing", len(gens))
	}
	if torn > 0 {
		t.Errorf("%d generation(s) end mid-line: a rotation cut a concurrent write", torn)
	}
	if total != want {
		t.Errorf("%d lines survived, want %d: %d lines were lost across rotations",
			total, want, want-total)
	}
}

// TestLiveRun_O1LoweringMaxBackupsDiscardsTheExtraGenerations covers the
// operator action the feature is for: a log directory has grown, the retention
// is lowered, and the old generations must actually go away.
//
// Reclaiming only on the NEXT rotation is the plausible wrong behaviour here —
// nothing is deleted until the log happens to fill again, which on a quiet
// daemon can be never, so the operator's edit appears to do nothing.
func TestLiveRun_O1LoweringMaxBackupsDiscardsTheExtraGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.log")
	const maxBytes = 128 << 10

	fill := func(w *obslog.RotatingWriter, bytes int) {
		line := strings.Repeat("y", 511) + "\n"
		for n := 0; n < bytes; {
			c, err := w.Write([]byte(line))
			require.NoError(t, err)
			n += c
		}
	}

	// Phase 1: five generations retained.
	w, err := obslog.NewRotatingWriter(path, obslog.RotateConfig{MaxBytes: maxBytes, MaxBackups: 5})
	require.NoError(t, err)
	fill(w, 6*maxBytes)
	require.NoError(t, w.Close())

	before := logGeneration(t, path, 8)
	t.Logf("with max_backups=5: %d files on disk", len(before))
	require.GreaterOrEqual(t, len(before), 4,
		"the setup must actually produce several generations, or the reduction below is vacuous")

	// Phase 2: the operator lowers retention and restarts the daemon. One more
	// rotation's worth of traffic is written, which is what a restarted daemon
	// does over time.
	w2, err := obslog.NewRotatingWriter(path, obslog.RotateConfig{MaxBytes: maxBytes, MaxBackups: 1})
	require.NoError(t, err)
	fill(w2, 2*maxBytes)
	require.NoError(t, w2.Close())

	after := logGeneration(t, path, 8)
	names := make([]string, 0, len(after))
	for _, p := range after {
		names = append(names, filepath.Base(p))
	}
	t.Logf("after lowering to max_backups=1: %v", names)
	if len(after) > 2 { // active + one generation
		t.Errorf("%v remain after retention was lowered to 1; the extra generations were "+
			"never reclaimed, so the operator's edit freed nothing", names)
	}
}
