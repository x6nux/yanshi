//go:build e2e_live

package main

import (
	"os/exec"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/ipc"
)

// buildCmd is exec.Command for the live suite (kept separate from runIn so a
// test can attach its own stdin/stdout).
func buildCmd(t *testing.T, dir, bin string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	return cmd
}

// socketPathForTest resolves the socket the daemon should have created.
func socketPathForTest(root string) (string, error) {
	return ipc.SocketPath(root)
}

// sleepShort is the live suite's poll interval.
func sleepShort() { time.Sleep(100 * time.Millisecond) }
