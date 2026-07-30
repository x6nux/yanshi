package http

import (
	"encoding/json"
	"net/http"

	"github.com/x6nux/yanshi/internal/store"
)

// Sessions registers GET /api/v1/sessions (list) and
// GET /api/v1/sessions/{id} (messages for a session).
func (s *Server) Sessions(st *store.Store) {
	s.HandleFunc("GET /api/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		sessions, err := st.ListSessions(0)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		type out struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			CreatedAt int64  `json:"created_at"`
			UpdatedAt int64  `json:"updated_at"`
		}
		items := make([]out, 0, len(sessions))
		for _, ss := range sessions {
			items = append(items, out{
				ID:        ss.ID,
				Title:     ss.Title,
				CreatedAt: ss.CreatedAt,
				UpdatedAt: ss.UpdatedAt,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(items)
	})

	s.HandleFunc("GET /api/v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		ss, err := st.GetSession(id)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if ss == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		msgs, err := st.Messages(id)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		type out struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			Seq       int    `json:"seq"`
			CreatedAt int64  `json:"created_at"`
		}
		items := make([]out, 0, len(msgs))
		for _, m := range msgs {
			items = append(items, out{
				Role:      m.Role,
				Content:   m.Content,
				Seq:       m.Seq,
				CreatedAt: m.CreatedAt,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(items)
	})
}
