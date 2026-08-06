package http

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/orchestrator"
	"github.com/x6nux/yanshi/internal/features"
	einollm "github.com/x6nux/yanshi/internal/llm/eino"
	obslog "github.com/x6nux/yanshi/internal/observe/log"
	"github.com/x6nux/yanshi/internal/proto"
)

// newTestRegistry builds a registry with the production specs so the flags
// under test carry their real defaults.
func newTestRegistry(t *testing.T) *features.Registry {
	t.Helper()
	reg := features.NewRegistry(false)
	for _, spec := range features.DefaultSpecs() {
		reg.Register(spec)
	}
	return reg
}

// captureLogs redirects the default logger through the production redacting
// handler into a buffer. It has to be the production handler: the trace id is
// injected by that handler from the context, so a plain slog.TextHandler would
// show nothing regardless of how the flag is set, and the test would pass for
// the wrong reason.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(obslog.New(obslog.Config{Level: "info", Writer: &buf}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// runWSTurn drives one complete WS turn against a server and returns nothing;
// callers assert on whatever side channel they installed beforehand.
func runWSTurn(t *testing.T, s *Server, o *orchestrator.Orchestrator, models map[string]model.BaseChatModel) {
	t.Helper()
	s.ChatWS(o, models, nil)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
	defer c.Close()
	require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))
	for {
		if recvFrame(t, c).Type == "done" {
			break
		}
	}
}

func newFlagTestServer(t *testing.T, reg *features.Registry) (*Server, *orchestrator.Orchestrator, map[string]model.BaseChatModel) {
	t.Helper()
	fm := einollm.NewFakeModel([]string{"hello"}, nil)
	o, err := orchestrator.New(orchestrator.Config{Model: fm})
	require.NoError(t, err)
	s := New(Config{Token: "t", FeaturesReg: reg, PriceTab: map[string]einollm.ModelPricing{
		"fake-1": {InputPerM: 1, OutputPerM: 2},
	}})
	return s, o, map[string]model.BaseChatModel{"fake-1": fm}
}

// TestSlogTraceIDFlagGatesCorrelationIDs pins the first of two flags that were
// registered, listed by /features, toggleable by the user -- and read by
// nothing outside their own package tests. Only observe.otel_export had a real
// consumer, so "new features can be dark-launched" was true for one flag in
// three, and toggling either of the other two changed nothing at all.
func TestSlogTraceIDFlagGatesCorrelationIDs(t *testing.T) {
	t.Run("on by default: ids reach the log", func(t *testing.T) {
		buf := captureLogs(t)
		reg := newTestRegistry(t)
		s, o, models := newFlagTestServer(t, reg)
		runWSTurn(t, s, o, models)
		if !strings.Contains(buf.String(), "trace_id") {
			t.Fatalf("no trace_id in the turn's log lines: %s", buf.String())
		}
	})

	t.Run("off: no ids anywhere in the turn's logs", func(t *testing.T) {
		buf := captureLogs(t)
		reg := newTestRegistry(t)
		require.NoError(t, reg.Set("observe.slog_trace_id", false))
		s, o, models := newFlagTestServer(t, reg)
		runWSTurn(t, s, o, models)
		for _, key := range []string{"trace_id", "session_id", "turn_id"} {
			if strings.Contains(buf.String(), key) {
				t.Errorf("%s present after the flag was turned off: %s", key, buf.String())
			}
		}
	})
}

// TestCostInStatusFlagGatesTheStatusFrame is the same defect for the second
// flag: turning cost reporting off left every cost field on the wire.
func TestCostInStatusFlagGatesTheStatusFrame(t *testing.T) {
	run := func(t *testing.T, reg *features.Registry) proto.ServerFrame {
		t.Helper()
		fm := einollm.NewFakeModel([]string{"hello"}, nil)
		o, err := orchestrator.New(orchestrator.Config{Model: fm})
		require.NoError(t, err)
		s := New(Config{Token: "t", FeaturesReg: reg, PriceTab: map[string]einollm.ModelPricing{
			"fake-1": {InputPerM: 1, OutputPerM: 2},
		}})
		s.ChatWS(o, map[string]model.BaseChatModel{"fake-1": fm}, nil)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()
		c := dial(t, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/chat/ws")
		defer c.Close()
		require.NoError(t, c.WriteJSON(proto.NewUserMessage("hi")))
		var status proto.ServerFrame
		for {
			f := recvFrame(t, c)
			if f.Type == "status" {
				status = f
			}
			if f.Type == "done" {
				break
			}
		}
		return status
	}

	t.Run("on by default: the cost fields are populated", func(t *testing.T) {
		st := run(t, newTestRegistry(t))
		if !st.CostKnown {
			t.Fatalf("cost_known false with a priced model and the flag on: %+v", st)
		}
	})

	t.Run("off: no cost reaches the client", func(t *testing.T) {
		reg := newTestRegistry(t)
		require.NoError(t, reg.Set("observe.cost_in_status", false))
		st := run(t, reg)
		if st.CostKnown || st.CostUSD != 0 {
			t.Fatalf("cost still on the wire after the flag was turned off: "+
				"cost_known=%v cost_usd=%v", st.CostKnown, st.CostUSD)
		}
	})
}

// TestNilRegistryFallsBackToRegisteredDefaults is the trap that makes the
// naive fix worse than no fix.
//
// Registry.Enabled returns false for a nil receiver -- correct for a flag
// whose default is off, and silently WRONG for observe.slog_trace_id, whose
// default is true. s.featuresReg is nil on every path that builds a Server
// without one (every existing test in this package, and any embedder that
// doesn't pass FeaturesReg), so gating on Enabled directly would have turned
// off a stable, default-on feature for those callers and left no trace of why.
func TestNilRegistryFallsBackToRegisteredDefaults(t *testing.T) {
	var nilReg *features.Registry
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"observe.slog_trace_id", true},   // Stable, Default: true
		{"observe.cost_in_status", true},  // Beta, Default: true
		{"observe.otel_export", false},    // Experimental, Default: false
		{"observe.does_not_exist", false}, // unknown: safe default
	} {
		if got := nilReg.EnabledOrDefault(tc.key); got != tc.want {
			t.Errorf("nil registry: EnabledOrDefault(%q) = %v, want the registered default %v",
				tc.key, got, tc.want)
		}
	}

	// A real registry still wins over the default -- otherwise the helper
	// would be a constant and the flags would be unusable in the other
	// direction.
	reg := newTestRegistry(t)
	require.NoError(t, reg.Set("observe.slog_trace_id", false))
	if reg.EnabledOrDefault("observe.slog_trace_id") {
		t.Error("an explicit off was overridden by the registered default")
	}
}
