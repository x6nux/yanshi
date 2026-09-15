//go:build e2e_live

package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// cleanDaemonLog removes the log file a `-b` start reported.
//
// The daemon's log is meant to OUTLIVE the daemon — that is what makes it
// useful after a crash — but a test's daemon is not a project: its root is a
// t.TempDir() that vanishes, and the log would sit in the operator's shared run
// directory forever (measured: several TestLiveBackgroundDaemonLifecycle logs
// had accumulated there, next to the real daemons' lockfiles). `daemon stop`
// removes the lockfile and the socket and deliberately leaves the log, so the
// test has to do this.
func cleanDaemonLog(t *testing.T, startOutput string) {
	t.Helper()
	var res struct {
		Log string `json:"log"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(startOutput)), &res); err != nil {
		return
	}
	if res.Log == "" {
		return
	}
	t.Cleanup(func() { _ = os.Remove(res.Log) })
}
