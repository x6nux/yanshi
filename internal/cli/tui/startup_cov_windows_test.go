//go:build windows

package tui

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCov_DetectShellEnv_Windows covers the Windows-only branches of
// detectShellEnv: bash on PATH (Git Bash), PSModulePath set (powershell), and
// the cmd fallback. The cross-platform SHELL-env branch is covered in
// startup_cov_test.go; the posix "sh" fallback is in startup_cov_unix_test.go.
func TestCov_DetectShellEnv_Windows(t *testing.T) {
	t.Setenv("SHELL", "")
	if _, err := exec.LookPath("bash"); err == nil {
		assert.Equal(t, "bash (Git Bash)", detectShellEnv())
	}
	t.Setenv("PATH", "")
	t.Setenv("PSModulePath", "something")
	assert.Equal(t, "powershell", detectShellEnv())

	t.Setenv("PATH", "")
	t.Setenv("PSModulePath", "")
	assert.Equal(t, "cmd", detectShellEnv())
}
