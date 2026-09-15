//go:build e2e_live

// Live end-to-end test for the unix-socket IPC (`yanshi ipc`).
//
// It builds the real binary, starts a daemon with -b, and drives it over the
// socket as a local tool would. The parts that only a real process pair can
// show: the socket actually being created 0600 next to the lockfile, the
// daemon serving the app-server protocol on it, the one-shot client, and the
// stdio bridge draining the responses after its input ends.
//
// Run with:
//
//	go test -tags e2e_live ./cmd/yanshi -run TestLiveIPC -v -timeout 10m
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveIPCSocketDrivesTheDaemon is the whole IPC in one test.
func TestLiveIPCSocketDrivesTheDaemon(t *testing.T) {
	bin := buildLiveBinary(t)
	work := t.TempDir()
	cfg := filepath.Join(work, "config.yaml")
	body := "server:\n  http_addr: \"127.0.0.1:0\"\nstorage:\n  sqlite_path: \"" +
		filepath.ToSlash(filepath.Join(work, "yanshi.db")) + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Cleanup(func() { _, _, _ = runIn(t, work, bin, "daemon", "stop", "-root", work) })

	if code, out, errs := runIn(t, work, bin, "-b", "-config", cfg, "-fake-model", "-json", "-wait", "90s"); code != 0 {
		t.Fatalf("start daemon: exit=%d stdout=%q stderr=%q", code, out, errs)
	} else {
		cleanDaemonLog(t, out)
	}

	// 1. One-shot call over the socket. `-root` AFTER the method is the spelling
	// a user types, and it is the one stdlib flag parsing rejected.
	code, out, errs := runIn(t, work, bin, "ipc", "capabilities", "-root", work)
	if code != 0 {
		t.Fatalf("ipc capabilities: exit=%d stdout=%q stderr=%q", code, out, errs)
	}
	var caps struct {
		Version string   `json:"version"`
		Methods []string `json:"methods"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &caps); err != nil {
		t.Fatalf("capabilities is not JSON: %v (%q)", err, out)
	}
	if caps.Version == "" || len(caps.Methods) == 0 {
		t.Fatalf("capabilities = %+v", caps)
	}

	// 2. initialize must agree with what the HTTP app-server advertises: the
	// socket is a transport, not a second protocol.
	code, out, errs = runIn(t, work, bin, "ipc", "initialize", "-root", work)
	if code != 0 {
		t.Fatalf("ipc initialize: exit=%d stderr=%q", code, errs)
	}
	if !strings.Contains(out, "\"version\":\"v1\"") {
		t.Fatalf("initialize result = %q", out)
	}

	// 3. A real turn over the bridge. Feeding two requests on stdin and reading
	// two answers is what proves the bridge drains after EOF — the first version
	// returned on stdin EOF and printed nothing.
	reqs := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"config/read\",\"params\":{\"key\":\"storage.sqlite_path\"}}\n"
	cmd := buildCmd(t, work, bin, "ipc", "-root", work)
	cmd.Stdin = strings.NewReader(reqs)
	var bridgeOut, bridgeErr strings.Builder
	cmd.Stdout = &bridgeOut
	cmd.Stderr = &bridgeErr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bridge: %v (stderr=%s)", err, bridgeErr.String())
	}
	var ids []float64
	for _, line := range strings.Split(strings.TrimSpace(bridgeOut.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var msg struct {
			ID float64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("bridge line is not JSON: %v (%q)", err, line)
		}
		ids = append(ids, msg.ID)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("bridge answered ids %v, want [1 2] (output=%q)", ids, bridgeOut.String())
	}

	// 4. The socket must be private to the user: 0600 is the whole access
	// control, standing in for the HTTP bearer token.
	sockPath, err := socketPathForTest(work)
	if err != nil {
		t.Fatalf("socket path: %v", err)
	}
	fi, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("socket not found at %s: %v", sockPath, err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want 0600", fi.Mode().Perm())
	}

	// 5. Stopping the daemon must take the socket with it, or the next start
	// would find litter (recoverable, but a leak nonetheless).
	if code, _, errs := runIn(t, work, bin, "daemon", "stop", "-root", work); code != 0 {
		t.Fatalf("daemon stop: exit=%d stderr=%q", code, errs)
	}
	socketGone := false
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sockPath); os.IsNotExist(err) {
			socketGone = true
			break
		}
		sleepShort()
	}
	if !socketGone {
		t.Fatalf("socket %s survived daemon stop", sockPath)
	}
}
