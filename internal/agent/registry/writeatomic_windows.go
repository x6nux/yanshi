//go:build windows

package registry

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// writeAtomic atomically writes data to path using a temp file + MoveFileEx with
// MOVEFILE_REPLACE_EXISTING. This is required on Windows where os.Rename fails
// when the target already exists.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()

	destPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		os.Remove(tmpPath)
		return err
	}
	tmpPtr, err := windows.UTF16PtrFromString(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return err
	}
	// MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH
	if err := windows.MoveFileEx(tmpPtr, destPtr, 3); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
