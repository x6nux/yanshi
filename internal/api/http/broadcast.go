package http

import (
	"sync"

	"github.com/x6nux/yanshi/internal/proto"
)

// Server-initiated frames: the one path that does not originate in a turn.
//
// Every other frame this package emits is a reply to something the client just
// did — a turn's events, a control frame's answer. Durable task transitions are
// not: the broker moves a task through running → completed on a worker
// goroutine, minutes after the turn that created it returned. The tool-layer
// path (TurnOpts.EmitWorkFrame) cannot carry them, because by then there is no
// turn and therefore no context to have bound a callback into.
//
// So the connection has to be reachable from outside the request that created
// it. wsClients is that registry. It is deliberately the smallest thing that
// works: no per-session filtering, no replay, no backpressure policy beyond
// what wsConn.write already does.
//
// No filtering by session is not an oversight. A durable task's parent thread
// is knowable, but a client may hold several sessions open on ONE connection
// (the TUI switches sessions without reconnecting), and a task_update names its
// own task, so a client that does not recognise the id ignores the frame. The
// alternative — filtering server-side on a session id that the connection may
// have since switched away from — drops updates the user is looking at.

// registerClient adds conn to the broadcast set and returns a function that
// removes it. Callers must defer the returned function: a connection left in
// the set is written to forever, and wsConn.write swallows the error, so the
// leak is silent.
func (s *Server) registerClient(conn *wsConn) func() {
	s.clientsMu.Lock()
	if s.wsClients == nil {
		s.wsClients = make(map[*wsConn]struct{})
	}
	s.wsClients[conn] = struct{}{}
	s.clientsMu.Unlock()
	return func() {
		s.clientsMu.Lock()
		delete(s.wsClients, conn)
		s.clientsMu.Unlock()
	}
}

// Broadcast writes f to every currently connected WebSocket client.
//
// Callers are background goroutines (the durable task broker), so this must
// never block on a slow reader holding up task dispatch. It does not: writes
// go through wsConn.write, which serializes on the connection's own mutex and
// drops write errors, and the snapshot below means the registry lock is not
// held across any write.
//
// SSE has no equivalent. It is request-scoped by construction — the client
// holds the history and replays it per request — so there is no open stream to
// push a later transition into. An SSE client learns a task finished by asking.
func (s *Server) Broadcast(f proto.ServerFrame) {
	s.clientsMu.RLock()
	conns := make([]*wsConn, 0, len(s.wsClients))
	for c := range s.wsClients {
		conns = append(conns, c)
	}
	s.clientsMu.RUnlock()
	for _, c := range conns {
		c.write(f)
	}
}

// clientRegistry is embedded in Server; kept here so the fields sit next to the
// only code that touches them.
type clientRegistry struct {
	clientsMu sync.RWMutex
	wsClients map[*wsConn]struct{}
}
