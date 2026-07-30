package execprobe

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestRun_TimeoutReturnsEmpty covers the timeout branch (probe.go:66-67): when
// a command outlives the deadline, the detached ctx.Done() select arm fires and
// Run returns "" without waiting for CombinedOutput. Run uses a package-level
// 3s deadline, so this exercises the real timeout path (not a fast error). The
// goroutine+select structure is what makes the timeout reliable — ctx.Done()
// closes at the deadline while the kill/pipe-teardown path returns slightly
// later, so the detached arm wins the select.
func TestRun_TimeoutReturnsEmpty(t *testing.T) {
	// Pick a command that runs comfortably longer than the 3s deadline on each
	// platform. It must still be alive at the deadline so the timeout arm — not
	// a normal exit — drives the return.
	var cmd string
	var args []string
	if runtime.GOOS == "windows" {
		// ping waits ~1s between echoes; 6 echoes ≈ 5s, well past the deadline.
		cmd, args = "ping", []string{"-n", "6", "127.0.0.1"}
	} else {
		cmd, args = "sleep", []string{"10"}
	}

	start := time.Now()
	got := Run(cmd, args...)
	elapsed := time.Since(start)

	assert.Equal(t, "", got, "a timed-out probe must return empty")
	// Must have waited for (roughly) the deadline, proving it was the timeout
	// arm rather than a sub-second error exit. Allow headroom for scheduling.
	assert.Greater(t, elapsed, 2500*time.Millisecond, "Run should block near the 3s deadline")
	assert.Less(t, elapsed, 8*time.Second, "Run must not block far past the deadline (detached goroutine reclaims)")
}
