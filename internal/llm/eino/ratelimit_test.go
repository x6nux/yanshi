package eino

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock, so refill behaviour is asserted
// exactly rather than by sleeping and hoping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestNewTokenBucketConfig is the M7 construction table, including the two
// behaviours the defaults encode: QPM 0 means unlimited (the historical
// behaviour must be the default) and a derived burst is capped.
func TestNewTokenBucketConfig(t *testing.T) {
	cases := []struct {
		name        string
		cfg         RateLimitConfig
		wantEnabled bool
		wantBurst   float64
		wantPerSec  float64
	}{
		{name: "zero QPM is unlimited", cfg: RateLimitConfig{}, wantEnabled: false},
		{name: "negative QPM is unlimited", cfg: RateLimitConfig{QPM: -5}, wantEnabled: false},
		{
			name: "small QPM derives its burst from the rate",
			cfg:  RateLimitConfig{QPM: 6}, wantEnabled: true, wantBurst: 6, wantPerSec: 0.1,
		},
		{
			name: "large QPM has its derived burst capped",
			cfg:  RateLimitConfig{QPM: 600}, wantEnabled: true, wantBurst: defaultMaxBurst, wantPerSec: 10,
		},
		{
			name: "explicit burst wins over the derivation",
			cfg:  RateLimitConfig{QPM: 600, Burst: 50}, wantEnabled: true, wantBurst: 50, wantPerSec: 10,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewTokenBucket(tc.cfg)
			if b.Enabled() != tc.wantEnabled {
				t.Fatalf("Enabled = %v, want %v", b.Enabled(), tc.wantEnabled)
			}
			if !tc.wantEnabled {
				return
			}
			if b.capacity != tc.wantBurst {
				t.Errorf("capacity = %v, want %v", b.capacity, tc.wantBurst)
			}
			if b.perSec != tc.wantPerSec {
				t.Errorf("perSec = %v, want %v", b.perSec, tc.wantPerSec)
			}
		})
	}
}

// TestTokenBucketBurstThenThrottle pins the shape a sub-agent fan-out sees: the
// burst goes straight through, and the request after it must wait one refill
// period.
func TestTokenBucketBurstThenThrottle(t *testing.T) {
	clk := newFakeClock()
	b := NewTokenBucket(RateLimitConfig{QPM: 60, Burst: 3}) // 1 token/sec
	b.now = clk.now

	for i := 0; i < 3; i++ {
		if d := b.reserve(); d != 0 {
			t.Fatalf("burst request %d waited %v, want 0", i, d)
		}
	}
	d := b.reserve()
	if d <= 0 {
		t.Fatalf("the 4th request waited %v, want a positive delay", d)
	}
	if d > time.Second+10*time.Millisecond || d < 900*time.Millisecond {
		t.Errorf("delay = %v, want ~1s at 60 QPM", d)
	}
	// The 5th queues behind the 4th rather than being told the same delay.
	if d5 := b.reserve(); d5 <= d {
		t.Errorf("the 5th request waited %v, want more than the 4th's %v", d5, d)
	}
}

// TestTokenBucketRefills pins that waiting restores capacity, capped at burst —
// an idle session must not accumulate an unbounded credit.
func TestTokenBucketRefills(t *testing.T) {
	clk := newFakeClock()
	b := NewTokenBucket(RateLimitConfig{QPM: 60, Burst: 2})
	b.now = clk.now

	b.reserve()
	b.reserve()
	if d := b.reserve(); d <= 0 {
		t.Fatal("bucket was not empty after draining the burst")
	}
	// Idle far longer than the burst is worth.
	clk.advance(time.Hour)
	if d := b.reserve(); d != 0 {
		t.Errorf("after refilling, reserve waited %v, want 0", d)
	}
	if d := b.reserve(); d != 0 {
		t.Errorf("second post-refill reserve waited %v, want 0", d)
	}
	if d := b.reserve(); d <= 0 {
		t.Error("the bucket accumulated more than its burst while idle")
	}
}

// TestTokenBucketWaitRespectsContext pins the cancellation requirement: a user
// pressing Ctrl-C behind a throttled fan-out must not wait out the queue.
func TestTokenBucketWaitRespectsContext(t *testing.T) {
	b := NewTokenBucket(RateLimitConfig{QPM: 1, Burst: 1})
	if err := b.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := b.Wait(ctx); err == nil {
		t.Fatal("Wait returned nil for a cancelled context")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Wait blocked %v on a cancelled context", elapsed)
	}
}

// TestTokenBucketDisabledWaitIsFree pins that the default (unlimited) path does
// not block, which is what makes it safe to call unconditionally.
func TestTokenBucketDisabledWaitIsFree(t *testing.T) {
	b := NewTokenBucket(RateLimitConfig{})
	start := time.Now()
	for i := 0; i < 1000; i++ {
		if err := b.Wait(context.Background()); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("1000 unlimited Waits took %v", elapsed)
	}
}

// TestRateLimiterIsPerModel pins the isolation that motivates M7: draining one
// model's bucket must not throttle another.
func TestRateLimiterIsPerModel(t *testing.T) {
	l := NewRateLimiter(RateLimitConfig{QPM: 60, Burst: 1}, nil)
	if d := l.bucketFor("a").reserve(); d != 0 {
		t.Fatalf("first call on a waited %v", d)
	}
	if d := l.bucketFor("a").reserve(); d <= 0 {
		t.Fatal("model a's bucket did not throttle after its burst")
	}
	if d := l.bucketFor("b").reserve(); d != 0 {
		t.Error("model b was throttled by model a's traffic")
	}
}

// TestRateLimiterPerModelOverride pins that an explicit entry beats the default,
// including the "unlimited for this one model" direction.
func TestRateLimiterPerModelOverride(t *testing.T) {
	l := NewRateLimiter(RateLimitConfig{QPM: 60}, map[string]RateLimitConfig{
		"fast": {QPM: 0},
		"slow": {QPM: 6, Burst: 1},
	})
	if l.Enabled("fast") {
		t.Error("an explicit QPM 0 override did not disable throttling")
	}
	if !l.Enabled("slow") || !l.Enabled("other") {
		t.Error("the default QPM did not apply")
	}
	if d := l.bucketFor("slow").reserve(); d != 0 {
		t.Fatalf("first slow call waited %v", d)
	}
	if d := l.bucketFor("slow").reserve(); d < 9*time.Second {
		t.Errorf("slow's second call waited %v, want ~10s at 6 QPM", d)
	}
}

// TestRateLimiterNilIsSafe pins the no-nil-check contract at the limiter level.
func TestRateLimiterNilIsSafe(t *testing.T) {
	var l *RateLimiter
	if err := l.Wait(context.Background(), "m"); err != nil {
		t.Errorf("nil limiter Wait: %v", err)
	}
	if l.Enabled("m") {
		t.Error("nil limiter reported enabled")
	}
}

// TestRateLimiterConcurrentBucketCreation drives first-use creation from many
// goroutines. Under -race this is what proves bucketFor's lock covers the map.
func TestRateLimiterConcurrentBucketCreation(t *testing.T) {
	l := NewRateLimiter(RateLimitConfig{QPM: 6000}, nil)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = l.Wait(context.Background(), "m")
		}(i)
	}
	wg.Wait()
	if len(l.buckets) != 1 {
		t.Errorf("created %d buckets for one model", len(l.buckets))
	}
}
