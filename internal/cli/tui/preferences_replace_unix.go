//go:build !windows

package tui

import "os"

// replacePreferencesFileOS atomically replaces dst on Unix; rename(2) keeps
// the old file intact if the operation fails.
func replacePreferencesFileOS(src, dst string) error { return os.Rename(src, dst) }
