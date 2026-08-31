// internal/llm/eino/m7_ratelimit_wire_test.go
//
// M7 over the wire: does a configured QPM actually change the RATE at which
// requests arrive at the provider?
//
// ratelimit_test.go already drives TokenBucket with an injected clock and
// asserts the arithmetic, which is the right way to test arithmetic. What it
// cannot observe is whether the bucket is consulted on the path a request
// takes, whether the per-model table is keyed the way the lookup keys it, or
// whether a cancelled turn actually stops waiting. Those are the parts that
// have historically been wrong (bootstrap's providerConfigsByKey exists
// entirely because a mismatched key made every per-provider limit a silent
// no-op), and all three are visible only as ARRIVAL TIMES at a server.
//
// So every test here measures when the stub was hit.
package eino

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

// TestM7_ConfiguredQPMThrottlesRealArrivals fires a burst at a QPM-limited
// model and measures the spread of arrivals at the server.
//
// The numbers are chosen so the assertion is not a race: with burst=1 and
// QPM=600 the bucket refills one token every 100ms, so four requests cannot
// arrive in less than ~300ms no matter how fast the machine is. An unthrottled
// run against a loopback stub completes in single-digit milliseconds, so the
// two outcomes are two orders of magnitude apart.
func TestM7_ConfiguredQPMThrottlesRealArrivals(t *testing.T) {
	const (
		n           = 4
		qpm         = 600 // one token per 100ms
		perToken    = 100 * time.Millisecond
		minExpected = 2 * perToken // n-1 refills, minus one refill of slack
	)
	// Why minExpected is n-2 refills, not n-1: 300ms is the EXACT boundary —
	// the last refill lands at t=300ms and the arrival stamp is taken after
	// the goroutine wakes, so timer jitter or scheduler delay on the FIRST
	// arrival pushes the measured spread a fraction below 300ms and the
	// assertion flakes under full-suite load (seen 2026-09-01 on -race).
	// 200ms still keeps two orders of magnitude between a throttled spread
	// and an unthrottled loopback fan-out (~single-digit ms), which is the
	// property this test exists to pin.
	s := newStubProvider(t, nil)
	inner, _ := buildStubModel(t, s, nil)
	limiter := NewRateLimiter(RateLimitConfig{}, map[string]RateLimitConfig{
		"stub-model-a": {QPM: qpm, Burst: 1},
	})
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "stub-model-a", Limiter: limiter})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = a.Generate(ctx, []*schema.Message{schema.UserMessage("hi")})
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	reqs := s.chatRequests()
	t.Logf("%d requests at qpm=%d burst=1 took %v; gaps=%v", len(reqs), qpm, elapsed, s.gaps())
	if len(reqs) != n {
		t.Fatalf("stub saw %d requests, want %d", len(reqs), n)
	}
	spread := reqs[len(reqs)-1].At.Sub(reqs[0].At)
	if spread < minExpected {
		t.Errorf("first-to-last arrival spread %v < %v: the configured QPM did not throttle "+
			"anything, so the whole fan-out hit the provider at once", spread, minExpected)
	}
}

// TestM7_ZeroQPMDoesNotThrottle is the negative control, and it also pins the
// default: an operator who configures nothing must keep exactly the behaviour
// they had before M7 existed.
//
// The upper bound is generous (a second for four loopback round trips) because
// this test must not become flaky on a loaded CI box. It is still three orders
// of magnitude below what any real throttle would produce.
func TestM7_ZeroQPMDoesNotThrottle(t *testing.T) {
	const n = 4
	s := newStubProvider(t, nil)
	inner, _ := buildStubModel(t, s, nil)
	limiter := NewRateLimiter(RateLimitConfig{}, nil) // QPM 0 everywhere = unlimited
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "stub-model-a", Limiter: limiter})

	if limiter.Enabled("stub-model-a") {
		t.Error("a zero-QPM limiter reports itself enabled; zero must mean unlimited, not empty")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	for range n {
		if _, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("hi")}); err != nil {
			t.Fatalf("Generate: %v", err)
		}
	}
	elapsed := time.Since(start)
	t.Logf("%d sequential unthrottled calls took %v", n, elapsed)
	if elapsed > time.Second {
		t.Errorf("unthrottled calls took %v; qpm=0 must not introduce any delay", elapsed)
	}
}

// TestM7_ThrottleWaitRespectsContextCancellation is the responsiveness
// property, and it is the one with real user impact: a fan-out queued behind a
// 20 QPM limit puts callers minutes deep in a wait, and Ctrl-C must not have to
// wait that queue out before the turn notices it was cancelled.
//
// The limit is set absurdly low so that, were cancellation ignored, this test
// would hang for a minute rather than fail — and the deadline below turns that
// hang into a failure with a message.
func TestM7_ThrottleWaitRespectsContextCancellation(t *testing.T) {
	s := newStubProvider(t, nil)
	inner, _ := buildStubModel(t, s, nil)
	limiter := NewRateLimiter(RateLimitConfig{}, map[string]RateLimitConfig{
		"stub-model-a": {QPM: 1, Burst: 1}, // one token, then a 60s wait
	})
	a := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "stub-model-a", Limiter: limiter})

	// Spend the single burst token.
	if _, err := a.Generate(context.Background(), []*schema.Message{schema.UserMessage("first")}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// The next call must queue ~60s. Cancel it shortly after it starts.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Generate(ctx, []*schema.Message{schema.UserMessage("second")})
		done <- err
	}()
	time.AfterFunc(150*time.Millisecond, cancel)

	start := time.Now()
	select {
	case err := <-done:
		waited := time.Since(start)
		t.Logf("cancelled throttle wait returned after %v with %v", waited, err)
		if err == nil {
			t.Error("the cancelled call succeeded; it should have returned the context error")
		}
		if waited > 5*time.Second {
			t.Errorf("cancellation took %v to take effect", waited)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the throttle wait ignored context cancellation: a cancelled turn would sit in " +
			"the rate-limit queue for the full refill period before noticing")
	}

	// Only the first call may have reached the provider.
	if n := len(s.chatRequests()); n != 1 {
		t.Errorf("stub saw %d requests, want 1: the cancelled call must not be sent", n)
	}
}

// TestM7_PerModelLimitsAreIndependent pins the reason the limiter is keyed by
// model at all: a background summarisation hammering a cheap model must not
// stall the user's turn on an expensive one.
//
// A single shared bucket would make the unthrottled model wait behind the
// throttled one, and nothing above this layer would report why.
func TestM7_PerModelLimitsAreIndependent(t *testing.T) {
	s := newStubProvider(t, nil)
	inner, _ := buildStubModel(t, s, nil)
	limiter := NewRateLimiter(RateLimitConfig{}, map[string]RateLimitConfig{
		"throttled": {QPM: 1, Burst: 1},
	})
	throttled := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "throttled", Limiter: limiter})
	free := NewAdaptiveModel(inner, AdaptiveConfig{ModelID: "free", Limiter: limiter})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Drain the throttled model's only token.
	if _, err := throttled.Generate(ctx, []*schema.Message{schema.UserMessage("x")}); err != nil {
		t.Fatalf("throttled warmup: %v", err)
	}

	start := time.Now()
	if _, err := free.Generate(ctx, []*schema.Message{schema.UserMessage("y")}); err != nil {
		t.Fatalf("free model: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("unconfigured model waited %v while another model's bucket was empty", elapsed)
	if elapsed > 2*time.Second {
		t.Errorf("an unconfigured model waited %v behind a different model's throttle; the "+
			"buckets are not independent", elapsed)
	}
}
