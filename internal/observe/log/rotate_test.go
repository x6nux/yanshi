package log

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRotateConfigResolve is the table for the zero/negative normalisation:
// zero means "default", negative means "that half of the policy is off".
func TestRotateConfigResolve(t *testing.T) {
	cases := []struct {
		name         string
		cfg          RotateConfig
		wantBytes    int64
		wantBackups  int
		descriptions string
	}{
		{
			name:        "zero value takes both defaults",
			cfg:         RotateConfig{},
			wantBytes:   DefaultRotateMaxBytes,
			wantBackups: DefaultRotateMaxBackups,
		},
		{
			name:        "explicit values pass through",
			cfg:         RotateConfig{MaxBytes: 1024, MaxBackups: 2},
			wantBytes:   1024,
			wantBackups: 2,
		},
		{
			name:        "negative MaxBytes disables size rotation",
			cfg:         RotateConfig{MaxBytes: -1, MaxBackups: 3},
			wantBytes:   0,
			wantBackups: 3,
		},
		{
			name:        "negative MaxBackups keeps no generations",
			cfg:         RotateConfig{MaxBytes: 512, MaxBackups: -1},
			wantBytes:   512,
			wantBackups: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBytes, gotBackups := tc.cfg.resolve()
			require.Equal(t, tc.wantBytes, gotBytes)
			require.Equal(t, tc.wantBackups, gotBackups)
		})
	}
}

// TestRotatingWriterRotatesBySize drives the writer past its cap and asserts
// the generation chain: the active file holds only the newest record, .1 holds
// the previous generation, and no generation beyond MaxBackups survives.
func TestRotatingWriterRotatesBySize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yanshi.log")
	w, err := NewRotatingWriter(path, RotateConfig{MaxBytes: 20, MaxBackups: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	// Each record is 10 bytes, so every second record trips the 20-byte cap.
	for i := 0; i < 8; i++ {
		_, err := w.Write([]byte(fmt.Sprintf("rec-%05d", i)))
		require.NoError(t, err)
	}

	active, err := os.ReadFile(path)
	require.NoError(t, err)
	require.LessOrEqual(t, int64(len(active)), int64(20),
		"active file must never exceed the cap after a completed rotation")

	for _, n := range []int{1, 2} {
		_, statErr := os.Stat(fmt.Sprintf("%s.%d", path, n))
		require.NoError(t, statErr, "generation .%d must exist", n)
	}
	_, statErr := os.Stat(path + ".3")
	require.True(t, os.IsNotExist(statErr),
		"generation beyond MaxBackups must be dropped, got %v", statErr)
	require.NoError(t, w.RotateError())
}

// TestRotatingWriterKeepsEveryByte proves rotation moves records rather than
// dropping them: the concatenation of the surviving generations must contain
// every record written while the retention window still covers them.
func TestRotatingWriterKeepsEveryByte(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keep.log")
	w, err := NewRotatingWriter(path, RotateConfig{MaxBytes: 16, MaxBackups: 4})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	for i := 0; i < 4; i++ {
		_, werr := w.Write([]byte(fmt.Sprintf("line%03d\n", i)))
		require.NoError(t, werr)
	}

	var all strings.Builder
	for n := 4; n >= 1; n-- {
		if data, rerr := os.ReadFile(fmt.Sprintf("%s.%d", path, n)); rerr == nil {
			all.Write(data)
		}
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	all.Write(data)

	for i := 0; i < 4; i++ {
		require.Contains(t, all.String(), fmt.Sprintf("line%03d", i))
	}
}

// TestRotatingWriterNoRotationWhenDisabled asserts a negative MaxBytes turns
// the writer into a plain append-only file: no cap check, no generations.
func TestRotatingWriterNoRotationWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unbounded.log")
	w, err := NewRotatingWriter(path, RotateConfig{MaxBytes: -1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	for i := 0; i < 50; i++ {
		_, werr := w.Write([]byte("0123456789"))
		require.NoError(t, werr)
	}
	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(500), fi.Size())
	_, statErr := os.Stat(path + ".1")
	require.True(t, os.IsNotExist(statErr), "size rotation was disabled")
}

// TestRotatingWriterTruncatesWhenNoBackups covers the MaxBackups<0 branch: the
// cap is still enforced, but by truncation rather than by renaming.
func TestRotatingWriterTruncatesWhenNoBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trunc.log")
	w, err := NewRotatingWriter(path, RotateConfig{MaxBytes: 12, MaxBackups: -1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	for i := 0; i < 5; i++ {
		_, werr := w.Write([]byte("aaaaaaaaaa"))
		require.NoError(t, werr)
	}
	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.LessOrEqual(t, fi.Size(), int64(12))
	_, statErr := os.Stat(path + ".1")
	require.True(t, os.IsNotExist(statErr), "no generations are kept")
}

// TestRotatingWriterOversizedRecord asserts a single record larger than the
// whole cap is still written whole. Splitting it would emit two invalid JSON
// lines, which is worse than one oversized file.
func TestRotatingWriterOversizedRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")
	w, err := NewRotatingWriter(path, RotateConfig{MaxBytes: 8, MaxBackups: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	huge := strings.Repeat("x", 100)
	n, err := w.Write([]byte(huge))
	require.NoError(t, err)
	require.Equal(t, 100, n)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, huge, string(data), "an oversized record must not be split")
}

// TestRotatingWriterConcurrentWrites is the concurrency contract: many
// goroutines writing while rotation happens must never tear a record and never
// lose a write's error path. Run under -race to be meaningful.
func TestRotatingWriterConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "race.log")
	w, err := NewRotatingWriter(path, RotateConfig{MaxBytes: 64, MaxBackups: 3})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	const goroutines, perGoroutine = 16, 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if _, werr := w.Write([]byte(fmt.Sprintf("g%02d-i%03d\n", g, i))); werr != nil {
					t.Errorf("write: %v", werr)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	// Every line that survived must be intact: a torn write would produce a
	// line that does not match the fixed "gNN-iNNN" shape.
	for n := 3; n >= 0; n-- {
		p := path
		if n > 0 {
			p = fmt.Sprintf("%s.%d", path, n)
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
			if line == "" {
				continue
			}
			require.Len(t, line, 8, "torn record in %s: %q", p, line)
		}
	}
}

// TestRotatingWriterSurvivesRotationFailure is the fail-soft contract: when the
// rename chain cannot run, Write must keep succeeding against the file it
// already holds, and the failure must be visible through RotateError.
//
// The failure is induced by placing a NON-EMPTY DIRECTORY at the .1 generation
// path: os.Remove refuses to unlink it and os.Rename refuses to overwrite it,
// on every platform. (An empty directory is not enough -- os.Remove unlinks
// that happily, and the rotation then succeeds.)
func TestRotatingWriterSurvivesRotationFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blocked.log")
	require.NoError(t, os.MkdirAll(path+".1", 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(path+".1", "occupied"), []byte("x"), 0o644))

	w, err := NewRotatingWriter(path, RotateConfig{MaxBytes: 10, MaxBackups: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	for i := 0; i < 5; i++ {
		n, werr := w.Write([]byte("0123456789"))
		require.NoError(t, werr, "a failed rotation must not fail the write")
		require.Equal(t, 10, n)
	}
	require.Error(t, w.RotateError(),
		"a failed rotation must be observable, not silent")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), "0123456789",
		"writes must keep landing in the active file after a failed rotation")
}

// TestRotatingWriterPrunesGenerationsAboveALoweredCap covers the transition
// the steady-state shift cannot: when an installation LOWERS MaxBackups, the
// generations left over from the higher setting have nothing that will ever
// rename over them, so retention silently stops bounding the footprint.
//
// This test exists because a mutation probe deleted the prune step and every
// other rotation test stayed green -- os.Rename replaces its target, so in
// steady state the prune is unobservable. The transition is where it is the
// only thing keeping the config's number honest.
func TestRotatingWriterPrunesGenerationsAboveALoweredCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shrink.log")

	// Simulate a prior run at MaxBackups=5: generations .1 through .5 exist.
	for n := 1; n <= 5; n++ {
		require.NoError(t, os.WriteFile(fmt.Sprintf("%s.%d", path, n),
			[]byte(fmt.Sprintf("old generation %d\n", n)), 0o644))
	}

	// Restart with a lower cap and force one rotation.
	w, err := NewRotatingWriter(path, RotateConfig{MaxBytes: 8, MaxBackups: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	_, err = w.Write([]byte("0123456789"))
	require.NoError(t, err)
	_, err = w.Write([]byte("0123456789"))
	require.NoError(t, err)

	for _, n := range []int{3, 4, 5} {
		_, statErr := os.Stat(fmt.Sprintf("%s.%d", path, n))
		require.True(t, os.IsNotExist(statErr),
			"generation .%d is above the lowered cap and must be pruned", n)
	}
	for _, n := range []int{1, 2} {
		_, statErr := os.Stat(fmt.Sprintf("%s.%d", path, n))
		require.NoError(t, statErr, "generation .%d is within the cap", n)
	}
	require.NoError(t, w.RotateError())
}

// TestRotatingWriterRotateErrorClears asserts RotateError answers "did the LAST
// rotation work". A stale failure that outlived its rotation would make the
// signal useless: the operator would see a permanent red after one transient
// hiccup and stop looking.
func TestRotatingWriterRotateErrorClears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clears.log")
	blocker := path + ".1"
	require.NoError(t, os.MkdirAll(blocker, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(blocker, "occupied"), []byte("x"), 0o644))

	w, err := NewRotatingWriter(path, RotateConfig{MaxBytes: 10, MaxBackups: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	_, err = w.Write([]byte("0123456789"))
	require.NoError(t, err)
	_, err = w.Write([]byte("0123456789"))
	require.NoError(t, err)
	require.Error(t, w.RotateError())

	require.NoError(t, os.RemoveAll(blocker))
	_, err = w.Write([]byte("0123456789"))
	require.NoError(t, err)
	require.NoError(t, w.RotateError(), "a recovered rotation must clear the error")
}

// TestRotatingWriterReopenResumesSize asserts a writer that opens an existing
// file adopts its current size, so a restart does not reset the cap and let the
// file grow past MaxBytes.
func TestRotatingWriterReopenResumesSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resume.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("z", 30)), 0o644))

	w, err := NewRotatingWriter(path, RotateConfig{MaxBytes: 32, MaxBackups: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })

	_, err = w.Write([]byte("0123456789"))
	require.NoError(t, err)

	rotated, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("z", 30), string(rotated),
		"the pre-existing bytes must be rotated, not overwritten")
}

// TestRotatingWriterCloseIsIdempotentAndFailsWrites pins the post-Close
// contract: Close twice is fine, and a Write afterwards returns an error
// rather than panicking on a nil handle.
func TestRotatingWriterCloseIsIdempotentAndFailsWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "closed.log")
	w, err := NewRotatingWriter(path, RotateConfig{})
	require.NoError(t, err)

	require.NoError(t, w.Close())
	require.NoError(t, w.Close())

	_, err = w.Write([]byte("after close"))
	require.Error(t, err)
}

// TestRotatingWriterPathIsAbsolute asserts Path reports the resolved absolute
// path, which is what the TUI status frame surfaces for /logs.
func TestRotatingWriterPathIsAbsolute(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "abs.log")
	w, err := NewRotatingWriter(path, RotateConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = w.Close() })
	require.True(t, filepath.IsAbs(w.Path()))
	_, err = os.Stat(filepath.Dir(w.Path()))
	require.NoError(t, err, "the parent directory must be created")
}

// TestNewRotatingWriterRejectsUnusablePath asserts construction fails loudly
// when the path cannot be opened, rather than returning a writer that silently
// swallows every record.
func TestNewRotatingWriterRejectsUnusablePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "isdir.log")
	require.NoError(t, os.MkdirAll(path, 0o755))

	_, err := NewRotatingWriter(path, RotateConfig{})
	require.Error(t, err)
}
