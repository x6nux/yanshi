//go:build unix

package shell

import (
	"context"
	"runtime"
)

// PlatformPTYCapability reports the Unix PTY backend state. Phase 0 returns
// Available=false — the selection between creack/pty, golang.org/x/term, and
// a hand-rolled openpty wrapper is intentionally undecided.
func PlatformPTYCapability() PTYCapability {
	return PTYCapability{
		Platform:  runtime.GOOS,
		Backend:   "unix-pty-pending",
		Reason:    "creack/pty vs x/term vs custom wrapper decision not yet made",
		Available: false,
	}
}

// StartPTYProcess returns ErrPTYUnavailable in Phase 0. Future Phase 1+
// implementations will allocate a real PTY pair here.
func StartPTYProcess(context.Context, LaunchSpec) (Process, Console, error) {
	return nil, nil, ErrPTYUnavailable
}

// CanKillTreeOnPlatform reports whether the OS factory can kill a process and
// all its descendants. On Unix this is true via setpgid(setpgid(0,0)) +
// kill(-pgid); the actual syscall wiring lands in Phase 1, but the capability
// bit is honest about the platform's potential so callers (and tests) can
// reason about the contract now.
func CanKillTreeOnPlatform() bool { return true }
