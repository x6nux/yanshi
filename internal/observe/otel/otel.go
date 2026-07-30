// Package otelobs owns yanshi's OpenTelemetry providers and OTLP exporters.
// Callers receive a Runtime even when setup fails: exporter failures are
// deliberately soft because observability must never prevent the agent booting
// (constraint 5 in the C4 plan). The process-global TracerProvider and
// MeterProvider are updated by Setup and restored at test end.
package otelobs

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Config is intentionally provider-neutral. Endpoint is the OTLP/HTTP
// host:port accepted by WithEndpoint, for example "127.0.0.1:4318".
type Config struct {
	Enabled     bool
	Endpoint    string
	ServiceName string
	SampleRatio float64
}

// factories is a test seam: the production default uses OTLPHTTP exporters;
// tests inject ones that return deterministic errors without hitting the
// network.
type factories struct {
	trace  func(context.Context, Config) (sdktrace.SpanExporter, error)
	metric func(context.Context, Config) (sdkmetric.Exporter, error)
}

// Runtime holds the live or no-op OTel providers. Enabled() tells callers
// whether they should create real spans (true) or skip (false). Shutdown
// flushes and closes the exporters. A nil *Runtime is valid (Enabled=false,
// Shutdown is a no-op) so callers that gate their telemetry behind an
// optional Runtime field do not need nil checks.
type Runtime struct {
	enabled    bool
	traces     *sdktrace.TracerProvider
	metrics    *sdkmetric.MeterProvider
	noopTracer trace.TracerProvider
	noopMeter  metric.MeterProvider
}

// normalizeConfig clamps SampleRatio to [0,1] and defaults the service name
// to "yanshi". It is exported so bootstrap can normalize before logging the
// computed config. It does NOT touch Enabled or Endpoint.
func normalizeConfig(cfg Config) Config {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "yanshi"
	}
	if cfg.SampleRatio < 0 {
		cfg.SampleRatio = 0
	}
	if cfg.SampleRatio > 1 {
		cfg.SampleRatio = 1
	}
	return cfg
}

// traceExporter builds the production OTLP/HTTP span exporter. An empty
// Endpoint means use the OTLP default (localhost:4318).
func traceExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	opts := make([]otlptracehttp.Option, 0, 1)
	if cfg.Endpoint != "" {
		opts = append(opts, otlptracehttp.WithEndpoint(cfg.Endpoint))
	}
	return otlptracehttp.New(ctx, opts...)
}

// metricExporter builds the production OTLP/HTTP metric exporter.
func metricExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	opts := make([]otlpmetrichttp.Option, 0, 1)
	if cfg.Endpoint != "" {
		opts = append(opts, otlpmetrichttp.WithEndpoint(cfg.Endpoint))
	}
	return otlpmetrichttp.New(ctx, opts...)
}

var defaults = factories{trace: traceExporter, metric: metricExporter}

// Noop returns a disabled Runtime with the process globals set to no-op
// providers. Bootstrap calls this when OTel is disabled or collector-probe
// fails. It does NOT mutate the OTel globals by default — use installNoop or
// call Setup with Enabled=false for that.
func Noop() *Runtime {
	return &Runtime{
		noopTracer: tracenoop.NewTracerProvider(),
		noopMeter:  metricnoop.NewMeterProvider(),
	}
}

// installNoop sets the no-op process globals (not enabled). Returns the
// Runtime so the caller can still call Shutdown (a no-op).
func installNoop() *Runtime {
	rt := Noop()
	otel.SetTracerProvider(rt.noopTracer)
	otel.SetMeterProvider(rt.noopMeter)
	return rt
}

// collectorAvailable does a TCP dial probe to the endpoint. A 500ms timeout
// keeps it snappy. When the endpoint is empty (use-defaults), we optimistically
// return true and let the exporter fail fast when the collector is actually
// absent — the failure is absorbed by setupWithFactories and degenerates to
// no-op.
func collectorAvailable(ctx context.Context, endpoint string) bool {
	if endpoint == "" {
		return true
	}
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", endpoint)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Setup installs the process-wide OTel providers. When cfg.Enabled is false,
// the collector is unreachable, OR an exporter construction fails, the runtime
// degrades to no-op (constraint 5: the agent must continue working). The
// caller MUST still call Shutdown on the returned Runtime to flush pending
// spans/metrics before process exit; Shutdown on a no-op Runtime is safe.
func Setup(ctx context.Context, cfg Config) *Runtime {
	if !cfg.Enabled {
		return installNoop()
	}
	cfg = normalizeConfig(cfg)
	if !collectorAvailable(ctx, cfg.Endpoint) {
		slog.WarnContext(ctx, "otel collector unavailable; telemetry disabled",
			"error_type", "collector_unreachable")
		return installNoop()
	}
	return setupWithFactories(ctx, cfg, defaults)
}

// setupWithFactories is the internal wire-up shared by Setup and tests. It
// builds the trace and metric exporters via f, constructs the SDK providers,
// and sets them as process globals. Exporter failures degrade to no-op.
func setupWithFactories(ctx context.Context, cfg Config, f factories) *Runtime {
	expTrace, err := f.trace(ctx, cfg)
	if err != nil {
		slog.WarnContext(ctx, "otel trace exporter unavailable; telemetry disabled",
			"error_type", "trace_exporter_setup")
		return installNoop()
	}
	expMetric, err := f.metric(ctx, cfg)
	if err != nil {
		_ = expTrace.Shutdown(ctx)
		slog.WarnContext(ctx, "otel metric exporter unavailable; telemetry disabled",
			"error_type", "metric_exporter_setup")
		return installNoop()
	}
	res := resource.NewWithAttributes(
		"",
		attribute.String("service.name", cfg.ServiceName),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(expTrace),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		sdktrace.WithResource(res),
	)
	reader := sdkmetric.NewPeriodicReader(expMetric, sdkmetric.WithInterval(15*time.Second))
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return &Runtime{enabled: true, traces: tp, metrics: mp}
}

// Enabled reports whether the Runtime has live (non-noop) providers. A nil
// receiver is valid and returns false.
func (r *Runtime) Enabled() bool {
	return r != nil && r.enabled
}

// Tracer returns the process tracer scoped to name. When the Runtime is
// disabled or nil, returns a no-op tracer so callers never need to branch.
func (r *Runtime) Tracer(name string) trace.Tracer {
	if r == nil || !r.enabled {
		return tracenoop.NewTracerProvider().Tracer(name)
	}
	return r.traces.Tracer(name)
}

// Meter returns the process meter scoped to name.
func (r *Runtime) Meter(name string) metric.Meter {
	if r == nil || !r.enabled {
		return metricnoop.NewMeterProvider().Meter(name)
	}
	return r.metrics.Meter(name)
}

// Shutdown flushes and closes the exporters. Idempotent: a second call
// returns nil. A nil or disabled Runtime is safe (returns nil).
func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil || !r.enabled {
		return nil
	}
	return errors.Join(r.traces.Shutdown(ctx), r.metrics.Shutdown(ctx))
}
