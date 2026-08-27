// Package acpserver implements the AGENT side of the Agent Client Protocol on
// stdio, so an ACP host (Zed and anything else that speaks the protocol) can
// drive yanshi's own orchestrator instead of yanshi driving someone else's CLI.
//
// Relationship to the rest of the tree:
//
//   - internal/acp is the CLIENT half. It has the wire types, and this package
//     reuses them rather than declaring a second set — a duplicate would drift,
//     and the two halves have to agree by construction because one repository
//     ships both ends of the same protocol.
//   - internal/appserver is the same SHAPE (JSON-RPC 2.0, one object per line,
//     stdout for frames and stderr for diagnostics) over a different method
//     vocabulary. This package deliberately does not build on it: appserver's
//     dispatcher is written around the v1 method names and their one-response
//     contract, whereas ACP's session/prompt holds a single response open for
//     the whole turn while notifications stream underneath it. Sharing the
//     dispatcher would mean parameterising it on the one behaviour that
//     differs.
//   - internal/api/v1.Service is the shared thread/turn engine, so a turn run
//     through this transport is the same turn HTTP and the app-server run.
package acpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/x6nux/yanshi/internal/acp"
	v1 "github.com/x6nux/yanshi/internal/api/v1"
)

// ProtocolVersion is the ACP version this server implements. It is echoed in
// the initialize result; a host that requires a newer one will say so.
const ProtocolVersion = 1

// Server speaks ACP as the agent over one stdio pair.
//
// Concurrency: reads are single-threaded (one request per line), but a
// session/prompt runs a turn that streams notifications while the dispatcher
// keeps reading — so a session/cancel can arrive mid-turn, which is the whole
// reason the loop does not simply block. writeMu serialises every outbound
// line so a notification cannot be spliced into the middle of a response.
type Server struct {
	agent *v1.Service
	// diag is where anything human-readable goes. It is a field rather than a
	// package-level logger because stdout is reserved for protocol frames: a
	// single stray line there desynchronises the host's line-oriented parser,
	// and a logger nobody passed would default to exactly that.
	diag io.Writer

	writeMu sync.Mutex

	// mu guards sessions.
	mu sync.Mutex
	// sessions maps an ACP session id to the v1 thread backing it, plus the
	// cancel for its active turn.
	sessions map[string]*acpSession
	nextID   int
	// inflight tracks turn goroutines so Serve does not return while one is
	// still writing notifications.
	inflight sync.WaitGroup
}

// acpSession is one ACP session: a v1 thread plus the cancel of whatever turn
// is running on it.
type acpSession struct {
	threadID string
	// cancel aborts the active turn. nil when no turn is running. Guarded by
	// the Server's mu, not by a per-session lock: session/cancel and the turn's
	// own completion both touch it, and one lock for the whole small map is
	// simpler to reason about than a lock per session.
	cancel context.CancelFunc
}

// New constructs a Server. agent is required. diag receives diagnostics; nil
// discards them, which is correct for a test but wrong for production — the
// CLI passes os.Stderr.
func New(agent *v1.Service, diag io.Writer) *Server {
	if diag == nil {
		diag = io.Discard
	}
	return &Server{agent: agent, diag: diag, sessions: map[string]*acpSession{}}
}

// Serve reads newline-delimited JSON-RPC from r until EOF, dispatching each
// message and writing responses and notifications to w.
//
// It waits for in-flight turns before returning, for the same reason
// appserver.Serve does: a fast EOF must not strand a goroutine that is still
// writing session/update notifications into a writer the caller is about to
// consider finished.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	// ACP prompts carry whole file contents; the default 64 KiB token limit
	// would turn a large paste into a scan error that reads as a protocol
	// failure.
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw acp.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			// A malformed line is skipped rather than fatal: there is no id to
			// answer, and killing the transport over one bad line would take
			// down a working session.
			fmt.Fprintf(s.diag, "acpserver: skipping unparseable line: %v\n", err)
			continue
		}
		if err := s.handle(ctx, w, raw); err != nil {
			s.inflight.Wait()
			return err
		}
	}
	s.inflight.Wait()
	if err := sc.Err(); err != nil {
		return fmt.Errorf("acpserver: read: %w", err)
	}
	return nil
}

// handle routes one inbound message. It returns an error only for a WRITE
// failure — a broken pipe means the host is gone and there is nothing left to
// serve. Protocol-level failures become JSON-RPC error responses.
func (s *Server) handle(ctx context.Context, w io.Writer, raw acp.RawMessage) error {
	if raw.IsNotification() {
		s.handleNotification(raw)
		return nil
	}
	if !raw.IsRequest() {
		// A response, addressed to a request this server never sent. Ignored
		// rather than answered: replying to a reply is how two peers loop.
		return nil
	}
	id := *raw.ID
	result, rpcErr := s.dispatch(ctx, w, id, raw)
	if rpcErr == errDeferred {
		// session/prompt answers from its own goroutine once the turn ends.
		return nil
	}
	if rpcErr != nil {
		return s.writeError(w, id, rpcErr)
	}
	return s.writeResult(w, id, result)
}

// errDeferred tells handle that the dispatcher took responsibility for writing
// the response later. It mirrors internal/acp's errNoResponse on the client
// side; the same pattern is needed here because session/prompt's response is
// the END of the turn, not the acknowledgement of it.
var errDeferred = &acp.RPCError{Code: 0, Message: "deferred"}

// dispatch routes one request by method.
func (s *Server) dispatch(ctx context.Context, w io.Writer, id int64, raw acp.RawMessage) (any, *acp.RPCError) {
	switch raw.Method {
	case "initialize":
		return s.initialize(), nil
	case "session/new":
		return s.newSession(ctx, raw.Params)
	case "session/prompt":
		return nil, s.startPrompt(ctx, w, id, raw.Params)
	case "session/load":
		return s.loadSession(ctx, raw.Params)
	default:
		return nil, &acp.RPCError{Code: -32601, Message: "method not found: " + raw.Method}
	}
}

// handleNotification processes the notifications a host sends. Only
// session/cancel is meaningful; anything else is ignored, because a
// notification has no id and therefore no way to report a complaint.
func (s *Server) handleNotification(raw acp.RawMessage) {
	if raw.Method != "session/cancel" {
		return
	}
	var p acp.CancelParams
	if err := json.Unmarshal(raw.Params, &p); err != nil {
		return
	}
	s.cancelSession(p.SessionID)
}

// cancelSession aborts the active turn on a session, if any.
//
// It does NOT delete the session. ACP cancellation ends the turn, not the
// conversation; dropping the session would make the host's next prompt fail
// with "unknown session" for a session it never closed.
func (s *Server) cancelSession(sessionID string) {
	s.mu.Lock()
	sess, ok := s.sessions[sessionID]
	var cancel context.CancelFunc
	if ok {
		cancel = sess.cancel
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// initialize reports the protocol version and what this agent can do.
//
// The capability set is deliberately narrow: yanshi's own tools run INSIDE the
// v1 service, behind its guard, so this agent does not ask the host to read or
// write files on its behalf. Advertising loadSession is the one real claim, and
// it is backed by v1's thread resume.
func (s *Server) initialize() acp.InitResult {
	caps, _ := json.Marshal(map[string]any{
		"loadSession": true,
		"promptCapabilities": map[string]any{
			"image": false, "audio": false, "embeddedContext": false,
		},
	})
	return acp.InitResult{
		ProtocolVersion:   ProtocolVersion,
		AgentCapabilities: caps,
		AgentInfo:         acp.AgentInfo{Name: "yanshi", Title: "yanshi", Version: v1.Version},
		AuthMethods:       []json.RawMessage{},
	}
}

// writeResult writes a success response.
func (s *Server) writeResult(w io.Writer, id int64, result any) error {
	data, err := json.Marshal(result)
	if err != nil {
		return s.writeError(w, id, &acp.RPCError{Code: -32603, Message: "encode result: " + err.Error()})
	}
	return s.writeLine(w, acp.Response{JSONRPC: "2.0", ID: id, Result: data})
}

// writeError writes a JSON-RPC error response.
func (s *Server) writeError(w io.Writer, id int64, rpcErr *acp.RPCError) error {
	return s.writeLine(w, acp.Response{JSONRPC: "2.0", ID: id, Error: rpcErr})
}

// writeNotify writes an outbound notification.
func (s *Server) writeNotify(w io.Writer, method string, params any) error {
	return s.writeLine(w, acp.Notification{JSONRPC: "2.0", Method: method, Params: params})
}

// writeLine marshals one value and writes it as a single newline-terminated
// line, holding writeMu so a streaming notification and a response can never
// interleave bytes.
func (s *Server) writeLine(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("acpserver: marshal: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
		return fmt.Errorf("acpserver: write: %w", err)
	}
	return nil
}
