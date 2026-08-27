//go:build linux

package sandbox

import (
	"fmt"
	"net"
	"os"
	"testing"
)

// TestMain routes the Landlock helper argument before the test framework runs.
//
// This is what makes the process-level Landlock tests real. Those tests re-exec
// THIS TEST BINARY with landlockHelperArg, and without this hook the binary
// would just start the test suite again with an unrecognised flag: the helper
// path would never execute, every assertion would be about a process that
// never got confined, and the tests would report a confidence nobody earned.
//
// It also mirrors exactly what cmd/yanshi must do to wire this backend up (see
// the report accompanying this change), so the dispatch shape is exercised
// here before it exists in main.
//
// The helper never returns on success -- it execve()s the target -- so
// reaching the error branch means the policy could not be applied, and exiting
// nonzero is the fail-closed answer.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == landlockHelperArg {
		if err := RunLandlockHelper(os.Args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// listenLoopbackForTest opens a loopback listener the network tests try to
// reach from inside a sandbox. Loopback is used rather than an external host
// so a failure is attributable to the network namespace and not to DNS, a
// proxy, or a CI runner with no egress.
func listenLoopbackForTest() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}
