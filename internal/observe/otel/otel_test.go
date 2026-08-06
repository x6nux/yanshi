package otelobs

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// restoreOTelGlobals snapshots the process-global TracerProvider and
// MeterProvider and restores them at test end. Setup() and setupWithFactories
// both call otel.SetTracerProvider / SetMeterProvider, so any test that
// exercises them MUST install this cleanup and MUST NOT call t.Parallel — the
// OTel globals are process-wide and concurrent SetTracerProvider calls race.
func restoreOTelGlobals(t *testing.T) {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevMP := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
	})
}

// resetOTelNoop is the defensive baseline installed at test start so a
// previous test that set a real provider cannot leak into this one before
// Setup runs.
func resetOTelNoop() {
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	otel.SetMeterProvider(metricnoop.NewMeterProvider())
}

// TestSetupDisabledReturnsNoop.
//
// ledger: C4/OBS2#3 可关闭
func TestSetupDisabledReturnsNoop(t *testing.T) {
	restoreOTelGlobals(t)
	resetOTelNoop()
	rt := Setup(context.Background(), Config{})
	if rt.Enabled() {
		t.Fatal("disabled config must be no-op")
	}
	if err := rt.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSetupExporterFailureSoftDegradesToNoop(t *testing.T) {
	restoreOTelGlobals(t)
	resetOTelNoop()
	boom := errors.New("collector unavailable")
	rt := setupWithFactories(context.Background(), Config{Enabled: true, ServiceName: "yanshi", SampleRatio: 1}, factories{
		trace:  func(context.Context, Config) (sdktrace.SpanExporter, error) { return nil, boom },
		metric: func(context.Context, Config) (sdkmetric.Exporter, error) { return nil, boom },
	})
	if rt.Enabled() {
		t.Fatal("exporter failure must soft-degrade to no-op")
	}
}

func TestNormalizeConfig(t *testing.T) {
	// Pure function — no global state touched, no isolation needed.
	cfg := normalizeConfig(Config{Enabled: true, SampleRatio: 2})
	if cfg.ServiceName != "yanshi" || cfg.SampleRatio != 1 {
		t.Fatalf("normalized=%+v", cfg)
	}
	cfg = normalizeConfig(Config{Enabled: true, SampleRatio: -1})
	if cfg.SampleRatio != 0 {
		t.Fatalf("ratio=%v", cfg.SampleRatio)
	}
}
