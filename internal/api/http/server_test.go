package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServer_TokenAuth(t *testing.T) {
	s := New(Config{Token: "secret"})
	s.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.HandleFunc("GET /api/v1/ping", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// healthz is public — no token required.
	resp, err := http.Get(httptest.NewServer(s.Handler()).URL + "/healthz")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Non-loopback protected route without token -> 401.
	req := httptest.NewRequest("GET", "/api/v1/ping", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// Non-loopback protected route with wrong token -> 401.
	req2 := httptest.NewRequest("GET", "/api/v1/ping", nil)
	req2.RemoteAddr = "10.0.0.5:1234"
	req2.Header.Set("Authorization", "Bearer wrong")
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusUnauthorized, rec2.Code)

	// Non-loopback protected route with correct token -> 200.
	req3 := httptest.NewRequest("GET", "/api/v1/ping", nil)
	req3.RemoteAddr = "10.0.0.5:1234"
	req3.Header.Set("Authorization", "Bearer secret")
	rec3 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec3, req3)
	assert.Equal(t, http.StatusOK, rec3.Code)
}

func TestAuth_LoopbackBypassesToken(t *testing.T) {
	s := New(Config{Token: "secret"})
	s.HandleFunc("POST /x", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	// Loopback dial: RemoteAddr will be 127.0.0.1:port. No token sent.
	resp, err := http.Post(ts.URL+"/x", "text/plain", strings.NewReader("hi"))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "loopback must bypass token")
	resp.Body.Close()
}

func TestAuth_NonLoopbackRequiresToken(t *testing.T) {
	s := New(Config{Token: "secret"})
	s.HandleFunc("POST /x", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Drive the handler directly with a spoofed non-loopback RemoteAddr.
	req := httptest.NewRequest("POST", "/x", strings.NewReader("hi"))
	req.RemoteAddr = "10.0.0.5:1234" // non-loopback
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "non-loopback without token must 401")

	req2 := req.Clone(req.Context())
	req2.Header.Set("Authorization", "Bearer secret")
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	assert.Equal(t, http.StatusOK, rec2.Code, "non-loopback with token must pass")
}
