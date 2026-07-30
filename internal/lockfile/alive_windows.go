// internal/lockfile/alive_windows.go
//go:build windows

package lockfile

import (
	"golang.org/x/sys/windows"
)

// stillActive is the Windows exit-code sentinel meaning "still running".
const stillActive = 259

// Alive reports whether the recorded PID is an existing, running process.
// Best-effort fast pre-filter; discovery confirms with a healthz probe.
func (lf Lockfile) Alive() bool {
	if lf.PID <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(lf.PID))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
