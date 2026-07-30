package http

import (
	"net/http"
	"sync"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/secrets"
)

// subagentRelay lets long-lived background agents retain a callback without
// retaining the WebSocket connection. Detach nils the writer on disconnect;
// later lifecycle events are intentionally dropped (Manager persists them).
//
// Emit holds its RLock across the write call so Detach (write-lock) waits for
// all in-flight writes before returning — no write uses a nil/disconnected conn.
type subagentRelay struct {
	mu    sync.RWMutex
	write func(proto.ServerFrame)
}

func newSubagentRelay(write func(proto.ServerFrame)) *subagentRelay {
	return &subagentRelay{write: write}
}

// Emit calls the bound writer under RLock so Detach's write-lock waits for
// in-flight writes to drain before releasing the conn reference.
func (r *subagentRelay) Emit(f proto.ServerFrame) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.write != nil {
		r.write(f)
	}
}

// Detach nils the writer under write-lock. The lock waits for all in-flight
// Emit calls to complete, so the caller can safely close the WS conn after
// Detach returns. Does NOT cancel the running managed agent: terminal states
// are persisted even if the connection is gone.
func (r *subagentRelay) Detach() {
	r.mu.Lock()
	r.write = nil
	r.mu.Unlock()
}

// sseLifecycleRelay is a bounded relay for SSE handlers. It separates
// terminal events (must not be lost) from progress events (best-effort).
type sseLifecycleRelay struct {
	mu       sync.RWMutex
	open     bool
	progress chan proto.ServerFrame
	terminal chan proto.ServerFrame
}

func newSSELifecycleRelay() *sseLifecycleRelay {
	return &sseLifecycleRelay{
		open:     true,
		progress: make(chan proto.ServerFrame, 64),
		terminal: make(chan proto.ServerFrame, 8),
	}
}

func (r *sseLifecycleRelay) Emit(f proto.ServerFrame) {
	r.mu.RLock()
	open := r.open
	r.mu.RUnlock()
	if !open {
		return
	}
	terminal := f.Event == "completed" || f.Event == "failed" ||
		f.Event == "cancelled" || f.Event == "persistence_failed"
	if terminal {
		select {
		case r.terminal <- f:
		default:
		}
		return
	}
	// Progress events: drop newest when full (persisted state is queryable).
	select {
	case r.progress <- f:
	default:
	}
}

func (r *sseLifecycleRelay) Close() {
	r.mu.Lock()
	r.open = false
	r.mu.Unlock()
}

// drainLifecycleFrames writes all remaining lifecycle frames to the SSE response
// writer. Must be called from the single writer goroutine. r is the
// process-wide secrets redactor forwarded by the SSE caller; nil disables
// redaction (tests only).
func drainLifecycleFrames(w http.ResponseWriter, fl http.Flusher, relay *sseLifecycleRelay, r *secrets.Redactor) {
	for {
		select {
		case f := <-relay.terminal:
			writeSSEFrame(w, fl, f, r)
		case f := <-relay.progress:
			writeSSEFrame(w, fl, f, r)
		default:
			return
		}
	}
}
