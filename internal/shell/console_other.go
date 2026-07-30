//go:build !unix && !windows

package shell

import (
	"context"
	"runtime"
)

// PlatformPTYCapability reports the fallback PTY state for platforms no
// reviewed PTY adapter exists for (e.g. plan9). Phase 0 is honest about the
// absence; Phase 1+ work would have to ship per-platform adapters.
func PlatformPTYCapability() PTYCapability {
	return PTYCapability{
		Platform:  runtime.GOOS,
		Backend:   "unsupported",
		Reason:    "no PTY adapter reviewed for this platform",
		Available: false,
	}
}

// StartPTYProcess returns ErrPTYUnavailable on unsupported platforms.
func StartPTYProcess(context.Context, LaunchSpec) (Process, Console, error) {
	return nil, nil, ErrPTYUnavailable
}

// CanKillTreeOnPlatform returns false on platforms without a reviewed kill
// implementation. Leaf-only kill still works via (*osProcess).Kill(); the
// capability bit only gates whether tree-kill is advertised.
func CanKillTreeOnPlatform() bool { return false }
