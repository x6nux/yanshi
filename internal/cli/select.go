package cli

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// newBackend selects the best transport for a backend at baseURL:
//  1. Try WebSocket at baseURL/api/v1/chat/ws (HTTP GET must return 101).
//  2. On any failure, fall back to SSE at baseURL/api/v1/chat.
//
// baseURL is an http:// or https:// origin (no path).
func newBackend(ctx context.Context, baseURL string) (ChatBackend, error) {
	wsURL := strings.Replace(baseURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1) + "/api/v1/chat/ws"

	if b, ok := tryWS(ctx, wsURL); ok {
		return b, nil
	}
	return newSSEBackend(baseURL), nil
}

// tryWS attempts a WebSocket dial with a short handshake timeout; returns
// (backend, true) on success.
func tryWS(ctx context.Context, wsURL string) (*wsBackend, bool) {
	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, err := newWSBackend(dialCtx, wsURL)
	if err != nil {
		return nil, false
	}
	return b, true
}

// healthz probes baseURL/healthz with a short timeout; true means a live backend.
func healthz(ctx context.Context, baseURL string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, "GET", baseURL+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}
