//go:build windows

package cli

import "syscall"

// Windows has no setsid. DETACHED_PROCESS detaches the child from the
// console — which is what stops a closing console window from killing it —
// and CREATE_NEW_PROCESS_GROUP keeps Ctrl+C aimed at the foreground process
// group instead of the daemon. The flags are written as literals because
// syscall does not export DETACHED_PROCESS on every Go release.
const (
	detachedProcessFlag       = 0x00000008
	createNewProcessGroupFlag = 0x00000200
)

func detachedProcessAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: detachedProcessFlag | createNewProcessGroupFlag}
}
