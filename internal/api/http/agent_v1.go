package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/x6nux/yanshi/internal/api/v1"
)

// AgentV1 registers the versioned v1 resource API beside the legacy chat routes
// on the existing Server mux. The handlers are thin: they decode JSON (unknown
// fields ignored), call into the shared *v1.Service, set the
// X-Yanshi-API-Version header, and write JSON or SSE responses. All turn/item
// semantics live in the service so HTTP and the JSON-RPC app-server agree.
//
// Routes registered:
//   - POST /api/v1/thread/start     — create a thread
//   - POST /api/v1/thread/resume    — load a thread by id (with items snapshot)
//   - POST /api/v1/thread/interrupt — cancel a thread's active turn (idempotent)
//   - POST /api/v1/turn/start       — start a turn; SSE-streams items
//   - GET  /api/v1/schema/agent-v1.json — v1 JSON Schema document
//
// SSE framing: turn/start emits `event: turn` once with the TurnStartResponse,
// then `event: item` per Item, then closes. Clients ignore the legacy
// agent_chunk event name (v1 items are the contract).
func (s *Server) AgentV1(agent *v1.Service) {
	s.HandleFunc("POST /api/v1/thread/start", func(w http.ResponseWriter, r *http.Request) {
		var p v1.ThreadStartParams
		if err := decodeAgentJSON(w, r, &p); err != nil {
			return
		}
		thread, err := agent.Start(r.Context(), p)
		if err != nil {
			writeAgentError(w, http.StatusInternalServerError, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, v1.ThreadStartResponse{Version: v1.Version, Thread: thread})
	})

	s.HandleFunc("POST /api/v1/thread/resume", func(w http.ResponseWriter, r *http.Request) {
		var p v1.ThreadResumeParams
		if err := decodeAgentJSON(w, r, &p); err != nil {
			return
		}
		snapshot, err := agent.Resume(r.Context(), p)
		if errors.Is(err, v1.ErrThreadNotFound) {
			writeAgentError(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeAgentError(w, http.StatusInternalServerError, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, v1.ThreadResumeResponse{
			Version: v1.Version, Thread: snapshot.Thread,
		})
	})

	s.HandleFunc("POST /api/v1/thread/interrupt", func(w http.ResponseWriter, r *http.Request) {
		var p v1.ThreadInterruptParams
		if err := decodeAgentJSON(w, r, &p); err != nil {
			return
		}
		if err := agent.Interrupt(r.Context(), p); err != nil {
			if errors.Is(err, v1.ErrThreadNotFound) {
				writeAgentError(w, http.StatusNotFound, err)
				return
			}
			// Stale TurnID / concurrent interrupt — 409 Conflict is the precise
			// semantic for "the resource state does not allow this mutation".
			writeAgentError(w, http.StatusConflict, err)
			return
		}
		writeAgentJSON(w, http.StatusOK, v1.InterruptResponse{
			Version: v1.Version, OK: true, ThreadID: p.ThreadID, TurnID: p.TurnID,
		})
	})

	s.HandleFunc("POST /api/v1/turn/start", func(w http.ResponseWriter, r *http.Request) {
		var p v1.TurnStartParams
		if err := decodeAgentJSON(w, r, &p); err != nil {
			return
		}
		started, items, err := agent.StartTurn(r.Context(), p)
		if errors.Is(err, v1.ErrThreadNotFound) {
			writeAgentError(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			// Already-active turn / blank input / etc. — 409 Conflict.
			writeAgentError(w, http.StatusConflict, err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Yanshi-API-Version", v1.Version)
		fl, _ := w.(http.Flusher)
		// First SSE event carries the TurnStartResponse so the client knows the
		// turn id + thread id before items arrive. Subsequent events are items.
		writeAgentSSE(w, fl, "turn", started)
		for item := range items {
			if !writeAgentSSE(w, fl, "item", item) {
				// Client disconnected (write failed). Stop draining; the service
				// releases its producer goroutine on ctx.Done from r.Context().
				return
			}
		}
	})

	s.HandleFunc("GET /api/v1/schema/agent-v1.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/schema+json")
		w.Header().Set("X-Yanshi-API-Version", v1.Version)
		_, _ = w.Write(v1.SchemaBytes())
	})
}

// decodeAgentJSON reads a JSON request body into dst using standard
// encoding/json semantics (unknown fields ignored, per the v1 contract). A
// decode failure writes a 400 response and returns the error so the handler
// can early-return. Body size is capped via limitBody (1 MiB) to prevent OOM.
func decodeAgentJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(limitBody(w, r))
	if err := dec.Decode(dst); err != nil {
		writeAgentError(w, http.StatusBadRequest, fmt.Errorf("invalid request: %w", err))
		return err
	}
	return nil
}

// writeAgentJSON writes value as JSON with the X-Yanshi-API-Version header and
// the given HTTP status. Used for non-streaming responses (thread/start,
// thread/resume, errors).
func writeAgentJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Yanshi-API-Version", v1.Version)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// writeAgentError writes a v1 error response body
// (`{"version":"v1","error":{"message":"..."}}`) with the X-Yanshi-API-Version
// header. The status code is the caller's choice (400/404/409/500).
func writeAgentError(w http.ResponseWriter, status int, err error) {
	writeAgentJSON(w, status, map[string]any{
		"version": v1.Version,
		"error":   map[string]string{"message": err.Error()},
	})
}

// writeAgentSSE writes one SSE event to w. Returns false on write failure so
// the streaming caller can stop draining and release its goroutine when the
// client disconnects. Each event is `event: <name>\ndata: <json>\n\n` per the
// SSE wire format; a non-nil http.Flusher is flushed so streaming clients see
// each event immediately.
func writeAgentSSE(w http.ResponseWriter, fl http.Flusher, event string, value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return false
	}
	if fl != nil {
		fl.Flush()
	}
	return true
}
