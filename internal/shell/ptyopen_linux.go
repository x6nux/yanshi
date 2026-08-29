//go:build linux

package shell

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ptyBackend names the mechanism openPTY uses, for the capability report.
const ptyBackend = "linux /dev/ptmx + TIOCSPTLCK/TIOCGPTN"

// openPTY allocates a pseudo-terminal pair and returns the master as an
// *os.File plus the filesystem path of the slave.
//
// Linux splits the POSIX grantpt/unlockpt pair differently from the BSDs:
// devpts already creates the slave owned by the opener, so grantpt is a no-op
// and only the unlock is a real operation. TIOCSPTLCK with a value of 0 is
// unlockpt; TIOCGPTN is ptsname, returning the numeric index whose device node
// is /dev/pts/<n>.
//
// TIOCSPTLCK takes a POINTER to an int, which is why this uses
// IoctlSetPointerInt rather than IoctlSetInt — the latter passes the value in
// the argument register and the kernel would dereference 0.
//
// O_CLOEXEC on the master matters: the master fd must not survive into the
// child, or the child holds the last reference to its own master and the
// parent's Read never sees EOF when the child exits.
func openPTY() (*os.File, string, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, "", fmt.Errorf("shell: open /dev/ptmx: %w", err)
	}
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(fd)
		return nil, "", fmt.Errorf("shell: unlockpt: %w", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		_ = unix.Close(fd)
		return nil, "", fmt.Errorf("shell: ptsname: %w", err)
	}
	return os.NewFile(uintptr(fd), "/dev/ptmx"), fmt.Sprintf("/dev/pts/%d", n), nil
}
