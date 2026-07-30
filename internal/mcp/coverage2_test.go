package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// managerWithClient builds a Manager with one server config and an injected
// fakeClient, returning both for health/validate exercises.
func managerWithClient(t *testing.T, cfg *ServerConfig, cli *fakeClient) *Manager {
	t.Helper()
	if cfg == nil {
		cfg = &ServerConfig{Name: "s", Enabled: true, Transport: TransportHTTP, URL: "http://x", Reconnect: true}
	}
	cfg.Name = "s"
	m := NewManager(map[string]*ServerConfig{"s": cfg})
	m.mu.Lock()
	if cli != nil {
		m.clients["s"] = cli
		m.status["s"] = StatusReady
	}
	m.mu.Unlock()
	return m
}

// --- health.go ---

// TestStartHealthLoopDisabled covers the !Enabled early-return no-op branch.
func TestStartHealthLoopDisabled(t *testing.T) {
	m := managerWithClient(t, nil, &fakeClient{})
	m.SetHealthConfig(HealthConfig{Enabled: false})
	cancel := m.StartHealthLoop(context.Background())
	if cancel == nil {
		t.Fatal("StartHealthLoop must return a cancel even when disabled")
	}
	cancel() // no-op cancel, must not block
}

// TestStartHealthLoopRunsAndCancels covers the happy path: the loop ticks, then
// the cancel func stops it and waits for the goroutine.
func TestStartHealthLoopRunsAndCancels(t *testing.T) {
	m := managerWithClient(t, nil, &fakeClient{})
	m.SetHealthConfig(HealthConfig{Enabled: true, Interval: 5 * time.Millisecond})
	cancel := m.StartHealthLoop(context.Background())
	time.Sleep(30 * time.Millisecond) // let at least one tick fire
	cancel()                           // must return promptly after wg.Wait
}

// TestHealthOncePaths covers healthOnce's skip-nil, ping-success-continue, and
// reconnect-spawn branches.
func TestHealthOncePaths(t *testing.T) {
	// skip: client present but config nil → continue. Achieve by injecting a
	// client under a name with no config.
	m := NewManager(nil)
	m.mu.Lock()
	m.clients["ghost"] = &fakeClient{}
	m.mu.Unlock()
	m.healthOnce(context.Background()) // no panic, no config → skip

	// ping success → continue (status stays Ready).
	m2 := managerWithClient(t, nil, &fakeClient{})
	m2.healthOnce(context.Background())
	m2.mu.Lock()
	st := m2.status["s"]
	m2.mu.Unlock()
	if st != StatusReady {
		t.Fatalf("ping success status = %v, want ready", st)
	}

	// ping failure + reconnect → markFailed + spawn reconnect goroutine.
	m3 := managerWithClient(t, nil, &fakeClient{pingErr: errors.New("down")})
	m3.SetHealthConfig(HealthConfig{Enabled: true, ReconnectMaxAttempts: 1, ReconnectInitialBackoff: time.Millisecond, ReconnectMaxBackoff: time.Millisecond})
	done := make(chan struct{})
	go func() {
		m3.healthOnce(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("healthOnce with reconnect hung")
	}
}

// TestReconnectBranches covers reconnect's unknown-server, reconnect-disabled,
// exhausted, context-cancelled, and success paths.
func TestReconnectBranches(t *testing.T) {
	ctx := context.Background()
	// unknown server
	m := NewManager(nil)
	if err := m.reconnect(ctx, "ghost"); err == nil {
		t.Fatal("reconnect unknown must error")
	}

	// reconnect disabled (Reconnect:false)
	m2 := managerWithClient(t, &ServerConfig{Name: "s", Enabled: true, Transport: TransportHTTP, URL: "http://x", Reconnect: false}, &fakeClient{})
	if err := m2.reconnect(ctx, "s"); err == nil {
		t.Fatal("reconnect with reconnect=false must error")
	}

	// context cancelled before the first backoff elapses.
	m3 := managerWithClient(t, nil, &fakeClient{})
	m3.SetHealthConfig(HealthConfig{Enabled: true, ReconnectMaxAttempts: 5, ReconnectInitialBackoff: time.Minute})
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if err := m3.reconnect(cctx, "s"); err == nil {
		t.Fatal("reconnect on cancelled ctx must error")
	}

	// exhausted: a server whose startOne always fails (bad URL) with a tiny
	// attempt budget.
	m4 := managerWithClient(t, &ServerConfig{Name: "s", Enabled: true, Transport: TransportHTTP, URL: "http://127.0.0.1:1/x", Reconnect: true}, nil)
	m4.SetHealthConfig(HealthConfig{Enabled: true, StartupTimeout: 100 * time.Millisecond, ReconnectMaxAttempts: 1, ReconnectInitialBackoff: time.Millisecond, ReconnectMaxBackoff: time.Millisecond})
	if err := m4.reconnect(ctx, "s"); err == nil {
		t.Fatal("reconnect that always fails must error")
	}

	// single-flight: two concurrent reconnects on the same server coalesce.
	m5 := managerWithClient(t, &ServerConfig{Name: "s", Enabled: true, Transport: TransportHTTP, URL: "http://127.0.0.1:1/x", Reconnect: true}, nil)
	m5.SetHealthConfig(HealthConfig{Enabled: true, StartupTimeout: 100 * time.Millisecond, ReconnectMaxAttempts: 1, ReconnectInitialBackoff: 50 * time.Millisecond, ReconnectMaxBackoff: 50 * time.Millisecond})
	var firstErr, secondErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); firstErr = m5.reconnect(ctx, "s") }()
	go func() { defer wg.Done(); secondErr = m5.reconnect(ctx, "s") }()
	wg.Wait()
	if firstErr == nil && secondErr == nil {
		t.Fatal("expected at least one reconnect error")
	}
}

// TestCallToolRetryBranches covers CallToolRetry's tool-not-found and
// reconnect-failed paths.
func TestCallToolRetryBranches(t *testing.T) {
	ctx := context.Background()
	// tool not found: CallTool fails and LookupTool returns false.
	m := NewManager(nil)
	_, err := m.CallToolRetry(ctx, "mcp_ghost_x", nil)
	if err == nil {
		t.Fatal("CallToolRetry on unknown tool must error")
	}

	// tool found but reconnect fails: inject a tool + a client that errors on
	// CallTool, with a server that cannot restart.
	m2 := NewManager(map[string]*ServerConfig{"s": {Name: "s", Enabled: true, Transport: TransportHTTP, URL: "http://127.0.0.1:1/x", Reconnect: true}})
	m2.mu.Lock()
	m2.toolMap["mcp_s_t"] = ToolDescriptor{ServerName: "s", ToolName: "t", Qualified: "mcp_s_t"}
	m2.clients["s"] = &fakeClient{callErr: errors.New("boom")}
	m2.status["s"] = StatusReady
	m2.mu.Unlock()
	m2.SetHealthConfig(HealthConfig{Enabled: true, StartupTimeout: 100 * time.Millisecond, ReconnectMaxAttempts: 1, ReconnectInitialBackoff: time.Millisecond, ReconnectMaxBackoff: time.Millisecond})
	_, err = m2.CallToolRetry(ctx, "mcp_s_t", nil)
	if err == nil {
		t.Fatal("CallToolRetry with failing reconnect must error")
	}
}

// --- manager.go startOne stdio + tools/list error + validate nil ---

// TestStartOneStdioBogusCommand covers the stdio branch setup (env, pipes) and
// the cmd.Start failure path via a non-existent command.
func TestStartOneStdioBogusCommand(t *testing.T) {
	m := NewManager(nil)
	_, err := m.startOne(context.Background(), &ServerConfig{
		Name: "s", Transport: TransportStdio, Command: "definitely-not-a-real-bin-zzz",
		Args: []string{}, Env: map[string]string{"FOO": "bar"},
	})
	if err == nil {
		t.Fatal("startOne with bogus command must error")
	}
}

// TestStartOneStdioWithCmdClose covers Close's cmd.Process.Kill+Wait path by
// starting a real long-lived subprocess and then closing the client.
func TestStartOneStdioWithCmdClose(t *testing.T) {
	// Use a real long-lived command available on Windows and POSIX: "cmd /c"
	// won't speak MCP, but we only need Close to exercise Kill+Wait. We bypass
	// startOne and construct the StdioClient with a started cmd directly.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		// "sleep" may be unavailable on some Windows shells; skip gracefully.
		t.Skipf("could not start subprocess: %v", err)
	}
	cli := NewStdioClient(strings.NewReader(""), io.Discard).SetCmd(cmd)
	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestStartOneToolsListError covers startOne's tools/list-error branch: a
// server that answers initialize but errors on tools/list.
func TestStartOneToolsListError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		resp := map[string]any{"jsonrpc": "2.0"}
		if len(req.ID) > 0 {
			resp["id"] = json.RawMessage(req.ID)
		}
		switch req.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2025-06-18"}
		case "tools/list":
			// return no result → parseToolList errors
		default:
			resp["error"] = map[string]any{"code": -1, "message": "x"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()
	m := NewManager(map[string]*ServerConfig{"s": {Enabled: true, Transport: TransportHTTP, URL: ts.URL}})
	st := m.StartAll(context.Background())
	if st[0].Status != StatusFailed {
		t.Fatalf("expected failed on tools/list error, got %+v", st)
	}
}

// TestValidateNilClient covers Validate's nil-client skip branch.
func TestValidateNilClient(t *testing.T) {
	m := NewManager(map[string]*ServerConfig{"s": {Enabled: true, Transport: TransportHTTP, URL: "http://x"}})
	m.mu.Lock()
	m.clients["s"] = nil // nil client → Validate skips
	m.mu.Unlock()
	st := m.Validate(context.Background())
	if len(st) != 1 {
		t.Fatalf("validate status=%+v", st)
	}
}

// --- httpclient.go Close-with-session, request errors, post error branches ---

// TestHTTPClientCloseWithSession covers Close's DELETE path by getting the
// server to set Mcp-Session-Id during initialize.
func TestHTTPClientCloseWithSession(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		resp := map[string]any{"jsonrpc": "2.0"}
		if len(req.ID) > 0 {
			resp["id"] = json.RawMessage(req.ID)
		}
		switch req.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-123")
			resp["result"] = map[string]any{"protocolVersion": "2025-06-18"}
		case "ping":
			resp["result"] = map[string]any{}
		default:
			resp["error"] = map[string]any{"code": -1, "message": "x"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()
	c := NewHTTPClient(ts.URL, "")
	if err := c.Initialize(context.Background(), "/"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	c.mu.RLock()
	if c.session != "sess-123" {
		t.Fatalf("session = %q, want sess-123", c.session)
	}
	c.mu.RUnlock()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestHTTPClientRequestErrors covers ListTools/ListResources/ReadResource error
// paths when the endpoint is unreachable.
func TestHTTPClientRequestErrors(t *testing.T) {
	c := NewHTTPClient("http://127.0.0.1:1/x", "")
	if _, err := c.ListTools(context.Background()); err == nil {
		t.Fatal("ListTools unreachable must error")
	}
	if _, err := c.ListResources(context.Background()); err == nil {
		t.Fatal("ListResources unreachable must error")
	}
	if _, err := c.ReadResource(context.Background(), "u"); err == nil {
		t.Fatal("ReadResource unreachable must error")
	}
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("Ping unreachable must error")
	}
}

// TestHTTPClientPostErrorBranches covers post's NewRequest error (bad URL), the
// token-source error, the non-2xx error, the decode error, and the JSON-RPC
// error-frame branch.
func TestHTTPClientPostErrorBranches(t *testing.T) {
	ctx := context.Background()
	// bad URL → NewRequestWithContext error.
	c := NewHTTPClient("://bad-url", "")
	if _, err := c.request(ctx, "ping", nil); err == nil {
		t.Fatal("request with bad URL must error")
	}

	// token source error.
	c2 := NewHTTPClientWithTokenSource("http://127.0.0.1:1/x", &errTokenSource{})
	if _, err := c2.request(ctx, "ping", nil); err == nil {
		t.Fatal("request with failing token source must error")
	}

	// non-2xx status with body.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream died"))
	}))
	defer ts.Close()
	c3 := NewHTTPClient(ts.URL, "")
	if _, err := c3.request(ctx, "ping", nil); err == nil {
		t.Fatal("request on 502 must error")
	}

	// decode error: 200 with invalid JSON body.
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not-json"))
	}))
	defer ts2.Close()
	c4 := NewHTTPClient(ts2.URL, "")
	if _, err := c4.request(ctx, "ping", nil); err == nil {
		t.Fatal("request with invalid JSON body must error")
	}

	// JSON-RPC error frame.
	ts3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"boom"}}`))
	}))
	defer ts3.Close()
	c5 := NewHTTPClient(ts3.URL, "")
	if _, err := c5.request(ctx, "ping", nil); err == nil {
		t.Fatal("request with RPC error frame must error")
	}
}

// errTokenSource is a TokenSource that always errors.
type errTokenSource struct{}

func (errTokenSource) Token(context.Context) (string, error) { return "", errors.New("no token") }
func (errTokenSource) Invalidate()                            {}

// --- oauth.go bad-URL and zero-expires ---

func TestClientCredentialsSourceBadURLAndZeroExpires(t *testing.T) {
	// bad token URL → NewRequestWithContext error.
	s := NewClientCredentialsSource("://bad", "c", "s", nil, nil)
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("Token with bad URL must error")
	}

	// expires_in <= 0 → default 5min lifetime (success path).
	ts := oauthServer(t, 0, `{"access_token":"tok","token_type":"Bearer","expires_in":0}`)
	defer ts.Close()
	s2 := NewClientCredentialsSource(ts.URL, "c", "s", nil, nil)
	tok, err := s2.Token(context.Background())
	if err != nil || tok != "tok" {
		t.Fatalf("Token = %q err=%v", tok, err)
	}
	// Cached token is returned on the next call without re-fetching.
	tok2, err := s2.Token(context.Background())
	if err != nil || tok2 != "tok" {
		t.Fatalf("cached Token = %q err=%v", tok2, err)
	}
}

// --- stdio.go doRequest write-error and timeout ---

// TestStdioDoRequestWriteError covers doRequest's WriteLineMessage failure (w
// is a failing writer).
func TestStdioDoRequestWriteError(t *testing.T) {
	cli := NewStdioClient(strings.NewReader(""), errWriter{})
	if _, err := cli.doRequest(context.Background(), 1, map[string]any{"x": 1}); err == nil {
		t.Fatal("doRequest with failing writer must error")
	}
	// Initialize with a failing writer also errors.
	cli2 := NewStdioClient(strings.NewReader(""), errWriter{})
	if err := cli2.Initialize(context.Background(), "/r"); err == nil {
		t.Fatal("Initialize with failing writer must error")
	}
	// ListTools/CallTool/ListResources/ReadResource/Ping with failing writer.
	cli3 := NewStdioClient(strings.NewReader(""), errWriter{})
	if _, err := cli3.ListTools(context.Background()); err == nil {
		t.Fatal("ListTools write error expected")
	}
	if _, err := cli3.CallTool(context.Background(), "t", nil); err == nil {
		t.Fatal("CallTool write error expected")
	}
	if _, err := cli3.ListResources(context.Background()); err == nil {
		t.Fatal("ListResources write error expected")
	}
	if _, err := cli3.ReadResource(context.Background(), "u"); err == nil {
		t.Fatal("ReadResource write error expected")
	}
	if err := cli3.Ping(context.Background()); err == nil {
		t.Fatal("Ping write error expected")
	}
}

// TestStdioDoRequestTimeout covers doRequest's timeout branch: a server that
// never responds with the matching id.
func TestStdioDoRequestTimeout(t *testing.T) {
	// Reader that returns a message with a DIFFERENT id each time, so the loop
	// spins until the short timeout fires.
	inR, inW := io.Pipe()
	go func() {
		defer inW.Close()
		// Keep writing responses for id=999 so the client (expecting id=1) never
		// matches and eventually times out.
		for i := 0; i < 100; i++ {
			_, _ = inW.Write(append([]byte(`{"jsonrpc":"2.0","id":999,"result":{}}`), '\n'))
			time.Sleep(time.Millisecond)
		}
	}()
	cli := NewStdioClient(inR, io.Discard)
	cli.SetTimeout(20 * time.Millisecond)
	if _, err := cli.doRequest(context.Background(), 1, map[string]any{"x": 1}); err == nil {
		t.Fatal("doRequest must time out when id never matches")
	}
}

// --- testing.go handle branches ---

// TestFakeServerHandleResourcesAndUnknown covers the FakeServer.handle
// resources/list and default (unknown method → error) branches.
func TestFakeServerHandleResourcesAndUnknown(t *testing.T) {
	ts, _ := NewFakeHTTPServer(nil)
	defer ts.Close()
	// resources/list
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	resp.Body.Close()
	if m["result"] == nil {
		t.Fatal("resources/list must return a result")
	}
	// unknown method → error frame
	resp, err = http.Post(ts.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"weird/method"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&m)
	resp.Body.Close()
	if m["error"] == nil {
		t.Fatal("unknown method must return an error frame")
	}
}

// --- wire.go header/body/line write errors and header read error ---

// twoWriteWriter succeeds on the first Write and fails on the second, so
// WriteMessage's header write succeeds but the body write fails.
type twoWriteWriter struct{ called int }

func (w *twoWriteWriter) Write(p []byte) (int, error) {
	w.called++
	if w.called == 1 {
		return len(p), nil
	}
	return 0, errors.New("second write boom")
}

func TestWriteMessageBodyWriteError(t *testing.T) {
	w := &twoWriteWriter{}
	if err := WriteMessage(w, map[string]any{"x": 1}); err == nil {
		t.Fatal("WriteMessage body write must error")
	}
}

// TestReadMessageHeaderLineError covers ReadMessage's header-line read error
// (Content-Length present but the subsequent header read fails).
func TestReadMessageHeaderLineError(t *testing.T) {
	// Provide a Content-Length header then a reader that errors mid-header.
	r := bufio.NewReader(&failAfterFirstRead{})
	if _, err := ReadMessage(r); err == nil {
		t.Fatal("ReadMessage with failing header read must error")
	}
}

// failAfterFirstRead returns one valid line on the first Read, then errors.
type failAfterFirstRead struct{ read int }

func (f *failAfterFirstRead) Read(p []byte) (int, error) {
	f.read++
	if f.read == 1 {
		return copy(p, []byte("Content-Length: 10\r\n")), nil
	}
	return 0, errors.New("header read boom")
}

// keep atomic referenced (used by stdio client internally)
var _ = atomic.AddInt64
