//go:build !windows

package execprobe

import "testing"

// TestIsWindowsAppExecutionAlias_OffWindows covers the runtime.GOOS != "windows"
// short-circuit (probe.go:80-82). Off Windows the check is always false without
// touching the filesystem, so a never-on-PATH name returns false instantly.
// This file is excluded from the Windows build, so it runs only on the POSIX CI
// matrix where that branch is reachable.
func TestIsWindowsAppExecutionAlias_OffWindows(t *testing.T) {
	if isWindowsAppExecutionAlias("definitely-not-real-xyz") {
		t.Fatal("off Windows the alias check must always be false")
	}
}
