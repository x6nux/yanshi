//go:build unix

package shell

import (
	"os"
	"syscall"
)

// sigZero returns the signal used to probe whether a pid is still live.
//
// Signal 0 performs the permission and existence checks without delivering
// anything — the standard way to ask "is this process still there".
func sigZero() os.Signal { return syscall.Signal(0) }
