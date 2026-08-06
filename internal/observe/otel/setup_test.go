package otelobs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// otlpStub returns a server that acknowledges OTLP/HTTP export requests with
// an empty (zero-value) protobuf response. Both the trace and metric HTTP
// exporters unmarshal the empty body successfully, so we can drive the real
// export path against it without an OTLP collector binary.
func otlpStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// httpFactories builds real OTLP/HTTP exporters pointed at an HTTP stub. The
// production defaults use the SDK's HTTPS scheme, which cannot talk to an HTTP
// test server; the factory seam lets the test exercise the genuine OTLP export
// path (New → Export → Shutdown) over plain HTTP without altering production
// behavior.
func httpFactories(endpoint string) factories {
	return factories{
		trace: func(ctx context.Context, _ Config) (sdktrace.SpanExporter, error) {
			return otlptracehttp.New(ctx,
				otlptracehttp.WithEndpoint(endpoint),
				otlptracehttp.WithInsecure())
		},
		metric: func(ctx context.Context, _ Config) (sdkmetric.Exporter, error) {
			return otlpmetrichttp.New(ctx,
				otlpmetrichttp.WithEndpoint(endpoint),
				otlpmetrichttp.WithInsecure())
		},
	}
}

func TestCollectorAvailable(t *testing.T) {
	// Empty endpoint optimistically returns true and defers failure to the
	// exporter construction path.
	if !collectorAvailable(context.Background(), "") {
		t.Fatal("empty endpoint must be optimistic-true")
	}
	// A closed port on loopback is unreachable.
	if collectorAvailable(context.Background(), "127.0.0.1:1") {
		t.Fatal("closed port must report unreachable")
	}
	// A live server is reachable.
	srv := httptest.NewServer(nil)
	defer srv.Close()
	if !collectorAvailable(context.Background(), srv.Listener.Addr().String()) {
		t.Fatal("live server must report reachable")
	}
}

func TestProductionExportersConstruct(t *testing.T) {
	ctx := context.Background()
	srv := otlpStub(t)
	endpoint := srv.Listener.Addr().String()

	te, err := traceExporter(ctx, Config{Endpoint: endpoint})
	if err != nil || te == nil {
		t.Fatalf("traceExporter(endpoint) err=%v", err)
	}
	_ = te.Shutdown(ctx)

	me, err := metricExporter(ctx, Config{Endpoint: endpoint})
	if err != nil || me == nil {
		t.Fatalf("metricExporter(endpoint) err=%v", err)
	}
	_ = me.Shutdown(ctx)

	// Empty endpoint uses the OTLP default and still constructs without dialing.
	te2, err := traceExporter(ctx, Config{})
	if err != nil || te2 == nil {
		t.Fatalf("traceExporter(default) err=%v", err)
	}
	_ = te2.Shutdown(ctx)

	me2, err := metricExporter(ctx, Config{})
	if err != nil || me2 == nil {
		t.Fatalf("metricExporter(default) err=%v", err)
	}
	_ = me2.Shutdown(ctx)
}

// TestSetupWithLiveCollectorFlushesCleanly drives the full happy path through
// setupWithFactories: providers are constructed, OTel globals are set, the
// runtime is enabled, a real span is recorded, and Shutdown flushes both
// exporters to the HTTP stub without error.
//
// ledger: C4/OBS2#1 trace 链可导出
func TestSetupWithLiveCollectorFlushesCleanly(t *testing.T) {
	restoreOTelGlobals(t)
	resetOTelNoop()
	srv := otlpStub(t)
	endpoint := srv.Listener.Addr().String()

	rt := setupWithFactories(context.Background(),
		Config{Enabled: true, Endpoint: endpoint, ServiceName: "yanshi-test", SampleRatio: 1},
		httpFactories(endpoint))
	if !rt.Enabled() {
		t.Fatal("live collector must produce an enabled Runtime")
	}
	if rt.Tracer("github.com/x6nux/yanshi") == nil {
		t.Fatal("enabled Runtime.Tracer must not be nil")
	}
	if rt.Meter("github.com/x6nux/yanshi") == nil {
		t.Fatal("enabled Runtime.Meter must not be nil")
	}

	// Record a real span + a metric so Shutdown exercises the export path,
	// not just an empty flush.
	ctx := context.Background()
	_, span := rt.Tracer("github.com/x6nux/yanshi").Start(ctx, "test.span")
	span.End()
	counter, cerr := rt.Meter("github.com/x6nux/yanshi").Int64Counter("yanshi.test.counter")
	if cerr != nil {
		t.Fatalf("create counter: %v", cerr)
	}
	counter.Add(ctx, 1)

	if err := rt.Shutdown(ctx); err != nil {
		t.Fatalf("enabled Runtime.Shutdown must flush cleanly: %v", err)
	}
}

// TestSetupLiveCollectorReachesDefaultFactories covers Setup's collector-
// available branch (line: return setupWithFactories(ctx, cfg, defaults)). The
// production defaults use HTTPS, so the final metric export on Shutdown hits a
// transport error against the HTTP stub — that export quirk is out of scope
// here; this test proves Setup wires an enabled runtime with live providers.
func TestSetupLiveCollectorReachesDefaultFactories(t *testing.T) {
	restoreOTelGlobals(t)
	resetOTelNoop()
	srv := otlpStub(t)

	rt := Setup(context.Background(), Config{
		Enabled:     true,
		Endpoint:    srv.Listener.Addr().String(),
		ServiceName: "yanshi-test",
		SampleRatio: 1,
	})
	if !rt.Enabled() {
		t.Fatal("Setup with a live collector must produce an enabled Runtime")
	}
	if rt.Tracer("github.com/x6nux/yanshi") == nil || rt.Meter("github.com/x6nux/yanshi") == nil {
		t.Fatal("Setup must install bound tracer and meter providers")
	}
	// Shutdown releases the periodic-reader goroutine; the HTTPS-vs-HTTP
	// transport error is expected and harmless for this wiring test.
	_ = rt.Shutdown(context.Background())
}

func TestSetupCollectorUnreachableDegradesToNoop(t *testing.T) {
	restoreOTelGlobals(t)
	resetOTelNoop()
	rt := Setup(context.Background(), Config{
		Enabled:     true,
		Endpoint:    "127.0.0.1:1",
		ServiceName: "yanshi-test",
	})
	if rt.Enabled() {
		t.Fatal("unreachable collector must soft-degrade to no-op")
	}
	if err := rt.Shutdown(context.Background()); err != nil {
		t.Fatalf("degraded Runtime.Shutdown err=%v", err)
	}
}

// fakeTraceExporter counts Shutdown calls so the metric-failure path can prove
// it cleans up the already-built trace exporter before returning no-op.
type fakeTraceExporter struct{ shutdowns int }

func (e *fakeTraceExporter) ExportSpans(_ context.Context, _ []sdktrace.ReadOnlySpan) error {
	return nil
}
func (e *fakeTraceExporter) Shutdown(_ context.Context) error {
	e.shutdowns++
	return nil
}

func TestSetupMetricExporterFailureShutsTraceDown(t *testing.T) {
	restoreOTelGlobals(t)
	resetOTelNoop()
	te := &fakeTraceExporter{}
	rt := setupWithFactories(context.Background(), Config{Enabled: true, SampleRatio: 1}, factories{
		trace: func(context.Context, Config) (sdktrace.SpanExporter, error) { return te, nil },
		metric: func(context.Context, Config) (sdkmetric.Exporter, error) {
			return nil, errors.New("metric exporter unavailable")
		},
	})
	if rt.Enabled() {
		t.Fatal("metric exporter failure must soft-degrade to no-op")
	}
	if te.shutdowns != 1 {
		t.Fatalf("trace exporter must be shut down once on metric failure; got %d", te.shutdowns)
	}
}

func TestSetupTraceExporterFailureSoftDegrades(t *testing.T) {
	restoreOTelGlobals(t)
	resetOTelNoop()
	rt := setupWithFactories(context.Background(), Config{Enabled: true, SampleRatio: 1}, factories{
		trace: func(context.Context, Config) (sdktrace.SpanExporter, error) {
			return nil, errors.New("trace exporter unavailable")
		},
		metric: func(context.Context, Config) (sdkmetric.Exporter, error) {
			return nil, errors.New("must not be reached")
		},
	})
	if rt.Enabled() {
		t.Fatal("trace exporter failure must soft-degrade to no-op")
	}
}

func TestNoopRuntimeAccessors(t *testing.T) {
	// A nil *Runtime is valid per the doc comment.
	var nilRT *Runtime
	if nilRT.Enabled() {
		t.Fatal("nil Runtime must not be enabled")
	}
	if nilRT.Tracer("x") == nil {
		t.Fatal("nil Runtime.Tracer must return a no-op tracer, not nil")
	}
	if nilRT.Meter("x") == nil {
		t.Fatal("nil Runtime.Meter must return a no-op meter, not nil")
	}
	if err := nilRT.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil Runtime.Shutdown err=%v", err)
	}

	// A disabled (Noop) Runtime still returns usable no-op instruments.
	disabled := Noop()
	if disabled.Enabled() {
		t.Fatal("Noop() must not be enabled")
	}
	if disabled.Tracer("x") == nil || disabled.Meter("x") == nil {
		t.Fatal("disabled Runtime must return no-op instruments")
	}
	if err := disabled.Shutdown(context.Background()); err != nil {
		t.Fatalf("disabled Runtime.Shutdown err=%v", err)
	}
}

func TestNormalizeConfigClampsAndDefaults(t *testing.T) {
	cfg := normalizeConfig(Config{SampleRatio: 5})
	if cfg.ServiceName != "yanshi" || cfg.SampleRatio != 1 {
		t.Fatalf("clamp-high/default-name failed: %+v", cfg)
	}
	cfg = normalizeConfig(Config{ServiceName: "custom", SampleRatio: -3})
	if cfg.ServiceName != "custom" || cfg.SampleRatio != 0 {
		t.Fatalf("clamp-low/keep-name failed: %+v", cfg)
	}
}
