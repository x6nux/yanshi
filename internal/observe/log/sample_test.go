package log

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestSamplingNeverDropsAnError is the acceptance clause stated as a test.
//
// "sampling does not lose critical errors" is only true if losing one is
// IMPOSSIBLE, not unlikely. A probabilistic keep-rate that happens to favour
// errors still loses one occasionally -- and the occasion where sampling
// matters most, an incident producing thousands of lines, is exactly when the
// odds are worst. So WARN and above bypass the sampler entirely and do not
// even consume another message's budget.
func TestSamplingNeverDropsAnError(t *testing.T) {
	var buf bytes.Buffer
	s := newSampler(3, time.Minute)
	h := &redactHandler{inner: slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}), sample: s}
	l := slog.New(h)

	for i := 0; i < 500; i++ {
		l.Info("chatty")
		l.Error("boom")
		l.Warn("careful")
	}

	out := buf.String()
	if got := strings.Count(out, "boom"); got != 500 {
		t.Fatalf("errors dropped: %d of 500 survived", got)
	}
	if got := strings.Count(out, "careful"); got != 500 {
		t.Fatalf("warnings dropped: %d of 500 survived", got)
	}
	if got := strings.Count(out, "chatty"); got != 3 {
		t.Fatalf("info was not throttled to the burst of 3: %d survived", got)
	}
}

// TestSamplingIsPerMessage pins the keying choice. A global budget would let
// one tight loop crowd out every other call site -- the line that explains an
// incident would be dropped to make room for the thousandth health check.
func TestSamplingIsPerMessage(t *testing.T) {
	var buf bytes.Buffer
	s := newSampler(2, time.Minute)
	l := slog.New(&redactHandler{inner: slog.NewTextHandler(&buf, nil), sample: s})

	for i := 0; i < 100; i++ {
		l.Info("noisy")
	}
	l.Info("rare")

	out := buf.String()
	if strings.Count(out, "noisy") != 2 {
		t.Fatalf("noisy not throttled: %d", strings.Count(out, "noisy"))
	}
	if !strings.Contains(out, "rare") {
		t.Fatal("a rare message was crowded out by a noisy one: the budget is not per-message")
	}
}

// TestSuppressedCountIsReported: a reader who cannot tell throttling from
// absence draws the wrong conclusion about how often something happened, so
// the first survivor of a new window carries the count of what was dropped.
func TestSuppressedCountIsReported(t *testing.T) {
	var buf bytes.Buffer
	s := newSampler(2, time.Minute)
	now := time.Now()
	s.now = func() time.Time { return now } // drive time rather than sleeping
	l := slog.New(&redactHandler{inner: slog.NewTextHandler(&buf, nil), sample: s})

	for i := 0; i < 10; i++ {
		l.Info("burst")
	}
	buf.Reset()

	now = now.Add(2 * time.Minute) // next window
	l.Info("burst")

	out := buf.String()
	if !strings.Contains(out, "suppressed=8") {
		t.Fatalf("the gap must be visible, not silent: %q", out)
	}
}

// TestSamplerSurvivesDerivedLoggers: WithAttrs/WithGroup must share the
// budget. A logger re-derived per request that got a fresh budget each time
// would never throttle at all -- the sampler would exist and do nothing.
func TestSamplerSurvivesDerivedLoggers(t *testing.T) {
	var buf bytes.Buffer
	s := newSampler(2, time.Minute)
	base := slog.New(&redactHandler{inner: slog.NewTextHandler(&buf, nil), sample: s})

	for i := 0; i < 50; i++ {
		base.With(slog.Int("req", i)).Info("per-request")
	}
	if got := strings.Count(buf.String(), "per-request"); got != 2 {
		t.Fatalf("derived loggers each got a fresh budget: %d survived", got)
	}
}

var _ = context.Background
