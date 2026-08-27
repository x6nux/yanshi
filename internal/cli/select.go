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

// probeTimeout bounds a single discovery probe. Discovery runs on the TUI's
// startup path, so a hung backend must not hold the launch: the fallback is
// always "bootstrap our own", which is correct if slow.
const probeTimeout = 300 * time.Millisecond

// ReadyPath is the readiness endpoint discovery prefers. It answers "assembly
// finished and this process can serve", which is the question a second window
// actually has.
const ReadyPath = "/readyz"

// HealthPath is the liveness endpoint. It answers "the process is up", which a
// backend still building its store or VCS also answers with 200 -- so a second
// window that trusted it would connect to a backend that is not yet serving.
const HealthPath = "/healthz"

// ready reports whether a backend at baseURL is ready to serve.
//
// It probes ReadyPath first and falls back to HealthPath on 404. The fallback
// is not optional politeness: during an upgrade the running owner is an OLD
// binary with no /readyz route, and a new client that treated 404 as "not
// ready" would decline to join it, bootstrap a second backend, and lose the
// owner election every time -- so every window in the project would fail to
// find the backend that is actually running.
//
// Any status other than 200 or 404 on ReadyPath is taken at face value (not
// ready) and is NOT retried against HealthPath. A 503 from /readyz is a
// backend that exists and is telling us it is still assembling; asking a
// second endpoint would only find the liveness answer we already know is the
// wrong question.
func ready(ctx context.Context, baseURL string) bool {
	switch probe(ctx, baseURL+ReadyPath) {
	case http.StatusOK:
		return true
	case http.StatusNotFound:
		// Old backend without the route: fall back to liveness.
		return probe(ctx, baseURL+HealthPath) == http.StatusOK
	default:
		return false
	}
}

// probe issues one short GET and returns the status code, or 0 when the
// request could not be made at all (connection refused, timeout, bad URL).
// Zero is distinct from every real status, so callers never confuse "no
// backend there" with "backend said no".
func probe(ctx context.Context, url string) int {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, "GET", url, nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
