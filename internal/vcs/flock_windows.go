// internal/vcs/flock_windows.go

//go:build windows

package vcs

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lockedRegionLength is the byte range locked on the lock file. The file itself
// stays empty — LockFileEx locks a byte range regardless of file size, so a
// single fixed region is enough to express "this lane is held" without writing
// any payload.
const lockedRegionLength = 1

// lockFileExclusive takes a blocking exclusive lock on f.
//
// LockFileEx without LOCKFILE_FAIL_IMMEDIATELY blocks until the range is
// available, matching the POSIX flock(LOCK_EX) behaviour the callers expect.
// Windows releases the lock when the handle closes or the process exits, so a
// crashed holder is reclaimed by the OS exactly as on Unix.
func lockFileExclusive(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0, lockedRegionLength, 0, ol,
	)
	if err != nil {
		return fmt.Errorf("vcs: acquire cross-process repo lock: %w", err)
	}
	return nil
}

// unlockFile releases the locked region on f.
func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.UnlockFileEx(
		windows.Handle(f.Fd()), 0, lockedRegionLength, 0, ol,
	)
	if err != nil {
		return fmt.Errorf("vcs: release cross-process repo lock: %w", err)
	}
	return nil
}

// tryLockFileExclusive takes the lock only if it is free, reporting whether it
// got it. It never blocks.
//
// LOCKFILE_FAIL_IMMEDIATELY turns contention into ERROR_LOCK_VIOLATION rather
// than a wait, which is the answer the reclaim sweep wants: a held lane means
// "in use, leave the file alone", not an error. See the Unix twin for why the
// sweep must not block.
func tryLockFileExclusive(f *os.File) (bool, error) {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, lockedRegionLength, 0, ol,
	)
	if err == nil {
		return true, nil
	}
	if err == windows.ERROR_LOCK_VIOLATION || err == windows.ERROR_IO_PENDING {
		return false, nil
	}
	return false, fmt.Errorf("vcs: probe cross-process repo lock: %w", err)
}
