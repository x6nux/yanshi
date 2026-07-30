package http

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/store"
)

// doAuthed creates a GET request with the Bearer token set.
func doAuthed(t *testing.T, method, url, token string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func TestSessions_ListAndDetail(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	defer st.Close()

	// Create a session and append a message.
	sid, err := st.CreateSession("test session")
	require.NoError(t, err)
	require.NoError(t, st.AppendMessage(sid, 0, "user", "hello"))
	require.NoError(t, st.AppendMessage(sid, 1, "assistant", "world"))

	s := New(Config{Token: "tok"})
	s.Sessions(st)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// GET /api/v1/sessions — list
	resp, err := ts.Client().Do(doAuthed(t, "GET", ts.URL+"/api/v1/sessions", "tok"))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	var list []map[string]any
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list, 1)
	assert.Equal(t, sid, list[0]["id"])
	assert.Equal(t, "test session", list[0]["title"])

	// GET /api/v1/sessions/{id} — messages
	resp2, err := ts.Client().Do(doAuthed(t, "GET", ts.URL+"/api/v1/sessions/"+sid, "tok"))
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	body2, _ := io.ReadAll(resp2.Body)
	var msgs []map[string]any
	require.NoError(t, json.Unmarshal(body2, &msgs))
	require.Len(t, msgs, 2)
	assert.Equal(t, "user", msgs[0]["role"])
	assert.Equal(t, "hello", msgs[0]["content"])
	assert.Equal(t, "assistant", msgs[1]["role"])
	assert.Equal(t, "world", msgs[1]["content"])

	// GET /api/v1/sessions/nonexistent — 404
	resp3, err := ts.Client().Do(doAuthed(t, "GET", ts.URL+"/api/v1/sessions/nonexistent", "tok"))
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp3.StatusCode)
}

func TestSessions_Unauthorized(t *testing.T) {
	st, err := store.Open(":memory:")
	require.NoError(t, err)
	defer st.Close()

	s := New(Config{Token: "tok"})
	s.Sessions(st)
	handler := s.Handler()

	// Use a non-loopback RemoteAddr so the request does not bypass auth.
	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// With correct token on non-loopback, it should pass.
	req2 := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req2.RemoteAddr = "10.0.0.5:1234"
	req2.Header.Set("Authorization", "Bearer tok")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	assert.NotEqual(t, http.StatusUnauthorized, rec2.Code, "non-loopback with token should not be unauthorized")
}
