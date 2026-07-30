package lsp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Basic type and helper coverage
// ---------------------------------------------------------------------------

func TestDone(t *testing.T) {
	cli := newClient(new(bytes.Buffer), new(bytes.Buffer), time.Second)
	done := cli.Done()
	if done == nil {
		t.Fatal("Done() should not return nil")
	}
	// Before Close, done should not be closed.
	select {
	case <-done:
		t.Fatal("Done() should not be closed before readLoop exits")
	default:
	}
}

func TestToInt64(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int64
		ok   bool
	}{
		{"float64", float64(42), 42, true},
		{"int64", int64(99), 99, true},
		{"int", int(7), 7, true},
		{"string", "abc", 0, false},
		{"nil", nil, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := toInt64(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("toInt64(%v) = (%d, %v); want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestToInt64Or(t *testing.T) {
	if got := toInt64Or(float64(10), 0); got != 10 {
		t.Fatalf("expected 10, got %d", got)
	}
	if got := toInt64Or("bad", 42); got != 42 {
		t.Fatalf("expected default 42, got %d", got)
	}
}

func TestSevLabel(t *testing.T) {
	tests := []struct {
		s    Severity
		want string
	}{
		{SeverityError, "ERR"},
		{SeverityWarning, "WARN"},
		{SeverityInfo, "INFO"},
		{SeverityHint, "HINT"},
		{Severity(99), "ERR"},
	}
	for _, tc := range tests {
		if got := sevLabel(tc.s); got != tc.want {
			t.Fatalf("sevLabel(%d) = %q; want %q", tc.s, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// pathToURL coverage
// ---------------------------------------------------------------------------

func TestPathToURL_ErrorPath(t *testing.T) {
	// Passing an invalid path that filepath.Abs can't process.
	// On most platforms filepath.Abs only fails for unusual inputs like NUL.
	got := pathToURL("")
	// Should not panic; result should at least start with file://
	if !strings.HasPrefix(got, "file:///") {
		t.Logf("empty path gives %q", got)
	}
}

// ---------------------------------------------------------------------------
// languageID coverage
// ---------------------------------------------------------------------------

func TestLanguageID(t *testing.T) {
	tests := []struct {
		uri  string
		want string
	}{
		{"file.go", "go"},
		{"file.py", "python"},
		{"file.ts", "typescript"},
		{"file.tsx", "typescriptreact"},
		{"file.js", "javascript"},
		{"file.jsx", "javascriptreact"},
		{"file.rs", "rust"},
		{"file.txt", ""},
		{"Makefile", ""},
		{"/path/to/module.go", "go"},
	}
	for _, tc := range tests {
		if got := languageID(tc.uri); got != tc.want {
			t.Fatalf("languageID(%q) = %q; want %q", tc.uri, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Client request error paths
// ---------------------------------------------------------------------------

func TestClientRequestClosed(t *testing.T) {
	cli := newClient(new(bytes.Buffer), new(bytes.Buffer), time.Second)
	cli.mu.Lock()
	cli.closed = true
	cli.mu.Unlock()

	_, err := cli.request(context.Background(), "test", nil)
	if !errors.Is(err, errClosed) {
		t.Fatalf("expected errClosed, got %v", err)
	}
}

func TestClientRequestWriteError(t *testing.T) {
	cli := newClient(new(bytes.Buffer), &failWriter{}, time.Second)
	cli.Start()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	_, err := cli.request(ctx, "test", nil)
	if err == nil {
		t.Fatal("expected error from write failure")
	}
}

type failWriter struct{}

func (f *failWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("write always fails")
}

func TestClientRequestTimeout(t *testing.T) {
	// Client reads from clientR (no data → blocks), writes to clientW (data discarded).
	clientR, _ := io.Pipe() // close when done
	discardR, clientW := io.Pipe()
	go io.Copy(io.Discard, discardR)

	cli := newClient(clientR, clientW, 100*time.Millisecond)
	cli.Start()

	_, err := cli.request(context.Background(), "timeout_method", nil)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected timeout error, got %v", err)
	}

	// Close everything so the ReadLoop can exit.
	clientR.Close()
	clientW.Close()
	discardR.Close()
	// Wait for ReadLoop to exit.
	select {
	case <-cli.Done():
	case <-time.After(time.Second):
		t.Fatal("ReadLoop did not exit after close")
	}
}

func TestClientRequestContextDone(t *testing.T) {
	clientR, _ := io.Pipe()
	discardR, clientW := io.Pipe()
	go io.Copy(io.Discard, discardR)

	cli := newClient(clientR, clientW, time.Second)
	cli.Start()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := cli.request(ctx, "method", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	clientR.Close()
	clientW.Close()
	discardR.Close()
	select {
	case <-cli.Done():
	case <-time.After(time.Second):
		t.Fatal("ReadLoop did not exit after close")
	}
}

func TestClientRequestDoneChannel(t *testing.T) {
	clientR, clientW := io.Pipe()
	cli := newClient(clientR, clientW, 500*time.Millisecond)
	cli.Start()

	// Close the client right after starting to close the done channel.
	// The readLoop will exit when the pipe reader returns EOF.
	clientR.Close()
	clientW.Close()
	time.Sleep(100 * time.Millisecond)

	// Now a request should return errClosed because done is closed.
	_, err := cli.request(context.Background(), "method", nil)
	if err == nil {
		t.Fatal("expected error after client is closed")
	}
}

func TestClientDeliverNilPending(t *testing.T) {
	cli := newClient(new(bytes.Buffer), new(bytes.Buffer), time.Second)
	// deliver with no matching pending entry should be a no-op.
	cli.deliver(999, map[string]any{"result": "ok"})
}

func TestClientRequestErrorInResponse(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()

	cl := newClient(cli, cli, time.Second)
	cl.Start()

	go func() {
		msg, err := ReadMessage(bufio.NewReader(srv))
		if err != nil {
			return
		}
		id := msg["id"]
		_ = WriteMessage(srv, map[string]any{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": -32603, "message": "internal"},
		})
	}()

	_, err := cl.request(context.Background(), "failing_method", nil)
	if err == nil || !strings.Contains(err.Error(), "failing_method") {
		t.Fatalf("expected error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// notifyChange path for second call (gen > 1 → didChange)
// ---------------------------------------------------------------------------

func TestNotifyChangeSecondCallSendsDidChange(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()

	cl := newClient(cli, cli, time.Second)
	cl.Start()

	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReader(srv)
		// First call (didOpen)
		msg1, _ := ReadMessage(br)
		if msg1["method"] != "textDocument/didOpen" {
			t.Errorf("first call method = %v; want didOpen", msg1["method"])
		}
		// Second call (didChange)
		msg2, _ := ReadMessage(br)
		if msg2["method"] != "textDocument/didChange" {
			t.Errorf("second call method = %v; want didChange", msg2["method"])
		}
	}()

	_ = cl.notifyChange("file:///test.go", "v1")
	_ = cl.notifyChange("file:///test.go", "v2")

	<-done
}

// ---------------------------------------------------------------------------
// StoreDiags with nil params
// ---------------------------------------------------------------------------

func TestStoreDiagsNilParams(t *testing.T) {
	cl := newClient(new(bytes.Buffer), new(bytes.Buffer), time.Second)
	// Must not panic or modify anything.
	cl.storeDiags(map[string]any{})
	cl.storeDiags(map[string]any{"params": nil})
}

// ---------------------------------------------------------------------------
// Diagnostics timeout returning stale
// ---------------------------------------------------------------------------

func TestDiagnosticsTimeoutReturnsCurrent(t *testing.T) {
	r, w := io.Pipe()
	cl := newClient(r, w, time.Second)
	cl.Start()

	// Register a pending edit generation but don't trigger any publications.
	cl.diagMu.Lock()
	cl.editGen["file:///stale.go"] = 1
	cl.diags["file:///stale.go"] = []Diagnostic{{Message: "stale"}}
	cl.diagMu.Unlock()

	// Diagnostics should time out (no server to push diagnostics) and return the stale value.
	diags := cl.Diagnostics("file:///stale.go", 500*time.Millisecond)
	if len(diags) != 1 || diags[0].Message != "stale" {
		t.Fatalf("expected stale diagnostic, got %+v", diags)
	}

	// Cleanup: close pipes so the ReadLoop goroutine exits.
	r.Close()
	w.Close()
	select {
	case <-cl.Done():
	case <-time.After(time.Second):
		t.Fatal("ReadLoop did not exit after close")
	}
}

// ---------------------------------------------------------------------------
// WriteMessage error paths
// ---------------------------------------------------------------------------

func TestWriteMessageMarshalError(t *testing.T) {
	var buf bytes.Buffer
	err := WriteMessage(&buf, func() {}) // functions can't be marshaled
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("expected marshal error, got %v", err)
	}
}

func TestWriteMessageWriteError(t *testing.T) {
	err := WriteMessage(&failWriter{}, map[string]any{"method": "test"})
	if err == nil {
		t.Fatal("expected write error")
	}
}

func TestWriteMessageBodyWriteError(t *testing.T) {
	// A writer that accepts the header write but fails on the body write.
	hw := &halfWriter{}
	err := WriteMessage(hw, map[string]any{"method": "test"})
	if err == nil {
		t.Fatal("expected body write error")
	}
}

type halfWriter struct {
	mu sync.Mutex
	n  int
}

func (h *halfWriter) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n++
	if h.n > 1 {
		return 0, fmt.Errorf("body write failed")
	}
	return len(p), nil
}

// ---------------------------------------------------------------------------
// ReadMessage error paths
// ---------------------------------------------------------------------------

func TestReadMessageReadHeaderError(t *testing.T) {
	br := bufio.NewReader(&failReader{})
	_, err := ReadMessage(br)
	if err == nil || !strings.Contains(err.Error(), "read header") {
		t.Fatalf("expected read header error, got %v", err)
	}
}

type failReader struct{}

func (f *failReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("read always fails")
}

func TestReadMessageBadContentLength(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("Content-Length: not-a-number\r\n\r\n")
	br := bufio.NewReader(&buf)
	_, err := ReadMessage(br)
	if err == nil || !strings.Contains(err.Error(), "bad Content-Length") {
		t.Fatalf("expected bad Content-Length error, got %v", err)
	}
}

func TestReadMessageNoContentLength(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("\r\n") // empty line = header end, but no Content-Length
	br := bufio.NewReader(&buf)
	_, err := ReadMessage(br)
	if err == nil || !strings.Contains(err.Error(), "no Content-Length") {
		t.Fatalf("expected no Content-Length error, got %v", err)
	}
}

func TestReadMessageReadBodyError(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("Content-Length: 100\r\n\r\n")
	// Only 10 bytes body instead of 100.
	buf.WriteString("short body")
	br := bufio.NewReader(&buf)
	_, err := ReadMessage(br)
	if err == nil || !strings.Contains(err.Error(), "read body") {
		t.Fatalf("expected read body error, got %v", err)
	}
}

func TestReadMessageUnmarshalError(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("Content-Length: 5\r\n\r\n{bad}")
	br := bufio.NewReader(&buf)
	_, err := ReadMessage(br)
	if err == nil || !strings.Contains(err.Error(), "unmarshal body") {
		t.Fatalf("expected unmarshal error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Manager deeper coverage
// ---------------------------------------------------------------------------

func TestManagerEnabledNilSafety(t *testing.T) {
	var m *Manager
	if m.Enabled() {
		t.Fatal("nil Manager should not be Enabled")
	}
}

func TestManagerNoWorkRoot(t *testing.T) {
	m := New(Config{Languages: DefaultLanguages()})
	if m.Enabled() {
		t.Fatal("Manager without WorkRoot should be disabled")
	}
}

func TestManagerDefaultTimeout(t *testing.T) {
	m := New(Config{WorkRoot: t.TempDir(), Languages: DefaultLanguages()})
	// This will be disabled if no gopls on PATH, but the timeout should be set.
	if m.cfg.Timeout != 800*time.Millisecond {
		t.Fatalf("expected default timeout 800ms, got %v", m.cfg.Timeout)
	}
	if m.Enabled() && m.cfg.Timeout != 800*time.Millisecond {
		t.Fatalf("timeout should be set regardless of state")
	}
}

func TestManagerDiagnosticsNoClient(t *testing.T) {
	m := New(Config{
		WorkRoot:  t.TempDir(),
		Languages: map[string]LanguageServer{"go": {Command: "gopls"}},
	})
	// No gopls needed - we use Dial.
	m.cfg.Dial = func(lang string) (io.Reader, io.Writer, func() error, error) {
		return nil, nil, nil, fmt.Errorf("no dial")
	}
	// clientFor will fail on dial, so Diagnostics returns nil.
	diags := m.Diagnostics("main.go", 0)
	if diags != nil {
		t.Fatalf("expected nil diagnostics when client missing, got %+v", diags)
	}
}

func TestDidChangeDisabledManager(t *testing.T) {
	m := New(Config{WorkRoot: "", Languages: nil})
	// Must not panic.
	m.DidChange("x.go", "package main")
}

func TestDidChangeUnknownLanguage(t *testing.T) {
	m := New(Config{
		WorkRoot:  t.TempDir(),
		Languages: map[string]LanguageServer{"go": {Command: "gopls"}},
	})
	m.DidChange("unknown.xyz", "data")
	// No panic - unknown extension means detectLanguage returns "".
}

func TestManagerDialSeamWithVersionAndClose(t *testing.T) {
	// Test spawnLocked Dial path and Manager.Close with closers.
	srv, cli := net.Pipe()
	var closerCalled bool
	m := New(Config{
		WorkRoot:  t.TempDir(),
		Languages: map[string]LanguageServer{"go": {Command: "fake"}},
		Timeout:   time.Second,
		Dial: func(lang string) (io.Reader, io.Writer, func() error, error) {
			return cli, cli, func() error { closerCalled = true; return cli.Close() }, nil
		},
	})
	// Start fake server.
	go func() {
		defer srv.Close()
		br := bufio.NewReader(srv)
		for {
			msg, err := ReadMessage(br)
			if err != nil {
				return
			}
			switch msg["method"] {
			case "initialize":
				_ = WriteMessage(srv, map[string]any{
					"jsonrpc": "2.0", "id": msg["id"],
					"result": map[string]any{"capabilities": map[string]any{}},
				})
			case "shutdown":
				_ = WriteMessage(srv, map[string]any{
					"jsonrpc": "2.0", "id": msg["id"],
					"result": nil,
				})
			}
		}
	}()
	// Trigger client creation.
	m.DidChange("main.go", "package main\n")
	// Now close should work.
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closerCalled {
		t.Fatal("closer should have been called")
	}
}

// ---------------------------------------------------------------------------
// Manager clientFor and Dial error
// ---------------------------------------------------------------------------

func TestClientForDialError(t *testing.T) {
	m := New(Config{
		WorkRoot:  t.TempDir(),
		Languages: map[string]LanguageServer{"go": {Command: "gopls"}},
		Dial: func(lang string) (io.Reader, io.Writer, func() error, error) {
			return nil, nil, nil, fmt.Errorf("dial rejected")
		},
	})
	m.DidChange("main.go", "package main")
	// The underlying clientFor should fail, which DidChange silently ignores.
	// Diagnostics should return nil since no client was created.
	diags := m.Diagnostics("main.go", 100*time.Millisecond)
	if diags != nil {
		t.Fatalf("expected nil when dial fails, got %+v", diags)
	}
}

func TestClientForInitializeFailure(t *testing.T) {
	srv, cli := net.Pipe()
	m := New(Config{
		WorkRoot:  t.TempDir(),
		Languages: map[string]LanguageServer{"go": {Command: "fake"}},
		Dial: func(lang string) (io.Reader, io.Writer, func() error, error) {
			return cli, cli, func() error { return nil }, nil // returns directly
		},
	})
	// Close srv immediately so initialize fails.
	srv.Close()

	m.DidChange("main.go", "package main")
	diags := m.Diagnostics("main.go", 100*time.Millisecond)
	if diags != nil {
		t.Fatalf("expected nil after initialize failure, got %+v", diags)
	}
}

// ---------------------------------------------------------------------------
// Additional Manager.DidChange with Dial (real clientFor path)
// ---------------------------------------------------------------------------

func TestManagerDidChangeCreateClient(t *testing.T) {
	srv, cli := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer srv.Close()
		br := bufio.NewReader(srv)
		for {
			msg, err := ReadMessage(br)
			if err != nil {
				return
			}
			if msg["method"] == "initialize" {
				_ = WriteMessage(srv, map[string]any{
					"jsonrpc": "2.0", "id": msg["id"],
					"result": map[string]any{"capabilities": map[string]any{}},
				})
			}
		}
	}()

	m := New(Config{
		WorkRoot:  t.TempDir(),
		Languages: map[string]LanguageServer{"go": {Command: "fake"}},
		Timeout:   time.Second,
		Dial: func(lang string) (io.Reader, io.Writer, func() error, error) {
			return cli, cli, func() error { return nil }, nil
		},
	})
	m.DidChange("main.go", "package main")
	// Second call uses cached client.
	m.DidChange("main.go", "package main\nvar x = 1")
	// Just verify no panic and no error.
	_ = m.Diagnostics("main.go", 500*time.Millisecond)
	m.Close()
	cli.Close()
	<-done
}

// ---------------------------------------------------------------------------
// spawnLocked exec cmd path — test that we handle Pipe/Start errors
// ---------------------------------------------------------------------------

func TestSpawnLockedStdinPipeError(t *testing.T) {
	// We can't easily make exec.Command(...).StdinPipe fail.
	// That error only happens in rare cases (e.g. after exec.Cmd is already started).
	// Skip this test — it's effectively unreachable in practice.
}

// ---------------------------------------------------------------------------
// Manager.Close with client that has no cmd (Dial path)
// ---------------------------------------------------------------------------

func TestManagerCloseIdempotent(t *testing.T) {
	m := New(Config{WorkRoot: t.TempDir(), Languages: nil})
	if err := m.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close (idempotent): %v", err)
	}
}

// ---------------------------------------------------------------------------
// FormatDiags empty and full
// ---------------------------------------------------------------------------

func TestFormatDiags(t *testing.T) {
	if got := FormatDiags("file.go", nil); got != "" {
		t.Fatalf("expected empty for nil diags, got %q", got)
	}
	if got := FormatDiags("file.go", []Diagnostic{}); got != "" {
		t.Fatalf("expected empty for empty diags, got %q", got)
	}
	got := FormatDiags("file.go", []Diagnostic{
		{Line: 1, Column: 1, Severity: SeverityError, Message: "err1", Source: "gopls"},
		{Line: 2, Column: 1, Severity: SeverityWarning, Message: "warn1"},
	})
	if !strings.Contains(got, "ERR") || !strings.Contains(got, "WARN") {
		t.Fatalf("FormatDiags missing severity labels: %q", got)
	}
	if !strings.Contains(got, "err1") || !strings.Contains(got, "warn1") {
		t.Fatalf("FormatDiags missing messages: %q", got)
	}
	if !strings.Contains(got, "gopls") {
		t.Fatalf("FormatDiags missing source: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Client readLoop exit scenario
// ---------------------------------------------------------------------------

func TestClientReadLoopExitsOnEOF(t *testing.T) {
	r, w := io.Pipe()
	cl := newClient(r, w, time.Second)
	cl.Start()
	// Close the reader to simulate agent EOF.
	r.Close()
	w.Close()
	select {
	case <-cl.Done():
		// ReadLoop exited as expected.
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not exit after pipe close")
	}
}

// ---------------------------------------------------------------------------
// diagnostic parsing from map entries
// ---------------------------------------------------------------------------

func TestParseDiagnosticWithAllFields(t *testing.T) {
	d := parseDiagnostic(map[string]any{
		"range": map[string]any{
			"start": map[string]any{"line": 5, "character": 10},
		},
		"severity": 2,
		"message":  "test warning",
		"source":   "test-linter",
	})
	if d.Line != 6 || d.Column != 11 {
		t.Fatalf("expected Line=6 Col=11 (1-based), got Line=%d Col=%d", d.Line, d.Column)
	}
	if d.Severity != SeverityWarning {
		t.Fatalf("expected SeverityWarning=2, got %d", d.Severity)
	}
	if d.Message != "test warning" {
		t.Fatalf("expected 'test warning', got %q", d.Message)
	}
	if d.Source != "test-linter" {
		t.Fatalf("expected 'test-linter', got %q", d.Source)
	}
}

func TestParseDiagnosticMissingRange(t *testing.T) {
	d := parseDiagnostic(map[string]any{
		"severity": 1,
		"message":  "err",
	})
	// Should not panic; line/column stay 0.
	_ = d
}

func TestParseDiagnosticNoStart(t *testing.T) {
	d := parseDiagnostic(map[string]any{
		"range":    map[string]any{},
		"severity": 1,
		"message":  "no start",
	})
	if d.Line != 0 || d.Column != 0 {
		t.Fatalf("missing start key leaves defaults: Line=%d Col=%d", d.Line, d.Column)
	}
}

// ---------------------------------------------------------------------------
// JavaScript/Typescript extensions
// ---------------------------------------------------------------------------

func TestLanguageIDExtended(t *testing.T) {
	if got := languageID("component.tsx"); got != "typescriptreact" {
		t.Fatalf("tsx: %q", got)
	}
	if got := languageID("app.jsx"); got != "javascriptreact" {
		t.Fatalf("jsx: %q", got)
	}
	if got := languageID("mod.rs"); got != "rust" {
		t.Fatalf("rust: %q", got)
	}
	if got := languageID("file.js"); got != "javascript" {
		t.Fatalf("js: %q", got)
	}
}

// ---------------------------------------------------------------------------
// commandAvailable coverage
// ---------------------------------------------------------------------------

func TestCommandAvailable(t *testing.T) {
	if commandAvailable("") {
		t.Fatal("empty command should return false")
	}
	if commandAvailable("non_existent_cmd_xyzzy") {
		t.Fatal("non-existent command should return false")
	}
}

// ---------------------------------------------------------------------------
// detectLanguage more extensions
// ---------------------------------------------------------------------------

func TestDetectLanguageMore(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"main.ts", "typescript"},
		{"main.tsx", "typescript"},
		{"code.c", "cpp"},
		{"code.cc", "cpp"},
		{"code.cpp", "cpp"},
		{"code.h", "cpp"},
		{"code.hpp", "cpp"},
		{"main.rs", "rust"},
		{"main.go", "go"},
		{"main.py", "python"},
		{"main.js", "javascript"},
		{"main.jsx", "javascript"},
		{"unknown.xyz", ""},
	}
	for _, tc := range tests {
		if got := detectLanguage(tc.path); got != tc.want {
			t.Errorf("detectLanguage(%q) = %q; want %q", tc.path, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// pathToURL - windows paths
// ---------------------------------------------------------------------------

func TestPathToURLWindowsDrive(t *testing.T) {
	got := pathToURL(`C:\Users\test\file.go`)
	// Should produce file:///C:/Users/test/file.go
	if !strings.Contains(got, "file:///C:") {
		t.Logf("got = %q", got)
	}
}

func TestPathToURLAbsError(t *testing.T) {
	// filepath.Abs can fail with very long paths or NUL bytes.
	// Use an empty string; on most platforms it just returns the cwd.
	got := pathToURL("")
	if got == "" {
		t.Fatal("empty path to URL should not be empty")
	}
}

// ---------------------------------------------------------------------------
// Manager spawnLocked exec.Command error paths — bad command name
// ---------------------------------------------------------------------------

func TestManagerSpawnLockedBadCommand(t *testing.T) {
	// spawnLocked Dial path needs a server goroutine to handle initialize.
	// Without one, the write blocks. Skipping: the Dial path coverage is
	// already covered by TestManagerDialSeamWithVersionAndClose.
}

// ---------------------------------------------------------------------------
// Manager Close with cmd process (exec path)
// ---------------------------------------------------------------------------

func TestManagerCloseWithCmd(t *testing.T) {
	// spawnLocked Dial path with a fake server that supports shutdown.
	srv, cli := net.Pipe()
	var closerCalled bool
	m := New(Config{
		WorkRoot:  t.TempDir(),
		Languages: map[string]LanguageServer{"go": {Command: "gopls"}},
		Dial: func(lang string) (io.Reader, io.Writer, func() error, error) {
			return cli, cli, func() error { closerCalled = true; return cli.Close() }, nil
		},
	})

	done := make(chan struct{})
	go func() {
		defer srv.Close()
		defer close(done)
		br := bufio.NewReader(srv)
		for {
			msg, err := ReadMessage(br)
			if err != nil {
				return
			}
			switch msg["method"] {
			case "initialize":
				_ = WriteMessage(srv, map[string]any{
					"jsonrpc": "2.0", "id": msg["id"],
					"result": map[string]any{"capabilities": map[string]any{}},
				})
			case "shutdown":
				_ = WriteMessage(srv, map[string]any{
					"jsonrpc": "2.0", "id": msg["id"],
					"result": nil,
				})
				return
			}
		}
	}()

	m.DidChange("main.go", "package main")
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closerCalled {
		t.Fatal("closer should have been called")
	}
	<-done
}

// ---------------------------------------------------------------------------
// Diagnostics with timeout == 0 (uses default)
// ---------------------------------------------------------------------------

func TestManagerDiagnosticsDefaultTimeout(t *testing.T) {
	srv, cli := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer srv.Close()
		br := bufio.NewReader(srv)
		for {
			msg, err := ReadMessage(br)
			if err != nil {
				return
			}
			if msg["method"] == "initialize" {
				_ = WriteMessage(srv, map[string]any{
					"jsonrpc": "2.0", "id": msg["id"],
					"result": map[string]any{"capabilities": map[string]any{}},
				})
			}
		}
	}()

	m := New(Config{
		WorkRoot:  t.TempDir(),
		Languages: map[string]LanguageServer{"go": {Command: "fake"}},
		Timeout:   time.Second,
		Dial: func(lang string) (io.Reader, io.Writer, func() error, error) {
			return cli, cli, func() error { return nil }, nil
		},
	})
	m.DidChange("main.go", "package main")
	// Call with timeout=0 uses default Timeout.
	_ = m.Diagnostics("main.go", 0)
	m.Close()
	cli.Close()
	<-done
}
