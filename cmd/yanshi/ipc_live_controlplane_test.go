//go:build e2e_live

// Live end-to-end test for the operator control plane over IPC: the methods
// that let a script see and change the project's state through the socket.
//
// Run with:
//
//	go test -tags e2e_live ./cmd/yanshi -run TestLiveIPCControlPlane -v -timeout 10m
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLiveIPCControlPlane starts a daemon and drives the control-plane verbs
// both ways: through the generic `yanshi ipc <method>` client and through the
// dedicated CLI verbs, which must produce the same answers.
func TestLiveIPCControlPlane(t *testing.T) {
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

	// 1. The control methods are advertised by the running daemon.
	code, out, errs := runIn(t, work, bin, "ipc", "capabilities", "-root", work)
	if code != 0 {
		t.Fatalf("capabilities: exit=%d stderr=%q", code, errs)
	}
	for _, want := range []string{"session/list", "skills/list", "jobs/list", "vcs/log", "features/list"} {
		if !strings.Contains(out, want) {
			t.Errorf("capabilities does not advertise %s: %s", want, out)
		}
	}

	// 2. session/list answers over the socket (no sessions yet, but a list).
	code, out, errs = runIn(t, work, bin, "ipc", "session/list", "-params", `{"limit":5}`, "-root", work)
	if code != 0 {
		t.Fatalf("session/list: exit=%d stderr=%q", code, errs)
	}
	var sessions struct {
		Sessions []any `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &sessions); err != nil {
		t.Fatalf("session/list result is not JSON: %v (%q)", err, out)
	}

	// 3. features/list returns the runtime flag table, and features/set changes
	// it — the pair a script uses to flip a flag without a TUI.
	code, out, errs = runIn(t, work, bin, "ipc", "features/list", "-root", work)
	if code != 0 {
		t.Fatalf("features/list: exit=%d stderr=%q", code, errs)
	}
	var flags struct {
		Features []struct {
			Key     string `json:"key"`
			Enabled bool   `json:"enabled"`
		} `json:"features"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &flags); err != nil {
		t.Fatalf("features/list is not JSON: %v (%q)", err, out)
	}
	if len(flags.Features) == 0 {
		t.Fatal("features/list returned no flags")
	}
	key := flags.Features[0].Key
	want := !flags.Features[0].Enabled
	setParams, _ := json.Marshal(map[string]any{"key": key, "enabled": want})
	if code, out, errs := runIn(t, work, bin, "ipc", "features/set", "-params", string(setParams), "-root", work); code != 0 {
		t.Fatalf("features/set: exit=%d stdout=%q stderr=%q", code, out, errs)
	}
	_, out, _ = runIn(t, work, bin, "ipc", "features/list", "-root", work)
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &flags); err != nil {
		t.Fatalf("features/list after set: %v", err)
	}
	for _, f := range flags.Features {
		if f.Key == key && f.Enabled != want {
			t.Fatalf("features/set did not take: %s is %v, want %v", key, f.Enabled, want)
		}
	}

	// 4. The dedicated CLI verbs reach the same daemon through the same socket.
	for _, verb := range [][]string{
		{"skills", "list"},
		{"features", "list"},
		{"approvals", "list"},
		{"jobs", "list"},
		{"mcp", "list"},
		{"vcs", "log", "-limit", "2"},
		{"models", "list"},
	} {
		cmd := buildCmd(t, work, bin, verb...)
		cmd.Env = append(os.Environ(), "YANSHI_ROOT="+work)
		var outBuf, errBuf strings.Builder
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			t.Errorf("%v: %v (stderr=%s)", verb, err, errBuf.String())
		}
	}
}

// TestLiveIPCDeleteNeedsConfirmation pins the one irreversible method over the
// real transport: the guard is in the protocol, not only in the CLI.
func TestLiveIPCDeleteNeedsConfirmation(t *testing.T) {
	bin := buildLiveBinary(t)
	work := t.TempDir()
	cfg := filepath.Join(work, "config.yaml")
	body := "server:\n  http_addr: \"127.0.0.1:0\"\nstorage:\n  sqlite_path: \"" +
		filepath.ToSlash(filepath.Join(work, "yanshi.db")) + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Cleanup(func() { _, _, _ = runIn(t, work, bin, "daemon", "stop", "-root", work) })
	code0, out0, errs0 := runIn(t, work, bin, "-b", "-config", cfg, "-fake-model", "-json", "-wait", "90s")
	if code0 != 0 {
		t.Fatalf("start daemon: stderr=%q", errs0)
	}
	cleanDaemonLog(t, out0)
	code, _, errs := runIn(t, work, bin, "ipc", "session/delete", "-params", `{"id":"nope"}`, "-root", work)
	if code == 0 {
		t.Fatal("session/delete without confirm must fail")
	}
	if !strings.Contains(errs, "confirm") {
		t.Fatalf("error does not mention the confirm field: %q", errs)
	}
}
