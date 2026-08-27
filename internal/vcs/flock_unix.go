// internal/vcs/flock_unix.go

//go:build !windows

package vcs

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockFileExclusive takes a blocking exclusive advisory lock on f.
//
// flock is per open-file-description, not per process: two descriptors on the
// same inode contend even inside one process, which is what lets the tests
// exercise cross-process semantics without spawning a subprocess. The kernel
// releases the lock when the descriptor closes or the process exits by any
// means, so a crashed holder needs no reclaim logic.
//
// EINTR is retried rather than surfaced: a blocking flock interrupted by a
// signal (Go's own preemption signals reach this call) has not acquired
// anything, and returning an error there would make the write lane fail at
// random under normal scheduling.
func lockFileExclusive(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("vcs: acquire cross-process repo lock: %w", err)
		}
		return nil
	}
}

// unlockFile releases the advisory lock held on f.
func unlockFile(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_UN)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("vcs: release cross-process repo lock: %w", err)
		}
		return nil
	}
}

// tryLockFileExclusive takes the lock only if it is free, reporting whether it
// got it. It never blocks.
//
// This is the reclaim primitive, not a write-lane primitive: the lane itself
// must block (see the file header), but deleting a stale lock file must not
// wait behind a live holder — a sweep that blocked would stall a fresh process
// behind whatever long materialize another one is running.
//
// EWOULDBLOCK/EAGAIN means "somebody holds it", which is a normal answer and
// not an error. Everything else is reported.
func tryLockFileExclusive(f *os.File) (bool, error) {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		switch err {
		case nil:
			return true, nil
		case unix.EINTR:
			continue
		case unix.EWOULDBLOCK:
			return false, nil
		default:
			return false, fmt.Errorf("vcs: probe cross-process repo lock: %w", err)
		}
	}
}
