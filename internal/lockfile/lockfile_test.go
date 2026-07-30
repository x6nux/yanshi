// internal/lockfile/lockfile_test.go
package lockfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPath_IsPerProjectUnderCacheDir(t *testing.T) {
	root := "/home/me/projects/app"
	if runtime.GOOS == "windows" {
		root = `C:\Users\me\projects\app`
	}
	p, err := Path(root)
	require.NoError(t, err)

	dir, err := Dir()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(p, dir+string(filepath.Separator)),
		"%q must live under %q", p, dir)
	assert.True(t, strings.HasSuffix(p, ".lock"), "%q must end in .lock", p)
	// The key must be derived from the absolute root, not contain raw separators.
	assert.NotContains(t, p, root) // raw form with separators must not appear
	assert.Contains(t, p, sanitize(root))
}

func TestSanitize_ReplacesSeparatorsAndDrive(t *testing.T) {
	in := "/home/me/projects/app"
	out := sanitize(in)
	assert.NotContains(t, out, "/")
	assert.NotContains(t, out, ":")
	assert.NotEmpty(t, out)

	if runtime.GOOS == "windows" {
		w := sanitize(`C:\Users\me\app`)
		assert.NotContains(t, w, `\`)
		assert.NotContains(t, w, ":")
	}
}

func TestWriteReadRemove_RoundTrip(t *testing.T) {
	root := t.TempDir() // acts as the project root; lockfile key derives from this
	in := Lockfile{PID: 42, Addr: "127.0.0.1:9999", Auth: "none", Root: root, Version: currentVersion}

	require.NoError(t, Write(root, in))

	got, err := Read(root)
	require.NoError(t, err)
	assert.Equal(t, in.PID, got.PID)
	assert.Equal(t, in.Addr, got.Addr)
	assert.Equal(t, in.Root, got.Root)
	assert.Equal(t, currentVersion, got.Version)
	assert.False(t, got.StartedAt.IsZero(), "Write stamps StartedAt")

	require.NoError(t, Remove(root))

	_, err = Read(root)
	assert.Error(t, err, "Read after Remove must fail")

	// Remove is idempotent: removing a missing lockfile is not an error.
	require.NoError(t, Remove(root))
}

func TestAlive_CurrentProcessAlive_DeadPIDNot(t *testing.T) {
	live := Lockfile{PID: os.Getpid()}
	assert.True(t, live.Alive(), "current process must be alive")

	// A PID that is extremely unlikely to exist.
	dead := Lockfile{PID: 1<<31 - 1}
	assert.False(t, dead.Alive(), "bogus PID must not be alive")

	// Zero and negative PIDs are also not alive.
	zeroPID := Lockfile{PID: 0}
	assert.False(t, zeroPID.Alive(), "PID 0 must not be alive")
}

func TestAcquire_FirstCallerWins_SecondLoses(t *testing.T) {
	root := t.TempDir() + "/proj"
	lf := Lockfile{PID: os.Getpid(), Addr: "127.0.0.1:1", Auth: "none", Root: root}

	ok1, err := Acquire(root, lf)
	require.NoError(t, err)
	assert.True(t, ok1, "first Acquire must win")

	// A second caller with a different PID must lose (file already exists).
	ok2, err := Acquire(root, Lockfile{PID: os.Getpid() + 1, Addr: "127.0.0.1:2", Root: root})
	require.NoError(t, err)
	assert.False(t, ok2, "second Acquire must lose to the existing lockfile")

	// The recorded lockfile must be the winner's.
	got, err := Read(root)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:1", got.Addr)
}

func TestAcquire_StaleIsReclaimed(t *testing.T) {
	root := t.TempDir() + "/proj2"
	// Pre-write a lockfile whose PID is dead.
	require.NoError(t, Write(root, Lockfile{PID: 1<<31 - 1, Addr: "127.0.0.1:9", Root: root}))

	ok, err := Acquire(root, Lockfile{PID: os.Getpid(), Addr: "127.0.0.1:10", Root: root})
	require.NoError(t, err)
	assert.True(t, ok, "Acquire must reclaim a lockfile whose PID is dead")
}

func TestRead_CorruptDataReturnsError(t *testing.T) {
	root := t.TempDir() + "/proj3"
	p, err := Path(root)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte("{not valid json}"), 0o644))

	_, err = Read(root)
	require.Error(t, err, "Read must fail on corrupt data")
}

func TestWrite_AutoStampsDefaults(t *testing.T) {
	root := t.TempDir() + "/proj4"
	lf := Lockfile{PID: 100, Addr: "127.0.0.1:1111", Root: root}
	require.NoError(t, Write(root, lf))

	got, err := Read(root)
	require.NoError(t, err)
	assert.Equal(t, currentVersion, got.Version, "Write must stamp default Version")
	assert.False(t, got.StartedAt.IsZero(), "Write must stamp StartedAt")
}

func TestAcquire_AutoStampsDefaults(t *testing.T) {
	root := t.TempDir() + "/proj5"
	ok, err := Acquire(root, Lockfile{PID: os.Getpid(), Addr: "127.0.0.1:2222", Root: root})
	require.NoError(t, err)
	require.True(t, ok, "Acquire must succeed")

	got, err := Read(root)
	require.NoError(t, err)
	assert.Equal(t, currentVersion, got.Version, "Acquire must stamp default Version")
	assert.False(t, got.StartedAt.IsZero(), "Acquire must stamp StartedAt")
}

func TestAcquire_CorruptStaleNotReclaimed(t *testing.T) {
	root := t.TempDir() + "/proj6"
	p, err := Path(root)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	// Write invalid data so the stale-reclaim path fails at Read.
	require.NoError(t, os.WriteFile(p, []byte("garbage"), 0o644))

	_, err = Acquire(root, Lockfile{PID: os.Getpid(), Addr: "127.0.0.1:3333", Root: root})
	require.Error(t, err, "Acquire must fail when lockfile has corrupt data")
}
