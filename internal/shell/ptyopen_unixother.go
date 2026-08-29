//go:build unix && !linux && !darwin

package shell

import "os"

// ptyBackend names the mechanism openPTY uses, for the capability report.
const ptyBackend = "unsupported"

// openPTY reports that no reviewed PTY adapter exists for this Unix.
//
// The BSDs, Solaris and AIX each spell posix_openpt/grantpt/ptsname with their
// own ioctl numbers and their own device-name conventions, and guessing one
// wrong produces a master that reads EIO forever rather than a clean failure.
// Returning the sentinel keeps StartPTYProcess's contract — callers already
// branch on errors.Is(err, ErrPTYUnavailable) — instead of shipping an untested
// syscall sequence on a platform nobody here can run.
func openPTY() (*os.File, string, error) { return nil, "", ErrPTYUnavailable }
