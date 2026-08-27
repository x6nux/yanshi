package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// readyFixture builds a test backend whose /readyz and /healthz statuses are
// each pinned to a fixed code. A status of 0 means the route is not registered
// at all, which is how an OLD backend (one built before /readyz existed)
// behaves: the mux answers 404.
type readyFixture struct {
	readyzStatus  int
	healthzStatus int
}

// server returns the fixture backend plus per-path REQUEST counters. The
// counters are incremented by an outer wrapper rather than inside the
// handlers, so an unregistered route still records that it was ASKED -- which
// is exactly the case ("no healthz route at all") where a handler-side counter
// would report zero and make the fallback look like it never ran.
func (f readyFixture) server(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var readyzHits, healthzHits atomic.Int32
	mux := http.NewServeMux()
	if f.readyzStatus != 0 {
		mux.HandleFunc(ReadyPath, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(f.readyzStatus)
		})
	}
	if f.healthzStatus != 0 {
		mux.HandleFunc(HealthPath, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(f.healthzStatus)
		})
	}
	counting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ReadyPath:
			readyzHits.Add(1)
		case HealthPath:
			healthzHits.Add(1)
		}
		mux.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(counting)
	t.Cleanup(ts.Close)
	return ts, &readyzHits, &healthzHits
}

// TestReadyProbeSemantics is the O7 table: the discovery probe must answer
// "can this backend serve", and must keep working against a backend too old to
// have a readiness route.
func TestReadyProbeSemantics(t *testing.T) {
	cases := []struct {
		name string
		readyFixture
		want         bool
		wantFellBack bool
		why          string
	}{
		{
			name:         "new backend, assembled",
			readyFixture: readyFixture{readyzStatus: 200, healthzStatus: 200},
			want:         true,
			why:          "readyz says yes; no fallback needed",
		},
		{
			name:         "new backend, still assembling",
			readyFixture: readyFixture{readyzStatus: 503, healthzStatus: 200},
			want:         false,
			why: "this is the whole point of O7: the process is alive (healthz 200) " +
				"but not serving yet, and the old liveness probe said join it",
		},
		{
			name:         "old backend without readyz, alive",
			readyFixture: readyFixture{healthzStatus: 200},
			want:         true,
			wantFellBack: true,
			why: "during an upgrade the running owner has no readyz; treating 404 as " +
				"not-ready would make every window bootstrap its own backend",
		},
		{
			name:         "old backend without readyz, unhealthy",
			readyFixture: readyFixture{healthzStatus: 503},
			want:         false,
			wantFellBack: true,
			why:          "fallback must still respect the liveness answer",
		},
		{
			name:         "neither route registered",
			readyFixture: readyFixture{},
			want:         false,
			wantFellBack: true,
			why:          "a server that is not yanshi is not a backend to join",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, readyzHits, healthzHits := tc.readyFixture.server(t)
			got := ready(context.Background(), ts.URL)
			require.Equal(t, tc.want, got, tc.why)
			if tc.wantFellBack {
				require.Positive(t, healthzHits.Load(),
					"a 404 on readyz must fall back to healthz")
			}
			_ = readyzHits
		})
	}
}

// TestReadyDoesNotFallBackOnNon404 pins the narrowness of the compatibility
// escape hatch. A 503 from /readyz is a backend that EXISTS and is telling us
// it cannot serve; retrying against /healthz would find the liveness answer we
// already know is the wrong question, and would re-introduce exactly the bug
// O7 removes.
func TestReadyDoesNotFallBackOnNon404(t *testing.T) {
	ts, _, healthzHits := readyFixture{readyzStatus: 503, healthzStatus: 200}.server(t)
	require.False(t, ready(context.Background(), ts.URL))
	require.Zero(t, healthzHits.Load(),
		"a non-404 readyz answer is final; healthz must not be consulted")
}

// TestReadyPrefersReadyzOverHealthz proves the ORDER: readyz is consulted
// first, so a backend that answers both does not depend on the liveness route
// at all.
func TestReadyPrefersReadyzOverHealthz(t *testing.T) {
	ts, readyzHits, healthzHits := readyFixture{readyzStatus: 200, healthzStatus: 500}.server(t)
	require.True(t, ready(context.Background(), ts.URL))
	require.Equal(t, int32(1), readyzHits.Load())
	require.Zero(t, healthzHits.Load(),
		"a 200 from readyz is sufficient; healthz must not be consulted")
}

// TestProbeReportsZeroWhenUnreachable pins the sentinel: probe returns 0, not a
// status, when the request could not be made. Callers rely on 0 being distinct
// from every real code so "no backend there" never reads as "backend said 404"
// and triggers the compatibility fallback against a host that does not exist.
func TestProbeReportsZeroWhenUnreachable(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{name: "connection refused", url: "http://127.0.0.1:1" + ReadyPath},
		{name: "malformed url", url: "http://[::1" + ReadyPath},
		{name: "empty url", url: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Zero(t, probe(context.Background(), tc.url))
		})
	}
}

// TestReadyRespectsCancelledContext asserts discovery cannot outlive its
// caller: a cancelled context yields "not ready" rather than blocking the TUI
// launch on a hung backend.
func TestReadyRespectsCancelledContext(t *testing.T) {
	ts, _, _ := readyFixture{readyzStatus: 200}.server(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, ready(ctx, ts.URL))
}

// TestProbePathConstantsAreDistinct guards against a copy-paste that would
// point both probes at the same route, which would silently restore the
// liveness-only behaviour while every test above kept passing.
func TestProbePathConstantsAreDistinct(t *testing.T) {
	require.NotEqual(t, ReadyPath, HealthPath)
	require.Equal(t, "/readyz", ReadyPath)
	require.Equal(t, "/healthz", HealthPath)
}
