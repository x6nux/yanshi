package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/x6nux/yanshi/internal/api/v1"
)

// ConfigBackend is the local-supervisor config interface the app-server
// exposes via config/read and config/write. Implementations MUST reject secret
// paths (token / api_key / *password*) so secret material never crosses the
// JSON-RPC wire.
type ConfigBackend interface {
	Read(key string) (any, error)
	Write(key string, value json.RawMessage) error
}

// Server is the JSON-RPC 2.0 stdio dispatcher. It serves one request line at
// a time on the reader; responses and notifications share one writer guarded
// by writeMu so they never byte-interleave on stdout. Item streaming for
// turn/start runs in a goroutine so the dispatcher can keep reading requests
// while items flow.
//
// Goroutine lifecycle: each turn/start spawns a forwardItems goroutine
// registered on inflight. Serve waits on inflight before returning from
// shutdown or from a read EOF so stream notifications reach the wire before
// the caller assumes the exchange is complete. Without this wait a fast
// shutdown could return while items were still pending in a goroutine,
// racing callers that read stdout the moment Serve returns.
type Server struct {
	agent   *v1.Service
	config  ConfigBackend
	writeMu sync.Mutex
	inflight sync.WaitGroup
}

// New constructs a Server. agent is required; config is optional (nil disables
// config/read + config/write with a clear -32603 error).
func New(agent *v1.Service, config ConfigBackend) *Server {
	return &Server{agent: agent, config: config}
}

// Serve drains the reader one JSON-RPC request per line until EOF, a shutdown
// method, or a read error. Each request produces zero or one response (plus an
// asynchronous stream of item/updated notifications when the method is
// turn/start). Notifications (requests without an ID) produce no response.
//
// Error policy: parse/dispatch errors send a JSON-RPC error response (or drop
// silently for notifications); writer errors abort Serve so a broken pipe
// surfaces to the caller.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		req, err := parseRPCLine(line)
		if err != nil {
			if rpcErr, ok := err.(*RPCError); ok {
				if writeErr := s.write(w, rpcErrorResponse(nil, rpcErr.Code, rpcErr.Message, rpcErr.Data)); writeErr != nil {
					return writeErr
				}
				continue
			}
			return err
		}
		result, stream, dispatchErr := s.dispatch(ctx, req)
		if dispatchErr != nil {
			// Per spec: notifications never receive a response — even on error.
			if len(req.ID) != 0 {
				if writeErr := s.write(w, rpcErrorResponse(req.ID, dispatchErr.Code, dispatchErr.Message, dispatchErr.Data)); writeErr != nil {
					return writeErr
				}
			}
			continue
		}
		if len(req.ID) != 0 {
			if writeErr := s.write(w, rpcResponse(req.ID, result)); writeErr != nil {
				return writeErr
			}
		}
		if stream != nil {
			s.inflight.Add(1)
			go func(items <-chan v1.Item) {
				defer s.inflight.Done()
				s.forwardItems(ctx, w, items)
			}(stream)
		}
		if req.Method == "shutdown" {
			s.inflight.Wait()
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		s.inflight.Wait()
		return err
	}
	s.inflight.Wait()
	return nil
}

// dispatch routes one parsed request to its handler. Returns (result, nil, nil)
// for ordinary methods, (started, items, nil) for turn/start (the caller
// streams items asynchronously), or (nil, nil, *RPCError) on any error.
func (s *Server) dispatch(ctx context.Context, req RPCRequest) (any, <-chan v1.Item, *RPCError) {
	switch req.Method {
	case "initialize":
		return capabilitiesResult(true), nil, nil
	case "capabilities":
		return capabilitiesResult(false), nil, nil
	case "thread/start":
		var p v1.ThreadStartParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		thread, err := s.agent.Start(ctx, p)
		if err != nil {
			return nil, nil, &RPCError{Code: codeInternalError, Message: err.Error()}
		}
		return v1.ThreadStartResponse{Version: v1.Version, Thread: thread}, nil, nil
	case "thread/resume":
		var p v1.ThreadResumeParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		snapshot, err := s.agent.Resume(ctx, p)
		if errors.Is(err, v1.ErrThreadNotFound) {
			return nil, nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		if err != nil {
			return nil, nil, &RPCError{Code: codeInternalError, Message: err.Error()}
		}
		return v1.ThreadResumeResponse{Version: v1.Version, Thread: snapshot.Thread, Items: snapshot.Items}, nil, nil
	case "thread/interrupt", "turn/interrupt":
		var p v1.ThreadInterruptParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		if err := s.agent.Interrupt(ctx, p); err != nil {
			return nil, nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		return v1.InterruptResponse{Version: v1.Version, OK: true, ThreadID: p.ThreadID, TurnID: p.TurnID}, nil, nil
	case "turn/start":
		var p v1.TurnStartParams
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		started, items, err := s.agent.StartTurn(ctx, p)
		if errors.Is(err, v1.ErrThreadNotFound) || errors.Is(err, v1.ErrTurnAlreadyActive) {
			return nil, nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		if err != nil {
			return nil, nil, &RPCError{Code: codeInternalError, Message: err.Error()}
		}
		return started, items, nil
	case "config/read":
		if s.config == nil {
			return nil, nil, &RPCError{Code: codeInternalError, Message: "config backend is unavailable"}
		}
		var p struct {
			Key string `json:"key"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		value, err := s.config.Read(p.Key)
		if err != nil {
			return nil, nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		return map[string]any{"version": v1.Version, "key": p.Key, "value": value}, nil, nil
	case "config/write":
		if s.config == nil {
			return nil, nil, &RPCError{Code: codeInternalError, Message: "config backend is unavailable"}
		}
		var p struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		if err := s.config.Write(p.Key, p.Value); err != nil {
			return nil, nil, &RPCError{Code: codeInvalidParams, Message: err.Error()}
		}
		return map[string]any{"version": v1.Version, "ok": true, "key": p.Key}, nil, nil
	case "shutdown":
		return map[string]any{"version": v1.Version, "ok": true}, nil, nil
	default:
		return nil, nil, &RPCError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

// capabilitiesResult returns the v1 Capabilities payload. `initialize`
// includes lifecycle/config methods; `capabilities` advertises the user-facing
// subset (lifecycle is implicit). Both surfaces share item types and the
// `item/updated` stream notification name.
func capabilitiesResult(initialize bool) v1.Capabilities {
	methods := []string{
		"thread/start", "thread/resume", "thread/interrupt", "turn/start", "turn/interrupt",
	}
	if initialize {
		methods = append(methods, "capabilities", "config/read", "config/write", "initialize", "shutdown")
	} else {
		methods = append(methods, "config/read", "config/write")
	}
	return v1.Capabilities{
		Version:   v1.Version,
		Methods:   methods,
		ItemTypes: []string{v1.ItemTurnStarted, v1.ItemMessageDelta, v1.ItemReasoningDelta, v1.ItemToolCall, v1.ItemToolResult, v1.ItemToolProgress, v1.ItemStructuredResult, v1.ItemTurnError, v1.ItemTurnCompleted},
		UnknownFields: "ignored",
		Stream:    "item/updated",
	}
}

// forwardItems drains an item channel and writes one item/updated notification
// per item. The writer mutex keeps the notifications byte-aligned with any
// concurrent responses so each `\n`-terminated line is independently parseable
// by the client.
func (s *Server) forwardItems(ctx context.Context, w io.Writer, items <-chan v1.Item) {
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-items:
			if !ok {
				return
			}
			_ = s.write(w, RPCNotification{JSONRPC: "2.0", Method: "item/updated", Params: item})
		}
	}
}

// write serializes one value to the writer as a single JSON line, holding
// writeMu so concurrent responses and notifications never interleave bytes.
func (s *Server) write(w io.Writer, value any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal JSON-RPC response: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}
