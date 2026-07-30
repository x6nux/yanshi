package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/x6nux/yanshi/internal/guard"
	"github.com/x6nux/yanshi/internal/task"
)

// maxBodyBytes is the maximum allowed size for inbound JSON request bodies
// (1 MiB). Handlers wrap r.Body with http.MaxBytesReader before decoding
// to prevent OOM from oversized payloads.
const maxBodyBytes int64 = 1 << 20

// limitBody wraps r.Body with an http.MaxBytesReader capped at maxBodyBytes.
// The ResponseWriter is needed so MaxBytesReader can set a 413 status on the
// response if the limit is exceeded after the handler starts writing.
func limitBody(w http.ResponseWriter, r *http.Request) io.ReadCloser {
	return http.MaxBytesReader(w, r.Body, maxBodyBytes)
}

// TaskAPI registers the task-related HTTP endpoints on the server:
//   - GET    /api/v1/agent/profile?worker=<name>  — returns the worker's permission profile.
//   - POST   /api/v1/tasks/claim                  — claims the next pending task.
//   - GET    /api/v1/tasks/{id}                   — retrieves a task by id.
func (s *Server) TaskAPI(b *task.Broker, profiles map[string]guard.PermissionProfile) {
	s.HandleFunc("GET /api/v1/agent/profile", func(w http.ResponseWriter, r *http.Request) {
		worker := r.URL.Query().Get("worker")
		prof, ok := profiles[worker]
		if !ok {
			// Fail-closed: unknown workers get a deny-all profile (no tools,
			// no FS, no shell, no network). This overrides the M5 plan's
			// "default permissive" stance — a fail-open default is a security
			// risk because any unrecognised worker would get wildcard access.
			prof = guard.PermissionProfile{
				Shell: guard.ShellPerm{Policy: "deny"},
				Net:   guard.NetPerm{Allow: false},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(prof)
	})

	s.HandleFunc("POST /api/v1/tasks/claim", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Worker string   `json:"worker"`
			Caps   []string `json:"caps"`
		}
		if err := json.NewDecoder(limitBody(w, r)).Decode(&req); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}

		t, err := b.Claim(req.Worker)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if t == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(t)
	})

	s.HandleFunc("GET /api/v1/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		t, err := b.Get(id)
		if err != nil {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(t)
	})

	s.HandleFunc("POST /api/v1/tasks/{id}/result", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		var req struct {
			Worker string `json:"worker"`
			Status string `json:"status"`
			Result string `json:"result"`
		}
		if err := json.NewDecoder(limitBody(w, r)).Decode(&req); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}

		err := b.RecordResult(id, req.Worker, req.Status, req.Result)
		switch {
		case errors.Is(err, task.ErrInvalidStatus):
			http.Error(w, "invalid status", http.StatusBadRequest)
		case errors.Is(err, task.ErrNotOwner):
			http.Error(w, "conflict: task not running or not owned by caller", http.StatusConflict)
		case err != nil:
			http.Error(w, "task not found", http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	s.HandleFunc("POST /api/v1/tasks/{id}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		if err := b.Heartbeat(id); err != nil {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	s.HandleFunc("POST /api/v1/tasks/{id}/progress", func(w http.ResponseWriter, r *http.Request) {
		// No-op store for M5; just acknowledge.
		var req struct {
			Note string `json:"note"`
		}
		_ = json.NewDecoder(limitBody(w, r)).Decode(&req)
		w.WriteHeader(http.StatusOK)
	})

	s.HandleFunc("GET /api/v1/tasks/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		notify := b.Notify()
		ctx := r.Context()

		for {
			select {
			case <-ctx.Done():
				return
			case <-notify:
				fmt.Fprintf(w, "event: task_available\ndata: \n\n")
				flusher.Flush()
			}
		}
	})
}
