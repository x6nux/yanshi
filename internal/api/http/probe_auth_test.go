package http

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProbeEndpointsAreUnauthenticatedFromRemote pins the O7 auth exemption
// table.
//
// It drives every case from a NON-LOOPBACK RemoteAddr, which is the whole
// point: the loopback bypass already lets a local client reach anything, so a
// test that dials 127.0.0.1 passes whether or not the exemption exists. The
// caller this protects is a remote supervisor — a load balancer, a container
// orchestrator, an uptime check — and none of those hold a token.
//
// The failure mode being pinned is specific and one-directional: a readiness
// probe answered with 401 reads to every supervisor as "not ready", so a
// backend that is serving perfectly is taken out of rotation and stays out.
func TestProbeEndpointsAreUnauthenticatedFromRemote(t *testing.T) {
	s := New(Config{Token: "secret"})
	for _, p := range []string{"/healthz", "/readyz"} {
		s.HandleFunc("GET "+p, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	}
	s.HandleFunc("GET /api/v1/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name string
		path string
		want int
	}{
		{"liveness is public", "/healthz", http.StatusOK},
		{"readiness is public", "/readyz", http.StatusOK},
		// Sub-paths are exempt from AUTH but have no route, so they reach the
		// mux and 404. That distinction is the assertion: 404 proves the
		// request got past auth, and 401 would prove it did not.
		{"liveness sub-path reaches the mux", "/healthz/db", http.StatusNotFound},
		{"readiness sub-path reaches the mux", "/readyz/db", http.StatusNotFound},
		// The exemption must be anchored: a route that merely STARTS with the
		// probe name is a different route and stays protected. Without the
		// separator in the prefix test, "/readyzzz" or an attacker-chosen
		// "/healthzsecrets" would inherit the exemption.
		{"a longer name is not the probe", "/readyzzz", http.StatusUnauthorized},
		{"a longer liveness name is not the probe", "/healthzsecrets", http.StatusUnauthorized},
		{"ordinary API stays protected", "/api/v1/ping", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.path, nil)
			req.RemoteAddr = "10.0.0.5:1234" // deliberately not loopback
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			assert.Equal(t, tc.want, rec.Code)
		})
	}
}

// TestProbeEndpointsDiscloseNothing asserts both probes answer with an empty
// body.
//
// They are the only two routes any unauthenticated remote client can reach, so
// "what do they say" is a security question and not a cosmetic one. A future
// edit that made /readyz report which subsystem is still assembling would hand
// an unauthenticated caller the deployment's component inventory.
func TestProbeEndpointsDiscloseNothing(t *testing.T) {
	s := New(Config{Token: "secret"})
	for _, p := range []string{"/healthz", "/readyz"} {
		s.HandleFunc("GET "+p, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	for _, p := range []string{"/healthz", "/readyz"} {
		resp, err := ts.Client().Get(ts.URL + p)
		require.NoError(t, err)
		body := make([]byte, 1)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, p)
		assert.Zero(t, n, "%s must answer with an empty body", p)
	}
}

// TestIsPublicProbePath is the table for the predicate itself, kept separate
// from the handler test so a change to the path set fails on the rule rather
// than on the six HTTP round trips above.
func TestIsPublicProbePath(t *testing.T) {
	cases := map[string]bool{
		"/healthz":         true,
		"/readyz":          true,
		"/healthz/":        true,
		"/readyz/":         true,
		"/healthz/db":      true,
		"/readyz/store":    true,
		"/healthzz":        false,
		"/readyzz":         false,
		"/readyz-internal": false,
		"/api/v1/chat":     false,
		"/":                false,
		"":                 false,
		// A prefix that CONTAINS a probe path but does not start with it must
		// not match: an exemption keyed on containment would exempt any route
		// an attacker can get into the path.
		"/api/healthz": false,
	}
	for path, want := range cases {
		assert.Equal(t, want, isPublicProbePath(path), "isPublicProbePath(%q)", path)
	}
}
