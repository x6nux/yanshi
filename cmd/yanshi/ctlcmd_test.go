package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/ipc"
)

// fakeRPC is a daemon stub that answers the operator methods this file tests.
// It speaks the real framing (one JSON-RPC object per line) because the CLI's
// client is what is under test — a stub that skipped the framing would not
// exercise the id matching or the error mapping.
type fakeRPC struct {
	root     string
	handlers map[string]func(params json.RawMessage) (any, string)
	seen     []string
}

func startFakeRPC(t *testing.T, handlers map[string]func(json.RawMessage) (any, string)) *fakeRPC {
	t.Helper()
	root := t.TempDir()
	f := &fakeRPC{root: root, handlers: handlers}
	ln, err := ipc.Listen(root)
	if err != nil {
		t.Fatalf("ipc.Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ipc.Serve(ctx, ln, func(_ context.Context, c net.Conn) {
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				var req struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if json.Unmarshal(sc.Bytes(), &req) != nil {
					continue
				}
				f.seen = append(f.seen, req.Method)
				h, ok := f.handlers[req.Method]
				if !ok {
					_, _ = c.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"error":{"code":-32601,"message":"method not found: ` + req.Method + `"}}` + "\n"))
					continue
				}
				result, errMsg := h(req.Params)
				if errMsg != "" {
					_, _ = c.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"error":{"code":-32602,"message":` + strconvQuote(errMsg) + `}}` + "\n"))
					continue
				}
				blob, _ := json.Marshal(result)
				_, _ = c.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":` + string(blob) + "}\n"))
			}
		})
	}()
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		<-done
	})
	t.Setenv("YANSHI_ROOT", root)
	return f
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func runCtlVerb(t *testing.T, fn func([]string, io.Writer, io.Writer) int, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := fn(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// TestSkillsVerbRendersTheDaemonView drives the real client against a stub and
// pins the human-readable rendering.
func TestSkillsVerbRendersTheDaemonView(t *testing.T) {
	startFakeRPC(t, map[string]func(json.RawMessage) (any, string){
		"skills/list": func(json.RawMessage) (any, string) {
			return map[string]any{"skills": []map[string]any{
				{"name": "greeter", "source": "user", "enabled": true, "trusted": false, "missing": []string{"rg"}},
			}}, ""
		},
	})
	code, out, errs := runCtlVerb(t, runSkills, "list")
	if code != exitOK {
		t.Fatalf("exit=%d stderr=%q", code, errs)
	}
	if !strings.Contains(out, "greeter") || !strings.Contains(out, "missing:rg") {
		t.Fatalf("output = %q", out)
	}
}

// TestCtlVerbsPassThroughJSON proves -json hands the daemon's object through
// verbatim: a re-rendered structure would drop fields a script depends on.
func TestCtlVerbsPassThroughJSON(t *testing.T) {
	startFakeRPC(t, map[string]func(json.RawMessage) (any, string){
		"jobs/list": func(json.RawMessage) (any, string) {
			return map[string]any{"jobs": []map[string]any{
				{"id": "j1", "state": "exited", "command": "make", "exitCode": 0, "outputLen": 12, "extra": "field"},
			}}, ""
		},
	})
	code, out, _ := runCtlVerb(t, runJobs, "list", "-json")
	if code != exitOK {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, `"extra":"field"`) {
		t.Fatalf("json was rebuilt instead of passed through: %q", out)
	}
}

// TestCtlVerbReportsADaemonError pins that a JSON-RPC error is surfaced with
// its message, not swallowed into a success.
func TestCtlVerbReportsADaemonError(t *testing.T) {
	startFakeRPC(t, map[string]func(json.RawMessage) (any, string){
		"features/set": func(json.RawMessage) (any, string) {
			return nil, "unknown feature flag: nope"
		},
	})
	code, _, errs := runCtlVerb(t, runFeatures, "set", "nope", "on")
	if code != exitErr {
		t.Fatalf("exit=%d, want exitErr", code)
	}
	if !strings.Contains(errs, "unknown feature flag") {
		t.Fatalf("stderr = %q", errs)
	}
}

// TestCtlVerbWithoutADaemonPointsAtTheFix is the honest-failure rule at the CLI
// boundary: the state lives in a process, and the error says how to get one.
func TestCtlVerbWithoutADaemonPointsAtTheFix(t *testing.T) {
	t.Setenv("YANSHI_ROOT", t.TempDir()) // a root with no socket
	code, _, errs := runCtlVerb(t, runJobs, "list")
	if code != exitErr {
		t.Fatalf("exit=%d, want exitErr", code)
	}
	if !strings.Contains(errs, "yanshi -b") {
		t.Fatalf("stderr does not point at starting a daemon: %q", errs)
	}
}

// TestMcpManagementVerbDetection pins the one command name that is two things:
// bare `yanshi mcp` runs the stdio server, `yanshi mcp list` manages servers.
func TestMcpManagementVerbDetection(t *testing.T) {
	cases := map[string]bool{
		"list":        true,
		"enable":      true,
		"disable":     true,
		"-config":     false,
		"-fake-model": false,
		"":            false,
	}
	for arg, want := range cases {
		args := []string{}
		if arg != "" {
			args = append(args, arg)
		}
		if got := isMcpManagementVerb(args); got != want {
			t.Errorf("isMcpManagementVerb(%q) = %v, want %v", arg, got, want)
		}
	}
}

// TestUsageVerbIsOffline proves the roll-up needs no daemon: it reads the store.
func TestUsageVerbIsOffline(t *testing.T) {
	cfg, _ := sessionTestEnv(t, "offline usage")
	var out, errBuf bytes.Buffer
	code := runUsage([]string{"-config", cfg, "-json"}, &out, &errBuf)
	if code != exitOK {
		t.Fatalf("exit=%d stderr=%q", code, errBuf.String())
	}
	var usage struct {
		Sessions int `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &usage); err != nil {
		t.Fatalf("output is not JSON: %v (%q)", err, out.String())
	}
	if usage.Sessions != 1 {
		t.Fatalf("sessions = %d, want 1 (the fixture session)", usage.Sessions)
	}
	// And it must work with YANSHI_ROOT unset to a bogus value: usage does not
	// touch the socket.
	t.Setenv("YANSHI_ROOT", filepath.Join(t.TempDir(), "nope"))
	out.Reset()
	errBuf.Reset()
	if code := runUsage([]string{"-config", cfg}, &out, &errBuf); code != exitOK {
		t.Fatalf("usage with no daemon: exit=%d stderr=%q", code, errBuf.String())
	}
}
