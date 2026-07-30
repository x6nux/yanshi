//go:build !windows

package secrets

import "os"

func replaceEncryptedFileOS(src, dst string) error { return os.Rename(src, dst) }
