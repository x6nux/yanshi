// internal/lockfile/alive_unix.go
//go:build !windows

package lockfile

import "syscall"

// Alive reports whether the recorded PID is an existing process. It is a
// best-effort fast pre-filter; discovery always confirms with a healthz probe
// because PID liveness does not prove the backend is serving.
func (lf Lockfile) Alive() bool {
	if lf.PID <= 0 {
		return false
	}
	// signal 0: no signal sent; success means the process exists.
	if err := syscall.Kill(lf.PID, syscall.Signal(0)); err != nil {
		return false
	}
	return true
}
