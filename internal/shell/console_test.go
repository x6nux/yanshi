package shell

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

func TestPlatformPTYCapabilityIsHonestUnavailable(t *testing.T) {
	cap := PlatformPTYCapability()
	if cap.Available {
		t.Fatalf("Phase 0 must NOT advertise PTY as available: %#v", cap)
	}
	if cap.Backend == "" || cap.Reason == "" {
		t.Fatalf("capability fields missing: %#v", cap)
	}
}

func TestStartPTYReturnsErrPTYUnavailable(t *testing.T) {
	_, _, err := StartPTYProcess(context.Background(), LaunchSpec{Program: "echo", PTY: true})
	if !errors.Is(err, ErrPTYUnavailable) {
		t.Fatalf("want ErrPTYUnavailable, got %v", err)
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
