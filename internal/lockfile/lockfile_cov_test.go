// Package lockfile coverage tests for hard-to-reach error paths.
// These tests are in the same package (not lockfile_test) to access unexported
// functions and struct fields, though the coverage gaps are primarily in
// exported error-handling branches.
package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/testutil"
)

// TestRead_DirectoryInsteadOfFile verifies that Read returns a non-ErrNotExist
// error when the lockfile path is a directory rather than a regular file.
// This exercises the os.ReadFile non-ErrNotExist branch.
func TestRead_DirectoryInsteadOfFile(t *testing.T) {
	root := t.TempDir() + "/read-dir"
	p, err := Path(root)
	require.NoError(t, err)

	// Create the lockfile parent directory, then place a directory at the
	// lockfile path so os.ReadFile fails with an error other than ErrNotExist.
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.MkdirAll(p, 0o755))
	t.Cleanup(func() { os.RemoveAll(p) })

	_, err = Read(root)
	require.Error(t, err)
	require.False(t, errors.Is(err, os.ErrNotExist),
		"reading a directory must produce a non-ErrNotExist error")
}

// TestRemove_NonEmptyDirectory verifies that Remove returns an error (not
// silently swallowed) when the lockfile path is a non-empty directory,
// exercising the non-ErrNotExist return in Remove.
func TestRemove_NonEmptyDirectory(t *testing.T) {
	root := t.TempDir() + "/remove-dir"
	p, err := Path(root)
	require.NoError(t, err)

	// Create a non-empty directory at the lockfile path.
	require.NoError(t, os.MkdirAll(p, 0o755))
	inner := filepath.Join(p, "inner.txt")
	require.NoError(t, os.WriteFile(inner, []byte("x"), 0o644))
	t.Cleanup(func() { os.RemoveAll(p) })

	err = Remove(root)
	require.Error(t, err, "Remove on a non-empty directory must fail")
	require.False(t, errors.Is(err, os.ErrNotExist))
}

// TestAcquire_O_EXCL_Directory verifies that Acquire returns an error (not
// os.ErrExist) when os.OpenFile with O_EXCL fails because the target path
// is a directory, exercising the non-ErrExist return in Acquire.
func TestAcquire_O_EXCL_Directory(t *testing.T) {
	root := t.TempDir() + "/oexcl-dir"
	p, err := Path(root)
	require.NoError(t, err)

	// Place a directory at the lockfile path so O_CREATE|O_EXCL fails with
	// an error that is NOT os.ErrExist (typically EISDIR on Unix or
	// ERROR_ACCESS_DENIED on Windows).
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.MkdirAll(p, 0o755))
	t.Cleanup(func() { os.RemoveAll(p) })

	lf := Lockfile{PID: 42, Addr: "127.0.0.1:1", Root: root}
	_, err = Acquire(root, lf)
	require.Error(t, err)
	require.False(t, errors.Is(err, os.ErrExist),
		"opening a directory with O_EXCL must produce non-ErrExist error")
}

// TestAcquire_StaleReclaimWriteFailure verifies that Acquire returns an error
// when the stale-reclaim path fails to overwrite a read-only lockfile. This
// exercises the os.WriteFile error branch in the stale-reclaim path.
func TestAcquire_StaleReclaimWriteFailure(t *testing.T) {
	root := t.TempDir() + "/stale-write-fail"

	// Write a lockfile with a dead PID (stale).
	require.NoError(t, Write(root, Lockfile{PID: 999999, Root: root}))

	p, err := Path(root)
	require.NoError(t, err)

	// Make the lockfile read-only so the overwrite attempt fails.
	testutil.SkipIfRoot(t) // root bypasses the 0444 guard below
	require.NoError(t, os.Chmod(p, 0o444))
	t.Cleanup(func() {
		_ = os.Chmod(p, 0o644) // restore before removal
		os.RemoveAll(p)
	})

	_, err = Acquire(root, Lockfile{PID: os.Getpid(), Addr: "127.0.0.1:2", Root: root})
	require.Error(t, err, "Acquire must fail when WriteFile fails during stale reclaim")
}

// TestWrite_JsonMarshalError verifies that Write returns an error when
// json.MarshalIndent fails (e.g., a time.Time with an out-of-range year
// causes MarshalJSON to error).
func TestWrite_JsonMarshalError(t *testing.T) {
	root := t.TempDir() + "/write-json-err"
	// A year outside the range [0,9999] causes time.Time.MarshalJSON to
	// return an error. Since StartedAt is not zero (year -1), the IsZero
	// check in Write skips auto-stamp, so the invalid date reaches Marshal.
	lf := Lockfile{
		PID:       42,
		Addr:      "127.0.0.1:1",
		Root:      root,
		StartedAt: time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC),
		Version:   currentVersion,
	}
	err := Write(root, lf)
	require.Error(t, err)
}

// TestAcquire_JsonMarshalError verifies that Acquire returns an error when
// json.Marshal fails (same mechanism as TestWrite_JsonMarshalError).
func TestAcquire_JsonMarshalError(t *testing.T) {
	root := t.TempDir() + "/acquire-json-err"
	lf := Lockfile{
		PID:       42,
		Addr:      "127.0.0.1:1",
		Root:      root,
		StartedAt: time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC),
		Version:   currentVersion,
	}
	_, err := Acquire(root, lf)
	require.Error(t, err)
}

// TestCov_NoCacheDir covers the os.UserCacheDir-error propagation across Dir,
// Path, Write, Read, Remove, and Acquire. Clearing LocalAppData (Windows) plus
// XDG_CACHE_HOME and HOME (Linux/macOS) makes UserCacheDir fail everywhere.
func TestCov_NoCacheDir(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	_, err := Dir()
	require.Error(t, err)
	_, err = Path("/proj")
	require.Error(t, err)
	require.Error(t, Write("/proj", Lockfile{PID: 1}))
	_, err = Read("/proj")
	require.Error(t, err)
	require.Error(t, Remove("/proj"))
	_, err = Acquire("/proj", Lockfile{PID: 1})
	require.Error(t, err)
}
