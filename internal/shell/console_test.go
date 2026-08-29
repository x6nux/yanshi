package shell

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// TestPTYRequestNeedsAProgram pins the one input error that is the caller's
// fault rather than the platform's.
//
// It is asserted separately from the sentinel because the two mean opposite
// things to a caller: ErrPTYUnavailable says "ask for a pipe instead", an empty
// Program says "your LaunchSpec is wrong and a pipe will not help either". The
// pipe path in process.go already rejects it; this keeps the PTY path from
// answering a different question for the same broken spec.
//
// Platforms without a PTY backend legitimately answer with the sentinel before
// they ever look at Program, so both outcomes are accepted here and the
// availability-vs-sentinel agreement is pinned by
// TestPTYConsoleIsWiredOnEveryPlatform instead.
func TestPTYRequestNeedsAProgram(t *testing.T) {
	_, _, err := StartPTYProcess(context.Background(), LaunchSpec{PTY: true})
	if err == nil {
		t.Fatalf("a PTY spawn with no Program must fail")
	}
	if errors.Is(err, ErrPTYUnavailable) {
		return
	}
	if !strings.Contains(err.Error(), "Program required") {
		t.Fatalf("want a spec error naming Program, got %v", err)
	}
}

func TestOSProcessFactoryCanKillTreeReflectsPlatform(t *testing.T) {
	caps := (&OSProcessFactory{}).Capabilities(context.Background())
	// Bidirectional (M1): Windows Phase 0 cannot tree-kill yet; Unix kills via
	// the process group. Asserting BOTH directions stops the test from silently
	// passing as a no-op on either platform.
	if runtime.GOOS == "windows" {
		if caps.CanKillTree {
			t.Fatalf("Windows Phase 0 must not claim tree kill: %#v", caps)
		}
	} else {
		if !caps.CanKillTree {
			t.Fatalf("Unix must claim tree kill via process group: %#v", caps)
		}
	}
}
