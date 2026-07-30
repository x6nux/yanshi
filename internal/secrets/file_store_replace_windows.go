//go:build windows

package secrets

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// MoveFileEx(REPLACE_EXISTING|WRITE_THROUGH) supplies the replace-existing
// semantics that os.Rename does not guarantee on Windows.
func replaceEncryptedFileOS(src, dst string) error {
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
