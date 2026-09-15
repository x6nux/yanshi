package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/x6nux/yanshi/internal/ipc"
)

// startFakeDaemon serves a JSON-RPC socket for one project root and answers
// every request with a fixed result. It is enough to exercise the client: what
// is under test is the client's framing, flag handling and drain behaviour, not
// the app-server's dispatch (which internal/appserver tests cover).
func startFakeDaemon(t *testing.T, root, result string) {
	t.Helper()
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
				line := sc.Bytes()
				var req struct {
					ID json.RawMessage `json:"id"`
				}
				if json.Unmarshal(line, &req) != nil || len(req.ID) == 0 {
					continue
				}
				resp := `{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":` + result + `}`
				if _, err := c.Write(append([]byte(resp), '\n')); err != nil {
					return
				}
			}
		})
	}()
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		<-done
	})
}

func runIPCClient(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := runIPC(args, strings.NewReader(stdin), &out, &errBuf)
	return code, out.String(), errBuf.String()
}

// TestIPCClientOneShotCallAcceptsFlagsAfterTheMethod pins both the happy path
// and the flag placement that stdlib flag parsing broke: `yanshi ipc initialize
// -root DIR` used to print usage and exit 2 because FlagSet stops at the first
// positional argument.
func TestIPCClientOneShotCallAcceptsFlagsAfterTheMethod(t *testing.T) {
	root := t.TempDir()
	startFakeDaemon(t, root, `{"version":"v1"}`)

	code, out, errs := runIPCClient(t, "", "initialize", "-root", root)
	if code != exitOK {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, out, errs)
	}
	var res struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &res); err != nil {
		t.Fatalf("stdout is not the result JSON: %v (%q)", err, out)
	}
	if res.Version != "v1" {
		t.Fatalf("result = %+v", res)
	}
}

// TestIPCClientBridgeDrainsResponsesAfterStdinEOF is the bug the first bridge
// shipped with: it returned as soon as stdin hit EOF, so the responses to what
// had just been sent were dropped and `printf … | yanshi ipc` exited 0 with no
// output.
func TestIPCClientBridgeDrainsResponsesAfterStdinEOF(t *testing.T) {
	root := t.TempDir()
	startFakeDaemon(t, root, `{"ok":true}`)

	reqs := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"capabilities\"}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"initialize\"}\n"
	code, out, errs := runIPCClient(t, reqs, "-root", root)
	if code != exitOK {
		t.Fatalf("exit=%d stderr=%q", code, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d response lines, want 2: %q", len(lines), out)
	}
	for i, ln := range lines {
		if !strings.Contains(ln, `"id":`+itoa(i+1)) {
			t.Fatalf("line %d does not answer request %d: %q", i, i+1, ln)
		}
	}
}

// TestIPCClientErrorsWhenNoDaemonIsListening proves the failure names the next
// action instead of surfacing a bare dial error — the common case for a user
// who has not started a daemon yet.
func TestIPCClientErrorsWhenNoDaemonIsListening(t *testing.T) {
	code, _, errs := runIPCClient(t, "", "capabilities", "-root", t.TempDir())
	if code != exitErr {
		t.Fatalf("exit=%d, want exitErr", code)
	}
	if !strings.Contains(errs, "yanshi -b") {
		t.Fatalf("stderr does not point at starting a daemon: %q", errs)
	}
}

// TestIPCClientUsageAndHelp covers the three inputs that must not dial: an
// unknown flag, too many positionals, and -h.
func TestIPCClientUsageAndHelp(t *testing.T) {
	if code, _, _ := runIPCClient(t, "", "-nope"); code != exitUsage {
		t.Errorf("unknown flag: exit=%d, want exitUsage", code)
	}
	if code, _, _ := runIPCClient(t, "", "a", "b"); code != exitUsage {
		t.Errorf("two positionals: exit=%d, want exitUsage", code)
	}
	if code, out, _ := runIPCClient(t, "", "-h"); code != exitOK || !strings.Contains(out, "usage: yanshi ipc") {
		t.Errorf("-h: exit=%d out=%q", code, out)
	}
	if code, _, _ := runIPCClient(t, "", "capabilities", "-params", "{not json}"); code != exitUsage {
		t.Errorf("bad -params: exit=%d, want exitUsage", code)
	}
}

// TestIPCClientTimeoutIsValidatedEarly keeps a typo in -timeout a usage error
// rather than a call that silently runs unbounded.
func TestIPCClientTimeoutIsValidatedEarly(t *testing.T) {
	if code, _, _ := runIPCClient(t, "", "capabilities", "-timeout", "soon"); code != exitUsage {
		t.Fatalf("exit=%d, want exitUsage", code)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

var _ = time.Second
