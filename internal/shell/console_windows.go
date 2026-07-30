//go:build windows

package shell

import (
	"context"
	"runtime"
)

// PlatformPTYCapability reports the Windows PTY backend state. Phase 0 returns
// Available=false — the ConPTY wrapper + Job Object tree-kill design is
// intentionally undecided.
func PlatformPTYCapability() PTYCapability {
	return PTYCapability{
		Platform:  runtime.GOOS,
		Backend:   "conpty-pending",
		Reason:    "ConPTY wrapper and job-object tree-kill decision pending",
		Available: false,
	}
}

// StartPTYProcess returns ErrPTYUnavailable in Phase 0. Future Phase 1+
// implementations will allocate a ConPTY here.
func StartPTYProcess(context.Context, LaunchSpec) (Process, Console, error) {
	return nil, nil, ErrPTYUnavailable
}

// CanKillTreeOnPlatform reports whether the OS factory can kill a process and
// all its descendants. On Windows Phase 0 returns false — the Job Object
// approach (which closes the handle on process exit and cascades the kill to
// children) is the obvious candidate but not yet wired. When the real
// implementation lands, flip this to true.
func CanKillTreeOnPlatform() bool { return false }
