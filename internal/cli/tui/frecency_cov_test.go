package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- LoadFrecency ----

// TestCov_LoadFrecency_ReadError covers the non-NotExist read-error path
// (reading a directory errors, and it is not IsNotExist).
func TestCov_LoadFrecency_ReadError(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadFrecency(dir) // dir, not file → read error
	assert.Error(t, err)
}

// TestCov_LoadFrecency_CorruptJSON covers the self-heal path: a corrupt JSON
// file degrades to an empty Frecency instead of failing.
func TestCov_LoadFrecency_CorruptJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "frec.json")
	require.NoError(t, os.WriteFile(p, []byte("not json {"), 0o644))
	f, err := LoadFrecency(p)
	require.NoError(t, err)
	require.NotNil(t, f)
	assert.Empty(t, f.TopN(10), "corrupt JSON degrades to empty")
}

// ---- TopN ----

// TestCov_TopN_TiebreakerAndOverflow covers the firstSeen tiebreaker (equal
// score + equal lastSeen → earlier firstSeen wins) and n > len clamping.
func TestCov_TopN_TiebreakerAndOverflow(t *testing.T) {
	now := time.Now()
	f := &Frecency{entries: []frecencyEntry{
		{Path: "newer", Count: 1, FirstSeen: now.Add(-2 * time.Hour), LastSeen: now},
		{Path: "older", Count: 1, FirstSeen: now.Add(-1 * time.Hour), LastSeen: now},
	}}
	// Equal score + equal lastSeen → firstSeen tiebreaker: the earlier-firstSeen
	// path ("newer") sorts first.
	top := f.TopN(2)
	assert.Equal(t, []string{"newer", "older"}, top)

	// n > len → clamped to len.
	assert.Len(t, f.TopN(100), 2)
}

// ---- score decay buckets ----

// TestCov_Score_DecayBuckets covers the 1h–24h (0.9) and 1d–7d (0.5) decay
// buckets (the <1h and >=7d buckets are already covered).
func TestCov_Score_DecayBuckets(t *testing.T) {
	now := time.Now()
	// 1h–24h → 0.9
	s := frecencyEntry{Count: 10, LastSeen: now.Add(-2 * time.Hour)}.score(now)
	assert.InDelta(t, 9.0, s, 1e-6, "1h-24h decay = 0.9")
	// 1d–7d → 0.5
	s = frecencyEntry{Count: 10, LastSeen: now.Add(-48 * time.Hour)}.score(now)
	assert.InDelta(t, 5.0, s, 1e-6, "1d-7d decay = 0.5")
}

// ---- Save ----

// TestCov_Save_MkdirAllDir covers the MkdirAll branch: saving into a not-yet-
// existing subdirectory creates it before writing.
func TestCov_Save_MkdirAllDir(t *testing.T) {
	tmp := t.TempDir()
	f := &Frecency{
		path:    filepath.Join(tmp, "sub", "deep", "frec.json"),
		entries: []frecencyEntry{{Path: "/x", Count: 1, FirstSeen: time.Now(), LastSeen: time.Now()}},
	}
	require.NoError(t, f.Save())
	_, err := os.Stat(f.path)
	assert.NoError(t, err, "Save created the nested dir + file")
}

// TestCov_Save_MkdirAllError covers the MkdirAll error branch: a FILE sitting
// where a directory must be created makes MkdirAll (and thus Save) fail.
func TestCov_Save_MkdirAllError(t *testing.T) {
	tmp := t.TempDir()
	// A file at tmp/sub blocks MkdirAll(tmp/sub/deep).
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "sub"), []byte("x"), 0o644))
	f := &Frecency{path: filepath.Join(tmp, "sub", "deep", "frec.json")}
	assert.Error(t, f.Save(), "MkdirAll fails when a file blocks the dir path")
}

// ---- frecencyPath ----

// TestCov_FrecencyPath_NoConfigDir covers the UserConfigDir-error path.
func TestCov_FrecencyPath_NoConfigDir(t *testing.T) {
	t.Setenv("APPDATA", "")
	t.Setenv("LOCALAPPDATA", "")
	// On Windows UserConfigDir reads APPDATA/LOCALAPPDATA; unsetting may yield "".
	if got := frecencyPath("root"); got != "" {
		t.Skipf("UserConfigDir did not error on this platform (got %q)", got)
	}
}

// ---- waitSave ----

// TestCov_WaitSave_NilAndClosed covers the nil-queue fast return and the
// closed-channel !ok branch.
func TestCov_WaitSave_NilAndClosed(t *testing.T) {
	assert.Nil(t, waitSave(nil))

	ch := make(chan saveCmd)
	close(ch)
	msg := waitSave(ch)()
	assert.Nil(t, msg, "closed channel → nil Msg")
}

// ---- enqueueSave ----

// TestCov_EnqueueSave_NilAndFull covers the nil-queue no-op and the full-queue
// drop (select default).
func TestCov_EnqueueSave_NilAndFull(t *testing.T) {
	// nil queue → no-op.
	m := &model{}
	m.enqueueSave(func() error { return nil }) // must not panic

	// Full queue → dropped via default (non-blocking).
	m = &model{saveQueue: make(chan saveCmd, 1)}
	m.saveQueue <- saveCmd{fn: func() error { return nil }} // fill it
	// This second enqueue hits the default branch; it must not block.
	done := make(chan struct{})
	go func() {
		m.enqueueSave(func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("enqueueSave blocked on a full queue")
	}
}

// ---- extractPathFromToolArgs ----

// TestCov_ExtractPath_EdgeCases covers the no-"path"-key, no-colon, and
// no-end-delimiter branches of the hand-rolled path extractor.
func TestCov_ExtractPath_EdgeCases(t *testing.T) {
	// No "path" key.
	assert.Equal(t, "", extractPathFromToolArgs("fs_write", `{"foo":"bar"}`))
	// "path" present but no colon follows.
	assert.Equal(t, "", extractPathFromToolArgs("fs_edit", `{"path"  }`))
	// No closing delimiter (",,}) → return the trimmed remainder.
	assert.Equal(t, "bareword", extractPathFromToolArgs("fs_mkdir", `{"path":  bareword`))
	// Sanity: the happy path still works.
	assert.Equal(t, "/proj/main.go", extractPathFromToolArgs("fs_write", `{"path":"/proj/main.go"}`))
}
