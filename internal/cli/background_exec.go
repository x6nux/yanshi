package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
)

// spawnedProcess adapts *os.Process to BackgroundProcess.
type spawnedProcess struct{ p *os.Process }

func (s spawnedProcess) Pid() int { return s.p.Pid }
func (s spawnedProcess) Wait() error {
	_, err := s.p.Wait()
	return err
}

// spawnDetached starts `yanshi serve` in its own session, with its output
// going to logPath and its stdin at /dev/null.
//
// Environment: INHERITED DELIBERATELY. The child is yanshi itself, and it must
// read the operator's YANSHI_* variables and — the usual reason it cannot be
// scrubbed — the ${VAR} API keys its own config expands. Scrubbing here would
// start a daemon that boots with no credentials and looks like a provider
// outage. Nothing flows the other way: the child writes to the log file and
// the lockfile, and this process does not read its stdout.
//
// The child gets no stdin. A detached process that inherits a terminal it no
// longer owns will block on a read nobody will complete; /dev/null makes that
// an immediate EOF instead.
func spawnDetached(exe string, args []string, logPath, root string, stdin io.Reader) (BackgroundProcess, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log %s: %w", logPath, err)
	}
	// The file handle is deliberately NOT closed: it is the child's stdout and
	// stderr for as long as the child lives, and closing it here would close
	// the child's own descriptor. The OS releases it when this process exits.
	cmd := exec.Command(exe, args...)
	cmd.Dir = root
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if stdin == nil {
		devNull, err := os.Open(os.DevNull)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", os.DevNull, err)
		}
		cmd.Stdin = devNull
	} else {
		cmd.Stdin = stdin
	}
	cmd.SysProcAttr = detachedProcessAttr()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return spawnedProcess{p: cmd.Process}, nil
}
