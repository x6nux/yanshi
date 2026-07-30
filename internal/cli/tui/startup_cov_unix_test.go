//go:build !windows

package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_DetectShellEnv_Posix covers the posix fallback of detectShellEnv:
// with SHELL unset it returns "sh". Windows-only branches live in
// startup_cov_windows_test.go.
func TestCov_DetectShellEnv_Posix(t *testing.T) {
	t.Setenv("SHELL", "")
	assert.Equal(t, "sh", detectShellEnv())
}
