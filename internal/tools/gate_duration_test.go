package tools

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// durationFromClock matches the evidence field being derived from elapsed time
// rather than from a constant, a hardcoded value, or nothing at all.
var durationFromClock = regexp.MustCompile(`DurationMs:\s+time\.Since\(\w+\)\.Milliseconds\(\)`)

// TestGateEvidenceDurationIsMeasured closes the last unevidenced clause of
// A2/DT2 ("退出码/duration 准确").
//
// The exit code half was already covered. Duration was not, at all: measured
// during W1 review round 26, `grep DurationMs` across every _test.go turned up
// zero assert/require — the field appeared only as a literal input when tests
// constructed an Evidence by hand. Setting gate.go's DurationMs to 0 reddened
// nothing.
//
// Asserted at the source because the honest end-to-end alternative is worse:
// driving the real gate needs a task manager, a store and a live process, and
// the only assertion available at the end would be "elapsed >= 0", which a
// hardcoded 0 also satisfies. What matters is that the number comes from the
// clock, and that is what this checks.
func TestGateEvidenceDurationIsMeasured(t *testing.T) {
	src, err := os.ReadFile("gate.go")
	if err != nil {
		t.Fatalf("read gate.go: %v", err)
	}
	if !durationFromClock.Match(src) {
		t.Error("gate.go no longer derives Evidence.DurationMs from elapsed time; " +
			"a constant or zero here makes every gate look instantaneous and the " +
			"field useless for spotting a slow gate")
	}
	// The clock must be started before the command runs, not after.
	body := string(src)
	startedAt := strings.Index(body, "started := time.Now()")
	if startedAt < 0 {
		t.Fatal("gate.go no longer records a start time")
	}
	if runAt := strings.Index(body, "CommandContext"); runAt >= 0 && runAt < startedAt {
		t.Error("the clock starts after the command is launched, so DurationMs " +
			"excludes the part of the run it exists to measure")
	}
}
