// Package testutil holds helpers shared by tests across packages. It imports
// "testing" from non-test files on purpose: Go only links a package into a
// binary that actually imports it, and nothing outside _test.go files does, so
// the yanshi binary never carries it.
//
// Keep this package tiny. A helper belongs here only when the same logic would
// otherwise be copy-pasted into three or more packages' test files; anything
// used by a single package stays in that package's own _test.go.
package testutil

import (
	"os"
	"runtime"
	"testing"
)

// SkipIfRoot skips the test when the process is running as root on a
// Unix-like system.
//
// Tests that prove a write FAILS by first chmod-ing the target to 0444 are
// asserting a DAC permission check — and root bypasses DAC entirely, so the
// write succeeds and the "must fail" assertion goes red. That never happens on
// GitHub's hosted runners (they run as a normal user), but it does in a
// container with no USER directive and on self-hosted runners started as root.
//
// Windows is excluded because os.Chmod there flips the read-only file
// attribute rather than a permission bit, and that is honored regardless of
// privilege; os.Geteuid also returns -1 on Windows, so the check would be
// meaningless.
func SkipIfRoot(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		t.Skip("running as root: a 0444 chmod does not block writes, so the read-only assertion cannot hold")
	}
}
