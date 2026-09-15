package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain gives this package's tests their own HOME and cache directory.
//
// lockfile.Path resolves through os.UserCacheDir(), and several tests here
// exercise the owner-election and discovery paths that WRITE a lockfile — into
// the operator's real run directory, next to the live daemon's own file, with
// nothing to remove them (measured: TestBootstrapOwner_LosesToAliveOwner,
// TestResolve_ConnectsToLiveRemote and TestCheckLockfile_Stale each left one
// behind). Redirecting the environment keeps the production resolution path
// under test while keeping its output out of the user's home; the variables are
// per-platform because os.UserHomeDir/UserCacheDir read a different one on each
// (HOME on darwin, HOME/XDG_CACHE_HOME on linux, USERPROFILE/LocalAppData on
// windows).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "yanshi-cli-test-home-*")
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
