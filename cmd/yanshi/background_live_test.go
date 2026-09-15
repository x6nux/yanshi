//go:build e2e_live

// Live end-to-end test for `yanshi -b` (background daemon).
//
// It builds the real binary and drives it as a user would, because the parts
// that break are exactly the parts an in-process test cannot reach: detaching
// the child from the terminal, the child writing its own lockfile with the port
// it ACTUALLY bound, and a second `-b` finding that daemon instead of starting
// a rival one.
//
// Run with:
//
//	go test -tags e2e_live ./cmd/yanshi -run TestLiveBackground -v -timeout 10m
//
// No provider or API key is needed: the daemon boots with -fake-model.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildLiveBinary compiles ./cmd/yanshi into a temp dir and returns its path.
func buildLiveBinary(t *testing.T) string {
	t.Helper()
	root := moduleRootForTest(t)
	bin := filepath.Join(t.TempDir(), "yanshi-e2e")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/yanshi")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// moduleRootForTest walks up from the test's cwd until it finds go.mod.
func moduleRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// backgroundDaemonResult mirrors the JSON `yanshi -b -json` prints.
type backgroundDaemonResult struct {
	Started        bool   `json:"started"`
	AlreadyRunning bool   `json:"alreadyRunning"`
	PID            int    `json:"pid"`
	Addr           string `json:"addr"`
	Log            string `json:"log"`
	Root           string `json:"root"`
}

func runIn(t *testing.T, dir, bin string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	var out, errs strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errs
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("%s %s: %v", bin, strings.Join(args, " "), err)
	}
	return code, out.String(), errs.String()
}

// TestLiveBackgroundDaemonLifecycle is the whole feature in one test: start,
// be idempotent, be reachable, stop.
func TestLiveBackgroundDaemonLifecycle(t *testing.T) {
	bin := buildLiveBinary(t)
	work := t.TempDir()
	cfg := filepath.Join(work, "config.yaml")
	body := "server:\n  http_addr: \"127.0.0.1:0\"\nstorage:\n  sqlite_path: \"" +
		filepath.ToSlash(filepath.Join(work, "yanshi.db")) + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Cleanup(func() {
		_, _, _ = runIn(t, work, bin, "daemon", "stop", "-root", work)
	})

	// 1. Start. The config asks for port 0, so the ONLY way this succeeds is if
	// the daemon records the port the kernel actually gave it — the regression
	// this test exists for is a lockfile full of "127.0.0.1:0".
	code, out, errs := runIn(t, work, bin, "-b", "-config", cfg, "-fake-model", "-json", "-wait", "90s")
	if code != 0 {
		t.Fatalf("first -b: exit=%d stdout=%q stderr=%q", code, out, errs)
	}
	var first backgroundDaemonResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &first); err != nil {
		t.Fatalf("first -b output is not one JSON object: %v (%q)", err, out)
	}
	if !first.Started || first.AlreadyRunning {
		t.Fatalf("first -b = %+v, want started", first)
	}
	if first.PID == 0 || first.Addr == "" || strings.HasSuffix(first.Addr, ":0") {
		t.Fatalf("first -b reported pid=%d addr=%q; addr must be the bound port", first.PID, first.Addr)
	}
	if first.Log == "" {
		t.Fatalf("first -b reported no log path: %+v", first)
	}
	// The log outlives the daemon by design, so the TEST removes it: a temp
	// project root leaves nothing to revisit the file, and the run directory is
	// shared with the operator's real daemons.
	t.Cleanup(func() { _ = os.Remove(first.Log) })

	// 2. A second `-b` must not start a rival daemon.
	code, out, errs = runIn(t, work, bin, "-b", "-config", cfg, "-fake-model", "-json", "-wait", "30s")
	if code != 0 {
		t.Fatalf("second -b: exit=%d stdout=%q stderr=%q", code, out, errs)
	}
	var second backgroundDaemonResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &second); err != nil {
		t.Fatalf("second -b output is not JSON: %v (%q)", err, out)
	}
	if !second.AlreadyRunning || second.Started {
		t.Fatalf("second -b = %+v, want alreadyRunning", second)
	}
	if second.PID != first.PID {
		t.Fatalf("second -b reported pid %d, want the running daemon %d", second.PID, first.PID)
	}

	// 3. The daemon must outlive the terminal that started it: it answers
	// readiness from a SEPARATE process invocation.
	code, out, errs = runIn(t, work, bin, "daemon", "status", "-root", work, "-json")
	if code != 0 {
		t.Fatalf("daemon status: exit=%d stdout=%q stderr=%q", code, out, errs)
	}
	var status struct {
		Alive bool `json:"alive"`
		Ready bool `json:"ready"`
		PID   int  `json:"pid"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &status); err != nil {
		t.Fatalf("daemon status output is not JSON: %v (%q)", err, out)
	}
	if !status.Alive || !status.Ready {
		t.Fatalf("status = %+v, want alive and ready", status)
	}

	// 4. Stop, and prove the lockfile went with it.
	if code, out, errs := runIn(t, work, bin, "daemon", "stop", "-root", work); code != 0 {
		t.Fatalf("daemon stop: exit=%d stdout=%q stderr=%q", code, out, errs)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, out, _ = runIn(t, work, bin, "daemon", "status", "-root", work, "-json")
		if code != 0 || strings.Contains(out, "\"alive\":false") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon still alive after stop: %s", out)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
