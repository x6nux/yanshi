//go:build darwin

package shell

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ptyBackend names the mechanism openPTY uses, for the capability report.
const ptyBackend = "darwin /dev/ptmx + TIOCPTYGRANT/TIOCPTYUNLK/TIOCPTYGNAME"

// ptySlaveNameLen is the buffer size TIOCPTYGNAME writes into. It is fixed by
// the kernel's ioctl definition (_IOC(IOC_OUT, 't', 83, 128)), NOT a guess: the
// kernel copies exactly 128 bytes out regardless of what the caller thinks it
// allocated, so a smaller buffer is a stack overwrite rather than a truncated
// name.
const ptySlaveNameLen = 128

// openPTY allocates a pseudo-terminal pair and returns the master as an
// *os.File plus the filesystem path of the slave.
//
// The sequence is the POSIX one (posix_openpt / grantpt / unlockpt / ptsname)
// expressed in raw ioctls, because those four are libc functions and this
// binary is built with CGO_ENABLED=0 in the release matrix.
//
//   - TIOCPTYGRANT is grantpt: it hands ownership of the slave device to the
//     calling user. Without it the slave is owned by root and the open below
//     fails with EACCES for a non-root operator.
//   - TIOCPTYUNLK is unlockpt. The kernel keeps a freshly cloned slave locked
//     precisely so that a caller cannot open it between clone and grant, and
//     opening it while locked returns EIO.
//   - TIOCPTYGNAME is ptsname. It is the only one of the three that takes a
//     pointer argument, and x/sys/unix exports no darwin ioctl helper with a
//     128-byte out-parameter (IoctlGetInt writes 8, IoctlGetTermios 72), so it
//     goes through unix.Syscall directly. unix.IoctlSetString would allocate a
//     large enough buffer but discards it, which is exactly the byte we need.
//
// O_CLOEXEC on the master matters: the master fd must not survive into the
// child, or the child holds the last reference to its own master and the
// parent's Read never sees EOF when the child exits.
func openPTY() (*os.File, string, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, "", fmt.Errorf("shell: open /dev/ptmx: %w", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		_ = unix.Close(fd)
		return nil, "", fmt.Errorf("shell: grantpt: %w", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		_ = unix.Close(fd)
		return nil, "", fmt.Errorf("shell: unlockpt: %w", err)
	}
	var buf [ptySlaveNameLen]byte
	if _, _, errno := unix.Syscall(
		unix.SYS_IOCTL,
		uintptr(fd),
		uintptr(unix.TIOCPTYGNAME),
		uintptr(unsafe.Pointer(&buf[0])),
	); errno != 0 {
		_ = unix.Close(fd)
		return nil, "", fmt.Errorf("shell: ptsname: %w", errno)
	}
	name := string(buf[:cstringLen(buf[:])])
	if name == "" {
		_ = unix.Close(fd)
		return nil, "", fmt.Errorf("shell: ptsname returned an empty slave name")
	}
	return os.NewFile(uintptr(fd), "/dev/ptmx"), name, nil
}

// cstringLen returns the length of the NUL-terminated string at the head of b,
// or len(b) when there is no NUL. The kernel fills the tail of the TIOCPTYGNAME
// buffer with zeros, so slicing on the first NUL is what turns 128 bytes into
// "/dev/ttys004".
func cstringLen(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}
