package appserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/x6nux/yanshi/internal/ctl"
	"github.com/x6nux/yanshi/internal/store"
)

// ctlTestServer builds a JSON-RPC server whose control plane reads a temp
// store. Passing a nil agent is deliberate: the operator methods never touch
// the conversation service, and a test that needed one would be testing the
// wrong seam.
func ctlTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.ToSlash(filepath.Join(dir, "test.db"))
	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf("server:\n  http_addr: \"127.0.0.1:0\"\nstorage:\n  sqlite_path: %q\n", dbPath)
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// Seed a session through the store directly: the point of this file is the
	// PROTOCOL surface, and creating the fixture through the surface under test
	// would hide a broken list behind a broken create.
	seed, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("seed store: %v", err)
	}
	defer seed.Close()
	id, err := seed.CreateSession("rpc session")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	svc, err := ctl.Open(cfgPath)
	if err != nil {
		t.Fatalf("ctl.Open: %v", err)
	}
	t.Cleanup(svc.Close)
	return New(nil, nil).WithCtl(svc), id
}

// exchange runs one request through the real Serve loop and returns the parsed
// response.
func exchange(t *testing.T, srv *Server, request string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := srv.Serve(context.Background(), strings.NewReader(request+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	line := strings.TrimSpace(out.String())
	if line == "" {
		t.Fatalf("no response to %s", request)
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, line)
	}
	return msg
}

// TestCtlSessionListOverTheProtocol is the whole point of this file: a client
// that can start a turn can also enumerate sessions, on the same connection
// and through the same framing.
func TestCtlSessionListOverTheProtocol(t *testing.T) {
	srv, id := ctlTestServer(t)
	msg := exchange(t, srv, `{"jsonrpc":"2.0","id":1,"method":"session/list","params":{"limit":5}}`)
	result, ok := msg["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %+v", msg)
	}
	sessions, _ := result["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %+v", result["sessions"])
	}
	first, _ := sessions[0].(map[string]any)
	if first["id"] != id || first["title"] != "rpc session" {
		t.Fatalf("session = %+v", first)
	}
}

// TestCtlSessionDeleteNeedsConfirmation pins the guard on the one irreversible
// method, on the machine surface as well as the human one.
func TestCtlSessionDeleteNeedsConfirmation(t *testing.T) {
	srv, id := ctlTestServer(t)
	msg := exchange(t, srv, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"session/delete","params":{"id":%q}}`, id))
	if msg["error"] == nil {
		t.Fatalf("delete without confirm succeeded: %+v", msg)
	}
	msg = exchange(t, srv, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"session/delete","params":{"id":%q,"confirm":"yes"}}`, id))
	if msg["error"] != nil {
		t.Fatalf("delete with confirm failed: %+v", msg)
	}
}

// TestCtlLiveMethodSaysItNeedsADaemon proves the missing-manager case is
// diagnosable over the wire: an operator reading the error must learn that the
// state lives in a process, not that the feature is absent.
func TestCtlLiveMethodSaysItNeedsADaemon(t *testing.T) {
	srv, _ := ctlTestServer(t)
	msg := exchange(t, srv, `{"jsonrpc":"2.0","id":1,"method":"jobs/list"}`)
	rpcErr, _ := msg["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("expected an error: %+v", msg)
	}
	text, _ := rpcErr["message"].(string)
	if !strings.Contains(text, "yanshi -b") {
		t.Fatalf("error does not point at starting a daemon: %q", text)
	}
}

// TestCtlMethodsAreAdvertised pins that capabilities lists the operator methods
// on BOTH surfaces. A client that discovered the protocol through
// `capabilities` must see the same set as one that used `initialize`, or it
// will never try them.
func TestCtlMethodsAreAdvertised(t *testing.T) {
	srv, _ := ctlTestServer(t)
	for _, method := range []string{"capabilities", "initialize"} {
		msg := exchange(t, srv, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, method))
		result, _ := msg["result"].(map[string]any)
		raw, _ := json.Marshal(result["methods"])
		var methods []string
		_ = json.Unmarshal(raw, &methods)
		found := map[string]bool{}
		for _, m := range methods {
			found[m] = true
		}
		for _, want := range []string{"session/list", "skills/list", "jobs/list", "usage/get", "vcs/log"} {
			if !found[want] {
				t.Errorf("%s does not advertise %s: %v", method, want, methods)
			}
		}
	}
}

// TestUnknownMethodStaysMethodNotFound proves the operator dispatch does not
// swallow the protocol's own error: a genuine typo must still be -32601.
func TestUnknownMethodStaysMethodNotFound(t *testing.T) {
	srv, _ := ctlTestServer(t)
	msg := exchange(t, srv, `{"jsonrpc":"2.0","id":1,"method":"session/nope"}`)
	rpcErr, _ := msg["error"].(map[string]any)
	if rpcErr == nil {
		t.Fatalf("expected an error: %+v", msg)
	}
	if code, _ := rpcErr["code"].(float64); int(code) != -32601 {
		t.Fatalf("code = %v, want -32601: %+v", rpcErr["code"], msg)
	}
}
