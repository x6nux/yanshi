package execprobe

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestRun_SkipsWindowsAppExecutionAlias verifies the alias short-circuit: on
// Windows, python3 typically resolves to a Store App Execution Alias that would
// otherwise block for the full 3s deadline. The skip must make Run return ""
// near-instantly (LookPath only — the target is never executed).
func TestRun_SkipsWindowsAppExecutionAlias(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific alias behavior")
	}
	if !isWindowsAppExecutionAlias("python3") {
		t.Skip("python3 is not an App Execution Alias on this machine")
	}
	start := time.Now()
	got := Run("python3", "--version")
	elapsed := time.Since(start)
	assert.Equal(t, "", got, "alias probe must return empty (skipped, not executed)")
	assert.Less(t, elapsed, 500*time.Millisecond,
		"alias skip must be near-instant via LookPath, not wait for the 3s deadline")
}

// TestRun_NonexistentCommandReturnsEmpty confirms a missing tool resolves to ""
// quickly (LookPath fails inside the alias check, Run still proceeds and the
// probe fails fast) — not a hang.
func TestRun_NonexistentCommandReturnsEmpty(t *testing.T) {
	start := time.Now()
	got := Run("definitely-not-a-real-tool-xyzzy", "--version")
	elapsed := time.Since(start)
	assert.Equal(t, "", got)
	assert.Less(t, elapsed, 500*time.Millisecond)
}

func TestRun_SuccessfulCommandReturnsFirstLine(t *testing.T) {
	got := Run("go", "version")
	if got == "" {
		t.Fatal("'go version' must produce output on a Go-enabled system")
	}
}
