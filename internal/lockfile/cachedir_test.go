// internal/lockfile/cachedir_test.go
package lockfile

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain points this package's tests at a throwaway cache directory.
//
// Path/Acquire/Write all resolve through os.UserCacheDir(), which is the REAL
// per-user cache directory, so every test in this file was writing its lockfile
// into the same directory the operator's daemon uses — and none of them removed
// it. Measured before this existed: 97 files and 392 KB of TestAcquire_*,
// TestRead_* and TestWrite_* lockfiles had accumulated in
// ~/Library/Caches/yanshi/run, next to the live daemon's own lockfile.
//
// Redirecting the environment rather than injecting a directory keeps the
// tests testing the real resolution path (the thing under test IS "where does
// the cache dir point"), and the per-OS variables are all set because
// os.UserCacheDir reads a different one on each platform: HOME (darwin),
// XDG_CACHE_HOME then HOME (linux), LocalAppData (windows).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "yanshi-lockfile-test-cache-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", dir)
	_ = os.Setenv("USERPROFILE", dir)
	_ = os.Setenv("XDG_CACHE_HOME", filepath.Join(dir, ".cache"))
	_ = os.Setenv("LocalAppData", filepath.Join(dir, "AppData", "Local"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
