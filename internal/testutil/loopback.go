// Package testutil's loopback helper. See root.go for why this package may
// import "testing".
package testutil

import (
	"net"
	"strconv"
	"testing"
)

const (
	// closedPortBase is where the scan for a free loopback port starts. It sits
	// in the registered range rather than in the ephemeral range the OS hands
	// out, so it is unlikely to collide with a port something else just took.
	closedPortBase = 17321
	// closedPortAttempts bounds the scan. The first probe almost always
	// succeeds, so this is a runaway guard, not a search budget.
	closedPortAttempts = 200
)

// ClosedLoopbackAddr returns a "localhost:port" that nothing is listening on,
// so a dial to it is refused immediately.
//
// It exists because tests across this repo used to hardcode 127.0.0.1:1 as "a
// closed port", and that is an assumption about the machine rather than about
// the code. Port 1 is a normal TCP port: on a developer box running VS Code, a
// Node utility process holds 127.0.0.1:1 in LISTEN. The address is then not
// closed at all — the dial SUCCEEDS and the peer never answers — which is a
// different failure with a different, much larger cost:
//
//   - internal/cli's TestRunHeadless_SendError hung on it until the package's
//     own 600s test deadline fired.
//   - internal/mcp's TestHTTPClientRequestErrors spent 120s on it: four calls
//     that each waited out the MCP client's 30s timeout for an error that a
//     refused connection returns in microseconds. TestClientCredentialsSourceErrors
//     added another 30s. Those 150s were, between them, the single largest
//     remaining cost in the whole suite.
//   - internal/observe/otel's TestCollectorAvailable went red on a host where
//     nothing was actually wrong.
//
// The port is found by BINDING it: start at closedPortBase and step up by one
// on every failure until a bind succeeds, then close the listener and hand the
// address back. A successful bind is the only evidence available that a port is
// free, and scanning forward is what keeps this from landing on a port that is
// already taken — the failure mode that hardcoding port 1 produced.
//
// Closing the listener does leave a window in which another process could claim
// the port before the caller dials it. That window is microseconds wide, and
// every caller here either tolerates it (mcp, otel) or is bounded by a timeout
// it should have had anyway (cli's SSE client).
func ClosedLoopbackAddr(t *testing.T) string {
	t.Helper()
	for port := closedPortBase; port < closedPortBase+closedPortAttempts; port++ {
		addr := net.JoinHostPort("localhost", strconv.Itoa(port))
		l, err := net.Listen("tcp", addr)
		if err != nil {
			// Occupied (or otherwise unbindable): step to the next port.
			continue
		}
		if err := l.Close(); err != nil {
			t.Fatalf("release the probed loopback port: %v", err)
		}
		return addr
	}
	t.Fatalf("no free loopback port in %d..%d",
		closedPortBase, closedPortBase+closedPortAttempts-1)
	return ""
}
