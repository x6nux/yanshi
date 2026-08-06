//go:build !unix

package shell

import "os/exec"

// setProcessGroup is a no-op on platforms without a reviewed process-group
// story. CanKillTreeOnPlatform reports false there, so nothing promises a tree
// kill that this would have to deliver.
func setProcessGroup(*exec.Cmd) {}

// killProcessTree kills the direct child only. Matching the capability bit is
// the point: a platform that reports CanKillTree=false must not quietly do
// something half-way that callers cannot reason about.
func killProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
