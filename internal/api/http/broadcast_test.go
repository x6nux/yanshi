package http

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/proto"
)

// TestRegisterClientUnregisters pins the half of the registry that fails
// silently.
//
// A connection left in the set is written to forever, and wsConn.write drops
// its errors by design (the read loop is what notices a closed socket), so a
// leaked entry produces no log line, no error and no test failure — just an
// unbounded map on a long-running server. The end-to-end test cannot see this:
// it only asserts that frames DO arrive.
//
// wsConn embeds a nil *websocket.Conn here on purpose. Broadcast must not be
// reached for an unregistered connection, so if the unregister is broken this
// test panics on the nil write rather than passing quietly.
func TestRegisterClientUnregisters(t *testing.T) {
	s := New(Config{Token: "t"})
	conn := &wsConn{}

	unregister := s.registerClient(conn)
	s.clientsMu.RLock()
	n := len(s.wsClients)
	s.clientsMu.RUnlock()
	require.Equal(t, 1, n, "the connection was never added, so it receives nothing")

	unregister()
	s.clientsMu.RLock()
	n = len(s.wsClients)
	s.clientsMu.RUnlock()
	require.Equal(t, 0, n, "the connection outlives its request and is written to forever")

	// Reaching a nil *websocket.Conn would panic; not panicking is the assertion.
	s.Broadcast(proto.ServerFrame{Type: "task_update"})
}
