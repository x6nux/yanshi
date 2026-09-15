//go:build !windows

package cli

import "syscall"

// detachedProcessAttr puts the daemon in its own session (setsid), so it
// survives the terminal that started it: a login shell sending SIGHUP to its
// process group, or the terminal closing, must not take the backend down. That
// is the whole difference between `yanshi serve` in a terminal and `yanshi -b`.
func detachedProcessAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
