package execbroker

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests in this file are about a threat the rest of the package does not
// cover: the socket is reachable by every untrusted child by design, so a child
// that never intends to elevate anything can still spend the PARENT'S
// resources. The parent is the process holding every provider key, which is the
// whole reason W-B-08 exists — a child that can make it run out of file
// descriptors takes down the HTTP listener, the WebSocket sessions and SQLite
// along with the broker.
//
// None of these probes floods anything. Each shrinks the bound it is testing to
// a size a handful of connections crosses, which is what makes them run in
// milliseconds and what keeps them from being a denial-of-service against the
// machine running the suite.

// dialBroker opens a connection to the broker under test.
func dialBroker(t *testing.T, s *Server) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", s.listener.Addr().String())
	if err != nil {
		t.Fatalf("dial the broker: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readVerdict reads one newline-terminated Response.
func readVerdict(t *testing.T, conn net.Conn) Response {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read the verdict: %v (got %q)", err, line)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal the verdict %q: %v", line, err)
	}
	return resp
}

// TestBrokerDropsAConnectionThatSendsNothing pins the request read deadline.
//
// Without it the handler sits in ReadBytes forever. Server.Close does not
// reclaim those goroutines — it closes the listener and removes the shim
// directory, neither of which touches an already-accepted connection — so the
// leak outlives the launch that created it. A child does not need to be
// approved for anything to cause it; connecting is enough.
//
// The assertion is that the PARENT hangs up, observed from the client side as a
// read that ends. A test that only checked "the goroutine count came back down"
// would also pass on a host where the runtime happened to schedule differently.
func TestBrokerDropsAConnectionThatSendsNothing(t *testing.T) {
	restore := requestReadTimeout
	requestReadTimeout = 150 * time.Millisecond
	t.Cleanup(func() { requestReadTimeout = restore })

	s := newTestServer(t, func(context.Context, Request) error { return nil })
	conn := dialBroker(t, s)

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("the broker sent data to a connection that sent no request")
	} else if strings.Contains(err.Error(), "i/o timeout") {
		t.Fatalf("the broker never closed a silent connection: its handler is still "+
			"parked in ReadBytes, and Server.Close will not reclaim it (%v)", err)
	}
}

// TestBrokerRefusesAnOversizedRequest pins the request size cap.
//
// bufio.Reader.ReadBytes grows its buffer until it finds the delimiter, so one
// connection writing a long line with no newline makes the parent allocate in
// proportion to what the child chose to send.
//
// The verdict text is asserted, not just the refusal: "too large" and
// "malformed request" lead an operator to different places, and without the
// explicit branch an over-cap request arrives as truncated JSON and reads as
// the second.
func TestBrokerRefusesAnOversizedRequest(t *testing.T) {
	restore := maxRequestBytes
	maxRequestBytes = 4 << 10
	t.Cleanup(func() { maxRequestBytes = restore })

	s := newTestServer(t, func(context.Context, Request) error { return nil })
	conn := dialBroker(t, s)

	// One line, no newline, comfortably over the cap. The write runs in its own
	// goroutine and its error is ignored on purpose: the server answers and
	// hangs up as soon as it hits the cap, so a client still pushing bytes gets
	// EPIPE. That broken pipe IS the bound working — failing the test on it
	// would be asserting the absence of the behaviour under test.
	go func() {
		_, _ = conn.Write([]byte(`{"token":"` + strings.Repeat("A", 16<<10)))
	}()
	resp := readVerdict(t, conn)
	if resp.Allow {
		t.Fatal("an oversized request was ALLOWED")
	}
	if !strings.Contains(resp.Reason, "too large") {
		t.Errorf("an over-cap request was reported as %q; the size branch is not being "+
			"taken, which means the reader is buffering the whole line", resp.Reason)
	}
}

// TestBrokerRefusesRequestsBeyondTheConcurrencyLimit pins the accept bound.
//
// The decider here blocks, which is the realistic case rather than a contrived
// one: an adjudication that reaches the interactive approval callback waits for
// a human. That is exactly when the in-flight count can climb, and exactly when
// an unbounded serve loop hands one goroutine per connection to whatever is
// dialling.
//
// The refusal is asserted to arrive as a VERDICT rather than as a dropped
// connection. The shim has no read deadline by design, so a broker that queued
// instead would leave the child hanging with no way to tell "waiting for the
// operator" from "waiting for a slot".
func TestBrokerRefusesRequestsBeyondTheConcurrencyLimit(t *testing.T) {
	restore := maxConcurrentRequests
	maxConcurrentRequests = 2
	t.Cleanup(func() { maxConcurrentRequests = restore })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s := newTestServer(t, func(context.Context, Request) error {
		<-release
		return nil
	})

	for i := 0; i < maxConcurrentRequests; i++ {
		conn := dialBroker(t, s)
		req, err := json.Marshal(Request{Token: s.token, Program: "sudo", Args: []string{"true"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(append(req, '\n')); err != nil {
			t.Fatalf("write request %d: %v", i, err)
		}
	}
	// The accept loop and the handler goroutines race with this client, so wait
	// for the slots to actually be occupied rather than assuming they are.
	deadline := time.Now().Add(5 * time.Second)
	for len(s.slots) < maxConcurrentRequests && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(s.slots) < maxConcurrentRequests {
		t.Fatalf("only %d of %d slots were taken; the probe never reached the limit",
			len(s.slots), maxConcurrentRequests)
	}

	over := dialBroker(t, s)
	resp := readVerdict(t, over)
	if resp.Allow {
		t.Fatal("a request past the concurrency limit was ALLOWED")
	}
	if !strings.Contains(resp.Reason, "too many") {
		t.Errorf("the request past the limit was answered %q, want the over-limit refusal; "+
			"an unbounded serve loop answers it normally", resp.Reason)
	}
}

// TestResolveSkipsACandidateThatIsThisExecutable pins the self-identity guard,
// which is the invariant the directory comparison was approximating.
//
// The four spellings below all defeat a string comparison of directories and
// all end at the same place: syscall.Exec on the shim, which re-enters RunShim,
// which resolves the shim again. There is no stack to overflow and no counter
// to trip; the observable symptom is the parent being asked to adjudicate the
// same elevation forever, which in the default permission mode is an endless
// dialog.
//
// This probe never execs anything. It asks the resolver what it WOULD run, so a
// regression here costs a failed assertion rather than a fork bomb — the
// mutation that first exposed this had to be killed by hand.
func TestResolveSkipsACandidateThatIsThisExecutable(t *testing.T) {
	// Every case below ends at "the resolver picked the real program", which
	// needs a candidate the execute-bit filter can accept. See
	// needsPOSIXExecuteBit for why that has no Windows spelling.
	needsPOSIXExecuteBit(t)
	self, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable is unavailable here: %v", err)
	}

	// A directory of shims that is NOT the one named in ShimDirEnv: the alias
	// case, reproduced with a second directory of symlinks to this binary.
	alias := t.TempDir()
	if err := os.Symlink(self, filepath.Join(alias, "doas")); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	// The real program, further down PATH.
	realDir := t.TempDir()
	realProg := filepath.Join(realDir, "doas")
	if err := os.WriteFile(realProg, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A third directory that is the one ShimDirEnv names, so the string guard
	// is doing its normal job and only the alias is left to catch.
	named := t.TempDir()

	cases := []struct{ name, path, shimDir string }{
		{"alias directory ahead of the real one", alias + sep() + realDir, named},
		{"shim dir spelled \".\"", alias + sep() + realDir, "."},
		{"two shim dirs, only one named", named + sep() + alias + sep() + realDir, named},
		{"trailing separator on the named dir", alias + sep() + realDir, named + string(os.PathSeparator)},
	}
	for _, tc := range cases {
		got, err := resolveOutsideShimDir("doas", tc.path, tc.shimDir)
		if err != nil {
			t.Errorf("%s: resolve failed outright: %v", tc.name, err)
			continue
		}
		if got == filepath.Join(alias, "doas") {
			t.Errorf("%s: the resolver chose the SHIM ITSELF (%s); syscall.Exec on it "+
				"re-enters RunShim and the loop never ends", tc.name, got)
			continue
		}
		if got != realProg {
			t.Errorf("%s: resolved to %q, want the real program %q", tc.name, got, realProg)
		}
	}
}

// sep is the PATH list separator as a string.
func sep() string { return string(os.PathListSeparator) }
