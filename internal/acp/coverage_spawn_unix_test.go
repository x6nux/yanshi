//go:build !windows

package acp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// createNonACPAgent places a fake "opencode" on PATH that starts but does not
// speak ACP (prints non-JSON and exits), so Spawn's Initialize step fails and
// the cleanup path is exercised. Posix form: an executable shell script.
func createNonACPAgent(t *testing.T, dir string) {
	t.Helper()
	script := "#!/bin/sh\necho not-json\nexit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755))
}
