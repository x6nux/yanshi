//go:build windows

package tui

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// replacePreferencesFileOS uses MoveFileEx with REPLACE_EXISTING so an
// existing prefs file can be atomically replaced on Windows. WRITE_THROUGH
// asks the OS to flush the move before reporting success, mirroring the
// fsync-after-rename intent of the Unix path.
func replacePreferencesFileOS(src, dst string) error {
	srcp, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	dstp, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	err = windows.MoveFileEx(srcp, dstp,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return os.ErrNotExist
	}
	return err
}
