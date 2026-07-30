//go:build windows

package acp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// createNonACPAgent places a fake "opencode" on PATH that starts but does not
// speak ACP (prints non-JSON and exits), so Spawn's Initialize step fails and
// the cleanup path is exercised. Windows form: a .bat batch script.
func createNonACPAgent(t *testing.T, dir string) {
	t.Helper()
	script := "@echo off\r\necho not-json\r\nexit /b 1\r\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "opencode.bat"), []byte(script), 0o644))
}
