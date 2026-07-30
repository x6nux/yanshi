package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// errNoResponse is a sentinel returned by an onRequest handler to tell the
// transport's ReadLoop that it should NOT write a response for this request.
// The handler takes responsibility for writing a deferred response (used by
// the hold-mode prompt flow: the prompt id stays pending until Cancel arrives).
var errNoResponse = fmt.Errorf("no response — handler will respond asynchronously")

// inboundRequest represents a server->client JSON-RPC request.
type inboundRequest struct {
	ID     int64
	Method string
	Params json.RawMessage
}

// Transport is a concurrency-safe JSON-RPC 2.0 codec for newline-delimited stdio.
// It demultiplexes inbound messages into responses (matched by id to pending calls),
// notifications (routed to onNotify), and inbound requests (routed to onRequest).
type Transport struct {
	w       io.Writer
	r       *bufio.Reader
	mu      sync.Mutex // guards w, nextID, pending, closed
	nextID  int64
	pending map[int64]chan Response
	closed  bool

	onNotify  func(method string, params json.RawMessage)
	onRequest func(inboundRequest) (json.RawMessage, error)
}

// NewTransport creates a Transport reading from in and writing to out.
func NewTransport(in io.Reader, out io.Writer) *Transport {
	return &Transport{
		w:       out,
		r:       bufio.NewReader(in),
		pending: make(map[int64]chan Response),
	}
}

// SetHandlers installs the notification and inbound-request callbacks.
func (t *Transport) SetHandlers(
	onNotify func(method string, params json.RawMessage),
	onRequest func(inboundRequest) (json.RawMessage, error),
) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.onNotify = onNotify
	t.onRequest = onRequest
}

// writeLine serializes msg as JSON, appends a newline, and writes under mutex.
func (t *Transport) writeLine(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("acp: marshal: %w", err)
	}
	data = append(data, '\n')
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return io.ErrClosedPipe
	}
	_, err = t.w.Write(data)
	return err
}

// Call sends a JSON-RPC request and blocks until the response arrives or ctx is cancelled.
func (t *Transport) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	// Fast-path a pre-cancelled context: without this check the request line
	// is written and the select below races ctx.Done() against the response.
	// Under -race a fast FakeAgent can respond before the runtime schedules
	// the ctx.Done() branch, so Call returns the result instead of the error.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Allocate an id and register the pending channel before writing.
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, io.ErrClosedPipe
	}
	t.nextID++
	id := t.nextID
	ch := make(chan Response, 1)
	t.pending[id] = ch
	t.mu.Unlock()

	// Write the request line. If this fails, clean up the pending entry.
	req := Request{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	if err := t.writeLine(req); err != nil {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
		return nil, err
	}

	// Wait for the response or context cancellation.
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		// Clean up the pending entry so the id cannot be reused by a late response.
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Notify sends a JSON-RPC notification (no id, no response expected).
func (t *Transport) Notify(method string, params any) error {
	notif := Notification{JSONRPC: "2.0", Method: method, Params: params}
	return t.writeLine(notif)
}

// Respond writes a JSON-RPC Response for an inbound server->client request.
func (t *Transport) Respond(id int64, result any, rpcErr error) error {
	resp := Response{JSONRPC: "2.0", ID: id}
	if rpcErr != nil {
		var ok bool
		resp.Error, ok = rpcErr.(*RPCError)
		if !ok {
			resp.Error = &RPCError{Code: -32603, Message: rpcErr.Error()}
		}
	} else {
		data, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("acp: marshal result: %w", err)
		}
		resp.Result = data
	}
	return t.writeLine(resp)
}

// ReadLoop reads newline-delimited JSON-RPC messages and demultiplexes them.
// It blocks until the reader returns an error or ctx is cancelled.
// On exit, all pending callers are failed.
func (t *Transport) ReadLoop(ctx context.Context) error {
	scanner := bufio.NewScanner(t.r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		// Check context between lines for prompt cancellation.
		if err := ctx.Err(); err != nil {
			t.failPending(io.ErrClosedPipe)
			return err
		}

		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var raw RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			// Skip malformed lines — don't kill the transport.
			continue
		}

		switch {
		case raw.IsResponse():
			t.deliverResponse(raw)
		case raw.IsRequest():
			t.handleRequest(raw)
		case raw.IsNotification():
			t.handleNotification(raw)
		}
	}

	// Scanner stopped (EOF or error).
	t.failPending(io.ErrClosedPipe)
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

// deliverResponse routes a response to the waiting Call, if any.
func (t *Transport) deliverResponse(raw RawMessage) {
	if raw.ID == nil {
		return
	}
	id := *raw.ID
	t.mu.Lock()
	ch, ok := t.pending[id]
	if ok {
		delete(t.pending, id)
	}
	t.mu.Unlock()
	if !ok {
		return // response with no matching pending call — drop
	}
	resp := Response{
		JSONRPC: raw.JSONRPC,
		Result:  raw.Result,
		Error:   raw.Error,
	}
	if raw.ID != nil {
		resp.ID = *raw.ID
	}
	ch <- resp
}

// handleRequest dispatches an inbound server->client request to the handler
// and writes the response back.
func (t *Transport) handleRequest(raw RawMessage) {
	if raw.ID == nil {
		return
	}
	t.mu.Lock()
	handler := t.onRequest
	t.mu.Unlock()

	if handler == nil {
		// No handler — respond with an error.
		t.Respond(*raw.ID, nil, &RPCError{Code: -32601, Message: "no request handler"})
		return
	}

	req := inboundRequest{
		ID:     *raw.ID,
		Method: raw.Method,
		Params: raw.Params,
	}
	result, err := handler(req)
	if err == errNoResponse {
		// Handler will write a deferred response itself — skip Respond.
		return
	}
	t.Respond(req.ID, result, err)
}

// handleNotification dispatches a notification to the onNotify callback.
func (t *Transport) handleNotification(raw RawMessage) {
	t.mu.Lock()
	handler := t.onNotify
	t.mu.Unlock()
	if handler != nil {
		handler(raw.Method, raw.Params)
	}
}

// failPending closes all pending channels with an error.
func (t *Transport) failPending(err error) {
	t.mu.Lock()
	for id, ch := range t.pending {
		ch <- Response{Error: &RPCError{Code: -32000, Message: err.Error()}}
		delete(t.pending, id)
	}
	t.closed = true
	t.mu.Unlock()
}
